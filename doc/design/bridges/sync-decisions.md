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
- **Titles have their `#refs` translated too**, as a GitHub workflow may
  append each issue's number to its title (co2: `… (#2518)`), which on
  GitLab named another issue. What a forge saw of a title is it as
  translated, so whether git-bug changed it is told by its operations, as
  for texts. Unlike a text, a title is translated again at each push: a
  reference to the issue itself is known only once the issue exists, so it
  is created with `` `#n` `` and its title set again right after (one more
  write). An unknown ref is `` `#n` ``, not a link: titles show no links.
  Mentions in titles are left alone. *If wrong:* one title write when an
  issue a title names reaches the forge; a mention in a title may notify
  someone on another forge.
- **What a pre-sync bridge pulled goes back to its own project as it is**,
  and its `#refs` translate elsewhere: bugs with `<kind>-id` are indexed by
  the number of their `<kind>-url`, under the key the adapter gives their
  project (`github.com/<owner>/<repo>` from `github-url`;
  `<host>/<gitlab-project-id>` for GitLab, whose URL names the project by
  path, not id), and what it pulled refers to that project. So a `#5` of one
  project is never another's, and GitLab's `#77` reaches GitHub as `#2518`,
  co2's bug 228 being both. A bug whose project can't be read refers to
  nothing (`` `#n` ``). Without it, co2's 228 bugs of the old GitHub bridge
  had their titles' `#n` turned into `` `#n` `` on GitHub itself. Titles
  are rendered at each push, texts only when they change: the first sync
  after this change sets once again each title whose ref now translates
  (a legacy title naming a GitHub issue created since the migration, a
  GitLab title naming a legacy one), one write per issue that a dry run
  lists as "would send: title". *If wrong:* a reference to a legacy issue
  goes out as a link to its forge.
- **Titles already mirrored without translation are set once again** at
  the first sync after this change, where they hold a `#n`: one write per
  issue, its echo claimed by the next pull. That fixes their refs, and is a
  burst of writes on a project like co2's GitLab mirror.
- **A value reconciled from a forge, without event, was written on that
  forge**: GitLab's and Gitea's text edits (they have no events) were
  pushed elsewhere as git-bug text, their mentions live (one would notify
  on GitHub) and refs untranslated. The value recorded by reconcile now
  names the forge. *If wrong:* nothing.
- **A push sends label changes against what the forge last saw, never
  git-bug's set** (`Remote.ChangeLabels`): a push without a pull first, as
  git-board's Send, would otherwise remove the labels added on the forge
  since, and put back the ones removed there. What the push records as seen
  stays git-bug's set, not the forge's: the next pull then takes a label
  added or removed on the forge as the forge's change, with no conflict.
  GitHub adds and removes with `addLabelsToLabelable` and
  `removeLabelsFromLabelable` (GraphQL, as its other writes), creating
  missing labels with its REST API (GraphQL can't); GitLab with
  `add_labels` and `remove_labels`, which it creates when missing as for a
  full set (not checked against a live GitLab); Gitea posts ids, creating
  missing ones as it silently ignores unknown names, and deletes one label
  at a time. A label to remove that the project doesn't have is on no
  issue, and is skipped. GitHub's and Gitea's adapters read the project's
  labels again when one is missing, before creating or skipping it: an
  engine may live long (git-board keeps one between a check and a send),
  and a label made on the forge since would otherwise never be removed.
  A bug the old bridges pulled has a base, what they pulled (`base` reads
  their tags), so its first sync and a push alone send git-bug's changes
  since, removals included; only a push alone after `Link`, whose base is
  empty on purpose, adds without removing.
  *If wrong:* a push alone no longer makes the
  forge's labels git-bug's; the forge's own changes merge at the next pull,
  as in a full sync. A change that adds and removes is two writes on
  GitHub, and Gitea takes one write per label removed.

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
- **One issue syncs alone: `SyncIssue` (`bridge sync --issue`) runs
  `SyncAll`'s rules on one bug** — same bridges, lock, and no push to a
  forge whose pull failed. Its issue is fetched by its recorded id and
  number from each forge it is on (`Remote.Fetch`, a GraphQL
  `issue(number:)` on GitHub, `GET …/issues/:iid` on GitLab and Gitea),
  then the bug is pushed to every forge, created where a full sync would
  create it. The bridges' `lastSyncTime` stays: no other issue was read,
  so the next full sync still reads everything changed since the last one.
  For a person who wants one comment out now, without a run over every
  issue (a minute and more on co2). *If wrong:* a full sync does it all.
- **A bug synced alone that isn't on a forge yet is first looked for there
  by its git-bug marker**, as a full pull attaches it: another clone may
  have synced it, or a push died between creating the issue and recording
  it. `PullIssue` lists the issues updated since the bug was created (less
  `claimWindow`, for clocks), through `Remote.Issues`, and pulls the first
  one carrying the bug's marker, posted by one of the accounts. A dry run
  says it would sync with that issue instead of creating one. When the
  listing fails or stops, the push to that forge doesn't happen. No forge
  search API: one listing works on every forge. *If wrong:* a bug never
  synced whose creation is old, like one pulled long ago from another
  forge, lists most of the project at each one-issue sync until it is on
  that forge (a minute and more on co2; git-board's Send makes two, its
  dry run then the real one); a forge search for the marker would then
  do.

## One issue on one forge: link, unlink, take

`Link`, `Unlink` and `Take` in `bridge/core` (for git-board), and
`bridge link`, `bridge unlink`, `bridge take` on the command line. Each runs
under the bridge lock and writes nothing to the forge.

- **Link records the forge issue on the bug as a pull would**, found by its
  number or URL (`Remote.Fetch` by number: GitLab's issue id is its number).
  The forge's events are taken as seen, not replayed over git-bug's
  history. A value both sides hold is recorded in sync (the title as
  git-bug would send it there, its `#refs` translated); git-bug's others
  are changes not sent yet, so the next sync sends git-bug's title,
  description and status. Comments of each side are added to the other,
  and labels merge, as if the issue had none when last in sync (a lone
  `bridge push` adds git-bug's, and the next pull the forge's). `take` right after keeps the
  forge's version. Refused: an issue that is another bug's (by id or legacy
  metadata), a bug that has or had an issue on that forge, a URL of another
  project. *If wrong:* the forge's title, description and status are
  edited at the next sync; comments are matched by marker only, as in a
  pull, so copies of git-bug's comments posted by another account appear
  twice.
- **Unlink is the mark the engine already puts on a bug whose issue was
  deleted** (`sync:<remote>:gone`, here `unlinked`): the bug is no longer
  pushed there, so no issue is created again, and the forge issue is no
  longer pulled, into the bug or as a new bug. The issue is free to be
  linked to another bug, and references to it stop translating to the old
  bug (deleted issues' too). It's final: metadata keeps a key's first value,
  so neither the bug's issue id nor the mark can change, and linking a bug
  once unlinked is refused. *If wrong:* a bug unlinked by mistake never
  syncs with that forge again; a new bug can be linked to the issue.
- **"Changes not sent yet", for take, are what a push would send**, read
  from the records: a status or label set other than what the forge last
  saw, a title operation since the forge last saw the title, and a
  description or comment text whose last version isn't the one the forge
  last saw (refused edits included). Take pulls the issue (`Fetch`, as
  `SyncIssue` does) with every conflict resolved for the forge, then undoes
  each change not sent yet with the forge's current value, made by the
  user's identity at that time (a forge change keeps its author and time).
  Comments the forge doesn't have stay, and the next sync sends them:
  git-bug can't delete a comment, and an addition conflicts with nothing.
  Assignees aren't synced. The bug's other forges get the forge's version
  at the next sync, as git-bug changes. *If wrong:* a comment written in
  git-bug by mistake still reaches the forge.
- **`bridge take <id> <bridge>`, not `bridge sync --issue <id> --theirs`**:
  "the forge's version" names one forge, and a sync runs over several.

## Pull requests: git-board's review

git-board writes review comments as git-bug comments (its milestone M4);
the engine mirrors them to the issue's GitHub pull requests. The contract
with git-board: a line comment carries `review:path`, `review:line`,
`review:side` (`old` or `new`), `review:commit` (a full SHA) and
`review:pr` (`github.com/<owner>/<repo>#<n>`); a reply also
`review:reply-to`, the comment id (`CombinedId`) of the comment it answers;
a review `review:state` (`APPROVE`, `REQUEST_CHANGES`, `COMMENT`),
`review:commit` and `review:pr`, its summary as text. A SetMetadata
`review:resolved` (`true` or `false`) on a thread's first comment resolves
it, the last one winning. What is pulled from a pull request carries the
same keys, and its origin `sync:<remote>:pr-comment`, its GitHub id.

- **A comment with any `review:*` key, or pulled from a pull request, is
  never an issue comment**, on any forge: not posted, not edited, not
  counted by a dry run. *If wrong:* a comment written for the issue with a
  `review:*` key never reaches it.
- **Only a remote whose adapter is a `PullRemote` has pull requests:
  GitHub's.** GitLab's merge requests and Gitea's pull requests stay out.
  *If wrong:* review comments stay in git-bug on those forges.
- **What was pulled from a pull request records its origin as
  `sync:<remote>:pr-comment`, not `:id`**: an own `:id` makes an operation
  pulled from that remote's issue (`sourceOf`) and an issue comment there.
  What was sent records `:id` and `:url` with SetMetadata, as a pushed
  issue comment. Both are GitHub node ids (`PRRC_`, `PRR_`, `IC_`): the
  REST ids of line comments, reviews and comments are three sequences that
  may collide. A reply's `in_reply_to` is the number in its parent's
  recorded URL (`#discussion_r<n>`), so no comment is fetched for it.
  *If wrong:* a reply to a parent that went out as a plain comment starts
  its own thread on the same line.
- **A push trusts `review:pr`** (the first value, SetMetadata included);
  `PullsOf` serves pulls only. A comment without one waits for git-board to
  add it: a dry run says "would send later: … (waits for a pull request)".
  *If wrong:* nothing.
- **A pull request comment goes with the author's account, or with sync the
  default one, attributed, as an issue comment; an approval only with its
  author's own**, else it's reported ("missing author token: approval by
  … not sent"). A request for changes may go with the default account, as
  the contract says. *If wrong:* a request for changes blocks a pull
  request under the bot's name until someone dismisses it.
- **A line comment GitHub won't put on its line (422: outside the diff, or
  on a commit no longer in it) goes as a comment of the pull request**,
  starting with "On `path` line N (new side) at abc1234:". It counts as
  sent: it isn't sent again if the line comes back. A review GitHub
  refuses (422: approving one's own pull request) is refused for good,
  warned once. A pull request GitHub doesn't find (404) warns, and is tried
  at the next run. *If wrong:* a comment that could be on its line later
  stays a plain comment.
- **What a push sends a pull request carries the git-bug marker**, and is
  committed right after: a pull with `PullsOf` takes a comment posted by an
  account with the marker of a comment not recorded as sent as that
  comment, as for issue comments. The command line has no `PullsOf`: there,
  a push that died between GitHub's answer and the commit posts again.
  *If wrong:* one duplicate comment after such a crash, on the command line.
- **A pull lists each pull request whole**, three REST listings of 100 per
  page (line comments, reviews, comments), not since the last sync: the
  reviews' listing has no `since`, and a reply's parent must be listed to
  map GitHub's `in_reply_to_id`. *If wrong:* three requests per pull
  request `PullsOf` gives at each pull: git-board gives the open ones.
- **A pulled line comment is anchored where it was written**
  (`original_line`, `original_commit_id`), which an outdated comment keeps
  while GitHub's `line` becomes null. *If wrong:* nothing; git-board tells
  outdated from the commit.
- **Reviews without text that only hold line comments are not pulled**
  (GitHub makes one for each line comment posted alone), nor pending or
  dismissed ones: GitHub no longer says what a dismissed review was. One
  pulled before its dismissal stays as it was. *If wrong:* a dismissed
  review's text is missing.
- **An edit on GitHub of a pull request comment is pulled, and wins**:
  git-bug's edits of them aren't sent (the contract sends new comments
  only). *If wrong:* an edit made in git-bug is lost at the next edit on
  GitHub, and never reaches GitHub.
- **`review:resolved` is sent with GraphQL `resolveReviewThread` or
  `unresolveReviewThread`**, which the `repo` scope allows to whoever may
  resolve it on GitHub, under the account of whoever set it (the default
  one on their behalf). The thread is found by its first comment through
  `reviewThreads`, with no team field. What GitHub last saw is
  `sync:<remote>:resolved` on the thread's first comment, SetMetadata, the
  last one winning; a thread is open until then. One GitHub can't find (a
  comment that went out as a plain comment has no thread) or refuses is
  refused for good. Resolutions made on GitHub are not pulled. *If wrong:*
  a thread resolved on GitHub shows open in git-board.
- **A pull request that can't be read is an error, as a failed issue
  listing**: that forge isn't pushed to in that run. Not found (404) is a
  warning. *If wrong:* a transient failure delays one run's push.
- **`WithPullsOf` gives one function for every bridge of a run**, as agreed
  with git-board: it takes a bug, not a remote. *If wrong:* with two GitHub
  bridges, a number `PullsOf` gives is read on both repositories, and
  another repository's pull request of that number lands on the bug.
- **Only `SyncAll`, `SyncIssue` and a bridge's own runs with `Bridge.Use`
  pull pull request comments**; the command line (`bridge sync`, `bridge
  pull`), `Link` and `Take` don't, as they don't know an issue's pull
  requests: git-board's branches do. Every push sends them. *If wrong:*
  nothing.

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
