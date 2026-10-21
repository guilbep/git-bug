package gitlab

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	gitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/git-bug/git-bug/bridge/core"
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
