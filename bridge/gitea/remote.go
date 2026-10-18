package gitea

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/common"
)

// pageSize is the number of items asked for per page.
const pageSize = 50

var _ core.RemoteBridge = &Gitea{}

// NewRemote returns the Gitea repository of the bridge, for the sync engine,
// acting with every token of the Gitea instance.
func (g *Gitea) NewRemote(ctx context.Context, repo *cache.RepoCache, conf core.Configuration) (core.Remote, error) {
	creds, err := auth.List(repo,
		auth.WithTarget(target),
		auth.WithKind(auth.KindToken),
		auth.WithMeta(auth.MetaKeyBaseURL, conf[confKeyBaseURL]),
	)
	if err != nil {
		return nil, err
	}
	var tokens []string
	for _, cred := range creds {
		tokens = append(tokens, cred.(*auth.Token).Value)
	}
	return newRemote(ctx, http.DefaultClient, conf[confKeyBaseURL], conf[confKeyOwner], conf[confKeyProject], conf[confKeyDefaultLogin], tokens)
}

var _ core.Remote = &remote{}

// remote is a Gitea repository, as the sync engine sees it.
type remote struct {
	api
	instance       string
	owner, project string
	accounts       []core.Account
	tokens         map[string]string // by account id
	reader         string            // token listing the repository
	mu             sync.Mutex
	labels         map[string]int64 // id by lowercased name
}

func newRemote(ctx context.Context, client *http.Client, baseURL, owner, project, defaultLogin string, tokens []string) (*remote, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	r := &remote{
		api:      api{http: client, base: strings.TrimSuffix(baseURL, "/") + "/api/v1"},
		instance: u.Host,
		owner:    owner,
		project:  project,
		tokens:   map[string]string{},
	}
	for _, token := range tokens {
		var me apiUser
		if err := r.do(ctx, token, http.MethodGet, "/user", nil, &me); err != nil {
			return nil, fmt.Errorf("gitea token: %w", err)
		}
		account := core.Account{User: me.user()}
		if _, ok := r.tokens[account.ID]; ok {
			continue
		}
		r.tokens[account.ID] = token
		r.accounts = append(r.accounts, account)
		if r.reader == "" || account.Login == defaultLogin {
			r.reader = token
		}
	}
	if r.reader == "" {
		return nil, fmt.Errorf("no Gitea token for %s", baseURL)
	}
	return r, nil
}

type apiUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	FullName  string `json:"full_name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

func (u *apiUser) user() core.User {
	if u == nil {
		return core.User{ID: "-1", Login: "ghost", Name: "Ghost"}
	}
	return core.User{ID: strconv.FormatInt(u.ID, 10), Login: u.Login, Name: u.FullName, Email: u.Email, AvatarURL: u.AvatarURL}
}

type apiLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type apiIssue struct {
	ID          int64      `json:"id"`
	Number      int64      `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	User        *apiUser   `json:"user"`
	State       string     `json:"state"`
	Labels      []apiLabel `json:"labels"`
	HTMLURL     string     `json:"html_url"`
	Created     time.Time  `json:"created_at"`
	Updated     time.Time  `json:"updated_at"`
	PullRequest any        `json:"pull_request"`
}

// apiTimeline is an item of an issue's timeline: a comment, or an event.
type apiTimeline struct {
	ID       int64     `json:"id"`
	Type     string    `json:"type"`
	Body     string    `json:"body"`
	User     *apiUser  `json:"user"`
	HTMLURL  string    `json:"html_url"`
	Created  time.Time `json:"created_at"`
	Updated  time.Time `json:"updated_at"`
	OldTitle string    `json:"old_title"`
	NewTitle string    `json:"new_title"`
	Label    *apiLabel `json:"label"`
}

// issueRef identifies an issue by its number, which the API addresses it by.
// Its id is prefixed: Gitea numbers issues and comments separately, and both
// are remote ids of operations of the same bug.
func issueRef(gi apiIssue) core.Ref {
	number := strconv.FormatInt(gi.Number, 10)
	return core.Ref{ID: "issue-" + number, Number: number, URL: gi.HTMLURL}
}

func (r *remote) Key() string      { return r.instance + "/" + strings.ToLower(r.owner+"/"+r.project) }
func (r *remote) Instance() string { return r.instance }
func (r *remote) Kind() string     { return target }

func (r *remote) Accounts(context.Context) ([]core.Account, error) {
	return slices.Clone(r.accounts), nil
}

func (r *remote) repoPath(suffix string) string {
	return "/repos/" + url.PathEscape(r.owner) + "/" + url.PathEscape(r.project) + suffix
}

func (r *remote) Issues(ctx context.Context, since time.Time) iter.Seq2[*core.Issue, error] {
	return func(yield func(*core.Issue, error) bool) {
		for page := 1; ; page++ {
			q := url.Values{"state": {"all"}, "type": {"issues"}, "page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(pageSize)}}
			if !since.IsZero() {
				q.Set("since", since.UTC().Format(time.RFC3339))
			}
			var issues []apiIssue
			if err := r.do(ctx, r.reader, http.MethodGet, r.repoPath("/issues?"+q.Encode()), nil, &issues); err != nil {
				yield(nil, err)
				return
			}
			// the oldest first: Gitea lists the newest first, and dates to
			// the second
			slices.SortFunc(issues, func(a, b apiIssue) int {
				return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.Number, b.Number))
			})
			for _, gi := range issues {
				if gi.PullRequest != nil {
					continue
				}
				issue, err := r.convert(ctx, gi)
				if !yield(issue, err) || err != nil {
					return
				}
			}
			if len(issues) < pageSize {
				return
			}
		}
	}
}

func (r *remote) Fetch(ctx context.Context, ref core.Ref) (*core.Issue, error) {
	number := cmp.Or(ref.Number, strings.TrimPrefix(ref.ID, "issue-"))
	var gi apiIssue
	if err := r.do(ctx, r.reader, http.MethodGet, r.repoPath("/issues/"+url.PathEscape(number)), nil, &gi); err != nil {
		return nil, err
	}
	// numbered with the issues, and served by the same API: as on GitHub,
	// no issue has that number
	if gi.PullRequest != nil {
		return nil, fmt.Errorf("%w: #%s is a pull request", core.ErrGone, number)
	}
	return r.convert(ctx, gi)
}

// convert returns a Gitea issue and its timeline as the engine wants it.
// Gitea keeps no history of texts: the original ones are the current ones,
// and text edits come without event.
func (r *remote) convert(ctx context.Context, gi apiIssue) (*core.Issue, error) {
	status := common.OpenStatus
	if gi.State == "closed" {
		status = common.ClosedStatus
	}
	issue := &core.Issue{
		Ref:           issueRef(gi),
		Author:        gi.User.user(),
		Created:       gi.Created,
		Updated:       gi.Updated,
		OriginalTitle: gi.Title,
		Title:         gi.Title,
		OriginalBody:  gi.Body,
		Body:          gi.Body,
		Status:        status,
	}
	for _, l := range gi.Labels {
		issue.Labels = append(issue.Labels, l.Name)
	}
	slices.Sort(issue.Labels)

	renamed := false
	for page := 1; ; page++ {
		var items []apiTimeline
		q := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(pageSize)}}
		if err := r.do(ctx, r.reader, http.MethodGet, r.repoPath(fmt.Sprintf("/issues/%d/timeline?%s", gi.Number, q.Encode())), nil, &items); err != nil {
			return nil, err
		}
		for _, item := range items {
			id := strconv.FormatInt(item.ID, 10)
			ev := core.Event{ID: id, Author: item.User.user(), Time: item.Created}
			switch item.Type {
			case "comment":
				issue.Comments = append(issue.Comments, core.Comment{
					Ref:      core.Ref{ID: id, URL: item.HTMLURL},
					Author:   item.User.user(),
					Created:  item.Created,
					Updated:  item.Updated,
					Original: item.Body,
					Body:     item.Body,
				})
				continue
			case "close", "reopen":
				ev.Kind, ev.Status = core.EventStatus, common.OpenStatus
				if item.Type == "close" {
					ev.Status = common.ClosedStatus
				}
			case "change_title":
				if !renamed {
					renamed = true
					issue.OriginalTitle = item.OldTitle
				}
				ev.Kind, ev.Title = core.EventTitle, item.NewTitle
			case "label":
				if item.Label == nil {
					continue
				}
				ev.Kind = core.EventLabels
				// "1" when added, "" when removed
				if item.Body == "1" {
					ev.Added = []string{item.Label.Name}
				} else {
					ev.Removed = []string{item.Label.Name}
				}
			default:
				continue
			}
			issue.Events = append(issue.Events, ev)
		}
		if len(items) < pageSize {
			break
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
	// Gitea finds users by login, not id
	var res struct {
		Data []apiUser `json:"data"`
	}
	q := url.Values{"uid": {strconv.FormatInt(n, 10)}}
	if err := r.do(ctx, r.reader, http.MethodGet, "/users/search?"+q.Encode(), nil, &res); err != nil {
		return core.User{}, err
	}
	if len(res.Data) == 0 {
		return core.User{}, fmt.Errorf("no Gitea user %s", id)
	}
	return res.Data[0].user(), nil
}

func (r *remote) CreateIssue(ctx context.Context, as core.Account, title, body string) (core.Ref, error) {
	var gi apiIssue
	err := r.write(ctx, as, http.MethodPost, r.repoPath("/issues"), map[string]any{"title": title, "body": body}, &gi)
	if err != nil {
		return core.Ref{}, err
	}
	return issueRef(gi), nil
}

func (r *remote) AddComment(ctx context.Context, as core.Account, issue core.Ref, body string) (core.Ref, error) {
	var c apiTimeline
	err := r.write(ctx, as, http.MethodPost, r.repoPath("/issues/"+issue.Number+"/comments"), map[string]any{"body": body}, &c)
	if err != nil {
		return core.Ref{}, r.confirm(ctx, err, issue.Number, "")
	}
	return core.Ref{ID: strconv.FormatInt(c.ID, 10), URL: c.HTMLURL}, nil
}

func (r *remote) EditText(ctx context.Context, as core.Account, issue, target core.Ref, body string) error {
	if target.ID == issue.ID {
		err := r.write(ctx, as, http.MethodPatch, r.repoPath("/issues/"+issue.Number), map[string]any{"body": body}, nil)
		return r.confirm(ctx, err, issue.Number, "")
	}
	err := r.write(ctx, as, http.MethodPatch, r.repoPath("/issues/comments/"+target.ID), map[string]any{"body": body}, nil)
	return r.confirm(ctx, err, issue.Number, target.ID)
}

func (r *remote) SetStatus(ctx context.Context, as core.Account, issue core.Ref, status common.Status) error {
	state := "open"
	if status == common.ClosedStatus {
		state = "closed"
	}
	err := r.write(ctx, as, http.MethodPatch, r.repoPath("/issues/"+issue.Number), map[string]any{"state": state}, nil)
	return r.confirm(ctx, err, issue.Number, "")
}

func (r *remote) SetTitle(ctx context.Context, as core.Account, issue core.Ref, title string) error {
	err := r.write(ctx, as, http.MethodPatch, r.repoPath("/issues/"+issue.Number), map[string]any{"title": title}, nil)
	return r.confirm(ctx, err, issue.Number, "")
}

// ChangeLabels adds labels by id, creating the missing ones: Gitea silently
// ignores the names of labels that don't exist. Then it removes labels one
// by one, as Gitea removes them.
func (r *remote) ChangeLabels(ctx context.Context, as core.Account, issue core.Ref, added, removed []string) error {
	ids := make([]int64, 0, len(added))
	for _, l := range added {
		id, err := r.labelID(ctx, as, l, true)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if len(ids) > 0 {
		err := r.write(ctx, as, http.MethodPost, r.repoPath("/issues/"+issue.Number+"/labels"), map[string]any{"labels": ids}, nil)
		if err != nil {
			return r.confirm(ctx, err, issue.Number, "")
		}
	}
	for _, l := range removed {
		id, err := r.labelID(ctx, as, l, false)
		if err != nil {
			return err
		}
		if id == 0 {
			// not a label of the repository: on no issue
			continue
		}
		path := r.repoPath(fmt.Sprintf("/issues/%s/labels/%d", issue.Number, id))
		if err := r.write(ctx, as, http.MethodDelete, path, nil, nil); err != nil {
			return r.confirm(ctx, err, issue.Number, "")
		}
	}
	return nil
}

// confirm checks with the reader that an issue (and a comment, if any) a
// write didn't find is gone: Gitea answers the same 404 to a token that
// can't see the repository. The issue still there, the write was refused.
func (r *remote) confirm(ctx context.Context, err error, number, commentID string) error {
	if !errors.Is(err, core.ErrGone) {
		return err
	}
	var ignored any
	if rerr := r.do(ctx, r.reader, http.MethodGet, r.repoPath(""), nil, &ignored); rerr != nil {
		return fmt.Errorf("checking the repository after %w: %w", err, rerr)
	}
	path := r.repoPath("/issues/" + number)
	gerr := r.do(ctx, r.reader, http.MethodGet, path, nil, &ignored)
	if gerr == nil && commentID != "" {
		gerr = r.do(ctx, r.reader, http.MethodGet, r.repoPath("/issues/comments/"+commentID), nil, &ignored)
	}
	switch {
	case errors.Is(gerr, core.ErrGone):
		return err
	case gerr != nil:
		return gerr
	}
	return fmt.Errorf("%w: the token can't see it: %w", core.ErrRefused, err)
}

// labelID returns the id of a label of the repository, created if needed and
// create, else 0.
func (r *remote) labelID(ctx context.Context, as core.Account, name string, create bool) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// read on a miss: the label may have been made on Gitea since the labels
	// were read, as an engine may live long.
	// ponytail: one read of every label per missing one; a new label is a
	// write anyway.
	if _, ok := r.labels[strings.ToLower(name)]; !ok {
		labels := map[string]int64{}
		for page := 1; ; page++ {
			var res []apiLabel
			q := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(pageSize)}}
			if err := r.do(ctx, r.reader, http.MethodGet, r.repoPath("/labels?"+q.Encode()), nil, &res); err != nil {
				return 0, err
			}
			for _, l := range res {
				labels[strings.ToLower(l.Name)] = l.ID
			}
			if len(res) < pageSize {
				break
			}
		}
		r.labels = labels
	}
	if id, ok := r.labels[strings.ToLower(name)]; ok || !create {
		return id, nil
	}
	rgba := common.Label(name).Color().RGBA()
	var created apiLabel
	body := map[string]any{"name": name, "color": fmt.Sprintf("#%.2x%.2x%.2x", rgba.R, rgba.G, rgba.B)}
	if err := r.write(ctx, as, http.MethodPost, r.repoPath("/labels"), body, &created); err != nil {
		return 0, fmt.Errorf("creating label %s: %w", name, err)
	}
	r.labels[strings.ToLower(name)] = created.ID
	return created.ID, nil
}

func (r *remote) write(ctx context.Context, as core.Account, method, path string, body, res any) error {
	token, ok := r.tokens[as.ID]
	if !ok {
		return fmt.Errorf("no Gitea token for %s", as.Login)
	}
	return r.do(ctx, token, method, path, body, res)
}

// api is a client of the Gitea API.
type api struct {
	http *http.Client
	base string // the API's URL
}

// do calls the Gitea API, and turns its answers into the engine's errors.
func (a api) do(ctx context.Context, token, method, path string, body, res any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s %s", core.ErrRefused, method, path)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s %s", core.ErrGone, method, path)
	case resp.StatusCode == http.StatusTooManyRequests:
		wait := time.Minute
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			wait = time.Duration(s) * time.Second
		}
		return core.ErrRateLimited{Until: time.Now().Add(wait)}
	case resp.StatusCode >= 300:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("gitea: %s %s: %s: %s", method, path, resp.Status, msg)
	}
	if res == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(res)
}

func (r *remote) Dialect() core.Dialect {
	return core.Dialect{
		// "!12" is a pull request
		Ref:       regexp.MustCompile(`(?:^|[^\w/#!])(#(\d+))\b`),
		FormatRef: func(n string) string { return "#" + n },
	}
}

// Legacy reads nothing: the Gitea bridge was born with the sync engine.
func (r *remote) Legacy(_, _ map[string]string) (string, bool) { return "", false }
func (r *remote) LegacyUser() (string, bool)                   { return "", false }
