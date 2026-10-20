package github

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/bridge/core/synctest"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/repository/repotest"
)

// fakeGithub answers the GraphQL queries and mutations of the adapter, for
// one repository, from memory, behaving like GitHub: edits keep a history
// (newest first, the creation being the oldest), and state, title and label
// changes leave timeline events.
type fakeGithub struct {
	owner string
	// outsiders can't see the repository: GitHub resolves nothing for them
	outsiders map[string]bool
	// unassignable can't be assigned: GitHub leaves them out silently
	unassignable map[string]bool
	// noUser are logins without a user
	noUser map[string]bool

	mu       sync.Mutex
	issues   []*ghIssue
	labels   map[string]string // name by id
	lastID   int
	writes   int
	failNext bool
	pulls    map[int]*ghPull
}

type ghIssue struct {
	id, url     string
	number      int
	title, body string
	author      string
	created     time.Time
	updated     time.Time
	closed      bool
	labels      []string
	assignees   []string
	bodyEdits   []userContentEdit // oldest first
	timeline    []timelineItem
	edits       map[string][]userContentEdit // comment edits by comment id, oldest first
	deleted     bool
}

func newFakeGithub(owner string) *fakeGithub {
	return &fakeGithub{owner: owner, labels: map[string]string{}, outsiders: map[string]bool{},
		unassignable: map[string]bool{}, noUser: map[string]bool{}}
}

// The following are called with f.mu held.

func (f *fakeGithub) nextID(prefix string) string {
	f.lastID++
	return fmt.Sprintf("%s_%d", prefix, f.lastID)
}

func now() githubv4.DateTime {
	return githubv4.DateTime{Time: time.Now().UTC().Truncate(time.Second)}
}

func actorOf(login string) *actor {
	name := githubv4.String(strings.ToUpper(login[:1]) + login[1:])
	return &actor{Typename: "User", Login: githubv4.String(login), User: userActor{Name: &name}}
}

func (f *fakeGithub) find(id githubv4.ID) (*ghIssue, error) {
	for _, issue := range f.issues {
		if issue.id == fmt.Sprint(id) && !issue.deleted {
			return issue, nil
		}
	}
	return nil, fmt.Errorf("Could not resolve to a node with the global id of '%v'", id)
}

func (f *fakeGithub) findComment(id githubv4.ID) (*ghIssue, *timelineItem, error) {
	for _, issue := range f.issues {
		for i := range issue.timeline {
			if item := &issue.timeline[i]; item.Typename == "IssueComment" && fmt.Sprint(item.IssueComment.Id) == fmt.Sprint(id) {
				if issue.deleted {
					break
				}
				return issue, item, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("Could not resolve to a node with the global id of '%v'", id)
}

func (f *fakeGithub) createIssue(login, title, body string) *ghIssue {
	number := len(f.issues) + 1
	issue := &ghIssue{
		id: f.nextID("I"), number: number, title: title, body: body, author: login,
		url:     fmt.Sprintf("https://github.com/%s/project/issues/%d", f.owner, number),
		created: now().Time, updated: now().Time, edits: map[string][]userContentEdit{},
	}
	f.issues = append(f.issues, issue)
	return issue
}

func (f *fakeGithub) addComment(login string, issue *ghIssue, body string) timelineItem {
	id := f.nextID("IC")
	u, _ := url.Parse(fmt.Sprintf("%s#issuecomment-%s", issue.url, id))
	item := timelineItem{Typename: "IssueComment", IssueComment: issueComment{
		authorEvent: authorEvent{Id: githubv4.ID(id), CreatedAt: now(), Author: actorOf(login)},
		Body:        githubv4.String(body),
		Url:         githubv4.URI{URL: u},
	}}
	issue.timeline = append(issue.timeline, item)
	issue.updated = now().Time
	return item
}

// edited returns the edit history of a text after an edit by login.
func (f *fakeGithub) edited(history []userContentEdit, author string, created githubv4.DateTime, was, login, text string) []userContentEdit {
	diff := func(s string) *githubv4.String { v := githubv4.String(s); return &v }
	if len(history) == 0 {
		history = append(history, userContentEdit{Id: githubv4.ID(f.nextID("UCE")), CreatedAt: created, Editor: actorOf(author), Diff: diff(was)})
	}
	return append(history, userContentEdit{Id: githubv4.ID(f.nextID("UCE")), CreatedAt: now(), Editor: actorOf(login), Diff: diff(text)})
}

func (f *fakeGithub) editBody(login string, issue *ghIssue, body string) {
	issue.bodyEdits = f.edited(issue.bodyEdits, issue.author, githubv4.DateTime{Time: issue.created}, issue.body, login, body)
	issue.body = body
	issue.updated = now().Time
}

func (f *fakeGithub) editComment(login string, issue *ghIssue, item *timelineItem, body string) {
	c := &item.IssueComment
	id := fmt.Sprint(c.Id)
	issue.edits[id] = f.edited(issue.edits[id], string(c.Author.Login), c.CreatedAt, string(c.Body), login, body)
	c.Body = githubv4.String(body)
	issue.updated = now().Time
}

func (f *fakeGithub) setState(login string, issue *ghIssue, closed bool) {
	if issue.closed == closed {
		return
	}
	issue.closed = closed
	event := actorEvent{Id: githubv4.ID(f.nextID("E")), CreatedAt: now(), Actor: actorOf(login)}
	item := timelineItem{Typename: "ReopenedEvent"}
	item.ReopenedEvent.actorEvent = event
	if closed {
		item = timelineItem{Typename: "ClosedEvent"}
		item.ClosedEvent.actorEvent = event
	}
	issue.timeline = append(issue.timeline, item)
	issue.updated = now().Time
}

func (f *fakeGithub) setTitle(login string, issue *ghIssue, title string) {
	if issue.title == title {
		return
	}
	issue.timeline = append(issue.timeline, timelineItem{Typename: "RenamedTitleEvent", RenamedTitleEvent: renamedTitleEvent{
		actorEvent:    actorEvent{Id: githubv4.ID(f.nextID("E")), CreatedAt: now(), Actor: actorOf(login)},
		CurrentTitle:  githubv4.String(title),
		PreviousTitle: githubv4.String(issue.title),
	}})
	issue.title = title
	issue.updated = now().Time
}

func (f *fakeGithub) setLabels(login string, issue *ghIssue, labels []string) {
	event := func() actorEvent {
		return actorEvent{Id: githubv4.ID(f.nextID("E")), CreatedAt: now(), Actor: actorOf(login)}
	}
	for _, l := range labels {
		// an issue's labels are the repository's
		if !slices.Contains(slices.Collect(maps.Values(f.labels)), l) {
			f.labels[f.nextID("LA")] = l
		}
		if !slices.Contains(issue.labels, l) {
			issue.timeline = append(issue.timeline, timelineItem{Typename: "LabeledEvent",
				LabeledEvent: labeledEvent{actorEvent: event(), Label: label{Name: githubv4.String(l)}}})
		}
	}
	for _, l := range issue.labels {
		if !slices.Contains(labels, l) {
			issue.timeline = append(issue.timeline, timelineItem{Typename: "UnlabeledEvent",
				UnlabeledEvent: unlabeledEvent{actorEvent: event(), Label: label{Name: githubv4.String(l)}}})
		}
	}
	issue.labels = slices.Sorted(slices.Values(labels))
	issue.updated = now().Time
}

func (f *fakeGithub) setAssigned(login string, issue *ghIssue, assignee string, assigned bool) {
	if slices.Contains(issue.assignees, assignee) == assigned || assigned && f.unassignable[assignee] {
		return
	}
	e := assignedEvent{actorEvent: actorEvent{Id: githubv4.ID(f.nextID("E")), CreatedAt: now(), Actor: actorOf(login)}}
	e.Assignee.Actor.Login = githubv4.String(assignee)
	if assigned {
		issue.assignees = append(issue.assignees, assignee)
		issue.timeline = append(issue.timeline, timelineItem{Typename: "AssignedEvent", AssignedEvent: e})
	} else {
		issue.assignees = slices.DeleteFunc(issue.assignees, func(a string) bool { return a == assignee })
		issue.timeline = append(issue.timeline, timelineItem{Typename: "UnassignedEvent", UnassignedEvent: e})
	}
	issue.updated = now().Time
}

// assignees changes an issue's assignees, given as user ids, and answers
// with its assignees.
func (f *fakeGithub) assignees(login string, id githubv4.ID, users []githubv4.ID, assigned bool, a *assignable) error {
	issue, err := f.find(id)
	if err != nil {
		return err
	}
	for _, u := range users {
		f.setAssigned(login, issue, strings.TrimPrefix(fmt.Sprint(u), "U_"), assigned)
	}
	for _, l := range issue.assignees {
		a.Issue.Assignees.Nodes = append(a.Issue.Assignees.Nodes, struct {
			Login string `graphql:"login"`
		}{Login: l})
	}
	return nil
}

func newest(edits []userContentEdit) []userContentEdit {
	res := slices.Clone(edits)
	slices.Reverse(res)
	return res
}

func (f *fakeGithub) node(gi *ghIssue) issueNode {
	u, _ := url.Parse(gi.url)
	n := issueNode{issue: issue{
		authorEvent: authorEvent{Id: githubv4.ID(gi.id), CreatedAt: githubv4.DateTime{Time: gi.created}, Author: actorOf(gi.author)},
		Title:       githubv4.String(gi.title),
		Number:      githubv4.Int(gi.number),
		Body:        githubv4.String(gi.body),
		Url:         githubv4.URI{URL: u},
	}}
	n.UserContentEdits.Nodes = newest(gi.bodyEdits)
	for _, item := range gi.timeline {
		if item.Typename == "IssueComment" {
			item.IssueComment.UserContentEdits.Nodes = newest(gi.edits[fmt.Sprint(item.IssueComment.Id)])
		}
		n.TimelineItems.Nodes = append(n.TimelineItems.Nodes, item)
	}
	return n
}

// fakeClient is a GitHub API client acting as one user.
type fakeClient struct {
	f     *fakeGithub
	login string
}

func (c fakeClient) Query(_ context.Context, q interface{}, vars map[string]interface{}) error {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()

	switch q := q.(type) {
	case *loginQuery:
		q.Viewer.Login = c.login
	case *userQuery:
		login := string(vars["login"].(githubv4.String))
		a := actorOf(login)
		q.User = user{Login: a.Login, Name: a.User.Name}
	case *userIDQuery:
		login := string(vars["login"].(githubv4.String))
		if f.noUser[login] {
			return fmt.Errorf("Could not resolve to a User with the login of '%s'.", login)
		}
		q.User.ID = githubv4.ID("U_" + login)
	case *repositoryIDQuery:
		if f.outsiders[c.login] {
			return fmt.Errorf("Could not resolve to a Repository with the name '%s/project'", f.owner)
		}
		q.Repository.ID = "R_1"
	case *nodeQuery:
		id := vars["id"].(githubv4.ID)
		if _, err := f.find(id); err == nil {
			q.Node = &struct {
				Typename githubv4.String `graphql:"__typename"`
			}{Typename: "Issue"}
			return nil
		}
		if _, _, err := f.findComment(id); err == nil {
			q.Node = &struct {
				Typename githubv4.String `graphql:"__typename"`
			}{Typename: "IssueComment"}
			return nil
		}
		return fmt.Errorf("Could not resolve to a node with the global id of '%v'", id)
	case *labelsQuery:
		for id, name := range f.labels {
			q.Repository.Labels.Nodes = append(q.Repository.Labels.Nodes, struct {
				ID          string `graphql:"id"`
				Name        string `graphql:"name"`
				Color       string `graphql:"color"`
				Description string `graphql:"description"`
			}{ID: id, Name: name})
		}
	case *issueQuery:
		since := vars["issueSince"].(githubv4.DateTime)
		for _, issue := range f.issues {
			if !issue.deleted && !issue.updated.Before(since.Time) {
				q.Repository.Issues.Nodes = append(q.Repository.Issues.Nodes, f.node(issue))
			}
		}
	case *reviewThreadsQuery:
		return f.reviewThreads(q, vars)
	case *oneIssueQuery:
		number := int(vars["issueNumber"].(githubv4.Int))
		for _, issue := range f.issues {
			if issue.number == number && !issue.deleted {
				node := f.node(issue)
				q.Repository.Issue = &node
				return nil
			}
		}
		return fmt.Errorf("Could not resolve to an Issue with the number of %d", number)
	default:
		return fmt.Errorf("fake GitHub doesn't answer %T", q)
	}
	return nil
}

func (c fakeClient) Mutate(_ context.Context, m interface{}, input githubv4.Input, _ map[string]interface{}) error {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++

	err := c.mutate(m, input)
	if err == nil && f.failNext {
		// done, but the answer is lost
		f.failNext = false
		return errors.New("connection lost")
	}
	return err
}

func (c fakeClient) mutate(m interface{}, input githubv4.Input) error {
	f := c.f
	if f.outsiders[c.login] {
		return fmt.Errorf("Could not resolve to a node with the global id of '%v'", input)
	}
	switch m := m.(type) {
	case *createIssueMutation:
		in := input.(githubv4.CreateIssueInput)
		issue := f.createIssue(c.login, string(in.Title), string(*in.Body))
		m.CreateIssue.Issue.ID, m.CreateIssue.Issue.URL = issue.id, issue.url
	case *addCommentToIssueMutation:
		in := input.(githubv4.AddCommentInput)
		issue, err := f.find(in.SubjectID)
		if err != nil {
			return err
		}
		item := f.addComment(c.login, issue, string(in.Body))
		m.AddComment.CommentEdge.Node.ID = fmt.Sprint(item.IssueComment.Id)
		m.AddComment.CommentEdge.Node.URL = item.IssueComment.Url.String()
	case *updateIssueCommentMutation:
		in := input.(githubv4.UpdateIssueCommentInput)
		issue, item, err := f.findComment(in.ID)
		if err != nil {
			return err
		}
		f.editComment(c.login, issue, item, string(in.Body))
	case *updateIssueMutation:
		in := input.(githubv4.UpdateIssueInput)
		issue, err := f.find(in.ID)
		if err != nil {
			return err
		}
		if in.Title != nil {
			f.setTitle(c.login, issue, string(*in.Title))
		}
		if in.Body != nil {
			f.editBody(c.login, issue, string(*in.Body))
		}
		if in.State != nil {
			f.setState(c.login, issue, *in.State == githubv4.IssueStateClosed)
		}
	case *addLabelsToLabelableMutation:
		in := input.(githubv4.AddLabelsToLabelableInput)
		issue, err := f.find(in.LabelableID)
		if err != nil {
			return err
		}
		labels := slices.Clone(issue.labels)
		for _, id := range in.LabelIDs {
			labels = append(labels, f.labels[fmt.Sprint(id)])
		}
		f.setLabels(c.login, issue, slices.Compact(slices.Sorted(slices.Values(labels))))
	case *removeLabelsFromLabelableMutation:
		in := input.(githubv4.RemoveLabelsFromLabelableInput)
		issue, err := f.find(in.LabelableID)
		if err != nil {
			return err
		}
		f.setLabels(c.login, issue, slices.DeleteFunc(slices.Clone(issue.labels), func(l string) bool {
			return slices.ContainsFunc(in.LabelIDs, func(id githubv4.ID) bool { return f.labels[fmt.Sprint(id)] == l })
		}))
	case *addAssigneesToAssignableMutation:
		in := input.(githubv4.AddAssigneesToAssignableInput)
		return f.assignees(c.login, in.AssignableID, in.AssigneeIDs, true, &m.AddAssignees.Assignable)
	case *removeAssigneesFromAssignableMutation:
		in := input.(githubv4.RemoveAssigneesFromAssignableInput)
		return f.assignees(c.login, in.AssignableID, in.AssigneeIDs, false, &m.RemoveAssignees.Assignable)
	case *resolveReviewThreadMutation:
		return f.resolve(input.(githubv4.ResolveReviewThreadInput).ThreadID, true)
	case *unresolveReviewThreadMutation:
		return f.resolve(input.(githubv4.UnresolveReviewThreadInput).ThreadID, false)
	default:
		return fmt.Errorf("fake GitHub doesn't do %T", m)
	}
	return nil
}

// githubForge is the adapter on a fake GitHub, for the conformance suite.
type githubForge struct {
	*remote
	fake *fakeGithub
}

func newGithubForge(t *testing.T, owner string) githubForge {
	fake := newFakeGithub(owner)
	r, err := newRemote(context.Background(), owner, "project", "bot", []*auth.Token{auth.NewToken(target, "token-bot")},
		func(*auth.Token) Client { return fakeClient{f: fake, login: "bot"} })
	require.NoError(t, err)
	r.createLabel = func(_ context.Context, name, _ string) (string, error) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.writes++
		id := fake.nextID("LA")
		fake.labels[id] = name
		return id, nil
	}
	return githubForge{remote: r, fake: fake}
}

func (g githubForge) UserAct(login string) synctest.Actor {
	return githubActor{fake: g.fake, login: login}
}

func (g githubForge) Issue(number string) *core.Issue {
	for issue, err := range g.Issues(context.Background(), time.Time{}) {
		if err == nil && issue.Number == number {
			return issue
		}
	}
	return nil
}

func (g githubForge) Writes() int {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	return g.fake.writes
}

func (g githubForge) FailNextWrite() {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	g.fake.failNext = true
}

type githubActor struct {
	fake  *fakeGithub
	login string
}

func (a githubActor) do(fn func()) {
	a.fake.mu.Lock()
	defer a.fake.mu.Unlock()
	fn()
}

func (a githubActor) issue(ref core.Ref) *ghIssue {
	issue, err := a.fake.find(githubv4.ID(ref.ID))
	if err != nil {
		panic(err)
	}
	return issue
}

func (a githubActor) CreateIssue(title, body string) (ref core.Ref) {
	a.do(func() {
		issue := a.fake.createIssue(a.login, title, body)
		ref = core.Ref{ID: issue.id, URL: issue.url, Number: fmt.Sprint(issue.number)}
	})
	return ref
}

func (a githubActor) Comment(issue core.Ref, body string) (ref core.Ref) {
	a.do(func() {
		item := a.fake.addComment(a.login, a.issue(issue), body)
		ref = core.Ref{ID: fmt.Sprint(item.IssueComment.Id), URL: item.IssueComment.Url.String()}
	})
	return ref
}

func (a githubActor) EditText(issue, target core.Ref, body string) {
	a.do(func() {
		if target.ID == issue.ID {
			a.fake.editBody(a.login, a.issue(issue), body)
			return
		}
		i, item, err := a.fake.findComment(githubv4.ID(target.ID))
		if err != nil {
			panic(err)
		}
		a.fake.editComment(a.login, i, item, body)
	})
}

func (a githubActor) SetStatus(issue core.Ref, status common.Status) {
	a.do(func() { a.fake.setState(a.login, a.issue(issue), status == common.ClosedStatus) })
}

func (a githubActor) SetTitle(issue core.Ref, title string) {
	a.do(func() { a.fake.setTitle(a.login, a.issue(issue), title) })
}

func (a githubActor) SetLabels(issue core.Ref, labels []string) {
	a.do(func() { a.fake.setLabels(a.login, a.issue(issue), labels) })
}

func (a githubActor) Delete(issue core.Ref) {
	a.do(func() { a.issue(issue).deleted = true })
}

func TestGithubConformance(t *testing.T) {
	synctest.Run(t, synctest.Harness{
		NewForge: func(t *testing.T, name string) synctest.Forge { return newGithubForge(t, "owner-"+name) },
		LoginKey: metaKeyGithubLogin,
	})
}

// A bug imported by the GitHub bridge before the sync engine is recognized,
// with its comments, and nothing is imported twice.
func TestGithubLegacyMetadata(t *testing.T) {
	g := newGithubForge(t, "owner")
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	comment := alice.Comment(ref, "a comment")

	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	defer backend.Close()
	me, err := backend.Identities().New("Me", "me@example.com")
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(me))

	// as the importer of the GitHub bridge left it
	b, _, err := backend.Bugs().NewRaw(me, time.Now().Unix(), "title", "body", nil, map[string]string{
		core.MetaKeyOrigin: target,
		metaKeyGithubId:    ref.ID,
		metaKeyGithubUrl:   ref.URL,
	})
	require.NoError(t, err)
	_, _, err = b.AddCommentRaw(me, time.Now().Unix(), "a comment", nil, map[string]string{
		metaKeyGithubId:  comment.ID,
		metaKeyGithubUrl: comment.URL,
	})
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	ops := len(b.Snapshot().Operations)

	e, err := core.NewEngine(context.Background(), backend, g, core.SyncOptions{
		Foreign: true, DefaultLogin: "bot", LoginKey: metaKeyGithubLogin,
	})
	require.NoError(t, err)
	for r := range e.Pull(context.Background(), time.Time{}) {
		require.NotEqual(t, core.ImportEventError, r.Event, r.String())
	}
	for r := range e.Push(context.Background()) {
		require.NotEqual(t, core.ExportEventError, r.Event, r.String())
	}

	require.Len(t, backend.Bugs().AllIds(), 1)
	got, err := backend.Bugs().Resolve(b.Id())
	require.NoError(t, err)
	require.Len(t, got.Snapshot().Comments, 2)
	require.Len(t, got.Snapshot().Operations, ops, "nothing new")
	require.Zero(t, g.Writes(), "nothing pushed")
}

// splitClient makes the timeline of an issue need a second page, which then
// fails to load.
type splitClient struct {
	fakeClient
	split *bool
}

func (c splitClient) Query(ctx context.Context, q interface{}, vars map[string]interface{}) error {
	if !*c.split {
		return c.fakeClient.Query(ctx, q, vars)
	}
	switch q := q.(type) {
	case *timelineQuery:
		return errors.New("502 Bad Gateway")
	case *issueQuery:
		if err := c.fakeClient.Query(ctx, q, vars); err != nil {
			return err
		}
		for i := range q.Repository.Issues.Nodes {
			items := &q.Repository.Issues.Nodes[i].TimelineItems
			if len(items.Nodes) > 1 {
				items.Nodes = items.Nodes[:1]
				items.PageInfo.HasNextPage = true
			}
		}
	default:
		return c.fakeClient.Query(ctx, q, vars)
	}
	return nil
}

// Review I1: an issue whose timeline failed to load is not given to the
// engine as if it were complete.
func TestGithubPartialTimelineIsAnError(t *testing.T) {
	defer func(d time.Duration) { retryDelay = d }(retryDelay)
	retryDelay = time.Millisecond

	fake := newFakeGithub("owner")
	split := false
	r, err := newRemote(context.Background(), "owner", "project", "bot", []*auth.Token{auth.NewToken(target, "token-bot")},
		func(*auth.Token) Client { return splitClient{fakeClient{f: fake, login: "bot"}, &split} })
	require.NoError(t, err)
	g := githubForge{remote: r, fake: fake}
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("closed one", "body")
	alice.Comment(ref, "a comment")
	alice.SetStatus(ref, common.ClosedStatus)
	alice.CreateIssue("another one", "body")

	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	defer backend.Close()
	me, err := backend.Identities().New("Me", "me@example.com")
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(me))
	pull := func() (errs int) {
		e, err := core.NewEngine(context.Background(), backend, g, core.SyncOptions{DefaultLogin: "bot", LoginKey: metaKeyGithubLogin})
		require.NoError(t, err)
		for r := range e.Pull(context.Background(), time.Time{}) {
			if r.Event == core.ImportEventError {
				errs++
			}
		}
		return errs
	}
	require.Zero(t, pull())
	closed, err := backend.Bugs().ResolveBugCreateMetadata("sync:github.com/owner/project:id", ref.ID)
	require.NoError(t, err)
	require.Equal(t, common.ClosedStatus, closed.Snapshot().Status)

	split = true
	require.Positive(t, pull(), "the failure is reported")
	closed, err = backend.Bugs().Resolve(closed.Id())
	require.NoError(t, err)
	require.Equal(t, common.ClosedStatus, closed.Snapshot().Status, "not reopened from a partial timeline")
}

// Review I3: GitHub resolves nothing for a token that can't see a private
// repository, as for a deleted issue. That must not mark the bug as deleted
// on the forge.
func TestGithubNotFoundByOutsiderIsNotGone(t *testing.T) {
	fake := newFakeGithub("owner")
	fake.outsiders["alice"] = true
	tokens := []*auth.Token{auth.NewToken(target, "bot"), auth.NewToken(target, "alice")}
	r, err := newRemote(context.Background(), "owner", "project", "bot", tokens,
		func(token *auth.Token) Client { return fakeClient{f: fake, login: token.Value} })
	require.NoError(t, err)
	g := githubForge{remote: r, fake: fake}
	ref := g.UserAct("bot").CreateIssue("title", "body")

	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	defer backend.Close()
	alice, err := backend.Identities().NewRaw("Alice", "", "alice", "", nil, map[string]string{metaKeyGithubLogin: "alice"})
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(alice))
	bot, err := backend.Identities().NewRaw("Bot", "", "bot", "", nil, map[string]string{metaKeyGithubLogin: "bot"})
	require.NoError(t, err)
	sync := func() {
		e, err := core.NewEngine(context.Background(), backend, g, core.SyncOptions{Foreign: true, DefaultLogin: "bot", LoginKey: metaKeyGithubLogin})
		require.NoError(t, err)
		for r := range e.Pull(context.Background(), time.Time{}) {
			require.NotEqual(t, core.ImportEventError, r.Event, r.String())
		}
		for r := range e.Push(context.Background()) {
			require.NotEqual(t, core.ExportEventError, r.Event, r.String())
		}
	}
	sync()

	b, err := backend.Bugs().Resolve(backend.Bugs().AllIds()[0])
	require.NoError(t, err)
	_, err = b.SetTitleRaw(alice, time.Now().Unix(), "by alice", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	sync()

	_, err = b.SetTitleRaw(bot, time.Now().Unix()+1, "by the bot", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	sync()
	require.Equal(t, "by the bot", g.Issue(ref.Number).Title)
}

// costlyClient refuses pages of more than 5 issues, as GitHub refuses a query
// too costly to compute on projects with long issue histories.
type costlyClient struct {
	fakeClient
	calls *int
}

func (c costlyClient) Query(ctx context.Context, q interface{}, vars map[string]interface{}) error {
	if _, ok := q.(*issueQuery); ok {
		*c.calls++
		if vars["issueFirst"].(githubv4.Int) > 5 {
			return errors.New("Resource limits for this query exceeded.")
		}
	}
	return c.fakeClient.Query(ctx, q, vars)
}

func TestGithubPullShrinksCostlyPages(t *testing.T) {
	fake := newFakeGithub("owner")
	calls := 0
	r, err := newRemote(context.Background(), "owner", "project", "bot", []*auth.Token{auth.NewToken(target, "token-bot")},
		func(*auth.Token) Client { return costlyClient{fakeClient{f: fake, login: "bot"}, &calls} })
	require.NoError(t, err)
	g := githubForge{remote: r, fake: fake}
	alice := g.UserAct("alice")
	alice.CreateIssue("one", "body")
	alice.CreateIssue("two", "body")

	n := 0
	for _, err := range g.Issues(context.Background(), time.Time{}) {
		require.NoError(t, err)
		n++
	}
	require.Equal(t, 2, n)
	require.Equal(t, 4, calls, "40, 20, 10 refused at once, then 5")
}

// limitedClient answers the next writes with GitHub's secondary rate limit.
type limitedClient struct {
	fakeClient
	left *int
}

func (c limitedClient) Mutate(ctx context.Context, m interface{}, input githubv4.Input, vars map[string]interface{}) error {
	if *c.left > 0 {
		*c.left--
		return errors.New("You have exceeded a secondary rate limit. Please wait a few minutes before you try again.")
	}
	return c.fakeClient.Mutate(ctx, m, input, vars)
}

// Many writes in a row, as a first sync makes, hit GitHub's secondary rate
// limit: the engine waits and goes on, instead of failing each bug.
func TestGithubSecondaryRateLimitWaits(t *testing.T) {
	defer func(d time.Duration) { rateLimitWait = d }(rateLimitWait)
	rateLimitWait = time.Millisecond

	fake := newFakeGithub("owner")
	left := 2
	r, err := newRemote(context.Background(), "owner", "project", "bot", []*auth.Token{auth.NewToken(target, "token-bot")},
		func(*auth.Token) Client { return limitedClient{fakeClient{f: fake, login: "bot"}, &left} })
	require.NoError(t, err)
	g := githubForge{remote: r, fake: fake}

	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	defer backend.Close()
	me, err := backend.Identities().New("Me", "me@example.com")
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(me))
	_, _, err = backend.Bugs().New("local", "body")
	require.NoError(t, err)

	e, err := core.NewEngine(context.Background(), backend, g, core.SyncOptions{Foreign: true, DefaultLogin: "bot", LoginKey: metaKeyGithubLogin})
	require.NoError(t, err)
	for r := range e.Push(context.Background()) {
		require.NotEqual(t, core.ExportEventError, r.Event, r.String())
	}
	require.Equal(t, "local", g.Issue("1").Title)
}

// A label made on GitHub after the adapter read the repository's labels is
// found again, to be removed: an engine may live long, as git-board's do.
func TestGithubRemovesLabelMadeSinceRead(t *testing.T) {
	ctx := context.Background()
	g := newGithubForge(t, "owner")
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	require.NoError(t, g.ChangeLabels(ctx, g.accounts[0], ref, []string{"a"}, nil))

	// made on GitHub, as the adapter had read the labels
	alice.SetLabels(ref, []string{"a", "new"})

	require.NoError(t, g.ChangeLabels(ctx, g.accounts[0], ref, nil, []string{"new"}))
	require.Equal(t, []string{"a"}, g.Issue(ref.Number).Labels)
}
