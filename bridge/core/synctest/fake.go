// Package synctest helps testing the sync engine and its adapters: an
// in-memory forge, and the conformance suite every adapter runs.
package synctest

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/entities/common"
)

// Forge is a Remote whose people can also act directly on the forge. The
// conformance suite drives adapters through it.
type Forge interface {
	core.Remote
	// UserAct returns a forge user acting directly on the forge.
	UserAct(login string) Actor
	// Issue returns the forge's current view of an issue, nil if there's none.
	Issue(number string) *core.Issue
	// Writes counts the writes received through the Remote interface.
	Writes() int
}

// Actor is someone acting directly on the forge. Failures panic.
type Actor interface {
	CreateIssue(title, body string) core.Ref
	Comment(issue core.Ref, body string) core.Ref
	EditText(issue, target core.Ref, body string)
	SetStatus(issue core.Ref, status common.Status)
	SetTitle(issue core.Ref, title string)
	SetLabels(issue core.Ref, labels []string)
	Delete(issue core.Ref)
}

// Quirks are behaviors of real forges the fake can reproduce.
type Quirks struct {
	// NoEditEvents: editing a text leaves no event (GitLab).
	NoEditEvents bool
	// AuthorOnlyEdits: only its author may edit a comment (GitLab).
	AuthorOnlyEdits bool
	// FailAfterWrite: the next write is done, then reports an error.
	FailAfterWrite bool
	// RateLimitOnce: the next write is refused with ErrRateLimited.
	RateLimitOnce bool
}

var _ Forge = &Fake{}

// Fake is an in-memory forge with one project.
type Fake struct {
	// Clock gives the time of what happens on the forge.
	Clock func() time.Time

	key, instance string
	accounts      []core.Account

	mu     sync.Mutex
	quirks Quirks
	users  map[string]core.User // by id
	issues []*fakeIssue
	lastID int
	writes int
}

type fakeIssue struct {
	core.Issue
	deleted bool
}

func NewFake(key, instance string, q Quirks, accounts ...core.Account) *Fake {
	f := &Fake{
		Clock:    time.Now,
		key:      key,
		instance: instance,
		accounts: accounts,
		quirks:   q,
		users:    map[string]core.User{},
	}
	for _, a := range accounts {
		f.users[a.ID] = a.User
	}
	return f
}

func (f *Fake) Key() string      { return f.key }
func (f *Fake) Instance() string { return f.instance }
func (f *Fake) Kind() string     { return "fake" }

func (f *Fake) Accounts(context.Context) ([]core.Account, error) {
	return slices.Clone(f.accounts), nil
}

func (f *Fake) Issues(_ context.Context, since time.Time) iter.Seq2[*core.Issue, error] {
	f.mu.Lock()
	var res []*core.Issue
	for _, issue := range f.issues {
		if !issue.deleted && !issue.Updated.Before(since) {
			res = append(res, issue.clone())
		}
	}
	f.mu.Unlock()

	return func(yield func(*core.Issue, error) bool) {
		for _, issue := range res {
			if !yield(issue, nil) {
				return
			}
		}
	}
}

func (f *Fake) User(_ context.Context, id string) (core.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return core.User{}, fmt.Errorf("no user %s", id)
	}
	return u, nil
}

func (f *Fake) CreateIssue(_ context.Context, as core.Account, title, body string) (core.Ref, error) {
	var ref core.Ref
	err := f.write(func() error {
		ref = f.createIssue(as.User, title, body)
		return nil
	})
	return ref, err
}

func (f *Fake) AddComment(_ context.Context, as core.Account, issue core.Ref, body string) (core.Ref, error) {
	var ref core.Ref
	err := f.write(func() (err error) {
		ref, err = f.addComment(as.User, issue, body)
		return err
	})
	return ref, err
}

func (f *Fake) EditText(_ context.Context, as core.Account, issue, target core.Ref, body string) error {
	return f.write(func() error { return f.editText(as.User, issue, target, body) })
}

func (f *Fake) SetStatus(_ context.Context, as core.Account, issue core.Ref, status common.Status) error {
	return f.write(func() error { return f.setStatus(as.User, issue, status) })
}

func (f *Fake) SetTitle(_ context.Context, as core.Account, issue core.Ref, title string) error {
	return f.write(func() error { return f.setTitle(as.User, issue, title) })
}

func (f *Fake) SetLabels(_ context.Context, as core.Account, issue core.Ref, labels []string) error {
	return f.write(func() error { return f.setLabels(as.User, issue, labels) })
}

func (f *Fake) Dialect() core.Dialect {
	return core.Dialect{
		Ref:       regexp.MustCompile(`(?:^|[^\w/#])(#(\d+))\b`),
		FormatRef: func(n string) string { return "#" + n },
	}
}

func (f *Fake) Legacy(_, _ map[string]string) (string, bool) { return "", false }
func (f *Fake) LegacyUser() (string, bool)                   { return "", false }

// UserAct returns someone acting directly on the forge. A token owner's login
// gives their account's user: they also act on the forge themselves.
func (f *Fake) UserAct(login string) Actor {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.Login == login {
			return actor{f: f, u: u}
		}
	}
	u := core.User{ID: "u-" + login, Login: login, Name: login}
	f.users[u.ID] = u
	return actor{f: f, u: u}
}

func (f *Fake) Issue(number string) *core.Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, issue := range f.issues {
		if issue.Number == number && !issue.deleted {
			return issue.clone()
		}
	}
	return nil
}

func (f *Fake) Writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

// write runs an engine write with the quirks that apply to it.
func (f *Fake) write(fn func() error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.quirks.RateLimitOnce {
		f.quirks.RateLimitOnce = false
		return core.ErrRateLimited{Until: time.Now().Add(time.Second)}
	}
	f.writes++
	if err := fn(); err != nil {
		return err
	}
	if f.quirks.FailAfterWrite {
		f.quirks.FailAfterWrite = false
		return errors.New("connection lost after the write")
	}
	return nil
}

// The following are called with f.mu held.

func (f *Fake) now() time.Time {
	return f.Clock().Truncate(time.Second)
}

func (f *Fake) nextID() string {
	f.lastID++
	return strconv.Itoa(f.lastID)
}

func (f *Fake) find(ref core.Ref) (*fakeIssue, error) {
	for _, issue := range f.issues {
		if issue.ID == ref.ID {
			if issue.deleted {
				return nil, core.ErrGone
			}
			return issue, nil
		}
	}
	return nil, core.ErrGone
}

func (f *Fake) event(issue *fakeIssue, e core.Event) {
	e.ID = f.nextID()
	e.Time = f.now()
	issue.Events = append(issue.Events, e)
	issue.Updated = e.Time
}

func (f *Fake) createIssue(u core.User, title, body string) core.Ref {
	number := strconv.Itoa(len(f.issues) + 1)
	ref := core.Ref{
		ID:     f.nextID(),
		Number: number,
		URL:    fmt.Sprintf("https://%s/issues/%s", f.instance, number),
	}
	now := f.now()
	f.issues = append(f.issues, &fakeIssue{Issue: core.Issue{
		Ref:           ref,
		Author:        u,
		Created:       now,
		Updated:       now,
		OriginalTitle: title,
		Title:         title,
		OriginalBody:  body,
		Body:          body,
		Status:        common.OpenStatus,
	}})
	return ref
}

func (f *Fake) addComment(u core.User, ref core.Ref, body string) (core.Ref, error) {
	issue, err := f.find(ref)
	if err != nil {
		return core.Ref{}, err
	}
	id := f.nextID()
	c := core.Comment{
		Ref:      core.Ref{ID: id, URL: issue.URL + "#c" + id},
		Author:   u,
		Created:  f.now(),
		Updated:  f.now(),
		Original: body,
		Body:     body,
	}
	issue.Comments = append(issue.Comments, c)
	issue.Updated = c.Created
	return c.Ref, nil
}

func (f *Fake) editText(u core.User, ref, target core.Ref, body string) error {
	issue, err := f.find(ref)
	if err != nil {
		return err
	}
	e := core.Event{Kind: core.EventTextEdit, Author: u, Text: body}
	if target.ID == issue.ID {
		issue.Body = body
		issue.BodyEditor = &u
	} else {
		i := slices.IndexFunc(issue.Comments, func(c core.Comment) bool { return c.ID == target.ID })
		if i < 0 {
			return core.ErrGone
		}
		if f.quirks.AuthorOnlyEdits && issue.Comments[i].Author.ID != u.ID {
			return errors.New("403 forbidden: only the author can edit a comment")
		}
		issue.Comments[i].Body = body
		issue.Comments[i].Updated = f.now()
		e.Target = target.ID
	}
	if f.quirks.NoEditEvents {
		issue.Updated = f.now()
		return nil
	}
	f.event(issue, e)
	return nil
}

func (f *Fake) setStatus(u core.User, ref core.Ref, status common.Status) error {
	issue, err := f.find(ref)
	if err != nil || issue.Status == status {
		return err
	}
	issue.Status = status
	f.event(issue, core.Event{Kind: core.EventStatus, Author: u, Status: status})
	return nil
}

func (f *Fake) setTitle(u core.User, ref core.Ref, title string) error {
	issue, err := f.find(ref)
	if err != nil || issue.Title == title {
		return err
	}
	issue.Title = title
	f.event(issue, core.Event{Kind: core.EventTitle, Author: u, Title: title})
	return nil
}

func (f *Fake) setLabels(u core.User, ref core.Ref, labels []string) error {
	issue, err := f.find(ref)
	if err != nil {
		return err
	}
	var added, removed []string
	for _, l := range labels {
		if !slices.Contains(issue.Labels, l) {
			added = append(added, l)
		}
	}
	for _, l := range issue.Labels {
		if !slices.Contains(labels, l) {
			removed = append(removed, l)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	issue.Labels = slices.Sorted(slices.Values(labels))
	f.event(issue, core.Event{Kind: core.EventLabels, Author: u, Added: added, Removed: removed})
	return nil
}

func (i *fakeIssue) clone() *core.Issue {
	c := i.Issue
	c.Labels = slices.Clone(i.Labels)
	c.Comments = slices.Clone(i.Comments)
	c.Events = slices.Clone(i.Events)
	return &c
}

type actor struct {
	f *Fake
	u core.User
}

func (a actor) do(fn func() error) {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	if err := fn(); err != nil {
		panic(err)
	}
}

func (a actor) CreateIssue(title, body string) (ref core.Ref) {
	a.do(func() error { ref = a.f.createIssue(a.u, title, body); return nil })
	return ref
}

func (a actor) Comment(issue core.Ref, body string) (ref core.Ref) {
	a.do(func() (err error) { ref, err = a.f.addComment(a.u, issue, body); return err })
	return ref
}

func (a actor) EditText(issue, target core.Ref, body string) {
	a.do(func() error { return a.f.editText(a.u, issue, target, body) })
}

func (a actor) SetStatus(issue core.Ref, status common.Status) {
	a.do(func() error { return a.f.setStatus(a.u, issue, status) })
}

func (a actor) SetTitle(issue core.Ref, title string) {
	a.do(func() error { return a.f.setTitle(a.u, issue, title) })
}

func (a actor) SetLabels(issue core.Ref, labels []string) {
	a.do(func() error { return a.f.setLabels(a.u, issue, labels) })
}

func (a actor) Delete(issue core.Ref) {
	a.do(func() error {
		i, err := a.f.find(issue)
		if err == nil {
			i.deleted = true
		}
		return err
	})
}
