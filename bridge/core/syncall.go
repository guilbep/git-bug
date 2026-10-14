package core

import (
	"context"
	"fmt"
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
// bridge pulls reaches the others in the same run.
func SyncAll(ctx context.Context, repo *cache.RepoCache, names []string, dryRun bool) (<-chan SyncResult, error) {
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

	out := make(chan SyncResult)
	go func() {
		defer close(out)
		defer unlock()

		for i, b := range bridges {
			// a bit before, just to be sure
			start := time.Now().Add(-5 * time.Second)
			ok := true
			for r := range engines[i].Pull(ctx, b.lastImportTime()) {
				ok = ok && r.Event != ImportEventError
				out <- SyncResult{Bridge: b.Name, Import: &r}
			}
			if ok && !dryRun {
				_ = b.storeImportTime(start)
			}
		}
		for i, b := range bridges {
			for r := range engines[i].Push(ctx) {
				out <- SyncResult{Bridge: b.Name, Export: &r}
			}
		}
	}()
	return out, nil
}
