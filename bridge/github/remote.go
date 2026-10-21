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
	"maps"
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
	r, err := newRemote(ctx, conf[confKeyOwner], conf[confKeyProject], conf[confKeyDefaultLogin], tokens, graphqlClient)
	if err != nil {
		return nil, err
	}
	r.kinds = conf[core.ConfigKeyKinds] != core.ModeLabels
	if r.kindTypes, err = parseKindTypes(conf[core.ConfigKeyKindTypes]); err != nil {
		return nil, err
	}
	return r, nil
}

// parseKindTypes reads kind-types, the labels of issue types, as
// label=Type,...: a type has one label, and a label one type. It returns the
// labels by lowercased type name, nil for none.
func parseKindTypes(v string) (map[string]string, error) {
	var res map[string]string
	labelOf := map[string]string{} // type by label, lowercased
	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		label, name, ok := strings.Cut(entry, "=")
		label, name = strings.TrimSpace(label), strings.TrimSpace(name)
		if !ok || label == "" || name == "" {
			return nil, fmt.Errorf("kind-types: %q is not label=Type", entry)
		}
		key := strings.ToLower(name)
		if _, ok := res[key]; ok {
			return nil, fmt.Errorf("kind-types: %q gives type %s a second label", entry, name)
		}
		if _, ok := labelOf[strings.ToLower(label)]; ok {
			return nil, fmt.Errorf("kind-types: %q gives label %s a second type", entry, label)
		}
		if res == nil {
			res = map[string]string{}
		}
		res[key], labelOf[strings.ToLower(label)] = label, name
	}
	return res, nil
}

func graphqlClient(token *auth.Token) Client {
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token.Value})
	hc := oauth2.NewClient(context.TODO(), src)
	return apiClient{Client: githubv4.NewClient(hc), http: hc}
}

var _ core.AssigneeRemote = &remote{}

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
	// kinds holds the issue's kind as its GitHub type (native), not as a
	// label (core.ModeLabels).
	kinds bool
	// kindTypes are the labels of types by lowercased name: kindLabel's.
	kindTypes map[string]string

	mu     sync.Mutex
	repoID string
	labels map[string]string // id by lowercased name
	// types are the ids of the enabled issue types by label, matched exactly;
	// nil for a user's repository, which has none. typesRead tells readTypes
	// ran.
	types     map[string]string
	typesRead bool
}

func newRemote(ctx context.Context, owner, project, defaultLogin string, tokens []*auth.Token, newClient func(*auth.Token) Client) (*remote, error) {
	r := &remote{owner: owner, project: project, kinds: true, clients: map[string]*rateLimitHandlerClient{}}
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
		if err := r.readKinds(ctx); err != nil {
			yield(nil, err)
			return
		}
		build(NewImportMediator(ctx, r.reader, r.owner, r.project, since), r, yield)
	}
}

func (r *remote) Fetch(ctx context.Context, ref core.Ref) (*core.Issue, error) {
	number, err := strconv.Atoi(cmp.Or(ref.Number, trailingNumber.FindString(ref.URL)))
	if err != nil {
		return nil, fmt.Errorf("issue %s: no number to fetch it by", cmp.Or(ref.URL, ref.ID))
	}
	if err := r.readKinds(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var issue *core.Issue
	build(newIssueMediator(ctx, r.reader, r.owner, r.project, number), r, func(i *core.Issue, e error) bool {
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
func build(mm *importMediator, r *remote, yield func(*core.Issue, error) bool) {
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
			cur.r = r
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
	// GitHub's labels: with its type, the issue's labels
	labels []string
	// r holds the bridge's configuration
	r *remote
	// issueType is the name of the issue's type, "" for none
	issueType string
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
		b.relabel(e.actorEvent, func() { b.labels = append(b.labels, string(e.Label.Name)) })
	case "UnlabeledEvent":
		e := item.UnlabeledEvent
		b.relabel(e.actorEvent, func() {
			b.labels = slices.DeleteFunc(b.labels, func(l string) bool { return l == string(e.Label.Name) })
		})
	case "AssignedEvent":
		e := item.AssignedEvent
		if login := string(e.Assignee.Actor.Login); login != "" {
			u := core.User{ID: login, Login: login}
			b.issue.Assignees = append(b.issue.Assignees, u)
			b.event(core.Event{ID: fmt.Sprint(e.Id), Kind: core.EventAssignees, Author: actorUser(e.Actor), Time: e.CreatedAt.Time, Assigned: []core.User{u}})
		}
	case "UnassignedEvent":
		e := item.UnassignedEvent
		if login := string(e.Assignee.Actor.Login); login != "" {
			u := core.User{ID: login, Login: login}
			b.issue.Assignees = slices.DeleteFunc(b.issue.Assignees, func(a core.User) bool { return a == u })
			b.event(core.Event{ID: fmt.Sprint(e.Id), Kind: core.EventAssignees, Author: actorUser(e.Actor), Time: e.CreatedAt.Time, Unassigned: []core.User{u}})
		}
	case "IssueTypeAddedEvent", "IssueTypeChangedEvent", "IssueTypeRemovedEvent":
		e := item.IssueTypeAddedEvent
		switch item.Typename {
		case "IssueTypeChangedEvent":
			e = item.IssueTypeChangedEvent
		case "IssueTypeRemovedEvent":
			e = item.IssueTypeRemovedEvent
		}
		name := ""
		if e.IssueType != nil && item.Typename != "IssueTypeRemovedEvent" {
			name = string(e.IssueType.Name)
		}
		b.relabel(e.actorEvent, func() { b.issueType = name })
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

// issueLabels are GitHub's labels, and its type as a kind label. Its
// assignees are the engine's to make labels.
func (b *issueBuilder) issueLabels() []string {
	labels := slices.Clone(b.labels)
	if b.issueType != "" {
		labels = append(labels, b.r.kindLabel(b.issueType))
	}
	slices.Sort(labels)
	return slices.Compact(labels)
}

// relabel makes a change of GitHub's labels or type an event, if it changes
// the issue's labels: typing an issue labeled the old way doesn't.
func (b *issueBuilder) relabel(e actorEvent, change func()) {
	was := b.issueLabels()
	change()
	now := b.issueLabels()
	ev := core.Event{ID: fmt.Sprint(e.Id), Kind: core.EventLabels, Author: actorUser(e.Actor), Time: e.CreatedAt.Time}
	for _, l := range now {
		if !slices.Contains(was, l) {
			ev.Added = append(ev.Added, l)
		}
	}
	for _, l := range was {
		if !slices.Contains(now, l) {
			ev.Removed = append(ev.Removed, l)
		}
	}
	if ev.Added != nil || ev.Removed != nil {
		b.event(ev)
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
	b.issue.Labels = b.issueLabels()
	// the kind GitHub holds the other way: as a label of a type, when the
	// issue has none or that one's, or as the type with kinds = labels
	if b.issueType != "" && !b.r.kinds {
		b.issue.Resend = append(b.issue.Resend, b.r.kindLabel(b.issueType))
	}
	for _, l := range b.issue.Labels {
		if _, ok := b.r.typeID(l); ok && b.r.kinds && slices.Contains(b.labels, l) &&
			(b.issueType == "" || b.r.kindLabel(b.issueType) == l) {
			b.issue.Resend = append(b.issue.Resend, l)
		}
	}
	slices.SortStableFunc(b.issue.Events, func(x, y core.Event) int { return x.Time.Compare(y.Time) })
	return b.issue
}

// kindLabel is the label of an issue type: kind-types', else kind:<name>.
func (r *remote) kindLabel(typeName string) string {
	if l, ok := r.kindTypes[strings.ToLower(typeName)]; ok {
		return l
	}
	return "kind:" + core.Slug(typeName)
}

// readKinds reads the issue types before a pull, as ones may have been made
// since the last.
func (r *remote) readKinds(ctx context.Context) error {
	if !r.kinds {
		return nil
	}
	return r.readTypes(ctx)
}

// readTypes reads the enabled issue types of the repository's organization.
// A user's repository has none.
func (r *remote) readTypes(ctx context.Context) error {
	var q issueTypesQuery
	vars := varmap{"owner": githubv4.String(r.owner), "name": githubv4.String(r.project)}
	if err := r.reader.queryPrintMsgs(ctx, &q, vars); err != nil {
		return err
	}
	var types map[string]string
	if q.Repository.IssueTypes != nil {
		// an organization's, even with none enabled
		types = map[string]string{}
		for _, t := range q.Repository.IssueTypes.Nodes {
			if t.IsEnabled {
				types[r.kindLabel(t.Name)] = t.ID
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.types, r.typesRead = types, true
	return nil
}

// typeID returns the id of the enabled type a label stands for, matched
// exactly: kind:Bug is no type's.
func (r *remote) typeID(label string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.types[label]
	return id, ok
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

// ChangeLabels adds labels, creating the missing ones, then removes labels.
// A kind is the issue's type, set first, so that its old way's label comes
// off once it is. With kinds = labels, a type is cleared once its label is
// added: the issue keeps its kind throughout, which makes no event. The
// kinds GitHub refuses are RefusedLabels; the other labels go through.
func (r *remote) ChangeLabels(ctx context.Context, as core.Account, issue core.Ref, added, removed []string) error {
	var refused core.RefusedLabels
	var err error
	if r.kinds {
		if added, removed, refused, err = r.changeType(ctx, as, issue, added, removed); err != nil {
			return err
		}
	}
	add, err := r.labelIDs(ctx, added, true)
	if err != nil {
		return err
	}
	if len(add) > 0 {
		var m addLabelsToLabelableMutation
		input := githubv4.AddLabelsToLabelableInput{LabelableID: issue.ID, LabelIDs: add}
		if err := r.mutate(ctx, as, &m, input); err != nil {
			return r.confirm(ctx, err, issue.ID)
		}
	}
	if !r.kinds {
		// a refused type keeps its label, already added
		if _, removed, refused, err = r.changeType(ctx, as, issue, added, removed); err != nil {
			return err
		}
	}
	// ponytail: an assignee's label missing from the repository, as most are,
	// reads every label again; cache the misses if pushes get slow
	remove, err := r.labelIDs(ctx, removed, false)
	if err != nil {
		return err
	}
	if len(remove) > 0 {
		var m removeLabelsFromLabelableMutation
		input := githubv4.RemoveLabelsFromLabelableInput{LabelableID: issue.ID, LabelIDs: remove}
		if err := r.confirm(ctx, r.mutate(ctx, as, &m, input), issue.ID); err != nil {
			return err
		}
	}
	if refused != nil {
		return refused
	}
	return nil
}

// Assign unassigns, then assigns, and reads the assignees back: GitHub
// silently leaves out those it can't assign, like someone without access.
// A login without a user, like a bot's (user(login:) finds no bot), is
// refused. GitHub logins stand for user ids.
func (r *remote) Assign(ctx context.Context, as core.Account, issue core.Ref, added, removed []core.User) error {
	var refused core.RefusedUsers
	for _, change := range []struct {
		users []core.User
		add   bool
	}{{removed, false}, {added, true}} {
		if len(change.users) == 0 {
			continue
		}
		assigned, unknown, err := r.assign(ctx, as, issue, change.users, change.add)
		if err != nil {
			return err
		}
		refused = append(refused, unknown...)
		for _, u := range change.users {
			has := slices.ContainsFunc(assigned, func(a string) bool { return strings.EqualFold(a, loginOf(u)) })
			if !slices.Contains(unknown, u) && has != change.add {
				refused = append(refused, u)
			}
		}
	}
	if refused != nil {
		return refused
	}
	return nil
}

// changeType sets the issue's type from the kinds added or removed. It
// returns the labels left to add and to remove, and the kinds GitHub refused
// to set or clear, left out of both. The issue has one type: another kind is
// a label, and the type's label held the old way comes off. A removed kind
// clears its type even disabled. With kinds = labels, a type whose label is
// added or removed is cleared.
func (r *remote) changeType(ctx context.Context, as core.Account, issue core.Ref, added, removed []string) (add, remove []string, refused core.RefusedLabels, err error) {
	// mayBeType tells a label that may be a type's, as types aren't read in
	// labels mode, and a disabled one has no id
	mayBeType := func(l string) bool {
		return strings.HasPrefix(l, "kind:") || slices.Contains(slices.Collect(maps.Values(r.kindTypes)), l)
	}
	hasType := mayBeType
	read := slices.ContainsFunc(added, mayBeType) || slices.ContainsFunc(removed, mayBeType)
	if r.kinds {
		r.mu.Lock()
		typesRead := r.typesRead
		r.mu.Unlock()
		if !typesRead {
			if err := r.readTypes(ctx); err != nil {
				return nil, nil, nil, err
			}
		}
		r.mu.Lock()
		org := r.types != nil
		r.mu.Unlock()
		hasType = func(l string) bool { _, ok := r.typeID(l); return ok }
		// a user's repository has no type to clear
		read = slices.ContainsFunc(added, hasType) || org && slices.ContainsFunc(removed, mayBeType)
	}
	if !read {
		return added, removed, nil, nil
	}

	var q issueTypeOfQuery
	// no retries: a deleted issue is an answer
	err = mapErr(r.reader.callAPIDealWithLimit(ctx, func(ctx context.Context) error {
		return r.reader.sc.Query(ctx, &q, varmap{"id": githubv4.ID(issue.ID)})
	}, func(string) {}))
	if err != nil {
		return nil, nil, nil, err
	}
	current := ""
	if t := q.Node.Issue.IssueType; t != nil {
		current = r.kindLabel(t.Name)
	}
	next, add, remove := current, added, removed
	if r.kinds {
		next, add, remove = core.OneNative(current, added, removed, hasType)
	} else if current != "" && (slices.Contains(added, current) || slices.Contains(removed, current)) {
		next = ""
	}
	if next == current {
		return add, remove, nil, nil
	}

	input := UpdateIssueIssueTypeInput{IssueID: githubv4.ID(issue.ID)}
	if next != "" {
		id, _ := r.typeID(next)
		input.IssueTypeID = githubv4.NewID(id)
	}
	var m updateIssueIssueTypeMutation
	err = r.confirm(ctx, r.mutate(ctx, as, &m, input), issue.ID)
	if errors.Is(err, core.ErrRefused) {
		// a replace is a set and a clear: both kinds are refused
		refused := core.RefusedLabels(slices.DeleteFunc([]string{next, current}, func(s string) bool { return s == "" }))
		is := func(s string) bool { return slices.Contains(refused, s) }
		return slices.DeleteFunc(slices.Clone(add), is), slices.DeleteFunc(slices.Clone(remove), is), refused, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	return add, remove, nil, nil
}

// loginOf is a user's login: their id when only it is known.
func loginOf(u core.User) string {
	return cmp.Or(u.Login, u.ID)
}

// assign adds users to the issue's assignees, or removes them, and returns
// its assignees then, and the users without a user on GitHub, left out.
func (r *remote) assign(ctx context.Context, as core.Account, issue core.Ref, users []core.User, add bool) (assigned []string, unknown []core.User, err error) {
	var ids []githubv4.ID
	for _, u := range users {
		var q userIDQuery
		// no retries: a login without a user is an answer
		err := mapErr(r.reader.callAPIDealWithLimit(ctx, func(ctx context.Context) error {
			return r.reader.sc.Query(ctx, &q, varmap{"login": githubv4.String(loginOf(u))})
		}, func(string) {}))
		if errors.Is(err, core.ErrGone) || err == nil && q.User.ID == nil {
			unknown = append(unknown, u)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, q.User.ID)
	}
	if len(ids) == 0 {
		return nil, unknown, nil
	}

	var after *assignable
	if add {
		var m addAssigneesToAssignableMutation
		err = r.mutate(ctx, as, &m, githubv4.AddAssigneesToAssignableInput{AssignableID: issue.ID, AssigneeIDs: ids})
		after = &m.AddAssignees.Assignable
	} else {
		var m removeAssigneesFromAssignableMutation
		err = r.mutate(ctx, as, &m, githubv4.RemoveAssigneesFromAssignableInput{AssignableID: issue.ID, AssigneeIDs: ids})
		after = &m.RemoveAssignees.Assignable
	}
	if err != nil {
		return nil, nil, r.confirm(ctx, err, issue.ID)
	}
	for _, n := range after.Issue.Assignees.Nodes {
		assigned = append(assigned, n.Login)
	}
	return assigned, unknown, nil
}

// labelIDs returns the ids of labels of the repository. The missing ones are
// created if create, else left out: they are on no issue.
func (r *remote) labelIDs(ctx context.Context, names []string, create bool) ([]githubv4.ID, error) {
	var ids []githubv4.ID
	for _, name := range names {
		id, err := r.labelID(ctx, name, create)
		if err != nil {
			return nil, err
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
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

// labelID returns the id of a label of the repository, created if needed and
// create, else "".
func (r *remote) labelID(ctx context.Context, name string, create bool) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// read on a miss: the label may have been made on GitHub since the labels
	// were read, as an engine may live long.
	// ponytail: one read of every label per missing one; a new label is a
	// write anyway.
	if _, ok := r.labels[strings.ToLower(name)]; !ok {
		labels := map[string]string{}
		vars := varmap{
			"owner": githubv4.String(r.owner),
			"name":  githubv4.String(r.project),
			"first": githubv4.Int(100),
			"after": (*githubv4.String)(nil),
		}
		for {
			var q labelsQuery
			if err := r.reader.queryPrintMsgs(ctx, &q, vars); err != nil {
				return "", err
			}
			for _, l := range q.Repository.Labels.Nodes {
				labels[strings.ToLower(l.Name)] = l.ID
			}
			if !q.Repository.Labels.PageInfo.HasNextPage {
				break
			}
			vars["after"] = q.Repository.Labels.PageInfo.EndCursor
		}
		r.labels = labels
	}
	if id, ok := r.labels[strings.ToLower(name)]; ok || !create {
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
		strings.Contains(msg, "Could not resolve to a User"),
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
