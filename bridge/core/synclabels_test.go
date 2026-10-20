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
	"github.com/git-bug/git-bug/entities/common"
)

func exportWarnings(results []core.ExportResult) int {
	n := 0
	for _, r := range results {
		if r.Event == core.ExportEventWarning {
			n++
		}
	}
	return n
}

// steady syncs twice more and checks that nothing is written, warned or
// recorded anymore.
func steady(t *testing.T, repo *cache.RepoCache, f *synctest.Fake) {
	t.Helper()
	sync(t, repo, f)
	writes := f.Writes()
	ops := len(onlyBug(t, repo).Snapshot().Operations)
	require.Zero(t, warnings(pull(t, repo, f)))
	require.Zero(t, exportWarnings(push(t, repo, f, true)))
	require.Equal(t, writes, f.Writes(), "nothing more sent")
	require.Len(t, onlyBug(t, repo).Snapshot().Operations, ops, "nothing more recorded")
}

// planned returns what a dry run of a sync would do.
func planned(t *testing.T, repo *cache.RepoCache, f core.Remote) string {
	t.Helper()
	dry, err := core.NewEngine(context.Background(), repo, f, core.SyncOptions{
		Foreign: true, DefaultLogin: "bot", LoginKey: "fake-login", DryRun: true,
	})
	require.NoError(t, err)
	var res []string
	for r := range dry.Pull(context.Background(), time.Time{}) {
		res = append(res, r.String())
	}
	for r := range dry.Push(context.Background()) {
		res = append(res, r.String())
	}
	return strings.Join(res, "\n")
}

// A label the forge refuses, like an assignee GitHub can't assign, stays in
// git-bug with a warning; the others go through, and nothing fails.
func TestPushRefusedLabelStays(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{RefuseLabels: []string{"assignee:eve"}}, bot)
	issue, b := pulled(t, f, repo)

	_, _, err := b.ChangeLabels([]string{"a", "assignee:eve"}, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())

	results := push(t, repo, f, true)
	require.Equal(t, 1, exportWarnings(results))
	require.Equal(t, []string{"a"}, f.Issue(issue.Number).Labels)
	steady(t, repo, f)
	snap := onlyBug(t, repo).Snapshot()
	require.Equal(t, []common.Label{"a", "assignee:eve"}, snap.Labels)
	require.Equal(t, 1, countOps[*bug.LabelChangeOperation](snap), "the push's echo is claimed")
	require.Equal(t, []string{"a"}, f.Issue(issue.Number).Labels)
	require.NotContains(t, planned(t, repo, f), "would", "as the push, a dry run skips the refused label")
}

// A label the forge holds the old way, like a GitHub assignee:<login> label,
// is sent again, the new way.
func TestSyncResendsOldWayLabel(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{OldLabels: "assignee:"}, bot)
	alice := f.UserAct("alice")
	issue := alice.CreateIssue("title", "body")
	alice.SetLabels(issue, []string{"a", "assignee:bob"})
	pull(t, repo, f)
	require.Equal(t, []string{"assignee:bob"}, f.Issue(issue.Number).Resend)

	sync(t, repo, f)
	require.Empty(t, f.Issue(issue.Number).Resend, "sent again")
	steady(t, repo, f)
	require.Equal(t, []common.Label{"a", "assignee:bob"}, onlyBug(t, repo).Snapshot().Labels)
	require.Equal(t, []string{"a", "assignee:bob"}, f.Issue(issue.Number).Labels)
}

// One put on the forge the old way after the issue synced is sent again too.
func TestSyncResendsLaterOldWayLabel(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{OldLabels: "assignee:"}, bot)
	alice := f.UserAct("alice")
	issue := alice.CreateIssue("title", "body")
	sync(t, repo, f)

	alice.SetLabels(issue, []string{"assignee:bob"})
	sync(t, repo, f)
	require.Empty(t, f.Issue(issue.Number).Resend)
	steady(t, repo, f)
	require.Equal(t, []common.Label{"assignee:bob"}, onlyBug(t, repo).Snapshot().Labels)
}

// When the forge refuses it the new way, the label stays as it is on both
// sides, with one warning.
func TestSyncRefusedOldWayLabelStays(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{OldLabels: "assignee:", RefuseLabels: []string{"assignee:eve"}}, bot)
	alice := f.UserAct("alice")
	issue := alice.CreateIssue("title", "body")
	alice.SetLabels(issue, []string{"assignee:eve"})

	pull(t, repo, f)
	require.Equal(t, 1, exportWarnings(push(t, repo, f, true)))
	steady(t, repo, f)
	require.Equal(t, []common.Label{"assignee:eve"}, onlyBug(t, repo).Snapshot().Labels)
	require.Equal(t, []string{"assignee:eve"}, f.Issue(issue.Number).Labels)
	require.NotContains(t, planned(t, repo, f), "would")
}

// A dry run tells which labels a sync would send again, of an issue to
// import too.
func TestDryRunNamesOldWayLabels(t *testing.T) {
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{OldLabels: "assignee:"}, bot)
	alice := f.UserAct("alice")
	issue := alice.CreateIssue("title", "body")
	alice.SetLabels(issue, []string{"assignee:bob"})

	require.Contains(t, planned(t, repo, f), "would send again: labels +assignee:bob")
	pull(t, repo, f)
	require.Contains(t, planned(t, repo, f), "would send again: labels +assignee:bob")
	require.Equal(t, []string{"assignee:bob"}, f.Issue(issue.Number).Resend, "nothing sent")
}
