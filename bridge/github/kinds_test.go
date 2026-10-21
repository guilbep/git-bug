package github

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
)

// setType gives an issue a type on GitHub, "" for none, as login.
func (g githubForge) setType(login string, ref core.Ref, name string) {
	githubActor{fake: g.fake, login: login}.do(func() {
		g.fake.setType(login, githubActor{fake: g.fake}.issue(ref), name)
	})
}

// newTypedForge is a GitHub whose owner has the types Bug, Feature and Task,
// with the labels bug and feat for the first two.
func newTypedForge(t *testing.T) githubForge {
	g := newGithubForge(t, "owner")
	g.fake.types = []ghType{{"Bug", true}, {"Feature", true}, {"Task", true}}
	var err error
	g.kindTypes, err = parseKindTypes("bug=Bug,feat=Feature")
	require.NoError(t, err)
	return g
}

func TestGithubTypesAreKinds(t *testing.T) {
	g := newTypedForge(t)
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")

	g.setType("alice", ref, "Bug")
	require.Equal(t, []string{"bug"}, g.Issue(ref.Number).Labels)

	g.setType("alice", ref, "Feature")
	events := g.Issue(ref.Number).Events
	ev := events[len(events)-1]
	require.Equal(t, []string{"feat"}, ev.Added)
	require.Equal(t, []string{"bug"}, ev.Removed)

	g.setType("alice", ref, "Task")
	require.Equal(t, []string{"kind:task"}, g.Issue(ref.Number).Labels)

	g.setType("alice", ref, "")
	require.Empty(t, g.Issue(ref.Number).Labels)
}

func TestGithubTypeLabelsResent(t *testing.T) {
	g := newTypedForge(t)
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")

	alice.SetLabels(ref, []string{"bug", "polish"})
	require.Equal(t, []string{"bug"}, g.Issue(ref.Number).Resend, "no type yet; polish has none")

	g.setType("alice", ref, "Bug")
	issue := g.Issue(ref.Number)
	require.Equal(t, []string{"bug", "polish"}, issue.Labels, "one label")
	require.Equal(t, []string{"bug"}, issue.Resend)

	alice.SetLabels(ref, []string{"feat"})
	require.Empty(t, g.Issue(ref.Number).Resend, "a second kind belongs as a label")
}

func TestGithubKindsAsLabels(t *testing.T) {
	g := newTypedForge(t)
	g.kinds = false
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")

	g.setType("alice", ref, "Bug")
	issue := g.Issue(ref.Number)
	require.Equal(t, []string{"bug"}, issue.Labels)
	require.Equal(t, []string{"bug"}, issue.Resend, "held as a type, the other way")

	alice.SetLabels(ref, []string{"feat"})
	g.setType("alice", ref, "")
	require.Empty(t, g.Issue(ref.Number).Resend, "a label is where labels mode holds it")
}

func TestGithubUserRepoHasNoTypes(t *testing.T) {
	g := newGithubForge(t, "owner")
	alice := g.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	alice.SetLabels(ref, []string{"bug", "kind:feature"})

	issue := g.Issue(ref.Number)
	require.Equal(t, []string{"bug", "kind:feature"}, issue.Labels)
	require.Empty(t, issue.Resend)
}

// A type disabled after it was set is still the issue's kind.
func TestGithubDisabledType(t *testing.T) {
	g := newTypedForge(t)
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.setType("alice", ref, "Feature")
	g.fake.types[1].enabled = false

	issue := g.Issue(ref.Number)
	require.Equal(t, []string{"feat"}, issue.Labels)
	require.Empty(t, issue.Resend)
}

func TestParseKindTypes(t *testing.T) {
	m, err := parseKindTypes(" bug=Bug, kind:feat=Feature ")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"bug": "bug", "feature": "kind:feat"}, m)
	m, err = parseKindTypes("")
	require.NoError(t, err)
	require.Nil(t, m)

	for bad, entry := range map[string]string{
		"bug": "bug", "=Bug": "=Bug", "bug=": "bug=", "a=Bug,b=Bug": "b=Bug", "bug=Bug,bug=Feature": "bug=Feature",
	} {
		_, err := parseKindTypes(bad)
		require.ErrorContains(t, err, entry, bad)
	}
}
