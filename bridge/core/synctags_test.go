package core

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/entity"
	"github.com/git-bug/git-bug/repository/repotest"
)

// legacyRemote is a Remote whose pre-sync metadata key is "old-id".
type legacyRemote struct {
	Remote
}

func (legacyRemote) Key() string { return "forge.test/1" }

func (legacyRemote) Legacy(meta, _ map[string]string) (string, bool) {
	id, ok := meta["old-id"]
	return id, ok
}

func newTestBug(t *testing.T) (*cache.RepoCache, *cache.IdentityCache, *cache.BugCache) {
	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })

	author, err := backend.Identities().New("Alice", "alice@example.com")
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(author))

	b, _, err := backend.Bugs().NewRaw(author, 1000, "title", "body", nil, nil)
	require.NoError(t, err)
	return backend, author, b
}

func TestFingerprint(t *testing.T) {
	require.Equal(t, Fingerprint("a"), Fingerprint("a\r\n"))
	require.Equal(t, Fingerprint("a\nb"), Fingerprint("a\r\nb  "))
	require.NotEqual(t, Fingerprint("a"), Fingerprint("b"))
	require.Len(t, Fingerprint(""), 64)
}

func TestLabelsValue(t *testing.T) {
	require.Equal(t, "a\nb", labelsValue([]string{"b", "a"}))
	require.Equal(t, "", labelsValue(nil))
}

func TestBaseUntagged(t *testing.T) {
	_, _, b := newTestBug(t)
	r := legacyRemote{}
	snap := b.Snapshot()
	for _, s := range []slot{
		{kind: slotTitle}, {kind: slotStatus}, {kind: slotLabels},
		{kind: slotText, target: snap.Operations[0].Id()},
	} {
		_, _, ok := base(snap, r, s)
		require.False(t, ok, s.kind)
	}
	_, ok := remoteIssueID(snap, r)
	require.False(t, ok)
}

func TestBaseTitleFirstTagged(t *testing.T) {
	_, author, b := newTestBug(t)
	r := legacyRemote{}

	t1, err := b.SetTitleRaw(author, 1001, "first", map[string]string{syncKey(r.Key(), "title"): "first"})
	require.NoError(t, err)
	_, err = b.SetTitleRaw(author, 1002, "second", nil)
	require.NoError(t, err)

	value, op, ok := base(b.Snapshot(), r, slot{kind: slotTitle})
	require.True(t, ok)
	require.Equal(t, "first", value)
	require.Equal(t, t1.Id(), op.Id())
}

func TestBaseTextTaggedLater(t *testing.T) {
	_, author, b := newTestBug(t)
	r := legacyRemote{}

	commentId, comment, err := b.AddCommentRaw(author, 1001, "comment", nil, nil)
	require.NoError(t, err)
	edit1, err := b.EditCommentRaw(author, 1002, commentId, "edit 1", nil)
	require.NoError(t, err)
	_, err = b.EditCommentRaw(author, 1003, commentId, "edit 2", nil)
	require.NoError(t, err)

	// tagged after the fact, as a push does
	_, err = b.SetMetadata(edit1.Id(), map[string]string{syncKey(r.Key(), "text"): Fingerprint("pushed edit 1")})
	require.NoError(t, err)

	value, op, ok := base(b.Snapshot(), r, slot{kind: slotText, target: comment.Id()})
	require.True(t, ok)
	require.Equal(t, Fingerprint("pushed edit 1"), value)
	require.Equal(t, edit1.Id(), op.Id())

	// the body is a different slot
	_, _, ok = base(b.Snapshot(), r, slot{kind: slotText, target: b.Snapshot().Operations[0].Id()})
	require.False(t, ok)
}

func TestBaseLegacy(t *testing.T) {
	backend, author, _ := newTestBug(t)
	r := legacyRemote{}

	b, create, err := backend.Bugs().NewRaw(author, 1000, "title", "body", nil, map[string]string{"old-id": "7"})
	require.NoError(t, err)
	_, _, err = b.ChangeLabelsRaw(author, 1001, []string{"bug", "ui"}, nil, map[string]string{"old-id": "e1"})
	require.NoError(t, err)
	_, _, err = b.ChangeLabelsRaw(author, 1002, nil, []string{"ui"}, nil)
	require.NoError(t, err)
	closeOp, err := b.CloseRaw(author, 1003, map[string]string{"old-id": "e2"})
	require.NoError(t, err)
	snap := b.Snapshot()

	id, ok := remoteIssueID(snap, r)
	require.True(t, ok)
	require.Equal(t, "7", id)

	title, _, ok := base(snap, r, slot{kind: slotTitle})
	require.True(t, ok)
	require.Equal(t, "title", title)

	body, _, ok := base(snap, r, slot{kind: slotText, target: create.Id()})
	require.True(t, ok)
	require.Equal(t, Fingerprint("body"), body)

	// the forge saw the first label change, not the second one
	labels, _, ok := base(snap, r, slot{kind: slotLabels})
	require.True(t, ok)
	require.Equal(t, "bug\nui", labels)

	status, op, ok := base(snap, r, slot{kind: slotStatus})
	require.True(t, ok)
	require.Equal(t, common.ClosedStatus.String(), status)
	require.Equal(t, closeOp.Id(), op.Id())

	opId, ok := opOnRemote(closeOp, snap, r)
	require.True(t, ok)
	require.Equal(t, "e2", opId)
}

func TestSyncKeys(t *testing.T) {
	require.Equal(t, "sync:gitlab.com/42:id", syncKey("gitlab.com/42", "id"))
	require.Equal(t, "sync:gitlab.com:user", userKey("gitlab.com"))
	var _ entity.Id = slot{}.target
	var _ *bug.Snapshot
}
