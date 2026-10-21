package gitlab

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitlab.com/gitlab-org/api/client-go"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/bridge/core/auth"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/common"
)

var _ core.RemoteBridge = &Gitlab{}

// NewRemote returns the GitLab project of the bridge, for the sync engine,
// acting with every GitLab token of the instance.
func (g *Gitlab) NewRemote(ctx context.Context, repo *cache.RepoCache, conf core.Configuration) (core.Remote, error) {
	creds, err := instanceTokens(repo, conf[confKeyGitlabBaseUrl])
	if err != nil {
		return nil, err
	}
	tokens := make([]*auth.Token, 0, len(creds))
	for _, cred := range creds {
		tokens = append(tokens, cred.(*auth.Token))
	}
	r, err := newRemote(ctx, conf[confKeyGitlabBaseUrl], conf[confKeyProjectID], conf[confKeyDefaultLogin], tokens)
	if err != nil {
		return nil, err
	}
	r.sprints = conf[core.ConfigKeySprints] != core.ModeOff
	return r, nil
}

var _ core.AssigneeRemote = &remote{}

// remote is a GitLab project, as the sync engine sees it.
type remote struct {
	baseURL   string
	projectID string
	instance  string

	accounts []core.Account
	// writers act as an account, by account id. They don't retry: GitLab
	// may have done a write whose answer was lost, and the engine recovers
	// from that without writing twice.
	writers map[string]*gitlab.Client
	// reader lists the project, with the default login's token if any.
	reader *gitlab.Client

	// sprints: milestones are sprint labels
	sprints bool
	mu      sync.Mutex
	// milestones are the active ones' ids, by sprint label
	milestones map[string]int64
	// milestonesRead tells readMilestones ran
	milestonesRead bool
}

func newRemote(ctx context.Context, baseURL, projectID, defaultLogin string, tokens []*auth.Token) (*remote, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	r := &remote{
		baseURL:   baseURL,
		projectID: projectID,
		instance:  u.Host,
		writers:   map[string]*gitlab.Client{},
		sprints:   true,
	}
	for _, token := range tokens {
		reader, err := buildClient(baseURL, token)
		if err != nil {
			return nil, err
		}
		user, _, err := reader.Users.CurrentUser(gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("token %s: %w", token.ID().Human(), err)
		}
		id := strconv.FormatInt(user.ID, 10)
		if _, ok := r.writers[id]; ok {
			continue
		}
		writer, err := gitlab.NewClient(token.Value, gitlab.WithBaseURL(baseURL), gitlab.WithoutRetries())
		if err != nil {
			return nil, err
		}
		r.writers[id] = writer
		r.accounts = append(r.accounts, core.Account{User: core.User{
			ID:        id,
			Login:     user.Username,
			Name:      user.Name,
			Email:     user.PublicEmail,
			AvatarURL: user.AvatarURL,
		}})
		if r.reader == nil || user.Username == defaultLogin {
			r.reader = reader
		}
	}
	if r.reader == nil {
		return nil, ErrMissingIdentityToken
	}
	return r, nil
}

func (r *remote) Key() string      { return r.instance + "/" + r.projectID }
func (r *remote) Instance() string { return r.instance }
func (r *remote) Kind() string     { return target }

func (r *remote) Accounts(context.Context) ([]core.Account, error) {
	return slices.Clone(r.accounts), nil
}

func (r *remote) Issues(ctx context.Context, since time.Time) iter.Seq2[*core.Issue, error] {
	return func(yield func(*core.Issue, error) bool) {
		if err := r.readMilestones(ctx); err != nil {
			yield(nil, err)
			return
		}
		// stops the listing if the caller stops early
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		if !since.IsZero() {
			since = since.Add(-updatedMargin)
		}
		for res := range Issues(ctx, r.reader, r.projectID, since) {
			if res.Err != nil {
				yield(nil, res.Err)
				return
			}
			issue, err := r.convert(ctx, res.Issue)
			if !yield(issue, err) || err != nil {
				return
			}
		}
	}
}

// updatedMargin is how much earlier than asked issues are listed: GitLab
// moves an issue's updated_at at most about once a minute for label and
// assignee changes (gitlab.epfl.ch 19.4, checked 2026-10-21), so one
// changed just after a sync can be dated before it.
const updatedMargin = 2 * time.Minute

func (r *remote) Fetch(ctx context.Context, ref core.Ref) (*core.Issue, error) {
	// an issue's ID is its number
	iid, err := strconv.ParseInt(cmp.Or(ref.ID, ref.Number), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("issue %q: %w", cmp.Or(ref.ID, ref.Number), err)
	}
	if err := r.readMilestones(ctx); err != nil {
		return nil, err
	}
	gi, _, err := r.reader.Issues.GetIssue(r.projectID, iid, gitlab.WithContext(ctx))
	if err != nil {
		return nil, mapErr(err)
	}
	return r.convert(ctx, gi)
}

// convert returns a GitLab issue, its notes and events, as the engine wants
// it. GitLab keeps no history of titles and texts: the original ones are the
// current ones, and text edits come without event.
func (r *remote) convert(ctx context.Context, gi *gitlab.Issue) (*core.Issue, error) {
	status := common.OpenStatus
	if gi.State == "closed" {
		status = common.ClosedStatus
	}
	iid := strconv.FormatInt(gi.IID, 10)
	issue := &core.Issue{
		Ref:           core.Ref{ID: iid, Number: iid, URL: gi.WebURL},
		Author:        issueUser(gi.Author),
		Created:       *gi.CreatedAt,
		Updated:       *gi.UpdatedAt,
		OriginalTitle: gi.Title,
		Title:         gi.Title,
		OriginalBody:  gi.Description,
		Body:          gi.Description,
		Status:        status,
		Labels:        slices.Clone(gi.Labels),
	}
	for _, a := range gi.Assignees {
		issue.Assignees = append(issue.Assignees, core.User{ID: strconv.FormatInt(a.ID, 10), Login: a.Username, Name: a.Name})
	}
	if r.sprints {
		if gi.Milestone != nil {
			if l := sprintLabel(gi.Milestone.Title); !slices.Contains(issue.Labels, l) {
				issue.Labels = append(issue.Labels, l)
			}
		}
		// a sprint label GitLab holds as a label, sent again as the milestone
		// when the issue has none or that one's
		for _, l := range gi.Labels {
			if _, ok := r.milestoneID(l); ok && (gi.Milestone == nil || sprintLabel(gi.Milestone.Title) == l) {
				issue.Resend = append(issue.Resend, l)
			}
		}
	}

	for e := range Notes(ctx, r.reader, gi) {
		switch e := e.(type) {
		case ErrorEvent:
			return nil, e.Err
		case NoteEvent:
			author := core.User{ID: strconv.FormatInt(e.Author.ID, 10), Login: e.Author.Username, Name: e.Author.Name}
			if !e.System {
				issue.Comments = append(issue.Comments, core.Comment{
					Ref:      core.Ref{ID: e.ID(), URL: fmt.Sprintf("%s#note_%s", gi.WebURL, e.ID())},
					Author:   author,
					Created:  *e.Note.CreatedAt,
					Updated:  *e.Note.UpdatedAt,
					Original: e.Body,
					Body:     e.Body,
				})
				continue
			}
			// the only trace of a description edit: when, and by whom
			if e.Kind() == EventDescriptionChanged && e.CreatedAt().After(issue.BodyUpdated) {
				issue.BodyUpdated = e.CreatedAt()
				issue.BodyEditor = &author
				continue
			}
			if e.Kind() == EventTitleChanged {
				title, err := e.Title()
				if err != nil {
					continue
				}
				issue.Events = append(issue.Events, core.Event{
					ID: e.ID(), Kind: core.EventTitle, Author: author, Time: e.CreatedAt(), Title: title,
				})
			}
		}
	}

	// label and milestone events, netted as the engine sees labels: GitLab's
	// and the milestone's, so a label moved to the milestone is no change
	type relabel struct {
		ev     core.Event
		change func()
	}
	var changes []relabel
	var labels []string
	held := ""
	if r.sprints {
		for e := range MilestoneEvents(ctx, r.reader, gi) {
			switch e := e.(type) {
			case ErrorEvent:
				return nil, e.Err
			case MilestoneEvent:
				// a deleted milestone leaves no title
				if e.Milestone == nil {
					continue
				}
				l := sprintLabel(e.Milestone.Title)
				ev := core.Event{ID: e.ID(), Kind: core.EventLabels, Author: basicUser(e.User), Time: e.CreatedAt()}
				switch e.Kind() {
				case EventChangedMilestone:
					// a change gives an add of the new milestone only
					changes = append(changes, relabel{ev, func() { held = l }})
				case EventRemovedMilestone:
					changes = append(changes, relabel{ev, func() { held = "" }})
				}
			}
		}
	}
	for e := range LabelEvents(ctx, r.reader, gi) {
		switch e := e.(type) {
		case ErrorEvent:
			return nil, e.Err
		case LabelEvent:
			l := e.Label.Name
			ev := core.Event{ID: e.ID(), Kind: core.EventLabels, Author: basicUser(&e.User), Time: e.CreatedAt()}
			switch e.Kind() {
			case EventAddLabel:
				changes = append(changes, relabel{ev, func() {
					if !slices.Contains(labels, l) {
						labels = append(labels, l)
					}
				}})
			case EventRemoveLabel:
				changes = append(changes, relabel{ev, func() {
					labels = slices.DeleteFunc(labels, func(x string) bool { return x == l })
				}})
			}
		}
	}
	// ponytail: on a tie the milestone's go first, as a moved label comes off
	// after it; a milestone cleared and its label added at once show as a
	// removal and an add
	slices.SortStableFunc(changes, func(a, b relabel) int { return a.ev.Time.Compare(b.ev.Time) })
	union := func() []string {
		if held == "" || slices.Contains(labels, held) {
			return slices.Clone(labels)
		}
		return append(slices.Clone(labels), held)
	}
	for _, c := range changes {
		was := union()
		c.change()
		now := union()
		for _, l := range now {
			if !slices.Contains(was, l) {
				c.ev.Added = append(c.ev.Added, l)
			}
		}
		for _, l := range was {
			if !slices.Contains(now, l) {
				c.ev.Removed = append(c.ev.Removed, l)
			}
		}
		if c.ev.Added != nil || c.ev.Removed != nil {
			issue.Events = append(issue.Events, c.ev)
		}
	}

	for e := range StateEvents(ctx, r.reader, gi) {
		switch e := e.(type) {
		case ErrorEvent:
			return nil, e.Err
		case StateEvent:
			ev := core.Event{ID: e.ID(), Kind: core.EventStatus, Author: basicUser(e.User), Time: e.CreatedAt()}
			switch e.Kind() {
			case EventClosed:
				ev.Status = common.ClosedStatus
			case EventReopened:
				ev.Status = common.OpenStatus
			default:
				continue
			}
			issue.Events = append(issue.Events, ev)
		}
	}

	slices.SortStableFunc(issue.Events, func(a, b core.Event) int { return a.Time.Compare(b.Time) })
	return issue, nil
}

// sprintLabel is the label of a milestone: sprint:<slug of its title>.
func sprintLabel(title string) string {
	return "sprint:" + core.Slug(title)
}

// readMilestones reads the active milestones of the project and of its
// groups, before a pull, as ones may have been made since the last. A
// project's wins over a group's of the same label.
func (r *remote) readMilestones(ctx context.Context) error {
	if !r.sprints {
		return nil
	}
	milestones := map[string]int64{}
	fromProject := map[string]bool{}
	opts := gitlab.ListMilestonesOptions{
		State:            gitlab.Ptr("active"),
		IncludeAncestors: gitlab.Ptr(true),
		ListOptions:      gitlab.ListOptions{PerPage: 100},
	}
	for {
		list, resp, err := r.reader.Milestones.ListMilestones(r.projectID, &opts, gitlab.WithContext(ctx))
		if err != nil {
			return mapErr(err)
		}
		for _, m := range list {
			l := sprintLabel(m.Title)
			_, seen := milestones[l]
			if !seen || (m.ProjectID != 0 && !fromProject[l]) {
				milestones[l] = m.ID
				fromProject[l] = m.ProjectID != 0
			}
		}
		if resp.CurrentPage >= resp.TotalPages {
			break
		}
		opts.Page = resp.NextPage
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.milestones, r.milestonesRead = milestones, true
	return nil
}

// milestoneID returns the id of the active milestone a label stands for,
// matched exactly: "sprint:Sprint 2" is no milestone's.
func (r *remote) milestoneID(label string) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.milestones[label]
	return id, ok
}

func (r *remote) User(ctx context.Context, id string) (core.User, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return core.User{}, err
	}
	u, _, err := r.reader.Users.GetUser(n, gitlab.GetUsersOptions{}, gitlab.WithContext(ctx))
	if err != nil {
		return core.User{}, err
	}
	return core.User{ID: id, Login: u.Username, Name: u.Name, Email: u.PublicEmail, AvatarURL: u.AvatarURL}, nil
}

func (r *remote) CreateIssue(ctx context.Context, as core.Account, title, body string) (core.Ref, error) {
	c, err := r.writer(as)
	if err != nil {
		return core.Ref{}, err
	}
	issue, _, err := c.Issues.CreateIssue(r.projectID, &gitlab.CreateIssueOptions{
		Title:       &title,
		Description: &body,
	}, gitlab.WithContext(ctx))
	if err != nil {
		return core.Ref{}, mapErr(err)
	}
	iid := strconv.FormatInt(issue.IID, 10)
	return core.Ref{ID: iid, Number: iid, URL: issue.WebURL}, nil
}

func (r *remote) AddComment(ctx context.Context, as core.Account, issue core.Ref, body string) (core.Ref, error) {
	c, iid, err := r.writerOn(as, issue)
	if err != nil {
		return core.Ref{}, err
	}
	note, _, err := c.Notes.CreateIssueNote(r.projectID, iid, &gitlab.CreateIssueNoteOptions{Body: &body}, gitlab.WithContext(ctx))
	if err != nil {
		return core.Ref{}, r.confirm(ctx, mapErr(err), iid, 0)
	}
	id := strconv.FormatInt(note.ID, 10)
	return core.Ref{ID: id, URL: fmt.Sprintf("%s#note_%s", issue.URL, id)}, nil
}

func (r *remote) EditText(ctx context.Context, as core.Account, issue, target core.Ref, body string) error {
	if target.ID == issue.ID {
		return r.update(ctx, as, issue, &gitlab.UpdateIssueOptions{Description: &body})
	}
	c, iid, err := r.writerOn(as, issue)
	if err != nil {
		return err
	}
	noteID, err := strconv.ParseInt(target.ID, 10, 64)
	if err != nil {
		return err
	}
	_, _, err = c.Notes.UpdateIssueNote(r.projectID, iid, noteID, &gitlab.UpdateIssueNoteOptions{Body: &body}, gitlab.WithContext(ctx))
	return r.confirm(ctx, mapErr(err), iid, noteID)
}

func (r *remote) SetStatus(ctx context.Context, as core.Account, issue core.Ref, status common.Status) error {
	state := "reopen"
	if status == common.ClosedStatus {
		state = "close"
	}
	return r.update(ctx, as, issue, &gitlab.UpdateIssueOptions{StateEvent: &state})
}

func (r *remote) SetTitle(ctx context.Context, as core.Account, issue core.Ref, title string) error {
	return r.update(ctx, as, issue, &gitlab.UpdateIssueOptions{Title: &title})
}

// ChangeLabels adds and removes labels in one update; GitLab creates the
// missing ones it adds. A sprint with a milestone is set as the issue's
// milestone in the same update: the issue has one, another sprint is a
// label, and the milestone's label held the old way comes off, in a second
// update once the milestone holds it. A removed sprint clears its milestone
// even closed. GitLab silently ignores a milestone it can't assign: that
// sprint is refused, and its label left.
func (r *remote) ChangeLabels(ctx context.Context, as core.Account, issue core.Ref, added, removed []string) error {
	c, iid, err := r.writerOn(as, issue)
	if err != nil {
		return err
	}
	held := func(gi *gitlab.Issue) string {
		if gi.Milestone == nil {
			return ""
		}
		return sprintLabel(gi.Milestone.Title)
	}
	var opt gitlab.UpdateIssueOptions
	current, next := "", ""
	// later are the removals sent after the milestone
	var later []string
	if r.sprints {
		r.mu.Lock()
		read := r.milestonesRead
		r.mu.Unlock()
		if !read {
			if err := r.readMilestones(ctx); err != nil {
				return err
			}
		}
		hasMilestone := func(l string) bool { _, ok := r.milestoneID(l); return ok }
		// ponytail: a closed milestone isn't known, so any removed sprint
		// reads the issue, one request each such push
		isSprint := func(l string) bool { return strings.HasPrefix(l, "sprint:") }
		if slices.ContainsFunc(added, hasMilestone) || slices.ContainsFunc(removed, isSprint) {
			gi, _, err := r.reader.Issues.GetIssue(r.projectID, iid, gitlab.WithContext(ctx))
			if err != nil {
				return r.confirm(ctx, mapErr(err), iid, 0)
			}
			current = held(gi)
			next, added, removed = core.OneNative(current, added, removed, hasMilestone)
			// the old-way label comes off once the milestone holds it: no
			// pull sees the sprint gone
			if next != current && slices.Contains(gi.Labels, next) {
				later, removed = removed, nil
			}
		}
	}
	switch {
	case next == current:
	case next == "":
		opt.ResetMilestoneID = true
	default:
		id, _ := r.milestoneID(next)
		opt.MilestoneID = &id
	}
	if len(added) > 0 {
		opt.AddLabels = (*gitlab.LabelOptions)(&added)
	}
	if len(removed) > 0 {
		opt.RemoveLabels = (*gitlab.LabelOptions)(&removed)
	}
	gi, _, err := c.Issues.UpdateIssue(r.projectID, iid, &opt, gitlab.WithContext(ctx))
	if err := r.confirm(ctx, mapErr(err), iid, 0); err != nil {
		return err
	}
	var refused core.RefusedLabels
	if next != current && held(gi) != next {
		// a replace is a set and a clear: both sprints are refused
		refused = slices.DeleteFunc([]string{next, current}, func(s string) bool { return s == "" })
	}
	if later = slices.DeleteFunc(later, func(l string) bool { return slices.Contains(refused, l) }); len(later) > 0 {
		opt := gitlab.UpdateIssueOptions{RemoveLabels: (*gitlab.LabelOptions)(&later)}
		_, _, err := c.Issues.UpdateIssue(r.projectID, iid, &opt, gitlab.WithContext(ctx))
		if err := r.confirm(ctx, mapErr(err), iid, 0); err != nil {
			return err
		}
	}
	if refused != nil {
		return refused
	}
	return nil
}

// Assign writes the issue's assignees whole, as GitLab's API takes them,
// then reads them back: the free edition keeps the first id of a user, and
// drops ids of no user. A login without a user is refused.
// ponytail: an assignee set on GitLab between the read and the write is
// lost; GitLab has no add or remove for them
func (r *remote) Assign(ctx context.Context, as core.Account, issue core.Ref, added, removed []core.User) error {
	c, iid, err := r.writerOn(as, issue)
	if err != nil {
		return err
	}
	gi, _, err := r.reader.Issues.GetIssue(r.projectID, iid, gitlab.WithContext(ctx))
	if err != nil {
		return mapErr(err)
	}
	ids := []int64{}
	for _, a := range gi.Assignees {
		ids = append(ids, a.ID)
	}
	was := slices.Clone(ids)

	var refused core.RefusedUsers
	idOf := map[core.User]int64{}
	for _, u := range slices.Concat(removed, added) {
		id, ok, err := r.userID(ctx, u)
		if err != nil {
			return err
		}
		if ok {
			idOf[u] = id
		}
	}
	for _, u := range removed {
		if id, ok := idOf[u]; ok {
			ids = slices.DeleteFunc(ids, func(a int64) bool { return a == id })
		}
	}
	for _, u := range added {
		id, ok := idOf[u]
		switch {
		case !ok:
			refused = append(refused, u)
		case !slices.Contains(ids, id):
			ids = append(ids, id)
		}
	}

	after := was
	if !slices.Equal(ids, was) {
		gi, _, err := c.Issues.UpdateIssue(r.projectID, iid, &gitlab.UpdateIssueOptions{AssigneeIDs: &ids}, gitlab.WithContext(ctx))
		if err != nil {
			return r.confirm(ctx, mapErr(err), iid, 0)
		}
		after = nil
		for _, a := range gi.Assignees {
			after = append(after, a.ID)
		}
	}
	for _, u := range slices.Concat(added, removed) {
		id, ok := idOf[u]
		if ok && slices.Contains(after, id) != slices.Contains(added, u) {
			refused = append(refused, u)
		}
	}
	if refused != nil {
		return refused
	}
	return nil
}

// userID returns a user's id, found by login when not known.
func (r *remote) userID(ctx context.Context, u core.User) (int64, bool, error) {
	if u.ID != "" {
		id, err := strconv.ParseInt(u.ID, 10, 64)
		return id, err == nil, nil
	}
	found, _, err := r.reader.Users.ListUsers(&gitlab.ListUsersOptions{Username: &u.Login}, gitlab.WithContext(ctx))
	if err != nil || len(found) == 0 {
		return 0, false, mapErr(err)
	}
	return found[0].ID, true, nil
}

func (r *remote) update(ctx context.Context, as core.Account, issue core.Ref, opt *gitlab.UpdateIssueOptions) error {
	c, iid, err := r.writerOn(as, issue)
	if err != nil {
		return err
	}
	_, _, err = c.Issues.UpdateIssue(r.projectID, iid, opt, gitlab.WithContext(ctx))
	return r.confirm(ctx, mapErr(err), iid, 0)
}

// confirm checks with the reader that an issue (and a note, if not 0) a
// write didn't find is gone: GitLab answers the same 404 to a token that
// can't see the project. The issue still there, the write was refused.
func (r *remote) confirm(ctx context.Context, err error, iid, noteID int64) error {
	if !errors.Is(err, core.ErrGone) {
		return err
	}
	if _, _, perr := r.reader.Projects.GetProject(r.projectID, nil, gitlab.WithContext(ctx)); perr != nil {
		return fmt.Errorf("checking the project after %w: %w", err, perr)
	}
	_, _, gerr := r.reader.Issues.GetIssue(r.projectID, iid, gitlab.WithContext(ctx))
	if gerr == nil && noteID != 0 {
		_, _, gerr = r.reader.Notes.GetIssueNote(r.projectID, iid, noteID, gitlab.WithContext(ctx))
	}
	switch {
	case errors.Is(gerr, gitlab.ErrNotFound):
		return err
	case gerr != nil:
		return gerr
	}
	return fmt.Errorf("%w: the token can't see it: %w", core.ErrRefused, err)
}

func (r *remote) writer(as core.Account) (*gitlab.Client, error) {
	c, ok := r.writers[as.ID]
	if !ok {
		return nil, fmt.Errorf("no GitLab token for %s", as.Login)
	}
	return c, nil
}

func (r *remote) writerOn(as core.Account, issue core.Ref) (*gitlab.Client, int64, error) {
	c, err := r.writer(as)
	if err != nil {
		return nil, 0, err
	}
	iid, err := strconv.ParseInt(issue.ID, 10, 64)
	return c, iid, err
}

func (r *remote) Dialect() core.Dialect {
	return core.Dialect{
		// "!12" is a merge request and "&12" an epic: only "#12" is an issue
		Ref:       regexp.MustCompile(`(?:^|[^\w/#!&])(#(\d+))\b`),
		FormatRef: func(n string) string { return "#" + n },
	}
}

// Legacy reads the metadata of the GitLab bridge before the sync engine: a
// gitlab-id on operations of a bug of this project.
func (r *remote) Legacy(meta, createMeta map[string]string) (string, bool) {
	id, ok := meta[metaKeyGitlabId]
	if !ok || createMeta[metaKeyGitlabProject] != r.projectID {
		return "", false
	}
	// older versions exported with an empty base-url
	if baseURL := createMeta[metaKeyGitlabBaseUrl]; baseURL != "" && !sameBaseURL(baseURL, r.baseURL) {
		return "", false
	}
	return id, true
}

func (r *remote) LegacyUser() (string, bool) {
	return metaKeyGitlabId, false
}

// mapErr turns GitLab's answers into the engine's errors.
func mapErr(err error) error {
	if errors.Is(err, gitlab.ErrNotFound) {
		return fmt.Errorf("%w: %w", core.ErrGone, err)
	}
	var resp *gitlab.ErrorResponse
	if !errors.As(err, &resp) || resp.Response == nil {
		return err
	}
	switch resp.Response.StatusCode {
	case http.StatusForbidden:
		return fmt.Errorf("%w: %w", core.ErrRefused, err)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %w", core.ErrGone, err)
	case http.StatusTooManyRequests:
		wait := time.Minute
		if s, err := strconv.Atoi(resp.Response.Header.Get("Retry-After")); err == nil {
			wait = time.Duration(s) * time.Second
		}
		return core.ErrRateLimited{Until: time.Now().Add(wait)}
	}
	return err
}

func issueUser(a *gitlab.IssueAuthor) core.User {
	if a == nil {
		return core.User{}
	}
	return core.User{ID: strconv.FormatInt(a.ID, 10), Login: a.Username, Name: a.Name, AvatarURL: a.AvatarURL}
}

func basicUser(u *gitlab.BasicUser) core.User {
	if u == nil {
		return core.User{}
	}
	return core.User{ID: strconv.FormatInt(u.ID, 10), Login: u.Username, Name: u.Name}
}
