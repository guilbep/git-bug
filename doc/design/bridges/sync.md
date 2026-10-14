# Bridge sync

Status: **implemented** for GitHub, GitLab and Gitea on branch `bridge-sync`. The
working checklist is in [sync-todo.md](sync-todo.md), and the decisions
taken while building it in [sync-decisions.md](sync-decisions.md).

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
   (author, date, link to the source). **Text written on another forge**,
   whoever posts it, gets `@alice` defused to `` `@alice` `` so it doesn't
   ping whoever is alice on the target, and `#123` translated to the
   mirrored issue's number on the target when git-bug knows it, or to a link
   to the source otherwise. See [Rendering](#rendering).
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
| changed | changed, different | conflict: the later change wins (see below) |

The **base is derived from the git data**, not kept on the side. Every
operation R has seen, because it was pulled from R or pushed to R, is
tagged with write-once metadata for R: the remote id, and the value R holds
after it. For text, that value is a fingerprint (SHA-256) of the text as R
holds it, after `text.Cleanup`; for labels, the full label set. The base
for a value is what the latest R-tagged operation for it records. It
travels with the git data, so any machine can run the sync.

"Later" compares the time of the git-bug operation with the time of the
forge's event, or the forge's last update time when it reports no event.

Text is compared by **fingerprint**, not by undoing the rendering: the
attribution header and translated `#refs` can't be reliably reversed. Pulled
text that did change is turned back into git-bug text on a best-effort basis
(header removed, mentions restored).

## Pull replays, then reconciles

Pull replays the forge's timeline, as the importers do today: each event
becomes its own git-bug operation, with its author and time. History stays
complete, and `bridge pull` keeps behaving as it does.

Events that git-bug pushed itself come back on the next pull. They are
recognized by **claiming**: an event matches a pushed operation of the same
issue, kind and value, not yet claimed, posted by one of the remote's
accounts, no earlier than 5 minutes before the push (clocks differ). The
oldest such operation gets tagged with the event's id, and no new operation
is created. Authorship alone never makes an event an echo, as a token owner
also acts on the forge directly.

After replaying, the engine compares the forge's current state with
git-bug's, three-way as above. A difference that no event explained (for
example a GitLab comment edit, which has no event) is recorded as one
operation.

Push sends a value when it differs from the base, never each intermediate
operation. A local close followed by a reopen, when the base is open,
sends nothing.

## Adapter interface

A forge plugs in by implementing `Remote`, in `bridge/core`. Everything
else is the engine's: which bugs and values to send, with which account,
the attribution header, defusing mentions, translating refs, tagging,
claiming echoes, identities, dry runs and reporting.

```go
// Remote is one forge project, as the sync engine sees it.
type Remote interface {
	// Who it is
	Key() string      // stable project id: "gitlab.com/42", "github.com/git-bug/git-bug"
	Instance() string // the server: "gitlab.com". People are matched per server.
	Accounts(ctx context.Context) ([]Account, error) // configured tokens, and who each one is

	// Read
	Issues(ctx context.Context, since time.Time) iter.Seq2[*Issue, error]
	User(ctx context.Context, id string) (User, error)

	// Write, always as a given account
	CreateIssue(ctx context.Context, as Account, title, body string) (Ref, error)
	AddComment(ctx context.Context, as Account, issue Ref, body string) (Ref, error)
	EditText(ctx context.Context, as Account, issue, target Ref, body string) error
	SetStatus(ctx context.Context, as Account, issue Ref, status common.Status) error
	SetTitle(ctx context.Context, as Account, issue Ref, title string) error
	SetLabels(ctx context.Context, as Account, issue Ref, labels []string) error

	// How @mentions and #refs are written on this forge
	Dialect() Dialect
}
```

- `Ref` is a remote object: its id, URL, and for an issue the number people
  type in references.
- `Issue` holds the current title, description, status, labels and comment
  texts, and the events the forge exposes (comment added or edited, status,
  title, labels), each with its id, author and time. Events can be
  partial: what a forge doesn't report is caught by reconciling.
- A read-only forge (Launchpad) returns `ErrNotSupported` from the write
  methods.
- `EditText` targets either the issue's description or a comment. The
  engine always edits a text with the account that posted it, as GitLab
  only lets a note's author edit it.

Checked against the forges:

| | GitHub | GitLab | Gitea | Jira |
|---|---|---|---|---|
| create issue/comment returns an id | ✅ | ✅ | ✅ | ✅ |
| timeline events with ids | ✅ | ✅ except comment edits | ✅ `/timeline` | ✅ changelog (made-up ids) |
| set labels as a full set | ✅ | ✅ | ✅ | ✅ |
| status | close/reopen | state event | state field | workflow transition, mapped in the adapter |
| who can edit a comment | author or maintainers | author only | author or writers | per permissions |

## The sync run

`git bug bridge sync [--dry-run] [bridge...]`:

1. Takes the repository's bridge lock, from the unmerged `bridge-lock`
   commit (`0cae4552`): a lock file created exclusively, so a concurrent run
   fails at once.
2. Pulls every sync bridge: replay, then reconcile.
3. Pushes every sync bridge, for every bug, three-way. What step 2 pulled
   from one forge reaches the others in step 3, so one run propagates
   everything. Each bug is committed as soon as it's done.
4. Leaves git remotes alone. A CI job runs
   `git bug pull && git bug bridge sync && git bug push`.

A bridge takes part when `git-bug.bridge.<name>.sync` is `true`, which
replaces `export-foreign`. Sync requires a working token for its
`default-login`. Without the option, `bridge pull` and `bridge push` behave
as before: only what a token owner can post, never issues from another
forge.

`--dry-run` reads every remote and prints what pull would record and push
would send, and writes nothing, locally or remotely. It can't show that a
change it would pull from one forge would then be pushed to another in the
same run. What the first backfill would create, which is what it's for, is
exact.

## Errors

- A bad token or configuration fails before anything is written.
- An error on one bug or value is reported, and the run goes on with the
  next one. A refused write (403) is reported for that value only.
- Adapters report rate limits. The engine waits and retries.
- Every issue and comment the engine creates carries an invisible marker,
  `<!-- git-bug:<id> -->`. If a run dies between the forge accepting a write
  and git-bug recording it, the next pull matches the remote item to its
  git-bug item through the marker: it's neither created twice on the forge
  nor imported back as a duplicate. The forge's dialect decides how the
  marker is written, for forges that render HTML comments (Jira).
- When both sides changed a value since the last sync, the later change
  wins, and the run reports it.
- Deletions on a forge are not propagated. The item stops syncing to that
  forge, and the run warns.

## Rendering

Every text in git-bug knows where it was written: on a forge, if it was
pulled from one, or in git-bug itself. Rendering for a target forge
depends on that, and on who posts:

- **Attribution header**, only when the author has no token on the target
  and the text is posted with the bot account:
  - written on a forge: `*Originally posted by Alice on 2026-01-02, [view on GitHub](url)*`
  - written in git-bug: `*Posted by Alice on 2026-01-02 via git-bug*`
- **Mentions and refs** are rewritten by where the text was written,
  whoever posts it. Text written on forge A and posted to forge B gets its
  `@bob` defused to `` `@bob` ``, and its `#123` translated to B's number
  for the same bug, or to a link to A's issue when that bug isn't on B yet.
  Text written in git-bug is left alone: git-bug has no issue numbers.
- Code blocks, inline code, emails and URLs are never rewritten. A text is
  rendered once, when posted: a ref posted as a link isn't edited later when
  its bug gets mirrored.
- Every issue and comment created carries the marker described in
  [Errors](#errors).

When a forge's copy of a text really changed, the pull turns it back into
git-bug text: marker and header removed, defused mentions restored. Refs are
kept as they are: the edit was written on that forge, so they are its refs.

## Identities and tokens

- People are matched per server: an identity is tagged with each server's
  user id (`Instance()` + user id). The legacy `github-login` and
  `gitlab-id` tags are still read.
- For each account (token), the engine finds its identity by user id, then
  by login (which `bridge new` tags today), and adds the user id when
  missing. A pull never creates a second identity for a token owner.
- Text is posted with the author's own account on the target when they have
  a token there, and otherwise with the `default-login` account and the
  attribution header. Status, title and label changes without a token are
  made with the `default-login` account, without attribution.
- A text is always edited with the account that posted it, recorded on the
  operation when pushed.
- The same person on two forges is two git-bug identities. They are not
  merged automatically, as matching people by email or name is risky, and
  git-bug has no way to link identities yet (see #642, #351). Alice's GitHub
  comments reach GitLab through the bot, with the header, even if she has a
  GitLab token. Linking identities can come later without changing the
  engine.

## Migration

Existing repositories keep working, and nothing changes for users who don't
enable `sync`.

- **Data is not rewritten.** Each adapter reads its legacy tags
  (`github-id` and `github-url`; `gitlab-id`, `gitlab-project-id` and
  `gitlab-base-url`), so bugs pulled or pushed by today's bridges count as
  seen by that forge. New tags use the per-project format. Duplicates
  created by today's bridges stay.
- **The GitHub and GitLab bridges become adapters.** `bridge pull` and
  `bridge push` run the engine for that one bridge. Without `sync`, push
  keeps today's limits: never issues from another forge, and only content
  someone has a token for.
- **Jira and Launchpad keep their importer and exporter.** Only a bridge with
  an adapter can be set to `sync`; configuring it on another fails with an
  error.
- **`export-foreign` is removed**, as it was never released, and replaced by
  `sync`. Its attribution code moves into the engine.

## Testing

Three layers, all running offline:

1. **A fake forge**: an in-memory `Remote` with switchable quirks (no event
   ids on writes, author-only edits, partial timelines), to test the engine.
2. **A conformance suite** every adapter runs, against the fake forge, and
   for each real adapter against an HTTP fake of its API (like the GitLab
   one on this branch; GitHub's answers the few GraphQL operations its
   adapter uses). A forge is supported when it passes. Scenarios:
   - a second sync changes nothing, on any side;
   - an issue from A reaches B with its comments, edits, status, title and
     labels, attributed, mentions defused, refs translated;
   - echoes are claimed, never duplicated;
   - a local edit made after a push survives the next sync;
   - an edit made on a forge is pulled;
   - a conflict keeps the later change and is reported;
   - a run that dies after a write and before recording it recovers through
     the marker;
   - token owners don't get a second identity;
   - a deletion on a forge is not propagated;
   - two projects of the same kind sync with each other.
3. **Real forges**: today's bridges were characterized by a real run
   (GitHub `EPFL-ENAC/wiki` to gitlab.epfl.ch, see sync-todo.md), as the
   suite drives the engine and can't run against them. At the end, a two-way
   run against real scratch projects, and the existing end-to-end tests,
   which need tokens.
