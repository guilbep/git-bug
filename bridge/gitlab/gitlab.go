package gitlab

import (
	"errors"
	"time"

	"gitlab.com/gitlab-org/api/client-go"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
)

const (
	target = "gitlab"

	metaKeyGitlabId      = "gitlab-id"
	metaKeyGitlabUrl     = "gitlab-url"
	metaKeyGitlabLogin   = "gitlab-login"
	metaKeyGitlabProject = "gitlab-project-id"
	metaKeyGitlabBaseUrl = "gitlab-base-url"

	confKeyProjectID     = "project-id"
	confKeyGitlabBaseUrl = "base-url"
	confKeyDefaultLogin  = "default-login"

	defaultBaseURL = "https://gitlab.com/"
	defaultTimeout = 60 * time.Second
)

var ErrMissingIdentityToken = errors.New("missing identity token")

var _ core.BridgeImpl = &Gitlab{}

type Gitlab struct{}

func (Gitlab) Target() string {
	return target
}

func (g *Gitlab) LoginMetaKey() string {
	return metaKeyGitlabLogin
}

// NewImporter returns nothing: the bridge imports with the sync engine, see
// NewRemote.
func (Gitlab) NewImporter() core.Importer {
	return nil
}

// NewExporter returns nothing: the bridge exports with the sync engine, see
// NewRemote.
func (Gitlab) NewExporter() core.Exporter {
	return nil
}

func buildClient(baseURL string, token *auth.Token) (*gitlab.Client, error) {
	gitlabClient, err := gitlab.NewClient(token.Value,
		gitlab.WithBaseURL(baseURL),
	)
	if err != nil {
		return nil, err
	}

	return gitlabClient, nil
}
