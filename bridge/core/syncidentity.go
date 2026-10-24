package core

import (
	"context"
	"errors"
	"strings"

	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entity"
)

// identityFor returns the identity of a forge user: the one their login is
// linked to in the bridge's configuration, else the one the package-level
// identityFor finds or creates.
func (e *Engine) identityFor(ctx context.Context, u User) (*cache.IdentityCache, error) {
	if id, ok := e.links[strings.ToLower(u.Login)]; ok {
		return e.repo.Identities().Resolve(ctx, id)
	}
	return identityFor(ctx, e.repo, e.remote, u, "")
}

// identityFor returns the identity of a forge user, created if there's none.
// See identitiesFor.
func identityFor(ctx context.Context, repo *cache.RepoCache, r Remote, u User, loginKey string) (*cache.IdentityCache, error) {
	ids, err := identitiesFor(ctx, repo, r, u, loginKey, true)
	if err != nil {
		return nil, err
	}
	return repo.Identities().Resolve(ctx, ids[0])
}

// identitiesFor returns the identities of a forge user, found by their user id
// on the server, then by the bridge's legacy key, then, if loginKey is set, by
// the login tag `bridge new` writes on token owners. Found identities get
// tagged with the user id. With none, one is created if create is set.
func identitiesFor(ctx context.Context, repo *cache.RepoCache, r Remote, u User, loginKey string, create bool) ([]entity.Id, error) {
	key := userKey(r.Instance())
	ids, err := resolveAll(ctx, repo, key, u.ID)
	if err != nil || len(ids) > 0 {
		return ids, err
	}

	var lookups [][2]string
	if legacy, isLogin := r.LegacyUser(); legacy != "" {
		value := u.ID
		if isLogin {
			value = u.Login
		}
		lookups = append(lookups, [2]string{legacy, value})
	}
	if loginKey != "" {
		lookups = append(lookups, [2]string{loginKey, u.Login})
	}
	for _, l := range lookups {
		ids, err := resolveAll(ctx, repo, l[0], l[1])
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			continue
		}
		if !create {
			return ids, nil
		}
		for _, id := range ids {
			i, err := repo.Identities().Resolve(ctx, id)
			if err != nil {
				return nil, err
			}
			i.SetMetadata(key, u.ID)
			if err := i.Commit(); err != nil {
				return nil, err
			}
		}
		return ids, nil
	}

	if !create {
		return nil, nil
	}
	name := u.Name
	if name == "" {
		name = u.Login
	}
	i, err := repo.Identities().NewRaw(name, u.Email, u.Login, u.AvatarURL, nil, map[string]string{key: u.ID})
	if err != nil {
		return nil, err
	}
	return []entity.Id{i.Id()}, nil
}

// resolveAll returns the identities whose immutable metadata key is value.
// Old bridges could create several for one person.
func resolveAll(ctx context.Context, repo *cache.RepoCache, key, value string) ([]entity.Id, error) {
	if value == "" {
		return nil, nil
	}
	i, err := repo.Identities().ResolveIdentityImmutableMetadata(ctx, key, value)
	if multiple, ok := errors.AsType[*entity.ErrMultipleMatch](err); ok {
		return multiple.Matching, nil
	}
	if entity.IsErrNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []entity.Id{i.Id()}, nil
}

// loadAccounts maps the identities of the remote's token owners to their
// account.
func loadAccounts(ctx context.Context, repo *cache.RepoCache, r Remote, loginKey string) (map[entity.Id]Account, error) {
	accounts, err := r.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	return mapAccounts(ctx, repo, r, accounts, loginKey, true)
}

func mapAccounts(ctx context.Context, repo *cache.RepoCache, r Remote, accounts []Account, loginKey string, create bool) (map[entity.Id]Account, error) {
	res := make(map[entity.Id]Account)
	for _, a := range accounts {
		ids, err := identitiesFor(ctx, repo, r, a.User, loginKey, create)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			res[id] = a
		}
	}
	return res, nil
}
