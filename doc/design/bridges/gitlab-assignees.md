# GitLab native assignees, translated per person

Starting brief for the `gitlab-assignees` branch (off `bridge-sync` at
`8227b073`, after `v0.11.1-sync.7`). Decided with the user on 2026-10-20;
the rulings are in [sync-decisions.md](sync-decisions.md#assignees).

## What to build

- On GitLab, git-bug's `assignee:<x>` labels are GitLab's own assignees,
  as on GitHub since sync.7. The per-project switch `assignees = labels`
  (`core.WithAssignees`, the bridge key) applies to GitLab too.
- `assignee:<x>` names a git-bug person, not a forge login. Each forge gets
  that person's own account there. The engine translates, as it already
  does `#n` in titles:
  - push: the identity, then its user id on that forge
    (`sync:<instance>:user`) or the bridge's `login-<identity>` link;
  - pull: the forge user, then its identity (`identityFor`), then the
    identity's login.
- A person unknown on a forge is refused there: the label stays in git-bug,
  with one warning (`RefusedLabels`). A login is never sent as is to
  another forge, where it may be someone else.
- gitlab.epfl.ch is GitLab's free edition (19.4, per notes of 2026-10-15):
  one assignee per issue. The others are refused the same way. The team
  doesn't need several (user, 2026-10-20).

## Why it matters

Today, with GitHub and GitLab in one sync (the wiki's case), a GitLab login
in an `assignee:` label is sent to GitHub as that GitHub login, possibly a
different person, and GitHub's assignees reach GitLab as plain labels.

## Design notes

- The GitHub adapter's union of its assignees and old `assignee:*` labels
  moves into the engine: an old label holds git-bug's value, not
  necessarily a GitHub login. So adapters likely report assignees as
  forge users (`Issue.Assignees []User`, assignee events with users) and
  take users to assign, and the engine does the translation, the union and
  `Resend`. Keep everything already true in sync.7: migration of old
  labels, refusals, the labels mode, steady syncs.
- GitLab has no resource events for assignees (only system notes): its
  assignees come from the issue's state, through reconciling.
- GitLab assigns with `assignee_ids` on an issue update: read the assignees
  back after the write, as the GitHub adapter does, rather than trusting
  how the free edition treats several ids. Check this live.

## Rules

- Engine tests with the synctest fake first, then the adapters' fakes.
- Live tests only on scratch: GitHub guilbep/git-bug-sync-test, and
  gitlab.epfl.ch project 42400. Never co2-calculator, never Project 40.
- gofmt with Go 1.26 (`$(GOTOOLCHAIN=go1.26.0 go env GOROOT)/bin/gofmt -l .`)
  and `go generate` (don't commit its `api/graphql` drift).
- Never push `bridge-sync` or tags: the user merges and tags.
- Record every ruling in sync-decisions.md with its *If wrong*.

## Done when

GitHub and GitLab in one sync carry assignees to the right person on each
forge; someone unknown on a forge is refused with one warning; syncs after
are quiet; a live two-forge run on the scratch projects shows it.
