package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	gitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entities/identity"
	"github.com/git-bug/git-bug/entity"
	"github.com/git-bug/git-bug/repository"
	"github.com/git-bug/git-bug/repository/repotest"
	"github.com/git-bug/git-bug/util/text"
)

type fakeWrite struct{ token, method, path string }

// fakeGitlab serves the part of the GitLab API used by the bridge, for one
// project, from memory.
type fakeGitlab struct {
	*httptest.Server

	mu         sync.Mutex
	users      map[string]*gitlab.User // by token
	issues     []*gitlab.Issue
	notes      map[int64][]*gitlab.Note // by issue iid
	nextNoteID int64
	writes     []fakeWrite
}

func newFakeGitlab(t *testing.T, users map[string]*gitlab.User) *fakeGitlab {
	f := &fakeGitlab{users: users, notes: map[int64][]*gitlab.Note{}, nextNoteID: 1000}

	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	iid := func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.PathValue("iid"), 10, 64)
		return v
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		for _, u := range f.users {
			if strconv.FormatInt(u.ID, 10) == r.PathValue("id") {
				reply(w, u)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /api/v4/projects/{pid}/issues", func(w http.ResponseWriter, r *http.Request) {
		reply(w, f.issues)
	})
	mux.HandleFunc("POST /api/v4/projects/{pid}/issues", func(w http.ResponseWriter, r *http.Request) {
		var opt gitlab.CreateIssueOptions
		_ = json.NewDecoder(r.Body).Decode(&opt)
		u := f.users[r.Header.Get("PRIVATE-TOKEN")]
		now := time.Now()
		n := int64(len(f.issues) + 1)
		issue := &gitlab.Issue{
			ID: 100 + n, IID: n, ProjectID: 1,
			Title: *opt.Title, Description: *opt.Description, State: "opened",
			Author:    &gitlab.IssueAuthor{ID: u.ID, Username: u.Username, Name: u.Name},
			CreatedAt: &now, UpdatedAt: &now,
			WebURL: fmt.Sprintf("%s/issues/%d", f.URL, n),
		}
		f.issues = append(f.issues, issue)
		reply(w, issue)
	})
	mux.HandleFunc("GET /api/v4/projects/{pid}/issues/{iid}/notes", func(w http.ResponseWriter, r *http.Request) {
		reply(w, f.notes[iid(r)])
	})
	mux.HandleFunc("POST /api/v4/projects/{pid}/issues/{iid}/notes", func(w http.ResponseWriter, r *http.Request) {
		var opt gitlab.CreateIssueNoteOptions
		_ = json.NewDecoder(r.Body).Decode(&opt)
		u := f.users[r.Header.Get("PRIVATE-TOKEN")]
		now := time.Now()
		note := &gitlab.Note{
			ID: f.nextNoteID, Body: *opt.Body,
			Author:    gitlab.NoteAuthor{ID: u.ID, Username: u.Username, Name: u.Name},
			CreatedAt: &now, UpdatedAt: &now,
		}
		f.nextNoteID++
		f.notes[iid(r)] = append(f.notes[iid(r)], note)
		reply(w, note)
	})
	mux.HandleFunc("PUT /api/v4/projects/{pid}/issues/{iid}/notes/{id}", func(w http.ResponseWriter, r *http.Request) {
		var opt gitlab.UpdateIssueNoteOptions
		_ = json.NewDecoder(r.Body).Decode(&opt)
		for _, note := range f.notes[iid(r)] {
			if strconv.FormatInt(note.ID, 10) == r.PathValue("id") {
				note.Body = *opt.Body
				reply(w, note)
				return
			}
		}
		http.NotFound(w, r)
	})
	empty := func(w http.ResponseWriter, r *http.Request) { reply(w, []any{}) }
	mux.HandleFunc("GET /api/v4/projects/{pid}/issues/{iid}/resource_label_events", empty)
	mux.HandleFunc("GET /api/v4/projects/{pid}/issues/{iid}/resource_state_events", empty)

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodGet {
			f.writes = append(f.writes, fakeWrite{r.Header.Get("PRIVATE-TOKEN"), r.Method, r.URL.Path})
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func TestGitlabExportForeign(t *testing.T) {
	ctx := context.Background()

	users := map[string]*gitlab.User{
		"bot-token":   {ID: 1, Username: "bot", Name: "Bot"},
		"owner-token": {ID: 2, Username: "owner", Name: "Owner"},
	}
	srv := newFakeGitlab(t, users)

	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	defer backend.Close()

	// two identities with a token: bot is the default-login, owner posts as themselves
	var owner *cache.IdentityCache
	for token, user := range users {
		i, err := backend.Identities().New(user.Name, user.Username+"@example.com")
		require.NoError(t, err)
		i.SetMetadata(metaKeyGitlabLogin, user.Username)
		require.NoError(t, i.Commit())

		cred := auth.NewToken(target, token)
		cred.SetMetadata(auth.MetaKeyLogin, user.Username)
		cred.SetMetadata(auth.MetaKeyBaseURL, srv.URL)
		require.NoError(t, auth.Store(repo, cred))

		if user.Username == "owner" {
			owner = i
		}
	}
	require.NoError(t, backend.SetUserIdentity(owner))

	// a bug imported from github, by alice who has no gitlab token
	alice, err := backend.Identities().NewRaw("Alice", "alice@example.com", "alice", "", nil,
		map[string]string{"github-login": "alice"})
	require.NoError(t, err)

	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix()
	b, _, err := backend.Bugs().NewRaw(alice, created, "foreign bug", "from github", nil, map[string]string{
		core.MetaKeyOrigin: "github",
		"github-id":        "I_1",
		"github-url":       "https://github.com/o/r/issues/1",
	})
	require.NoError(t, err)
	aliceComment, _, err := b.AddCommentRaw(alice, created, "alice comment", nil, map[string]string{
		"github-id":  "IC_1",
		"github-url": "https://github.com/o/r/issues/1#issuecomment-1",
	})
	require.NoError(t, err)
	_, err = b.EditCommentRaw(alice, created, aliceComment, "alice comment edited", nil)
	require.NoError(t, err)
	_, _, err = b.AddCommentRaw(owner, created, "owner comment", nil, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())

	conf := core.Configuration{
		confKeyProjectID:     "1",
		confKeyGitlabBaseUrl: srv.URL,
		confKeyDefaultLogin:  "bot",
	}
	exportAll := func(conf core.Configuration) []core.ExportResult {
		exporter := &gitlabExporter{}
		require.NoError(t, exporter.Init(ctx, backend, conf))
		events, err := exporter.ExportAll(ctx, backend, time.Time{})
		require.NoError(t, err)
		var results []core.ExportResult
		for result := range events {
			require.NoError(t, result.Err)
			results = append(results, result)
		}
		return results
	}

	t.Run("skipped by default", func(t *testing.T) {
		results := exportAll(conf)
		require.Len(t, results, 1)
		require.Equal(t, "issue tagged with origin: github", results[0].Reason)
		require.Empty(t, srv.writes)
	})

	foreignConf := maps.Clone(conf)
	foreignConf[confKeyExportForeign] = "true"

	t.Run("exported with attribution", func(t *testing.T) {
		exportAll(foreignConf)

		// alice's text is posted by the default-login, the owner's by themselves
		require.Equal(t, []fakeWrite{
			{"bot-token", "POST", "/api/v4/projects/1/issues"},
			{"bot-token", "POST", "/api/v4/projects/1/issues/1/notes"},
			{"bot-token", "PUT", "/api/v4/projects/1/issues/1/notes/1000"},
			{"owner-token", "POST", "/api/v4/projects/1/issues/1/notes"},
		}, srv.writes)

		require.Len(t, srv.issues, 1)
		desc := srv.issues[0].Description
		require.Contains(t, desc, "*Originally posted by Alice on 2026-01-02, [view on github](https://github.com/o/r/issues/1)*")
		require.Equal(t, "from github", stripAttribution(text.Cleanup(desc)))

		notes := srv.notes[1]
		require.Len(t, notes, 2)
		require.Contains(t, notes[0].Body, "(https://github.com/o/r/issues/1#issuecomment-1)")
		require.Equal(t, "alice comment edited", stripAttribution(text.Cleanup(notes[0].Body)))
		require.Equal(t, "owner comment", notes[1].Body)

		baseUrl, _ := b.Snapshot().GetCreateMetadata(metaKeyGitlabBaseUrl)
		require.Equal(t, srv.URL, baseUrl)
	})

	t.Run("second push posts nothing", func(t *testing.T) {
		writes := len(srv.writes)
		exportAll(foreignConf)
		require.Len(t, srv.writes, writes)
	})

	t.Run("pull resolves to the same bug", func(t *testing.T) {
		numOps := len(b.Snapshot().Operations)

		importer := &gitlabImporter{}
		require.NoError(t, importer.Init(ctx, backend, foreignConf))
		events, err := importer.ImportAll(ctx, backend, time.Time{})
		require.NoError(t, err)
		for result := range events {
			require.NoError(t, result.Err)
		}

		require.Len(t, backend.Bugs().AllIds(), 1)
		require.Len(t, b.Snapshot().Operations, numOps)
	})

	// the pull created other identities for bot and owner, with the same login
	t.Run("push after pull posts nothing", func(t *testing.T) {
		writes := len(srv.writes)
		exportAll(conf)
		exportAll(foreignConf)
		require.Len(t, srv.writes, writes)
	})
}

func TestGitlabExportForeignNeedsDefaultToken(t *testing.T) {
	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	defer backend.Close()

	// the default-login has an identity, but no token
	bot, err := backend.Identities().New("Bot", "bot@example.com")
	require.NoError(t, err)
	bot.SetMetadata(metaKeyGitlabLogin, "bot")
	require.NoError(t, bot.Commit())

	err = (&gitlabExporter{}).Init(context.Background(), backend, core.Configuration{
		confKeyProjectID:     "1",
		confKeyGitlabBaseUrl: defaultBaseURL,
		confKeyDefaultLogin:  "bot",
		confKeyExportForeign: "true",
	})
	require.ErrorIs(t, err, ErrMissingIdentityToken)
}

func TestAttributionRoundTrip(t *testing.T) {
	alice, err := identity.NewIdentity(repository.NewMockRepo(), "Alice", "alice@example.com")
	require.NoError(t, err)
	ge := &gitlabExporter{exportForeign: true, identityClient: map[entity.Id]*gitlab.Client{}}

	for _, body := range []string{"text", "", "first\n\nsecond", "<!-- git-bug attribution -->"} {
		op := bug.NewAddCommentOp(alice, 0, body, nil)
		posted := ge.withAttribution(op, "github", body)
		require.NotEqual(t, body, posted)
		// as returned by gitlab, possibly with windows new lines
		require.Equal(t, body, stripAttribution(text.Cleanup(posted)))
		require.Equal(t, body, stripAttribution(text.Cleanup(strings.ReplaceAll(posted, "\n", "\r\n"))))
	}

	// text without the header is left alone
	require.Equal(t, "text", stripAttribution("text"))
}
