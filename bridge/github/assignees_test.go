package github

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/repository/repotest"
)

// assign assigns, or unassigns, someone on GitHub, as login.
func (g githubForge) assign(login string, ref core.Ref, assignee string, assigned bool) {
	githubActor{fake: g.fake, login: login}.do(func() {
		issue, err := g.fake.find(ref.ID)
		if err != nil {
			panic(err)
		}
		g.fake.setAssigned(login, issue, assignee, assigned)
	})
}

// held returns an issue's labels and assignees as GitHub holds them.
func (g githubForge) held(ref core.Ref) (labels, assignees []string) {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	issue, err := g.fake.find(ref.ID)
	if err != nil {
		panic(err)
	}
	return slices.Clone(issue.labels), slices.Sorted(slices.Values(issue.assignees))
}

func TestGithubAssigneesAreLabels(t *testing.T) {
	g := newGithubForge(t, "owner")
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	g.assign("alice", ref, "bob", true)
	alice.SetLabels(ref, []string{"assignee:carol", "bug"})

	issue := g.Issue(ref.Number)
	require.Equal(t, []string{"assignee:bob", "assignee:carol", "bug"}, issue.Labels)
	require.Equal(t, []string{"assignee:carol"}, issue.Resend, "held the old way")
	require.Equal(t, []string{"assignee:bob"}, issue.Events[0].Added)
	require.Equal(t, "alice", issue.Events[0].Author.Login)

	g.assign("alice", ref, "bob", false)
	issue = g.Issue(ref.Number)
	require.Equal(t, []string{"assignee:carol", "bug"}, issue.Labels)
	require.Equal(t, []string{"assignee:bob"}, issue.Events[len(issue.Events)-1].Removed)
}

// Someone both assigned and labeled the old way is one label: assigning or
// unlabeling them then is no change.
func TestGithubAssigneeAndOldWayLabelAreOne(t *testing.T) {
	g := newGithubForge(t, "owner")
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	alice.SetLabels(ref, []string{"assignee:bob"})
	g.assign("alice", ref, "bob", true)
	alice.SetLabels(ref, nil)

	issue := g.Issue(ref.Number)
	require.Equal(t, []string{"assignee:bob"}, issue.Labels)
	require.Empty(t, issue.Resend)
	require.Len(t, issue.Events, 1)
}

func TestGithubChangeLabelsAssigns(t *testing.T) {
	ctx := context.Background()
	g := newGithubForge(t, "owner")
	g.fake.unassignable["eve"] = true
	g.fake.noUser["nobody"] = true
	bot := g.accounts[0]
	ref := g.UserAct("alice").CreateIssue("title", "body")

	err := g.ChangeLabels(ctx, bot, ref, []string{"a", "assignee:bob", "assignee:eve", "assignee:nobody"}, nil)
	var refused core.RefusedLabels
	require.True(t, errors.As(err, &refused), "%v", err)
	require.Equal(t, core.RefusedLabels{"assignee:eve", "assignee:nobody"}, refused)
	require.ErrorIs(t, err, core.ErrRefused)
	labels, assignees := g.held(ref)
	require.Equal(t, []string{"a"}, labels)
	require.Equal(t, []string{"bob"}, assignees)
	require.Equal(t, []string{"a"}, slices.Collect(maps.Values(g.fake.labels)), "no assignee label on GitHub")

	err = g.ChangeLabels(ctx, bot, ref, nil, []string{"assignee:bob", "assignee:nobody"})
	require.Equal(t, core.RefusedLabels{"assignee:nobody"}, err, "without a user, like a bot, it may still be assigned")
	labels, assignees = g.held(ref)
	require.Equal(t, []string{"a"}, labels)
	require.Empty(t, assignees)
}

// Sent again, an old way's label comes off the issue once its login is
// assigned, and stays when GitHub can't assign it.
func TestGithubChangeLabelsMovesOldWayLabels(t *testing.T) {
	g := newGithubForge(t, "owner")
	g.fake.unassignable["eve"] = true
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.UserAct("alice").SetLabels(ref, []string{"assignee:bob", "assignee:eve"})
	events := len(g.Issue(ref.Number).Events)

	err := g.ChangeLabels(context.Background(), g.accounts[0], ref, []string{"assignee:bob", "assignee:eve"}, nil)
	require.Equal(t, core.RefusedLabels{"assignee:eve"}, err)
	labels, assignees := g.held(ref)
	require.Equal(t, []string{"assignee:eve"}, labels)
	require.Equal(t, []string{"bob"}, assignees)
	issue := g.Issue(ref.Number)
	require.Equal(t, []string{"assignee:bob", "assignee:eve"}, issue.Labels)
	require.Equal(t, []string{"assignee:eve"}, issue.Resend)
	require.Len(t, issue.Events, events, "no change for the engine")
}

// syncing returns a git-bug repository, and a sync of it with GitHub that
// returns its number of warnings.
func syncing(t *testing.T, g githubForge) (*cache.RepoCache, func() int) {
	backend, err := cache.NewRepoCacheNoEvents(repotest.NewGoGitRepo(t, false), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	me, err := backend.Identities().New("Me", "me@example.com")
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(me))
	return backend, func() int {
		e, err := core.NewEngine(context.Background(), backend, g, core.SyncOptions{Foreign: true, DefaultLogin: "bot", LoginKey: metaKeyGithubLogin})
		require.NoError(t, err)
		warnings := 0
		for r := range e.Pull(context.Background(), time.Time{}) {
			require.NotEqual(t, core.ImportEventError, r.Event, r.String())
			if r.Event == core.ImportEventWarning {
				warnings++
			}
		}
		for r := range e.Push(context.Background()) {
			require.NotEqual(t, core.ExportEventError, r.Event, r.String())
			if r.Event == core.ExportEventWarning {
				warnings++
			}
		}
		return warnings
	}
}

// A sync moves GitHub's assignee labels to its assignees, sends git-bug's
// assignees as GitHub's, and then has nothing more to do.
func TestGithubSyncAssignees(t *testing.T) {
	g := newGithubForge(t, "owner")
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	alice.SetLabels(ref, []string{"assignee:bob", "bug"})

	backend, sync := syncing(t, g)
	sync()
	labels, assignees := g.held(ref)
	require.Equal(t, []string{"bug"}, labels)
	require.Equal(t, []string{"bob"}, assignees)

	b, err := backend.Bugs().Resolve(backend.Bugs().AllIds()[0])
	require.NoError(t, err)
	_, _, err = b.ChangeLabels([]string{"assignee:carol"}, []string{"assignee:bob"})
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	sync()
	labels, assignees = g.held(ref)
	require.Equal(t, []string{"bug"}, labels)
	require.Equal(t, []string{"carol"}, assignees)

	sync()
	writes, ops := g.Writes(), len(b.Snapshot().Operations)
	sync()
	require.Equal(t, writes, g.Writes(), "nothing more sent")
	require.Len(t, b.Snapshot().Operations, ops, "nothing more pulled")
	require.Equal(t, []common.Label{"assignee:carol", "bug"}, b.Snapshot().Labels)
}

// Unassigning a login GitHub finds no user of, like a bot's, is refused, with
// a warning: the next pull doesn't assign it again in git-bug.
func TestGithubSyncUnassignBot(t *testing.T) {
	g := newGithubForge(t, "owner")
	g.fake.noUser["Copilot"] = true
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.assign("alice", ref, "Copilot", true)
	backend, sync := syncing(t, g)
	require.Zero(t, sync())

	b, err := backend.Bugs().Resolve(backend.Bugs().AllIds()[0])
	require.NoError(t, err)
	_, _, err = b.ChangeLabels(nil, []string{"assignee:Copilot"})
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	require.Equal(t, 1, sync())
	require.Zero(t, sync())
	require.Zero(t, sync())
	require.Empty(t, b.Snapshot().Labels)
	_, assignees := g.held(ref)
	require.Equal(t, []string{"Copilot"}, assignees)
}
