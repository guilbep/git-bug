package core

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/entity"
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
	}()
	return out
}

func (e *Engine) pullIssue(ctx context.Context, issue *Issue, out chan<- ImportResult) error {
	b, err := e.findBug(issue)
	if err != nil {
		return err
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

	for _, it := range items {
		if it.comment != nil {
			err = e.pullComment(b, it.comment, out)
		} else {
			err = e.pullEvent(b, it.event, out)
		}
		if err != nil {
			return err
		}
	}

	if err := e.reconcile(ctx, b, issue, out); err != nil {
		return err
	}

	if !b.NeedCommit() {
		out <- NewImportNothing(b.Id(), "no change")
		return nil
	}
	return b.Commit()
}

// findBug returns the bug of a forge issue, nil if there's none.
func (e *Engine) findBug(issue *Issue) (*cache.BugCache, error) {
	idKey := e.key("id")
	b, err := e.repo.Bugs().ResolveMatcher(func(excerpt *cache.BugExcerpt) bool {
		if excerpt.CreateMetadata[idKey] == issue.ID {
			return true
		}
		id, ok := e.remote.Legacy(excerpt.CreateMetadata, excerpt.CreateMetadata)
		return ok && id == issue.ID
	})
	if err == nil || !entity.IsErrNotFound(err) {
		return b, err
	}

	// created by a push that died before recording it
	id, ok := MarkerID(text.Cleanup(issue.OriginalBody))
	if !ok {
		return nil, nil
	}
	b, err = e.repo.Bugs().Resolve(id)
	if entity.IsErrNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, onRemote := remoteIssueID(b.Snapshot(), e.remote); onRemote {
		return nil, nil
	}
	_, err = b.SetMetadata(b.Snapshot().Operations[0].Id(), e.issueTags(issue))
	return b, err
}

// issueTags are the metadata of a create operation for its forge issue, as
// created.
func (e *Engine) issueTags(issue *Issue) map[string]string {
	return map[string]string{
		e.key("id"):     issue.ID,
		e.key("number"): issue.Number,
		e.key("url"):    issue.URL,
		e.key("title"):  cleanTitle(issue.OriginalTitle),
		e.key("status"): common.OpenStatus.String(),
		e.key("labels"): "",
		e.key("text"):   Fingerprint(issue.OriginalBody),
	}
}

func (e *Engine) newBug(issue *Issue) (*cache.BugCache, error) {
	author, err := identityFor(e.repo, e.remote, issue.Author, "")
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

	// posted by a push that died before recording it
	if id, ok := MarkerID(text.Cleanup(c.Original)); ok {
		if op := opByID(snap, id); op != nil {
			if _, onRemote := opOnRemote(op, snap, e.remote); !onRemote {
				tags[e.key("as")] = c.Author.Login
				_, err := b.SetMetadata(op.Id(), tags)
				return err
			}
		}
	}

	author, err := identityFor(e.repo, e.remote, c.Author, "")
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

func (e *Engine) pullEvent(b *cache.BugCache, ev *Event, out chan<- ImportResult) error {
	snap := b.Snapshot()
	if e.findOp(snap, ev.ID) != nil {
		return nil
	}
	claimed, err := e.claim(b, ev)
	if err != nil || claimed {
		return err
	}

	author, err := identityFor(e.repo, e.remote, ev.Author, "")
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
		labels := applyLabels(snap.Labels, added, removed)
		tags[e.key("labels")] = labelsValue(labels)
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

// claim recognizes an event that git-bug's own push caused, and records it
// on the pushed operation instead of creating a new one.
func (e *Engine) claim(b *cache.BugCache, ev *Event) (bool, error) {
	return false, nil
}

// reconcile records what changed on the forge without an event to replay.
func (e *Engine) reconcile(ctx context.Context, b *cache.BugCache, issue *Issue, out chan<- ImportResult) error {
	return nil
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
