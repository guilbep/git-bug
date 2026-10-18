package core_test

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/synctest"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/entities/identity"
	"github.com/git-bug/git-bug/entity"
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
		if r.Event == core.ImportEventError {
			require.NoError(t, r.Err, r.String())
		}
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

// A dry run that would attach an issue by its marker doesn't plan to
// create another: the real run records the link before pushing.
func TestDryRunKnowsWhatPullWouldAttach(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	b, _, err := repo.Bugs().New("local", "body")
	require.NoError(t, err)
	issue, err := f.CreateIssue(context.Background(), bot, "local", core.Render("body", b.Id(), nil, nil, nil))
	require.NoError(t, err)

	e, err := core.NewEngine(context.Background(), repo, f, core.SyncOptions{
		Foreign: true, DefaultLogin: "bot", LoginKey: "fake-login", DryRun: true,
	})
	require.NoError(t, err)
	for r := range e.Pull(context.Background(), time.Time{}) {
		require.NotEqual(t, core.ImportEventError, r.Event, r.String())
	}
	var planned []string
	for r := range e.Push(context.Background()) {
		require.NotEqual(t, core.ExportEventError, r.Event, r.String())
		planned = append(planned, r.String())
	}
	require.Len(t, planned, 1)
	require.NotContains(t, planned[0], "would create")
	require.Contains(t, planned[0], issue.URL)
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
}

// pulled pulls a forge issue created by alice, and returns its bug.
func pulled(t *testing.T, f *synctest.Fake, repo *cache.RepoCache) (core.Ref, *cache.BugCache) {
	issue := f.UserAct("alice").CreateIssue("title", "body")
	pull(t, repo, f)
	return issue, onlyBug(t, repo)
}

// markPushed records on op what a push of it does.
func markPushed(t *testing.T, b *cache.BugCache, opId entity.Id, tags map[string]string) {
	meta := map[string]string{"sync:" + key + ":pushed": strconv.FormatInt(time.Now().Unix(), 10)}
	for k, v := range tags {
		meta["sync:"+key+":"+k] = v
	}
	_, err := b.SetMetadata(opId, meta)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
}

func warnings(results []core.ImportResult) int {
	n := 0
	for _, r := range results {
		if r.Event == core.ImportEventWarning {
			n++
		}
	}
	return n
}

func TestPullClaimsEcho(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	issue, b := pulled(t, f, repo)

	// a push of a local close
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	closeOp, err := b.CloseRaw(me, time.Now().Unix(), nil)
	require.NoError(t, err)
	require.NoError(t, f.SetStatus(context.Background(), bot, issue, common.ClosedStatus))
	markPushed(t, b, closeOp.Id(), map[string]string{"status": "closed"})

	pull(t, repo, f)
	ops := len(onlyBug(t, repo).Snapshot().Operations)
	pull(t, repo, f)

	snap := onlyBug(t, repo).Snapshot()
	require.Equal(t, 1, countOps[*bug.SetStatusOperation](snap), "the echo is claimed, not imported")
	require.Len(t, snap.Operations, ops, "and stays claimed")
}

// countOps counts the operations of a type.
func countOps[T any](snap *bug.Snapshot) int {
	n := 0
	for _, op := range snap.Operations {
		if _, ok := op.(T); ok {
			n++
		}
	}
	return n
}

func TestPullClaimsSeveralLabelEvents(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	issue, b := pulled(t, f, repo)

	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	_, op, err := b.ChangeLabelsRaw(me, time.Now().Unix(), []string{"a", "b"}, nil, nil)
	require.NoError(t, err)
	// a forge emitting one event per label
	require.NoError(t, f.ChangeLabels(context.Background(), bot, issue, []string{"a"}, nil))
	require.NoError(t, f.ChangeLabels(context.Background(), bot, issue, []string{"b"}, nil))
	markPushed(t, b, op.Id(), map[string]string{"labels": "a\nb"})

	pull(t, repo, f)
	ops := len(onlyBug(t, repo).Snapshot().Operations)
	pull(t, repo, f)

	snap := onlyBug(t, repo).Snapshot()
	require.Equal(t, 1, countOps[*bug.LabelChangeOperation](snap))
	require.Len(t, snap.Operations, ops)
}

func TestPullImportsTokenOwnerOwnChange(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	issue, _ := pulled(t, f, repo)

	// the bot account's owner closes the issue on the forge, nothing pushed
	f.UserAct("bot").SetStatus(issue, common.ClosedStatus)
	pull(t, repo, f)

	require.Equal(t, common.ClosedStatus, onlyBug(t, repo).Snapshot().Status)
}

func TestPullForgeEditWithoutEvent(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{NoEditEvents: true}, bot)
	alice := f.UserAct("alice")
	issue := alice.CreateIssue("title", "body")
	comment := alice.Comment(issue, "hello")
	pull(t, repo, f)
	before := len(onlyBug(t, repo).Snapshot().Operations)

	alice.EditText(issue, comment, "hello, edited")
	alice.EditText(issue, issue, "body, edited")
	pull(t, repo, f)
	pull(t, repo, f)

	snap := onlyBug(t, repo).Snapshot()
	require.Len(t, snap.Operations, before+2)
	require.Equal(t, "body, edited", snap.Comments[0].Message)
	require.Equal(t, "hello, edited", snap.Comments[1].Message)
	require.Equal(t, "alice", snap.Comments[1].Author.Login())
}

func TestPullKeepsUnpushedLocalEdit(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{NoEditEvents: true}, bot)
	alice := f.UserAct("alice")
	issue := alice.CreateIssue("title", "body")
	alice.Comment(issue, "hello")
	pull(t, repo, f)

	b := onlyBug(t, repo)
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	_, err = b.EditCommentRaw(me, time.Now().Unix(), b.Snapshot().Comments[1].CombinedId(), "local edit", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())

	pull(t, repo, f)
	require.Equal(t, "local edit", onlyBug(t, repo).Snapshot().Comments[1].Message)
}

func TestPullConflictLaterWins(t *testing.T) {
	for _, tc := range []struct {
		name      string
		localTime time.Time
		want      string
	}{
		{"forge later", time.Now().Add(-time.Hour), "forge edit"},
		{"local later", time.Now().Add(time.Hour), "local edit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepo(t)
			f := synctest.NewFake(key, "fake.test", synctest.Quirks{NoEditEvents: true}, bot)
			alice := f.UserAct("alice")
			issue := alice.CreateIssue("title", "body")
			comment := alice.Comment(issue, "hello")
			pull(t, repo, f)

			b := onlyBug(t, repo)
			me, err := repo.GetUserIdentity()
			require.NoError(t, err)
			_, err = b.EditCommentRaw(me, tc.localTime.Unix(), b.Snapshot().Comments[1].CombinedId(), "local edit", nil)
			require.NoError(t, err)
			require.NoError(t, b.Commit())
			alice.EditText(issue, comment, "forge edit")

			results := pull(t, repo, f)
			require.Equal(t, tc.want, onlyBug(t, repo).Snapshot().Comments[1].Message)
			require.Equal(t, 1, warnings(results), "the conflict is reported")
		})
	}
}

func TestPullConverged(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	issue, b := pulled(t, f, repo)

	// renamed the same way on both sides
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	_, err = b.SetTitleRaw(me, time.Now().Unix(), "same", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	f.UserAct("alice").SetTitle(issue, "same")
	before := len(b.Snapshot().Operations)

	results := pull(t, repo, f)
	snap := onlyBug(t, repo).Snapshot()
	require.Equal(t, "same", snap.Title)
	require.Zero(t, warnings(results))
	// the forge's event is replayed, nothing else
	require.Len(t, snap.Operations, before+1)
}

func TestPullMergesLabels(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	alice := f.UserAct("alice")
	issue := alice.CreateIssue("title", "body")
	alice.SetLabels(issue, []string{"a"})
	pull(t, repo, f)

	b := onlyBug(t, repo)
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	_, err = b.ForceChangeLabelsRaw(me, time.Now().Unix(), nil, []string{"a"}, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	alice.SetLabels(issue, []string{"a", "b"})

	pull(t, repo, f)
	require.Equal(t, []common.Label{"b"}, onlyBug(t, repo).Snapshot().Labels)
}

// A push alone, without a pull first, sends git-bug's label changes, not its
// set: what the forge changed since the last pull stays, and the next sync
// brings it in as the forge's change.
func TestPushAloneKeepsForgeLabels(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	alice := f.UserAct("alice")
	issue := alice.CreateIssue("title", "body")
	alice.SetLabels(issue, []string{"a", "b", "c"})
	pull(t, repo, f)

	// since the pull, the forge adds x and removes b, git-bug adds y and
	// removes c
	alice.SetLabels(issue, []string{"a", "c", "x"})
	b := onlyBug(t, repo)
	_, _, err := b.ChangeLabels([]string{"y"}, []string{"c"})
	require.NoError(t, err)
	require.NoError(t, b.Commit())

	dry, err := core.NewEngine(context.Background(), repo, f, core.SyncOptions{
		Foreign: true, DefaultLogin: "bot", LoginKey: "fake-login", DryRun: true,
	})
	require.NoError(t, err)
	var planned []string
	for r := range dry.Push(context.Background()) {
		planned = append(planned, r.String())
	}
	require.Len(t, planned, 1)
	require.Contains(t, planned[0], "would send: labels +y -c")

	push(t, repo, f, true)
	require.Equal(t, []string{"a", "x", "y"}, f.Issue(issue.Number).Labels)
	writes := f.Writes()

	results := pull(t, repo, f)
	require.Zero(t, warnings(results), "no conflict")
	require.Equal(t, []common.Label{"a", "x", "y"}, onlyBug(t, repo).Snapshot().Labels)
	push(t, repo, f, true)
	require.Equal(t, writes, f.Writes(), "nothing more sent")
	ops := len(onlyBug(t, repo).Snapshot().Operations)
	sync(t, repo, f)
	require.Equal(t, writes, f.Writes())
	require.Len(t, onlyBug(t, repo).Snapshot().Operations, ops)
}

func push(t *testing.T, repo *cache.RepoCache, f *synctest.Fake, foreign bool) []core.ExportResult {
	var res []core.ExportResult
	for r := range engine(t, repo, f, foreign).Push(context.Background()) {
		if r.Event == core.ExportEventError {
			require.NoError(t, r.Err, r.String())
		}
		res = append(res, r)
	}
	return res
}

// botIdentity is the identity of the bot account's owner, as `bridge new` tags it.
func botIdentity(t *testing.T, repo *cache.RepoCache) *cache.IdentityCache {
	i, err := repo.Identities().NewRaw("Bot", "", "bot", "", nil, map[string]string{"fake-login": "bot"})
	require.NoError(t, err)
	return i
}

func TestPushNewBug(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	botId := botIdentity(t, repo)

	b, _, err := repo.Bugs().NewRaw(me, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).Unix(), "title", "body @bob #1", nil, nil)
	require.NoError(t, err)
	_, _, err = b.AddCommentRaw(me, time.Now().Unix(), "mine", nil, nil)
	require.NoError(t, err)
	_, _, err = b.AddCommentRaw(botId, time.Now().Unix(), "the bot's", nil, nil)
	require.NoError(t, err)
	_, err = b.CloseRaw(me, time.Now().Unix(), nil)
	require.NoError(t, err)
	_, _, err = b.ChangeLabelsRaw(me, time.Now().Unix(), []string{"bug"}, nil, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())

	push(t, repo, f, true)

	issue := f.Issue("1")
	require.NotNil(t, issue)
	require.Equal(t, 5, f.Writes(), "issue, 2 comments, status, labels")
	require.Equal(t, "title", issue.Title)
	require.Equal(t, common.ClosedStatus, issue.Status)
	require.Equal(t, []string{"bug"}, issue.Labels)
	require.Equal(t, "bot", issue.Author.Login)

	// written in git-bug, by someone without an account: attributed, left as is
	require.Contains(t, issue.Body, "*Posted by Me on 2026-01-02 via git-bug*")
	require.Contains(t, issue.Body, "body @bob #1")
	require.Equal(t, "body @bob #1", core.Unrender(issue.Body))
	require.Contains(t, issue.Comments[0].Body, "*Posted by Me on")
	// the bot's own comment: not attributed
	require.NotContains(t, issue.Comments[1].Body, "Posted by")
	require.Equal(t, "the bot's", core.Unrender(issue.Comments[1].Body))

	// a second push writes nothing
	push(t, repo, f, true)
	require.Equal(t, 5, f.Writes())

	// and a pull brings nothing back
	pull(t, repo, f)
	snap := onlyBug(t, repo).Snapshot()
	require.Equal(t, 1, countOps[*bug.SetStatusOperation](snap))
	require.Equal(t, 1, countOps[*bug.LabelChangeOperation](snap))
	require.Equal(t, 1, countOps[*bug.CreateOperation](snap))
	require.Len(t, snap.Comments, 3)
	require.Len(t, repo.Bugs().AllIds(), 1)
}

func TestPushWithoutSync(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	botId := botIdentity(t, repo)
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)

	// from another forge: never pushed without sync
	_, _, err = repo.Bugs().NewRaw(botId, time.Now().Unix(), "foreign", "body", nil, map[string]string{core.MetaKeyOrigin: "other"})
	require.NoError(t, err)
	// by someone without an account: nothing to post as
	_, _, err = repo.Bugs().NewRaw(me, time.Now().Unix(), "not mine", "body", nil, nil)
	require.NoError(t, err)
	// by the bot, with a comment from someone without an account
	b, _, err := repo.Bugs().NewRaw(botId, time.Now().Unix(), "mine", "body", nil, nil)
	require.NoError(t, err)
	_, _, err = b.AddCommentRaw(me, time.Now().Unix(), "skipped", nil, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())

	push(t, repo, f, false)

	require.Equal(t, 1, f.Writes())
	require.Equal(t, "mine", f.Issue("1").Title)
	require.Empty(t, f.Issue("1").Comments)
}

func TestPushTranslatesRefs(t *testing.T) {
	repo := newRepo(t)
	src := synctest.NewFake("src.test/1", "src.test", synctest.Quirks{}, bot)
	dst := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	alice := src.UserAct("alice")
	alice.CreateIssue("first", "first issue")
	alice.CreateIssue("second", "see #1 and #7, cc @carol")

	// pulled from src, pushed to dst
	var res []core.ImportResult
	for r := range engine(t, repo, src, true).Pull(context.Background(), time.Time{}) {
		res = append(res, r)
	}
	push(t, repo, dst, true)

	var second *core.Issue
	for _, n := range []string{"1", "2"} {
		if i := dst.Issue(n); i.Title == "second" {
			second = i
		}
	}
	require.NotNil(t, second)
	first := "1"
	if dst.Issue("1").Title == "second" {
		first = "2"
	}
	require.Contains(t, second.Body, "see #"+first+" and [#7](https://src.test/issues/7), cc `@carol`")
	require.Contains(t, second.Body, "*Originally posted by alice on")
	require.Contains(t, second.Body, "[view on fake](https://src.test/issues/2)")
}

// A forge's edit that came without event, so reconciled, was still written
// on that forge: elsewhere its mentions are defused and its refs translated.
func TestPushRendersReconciledEdit(t *testing.T) {
	repo := newRepo(t)
	src := synctest.NewFake("src.test/1", "src.test", synctest.Quirks{NoEditEvents: true}, bot)
	dst := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	alice := src.UserAct("alice")
	issue := alice.CreateIssue("title", "body")
	pull(t, repo, src)
	push(t, repo, dst, true)

	alice.EditText(issue, issue, "cc @carol, see #9")
	pull(t, repo, src)
	push(t, repo, dst, true)
	require.Contains(t, dst.Issue("1").Body, "cc `@carol`, see [#9](https://src.test/issues/9)")
}

func sync(t *testing.T, repo *cache.RepoCache, f *synctest.Fake) []core.ExportResult {
	pull(t, repo, f)
	return push(t, repo, f, true)
}

func TestPushUpdates(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	b, _, err := repo.Bugs().NewRaw(me, time.Now().Unix(), "title", "body", nil, nil)
	require.NoError(t, err)
	commentId, _, err := b.AddCommentRaw(me, time.Now().Unix(), "comment", nil, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	sync(t, repo, f)
	writes := f.Writes()

	_, err = b.SetTitleRaw(me, time.Now().Unix(), "new title", nil)
	require.NoError(t, err)
	_, err = b.CloseRaw(me, time.Now().Unix(), nil)
	require.NoError(t, err)
	_, _, err = b.ChangeLabelsRaw(me, time.Now().Unix(), []string{"x"}, nil, nil)
	require.NoError(t, err)
	_, _, err = b.EditCreateCommentRaw(me, time.Now().Unix(), "new body", nil)
	require.NoError(t, err)
	_, err = b.EditCommentRaw(me, time.Now().Unix(), commentId, "new comment", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())

	sync(t, repo, f)
	require.Equal(t, writes+5, f.Writes())
	issue := f.Issue("1")
	require.Equal(t, "new title", issue.Title)
	require.Equal(t, common.ClosedStatus, issue.Status)
	require.Equal(t, []string{"x"}, issue.Labels)
	require.Equal(t, "new body", core.Unrender(issue.Body))
	require.Equal(t, "new comment", core.Unrender(issue.Comments[0].Body))

	// the next sync claims the echoes of these writes, then all is stable
	sync(t, repo, f)
	ops := len(onlyBug(t, repo).Snapshot().Operations)
	sync(t, repo, f)
	require.Equal(t, writes+5, f.Writes())
	snap := onlyBug(t, repo).Snapshot()
	require.Equal(t, 1, countOps[*bug.SetStatusOperation](snap))
	require.Equal(t, 1, countOps[*bug.SetTitleOperation](snap))
	require.Equal(t, 2, countOps[*bug.EditCommentOperation](snap))
	require.Len(t, snap.Operations, ops)
}

func TestPushEditsAsPoster(t *testing.T) {
	repo := newRepo(t)
	owner := core.Account{User: core.User{ID: "2", Login: "owner", Name: "Owner"}}
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{AuthorOnlyEdits: true}, bot, owner)
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	ownerId, err := repo.Identities().NewRaw("Owner", "", "owner", "", nil, map[string]string{"fake-login": "owner"})
	require.NoError(t, err)

	b, _, err := repo.Bugs().NewRaw(me, time.Now().Unix(), "title", "body", nil, nil)
	require.NoError(t, err)
	// posted by the bot on my behalf
	commentId, _, err := b.AddCommentRaw(me, time.Now().Unix(), "mine", nil, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	sync(t, repo, f)

	// the owner edits it in git-bug: the forge only lets the bot edit it
	_, err = b.EditCommentRaw(ownerId, time.Now().Unix(), commentId, "fixed by the owner", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	sync(t, repo, f)

	got := f.Issue("1").Comments[0].Body
	require.Equal(t, "fixed by the owner", core.Unrender(got))
	require.Contains(t, got, "*Posted by Me on", "still attributed to its author")
}

func TestPushRecoversFromFailedWrite(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{FailAfterWrite: true}, bot)
	_, _, err := repo.Bugs().New("title", "body")
	require.NoError(t, err)

	var failed bool
	for r := range engine(t, repo, f, true).Push(context.Background()) {
		failed = failed || r.Event == core.ExportEventError
	}
	require.True(t, failed, "the write was reported as failed")
	require.NotNil(t, f.Issue("1"), "but was done")

	sync(t, repo, f)
	sync(t, repo, f)
	require.Nil(t, f.Issue("2"), "not created twice")
	require.Len(t, repo.Bugs().AllIds(), 1, "not imported back")
}

func TestPushRetriesRateLimit(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{RateLimitOnce: true}, bot)
	_, _, err := repo.Bugs().New("title", "body")
	require.NoError(t, err)

	push(t, repo, f, true)
	require.NotNil(t, f.Issue("1"))
}

func TestPushDeletedIssue(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	b, _, err := repo.Bugs().New("title", "body")
	require.NoError(t, err)
	sync(t, repo, f)
	f.UserAct("alice").Delete(core.Ref{ID: f.Issue("1").ID})

	_, err = b.SetTitleRaw(b.Snapshot().Author, time.Now().Unix(), "renamed", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())

	results := sync(t, repo, f)
	warned := false
	for _, r := range results {
		warned = warned || r.Event == core.ExportEventWarning
	}
	require.True(t, warned)
	writes := f.Writes()

	sync(t, repo, f)
	require.Equal(t, writes, f.Writes(), "not retried")
	require.Len(t, repo.Bugs().AllIds(), 1, "kept in git-bug")
}

func TestPushErrorOnOneBugOnly(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	_, _, err = repo.Bugs().NewRaw(me, time.Now().Unix(), "first", "body", nil, nil)
	require.NoError(t, err)
	_, _, err = repo.Bugs().NewRaw(me, time.Now().Unix()+1, "second", "body", nil, nil)
	require.NoError(t, err)
	f.FailNextWrite()

	var errs int
	for r := range engine(t, repo, f, true).Push(context.Background()) {
		if r.Event == core.ExportEventError {
			errs++
		}
	}
	require.Equal(t, 1, errs)
	require.Equal(t, "second", f.Issue("2").Title, "the other bug is still pushed")
}

func TestPushRefusedEditWarnsOnce(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{AuthorOnlyEdits: true}, bot)
	alice := f.UserAct("alice")
	issue := alice.CreateIssue("alice's", "body")
	alice.Comment(issue, "alice's comment")
	pull(t, repo, f)

	// an edit of alice's comment, which only alice can edit on the forge
	b := onlyBug(t, repo)
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	_, err = b.EditCommentRaw(me, time.Now().Unix(), b.Snapshot().Comments[1].CombinedId(), "edited", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())

	countWarnings := func(results []core.ExportResult) int {
		n := 0
		for _, r := range results {
			if r.Event == core.ExportEventWarning {
				n++
			}
		}
		return n
	}
	require.Equal(t, 1, countWarnings(push(t, repo, f, true)))
	require.Zero(t, countWarnings(push(t, repo, f, true)), "not tried again")
	require.Equal(t, "alice's comment", f.Issue("1").Comments[0].Body)
}

func TestPullSkipsDuplicateByMarker(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	b, _, err := repo.Bugs().New("local", "body")
	require.NoError(t, err)
	push(t, repo, f, true)

	// a retried write created the issue twice on the forge
	_, err = f.CreateIssue(context.Background(), bot, "local", core.Render("body", b.Id(), nil, nil, nil))
	require.NoError(t, err)

	require.Equal(t, 1, warnings(pull(t, repo, f)))
	require.Len(t, repo.Bugs().AllIds(), 1)
}

// Review C1: when a value changed on both sides, the later change wins, even
// if the forge's issue was updated again later for something else.
func TestPullConflictIgnoresUnrelatedActivity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		forge func(alice synctest.Actor, issue core.Ref)
		local func(b *cache.BugCache, me identity.Interface, at int64) error
		got   func(snap *bug.Snapshot) string
		want  string
	}{
		{
			name:  "title",
			forge: func(alice synctest.Actor, issue core.Ref) { alice.SetTitle(issue, "forge title") },
			local: func(b *cache.BugCache, me identity.Interface, at int64) error {
				_, err := b.SetTitleRaw(me, at, "local title", nil)
				return err
			},
			got:  func(snap *bug.Snapshot) string { return snap.Title },
			want: "local title",
		},
		{
			name:  "body",
			forge: func(alice synctest.Actor, issue core.Ref) { alice.EditText(issue, issue, "forge body") },
			local: func(b *cache.BugCache, me identity.Interface, at int64) error {
				_, _, err := b.EditCreateCommentRaw(me, at, "local body", nil)
				return err
			},
			got:  func(snap *bug.Snapshot) string { return snap.Comments[0].Message },
			want: "local body",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepo(t)
			f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
			t0 := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
			f.Clock = func() time.Time { return t0 }
			alice := f.UserAct("alice")
			issue := alice.CreateIssue("title", "body")
			pull(t, repo, f)

			// changed on the forge at t0+1h, in git-bug at t0+2h
			f.Clock = func() time.Time { return t0.Add(time.Hour) }
			tc.forge(alice, issue)
			b := onlyBug(t, repo)
			me, err := repo.GetUserIdentity()
			require.NoError(t, err)
			require.NoError(t, tc.local(b, me, t0.Add(2*time.Hour).Unix()))
			require.NoError(t, b.Commit())
			// then something unrelated on the forge, at t0+3h
			f.Clock = func() time.Time { return t0.Add(3 * time.Hour) }
			f.UserAct("bob").Comment(issue, "unrelated")

			results := pull(t, repo, f)
			require.Equal(t, tc.want, tc.got(onlyBug(t, repo).Snapshot()))
			require.Equal(t, 1, warnings(results), "one conflict, reported once")
		})
	}
}

// Review C2: without sync, nothing someone without an account wrote reaches
// the forge, not even through the text of someone who has one.
func TestPushWithoutSyncKeepsOthersText(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	botId := botIdentity(t, repo)
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)

	b, _, err := repo.Bugs().NewRaw(botId, time.Now().Unix(), "mine", "the bot's body", nil, nil)
	require.NoError(t, err)
	commentId, _, err := b.AddCommentRaw(botId, time.Now().Unix(), "the bot's words", nil, nil)
	require.NoError(t, err)
	// edited by someone without an account before the first push
	_, _, err = b.EditCreateCommentRaw(me, time.Now().Unix(), "body by Me", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	push(t, repo, f, false)

	// and after
	_, err = b.EditCommentRaw(me, time.Now().Unix(), commentId, "words of Me", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	push(t, repo, f, false)

	issue := f.Issue("1")
	require.Equal(t, "the bot's body", core.Unrender(issue.Body))
	require.Equal(t, "the bot's words", core.Unrender(issue.Comments[0].Body))
}

// legacyFake is a forge whose pre-sync bridge tagged operations "fake-id".
type legacyFake struct {
	*synctest.Fake
}

func (legacyFake) Legacy(meta, _ map[string]string) (string, bool) {
	id, ok := meta["fake-id"]
	return id, ok
}

// Review I2: the old bridges could leave two bugs for one issue, one pushed
// and one pulled. Pull picks one, the one pulled from this forge, warns, and
// goes on.
func TestPullLegacyDuplicatePair(t *testing.T) {
	repo := newRepo(t)
	f := legacyFake{synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)}
	alice := f.UserAct("alice")
	issue := alice.CreateIssue("title", "body")
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)

	_, _, err = repo.Bugs().NewRaw(me, time.Now().Unix(), "title", "body", nil, map[string]string{"fake-id": issue.ID})
	require.NoError(t, err)
	pulled, _, err := repo.Bugs().NewRaw(me, time.Now().Unix()+1, "title", "body", nil,
		map[string]string{"fake-id": issue.ID, core.MetaKeyOrigin: "fake"})
	require.NoError(t, err)

	alice.Comment(issue, "new on the forge")
	e, err := core.NewEngine(context.Background(), repo, f, core.SyncOptions{Foreign: true, DefaultLogin: "bot", LoginKey: "fake-login"})
	require.NoError(t, err)
	warned := 0
	for r := range e.Pull(context.Background(), time.Time{}) {
		require.NotEqual(t, core.ImportEventError, r.Event, r.String())
		if r.Event == core.ImportEventWarning {
			warned++
		}
	}

	require.Equal(t, 1, warned)
	got, err := repo.Bugs().Resolve(pulled.Id())
	require.NoError(t, err)
	require.Len(t, got.Snapshot().Comments, 2, "the new comment went to the pulled copy")
}

// githubFake is a forge whose pre-sync bridge was GitHub's, which tagged
// operations with github-id and bugs with github-url.
type githubFake struct {
	*synctest.Fake
}

func (githubFake) Legacy(meta, _ map[string]string) (string, bool) {
	id, ok := meta["github-id"]
	return id, ok
}

// A title pulled by the old GitHub bridge goes back to GitHub as it is, and
// to another forge with its refs translated, the issue's own number too.
func TestPushLegacyTitleRefs(t *testing.T) {
	repo := newRepo(t)
	gh := githubFake{synctest.NewFake("github.com/o/r", "github.com", synctest.Quirks{}, bot)}
	dst := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	dst.UserAct("bob").CreateIssue("already there", "body")
	issue := gh.UserAct("alice").CreateIssue("wrong example (#1)", "body")
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	_, _, err = repo.Bugs().NewRaw(me, time.Now().Unix(), "wrong example (#1)", "body", nil,
		map[string]string{"github-id": issue.ID, "github-url": issue.URL, core.MetaKeyOrigin: "github"})
	require.NoError(t, err)
	opts := core.SyncOptions{Foreign: true, DefaultLogin: "bot", LoginKey: "fake-login"}

	ghEngine, err := core.NewEngine(context.Background(), repo, gh, opts)
	require.NoError(t, err)
	for r := range ghEngine.Push(context.Background()) {
		require.NotEqual(t, core.ExportEventError, r.Event, r.String())
	}
	require.Zero(t, gh.Writes(), "nothing to send to GitHub")

	push(t, repo, dst, true)
	require.Equal(t, "wrong example (#2)", dst.Issue("2").Title)
}

// Review: only git-bug's accounts post markers. Anyone else's marker is just
// text: a stranger can't attach their issue or comment to a bug, and then
// edit it through the forge.
func TestPullIgnoresStrangersMarkers(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	b, _, err := repo.Bugs().New("local", "body")
	require.NoError(t, err)
	_, comment, err := b.AddComment("local comment")
	require.NoError(t, err)
	require.NoError(t, b.Commit())

	mallory := f.UserAct("mallory")
	issue := mallory.CreateIssue("local", core.Render("body", b.Id(), nil, nil, nil))
	mallory.Comment(issue, core.Render("local comment", comment.Id(), nil, nil, nil))

	pull(t, repo, f)

	require.Len(t, repo.Bugs().AllIds(), 2, "imported as a bug of its own")
	got, err := repo.Bugs().Resolve(b.Id())
	require.NoError(t, err)
	_, onForge := got.Snapshot().GetCreateMetadata("sync:" + key + ":id")
	require.False(t, onForge)
}

// Tools built on the old bridges, such as git-board, find a bug's forge issue
// through "<kind>-url" on its create operation.
func TestIssueURLForOtherTools(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	pulled := f.UserAct("alice").CreateIssue("pulled", "body")

	attached, _, err := repo.Bugs().New("attached", "body")
	require.NoError(t, err)
	marked, err := f.CreateIssue(context.Background(), bot, "attached", core.Render("body", attached.Id(), nil, nil, nil))
	require.NoError(t, err)
	pull(t, repo, f)

	pushed, _, err := repo.Bugs().New("pushed", "body")
	require.NoError(t, err)
	push(t, repo, f, true)

	urls := map[entity.Id]string{}
	for _, id := range repo.Bugs().AllIds() {
		b, err := repo.Bugs().Resolve(id)
		require.NoError(t, err)
		url, ok := b.Snapshot().GetCreateMetadata("sync:" + key + ":url")
		require.True(t, ok)
		excerpt, err := repo.Bugs().ResolveExcerpt(id)
		require.NoError(t, err)
		require.Equal(t, url, excerpt.CreateMetadata["fake-url"], b.Snapshot().Title)
		urls[id] = url
	}
	require.Len(t, urls, 3)
	require.Equal(t, marked.URL, urls[attached.Id()])
	require.NotEmpty(t, urls[pushed.Id()])
	require.Contains(t, slices.Collect(maps.Values(urls)), pulled.URL)
}

// pushBug pushes one bug, and fails the test on an error.
func pushBug(t *testing.T, repo *cache.RepoCache, f *synctest.Fake, b *cache.BugCache) {
	for r := range engine(t, repo, f, true).PushBug(context.Background(), b) {
		require.NotEqual(t, core.ExportEventError, r.Event, r.String())
	}
}

// legacyBug is a bug a pre-sync bridge pulled, or pushed, with its metadata.
func legacyBug(t *testing.T, repo *cache.RepoCache, title, message string, meta map[string]string) *cache.BugCache {
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	b, _, err := repo.Bugs().NewRaw(me, time.Now().Unix(), title, message, nil, meta)
	require.NoError(t, err)
	return b
}

// A #5 written on a project, by its pre-sync bridge, is that project's
// issue, not another project's #5.
func TestPushTranslatesLegacyRefsOfTheirProject(t *testing.T) {
	repo := newRepo(t)
	dst := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot)
	for _, p := range []struct{ project, number string }{{"one/repo", "3"}, {"Two/Repo", "4"}} {
		legacyBug(t, repo, p.project+"#5", "body", map[string]string{
			"github-id": p.project + "-5", "github-url": "https://github.com/" + p.project + "/issues/5",
			"sync:" + key + ":number": p.number,
		})
	}
	b := legacyBug(t, repo, "refers", "see #5", map[string]string{
		"github-id": "one-7", "github-url": "https://github.com/one/repo/issues/7",
	})

	pushBug(t, repo, dst, b)
	body := dst.Issue("1").Body
	require.Contains(t, body, "see #3")
	require.Contains(t, body, "[view on GitHub](https://github.com/one/repo/issues/7)")
}

// A bug the GitHub pre-sync bridge pulled is that GitHub project's issue
// wherever it is referred to: GitLab's #77 is GitHub's #2518.
func TestPushTranslatesRefsToLegacyIssues(t *testing.T) {
	for name, onGitlab := range map[string]map[string]string{
		"synced": {"sync:gitlab.test/42:number": "77"},
		"legacy": {"gitlab-id": "77", "gitlab-url": "https://gitlab.test/group/proj/-/issues/77", "gitlab-project-id": "42", "gitlab-base-url": "https://gitlab.test/"},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newRepo(t)
			gitlab := synctest.NewFake("gitlab.test/42", "gitlab.test", synctest.Quirks{}, bot)
			github := synctest.NewFake("github.com/owner/repo", "github.com", synctest.Quirks{}, bot)
			meta := map[string]string{"github-id": "I_2518", "github-url": "https://github.com/Owner/Repo/issues/2518"}
			maps.Copy(meta, onGitlab)
			legacyBug(t, repo, "bug 228", "body", meta)
			gitlab.UserAct("alice").CreateIssue("refers", "see #77")
			pull(t, repo, gitlab)

			b, err := repo.Bugs().ResolveMatcher(func(x *cache.BugExcerpt) bool { return x.Title == "refers" })
			require.NoError(t, err)
			pushBug(t, repo, github, b)
			require.Contains(t, github.Issue("1").Body, "see #2518")
		})
	}
}
