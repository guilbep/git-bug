package synctest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/repository/repotest"
)

// Harness gives the conformance suite forges of one kind to sync through a
// git-bug repository.
type Harness struct {
	// NewForge returns a new forge with one project, and an account whose
	// login is "bot" (the default login), so that every forge it returns
	// is a different project.
	NewForge func(t *testing.T, name string) Forge
	// LoginKey is the identity metadata `bridge new` tags token owners
	// with, for this kind of forge.
	LoginKey string
}

// Run runs the scenarios every adapter must pass: two projects synced through
// one git-bug repository, each scenario on new ones.
func Run(t *testing.T, h Harness) {
	for name, scenario := range map[string]func(*world){
		"mirror then stable":         mirrorThenStable,
		"issue opened on each side":  openedOnEachSide,
		"local edit survives":        localEditSurvives,
		"forge edit is pulled":       forgeEditIsPulled,
		"conflict keeps later edit":  conflictKeepsLater,
		"crash after a write":        crashAfterWrite,
		"labels merge":               labelsMerge,
		"token owner acts directly":  tokenOwnerActsDirectly,
		"refs never mirrored":        refsNeverMirrored,
		"deletion is not propagated": deletionNotPropagated,
		"one issue syncs alone":      oneIssueAlone,
	} {
		t.Run(name, func(t *testing.T) {
			scenario(newWorld(t, h))
		})
	}
}

type world struct {
	t    *testing.T
	h    Harness
	repo *cache.RepoCache
	a, b Forge
}

func newWorld(t *testing.T, h Harness) *world {
	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })

	me, err := backend.Identities().New("Me", "me@example.com")
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(me))

	return &world{t: t, h: h, repo: backend, a: h.NewForge(t, "a"), b: h.NewForge(t, "b")}
}

func (w *world) engine(f Forge) *core.Engine {
	e, err := core.NewEngine(context.Background(), w.repo, f, core.SyncOptions{
		Foreign:      true,
		DefaultLogin: "bot",
		LoginKey:     w.h.LoginKey,
	})
	require.NoError(w.t, err)
	return e
}

// sync pulls both forges, then pushes to both, as `bridge sync` does. It
// returns the number of warnings.
func (w *world) sync() int {
	warnings := 0
	for _, f := range []Forge{w.a, w.b} {
		for r := range w.engine(f).Pull(context.Background(), time.Time{}) {
			require.NotEqual(w.t, core.ImportEventError, r.Event, r.String())
			if r.Event == core.ImportEventWarning {
				warnings++
			}
		}
	}
	for _, f := range []Forge{w.a, w.b} {
		for r := range w.engine(f).Push(context.Background()) {
			require.NotEqual(w.t, core.ExportEventError, r.Event, r.String())
			if r.Event == core.ExportEventWarning {
				warnings++
			}
		}
	}
	return warnings
}

// stable checks that syncing again writes nothing anywhere, once echoes are
// claimed.
func (w *world) stable() {
	w.sync()
	writesA, writesB := w.a.Writes(), w.b.Writes()
	ops := w.opCount()
	w.sync()
	require.Equal(w.t, writesA, w.a.Writes(), "writes to a")
	require.Equal(w.t, writesB, w.b.Writes(), "writes to b")
	require.Equal(w.t, ops, w.opCount(), "operations in git-bug")
}

func (w *world) opCount() int {
	n := 0
	for _, id := range w.repo.Bugs().AllIds() {
		b, err := w.repo.Bugs().Resolve(id)
		require.NoError(w.t, err)
		n += len(b.Snapshot().Operations)
	}
	return n
}

// issue returns the forge's issue with the given title.
func (w *world) issue(f Forge, title string) *core.Issue {
	var found *core.Issue
	for issue, err := range f.Issues(context.Background(), time.Time{}) {
		require.NoError(w.t, err)
		if issue.Title == title {
			require.Nil(w.t, found, "two issues titled %q", title)
			found = issue
		}
	}
	require.NotNil(w.t, found, "no issue titled %q", title)
	return found
}

func (w *world) bugCount() int {
	return len(w.repo.Bugs().AllIds())
}

func (w *world) onlyBug() *cache.BugCache {
	ids := w.repo.Bugs().AllIds()
	require.Len(w.t, ids, 1)
	b, err := w.repo.Bugs().Resolve(ids[0])
	require.NoError(w.t, err)
	return b
}

func mirrorThenStable(w *world) {
	alice, bob := w.a.UserAct("alice"), w.a.UserAct("bob")
	first := alice.CreateIssue("first", "body")
	ref := alice.CreateIssue("title", "hi @carol, see #"+first.Number)
	comment := bob.Comment(ref, "a comment")
	bob.EditText(ref, comment, "a comment, edited")
	alice.SetTitle(ref, "renamed")
	alice.SetLabels(ref, []string{"bug"})
	alice.SetStatus(ref, common.ClosedStatus)

	w.sync()

	got := w.issue(w.b, "renamed")
	require.Equal(w.t, common.ClosedStatus, got.Status)
	require.Equal(w.t, []string{"bug"}, got.Labels)
	require.Contains(w.t, strings.ToLower(got.Body), "originally posted by alice", "attributed")
	require.Contains(w.t, got.Body, "`@carol`", "mention defused")
	require.Contains(w.t, got.Body, "see #"+w.issue(w.b, "first").Number, "the ref is the mirrored issue")
	require.Len(w.t, got.Comments, 1)
	require.Equal(w.t, "a comment, edited", core.Unrender(got.Comments[0].Body))
	require.Equal(w.t, 2, w.bugCount())

	w.stable()
}

func openedOnEachSide(w *world) {
	w.a.UserAct("alice").CreateIssue("from a", "body")
	w.b.UserAct("carol").CreateIssue("from b", "body")

	w.sync()

	require.Contains(w.t, strings.ToLower(w.issue(w.a, "from b").Body), "originally posted by carol")
	require.Contains(w.t, strings.ToLower(w.issue(w.b, "from a").Body), "originally posted by alice")
	require.Equal(w.t, 2, w.bugCount())
	w.stable()
}

func localEditSurvives(w *world) {
	ref := w.a.UserAct("alice").CreateIssue("title", "body")
	w.a.UserAct("bob").Comment(ref, "a comment")
	w.sync()

	b := w.onlyBug()
	me, err := w.repo.GetUserIdentity()
	require.NoError(w.t, err)
	_, err = b.EditCommentRaw(me, time.Now().Unix(), b.Snapshot().Comments[1].CombinedId(), "edited in git-bug", nil)
	require.NoError(w.t, err)
	require.NoError(w.t, b.Commit())

	w.sync()

	require.Equal(w.t, "edited in git-bug", w.onlyBug().Snapshot().Comments[1].Message)
	require.Equal(w.t, "edited in git-bug", core.Unrender(w.issue(w.b, "title").Comments[0].Body))
	w.stable()
}

func forgeEditIsPulled(w *world) {
	carol := w.b.UserAct("carol")
	ref := carol.CreateIssue("title", "body")
	comment := carol.Comment(ref, "a comment")
	w.sync()

	carol.EditText(ref, comment, "edited on b")
	carol.EditText(ref, ref, "body, edited on b")
	w.sync()

	snap := w.onlyBug().Snapshot()
	require.Equal(w.t, "body, edited on b", snap.Comments[0].Message)
	require.Equal(w.t, "edited on b", snap.Comments[1].Message)
	got := w.issue(w.a, "title")
	require.Equal(w.t, "body, edited on b", core.Unrender(got.Body))
	require.Equal(w.t, "edited on b", core.Unrender(got.Comments[0].Body))
	w.stable()
}

func conflictKeepsLater(w *world) {
	carol := w.b.UserAct("carol")
	ref := carol.CreateIssue("title", "body")
	comment := carol.Comment(ref, "a comment")
	w.sync()

	// edited in git-bug an hour ago, and on b now
	b := w.onlyBug()
	me, err := w.repo.GetUserIdentity()
	require.NoError(w.t, err)
	_, err = b.EditCommentRaw(me, time.Now().Add(-time.Hour).Unix(), b.Snapshot().Comments[1].CombinedId(), "older edit", nil)
	require.NoError(w.t, err)
	require.NoError(w.t, b.Commit())
	carol.EditText(ref, comment, "later edit")

	require.Positive(w.t, w.sync(), "the conflict is reported")

	require.Equal(w.t, "later edit", w.onlyBug().Snapshot().Comments[1].Message)
	require.Equal(w.t, "later edit", core.Unrender(w.issue(w.a, "title").Comments[0].Body))
	require.Equal(w.t, "later edit", core.Unrender(w.issue(w.b, "title").Comments[0].Body))
	w.stable()
}

func crashAfterWrite(w *world) {
	_, _, err := w.repo.Bugs().New("local", "body")
	require.NoError(w.t, err)
	w.a.FailNextWrite()

	// the push to a reports an error, though a did create the issue
	for r := range w.engine(w.a).Push(context.Background()) {
		_ = r
	}
	w.sync()

	w.issue(w.a, "local") // exactly one
	w.issue(w.b, "local")
	require.Equal(w.t, 1, w.bugCount())
	w.stable()
}

func labelsMerge(w *world) {
	alice := w.a.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	alice.SetLabels(ref, []string{"x"})
	w.sync()

	// added on a, removed on b, between two syncs
	alice.SetLabels(ref, []string{"x", "y"})
	onB := w.issue(w.b, "title")
	w.b.UserAct("carol").SetLabels(onB.Ref, nil)
	w.sync()

	require.Equal(w.t, []common.Label{"y"}, w.onlyBug().Snapshot().Labels)
	require.Equal(w.t, []string{"y"}, w.issue(w.a, "title").Labels)
	require.Equal(w.t, []string{"y"}, w.issue(w.b, "title").Labels)
	w.stable()
}

func tokenOwnerActsDirectly(w *world) {
	ref := w.a.UserAct("alice").CreateIssue("title", "body")
	w.sync()
	identities := len(w.repo.Identities().AllIds())

	// the bot account's owner, on the forge itself
	w.a.UserAct("bot").SetStatus(ref, common.ClosedStatus)
	w.sync()

	require.Equal(w.t, common.ClosedStatus, w.onlyBug().Snapshot().Status)
	require.Equal(w.t, common.ClosedStatus, w.issue(w.b, "title").Status)
	require.Equal(w.t, identities, len(w.repo.Identities().AllIds()), "no second identity")
	w.stable()
}

func refsNeverMirrored(w *world) {
	w.a.UserAct("alice").CreateIssue("title", "see #99, not group/x#1")
	w.sync()

	body := w.issue(w.b, "title").Body
	require.Contains(w.t, body, "group/x#1", "cross-project refs are left alone")
	require.NotContains(w.t, body, "see #99,", "not a ref to the wrong issue")
	require.Contains(w.t, body, "#99")
	w.stable()
	require.Equal(w.t, body, w.issue(w.b, "title").Body, "rendered once")
}

func deletionNotPropagated(w *world) {
	alice := w.a.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	w.sync()

	alice.Delete(ref)
	b := w.onlyBug()
	me, err := w.repo.GetUserIdentity()
	require.NoError(w.t, err)
	_, err = b.SetTitleRaw(me, time.Now().Unix(), "renamed", nil)
	require.NoError(w.t, err)
	require.NoError(w.t, b.Commit())

	require.Positive(w.t, w.sync(), "the deletion is reported")

	require.Equal(w.t, 1, w.bugCount())
	got := w.issue(w.b, "renamed")
	require.Equal(w.t, common.OpenStatus, got.Status)
	require.False(w.t, strings.Contains(got.Body, "deleted"))
	w.stable()
}

// oneIssueAlone syncs one issue (Fetch, PullIssue, PushBug): its changes
// come and go, no other issue is read.
func oneIssueAlone(w *world) {
	ctx := context.Background()
	alice := w.a.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	w.sync()
	alice.Comment(ref, "from a")
	alice.CreateIssue("another", "body")

	got, err := w.a.Fetch(ctx, ref)
	require.NoError(w.t, err)
	require.Equal(w.t, "title", got.Title)
	require.Len(w.t, got.Comments, 1)

	b := w.onlyBug()
	for _, f := range []Forge{w.a, w.b} {
		for r := range w.engine(f).PullIssue(ctx, b) {
			require.NotEqual(w.t, core.ImportEventError, r.Event, r.String())
		}
	}
	for _, f := range []Forge{w.a, w.b} {
		for r := range w.engine(f).PushBug(ctx, b) {
			require.NotEqual(w.t, core.ExportEventError, r.Event, r.String())
		}
	}
	require.Equal(w.t, 1, w.bugCount(), "the other issue wasn't read")
	require.Len(w.t, w.issue(w.b, "title").Comments, 1, "a's comment reached b")

	alice.Delete(ref)
	_, err = w.a.Fetch(ctx, ref)
	require.ErrorIs(w.t, err, core.ErrGone)
}
