package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/repository/repotest"
)

// identityRemote is a Remote with accounts, whose legacy user key is
// "old-login".
type identityRemote struct {
	Remote
	accounts []Account
}

func (identityRemote) Instance() string           { return "forge.test" }
func (identityRemote) LegacyUser() (string, bool) { return "old-login", true }
func (r identityRemote) Accounts(context.Context) ([]Account, error) {
	return r.accounts, nil
}

func newTestRepo(t *testing.T) *cache.RepoCache {
	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	return backend
}

func identityCount(t *testing.T, repo *cache.RepoCache) int {
	return len(repo.Identities().AllIds())
}

func TestIdentityForCreatesOnce(t *testing.T) {
	repo := newTestRepo(t)
	r := identityRemote{}
	alice := User{ID: "7", Login: "alice", Name: "Alice"}

	first, err := identityFor(repo, r, alice, "")
	require.NoError(t, err)
	second, err := identityFor(repo, r, alice, "")
	require.NoError(t, err)

	require.Equal(t, first.Id(), second.Id())
	require.Equal(t, 1, identityCount(t, repo))
	require.Equal(t, "Alice", first.Name())
	require.Equal(t, "alice", first.Login())
}

func TestIdentityForLegacy(t *testing.T) {
	repo := newTestRepo(t)
	r := identityRemote{}
	old, err := repo.Identities().NewRaw("Alice", "", "alice", "", nil, map[string]string{"old-login": "alice"})
	require.NoError(t, err)

	got, err := identityFor(repo, r, User{ID: "7", Login: "alice", Name: "Alice"}, "")
	require.NoError(t, err)
	require.Equal(t, old.Id(), got.Id())
	require.Equal(t, 1, identityCount(t, repo))

	// tagged, found by id from now on
	tagged, err := repo.Identities().ResolveIdentityImmutableMetadata(userKey(r.Instance()), "7")
	require.NoError(t, err)
	require.Equal(t, old.Id(), tagged.Id())
}

func TestLoadAccountsFindsLoginTag(t *testing.T) {
	repo := newTestRepo(t)
	// what `bridge new` does: tag the user's identity with the login
	me, err := repo.Identities().NewRaw("Me", "me@example.com", "", "", nil, map[string]string{"forge-login": "me"})
	require.NoError(t, err)
	r := identityRemote{accounts: []Account{{User: User{ID: "3", Login: "me", Name: "Me"}}}}

	accounts, err := loadAccounts(context.Background(), repo, r, "forge-login")
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, "me", accounts[me.Id()].Login)
	require.Equal(t, 1, identityCount(t, repo), "no second identity for a token owner")

	// a pull then resolves the token owner to the same identity
	got, err := identityFor(repo, r, User{ID: "3", Login: "me"}, "forge-login")
	require.NoError(t, err)
	require.Equal(t, me.Id(), got.Id())
}

func TestLoadAccountsSeveralIdentities(t *testing.T) {
	repo := newTestRepo(t)
	// left behind by an older bridge: two identities with the same login
	a, err := repo.Identities().NewRaw("Me", "", "", "", nil, map[string]string{"forge-login": "me"})
	require.NoError(t, err)
	b, err := repo.Identities().NewRaw("Me", "", "", "", nil, map[string]string{"forge-login": "me"})
	require.NoError(t, err)
	r := identityRemote{accounts: []Account{{User: User{ID: "3", Login: "me"}}}}

	accounts, err := loadAccounts(context.Background(), repo, r, "forge-login")
	require.NoError(t, err)
	require.Contains(t, accounts, a.Id())
	require.Contains(t, accounts, b.Id())
	require.Equal(t, 2, identityCount(t, repo))
}

func TestLoadAccountsCreatesMissing(t *testing.T) {
	repo := newTestRepo(t)
	r := identityRemote{accounts: []Account{{User: User{ID: "9", Login: "bot", Name: "Bot"}}}}

	accounts, err := loadAccounts(context.Background(), repo, r, "forge-login")
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	_, err = loadAccounts(context.Background(), repo, r, "forge-login")
	require.NoError(t, err)
	require.Equal(t, 1, identityCount(t, repo))
}
