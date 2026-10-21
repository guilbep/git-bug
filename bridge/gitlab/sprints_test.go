package gitlab

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	gitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/entity/dag"
	"github.com/git-bug/git-bug/repository/repotest"
)

// setMilestone gives an issue a milestone on GitLab, "" for none, as login.
func (g gitlabForge) setMilestone(login string, ref core.Ref, title string) {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	a := gitlabActor{fake: g.fake}
	g.fake.setMilestone(g.fake.userNamed(login), a.find(ref), title)
}

// held returns an issue's labels and milestone as GitLab holds them.
func (g gitlabForge) held(ref core.Ref) (labels []string, milestone string) {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	issue := gitlabActor{fake: g.fake}.find(ref)
	if issue.Milestone != nil {
		milestone = issue.Milestone.Title
	}
	return slices.Clone(issue.Labels), milestone
}

func (g gitlabForge) addMilestone(title string, group bool) *gitlab.Milestone {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	return g.fake.addMilestone(title, group)
}

func TestGitlabMilestonesAreSprints(t *testing.T) {
	g := newGitlabForge(t)
	g.addMilestone("Sprint 2", false)
	ref := g.UserAct("alice").CreateIssue("title", "body")

	g.setMilestone("alice", ref, "Sprint 2")
	issue := g.Issue(ref.Number)
	require.Equal(t, []string{"sprint:sprint-2"}, issue.Labels)
	ev := issue.Events[len(issue.Events)-1]
	require.Equal(t, core.EventLabels, ev.Kind)
	require.Equal(t, []string{"sprint:sprint-2"}, ev.Added)
	require.True(t, strings.HasPrefix(ev.ID, "milestone-"))

	g.setMilestone("alice", ref, "")
	issue = g.Issue(ref.Number)
	require.Empty(t, issue.Labels)
	ev = issue.Events[len(issue.Events)-1]
	require.Equal(t, []string{"sprint:sprint-2"}, ev.Removed)
}

func TestGitlabMilestoneChangeIsOneEvent(t *testing.T) {
	g := newGitlabForge(t)
	g.addMilestone("A", false)
	g.addMilestone("B", false)
	ref := g.UserAct("alice").CreateIssue("title", "body")

	g.setMilestone("alice", ref, "A")
	g.setMilestone("alice", ref, "B")
	issue := g.Issue(ref.Number)
	require.Len(t, issue.Events, 2)
	require.Equal(t, []string{"sprint:b"}, issue.Events[1].Added)
	require.Equal(t, []string{"sprint:a"}, issue.Events[1].Removed)
	require.Equal(t, []string{"sprint:b"}, issue.Labels)

	// cleared, then set again to the one it was
	g.setMilestone("alice", ref, "")
	g.setMilestone("alice", ref, "B")
	require.Len(t, g.Issue(ref.Number).Events, 4)
}

func TestGitlabSprintLabelsResent(t *testing.T) {
	g := newGitlabForge(t)
	g.addMilestone("Sprint 2", false)
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")

	alice.SetLabels(ref, []string{"sprint:sprint-2", "sprint:sprint-9", "sprint:Sprint 2", "polish"})
	require.Equal(t, []string{"sprint:sprint-2"}, g.Issue(ref.Number).Resend, "no milestone yet; the others are no milestone's")

	g.setMilestone("alice", ref, "Sprint 2")
	issue := g.Issue(ref.Number)
	require.Equal(t, []string{"sprint:sprint-2"}, issue.Resend, "the milestone is that one")
	require.Equal(t, 1, strings.Count(strings.Join(issue.Labels, " "), "sprint:sprint-2"), "one label")

	g.addMilestone("Sprint 3", false)
	g.setMilestone("alice", ref, "Sprint 3")
	require.Empty(t, g.Issue(ref.Number).Resend, "the milestone is another")
}

func TestGitlabSprintsOff(t *testing.T) {
	g := newGitlabForge(t)
	g.addMilestone("Sprint 2", false)
	g.sprints = false
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.setMilestone("alice", ref, "Sprint 2")

	require.Empty(t, g.Issue(ref.Number).Labels)
	_, err := g.Fetch(t.Context(), ref)
	require.NoError(t, err)

	// a sprint is a label
	require.NoError(t, g.ChangeLabels(t.Context(), g.accounts[0], ref, []string{"sprint:sprint-2"}, nil))
	labels, milestone := g.held(ref)
	require.Equal(t, []string{"sprint:sprint-2"}, labels)
	require.Equal(t, "Sprint 2", milestone)
	require.Zero(t, g.fake.milestoneRequests)
}

// A deleted milestone leaves an event without milestone.
func TestGitlabDeletedMilestoneEvent(t *testing.T) {
	g := newGitlabForge(t)
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.fake.mu.Lock()
	issue := gitlabActor{fake: g.fake}.find(ref)
	issue.milestoneEvents = append(issue.milestoneEvents, &gitlab.MilestoneEvent{
		ID: 1, Action: "add", CreatedAt: g.fake.now(), User: &gitlab.BasicUser{ID: 1},
	})
	g.fake.mu.Unlock()

	require.NotPanics(t, func() { g.Issue(ref.Number) })
	require.Empty(t, g.Issue(ref.Number).Labels)
	require.Empty(t, g.Issue(ref.Number).Events)
}

func TestGitlabSameTitleMilestones(t *testing.T) {
	g := newGitlabForge(t)
	group := g.addMilestone("Sprint 2", true)
	project := g.addMilestone("Sprint 2", false)
	require.NoError(t, g.readMilestones(t.Context()))
	id, ok := g.milestoneID("sprint:sprint-2")
	require.True(t, ok)
	require.Equal(t, project.ID, id)
	require.NotEqual(t, group.ID, id)

	// whatever the order they come in
	g.fake.mu.Lock()
	slices.Reverse(g.fake.milestones)
	g.fake.mu.Unlock()
	require.NoError(t, g.readMilestones(t.Context()))
	id, _ = g.milestoneID("sprint:sprint-2")
	require.Equal(t, project.ID, id)
}

func TestGitlabClosedMilestoneIsNotKnown(t *testing.T) {
	g := newGitlabForge(t)
	m := g.addMilestone("Old", false)
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.setMilestone("alice", ref, "Old")
	g.fake.mu.Lock()
	m.State = "closed"
	g.fake.mu.Unlock()

	require.Equal(t, []string{"sprint:old"}, g.Issue(ref.Number).Labels, "an issue keeps its milestone's label")
	_, ok := g.milestoneID("sprint:old")
	require.False(t, ok)
}

func TestGitlabRenamedMilestone(t *testing.T) {
	g := newGitlabForge(t)
	m := g.addMilestone("Sprint 2", false)
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.setMilestone("alice", ref, "Sprint 2")

	g.fake.mu.Lock()
	m.Title = "Sprint 2b"
	g.fake.mu.Unlock()
	require.Equal(t, []string{"sprint:sprint-2b"}, g.Issue(ref.Number).Labels)
}

func TestGitlabDeletedMilestone(t *testing.T) {
	g := newGitlabForge(t)
	m := g.addMilestone("Sprint 2", false)
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.setMilestone("alice", ref, "Sprint 2")

	g.fake.mu.Lock()
	g.fake.deleteMilestone(m)
	g.fake.mu.Unlock()
	issue := g.Issue(ref.Number)
	require.Empty(t, issue.Labels)
	require.Empty(t, issue.Events)
}

func TestGitlabChangeLabelsSetsMilestone(t *testing.T) {
	ctx := t.Context()
	g := newGitlabForge(t)
	g.addMilestone("Sprint 2", false)
	g.addMilestone("Sprint 3", false)
	bot := g.accounts[0]
	ref := g.UserAct("alice").CreateIssue("title", "body")

	require.NoError(t, g.ChangeLabels(ctx, bot, ref, []string{"sprint:sprint-2", "a"}, nil))
	labels, milestone := g.held(ref)
	require.Equal(t, []string{"a"}, labels)
	require.Equal(t, "Sprint 2", milestone)

	require.NoError(t, g.ChangeLabels(ctx, bot, ref, []string{"sprint:sprint-3"}, []string{"sprint:sprint-2"}))
	_, milestone = g.held(ref)
	require.Equal(t, "Sprint 3", milestone)

	require.NoError(t, g.ChangeLabels(ctx, bot, ref, nil, []string{"sprint:sprint-3"}))
	_, milestone = g.held(ref)
	require.Empty(t, milestone)

	require.NoError(t, g.ChangeLabels(ctx, bot, ref, []string{"sprint:sprint-9"}, nil))
	labels, milestone = g.held(ref)
	require.Equal(t, []string{"a", "sprint:sprint-9"}, labels, "no milestone: a label")
	require.Empty(t, milestone)

	require.NoError(t, g.ChangeLabels(ctx, bot, ref, []string{"sprint:sprint-2", "sprint:sprint-3"}, nil))
	labels, milestone = g.held(ref)
	require.Equal(t, []string{"a", "sprint:sprint-3", "sprint:sprint-9"}, labels, "one milestone: a second sprint is a label")
	require.Equal(t, "Sprint 2", milestone)
}

// A sprint's label held as a label moves to the milestone, with no change
// for the engine: the label comes off once the milestone holds it.
func TestGitlabChangeLabelsMovesSprintLabel(t *testing.T) {
	ctx := t.Context()
	g := newGitlabForge(t)
	g.addMilestone("Sprint 2", false)
	m := g.addMilestone("Sprint 3", false)
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	alice.SetLabels(ref, []string{"sprint:sprint-2"})
	events := len(g.Issue(ref.Number).Events)

	require.NoError(t, g.ChangeLabels(ctx, g.accounts[0], ref, []string{"sprint:sprint-2"}, nil))
	labels, milestone := g.held(ref)
	require.Empty(t, labels)
	require.Equal(t, "Sprint 2", milestone)
	require.Len(t, g.Issue(ref.Number).Events, events, "no change for the engine")

	// a milestone GitLab ignores keeps the label
	other := alice.CreateIssue("other", "body")
	alice.SetLabels(other, []string{"sprint:sprint-3"})
	require.NoError(t, g.readMilestones(ctx))
	g.fake.mu.Lock()
	m.State = "closed"
	g.fake.mu.Unlock()
	err := g.ChangeLabels(ctx, g.accounts[0], other, []string{"sprint:sprint-3"}, nil)
	require.Equal(t, core.RefusedLabels{"sprint:sprint-3"}, err)
	labels, milestone = g.held(other)
	require.Equal(t, []string{"sprint:sprint-3"}, labels)
	require.Empty(t, milestone)
}

// A removed sprint clears its milestone, even closed, so not known.
func TestGitlabRemovedSprintClearsClosedMilestone(t *testing.T) {
	ctx := t.Context()
	g := newGitlabForge(t)
	m := g.addMilestone("Sprint 2", false)
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.setMilestone("alice", ref, "Sprint 2")
	g.fake.mu.Lock()
	m.State = "closed"
	g.fake.mu.Unlock()
	require.NoError(t, g.readMilestones(ctx))
	_, ok := g.milestoneID("sprint:sprint-2")
	require.False(t, ok)

	require.NoError(t, g.ChangeLabels(ctx, g.accounts[0], ref, nil, []string{"sprint:sprint-2"}))
	_, milestone := g.held(ref)
	require.Empty(t, milestone)
}

// GitLab silently ignores a milestone it can't assign, like one closed since
// it was read: the sprint is refused, the other labels go through.
func TestGitlabMilestoneWriteIgnored(t *testing.T) {
	ctx := t.Context()
	g := newGitlabForge(t)
	g.addMilestone("Sprint 2", false)
	m := g.addMilestone("Sprint 3", false)
	bot := g.accounts[0]
	ref := g.UserAct("alice").CreateIssue("title", "body")
	require.NoError(t, g.readMilestones(ctx))
	g.fake.mu.Lock()
	m.State = "closed"
	g.fake.mu.Unlock()

	err := g.ChangeLabels(ctx, bot, ref, []string{"sprint:sprint-3", "a"}, nil)
	require.Equal(t, core.RefusedLabels{"sprint:sprint-3"}, err)
	labels, milestone := g.held(ref)
	require.Equal(t, []string{"a"}, labels)
	require.Empty(t, milestone)

	// a replace is a set and a clear: both are refused
	g.setMilestone("alice", ref, "Sprint 2")
	err = g.ChangeLabels(ctx, bot, ref, []string{"sprint:sprint-3", "b"}, []string{"sprint:sprint-2"})
	require.Equal(t, core.RefusedLabels{"sprint:sprint-3", "sprint:sprint-2"}, err)
	labels, milestone = g.held(ref)
	require.Equal(t, []string{"a", "b"}, labels)
	require.Equal(t, "Sprint 2", milestone)
}

// syncing returns a git-bug repository, and a sync of it with a GitLab that
// returns its number of warnings.
func syncing(t *testing.T) (*cache.RepoCache, func(g gitlabForge) int) {
	backend, err := cache.NewRepoCacheNoEvents(repotest.NewGoGitRepo(t, false), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	me, err := backend.Identities().New("Me", "me@example.com")
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(me))
	return backend, func(g gitlabForge) int {
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
}

// changes counts a bug's operations but metadata, which claims of echoes are.
func changes(b *cache.BugCache) (n int) {
	for _, op := range b.Snapshot().Operations {
		if _, ok := op.(*dag.SetMetadataOperation[*bug.Snapshot]); !ok {
			n++
		}
	}
	return n
}

// A sync sets git-bug's sprints as milestones, moves GitLab's sprint labels
// to milestones, and then sends nothing more.
func TestGitlabSprintsSync(t *testing.T) {
	g := newGitlabForge(t)
	g.addMilestone("Sprint 2", false)
	backend, sync := syncing(t)
	b, _, err := backend.Bugs().New("local", "body")
	require.NoError(t, err)
	_, _, err = b.ChangeLabels([]string{"sprint:sprint-2", "a"}, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	require.Zero(t, sync(g))
	ref := g.Issue("1").Ref
	labels, milestone := g.held(ref)
	require.Equal(t, []string{"a"}, labels)
	require.Equal(t, "Sprint 2", milestone)
	ops := changes(b)
	require.Zero(t, sync(g))
	require.Equal(t, ops, changes(b), "echoes claimed")
	require.Equal(t, []common.Label{"a", "sprint:sprint-2"}, b.Snapshot().Labels)

	alice := g.UserAct("alice")
	old := alice.CreateIssue("old way", "body")
	alice.SetLabels(old, []string{"sprint:sprint-2"})
	require.Zero(t, sync(g))
	labels, milestone = g.held(old)
	require.Empty(t, labels)
	require.Equal(t, "Sprint 2", milestone)
	ob, err := backend.Bugs().ResolveBugCreateMetadata("sync:"+g.Key()+":id", old.ID)
	require.NoError(t, err)
	writes, ops := g.Writes(), len(ob.Snapshot().Operations)
	require.Zero(t, sync(g))
	require.Equal(t, writes, g.Writes(), "nothing more sent")
	require.Len(t, ob.Snapshot().Operations, ops, "nothing more pulled")
	require.Equal(t, []common.Label{"sprint:sprint-2"}, ob.Snapshot().Labels)
}

// Ruled: after a native sync, a remote with sprints = off sees no milestone.
// git-bug drops the sprint the milestone holds, and keeps one GitLab holds
// as a label; GitLab keeps the milestone.
func TestGitlabSprintsOffAfterNative(t *testing.T) {
	g := newGitlabForge(t)
	g.addMilestone("Sprint 2", false)
	backend, sync := syncing(t)
	b, _, err := backend.Bugs().New("local", "body")
	require.NoError(t, err)
	_, _, err = b.ChangeLabels([]string{"sprint:sprint-2", "sprint:sprint-9"}, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	require.Zero(t, sync(g))
	ref := g.Issue("1").Ref
	labels, milestone := g.held(ref)
	require.Equal(t, []string{"sprint:sprint-9"}, labels)
	require.Equal(t, "Sprint 2", milestone)

	r, err := newRemote(t.Context(), g.fake.URL+"/", "1", "bot", []*auth.Token{auth.NewToken(target, "token-bot")})
	require.NoError(t, err)
	r.sprints = false
	off := gitlabForge{remote: r, fake: g.fake}
	require.Zero(t, sync(off))
	require.Equal(t, []common.Label{"sprint:sprint-9"}, b.Snapshot().Labels)
	_, milestone = off.held(ref)
	require.Equal(t, "Sprint 2", milestone, "GitLab keeps the milestone")
	writes := off.Writes()
	require.Zero(t, sync(off))
	require.Equal(t, writes, off.Writes(), "nothing more sent")
}
