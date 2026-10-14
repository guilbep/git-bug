# Bridge sync: decisions taken while building it

Decisions taken while implementing [sync.md](sync.md), where the plan or the
spec didn't settle something, each with what it costs if it's wrong. The
spec stays the authority.

## Data model

- **What a forge saw is read from SetMetadata operations, in order, the last
  one winning**, plus the own metadata of pulled operations. git-bug keeps
  only the first value of a metadata key on an operation, so a second push
  of the same value could never update a per-operation tag. *If wrong:*
  re-derive the base; data written stays readable.
- **One forge event beyond the first can claim the same push**
  (`sync:<remote>:also:<event>`), as GitLab emits one event per label of a
  single label change. *If wrong:* nothing.
- **A pulled label event records the forge's label set, computed from what
  the forge last saw**, not from git-bug's labels, which local changes may
  have moved. *If wrong:* labels flapping between runs.
- **Synced bugs keep `<kind>-url` on their create operation**, as the old
  bridges did: tools built on them, such as git-board, find a bug's issue
  through it. *If wrong:* nothing; with two remotes of one kind, it's the
  first one's URL.
- **Older git-bug versions don't read what sync records**: their `bridge
  pull` matches `github-id` and `gitlab-id`, and imports synced issues
  again. Writing those too would half-work (comments would still
  duplicate), so it's documented instead. *If wrong:* duplicates, from
  someone else's older binary.
- **Remote ids**: GitLab keeps raw numbers as its old bridge did, so old data
  stays recognized; Gitea prefixes issue ids (`issue-<n>`), as its issue
  numbers and comment ids collide; GitHub uses its node ids. *If wrong:* a
  rare GitLab id collision skips one event.
- **GitHub logins stand for user ids**, as the old bridge did. *If wrong:* a
  renamed GitHub user becomes a new identity.

## Pull

- **A replayed event that changes a value also changed in git-bug since the
  last sync follows "the later change wins"**, as reconciling does, and is
  reported once. Reconcile then leaves that value alone.
- **"Later" is the time of the forge's latest event about that value**, then
  for a body the time the forge says it was edited (GitLab's "changed the
  description" note), then the issue's last update. *If wrong:* on Gitea,
  which records no body edit time, an unrelated comment can make an older
  forge body edit win.
- **A forge issue carrying the marker of a bug already on that forge is a
  duplicate** (a retried write) and is reported, not imported.
- **Only the engine's accounts' markers count.** Anyone else's is text, so a
  stranger can't attach an issue to a bug. *If wrong:* nothing.
- **Bugs the old bridges left twice for one issue**: the copy pulled from the
  forge is synced, else the oldest, with a warning. The other copy still
  counts as on the forge. *If wrong:* both copies may push to the issue.
- **GitLab and Gitea text edits leave no event**: they're reconciled, and the
  original title and body are the current ones (no history). *If wrong:*
  imported history starts from the current text, as the old importers did.
- **GitHub status and labels are derived from the timeline**, keeping the
  existing GraphQL query. An issue whose timeline failed to load is an
  error, never a partial issue.

## Push

- **New issues are created from git-bug's current state** (title, body,
  comments, then status and labels), oldest bug first, not by replaying
  history: fewer writes and notifications, and issue numbers follow
  git-bug's order so references to older bugs translate. *If wrong:* edit
  history isn't visible on the forge (it's in git-bug).
- **Without `sync`, the text sent is the last version written by someone with
  an account**: nothing by someone without one reaches the forge.
- **A write the forge refuses for good (403) warns once** and is recorded on
  that value; a later change is tried again. *If wrong:* a refusal that was
  temporary needs a new change.
- **"Not found" is checked with the reader token** before calling an issue or
  comment deleted: forges answer the same to a token that can't see a
  private project.
- **Writes are never retried by the forge clients** (GitLab `WithoutRetries`,
  GitHub too): a write the forge did without answering would be done twice.
  The engine retries rate limits, and the marker recovers lost answers on
  the next pull. *If wrong:* a transient 5xx on a write is an error,
  retried next run.
- **No batching: forges pace writes, the engine waits.** Neither GitHub nor
  GitLab can create or update several issues in one call. GitLab's 429 and
  GitHub's secondary limit (no reset time given: one minute) reach the
  engine, which waits and retries up to three times; a run stopped by a
  limit goes on where it was at the next run, as every write is recorded
  when made. Reads are paged, and GitHub's page halves when GitHub finds it
  too costly. *If wrong:* a first sync of thousands of writes may need
  several runs.
- **An unresolvable `#ref` with no known source URL becomes `` `#n` ``**, never
  a link to the wrong issue.
- **Labels**: GitHub sets the full set with `updateIssue(labelIds)` and
  creates missing ones with its REST API (GraphQL can't); Gitea sets ids and
  creates missing ones, as it silently ignores unknown names.

## Rendering and identities

- **`Render` takes the source forge's dialect and a reference rewriter**; the
  engine owns the lookup. Engines register their remote's kind and dialect,
  so `bridge sync`, which creates every engine first, renders text from any
  forge for any other. *If wrong:* references aren't translated in a lone
  `bridge push` of text from an unusual forge.
- **The marker says "attributed" when a header follows**; unrendering restores
  mentions only in texts with a marker, and also removes the header of the
  first `export-foreign` version, which exists on gitlab.epfl.ch.
- **A stray backtick doesn't start code**: mentions after it are defused.
- **Token owners are found by user id, then the bridge's legacy key, then the
  login tag `bridge new` writes**; that last lookup is for token owners only,
  as login tags are per kind of forge, not per server. *If wrong:* someone
  with an account on another server of the same kind gets a separate
  identity.
- **`Account` carries no token**: `bridge/core/auth` imports `bridge/core`, and
  it keeps secrets out of the engine.
- **The same person on two forges is linked by hand, in each bridge's
  configuration** (`login-<identity> = <login>`), checked before any other
  match. Not on the identity: its metadata keeps a key's first value
  forever, and a wrong link posts someone's text with another person's
  token. Not by email or login: unverified on some forges, with the same
  risk. *If wrong:* links don't travel with the repository; whoever runs
  sync sets them, as they set the bridges. A renamed account needs its link
  updated.

## The command and the bridges

- **`bridge sync` records its own `lastSyncTime`**, apart from
  `lastImportTime`: an older bridge may have imported part of a project
  (co2-calculator: 228 of about 811 issues), and its time would skip the
  rest. The first sync of a bridge reads every issue. *If wrong:* one full
  read per bridge.
- **`SyncAll` lives in core and holds the bridge lock for the whole run**, so
  no other git-bug process runs between the pull and the push phases.
- **A dry run creates nothing**, identities included. It counts new issues and
  comments exactly, and updates as "up to N changes", as echoes can't be
  claimed without writing.
- **`default-login` is a core configuration key**, as every bridge used it.
- **GitLab base URLs are compared without their trailing slash**, for tokens
  and old bugs: `bridge new` stores whichever form was typed. *If wrong:*
  nothing.
- **Sync lives in this git-bug fork, and git-board stays its own
  repository.** Sync replaces git-bug's bridges, and needs their
  configuration, the keyring, the bridge lock and the command; git-board
  uses git-bug as a library (cache, query) to add fields, sprints and
  views. Their contract is git-bug's data: labels, and `<kind>-url`. *If
  wrong:* moving code between them later.
- **The Gitea client is written on the standard library**: about ten
  endpoints, no new dependency.
- **The end-to-end tests of GitHub and GitLab assert each bug's final state**
  instead of the old exporters' operation counts, and `GITLAB_BASE_URL`
  selects the instance. GitLab's passed on gitlab.epfl.ch; GitHub's and
  `TestGitlabImport` couldn't run here.

## Set aside

Considered and left as they are, each covered by the spec, an earlier
decision, or the known limits in [sync-todo.md](sync-todo.md):

- push walks every bug and pull scans every bug per issue, as the old
  bridges did;
- a stale lock after a crash needs removing by hand (the maintainer's lock);
- a refused write isn't retried with the default account (spec: refused for
  that value only);
- with `sync`, a text is edited with the account that posted it, whoever
  edited it (spec);
- one runner, attribution lost through another hub (spec and known
  limits).
