package gitlab

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/git-bug/git-bug/entity"
	"github.com/git-bug/git-bug/repository/repotest"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/cache"
)

const (
	testRepoBaseName = "git-bug-test-gitlab-exporter"
)

type testCase struct {
	name string
	bug  *cache.BugCache
}

func testCases(t *testing.T, repo *cache.RepoCache) []*testCase {
	// simple bug
	simpleBug, _, err := repo.Bugs().New("simple bug", "new bug")
	require.NoError(t, err)

	// bug with comments
	bugWithComments, _, err := repo.Bugs().New("bug with comments", "new bug")
	require.NoError(t, err)

	_, _, err = bugWithComments.AddComment("new comment")
	require.NoError(t, err)
	require.NoError(t, bugWithComments.Commit())

	// bug with label changes
	bugLabelChange, _, err := repo.Bugs().New("bug label change", "new bug")
	require.NoError(t, err)

	_, _, err = bugLabelChange.ChangeLabels([]string{"bug"}, nil)
	require.NoError(t, err)

	_, _, err = bugLabelChange.ChangeLabels([]string{"core"}, nil)
	require.NoError(t, err)

	_, _, err = bugLabelChange.ChangeLabels(nil, []string{"bug"})
	require.NoError(t, err)
	require.NoError(t, bugLabelChange.Commit())

	// bug with comments editions
	bugWithCommentEditions, createOp, err := repo.Bugs().New("bug with comments editions", "new bug")
	require.NoError(t, err)

	_, err = bugWithCommentEditions.EditComment(
		entity.CombineIds(bugWithCommentEditions.Id(), createOp.Id()), "first comment edited")
	require.NoError(t, err)

	commentId, _, err := bugWithCommentEditions.AddComment("first comment")
	require.NoError(t, err)

	_, err = bugWithCommentEditions.EditComment(commentId, "first comment edited")
	require.NoError(t, err)
	require.NoError(t, bugWithCommentEditions.Commit())

	// bug status changed
	bugStatusChanged, _, err := repo.Bugs().New("bug status changed", "new bug")
	require.NoError(t, err)

	_, err = bugStatusChanged.Close()
	require.NoError(t, err)

	_, err = bugStatusChanged.Open()
	require.NoError(t, err)
	require.NoError(t, bugStatusChanged.Commit())

	// bug title changed
	bugTitleEdited, _, err := repo.Bugs().New("bug title edited", "new bug")
	require.NoError(t, err)

	_, err = bugTitleEdited.SetTitle("bug title edited again")
	require.NoError(t, err)
	require.NoError(t, bugTitleEdited.Commit())

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

// syncEngine returns the sync engine of a GitLab project, acting with token.
func syncEngine(t *testing.T, ctx context.Context, backend *cache.RepoCache, baseURL string, projectID int64, login string, token *auth.Token, foreign bool) *core.Engine {
	r, err := newRemote(ctx, baseURL, strconv.FormatInt(projectID, 10), login, []*auth.Token{token})
	require.NoError(t, err)
	e, err := core.NewEngine(ctx, backend, r, core.SyncOptions{
		Foreign:      foreign,
		DefaultLogin: login,
		LoginKey:     metaKeyGitlabLogin,
	})
	require.NoError(t, err)
	return e
}

// testBaseURL is the GitLab instance of the end-to-end tests, gitlab.com
// unless GITLAB_BASE_URL says otherwise.
func testBaseURL() string {
	if u := os.Getenv("GITLAB_BASE_URL"); u != "" {
		return u
	}
	return defaultBaseURL
}

func TestGitlabPushPull(t *testing.T) {
	// token must have 'repo' and 'delete_repo' scopes
	envToken := os.Getenv("GITLAB_API_TOKEN")
	if envToken == "" {
		t.Skip("Env var GITLAB_API_TOKEN missing")
	}
	baseURL := testBaseURL()

	// create repo backend
	repo := repotest.NewGoGitRepo(t, false)

	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	defer backend.Close()

	token := auth.NewToken(target, envToken)
	login, err := getLoginFromToken(baseURL, token)
	require.NoError(t, err)

	// set author identity
	author, err := backend.Identities().New("test identity", "test@test.org")
	require.NoError(t, err)
	author.SetMetadata(metaKeyGitlabLogin, login)
	err = author.Commit()
	require.NoError(t, err)

	err = backend.SetUserIdentity(author)
	require.NoError(t, err)

	tests := testCases(t, backend)

	// On interrupt, cancel ctx instead of killing the process, so that the test
	// fails normally and the cleanups below (removing the Gitlab repository) run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	t.Cleanup(stop)

	// generate project name
	projectName := generateRepoName()

	// create target Gitlab repository
	projectID, err := createRepository(ctx, baseURL, projectName, token)
	require.NoError(t, err)

	fmt.Printf("created project: %s (%d)\n", projectName, projectID)

	// Make sure to remove the Gitlab repository when the test end.
	// ctx is not used, as it's already canceled after an interrupt.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
		defer cancel()
		if err := deleteRepository(ctx, baseURL, projectID, token); err != nil {
			t.Error(err)
			return
		}
		fmt.Printf("deleted repository: %s (%d)\n", projectName, projectID)
	})

	start := time.Now()

	// export all bugs, as `bridge push` does
	for result := range syncEngine(t, ctx, backend, baseURL, projectID, login, token, false).Push(ctx) {
		require.NotEqual(t, core.ExportEventError, result.Event, result.String())
	}

	fmt.Printf("repository exported in %f seconds\n", time.Since(start).Seconds())

	repoTwo := repotest.NewGoGitRepo(t, false)

	// create a second backend
	backendTwo, err := cache.NewRepoCacheNoEvents(repoTwo, nil)
	require.NoError(t, err)
	defer backendTwo.Close()
	two, err := backendTwo.Identities().New("other identity", "other@test.org")
	require.NoError(t, err)
	require.NoError(t, backendTwo.SetUserIdentity(two))

	// import all exported bugs to the second backend
	for result := range syncEngine(t, ctx, backendTwo, baseURL, projectID, login, token, false).Pull(ctx, time.Time{}) {
		require.NotEqual(t, core.ImportEventError, result.Event, result.String())
	}

	require.Len(t, backendTwo.Bugs().AllIds(), len(tests))

	key := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://"), "/") +
		"/" + strconv.FormatInt(projectID, 10)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := tt.bug.Snapshot()

			// get bug gitlab ID
			bugGitlabID, ok := snap.GetCreateMetadata("sync:" + key + ":id")
			require.True(t, ok)

			// retrieve bug from backendTwo
			importedBug, err := backendTwo.Bugs().ResolveBugCreateMetadata("sync:"+key+":id", bugGitlabID)
			require.NoError(t, err)
			imported := importedBug.Snapshot()

			// the same issue, from GitLab's point of view
			require.Equal(t, snap.Title, imported.Title)
			require.Equal(t, snap.Status, imported.Status)
			require.ElementsMatch(t, snap.Labels, imported.Labels)
			require.Len(t, imported.Comments, len(snap.Comments))
			for i := range snap.Comments {
				require.Equal(t, snap.Comments[i].Message, imported.Comments[i].Message)
			}

			// verify bugs are tagged with origin=gitlab
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
func createRepository(ctx context.Context, baseURL, name string, token *auth.Token) (int64, error) {
	client, err := buildClient(baseURL, token)
	if err != nil {
		return 0, err
	}

	project, _, err := client.Projects.CreateProject(
		&gitlab.CreateProjectOptions{
			Name: gitlab.Ptr(name),
		},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return 0, err
	}

	// fmt.Println("Project URL:", project.WebURL)

	return project.ID, nil
}

// delete repository need a token with scope 'delete_repo'
func deleteRepository(ctx context.Context, baseURL string, project int64, token *auth.Token) error {
	client, err := buildClient(baseURL, token)
	if err != nil {
		return err
	}

	_, err = client.Projects.DeleteProject(project, nil, gitlab.WithContext(ctx))
	return err
}
