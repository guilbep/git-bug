# The fork: what it's for, and the way back upstream

- **Status:** proposed, 2026-10-21. Updated 2026-10-23, after the squash
  and the first rebase.
- **Decides:** guilbep, who owns the fork.
- **Scope:** the `bridge-sync` branch of guilbep/git-bug, and how it goes
  back to git-bug/git-bug. How the sync works is in
  [sync.md](bridges/sync.md), and each ruling taken while building it is
  in [sync-decisions.md](bridges/sync-decisions.md), with what it costs if
  wrong. This doc doesn't repeat them.

## Vision

git-bug's data is the single source of truth for a project's issues. It
syncs both ways with any number of forge projects, of any kind: GitHub,
GitLab and Gitea today. An issue opened, commented or changed on any of
them reaches all the others through git-bug. Forges never talk to each
other.

People keep working where they are, so the sync must be seamless:

- **Native where the forge has the concept, labels otherwise.** git-bug
  holds an issue's fields as labels: `assignee:<person>`, `kind:<kind>`,
  `sprint:<sprint>`. Where a forge has assignees, issue types or milestones,
  these labels become those fields there (assignees since sync.7, issue
  types and milestones since sync.9). Elsewhere they stay
  plain labels.
  A project that wants labels on the forge too switches per project
  (`assignees = labels`), the same in every clone.
- **A person when git-bug knows one, else a login.** When `assignee:<x>`
  names a git-bug identity, each forge gets that person's own account
  there. Otherwise `<x>` is a login, sent as is, as before sync.8 (decided
  2026-10-21).
- **Retro-compatible.** What repositories hold today keeps working. The old
  bridges' `github-*` and `gitlab-*` metadata stays readable. Data in the
  old shape, such as an `assignee:` label on GitHub, moves to the native
  field once, after one full pull.
- **Nothing lost silently.** A value a forge refuses stays in git-bug, with
  one warning.
- **Nothing published that wasn't public.** Upstream's bridges only import;
  the sync also publishes. Confidential issues stay where they are, and
  nothing pulled from a project that isn't public reaches one that is,
  unless that bridge allows it (`publish-private`). Moderation and
  attribution follow from the same rule: a deletion isn't propagated, and
  the account that posts for others answers for it.
- **Any clone can run the sync, given the same bridge config.** What each
  forge last saw is recorded in git-bug's own data, so it travels with
  `git bug push` and `git bug pull`. The links (`login-<identity>`) and the
  modes (`assignees`, `kinds`, `sprints`) don't: they are per clone, and
  every clone that syncs must hold the same.

The design rules that follow from it:

- **One engine, thin adapters.** Every sync rule lives once, in
  `bridge/core`. An adapter only reads and writes its forge. A fake forge
  (`bridge/core/synctest`) and a conformance suite test the engine and each
  adapter without the network.
- **git-bug's entities and operations are untouched.** `entities/`,
  `entity/`, `cache/` and `api/` don't change. The data format does
  change, though:
  - new metadata keys, `sync:<remote>:*`, which the spec doesn't list;
  - a reading of them the spec doesn't define: SetMetadata operations in
    order, the last one winning, where the spec keeps a key's first value;
  - the GitHub and GitLab bridges no longer write `github-id` and
    `gitlab-id`, so upstream's binaries import synced issues again.
  [upstream-review.md](upstream-review.md), §5.1, has the details.

Not goals:

- forges syncing with each other directly;
- several runners at once on one repository: the bridge lock covers one
  machine;
- Jira and Launchpad, which keep their importer and exporter until someone
  ports them.

## Who it's for

The driving case is a team whose code and issues live on GitHub and on a
self-hosted GitLab (free edition), with git-bug as the shared copy.

**The fork is the product.** It serves that team and git-board first.
Upstream gets the parts that stand on their own, when its maintainers want
them; the fork isn't shaped for one big pull request.

git-board, a separate board and issue-management UI over git-bug, builds
on the fork and pins its release tags (`v0.11.1-sync.N`). What it uses is
a contract that every rebase must keep:

- `core.SyncAll`, `core.SyncIssue`, `core.Link`, `core.Unlink` and
  `core.Take`;
- the options `WithAssignees` and `WithPullsOf`, and `(*Bridge).Use`;
- the bridge keys (`ConfigKeySync`, `ConfigKeyAssignees`, and so on) and
  `SetBridgeConfig`;
- the `<kind>-url` metadata on synced bugs, and the `review:*` metadata on
  review comments.

## Upstream's objections, and the answers

git-bug blocks, on purpose, pulling from one bridge and pushing to
another. A re-review of everything the maintainers wrote on it is in
[upstream-review.md](upstream-review.md) (2026-10-23), with quotes and
links. In short:

1. Muré, [#1223](https://github.com/git-bug/git-bug/issues/1223#issuecomment-2268697889):
   *"bridges can't push the events that are not owned by a known account
   (one we have an auth for), so (almost always) we can't push
   everything."* Earlier, in #779, he asked for credentials for every user,
   to replicate bugs "properly".
   - The fork posts those events with each forge's `default-login`
     account, under a header naming the author, the date and the source.
     Mentions are defused, and `#123` becomes the mirrored issue's number.
   - That's not the transparency he meant. It's the opt-in approximation
     he accepted in [#1500](https://github.com/git-bug/git-bug/discussions/1500#discussioncomment-15162175):
     *"it should be an opt-in behavior IMHO, a flag or something"*. The
     per-bridge `sync` switch is that opt-in.
   - Its costs, to name: status, title and label changes become the
     account's; nothing checks that `default-login` is a bot and not the
     operator's own account; the account's owner answers for everything it
     posts; GitHub caps it at 500 content writes per hour; mentions never
     notify across forges; deletions don't propagate.
   - Not tried: an admin token that acts as each user (GitLab and Gitea
     `sudo`, unverified), which Muré named as the better path in #1500.
2. Muré, same comment: *"git-bug on-purpose prevent from pulling from one
   bridge and pushing to another, because that would cause more problems
   than it would resolve."* He never listed the problems. The fork found
   and solved four (sync.md, "Why the current bridges can't sync"): echoes
   that bounced between forges, one id set per bug, GitLab's importer
   overwriting local edits, and conflicts. The review found more that a
   maintainer would add, still open: confidential issues get published,
   GitHub's wrong status (#609, #1638) spreads to the other forges, and
   identity keys clash with upstream's plan (#1576).
3. Muré's own proposal in #1223 was to make bridges feel *less* native,
   by renaming push and pull to import and export. The fork goes the other
   way. Say so; don't present it as an answer to him.
4. sudoforge, in [#1223](https://github.com/git-bug/git-bug/issues/1223#issuecomment-2271585365),
   on tagging entities with the bridge they came from: *"it is definitely
   possible, on a technical level… which would support this polyglot
   syncing approach. but that would require more work. we could keep the
   enforced limitation of 1 bridge connection in place"*. His transparency
   means bridges running inside `git bug push` and `git bug pull`, opt-in
   by config (#1391), where the fork adds four commands.
5. [#1476](https://github.com/git-bug/git-bug/issues/1476) (sudoforge,
   open) asks for one-way migration between bridged repositories. Live
   sync does more than it asks.

Timing: on 2026-10-22, Muré paused the only open new-bridge pull request
([#1628](https://github.com/git-bug/git-bug/pull/1628#issuecomment-6351219155)):
*"I'll keep making breaking changes underneath for some time. Sorry, I
don't have the head space to work on a new bridge for now."*

## What would go upstream

Against trunk at `948e63ce` (2026-10-22), the fork is one commit. It
changes 84 files, generated docs aside: +20,877 −2,786. 54 files are new
and hold 19,758 of the added lines. 26 are upstream's files, modified. 4
are deleted: the GitHub and GitLab importers and exporters, which the sync
adapters replace (`bridge pull` and `bridge push` run the engine).

- **Upstream-bound:**
  - the engine (`bridge/core/sync*.go`, `synctest`);
  - the GitHub, GitLab and Gitea sync adapters;
  - `git bug bridge sync`, `link`, `unlink` and `take`;
  - their docs (sync.md, sync-decisions.md, third-party.md,
    feature-matrix.md);
  - fixes to existing code that stand alone, such as pushing to every
    push URL of a remote (`f8f4521b`).
- **Upstream-optional, git-board-shaped:**
  - pull request review sync (`WithPullsOf`, `review:*`);
  - the label namespaces (`assignee:`, `kind:`, `sprint:`). Upstream
    git-bug has none, and both assignee modes act on the forge: `native`
    brings assignees in as `assignee:` labels, and `labels` moves them to
    labels and unassigns (`kinds` does the same with types). Upstream
    likely needs a third mode that leaves the forge's field alone, as
    `sprints = off` does with milestones, maybe as its default. That
    default is safe only for a project that never synced in another mode:
    switched to it later, the values the field held look removed. To ask
    upstream.
- **Fork-only** (about 2,800 lines, listed in
  [BEFORE-PR.md](../../BEFORE-PR.md) for removal):
  - the INSTALLATION section on building the fork;
  - the working docs (sync-plan.md, sync-todo.md, native-fields-plan.md);
  - the branch briefs (gitlab-assignees.md, native-fields.md);
  - this doc, upstream-review.md and BEFORE-PR.md.

## Staying close to trunk

Upstream moves fast: 41 commits from 2026-10-13 to 2026-10-22. The fork
rebases rather than merges, so the branch stays one clean commit that a
pull request can take.

- **One commit** (guilbep, 2026-10-23, rather than the 2–4 first
  planned). Squashed on the old base with a tree equal to `bridge-sync` at
  `fe924edc` (sync.9 and this doc), then rebased onto trunk, on branch
  `upstream`. The upstream-bound docs no longer point at fork-only ones.
  Fork-only material rides in the same commit; BEFORE-PR.md lists what a
  pull request drops. *If wrong:* the branch loses its fine-grained
  history, which stays reachable through the release tags; every ruling is
  in sync-decisions.md.
- **Release tags never move and are never deleted.** git-board pins them,
  and the Go module proxy caches them. A tag keeps its commit alive, so old
  pins still resolve after a squash. guilbep decides whether the squashed
  series replaces `bridge-sync` (a force push) or lives beside it.
- **Rebase at each upstream release, or sooner when trunk touches
  `bridge/`, `cache/` or `repository/`.** Small rebases are cheaper.
  - Before each rebase, new fork work is folded into the commit
    (`fixup!`).
  - After each one: the tests, gofmt with Go 1.26 and `go generate`
    (whose `api/graphql` drift isn't committed), then the fork's CI and a
    tag.
- **Edits to upstream's files stay small; new code goes in new files.**
  The 26 upstream files the fork modifies are where rebases conflict:
  the first rebase met 7 conflicts in them, and 4 in the deleted
  importers and exporters.

The first rebase (2026-10-23, onto `948e63ce`):

- The fork's early copy of upstream's bridge lock (`58ace3da`) was left
  out (`git rebase --onto origin/trunk <its copy>`). The sync takes
  upstream's lock.
- Conflicts kept both sides: `bridge/core/bridge.go` stores the resume
  time only when the import ran to completion (upstream's `3d9102bb`);
  `repository/gogit.go` pushes to every push URL with go-git's
  `PushContext` and upstream's hint about the native git transport.
- The deleted importers and exporters stay deleted. Upstream's one new
  rule in them, that saving what a forge write created is never cancelled
  (`context.WithoutCancel`), now holds in the engine's push.
- Every cache call takes a context (`fd2268a7`, `04a166d9`, `ec2cc192`):
  the engine passes its own down.
- A new bug is staged now, not committed (`9eccd6f7`). The engine already
  committed after pulling an issue; the tests commit after creating one.
- A cancelled import may drop its events (`3d9102bb`): the test of a
  stopped pull checks only that no resume time is stored.
- sqlite replaced bleve and the excerpt blob (`eaf03671`), and the
  repo-wide cache lock is gone (`a0bb5832`). git-bug's tests pass on both.
- For git-board's next pin bump onto this base: every cache call takes a
  context; `Bugs().New` stages, so `internal/gitbugio/thread.go` must
  commit; free-text search now runs on sqlite, so recheck its results.

## The way to a pull request

guilbep gives the go for every outward step.

1. Once the branch is rebased and green, and not before Muré's breaking
   changes settle (#1628), ask on discussion #1500 or on #1476, citing
   #1223, with a link to sync.md and the answers above. Ask in what shape
   the maintainers would take it. The review (§7) suggests a series:
   standalone fixes, then opt-in foreign export on the existing exporters,
   then the engine.
2. Offer the branch's series of commits (guilbep, 2026-10-24): standalone
   fixes, the engine and fake forge, each adapter, the commands, the docs,
   and fork-only material last, which a pull request drops. Each commit
   builds and passes its tests. Small standalone fixes can go first, as
   pull requests of their own.
3. Upstream's CI runs only on pull requests to trunk. Until then, the
   fork's CI and the local checks stand in for it.

## Consequences

- Rebasing gets easier, and so does review.
- Scope has a test: a change that serves neither the vision nor upstream is
  marked fork-only, or not built.
- Work in flight must land before a squash. After a force push, every
  clone of the fork resets its `bridge-sync`.
- To revisit at each upstream release, and when upstream answers on #1476.
