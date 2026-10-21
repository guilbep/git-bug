# Native fields: kind and sprint

Starting brief for the `native-fields` branch (off `bridge-sync` at
`8227b073`, after `v0.11.1-sync.7`).

## The principle

The user, 2026-10-20: seamless first, so native where the forge has the
concept, labels otherwise; and retro-compatible: what projects hold today
keeps working, and existing labels move to the native field, as the old
`assignee:*` labels moved to GitHub's assignees in sync.7 (`Issue.Resend`,
refusals, the per-project `assignees = labels` switch). See
[sync-decisions.md](sync-decisions.md#assignees).

## kind: and GitHub's issue types

- A GitHub issue has at most one issue type; `kind:` is single-valued too.
  git-board's kinds come from `[branches] kinds` (bug, feat, polish, perf,
  refactor, docs, style, test on co2).
- Issue types are defined per organization: an EPFL-ENAC admin would create
  one type per kind. A kind without a type is refused (label kept, one
  warning), or stays a label: to decide.
- A user-owned repository has no issue types (to check), so the scratch
  repo guilbep/git-bug-sync-test can't test them: a scratch repository in
  an organization is needed. Ask the user which.
- GitLab has no equivalent (its work item types are issue, incident, task):
  `kind:` stays a label there.

## sprint: and iterations or milestones

- On co2 and Project 40, sprints are the Project's Sprint iteration field,
  not milestones, maybe for the flexibility: iterations span repositories,
  run on a cadence and allow breaks; a milestone is per repository, with a
  due date only. git-board already syncs Project fields with labels
  (`git-board bridge sync`), so on GitHub the native home may already be
  covered, outside the fork.
- GitLab's free edition has milestones, not iterations: `sprint:` and
  milestones there may fit. Not checked.

## First step

A short decision doc for each of the two, weighed with the user before any
code: what maps to what on each forge, what happens to existing labels,
what a project can switch off. Then build what's approved.

## Rules

- Engine tests with the synctest fake first, then the adapters' fakes.
- Live tests only on scratch projects. Never co2-calculator, never Project 40
  (read only).
- gofmt with Go 1.26 and `go generate` (don't commit its `api/graphql`
  drift). Never push `bridge-sync` or tags: the user merges and tags.
- Record every ruling in sync-decisions.md with its *If wrong*.
