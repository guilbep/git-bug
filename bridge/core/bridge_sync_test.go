package core_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/synctest"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/entity"
)

// forges are the fake forges of the fakesync bridges, by project name.
var forges = map[string]*synctest.Fake{}

// remoteConfs are the configurations the fakesync remotes were made with, by
// project name.
var remoteConfs = map[string]core.Configuration{}

// syncBridge is a bridge ported to the sync engine, on a fake forge.
type syncBridge struct{ plainBridge }

func (syncBridge) Target() string { return "fakesync" }

func (syncBridge) NewRemote(_ context.Context, _ *cache.RepoCache, conf core.Configuration) (core.Remote, error) {
	remoteConfs[conf["project"]] = conf
	return forges[conf["project"]], nil
}

// plainBridge is a bridge with its own importer and exporter.
type plainBridge struct{}

func (plainBridge) Target() string                      { return "fakeplain" }
func (plainBridge) LoginMetaKey() string                { return "fake-login" }
func (plainBridge) NewImporter() core.Importer          { return nil }
func (plainBridge) NewExporter() core.Exporter          { return nil }
func (plainBridge) ValidParams() map[string]interface{} { return nil }
func (plainBridge) ValidateConfig(core.Configuration) error {
	return nil
}
func (plainBridge) Configure(context.Context, *cache.RepoCache, core.BridgeParams, bool) (core.Configuration, error) {
	return nil, nil
}

func init() {
	core.Register(&syncBridge{})
	core.Register(&plainBridge{})
}

func addBridge(t *testing.T, repo *cache.RepoCache, name, target string, conf map[string]string) {
	for k, v := range conf {
		require.NoError(t, core.SetBridgeConfig(repo, name, k, v))
	}
	require.NoError(t, core.SetBridgeConfig(repo, name, core.ConfigKeyTarget, target))
}

func newSyncForge(t *testing.T, repo *cache.RepoCache, name string) *synctest.Fake {
	f := synctest.NewFake(name+".test/1", name+".test", synctest.Quirks{}, bot)
	forges[name] = f
	addBridge(t, repo, name, "fakesync", map[string]string{
		"project":                  name,
		core.ConfigKeyDefaultLogin: "bot",
		core.ConfigKeySync:         "true",
	})
	return f
}

func TestBridgeUsesEngine(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	f := newSyncForge(t, repo, "solo")
	f.UserAct("alice").CreateIssue("from the forge", "body")

	b, err := core.LoadBridge(repo, "solo")
	require.NoError(t, err)
	events, err := b.ImportAllSince(context.Background(), time.Time{})
	require.NoError(t, err)
	for e := range events {
		require.NotEqual(t, core.ImportEventError, e.Event, e.String())
	}
	require.Equal(t, "from the forge", onlyBug(t, repo).Snapshot().Title)

	created, _, err := repo.Bugs().New(ctx, "from git-bug", "body")
	require.NoError(t, err)
	require.NoError(t, created.Commit(ctx))
	exported, err := b.ExportAll(context.Background(), time.Time{})
	require.NoError(t, err)
	for e := range exported {
		require.NotEqual(t, core.ExportEventError, e.Event, e.String())
	}
	require.Equal(t, "from git-bug", f.Issue("2").Title)
}

func TestSyncOnlyForPortedBridges(t *testing.T) {
	repo := newRepo(t)
	addBridge(t, repo, "old", "fakeplain", map[string]string{core.ConfigKeySync: "true"})

	_, err := core.LoadBridge(repo, "old")
	require.ErrorContains(t, err, "sync")
}

func TestSyncAll(t *testing.T) {
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	b := newSyncForge(t, repo, "b")
	a.UserAct("alice").CreateIssue("from a", "body")

	results, err := core.SyncAll(context.Background(), repo, []string{"a", "b"}, false)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}

	require.Equal(t, "from a", b.Issue("1").Title, "pulled from a, pushed to b in one run")
}

// How a forge holds assignees, kinds and sprints comes from the run, as
// git-board gives it from the project's shared configuration, over each
// clone's bridge key: clones holding them differently would undo each
// other's syncs.
func TestSyncModes(t *testing.T) {
	repo := newRepo(t)
	newSyncForge(t, repo, "solo")
	sync := func(key string, opts ...core.Option) (string, error) {
		results, err := core.SyncAll(context.Background(), repo, nil, true, opts...)
		if err != nil {
			return "", err
		}
		for r := range results {
			require.False(t, r.IsError(), r.String())
		}
		return remoteConfs["solo"][key], nil
	}
	for _, c := range []struct{ key, other string }{
		{core.ConfigKeyAssignees, core.ModeLabels},
		{core.ConfigKeyKinds, core.ModeLabels},
		{core.ConfigKeySprints, core.ModeOff},
	} {
		require.NoError(t, core.SetBridgeConfig(repo, "solo", c.key, ""))
		mode, err := sync(c.key)
		require.NoError(t, err)
		require.Empty(t, mode, c.key+": native by default")
		require.NoError(t, core.SetBridgeConfig(repo, "solo", c.key, c.other))
		mode, err = sync(c.key)
		require.NoError(t, err)
		require.Equal(t, c.other, mode, c.key)
		mode, err = sync(c.key, core.WithConfig(c.key, core.ModeNative))
		require.NoError(t, err)
		require.Equal(t, core.ModeNative, mode, c.key+": the run's mode wins")

		require.NoError(t, core.SetBridgeConfig(repo, "solo", c.key, "lables"))
		_, err = sync(c.key)
		require.ErrorContains(t, err, "lables")
		_, err = sync(c.key, core.WithConfig(c.key, "lables"))
		require.ErrorContains(t, err, "lables")
		require.NoError(t, core.SetBridgeConfig(repo, "solo", c.key, ""))
	}

	mode, err := sync(core.ConfigKeyAssignees, core.WithAssignees(core.AssigneesLabels))
	require.NoError(t, err)
	require.Equal(t, core.AssigneesLabels, mode, "WithAssignees still works")

	_, err = sync(core.ConfigKeySprints, core.WithConfig(core.ConfigKeySprints, core.ModeLabels))
	require.ErrorContains(t, err, core.ModeLabels, "sprints have no labels mode")
	_, err = sync(core.ConfigKeyKinds, core.WithConfig(core.ConfigKeyKinds, core.ModeOff))
	require.ErrorContains(t, err, core.ModeOff, "kinds have no off mode")
}

func TestSyncAllDryRun(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	b := newSyncForge(t, repo, "b")
	a.UserAct("alice").CreateIssue("from a", "body")
	created, _, err := repo.Bugs().New(ctx, "from git-bug", "body")
	require.NoError(t, err)
	require.NoError(t, created.Commit(ctx))
	identities := len(repo.Identities().AllIds())

	results, err := core.SyncAll(context.Background(), repo, []string{"a", "b"}, true)
	require.NoError(t, err)
	var report []string
	for r := range results {
		require.False(t, r.IsError(), r.String())
		report = append(report, r.String())
	}

	require.Zero(t, a.Writes()+b.Writes(), "nothing written to forges")
	require.Len(t, repo.Bugs().AllIds(), 1, "nothing imported")
	require.Equal(t, identities, len(repo.Identities().AllIds()), "no identity created")
	all := strings.Join(report, "\n")
	require.Contains(t, all, "would import issue https://a.test/issues/1")
	require.Contains(t, all, "would create issue: from git-bug")
}

// The same person on two forges is one identity once each bridge links their
// login to it.
func TestSyncLinkedLogins(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	b := newSyncForge(t, repo, "b")
	a.UserAct("alice").CreateIssue("from a", "body")
	b.UserAct("alice-b").CreateIssue("from b", "body")
	b.UserAct("bob").CreateIssue("from bob", "body")

	alice, err := repo.Identities().New("Alice", "alice@example.com")
	require.NoError(t, err)
	require.NoError(t, core.SetBridgeConfig(repo, "a", "login-"+alice.Id().Human(), "alice"))
	require.NoError(t, core.SetBridgeConfig(repo, "b", "login-"+alice.Id().Human(), "Alice-B"))

	results, err := core.SyncAll(context.Background(), repo, []string{"a", "b"}, false)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}

	authors := map[string]entity.Id{}
	for _, id := range repo.Bugs().AllIds() {
		b, err := repo.Bugs().Resolve(ctx, id)
		require.NoError(t, err)
		authors[b.Snapshot().Title] = b.Snapshot().Author.Id()
	}
	require.Equal(t, alice.Id(), authors["from a"])
	require.Equal(t, alice.Id(), authors["from b"])
	require.NotEqual(t, alice.Id(), authors["from bob"])
}

func TestSyncLinkErrors(t *testing.T) {
	ctx := t.Context()
	for name, links := range map[string]map[string]string{
		"unknown identity": {"login-0000000": "alice"},
		"login twice":      {"login-" + "%me": "alice", "login-" + "%other": "ALICE"},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newRepo(t)
			newSyncForge(t, repo, "a")
			me, err := repo.GetUserIdentity(ctx)
			require.NoError(t, err)
			other, err := repo.Identities().New("Other", "other@example.com")
			require.NoError(t, err)
			for k, v := range links {
				k = strings.NewReplacer("%me", me.Id().Human(), "%other", other.Id().Human()).Replace(k)
				require.NoError(t, core.SetBridgeConfig(repo, "a", k, v))
			}

			_, err = core.SyncAll(context.Background(), repo, []string{"a"}, false)
			require.ErrorContains(t, err, "login-")
		})
	}
}

// An older bridge may have imported part of a project before sync was
// enabled: the first sync pulls everything, whatever it recorded.
func TestSyncAllIgnoresOldImportTime(t *testing.T) {
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	a.UserAct("alice").CreateIssue("never imported", "body")
	require.NoError(t, core.SetBridgeConfig(repo, "a", "lastImportTime", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)))

	results, err := core.SyncAll(context.Background(), repo, []string{"a"}, false)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}
	require.Equal(t, "never imported", onlyBug(t, repo).Snapshot().Title)
}

// A forge whose pull failed isn't pushed to: what git-bug knows of it may be
// stale, and pushing could undo what changed there since.
func TestSyncAllSkipsPushAfterFailedPull(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	b := newSyncForge(t, repo, "b")
	created, _, err := repo.Bugs().New(ctx, "local", "body")
	require.NoError(t, err)
	require.NoError(t, created.Commit(ctx))
	a.FailNextListing()

	results, err := core.SyncAll(context.Background(), repo, []string{"a", "b"}, false)
	require.NoError(t, err)
	var errs []string
	for r := range results {
		if r.IsError() {
			errs = append(errs, r.String())
		}
	}
	require.Len(t, errs, 2, "the pull, and the push not done")
	require.Contains(t, errs[1], "not pushed")
	require.Zero(t, a.Writes())
	require.Equal(t, "local", b.Issue("1").Title, "b is pushed to")
}

// A pull stopped early, such as with Ctrl-C, records no time: the next one
// would skip every issue it didn't read. Once stopped, events may be dropped.
func TestStoppedPullKeepsResumeTime(t *testing.T) {
	repo := newRepo(t)
	f := newSyncForge(t, repo, "solo")
	f.UserAct("alice").CreateIssue("one", "body")
	b, err := core.LoadBridge(repo, "solo")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events, err := b.ImportAllSince(ctx, time.Time{})
	require.NoError(t, err)
	for range events {
	}

	cfg, err := repo.Config().Read()
	require.NoError(t, err)
	last, err := cfg.Int("git-bug.bridge.solo.lastImportTime", 0)
	require.NoError(t, err)
	require.Zero(t, last)
}

// SyncIssue syncs one bug with every sync bridge: its changes go out,
// its issue's changes come in, and nothing else moves.
func TestSyncIssue(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	b := newSyncForge(t, repo, "b")
	alice := a.UserAct("alice")
	ref := alice.CreateIssue("from a", "body")
	results, err := core.SyncAll(context.Background(), repo, []string{"a", "b"}, false)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}
	bug := onlyBug(t, repo)

	// a local comment, a forge comment on it, and changes elsewhere
	_, _, err = bug.AddComment(ctx, "from git-bug")
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))
	alice.Comment(ref, "from the forge")
	other := alice.CreateIssue("not synced", "body")
	created, _, err := repo.Bugs().New(ctx, "local, not synced", "body")
	require.NoError(t, err)
	require.NoError(t, created.Commit(ctx))

	results, err = core.SyncIssue(context.Background(), repo, bug.Id().Human(), nil, false)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}

	var texts []string
	for _, c := range bug.Snapshot().Comments {
		texts = append(texts, c.Message)
	}
	require.Contains(t, texts, "from the forge", "its issue's comment came in")
	require.Len(t, a.Issue(ref.Number).Comments, 2, "the local comment went out to a")
	require.Len(t, b.Issue("1").Comments, 2, "and to b")
	require.Nil(t, b.Issue("2"), "the other local bug stayed local")
	require.Len(t, repo.Bugs().AllIds(), 2, "the other forge issue wasn't pulled")
	require.NotNil(t, a.Issue(other.Number))
}

func TestSyncIssueDryRun(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	bug, _, err := repo.Bugs().New(ctx, "local", "body")
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))

	results, err := core.SyncIssue(context.Background(), repo, bug.Id().Human(), []string{"a"}, true)
	require.NoError(t, err)
	var report []string
	for r := range results {
		require.False(t, r.IsError(), r.String())
		report = append(report, r.String())
	}
	require.Zero(t, a.Writes())
	all := strings.Join(report, "\n")
	require.Contains(t, all, "not on this forge yet")
	require.Contains(t, all, "would create issue: local")
}

func syncAll(t *testing.T, repo *cache.RepoCache, dryRun bool) []string {
	results, err := core.SyncAll(context.Background(), repo, []string{"a", "b"}, dryRun)
	require.NoError(t, err)
	var report []string
	for r := range results {
		require.False(t, r.IsError(), r.String())
		report = append(report, r.String())
	}
	return report
}

// numberOn returns the number of a bug's issue on a fake forge.
func numberOn(t *testing.T, b *cache.BugCache, f *synctest.Fake) string {
	n, ok := b.Snapshot().GetCreateMetadata("sync:" + f.Key() + ":number")
	require.True(t, ok)
	return n
}

// Titles carry #refs too, like the issue's own number that a workflow
// appends to it: on another forge they say that forge's numbers, and coming
// back they're no change.
func TestSyncTranslatesTitleRefs(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	b := newSyncForge(t, repo, "b")
	b.UserAct("bob").CreateIssue("already on b", "body")
	alice := a.UserAct("alice")
	alice.CreateIssue("first", "body")
	alice.CreateIssue("Wrong example (#2), see #1 and #9", "body")
	syncAll(t, repo, false)

	byTitle := func(title string) *cache.BugCache {
		b, err := repo.Bugs().ResolveMatcher(ctx, func(x *cache.BugExcerpt) bool { return x.Title == title })
		require.NoError(t, err)
		return b
	}
	bug, first := byTitle("Wrong example (#2), see #1 and #9"), byTitle("first")
	n, firstN := numberOn(t, bug, b), numberOn(t, first, b)
	require.NotEqual(t, "2", n, "a numbering that differs from a's")
	require.Equal(t, "Wrong example (#"+n+"), see #"+firstN+" and `#9`", b.Issue(n).Title)
	require.Equal(t, "Wrong example (#2), see #1 and #9", a.Issue("2").Title)
	require.Equal(t, "Wrong example (#2), see #1 and #9", bug.Snapshot().Title)

	// the next syncs claim the echo of the title, and write nothing
	writes := a.Writes() + b.Writes()
	syncAll(t, repo, false)
	ops := len(bug.Snapshot().Operations)
	syncAll(t, repo, false)
	require.Equal(t, writes, a.Writes()+b.Writes())
	require.Len(t, bug.Snapshot().Operations, ops)
	require.NotContains(t, strings.Join(syncAll(t, repo, true), "\n"), "would send")
	require.Equal(t, "Wrong example (#2), see #1 and #9", bug.Snapshot().Title)

	// renamed on b, in b's numbers: a gets it in its own
	b.UserAct("bob").SetTitle(core.Ref{ID: b.Issue(n).ID}, "Right example (#"+n+")")
	syncAll(t, repo, false)
	require.Equal(t, "Right example (#2)", a.Issue("2").Title)
	require.Equal(t, "Right example (#"+n+")", b.Issue(n).Title)
	writes = a.Writes() + b.Writes()
	syncAll(t, repo, false)
	syncAll(t, repo, false)
	require.Equal(t, writes, a.Writes()+b.Writes())
}

func TestSyncIssueUnknownBug(t *testing.T) {
	repo := newRepo(t)
	newSyncForge(t, repo, "a")
	_, err := core.SyncIssue(context.Background(), repo, "deadbeef", nil, false)
	require.Error(t, err)
}

func syncA(t *testing.T, repo *cache.RepoCache) {
	results, err := core.SyncAll(context.Background(), repo, []string{"a"}, false)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}
}

// Link makes a bug and an issue already on the forge one: the next sync
// sends git-bug's title, description and status, and adds each side's
// comments and labels to the other. Nothing is created twice.
func TestLink(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	alice := a.UserAct("alice")
	ref := alice.CreateIssue("forge title", "forge body")
	alice.Comment(ref, "forge comment")
	alice.SetLabels(ref, []string{"forge"})
	alice.SetTitle(ref, "forge title, renamed")
	alice.SetStatus(ref, common.ClosedStatus)

	bug, _, err := repo.Bugs().New(ctx, "local title", "local body")
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))
	_, _, err = bug.AddComment(ctx, "local comment")
	require.NoError(t, err)
	_, _, err = bug.ChangeLabels(ctx, []string{"local"}, nil)
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))

	linked, err := core.Link(context.Background(), repo, bug.Id().Human(), "a", ref.URL)
	require.NoError(t, err)
	require.Equal(t, ref.URL, linked.URL)
	require.Zero(t, a.Writes(), "nothing written to the forge")

	syncA(t, repo)
	require.Nil(t, a.Issue("2"), "not created again")
	require.Len(t, repo.Bugs().AllIds(), 1, "nor imported")
	issue := a.Issue(ref.Number)
	require.Equal(t, "local title", issue.Title)
	require.Equal(t, "local body", core.Unrender(issue.Body))
	require.Equal(t, common.OpenStatus, issue.Status)
	require.ElementsMatch(t, []string{"forge", "local"}, issue.Labels)
	require.Len(t, issue.Comments, 2)
	snap := bug.Snapshot()
	require.Equal(t, "local title", snap.Title)
	require.Equal(t, common.OpenStatus, snap.Status)
	require.ElementsMatch(t, []common.Label{"forge", "local"}, snap.Labels)
	require.Len(t, snap.Comments, 3, "the body, and both comments")

	writes := a.Writes()
	syncA(t, repo)
	syncA(t, repo)
	require.Equal(t, writes, a.Writes())
}

func TestLinkRefuses(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	ref := a.UserAct("alice").CreateIssue("on the forge", "body")
	syncA(t, repo)
	pulled := onlyBug(t, repo)
	bug, _, err := repo.Bugs().New(ctx, "local", "body")
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))

	_, err = core.Link(ctx, repo, bug.Id().Human(), "a", "#"+ref.Number)
	require.ErrorContains(t, err, pulled.Id().Human(), "the issue is another bug's")
	_, err = core.Link(ctx, repo, pulled.Id().Human(), "a", "2")
	require.ErrorContains(t, err, ref.URL, "the bug has an issue there")
	for _, issue := range []string{"https://elsewhere.test/issues/1", "99", "first"} {
		_, err = core.Link(ctx, repo, bug.Id().Human(), "a", issue)
		require.Error(t, err, issue)
	}
	_, err = core.Link(ctx, repo, bug.Id().Human(), "nope", "1")
	require.Error(t, err)
	require.Zero(t, a.Writes())
}

// Unlink stops syncing a bug with a forge, for good: neither is the issue
// created again, nor pulled again, and it can be linked to another bug.
func TestUnlink(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	bug, _, err := repo.Bugs().New(ctx, "local", "body")
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))
	syncA(t, repo)
	ref := a.Issue("1").Ref

	require.NoError(t, core.Unlink(ctx, repo, bug.Id().Human(), "a"))
	_, _, err = bug.AddComment(ctx, "after")
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))
	a.UserAct("alice").Comment(ref, "on the forge, after")
	writes := a.Writes()
	syncA(t, repo)
	results, err := core.SyncIssue(ctx, repo, bug.Id().Human(), nil, false)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}
	require.Equal(t, writes, a.Writes(), "nothing pushed")
	require.Nil(t, a.Issue("2"), "nor created again")
	require.Len(t, bug.Snapshot().Comments, 2, "nothing pulled")
	require.Len(t, repo.Bugs().AllIds(), 1, "nor imported as another bug")

	require.ErrorContains(t, core.Unlink(ctx, repo, bug.Id().Human(), "a"), "not synced")
	_, err = core.Link(ctx, repo, bug.Id().Human(), "a", "1")
	require.ErrorContains(t, err, "unlinked")
	other, _, err := repo.Bugs().New(ctx, "another", "body")
	require.NoError(t, err)
	require.NoError(t, other.Commit(ctx))
	_, err = core.Link(ctx, repo, other.Id().Human(), "a", "1")
	require.NoError(t, err, "the issue is free")
	syncA(t, repo)
	require.Len(t, other.Snapshot().Comments, 2, "it got the forge's comment")
}

func take(t *testing.T, repo *cache.RepoCache, b *cache.BugCache) {
	results, err := core.Take(context.Background(), repo, b.Id().Human(), "a")
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}
}

// Take replaces what git-bug hasn't sent to the forge yet with the forge's
// version. New comments stay, to be sent.
func TestTake(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	alice := a.UserAct("alice")
	ref := alice.CreateIssue("title", "body")
	alice.Comment(ref, "comment")
	alice.SetLabels(ref, []string{"x"})
	syncA(t, repo)
	bug := onlyBug(t, repo)

	// changes not sent yet, and one on both sides
	_, err := bug.SetTitle(ctx, "local title")
	require.NoError(t, err)
	_, _, err = bug.EditCreateComment(ctx, "local body")
	require.NoError(t, err)
	_, err = bug.EditComment(ctx, bug.Snapshot().Comments[1].CombinedId(), "local comment")
	require.NoError(t, err)
	_, err = bug.Close(ctx)
	require.NoError(t, err)
	_, _, err = bug.ChangeLabels(ctx, []string{"y"}, []string{"x"})
	require.NoError(t, err)
	_, _, err = bug.AddComment(ctx, "new in git-bug")
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))
	alice.SetTitle(ref, "forge title")
	alice.Comment(ref, "new on the forge")
	writes := a.Writes()

	take(t, repo, bug)
	require.Equal(t, writes, a.Writes(), "nothing written to the forge")
	snap := bug.Snapshot()
	require.Equal(t, "forge title", snap.Title)
	require.Equal(t, "body", snap.Comments[0].Message)
	require.Equal(t, "comment", snap.Comments[1].Message)
	require.Equal(t, common.OpenStatus, snap.Status)
	require.Equal(t, []common.Label{"x"}, snap.Labels)
	var texts []string
	for _, c := range snap.Comments {
		texts = append(texts, c.Message)
	}
	require.Contains(t, texts, "new in git-bug")
	require.Contains(t, texts, "new on the forge")

	syncA(t, repo)
	require.Equal(t, writes+1, a.Writes(), "only the new comment is sent")
	require.Equal(t, "forge title", a.Issue(ref.Number).Title)
	ops := len(bug.Snapshot().Operations)
	take(t, repo, bug)
	require.Len(t, bug.Snapshot().Operations, ops, "nothing left to take")
}

// After Link, Take keeps the forge's version instead of git-bug's.
func TestLinkThenTake(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	alice := a.UserAct("alice")
	ref := alice.CreateIssue("forge title", "forge body")
	alice.SetLabels(ref, []string{"forge"})
	bug, _, err := repo.Bugs().New(ctx, "local title", "local body")
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))
	_, _, err = bug.ChangeLabels(ctx, []string{"local"}, nil)
	require.NoError(t, err)
	_, err = bug.Close(ctx)
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))

	_, err = core.Link(context.Background(), repo, bug.Id().Human(), "a", ref.Number)
	require.NoError(t, err)
	take(t, repo, bug)
	snap := bug.Snapshot()
	require.Equal(t, "forge title", snap.Title)
	require.Equal(t, "forge body", snap.Comments[0].Message)
	require.Equal(t, common.OpenStatus, snap.Status)
	require.Equal(t, []common.Label{"forge"}, snap.Labels)

	writes := a.Writes()
	syncA(t, repo)
	require.Equal(t, writes, a.Writes(), "the forge has it all")
}

func TestTakeNeedsALink(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	newSyncForge(t, repo, "a")
	bug, _, err := repo.Bugs().New(ctx, "local", "body")
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))
	_, err = core.Take(context.Background(), repo, bug.Id().Human(), "a")
	require.ErrorContains(t, err, "not synced")
}

// A title with #refs is linked in sync when the forge's is the one git-bug
// would send there, its refs translated.
func TestLinkTitleWithRefs(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	b := newSyncForge(t, repo, "b")
	b.UserAct("bob").CreateIssue("already on b", "body")
	a.UserAct("alice").CreateIssue("first", "body")
	syncAll(t, repo, false)
	first, err := repo.Bugs().ResolveMatcher(ctx, func(x *cache.BugExcerpt) bool { return x.Title == "first" })
	require.NoError(t, err)
	n := numberOn(t, first, b)
	require.NotEqual(t, "1", n)

	a.UserAct("alice").CreateIssue("see #1", "body")
	ref := b.UserAct("bob").CreateIssue("see #"+n, "body")
	syncA(t, repo)
	bug, err := repo.Bugs().ResolveMatcher(ctx, func(x *cache.BugExcerpt) bool { return x.Title == "see #1" })
	require.NoError(t, err)

	_, err = core.Link(context.Background(), repo, bug.Id().Human(), "b", ref.Number)
	require.NoError(t, err)
	require.NotContains(t, strings.Join(syncAll(t, repo, true), "\n"), "would send")
	writes := b.Writes()
	syncAll(t, repo, false)
	require.Equal(t, writes, b.Writes())
}

// syncIssue syncs one bug with forge a, and returns the report.
func syncIssue(t *testing.T, repo *cache.RepoCache, b *cache.BugCache, dryRun bool) string {
	results, err := core.SyncIssue(context.Background(), repo, b.Id().Human(), []string{"a"}, dryRun)
	require.NoError(t, err)
	var report []string
	for r := range results {
		require.False(t, r.IsError(), r.String())
		report = append(report, r.String())
	}
	return strings.Join(report, "\n")
}

// A bug another clone synced, or whose push died before recording its
// issue, is already on the forge: synced alone, it finds its issue by its
// git-bug marker, as a full sync does, instead of creating another.
func TestSyncIssueFindsItsIssueByMarker(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	bug, _, err := repo.Bugs().New(ctx, "local", "body")
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))
	issue, err := a.CreateIssue(context.Background(), bot, "local", core.Render("body", bug.Id(), nil, nil, nil))
	require.NoError(t, err)
	a.UserAct("alice").Comment(issue, "from the forge")

	// the forge can't be searched: nothing is created
	a.FailNextListing()
	results, err := core.SyncIssue(context.Background(), repo, bug.Id().Human(), []string{"a"}, false)
	require.NoError(t, err)
	errs := 0
	for r := range results {
		if r.IsError() {
			errs++
		}
	}
	require.Positive(t, errs)
	require.Nil(t, a.Issue("2"))

	report := syncIssue(t, repo, bug, true)
	require.Contains(t, report, "would sync with "+issue.URL)
	require.NotContains(t, report, "would create")

	syncIssue(t, repo, bug, false)
	require.Nil(t, a.Issue("2"), "no second issue")
	require.Equal(t, issue.Number, numberOn(t, bug, a))
	require.Len(t, bug.Snapshot().Comments, 2, "its issue was pulled")
}

// Only git-bug's accounts post markers: anyone else's is just text, and
// syncing one bug imports no other.
func TestSyncIssueIgnoresStrangersMarker(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	bug, _, err := repo.Bugs().New(ctx, "local", "body")
	require.NoError(t, err)
	require.NoError(t, bug.Commit(ctx))
	a.UserAct("alice").CreateIssue("copied", core.Render("body", bug.Id(), nil, nil, nil))

	syncIssue(t, repo, bug, false)
	require.Equal(t, "local", a.Issue("2").Title)
	require.Len(t, repo.Bugs().AllIds(), 1)
}

// git-board gives the pull requests of each bug to SyncAll, SyncIssue, and
// a bridge's own pulls, which then bring their comments in.
func TestSyncWithPullsOf(t *testing.T) {
	repo := newRepo(t)
	a := newSyncForge(t, repo, "a")
	ref := a.UserAct("alice").CreateIssue("from a", "body")
	a.OpenPull("7", "c1", "main.go")
	a.PullAct("carol", "7", core.PullComment{Body: "one"})
	var asked []entity.Id
	pullsOf := core.WithPullsOf(func(bug entity.Id) []string {
		asked = append(asked, bug)
		return []string{"7"}
	})
	texts := func() (res []string) {
		for _, c := range onlyBug(t, repo).Snapshot().Comments {
			res = append(res, c.Message)
		}
		return res
	}

	results, err := core.SyncAll(context.Background(), repo, []string{"a"}, false, pullsOf)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}
	require.Equal(t, []string{"body", "one"}, texts())
	require.Equal(t, []entity.Id{onlyBug(t, repo).Id()}, asked)

	a.PullAct("carol", "7", core.PullComment{Body: "two"})
	results, err = core.SyncIssue(context.Background(), repo, onlyBug(t, repo).Id().Human(), nil, false, pullsOf)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}
	require.Equal(t, []string{"body", "one", "two"}, texts())

	a.PullAct("carol", "7", core.PullComment{Body: "three"})
	br, err := core.LoadBridge(repo, "a")
	require.NoError(t, err)
	br.Use(pullsOf)
	events, err := br.ImportAllSince(context.Background(), time.Time{})
	require.NoError(t, err)
	for e := range events {
		require.NotEqual(t, core.ImportEventError, e.Event, e.String())
	}
	require.Equal(t, []string{"body", "one", "two", "three"}, texts())
	require.Empty(t, a.Issue(ref.Number).Comments, "nothing sent to the issue")

	// without, as the command line runs it: none
	a.PullAct("carol", "7", core.PullComment{Body: "four"})
	results, err = core.SyncAll(context.Background(), repo, []string{"a"}, false)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}
	require.Len(t, texts(), 4)
}

// A sync to a public project refuses to run while another sync bridge isn't
// public, as what was pulled from there would be published, until the public
// bridge allows it.
func TestSyncRefusesToPublishPrivate(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	pub := newSyncForge(t, repo, "pub")
	pub.SetPublic(true)
	priv := newSyncForge(t, repo, "priv")
	priv.UserAct("alice").CreateIssue("private", "body")

	_, err := core.SyncAll(ctx, repo, nil, false)
	require.ErrorContains(t, err, "git-bug.bridge.pub."+core.ConfigKeyPublishPrivate)
	_, err = core.SyncAll(ctx, repo, []string{"pub"}, true)
	require.ErrorContains(t, err, core.ConfigKeyPublishPrivate, "the other bridge counts, run or not")
	b, err := core.LoadBridge(repo, "pub")
	require.NoError(t, err)
	_, err = b.ExportAll(ctx, time.Time{})
	require.ErrorContains(t, err, core.ConfigKeyPublishPrivate)

	// syncing the private one alone publishes nothing
	results, err := core.SyncAll(ctx, repo, []string{"priv"}, false)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}
	require.Zero(t, pub.Writes())

	require.NoError(t, core.SetBridgeConfig(repo, "pub", core.ConfigKeyPublishPrivate, "true"))
	results, err = core.SyncAll(ctx, repo, nil, false)
	require.NoError(t, err)
	for r := range results {
		require.False(t, r.IsError(), r.String())
	}
	require.Equal(t, "private", pub.Issue("1").Title)
}
