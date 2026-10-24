# What the fork missed about what the maintainers said

**Fork-only.** Adversarial review of guilbep/git-bug `bridge-sync` (HEAD `fe924edc`)
against what Michael Muré (MichaelMure) and sudoforge have written upstream. Read-only,
2026-10-23. [fork.md](fork.md) sums it up; [BEFORE-PR.md](../../BEFORE-PR.md) turns it
into a to-do list.

It was written before the squash and rebase onto trunk, so its `file:line` references
point into `fe924edc`. Since then, on branch `upstream`: the rebase is done (fork.md, "The
first rebase"), fork.md's overclaims (gap 10 below) are corrected, and the tests §4 flags as
creating bugs without committing them now commit.

**Legend.** **[V]** = verified in code, git history or a primary source during this
review. **[I]** = inferred: reasoning from verified facts, not run or tested.
Fork paths are on `bridge-sync`. Upstream paths are on `origin/trunk` (`ec2cc192`).

---

## 0. Ranked gaps (most likely to block first)

| # | Gap | What would close it |
|---|---|---|
| 1 | **Timing and scope.** On 2026-10-22 Muré paused the only open new-bridge PR: "I'll keep making breaking changes underneath for some time. Sorry, I don't have the head space to work on a new bridge for now." The fork deletes upstream's GitHub and GitLab importers and exporters, adds Gitea, +21.4k/−2.8k lines. | Don't open the PR yet. Ask on #1476 or discussion #1500, then offer a staged series (§7). |
| 2 | **Breaks the data contract for every GitHub/GitLab user, not only `sync` users.** The engine replaces `bridge pull/push`, writes `sync:<remote>:*` keys and never `github-id`/`gitlab-id`. Upstream binaries then re-import the same issues. Muré: format breaks "would be a massive problem". | Keep writing the spec's well-known keys (`github-id`, `gitlab-id`…) beside the new ones, or keep the old importer/exporter path for non-`sync` bridges. Add the `sync:*` keys and the last-wins reading to git-bug/spec. |
| 3 | **Two bridge architectures.** GitHub, GitLab and Gitea run on the engine. Jira and Launchpad keep the old importer/exporter. Muré counts Jira among the "quality bridges" shipped by default. | Port Jira, or keep the old GitHub/GitLab path until all are ported (same fix as #2). |
| 4 | **Identity keys collide with upstream's plan (PR #1576).** Upstream wants a numeric `github:user-id`, "never the mutable username". The fork uses the GitHub login as the user id under a third key (`sync:github.com:user`), and writes new versions onto existing identities, which v3 forbids for protected ones. | Use GitHub's numeric `databaseId` and upstream's key name. Don't commit to identities you don't own. Coordinate on #1576. |
| 5 | **Label namespaces vs native fields.** `assignee:`/`kind:`/`sprint:` labels, while PR #1526 adds an assignee operation (Muré wants a list) and Muré wants milestones as "their own entity". | Keep the namespaces fork-only. Port assignees onto #1526 when it lands. |
| 6 | **Bot attribution costs aren't named.** It isn't enforced either: `bridge new --sync` doesn't check `default-login`, which is usually the operator's own account. Also: ToS responsibility, 500 content writes per hour, mentions defused, deletions not propagated. | Name the trade-off in fork.md. Refuse or warn when `default-login` is a human token owner. Try forge "sudo" first (unverified, §2). |
| 7 | **Confidential/private data leaks across forges.** The GitLab listing doesn't filter `confidential`, and nothing checks visibility mismatch between forges. | Skip confidential issues. Refuse `sync` from a private project to a public one without a flag. |
| 8 | **GitHub status/labels come only from timeline events.** #609 and #1638 (silent truncation) give wrong values. With sync they become *writes* on the other forges, and reconcile can't catch them. | Read `state` and `labels` from the issue itself, and reconcile against those. |
| 9 | **More commands, against #1223/#1391.** sudoforge wants bridges inside `git bug push/pull` (opt-in config, which Muré agreed to). The fork adds four commands: `sync`, `link`, `unlink`, `take`. | Offer `git bug pull/push` running `SyncAll` behind opt-in config. Keep `bridge sync` as the explicit form. |
| 10 | **Overclaims and stale facts in fork.md.** "Data model untouched", "any clone can run the sync", "four of them changed on trunk" (it's 19). Selective quoting of sudoforge. #779, #1500, #1501, #1227, #1628 and #1576 not cited. | Fix the wording, cite the sources (§1). |

Also worth naming: overlap with open PRs #1628, #1501, #1663 and #1668 (mcepl); the
GitHub remote key isn't stable across repository renames (§5.6); review sync (§5.4).

---

## 1. Every maintainer concern, and whether the fork addresses it

### 1.1 #1223, Muré, objection 1: events without credentials

> "bridges can't push the events that are not owned by a known account (one we have an
> auth for), so (almost always) we can't push everything. This alone prevent bridges from
> being 'transparent'."
> — Muré, https://github.com/git-bug/git-bug/issues/1223#issuecomment-2268697889

**Fork's answer** (fork.md:83-89; sync.md:56-57, 254-260, 281-284; syncpush.go:489-497
**[V]**): use the remote's `default-login` account "on their behalf". Text gets a header,
status, title and label changes get no attribution.

**Classification: a trade-off the fork should name honestly. Partly addressed.**
- Muré said the same thing more precisely twice, and the fork doesn't cite either:
  - #779 and discussion #787 (2022): "you would need credentials for every user on the
    target bug tracker to replicate the bugs **properly**… This should be supported some
    day but will likely require some better story to collect and handle those
    credentials."
    https://github.com/git-bug/git-bug/issues/779#issuecomment-1116355909 ·
    https://github.com/git-bug/git-bug/discussions/787#discussioncomment-2682082
  - Discussion #1500 (2025-12), replying to mcepl, who had posted every ticket as himself
    with the original author in the text: "That's a solution, but it's not the normal
    behavior of a git-bug bridge. If you implement that, it should be an opt-in behavior
    IMHO, a flag or something."
    https://github.com/git-bug/git-bug/discussions/1500#discussioncomment-15162175
- So Muré has accepted the bot-proxy approach **as opt-in**. The fork's per-bridge
  `sync = true` meets that condition **[V]** (bridge_new.go:256-257). This is the fork's
  strongest card and it isn't played: cite #1500.
- In the same thread Muré names two better paths:
  - "It may be that the target bug tracker accept some kind of all-powerful admin
    credential, and in that case we would just need some logic for git-bug to understand
    that fact." (…#discussioncomment-15161176)
  - "Another option would be some kind of bridging Saas where every user grant permission
    and would do the transfer on their behalf." (same comment)
  - The fork does neither (§2).
- mcepl's #1501, "Impersonation while pushing to a bridge"
  (`--wilfully-impersonate-authors`, with an author mapping file and a migration
  header), is open prior art with no maintainer reply. https://github.com/git-bug/git-bug/issues/1501
  The fork doesn't mention it.

### 1.2 #1223, Muré, objection 2: "more problems than it would resolve"

> "git-bug **on-purpose** prevent from pulling from one bridge and pushing to another,
> because that would cause more problems than it would resolve"
> — same comment as above

**Fork's answer** (fork.md:90-104): "Building the sync found those problems… and solved
them". Four technical problems follow: echo loops, one id set per bug, GitLab overwriting
local edits, conflicts.

**Classification: partly addressed. The framing overclaims.**
- Muré never listed "those problems". The list is the fork's own reconstruction.
- When he did name one, it was credentials and user mapping: #779 above, and "you might
  need to make sure that the git-bug users are mapped properly to the remote tracker ID"
  (https://github.com/git-bug/git-bug/discussions/1500#discussioncomment-15161464).
- The four problems are real, and the engine's answers hold up in code:
  - base per remote and per value, read from metadata (synctags.go) **[V]**;
  - echo claiming (syncpull.go:541-620) **[V]**;
  - per-remote ids **[V]**.
- Problems a maintainer would add, which the fork's list leaves out:
  - **Notifications on real people.** Backfill creates about 1,064 GitLab issues for co2
    (sync-todo.md:98-103) **[V]**. The fork does offer `--dry-run`.
  - **Moderation.** "Deletions on a forge are not propagated" (sync.md:245-246) **[V]**.
    Spam removed on one forge stays mirrored on the others, posted by the bot. git-bug
    "doesn't have the notion of deleted comment at all" (Muré,
    https://github.com/git-bug/git-bug/issues/284#issuecomment-570706400).
  - **Confidential issues** (§5.5).
  - **Data-format compatibility** with upstream binaries (§5.1).
  - **Wrong forge state propagated** (§1.9).

### 1.3 #1223, Muré's own proposal: make bridges feel less native

> "the solution is to try to address the confusion directly. Possibly, rename the
> `push`/`pull` bridge operations to `import`/`export` to make then feel less 'native'"

**Not addressed. The fork goes the other way.** It makes bridges the "single source of
truth" path (fork.md:13-17) and adds `bridge sync|link|unlink|take` **[V]**
(commands/bridge/bridge_{sync,link,unlink,take}.go). That isn't wrong, but the fork should
say it diverges from Muré's stated preference instead of implying it answers him.

### 1.4 #1223, sudoforge: transparency means a single push/pull

> "I want to hook those into the lifecycle of `git-bug-push` and `git-bug-pull`, so that
> the `git-bug-bridge-{push,pull}` commands can be removed… there's no reason we can't try
> to push, and emit a warning… it is definitely possible, on a technical level, to
> associate an entity in git-bug with metadata stating that it came from a specific bridge
> configuration, which would support this polyglot syncing approach. **but that would
> require more work. we could keep the enforced limitation of 1 bridge connection in
> place** and still execute on making the import/export workflow transparent."
> — https://github.com/git-bug/git-bug/issues/1223#issuecomment-2271585365

**Not addressed, and selectively quoted.**
- fork.md:106-109 quotes the "technically possible" sentence and leaves out the next two.
  sudoforge would notice.
- The "transparency" in #1223 is about commands: bridges running inside
  `git bug push/pull`. It is not about posting other people's events. The fork answers a
  different question.
- Upstream's later consensus, discussion #1391:
  - Muré: "Maybe the good answer is to have a way to opt-in with a configuration?"
    (https://github.com/git-bug/git-bug/discussions/1391#discussioncomment-12981491)
  - sudoforge: "making sure we weren't auto-pushing by default by making it opt-in seems
    very reasonable to me." (…#discussioncomment-12984139)

### 1.5 #1476, sudoforge: migration between bridged repositories

> "using git-bug to migrate issues between two distinct bridged repositories is not an
> explicitly supported workflow, but should be possible in the future given the flexible
> architecture… on export, issues with URIs which do not match the bridge which we're
> exporting to are removed." … "we can keep this issue open as a feature request for
> supporting migrations"
> — https://github.com/git-bug/git-bug/issues/1476#issuecomment-3065636252 ·
> https://github.com/git-bug/git-bug/issues/1476#issuecomment-3083948080

**Addressed, more than asked.** The request is a one-way *migration*. fork.md:110-111
presents it as demand for N-way live sync.
- A maintainer is far likelier to take an opt-in "foreign export" first. That alone
  answers #1476, #1500 and #1501.
- The live engine would come after it.

### 1.6 Discussion #1193 (cited by #1223): separate workflows

sudoforge: `git-bug-push` and `git-bug-bridge-push` are distinct, and GitHub doesn't show
`refs/bugs`. https://github.com/git-bug/git-bug/discussions/1193#discussioncomment-10219484
(It's a discussion, not an issue: the issue URL 404s.)

**Partly addressed.**
- Sync state travels with `git bug push` (fork.md:39-40) **[V]**.
- The CI recipe is still three commands: `git bug pull && git bug bridge sync && git bug
  push` (sync.md:216-217) **[V]**. The confusion in #1193 remains.

### 1.7 #690 and #1582: push then pull duplicates

- #690: Muré, "This is most likely fixed already"
  (https://github.com/git-bug/git-bug/issues/690#issuecomment-876301732).
- #1582: closed by Muré's #1605, "cache: fix a case where metadata on an operation is
  incomplete" (`254e8b9f`, a cache fix, already in the fork's base) **[V]**. He closed the
  bridge-level PR #1585 as "not the right fix"
  (https://github.com/git-bug/git-bug/pull/1585#issuecomment-5655368538).

**Addressed** for status, title and label echoes through claiming. Comment echo was a
cache bug upstream already fixed.
- sync.md:59-80 still presents the comment duplicates as a bridge limitation.
- It should credit #1605 and narrow the claim to events that carry no id.
- The lesson for the PR: Muré prefers root-cause fixes in core over bridge workarounds.

### 1.8 #367: open/close noise, undo, propagated operations

> "have a way to 'undo', but that implies knowing what *can* be undo as you must not
> remove operation that were already propagated"
> — https://github.com/git-bug/git-bug/issues/367#issuecomment-612370438 ·
> https://github.com/git-bug/git-bug/issues/367#issuecomment-612479216

**Addressed on the forge side.**
- Push sends net values, so a close plus a reopen sends nothing (sync.md:135-139) **[V]**.
- `take` undoes with new operations and never deletes (sync-decisions.md:530-542) **[V]**.

### 1.9 #609 and #1638: GitHub status incoherent, timelines truncated

- #609: closed issues without a `ClosedEvent`. Muré: "Any idea how that's even possible on
  the github side?" https://github.com/git-bug/git-bug/issues/609#issuecomment-809172064
- #1638 (2026-09): about 17% of issues get the wrong status or labels because nested
  `timelineItems` are silently truncated. Muré:
  > "Your workaround would temporarily fix the final state, but that would open more
  > problems in later run of the bridge, as those operations wouldn't match any real
  > event in github."
  > — https://github.com/git-bug/git-bug/issues/1638#issuecomment-5865887463

**Not addressed. Sync makes it worse.**
- **[V]** The GitHub adapter starts every issue `Status: common.OpenStatus`
  (bridge/github/remote.go:269). Only `ClosedEvent`/`ReopenedEvent` items change it
  (:356-361), and labels are rebuilt from label events (:374-381, 463).
- **[V]** `import_query.go` never reads the issue's `state`.
- **[V]** The query keeps the nested shape #1638 shows truncates: `issues(first:…)
  { timelineItems(first:…) }` (import_query.go:59, 82).
- **[I]** Reconcile compares the forge's "current state" with git-bug's. On GitHub that
  state comes from the same partial events, so reconcile can't see the gap. sync.md:177
  ("what a forge doesn't report is caught by reconciling") is false for GitHub status and
  labels.
- **[I]** With sync, a wrong status doesn't stay local:
  1. A GitHub issue closed without a visible event syncs as open.
  2. It is created open on GitLab.
  3. If git-bug once held it closed, a later truncated pull reads as a forge reopen, and
     the next push *reopens it on GitLab*.
- **[V] Muré's #1638 warning applies to the fork's reconcile.**
  - It records a change no event explains as an operation authored by `issue.Author` (or
    the editor) at `issue.Updated` (syncpull.go:651-658, 696-702, 744-747).
  - That is fabricated authorship and time.
  - The base metadata stops the "later run" loop. It doesn't stop the misattribution.
- **Closing step:** select `state`, `stateReason` and `labels` on the issue, and use them
  as the current state. When reconcile has no real actor, author the operation with the
  runner's (or bot's) identity, not the issue author.

### 1.10 #289: one token, many repositories

Muré moved credentials to global storage tagged with metadata (login), so one token
serves every repository. https://github.com/git-bug/git-bug/issues/289#issuecomment-571568293

**Compatible, not affected.** The fork reuses `auth` and selects by login and base URL
**[V]**.
- Side note: credentials are global per user, but `login-<identity>` links and mode keys
  are local per clone (§3). So which tokens and links a sync uses depends on who runs it.

### 1.11 #642: whose identity an import maps to

Muré:
- > "an importer should create as much identities as needed to have the bug reflect the
  > real history" (https://github.com/git-bug/git-bug/issues/642#issuecomment-825732396)
- He agreed the GitHub `MichaelMure` and the GitLab `MichaelMure` may be different
  people.
- He described two scenarios (map the current identity, or create one), and settled on:
  > "ask the user for confirmation before mapping the current identity… otherwise create
  > a new identity" (…#issuecomment-826299164, #issuecomment-826313047)
- > "So, if there is a current user: `Is your current identity TestUser the one you want
  > to associate with $login? [Y/n]`" (…#issuecomment-826322842)

**Partly addressed.**
- **[V]** People are matched per server (sync.md:273-279). The same login on two servers
  isn't merged automatically, which follows Muré's model.
- **[V]** `identitiesFor` silently tags an existing identity found by legacy key or login
  tag with the user id, and commits it (syncidentity.go:56-74). No confirmation is asked,
  where Muré wanted one.
- **[V]** `login-<identity>` links in bridge config map other people (sync.md:287-291;
  syncidentity.go:15-19). That's a new mechanism #642 never discussed (§3).

### 1.12 #351: tagging an existing user with a forge login

> "we might also need a way to either: associate an existing credential to an existing
> user [or] tag an existing user with a login for a specific bridge"
> — https://github.com/git-bug/git-bug/issues/351#issuecomment-598296277

**Partly addressed, in a different place.**
- Muré framed the link as identity metadata ("tag an existing user").
- The fork puts it in per-clone bridge config, on purpose (sync-decisions.md:438-445):
  metadata is write-once, and a wrong link would post someone's text with another
  person's token.
- That's a defensible security argument. It should be argued upstream, not just decided.

### 1.13 #130: protected identities

> "a bridge would only: create an identity without public key (aka *unprotected*)… update
> an unprotected identity… **Once the identity is protected, the bridge can't touch it
> anymore.**"
> — https://github.com/git-bug/git-bug/issues/130#issuecomment-1369164875 (2023)

Plan restated 2026-07 (identity v3/v4, SSH-agent signing, PR #1576):
https://github.com/git-bug/git-bug/issues/130#issuecomment-4881740240

**Not addressed.** See §3: the fork's links and identity tagging will collide with this.

### 1.14 #639: milestones

Muré (2026-09):
> "I want to have strong reasons to include something, as removing a feature is next to
> impossible." … "implementing milestones like github does… it would be: their own entity
> - support title+description+due date+open/close"
> — https://github.com/git-bug/git-bug/issues/639#issuecomment-5845508050 ·
> https://github.com/git-bug/git-bug/issues/639#issuecomment-5845548894

mcepl in the same thread: "no labels `milestone-0.51` doesn't cut it, because labels are
conceptually text and couldn't be compared and ordered."
(https://github.com/git-bug/git-bug/issues/639#issuecomment-5840486493)

**Not addressed. The fork does the opposite.** `sprint:<slug>` labels become GitLab
milestones (sync-decisions.md:330-372) **[V]**, which is the label-shaped milestone both
reject. fork.md:128-138 already marks the namespaces "upstream-optional". Make them
fork-only.

### 1.15 #112, PR #1526, #1562: assignees

- #112: Muré, "Yes that's a feature supported by pretty much all bug tracker… Let's do it
  as well." https://github.com/git-bug/git-bug/issues/112#issuecomment-479179867
- #1562: "the data format is designed for additive changes: new entities… and new
  operations (bug assignee ....) can be added"
  https://github.com/git-bug/git-bug/issues/1562#issuecomment-4586648830
- PR #1526 (open) adds `SetAssigneeOperation` and an `assignee:<user>` query filter. Muré:
  "it would be better to have a **list** of assignees instead of just one. At the very
  least github supports that, so that would make things easier in the bridge."
  https://github.com/git-bug/git-bug/pull/1526#issuecomment-3985338371

**Not addressed. It collides.**
- The fork encodes a *person* as a login string in a label (`assignee:<name>`).
- What the string means is resolved at sync time through per-clone links (§3), not
  through an identity id.
- Upstream's direction is an operation that references identities. Its query filter
  `assignee:` would shadow the fork's label prefix.

### 1.16 #253: bridges from the termui

Muré: "for now, only wire the **default** bridge, we will deal with multiple bridges
later". https://github.com/git-bug/git-bug/issues/253#issuecomment-558796337

**Not addressed. Out of scope.** It shows multi-bridge was always a "later" upstream.

### 1.17 #1227, #349, discussions #1469 and #1391: maintenance burden and new bridges

- sudoforge: "I'm hestitant to add new bridges at all given that it adds to the
  maintenance burden" (https://github.com/git-bug/git-bug/issues/349#issuecomment-2267075506).
- sudoforge: "At the moment, I'm not too interested in accepting additional bridges due
  to the maintenance burden they introduce", citing #1370, a GitLab API break that
  "permanently broke the gitlab bridge in all historical versions"
  (https://github.com/git-bug/git-bug/discussions/1469#discussioncomment-13394305).
- sudoforge: "as long as there's a policy in place that any new bridge should be
  something the community builds and maintains via whatever interface we expose"; "i'd
  really like to rip out launchpad"
  (https://github.com/git-bug/git-bug/discussions/1391#discussioncomment-12984139).
- Muré: "it's really important to ship the quality bridges (github, gitlab, jira) by
  default" (https://github.com/git-bug/git-bug/issues/1227#issuecomment-2268681073).
- Muré: "wait for WIP bridges to mature until inclusion"
  (https://github.com/git-bug/git-bug/discussions/1391#discussioncomment-12981491).
- Muré: "we'll require it being somewhat feature complete to be included by default"
  (https://github.com/git-bug/git-bug/discussions/1469#discussioncomment-13392648).

**Not addressed.**
- An in-tree Gitea bridge runs against sudoforge's stated policy, and it competes with
  #1628.
- Jira, one of Muré's three "quality bridges", is left on the old architecture.

**What the fork can argue, and doesn't:**
- Muré: "the interfacing required between the bridges and git-bug core is massive… I've
  been trying to well define and reduce the APIs scopes, reduce coupling" (#1227).
- The fork's `Remote` interface is forge-only. Adapters touch `cache` only to build
  themselves (`NewRemote(ctx, repo *cache.RepoCache, …)`) **[V]** (bridge/{github,gitlab,gitea}/remote.go imports).
- That is the narrow bridge surface #1227 and #1391 wanted. Pitch it as such.

### 1.18 PR #1628: the pause

> "I think you can pause updating this PR for now, as I'll keep making breaking changes
> underneath for some time. Sorry, I don't have the head space to work on a new bridge
> for now."
> — Muré, 2026-10-22, https://github.com/git-bug/git-bug/pull/1628#issuecomment-6351219155

**Not addressed. Blocking for timing.**
- #1628 is mcepl's 8.6k-line Gitea importer.
- It also carries a `--foreign` push option that "promises to mirror imported bugs" (bot
  review).
- It overlaps the fork's Gitea adapter and its foreign export.

### 1.19 Discussion #1525: what metadata is for

> "metadata are meant to be a behind the scene tooling (for bridges or similar)."
> — https://github.com/git-bug/git-bug/discussions/1525#discussioncomment-15927297

**Supports the fork.** Keeping sync state in operation metadata is the intended use.
- But the published spec defines `SetMetadataOperation` as **first-write-wins**: "the
  first annotation that arrives in operation order persists permanently and cannot be
  overridden" (https://github.com/git-bug/spec/blob/main/bug.md, §5.8) **[V]**.
- The fork reads raw `SetMetadata` operations "in order, the last one winning"
  (sync-decisions.md:9-13; synctags.go:62-75; syncreview.go:247-256) **[V]**.
- That is a mutable register built on an immutable primitive, a semantic the spec
  doesn't have. A spec owner will push back (§5.1).

### 1.20 #1562 and the spec: format stability

> "the data format is stable-ish: people are using it and rely on it, therefore breaking
> change would be a massive problem"
> — https://github.com/git-bug/git-bug/issues/1562#issuecomment-4586648830

The spec moved to git-bug/spec (`564b6112`) **[V]**. Its §6.1 lists the well-known keys
`origin`, `github-id`, `github-url`, `gitlab-id`… **[V]**.

**Not addressed** (§5.1).

### 1.21 #1589: a server, live updates, no repo-wide lock

Muré: "It is a problem I need to solve, if only for the situation where B has a public
facing webui… that's a scenario I want to achieve."
https://github.com/git-bug/git-bug/issues/1589#issuecomment-5728265866

**Not addressed** (§4).

### 1.22 #478: a git-bug issue is not a GitHub issue

sudoforge: "a GitHub Issue and a git-bug issue are not the same thing… a reasonable
solution would be to indicate (in the UIs) that an issue is 'bridged', and show some
metadata from that bridge" (https://github.com/git-bug/git-bug/issues/478#issuecomment-3340319548).

**Consistent.** The fork keeps `<kind>-url` and per-remote numbers **[V]**. Exposing them
in the UIs is a small, upstreamable win.

---

## 2. Is bot posting the transparency Muré meant? No. It's a UX trade-off

"Transparent" in #1223 means bridges hidden inside `git bug push/pull` (§1.4). Muré's
"replicate the bugs **properly**" (#779) means each event under its real author. The bot
does neither. It is the opt-in approximation Muré allowed in #1500. Costs a maintainer
will raise, each checked against the code:

1. **Not enforced.**
   - **[V]** `bridge new --sync` sets `sync=true` without checking whose account
     `default-login` is (commands/bridge/bridge_new.go:240-257).
   - Upstream's `default-login` is the configuring user's own login, used for reads
     (origin/trunk bridge/github/config.go:145, import.go:38) **[V]**.
   - The fork silently changes its meaning to "the account that posts everyone else's
     text". "Use a dedicated account" is only advice (doc/usage/third-party.md:237-240)
     **[V]**, and sync.md:56-57 calls it an assumption.
   - In practice the operator's personal account posts everyone's text. That is exactly
     mcepl's M2Crypto migration, which Muré called "not the normal behavior".
2. **Who is responsible.**
   - GitHub ToS: a machine account's owner "is ultimately responsible for the machine's
     actions", and "You may maintain no more than one free machine account in addition
     to your free Personal Account"
     (https://docs.github.com/en/site-policy/github-terms/github-terms-of-service) **[V]**.
   - The bot owner answers for every mirrored spam or abusive comment.
   - Deletions aren't propagated (sync.md:245-246) **[V]**, so moderation on one forge
     doesn't reach the others.
3. **Rate limits.**
   - GitHub: "no more than 80 content-generating requests per minute and no more than 500
     content-generating requests per hour"
     (https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api)
     **[V]**.
   - The engine waits one minute on a secondary limit and retries three times
     (sync-decisions.md:80-87) **[V]**.
   - **[I]** An hourly cap won't clear in a minute: a co2-sized backfill to GitHub (a
     thousand issues, each with comments, status and labels) runs into several runs and
     hours. Fine, but say so. "A first sync of thousands of writes may need several runs"
     understates it.
4. **Attribution is lossy on the target.**
   - Status, title and label changes by people without a token are the bot's, with no
     attribution (sync.md:281-284) **[V]**.
   - Edits go through whoever posted the text (sync.md:285-286) **[V]**. An edit by Bob of
     a comment posted for Alice keeps "Originally posted by Alice" and names no editor
     **[I]**.
   - Reactions aren't synced (no `reaction` anywhere in bridge/ **[V]**; git-bug has none).
5. **Cross-forge mentions die.**
   - `@alice` written on forge A is defused on B (render.go:63-66) **[V]**. That's right
     against pinging a stranger.
   - But someone who exists on both forges, linked, is never pinged across forges either.
     Replies on B don't notify A's author.
   - Name it: the price of safety is no cross-forge notifications.
6. **A single, powerful token.**
   - Private repositories need the `repo` scope (third-party.md:116-119) **[V]**, which on
     a classic PAT also grants write access to code.
   - One CI secret then holds write access to every synced repository.
   - The same paragraph, inherited from upstream, says pull-request scope is "not used or
     required". Review sync writes PR comments and resolves threads (syncreview.go)
     **[V]**, so the paragraph is now wrong for git-board users.
7. **A hidden marker in every item.** `<!-- git-bug:<64-hex id> -->` (render.go:13-25)
   **[V]**. It's invisible in rendered views, visible in raw markdown, edit boxes, API
   consumers and some email clients **[I]**.
8. **The better path Muré named isn't tried.**
   - "all-powerful admin credential" (#1500).
   - **Unverified:** GitLab admin personal access tokens with the `sudo` scope can act as
     another user (the `Sudo:` header), and Gitea admin tokens support `sudo` too.
   - Where available, that gives real attribution on self-hosted forges and leaves the bot
     only as the fallback. The fork's target (a self-hosted GitLab) is exactly where it
     could apply.
   - Longer term, upstream's PR #1576 ADR 0004 adds OAuth logins on a public web UI. That
     is the "better story to collect and handle those credentials" of #779, and it would
     enable per-user tokens **[I]**.

What a maintainer would push back on: the silent repurposing of `default-login`; no guard
against a personal account; spam and moderation; and calling this "answered" when Muré
asked for per-user credentials or admin impersonation.

---

## 3. Identity: one person on two forges, and people on only one

**What the maintainers said.**
- Importers create as many identities as the history needs. The same login on two forges
  may be two people. Map to the current identity only with confirmation (§1.11).
- A bridge must not touch a protected identity (§1.13).
- From upstream's WIP identity work (PR #1576, files at head `875cf794`; read via the API)
  **[V]**:
  - design-auth.md: "The External Account Reference key format is `"<provider>:user-id"`…
    The value is always the provider's numeric/opaque user ID… never the mutable
    username."
  - open-threads.md, "Bridge numeric user ID gap (independent fix, blocks identity
    unification)": "Username changes break the lookup — the bridge will create a
    duplicate identity… OAuth and bridge imports use different keys."
  - ADR 0003: an AdminIdentity may sign "operations attributed to any *unprotected*
    Identity… Admin impersonation cannot bypass a protected Identity's key chain", and
    "bridge import tools and CI/CD bots can use it".
  - ADR 0004 Phase 4: ProjectConfig "absorbs other currently-local config: … bridge
    configuration".
  - Muré on #1516: "what about having a permanent way to configure sources of identities?"
    (https://github.com/git-bug/git-bug/discussions/1516#discussioncomment-15757873).

**What the fork does, against that.**
1. **The GitHub login stands in for the user id.**
   - `core.User{ID: string(a.Login), …}` (bridge/github/remote.go:534, 550) **[V]**.
   - It's ruled as such in sync-decisions.md:33-34 ("a renamed GitHub user becomes a new
     identity") **[V]**.
   - Stored under `sync:github.com:user` (synctags.go:24-26) **[V]**: a *third* key format,
     after the legacy `github-login` and upstream's planned `github:user-id`.
   - This is the gap upstream says "blocks identity unification", entrenched.
   - Fix: GitHub's numeric `databaseId`, and upstream's key name.
2. **The engine writes to identities it doesn't own.**
   - `identitiesFor` calls `i.SetMetadata(key, u.ID); i.Commit()` on existing identities
     found by legacy key or login tag (syncidentity.go:56-74) **[V]**.
   - Under #130 and v3 that fails for a protected identity **[I]**. It also writes a new
     identity version on someone else's behalf, the thing #642 wanted confirmed.
3. **Links put teammates' forge accounts on their own identities.**
   - `login-<identity> = <login>` (sync.md:287-291) **[V]**.
   - Pulled comments by Bob on GitLab become operations *authored by Bob's real git-bug
     identity*, created by the runner.
   - Once Bob protects his identity (v3), those operations need Bob's signature, and the
     runner can't sign them **[I]**.
   - ADR 0003's AdminRole covers only unprotected identities. So the link model is the
     part of the fork most exposed to upstream's identity work.
4. **Links are per-clone and keyed by lowercase login.**
   - The lookup is `e.links[strings.ToLower(u.Login)]` (syncidentity.go:15-19) **[V]**.
   - A forge rename breaks the link.
   - Links don't travel (sync-decisions.md:442-444) **[V]**. Neither do the `assignees`,
     `kinds` and `sprints` modes, which "Every clone must hold… the same way, or each sync
     undoes the others' and notifies people" (sync-decisions.md:268-279) **[V]**.
   - That contradicts fork.md:39-40, "**Any clone can run the sync.**" The *records*
     travel. The *interpretation* (links, modes, which tokens exist) does not.
   - Two clones with different links resolve `assignee:bob` to different forge users
     **[I]**.
   - Upstream's answer to that class of problem is ProjectConfig. The fork should say it
     waits on it.
5. **A wrong link is permanent in history.** Operations pulled under the wrong identity
   stay in git-bug's append-only history **[I]**, and there is no identity merge or unmerge
   (sync.md:290-291).

**People on only one forge (verified behaviour).**
- They get an unprotected identity created by the pull, tagged per server.
- Their text reaches other forges via the bot with a header.
- Their status and label changes appear as the bot's.
- Their assignments on a forge where they're "no one" are refused, with one warning to the
  runner (sync-decisions.md:188-199) **[V]**.
- **[I]** In native mode the assignment is invisible on that forge: no assignee, no
  label. The runner gets "Nothing lost silently", but the forge's users still lose it
  silently. `assignees = labels` avoids that, at the cost of real assignees.

**Verdict.** Links in local config, not in email or login heuristics, are safer than
upstream's legacy login tag, and the security argument is sound. Left open:
- numeric ids;
- protected identities;
- links and modes that should live in shared project config;
- confirmation before tagging an identity, per #642;
- no merge or unmerge.

---

## 4. Do the fork's answers survive trunk's direction?

41 trunk commits since `824430a5` **[V]** (fork.md:147 says 27).

- **No bridges inside `git bug push/pull` yet** **[V]**: origin/trunk commands/push.go
  and pull.go don't mention bridges. So trunk doesn't undercut `bridge sync` today.
  - The *stated* direction is still opt-in bridges inside push/pull (#1391 consensus, §1.4).
  - `1c4e7892` adds `git-bug.git-transport=git` (native git push/pull) **[V]**. That is
    git-bug pulling closer to git, not to bridges.
  - Native git also pushes to every `pushurl`. Part of the fork's `repository/gogit.go`
    pushurl fix (`f8f4521b`) is now redundant for that mode. It's still useful for go-git
    **[I]**.
- **"git-bug's data model is untouched"** (fork.md:48-50).
  - Literally true **[V]**: no diff under `entities/ entity/ cache/ api/`.
  - Substantively an overclaim: it adds new metadata keys to shared data, a last-wins
    reading of a first-wins primitive, and stops writing the spec's well-known keys
    (§5.1).
  - Trunk is reshaping exactly this layer: `27ac4c51` "edits in operation metadata are not
    shared in cache", #1605, "cache: change the paradigm", sqlite cache.
- **`9eccd6f7` "bug creation is not special anymore"** **[V]**: `NewRaw` now stages the
  creation, "Until then, the bug is only known to the returned view".
  - Production pull commits at the end of `pullIssue` (syncpull.go:110-180) **[V]**. An
    error mid-issue now drops the new bug instead of leaving a half bug. That's benign:
    the issue is re-read next run **[I]**.
  - **Tests change meaning** **[V]**:
    - `TestPushErrorOnOneBugOnly` (sync_test.go:745-764) creates two bugs with `NewRaw`
      and never commits. **[I]** On trunk the push sees no bug, and the test fails.
    - The test at sync_test.go:546-553 creates "foreign" and "not mine" bugs without
      committing. **[I]** On trunk it still passes, but vacuously: those bugs never exist.
- **Context wiring** (`fd2268a7`, `04a166d9`, `ec2cc192`) **[V]**:
  - `SetMetadata(ctx, …)` and `Commit(ctx)` now take a context (origin/trunk
    cache/bug_cache.go:165, cache/cached.go:151).
  - The fork's new core files hold about 49 such call sites (`Commit`, `SetMetadata`,
    `*Raw`) **[V]** (git grep count).
  - git-board's contract (`SyncAll`, `SyncIssue`, `Link`…) changes signature with them.
- **Rebase conflict surface.**
  - fork.md:181 says "Today four of them changed on trunk too".
  - **[V]** It's **19** files changed by both, including four **modify/delete**
    conflicts: trunk edited `bridge/{github,gitlab}/{import,export}.go`, which the fork
    deletes.
  - Plus `bridge/core/bridge.go` (+86/−8 on trunk), `bridge/gitlab/config.go` and
    `repository/gogit.go` (+153/−152).
- **`a0bb5832` removes the repo-wide cache lock**: "multiple UIs can now interact on the
  same repo safely" **[V]**.
  - With #1589 (live server) and `c2083ede` (web UI subscriptions), Muré's direction is a
    long-running git-bug process.
  - **[I]** That makes a continuously running sync inside the server natural.
  - The fork's design is a batch CLI or CI job with "one runner" and a file lock, plus a
    long-lived engine in git-board (sync-decisions.md:142).
  - The concurrency assumptions should be re-checked once the cache lock is gone.
- **`950e17ca`** (GraphQL mutations as one unit of work and operation pack): no conflict.
  Combined with `9eccd6f7`, it lets a pulled issue be created with its first operations in
  one commit, which helps the fork **[I]**.

---

## 5. What else a reviewer of a ~21k-line PR would raise

### 5.1 Data-format compatibility (likely blocking)
- **[V]** `bridge pull/push` on GitHub and GitLab now run the engine for *every* user:
  `NewImporter`/`NewExporter` return `nil` (bridge/github/github.go:41-51), and the old
  files are deleted.
- **[V]** New bugs get `sync:<remote>:id|number|url|title|status|labels|text`, `origin`
  and `github-url` (syncpull.go:319-349). They never get `github-id` (read-only at
  remote.go:955-962).
- sync-decisions.md:24-28 admits it: older versions "import synced issues again… so it's
  documented instead".
- **[I]** So a team where one member runs upstream git-bug gets duplicates with no one
  opting into sync. So do third-party readers of the format, like gitarena from #1562.
- That conflicts with Muré's "breaking change would be a massive problem" and with the
  spec's §6.1 table.

### 5.2 Two architectures and Jira
The engine (with conformance suite) serves GitHub, GitLab and Gitea. The old
importer/exporter serves Jira and Launchpad (fork.md:57-58; sync.md:307-309) **[V]**.
Reviewers maintain both, and `RemoteBridge` adds a second bridge interface beside
`Importer`/`Exporter` (bridge/core/interfaces.go) **[V]**.

### 5.3 Label namespaces
`assignee:`/`kind:`/`sprint:` (§1.14, §1.15) bring three per-project modes, migrations
"after one full pull", and GitHub org issue-type mapping. All of it is git-board-shaped
policy in core. fork.md:128-138 half-concedes this. Make it fork-only.

### 5.4 Pull-request review sync
`review:*`, `PullRemote` and `syncreview.go` (555 lines) **[V]** mirror git-board's
review comments to GitHub PRs as *bug comments*.
- Muré wants code review as its own entity (#1391: "the plan was very much to add support
  for code review (pull-request) and other custom entities").
- sudoforge doesn't want review in git-bug at all (same thread).
- Either way, review data on bug comments won't be accepted. Keep it fork-only (already
  "upstream-optional").

### 5.5 Confidential issues and visibility (security)
- **[V]** GitLab issues are listed with `UpdatedAfter`, `Scope: all` and `Sort` only. No
  `Confidential` filter (bridge/gitlab/remote.go:34-38). No confidential, private or
  visibility handling anywhere in bridge/ (grep).
- **[I]** A confidential GitLab security report is mirrored by the bot to a public GitHub
  repository, with notifications.
- Upstream's single bridge only imports into refs/bugs. The fork *publishes*.
- A maintainer would block on this.

### 5.6 GitHub and Gitea remote keys aren't stable
- **[V]** `Key()` is `"github.com/" + lower(owner/project)` (bridge/github/remote.go:152)
  and `instance/owner/project` for Gitea. GitLab uses the numeric project id.
- sync.md:152 calls it a "stable project id". git-bug itself moved from
  MichaelMure/git-bug to git-bug/git-bug.
- **[I]** After a rename or transfer whose new name is put in the bridge config, every
  metadata key changes. GitHub-native issues (no marker) import as new bugs, then get
  pushed to the other forges as duplicates.
- Fix: key by repository node id.

### 5.7 Overlap with open upstream work
- #1628 and #1565: Gitea, and `--foreign`.
- #1501: impersonation flag.
- #1663: credential selection and dangling identities.
- #1668: "explain skipped issues on push".
- #1526: assignees.
- #1576: identities.

mcepl is the most active bridge contributor upstream right now. Talk to him before
competing.

### 5.8 Size and review units
fork.md:205-207 plans one PR of 2-4 commits. At +21.4k lines (including 1,250 lines of
working plans), that's unreviewable against "I don't have the head space". Muré on #639:
"implementing this should be done carefully and step by steps (spec first, data model,
cache, UIs, ...)". That's the template.

---

## 6. What the fork gets right (lead with these)

- The narrow, forge-only `Remote` interface, plus a fake forge and conformance suite.
  That's the reduced coupling Muré said he's after (#1227) **[V]**.
- Echo claiming and three-way bases fix real, long-standing upstream bugs: status echoes
  and GitLab overwriting unpushed edits **[V]** (design; engine tests not run here).
- Opt-in per bridge, and `--dry-run`, which meets Muré's #1500 condition **[V]**.
- Defused mentions, translated refs, and markers for crash recovery are careful
  engineering upstream lacks.
- Standalone fixes (GitLab token-owner duplicate identity, base-URL trailing slash,
  push-URL handling) can go up on their own.

---

## 7. A path that maintainers could accept

1. **Ask first**, on discussion #1500 (where Muré set the opt-in condition) or #1476. Link
   sync.md and acknowledge #1501 and #1628. Ask whether they want foreign export now, and
   in what shape, given the ongoing breaking changes.
2. **Series A, standalone fixes**, each its own PR: GitLab identity duplicate, base-URL
   normalization, push URLs (if still wanted after `git-transport`), docs.
3. **Series B, opt-in foreign export** on the *existing* exporters: post with an
   admin/sudo token where the forge supports it, else a declared bot with a header, plus
   mentions, refs, markers and confidential filtering. This answers #1476, #1500 and #1501
   in a few hundred lines.
4. **Series C, the engine**, behind `sync` only, writing the spec's legacy keys too. Each
   adapter in its own PR. Spec additions first (git-bug/spec), identity keys aligned with
   #1576.
5. **Keep fork-only**: label namespaces, review sync, git-board contract, Gitea (until
   #1628 resolves).

---

## Appendix: how claims were checked

- Issues, PRs and comments fetched with `env -u GH_TOKEN gh issue/pr view … --comments`.
- Discussions fetched with `gh api graphql`.
- PR #1576 design files read through the contents API at `875cf794`.
- Spec read from github.com/git-bug/spec (bug.md §5.8, §6).
- GitHub ToS and rate limits fetched from docs.github.com.
- Code read with `git show bridge-sync:<path>` and `git show origin/trunk:<path>`.
- Overlap computed from `git diff --name-status 824430a5 bridge-sync` against
  `git diff --name-only 824430a5 origin/trunk`.
- No test was run. Every behaviour marked [I] (post-rebase test results, rename
  duplicates, status propagation, v3 signing failures) is reasoning from the code and
  diffs cited, not observed.
