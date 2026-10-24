package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"strings"
	"time"

	"github.com/git-bug/git-bug/entities/common"
)

// ErrNotSupported is returned by a Remote for what its forge can't do, like
// any write on a read-only forge.
var ErrNotSupported = errors.New("not supported by this forge")

// ErrGone is returned when the issue or comment no longer exists on the forge.
var ErrGone = errors.New("deleted on the forge")

// ErrRefused is returned when the forge refuses a write for good, like the
// edit of someone else's comment on GitLab.
var ErrRefused = errors.New("refused by the forge")

// RefusedLabels tells that a label change went through but for these
// labels, added or removed, like assignees the forge can't assign or
// unassign. It is an ErrRefused.
type RefusedLabels []string

func (r RefusedLabels) Error() string {
	return fmt.Sprintf("%s: labels %s", ErrRefused, strings.Join(r, ", "))
}

func (r RefusedLabels) Is(target error) bool { return target == ErrRefused }

// RefusedUsers is returned by Assign when the forge took the change but for
// these users, as given: someone it can't assign (no access, an edition's
// limit, no such user) or can't unassign. It is an ErrRefused.
type RefusedUsers []User

func (r RefusedUsers) Error() string {
	logins := make([]string, len(r))
	for i, u := range r {
		logins[i] = cmp.Or(u.Login, u.ID)
	}
	return fmt.Sprintf("%s: assignees %s", ErrRefused, strings.Join(logins, ", "))
}

func (r RefusedUsers) Is(target error) bool { return target == ErrRefused }

// ErrRateLimited is returned by a Remote when the forge asks to slow down.
type ErrRateLimited struct {
	Until time.Time
}

func (e ErrRateLimited) Error() string {
	return fmt.Sprintf("rate limited until %s", e.Until.Format(time.TimeOnly))
}

// Remote is one forge project, as the sync engine sees it. Adapters implement
// it; every sync rule lives in the engine.
type Remote interface {
	// Key identifies the project, stably across machines and clones,
	// like "gitlab.com/42" or "github.com/git-bug/git-bug".
	Key() string
	// Instance is the server, like "gitlab.com". People are matched per server.
	Instance() string
	// Kind is the forge type, like "github": origin metadata, attribution.
	Kind() string
	// Public tells whether anyone, logged in or not, can read the project.
	Public(ctx context.Context) (bool, error)

	// Accounts are the users the configured tokens act as.
	Accounts(ctx context.Context) ([]Account, error)

	// Issues lists the issues updated since the given time, oldest first.
	Issues(ctx context.Context, since time.Time) iter.Seq2[*Issue, error]
	// Fetch returns one issue as Issues lists them, ErrGone when it no
	// longer exists. The Ref is what git-bug knows of it: its ID, and its
	// URL and Number when recorded.
	Fetch(ctx context.Context, issue Ref) (*Issue, error)
	// User returns a forge user's profile.
	User(ctx context.Context, id string) (User, error)

	// Writes, always as one of the Accounts.
	CreateIssue(ctx context.Context, as Account, title, body string) (Ref, error)
	AddComment(ctx context.Context, as Account, issue Ref, body string) (Ref, error)
	// EditText edits the issue's description (target == issue) or a comment.
	EditText(ctx context.Context, as Account, issue, target Ref, body string) error
	SetStatus(ctx context.Context, as Account, issue Ref, status common.Status) error
	SetTitle(ctx context.Context, as Account, issue Ref, title string) error
	// ChangeLabels adds and removes labels, leaving the issue's others alone.
	// Removing a label the issue doesn't have does nothing.
	ChangeLabels(ctx context.Context, as Account, issue Ref, added, removed []string) error

	// Dialect tells how issue references are written on this forge.
	Dialect() Dialect

	// Legacy returns the remote id an operation carries in the metadata
	// written before the sync engine (github-id, gitlab-id...), if any, from
	// the operation's metadata and its bug's create operation's.
	Legacy(metadata, createMetadata map[string]string) (id string, ok bool)
	// LegacyUser returns the identity metadata key used before the sync
	// engine, and whether it holds the login (true) or the user id (false).
	LegacyUser() (key string, isLogin bool)
}

// Ref is an object on the forge.
type Ref struct {
	ID  string
	URL string
	// Number is, for an issue, what people type after '#'.
	Number string
}

// User is a forge user.
type User struct {
	ID        string
	Login     string
	Name      string
	Email     string
	AvatarURL string
}

// Account is a user one of the configured tokens acts as. The Remote keeps
// the tokens, and picks the one to use from the account's ID.
type Account struct {
	User
}

// Issue is a forge issue: its current state, and the events that led to it.
type Issue struct {
	Ref
	Author  User
	Created time.Time
	Updated time.Time
	// OriginalTitle and OriginalBody are what the issue was created with,
	// equal to Title and Body when the forge doesn't keep that history.
	OriginalTitle string
	Title         string
	OriginalBody  string
	Body          string
	// BodyEditor is who last edited the body, and BodyUpdated when, for
	// forges that tell without an edit event.
	BodyEditor  *User
	BodyUpdated time.Time
	Status      common.Status
	Labels      []string
	// Assignees are the forge's own, of an AssigneeRemote.
	Assignees []User
	Comments  []Comment
	// Events are time ordered.
	Events []Event
	// Resend are the labels the forge holds the other way for its own
	// fields, sent again for it to hold them its way: a kind held as a label
	// where it has a type, or the reverse. The remote sets it.
	Resend []string

	// resend are Resend, and the assignee:<x> labels the forge holds the
	// other way: as labels on a forge holding assignees natively, or the
	// reverse. The engine sets it.
	resend []string
}

// Comment is a comment on a forge issue.
type Comment struct {
	Ref
	Author  User
	Created time.Time
	Updated time.Time
	// Original is the text at creation, equal to Body when unknown.
	Original string
	Body     string
}

type EventKind int

const (
	EventStatus EventKind = iota + 1
	EventTitle
	EventLabels
	EventTextEdit
	EventAssignees
)

// Event is a change on a forge issue.
type Event struct {
	ID     string
	Kind   EventKind
	Author User
	Time   time.Time
	// EventStatus
	Status common.Status
	// EventTitle: the new title
	Title string
	// EventLabels
	Added   []string
	Removed []string
	// EventAssignees
	Assigned   []User
	Unassigned []User
	// EventTextEdit: Target is "" for the issue's body, else a comment ID.
	// Text is the text after the edit.
	Target string
	Text   string
}

// AssigneeRemote is a Remote whose forge has assignees, which hold git-bug's
// assignee:<x> labels unless its bridge says to hold them as labels
// (AssigneesLabels). Its Issues and Fetch give the issues' Assignees, and
// EventAssignees when the forge reports them. The engine translates people
// from one forge to another. Remotes without it hold them as labels.
type AssigneeRemote interface {
	Remote
	// Assign adds and removes assignees, leaving the issue's others alone.
	// It returns RefusedUsers for those the forge didn't take.
	Assign(ctx context.Context, as Account, issue Ref, added, removed []User) error
}

// ErrOutsideDiff is returned by a PullRemote for a line comment it can't put
// on its line: one outside the pull request's diff, or on a commit no longer
// in it.
var ErrOutsideDiff = errors.New("not on a line of the pull request's diff")

// PullRemote is a Remote whose project has pull requests, whose line
// comments, reviews and comments sync with the git-bug comments that the
// review:* metadata anchor (see doc/design/bridges/sync-decisions.md). Only
// GitHub's adapter has them.
type PullRemote interface {
	Remote
	// PullComments lists the line comments, reviews and comments of pull
	// request number pr, oldest first. Reviews without text that only hold
	// line comments are left out.
	PullComments(ctx context.Context, pr string) ([]PullComment, error)
	// AddPullComment posts c on pull request number pr, by its fields: a
	// review when State is set, else a line comment when Path is set (a
	// reply when ReplyTo is set, which then gives the line), else a comment.
	// It returns ErrOutsideDiff for a line comment it can't put on its line.
	AddPullComment(ctx context.Context, as Account, pr string, c PullComment) (Ref, error)
	// ResolveThread resolves, or unresolves, the thread of line comments
	// that starts with the given one.
	ResolveThread(ctx context.Context, as Account, pr string, first Ref, resolved bool) error
}

// PullComment is a comment on a pull request: on a line of its diff, a
// review, or a comment on its conversation.
type PullComment struct {
	Ref
	Author  User
	Created time.Time
	Updated time.Time
	Body    string

	// A line comment: the line of a file, on the old or the new side of the
	// diff, at a commit.
	Path   string
	Line   int
	Side   string // "old" or "new"
	Commit string
	// ReplyTo is, for a reply, the line comment it answers.
	ReplyTo Ref

	// State is, for a review: APPROVE, REQUEST_CHANGES or COMMENT. Commit is
	// the commit reviewed.
	State string
}

// Dialect is how a forge writes issue references.
type Dialect struct {
	// Ref matches a reference to an issue of the same project. Its first
	// group is the reference, replaced when rewriting it, and its second the
	// issue number. Cross-project references must not match.
	Ref *regexp.Regexp
	// FormatRef writes a reference to issue number n of the project.
	FormatRef func(n string) string
}
