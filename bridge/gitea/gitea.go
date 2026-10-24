// Package gitea contains the Gitea bridge, which works with Forgejo too.
package gitea

import (
	"github.com/git-bug/git-bug/bridge/core"
)

const (
	target = "gitea"

	metaKeyGiteaLogin = "gitea-login"

	confKeyBaseURL      = "base-url"
	confKeyOwner        = "owner"
	confKeyProject      = "project"
	confKeyDefaultLogin = core.ConfigKeyDefaultLogin
)

var _ core.BridgeImpl = &Gitea{}

type Gitea struct{}

func (*Gitea) Target() string {
	return target
}

func (*Gitea) LoginMetaKey() string {
	return metaKeyGiteaLogin
}

// NewImporter returns nothing: the bridge imports with the sync engine, see
// NewRemote.
func (*Gitea) NewImporter() core.Importer {
	return nil
}

// NewExporter returns nothing: the bridge exports with the sync engine, see
// NewRemote.
func (*Gitea) NewExporter() core.Exporter {
	return nil
}
