package core

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entity"
	"github.com/git-bug/git-bug/util/text"
)

// Link records that a bug is an issue already on a bridge's forge, given by
// its number ("12", "#12") or URL, so that sync treats them as one instead of
// creating the issue again. Nothing is written to the forge: the next sync
// sends git-bug's title, description and status where they differ, as
// changes not sent yet (Take keeps the forge's instead), and adds each side's
// comments and labels to the other. It refuses an issue that is another
// bug's, and a bug that has, or had, an issue there.
func Link(ctx context.Context, repo *cache.RepoCache, prefix, bridge, issue string) (Ref, error) {
	b, e, unlock, err := bugEngine(ctx, repo, prefix, bridge)
	if err != nil {
		return Ref{}, err
	}
	defer unlock()
	return e.link(ctx, b, bridge, issue)
}

// Unlink stops syncing a bug with a bridge's forge, for good: the bug keeps
// its content, the issue stays as it is, and sync creates neither again from
// the other. The issue can then be linked to another bug.
func Unlink(ctx context.Context, repo *cache.RepoCache, prefix, bridge string) error {
	b, e, unlock, err := bugEngine(ctx, repo, prefix, bridge)
	if err != nil {
		return err
	}
	defer unlock()
	if !e.synced(b) {
		return notSynced(b, bridge)
	}
	if _, err := b.SetMetadata(b.Snapshot().Operations[0].Id(), map[string]string{e.key("gone"): "unlinked"}); err != nil {
		return err
	}
	return b.Commit()
}

// Take replaces what git-bug hasn't sent to a bridge's forge yet, for one bug,
// with the forge's version: the title, description, comments' texts, status
// and labels the issue has now. The issue is pulled, as by SyncIssue, and
// nothing is written to the forge. Comments the forge doesn't have stay, to
// be sent by the next sync: git-bug can't remove a comment. The changes taken
// are made by the user's identity.
func Take(ctx context.Context, repo *cache.RepoCache, prefix, bridge string) (<-chan SyncResult, error) {
	me, err := repo.GetUserIdentity()
	if err != nil {
		return nil, err
	}
	b, e, unlock, err := bugEngine(ctx, repo, prefix, bridge)
	if err != nil {
		return nil, err
	}
	if !e.synced(b) {
		unlock()
		return nil, notSynced(b, bridge)
	}
	e.theirs = me

	out := make(chan SyncResult)
	go func() {
		defer close(out)
		defer unlock()
		for r := range e.PullIssue(ctx, b) {
			out <- SyncResult{Bridge: bridge, Import: &r}
		}
	}()
	return out, nil
}

// bugEngine resolves a bug, and creates the engine of a bridge under the
// bridge lock, which unlock releases.
func bugEngine(ctx context.Context, repo *cache.RepoCache, prefix, name string) (*cache.BugCache, *Engine, func(), error) {
	b, err := repo.Bugs().ResolvePrefix(prefix)
	if err != nil {
		return nil, nil, nil, err
	}
	if !BridgeExist(repo, name) {
		return nil, nil, nil, fmt.Errorf("no bridge named %s", name)
	}
	br, err := LoadBridge(repo, name)
	if err != nil {
		return nil, nil, nil, err
	}
	if !br.SupportsSync() {
		return nil, nil, nil, fmt.Errorf("bridge %s: the %s bridge doesn't sync issues", name, br.impl.Target())
	}
	unlock, err := lockBridges(repo)
	if err != nil {
		return nil, nil, nil, err
	}
	e, err := br.newEngine(ctx, false)
	if err != nil {
		unlock()
		return nil, nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	return b, e, unlock, nil
}

// synced tells whether the bug syncs with an issue of the forge.
func (e *Engine) synced(b *cache.BugCache) bool {
	snap := b.Snapshot()
	_, onRemote := remoteIssueID(snap, e.remote)
	_, gone := snap.Operations[0].GetMetadata(e.key("gone"))
	return onRemote && !gone
}

func notSynced(b *cache.BugCache, bridge string) error {
	return fmt.Errorf("bug %s is not synced with bridge %s", b.Id().Human(), bridge)
}

func (e *Engine) link(ctx context.Context, b *cache.BugCache, bridge, arg string) (Ref, error) {
	snap := b.Snapshot()
	createOp := snap.Operations[0]
	if ref, ok := e.issueRef(snap); ok {
		if _, gone := createOp.GetMetadata(e.key("gone")); gone {
			// metadata keeps a key's first value
			return Ref{}, fmt.Errorf("bug %s was issue %s on bridge %s, unlinked or deleted since: a bug's issue can't change", b.Id().Human(), ref.URL, bridge)
		}
		return Ref{}, fmt.Errorf("bug %s is already issue %s on bridge %s", b.Id().Human(), ref.URL, bridge)
	}

	want, err := issueArg(arg)
	if err != nil {
		return Ref{}, err
	}
	issue, err := e.remote.Fetch(ctx, want)
	if errors.Is(err, ErrGone) {
		return Ref{}, fmt.Errorf("bridge %s has no issue %s", bridge, arg)
	}
	if err != nil {
		return Ref{}, err
	}
	if want.URL != "" && !sameURL(want.URL, issue.URL) {
		return Ref{}, fmt.Errorf("%s is not an issue of bridge %s, whose issue %s is %s", want.URL, bridge, want.Number, issue.URL)
	}
	var gone entity.Id
	other, err := e.repo.Bugs().ResolveMatcher(e.issueMatcher(issue.ID, &gone))
	if err == nil {
		return Ref{}, fmt.Errorf("issue %s is already bug %s: %s", issue.URL, other.Id().Human(), other.Snapshot().Title)
	}
	if multiple, ok := errors.AsType[*entity.ErrMultipleMatch](err); ok {
		return Ref{}, fmt.Errorf("issue %s is already %d bugs", issue.URL, len(multiple.Matching))
	}
	if !entity.IsErrNotFound(err) {
		return Ref{}, err
	}

	tags := map[string]string{
		e.key("id"):     issue.ID,
		e.key("number"): issue.Number,
		e.key("url"):    issue.URL,
		e.urlKey():      issue.URL,
	}
	if e.isAccount(issue.Author) {
		tags[e.key("as")] = issue.Author.Login
	}
	// the forge's history isn't replayed over git-bug's: taken as seen
	for _, ev := range issue.Events {
		tags[e.key("also:"+ev.ID)] = "linked"
	}
	records := map[entity.Id]map[string]string{createOp.Id(): tags}
	// a value both sides hold is in sync; git-bug's others are changes not
	// sent yet. The title is git-bug's as sent there, its #refs translated.
	refs := newRefIndex(e.excerpts())
	refs.created(b.Id(), e.remote.Key(), issue.Ref)
	if title := cleanTitle(issue.Title); title == e.title(snap, refs) {
		addRecord(records, lastSlotOp(snap, s(slotTitle, "")).Id(), e.key("title"), title)
	}
	if issue.Status == snap.Status {
		addRecord(records, lastSlotOp(snap, s(slotStatus, "")).Id(), e.key("status"), issue.Status.String())
	}
	if Unrender(text.Cleanup(issue.Body)) == snap.Comments[0].Message {
		addRecord(records, lastSlotOp(snap, s(slotText, createOp.Id())).Id(), e.key("text"), Fingerprint(issue.Body))
	}
	// labels merge, as if the issue had none when last in sync
	addRecord(records, lastSlotOp(snap, s(slotLabels, "")).Id(), e.key("labels"), "")
	if err := setRecords(b, records); err != nil {
		return Ref{}, err
	}
	return issue.Ref, b.Commit()
}

// issueMatcher matches the bug synced with a forge issue. A bug the issue was
// unlinked from, or deleted for, doesn't match: its id goes to gone.
func (e *Engine) issueMatcher(id string, gone *entity.Id) func(*cache.BugExcerpt) bool {
	idKey, goneKey := e.key("id"), e.key("gone")
	return func(excerpt *cache.BugExcerpt) bool {
		match := excerpt.CreateMetadata[idKey] == id
		if !match {
			legacy, ok := e.remote.Legacy(excerpt.CreateMetadata, excerpt.CreateMetadata)
			match = ok && legacy == id
		}
		if match && excerpt.CreateMetadata[goneKey] != "" {
			*gone = excerpt.Id()
			return false
		}
		return match
	}
}

// issueArg reads an issue given by its number, "12" or "#12", or its URL.
func issueArg(s string) (Ref, error) {
	if n := strings.TrimPrefix(s, "#"); n != "" && strings.Trim(n, "0123456789") == "" {
		return Ref{Number: n}, nil
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		if n := trailingNumber.FindString(strings.TrimSuffix(u.Path, "/")); n != "" {
			return Ref{Number: n, URL: s}, nil
		}
	}
	return Ref{}, fmt.Errorf("%q is neither an issue number nor an issue URL", s)
}

// sameURL compares two URLs by host and path, ignoring case.
func sameURL(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(ua.Host+strings.TrimSuffix(ua.Path, "/"), ub.Host+strings.TrimSuffix(ub.Path, "/"))
}
