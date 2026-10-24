package core

import (
	"context"
	"time"

	"github.com/git-bug/git-bug/cache"
)

type Configuration map[string]string

type BridgeImpl interface {
	// Target return the target of the bridge (e.g.: "github")
	Target() string

	// NewImporter return an Importer implementation if the import is supported
	NewImporter() Importer

	// NewExporter return an Exporter implementation if the export is supported
	NewExporter() Exporter

	// Configure handle the user interaction and return a key/value configuration
	// for future use.
	Configure(ctx context.Context, repo *cache.RepoCache, params BridgeParams, interactive bool) (Configuration, error)

	// The set of the BridgeParams fields supported
	ValidParams() map[string]interface{}

	// ValidateConfig check the configuration for error
	ValidateConfig(conf Configuration) error

	// LoginMetaKey return the metadata key used to store the remote bug-tracker login
	// on the user identity. The corresponding value is used to match identities and
	// credentials.
	LoginMetaKey() string
}

// RemoteBridge is implemented by bridges ported to the sync engine: they
// import and export through it, and can take part in `bridge sync`.
type RemoteBridge interface {
	NewRemote(ctx context.Context, repo *cache.RepoCache, conf Configuration) (Remote, error)
}

type Importer interface {
	Init(ctx context.Context, repo *cache.RepoCache, conf Configuration) error
	ImportAll(ctx context.Context, repo *cache.RepoCache, since time.Time) (<-chan ImportResult, error)
}

type Exporter interface {
	Init(ctx context.Context, repo *cache.RepoCache, conf Configuration) error
	ExportAll(ctx context.Context, repo *cache.RepoCache, since time.Time) (<-chan ExportResult, error)
}
