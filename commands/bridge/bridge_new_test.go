package bridgecmd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/commands/bug/testenv"
	"github.com/git-bug/git-bug/commands/execenv"
)

func TestTargetOf(t *testing.T) {
	for url, target := range map[string]string{
		"https://github.com/EPFL-ENAC/wiki":     "github",
		"https://github.com/EPFL-ENAC/wiki.git": "github",
		"git@github.com:EPFL-ENAC/wiki.git":     "github",
		"https://gitlab.com/group/sub/project":  "gitlab",
		"https://gitlab.epfl.ch/EPFL-ENAC/wiki": "",
		"https://notgithub.com/o/p":             "",
		"":                                      "",
	} {
		require.Equal(t, target, targetOf(url), url)
	}
}

func TestBridgeNewSync(t *testing.T) {
	env, _ := testenv.NewTestEnvAndUser(t)

	require.NoError(t, runBridgeNew(env, bridgeNewOptions{name: "a", target: "fakecmd", sync: true}))
	b, err := core.LoadBridge(env.Backend, "a")
	require.NoError(t, err)
	require.True(t, b.IsSync())

	err = runBridgeNew(env, bridgeNewOptions{name: "lp", target: "launchpad-preview", sync: true,
		params: core.BridgeParams{URL: "https://bugs.launchpad.net/ubuntu/"}})
	require.ErrorContains(t, err, "doesn't support sync")
	require.False(t, core.BridgeExist(env.Repo, "lp"))
}

func storeToken(t *testing.T, env *execenv.Env, login, baseURL string) auth.Credential {
	token := auth.NewToken("fakecmd", "token-of-"+login)
	token.SetMetadata(auth.MetaKeyLogin, login)
	if baseURL != "" {
		token.SetMetadata(auth.MetaKeyBaseURL, baseURL)
	}
	require.NoError(t, auth.Store(env.Backend, token))
	return token
}

func TestBridgeNewPicksStoredCredential(t *testing.T) {
	env, _ := testenv.NewTestEnvAndUser(t)
	alice := storeToken(t, env, "alice", "https://a.test")
	bob := storeToken(t, env, "bob", "https://b.test/")
	storeToken(t, env, "carol", "") // of no instance

	// the only credential of the instance holding --url
	opts := bridgeNewOptions{name: "b", target: "fakecmd", nonInteractive: true,
		params: core.BridgeParams{URL: "https://b.test/o/p"}}
	require.NoError(t, runBridgeNew(env, opts))
	require.Equal(t, bob.ID().String(), fakeParams.CredPrefix)
	require.Contains(t, output(env), bob.ID().Human())

	// or of --base-url
	opts.params = core.BridgeParams{BaseURL: "https://a.test/"}
	require.NoError(t, runBridgeNew(env, opts))
	require.Equal(t, alice.ID().String(), fakeParams.CredPrefix)

	// none: left to the bridge
	opts.params = core.BridgeParams{URL: "https://c.test/o/p"}
	require.NoError(t, runBridgeNew(env, opts))
	require.Empty(t, fakeParams.CredPrefix)

	// several: --credential chooses
	bob2 := storeToken(t, env, "bob2", "https://b.test")
	opts.params = core.BridgeParams{URL: "https://b.test/o/p"}
	err := runBridgeNew(env, opts)
	require.ErrorContains(t, err, "--credential")
	require.ErrorContains(t, err, bob.ID().Human())
	require.ErrorContains(t, err, bob2.ID().Human())

	opts.params.CredPrefix = bob2.ID().String()
	require.NoError(t, runBridgeNew(env, opts))
	require.Equal(t, bob2.ID().String(), fakeParams.CredPrefix)

	// or the prompts, when interactive
	opts.params.CredPrefix = ""
	opts.nonInteractive = false
	require.NoError(t, runBridgeNew(env, opts))
	require.Empty(t, fakeParams.CredPrefix)
}

func TestPickCredentialWithoutInstances(t *testing.T) {
	env, _ := testenv.NewTestEnvAndUser(t)
	carol := storeToken(t, env, "carol", "")

	id, err := pickCredential(env, "fakecmd", false, core.BridgeParams{URL: "https://github.com/o/p"}, true)
	require.NoError(t, err)
	require.Equal(t, carol.ID().String(), id)
}
