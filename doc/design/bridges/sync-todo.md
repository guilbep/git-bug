# Bridge sync: TODO

Working checklist for [sync.md](sync.md), on branch `bridge-sync` (fork
`guilbep/git-bug`). One branch, one PR. Drop this file before the PR is
merged upstream.

## Done

- [x] GitLab fixes: multi-token export, empty base-url, push-then-pull
      duplicates, edited-comment multiple match, duplicate token-owner
      identities (`4ced8cd3`).
- [x] GitLab `export-foreign` option, attribution header, fake GitLab API
      test (`f258207c`). To be folded into the engine.

- [x] `@mentions` defused in mirrored text, restored on pull (`0da7c526`).

## Real-world trial, 2026-10-14

GitHub `EPFL-ENAC/wiki` (private, 5 issues, 1 comment, 1 closed, edit
history) → gitlab.epfl.ch, with the branch's binary and `export-foreign`.
Script: a scratch repository, `bridge pull gh`, `bridge push gl`, then
`bridge pull gl` and `bridge push gl` again.

- Scratch run: `guilbert/git-bug-sync-test` (project 42400, private, kept
  for the engine's real-forge test).
- Real run: `EPFL-ENAC/wiki` (project 41960), issues #2 to #6.

Worked: attribution and source links for the three other authors; your own
issues posted as you (your identity holds both logins); mentions defused
(`@nico`, `@pierre`, `@Quentin`, `@nicdub`...), the one in inline code left
alone; the closed issue closed; three GitHub body edits replayed as
description updates; pull after push imported no issue, no comment edit,
and no description edit; the second push sent nothing.

Failed, as predicted, both local only:

- [ ] The pushed close came back as a new status operation (echo).
- [ ] The pull created a second identity for the GitLab token owner (the
      exporter tolerates it since `4ced8cd3`, the importer still creates it).

## Real two-way run, 2026-10-14

`guilbep/git-bug-sync-test` (GitHub, private) and
`guilbert/git-bug-sync-test` (gitlab.epfl.ch, private, holding the 5 issues of
the first trial), both bridges with `sync`, the branch's binary:

1. `--dry-run`: would import the 5 GitLab issues, nothing else.
2. Sync: pulled 5 issues, pushed them to GitHub with their comment and
   close; mentions defused; posted as `guilbep` since the user's identity
   holds both logins.
3. Changes on each forge (`gh`, `glab`): an issue and a comment on GitHub,
   a comment and a close on GitLab. Sync: each reached the other forge, no
   echo imported.
4. Sync again: nothing written, only the claims of the previous run's
   echoes recorded. Once more: nothing at all.

Both forges then show the same 6 issues, statuses and comment counts.

Known limits it showed:

- [ ] Text mirrored by another git-bug repository loses its attribution on
      pull: the header is removed with the marker. Only the early
      export-foreign header exists in the wild (scratch and EPFL projects).
- [x] The repository that pushed the 5 EPFL-ENAC/wiki issues to gitlab.epfl.ch
      is the only one linking them to their GitHub originals: a new hub
      would import both as different bugs. Deleted those GitLab issues.

## EPFL-ENAC/wiki dry run, 2026-10-14 evening

GitLab issues #2-#6 deleted; the wiki's own git-bug data pulled (6 bugs,
4 identities), both bridges with `sync`, existing credentials.

- [x] The stored GitLab token (`https://gitlab.epfl.ch`) wasn't found by a
      bridge on `https://gitlab.epfl.ch/`: base URLs now compare without
      their trailing slash.
- [x] Bugs synced by the engine had no `github-url`, which git-board reads:
      kept on every synced bug.
- [x] Dry run: the 5 GitHub issues are the 5 bugs the old bridge left
      (recognized); it would create `e7ae216` on GitHub (a test bug, no
      GitHub link) and the 5 others on GitLab. `e7ae216`'s GitLab issue #1
      is deleted: marked gone, not recreated.
- [x] Same person on two forges: `login-<identity>` links in the bridge
      configuration.

## EPFL-ENAC/co2-calculator dry runs, 2026-10-14 evening

The 228 bugs of the local clone (old GitHub bridge, git-board labels) in a
scratch repository, GitHub and gitlab.epfl.ch `EPFL-ENAC/co2-calculator`.

- [x] First dry run: GitHub refused the first page ("Resource limits for this
      query exceeded"); the run still pushed, and exited 0. Now the page
      halves, a forge whose pull failed isn't pushed to, and a dry run with
      errors fails.
- [x] The clone's old bridge recorded a last import after 228 issues: sync
      would have skipped the rest. Sync keeps its own `lastSyncTime`.
- [x] GitHub's secondary rate limit on writes now reaches the engine, which
      waits a minute and retries.
- [x] Second dry run: 203 s, 125 GraphQL points, no error. Would import 836
      issues, set git-board labels on 182 GitHub issues, and create about
      1,064 issues on GitLab (the 228, then the 836).
- [ ] The real first sync, by the user. Creating about 1,064 GitLab issues
      may notify members watching the project.

## Next

- [x] git-board on the fork: branch `git-bug-fork` of guilbep/git-board,
      tests green, reads the co2 data. Its labels (`status:`, `prio:`,
      `sprint:`) are pushed to every synced forge.
- [x] Installing the fork: INSTALLATION.md, "The bridge-sync fork".
- [x] Friendlier setup and help: `bridge new --sync`, the target guessed
      from a github.com or gitlab.com URL, GitLab's base URL from the
      project URL, the only stored credential that fits used; the flow in
      `bridge sync -h`; a quick start in third-party.md.
- [x] A dry run reports the issues a pull would find by their git-bug
      marker ("would sync with …") instead of planning to create them
      again. Seen on the wiki: from a clone without the links of the first
      real sync (2026-10-14, GitLab #7–#11, GitHub #29), it announced five
      duplicates the real run wouldn't make.
- [x] `git bug push` reaches every push URL of a remote, as git push does.
      The wiki's origin mirrors to GitHub and GitLab (two pushurls): the
      sync's links had reached GitLab only.
- [x] One issue at a time: `bridge sync --issue <id>` (core `SyncIssue`,
      `Remote.Fetch` on GitHub, GitLab, Gitea and the fake, a conformance
      scenario). git-board's issue panel calls it: Send now, Refresh.
- [x] Titles carry translated `#refs` too (co2's `… (#2518)` named another
      issue on GitLab), and a reconciled GitLab or Gitea edit is rendered
      as written there (its mentions were posted live).
- [x] Duplicates, one issue at a time: `bridge link <id> <bridge> <number|url>`
      (it already is that forge issue), `bridge unlink <id> <bridge>` (stop
      syncing it there, for good), `bridge take <id> <bridge>` (drop what
      wasn't sent there, keep the forge's version); core `Link`, `Unlink`,
      `Take` for git-board. Rulings in sync-decisions.md.
- [x] GitHub assignees as `assignee:<login>` labels, both ways; GitHub's
      old `assignee:*` labels moved to the assignees. Live on
      guilbep/git-bug-sync-test #11 (2026-10-20): the old labels of
      `guilbep` (moved) and `torvalds` (no access: kept, one warning); then
      git-bug's and GitHub's own assignee changes; syncs after them quiet.
- [ ] GitLab assignees the same way.
- [ ] Issue numbers of git-bug's own, to name an issue across forges:
      discussed apart, not built.
- [ ] CI on the fork: draft PR guilbep/git-bug#1 (`bridge-sync` → the
      fork's `trunk`, never merged) runs build-and-test.yml on every push,
      once Actions are enabled on the fork (a one-time click in its Actions
      tab; no API for it).
- [ ] Binaries of the fork: after that click, a `v0.11.1-sync.N` tag runs
      release.yml, which makes a *draft* release to publish by hand.
- [ ] Later: GitHub end-to-end tests (token with `delete_repo`); upstream
      (discuss on #1476 first, given #1223).

## Design (sync.md), approved section by section

- [x] Goal and decisions
- [x] Core concept: three-way sync per remote
- [x] Pull: replay the remote timeline, claim echoes, then reconcile
- [x] Adapter interface (checked against Gitea's API too)
- [x] Sync loop, `bridge sync` command, `--dry-run`, lock
- [x] Rendering: attribution, `@mentions`, `#refs`
- [x] Identities and tokens
- [x] Errors and conflicts
- [x] Migration: legacy metadata, existing bridges, `export-foreign`
- [x] Testing: fake forge and conformance suite
- [x] Full spec: user said to proceed without waiting for review (2026-10-14)
- [x] Implementation plan written: [sync-plan.md](sync-plan.md). Build tasks are tracked there.

## Build (tasks in [sync-plan.md](sync-plan.md))

- [x] Bridge lock (maintainer's `0cae4552`, cherry-picked)
- [x] Engine in `bridge/core`: Remote interface, tags and bases, rendering,
      identities, pull (replay, claim, reconcile), push, errors
- [x] Fake forge and conformance suite (`bridge/core/synctest`). Today's
      bridges were characterized by the real-world trial instead.
- [x] `git bug bridge sync`, `--dry-run`, lock for the whole run
- [x] GitLab adapter: conformance passes on an HTTP fake; old importer and
      exporter removed; legacy metadata read; real end-to-end test passes
      on gitlab.epfl.ch
- [x] GitHub adapter: conformance passes on a fake of the GraphQL client;
      old importer and exporter removed; mock and end-to-end tests ported
- [x] Docs: configuration.md, third-party.md, feature-matrix.md, CLI help
- [x] Gitea/Forgejo bridge, born on the engine: conformance passes on an
      HTTP fake and on a real Gitea 1.22.6 (docker), which found two bugs the
      fake didn't have: labels need write access, issues of the same second
      listed newest first
- [x] Two-way run against real forges (scratch projects), see below
- [x] Whole-branch review: fixed two critical issues (conflicts decided by
      the issue's last update; text of people without a token pushed
      without sync), four important ones (partial GitHub timelines, legacy
      duplicate pairs, not found taken for deleted, quadratic fingerprints)
      and two re-graded ones (stray backtick, strangers' markers)

## Deferred from the review (minor)

- [x] Reconcile's title, status and label changes print nothing.
- [ ] `bridge push` alone, after a write whose answer was lost, creates the
      issue twice: the marker recovers it only on pull. Use `bridge sync`.
- [x] GitHub rate limits never reach the engine: big backfills may fail
      per bug. Writes now report them; pages too costly shrink.
- [ ] Goroutine leaks when a caller stops early (GitLab issue listing, GitHub
      mediator's edit reversal).
- [ ] The attribution names the raw remote key when bridges are pulled and
      pushed in separate processes instead of `bridge sync`.
- [ ] The header links to the issue itself when a git-bug edit goes back to
      the forge the text came from.
- [ ] A label's case can flip once (GitHub and Gitea ignore it, the engine
      doesn't).
- [ ] `sync=false` is refused on Jira and Launchpad.
- [ ] The 5 s import-time margin is smaller than the 5 min claim window.
- [ ] Gitea lists issues newest first across pages.
- [ ] `Remote.User` has no caller.

## Known issues to cover

- GitHub: an exported body edit is tagged with the issue id like the create
  operation, so re-import fails on multiple matches.
- GitHub and GitLab: status, title and label changes echo back on pull.
- GitLab: a pull before a push reverts unpushed local comment edits.
- GitLab: a note can only be edited by its author, so edits must use the
  token that posted it.
- The Jira bridge has no rate-limit handling.
- `bridge push` has no `--since` and always walks every bug.
