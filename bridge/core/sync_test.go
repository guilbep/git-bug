package core_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/synctest"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/repository/repotest"
)

const key = "fake.test/1"

var bot = core.Account{User: core.User{ID: "1", Login: "bot", Name: "Bot"}}

func newRepo(t *testing.T) *cache.RepoCache {
	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })

	me, err := backend.Identities().New("Me", "me@example.com")
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(me))
	return backend
}

func engine(t *testing.T, repo *cache.RepoCache, f *synctest.Fake, foreign bool) *core.Engine {
	e, err := core.NewEngine(context.Background(), repo, f, core.SyncOptions{
		Foreign:      foreign,
		DefaultLogin: "bot",
		LoginKey:     "fake-login",
	})
	require.NoError(t, err)
	return e
}

func pull(t *testing.T, repo *cache.RepoCache, f *synctest.Fake) []core.ImportResult {
	var res []core.ImportResult
	for r := range engine(t, repo, f, true).Pull(context.Background(), time.Time{}) {
		require.NoError(t, r.Err, r.String())
		res = append(res, r)
	}
	return res
}

func onlyBug(t *testing.T, repo *cache.RepoCache) *cache.BugCache {
	ids := repo.Bugs().AllIds()
	require.Len(t, ids, 1)
	b, err := repo.Bugs().Resolve(ids[0])
	require.NoError(t, err)
	return b
}

func TestPullNewIssue(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	alice, bob := f.UserAct("alice"), f.UserAct("bob")

	issue := alice.CreateIssue("title", "body")
	bob.Comment(issue, "first")
	alice.Comment(issue, "second")
	bob.SetStatus(issue, common.ClosedStatus)
	alice.SetTitle(issue, "renamed")
	alice.SetLabels(issue, []string{"bug"})

	pull(t, repo, f)

	b := onlyBug(t, repo)
	snap := b.Snapshot()
	require.Equal(t, "renamed", snap.Title)
	require.Equal(t, common.ClosedStatus, snap.Status)
	require.Equal(t, []common.Label{"bug"}, snap.Labels)
	require.Len(t, snap.Comments, 3)
	require.Equal(t, "body", snap.Comments[0].Message)
	require.Equal(t, "alice", snap.Comments[0].Author.Login())
	require.Equal(t, "first", snap.Comments[1].Message)
	require.Equal(t, "bob", snap.Comments[1].Author.Login())
	require.Len(t, snap.Operations, 6)

	origin, _ := snap.GetCreateMetadata(core.MetaKeyOrigin)
	require.Equal(t, "fake", origin)
	id, _ := snap.GetCreateMetadata("sync:" + key + ":id")
	require.Equal(t, issue.ID, id)
	number, _ := snap.GetCreateMetadata("sync:" + key + ":number")
	require.Equal(t, "1", number)

	// pulling again changes nothing
	pull(t, repo, f)
	require.Len(t, onlyBug(t, repo).Snapshot().Operations, 6)
}

func TestPullAttachesByMarker(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	b, _, err := repo.Bugs().New("local", "body")
	require.NoError(t, err)

	// a push created the issue, but died before recording it
	issue, err := f.CreateIssue(context.Background(), bot, "local", core.Render("body", b.Id(), nil, nil, nil))
	require.NoError(t, err)

	pull(t, repo, f)

	got := onlyBug(t, repo)
	require.Equal(t, b.Id(), got.Id())
	id, ok := got.Snapshot().GetCreateMetadata("sync:" + key + ":id")
	require.True(t, ok)
	require.Equal(t, issue.ID, id)
}

func TestPullUnrendersText(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	attr := &core.Attribution{Name: "Alice", When: time.Now(), Source: "GitHub", URL: "https://x/1"}
	// mirrored by another git-bug repository, unknown here
	d := f.Dialect()
	posted := core.Render("hi @bob", "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef", attr, &d, d.FormatRef)
	f.UserAct("bot").CreateIssue("title", posted)

	pull(t, repo, f)
	require.Equal(t, "hi @bob", onlyBug(t, repo).Snapshot().Comments[0].Message)
	var _ *bug.Snapshot
}
