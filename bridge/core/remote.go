package core

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"time"

	"github.com/git-bug/git-bug/entities/common"
	"github.com/git-bug/git-bug/entity/dag"
)

// ErrNotSupported is returned by a Remote for what its forge can't do, like
// any write on a read-only forge.
var ErrNotSupported = errors.New("not supported by this forge")

// ErrGone is returned when the issue or comment no longer exists on the forge.
var ErrGone = errors.New("deleted on the forge")

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

	// Accounts are the users the configured tokens act as.
	Accounts(ctx context.Context) ([]Account, error)

	// Issues lists the issues updated since the given time, oldest first.
	Issues(ctx context.Context, since time.Time) iter.Seq2[*Issue, error]
	// User returns a forge user's profile.
	User(ctx context.Context, id string) (User, error)

	// Writes, always as one of the Accounts.
	CreateIssue(ctx context.Context, as Account, title, body string) (Ref, error)
	AddComment(ctx context.Context, as Account, issue Ref, body string) (Ref, error)
	// EditText edits the issue's description (target == issue) or a comment.
	EditText(ctx context.Context, as Account, issue, target Ref, body string) error
	SetStatus(ctx context.Context, as Account, issue Ref, status common.Status) error
	SetTitle(ctx context.Context, as Account, issue Ref, title string) error
	// SetLabels sets the full set of labels.
	SetLabels(ctx context.Context, as Account, issue Ref, labels []string) error

	// Dialect tells how issue references are written on this forge.
	Dialect() Dialect

	// Legacy returns the remote id an operation carries in the metadata
	// written before the sync engine (github-id, gitlab-id...), if any.
	Legacy(op dag.Operation, createMetadata map[string]string) (id string, ok bool)
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
	// BodyEditor is who last edited the body, when the forge tells.
	BodyEditor *User
	Status     common.Status
	Labels     []string
	Comments   []Comment
	// Events are time ordered.
	Events []Event
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
	// EventTextEdit: Target is "" for the issue's body, else a comment ID.
	// Text is the text after the edit.
	Target string
	Text   string
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
