package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entity"
	"github.com/git-bug/git-bug/entity/dag"
	"github.com/git-bug/git-bug/util/text"
)

// Comments on pull requests sync with git-bug comments whose review:*
// metadata anchors them, as git-board writes them: line comments, replies
// and reviews. They never are issue comments, on any forge. See
// doc/design/bridges/sync-decisions.md.
const (
	metaReviewPR       = "review:pr"
	metaReviewPath     = "review:path"
	metaReviewLine     = "review:line"
	metaReviewSide     = "review:side"
	metaReviewCommit   = "review:commit"
	metaReviewReplyTo  = "review:reply-to"
	metaReviewState    = "review:state"
	metaReviewResolved = "review:resolved"
)

// isReview tells whether an operation belongs to a pull request: written in
// git-bug with review:* metadata, or pulled from a pull request.
func isReview(op dag.Operation) bool {
	for k := range op.AllMetadata() {
		if strings.HasPrefix(k, "review:") || isPullOrigin(k) {
			return true
		}
	}
	return false
}

// isPullOrigin tells whether a metadata key records the pull request comment
// an operation was pulled from: "sync:<remote>:pr-comment".
func isPullOrigin(k string) bool {
	return strings.HasPrefix(k, "sync:") && strings.HasSuffix(k, ":pr-comment")
}

// pullNumber returns the number of the pull request of this remote that an
// operation's review:pr names ("github.com/o/r#26").
func (e *Engine) pullNumber(op dag.Operation) (string, bool) {
	v, _ := op.GetMetadata(metaReviewPR)
	i := strings.LastIndex(v, "#")
	if i < 0 || !strings.EqualFold(v[:i], e.remote.Key()) || v[i+1:] == "" {
		return "", false
	}
	return v[i+1:], true
}

// pullID returns the id on this remote's pull request of an operation pulled
// from it, or sent to it.
func (e *Engine) pullID(op dag.Operation) string {
	if id, ok := op.GetMetadata(e.key("pr-comment")); ok {
		return id
	}
	return metadata(op, e.key("id"))
}

// toSend returns the comments written in git-bug for this remote's pull
// requests that it doesn't have yet, by pull request number.
func (e *Engine) toSend(snap *bug.Snapshot) []dag.Operation {
	var res []dag.Operation
	for _, op := range snap.Operations {
		if _, ok := op.(*bug.AddCommentOperation); !ok {
			continue
		}
		if _, ok := e.pullNumber(op); !ok || e.pullID(op) != "" {
			continue
		}
		if slices.ContainsFunc(slices.Collect(maps.Keys(op.AllMetadata())), isPullOrigin) {
			// pulled from another remote's pull request
			continue
		}
		if _, refused := op.GetMetadata(e.key("refused")); refused {
			continue
		}
		res = append(res, op)
	}
	return res
}

// pullAccount returns the account to send a comment of a pull request with:
// accountFor's, except that an approval is only ever its author's.
func (e *Engine) pullAccount(op dag.Operation) (Account, bool, bool) {
	as, onBehalf, ok := e.accountFor(op.Author())
	if state, _ := op.GetMetadata(metaReviewState); state == "APPROVE" && onBehalf {
		return Account{}, false, false
	}
	return as, onBehalf, ok
}

// toPullComment returns what an operation sends to a pull request, its body
// aside.
func (e *Engine) toPullComment(snap *bug.Snapshot, op dag.Operation) PullComment {
	meta := op.AllMetadata()
	c := PullComment{State: meta[metaReviewState], Commit: meta[metaReviewCommit]}
	if c.State != "" {
		return c
	}
	c.Path, c.Side = meta[metaReviewPath], meta[metaReviewSide]
	c.Line, _ = strconv.Atoi(meta[metaReviewLine])
	if parent := replyTarget(snap, meta[metaReviewReplyTo]); parent != nil {
		if id := e.pullID(parent); id != "" {
			c.ReplyTo = Ref{ID: id, URL: metadata(parent, e.key("url"))}
		}
	}
	return c
}

// replyTarget returns the comment operation a review:reply-to names, by its
// comment id (as git-board writes it) or its operation id.
func replyTarget(snap *bug.Snapshot, id string) dag.Operation {
	if id == "" {
		return nil
	}
	for _, op := range snap.Operations {
		if _, ok := op.(*bug.AddCommentOperation); ok &&
			(op.Id().String() == id || string(entity.CombineIds(snap.Id(), op.Id())) == id) {
			return op
		}
	}
	return nil
}

// what names a comment of a pull request as people do.
func (c PullComment) what() string {
	switch {
	case c.State == "APPROVE":
		return "approval"
	case c.State != "":
		return "review"
	case c.Path != "":
		return "line comment"
	}
	return "comment"
}

// anchorText writes out where a line comment was made, for a comment the
// forge wouldn't put on its line.
func anchorText(c PullComment) string {
	commit := c.Commit
	if len(commit) > 7 {
		commit = commit[:7]
	}
	return fmt.Sprintf("On `%s` line %d (%s side) at %s:", c.Path, c.Line, c.Side, commit)
}

// pushPulls sends to this remote's pull requests what was written for them in
// git-bug, then the threads resolved or unresolved since.
func (e *Engine) pushPulls(ctx context.Context, b *cache.BugCache, refs *refIndex, out chan<- ExportResult) error {
	pr, ok := e.remote.(PullRemote)
	if !ok {
		return nil
	}
	for _, op := range e.toSend(b.Snapshot()) {
		if err := e.pushPullComment(ctx, pr, b, op, refs, out); err != nil {
			return err
		}
	}
	return e.pushResolutions(ctx, pr, b, out)
}

func (e *Engine) pushPullComment(ctx context.Context, pr PullRemote, b *cache.BugCache, op dag.Operation, refs *refIndex, out chan<- ExportResult) error {
	// fresh: a reply's comment may have been sent just before
	snap := b.Snapshot()
	number, _ := e.pullNumber(op)
	c := e.toPullComment(snap, op)
	as, onBehalf, ok := e.pullAccount(op)
	if !ok {
		out <- NewExportNothing(b.Id(), fmt.Sprintf("missing author token: %s by %s not sent to pull request #%s", c.what(), op.Author().DisplayName(), number))
		return nil
	}
	latest, message, _ := e.textToPush(snap, op)
	c.Body = e.render(snap, refs, op.Id(), op, latest, message, onBehalf)

	var ref Ref
	err := e.call(ctx, func() (err error) {
		ref, err = pr.AddPullComment(ctx, as, number, c)
		return err
	})
	if errors.Is(err, ErrOutsideDiff) {
		// as a comment of the pull request, its anchor written out
		c = PullComment{Body: e.render(snap, refs, op.Id(), op, latest, anchorText(c)+"\n\n"+message, onBehalf)}
		err = e.call(ctx, func() (err error) {
			ref, err = pr.AddPullComment(ctx, as, number, c)
			return err
		})
	}
	switch {
	case errors.Is(err, ErrRefused):
		if err := e.refused(b, op.Id(), err, out); err != nil {
			return err
		}
		return b.Commit()
	case errors.Is(err, ErrGone):
		out <- NewExportWarning(fmt.Errorf("pull request #%s not found: %w", number, err), b.Id())
		return nil
	case err != nil:
		return err
	}

	records := map[entity.Id]map[string]string{op.Id(): {
		e.key("id"):  ref.ID,
		e.key("url"): ref.URL,
		e.key("as"):  as.Login,
	}}
	addRecord(records, latest.Id(), e.key("text"), Fingerprint(c.Body))
	if err := setRecords(b, records); err != nil {
		return err
	}
	// a crash from now on must not post it again
	if err := b.Commit(); err != nil {
		return err
	}
	out <- NewExportComment(b.Id())
	return nil
}

// resolution is a thread whose resolution git-bug changed since the forge
// last saw it.
type resolution struct {
	first    dag.Operation // the thread's first comment
	number   string        // its pull request
	by       dag.Operation // the SetMetadata operation that changed it
	resolved bool
}

// resolutions returns the threads of this remote's pull requests whose
// review:resolved differs from what the forge last saw: the last
// SetMetadata operation of each wins, as metadata keep their first value.
func (e *Engine) resolutions(snap *bug.Snapshot) []resolution {
	type state struct {
		wanted, seen string
		by           dag.Operation
	}
	states := map[entity.Id]*state{}
	var order []entity.Id
	for _, op := range snap.Operations {
		sm, ok := op.(*dag.SetMetadataOperation[*bug.Snapshot])
		if !ok {
			continue
		}
		st := states[sm.Target]
		if st == nil {
			st = &state{}
			states[sm.Target] = st
			order = append(order, sm.Target)
		}
		if v, ok := sm.NewMetadata[metaReviewResolved]; ok {
			st.wanted, st.by = v, op
		}
		if v, ok := sm.NewMetadata[e.key("resolved")]; ok {
			st.seen = v
		}
	}
	var res []resolution
	for _, target := range order {
		st := states[target]
		wanted, err := strconv.ParseBool(st.wanted)
		if st.by == nil || err != nil {
			continue
		}
		// a thread is open on the forge until resolved there
		seen, _ := strconv.ParseBool(st.seen)
		if wanted == seen {
			continue
		}
		if _, refused := st.by.GetMetadata(e.key("refused")); refused {
			continue
		}
		first := opByID(snap, target)
		if first == nil || e.pullID(first) == "" {
			continue
		}
		number, ok := e.pullNumber(first)
		if !ok {
			continue
		}
		res = append(res, resolution{first: first, number: number, by: st.by, resolved: wanted})
	}
	return res
}

func (e *Engine) pushResolutions(ctx context.Context, pr PullRemote, b *cache.BugCache, out chan<- ExportResult) error {
	for _, r := range e.resolutions(b.Snapshot()) {
		as, _, ok := e.accountFor(r.by.Author())
		if !ok {
			out <- NewExportNothing(b.Id(), fmt.Sprintf("missing author token: resolution by %s not sent to pull request #%s", r.by.Author().DisplayName(), r.number))
			continue
		}
		first := Ref{ID: e.pullID(r.first), URL: metadata(r.first, e.key("url"))}
		err := e.call(ctx, func() error { return pr.ResolveThread(ctx, as, r.number, first, r.resolved) })
		if errors.Is(err, ErrRefused) || errors.Is(err, ErrGone) {
			// a comment the forge took as a comment has no thread
			if err := e.refused(b, r.by.Id(), err, out); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if _, err := b.SetMetadata(r.first.Id(), map[string]string{e.key("resolved"): strconv.FormatBool(r.resolved)}); err != nil {
			return err
		}
		out <- NewExportCommentEdition(b.Id())
	}
	return nil
}

// planPulls reports what pushing a bug would send to this remote's pull
// requests.
func (e *Engine) planPulls(b *cache.BugCache, snap *bug.Snapshot, out chan<- ExportResult) {
	if _, ok := e.remote.(PullRemote); !ok {
		return
	}
	counts := map[string]map[string]int{} // pull request → what → count
	count := func(number, what string) {
		if counts[number] == nil {
			counts[number] = map[string]int{}
		}
		counts[number][what]++
	}
	for _, op := range e.toSend(snap) {
		if _, _, ok := e.pullAccount(op); !ok {
			continue
		}
		number, _ := e.pullNumber(op)
		switch c := e.toPullComment(snap, op); {
		case c.State != "":
			count(number, "reviews")
		case c.Path != "":
			count(number, "line comments")
		default:
			count(number, "comments")
		}
	}
	for _, r := range e.resolutions(snap) {
		count(r.number, "resolutions")
	}
	for _, number := range slices.Sorted(maps.Keys(counts)) {
		var parts []string
		for _, what := range []string{"line comments", "reviews", "comments", "resolutions"} {
			if n := counts[number][what]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, what))
			}
		}
		out <- NewExportNothing(b.Id(), fmt.Sprintf("would send to pull request #%s: %s", number, strings.Join(parts, ", ")))
	}
}

// pullPulls brings in the comments and reviews of a bug's pull requests on
// this remote, as PullsOf gives them.
func (e *Engine) pullPulls(ctx context.Context, b *cache.BugCache, out chan<- ImportResult) {
	pr, ok := e.remote.(PullRemote)
	if !ok || e.opts.PullsOf == nil {
		return
	}
	if _, gone := b.Snapshot().Operations[0].GetMetadata(e.key("gone")); gone {
		return
	}
	for _, number := range e.opts.PullsOf(b.Id()) {
		err := e.pullPull(ctx, pr, b, number, out)
		switch {
		case errors.Is(err, ErrGone):
			out <- NewImportWarning(fmt.Errorf("pull request #%s not found: %w", number, err), b.Id())
		case err != nil:
			out <- NewImportError(fmt.Errorf("pull request #%s: %w", number, err), b.Id())
		}
	}
}

// pullAllPulls is pullPulls for every bug.
func (e *Engine) pullAllPulls(ctx context.Context, out chan<- ImportResult) {
	if _, ok := e.remote.(PullRemote); !ok || e.opts.PullsOf == nil {
		return
	}
	for _, excerpt := range e.excerpts() {
		if ctx.Err() != nil {
			return
		}
		b, err := e.repo.Bugs().Resolve(excerpt.Id())
		if err != nil {
			out <- NewImportError(err, excerpt.Id())
			continue
		}
		e.pullPulls(ctx, b, out)
	}
}

func (e *Engine) pullPull(ctx context.Context, pr PullRemote, b *cache.BugCache, number string, out chan<- ImportResult) error {
	comments, err := pr.PullComments(ctx, number)
	if err != nil {
		return err
	}
	if e.opts.DryRun {
		n := 0
		for _, c := range comments {
			if findPullOp(b.Snapshot(), e, c.ID) == nil {
				n++
			}
		}
		if n > 0 {
			// some may be what a push posted, found by their marker instead
			out <- NewImportNothing(b.Id(), fmt.Sprintf("would pull %d comments from pull request #%s", n, number))
		}
		return nil
	}
	for _, c := range comments {
		if err := e.pullPullComment(b, number, c, out); err != nil {
			return err
		}
	}
	if !b.NeedCommit() {
		return nil
	}
	return b.Commit()
}

// findPullOp returns the operation that is a comment of this remote's pull
// requests, pulled or sent.
func findPullOp(snap *bug.Snapshot, e *Engine, id string) dag.Operation {
	for _, op := range snap.Operations {
		if _, ok := op.(*bug.AddCommentOperation); ok && e.pullID(op) == id {
			return op
		}
	}
	return nil
}

func (e *Engine) pullPullComment(b *cache.BugCache, number string, c PullComment, out chan<- ImportResult) error {
	snap := b.Snapshot()
	if op := findPullOp(snap, e, c.ID); op != nil {
		return e.followPullEdit(b, snap, op, c, out)
	}

	// posted by a push that died before recording it
	if id, ok := MarkerID(text.Cleanup(c.Body)); ok && e.isAccount(c.Author) {
		if op := opByID(snap, id); op != nil && e.pullID(op) == "" {
			records := map[entity.Id]map[string]string{op.Id(): {
				e.key("id"):  c.ID,
				e.key("url"): c.URL,
				e.key("as"):  c.Author.Login,
			}}
			addRecord(records, lastSlotOp(snap, s(slotText, op.Id())).Id(), e.key("text"), Fingerprint(c.Body))
			return setRecords(b, records)
		}
	}

	author, err := e.identityFor(c.Author)
	if err != nil {
		return err
	}
	meta := map[string]string{
		e.key("pr-comment"): c.ID,
		e.key("url"):        c.URL,
		e.key("text"):       Fingerprint(c.Body),
		metaReviewPR:        e.remote.Key() + "#" + number,
	}
	switch {
	case c.State != "":
		meta[metaReviewState], meta[metaReviewCommit] = c.State, c.Commit
	case c.Path != "":
		meta[metaReviewPath], meta[metaReviewLine] = c.Path, strconv.Itoa(c.Line)
		meta[metaReviewSide], meta[metaReviewCommit] = c.Side, c.Commit
		if parent := findPullOp(snap, e, c.ReplyTo.ID); c.ReplyTo.ID != "" && parent != nil {
			meta[metaReviewReplyTo] = string(entity.CombineIds(b.Id(), parent.Id()))
		}
	}
	commentId, _, err := b.AddCommentRaw(author, c.Created.Unix(), Unrender(text.Cleanup(c.Body)), nil, meta)
	if err != nil {
		return err
	}
	out <- NewImportComment(b.Id(), commentId)
	return nil
}

// followPullEdit records an edit made on the forge of a comment of a pull
// request. The forge's wins: git-bug's edits of them aren't sent.
func (e *Engine) followPullEdit(b *cache.BugCache, snap *bug.Snapshot, op dag.Operation, c PullComment, out chan<- ImportResult) error {
	sl := s(slotText, op.Id())
	baseValue, _, ok := base(snap, e.remote, sl)
	fingerprint := Fingerprint(c.Body)
	if ok && baseValue == fingerprint {
		return nil
	}
	comment, err := snap.SearchCommentByOpId(op.Id())
	if err != nil {
		return err
	}
	pulled := Unrender(text.Cleanup(c.Body))
	if pulled == comment.Message {
		_, err := b.SetMetadata(lastSlotOp(snap, sl).Id(), map[string]string{e.key("text"): fingerprint})
		return err
	}
	author, err := e.identityFor(c.Author)
	if err != nil {
		return err
	}
	_, err = b.EditCommentRaw(author, c.Updated.Unix(), comment.CombinedId(), pulled, map[string]string{e.key("text"): fingerprint})
	if err != nil {
		return err
	}
	out <- NewImportCommentEdition(b.Id(), comment.CombinedId())
	return nil
}
