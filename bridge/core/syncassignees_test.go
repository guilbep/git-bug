package core_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/synctest"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/entity"
)

// people syncs a git-bug repository with forges on their own servers, each
// with its bridge's links, as bridge sync does.
type people struct {
	t     *testing.T
	repo  *cache.RepoCache
	fakes []*synctest.Fake
	links map[*synctest.Fake]map[string]string
	mode  string
}

func newPeople(t *testing.T, fakes ...*synctest.Fake) *people {
	return &people{t: t, repo: newRepo(t), fakes: fakes, links: map[*synctest.Fake]map[string]string{}}
}

// githubLike is a forge that reports assignments as events, like GitHub.
func githubLike(q synctest.Quirks) *synctest.Fake {
	return synctest.NewFake("gh.test/1", "gh.test", q, bot)
}

// gitlabLike is a forge without assignment events and one assignee per
// issue, like GitLab's free edition.
func gitlabLike(q synctest.Quirks) *synctest.Fake {
	q.NoAssigneeEvents, q.MaxAssignees = true, 1
	return synctest.NewFake("gl.test/1", "gl.test", q, bot)
}

func (p *people) engine(f *synctest.Fake, dryRun bool) *core.Engine {
	e, err := core.NewEngine(context.Background(), p.repo, f, core.SyncOptions{
		Foreign: true, DefaultLogin: "bot", LoginKey: "fake-login", DryRun: dryRun,
		Links: p.links[f], Assignees: p.mode,
	})
	require.NoError(p.t, err)
	return e
}

// sync pulls every forge, then pushes to each, and returns the warnings.
func (p *people) sync() int {
	n := 0
	for _, f := range p.fakes {
		for r := range p.engine(f, false).Pull(context.Background(), time.Time{}) {
			require.NotEqual(p.t, core.ImportEventError, r.Event, r.String())
			if r.Event == core.ImportEventWarning {
				n++
			}
		}
	}
	for _, f := range p.fakes {
		for r := range p.engine(f, false).Push(context.Background()) {
			require.NotEqual(p.t, core.ExportEventError, r.Event, r.String())
			if r.Event == core.ExportEventWarning {
				n++
			}
		}
	}
	return n
}

// steady syncs, then checks that syncing again writes, warns and records
// nothing.
func (p *people) steady() {
	p.t.Helper()
	p.sync()
	var writes []int
	for _, f := range p.fakes {
		writes = append(writes, f.Writes())
	}
	ops := p.ops()
	require.Zero(p.t, p.sync(), "warnings")
	for i, f := range p.fakes {
		require.Equal(p.t, writes[i], f.Writes(), "nothing more sent to %s", f.Key())
	}
	require.Equal(p.t, ops, p.ops(), "nothing more recorded")
}

func (p *people) ops() int {
	n := 0
	for _, id := range p.repo.Bugs().AllIds() {
		b, err := p.repo.Bugs().Resolve(id)
		require.NoError(p.t, err)
		n += len(b.Snapshot().Operations)
	}
	return n
}

// planned returns what a dry run of a sync would do.
func (p *people) planned() string {
	var res []string
	for _, f := range p.fakes {
		for r := range p.engine(f, true).Pull(context.Background(), time.Time{}) {
			res = append(res, r.String())
		}
	}
	for _, f := range p.fakes {
		for r := range p.engine(f, true).Push(context.Background()) {
			res = append(res, r.String())
		}
	}
	return strings.Join(res, "\n")
}

// link links a forge login to an identity, in the forge's bridge.
func (p *people) link(f *synctest.Fake, login string, id entity.Id) {
	if p.links[f] == nil {
		p.links[f] = map[string]string{}
	}
	p.links[f][login] = id.String()
}

// person returns the one identity whose login is login.
func (p *people) person(login string) *cache.IdentityCache {
	var found *cache.IdentityCache
	for _, id := range p.repo.Identities().AllIds() {
		i, err := p.repo.Identities().Resolve(id)
		require.NoError(p.t, err)
		if i.Login() == login {
			require.Nil(p.t, found, "one identity is %s", login)
			found = i
		}
	}
	require.NotNil(p.t, found, "an identity is %s", login)
	return found
}

// bug returns the bug of a title.
func (p *people) bug(title string) *cache.BugCache {
	b, err := p.repo.Bugs().ResolveMatcher(func(e *cache.BugExcerpt) bool { return e.Title == title })
	require.NoError(p.t, err)
	return b
}

func (p *people) labels(title string) []common.Label {
	return p.bug(title).Snapshot().Labels
}

// labelOps counts a bug's label changes.
func (p *people) labelOps(title string) int {
	return countOps[*bug.LabelChangeOperation](p.bug(title).Snapshot())
}

func (p *people) changeLabels(title string, added, removed []string) {
	b := p.bug(title)
	_, _, err := b.ChangeLabels(added, removed)
	require.NoError(p.t, err)
	require.NoError(p.t, b.Commit())
}

// onForge returns a forge's issue of a title.
func onForge(t *testing.T, f *synctest.Fake, title string) *core.Issue {
	for issue, err := range f.Issues(context.Background(), time.Time{}) {
		require.NoError(t, err)
		if issue.Title == title {
			return issue
		}
	}
	require.Failf(t, "no issue", "%s has no issue %q", f.Key(), title)
	return nil
}

func logins(users []core.User) []string {
	res := []string{}
	for _, u := range users {
		res = append(res, u.Login)
	}
	return res
}

// A forge's assignee is a person in git-bug: an assignee:<login> label of
// an identity, made for them as for authors.
func TestSyncAssigneeIsPerson(t *testing.T) {
	gh := githubLike(synctest.Quirks{})
	p := newPeople(t, gh)
	ref := gh.Assignee("alice").CreateIssue("title", "body")
	gh.Assignee("alice").SetAssignees(ref, []string{"carol"})

	identities := len(p.repo.Identities().AllIds())
	require.NotContains(t, p.planned(), "would send again")
	require.Len(t, p.repo.Identities().AllIds(), identities, "a dry run creates no identity")
	require.Zero(t, p.sync())
	require.Equal(t, []common.Label{"assignee:carol"}, p.labels("title"))
	p.person("carol")
	p.steady()
	require.Equal(t, []string{"carol"}, logins(gh.Issue(ref.Number).Assignees))
	require.Empty(t, gh.Issue(ref.Number).Labels)
}

// One person, with a login on each forge linked to them, is assigned as
// each forge's account, under one label. Linking someone refused before
// sends them.
func TestSyncAssigneeIsEachForgesAccount(t *testing.T) {
	gh, gl := githubLike(synctest.Quirks{}), gitlabLike(synctest.Quirks{})
	p := newPeople(t, gh, gl)
	ref := gh.Assignee("alice").CreateIssue("title", "body")
	gh.Assignee("alice").SetAssignees(ref, []string{"pg"})
	require.Equal(t, 1, p.sync(), "pg is no one on gl")
	require.Empty(t, onForge(t, gl, "title").Assignees)

	gl.Assignee("pierre")
	p.link(gl, "pierre", p.person("pg").Id())
	require.Zero(t, p.sync())
	require.Equal(t, []string{"pierre"}, logins(onForge(t, gl, "title").Assignees))
	require.Empty(t, onForge(t, gl, "title").Labels)
	p.steady()

	glIssue := onForge(t, gl, "title").Ref
	gl.Assignee("carol").SetAssignees(glIssue, nil)
	require.Zero(t, p.sync())
	require.Empty(t, p.labels("title"))
	require.Empty(t, gh.Issue(ref.Number).Assignees)
	p.steady()

	gl.Assignee("carol").SetAssignees(glIssue, []string{"pierre"})
	require.Zero(t, p.sync())
	require.Equal(t, []common.Label{"assignee:pg"}, p.labels("title"))
	require.Equal(t, []string{"pg"}, logins(gh.Issue(ref.Number).Assignees))
	p.steady()
}

// The user's own identity, made by hand without a login, is named by the
// login `bridge new` tags it with, whichever forge assigns them.
func TestSyncAssigneeNamedByLoginTag(t *testing.T) {
	gh, gl := githubLike(synctest.Quirks{}), gitlabLike(synctest.Quirks{})
	p := newPeople(t, gh, gl)
	me, err := p.repo.Identities().NewRaw("Pierre", "pierre@example.com", "", "", nil, map[string]string{"github-login": "pg"})
	require.NoError(t, err)
	gh.Assignee("pg")
	p.link(gh, "pg", me.Id())
	p.link(gl, "pierre", me.Id())

	ref := gl.Assignee("alice").CreateIssue("title", "body")
	gl.Assignee("alice").SetAssignees(ref, []string{"pierre"})
	require.Zero(t, p.sync())
	require.Equal(t, []common.Label{"assignee:pg"}, p.labels("title"))
	require.Equal(t, []string{"pg"}, logins(onForge(t, gh, "title").Assignees))
	p.steady()
}

// Someone with no account on a forge is refused there, with one warning:
// the label stays in git-bug, and the forge's issue is left as it is.
func TestSyncRefusesPersonUnknownOnForge(t *testing.T) {
	gh, gl := githubLike(synctest.Quirks{}), gitlabLike(synctest.Quirks{})
	p := newPeople(t, gh, gl)
	ref := gh.Assignee("alice").CreateIssue("title", "body")
	gh.Assignee("alice").SetAssignees(ref, []string{"carol"})

	require.Equal(t, 1, p.sync())
	p.steady()
	require.Equal(t, []common.Label{"assignee:carol"}, p.labels("title"))
	require.Empty(t, onForge(t, gl, "title").Assignees)
	require.Empty(t, onForge(t, gl, "title").Labels)
}

// A label naming no identity, like one git-board wrote for someone never
// pulled, is refused on every forge: never sent as a login.
func TestSyncRefusesLabelOfNoOne(t *testing.T) {
	gh, gl := githubLike(synctest.Quirks{}), gitlabLike(synctest.Quirks{})
	p := newPeople(t, gh, gl)
	ref := gh.Assignee("alice").CreateIssue("title", "body")
	gh.Assignee("zed")
	require.Zero(t, p.sync())

	p.changeLabels("title", []string{"assignee:zed"}, nil)
	require.Equal(t, 2, p.sync())
	p.steady()
	require.Equal(t, []common.Label{"assignee:zed"}, p.labels("title"))
	require.Empty(t, gh.Issue(ref.Number).Assignees)
	require.Empty(t, onForge(t, gl, "title").Assignees)
}

// Two people with one login, never linked, are no one: the label isn't
// sent where it could be the other.
func TestSyncRefusesSharedName(t *testing.T) {
	gh, gl := githubLike(synctest.Quirks{}), gitlabLike(synctest.Quirks{})
	p := newPeople(t, gh, gl)
	gl.Assignee("alice").CreateIssue("hers", "on gl")
	ref := gh.Assignee("bob").CreateIssue("title", "body")
	gh.Assignee("bob").SetAssignees(ref, []string{"alice"})

	require.Equal(t, 1, p.sync())
	p.steady()
	require.Equal(t, []common.Label{"assignee:alice"}, p.labels("title"))
	require.Empty(t, onForge(t, gl, "title").Assignees)
	require.Equal(t, []string{"alice"}, logins(gh.Issue(ref.Number).Assignees))
}

// An assignee:<login> label held the old way, as a label, moves to the
// forge's assignees; on a forge without assignment events too, where the
// echo of its removal isn't an unassignment.
func TestSyncMovesOldWayLabel(t *testing.T) {
	for name, newFake := range map[string]func(synctest.Quirks) *synctest.Fake{"github": githubLike, "gitlab": gitlabLike} {
		t.Run(name, func(t *testing.T) {
			f := newFake(synctest.Quirks{})
			p := newPeople(t, f)
			carol := f.Assignee("carol")
			ref := carol.CreateIssue("title", "body")
			carol.SetLabels(ref, []string{"a", "assignee:carol"})

			require.Contains(t, p.planned(), "would send again: labels +assignee:carol")
			require.Zero(t, p.sync())
			require.Equal(t, []string{"a"}, f.Issue(ref.Number).Labels)
			require.Equal(t, []string{"carol"}, logins(f.Issue(ref.Number).Assignees))
			p.steady()
			require.Equal(t, []common.Label{"a", "assignee:carol"}, p.labels("title"))
			require.Equal(t, 1, p.labelOps("title"), "the move's echoes change nothing")
		})
	}
}

// One put on the forge the old way after the issue synced moves too.
func TestSyncMovesLaterOldWayLabel(t *testing.T) {
	f := githubLike(synctest.Quirks{})
	p := newPeople(t, f)
	carol := f.Assignee("carol")
	ref := carol.CreateIssue("title", "body")
	require.Zero(t, p.sync())

	carol.SetLabels(ref, []string{"assignee:carol"})
	require.Zero(t, p.sync())
	p.steady()
	require.Empty(t, f.Issue(ref.Number).Labels)
	require.Equal(t, []string{"carol"}, logins(f.Issue(ref.Number).Assignees))
	require.Equal(t, []common.Label{"assignee:carol"}, p.labels("title"))
}

// When the forge can't assign it, the old way's label stays as it is on
// both sides, with one warning.
func TestSyncRefusedOldWayLabelStays(t *testing.T) {
	f := githubLike(synctest.Quirks{Unassignable: []string{"eve"}})
	p := newPeople(t, f)
	eve := f.Assignee("eve")
	ref := eve.CreateIssue("title", "body")
	eve.SetLabels(ref, []string{"assignee:eve"})

	require.Equal(t, 1, p.sync())
	p.steady()
	require.Equal(t, []common.Label{"assignee:eve"}, p.labels("title"))
	require.Equal(t, []string{"assignee:eve"}, f.Issue(ref.Number).Labels)
	require.NotContains(t, p.planned(), "would")
}

// A dry run tells which labels a sync would send again, of an issue to
// import too, and creates no identity.
func TestDryRunNamesOldWayLabels(t *testing.T) {
	f := githubLike(synctest.Quirks{})
	p := newPeople(t, f)
	carol := f.Assignee("carol")
	ref := carol.CreateIssue("title", "body")
	carol.SetLabels(ref, []string{"assignee:carol"})

	identities := len(p.repo.Identities().AllIds())
	require.Contains(t, p.planned(), "would send again: labels +assignee:carol")
	require.Len(t, p.repo.Identities().AllIds(), identities)
	p.engine(f, false)
	for range p.engine(f, false).Pull(context.Background(), time.Time{}) {
	}
	require.Contains(t, p.planned(), "would send again: labels +assignee:carol")
	f.Assignee("dan").Comment(ref, "hi")
	carol.SetLabels(ref, []string{"assignee:carol", "assignee:dan"})
	require.Contains(t, p.planned(), "would send again: labels +assignee:carol +assignee:dan", "one still to pull too")
	require.Equal(t, []string{"assignee:carol", "assignee:dan"}, f.Issue(ref.Number).Labels, "nothing sent")
}

// Removed in git-bug before it moved, an old way's label comes off the
// forge.
func TestSyncRemovesOldWayLabel(t *testing.T) {
	f := githubLike(synctest.Quirks{})
	p := newPeople(t, f)
	carol := f.Assignee("carol")
	ref := carol.CreateIssue("title", "body")
	carol.SetLabels(ref, []string{"a", "assignee:carol"})
	for range p.engine(f, false).Pull(context.Background(), time.Time{}) {
	}

	p.changeLabels("title", nil, []string{"assignee:carol"})
	require.NotContains(t, p.planned(), "would send again")
	require.Zero(t, p.sync())
	p.steady()
	require.Equal(t, []common.Label{"a"}, p.labels("title"))
	require.Equal(t, []string{"a"}, f.Issue(ref.Number).Labels)
	require.Empty(t, f.Issue(ref.Number).Assignees)
}

// So does one the forge refused to assign.
func TestSyncRemovesRefusedOldWayLabel(t *testing.T) {
	f := githubLike(synctest.Quirks{Unassignable: []string{"eve"}})
	p := newPeople(t, f)
	eve := f.Assignee("eve")
	ref := eve.CreateIssue("title", "body")
	eve.SetLabels(ref, []string{"assignee:eve"})
	require.Equal(t, 1, p.sync())

	p.changeLabels("title", nil, []string{"assignee:eve"})
	require.NotContains(t, p.planned(), "would send again")
	p.sync()
	p.steady()
	require.Empty(t, p.labels("title"))
	require.Empty(t, f.Issue(ref.Number).Labels)
}

// Someone the forge silently leaves out of assignees, like GitHub someone
// without access, keeps their label in git-bug with one warning; the
// other labels go through.
func TestSyncUnassignableStays(t *testing.T) {
	f := githubLike(synctest.Quirks{Unassignable: []string{"eve"}})
	p := newPeople(t, f)
	ref := f.Assignee("eve").CreateIssue("title", "body")
	require.Zero(t, p.sync())

	p.changeLabels("title", []string{"a", "assignee:eve"}, nil)
	require.Equal(t, 1, p.sync())
	p.steady()
	require.Equal(t, []common.Label{"a", "assignee:eve"}, p.labels("title"))
	require.Equal(t, []string{"a"}, f.Issue(ref.Number).Labels)
	require.Empty(t, f.Issue(ref.Number).Assignees)
	require.NotContains(t, p.planned(), "would", "as the push, a dry run skips the refused label")
}

// Unassigning someone the forge can't unassign, like a GitHub bot, warns
// once, and the next pull doesn't assign them again in git-bug.
func TestSyncUnassignRefused(t *testing.T) {
	f := githubLike(synctest.Quirks{Unassignable: []string{"copilot"}})
	p := newPeople(t, f)
	alice := f.Assignee("alice")
	ref := alice.CreateIssue("title", "body")
	alice.SetAssignees(ref, []string{"copilot"})
	require.Zero(t, p.sync())
	require.Equal(t, []common.Label{"assignee:copilot"}, p.labels("title"))

	p.changeLabels("title", nil, []string{"assignee:copilot"})
	require.Equal(t, 1, p.sync())
	p.steady()
	require.Empty(t, p.labels("title"))
	require.Equal(t, []string{"copilot"}, logins(f.Issue(ref.Number).Assignees))
}

// GitLab's free edition keeps one assignee: the others are refused, with
// one warning, and stay in git-bug.
func TestSyncOneAssigneeOnFreeEdition(t *testing.T) {
	f := gitlabLike(synctest.Quirks{})
	p := newPeople(t, f)
	ref := f.Assignee("carol").CreateIssue("title", "body")
	f.Assignee("dan").Comment(ref, "hi")
	require.Zero(t, p.sync())

	p.changeLabels("title", []string{"assignee:carol", "assignee:dan"}, nil)
	require.Equal(t, 1, p.sync())
	p.steady()
	require.Equal(t, []string{"carol"}, logins(f.Issue(ref.Number).Assignees))
	require.Equal(t, []common.Label{"assignee:carol", "assignee:dan"}, p.labels("title"))
}

// Held as labels, assignees are labels on every forge, for anyone, not
// translated; the forges' own assignees move to labels.
func TestSyncAssigneesAsLabels(t *testing.T) {
	gh, gl := githubLike(synctest.Quirks{}), gitlabLike(synctest.Quirks{})
	p := newPeople(t, gh, gl)
	p.mode = core.AssigneesLabels
	carol := gh.Assignee("carol")
	ref := carol.CreateIssue("title", "body")
	carol.SetAssignees(ref, []string{"carol"})

	require.Contains(t, p.planned(), "would send again: labels +assignee:carol")
	require.Zero(t, p.sync())
	require.Equal(t, []string{"assignee:carol"}, gh.Issue(ref.Number).Labels)
	require.Empty(t, gh.Issue(ref.Number).Assignees)
	require.Equal(t, []string{"assignee:carol"}, onForge(t, gl, "title").Labels)
	p.steady()
	require.Equal(t, 1, p.labelOps("title"), "the move's echoes change nothing")

	p.changeLabels("title", []string{"assignee:zed"}, nil)
	require.Zero(t, p.sync())
	p.steady()
	require.Equal(t, []string{"assignee:carol", "assignee:zed"}, gh.Issue(ref.Number).Labels)
	require.Equal(t, []string{"assignee:carol", "assignee:zed"}, onForge(t, gl, "title").Labels)
	require.Equal(t, []common.Label{"assignee:carol", "assignee:zed"}, p.labels("title"))
}

// A dry run names who a forge would refuse, so they can be linked first.
func TestDryRunNamesRefusedAssignees(t *testing.T) {
	gh, gl := githubLike(synctest.Quirks{}), gitlabLike(synctest.Quirks{})
	p := newPeople(t, gh, gl)
	gh.Assignee("alice").CreateIssue("title", "body")
	gh.Assignee("carol").CreateIssue("other", "body")
	require.Zero(t, p.sync())

	p.changeLabels("title", []string{"assignee:carol", "assignee:zed"}, nil)
	plan := p.planned()
	require.Contains(t, plan, "would refuse: assignee:carol, no one on gl.test")
	require.Equal(t, 2, strings.Count(plan, "would refuse: assignee:zed, no one on"), plan)
	require.Equal(t, 2, p.sync())
	require.NotContains(t, p.planned(), "would refuse")
	require.True(t, slices.Contains(p.labels("title"), "assignee:zed"))
}

// One rule both ways: an assignee's label has a name, and its prefix in any
// case, kept as spelled. Without a name, it is a label.
func TestSyncAssigneeLabelSpellings(t *testing.T) {
	f := githubLike(synctest.Quirks{})
	p := newPeople(t, f)
	ref := f.Assignee("carol").CreateIssue("title", "body")
	require.Zero(t, p.sync())

	p.changeLabels("title", []string{"ASSIGNEE:carol", "assignee:"}, nil)
	require.Zero(t, p.sync())
	require.Equal(t, []string{"carol"}, logins(f.Issue(ref.Number).Assignees))
	require.Equal(t, []string{"assignee:"}, f.Issue(ref.Number).Labels)
}
