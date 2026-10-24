package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entity"
	"github.com/git-bug/git-bug/repository/repotest"
)

const (
	testRepoBaseName = "git-bug-test-github-exporter"
)

type testCase struct {
	name string
	bug  *cache.BugCache
}

func testCases(t *testing.T, repo *cache.RepoCache) []*testCase {
	// simple bug
	simpleBug, _, err := repo.Bugs().New(t.Context(), "simple bug", "new bug")
	require.NoError(t, err)
	require.NoError(t, simpleBug.Commit(t.Context()))

	// bug with comments
	bugWithComments, _, err := repo.Bugs().New(t.Context(), "bug with comments", "new bug")
	require.NoError(t, err)
	require.NoError(t, bugWithComments.Commit(t.Context()))

	_, _, err = bugWithComments.AddComment(t.Context(), "new comment")
	require.NoError(t, err)
	require.NoError(t, bugWithComments.Commit(t.Context()))

	// bug with label changes
	bugLabelChange, _, err := repo.Bugs().New(t.Context(), "bug label change", "new bug")
	require.NoError(t, err)
	require.NoError(t, bugLabelChange.Commit(t.Context()))

	_, _, err = bugLabelChange.ChangeLabels(t.Context(), []string{"bug"}, nil)
	require.NoError(t, err)

	_, _, err = bugLabelChange.ChangeLabels(t.Context(), []string{"core"}, nil)
	require.NoError(t, err)

	_, _, err = bugLabelChange.ChangeLabels(t.Context(), nil, []string{"bug"})
	require.NoError(t, err)

	_, _, err = bugLabelChange.ChangeLabels(t.Context(), []string{"InVaLiD"}, nil)
	require.NoError(t, err)

	_, _, err = bugLabelChange.ChangeLabels(t.Context(), []string{"bUG"}, nil)
	require.NoError(t, err)
	require.NoError(t, bugLabelChange.Commit(t.Context()))

	// bug with comments editions
	bugWithCommentEditions, createOp, err := repo.Bugs().New(t.Context(), "bug with comments editions", "new bug")
	require.NoError(t, err)
	require.NoError(t, bugWithCommentEditions.Commit(t.Context()))

	_, err = bugWithCommentEditions.EditComment(
		t.Context(), entity.CombineIds(bugWithCommentEditions.Id(), createOp.Id()), "first comment edited")
	require.NoError(t, err)

	commentId, _, err := bugWithCommentEditions.AddComment(t.Context(), "first comment")
	require.NoError(t, err)

	_, err = bugWithCommentEditions.EditComment(t.Context(), commentId, "first comment edited")
	require.NoError(t, err)
	require.NoError(t, bugWithCommentEditions.Commit(t.Context()))

	// bug status changed
	bugStatusChanged, _, err := repo.Bugs().New(t.Context(), "bug status changed", "new bug")
	require.NoError(t, err)
	require.NoError(t, bugStatusChanged.Commit(t.Context()))

	_, err = bugStatusChanged.Close(t.Context())
	require.NoError(t, err)

	_, err = bugStatusChanged.Open(t.Context())
	require.NoError(t, err)
	require.NoError(t, bugStatusChanged.Commit(t.Context()))

	// bug title changed
	bugTitleEdited, _, err := repo.Bugs().New(t.Context(), "bug title edited", "new bug")
	require.NoError(t, err)
	require.NoError(t, bugTitleEdited.Commit(t.Context()))

	_, err = bugTitleEdited.SetTitle(t.Context(), "bug title edited again")
	require.NoError(t, err)
	require.NoError(t, bugTitleEdited.Commit(t.Context()))

	return []*testCase{
		{
			name: "simple bug",
			bug:  simpleBug,
		},
		{
			name: "bug with comments",
			bug:  bugWithComments,
		},
		{
			name: "bug label change",
			bug:  bugLabelChange,
		},
		{
			name: "bug with comment editions",
			bug:  bugWithCommentEditions,
		},
		{
			name: "bug changed status",
			bug:  bugStatusChanged,
		},
		{
			name: "bug title edited",
			bug:  bugTitleEdited,
		},
	}
}

func TestGithubPushPull(t *testing.T) {
	// repo owner
	envUser := os.Getenv("GITHUB_USER")
	if envUser == "" {
		t.Skip("missing required environment variable: GITHUB_USER")
	}

	// token must have 'repo' and 'delete_repo' scopes
	envToken := os.Getenv("GITHUB_TOKEN")
	if envToken == "" {
		t.Skip("missing required environment variable: GITHUB_TOKEN")
	}

	// create repo backend
	repo := repotest.NewGoGitRepo(t, false)

	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	defer backend.Close()

	// set author identity
	login := "identity-test"
	author, err := backend.Identities().New("test identity", "test@test.org")
	require.NoError(t, err)
	author.SetMetadata(metaKeyGithubLogin, login)
	err = author.Commit()
	require.NoError(t, err)

	err = backend.SetUserIdentity(t.Context(), author)
	require.NoError(t, err)

	// Setup token + cleanup
	token := auth.NewToken(target, envToken)
	token.SetMetadata(auth.MetaKeyLogin, login)
	err = auth.Store(repo, token)
	require.NoError(t, err)

	defer auth.Remove(repo, token.ID())

	tests := testCases(t, backend)

	// On interrupt, cancel ctx instead of killing the process, so that the test
	// fails normally and the cleanups below (removing the Github repository) run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	t.Cleanup(stop)

	// generate project name
	projectName := generateRepoName()

	// create target Github repository
	err = createRepository(ctx, projectName, envToken)
	require.NoError(t, err)

	slog.Info("created github repository", "name", projectName)

	// Make sure to remove the Github repository when the test end.
	// ctx is not used, as it's already canceled after an interrupt.
	t.Cleanup(func() {
		if err := deleteRepository(context.Background(), projectName, envUser, envToken); err != nil {
			t.Error(err)
			return
		}
		fmt.Println("deleted repository:", projectName)
	})

	// Let Github handle the repo creation and update all their internal caches.
	// Avoid HTTP error 404 retrieving repository node id
	err = waitRepository(ctx, projectName, envUser, envToken, 60*time.Second)
	require.NoError(t, err)

	newEngine := func(backend *cache.RepoCache) *core.Engine {
		r, err := newRemote(ctx, envUser, projectName, login, []*auth.Token{token}, graphqlClient)
		require.NoError(t, err)
		e, err := core.NewEngine(ctx, backend, r, core.SyncOptions{DefaultLogin: login, LoginKey: metaKeyGithubLogin})
		require.NoError(t, err)
		return e
	}

	start := time.Now()

	// export all bugs, as `bridge push` does
	for result := range newEngine(backend).Push(ctx) {
		require.NotEqual(t, core.ExportEventError, result.Event, result.String())
	}

	fmt.Printf("test repository exported in %f seconds\n", time.Since(start).Seconds())

	repoTwo := repotest.NewGoGitRepo(t, false)

	// create a second backend
	backendTwo, err := cache.NewRepoCacheNoEvents(repoTwo, nil)
	require.NoError(t, err)
	defer backendTwo.Close()
	two, err := backendTwo.Identities().New("other identity", "other@test.org")
	require.NoError(t, err)
	require.NoError(t, backendTwo.SetUserIdentity(ctx, two))

	// import all exported bugs to the second backend
	for result := range newEngine(backendTwo).Pull(ctx, time.Time{}) {
		require.NotEqual(t, core.ImportEventError, result.Event, result.String())
	}

	require.Len(t, backendTwo.Bugs().AllIds(), len(tests))

	key := "sync:github.com/" + strings.ToLower(envUser+"/"+projectName) + ":id"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := tt.bug.Snapshot()

			// get bug github ID
			bugGithubID, ok := snap.GetCreateMetadata(key)
			require.True(t, ok)

			// retrieve bug from backendTwo
			importedBug, err := backendTwo.Bugs().ResolveBugCreateMetadata(t.Context(), key, bugGithubID)
			require.NoError(t, err)
			imported := importedBug.Snapshot()

			// the same issue, from GitHub's point of view
			require.Equal(t, snap.Title, imported.Title)
			require.Equal(t, snap.Status, imported.Status)
			require.ElementsMatch(t, snap.Labels, imported.Labels)
			require.Len(t, imported.Comments, len(snap.Comments))
			for i := range snap.Comments {
				require.Equal(t, snap.Comments[i].Message, imported.Comments[i].Message)
			}

			// verify bugs are tagged with origin=github
			issueOrigin, ok := imported.GetCreateMetadata(core.MetaKeyOrigin)
			require.True(t, ok)
			require.Equal(t, issueOrigin, target)
		})
	}
}

func generateRepoName() string {
	var letterRunes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	b := make([]rune, 8)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return fmt.Sprintf("%s-%s", testRepoBaseName, string(b))
}

// create repository need a token with scope 'repo'
func createRepository(ctx context.Context, project, token string) error {
	// This function use the V3 Github API because repository creation is not supported yet on the V4 API.
	url := fmt.Sprintf("%s/user/repos", githubV3Url)

	params := struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Private     bool   `json:"private"`
		HasIssues   bool   `json:"has_issues"`
	}{
		Name:        project,
		Description: "git-bug exporter temporary test repository",
		Private:     true,
		HasIssues:   true,
	}

	data, err := json.Marshal(params)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(data))
	if err != nil {
		return err
	}

	// need the token for private repositories
	req.Header.Set("Authorization", fmt.Sprintf("token %s", token))

	client := &http.Client{
		Timeout: defaultTimeout,
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("error creating repository: %s: %s", resp.Status, body)
	}

	return nil
}

// waitRepository polls the V3 API until the repository is visible, as Github
// can take a moment before a freshly created repository is usable.
func waitRepository(ctx context.Context, project, owner, token string, timeout time.Duration) error {
	url := fmt.Sprintf("%s/repos/%s/%s", githubV3Url, owner, project)

	client := &http.Client{
		Timeout: defaultTimeout,
	}

	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", fmt.Sprintf("token %s", token))

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("unexpected status: %s", resp.Status)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("repository %s/%s not available after %s: %w", owner, project, timeout, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// delete repository need a token with scope 'delete_repo'
func deleteRepository(ctx context.Context, project, owner, token string) error {
	// This function use the V3 Github API because repository removal is not supported yet on the V4 API.
	url := fmt.Sprintf("%s/repos/%s/%s", githubV3Url, owner, project)

	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}

	// need the token for private repositories
	req.Header.Set("Authorization", fmt.Sprintf("token %s", token))

	client := &http.Client{
		Timeout: defaultTimeout,
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("error deleting repository")
	}

	return nil
}
