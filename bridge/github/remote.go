package github

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
	neturl "net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/common"
)

var _ core.RemoteBridge = &Github{}

// NewRemote returns the GitHub repository of the bridge, for the sync engine,
// acting with every GitHub token.
func (g *Github) NewRemote(ctx context.Context, repo *cache.RepoCache, conf core.Configuration) (core.Remote, error) {
	creds, err := auth.List(repo, auth.WithTarget(target), auth.WithKind(auth.KindToken))
	if err != nil {
		return nil, err
	}
	tokens := make([]*auth.Token, 0, len(creds))
	for _, cred := range creds {
		tokens = append(tokens, cred.(*auth.Token))
	}
	return newRemote(ctx, conf[confKeyOwner], conf[confKeyProject], conf[confKeyDefaultLogin], tokens, graphqlClient)
}

func graphqlClient(token *auth.Token) Client {
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token.Value})
	return githubv4.NewClient(oauth2.NewClient(context.TODO(), src))
}

var _ core.Remote = &remote{}

// remote is a GitHub repository, as the sync engine sees it. GitHub logins
// stand for user ids: the API this adapter uses doesn't expose ids.
type remote struct {
	owner, project string

	accounts []core.Account
	clients  map[string]*rateLimitHandlerClient // by login
	reader   *rateLimitHandlerClient
	// createLabel creates a label and returns its id: GitHub creates labels
	// with its REST API only.
	createLabel func(ctx context.Context, name, color string) (string, error)

	mu     sync.Mutex
	repoID string
	labels map[string]string // id by lowercased name
}

func newRemote(ctx context.Context, owner, project, defaultLogin string, tokens []*auth.Token, newClient func(*auth.Token) Client) (*remote, error) {
	r := &remote{owner: owner, project: project, clients: map[string]*rateLimitHandlerClient{}}
	var readerToken *auth.Token
	for _, token := range tokens {
		c := &rateLimitHandlerClient{sc: newClient(token)}
		var login loginQuery
		if err := c.queryPrintMsgs(ctx, &login, nil); err != nil {
			return nil, fmt.Errorf("token %s: %w", token.ID().Human(), err)
		}
		if _, ok := r.clients[login.Viewer.Login]; ok {
			continue
		}
		u, err := r.queryUser(ctx, c, login.Viewer.Login)
		if err != nil {
			return nil, err
		}
		r.clients[login.Viewer.Login] = c
		r.accounts = append(r.accounts, core.Account{User: u})
		if r.reader == nil || u.Login == defaultLogin {
			r.reader, readerToken = c, token
		}
	}
	if r.reader == nil {
		return nil, fmt.Errorf("no GitHub token")
	}
	r.createLabel = func(ctx context.Context, name, color string) (string, error) {
		return createGithubLabel(ctx, readerToken, owner, project, name, color)
	}
	return r, nil
}

func (r *remote) Key() string      { return "github.com/" + strings.ToLower(r.owner+"/"+r.project) }
func (r *remote) Instance() string { return "github.com" }
func (r *remote) Kind() string     { return target }

func (r *remote) Accounts(context.Context) ([]core.Account, error) {
	return slices.Clone(r.accounts), nil
}

func (r *remote) Issues(ctx context.Context, since time.Time) iter.Seq2[*core.Issue, error] {
	return func(yield func(*core.Issue, error) bool) {
		// stops the mediator if the caller stops early
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		build(NewImportMediator(ctx, r.reader, r.owner, r.project, since), yield)
	}
}

func (r *remote) Fetch(ctx context.Context, ref core.Ref) (*core.Issue, error) {
	number, err := strconv.Atoi(cmp.Or(ref.Number, trailingNumber.FindString(ref.URL)))
	if err != nil {
		return nil, fmt.Errorf("issue %s: no number to fetch it by", cmp.Or(ref.URL, ref.ID))
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var issue *core.Issue
	build(newIssueMediator(ctx, r.reader, r.owner, r.project, number), func(i *core.Issue, e error) bool {
		issue, err = i, e
		return false
	})
	switch {
	case err != nil:
		return nil, mapErr(err)
	case issue == nil:
		return nil, fmt.Errorf("%w: issue #%d", core.ErrGone, number)
	}
	return issue, nil
}

var trailingNumber = regexp.MustCompile(`\d+$`)

// build yields the issues the mediator reads, each with its edits and
// timeline.
func build(mm *importMediator, yield func(*core.Issue, error) bool) {
	var cur *issueBuilder
	for {
		ev := mm.NextImportEvent()
		if ev == nil {
			break
		}
		switch ev := ev.(type) {
		case IssueEvent:
			// a failure reading the previous issue leaves it partial: the
			// mediator records it before going on with this one
			if err := mm.Error(); err != nil {
				yield(nil, err)
				return
			}
			if cur != nil && !yield(cur.finish(), nil) {
				return
			}
			cur = newIssueBuilder(ev.issue)
		case IssueEditEvent:
			cur.edit("", ev.userContentEdit)
		case TimelineEvent:
			cur.timeline(ev.timelineItem)
		case CommentEditEvent:
			cur.edit(fmt.Sprint(ev.commentId), ev.userContentEdit)
		}
	}
	if err := mm.Error(); err != nil {
		yield(nil, err)
		return
	}
	if cur != nil {
		yield(cur.finish(), nil)
	}
}

// issueBuilder turns the events of the import mediator into an issue.
type issueBuilder struct {
	issue   *core.Issue
	edited  map[string]bool // targets whose creation edit was seen
	renamed bool
}

func newIssueBuilder(i issue) *issueBuilder {
	return &issueBuilder{
		issue: &core.Issue{
			Ref:           core.Ref{ID: fmt.Sprint(i.Id), Number: fmt.Sprint(i.Number), URL: i.Url.String()},
			Author:        actorUser(i.Author),
			Created:       i.CreatedAt.Time,
			Updated:       i.CreatedAt.Time,
			OriginalTitle: string(i.Title),
			Title:         string(i.Title),
			OriginalBody:  string(i.Body),
			Body:          string(i.Body),
			Status:        common.OpenStatus,
		},
		edited: map[string]bool{},
	}
}

// edit records an edit of the body (target "") or of a comment. GitHub lists
// the creation as the first edit of a text that has been edited.
func (b *issueBuilder) edit(target string, e userContentEdit) {
	if e.Diff == nil || e.DeletedAt != nil {
		return
	}
	text := string(*e.Diff)
	if !b.edited[target] {
		b.edited[target] = true
		if target == "" {
			b.issue.OriginalBody = text
			return
		}
		for i := range b.issue.Comments {
			if b.issue.Comments[i].ID == target {
				b.issue.Comments[i].Original = text
			}
		}
		return
	}
	b.event(core.Event{
		ID: fmt.Sprint(e.Id), Kind: core.EventTextEdit, Author: actorUser(e.Editor),
		Time: e.CreatedAt.Time, Target: target, Text: text,
	})
}

func (b *issueBuilder) timeline(item timelineItem) {
	switch item.Typename {
	case "IssueComment":
		c := item.IssueComment
		b.issue.Comments = append(b.issue.Comments, core.Comment{
			Ref:      core.Ref{ID: fmt.Sprint(c.Id), URL: c.Url.String()},
			Author:   actorUser(c.Author),
			Created:  c.CreatedAt.Time,
			Updated:  c.CreatedAt.Time,
			Original: string(c.Body),
			Body:     string(c.Body),
		})
		b.touch(c.CreatedAt.Time)
	case "LabeledEvent":
		e := item.LabeledEvent
		b.event(core.Event{ID: fmt.Sprint(e.Id), Kind: core.EventLabels, Author: actorUser(e.Actor), Time: e.CreatedAt.Time, Added: []string{string(e.Label.Name)}})
		if !slices.Contains(b.issue.Labels, string(e.Label.Name)) {
			b.issue.Labels = append(b.issue.Labels, string(e.Label.Name))
		}
	case "UnlabeledEvent":
		e := item.UnlabeledEvent
		b.event(core.Event{ID: fmt.Sprint(e.Id), Kind: core.EventLabels, Author: actorUser(e.Actor), Time: e.CreatedAt.Time, Removed: []string{string(e.Label.Name)}})
		b.issue.Labels = slices.DeleteFunc(b.issue.Labels, func(l string) bool { return l == string(e.Label.Name) })
	case "ClosedEvent":
		e := item.ClosedEvent
		b.event(core.Event{ID: fmt.Sprint(e.Id), Kind: core.EventStatus, Author: actorUser(e.Actor), Time: e.CreatedAt.Time, Status: common.ClosedStatus})
		b.issue.Status = common.ClosedStatus
	case "ReopenedEvent":
		e := item.ReopenedEvent
		b.event(core.Event{ID: fmt.Sprint(e.Id), Kind: core.EventStatus, Author: actorUser(e.Actor), Time: e.CreatedAt.Time, Status: common.OpenStatus})
		b.issue.Status = common.OpenStatus
	case "RenamedTitleEvent":
		e := item.RenamedTitleEvent
		if !b.renamed {
			b.renamed = true
			b.issue.OriginalTitle = string(e.PreviousTitle)
		}
		b.event(core.Event{ID: fmt.Sprint(e.Id), Kind: core.EventTitle, Author: actorUser(e.Actor), Time: e.CreatedAt.Time, Title: string(e.CurrentTitle)})
	}
}

func (b *issueBuilder) event(e core.Event) {
	b.issue.Events = append(b.issue.Events, e)
	b.touch(e.Time)
	if e.Kind == core.EventTextEdit && e.Target != "" {
		for i := range b.issue.Comments {
			if b.issue.Comments[i].ID == e.Target && e.Time.After(b.issue.Comments[i].Updated) {
				b.issue.Comments[i].Updated = e.Time
			}
		}
	}
}

func (b *issueBuilder) touch(t time.Time) {
	if t.After(b.issue.Updated) {
		b.issue.Updated = t
	}
}

func (b *issueBuilder) finish() *core.Issue {
	slices.Sort(b.issue.Labels)
	slices.SortStableFunc(b.issue.Events, func(x, y core.Event) int { return x.Time.Compare(y.Time) })
	return b.issue
}

// actorUser returns a GitHub actor, the "ghost" user for a deleted one.
func actorUser(a *actor) core.User {
	if a == nil {
		return core.User{ID: "ghost", Login: "ghost", Name: "Deleted user"}
	}
	u := core.User{ID: string(a.Login), Login: string(a.Login), AvatarURL: string(a.AvatarUrl), Email: string(a.User.Email)}
	if a.User.Name != nil {
		u.Name = string(*a.User.Name)
	}
	return u
}

func (r *remote) User(ctx context.Context, id string) (core.User, error) {
	return r.queryUser(ctx, r.reader, id)
}

func (r *remote) queryUser(ctx context.Context, c *rateLimitHandlerClient, login string) (core.User, error) {
	var q userQuery
	if err := c.queryPrintMsgs(ctx, &q, varmap{"login": githubv4.String(login)}); err != nil {
		return core.User{}, err
	}
	u := core.User{ID: login, Login: login, AvatarURL: string(q.User.AvatarUrl)}
	if q.User.Name != nil {
		u.Name = string(*q.User.Name)
	}
	return u, nil
}

func (r *remote) CreateIssue(ctx context.Context, as core.Account, title, body string) (core.Ref, error) {
	repoID, err := r.repositoryID(ctx)
	if err != nil {
		return core.Ref{}, err
	}
	var m createIssueMutation
	input := githubv4.CreateIssueInput{
		RepositoryID: repoID,
		Title:        githubv4.String(title),
		Body:         githubv4.NewString(githubv4.String(body)),
	}
	if err := r.mutate(ctx, as, &m, input); err != nil {
		return core.Ref{}, err
	}
	url := m.CreateIssue.Issue.URL
	return core.Ref{ID: m.CreateIssue.Issue.ID, URL: url, Number: url[strings.LastIndex(url, "/")+1:]}, nil
}

func (r *remote) AddComment(ctx context.Context, as core.Account, issue core.Ref, body string) (core.Ref, error) {
	var m addCommentToIssueMutation
	input := githubv4.AddCommentInput{SubjectID: issue.ID, Body: githubv4.String(body)}
	if err := r.mutate(ctx, as, &m, input); err != nil {
		return core.Ref{}, r.confirm(ctx, err, issue.ID)
	}
	node := m.AddComment.CommentEdge.Node
	return core.Ref{ID: node.ID, URL: node.URL}, nil
}

func (r *remote) EditText(ctx context.Context, as core.Account, issue, target core.Ref, body string) error {
	if target.ID == issue.ID {
		b := githubv4.String(body)
		return r.update(ctx, as, githubv4.UpdateIssueInput{ID: issue.ID, Body: &b})
	}
	var m updateIssueCommentMutation
	err := r.mutate(ctx, as, &m, githubv4.UpdateIssueCommentInput{ID: target.ID, Body: githubv4.String(body)})
	return r.confirm(ctx, err, target.ID)
}

func (r *remote) SetStatus(ctx context.Context, as core.Account, issue core.Ref, status common.Status) error {
	state := githubv4.IssueStateOpen
	if status == common.ClosedStatus {
		state = githubv4.IssueStateClosed
	}
	return r.update(ctx, as, githubv4.UpdateIssueInput{ID: issue.ID, State: &state})
}

func (r *remote) SetTitle(ctx context.Context, as core.Account, issue core.Ref, title string) error {
	t := githubv4.String(title)
	return r.update(ctx, as, githubv4.UpdateIssueInput{ID: issue.ID, Title: &t})
}

func (r *remote) SetLabels(ctx context.Context, as core.Account, issue core.Ref, labels []string) error {
	ids := make([]githubv4.ID, 0, len(labels))
	for _, l := range labels {
		id, err := r.labelID(ctx, l)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	return r.update(ctx, as, githubv4.UpdateIssueInput{ID: issue.ID, LabelIDs: &ids})
}

func (r *remote) update(ctx context.Context, as core.Account, input githubv4.UpdateIssueInput) error {
	var m updateIssueMutation
	return r.confirm(ctx, r.mutate(ctx, as, &m, input), input.ID)
}

// confirm checks with the reader that an issue or comment a write didn't
// find is gone: GitHub resolves nothing either for a token that can't see the
// repository. Still there, the write was refused.
func (r *remote) confirm(ctx context.Context, err error, id githubv4.ID) error {
	if !errors.Is(err, core.ErrGone) {
		return err
	}
	var repo repositoryIDQuery
	vars := varmap{"owner": githubv4.String(r.owner), "name": githubv4.String(r.project)}
	if rerr := r.reader.queryPrintMsgs(ctx, &repo, vars); rerr != nil {
		return fmt.Errorf("checking the repository after %w: %w", err, rerr)
	}
	var q nodeQuery
	nerr := mapErr(r.reader.callAPIDealWithLimit(ctx, func(ctx context.Context) error {
		return r.reader.sc.Query(ctx, &q, varmap{"id": id})
	}, func(string) {}))
	switch {
	case errors.Is(nerr, core.ErrGone), nerr == nil && q.Node == nil:
		return err
	case nerr != nil:
		return nerr
	}
	return fmt.Errorf("%w: the token can't see it: %w", core.ErrRefused, err)
}

// mutate writes as an account. It waits out rate limits, but doesn't retry
// on errors, as GitHub may have done a write whose answer was lost: the
// engine recovers from that without writing twice.
// rateLimitWait is how long the engine waits when GitHub limits writes. Its
// secondary limit gives no reset time; GitHub asks to wait at least a minute.
var rateLimitWait = time.Minute

func (r *remote) mutate(ctx context.Context, as core.Account, m interface{}, input githubv4.Input) error {
	c, ok := r.clients[as.Login]
	if !ok {
		return fmt.Errorf("no GitHub token for %s", as.Login)
	}
	// the engine waits when GitHub limits writes
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	return mapErr(c.sc.Mutate(ctx, m, input, nil))
}

// nodeQuery finds an issue or a comment by id.
type nodeQuery struct {
	Node *struct {
		Typename githubv4.String `graphql:"__typename"`
	} `graphql:"node(id: $id)"`
}

type repositoryIDQuery struct {
	Repository struct {
		ID string `graphql:"id"`
	} `graphql:"repository(owner: $owner, name: $name)"`
}

func (r *remote) repositoryID(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.repoID != "" {
		return r.repoID, nil
	}
	var q repositoryIDQuery
	vars := varmap{"owner": githubv4.String(r.owner), "name": githubv4.String(r.project)}
	if err := r.reader.queryPrintMsgs(ctx, &q, vars); err != nil {
		return "", err
	}
	r.repoID = q.Repository.ID
	return r.repoID, nil
}

// labelID returns the id of a label of the repository, created if needed.
func (r *remote) labelID(ctx context.Context, name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.labels == nil {
		r.labels = map[string]string{}
		vars := varmap{
			"owner": githubv4.String(r.owner),
			"name":  githubv4.String(r.project),
			"first": githubv4.Int(100),
			"after": (*githubv4.String)(nil),
		}
		for {
			var q labelsQuery
			if err := r.reader.queryPrintMsgs(ctx, &q, vars); err != nil {
				r.labels = nil
				return "", err
			}
			for _, l := range q.Repository.Labels.Nodes {
				r.labels[strings.ToLower(l.Name)] = l.ID
			}
			if !q.Repository.Labels.PageInfo.HasNextPage {
				break
			}
			vars["after"] = q.Repository.Labels.PageInfo.EndCursor
		}
	}
	if id, ok := r.labels[strings.ToLower(name)]; ok {
		return id, nil
	}
	rgba := common.Label(name).Color().RGBA()
	id, err := r.createLabel(ctx, name, fmt.Sprintf("%.2x%.2x%.2x", rgba.R, rgba.G, rgba.B))
	if err != nil {
		return "", fmt.Errorf("creating label %s: %w", name, err)
	}
	r.labels[strings.ToLower(name)] = id
	return id, nil
}

func (r *remote) Dialect() core.Dialect {
	return core.Dialect{
		Ref:       regexp.MustCompile(`(?:^|[^\w/#])(#(\d+))\b`),
		FormatRef: func(n string) string { return "#" + n },
	}
}

// Legacy reads the metadata of the GitHub bridge before the sync engine: a
// github-id on operations of a bug of this repository.
func (r *remote) Legacy(meta, createMeta map[string]string) (string, bool) {
	id, ok := meta[metaKeyGithubId]
	prefix := fmt.Sprintf("https://github.com/%s/%s/issues/", r.owner, r.project)
	if !ok || !strings.HasPrefix(strings.ToLower(createMeta[metaKeyGithubUrl]), strings.ToLower(prefix)) {
		return "", false
	}
	return id, true
}

func (r *remote) LegacyUser() (string, bool) {
	return metaKeyGithubLogin, true
}

// mapErr turns GitHub's answers into the engine's errors.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "Could not resolve to a node"), strings.Contains(msg, "Could not resolve to an Issue"),
		strings.Contains(msg, "NOT_FOUND"):
		return fmt.Errorf("%w: %w", core.ErrGone, err)
	case strings.Contains(msg, "FORBIDDEN"), strings.Contains(msg, "Resource not accessible"),
		strings.Contains(msg, "does not have permission"):
		return fmt.Errorf("%w: %w", core.ErrRefused, err)
	case strings.Contains(msg, "rate limit"), strings.Contains(msg, "was submitted too quickly"):
		return core.ErrRateLimited{Until: time.Now().Add(rateLimitWait)}
	}
	return err
}

// createGithubLabel creates a label with the REST API, as the GraphQL one
// can't, and returns its id. An existing label of that name is returned.
func createGithubLabel(ctx context.Context, token *auth.Token, owner, project, label, color string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/labels", githubV3Url, owner, project)
	data, err := json.Marshal(map[string]string{"name": label, "color": color})
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", fmt.Sprintf("token %s", token.Value))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 422: it exists, for example created asynchronously by GitHub
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return getGithubLabel(ctx, token, owner, project, label)
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("creating label: response status %v", resp.StatusCode)
	}
	return decodeGithubLabelID(resp.Body)
}

func getGithubLabel(ctx context.Context, token *auth.Token, owner, project, label string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/labels/%s", githubV3Url, owner, project, neturl.PathEscape(label))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", fmt.Sprintf("token %s", token.Value))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("getting label: response status %v", resp.StatusCode)
	}
	return decodeGithubLabelID(resp.Body)
}

func decodeGithubLabelID(body io.Reader) (string, error) {
	var aux struct {
		NodeID string `json:"node_id"`
	}
	if err := json.NewDecoder(body).Decode(&aux); err != nil {
		return "", err
	}
	if aux.NodeID == "" {
		return "", errors.New("no label id in the answer")
	}
	return aux.NodeID, nil
}
