package gitlab

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gitlab.com/gitlab-org/api/client-go"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/repository/repotest"
)

// assign sets an issue's assignees on GitLab, as login.
func (g gitlabForge) assign(login string, ref core.Ref, assignees ...string) {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	var ids []any
	for _, a := range assignees {
		ids = append(ids, float64(g.fake.userNamed(a).ID))
	}
	g.fake.setAssignees(g.fake.userNamed(login), gitlabActor{fake: g.fake}.find(ref), ids)
}

// assignees returns the logins of an issue's assignees on GitLab.
func (g gitlabForge) assignees(ref core.Ref) []string {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	res := []string{}
	for _, a := range (gitlabActor{fake: g.fake}).find(ref).Assignees {
		res = append(res, a.Username)
	}
	return res
}

// GitLab's assignees are users of the issue, labels its labels.
func TestGitlabAssigneesAreUsers(t *testing.T) {
	g := newGitlabForge(t)
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.UserAct("alice").SetLabels(ref, []string{"assignee:carol"})
	g.assign("alice", ref, "bob")

	issue := g.Issue(ref.Number)
	require.Equal(t, []string{"assignee:carol"}, issue.Labels)
	require.Len(t, issue.Assignees, 1)
	require.Equal(t, "bob", issue.Assignees[0].Login)
	require.NotEmpty(t, issue.Assignees[0].ID)
}

// Assign takes users by id, or by login; it reads the assignees back, as
// the free edition keeps one, and refuses a login without a user.
func TestGitlabAssign(t *testing.T) {
	ctx := context.Background()
	g := newGitlabForge(t)
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.UserAct("bob")
	g.UserAct("carol")
	g.fake.mu.Lock()
	bob := core.User{ID: strconv.FormatInt(g.fake.userNamed("bob").ID, 10)}
	g.fake.mu.Unlock()
	carol := core.User{Login: "Carol"} // by login, its case aside
	nobody := core.User{Login: "nobody"}
	bot := g.accounts[0]

	err := g.Assign(ctx, bot, ref, []core.User{bob, carol, nobody}, nil)
	require.ErrorIs(t, err, core.ErrRefused)
	refused, ok := errors.AsType[core.RefusedUsers](err)
	require.True(t, ok, "%v", err)
	require.ElementsMatch(t, core.RefusedUsers{carol, nobody}, refused, "one assignee, and nobody is no one")
	require.Equal(t, []string{"bob"}, g.assignees(ref))

	writes := g.Writes()
	require.NoError(t, g.Assign(ctx, bot, ref, []core.User{bob}, []core.User{nobody}))
	require.Equal(t, writes, g.Writes(), "no change, no write")

	require.NoError(t, g.Assign(ctx, bot, ref, []core.User{carol}, []core.User{bob}))
	require.Equal(t, []string{"carol"}, g.assignees(ref))
	require.NoError(t, g.Assign(ctx, bot, ref, nil, []core.User{carol}))
	require.Empty(t, g.assignees(ref))
}

// GitLab moves updated_at about once a minute for label and assignee
// changes: issues are listed from a while before.
func TestGitlabListsWithMargin(t *testing.T) {
	g := newGitlabForge(t)
	ref := g.UserAct("alice").CreateIssue("title", "body")
	since := time.Now()
	g.fake.mu.Lock()
	g.fake.issues[0].UpdatedAt = gitlab.Ptr(since.Add(-time.Minute))
	g.fake.mu.Unlock()

	n := 0
	for issue, err := range g.Issues(context.Background(), since) {
		require.NoError(t, err)
		require.Equal(t, ref.Number, issue.Number)
		n++
	}
	require.Equal(t, 1, n)
}

// A sync moves old way's labels to GitLab's assignees, sends git-bug's
// assignees as GitLab's, and then has nothing more to do.
func TestGitlabSyncAssignees(t *testing.T) {
	ctx := t.Context()
	g := newGitlabForge(t)
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	alice.SetLabels(ref, []string{"assignee:bob", "bug"})
	// people git-bug knows
	g.UserAct("bob").Comment(ref, "on it")
	g.UserAct("carol").Comment(ref, "me too")

	backend, err := cache.NewRepoCacheNoEvents(repotest.NewGoGitRepo(t, false), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	me, err := backend.Identities().New("Me", "me@example.com")
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(ctx, me))
	sync := func() int {
		e, err := core.NewEngine(context.Background(), backend, g, core.SyncOptions{
			Foreign: true, DefaultLogin: "bot", LoginKey: metaKeyGitlabLogin,
		})
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
	require.Zero(t, sync())
	require.Equal(t, []string{"bug"}, g.Issue(ref.Number).Labels)
	require.Equal(t, []string{"bob"}, g.assignees(ref))

	b, err := backend.Bugs().Resolve(ctx, backend.Bugs().AllIds()[0])
	require.NoError(t, err)
	_, _, err = b.ChangeLabels(ctx, []string{"assignee:carol"}, []string{"assignee:bob"})
	require.NoError(t, err)
	require.NoError(t, b.Commit(ctx))
	require.Zero(t, sync())
	require.Equal(t, []string{"carol"}, g.assignees(ref))

	g.assign("alice", ref, "bob")
	require.Zero(t, sync())
	require.Equal(t, []common.Label{"assignee:bob", "bug"}, b.Snapshot().Labels)

	require.Zero(t, sync())
	writes, ops := g.Writes(), len(b.Snapshot().Operations)
	require.Zero(t, sync())
	require.Equal(t, writes, g.Writes(), "nothing more sent")
	require.Len(t, b.Snapshot().Operations, ops, "nothing more pulled")
}
