package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	gitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/bridge/core/synctest"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/repository/repotest"
)

// fakeGitlab serves the part of the GitLab API the adapter uses, for one
// project, from memory, behaving like GitLab: title changes leave a system
// note, state and label changes resource events (one per label), and only
// the author of a note may edit it.
type fakeGitlab struct {
	*httptest.Server

	mu    sync.Mutex
	users map[string]*gitlab.User // by token
	// outsiders can't see the project: GitLab answers them 404
	outsiders map[string]bool
	issues    []*fakeIssue
	lastID    int64
	writes    int
	failNext  bool
}

type fakeIssue struct {
	gitlab.Issue
	notes       []*gitlab.Note
	labelEvents []*gitlab.LabelEvent
	stateEvents []*gitlab.StateEvent
	deleted     bool
}

func newFakeGitlab(t *testing.T) *fakeGitlab {
	// notes and events ids never collide with issue iids
	f := &fakeGitlab{users: map[string]*gitlab.User{}, outsiders: map[string]bool{}, lastID: 10000}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		reply(w, f.users[r.Header.Get("PRIVATE-TOKEN")])
	})
	mux.HandleFunc("GET /api/v4/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		for _, u := range f.users {
			if strconv.FormatInt(u.ID, 10) == r.PathValue("id") {
				reply(w, u)
				return
			}
		}
		notFound(w)
	})
	mux.HandleFunc("GET /api/v4/projects/{pid}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, gitlab.Project{ID: 1})
	})
	mux.HandleFunc("GET /api/v4/projects/{pid}/issues/{iid}", func(w http.ResponseWriter, r *http.Request) {
		if issue := f.issue(r); issue != nil {
			reply(w, &issue.Issue)
			return
		}
		notFound(w)
	})
	mux.HandleFunc("GET /api/v4/projects/{pid}/issues/{iid}/notes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if issue := f.issue(r); issue != nil {
			for _, note := range issue.notes {
				if strconv.FormatInt(note.ID, 10) == r.PathValue("id") {
					reply(w, note)
					return
				}
			}
		}
		notFound(w)
	})
	mux.HandleFunc("GET /api/v4/projects/{pid}/issues", func(w http.ResponseWriter, r *http.Request) {
		since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("updated_after"))
		var res []*gitlab.Issue
		for _, issue := range f.issues {
			if !issue.deleted && !issue.UpdatedAt.Before(since) {
				res = append(res, &issue.Issue)
			}
		}
		reply(w, res)
	})
	mux.HandleFunc("POST /api/v4/projects/{pid}/issues", func(w http.ResponseWriter, r *http.Request) {
		var opt gitlab.CreateIssueOptions
		_ = json.NewDecoder(r.Body).Decode(&opt)
		reply(w, f.createIssue(f.user(r), *opt.Title, *opt.Description))
	})
	mux.HandleFunc("PUT /api/v4/projects/{pid}/issues/{iid}", func(w http.ResponseWriter, r *http.Request) {
		issue := f.issue(r)
		if issue == nil {
			notFound(w)
			return
		}
		var opt map[string]any
		_ = json.NewDecoder(r.Body).Decode(&opt)
		u := f.user(r)
		if v, ok := opt["title"].(string); ok {
			f.setTitle(u, issue, v)
		}
		if v, ok := opt["description"].(string); ok {
			f.setDescription(u, issue, v)
		}
		if v, ok := opt["state_event"].(string); ok {
			f.setState(u, issue, v == "close")
		}
		if v, ok := opt["labels"]; ok {
			f.setLabels(u, issue, labelList(v))
		}
		reply(w, &issue.Issue)
	})
	mux.HandleFunc("GET /api/v4/projects/{pid}/issues/{iid}/notes", func(w http.ResponseWriter, r *http.Request) {
		if issue := f.issue(r); issue != nil {
			reply(w, issue.notes)
			return
		}
		notFound(w)
	})
	mux.HandleFunc("POST /api/v4/projects/{pid}/issues/{iid}/notes", func(w http.ResponseWriter, r *http.Request) {
		issue := f.issue(r)
		if issue == nil {
			notFound(w)
			return
		}
		var opt gitlab.CreateIssueNoteOptions
		_ = json.NewDecoder(r.Body).Decode(&opt)
		reply(w, f.addNote(f.user(r), issue, *opt.Body, false))
	})
	mux.HandleFunc("PUT /api/v4/projects/{pid}/issues/{iid}/notes/{id}", func(w http.ResponseWriter, r *http.Request) {
		issue := f.issue(r)
		if issue == nil {
			notFound(w)
			return
		}
		var opt gitlab.UpdateIssueNoteOptions
		_ = json.NewDecoder(r.Body).Decode(&opt)
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		note, err := f.editNote(f.user(r), issue, id, *opt.Body)
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			reply(w, map[string]string{"message": err.Error()})
			return
		}
		reply(w, note)
	})
	mux.HandleFunc("GET /api/v4/projects/{pid}/issues/{iid}/resource_label_events", func(w http.ResponseWriter, r *http.Request) {
		if issue := f.issue(r); issue != nil {
			reply(w, issue.labelEvents)
			return
		}
		notFound(w)
	})
	mux.HandleFunc("GET /api/v4/projects/{pid}/issues/{iid}/resource_state_events", func(w http.ResponseWriter, r *http.Request) {
		if issue := f.issue(r); issue != nil {
			reply(w, issue.stateEvents)
			return
		}
		notFound(w)
	})

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if u := f.user(r); u != nil && f.outsiders[u.Username] && strings.HasPrefix(r.URL.Path, "/api/v4/projects/") {
			notFound(w)
			return
		}
		if r.Method == http.MethodGet {
			mux.ServeHTTP(w, r)
			return
		}
		f.writes++
		if !f.failNext {
			mux.ServeHTTP(w, r)
			return
		}
		// done, but the answer is lost
		f.failNext = false
		mux.ServeHTTP(httptest.NewRecorder(), r)
		w.WriteHeader(http.StatusBadRequest)
		reply(w, map[string]string{"message": "connection lost"})
	}))
	t.Cleanup(f.Close)
	return f
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	reply(w, map[string]string{"message": "404 Not found"})
}

// labelList reads labels as GitLab takes them: a comma separated string, or
// a list.
func labelList(v any) []string {
	var res []string
	switch v := v.(type) {
	case string:
		for _, l := range strings.Split(v, ",") {
			if l = strings.TrimSpace(l); l != "" {
				res = append(res, l)
			}
		}
	case []any:
		for _, l := range v {
			res = append(res, fmt.Sprint(l))
		}
	}
	return res
}

// The following are called with f.mu held.

func (f *fakeGitlab) userNamed(login string) *gitlab.User {
	token := "token-" + login
	if u, ok := f.users[token]; ok {
		return u
	}
	u := &gitlab.User{ID: int64(len(f.users) + 1), Username: login, Name: strings.ToUpper(login[:1]) + login[1:]}
	f.users[token] = u
	return u
}

func (f *fakeGitlab) user(r *http.Request) *gitlab.User {
	return f.users[r.Header.Get("PRIVATE-TOKEN")]
}

func (f *fakeGitlab) issue(r *http.Request) *fakeIssue {
	iid, _ := strconv.ParseInt(r.PathValue("iid"), 10, 64)
	for _, issue := range f.issues {
		if issue.IID == iid && !issue.deleted {
			return issue
		}
	}
	return nil
}

func (f *fakeGitlab) now() *time.Time {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &now
}

func (f *fakeGitlab) nextID() int64 {
	f.lastID++
	return f.lastID
}

func (f *fakeGitlab) createIssue(u *gitlab.User, title, description string) *gitlab.Issue {
	iid := int64(len(f.issues) + 1)
	issue := &fakeIssue{Issue: gitlab.Issue{
		ID: 100000 + iid, IID: iid, ProjectID: 1,
		Title: title, Description: description, State: "opened", Labels: gitlab.Labels{},
		Author:    &gitlab.IssueAuthor{ID: u.ID, Username: u.Username, Name: u.Name},
		CreatedAt: f.now(), UpdatedAt: f.now(),
		WebURL: fmt.Sprintf("%s/group/project/-/issues/%d", f.URL, iid),
	}}
	f.issues = append(f.issues, issue)
	return &issue.Issue
}

func (f *fakeGitlab) addNote(u *gitlab.User, issue *fakeIssue, body string, system bool) *gitlab.Note {
	note := &gitlab.Note{
		ID: f.nextID(), Body: body, System: system,
		Author:    gitlab.NoteAuthor{ID: u.ID, Username: u.Username, Name: u.Name},
		CreatedAt: f.now(), UpdatedAt: f.now(),
	}
	issue.notes = append(issue.notes, note)
	issue.UpdatedAt = f.now()
	return note
}

func (f *fakeGitlab) editNote(u *gitlab.User, issue *fakeIssue, id int64, body string) (*gitlab.Note, error) {
	for _, note := range issue.notes {
		if note.ID != id {
			continue
		}
		if note.Author.ID != u.ID {
			return nil, fmt.Errorf("403 Forbidden")
		}
		note.Body, note.UpdatedAt = body, f.now()
		issue.UpdatedAt = f.now()
		return note, nil
	}
	return nil, fmt.Errorf("404 Not found")
}

func (f *fakeGitlab) setTitle(u *gitlab.User, issue *fakeIssue, title string) {
	if title == issue.Title {
		return
	}
	f.addNote(u, issue, fmt.Sprintf("changed title from **%s** to **%s**", issue.Title, title), true)
	issue.Title = title
}

func (f *fakeGitlab) setDescription(u *gitlab.User, issue *fakeIssue, description string) {
	if description == issue.Description {
		return
	}
	f.addNote(u, issue, "changed the description", true)
	issue.Description = description
}

func (f *fakeGitlab) setState(u *gitlab.User, issue *fakeIssue, closed bool) {
	state := map[bool]string{true: "closed", false: "opened"}[closed]
	if issue.State == state {
		return
	}
	issue.State = state
	event := map[bool]string{true: "closed", false: "reopened"}[closed]
	issue.stateEvents = append(issue.stateEvents, &gitlab.StateEvent{
		ID: f.nextID(), User: &gitlab.BasicUser{ID: u.ID, Username: u.Username, Name: u.Name},
		CreatedAt: f.now(), State: gitlab.EventTypeValue(event),
	})
	issue.UpdatedAt = f.now()
}

func (f *fakeGitlab) setLabels(u *gitlab.User, issue *fakeIssue, labels []string) {
	event := func(action, label string) {
		e := &gitlab.LabelEvent{
			ID: f.nextID(), Action: action, CreatedAt: f.now(),
			User: gitlab.BasicUser{ID: u.ID, Username: u.Username, Name: u.Name},
		}
		e.Label.Name = label
		issue.labelEvents = append(issue.labelEvents, e)
		issue.UpdatedAt = f.now()
	}
	for _, l := range labels {
		if !slices.Contains(issue.Labels, l) {
			event("add", l)
		}
	}
	for _, l := range issue.Labels {
		if !slices.Contains(labels, l) {
			event("remove", l)
		}
	}
	issue.Labels = slices.Sorted(slices.Values(labels))
}

// gitlabForge is the adapter on a fake GitLab, for the conformance suite.
type gitlabForge struct {
	*remote
	fake *fakeGitlab
}

func newGitlabForge(t *testing.T) gitlabForge {
	fake := newFakeGitlab(t)
	fake.mu.Lock()
	bot := fake.userNamed("bot")
	fake.mu.Unlock()

	token := auth.NewToken(target, "token-"+bot.Username)
	r, err := newRemote(context.Background(), fake.URL+"/", "1", "bot", []*auth.Token{token})
	require.NoError(t, err)
	return gitlabForge{remote: r, fake: fake}
}

func (g gitlabForge) UserAct(login string) synctest.Actor {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	return gitlabActor{fake: g.fake, u: g.fake.userNamed(login)}
}

func (g gitlabForge) Issue(number string) *core.Issue {
	for issue, err := range g.Issues(context.Background(), time.Time{}) {
		if err == nil && issue.Number == number {
			return issue
		}
	}
	return nil
}

func (g gitlabForge) Writes() int {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	return g.fake.writes
}

func (g gitlabForge) FailNextWrite() {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	g.fake.failNext = true
}

type gitlabActor struct {
	fake *fakeGitlab
	u    *gitlab.User
}

func (a gitlabActor) find(ref core.Ref) *fakeIssue {
	for _, issue := range a.fake.issues {
		if strconv.FormatInt(issue.IID, 10) == ref.ID && !issue.deleted {
			return issue
		}
	}
	panic("no issue " + ref.ID)
}

func (a gitlabActor) CreateIssue(title, body string) core.Ref {
	a.fake.mu.Lock()
	defer a.fake.mu.Unlock()
	issue := a.fake.createIssue(a.u, title, body)
	iid := strconv.FormatInt(issue.IID, 10)
	return core.Ref{ID: iid, Number: iid, URL: issue.WebURL}
}

func (a gitlabActor) Comment(ref core.Ref, body string) core.Ref {
	a.fake.mu.Lock()
	defer a.fake.mu.Unlock()
	note := a.fake.addNote(a.u, a.find(ref), body, false)
	return core.Ref{ID: strconv.FormatInt(note.ID, 10)}
}

func (a gitlabActor) EditText(ref, target core.Ref, body string) {
	a.fake.mu.Lock()
	defer a.fake.mu.Unlock()
	issue := a.find(ref)
	if target.ID == ref.ID {
		a.fake.setDescription(a.u, issue, body)
		return
	}
	id, _ := strconv.ParseInt(target.ID, 10, 64)
	if _, err := a.fake.editNote(a.u, issue, id, body); err != nil {
		panic(err)
	}
}

func (a gitlabActor) SetStatus(ref core.Ref, status common.Status) {
	a.fake.mu.Lock()
	defer a.fake.mu.Unlock()
	a.fake.setState(a.u, a.find(ref), status == common.ClosedStatus)
}

func (a gitlabActor) SetTitle(ref core.Ref, title string) {
	a.fake.mu.Lock()
	defer a.fake.mu.Unlock()
	a.fake.setTitle(a.u, a.find(ref), title)
}

func (a gitlabActor) SetLabels(ref core.Ref, labels []string) {
	a.fake.mu.Lock()
	defer a.fake.mu.Unlock()
	a.fake.setLabels(a.u, a.find(ref), labels)
}

func (a gitlabActor) Delete(ref core.Ref) {
	a.fake.mu.Lock()
	defer a.fake.mu.Unlock()
	a.find(ref).deleted = true
}

func TestGitlabConformance(t *testing.T) {
	synctest.Run(t, synctest.Harness{
		NewForge: func(t *testing.T, name string) synctest.Forge { return newGitlabForge(t) },
		LoginKey: metaKeyGitlabLogin,
	})
}

// A bug imported by the GitLab bridge before the sync engine is recognized,
// with its comments, and nothing is imported twice.
func TestGitlabLegacyMetadata(t *testing.T) {
	g := newGitlabForge(t)
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

	// as the importer of the GitLab bridge left it
	b, _, err := backend.Bugs().NewRaw(me, time.Now().Unix(), "title", "body", nil, map[string]string{
		core.MetaKeyOrigin:   target,
		metaKeyGitlabId:      ref.ID,
		metaKeyGitlabProject: "1",
		metaKeyGitlabBaseUrl: g.baseURL,
	})
	require.NoError(t, err)
	_, _, err = b.AddCommentRaw(me, time.Now().Unix(), "a comment", nil, map[string]string{metaKeyGitlabId: comment.ID})
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	ops := len(b.Snapshot().Operations)

	e, err := core.NewEngine(context.Background(), backend, g, core.SyncOptions{
		Foreign: true, DefaultLogin: "bot", LoginKey: metaKeyGitlabLogin,
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

// Review C1, GitLab: a description edit leaves no event with its text, only
// a system note; its time, not the issue's last update, decides a conflict.
func TestGitlabBodyConflictIgnoresUnrelatedActivity(t *testing.T) {
	g := newGitlabForge(t)
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")

	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	defer backend.Close()
	me, err := backend.Identities().New("Me", "me@example.com")
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(me))
	pull := func() {
		e, err := core.NewEngine(context.Background(), backend, g, core.SyncOptions{DefaultLogin: "bot", LoginKey: metaKeyGitlabLogin})
		require.NoError(t, err)
		for r := range e.Pull(context.Background(), time.Time{}) {
			require.NotEqual(t, core.ImportEventError, r.Event, r.String())
		}
	}
	pull()

	// edited on GitLab, then in git-bug, then something unrelated on GitLab
	alice.EditText(ref, ref, "forge body")
	time.Sleep(1100 * time.Millisecond)
	b, err := backend.Bugs().Resolve(backend.Bugs().AllIds()[0])
	require.NoError(t, err)
	_, _, err = b.EditCreateCommentRaw(me, time.Now().Unix(), "local body", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	time.Sleep(1100 * time.Millisecond)
	g.UserAct("bob").Comment(ref, "unrelated")

	pull()
	b, err = backend.Bugs().Resolve(b.Id())
	require.NoError(t, err)
	require.Equal(t, "local body", b.Snapshot().Comments[0].Message)
}

// Review I3: GitLab answers 404 to a token that can't see a private project,
// as to a deleted issue. That must not mark the bug as deleted on the forge.
func TestGitlabNotFoundByOutsiderIsNotGone(t *testing.T) {
	fake := newFakeGitlab(t)
	fake.mu.Lock()
	fake.userNamed("bot")
	fake.userNamed("alice")
	fake.outsiders["alice"] = true
	fake.mu.Unlock()
	r, err := newRemote(context.Background(), fake.URL+"/", "1", "bot",
		[]*auth.Token{auth.NewToken(target, "token-bot"), auth.NewToken(target, "token-alice")})
	require.NoError(t, err)
	g := gitlabForge{remote: r, fake: fake}
	ref := g.UserAct("bot").CreateIssue("title", "body")

	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	defer backend.Close()
	alice, err := backend.Identities().NewRaw("Alice", "", "alice", "", nil, map[string]string{metaKeyGitlabLogin: "alice"})
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(alice))
	bot, err := backend.Identities().NewRaw("Bot", "", "bot", "", nil, map[string]string{metaKeyGitlabLogin: "bot"})
	require.NoError(t, err)
	sync := func() (warnings int) {
		e, err := core.NewEngine(context.Background(), backend, g, core.SyncOptions{Foreign: true, DefaultLogin: "bot", LoginKey: metaKeyGitlabLogin})
		require.NoError(t, err)
		for r := range e.Pull(context.Background(), time.Time{}) {
			require.NotEqual(t, core.ImportEventError, r.Event, r.String())
		}
		for r := range e.Push(context.Background()) {
			require.NotEqual(t, core.ExportEventError, r.Event, r.String())
			if r.Event == core.ExportEventWarning {
				warnings++
			}
		}
		return warnings
	}
	sync()

	// alice, who can't see the project, renames the bug
	b, err := backend.Bugs().Resolve(backend.Bugs().AllIds()[0])
	require.NoError(t, err)
	_, err = b.SetTitleRaw(alice, time.Now().Unix(), "by alice", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	require.Equal(t, 1, sync(), "the refusal is reported")

	// the bot's next change still reaches the issue: it's not taken for deleted
	_, err = b.SetTitleRaw(bot, time.Now().Unix()+1, "by the bot", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	sync()
	require.Equal(t, "by the bot", g.Issue(ref.Number).Title)
}

// A base URL is written with or without its trailing slash, by hand or by
// older versions: tokens and old bugs are found either way.
func TestGitlabBaseURLTrailingSlash(t *testing.T) {
	g := newGitlabForge(t)
	require.True(t, strings.HasSuffix(g.baseURL, "/"))
	bare := strings.TrimSuffix(g.baseURL, "/")

	id, ok := g.Legacy(map[string]string{metaKeyGitlabId: "7"},
		map[string]string{metaKeyGitlabProject: "1", metaKeyGitlabBaseUrl: bare})
	require.True(t, ok)
	require.Equal(t, "7", id)

	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	defer backend.Close()
	token := auth.NewToken(target, "token-bot")
	token.SetMetadata(auth.MetaKeyBaseURL, bare)
	token.SetMetadata(auth.MetaKeyLogin, "bot")
	require.NoError(t, auth.Store(repo, token))

	_, err = (&Gitlab{}).NewRemote(context.Background(), backend, core.Configuration{
		confKeyGitlabBaseUrl: g.baseURL, confKeyProjectID: "1", confKeyDefaultLogin: "bot",
	})
	require.NoError(t, err)
}
