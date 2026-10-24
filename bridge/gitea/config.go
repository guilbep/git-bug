package gitea

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/commands/input"
)

func (*Gitea) ValidParams() map[string]interface{} {
	return map[string]interface{}{
		"URL":        nil,
		"BaseURL":    nil,
		"CredPrefix": nil,
		"TokenRaw":   nil,
	}
}

func (*Gitea) Configure(ctx context.Context, repo *cache.RepoCache, params core.BridgeParams, interactive bool) (core.Configuration, error) {
	var err error
	repoURL := params.URL
	if repoURL == "" {
		if !interactive {
			return nil, fmt.Errorf("non-interactive mode: give the repository URL with --url")
		}
		repoURL, err = input.Prompt("Gitea repository URL", "URL", input.Required, input.IsURL)
		if err != nil {
			return nil, err
		}
	}
	baseURL, owner, project, err := splitURL(repoURL, params.BaseURL)
	if err != nil {
		return nil, err
	}

	var token *auth.Token
	switch {
	case params.CredPrefix != "":
		cred, err := auth.LoadWithPrefix(repo, params.CredPrefix)
		if err != nil {
			return nil, err
		}
		var ok bool
		if token, ok = cred.(*auth.Token); !ok {
			return nil, fmt.Errorf("the Gitea bridge only handles token credentials")
		}
	case params.TokenRaw != "":
		token = auth.NewToken(target, params.TokenRaw)
	default:
		if !interactive {
			return nil, fmt.Errorf("non-interactive mode: give the access token with --token")
		}
		fmt.Printf("Create a token at %suser/settings/applications, with the read and write\n"+
			"permissions on issues and repositories, and read on users.\n", baseURL)
		raw, err := input.Prompt("Gitea token", "token", input.Required)
		if err != nil {
			return nil, err
		}
		token = auth.NewToken(target, raw)
	}

	// validates the token and the repository
	r, err := newRemote(context.Background(), http.DefaultClient, baseURL, owner, project, "", []string{token.Value})
	if err != nil {
		return nil, err
	}
	login := r.accounts[0].Login
	var ignored any
	if err := r.do(context.Background(), token.Value, http.MethodGet, r.repoPath(""), nil, &ignored); err != nil {
		return nil, fmt.Errorf("repository %s/%s: %w", owner, project, err)
	}
	token.SetMetadata(auth.MetaKeyLogin, login)
	token.SetMetadata(auth.MetaKeyBaseURL, baseURL)

	conf := core.Configuration{
		core.ConfigKeyTarget: target,
		confKeyBaseURL:       baseURL,
		confKeyOwner:         owner,
		confKeyProject:       project,
		confKeyDefaultLogin:  login,
	}
	if !auth.IdExist(repo, token.ID()) {
		if err := auth.Store(repo, token); err != nil {
			return nil, err
		}
	}
	return conf, core.FinishConfig(ctx, repo, metaKeyGiteaLogin, login)
}

// splitURL returns the instance and repository of a repository URL. The
// instance is the URL's root, unless baseURL tells otherwise, for an instance
// served under a path.
func splitURL(repoURL, baseURL string) (base, owner, project string, err error) {
	u, err := url.Parse(strings.TrimSuffix(repoURL, ".git"))
	if err != nil || u.Host == "" {
		return "", "", "", fmt.Errorf("bad repository URL: %s", repoURL)
	}
	base = u.Scheme + "://" + u.Host + "/"
	if baseURL != "" {
		base = strings.TrimSuffix(baseURL, "/") + "/"
	}
	if !strings.HasPrefix(u.String()+"/", base) {
		return "", "", "", fmt.Errorf("the repository URL %s isn't under the base URL %s", repoURL, base)
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(u.String(), base), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("bad repository URL, expected %s<owner>/<repository>: %s", base, repoURL)
	}
	return base, parts[0], parts[1], nil
}

func (*Gitea) ValidateConfig(conf core.Configuration) error {
	if v := conf[core.ConfigKeyTarget]; v != target {
		return fmt.Errorf("unexpected target name: %v", v)
	}
	for _, key := range []string{confKeyBaseURL, confKeyOwner, confKeyProject, confKeyDefaultLogin} {
		if _, ok := conf[key]; !ok {
			return fmt.Errorf("missing %s key", key)
		}
	}
	return nil
}
