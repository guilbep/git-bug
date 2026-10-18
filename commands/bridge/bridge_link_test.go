package bridgecmd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/commands/bug/testenv"
)

func TestBridgeLinkTakeUnlink(t *testing.T) {
	env, _ := testenv.NewTestEnvAndUser(t)
	a := newFakeForge(t, env, "a")
	ref := a.UserAct("alice").CreateIssue("forge title", "forge body")
	bug, _, err := env.Backend.Bugs().New("local title", "local body")
	require.NoError(t, err)
	id := bug.Id().Human()

	require.NoError(t, runBridgeLink(env, []string{id, "a", ref.URL}))
	require.Contains(t, output(env), ref.URL)
	require.ErrorContains(t, runBridgeLink(env, []string{id, "a", "1"}), "already")

	require.NoError(t, runBridgeTake(env, []string{id, "a"}))
	require.Contains(t, output(env), "changed title")
	require.Equal(t, "forge title", bug.Snapshot().Title)
	require.NoError(t, runBridgeTake(env, []string{id, "a"}))
	require.Contains(t, output(env), "nothing to take")

	require.NoError(t, runBridgeUnlink(env, []string{id, "a"}))
	require.Contains(t, output(env), "no longer synced")
	require.ErrorContains(t, runBridgeTake(env, []string{id, "a"}), "not synced")
	require.Zero(t, a.Writes())
}

// A warning takes nothing: an issue deleted on the forge has no version to
// take.
func TestBridgeTakeDeletedIssue(t *testing.T) {
	env, _ := testenv.NewTestEnvAndUser(t)
	a := newFakeForge(t, env, "a")
	ref := a.UserAct("alice").CreateIssue("forge title", "forge body")
	bug, _, err := env.Backend.Bugs().New("local title", "local body")
	require.NoError(t, err)
	id := bug.Id().Human()
	require.NoError(t, runBridgeLink(env, []string{id, "a", ref.Number}))
	output(env)

	a.UserAct("alice").Delete(ref)
	require.NoError(t, runBridgeTake(env, []string{id, "a"}))
	out := output(env)
	require.Contains(t, out, "deleted on the forge")
	require.Contains(t, out, "nothing to take")
}
