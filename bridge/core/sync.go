package core

import (
	"context"
	"fmt"

	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entity"
	"github.com/git-bug/git-bug/entity/dag"
)

// SyncOptions configure the sync engine for a bridge.
type SyncOptions struct {
	// Foreign is set when the bridge has `sync` enabled: every bug is pushed,
	// with the default account for people who have none on the forge.
	// Without it, push keeps the pre-sync limits: no bug from another forge,
	// and only what someone has an account for.
	Foreign bool
	// DryRun reports what would happen, and writes nothing.
	DryRun bool
	// DefaultLogin is the login of the account posting for people without one.
	DefaultLogin string
	// LoginKey is the identity metadata `bridge new` tags token owners with.
	LoginKey string
}

// Engine syncs a git-bug repository with one forge project.
// See doc/design/bridges/sync.md.
type Engine struct {
	repo   *cache.RepoCache
	remote Remote
	opts   SyncOptions

	// accounts maps the identities of token owners to their account.
	accounts map[entity.Id]Account
	// defaultAccount posts for people without an account, with Foreign.
	defaultAccount *Account
}

func NewEngine(ctx context.Context, repo *cache.RepoCache, r Remote, opts SyncOptions) (*Engine, error) {
	accounts, err := loadAccounts(ctx, repo, r, opts.LoginKey)
	if err != nil {
		return nil, err
	}
	e := &Engine{repo: repo, remote: r, opts: opts, accounts: accounts}
	for _, a := range accounts {
		if a.Login == opts.DefaultLogin {
			e.defaultAccount = &a
			break
		}
	}
	if opts.Foreign && e.defaultAccount == nil {
		return nil, fmt.Errorf("sync needs a token for the default login %q", opts.DefaultLogin)
	}
	return e, nil
}

func (e *Engine) key(name string) string {
	return syncKey(e.remote.Key(), name)
}

// findOp returns the operation that is the given remote object or event,
// or that claimed the event.
func (e *Engine) findOp(snap *bug.Snapshot, remoteID string) dag.Operation {
	also := e.key("also:" + remoteID)
	for _, op := range snap.Operations {
		if id, ok := opOnRemote(op, snap, e.remote); ok && id == remoteID {
			return op
		}
		if _, ok := op.GetMetadata(also); ok {
			return op
		}
	}
	return nil
}

// opByID returns the operation with the given git-bug id.
func opByID(snap *bug.Snapshot, id entity.Id) dag.Operation {
	for _, op := range snap.Operations {
		if op.Id() == id {
			return op
		}
	}
	return nil
}
