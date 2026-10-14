package gitlab

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"time"

	"gitlab.com/gitlab-org/api/client-go"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/common"
)

var _ core.RemoteBridge = &Gitlab{}

// NewRemote returns the GitLab project of the bridge, for the sync engine,
// acting with every GitLab token of the instance.
func (g *Gitlab) NewRemote(ctx context.Context, repo *cache.RepoCache, conf core.Configuration) (core.Remote, error) {
	creds, err := instanceTokens(repo, conf[confKeyGitlabBaseUrl])
	if err != nil {
		return nil, err
	}
	tokens := make([]*auth.Token, 0, len(creds))
	for _, cred := range creds {
		tokens = append(tokens, cred.(*auth.Token))
	}
	return newRemote(ctx, conf[confKeyGitlabBaseUrl], conf[confKeyProjectID], conf[confKeyDefaultLogin], tokens)
}

var _ core.Remote = &remote{}

// remote is a GitLab project, as the sync engine sees it.
type remote struct {
	baseURL   string
	projectID string
	instance  string

	accounts []core.Account
	// writers act as an account, by account id. They don't retry: GitLab
	// may have done a write whose answer was lost, and the engine recovers
	// from that without writing twice.
	writers map[string]*gitlab.Client
	// reader lists the project, with the default login's token if any.
	reader *gitlab.Client
}

func newRemote(ctx context.Context, baseURL, projectID, defaultLogin string, tokens []*auth.Token) (*remote, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	r := &remote{
		baseURL:   baseURL,
		projectID: projectID,
		instance:  u.Host,
		writers:   map[string]*gitlab.Client{},
	}
	for _, token := range tokens {
		reader, err := buildClient(baseURL, token)
		if err != nil {
			return nil, err
		}
		user, _, err := reader.Users.CurrentUser(gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("token %s: %w", token.ID().Human(), err)
		}
		id := strconv.FormatInt(user.ID, 10)
		if _, ok := r.writers[id]; ok {
			continue
		}
		writer, err := gitlab.NewClient(token.Value, gitlab.WithBaseURL(baseURL), gitlab.WithoutRetries())
		if err != nil {
			return nil, err
		}
		r.writers[id] = writer
		r.accounts = append(r.accounts, core.Account{User: core.User{
			ID:        id,
			Login:     user.Username,
			Name:      user.Name,
			Email:     user.PublicEmail,
			AvatarURL: user.AvatarURL,
		}})
		if r.reader == nil || user.Username == defaultLogin {
			r.reader = reader
		}
	}
	if r.reader == nil {
		return nil, ErrMissingIdentityToken
	}
	return r, nil
}

func (r *remote) Key() string      { return r.instance + "/" + r.projectID }
func (r *remote) Instance() string { return r.instance }
func (r *remote) Kind() string     { return target }

func (r *remote) Accounts(context.Context) ([]core.Account, error) {
	return slices.Clone(r.accounts), nil
}

func (r *remote) Issues(ctx context.Context, since time.Time) iter.Seq2[*core.Issue, error] {
	return func(yield func(*core.Issue, error) bool) {
		for res := range Issues(ctx, r.reader, r.projectID, since) {
			if res.Err != nil {
				yield(nil, res.Err)
				return
			}
			issue, err := r.convert(ctx, res.Issue)
			if !yield(issue, err) || err != nil {
				return
			}
		}
	}
}

// convert returns a GitLab issue, its notes and events, as the engine wants
// it. GitLab keeps no history of titles and texts: the original ones are the
// current ones, and text edits come without event.
func (r *remote) convert(ctx context.Context, gi *gitlab.Issue) (*core.Issue, error) {
	status := common.OpenStatus
	if gi.State == "closed" {
		status = common.ClosedStatus
	}
	iid := strconv.FormatInt(gi.IID, 10)
	issue := &core.Issue{
		Ref:           core.Ref{ID: iid, Number: iid, URL: gi.WebURL},
		Author:        issueUser(gi.Author),
		Created:       *gi.CreatedAt,
		Updated:       *gi.UpdatedAt,
		OriginalTitle: gi.Title,
		Title:         gi.Title,
		OriginalBody:  gi.Description,
		Body:          gi.Description,
		Status:        status,
		Labels:        slices.Clone(gi.Labels),
	}

	for e := range Notes(ctx, r.reader, gi) {
		switch e := e.(type) {
		case ErrorEvent:
			return nil, e.Err
		case NoteEvent:
			author := core.User{ID: strconv.FormatInt(e.Author.ID, 10), Login: e.Author.Username, Name: e.Author.Name}
			if !e.System {
				issue.Comments = append(issue.Comments, core.Comment{
					Ref:      core.Ref{ID: e.ID(), URL: fmt.Sprintf("%s#note_%s", gi.WebURL, e.ID())},
					Author:   author,
					Created:  *e.Note.CreatedAt,
					Updated:  *e.Note.UpdatedAt,
					Original: e.Body,
					Body:     e.Body,
				})
				continue
			}
			// the only trace of a description edit: when, and by whom
			if e.Kind() == EventDescriptionChanged && e.CreatedAt().After(issue.BodyUpdated) {
				issue.BodyUpdated = e.CreatedAt()
				issue.BodyEditor = &author
				continue
			}
			if e.Kind() == EventTitleChanged {
				title, err := e.Title()
				if err != nil {
					continue
				}
				issue.Events = append(issue.Events, core.Event{
					ID: e.ID(), Kind: core.EventTitle, Author: author, Time: e.CreatedAt(), Title: title,
				})
			}
		}
	}

	for e := range LabelEvents(ctx, r.reader, gi) {
		switch e := e.(type) {
		case ErrorEvent:
			return nil, e.Err
		case LabelEvent:
			ev := core.Event{ID: e.ID(), Kind: core.EventLabels, Author: basicUser(&e.User), Time: e.CreatedAt()}
			switch e.Kind() {
			case EventAddLabel:
				ev.Added = []string{e.Label.Name}
			case EventRemoveLabel:
				ev.Removed = []string{e.Label.Name}
			default:
				continue
			}
			issue.Events = append(issue.Events, ev)
		}
	}

	for e := range StateEvents(ctx, r.reader, gi) {
		switch e := e.(type) {
		case ErrorEvent:
			return nil, e.Err
		case StateEvent:
			ev := core.Event{ID: e.ID(), Kind: core.EventStatus, Author: basicUser(e.User), Time: e.CreatedAt()}
			switch e.Kind() {
			case EventClosed:
				ev.Status = common.ClosedStatus
			case EventReopened:
				ev.Status = common.OpenStatus
			default:
				continue
			}
			issue.Events = append(issue.Events, ev)
		}
	}

	slices.SortStableFunc(issue.Events, func(a, b core.Event) int { return a.Time.Compare(b.Time) })
	return issue, nil
}

func (r *remote) User(ctx context.Context, id string) (core.User, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return core.User{}, err
	}
	u, _, err := r.reader.Users.GetUser(n, gitlab.GetUsersOptions{}, gitlab.WithContext(ctx))
	if err != nil {
		return core.User{}, err
	}
	return core.User{ID: id, Login: u.Username, Name: u.Name, Email: u.PublicEmail, AvatarURL: u.AvatarURL}, nil
}

func (r *remote) CreateIssue(ctx context.Context, as core.Account, title, body string) (core.Ref, error) {
	c, err := r.writer(as)
	if err != nil {
		return core.Ref{}, err
	}
	issue, _, err := c.Issues.CreateIssue(r.projectID, &gitlab.CreateIssueOptions{
		Title:       &title,
		Description: &body,
	}, gitlab.WithContext(ctx))
	if err != nil {
		return core.Ref{}, mapErr(err)
	}
	iid := strconv.FormatInt(issue.IID, 10)
	return core.Ref{ID: iid, Number: iid, URL: issue.WebURL}, nil
}

func (r *remote) AddComment(ctx context.Context, as core.Account, issue core.Ref, body string) (core.Ref, error) {
	c, iid, err := r.writerOn(as, issue)
	if err != nil {
		return core.Ref{}, err
	}
	note, _, err := c.Notes.CreateIssueNote(r.projectID, iid, &gitlab.CreateIssueNoteOptions{Body: &body}, gitlab.WithContext(ctx))
	if err != nil {
		return core.Ref{}, r.confirm(ctx, mapErr(err), iid, 0)
	}
	id := strconv.FormatInt(note.ID, 10)
	return core.Ref{ID: id, URL: fmt.Sprintf("%s#note_%s", issue.URL, id)}, nil
}

func (r *remote) EditText(ctx context.Context, as core.Account, issue, target core.Ref, body string) error {
	if target.ID == issue.ID {
		return r.update(ctx, as, issue, &gitlab.UpdateIssueOptions{Description: &body})
	}
	c, iid, err := r.writerOn(as, issue)
	if err != nil {
		return err
	}
	noteID, err := strconv.ParseInt(target.ID, 10, 64)
	if err != nil {
		return err
	}
	_, _, err = c.Notes.UpdateIssueNote(r.projectID, iid, noteID, &gitlab.UpdateIssueNoteOptions{Body: &body}, gitlab.WithContext(ctx))
	return r.confirm(ctx, mapErr(err), iid, noteID)
}

func (r *remote) SetStatus(ctx context.Context, as core.Account, issue core.Ref, status common.Status) error {
	state := "reopen"
	if status == common.ClosedStatus {
		state = "close"
	}
	return r.update(ctx, as, issue, &gitlab.UpdateIssueOptions{StateEvent: &state})
}

func (r *remote) SetTitle(ctx context.Context, as core.Account, issue core.Ref, title string) error {
	return r.update(ctx, as, issue, &gitlab.UpdateIssueOptions{Title: &title})
}

func (r *remote) SetLabels(ctx context.Context, as core.Account, issue core.Ref, labels []string) error {
	opt := gitlab.LabelOptions(labels)
	return r.update(ctx, as, issue, &gitlab.UpdateIssueOptions{Labels: &opt})
}

func (r *remote) update(ctx context.Context, as core.Account, issue core.Ref, opt *gitlab.UpdateIssueOptions) error {
	c, iid, err := r.writerOn(as, issue)
	if err != nil {
		return err
	}
	_, _, err = c.Issues.UpdateIssue(r.projectID, iid, opt, gitlab.WithContext(ctx))
	return r.confirm(ctx, mapErr(err), iid, 0)
}

// confirm checks with the reader that an issue (and a note, if not 0) a
// write didn't find is gone: GitLab answers the same 404 to a token that
// can't see the project. The issue still there, the write was refused.
func (r *remote) confirm(ctx context.Context, err error, iid, noteID int64) error {
	if !errors.Is(err, core.ErrGone) {
		return err
	}
	if _, _, perr := r.reader.Projects.GetProject(r.projectID, nil, gitlab.WithContext(ctx)); perr != nil {
		return fmt.Errorf("checking the project after %w: %w", err, perr)
	}
	_, _, gerr := r.reader.Issues.GetIssue(r.projectID, iid, gitlab.WithContext(ctx))
	if gerr == nil && noteID != 0 {
		_, _, gerr = r.reader.Notes.GetIssueNote(r.projectID, iid, noteID, gitlab.WithContext(ctx))
	}
	switch {
	case errors.Is(gerr, gitlab.ErrNotFound):
		return err
	case gerr != nil:
		return gerr
	}
	return fmt.Errorf("%w: the token can't see it: %w", core.ErrRefused, err)
}

func (r *remote) writer(as core.Account) (*gitlab.Client, error) {
	c, ok := r.writers[as.ID]
	if !ok {
		return nil, fmt.Errorf("no GitLab token for %s", as.Login)
	}
	return c, nil
}

func (r *remote) writerOn(as core.Account, issue core.Ref) (*gitlab.Client, int64, error) {
	c, err := r.writer(as)
	if err != nil {
		return nil, 0, err
	}
	iid, err := strconv.ParseInt(issue.ID, 10, 64)
	return c, iid, err
}

func (r *remote) Dialect() core.Dialect {
	return core.Dialect{
		// "!12" is a merge request and "&12" an epic: only "#12" is an issue
		Ref:       regexp.MustCompile(`(?:^|[^\w/#!&])(#(\d+))\b`),
		FormatRef: func(n string) string { return "#" + n },
	}
}

// Legacy reads the metadata of the GitLab bridge before the sync engine: a
// gitlab-id on operations of a bug of this project.
func (r *remote) Legacy(meta, createMeta map[string]string) (string, bool) {
	id, ok := meta[metaKeyGitlabId]
	if !ok || createMeta[metaKeyGitlabProject] != r.projectID {
		return "", false
	}
	// older versions exported with an empty base-url
	if baseURL := createMeta[metaKeyGitlabBaseUrl]; baseURL != "" && !sameBaseURL(baseURL, r.baseURL) {
		return "", false
	}
	return id, true
}

func (r *remote) LegacyUser() (string, bool) {
	return metaKeyGitlabId, false
}

// mapErr turns GitLab's answers into the engine's errors.
func mapErr(err error) error {
	if errors.Is(err, gitlab.ErrNotFound) {
		return fmt.Errorf("%w: %w", core.ErrGone, err)
	}
	var resp *gitlab.ErrorResponse
	if !errors.As(err, &resp) || resp.Response == nil {
		return err
	}
	switch resp.Response.StatusCode {
	case http.StatusForbidden:
		return fmt.Errorf("%w: %w", core.ErrRefused, err)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %w", core.ErrGone, err)
	case http.StatusTooManyRequests:
		wait := time.Minute
		if s, err := strconv.Atoi(resp.Response.Header.Get("Retry-After")); err == nil {
			wait = time.Duration(s) * time.Second
		}
		return core.ErrRateLimited{Until: time.Now().Add(wait)}
	}
	return err
}

func issueUser(a *gitlab.IssueAuthor) core.User {
	if a == nil {
		return core.User{}
	}
	return core.User{ID: strconv.FormatInt(a.ID, 10), Login: a.Username, Name: a.Name, AvatarURL: a.AvatarURL}
}

func basicUser(u *gitlab.BasicUser) core.User {
	if u == nil {
		return core.User{}
	}
	return core.User{ID: strconv.FormatInt(u.ID, 10), Login: u.Username, Name: u.Name}
}
