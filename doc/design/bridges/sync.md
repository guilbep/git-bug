# Bridge sync

Status: **draft, being designed** on branch `bridge-sync`. The working
checklist is in [sync-todo.md](sync-todo.md).

## Goal

git-bug's data is the single source of truth for issues. They sync both
ways with any number of forge projects (GitHub, GitLab, and any other forge
that gets an adapter). An issue opened, commented, or changed on any of them
ends up on all the others, through git-bug.

```
  GitHub repo  <--->  .-----------.  <--->  GitLab project
                      |  git-bug  |
  Gitea repo   <--->  '-----------'  <--->  another GitLab
```

Forges never talk to each other directly. git-bug is the hub.

## Decisions

Taken on 2026-10-14:

1. **Forges in this branch**: GitHub and GitLab, implemented and tested. A
   Gitea/Forgejo adapter is the last phase, to prove the engine is not
   GitHub/GitLab-shaped. Jira and Launchpad keep their current importer and
   exporter until someone ports them.
2. **Runner**: a new `git bug bridge sync` pulls every configured bridge, then
   pushes every bridge, under one lock for the repository. `bridge pull` and
   `bridge push` keep working.
3. **Backfill**: the first sync mirrors everything, open and closed, with full
   history. `git bug bridge sync --dry-run` lists what would be created or
   changed, so the notifications that follow are a deliberate choice.
4. **Text posted on someone else's behalf** carries an attribution header
   (author, date, link to the source). In it, `@alice` is defused to
   `` `@alice` `` so it doesn't ping whoever is alice on the target, and
   `#123` is translated to the mirrored issue's number on the target when
   git-bug knows it, or to a link to the source otherwise.
5. **Architecture**: one sync engine in `bridge/core`, and thin adapters per
   forge. All sync rules live in the engine once.
6. **Remote ids are kept per remote project**, so any number of remotes can
   be synced, of the same kind or not. The legacy `github-*` and `gitlab-*`
   metadata stays readable, so existing repositories keep working.

Assumptions, open to change:

- **One runner.** Only one machine or CI job runs `bridge sync` for a
  repository, after a `git bug pull`. A lock prevents concurrent runs on the
  same repository (see the unmerged `bridge-lock` branch). Two runners on two
  machines could create the same issue twice.
- **Conflicts**: when the same value changed on both sides since the last
  sync, the later change wins. The other one stays in git-bug's history.
- Each remote uses a **dedicated bot account** as `default-login`, to post
  what people without a token on that forge wrote.

## Why the current bridges can't sync

Found while building the GitLab `export-foreign` option (commits on this
branch) and reading the GitHub, GitLab, Jira and Gitea code:

- Every exporter skips issues whose `origin` is another bridge, on purpose
  (#1223). Jira's importer even matches bugs on `origin`.
- Each bug holds one set of `github-*` or `gitlab-*` ids, so a second
  project of the same kind can't be synced.
- **No forge returns the id of the event a change creates.** Creating an
  issue or a comment returns an id. Closing, reopening, renaming, changing
  labels, or editing a body or a comment does not (GitHub, GitLab, Gitea,
  Jira). The bridges tag these operations with the issue's id, while the
  importers recognize operations by event id. So on pull, every exported
  status, title or label change comes back as a new operation, authored by
  the bot. With two forges pushing, these bounce between them forever.
- The GitLab importer detects edits by comparing the remote text with the
  local text. When a pull runs before a push, an edit made locally since the
  last push is overwritten by the older remote text.
- The GitLab importer matches people by numeric id, while tokens are matched
  by login: the first pull creates a second identity for every token owner
  (fixed on this branch).

## Core concept: three-way sync per remote

For each remote R, and each value that syncs (an issue's status, title,
labels and body, and each comment's text), the engine compares three
versions:

- **local**: git-bug's current value;
- **remote**: what R shows now;
- **base**: what R last saw, as recorded in git-bug.

| local vs base | remote vs base | Action |
|---|---|---|
| same | same | nothing |
| changed | same | push the local value to R |
| same | changed | pull: record R's change as a git-bug operation |
| changed | changed, equal to local | converged: record the base, no write |
| changed | changed, different | conflict: the later change wins |

The **base is derived from the git data**, not kept on the side. Every
operation R has seen, because it was pulled from R or pushed to R, is
tagged with write-once metadata for R: the remote id, and for text a
fingerprint of the normalized text as R holds it. The base for R is the
state built from R's tagged operations only. It travels with the git data,
so any machine can run the sync.

Text is compared by **fingerprint**, not by undoing the rendering: the
attribution header and translated `#refs` can't be reliably reversed. Pulled
text that did change is turned back into git-bug text on a best-effort basis
(header removed, mentions restored).

## Open decision: replay or reconcile on pull

TODO: decide with the user.

## Still to design

Adapter interface, the sync loop and command, rendering, identities and
tokens, errors, migration of existing data and bridges, testing.
