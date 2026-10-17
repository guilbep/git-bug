package core

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entities/identity"
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
	// Links are identity id prefixes, by lowercased login: the identity of
	// that forge user, whatever else matches them.
	Links map[string]string
}

// Engine syncs a git-bug repository with one forge project.
// See doc/design/bridges/sync.md.
type Engine struct {
	repo   *cache.RepoCache
	remote Remote
	opts   SyncOptions

	// accounts maps the identities of token owners to their account.
	accounts map[entity.Id]Account
	// tokens are the accounts with a token, identity or not yet (a dry
	// run creates none).
	tokens []Account
	// defaultAccount posts for people without an account, with Foreign.
	defaultAccount *Account
	// links are the identities of forge users, by lowercased login.
	links map[string]entity.Id
	// attached are, in a dry run, the bugs a pull would attach to an issue
	// by its marker, with the issue's URL: the real run records the link,
	// so its push doesn't create another.
	attached map[entity.Id]string
	// theirs, when set, resolves every difference of a pull for the forge:
	// git-bug's changes not sent yet are undone, by that identity. See Take.
	theirs identity.Interface
}

func NewEngine(ctx context.Context, repo *cache.RepoCache, r Remote, opts SyncOptions) (*Engine, error) {
	registerRemote(r)
	list, err := r.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	e := &Engine{repo: repo, remote: r, opts: opts, tokens: list, links: map[string]entity.Id{}, attached: map[entity.Id]string{}}
	for login, prefix := range opts.Links {
		i, err := repo.Identities().ResolvePrefix(prefix)
		if err != nil {
			return nil, fmt.Errorf("%s%s: %w", ConfigKeyLoginPrefix, prefix, err)
		}
		e.links[login] = i.Id()
	}
	for _, a := range list {
		if a.Login == opts.DefaultLogin {
			e.defaultAccount = &a
			break
		}
	}
	if opts.Foreign && e.defaultAccount == nil {
		return nil, fmt.Errorf("sync needs a token for the default login %q", opts.DefaultLogin)
	}
	// a dry run creates no identity
	e.accounts, err = mapAccounts(repo, r, list, opts.LoginKey, !opts.DryRun)
	return e, err
}

func (e *Engine) key(name string) string {
	return syncKey(e.remote.Key(), name)
}

// findOp returns the operation that is the given remote object or event,
// or that claimed the event.
func (e *Engine) findOp(snap *bug.Snapshot, remoteID string) dag.Operation {
	also := e.key("also:" + remoteID)
	createMeta := snap.Operations[0].AllMetadata()
	for _, op := range snap.Operations {
		if id, ok := onRemote(op, createMeta, e.remote); ok && id == remoteID {
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

// Engines register the remotes they sync, so that text pulled from one is
// rendered for another with the right dialect.
var (
	remotesMu sync.Mutex
	remotes   = map[string]remoteInfo{}
)

type remoteInfo struct {
	kind    string
	dialect Dialect
}

// defaultDialect is how GitHub, GitLab and Gitea refer to an issue of the
// same project, used for remotes not registered.
var defaultDialect = Dialect{
	Ref:       regexp.MustCompile(`(?:^|[^\w/#])(#(\d+))\b`),
	FormatRef: func(n string) string { return "#" + n },
}

func registerRemote(r Remote) {
	remotesMu.Lock()
	defer remotesMu.Unlock()
	remotes[r.Key()] = remoteInfo{kind: r.Kind(), dialect: r.Dialect()}
}

func remoteInfoFor(key string) remoteInfo {
	remotesMu.Lock()
	defer remotesMu.Unlock()
	if info, ok := remotes[key]; ok {
		return info
	}
	// pulled by a pre-sync bridge: "legacy:github"
	return remoteInfo{kind: strings.TrimPrefix(key, "legacy:"), dialect: defaultDialect}
}

// displayKind names a kind of forge as people do.
func displayKind(kind string) string {
	switch kind {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	case "gitea":
		return "Gitea"
	}
	return kind
}

// sourceOf returns the key of the remote an operation was pulled from, and
// its URL there, if any.
func sourceOf(op dag.Operation) (key string, url string, ok bool) {
	own := ownMetadata(op)
	for k := range own {
		if strings.HasPrefix(k, "sync:") && strings.HasSuffix(k, ":id") {
			key = strings.TrimSuffix(strings.TrimPrefix(k, "sync:"), ":id")
			return key, own[syncKey(key, "url")], true
		}
	}
	// reconciled: the forge reported no event, only the value
	for k := range own {
		for _, kind := range []slotKind{slotTitle, slotText} {
			if key, ok := remoteKeyOf(k, string(kind)); ok {
				return key, "", true
			}
		}
	}
	// pulled by a pre-sync bridge
	for _, kind := range []string{"github", "gitlab"} {
		if _, ok := own[kind+"-id"]; ok {
			return "legacy:" + kind, own[kind+"-url"], true
		}
	}
	return "", "", false
}
