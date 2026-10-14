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

// forges are the fake forges of the fakesync bridges, by project name.
var forges = map[string]*synctest.Fake{}

// syncBridge is a bridge ported to the sync engine, on a fake forge.
type syncBridge struct{ plainBridge }

func (syncBridge) Target() string { return "fakesync" }

func (syncBridge) NewRemote(_ context.Context, _ *cache.RepoCache, conf core.Configuration) (core.Remote, error) {
	return forges[conf["project"]], nil
}

// plainBridge is a bridge with its own importer and exporter.
type plainBridge struct{}

func (plainBridge) Target() string                      { return "fakeplain" }
func (plainBridge) LoginMetaKey() string                { return "fake-login" }
func (plainBridge) NewImporter() core.Importer          { return nil }
func (plainBridge) NewExporter() core.Exporter          { return nil }
func (plainBridge) ValidParams() map[string]interface{} { return nil }
func (plainBridge) ValidateConfig(core.Configuration) error {
	return nil
}
func (plainBridge) Configure(*cache.RepoCache, core.BridgeParams, bool) (core.Configuration, error) {
	return nil, nil
}

func init() {
	core.Register(&syncBridge{})
	core.Register(&plainBridge{})
}

func addBridge(t *testing.T, repo *cache.RepoCache, name, target string, conf map[string]string) {
	for k, v := range conf {
		require.NoError(t, core.SetBridgeConfig(repo, name, k, v))
	}
	require.NoError(t, core.SetBridgeConfig(repo, name, core.ConfigKeyTarget, target))
}

func newSyncForge(t *testing.T, repo *cache.RepoCache, name string) *synctest.Fake {
	f := synctest.NewFake(name+".test/1", name+".test", synctest.Quirks{}, bot)
	forges[name] = f
	addBridge(t, repo, name, "fakesync", map[string]string{
		"project":                  name,
		core.ConfigKeyDefaultLogin: "bot",
		core.ConfigKeySync:         "true",
	})
	return f
}

func TestBridgeUsesEngine(t *testing.T) {
	repo := newRepo(t)
	f := newSyncForge(t, repo, "solo")
	f.UserAct("alice").CreateIssue("from the forge", "body")

	b, err := core.LoadBridge(repo, "solo")
	require.NoError(t, err)
	events, err := b.ImportAllSince(context.Background(), time.Time{})
	require.NoError(t, err)
	for e := range events {
		require.NotEqual(t, core.ImportEventError, e.Event, e.String())
	}
	require.Equal(t, "from the forge", onlyBug(t, repo).Snapshot().Title)

	_, _, err = repo.Bugs().New("from git-bug", "body")
	require.NoError(t, err)
	exported, err := b.ExportAll(context.Background(), time.Time{})
	require.NoError(t, err)
	for e := range exported {
		require.NotEqual(t, core.ExportEventError, e.Event, e.String())
	}
	require.Equal(t, "from git-bug", f.Issue("2").Title)
}

func TestSyncOnlyForPortedBridges(t *testing.T) {
	repo := newRepo(t)
	addBridge(t, repo, "old", "fakeplain", map[string]string{core.ConfigKeySync: "true"})

	_, err := core.LoadBridge(repo, "old")
	require.ErrorContains(t, err, "sync")
}

func TestSyncAll(t *testing.T) {
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	b := newSyncForge(t, repo, "b")
	a.UserAct("alice").CreateIssue("from a", "body")

	results, err := core.SyncAll(context.Background(), repo, []string{"a", "b"}, false)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}

	require.Equal(t, "from a", b.Issue("1").Title, "pulled from a, pushed to b in one run")
}

func TestSyncAllDryRun(t *testing.T) {
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	b := newSyncForge(t, repo, "b")
	a.UserAct("alice").CreateIssue("from a", "body")
	_, _, err := repo.Bugs().New("from git-bug", "body")
	require.NoError(t, err)
	identities := len(repo.Identities().AllIds())

	results, err := core.SyncAll(context.Background(), repo, []string{"a", "b"}, true)
	require.NoError(t, err)
	var report []string
	for r := range results {
		require.False(t, r.IsError(), r.String())
		report = append(report, r.String())
	}

	require.Zero(t, a.Writes()+b.Writes(), "nothing written to forges")
	require.Len(t, repo.Bugs().AllIds(), 1, "nothing imported")
	require.Equal(t, identities, len(repo.Identities().AllIds()), "no identity created")
	all := strings.Join(report, "\n")
	require.Contains(t, all, "would import issue https://a.test/issues/1")
	require.Contains(t, all, "would create issue: from git-bug")
}
