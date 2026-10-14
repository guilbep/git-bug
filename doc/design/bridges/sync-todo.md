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

## Build (order to be fixed by the implementation plan)

- [ ] Fake forge and conformance suite. Run it against today's GitHub and
      GitLab bridges first, and record what fails.
- [ ] Engine in `bridge/core`
- [ ] GitLab adapter
- [ ] GitHub adapter
- [ ] `git bug bridge sync`, `--dry-run`, lock
- [ ] Rendering: mentions and refs
- [ ] Legacy metadata migration
- [ ] Docs: configuration.md, third-party.md, feature-matrix.md
- [ ] Gitea/Forgejo adapter
- [ ] Run against real forges (scratch projects)

## Known issues to cover

- GitHub: an exported body edit is tagged with the issue id like the create
  operation, so re-import fails on multiple matches.
- GitHub and GitLab: status, title and label changes echo back on pull.
- GitLab: a pull before a push reverts unpushed local comment edits.
- GitLab: a note can only be edited by its author, so edits must use the
  token that posted it.
- GitLab and Jira bridges have no rate-limit handling.
- `bridge push` has no `--since` and always walks every bug.
