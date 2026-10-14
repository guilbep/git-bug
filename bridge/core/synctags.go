package core

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/entity"
	"github.com/git-bug/git-bug/entity/dag"
	"github.com/git-bug/git-bug/util/text"
)

// What a remote has seen is recorded as write-once metadata on operations,
// keyed by the remote: "sync:<remote key>:<name>". See doc/design/bridges/sync.md.

func syncKey(remoteKey, name string) string {
	return "sync:" + remoteKey + ":" + name
}

// userKey is the identity metadata holding a user's id on a forge server.
func userKey(instance string) string {
	return "sync:" + instance + ":user"
}

// Fingerprint identifies a text as a forge holds it, ignoring what
// text.Cleanup normalizes.
func Fingerprint(t string) string {
	sum := sha256.Sum256([]byte(text.Cleanup(t)))
	return hex.EncodeToString(sum[:])
}

func labelsValue(labels []string) string {
	sorted := slices.Clone(labels)
	slices.Sort(sorted)
	return strings.Join(sorted, "\n")
}

type slotKind string

const (
	slotTitle  slotKind = "title"
	slotStatus slotKind = "status"
	slotLabels slotKind = "labels"
	// slotText is the text of the create operation (the body) or of an
	// add-comment operation, designated by target.
	slotText slotKind = "text"
)

// slot is a value that syncs.
type slot struct {
	kind   slotKind
	target entity.Id
}

// base returns the value of the slot that remote r last saw, and the
// operation that records it.
func base(snap *bug.Snapshot, r Remote, s slot) (string, dag.Operation, bool) {
	createMeta := snap.Operations[0].AllMetadata()
	key := syncKey(r.Key(), string(s.kind))
	labels := map[string]struct{}{}

	var value string
	var found dag.Operation
	for _, op := range snap.Operations {
		own, ok := slotValue(op, s, labels)
		if !ok {
			continue
		}
		if v, tagged := op.GetMetadata(key); tagged {
			value, found = v, op
			continue
		}
		// seen by a pre-sync bridge, with the value git-bug has
		if _, legacy := r.Legacy(op, createMeta); legacy {
			value, found = own, op
		}
	}
	return value, found, found != nil
}

// slotValue returns the slot's value after op, if op changes the slot.
// labels tracks the label set along the operations.
func slotValue(op dag.Operation, s slot, labels map[string]struct{}) (string, bool) {
	switch op := op.(type) {
	case *bug.CreateOperation:
		switch s.kind {
		case slotTitle:
			return op.Title, true
		case slotStatus:
			return common.OpenStatus.String(), true
		case slotLabels:
			return "", true
		case slotText:
			return Fingerprint(op.Message), op.Id() == s.target
		}
	case *bug.SetTitleOperation:
		return op.Title, s.kind == slotTitle
	case *bug.SetStatusOperation:
		return op.Status.String(), s.kind == slotStatus
	case *bug.LabelChangeOperation:
		for _, l := range op.Added {
			labels[l.String()] = struct{}{}
		}
		for _, l := range op.Removed {
			delete(labels, l.String())
		}
		set := make([]string, 0, len(labels))
		for l := range labels {
			set = append(set, l)
		}
		return labelsValue(set), s.kind == slotLabels
	case *bug.AddCommentOperation:
		return Fingerprint(op.Message), s.kind == slotText && op.Id() == s.target
	case *bug.EditCommentOperation:
		return Fingerprint(op.Message), s.kind == slotText && op.Target == s.target
	}
	return "", false
}

// remoteIssueID returns the id of the bug's issue on remote r.
func remoteIssueID(snap *bug.Snapshot, r Remote) (string, bool) {
	return opOnRemote(snap.Operations[0], snap, r)
}

// opOnRemote returns the id of what op is on remote r.
func opOnRemote(op dag.Operation, snap *bug.Snapshot, r Remote) (string, bool) {
	if id, ok := op.GetMetadata(syncKey(r.Key(), "id")); ok {
		return id, true
	}
	return r.Legacy(op, snap.Operations[0].AllMetadata())
}
