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
