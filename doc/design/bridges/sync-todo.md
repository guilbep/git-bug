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

## Design (sync.md), approved section by section

- [x] Goal and decisions
- [x] Core concept: three-way sync per remote
- [x] Pull: replay the remote timeline, claim echoes, then reconcile
- [x] Adapter interface (checked against Gitea's API too)
- [x] Sync loop, `bridge sync` command, `--dry-run`, lock
- [ ] Rendering: attribution, `@mentions`, `#refs`
- [ ] Identities and tokens
- [x] Errors and conflicts
- [ ] Migration: legacy metadata, existing bridges, `export-foreign`
- [ ] Testing: fake forge and conformance suite

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
