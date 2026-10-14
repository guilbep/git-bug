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

## Pull replays, then reconciles

Pull replays the forge's timeline, as the importers do today: each event
becomes its own git-bug operation, with its author and time. History stays
complete, and `bridge pull` keeps behaving as it does.

Events that git-bug pushed itself come back on the next pull. They are
recognized by **claiming**: an event matches a pushed operation of the same
issue, kind and value, not yet claimed, posted by one of the remote's
accounts, at or after the push. The oldest such operation gets tagged with
the event's id, and no new operation is created. Authorship alone never
makes an event an echo, as a token owner also acts on the forge directly.

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

## Still to design

Rendering, identities and tokens, migration of existing data and bridges,
testing.
