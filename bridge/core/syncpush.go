package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
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
)

// maxRetries is how many times a rate limited write is retried.
const maxRetries = 3

// Push sends to the forge what changed in git-bug since the forge last saw
// it, bug by bug, oldest first.
func (e *Engine) Push(ctx context.Context) <-chan ExportResult {
	out := make(chan ExportResult)
	go func() {
		defer close(out)

		excerpts := e.excerpts()
		refs := newRefIndex(excerpts)

		for _, excerpt := range excerpts {
			if ctx.Err() != nil {
				return
			}
			b, err := e.repo.Bugs().Resolve(excerpt.Id())
			if err != nil {
				out <- NewExportError(err, excerpt.Id())
				continue
			}
			if err := e.pushBug(ctx, b, refs, out); err != nil {
				out <- NewExportError(err, b.Id())
			}
		}
	}()
	return out
}

// PushBug sends one bug to the forge, as Push does every bug.
func (e *Engine) PushBug(ctx context.Context, b *cache.BugCache) <-chan ExportResult {
	out := make(chan ExportResult)
	go func() {
		defer close(out)
		// every bug's issues: references in its texts are translated
		refs := newRefIndex(e.excerpts())
		if err := e.pushBug(ctx, b, refs, out); err != nil {
			out <- NewExportError(err, b.Id())
		}
	}()
	return out
}

// excerpts are every bug's, oldest first.
func (e *Engine) excerpts() []*cache.BugExcerpt {
	var excerpts []*cache.BugExcerpt
	for _, id := range e.repo.Bugs().AllIds() {
		excerpt, err := e.repo.Bugs().ResolveExcerpt(id)
		if err != nil {
			// removed since AllIds, by the cache's update: nothing to push
			continue
		}
		excerpts = append(excerpts, excerpt)
	}
	slices.SortFunc(excerpts, func(a, b *cache.BugExcerpt) int {
		return cmp.Or(cmp.Compare(a.CreateUnixTime, b.CreateUnixTime),
			cmp.Compare(a.CreateLamportTime, b.CreateLamportTime))
	})
	return excerpts
}

// issueRef is a bug's issue on the forge, if it is there.
func (e *Engine) issueRef(snap *bug.Snapshot) (Ref, bool) {
	id, ok := remoteIssueID(snap, e.remote)
	if !ok {
		return Ref{}, false
	}
	create := snap.Operations[0]
	ref := Ref{ID: id, URL: metadata(create, e.key("url")), Number: metadata(create, e.key("number"))}
	if ref.URL == "" {
		// pulled by a pre-sync bridge
		ref.URL = metadata(create, e.remote.Kind()+"-url")
	}
	if ref.Number == "" {
		ref.Number = trailingNumber.FindString(ref.URL)
	}
	return ref, true
}

func (e *Engine) pushBug(ctx context.Context, b *cache.BugCache, refs *refIndex, out chan<- ExportResult) error {
	snap := b.Snapshot()
	createOp := snap.Operations[0]
	if _, gone := createOp.GetMetadata(e.key("gone")); gone {
		return nil
	}

	_, onRemote := remoteIssueID(snap, e.remote)
	if !e.opts.Foreign && !onRemote {
		// without sync, a bug from a forge goes back to that forge only
		if origin, ok := snap.GetCreateMetadata(MetaKeyOrigin); ok {
			out <- NewExportNothing(b.Id(), fmt.Sprintf("issue tagged with origin: %s", origin))
			return nil
		}
	}

	if url, ok := e.attached[b.Id()]; ok && !onRemote {
		out <- NewExportNothing(b.Id(), "would sync with "+url+", found by its git-bug marker")
		return nil
	}
	if e.opts.DryRun {
		e.planPush(b, snap, refs, onRemote, out)
		return nil
	}

	var issue Ref
	if onRemote {
		issue, _ = e.issueRef(snap)
	} else {
		var ok bool
		var err error
		issue, ok, err = e.createIssue(ctx, b, snap, refs, out)
		if err != nil || !ok {
			return err
		}
	}

	err := e.pushChanges(ctx, b, issue, refs, out)
	if errors.Is(err, ErrGone) {
		out <- NewExportWarning(fmt.Errorf("issue deleted on the forge, not synced anymore"), b.Id())
		if _, err := b.SetMetadata(createOp.Id(), map[string]string{e.key("gone"): "1"}); err != nil {
			return err
		}
		return b.CommitAsNeeded()
	}
	if err != nil {
		return err
	}
	if !b.NeedCommit() {
		out <- NewExportNothing(b.Id(), "nothing has been exported")
		return nil
	}
	return b.Commit()
}

// planPush reports what pushing a bug would do, without doing it.
func (e *Engine) planPush(b *cache.BugCache, snap *bug.Snapshot, refs *refIndex, onRemote bool, out chan<- ExportResult) {
	if !onRemote {
		if _, _, ok := e.accountFor(snap.Operations[0].Author()); !ok {
			out <- NewExportNothing(b.Id(), "missing author token")
			return
		}
		comments := 0
		for _, op := range snap.Operations {
			if _, ok := op.(*bug.AddCommentOperation); !ok || isReview(op) {
				continue
			}
			if _, _, ok := e.accountFor(op.Author()); ok {
				comments++
			}
		}
		out <- NewExportNothing(b.Id(), fmt.Sprintf("would create issue: %s, with %d comments", snap.Title, comments))
		e.planPulls(b, snap, out)
		return
	}

	var changes []string
	comments, edits := 0, 0
	for _, op := range snap.Operations {
		switch op.(type) {
		case *bug.CreateOperation, *bug.AddCommentOperation:
		default:
			continue
		}
		if isReview(op) {
			continue
		}
		if _, gone := op.GetMetadata(e.key("gone")); gone {
			continue
		}
		if _, onRemote := opOnRemote(op, snap, e.remote); !onRemote {
			if _, _, ok := e.accountFor(op.Author()); ok {
				comments++
			}
			continue
		}
		sl := s(slotText, op.Id())
		_, baseOp, ok := base(snap, e.remote, sl)
		latest := lastSlotOp(snap, sl)
		if _, refused := latest.GetMetadata(e.key("refused")); !refused && (!ok || latest.Id() != baseOp.Id()) {
			edits++
		}
	}
	if comments > 0 {
		changes = append(changes, fmt.Sprintf("%d comments", comments))
	}
	if edits > 0 {
		changes = append(changes, fmt.Sprintf("%d edits", edits))
	}
	for kind, local := range map[slotKind]string{
		slotTitle:  e.title(snap, refs),
		slotStatus: snap.Status.String(),
	} {
		baseValue, _, ok := base(snap, e.remote, s(kind, ""))
		if !ok || baseValue != local {
			changes = append(changes, string(kind))
		}
	}
	baseLabels, _, _ := base(snap, e.remote, s(slotLabels, ""))
	added, removed := labelChanges(baseLabels, snap.Labels)
	// as the push, refused labels wait for a later change
	if _, refused := lastSlotOp(snap, s(slotLabels, "")).GetMetadata(e.key("refused")); !refused && len(added)+len(removed) > 0 {
		labels := "labels"
		for _, l := range added {
			labels += " +" + l
		}
		for _, l := range removed {
			labels += " -" + l
		}
		changes = append(changes, labels)
	}
	if len(changes) > 0 {
		slices.Sort(changes)
		out <- NewExportNothing(b.Id(), "would send: "+strings.Join(changes, ", "))
	}
	e.planPulls(b, snap, out)
}

// createIssue creates the forge issue of a bug, from its current title and
// body.
func (e *Engine) createIssue(ctx context.Context, b *cache.BugCache, snap *bug.Snapshot, refs *refIndex, out chan<- ExportResult) (Ref, bool, error) {
	createOp := snap.Operations[0]
	as, attributed, ok := e.accountFor(createOp.Author())
	if !ok {
		out <- NewExportNothing(b.Id(), "missing author token")
		return Ref{}, false, nil
	}

	latest, message, _ := e.textToPush(snap, createOp)
	text := e.render(snap, refs, b.Id(), createOp, latest, message, attributed)
	// a reference to the issue itself is unknown until created: pushChanges
	// sets the title again
	title := e.title(snap, refs)

	var issue Ref
	err := e.call(ctx, func() (err error) {
		issue, err = e.remote.CreateIssue(ctx, as, title, text)
		return err
	})
	if err != nil {
		return Ref{}, false, err
	}
	refs.created(b.Id(), e.remote.Key(), issue)

	records := map[entity.Id]map[string]string{
		createOp.Id(): {
			e.key("id"):     issue.ID,
			e.key("number"): issue.Number,
			e.key("url"):    issue.URL,
			e.urlKey():      issue.URL,
			e.key("as"):     as.Login,
			e.key("status"): common.OpenStatus.String(),
			e.key("labels"): "",
		},
	}
	addRecord(records, lastSlotOp(snap, s(slotTitle, "")).Id(), e.key("title"), title)
	addRecord(records, latest.Id(), e.key("text"), Fingerprint(text))
	if err := setRecords(b, records); err != nil {
		return Ref{}, false, err
	}
	// a crash from now on must not create the issue again
	if err := b.Commit(); err != nil {
		return Ref{}, false, err
	}
	out <- NewExportBug(b.Id())
	return issue, true, nil
}

// pushChanges sends what differs from what the forge last saw, for a bug on
// the forge.
func (e *Engine) pushChanges(ctx context.Context, b *cache.BugCache, issue Ref, refs *refIndex, out chan<- ExportResult) error {
	snap := b.Snapshot()
	now := strconv.FormatInt(time.Now().Unix(), 10)

	// comments the forge doesn't have
	for _, op := range snap.Operations {
		addComment, ok := op.(*bug.AddCommentOperation)
		if !ok {
			continue
		}
		if _, onRemote := opOnRemote(op, snap, e.remote); onRemote || isReview(op) {
			continue
		}
		if _, gone := op.GetMetadata(e.key("gone")); gone {
			continue
		}
		as, attributed, ok := e.accountFor(op.Author())
		if !ok {
			continue
		}
		latest, message, _ := e.textToPush(snap, op)
		text := e.render(snap, refs, addComment.Id(), op, latest, message, attributed)
		var ref Ref
		err := e.call(ctx, func() (err error) {
			ref, err = e.remote.AddComment(ctx, as, issue, text)
			return err
		})
		if err != nil {
			return err
		}
		records := map[entity.Id]map[string]string{op.Id(): {
			e.key("id"):  ref.ID,
			e.key("url"): ref.URL,
			e.key("as"):  as.Login,
		}}
		addRecord(records, latest.Id(), e.key("text"), Fingerprint(text))
		if err := setRecords(b, records); err != nil {
			return err
		}
		// a crash from now on must not post the comment again
		if err := b.Commit(); err != nil {
			return err
		}
		out <- NewExportComment(b.Id())
	}

	// texts edited since the forge last saw them
	snap = b.Snapshot()
	for _, owner := range snap.Operations {
		var target Ref
		markerID := owner.Id()
		switch owner.(type) {
		case *bug.CreateOperation:
			target, markerID = issue, b.Id()
		case *bug.AddCommentOperation:
			id, onRemote := opOnRemote(owner, snap, e.remote)
			if !onRemote || isReview(owner) {
				continue
			}
			target = Ref{ID: id, URL: metadata(owner, e.key("url"))}
		default:
			continue
		}
		if _, gone := owner.GetMetadata(e.key("gone")); gone {
			continue
		}
		sl := s(slotText, owner.Id())
		_, baseOp, ok := base(snap, e.remote, sl)
		latest, message, toPush := e.textToPush(snap, owner)
		if !toPush || (ok && latest.Id() == baseOp.Id()) {
			continue
		}
		if _, refused := latest.GetMetadata(e.key("refused")); refused {
			continue
		}
		// edited with the account that posted it: forges like GitLab only let
		// a comment's author edit it
		as, ok := e.accountByLogin(metadata(owner, e.key("as")))
		if !ok {
			if as, _, ok = e.accountFor(latest.Author()); !ok {
				continue
			}
		}
		ownerAccount, hasAccount := e.accounts[owner.Author().Id()]
		onBehalf := !hasAccount || ownerAccount.ID != as.ID

		text := e.render(snap, refs, markerID, owner, latest, message, onBehalf)
		err := e.call(ctx, func() error { return e.remote.EditText(ctx, as, issue, target, text) })
		if errors.Is(err, ErrRefused) {
			if err := e.refused(b, latest.Id(), err, out); err != nil {
				return err
			}
			continue
		}
		if errors.Is(err, ErrGone) && target.ID != issue.ID {
			out <- NewExportWarning(fmt.Errorf("comment deleted on the forge, not synced anymore"), b.Id())
			if _, err := b.SetMetadata(owner.Id(), map[string]string{e.key("gone"): "1"}); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		_, err = b.SetMetadata(latest.Id(), map[string]string{
			e.key("pushed"): now,
			e.key("text"):   Fingerprint(text),
		})
		if err != nil {
			return err
		}
		out <- NewExportCommentEdition(b.Id())
	}

	// title, status and labels that differ from what the forge last saw
	snap = b.Snapshot()
	title := e.title(snap, refs)
	baseLabels, _, _ := base(snap, e.remote, s(slotLabels, ""))
	added, removed := labelChanges(baseLabels, snap.Labels)
	for _, sc := range []struct {
		kind  slotKind
		local string
		write func(Account) error
		event func(entity.Id) ExportResult
	}{
		{slotTitle, title, func(as Account) error {
			return e.remote.SetTitle(ctx, as, issue, title)
		}, NewExportTitleEdition},
		{slotStatus, snap.Status.String(), func(as Account) error {
			return e.remote.SetStatus(ctx, as, issue, snap.Status)
		}, NewExportStatusChange},
		{slotLabels, labelsValue(labelStrings(snap.Labels)), func(as Account) error {
			// the changes only: the forge's own since it was last seen stay
			return e.remote.ChangeLabels(ctx, as, issue, added, removed)
		}, NewExportLabelChange},
	} {
		baseValue, _, ok := base(snap, e.remote, s(sc.kind, ""))
		if ok && baseValue == sc.local {
			continue
		}
		local := lastSlotOp(snap, s(sc.kind, ""))
		if _, refused := local.GetMetadata(e.key("refused")); refused {
			continue
		}
		as, _, ok := e.accountFor(local.Author())
		if !ok {
			continue
		}
		err := e.call(ctx, func() error { return sc.write(as) })
		records := map[string]string{e.key("pushed"): now, e.key(string(sc.kind)): sc.local}
		if refused, ok := errors.AsType[RefusedLabels](err); ok {
			// the others went through: the forge holds these as it did
			out <- NewExportWarning(err, b.Id())
			seen := slices.DeleteFunc(labelStrings(snap.Labels), func(l string) bool { return slices.Contains(refused, l) })
			for _, l := range removed {
				if slices.Contains(refused, l) {
					seen = append(seen, l)
				}
			}
			records[e.key(string(sc.kind))] = labelsValue(seen)
			records[e.key("refused")] = "1"
			err = nil
		}
		if errors.Is(err, ErrRefused) {
			if err := e.refused(b, local.Id(), err, out); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		_, err = b.SetMetadata(local.Id(), records)
		if err != nil {
			return err
		}
		out <- sc.event(b.Id())
	}
	return e.pushPulls(ctx, b, refs, out)
}

// accountFor returns the account to act as the author: theirs, or with sync
// the default one, on their behalf.
func (e *Engine) accountFor(author identity.Interface) (as Account, onBehalf bool, ok bool) {
	if a, ok := e.accounts[author.Id()]; ok {
		return a, false, true
	}
	if e.opts.Foreign && e.defaultAccount != nil {
		return *e.defaultAccount, true, true
	}
	return Account{}, false, false
}

// refused records that the forge refused a value, warns, and doesn't try it
// again: a later change of the value will be tried.
func (e *Engine) refused(b *cache.BugCache, opId entity.Id, err error, out chan<- ExportResult) error {
	out <- NewExportWarning(err, b.Id())
	_, err = b.SetMetadata(opId, map[string]string{e.key("refused"): "1"})
	return err
}

// textToPush returns the version of a text (of a create or add-comment
// operation) to send, and the operation that wrote it: the current one, or
// without sync the last one written by someone with an account, as nothing
// someone without one wrote reaches the forge then.
func (e *Engine) textToPush(snap *bug.Snapshot, owner dag.Operation) (dag.Operation, string, bool) {
	var latest dag.Operation
	var message string
	for _, op := range snap.Operations {
		var text string
		switch op := op.(type) {
		case *bug.CreateOperation:
			if op.Id() != owner.Id() {
				continue
			}
			text = op.Message
		case *bug.AddCommentOperation:
			if op.Id() != owner.Id() {
				continue
			}
			text = op.Message
		case *bug.EditCommentOperation:
			if op.Target != owner.Id() {
				continue
			}
			text = op.Message
		default:
			continue
		}
		if _, ok := e.accounts[op.Author().Id()]; !ok && !e.opts.Foreign {
			continue
		}
		latest, message = op, text
	}
	return latest, message, latest != nil
}

func (e *Engine) accountByLogin(login string) (Account, bool) {
	for _, a := range e.accounts {
		if login != "" && a.Login == login {
			return a, true
		}
	}
	return Account{}, false
}

// render writes the text of owner (a create or add-comment operation), whose
// current version comes from latest, for this remote. markerID is the git-bug
// id of the bug or comment.
func (e *Engine) render(snap *bug.Snapshot, refs *refIndex, markerID entity.Id, owner, latest dag.Operation, message string, onBehalf bool) string {
	var attr *Attribution
	if onBehalf {
		name := owner.Author().Name()
		if name == "" {
			name = owner.Author().Login()
		}
		attr = &Attribution{Name: name, When: owner.Time()}
		if _, kind, url, ok := sourceOf(owner, snap.Operations[0].AllMetadata()); ok {
			attr.Source = displayKind(kind)
			attr.URL = url
		}
	}

	src, ok := e.foreignSource(snap, latest)
	if !ok {
		return Render(message, markerID, attr, nil, nil)
	}
	dialect := remoteInfoFor(src).dialect
	return Render(message, markerID, attr, &dialect, func(n string) string {
		return refs.translate(src, n, e.remote)
	})
}

// title is the bug's title for this remote, its #refs translated as in texts
// when it was written on another forge. Unlike a text, it is rendered again
// when a ref gets known, such as to the issue itself once created.
func (e *Engine) title(snap *bug.Snapshot, refs *refIndex) string {
	src, ok := e.foreignSource(snap, lastSlotOp(snap, s(slotTitle, "")))
	if !ok {
		return snap.Title
	}
	re := remoteInfoFor(src).dialect.Ref
	return mapProse(snap.Title, func(prose string) string {
		return replaceRefs(prose, re, func(n string) string {
			return refs.translateTitle(src, n, e.remote)
		})
	})
}

// foreignSource returns the remote an operation was pulled from, unless it
// is this one, where what it wrote goes back as it is.
func (e *Engine) foreignSource(snap *bug.Snapshot, op dag.Operation) (string, bool) {
	src, _, _, ok := sourceOf(op, snap.Operations[0].AllMetadata())
	if !ok || src == e.remote.Key() {
		return "", false
	}
	// pulled from this project by its pre-sync bridge
	if _, legacy := e.remote.Legacy(op.AllMetadata(), snap.Operations[0].AllMetadata()); legacy {
		return "", false
	}
	return src, true
}

// call runs a write, waiting and retrying when the forge rate limits.
func (e *Engine) call(ctx context.Context, write func() error) error {
	for attempt := 0; ; attempt++ {
		err := write()
		var limited ErrRateLimited
		if !errors.As(err, &limited) || attempt == maxRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(limited.Until)):
		}
	}
}

// refIndex finds bugs by their issue number on remotes, to translate
// references from one forge to another.
type refIndex struct {
	byNumber map[string]map[string]entity.Id // remote key → number → bug
	numbers  map[entity.Id]map[string]string // bug → remote key → number
	urls     map[string]string               // remote key → an issue URL there
}

func newRefIndex(excerpts []*cache.BugExcerpt) *refIndex {
	idx := &refIndex{
		byNumber: map[string]map[string]entity.Id{},
		numbers:  map[entity.Id]map[string]string{},
		urls:     map[string]string{},
	}
	for _, excerpt := range excerpts {
		meta := excerpt.CreateMetadata
		issues := map[string]Ref{} // by remote key
		// on a forge through its pre-sync bridge
		for _, kind := range legacyKinds {
			url := meta[kind+"-url"]
			key, ok := legacyKey(kind, meta)
			if _, legacy := meta[kind+"-id"]; legacy && ok && trailingNumber.MatchString(url) {
				issues[key] = Ref{Number: trailingNumber.FindString(url), URL: url}
			}
		}
		for k, number := range meta {
			if key, ok := remoteKeyOf(k, "number"); ok {
				issues[key] = Ref{Number: number, URL: meta[syncKey(key, "url")]}
			}
		}
		for key, issue := range issues {
			// an issue unlinked or deleted is no longer the bug's
			if meta[syncKey(key, "gone")] == "" {
				idx.add(excerpt.Id(), key, issue)
			}
		}
	}
	return idx
}

func (idx *refIndex) add(bugId entity.Id, key string, issue Ref) {
	if idx.byNumber[key] == nil {
		idx.byNumber[key] = map[string]entity.Id{}
	}
	idx.byNumber[key][issue.Number] = bugId
	if idx.numbers[bugId] == nil {
		idx.numbers[bugId] = map[string]string{}
	}
	idx.numbers[bugId][key] = issue.Number
	if issue.URL != "" {
		idx.urls[key] = issue.URL
	}
}

// created records an issue created during this push.
func (idx *refIndex) created(bugId entity.Id, key string, issue Ref) {
	idx.add(bugId, key, issue)
}

var trailingNumber = regexp.MustCompile(`\d+$`)

// translate rewrites a reference to issue number n of remote src for remote
// to: to its number there, or else a link to it on src.
func (idx *refIndex) translate(src, n string, to Remote) string {
	if ref, ok := idx.onRemote(src, n, to); ok {
		return ref
	}
	if url, ok := idx.urls[src]; ok && trailingNumber.MatchString(url) {
		return fmt.Sprintf("[#%s](%s)", n, trailingNumber.ReplaceAllString(url, n))
	}
	// not a link to the wrong issue, at least
	return "`#" + n + "`"
}

// translateTitle is translate for a title, which shows no links.
func (idx *refIndex) translateTitle(src, n string, to Remote) string {
	if ref, ok := idx.onRemote(src, n, to); ok {
		return ref
	}
	return "`#" + n + "`"
}

// onRemote returns the reference on remote to of the bug that is issue
// number n of remote src, if it is on both.
func (idx *refIndex) onRemote(src, n string, to Remote) (string, bool) {
	if number, ok := idx.numbers[idx.byNumber[src][n]][to.Key()]; ok {
		return to.Dialect().FormatRef(number), true
	}
	return "", false
}

// remoteKeyOf returns the remote key of a "sync:<key>:<name>" metadata key.
func remoteKeyOf(metaKey, name string) (string, bool) {
	const prefix = "sync:"
	suffix := ":" + name
	if len(metaKey) <= len(prefix)+len(suffix) || metaKey[:len(prefix)] != prefix || metaKey[len(metaKey)-len(suffix):] != suffix {
		return "", false
	}
	return metaKey[len(prefix) : len(metaKey)-len(suffix)], true
}

func s(kind slotKind, target entity.Id) slot {
	return slot{kind: kind, target: target}
}

func metadata(op dag.Operation, key string) string {
	v, _ := op.GetMetadata(key)
	return v
}

func addRecord(records map[entity.Id]map[string]string, target entity.Id, k, v string) {
	if records[target] == nil {
		records[target] = map[string]string{}
	}
	records[target][k] = v
}

func setRecords(b *cache.BugCache, records map[entity.Id]map[string]string) error {
	for target, meta := range records {
		if _, err := b.SetMetadata(target, meta); err != nil {
			return err
		}
	}
	return nil
}

// labelChanges returns the labels to add to and remove from base, a label
// set as recorded, to have labels.
func labelChanges(base string, labels []common.Label) (added, removed []string) {
	was := splitLabels(base)
	for _, l := range labels {
		if !slices.Contains(was, l) {
			added = append(added, l.String())
		}
	}
	for _, l := range was {
		if !slices.Contains(labels, l) {
			removed = append(removed, l.String())
		}
	}
	return added, removed
}

func labelStrings(labels []common.Label) []string {
	res := make([]string, len(labels))
	for i, l := range labels {
		res[i] = l.String()
	}
	return res
}
