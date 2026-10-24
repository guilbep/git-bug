# Bridge sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Two-way issue sync between git-bug and any number of forge projects,
through one engine in `bridge/core` and thin per-forge adapters, starting with
GitLab and GitHub, then Gitea.

**Architecture:** A `Remote` interface (one forge project) is implemented by
each adapter. The engine pulls (replays a forge's timeline, claims echoes of
its own pushes, reconciles the rest three-way) and pushes (sends current
values that differ from what the forge last saw). What each forge has seen is
recorded as write-once metadata on git-bug operations, so the sync state
travels with the git data.

**Tech Stack:** Go 1.26, git-bug's `cache` and `entities/bug` packages,
`gitlab.com/gitlab-org/api/client-go`, `github.com/shurcooL/githubv4`,
`net/http/httptest` for API fakes, `testify/require`.

**Spec:** [sync.md](sync.md). Progress is tracked in [sync-todo.md](sync-todo.md).

## Global Constraints

- One branch (`bridge-sync`), one PR. Commit after each task, push to `fork`.
- Without `git-bug.bridge.<name>.sync true`, `bridge pull` and `bridge push`
  behave as before: no issue from another forge is pushed, and only content
  someone has a token for.
- Existing data is never rewritten. Legacy `github-*`/`gitlab-*` metadata
  stays readable.
- Metadata keys are written once per operation (git-bug keeps the first value).
- Text fingerprints: SHA-256 of the text after `text.Cleanup`, hex encoded.
- Echo claiming window: an event may be claimed by a push no more than
  5 minutes after the event.
- Commit messages end with
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Use stdlib `errors` (the repo dropped `pkg/errors`). Match surrounding code.
- Tests close the cache (`defer backend.Close()`), use
  `repotest.NewGoGitRepo(t, false)` and `cache.NewRepoCacheNoEvents`.

## Review Focus

1. A forge user edits a mirrored comment while the same comment was edited
   in git-bug: the later edit wins on both sides, and nothing loops.
2. A run killed right after `CreateIssue` succeeds: the next run neither
   creates the issue again nor imports it as a new bug (marker).
3. Labels changed on two forges between syncs (add on A, remove on B): the
   result is the union of the changes, no label flaps between runs.
4. A token owner who also acts directly on the forge: their own close is
   imported, not swallowed as an echo.
5. Text with `#123` where 123 is an issue never mirrored, or a cross-project
   `group/proj#5`: rendered as a link to the source, and never rewritten on
   later runs.

Each has a scenario in Task 11's conformance suite.

---

## File structure

```
bridge/core/
  remote.go          Remote interface and its types (Task 2)
  synctags.go        metadata keys, fingerprints, base derivation (Task 3)
  render.go          mentions (done), refs, attribution, marker (Task 4)
  syncidentity.go    remote users and accounts → identities (Task 6)
  syncpull.go        engine: pull (Tasks 7, 8)
  syncpush.go        engine: push (Tasks 9, 10)
  sync.go            Engine type, options, results, dry run (Tasks 7, 12)
  bridge.go          use the engine when the impl provides a Remote (Task 12)
  synctest/
    fake.go          in-memory forge implementing Remote + Forge (Task 5)
    conformance.go   scenarios every adapter must pass (Task 11)
bridge/gitlab/remote.go, remote_test.go   GitLab adapter (Task 14)
bridge/github/remote.go, remote_test.go   GitHub adapter (Task 15)
bridge/gitea/...                          Gitea adapter (Task 18)
commands/bridge/bridge_sync.go            `git bug bridge sync` (Task 13)
```

## Data model (shared by all tasks)

Remote key `K` = `Remote.Key()`, e.g. `gitlab.epfl.ch/41960`,
`github.com/EPFL-ENAC/wiki`. Instance `I` = `Remote.Instance()`.

Operation metadata, each key written once:

| key | on | value |
|---|---|---|
| `sync:K:id` | any op | the forge object or event this op is (issue, comment, event id) |
| `sync:K:number` | create op | issue number, for `#refs` |
| `sync:K:url` | create, comment ops | web URL |
| `sync:K:pushed` | pushed op | unix time of the push, for claiming |
| `sync:K:as` | create, comment ops | login of the account that posted the text |
| `sync:K:title` | create, set-title ops | title as the forge holds it |
| `sync:K:status` | create, set-status ops | `open` or `closed` |
| `sync:K:labels` | create, label ops | full label set, sorted, `\n`-joined |
| `sync:K:text` | create, comment, edit ops | fingerprint of the text as the forge holds it |

| `sync:K:also:<event id>` | claimed op | id of the push record that claimed the event |

Identity metadata: `sync:I:user` = the forge user id.

**Records.** git-bug keeps only the first value of a key on an operation, so
pushes, claims and convergences are written as SetMetadata operations, and
read back from them: a push record is a SetMetadata operation whose new
metadata holds `sync:K:pushed` and the value pushed. Pulled operations hold
their values in their own metadata.

**Slots.** A value that syncs: `title`, `status`, `labels`, and `text` of a
create or add-comment op. The base of a slot for K is the last record of it,
in operation order: a pulled op's own metadata, or a SetMetadata operation
targeting an op of the slot.

---

### Task 1: Bridge lock

**Files:** cherry-pick `0cae4552` (modifies `bridge/core/bridge.go`, adds
`bridge/core/lock_test.go`, touches `bridge/launchpad/import.go`).

- [ ] `git cherry-pick 0cae4552`, resolve conflicts with this branch's
      `bridge.go` if any.
- [ ] `go test ./bridge/core/` passes, including `TestLock*`.
- [ ] Commit (cherry-pick keeps the maintainer's authorship). Push.

### Task 2: Remote interface and types

**Files:** Create `bridge/core/remote.go`.

**Produces:**

```go
var ErrNotSupported = errors.New("not supported by this forge")

// ErrGone is returned when the issue or comment no longer exists on the forge.
var ErrGone = errors.New("deleted on the forge")

// ErrRateLimited is returned by a Remote when the forge asks to slow down.
type ErrRateLimited struct{ Until time.Time }

func (e ErrRateLimited) Error() string

type Remote interface {
	Key() string
	Instance() string
	Kind() string // "github", "gitlab": origin metadata and attribution wording
	Accounts(ctx context.Context) ([]Account, error)
	Issues(ctx context.Context, since time.Time) iter.Seq2[*Issue, error]
	User(ctx context.Context, id string) (User, error)
	CreateIssue(ctx context.Context, as Account, title, body string) (Ref, error)
	AddComment(ctx context.Context, as Account, issue Ref, body string) (Ref, error)
	EditText(ctx context.Context, as Account, issue, target Ref, body string) error
	SetStatus(ctx context.Context, as Account, issue Ref, status common.Status) error
	SetTitle(ctx context.Context, as Account, issue Ref, title string) error
	SetLabels(ctx context.Context, as Account, issue Ref, labels []string) error
	Dialect() Dialect
	// Legacy reports the remote id an operation carries in this bridge's
	// pre-sync metadata, if any (github-id, gitlab-id...).
	Legacy(op dag.Operation, createMetadata map[string]string) (id string, ok bool)
	// LegacyUser is the identity metadata key pre-sync importers used, and
	// whether it holds the login (true) or the user id (false).
	LegacyUser() (key string, isLogin bool)
}

type Ref struct {
	ID     string
	URL    string
	Number string // issues only: what people type after '#'
}

type User struct {
	ID, Login, Name, Email, AvatarURL string
}

// The Remote keeps the tokens and picks one from the account's ID
// (bridge/core/auth imports bridge/core, so no token type here).
type Account struct {
	User
}

type Issue struct {
	Ref
	Author        User
	Created       time.Time
	Updated       time.Time
	OriginalTitle string // title at creation, == Title when unknown
	Title         string
	OriginalBody  string // body at creation, == Body when unknown
	Body          string
	BodyEditor    *User  // last editor of the body, when known
	Status        common.Status
	Labels        []string
	Comments      []Comment
	Events        []Event // time ordered
}

type Comment struct {
	Ref
	Author   User
	Created  time.Time
	Updated  time.Time
	Original string // text at creation, == Body when unknown
	Body     string
}

type EventKind int

const (
	EventStatus EventKind = iota + 1
	EventTitle
	EventLabels
	EventTextEdit
)

type Event struct {
	ID      string
	Kind    EventKind
	Author  User
	Time    time.Time
	Status  common.Status // EventStatus
	Title   string        // EventTitle: the new title
	Added   []string      // EventLabels
	Removed []string      // EventLabels
	Target  string        // EventTextEdit: "" for the body, else the comment id
	Text    string        // EventTextEdit: the text after the edit
}

type Dialect struct {
	// Ref matches an issue reference of this forge; group 1 is the number.
	// Cross-project references must not match.
	Ref *regexp.Regexp
	// FormatRef writes a reference to issue number n of this project.
	FormatRef func(n string) string
}
```

- [ ] Write the file. `go build ./bridge/...`. Commit.

### Task 3: Tags, fingerprints, bases

**Files:** Create `bridge/core/synctags.go`, `bridge/core/synctags_test.go`.

**Produces:**

```go
func Fingerprint(text string) string              // sha256 of text.Cleanup(text)
func syncKey(remoteKey, name string) string       // "sync:K:name"
func userKey(instance string) string              // "sync:I:user"

type slot struct{ kind string; target entity.Id } // kind: title|status|labels|body|comment
// base returns the value K last saw for the slot, and the op that recorded it.
func base(snap *bug.Snapshot, r Remote, s slot) (value string, op dag.Operation, ok bool)
// remoteIssueID returns the issue id a bug has on K (sync tag, or legacy).
func remoteIssueID(snap *bug.Snapshot, r Remote) (string, bool)
// opOnRemote returns the remote id an op has on K (sync tag, or legacy).
func opOnRemote(op dag.Operation, snap *bug.Snapshot, r Remote) (string, bool)
func labelsValue(labels []string) string          // sorted, \n joined
```

Legacy rule: an op with a legacy id for K counts as seen, with its own value
(title, status, `Fingerprint(message)`, and for labels the snapshot's label
set right after that op).

- [ ] Tests first, on snapshots built with `bug.NewCreateOp`, `NewSetTitleOp`,
      `NewSetStatusOp`, `NewLabelChangeOperation`, `NewAddCommentOp`,
      `NewEditCommentOp` and `op.SetMetadata`:
  - no tag → `ok == false` for every slot;
  - title set twice, first tagged → base is the first title;
  - text slot of a comment edited twice, edit 1 tagged → edit 1's fingerprint;
  - legacy-only op (fake Remote whose `Legacy` returns ids) → counted;
  - `Fingerprint("a\r\n")` == `Fingerprint("a")`.
- [ ] Implement, `go test ./bridge/core/ -run Tags`. Commit.

### Task 4: Rendering

**Files:** Modify `bridge/core/render.go`, `bridge/core/render_test.go`.

**Consumes:** `Dialect`, `Remote` (Task 2), `remoteIssueID` (Task 3).

**Produces:**

```go
const markerPrefix = "<!-- git-bug:"
func Marker(id entity.Id) string                         // "<!-- git-bug:<id> -->"
func MarkerID(text string) (entity.Id, bool)             // first line marker
type Attribution struct{ Name string; When time.Time; Source, URL string } // Source "" = written in git-bug
// Render writes text for target. from is the forge it was written on (nil
// for git-bug); refs maps a source issue number to the target ref, or "" if
// unknown, then link to sourceURL(n).
func Render(text string, id entity.Id, attr *Attribution, from, target Remote,
	refs func(n string) (targetNumber string, ok bool), sourceURL func(n string) string) string
func Unrender(text string) string                        // marker + header removed, mentions restored
```

Header line: `*Originally posted by NAME on YYYY-MM-DD, [view on SOURCE](URL)*`,
or `*Posted by NAME on YYYY-MM-DD via git-bug*`. The marker is the first line.

- [ ] Tests: marker round trip; header both forms; `#12` known →
      target's `FormatRef`; unknown → `[#12](sourceURL)`; `group/x#12`
      untouched; `#12` inside inline code and fences untouched; text from
      git-bug (`from == nil`) keeps mentions and refs; `Unrender(Render(x))`
      == x for texts without refs; empty body.
- [ ] Implement with `mapProse` (exists). Commit.

### Task 5: Fake forge

**Files:** Create `bridge/core/synctest/fake.go`, `fake_test.go`.

**Produces:**

```go
// Forge simulates people acting directly on a forge. The conformance suite
// drives adapters through it.
type Forge interface {
	core.Remote
	UserAct(login string) Actor
	// Issue returns the forge's current view of an issue, by number.
	Issue(number string) *core.Issue
	Writes() int // number of write calls received
}
type Actor interface {
	CreateIssue(title, body string) core.Ref
	Comment(issue core.Ref, body string) core.Ref
	EditText(issue, target core.Ref, body string)
	SetStatus(issue core.Ref, s common.Status)
	SetTitle(issue core.Ref, title string)
	SetLabels(issue core.Ref, labels []string)
	Delete(issue core.Ref)
}
type Quirks struct {
	NoEditEvents    bool // GitLab: edits produce no event
	AuthorOnlyEdits bool // GitLab: only a comment's author may edit it
	FailAfterWrite  bool // next write succeeds remotely then returns an error
	RateLimitOnce   bool // next write returns ErrRateLimited{Until: now+1s} once
}
func NewFake(key, instance string, q Quirks, accounts ...core.Account) *Fake
```

- [ ] Tests on the fake alone: actor actions show in `Issues()` with events;
      engine-style writes produce events authored by the account; quirks
      behave. Commit.

### Task 6: Identities

**Files:** Create `bridge/core/syncidentity.go`, test.

**Produces:**

```go
// identityFor finds or creates the identity of a forge user: by sync:I:user,
// then by the legacy key, then (accounts only) by the login tag bridge new
// writes; tags the user id when missing.
func identityFor(ctx context.Context, repo *cache.RepoCache, r Remote, u User) (*cache.IdentityCache, error)
// accountFor returns the account that posts as id on r, if any.
func accountFor(accounts map[entity.Id]Account, id entity.Id) (Account, bool)
func loadAccounts(ctx context.Context, repo *cache.RepoCache, r Remote) (map[entity.Id]Account, error)
```

- [ ] Tests: existing identity tagged only with the login → found and tagged,
      no new identity; unknown user → created once, found the second time;
      two identities with the same login (left by old bridges) → both map to
      the account. Commit.

### Task 7: Engine skeleton and pull of new issues

**Files:** Create `bridge/core/sync.go`, `bridge/core/syncpull.go`, tests in
`bridge/core/sync_test.go` (using `synctest.NewFake`).

**Produces:**

```go
type SyncOptions struct {
	Foreign bool // the bridge has `sync` enabled
	DryRun  bool
	Default string // default-login
}
type Engine struct{ repo *cache.RepoCache; remote Remote; opts SyncOptions; accounts map[entity.Id]Account }
func NewEngine(ctx context.Context, repo *cache.RepoCache, r Remote, opts SyncOptions) (*Engine, error)
func (e *Engine) Pull(ctx context.Context, since time.Time) <-chan ImportResult
```

New issue: find the bug by `sync:K:id`, legacy, or the body's marker; else
create it from `OriginalTitle`/`OriginalBody` with `origin=Kind()` and tags,
add comments from `Original`, replay `Events` as ops tagged with their ids,
then reconcile (Task 8). Comments are matched by `sync:K:id`, legacy, or
marker.

- [ ] Tests: an issue with 2 comments, a close, a rename and a label is
      imported with the right authors, times and tags; pulling twice changes
      nothing; an issue whose body carries a marker of an existing bug is
      attached to it (tagged), not duplicated. Commit.

### Task 8: Pull of known issues: replay, claim, reconcile

**Files:** Modify `syncpull.go`, tests.

Claiming: an event of kind/value matching an op tagged `sync:K:pushed`
without `sync:K:id`, posted by one of the accounts, with
`event.Time >= pushed - 5min`: tag the oldest such op with `sync:K:id`.

Reconcile, per slot: remote vs base vs local (three-way table in the spec).
Remote-only change → new op (author: event author, `BodyEditor`, the
comment's author, or the default account's identity). Converged → tag the
local op. Conflict → later time wins (local op time vs event time or
`Updated`); the losing side gets nothing written; reported as a warning.

- [ ] Tests (fake with `NoEditEvents`): echo of a pushed close is claimed;
      the token owner's own close made on the forge is imported (Review
      Focus 4); a comment edited on the forge is imported once; a local edit
      made after a push is not reverted by the next pull; conflict keeps the
      later edit and reports it (Review Focus 1); labels added on the forge
      and removed locally merge (Review Focus 3). Commit.

### Task 9: Push of new issues

**Files:** Create `syncpush.go`, tests.

**Produces:** `func (e *Engine) Push(ctx context.Context) <-chan ExportResult`

A bug not on K: `CreateIssue` with the current title and the rendered current
body (marker + attribution when needed), as the author's account or the
default one; then each comment in order; then status and labels if not the
defaults. Tag every op as described in the data model. Commit each bug.
Without `Foreign`: skip bugs with another origin, and ops whose author has no
account.

- [ ] Tests: a git-bug bug with 2 comments, closed, labelled is created with
      4 writes or fewer per value (not one per historical op); attribution
      only for authors without an account; `Foreign == false` skips a bug
      with origin `other`; a second push writes nothing. Commit.

### Task 10: Push of updates

**Files:** Modify `syncpush.go`, tests.

Per slot of a bug on K: local value differs from base (text: the latest text
op isn't tagged for K) → write, as the account recorded in `sync:K:as` for
text, else the author's account or the default one; tag with value and
`pushed`. A local close then reopen with base open writes nothing.

Every Remote call goes through `e.call(ctx, func() error)`: on
`ErrRateLimited` it waits until `Until` (or ctx is done) and retries, up to 3
times. `ErrGone` on a write: tag the op `sync:K:gone`, warn, stop syncing that
item to K (a deleted issue: the whole bug). Any other error on a bug: report
it and go on with the next bug.

- [ ] Tests: `RateLimitOnce` write is retried and succeeds; an issue deleted
      by an actor gets `ErrGone`, the run warns, the next run doesn't retry it
      and git-bug keeps the bug; an error on one bug doesn't stop the others.
- [ ] Tests: title, status, labels, body edit, comment edit each pushed once;
      `AuthorOnlyEdits` fake accepts the edit of a bot-posted comment;
      `FailAfterWrite` on `CreateIssue` then a second run: no second issue,
      no duplicate bug (Review Focus 2). Commit.

### Task 11: Conformance suite

**Files:** Create `bridge/core/synctest/conformance.go`,
`bridge/core/synctest/conformance_test.go` (runs it on two fakes).

**Produces:**

```go
// Harness gives the suite two forges to sync through one git-bug repo.
type Harness struct {
	NewForge func(t *testing.T, name string) Forge
}
func Run(t *testing.T, h Harness)
```

Scenarios (each a subtest, each on fresh forges and repo): the spec's list,
plus Review Focus 1-5. "Sync" = pull A, pull B, push A, push B.

- [ ] Write the suite, run on fakes with and without quirks. Commit.

### Task 12: Wire the engine into bridges

**Files:** Modify `bridge/core/bridge.go`, `bridge/core/interfaces.go`.

**Produces:**

```go
// RemoteBridge is implemented by bridges ported to the sync engine.
type RemoteBridge interface {
	NewRemote(ctx context.Context, repo *cache.RepoCache, conf Configuration) (Remote, error)
}
const ConfigKeySync = "sync"
```

`ImportAllSince` and `ExportAll` use the engine when `impl` is a
`RemoteBridge`. `ValidateConfig` path: `sync=true` on a bridge without
`NewRemote` fails. `Bridge.Sync`-friendly accessors: `IsSync()`.

- [ ] Tests: a fake `BridgeImpl` with `NewRemote` goes through the engine;
      `sync=true` on a non-remote bridge fails to load. Commit.

### Task 13: `git bug bridge sync`

**Files:** Create `commands/bridge/bridge_sync.go`, modify
`commands/bridge/bridge.go`; regenerate `doc/md`, `doc/man` with
`go generate ./doc`.

Pull every sync bridge (or the named ones), then push every one, under the
lock; print results like `bridge pull`/`push`; `--dry-run` passes
`DryRun` and prints the plan.

- [ ] Test with `commands/bridge` test helpers if present, else a
      `TestBridgeSync` using two fake-backed bridges registered in the test.
      Commit.

### Task 14: GitLab adapter

**Files:** Create `bridge/gitlab/remote.go`, `bridge/gitlab/remote_test.go`
(HTTP fake grown from `export_foreign_test.go` into a `synctest.Forge`).
Delete `export.go`, `import.go` and their foreign/E2E tests once the adapter
passes, keeping `TestGitlabPushPull`/`TestGitlabImport` rewritten on the
engine (they need `GITLAB_API_TOKEN`).

Key `<host>/<project id>`, instance `<host>`. Events: state events, label
events, title system notes. No edit events (reconcile). `BodyEditor` from
`updated_by_id`. Comments: user notes. Legacy: `gitlab-id` with the bug's
`gitlab-project-id` and `gitlab-base-url` (empty accepted). Legacy user:
`gitlab-id`, by id. Rate limits: 429 → `ErrRateLimited`.

- [ ] `synctest.Run` passes against the HTTP fake. Remove `export-foreign`
      code and doc. Commit.

### Task 15: GitHub adapter

**Files:** Create `bridge/github/remote.go`, `bridge/github/remote_test.go`
(GraphQL HTTP fake answering the adapter's operations). Reuse
`import_query.go`, `import_mediator.go`, `export_mutation.go`, the rate-limit
client and label id cache; delete the old importer/exporter.

Key `github.com/<owner>/<project>`. Events: timeline items and
userContentEdits (edit events with ids). Labels as a full set via
`updateIssue(labelIds)`, creating missing labels. Legacy: `github-id` with
the bug's `github-url` under this repo. Legacy user: `github-login`, by login.

- [ ] `synctest.Run` passes against the fake. Existing E2E tests rewritten on
      the engine. Commit.

### Task 16: Real forges

- [ ] Create `guilbep/git-bug-sync-test` on GitHub (private). Sync it two-way
      with `guilbert/git-bug-sync-test` on gitlab.epfl.ch: issues created on
      each side, edits, closes, labels, mentions and refs, a second sync
      writes nothing. Record results in sync-todo.md.

### Task 17: Docs

- [ ] `configuration.md`: `sync` key, remove `export-foreign`.
      `third-party.md`: bot account advice. `feature-matrix.md`: sync row.
      `sync.md` status. Commit.

### Task 18: Gitea adapter

**Files:** Create `bridge/gitea/` (bridge impl, config, remote, test with an
HTTP fake of the Gitea API v1 timeline, issues, comments, labels).

- [ ] `synctest.Run` passes. Register the bridge. Commit.

### Task 19: Whole-branch review

- [ ] Fresh reviewer on the whole branch against sync.md; fix findings;
      final real-forge run; update sync-todo.md.
