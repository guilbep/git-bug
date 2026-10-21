package github

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/entity/dag"
)

// setType gives an issue a type on GitHub, "" for none, as login.
func (g githubForge) setType(login string, ref core.Ref, name string) {
	githubActor{fake: g.fake, login: login}.do(func() {
		g.fake.setType(login, githubActor{fake: g.fake}.issue(ref), name)
	})
}

// heldType returns an issue's labels and type as GitHub holds them.
func (g githubForge) heldType(ref core.Ref) (labels []string, typ string) {
	g.fake.mu.Lock()
	defer g.fake.mu.Unlock()
	issue := githubActor{fake: g.fake}.issue(ref)
	return slices.Clone(issue.labels), issue.issueType
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

func TestGithubChangeLabelsSetsType(t *testing.T) {
	ctx := context.Background()
	g := newTypedForge(t)
	bot := g.accounts[0]
	ref := g.UserAct("alice").CreateIssue("title", "body")

	require.NoError(t, g.ChangeLabels(ctx, bot, ref, []string{"bug", "a"}, nil))
	labels, typ := g.heldType(ref)
	require.Equal(t, []string{"a"}, labels)
	require.Equal(t, "Bug", typ)

	require.NoError(t, g.ChangeLabels(ctx, bot, ref, []string{"feat"}, nil))
	labels, typ = g.heldType(ref)
	require.Equal(t, []string{"a", "feat"}, labels, "one type: a second kind is a label")
	require.Equal(t, "Bug", typ)

	require.NoError(t, g.ChangeLabels(ctx, bot, ref, nil, []string{"bug"}))
	_, typ = g.heldType(ref)
	require.Empty(t, typ)

	require.NoError(t, g.ChangeLabels(ctx, bot, ref, []string{"polish"}, nil))
	labels, typ = g.heldType(ref)
	require.Equal(t, []string{"a", "feat", "polish"}, labels, "no Polish type: a label")
	require.Empty(t, typ)
}

// A kind's label held as a label moves to the type, with no change for the
// engine.
func TestGithubChangeLabelsMovesKindLabel(t *testing.T) {
	g := newTypedForge(t)
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.UserAct("alice").SetLabels(ref, []string{"bug"})
	events := len(g.Issue(ref.Number).Events)

	require.NoError(t, g.ChangeLabels(context.Background(), g.accounts[0], ref, []string{"bug"}, nil))
	labels, typ := g.heldType(ref)
	require.Empty(t, labels)
	require.Equal(t, "Bug", typ)
	require.Len(t, g.Issue(ref.Number).Events, events, "no change for the engine")
}

// A user's repository has no types: kinds are labels, with no type read.
func TestGithubChangeLabelsUserRepo(t *testing.T) {
	g := newGithubForge(t, "owner")
	var err error
	g.kindTypes, err = parseKindTypes("bug=Bug")
	require.NoError(t, err)
	ref := g.UserAct("alice").CreateIssue("title", "body")

	require.NoError(t, g.ChangeLabels(context.Background(), g.accounts[0], ref, []string{"bug", "kind:task"}, []string{"kind:feature"}))
	labels, typ := g.heldType(ref)
	require.Equal(t, []string{"bug", "kind:task"}, labels)
	require.Empty(t, typ)
	require.Zero(t, g.fake.typeReads)
	require.Zero(t, g.fake.typeWrites)
}

// With kinds = labels, a type moves to its label, with no change for the
// engine.
func TestGithubChangeLabelsKindsAsLabels(t *testing.T) {
	ctx := context.Background()
	g := newTypedForge(t)
	g.kinds = false
	bot := g.accounts[0]
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.setType("alice", ref, "Bug")
	events := len(g.Issue(ref.Number).Events)

	require.NoError(t, g.ChangeLabels(ctx, bot, ref, []string{"bug"}, nil))
	labels, typ := g.heldType(ref)
	require.Equal(t, []string{"bug"}, labels)
	require.Empty(t, typ, "moved to a label")
	require.Len(t, g.Issue(ref.Number).Events, events, "no change for the engine")

	// a type left would bring its kind back at the next pull
	g.setType("alice", ref, "Feature")
	require.NoError(t, g.ChangeLabels(ctx, bot, ref, nil, []string{"feat"}))
	_, typ = g.heldType(ref)
	require.Empty(t, typ)

	reads := g.fake.typeReads
	require.NoError(t, g.ChangeLabels(ctx, bot, ref, []string{"polish"}, nil))
	require.Equal(t, reads, g.fake.typeReads, "no type's label, no read")
}

// A type GitHub refuses to set or clear is refused; the other labels go
// through.
func TestGithubTypeWriteRefused(t *testing.T) {
	ctx := context.Background()
	g := newTypedForge(t)
	g.fake.refuseTypes = true
	bot := g.accounts[0]
	ref := g.UserAct("alice").CreateIssue("title", "body")

	err := g.ChangeLabels(ctx, bot, ref, []string{"bug", "a"}, nil)
	require.Equal(t, core.RefusedLabels{"bug"}, err)
	labels, typ := g.heldType(ref)
	require.Equal(t, []string{"a"}, labels)
	require.Empty(t, typ)

	err = g.ChangeLabels(ctx, bot, ref, []string{"feat"}, nil)
	require.Equal(t, core.RefusedLabels{"feat"}, err)

	g.setType("alice", ref, "Bug")
	err = g.ChangeLabels(ctx, bot, ref, nil, []string{"bug"})
	require.Equal(t, core.RefusedLabels{"bug"}, err)
	labels, typ = g.heldType(ref)
	require.Equal(t, []string{"a"}, labels)
	require.Equal(t, "Bug", typ)

	// with kinds = labels, the label is added before the type is cleared
	g.kinds = false
	err = g.ChangeLabels(ctx, bot, ref, []string{"bug"}, nil)
	require.Equal(t, core.RefusedLabels{"bug"}, err)
	labels, typ = g.heldType(ref)
	require.Equal(t, []string{"a", "bug"}, labels)
	require.Equal(t, "Bug", typ)
}

// null clears a type: issueTypeId is sent even when nil.
func TestUpdateIssueIssueTypeInputSendsNull(t *testing.T) {
	data, err := json.Marshal(UpdateIssueIssueTypeInput{IssueID: "I_1"})
	require.NoError(t, err)
	require.JSONEq(t, `{"issueId":"I_1","issueTypeId":null}`, string(data))
}

// A sync sets git-bug's kinds as types, moves GitHub's kind labels to types,
// and then has nothing more to do.
func TestGithubKindsSync(t *testing.T) {
	g := newTypedForge(t)
	backend, sync := syncing(t, g)
	b, _, err := backend.Bugs().New("local", "body")
	require.NoError(t, err)
	_, _, err = b.ChangeLabels([]string{"bug", "polish"}, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	require.Zero(t, sync())
	ref := g.Issue("1").Ref
	labels, typ := g.heldType(ref)
	require.Equal(t, []string{"polish"}, labels)
	require.Equal(t, "Bug", typ)
	// changes are the operations but metadata, which claims of echoes are
	changes := func() (n int) {
		for _, op := range b.Snapshot().Operations {
			if _, ok := op.(*dag.SetMetadataOperation[*bug.Snapshot]); !ok {
				n++
			}
		}
		return n
	}
	ops := changes()
	require.Zero(t, sync())
	require.Equal(t, ops, changes(), "echoes claimed")
	require.Equal(t, []common.Label{"bug", "polish"}, b.Snapshot().Labels)

	alice := g.UserAct("alice")
	old := alice.CreateIssue("old way", "body")
	alice.SetLabels(old, []string{"bug"})
	require.Zero(t, sync())
	labels, typ = g.heldType(old)
	require.Empty(t, labels)
	require.Equal(t, "Bug", typ)
	ob, err := backend.Bugs().ResolveBugCreateMetadata("sync:"+g.Key()+":id", old.ID)
	require.NoError(t, err)
	writes, ops := g.Writes(), len(ob.Snapshot().Operations)
	require.Zero(t, sync())
	require.Equal(t, writes, g.Writes(), "nothing more sent")
	require.Len(t, ob.Snapshot().Operations, ops, "nothing more pulled")
	require.Equal(t, []common.Label{"bug"}, ob.Snapshot().Labels)
}

// Switching how GitHub holds kinds moves them, once each way.
func TestGithubKindsSwitch(t *testing.T) {
	g := newTypedForge(t)
	backend, sync := syncing(t, g)
	b, _, err := backend.Bugs().New("local", "body")
	require.NoError(t, err)
	_, _, err = b.ChangeLabels([]string{"bug"}, nil)
	require.NoError(t, err)
	require.NoError(t, b.Commit())
	require.Zero(t, sync())
	ref := g.Issue("1").Ref
	held := func(labels []string, typ string) {
		t.Helper()
		l, ty := g.heldType(ref)
		require.Equal(t, labels, l)
		require.Equal(t, typ, ty)
		require.Equal(t, []common.Label{"bug"}, b.Snapshot().Labels)
	}
	held(nil, "Bug")

	g.kinds = false
	require.Zero(t, sync())
	held([]string{"bug"}, "")

	g.kinds = true
	require.Zero(t, sync())
	held(nil, "Bug")

	writes, ops := g.Writes(), len(b.Snapshot().Operations)
	require.Zero(t, sync())
	require.Equal(t, writes, g.Writes(), "nothing more sent")
	require.Len(t, b.Snapshot().Operations, ops, "nothing more pulled")
}

// Labels match types exactly: kind:Bug is a label, kind:bug the type Bug.
// GitHub's labels ignore case, so each is on its own issue.
func TestGithubKindLabelsMatchExactly(t *testing.T) {
	ctx := context.Background()
	g := newGithubForge(t, "owner")
	g.fake.types = []ghType{{"Bug", true}}
	bot := g.accounts[0]
	alice := g.UserAct("alice")
	label, kind := alice.CreateIssue("label", "body"), alice.CreateIssue("kind", "body")

	require.NoError(t, g.ChangeLabels(ctx, bot, label, []string{"kind:Bug"}, nil))
	labels, typ := g.heldType(label)
	require.Equal(t, []string{"kind:Bug"}, labels)
	require.Empty(t, typ)

	require.NoError(t, g.ChangeLabels(ctx, bot, kind, []string{"kind:bug"}, nil))
	labels, typ = g.heldType(kind)
	require.Empty(t, labels)
	require.Equal(t, "Bug", typ)
}

// Removing the kind of a disabled type clears it: else the next pull would
// bring the kind back.
func TestGithubRemovedKindClearsDisabledType(t *testing.T) {
	g := newTypedForge(t)
	ref := g.UserAct("alice").CreateIssue("title", "body")
	g.setType("alice", ref, "Feature")
	g.fake.types[1].enabled = false
	require.Equal(t, []string{"feat"}, g.Issue(ref.Number).Labels)

	require.NoError(t, g.ChangeLabels(context.Background(), g.accounts[0], ref, nil, []string{"feat"}))
	_, typ := g.heldType(ref)
	require.Empty(t, typ)
	require.NotContains(t, g.Issue(ref.Number).Labels, "feat")
}
