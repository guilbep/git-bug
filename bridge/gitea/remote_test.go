package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/synctest"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/repository/repotest"
)

// fakeGitea serves the part of the Gitea API the adapter and the tests use,
// for one repository, from memory, behaving like Gitea 1.22: the timeline
// lists comments, title, state and label changes (one per label), edits
// leave no trace, and only the author of a comment or an admin may edit it.
type fakeGitea struct {
	*httptest.Server

	mu    sync.Mutex
	users map[string]*apiUser // by token
	// outsiders can't see the repository: Gitea answers them 404
	outsiders map[string]bool
	issues    []*fakeIssue
	labels    []apiLabel
	lastID    int64
}

type fakeIssue struct {
	apiIssue
	timeline []*apiTimeline
	deleted  bool
}

func newFakeGitea(t *testing.T) *fakeGitea {
	f := &fakeGitea{users: map[string]*apiUser{}, outsiders: map[string]bool{}}
	mux := http.NewServeMux()
	const repo = "/api/v1/repos/{owner}/{repo}"

	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) {
		reply(w, f.user(r))
	})
	mux.HandleFunc("GET /api/v1/users/search", func(w http.ResponseWriter, r *http.Request) {
		var data []*apiUser
		for _, u := range f.users {
			if strconv.FormatInt(u.ID, 10) == r.URL.Query().Get("uid") {
				data = append(data, u)
			}
		}
		reply(w, map[string]any{"data": data})
	})
	mux.HandleFunc("GET "+repo, func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"id": 1})
	})
	mux.HandleFunc("GET "+repo+"/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		if issue := f.issue(w, r); issue != nil {
			reply(w, issue.apiIssue)
		}
	})
	mux.HandleFunc("GET "+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
		res := []apiIssue{}
		if r.URL.Query().Get("page") == "1" {
			// the newest first, as Gitea does
			for _, issue := range slices.Backward(f.issues) {
				if !issue.deleted && !issue.Updated.Before(since) {
					res = append(res, issue.apiIssue)
				}
			}
		}
		reply(w, res)
	})
	mux.HandleFunc("POST "+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		var opt struct{ Title, Body string }
		_ = json.NewDecoder(r.Body).Decode(&opt)
		u := f.user(r)
		now := f.now()
		number := int64(len(f.issues) + 1)
		issue := &fakeIssue{apiIssue: apiIssue{
			ID: 1000 + number, Number: number, Title: opt.Title, Body: opt.Body, User: u, State: "open",
			HTMLURL: fmt.Sprintf("%s/%s/%s/issues/%d", f.URL, r.PathValue("owner"), r.PathValue("repo"), number),
			Labels:  []apiLabel{}, Created: now, Updated: now,
		}}
		f.issues = append(f.issues, issue)
		reply(w, issue.apiIssue)
	})
	mux.HandleFunc("DELETE "+repo+"/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		issue := f.issue(w, r)
		if issue == nil {
			return
		}
		if f.user(r).Login != "bot" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		issue.deleted = true
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PATCH "+repo+"/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		issue := f.issue(w, r)
		if issue == nil {
			return
		}
		var opt map[string]string
		_ = json.NewDecoder(r.Body).Decode(&opt)
		u := f.user(r)
		if v, ok := opt["title"]; ok && v != issue.Title {
			f.event(issue, &apiTimeline{Type: "change_title", User: u, OldTitle: issue.Title, NewTitle: v})
			issue.Title = v
		}
		if v, ok := opt["body"]; ok {
			issue.Body = v
		}
		if v, ok := opt["state"]; ok && v != issue.State {
			kind := map[string]string{"closed": "close", "open": "reopen"}[v]
			f.event(issue, &apiTimeline{Type: kind, User: u})
			issue.State = v
		}
		issue.Updated = f.now()
		reply(w, issue.apiIssue)
	})
	mux.HandleFunc("POST "+repo+"/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		issue := f.issue(w, r)
		if issue == nil {
			return
		}
		var opt struct{ Body string }
		_ = json.NewDecoder(r.Body).Decode(&opt)
		c := &apiTimeline{Type: "comment", User: f.user(r), Body: opt.Body}
		f.event(issue, c)
		c.HTMLURL = fmt.Sprintf("%s#issuecomment-%d", issue.HTMLURL, c.ID)
		reply(w, c)
	})
	mux.HandleFunc("PATCH "+repo+"/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		var opt struct{ Body string }
		_ = json.NewDecoder(r.Body).Decode(&opt)
		u := f.user(r)
		for _, issue := range f.issues {
			for _, c := range issue.timeline {
				if c.Type != "comment" || strconv.FormatInt(c.ID, 10) != r.PathValue("id") || issue.deleted {
					continue
				}
				if c.User.ID != u.ID && u.Login != "bot" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				c.Body, c.Updated = opt.Body, f.now()
				issue.Updated = f.now()
				reply(w, c)
				return
			}
		}
		http.NotFound(w, r)
	})
	// one route for "issues/<n>/timeline" and "issues/comments/<id>",
	// which overlap
	mux.HandleFunc("GET "+repo+"/issues/{n}/{sub}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("n") == "comments" {
			for _, issue := range f.issues {
				for _, c := range issue.timeline {
					if c.Type == "comment" && strconv.FormatInt(c.ID, 10) == r.PathValue("sub") && !issue.deleted {
						reply(w, c)
						return
					}
				}
			}
			http.NotFound(w, r)
			return
		}
		issue := f.issue(w, r)
		if issue == nil {
			return
		}
		res := []*apiTimeline{}
		if r.URL.Query().Get("page") == "1" {
			res = issue.timeline
		}
		reply(w, res)
	})
	mux.HandleFunc("GET "+repo+"/labels", func(w http.ResponseWriter, r *http.Request) {
		res := []apiLabel{}
		if r.URL.Query().Get("page") == "1" {
			res = f.labels
		}
		reply(w, res)
	})
	mux.HandleFunc("POST "+repo+"/labels", func(w http.ResponseWriter, r *http.Request) {
		var opt struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&opt)
		l := apiLabel{ID: int64(len(f.labels) + 1), Name: opt.Name}
		f.labels = append(f.labels, l)
		reply(w, l)
	})
	mux.HandleFunc("PUT "+repo+"/issues/{n}/labels", func(w http.ResponseWriter, r *http.Request) {
		issue := f.issue(w, r)
		if issue == nil {
			return
		}
		var opt struct{ Labels []int64 }
		_ = json.NewDecoder(r.Body).Decode(&opt)
		var labels []apiLabel
		for _, l := range f.labels {
			if slices.Contains(opt.Labels, l.ID) {
				labels = append(labels, l)
			}
		}
		u := f.user(r)
		has := func(set []apiLabel, l apiLabel) bool { return slices.Contains(set, l) }
		for _, l := range labels {
			if !has(issue.Labels, l) {
				f.event(issue, &apiTimeline{Type: "label", User: u, Body: "1", Label: &l})
			}
		}
		for _, l := range issue.Labels {
			if !has(labels, l) {
				f.event(issue, &apiTimeline{Type: "label", User: u, Body: "", Label: &l})
			}
		}
		issue.Labels = append([]apiLabel{}, labels...)
		issue.Updated = f.now()
		reply(w, issue.Labels)
	})

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.outsiders[f.user(r).Login] && strings.HasPrefix(r.URL.Path, "/api/v1/repos/") {
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// The following are called with f.mu held.

func (f *fakeGitea) now() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}

func (f *fakeGitea) user(r *http.Request) *apiUser {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
	if u, ok := f.users[token]; ok {
		return u
	}
	login := strings.TrimPrefix(token, "token-")
	u := &apiUser{ID: int64(len(f.users) + 1), Login: login}
	f.users[token] = u
	return u
}

func (f *fakeGitea) issue(w http.ResponseWriter, r *http.Request) *fakeIssue {
	for _, issue := range f.issues {
		if strconv.FormatInt(issue.Number, 10) == r.PathValue("n") && !issue.deleted {
			return issue
		}
	}
	http.NotFound(w, r)
	return nil
}

func (f *fakeGitea) event(issue *fakeIssue, e *apiTimeline) {
	f.lastID++
	e.ID, e.Created, e.Updated = f.lastID, f.now(), f.now()
	issue.timeline = append(issue.timeline, e)
	issue.Updated = f.now()
}

// giteaHarness runs the conformance suite on a Gitea, fake or real, with
// a token per user: "bot" is an admin, "alice", "bob" and "carol" aren't.
type giteaHarness struct {
	baseURL string
	tokens  map[string]string
	// newRepo returns a new repository, owned by bot.
	newRepo func(t *testing.T) string
}

// counter counts the writes of the adapter, and can make one fail after
// it's done, like a connection lost.
type counter struct {
	mu       sync.Mutex
	writes   int
	failNext bool
}

func (c *counter) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if req.Method == http.MethodGet {
		return resp, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	if c.failNext && err == nil {
		c.failNext = false
		_ = resp.Body.Close()
		return nil, errors.New("connection lost")
	}
	return resp, err
}

type giteaForge struct {
	*remote
	h       giteaHarness
	counter *counter
}

func (h giteaHarness) newForge(t *testing.T) synctest.Forge {
	project := h.newRepo(t)
	c := &counter{}
	r, err := newRemote(context.Background(), &http.Client{Transport: c}, h.baseURL, "bot", project, "bot", []string{h.tokens["bot"]})
	require.NoError(t, err)
	return giteaForge{remote: r, h: h, counter: c}
}

func (g giteaForge) UserAct(login string) synctest.Actor {
	token, ok := g.h.tokens[login]
	if !ok {
		panic("no token for " + login)
	}
	return giteaActor{g: g, token: token}
}

func (g giteaForge) Issue(number string) *core.Issue {
	for issue, err := range g.Issues(context.Background(), time.Time{}) {
		if err == nil && issue.Number == number {
			return issue
		}
	}
	return nil
}

func (g giteaForge) Writes() int {
	g.counter.mu.Lock()
	defer g.counter.mu.Unlock()
	return g.counter.writes
}

func (g giteaForge) FailNextWrite() {
	g.counter.mu.Lock()
	defer g.counter.mu.Unlock()
	g.counter.failNext = true
}

// giteaActor acts on Gitea through its API, with its own token.
type giteaActor struct {
	g     giteaForge
	token string
}

func (a giteaActor) call(token, method, path string, body, res any) {
	// an actor isn't the adapter: its writes aren't counted
	client := api{http: http.DefaultClient, base: a.g.base}
	if err := client.do(context.Background(), token, method, a.g.repoPath(path), body, res); err != nil {
		panic(err)
	}
}

func (a giteaActor) CreateIssue(title, body string) core.Ref {
	var gi apiIssue
	a.call(a.token, http.MethodPost, "/issues", map[string]any{"title": title, "body": body}, &gi)
	return issueRef(gi)
}

func (a giteaActor) Comment(issue core.Ref, body string) core.Ref {
	var c apiTimeline
	a.call(a.token, http.MethodPost, "/issues/"+issue.Number+"/comments", map[string]any{"body": body}, &c)
	return core.Ref{ID: strconv.FormatInt(c.ID, 10), URL: c.HTMLURL}
}

func (a giteaActor) EditText(issue, target core.Ref, body string) {
	if target.ID == issue.ID {
		a.call(a.token, http.MethodPatch, "/issues/"+issue.Number, map[string]any{"body": body}, nil)
		return
	}
	a.call(a.token, http.MethodPatch, "/issues/comments/"+target.ID, map[string]any{"body": body}, nil)
}

func (a giteaActor) SetStatus(issue core.Ref, status common.Status) {
	state := map[common.Status]string{common.OpenStatus: "open", common.ClosedStatus: "closed"}[status]
	a.call(a.token, http.MethodPatch, "/issues/"+issue.Number, map[string]any{"state": state}, nil)
}

func (a giteaActor) SetTitle(issue core.Ref, title string) {
	a.call(a.token, http.MethodPatch, "/issues/"+issue.Number, map[string]any{"title": title}, nil)
}

func (a giteaActor) SetLabels(issue core.Ref, labels []string) {
	ids := []int64{}
	for _, l := range labels {
		id, err := a.g.labelID(context.Background(), a.g.accounts[0], l)
		if err != nil {
			panic(err)
		}
		ids = append(ids, id)
	}
	a.call(a.token, http.MethodPut, "/issues/"+issue.Number+"/labels", map[string]any{"labels": ids}, nil)
}

func (a giteaActor) Delete(issue core.Ref) {
	// only an admin can delete an issue
	a.call(a.g.h.tokens["bot"], http.MethodDelete, "/issues/"+issue.Number, nil, nil)
}

func TestGiteaConformance(t *testing.T) {
	tokens := map[string]string{}
	for _, login := range []string{"bot", "alice", "bob", "carol"} {
		tokens[login] = "token-" + login
	}
	synctest.Run(t, synctest.Harness{
		NewForge: func(t *testing.T, name string) synctest.Forge {
			fake := newFakeGitea(t)
			return giteaHarness{baseURL: fake.URL + "/", tokens: tokens, newRepo: func(*testing.T) string { return "project" }}.newForge(t)
		},
		LoginKey: metaKeyGiteaLogin,
	})
}

// TestGiteaConformanceLive runs the conformance suite on a real Gitea, whose
// URL is GITEA_TEST_URL, with the tokens of users bot (an admin), alice, bob
// and carol in GITEA_TEST_TOKEN_<LOGIN>.
func TestGiteaConformanceLive(t *testing.T) {
	baseURL := os.Getenv("GITEA_TEST_URL")
	if baseURL == "" {
		t.Skip("GITEA_TEST_URL missing")
	}
	tokens := map[string]string{}
	for _, login := range []string{"bot", "alice", "bob", "carol"} {
		tokens[login] = os.Getenv("GITEA_TEST_TOKEN_" + strings.ToUpper(login))
		require.NotEmpty(t, tokens[login], "GITEA_TEST_TOKEN_"+strings.ToUpper(login))
	}
	h := giteaHarness{baseURL: baseURL, tokens: tokens}
	h.newRepo = func(t *testing.T) string {
		name := fmt.Sprintf("git-bug-sync-%d", rand.Int63())
		r, err := newRemote(context.Background(), http.DefaultClient, baseURL, "bot", name, "bot", []string{tokens["bot"]})
		require.NoError(t, err)
		require.NoError(t, r.do(context.Background(), tokens["bot"], http.MethodPost, "/user/repos", map[string]any{"name": name}, nil))
		// the team: on Gitea, only people with write access set labels
		for _, login := range []string{"alice", "bob", "carol"} {
			require.NoError(t, r.do(context.Background(), tokens["bot"], http.MethodPut, r.repoPath("/collaborators/"+login), map[string]any{"permission": "write"}, nil))
		}
		t.Cleanup(func() {
			_ = r.do(context.Background(), tokens["bot"], http.MethodDelete, r.repoPath(""), nil, nil)
		})
		return name
	}
	synctest.Run(t, synctest.Harness{
		NewForge: func(t *testing.T, name string) synctest.Forge { return h.newForge(t) },
		LoginKey: metaKeyGiteaLogin,
	})
}

func TestSplitURL(t *testing.T) {
	base, owner, project, err := splitURL("https://codeberg.org/forgejo/forgejo.git", "")
	require.NoError(t, err)
	require.Equal(t, []string{"https://codeberg.org/", "forgejo", "forgejo"}, []string{base, owner, project})

	base, owner, project, err = splitURL("https://example.org/gitea/me/repo", "https://example.org/gitea")
	require.NoError(t, err)
	require.Equal(t, []string{"https://example.org/gitea/", "me", "repo"}, []string{base, owner, project})

	_, _, _, err = splitURL("https://example.org/me", "")
	require.Error(t, err)
	_, _, _, err = splitURL("https://example.org/a/b", "https://other.org/")
	require.Error(t, err)
}

// Review I3: Gitea answers 404 to a token that can't see a private
// repository, as to a deleted issue. That must not mark the bug as deleted on
// the forge.
func TestGiteaNotFoundByOutsiderIsNotGone(t *testing.T) {
	fake := newFakeGitea(t)
	fake.outsiders["alice"] = true
	r, err := newRemote(context.Background(), http.DefaultClient, fake.URL+"/", "bot", "project", "bot",
		[]string{"token-bot", "token-alice"})
	require.NoError(t, err)
	g := giteaForge{remote: r, h: giteaHarness{baseURL: fake.URL + "/", tokens: map[string]string{"bot": "token-bot"}}, counter: &counter{}}
	ref := g.UserAct("bot").CreateIssue("title", "body")

	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	defer backend.Close()
	alice, err := backend.Identities().NewRaw("Alice", "", "alice", "", nil, map[string]string{metaKeyGiteaLogin: "alice"})
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(alice))
	bot, err := backend.Identities().NewRaw("Bot", "", "bot", "", nil, map[string]string{metaKeyGiteaLogin: "bot"})
	require.NoError(t, err)
	sync := func() {
		e, err := core.NewEngine(context.Background(), backend, g, core.SyncOptions{Foreign: true, DefaultLogin: "bot", LoginKey: metaKeyGiteaLogin})
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
