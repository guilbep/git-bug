package gitlab

import (
	"errors"
	"slices"
	"strings"
	"time"

	"gitlab.com/gitlab-org/api/client-go"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/repository"
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

// sameBaseURL compares base URLs written with or without their trailing
// slash, by hand or by older versions.
func sameBaseURL(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

// instanceTokens lists the GitLab tokens of an instance.
func instanceTokens(repo repository.RepoKeyring, baseUrl string, opts ...auth.ListOption) ([]auth.Credential, error) {
	creds, err := auth.List(repo, append(opts, auth.WithTarget(target), auth.WithKind(auth.KindToken))...)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(creds, func(c auth.Credential) bool {
		return !sameBaseURL(c.Metadata()[auth.MetaKeyBaseURL], baseUrl)
	}), nil
}

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
