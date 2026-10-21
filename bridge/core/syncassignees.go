package core

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entity"
)

// An assignee:<name> label names a git-bug person, by their name: on an
// AssigneeRemote, it is the forge's assignee of that person's account there,
// unless the forge holds assignees as labels (AssigneesLabels). See
// doc/design/bridges/sync-decisions.md.

// assigneePrefix prefixes an assignee's label.
const assigneePrefix = "assignee:"

// assigneeName returns the name of an assignee's label, its prefix in any
// case. One without a name is a label.
func assigneeName(label string) (string, bool) {
	if len(label) <= len(assigneePrefix) || !strings.EqualFold(label[:len(assigneePrefix)], assigneePrefix) {
		return "", false
	}
	return label[len(assigneePrefix):], true
}

// splitAssignees returns the assignees' labels, and the other labels.
func splitAssignees(labels []string) (assignees, others []string) {
	for _, l := range labels {
		if _, ok := assigneeName(l); ok {
			assignees = append(assignees, l)
		} else {
			others = append(others, l)
		}
	}
	return assignees, others
}

// loginTags are the logins `bridge new` tags its token owners with, which
// name a person without a login of their own, in git-board's order.
var loginTags = []string{"github-login", "gitlab-login", "gitea-login"}

// personName is the name an identity is assigned by: its login, else a
// login tag.
func personName(login string, meta map[string]string) string {
	if login != "" {
		return login
	}
	for _, k := range loginTags {
		if v := meta[k]; v != "" {
			return v
		}
	}
	return ""
}

// assigneeLabel returns the label of a forge user as an assignee: the name
// of their identity, created as for authors, else their login.
func (e *Engine) assigneeLabel(u User) (string, error) {
	i, err := e.person(u)
	if err != nil {
		return "", err
	}
	name := u.Login
	if i != nil {
		name = cmp.Or(personName(i.Login(), i.ImmutableMetadata()), name)
	}
	return assigneePrefix + name, nil
}

// person returns the identity of a forge user, nil when a dry run would
// create it.
func (e *Engine) person(u User) (*cache.IdentityCache, error) {
	if !e.opts.DryRun {
		return e.identityFor(u)
	}
	if id, ok := e.links[strings.ToLower(u.Login)]; ok {
		return e.repo.Identities().Resolve(id)
	}
	ids, err := identitiesFor(e.repo, e.remote, u, "", false)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return e.repo.Identities().Resolve(ids[0])
}

// severalPeople is why an assignee's label isn't sent: its name is the
// name of several identities.
const severalPeople = "the name of several people"

// accountOf returns the forge user of the person an assignee's name names,
// or why there's none: no identity has that name, or none has an account
// on this forge, or several identities have it and aren't one user here.
// A name is never taken as a login: on another forge, it may be someone
// else's.
func (e *Engine) accountOf(name string) (User, string) {
	// ponytail: reads every identity per assignee sent; index them per push
	// if pushes get slow
	var accounts []User
	people := 0
	for _, id := range e.repo.Identities().AllIds() {
		x, err := e.repo.Identities().ResolveExcerpt(id)
		if err != nil || !strings.EqualFold(personName(x.Login, x.ImmutableMetadata), name) {
			continue
		}
		people++
		if u, ok := e.accountOfIdentity(id, x.ImmutableMetadata); ok {
			accounts = append(accounts, u)
		}
	}
	switch {
	case len(accounts) == 0:
		return User{}, "no one on " + e.remote.Instance()
	case len(accounts) < people || slices.ContainsFunc(accounts, func(u User) bool { return !sameUser(u, accounts[0]) }):
		return User{}, severalPeople
	}
	return accounts[0], ""
}

// sameUser tells whether two users of a forge are one, by id or login.
// ponytail: one known by id and the other by login only, they're two
func sameUser(a, b User) bool {
	return a.ID != "" && a.ID == b.ID || a.Login != "" && strings.EqualFold(a.Login, b.Login)
}

// accountOfIdentity returns an identity's user on this forge, as
// identitiesFor finds them, the other way: by the bridge's link, as a
// token owner, by user id, then by the bridge's legacy key.
func (e *Engine) accountOfIdentity(id entity.Id, meta map[string]string) (User, bool) {
	for _, login := range slices.Sorted(maps.Keys(e.links)) {
		if e.links[login] == id {
			return User{Login: login}, true
		}
	}
	if a, ok := e.accounts[id]; ok {
		return a.User, true
	}
	if v := meta[userKey(e.remote.Instance())]; v != "" {
		return User{ID: v}, true
	}
	if key, isLogin := e.remote.LegacyUser(); key != "" && meta[key] != "" {
		if isLogin {
			return User{Login: meta[key]}, true
		}
		return User{ID: meta[key]}, true
	}
	return User{}, false
}

// labeled returns a forge issue as the engine syncs it, in git-bug's
// labels: its labels and its assignees' labels, assignment events as label
// events, those that change neither left out, and the labels to send again,
// that the forge holds the other way.
func (e *Engine) labeled(issue *Issue) (*Issue, error) {
	if _, ok := e.remote.(AssigneeRemote); !ok {
		return issue, nil
	}
	labelOf := map[User]string{}
	labelsOf := func(users []User) ([]string, error) {
		var res []string
		for _, u := range users {
			l, ok := labelOf[u]
			if !ok {
				var err error
				if l, err = e.assigneeLabel(u); err != nil {
					return nil, err
				}
				labelOf[u] = l
			}
			res = append(res, l)
		}
		return res, nil
	}
	names, err := labelsOf(issue.Assignees)
	if err != nil {
		return nil, err
	}

	res := *issue
	res.Events = nil
	// replayed as the forge changed them, from none
	var labels, assigned []string
	if !slices.ContainsFunc(issue.Events, func(ev Event) bool { return ev.Kind == EventAssignees }) {
		// ponytail: a forge reporting no assignments (GitLab) held its
		// assignees all along, as far as its label events go; reconcile
		// brings their changes
		assigned = names
	}
	for _, ev := range issue.Events {
		was := union(labels, assigned)
		switch ev.Kind {
		case EventLabels:
			labels = union(without(labels, ev.Removed), ev.Added)
		case EventAssignees:
			added, err := labelsOf(ev.Assigned)
			if err != nil {
				return nil, err
			}
			removed, err := labelsOf(ev.Unassigned)
			if err != nil {
				return nil, err
			}
			assigned = union(without(assigned, removed), added)
		default:
			res.Events = append(res.Events, ev)
			continue
		}
		now := union(labels, assigned)
		ev.Kind, ev.Assigned, ev.Unassigned = EventLabels, nil, nil
		ev.Added, ev.Removed = without(now, was), without(was, now)
		if ev.Added != nil || ev.Removed != nil {
			res.Events = append(res.Events, ev)
		}
	}

	res.Labels = union(issue.Labels, names)
	res.resend = nil
	if e.assignees() == AssigneesLabels {
		res.resend = without(names, issue.Labels)
	} else {
		res.resend, _ = splitAssignees(issue.Labels)
	}
	return &res, nil
}

// union returns the labels of a and b, sorted.
func union(a, b []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(slices.Concat(a, b))))
}

// without returns the labels of a that aren't in b.
func without(a, b []string) []string {
	var res []string
	for _, l := range a {
		if !slices.Contains(b, l) {
			res = append(res, l)
		}
	}
	return res
}

// notSent is a RefusedLabels, with why each one wasn't sent.
type notSent struct {
	RefusedLabels
	why []string
	// unknown are those the engine refused for want of one account on the
	// forge, sent once there's one
	unknown []string
}

func (n notSent) Error() string { return "not sent: " + strings.Join(n.why, "; ") }
func (n notSent) Unwrap() error { return n.RefusedLabels }

// changeLabels adds and removes labels on the forge, assignee:<name> labels
// as the assignees of the person's account there, unless the forge holds
// them as labels. Those not sent make a notSent.
func (e *Engine) changeLabels(ctx context.Context, as Account, issue Ref, added, removed []string) error {
	ar, ok := e.remote.(AssigneeRemote)
	if !ok {
		return e.remote.ChangeLabels(ctx, as, issue, added, removed)
	}
	assign, added := splitAssignees(added)
	unassign, removed := splitAssignees(removed)
	asLabels := e.assignees() == AssigneesLabels

	var refused notSent
	refuse := func(l, why string) {
		refused.RefusedLabels = append(refused.RefusedLabels, l)
		refused.why = append(refused.why, l+", "+why)
	}
	// the users to assign and unassign, and their labels
	var toAssign, toUnassign []User
	labelOf := map[User]string{}
	for _, l := range assign {
		name, _ := assigneeName(l)
		u, why := e.accountOf(name)
		switch {
		case why == "" && asLabels:
			// moves off the forge's assignees, if there
			toUnassign = append(toUnassign, u)
		case why == "":
			toAssign = append(toAssign, u)
			labelOf[u] = l
		case !asLabels:
			refuse(l, why)
			refused.unknown = append(refused.unknown, l)
		}
	}
	for _, l := range unassign {
		name, _ := assigneeName(l)
		u, why := e.accountOf(name)
		switch why {
		case "":
			toUnassign = append(toUnassign, u)
			labelOf[u] = l
		case severalPeople:
			refuse(l, why)
			refused.unknown = append(refused.unknown, l)
		}
		// held the old way, maybe
		removed = append(removed, l)
	}
	if asLabels {
		added = append(added, assign...)
	}

	changeLabels := func() error {
		if len(added) == 0 && len(removed) == 0 {
			return nil
		}
		return e.remote.ChangeLabels(ctx, as, issue, added, removed)
	}
	assignees := func() error {
		if len(toAssign) == 0 && len(toUnassign) == 0 {
			return nil
		}
		err := ar.Assign(ctx, as, issue, toAssign, toUnassign)
		users, ok := errors.AsType[RefusedUsers](err)
		if !ok {
			return err
		}
		for _, u := range users {
			// a move to labels leaves them assigned, as a bot GitHub won't
			// unassign
			if l, ok := labelOf[u]; ok {
				refuse(l, "refused by the forge")
			}
		}
		return nil
	}

	// in an order where the forge's assignees and labels never lack one of
	// the labels on their way: their events would be unassignments
	if asLabels {
		if err := changeLabels(); err != nil {
			return err
		}
		if err := assignees(); err != nil {
			return err
		}
	} else {
		if err := assignees(); err != nil {
			return err
		}
		for _, l := range assign {
			if !slices.Contains(refused.RefusedLabels, l) {
				// assigned: the old way's label comes off, if there
				removed = append(removed, l)
			}
		}
		if err := changeLabels(); err != nil {
			return err
		}
	}
	if refused.RefusedLabels != nil {
		return refused
	}
	return nil
}

// wouldRefuse returns, for a dry run, the assignees' labels a push would
// refuse, and why.
func (e *Engine) wouldRefuse(added, removed []string) []string {
	if _, ok := e.remote.(AssigneeRemote); !ok || e.assignees() == AssigneesLabels {
		return nil
	}
	var res []string
	assign, _ := splitAssignees(added)
	unassign, _ := splitAssignees(removed)
	for _, l := range slices.Concat(assign, unassign) {
		name, _ := assigneeName(l)
		if _, why := e.accountOf(name); why == severalPeople || why != "" && slices.Contains(assign, l) {
			res = append(res, l+", "+why)
		}
	}
	return res
}
