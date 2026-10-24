package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/git-bug/git-bug/cache"
)

// SyncResult is a result of SyncAll, from one of the bridges.
type SyncResult struct {
	Bridge string
	Import *ImportResult
	Export *ExportResult
}

func (r SyncResult) IsError() bool {
	return (r.Import != nil && r.Import.Event == ImportEventError) ||
		(r.Export != nil && r.Export.Event == ExportEventError)
}

func (r SyncResult) String() string {
	if r.Import != nil {
		return r.Bridge + ": " + r.Import.String()
	}
	return r.Bridge + ": " + r.Export.String()
}

// SyncAll pulls the named bridges, or every bridge with sync enabled, then
// pushes to each of them, under the repository's bridge lock. What one
// bridge pulls reaches the others in the same run. opts apply to every
// bridge.
func SyncAll(ctx context.Context, repo *cache.RepoCache, names []string, dryRun bool, opts ...Option) (<-chan SyncResult, error) {
	return syncRun(ctx, repo, names, dryRun, nil, opts)
}

// SyncIssue syncs one bug, by id prefix, as SyncAll does every bug: its
// issue is pulled from each forge it is on, then the bug pushed to each,
// its issue created where a push would create it. The bridges' sync times
// stay as they were: no other issue was pulled.
func SyncIssue(ctx context.Context, repo *cache.RepoCache, prefix string, names []string, dryRun bool, opts ...Option) (<-chan SyncResult, error) {
	b, err := repo.Bugs().ResolvePrefix(ctx, prefix)
	if err != nil {
		return nil, err
	}
	return syncRun(ctx, repo, names, dryRun, b, opts)
}

// syncRun is SyncAll, or SyncIssue when only is set.
func syncRun(ctx context.Context, repo *cache.RepoCache, names []string, dryRun bool, only *cache.BugCache, opts []Option) (<-chan SyncResult, error) {
	if len(names) == 0 {
		all, err := ConfiguredBridges(repo)
		if err != nil {
			return nil, err
		}
		for _, name := range all {
			if b, err := LoadBridge(repo, name); err == nil && b.IsSync() {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("no bridge has sync enabled: set git-bug.bridge.<name>.%s to true", ConfigKeySync)
		}
	}

	var bridges []*Bridge
	for _, name := range names {
		b, err := LoadBridge(repo, name)
		if err != nil {
			return nil, err
		}
		if !b.IsSync() {
			return nil, fmt.Errorf("bridge %s doesn't have sync enabled: set git-bug.bridge.%s.%s to true", name, name, ConfigKeySync)
		}
		b.Use(opts...)
		bridges = append(bridges, b)
	}

	unlock, err := lockBridges(repo)
	if err != nil {
		return nil, err
	}

	// every engine first: each registers its remote, for the others to
	// render text pulled from it
	engines := make([]*Engine, len(bridges))
	for i, b := range bridges {
		if engines[i], err = b.newEngine(ctx, dryRun); err != nil {
			unlock()
			return nil, fmt.Errorf("%s: %w", b.Name, err)
		}
	}

	remotes := map[string]Remote{}
	for i, b := range bridges {
		remotes[b.Name] = engines[i].remote
	}
	for _, b := range bridges {
		if err := checkPublish(ctx, b, remotes); err != nil {
			unlock()
			return nil, err
		}
	}

	out := make(chan SyncResult)
	go func() {
		defer close(out)
		defer unlock()

		pulled := make([]bool, len(bridges))
		for i, b := range bridges {
			// a bit before, just to be sure
			start := time.Now().Add(-5 * time.Second)
			pulled[i] = true
			var pulls <-chan ImportResult
			if only != nil {
				pulls = engines[i].PullIssue(ctx, only)
			} else {
				pulls = engines[i].Pull(ctx, b.configTime(configKeyLastSyncTime))
			}
			for r := range pulls {
				pulled[i] = pulled[i] && r.Event != ImportEventError
				out <- SyncResult{Bridge: b.Name, Import: &r}
			}
			if pulled[i] && !dryRun && only == nil {
				_ = b.storeConfigTime(configKeyLastSyncTime, start)
			}
		}
		for i, b := range bridges {
			// what git-bug knows of that forge may be stale: pushing could
			// undo what changed there since
			if !pulled[i] {
				r := NewExportError(errors.New("not pushed, as its pull failed"), "")
				out <- SyncResult{Bridge: b.Name, Export: &r}
				continue
			}
			var pushes <-chan ExportResult
			if only != nil {
				pushes = engines[i].PushBug(ctx, only)
			} else {
				pushes = engines[i].Push(ctx)
			}
			for r := range pushes {
				out <- SyncResult{Bridge: b.Name, Export: &r}
			}
		}
	}()
	return out, nil
}

// checkPublish refuses to sync b to a public project while another sync
// bridge of the repository isn't public: what the sync pulled from it would be
// published. remotes are those already made, by bridge name. A project whose
// visibility can't be read counts as not public.
// ponytail: decided per repository, not per bug or operation: a clone without
// the private bridge configured can't tell. Upgrade path: record the
// visibility of pulled data in git-bug, to check each operation pushed.
func checkPublish(ctx context.Context, b *Bridge, remotes map[string]Remote) error {
	if !b.IsSync() {
		return nil
	}
	if ok, _ := strconv.ParseBool(b.conf[ConfigKeyPublishPrivate]); ok {
		return nil
	}
	names, err := ConfiguredBridges(b.repo)
	if err != nil {
		return err
	}
	var others []*Bridge
	for _, name := range names {
		o, err := LoadBridge(b.repo, name)
		if err != nil || name == b.Name || !o.IsSync() {
			continue
		}
		if _, ported := o.impl.(RemoteBridge); ported {
			others = append(others, o)
		}
	}
	if len(others) == 0 {
		return nil
	}
	public, err := remotes[b.Name].Public(ctx)
	if err != nil {
		return fmt.Errorf("%s: reading its visibility: %w", b.Name, err)
	}
	if !public {
		return nil
	}
	for _, o := range others {
		r, ok := remotes[o.Name]
		if !ok {
			if r, err = o.impl.(RemoteBridge).NewRemote(ctx, o.repo, o.conf); err != nil {
				return refusePublish(b, o, err)
			}
		}
		if public, err := r.Public(ctx); err != nil || !public {
			return refusePublish(b, o, err)
		}
	}
	return nil
}

func refusePublish(b, other *Bridge, err error) error {
	why := "isn't"
	if err != nil {
		why = fmt.Sprintf("may not be (%v)", err)
	}
	return fmt.Errorf("bridge %s is public and bridge %s %s: syncing would publish what was pulled from %s. "+
		"To allow it, set git-bug.bridge.%s.%s to true", b.Name, other.Name, why, other.Name, b.Name, ConfigKeyPublishPrivate)
}
