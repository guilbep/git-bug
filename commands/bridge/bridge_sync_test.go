package bridgecmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/synctest"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/commands/bug/testenv"
	"github.com/git-bug/git-bug/commands/execenv"
	"github.com/git-bug/git-bug/commands/execenv/envtest"
)

// fakeForges are the forges of the fakecmd bridges, by project name.
var fakeForges = map[string]*synctest.Fake{}

// fakeParams are the parameters of the last fakecmd bridge configured.
var fakeParams core.BridgeParams

// fakeBridge is a bridge ported to the sync engine, on a fake forge. Like
// GitLab, it has instances.
type fakeBridge struct{}

func (fakeBridge) Target() string             { return "fakecmd" }
func (fakeBridge) LoginMetaKey() string       { return "fakecmd-login" }
func (fakeBridge) NewImporter() core.Importer { return nil }
func (fakeBridge) NewExporter() core.Exporter { return nil }
func (fakeBridge) ValidParams() map[string]interface{} {
	return map[string]interface{}{"URL": nil, "BaseURL": nil, "CredPrefix": nil}
}
func (fakeBridge) ValidateConfig(core.Configuration) error {
	return nil
}
func (fakeBridge) Configure(_ *cache.RepoCache, params core.BridgeParams, _ bool) (core.Configuration, error) {
	fakeParams = params
	return core.Configuration{core.ConfigKeyTarget: "fakecmd"}, nil
}
func (fakeBridge) NewRemote(_ context.Context, _ *cache.RepoCache, conf core.Configuration) (core.Remote, error) {
	return fakeForges[conf["project"]], nil
}

func init() {
	core.Register(&fakeBridge{})
}

func newFakeForge(t *testing.T, env *execenv.Env, name string) *synctest.Fake {
	bot := core.Account{User: core.User{ID: "bot-" + name, Login: "bot", Name: "Bot"}}
	f := synctest.NewFake(name+".test/1", name+".test", synctest.Quirks{}, bot)
	fakeForges[name] = f
	for k, v := range map[string]string{
		core.ConfigKeyTarget:       "fakecmd",
		"project":                  name,
		core.ConfigKeyDefaultLogin: "bot",
		core.ConfigKeySync:         "true",
	} {
		require.NoError(t, core.SetBridgeConfig(env.Backend, name, k, v))
	}
	return f
}

func output(env *execenv.Env) string {
	out := env.Out.(*envtest.TestOut)
	defer out.Reset()
	return out.String()
}

func TestBridgeSync(t *testing.T) {
	env, _ := testenv.NewTestEnvAndUser(t)
	a := newFakeForge(t, env, "a")
	b := newFakeForge(t, env, "b")
	a.UserAct("alice").CreateIssue("from a", "body")

	require.NoError(t, runBridgeSync(env, bridgeSyncOptions{dryRun: true}, nil))
	out := output(env)
	require.Contains(t, out, "would import issue https://a.test/issues/1")
	require.NotContains(t, out, "no action taken: no change", "only what would happen")
	require.Zero(t, b.Writes())

	require.NoError(t, runBridgeSync(env, bridgeSyncOptions{}, nil))
	require.Equal(t, "from a", b.Issue("1").Title)
	require.Contains(t, output(env), "pulled 1 issues, pushed 1 issues")

	require.NoError(t, runBridgeSync(env, bridgeSyncOptions{}, []string{"a", "b"}))
	require.Contains(t, output(env), "pulled 0 issues, pushed 0 issues")
}

func TestBridgeSyncFailsOnErrors(t *testing.T) {
	env, _ := testenv.NewTestEnvAndUser(t)
	a := newFakeForge(t, env, "a")
	_, _, err := env.Backend.Bugs().New("local", "body")
	require.NoError(t, err)
	a.FailNextWrite()

	require.ErrorContains(t, runBridgeSync(env, bridgeSyncOptions{}, nil), "1 error")
}

func TestBridgeSyncDryRunFailsOnErrors(t *testing.T) {
	env, _ := testenv.NewTestEnvAndUser(t)
	a := newFakeForge(t, env, "a")
	a.FailNextListing()

	require.ErrorContains(t, runBridgeSync(env, bridgeSyncOptions{dryRun: true}, nil), "error")
}

func TestBridgeSyncOneIssue(t *testing.T) {
	env, _ := testenv.NewTestEnvAndUser(t)
	a := newFakeForge(t, env, "a")
	mine, _, err := env.Backend.Bugs().New("mine", "body")
	require.NoError(t, err)
	_, _, err = env.Backend.Bugs().New("not mine", "body")
	require.NoError(t, err)

	require.NoError(t, runBridgeSync(env, bridgeSyncOptions{issue: mine.Id().Human()}, nil))
	require.Equal(t, "mine", a.Issue("1").Title)
	require.Nil(t, a.Issue("2"), "only that issue")
	require.Contains(t, output(env), "pulled 0 issues, pushed 1 issues")

	require.Error(t, runBridgeSync(env, bridgeSyncOptions{issue: "deadbeef"}, nil))
}
