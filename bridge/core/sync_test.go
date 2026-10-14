package core_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/synctest"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entities/common"
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
	require.NoError(t, f.SetLabels(context.Background(), bot, issue, []string{"a"}))
	require.NoError(t, f.SetLabels(context.Background(), bot, issue, []string{"a", "b"}))
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
