package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/entities/identity"
	"github.com/git-bug/git-bug/entity"
	"github.com/git-bug/git-bug/entity/dag"
	"github.com/git-bug/git-bug/util/text"
)

// emptyTitle replaces a forge issue's title that git-bug can't hold.
const emptyTitle = "(no title)"

// Pull brings what changed on the forge since the given time into git-bug.
func (e *Engine) Pull(ctx context.Context, since time.Time) <-chan ImportResult {
	out := make(chan ImportResult)
	go func() {
		defer close(out)
		for issue, err := range e.remote.Issues(ctx, since) {
			if err != nil {
				out <- NewImportError(fmt.Errorf("listing issues: %w", err), "")
				return
			}
			if err := e.pullIssue(ctx, issue, out); err != nil {
				out <- NewImportError(fmt.Errorf("issue %s: %w", issue.URL, err), "")
			}
		}
		e.pullAllPulls(ctx, out)
		// a forge may end its listing quietly when stopped: a pull that
		// didn't read everything must not look complete
		if err := ctx.Err(); err != nil {
			out <- NewImportError(fmt.Errorf("pull stopped: %w", err), "")
		}
	}()
	return out
}

// PullIssue brings one bug's issue into git-bug, as Pull does every issue
// changed since a time: nothing when the bug isn't on the forge. A bug not
// recorded there may be all the same, see pullMarked.
func (e *Engine) PullIssue(ctx context.Context, b *cache.BugCache) <-chan ImportResult {
	out := make(chan ImportResult)
	go func() {
		defer close(out)
		defer e.pullPulls(ctx, b, out)
		snap := b.Snapshot()
		ref, ok := e.issueRef(snap)
		if !ok {
			e.pullMarked(ctx, b, out)
			return
		}
		if _, gone := snap.Operations[0].GetMetadata(e.key("gone")); gone {
			out <- NewImportNothing(b.Id(), "not synced with this forge anymore")
			return
		}
		issue, err := e.remote.Fetch(ctx, ref)
		if errors.Is(err, ErrGone) {
			out <- NewImportWarning(fmt.Errorf("issue %s deleted on the forge", ref.URL), b.Id())
			return
		}
		if err == nil {
			err = e.pullIssue(ctx, issue, out)
		}
		if err != nil {
			out <- NewImportError(fmt.Errorf("issue %s: %w", ref.URL, err), b.Id())
		}
	}()
	return out
}

// pullMarked pulls the issue of a bug not recorded on the forge, created by
// another clone's push or by one that died before recording it, as Pull
// would: found by its git-bug marker among the issues updated since the bug
// was created, as it was pushed after.
func (e *Engine) pullMarked(ctx context.Context, b *cache.BugCache, out chan<- ImportResult) {
	// ponytail: lists the project's issues updated since, once per bug not
	// pushed yet; a forge search for the marker if that costs too much
	for issue, err := range e.remote.Issues(ctx, b.Snapshot().CreateTime.Add(-claimWindow)) {
		if err != nil {
			out <- NewImportError(fmt.Errorf("listing issues: %w", err), b.Id())
			return
		}
		if id, ok := e.marker(issue); ok && id == b.Id() {
			if err := e.pullIssue(ctx, issue, out); err != nil {
				out <- NewImportError(fmt.Errorf("issue %s: %w", issue.URL, err), b.Id())
			}
			return
		}
	}
	// a forge may end its listing quietly when stopped: the push must not
	// create an issue that may be there
	if err := ctx.Err(); err != nil {
		out <- NewImportError(fmt.Errorf("pull stopped: %w", err), b.Id())
		return
	}
	out <- NewImportNothing(b.Id(), "not on this forge yet")
}

func (e *Engine) pullIssue(ctx context.Context, issue *Issue, out chan<- ImportResult) error {
	b, duplicate, err := e.findBug(issue, out)
	if err != nil {
		return err
	}
	if duplicate {
		out <- NewImportWarning(fmt.Errorf("issue %s duplicates the issue of bug %s, as a retried write can do: not imported", issue.URL, b.Id().Human()), b.Id())
		return nil
	}
	if b != nil {
		if _, gone := b.Snapshot().Operations[0].GetMetadata(e.key("gone")); gone {
			out <- NewImportNothing(b.Id(), "not synced with this forge anymore")
			return nil
		}
	}
	if e.opts.DryRun {
		e.planPull(b, issue, out)
		return nil
	}
	if b == nil {
		if b, err = e.newBug(issue); err != nil {
			return err
		}
		out <- NewImportBug(b.Id())
	}

	// replay comments and events in the order they happened
	type item struct {
		at      time.Time
		comment *Comment
		event   *Event
	}
	var items []item
	for i := range issue.Comments {
		items = append(items, item{at: issue.Comments[i].Created, comment: &issue.Comments[i]})
	}
	for i := range issue.Events {
		items = append(items, item{at: issue.Events[i].Time, event: &issue.Events[i]})
	}
	slices.SortStableFunc(items, func(a, b item) int { return a.at.Compare(b.at) })

	// values whose conflict the replay decided
	decided := map[slot]bool{}
	for _, it := range items {
		if it.comment != nil {
			err = e.pullComment(b, it.comment, out)
		} else {
			err = e.pullEvent(b, it.event, decided, out)
		}
		if err != nil {
			return err
		}
	}

	if err := e.reconcile(ctx, b, issue, decided, out); err != nil {
		return err
	}

	if !b.NeedCommit() {
		out <- NewImportNothing(b.Id(), "no change")
		return nil
	}
	return b.Commit()
}

// findBug returns the bug of a forge issue, nil if there's none, or the bug
// it was unlinked from. duplicate tells that the issue carries the marker of
// a bug already on the forge with another issue.
func (e *Engine) findBug(issue *Issue, out chan<- ImportResult) (b *cache.BugCache, duplicate bool, err error) {
	var gone entity.Id
	b, err = e.repo.Bugs().ResolveMatcher(e.issueMatcher(issue.ID, &gone))
	if multiple, ok := errors.AsType[*entity.ErrMultipleMatch](err); ok {
		// the old bridges could import an issue they had exported
		b, err = e.pickDuplicate(multiple.Matching)
		if err == nil {
			out <- NewImportWarning(fmt.Errorf("%d bugs are issue %s, left by an older bridge: syncing %s", len(multiple.Matching), issue.URL, b.Id().Human()), b.Id())
		}
	}
	if err == nil || !entity.IsErrNotFound(err) {
		return b, false, err
	}
	if gone != "" {
		// not synced anymore, nor imported again
		b, err = e.repo.Bugs().Resolve(gone)
		return b, false, err
	}

	// created by a push that died before recording it
	id, ok := e.marker(issue)
	if !ok {
		return nil, false, nil
	}
	b, err = e.repo.Bugs().Resolve(id)
	if entity.IsErrNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if _, onRemote := remoteIssueID(b.Snapshot(), e.remote); onRemote {
		return b, true, nil
	}
	if e.opts.DryRun {
		e.attached[b.Id()] = issue.URL
		return b, false, nil
	}
	tags := e.issueTags(issue)
	tags[e.key("as")] = issue.Author.Login
	_, err = b.SetMetadata(b.Snapshot().Operations[0].Id(), tags)
	return b, false, err
}

// marker returns the bug whose git-bug marker an issue carries: only
// git-bug's accounts post markers, anyone else's is just text.
func (e *Engine) marker(issue *Issue) (entity.Id, bool) {
	id, ok := MarkerID(text.Cleanup(issue.OriginalBody))
	return id, ok && e.isAccount(issue.Author)
}

// planPull reports what pulling an issue would do, without doing it.
func (e *Engine) planPull(b *cache.BugCache, issue *Issue, out chan<- ImportResult) {
	if b == nil {
		out <- NewImportNothing("", "would import issue "+issue.URL)
		return
	}
	snap := b.Snapshot()
	n := 0
	for _, c := range issue.Comments {
		if e.findOp(snap, c.ID) == nil {
			n++
		}
	}
	for _, ev := range issue.Events {
		if e.findOp(snap, ev.ID) == nil {
			n++
		}
	}
	if n > 0 {
		// some may be echoes of pushes, claimed instead of pulled
		out <- NewImportNothing(b.Id(), fmt.Sprintf("would pull up to %d changes from %s", n, issue.URL))
	}
}

// pickDuplicate returns, of bugs that are the same forge issue, the one
// pulled from this forge, then the oldest.
func (e *Engine) pickDuplicate(ids []entity.Id) (*cache.BugCache, error) {
	var best *cache.BugExcerpt
	for _, id := range ids {
		excerpt, err := e.repo.Bugs().ResolveExcerpt(id)
		if err != nil {
			return nil, err
		}
		pulled := excerpt.CreateMetadata[MetaKeyOrigin] == e.remote.Kind()
		if best == nil {
			best = excerpt
			continue
		}
		bestPulled := best.CreateMetadata[MetaKeyOrigin] == e.remote.Kind()
		if pulled && !bestPulled || pulled == bestPulled && excerpt.CreateLamportTime < best.CreateLamportTime {
			best = excerpt
		}
	}
	return e.repo.Bugs().Resolve(best.Id())
}

// issueTags are the metadata of a create operation for its forge issue, as
// created.
func (e *Engine) issueTags(issue *Issue) map[string]string {
	return map[string]string{
		e.key("id"):     issue.ID,
		e.key("number"): issue.Number,
		e.key("url"):    issue.URL,
		e.urlKey():      issue.URL,
		e.key("title"):  cleanTitle(issue.OriginalTitle),
		e.key("status"): common.OpenStatus.String(),
		e.key("labels"): "",
		e.key("text"):   Fingerprint(issue.OriginalBody),
	}
}

// urlKey is where the old bridges kept an issue's URL, which tools built on
// them read. An operation keeps a key's first value, so with two remotes of
// one kind it's the first one's.
func (e *Engine) urlKey() string {
	return e.remote.Kind() + "-url"
}

func (e *Engine) newBug(issue *Issue) (*cache.BugCache, error) {
	author, err := e.identityFor(issue.Author)
	if err != nil {
		return nil, err
	}
	meta := e.issueTags(issue)
	meta[MetaKeyOrigin] = e.remote.Kind()
	b, _, err := e.repo.Bugs().NewRaw(author, issue.Created.Unix(), cleanTitle(issue.OriginalTitle),
		Unrender(text.Cleanup(issue.OriginalBody)), nil, meta)
	return b, err
}

func (e *Engine) pullComment(b *cache.BugCache, c *Comment, out chan<- ImportResult) error {
	snap := b.Snapshot()
	if e.findOp(snap, c.ID) != nil {
		return nil
	}
	tags := map[string]string{
		e.key("id"):   c.ID,
		e.key("url"):  c.URL,
		e.key("text"): Fingerprint(c.Original),
	}

	// posted by a push that died before recording it, as for issues
	if id, ok := MarkerID(text.Cleanup(c.Original)); ok && e.isAccount(c.Author) {
		if op := opByID(snap, id); op != nil {
			if _, onRemote := opOnRemote(op, snap, e.remote); !onRemote {
				tags[e.key("as")] = c.Author.Login
				_, err := b.SetMetadata(op.Id(), tags)
				return err
			}
		}
	}

	author, err := e.identityFor(c.Author)
	if err != nil {
		return err
	}
	commentId, _, err := b.AddCommentRaw(author, c.Created.Unix(), Unrender(text.Cleanup(c.Original)), nil, tags)
	if err != nil {
		return err
	}
	out <- NewImportComment(b.Id(), commentId)
	return nil
}

func (e *Engine) pullEvent(b *cache.BugCache, ev *Event, decided map[slot]bool, out chan<- ImportResult) error {
	snap := b.Snapshot()
	if e.findOp(snap, ev.ID) != nil {
		return nil
	}
	claimed, err := e.claim(b, ev)
	if err != nil || claimed {
		return err
	}
	skip, err := e.replayConflict(b, snap, ev, decided, out)
	if err != nil || skip {
		return err
	}

	author, err := e.identityFor(ev.Author)
	if err != nil {
		return err
	}
	at := ev.Time.Unix()
	tags := map[string]string{e.key("id"): ev.ID}

	switch ev.Kind {
	case EventStatus:
		tags[e.key("status")] = ev.Status.String()
		var opId entity.Id
		if ev.Status == common.ClosedStatus {
			op, err := b.CloseRaw(author, at, tags)
			if err != nil {
				return err
			}
			opId = op.Id()
		} else {
			op, err := b.OpenRaw(author, at, tags)
			if err != nil {
				return err
			}
			opId = op.Id()
		}
		out <- NewImportStatusChange(b.Id(), opId)

	case EventTitle:
		title := cleanTitle(ev.Title)
		tags[e.key("title")] = title
		op, err := b.SetTitleRaw(author, at, title, tags)
		if err != nil {
			return err
		}
		out <- NewImportTitleEdition(b.Id(), op.Id())

	case EventLabels:
		added, removed := cleanLabels(ev.Added), cleanLabels(ev.Removed)
		// what the forge holds after the event, which local changes don't affect
		held := snap.Labels
		if v, _, ok := base(snap, e.remote, slot{kind: slotLabels}); ok {
			held = splitLabels(v)
		}
		tags[e.key("labels")] = labelsValue(applyLabels(held, added, removed))
		op, err := b.ForceChangeLabelsRaw(author, at, added, removed, tags)
		if err != nil {
			return err
		}
		out <- NewImportLabelChange(b.Id(), op.Id())

	case EventTextEdit:
		target := snap.Comments[0].CombinedId()
		if ev.Target != "" {
			op := e.findOp(snap, ev.Target)
			if op == nil {
				// a comment git-bug doesn't have, like a deleted one
				return nil
			}
			c, err := snap.SearchCommentByOpId(op.Id())
			if err != nil {
				return err
			}
			target = c.CombinedId()
		}
		tags[e.key("text")] = Fingerprint(ev.Text)
		if _, err := b.EditCommentRaw(author, at, target, Unrender(text.Cleanup(ev.Text)), tags); err != nil {
			return err
		}
		out <- NewImportCommentEdition(b.Id(), target)

	default:
		return fmt.Errorf("unknown event kind %d", ev.Kind)
	}
	return nil
}

// replayConflict handles an event changing a value that also changed in
// git-bug since the forge last saw it: the later change wins, as when
// reconciling. It tells whether to skip replaying the event.
func (e *Engine) replayConflict(b *cache.BugCache, snap *bug.Snapshot, ev *Event, decided map[slot]bool, out chan<- ImportResult) (bool, error) {
	var s slot
	var local, value string
	switch ev.Kind {
	case EventStatus:
		s, local, value = slot{kind: slotStatus}, snap.Status.String(), ev.Status.String()
	case EventTitle:
		s, local, value = slot{kind: slotTitle}, snap.Title, cleanTitle(ev.Title)
	case EventTextEdit:
		target := snap.Operations[0]
		if ev.Target != "" {
			if target = e.findOp(snap, ev.Target); target == nil {
				return false, nil
			}
		}
		comment, err := snap.SearchCommentByOpId(target.Id())
		if err != nil {
			return false, err
		}
		s, local, value = slot{kind: slotText, target: target.Id()}, comment.Message, Unrender(text.Cleanup(ev.Text))
	default:
		// labels merge
		return false, nil
	}

	baseValue, baseOp, ok := base(snap, e.remote, s)
	if !ok {
		return false, nil
	}
	localOp := lastSlotOp(snap, s)
	if !localChanged(s, local, baseValue, localOp, baseOp) {
		return false, nil
	}

	decided[s] = true
	seen := map[string]string{e.key("also:" + ev.ID): "replayed"}
	if local == value {
		// converged
		recorded := value
		if s.kind == slotText {
			recorded = Fingerprint(ev.Text)
		}
		seen[e.key(string(s.kind))] = recorded
		_, err := b.SetMetadata(localOp.Id(), seen)
		return true, err
	}
	if e.theirs != nil || ev.Time.After(localOp.Time()) {
		out <- NewImportWarning(fmt.Errorf("%s changed on both sides, kept the forge's change", s.kind), b.Id())
		return false, nil
	}
	out <- NewImportWarning(fmt.Errorf("%s changed on both sides, kept git-bug's change", s.kind), b.Id())
	_, err := b.SetMetadata(localOp.Id(), seen)
	return true, err
}

// claimWindow is how much earlier than a push its echo may be dated, as
// clocks differ.
const claimWindow = 5 * time.Minute

// claim recognizes an event that a push of git-bug caused, and records it on
// the push instead of creating an operation: an event posted by one of the
// accounts, at most claimWindow before the push, matching the kind and value
// of a push not claimed yet. A label push may be claimed by several events,
// as forges emit one per label.
func (e *Engine) claim(b *cache.BugCache, ev *Event) (bool, error) {
	if !e.isAccount(ev.Author) {
		return false, nil
	}
	snap := b.Snapshot()
	pushedKey, alsoPrefix := e.key("pushed"), e.key("also:")

	claimed := map[string]bool{}
	for _, op := range snap.Operations {
		if sm, ok := op.(*dag.SetMetadataOperation[*bug.Snapshot]); ok {
			for k, v := range sm.NewMetadata {
				if strings.HasPrefix(k, alsoPrefix) {
					claimed[v] = true
				}
			}
		}
	}

	for _, op := range snap.Operations {
		push, ok := op.(*dag.SetMetadataOperation[*bug.Snapshot])
		if !ok {
			continue
		}
		at, ok := push.NewMetadata[pushedKey]
		if !ok || (claimed[push.Id().String()] && ev.Kind != EventLabels) {
			continue
		}
		unix, err := strconv.ParseInt(at, 10, 64)
		if err != nil || ev.Time.Before(time.Unix(unix, 0).Add(-claimWindow)) {
			continue
		}
		if !e.pushedAs(push.NewMetadata, ev) {
			continue
		}
		_, err = b.SetMetadata(push.Target, map[string]string{alsoPrefix + ev.ID: push.Id().String()})
		return true, err
	}
	return false, nil
}

// pushedAs tells whether a push recorded with the given metadata would make
// the forge emit the event.
func (e *Engine) pushedAs(push map[string]string, ev *Event) bool {
	switch ev.Kind {
	case EventStatus:
		return push[e.key("status")] == ev.Status.String()
	case EventTitle:
		return push[e.key("title")] == cleanTitle(ev.Title)
	case EventLabels:
		v, ok := push[e.key("labels")]
		if !ok {
			return false
		}
		set := splitLabels(v)
		for _, l := range cleanLabels(ev.Added) {
			if !slices.Contains(set, common.Label(l)) {
				return false
			}
		}
		for _, l := range cleanLabels(ev.Removed) {
			if slices.Contains(set, common.Label(l)) {
				return false
			}
		}
		return true
	case EventTextEdit:
		return push[e.key("text")] == Fingerprint(ev.Text)
	}
	return false
}

// isAccount tells whether a forge user owns one of the tokens: only their
// markers are git-bug's.
func (e *Engine) isAccount(u User) bool {
	for _, a := range e.tokens {
		if a.ID == u.ID {
			return true
		}
	}
	return false
}

// reconcile records the changes of the forge that no event explained,
// three-way: see doc/design/bridges/sync.md.
func (e *Engine) reconcile(ctx context.Context, b *cache.BugCache, issue *Issue, decided map[slot]bool, out chan<- ImportResult) error {
	// SetMetadata records to write, by target
	records := map[entity.Id]map[string]string{}
	record := func(target entity.Id, k, v string) {
		if records[target] == nil {
			records[target] = map[string]string{}
		}
		records[target][e.key(k)] = v
	}
	conflict := func(what string, forgeWins bool) {
		kept := "git-bug's"
		if forgeWins {
			kept = "the forge's"
		}
		out <- NewImportWarning(fmt.Errorf("%s changed on both sides, kept %s change", what, kept), b.Id())
	}

	// who and when sets a value taken from the forge: the forge's change, or
	// with theirs undoing git-bug's, the user now
	by := func(u User, at time.Time, forgeChanged bool) (identity.Interface, int64, error) {
		if !forgeChanged && e.theirs != nil {
			return e.theirs, time.Now().Unix(), nil
		}
		author, err := e.identityFor(u)
		return author, at.Unix(), err
	}
	// skip tells whether to leave a value alone: the forge didn't change it,
	// and with theirs, git-bug didn't either
	skip := func(s slot, forgeChanged, changed bool) bool {
		return decided[s] || !forgeChanged && (e.theirs == nil || !changed)
	}

	snap := b.Snapshot()

	// title and status: the later change wins
	for _, sc := range []struct {
		kind          slotKind
		remote, local string
		apply         func(identity.Interface, int64, map[string]string) (dag.Operation, error)
		event         func(entity.Id, entity.Id) ImportResult
	}{
		{slotTitle, cleanTitle(issue.Title), snap.Title, func(a identity.Interface, at int64, tags map[string]string) (dag.Operation, error) {
			return b.SetTitleRaw(a, at, cleanTitle(issue.Title), tags)
		}, NewImportTitleEdition},
		{slotStatus, issue.Status.String(), snap.Status.String(), func(a identity.Interface, at int64, tags map[string]string) (dag.Operation, error) {
			if issue.Status == common.ClosedStatus {
				return b.CloseRaw(a, at, tags)
			}
			return b.OpenRaw(a, at, tags)
		}, NewImportStatusChange},
	} {
		s := slot{kind: sc.kind}
		baseValue, baseOp, ok := base(snap, e.remote, s)
		local := lastSlotOp(snap, s)
		forgeChanged := ok && sc.remote != baseValue
		changed := !ok || localChanged(s, sc.local, baseValue, local, baseOp)
		if skip(s, forgeChanged, changed) {
			continue
		}
		at := changedAt(issue, s, issue.Updated)
		switch {
		case sc.local == sc.remote:
			record(local.Id(), string(sc.kind), sc.remote)
		case e.theirs != nil || !changed || at.After(local.Time()):
			if changed && forgeChanged {
				conflict(string(sc.kind), true)
			}
			author, unix, err := by(issue.Author, at, forgeChanged)
			if err != nil {
				return err
			}
			op, err := sc.apply(author, unix, map[string]string{e.key(string(sc.kind)): sc.remote})
			if err != nil {
				return err
			}
			out <- sc.event(b.Id(), op.Id())
		default:
			conflict(string(sc.kind), false)
		}
	}

	// labels: both sides' changes merge, or with theirs the forge's set stays
	ls := slot{kind: slotLabels}
	baseValue, _, ok := base(snap, e.remote, ls)
	remote := cleanLabels(issue.Labels)
	forgeChanged := ok && labelsValue(remote) != baseValue
	changed := !ok || labelsValue(labelStrings(snap.Labels)) != baseValue
	if !skip(ls, forgeChanged, changed) {
		was := splitLabels(baseValue)
		if e.theirs != nil {
			was = snap.Labels
		}
		var added, removed []string
		for _, l := range remote {
			if !slices.Contains(was, common.Label(l)) && !slices.Contains(snap.Labels, common.Label(l)) {
				added = append(added, l)
			}
		}
		for _, l := range was {
			if !slices.Contains(remote, l.String()) && slices.Contains(snap.Labels, l) {
				removed = append(removed, l.String())
			}
		}
		if len(added) == 0 && len(removed) == 0 {
			record(lastSlotOp(snap, ls).Id(), string(slotLabels), labelsValue(remote))
		} else {
			author, unix, err := by(issue.Author, changedAt(issue, ls, issue.Updated), forgeChanged)
			if err != nil {
				return err
			}
			op, err := b.ForceChangeLabelsRaw(author, unix, added, removed,
				map[string]string{e.key(string(slotLabels)): labelsValue(remote)})
			if err != nil {
				return err
			}
			out <- NewImportLabelChange(b.Id(), op.Id())
		}
	}

	// texts: the later change wins
	type remoteText struct {
		target entity.Id // the create or add-comment operation
		text   string
		at     time.Time
		editor User
	}
	editor := issue.Author
	if issue.BodyEditor != nil {
		editor = *issue.BodyEditor
	}
	bodyAt := issue.Updated
	if !issue.BodyUpdated.IsZero() {
		bodyAt = issue.BodyUpdated
	}
	createOp := snap.Operations[0]
	texts := []remoteText{{createOp.Id(), issue.Body, changedAt(issue, slot{kind: slotText}, bodyAt), editor}}
	for _, c := range issue.Comments {
		if op := e.findOp(snap, c.ID); op != nil {
			texts = append(texts, remoteText{op.Id(), c.Body, c.Updated, c.Author})
		}
	}
	for _, rt := range texts {
		s := slot{kind: slotText, target: rt.target}
		baseValue, baseOp, ok := base(snap, e.remote, s)
		fingerprint := Fingerprint(rt.text)
		local := lastSlotOp(snap, s)
		forgeChanged := ok && fingerprint != baseValue
		changed := !ok || localChanged(s, "", baseValue, local, baseOp)
		if skip(s, forgeChanged, changed) {
			continue
		}
		comment, err := snap.SearchCommentByOpId(rt.target)
		if err != nil {
			return err
		}
		pulled := Unrender(text.Cleanup(rt.text))
		switch {
		case pulled == comment.Message:
			record(local.Id(), string(slotText), fingerprint)
		case e.theirs != nil || !changed || rt.at.After(local.Time()):
			if changed && forgeChanged {
				conflict("a text", true)
			}
			author, unix, err := by(rt.editor, rt.at, forgeChanged)
			if err != nil {
				return err
			}
			_, err = b.EditCommentRaw(author, unix, comment.CombinedId(), pulled,
				map[string]string{e.key(string(slotText)): fingerprint})
			if err != nil {
				return err
			}
			out <- NewImportCommentEdition(b.Id(), comment.CombinedId())
		default:
			conflict("a text", false)
		}
	}

	for target, meta := range records {
		if _, err := b.SetMetadata(target, meta); err != nil {
			return err
		}
	}
	return nil
}

// localChanged tells whether git-bug changed a value since the forge last
// saw it, given its base. A text's base is the fingerprint of it as rendered
// for the forge, and a title's has its #refs translated: their operations
// tell, not their values.
func localChanged(s slot, local, baseValue string, localOp, baseOp dag.Operation) bool {
	if s.kind == slotText || s.kind == slotTitle {
		return localOp.Id() != baseOp.Id()
	}
	return local != baseValue
}

// changedAt returns when the forge last changed a value: the time of its
// latest event about it, or def when it reported none. The slot of the body
// has no target here: forge events name it with an empty Target.
func changedAt(issue *Issue, s slot, def time.Time) time.Time {
	var at time.Time
	for _, ev := range issue.Events {
		var match bool
		switch s.kind {
		case slotTitle:
			match = ev.Kind == EventTitle
		case slotStatus:
			match = ev.Kind == EventStatus
		case slotLabels:
			match = ev.Kind == EventLabels
		case slotText:
			match = ev.Kind == EventTextEdit && ev.Target == ""
		}
		if match && ev.Time.After(at) {
			at = ev.Time
		}
	}
	if at.IsZero() {
		return def
	}
	return at
}

func splitLabels(v string) []common.Label {
	if v == "" {
		return nil
	}
	var res []common.Label
	for _, l := range strings.Split(v, "\n") {
		res = append(res, common.Label(l))
	}
	return res
}

func cleanTitle(title string) string {
	title = text.CleanupOneLine(title)
	if text.Empty(title) {
		return emptyTitle
	}
	return title
}

func cleanLabels(labels []string) []string {
	res := make([]string, 0, len(labels))
	for _, l := range labels {
		if l = text.CleanupOneLine(l); l != "" {
			res = append(res, l)
		}
	}
	return res
}

// applyLabels returns the label set after a change.
func applyLabels(current []common.Label, added, removed []string) []string {
	var res []string
	for _, l := range current {
		if !slices.Contains(removed, l.String()) {
			res = append(res, l.String())
		}
	}
	for _, l := range added {
		if !slices.Contains(res, l) {
			res = append(res, l)
		}
	}
	slices.SortFunc(res, cmp.Compare)
	return res
}
