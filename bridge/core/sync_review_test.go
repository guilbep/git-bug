package core_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/synctest"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entities/identity"
	"github.com/git-bug/git-bug/entity"
	"github.com/git-bug/git-bug/entity/dag"
)

var alice = core.Account{User: core.User{ID: "2", Login: "alice", Name: "Alice"}}

// reviewSetup is a bug on a fake forge's issue 1, whose pull request 7 is
// at commit c1 and changes main.go. Alice has a token, Me doesn't.
func reviewSetup(t *testing.T) (*cache.RepoCache, *synctest.Fake, *cache.BugCache, *cache.IdentityCache) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{}, bot, alice)
	f.OpenPull("7", "c1", "main.go")
	aliceId, err := repo.Identities().NewRaw("Alice", "", "alice", "", nil, map[string]string{"fake-login": "alice"})
	require.NoError(t, err)
	b, _, err := repo.Bugs().New("title", "body")
	require.NoError(t, err)
	push(t, repo, f, true)
	require.NotNil(t, f.Issue("1"))
	return repo, f, b, aliceId
}

// anchor is the metadata of a line comment on pull request 7.
func anchor(path, line, side, commit string) map[string]string {
	return map[string]string{
		"review:path": path, "review:line": line, "review:side": side,
		"review:commit": commit, "review:pr": key + "#7",
	}
}

func comment(t *testing.T, b *cache.BugCache, author identity.Interface, text string, meta map[string]string) entity.CombinedId {
	id, _, err := b.AddCommentRaw(author, time.Now().Unix(), text, nil, meta)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	return id
}

func me(t *testing.T, repo *cache.RepoCache) identity.Interface {
	i, err := repo.GetUserIdentity()
	require.NoError(t, err)
	return i
}

func withPulls(pulls ...string) func(entity.Id) []string {
	return func(entity.Id) []string { return pulls }
}

func reviewEngine(t *testing.T, repo *cache.RepoCache, r core.Remote, pullsOf func(entity.Id) []string, dryRun bool) *core.Engine {
	e, err := core.NewEngine(context.Background(), repo, r, core.SyncOptions{
		Foreign: true, DefaultLogin: "bot", LoginKey: "fake-login", PullsOf: pullsOf, DryRun: dryRun,
	})
	require.NoError(t, err)
	return e
}

func pullPRs(t *testing.T, repo *cache.RepoCache, r core.Remote, pullsOf func(entity.Id) []string) []string {
	var res []string
	for r := range reviewEngine(t, repo, r, pullsOf, false).Pull(context.Background(), time.Time{}) {
		require.NotEqual(t, core.ImportEventError, r.Event, r.String())
		res = append(res, r.String())
	}
	return res
}

func TestPushLineCommentReplyAndReview(t *testing.T) {
	repo, f, b, aliceId := reviewSetup(t)
	line := comment(t, b, aliceId, "why?", anchor("main.go", "3", "new", "c1"))
	reply := anchor("main.go", "3", "new", "c1")
	reply["review:reply-to"] = string(line)
	comment(t, b, me(t, repo), "because", reply)
	comment(t, b, aliceId, "looks good", map[string]string{
		"review:state": "APPROVE", "review:commit": "c1", "review:pr": key + "#7",
	})

	push(t, repo, f, true)

	got, _ := f.Pull("7")
	require.Len(t, got, 3)
	require.Equal(t, "main.go", got[0].Path)
	require.Equal(t, 3, got[0].Line)
	require.Equal(t, "new", got[0].Side)
	require.Equal(t, "c1", got[0].Commit)
	require.Equal(t, "alice", got[0].Author.Login, "under its author's token")
	require.Equal(t, "why?", core.Unrender(got[0].Body))

	require.Equal(t, got[0].ID, got[1].ReplyTo.ID, "a reply to the first")
	require.Equal(t, "bot", got[1].Author.Login, "Me has no token: the default one, attributed")
	require.Contains(t, got[1].Body, "*Posted by Me on")

	require.Equal(t, "APPROVE", got[2].State)
	require.Equal(t, "c1", got[2].Commit)
	require.Equal(t, "alice", got[2].Author.Login)
	require.Equal(t, "looks good", core.Unrender(got[2].Body))

	require.Empty(t, f.Issue("1").Comments, "nothing on the issue")
	id, ok := opOf(t, b, line).GetMetadata("sync:" + key + ":id")
	require.True(t, ok)
	require.Equal(t, got[0].ID, id)

	writes := f.Writes()
	push(t, repo, f, true)
	require.Equal(t, writes, f.Writes(), "nothing sent twice")
}

func opOf(t *testing.T, b *cache.BugCache, id entity.CombinedId) dag.Operation {
	c, err := b.Snapshot().SearchComment(id)
	require.NoError(t, err)
	for _, op := range b.Snapshot().Operations {
		if op.Id() == c.TargetId() {
			return op
		}
	}
	t.Fatal("no operation")
	return nil
}

// GitHub refuses a line comment outside the diff, or on a commit the pull
// request no longer has: it goes as a comment, its anchor written out.
func TestPushLineCommentOutsideDiff(t *testing.T) {
	repo, f, b, aliceId := reviewSetup(t)
	comment(t, b, aliceId, "not in the diff", anchor("other.go", "9", "new", "c1"))
	comment(t, b, aliceId, "on an old commit", anchor("main.go", "2", "old", "c0"))

	push(t, repo, f, true)

	got, _ := f.Pull("7")
	require.Len(t, got, 2)
	require.Empty(t, got[0].Path)
	require.Contains(t, got[0].Body, "`other.go` line 9")
	require.Contains(t, got[0].Body, "not in the diff")
	require.Empty(t, got[1].Path)
	require.Contains(t, got[1].Body, "`main.go` line 2 (old side) at c0")
	require.Empty(t, f.Issue("1").Comments)

	writes := f.Writes()
	push(t, repo, f, true)
	require.Equal(t, writes, f.Writes())
}

// An approval is the person's own: never sent with the default token.
func TestPushApprovalNeedsItsAuthorsToken(t *testing.T) {
	repo, f, b, _ := reviewSetup(t)
	comment(t, b, me(t, repo), "ok", map[string]string{
		"review:state": "APPROVE", "review:commit": "c1", "review:pr": key + "#7",
	})
	comment(t, b, me(t, repo), "some thoughts", map[string]string{
		"review:state": "COMMENT", "review:commit": "c1", "review:pr": key + "#7",
	})

	var reasons []string
	for _, r := range push(t, repo, f, true) {
		reasons = append(reasons, r.Reason)
	}

	got, _ := f.Pull("7")
	require.Len(t, got, 1)
	require.Equal(t, "COMMENT", got[0].State)
	require.Equal(t, "bot", got[0].Author.Login)
	require.Contains(t, strings.Join(reasons, "\n"), "missing author token: approval")
}

// What a pull request holds is never an issue comment, on any forge: not
// sent, nor edited, to an issue.
func TestReviewCommentsNeverReachAnIssue(t *testing.T) {
	repo, f, b, aliceId := reviewSetup(t)
	other := synctest.NewFake("other.test/1", "other.test", synctest.Quirks{}, bot)
	line := comment(t, b, aliceId, "why?", anchor("main.go", "3", "new", "c1"))
	comment(t, b, aliceId, "pulled from a pull request", map[string]string{
		"sync:github.com/o/r:pr-comment": "PRRC_1", "review:pr": "github.com/o/r#3",
	})
	comment(t, b, aliceId, "a comment", nil)

	// a forge without pull requests, like GitLab or Gitea
	push(t, repo, issuesOnly{other}, true)
	require.Len(t, other.Issue("1").Comments, 1)
	require.Equal(t, "a comment", core.Unrender(other.Issue("1").Comments[0].Body))

	// a forge with them: the line comment goes to the pull request only
	push(t, repo, f, true)
	require.Len(t, f.Issue("1").Comments, 1)
	got, _ := f.Pull("7")
	require.Len(t, got, 1)

	// an edit of it isn't sent as an issue comment's
	_, err := b.EditCommentRaw(aliceId, time.Now().Unix(), line, "why, really?", nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	writes := f.Writes()
	push(t, repo, f, true)
	push(t, repo, issuesOnly{other}, true)
	require.Equal(t, writes, f.Writes())
	require.Len(t, other.Issue("1").Comments, 1)
}

// issuesOnly is a forge without pull requests.
type issuesOnly struct{ core.Remote }

func TestDryRunReportsReviews(t *testing.T) {
	repo, f, b, aliceId := reviewSetup(t)
	comment(t, b, aliceId, "why?", anchor("main.go", "3", "new", "c1"))
	comment(t, b, aliceId, "ok", map[string]string{"review:state": "APPROVE", "review:commit": "c1", "review:pr": key + "#7"})
	comment(t, b, aliceId, "a comment", nil)
	f.PullAct("carol", "7", core.PullComment{Body: "from GitHub"})
	writes := f.Writes()

	var report []string
	e := reviewEngine(t, repo, f, withPulls("7"), true)
	for r := range e.Pull(context.Background(), time.Time{}) {
		report = append(report, r.String())
	}
	for r := range e.Push(context.Background()) {
		report = append(report, r.String())
	}

	all := strings.Join(report, "\n")
	require.Contains(t, all, "would pull 1 comments from pull request #7")
	require.Contains(t, all, "would send: 1 comments")
	require.Contains(t, all, "would send to pull request #7: 1 line comments, 1 reviews")
	require.Equal(t, writes, f.Writes())
	require.Len(t, b.Snapshot().Comments, 4)
}

func TestPullPullRequestComments(t *testing.T) {
	repo, f, b, _ := reviewSetup(t)
	line := f.PullAct("carol", "7", core.PullComment{Body: "why?", Path: "main.go", Line: 3, Side: "new", Commit: "c1"})
	f.PullAct("Copilot", "7", core.PullComment{Body: "I agree", Path: "main.go", ReplyTo: line})
	f.PullAct("carol", "7", core.PullComment{Body: "fix it", State: "REQUEST_CHANGES", Commit: "c1"})
	f.PullAct("github-actions[bot]", "7", core.PullComment{Body: "CI passed"})
	writes := f.Writes()

	pullPRs(t, repo, f, withPulls("7"))

	snap := b.Snapshot()
	require.Len(t, snap.Comments, 5)
	ops := commentOps(snap)
	origin := "sync:" + key + ":pr-comment"

	first := ops[1]
	require.Equal(t, "why?", first.Message)
	require.Equal(t, "carol", first.Author().Login())
	require.Equal(t, line.ID, first.Metadata[origin])
	require.Equal(t, key+"#7", first.Metadata["review:pr"])
	require.Equal(t, "main.go", first.Metadata["review:path"])
	require.Equal(t, "3", first.Metadata["review:line"])
	require.Equal(t, "new", first.Metadata["review:side"])
	require.Equal(t, "c1", first.Metadata["review:commit"])

	reply := ops[2]
	require.Equal(t, "Copilot", reply.Author().Login())
	require.Equal(t, string(entity.CombineIds(b.Id(), first.Id())), reply.Metadata["review:reply-to"])
	require.Equal(t, "main.go", reply.Metadata["review:path"])

	review := ops[3]
	require.Equal(t, "REQUEST_CHANGES", review.Metadata["review:state"])
	require.Equal(t, "c1", review.Metadata["review:commit"])
	require.Equal(t, "fix it", review.Message)

	conversation := ops[4]
	require.Equal(t, "CI passed", conversation.Message)
	require.Equal(t, key+"#7", conversation.Metadata["review:pr"])
	require.NotEmpty(t, conversation.Metadata[origin])
	for _, k := range []string{"review:path", "review:state"} {
		require.NotContains(t, conversation.Metadata, k)
	}

	// once, and never sent anywhere
	pullPRs(t, repo, f, withPulls("7"))
	require.Len(t, b.Snapshot().Comments, 5)
	push(t, repo, f, true)
	require.Equal(t, writes, f.Writes())
	require.Empty(t, f.Issue("1").Comments)

	// an edit on the forge is followed
	f.EditPullComment("7", line.ID, "why, really?")
	pullPRs(t, repo, f, withPulls("7"))
	require.Equal(t, "why, really?", b.Snapshot().Comments[1].Message)
	pullPRs(t, repo, f, withPulls("7"))
	require.Len(t, b.Snapshot().Operations, len(snap.Operations)+1)
}

func commentOps(snap *bug.Snapshot) []*bug.AddCommentOperation {
	res := []*bug.AddCommentOperation{nil}
	for _, op := range snap.Operations {
		if c, ok := op.(*bug.AddCommentOperation); ok {
			res = append(res, c)
		}
	}
	return res
}

func TestPullWithoutPullsOfImportsNoPullComments(t *testing.T) {
	repo, f, b, _ := reviewSetup(t)
	f.PullAct("carol", "7", core.PullComment{Body: "CI passed"})

	pullPRs(t, repo, f, nil)

	require.Len(t, b.Snapshot().Comments, 1)
}

// A push that dies after GitHub took a comment, before recording it, doesn't
// post it again: the next pull finds it by its marker.
func TestPushLineCommentRecoversFromFailedWrite(t *testing.T) {
	repo, f, b, aliceId := reviewSetup(t)
	comment(t, b, aliceId, "why?", anchor("main.go", "3", "new", "c1"))

	f.FailNextWrite()
	for r := range reviewEngine(t, repo, f, nil, false).Push(context.Background()) {
		_ = r
	}
	got, _ := f.Pull("7")
	require.Len(t, got, 1)

	pullPRs(t, repo, f, withPulls("7"))
	push(t, repo, f, true)

	got, _ = f.Pull("7")
	require.Len(t, got, 1, "not posted twice")
	require.Len(t, b.Snapshot().Comments, 2, "not imported back")
}

// Resolving a thread in git-bug resolves it on the forge, the last change
// winning.
func TestPushResolvesThread(t *testing.T) {
	repo, f, b, aliceId := reviewSetup(t)
	line := comment(t, b, aliceId, "why?", anchor("main.go", "3", "new", "c1"))
	push(t, repo, f, true)
	got, _ := f.Pull("7")

	resolve := func(v string) {
		_, err := b.SetMetadataRaw(aliceId, time.Now().Unix(), opOf(t, b, line).Id(),
			map[string]string{"review:resolved": v})
		require.NoError(t, err)
		require.NoError(t, b.Commit())
	}

	resolve("true")
	push(t, repo, f, true)
	_, resolved := f.Pull("7")
	require.True(t, resolved[got[0].ID])
	writes := f.Writes()
	push(t, repo, f, true)
	require.Equal(t, writes, f.Writes())

	resolve("false")
	push(t, repo, f, true)
	_, resolved = f.Pull("7")
	require.False(t, resolved[got[0].ID])
}

// A comment anchored before its pull request is known waits: git-board
// adds review:pr later, and the next push sends it.
func TestPushLineCommentWaitsForItsPull(t *testing.T) {
	repo, f, b, aliceId := reviewSetup(t)
	meta := anchor("main.go", "3", "new", "c1")
	delete(meta, "review:pr")
	line := comment(t, b, aliceId, "why?", meta)

	var report []string
	for r := range reviewEngine(t, repo, f, nil, true).Push(context.Background()) {
		report = append(report, r.String())
	}
	require.Contains(t, strings.Join(report, "\n"), "would send later: 1 line comments (waits for a pull request)")
	push(t, repo, f, true)
	got, _ := f.Pull("7")
	require.Empty(t, got)
	require.Empty(t, f.Issue("1").Comments)

	_, err := b.SetMetadataRaw(aliceId, time.Now().Unix(), opOf(t, b, line).Id(), map[string]string{"review:pr": key + "#7"})
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	push(t, repo, f, true)
	got, _ = f.Pull("7")
	require.Len(t, got, 1)
	require.Equal(t, "main.go", got[0].Path)
}
