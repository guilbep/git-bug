# Before the upstream pull request

**Fork-only.** This file is the to-do list and reading guide for taking this
fork's work to [git-bug/git-bug](https://github.com/git-bug/git-bug). Delete it,
with the other fork-only files below, from what goes upstream.

This fork (guilbep/git-bug) adds two-way sync between git-bug and several
forges at once: GitHub, GitLab and Gitea. git-bug's data is the single source of
truth, and forges never talk to each other. Upstream deliberately blocks
pulling from one bridge and pushing to another
([#1223](https://github.com/git-bug/git-bug/issues/1223)), and
[#1476](https://github.com/git-bug/git-bug/issues/1476) asks for it.

## Read, in this order

1. [doc/design/fork.md](doc/design/fork.md): why the fork exists, upstream's
   objections and the answers to them, what goes upstream and what stays
   here, and how the branch keeps up with trunk.
2. [doc/design/bridges/sync.md](doc/design/bridges/sync.md): the design. The
   pull request links to it.
3. [doc/design/bridges/sync-decisions.md](doc/design/bridges/sync-decisions.md):
   every ruling, with what it costs if wrong.
4. [doc/design/upstream-review.md](doc/design/upstream-review.md): every
   concern the maintainers raised, quoted and linked, and whether the fork
   answers it. The gaps below come from it.
5. [doc/design/bridges/sync-todo.md](doc/design/bridges/sync-todo.md): known
   gaps and deferred review findings.
6. [doc/usage/third-party.md](doc/usage/third-party.md): the user-facing
   setup, as a maintainer will try it.

## To do, in order

guilbep gives the go for every step that leaves this machine.

- [ ] Rebase on the current `origin/trunk` (fork.md, "Staying close to
      trunk"). Then: `go build ./...`, `go vet ./...`, `make test`, gofmt with
      Go 1.26 (`$(GOTOOLCHAIN=go1.26.0 go env GOROOT)/bin/gofmt -l .`),
      `go generate ./...` (commit only `doc/`), `go mod tidy`. Each leaves
      `git status` clean.
- [ ] The fork's CI is green on the branch.
- [ ] Close the gaps below, or name each one in the pull request text.
- [ ] Wait for Muré's breaking changes to settle (#1628). Then ask on
      discussion #1500 or on #1476, citing #1223, with links to sync.md and
      to fork.md's answers. Ask in what shape the maintainers would take it:
      one pull request, or a series.
- [ ] Drop the fork-only files (below) from the pull request branch.
- [ ] Open the pull request: one commit unless asked otherwise, with the
      commit message as its description.

## Gaps a maintainer may block on

From [doc/design/upstream-review.md](doc/design/upstream-review.md), most
likely to block first. All of them are open.

1. **Timing and size.** On 2026-10-22, Muré paused the only open new-bridge
   pull request (#1628): *"I'll keep making breaking changes underneath for
   some time. Sorry, I don't have the head space to work on a new bridge
   for now."* This one is +21k lines. *Close:* ask first, then send a
   series: standalone fixes, then opt-in foreign export on the existing
   exporters, then the engine.
2. **Every GitHub and GitLab user changes format, not only `sync` users.**
   `bridge pull` and `bridge push` run the engine. It writes `sync:*` keys
   and stops writing `github-id` and `gitlab-id`, so upstream's binaries
   import synced issues again. *Close:* keep writing the spec's keys, or
   keep the old path for bridges without `sync`. Propose the new keys, and
   their last-one-wins reading, to git-bug/spec.
3. **Confidential GitLab issues got published.** *Closed* (sync.10):
   confidential issues aren't synced, and a sync bridge to a public project
   refuses to run while another sync bridge isn't public, unless it has
   `publish-private`. Left: it's decided per repository, so a clone without
   the private bridge configured can't tell.
4. **Two bridge architectures.** Jira and Launchpad keep the old importer
   and exporter, and Muré counts Jira among the "quality bridges". *Close:*
   port Jira, or keep the old path (same fix as 2).
5. **Identity keys clash with upstream's plan (#1576).** The GitHub login
   stands in for the user id (`sync:github.com:user`), where upstream wants
   the numeric id, "never the mutable username". The engine also commits to
   identities it didn't create, which protected identities will refuse.
   *Close:* numeric ids under upstream's key name, no writes to others'
   identities.
6. **The bot account isn't enforced.** `bridge new --sync` never checks
   that `default-login` is a bot, so it's usually the operator posting
   everyone's text. *Close:* name the costs (fork.md lists them), warn or
   refuse when `default-login` is a person's own token, try admin `sudo`
   tokens first on GitLab and Gitea.
7. **GitHub's status and labels came from timeline events only.** *Closed*
   (sync.10): they're read from the issue itself, and reconcile records a
   difference once. Left: assignees still come from the timeline, and an
   issue pulled wrong before is corrected only when it changes again, or on
   `bridge pull --no-resume`.
8. **Label namespaces** (`assignee:`, `kind:`, `sprint:`) run against
   #1526 (an assignee operation, as a list) and #639 (milestones as their
   own entity). Review sync (`review:*`) too. *Close:* keep them fork-only.
9. **Four new commands**, where upstream's consensus (#1223, #1391) is
   bridges inside `git bug pull` and `git bug push`, opt-in by config.
   *Close:* offer that, and keep `bridge sync` as the explicit form.
10. **Fork.md overclaimed** ("data model untouched", "any clone can run the
    sync"). Corrected on 2026-10-23.

Lead with what upstream asked for: the opt-in Muré set as the condition in
#1500, and the narrow, forge-only `Remote` interface with a fake forge,
which is the reduced coupling he wants in #1227.

## Fork-only files

Remove them from the pull request branch:

- `BEFORE-PR.md` (this file)
- `doc/design/fork.md`
- `doc/design/upstream-review.md`
- `doc/design/bridges/sync-plan.md`
- `doc/design/bridges/sync-todo.md`
- `doc/design/bridges/native-fields-plan.md`
- `doc/design/bridges/native-fields.md`
- `doc/design/bridges/gitlab-assignees.md`
- the "The bridge-sync fork" section of `INSTALLATION.md`, and its line in the
  table of contents

None of the upstream-bound docs links to them.
