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

// A dry run doesn't take the forge's echo of a push, not claimed yet, for a
// change to pull: it names nothing to send again while a refusal holds, as
// the sync then sends nothing.
func TestDryRunSkipsEchoes(t *testing.T) {
	ctx := t.Context()
	f := githubLike(synctest.Quirks{Unassignable: []string{"eve"}})
	p := newPeople(t, f)
	eve := f.Assignee("eve")
	issue := eve.CreateIssue("title", "body")
	eve.SetLabels(issue, []string{"assignee:eve"})
	p.sync()
	p.changeLabels(ctx, "title", []string{"a"}, nil)
	for range p.engine(f, false).Push(context.Background()) {
	}
	require.Equal(t, []string{"a", "assignee:eve"}, f.Issue(issue.Number).Labels)

	require.NotContains(t, p.planned(), "would send")
	writes := f.Writes()
	p.sync()
	require.Equal(t, writes, f.Writes(), "the sync sends nothing")
}

// A refusal of the whole label change holds while the forge holds assignees
// the same way, and is tried again once it holds them the other way.
func TestWholeLabelRefusalHoldsPerMode(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	f := synctest.NewFake(key, "fake.test", synctest.Quirks{RefuseLabelChanges: true}, bot)
	_, b := pulled(t, f, repo)
	_, _, err := b.ChangeLabels(ctx, []string{"a"}, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit(ctx))
	pushAs := func(mode string) int {
		e, err := core.NewEngine(context.Background(), repo, f, core.SyncOptions{
			Foreign: true, DefaultLogin: "bot", LoginKey: "fake-login", Assignees: mode,
		})
		require.NoError(t, err)
		var res []core.ExportResult
		for r := range e.Push(context.Background()) {
			require.NotEqual(t, core.ExportEventError, r.Event, r.String())
			res = append(res, r)
		}
		return exportWarnings(res)
	}

	require.Equal(t, 1, pushAs(core.AssigneesNative))
	writes := f.Writes()
	require.Zero(t, pushAs(core.AssigneesNative))
	require.Equal(t, writes, f.Writes(), "held")
	require.Equal(t, 1, pushAs(core.AssigneesLabels), "tried again the other way")
	writes = f.Writes()
	require.Zero(t, pushAs(core.AssigneesLabels))
	require.Equal(t, writes, f.Writes(), "held again")
}
