package github

import (
	"context"
	"errors"
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

// GitHub's assignees are users, its assignee:* labels labels: the engine
// makes them git-bug's labels.
func TestGithubAssigneesAreUsers(t *testing.T) {
	g := newGithubForge(t, "owner")
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	g.assign("alice", ref, "bob", true)
	alice.SetLabels(ref, []string{"assignee:carol", "bug"})

	issue := g.Issue(ref.Number)
	require.Equal(t, []string{"assignee:carol", "bug"}, issue.Labels)
	require.Equal(t, []core.User{{ID: "bob", Login: "bob"}}, issue.Assignees)
	require.Equal(t, core.EventAssignees, issue.Events[0].Kind)
	require.Equal(t, []core.User{{ID: "bob", Login: "bob"}}, issue.Events[0].Assigned)
	require.Equal(t, "alice", issue.Events[0].Author.Login)

	g.assign("alice", ref, "bob", false)
	issue = g.Issue(ref.Number)
	require.Empty(t, issue.Assignees)
	require.Equal(t, []core.User{{ID: "bob", Login: "bob"}}, issue.Events[len(issue.Events)-1].Unassigned)
}

// Assign reads GitHub's assignees back: who it silently leaves out, and a
// login without a user, like a bot's, are refused.
func TestGithubAssign(t *testing.T) {
	ctx := context.Background()
	g := newGithubForge(t, "owner")
	g.fake.unassignable["eve"] = true
	g.fake.noUser["nobody"] = true
	bot := g.accounts[0]
	ref := g.UserAct("alice").CreateIssue("title", "body")
	bob, eve, nobody := core.User{ID: "bob"}, core.User{Login: "eve"}, core.User{ID: "nobody", Login: "nobody"}

	err := g.Assign(ctx, bot, ref, []core.User{bob, eve, nobody}, nil)
	var refused core.RefusedUsers
	require.True(t, errors.As(err, &refused), "%v", err)
	require.ElementsMatch(t, core.RefusedUsers{eve, nobody}, refused)
	require.ErrorIs(t, err, core.ErrRefused)
	labels, assignees := g.held(ref)
	require.Empty(t, labels)
	require.Equal(t, []string{"bob"}, assignees)

	err = g.Assign(ctx, bot, ref, nil, []core.User{bob, nobody})
	require.Equal(t, core.RefusedUsers{nobody}, err, "without a user, like a bot, it may still be assigned")
	_, assignees = g.held(ref)
	require.Empty(t, assignees)
}

// ChangeLabels changes labels only: an assignee's label is a label.
func TestGithubChangeLabelsLeavesAssignees(t *testing.T) {
	ctx := context.Background()
	g := newGithubForge(t, "owner")
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.assign("alice", ref, "bob", true)

	require.NoError(t, g.ChangeLabels(ctx, g.accounts[0], ref, []string{"assignee:carol"}, []string{"assignee:bob"}))
	labels, assignees := g.held(ref)
	require.Equal(t, []string{"assignee:carol"}, labels)
	require.Equal(t, []string{"bob"}, assignees)
}

// syncing returns a git-bug repository, and a sync of it with GitHub that
// returns its number of warnings.
// mode is how GitHub holds assignees, AssigneesNative when nil.
func syncing(t *testing.T, g githubForge, mode ...*string) (*cache.RepoCache, func() int) {
	backend, err := cache.NewRepoCacheNoEvents(repotest.NewGoGitRepo(t, false), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	me, err := backend.Identities().New("Me", "me@example.com")
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(me))
	return backend, func() int {
		opts := core.SyncOptions{Foreign: true, DefaultLogin: "bot", LoginKey: metaKeyGithubLogin}
		if len(mode) > 0 {
			opts.Assignees = *mode[0]
		}
		if !g.kinds {
			opts.Kinds = core.ModeLabels
		}
		e, err := core.NewEngine(context.Background(), backend, g, opts)
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
	// people git-bug knows
	g.UserAct("bob").Comment(ref, "on it")
	g.UserAct("carol").Comment(ref, "me too")

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

// An old way's label of no one, removed in git-bug, comes off GitHub.
func TestGithubSyncRemovesOldWayLabelWithoutUser(t *testing.T) {
	g := newGithubForge(t, "owner")
	g.fake.noUser["nobody"] = true
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	alice.SetLabels(ref, []string{"assignee:nobody"})
	backend, sync := syncing(t, g)
	require.Equal(t, 1, sync(), "no one")

	b, err := backend.Bugs().Resolve(backend.Bugs().AllIds()[0])
	require.NoError(t, err)
	_, _, err = b.ChangeLabels(nil, []string{"assignee:nobody"})
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	require.Zero(t, sync())
	require.Zero(t, sync(), "claims the removal's echo")
	writes, ops := g.Writes(), len(b.Snapshot().Operations)
	require.Zero(t, sync())
	require.Equal(t, writes, g.Writes(), "nothing more sent")
	require.Len(t, b.Snapshot().Operations, ops, "nothing more pulled")
	require.Empty(t, b.Snapshot().Labels)
	labels, assignees := g.held(ref)
	require.Empty(t, labels)
	require.Empty(t, assignees)
}

// Switching how GitHub holds assignees moves them, once each way.
func TestGithubSyncSwitchesAssigneesMode(t *testing.T) {
	g := newGithubForge(t, "owner")
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.assign("alice", ref, "bob", true)
	mode := core.AssigneesNative
	backend, sync := syncing(t, g, &mode)
	require.Zero(t, sync())
	steady := func() {
		t.Helper()
		b, err := backend.Bugs().Resolve(backend.Bugs().AllIds()[0])
		require.NoError(t, err)
		writes, ops := g.Writes(), len(b.Snapshot().Operations)
		require.Zero(t, sync())
		require.Equal(t, writes, g.Writes(), "nothing more sent")
		require.Len(t, b.Snapshot().Operations, ops, "nothing more pulled")
		require.Equal(t, []common.Label{"assignee:bob"}, b.Snapshot().Labels)
	}

	mode = core.AssigneesLabels
	require.Zero(t, sync())
	labels, assignees := g.held(ref)
	require.Equal(t, []string{"assignee:bob"}, labels)
	require.Empty(t, assignees)
	steady()

	mode = core.AssigneesNative
	require.Zero(t, sync())
	labels, assignees = g.held(ref)
	require.Empty(t, labels)
	require.Equal(t, []string{"bob"}, assignees)
	steady()
}

// A bot GitHub won't unassign, as user(login:) finds none, gets its label
// beside, once.
func TestGithubLabelsModeBot(t *testing.T) {
	g := newGithubForge(t, "owner")
	g.fake.noUser["Copilot"] = true
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.assign("alice", ref, "Copilot", true)
	mode := core.AssigneesLabels
	_, sync := syncing(t, g, &mode)
	require.Zero(t, sync())
	labels, assignees := g.held(ref)
	require.Equal(t, []string{"assignee:Copilot"}, labels)
	require.Equal(t, []string{"Copilot"}, assignees)
	writes := g.Writes()
	require.Zero(t, sync())
	require.Equal(t, writes, g.Writes())
}

// Who GitHub refused to assign goes out as a label once the project holds
// assignees as labels, beside those moved, and stays a label back in native.
func TestGithubSwitchSendsRefusedAsLabels(t *testing.T) {
	g := newGithubForge(t, "owner")
	g.fake.unassignable["eve"] = true
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.UserAct("eve").Comment(ref, "hi")
	g.assign("alice", ref, "bob", true)
	mode := core.AssigneesNative
	backend, sync := syncing(t, g, &mode)
	require.Zero(t, sync())
	b, err := backend.Bugs().Resolve(backend.Bugs().AllIds()[0])
	require.NoError(t, err)
	_, _, err = b.ChangeLabels([]string{"assignee:eve"}, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	require.Equal(t, 1, sync(), "eve refused")
	require.Zero(t, sync())

	mode = core.AssigneesLabels
	require.Zero(t, sync())
	labels, assignees := g.held(ref)
	require.Equal(t, []string{"assignee:bob", "assignee:eve"}, labels)
	require.Empty(t, assignees)
	writes := g.Writes()
	require.Zero(t, sync())
	require.Equal(t, writes, g.Writes())

	mode = core.AssigneesNative
	require.Equal(t, 1, sync(), "eve refused again")
	labels, assignees = g.held(ref)
	require.Equal(t, []string{"assignee:eve"}, labels, "kept, as GitHub can't assign her")
	require.Equal(t, []string{"bob"}, assignees)
	writes = g.Writes()
	require.Zero(t, sync())
	require.Equal(t, writes, g.Writes())
	require.Equal(t, []common.Label{"assignee:bob", "assignee:eve"}, b.Snapshot().Labels)
}
