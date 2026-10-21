# Native fields Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** git-bug's kinds sync with GitHub issue types, and its sprints with
GitLab milestones. Each value is native where the forge has it, and stays a
label otherwise.

**Architecture:** As for assignees, the adapters alone map labels to native
values; the engine merges them as labels. The engine gains only:
- the `kinds`, `sprints` and `kind-types` keys, given per run with
  `WithConfig`;
- `Slug`;
- `OneNative`, the rule both adapters share for a single-valued field: the
  value the forge holds stays native, others go as labels.

Old-way labels move through `Issue.Resend`, as `assignee:*` labels did.

**Tech Stack:** Go; shurcooL/githubv4 (its generated inputs predate issue
types, so the fork declares the mutation input itself);
gitlab.com/gitlab-org/api/client-go v1.46.0; golang.org/x/text/unicode/norm.

**Spec:** [native-fields.md](native-fields.md), and the rulings in
[sync-decisions.md](sync-decisions.md#assignees).

## Global Constraints

- Write code first, tested against the adapters' fakes. The engine logic
  isn't changed, so synctest's fake needs nothing. The conformance suites
  (`TestGithubConformance`, the GitLab one) must still pass with no types
  and no milestones.
- Labels match exactly, both ways. A type is `kind:<Slug(name)>` unless
  `kind-types` maps it; a milestone is `sprint:<Slug(title)>`. Anything else
  stays a label.
- Mode values are `native` (the default) and `labels`, for `assignees`,
  `kinds` and `sprints`. Any other value is an error that names it.
- `labels` mode reads and writes nothing native: no type or milestone query.
- No forge object is ever created: no issue type, no milestone.
- gofmt with Go 1.26: `$(GOTOOLCHAIN=go1.26.0 go env GOROOT)/bin/gofmt -l .`
  prints nothing.
- Run `go generate` after a help text change, and commit `doc/` only, never
  the `api/graphql` drift.
- Commit each task on `native-fields`, ending with
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never push,
  never tag.
- Live tests only on scratch projects: the EPFL-ENAC scratch repository from
  guilbep/git-board#29, and gitlab.epfl.ch 42400. Never co2-calculator,
  never Project 40.
- Record each call made while building in sync-decisions.md, with its *If
  wrong*.

## Review Focus

1. **A type disabled or deleted in the organization** while issues still
   hold it. The pull still gives its label (`kindLabel` needs no type list),
   and a push of that label stays a label. Test in Task 2.
2. **A malformed `kind-types`**: an entry without `=`, an empty side, a type
   or label named twice. The engine isn't created, and the error names the
   entry. Test in Task 2.
3. **A project milestone and a group milestone with the same title** both
   slug to one label. The project's wins, every run. Test in Task 4.
4. **A token that may label but not set types** (GitHub answers FORBIDDEN).
   The kind's label is refused (`RefusedLabels`, one warning) and the other
   labels go through. Test in Task 3.
5. **A renamed milestone or type**: the pull gives the label of its current
   name, so the next reconcile moves the issue's label; no error. Test in
   Task 4, for a milestone.

---

### Task 1: Core: the keys, `WithConfig`, `Slug` and `OneNative`

**Files:**
- Modify: `bridge/core/bridge.go:37-45` (constants), `bridge/core/bridge.go:485-492` (overrides and validation)
- Modify: `bridge/core/sync.go:40-59` (`SyncOptions.Config`, `WithConfig`, `WithAssignees`)
- Create: `bridge/core/native.go`, `bridge/core/native_test.go`
- Test: `bridge/core/bridge_sync_test.go` (`TestSyncAssigneesMode` becomes `TestSyncModes`)

**Interfaces:**
- Produces:
  - constants `ConfigKeyKinds = "kinds"`, `ConfigKeySprints = "sprints"`,
    `ConfigKeyKindTypes = "kind-types"`, `ModeNative = "native"` and
    `ModeLabels = "labels"`;
  - `AssigneesNative = ModeNative` and `AssigneesLabels = ModeLabels`, kept
    because git-board uses them;
  - `SyncOptions.Config map[string]string`, bridge keys given over each
    clone's;
  - `func WithConfig(key, value string) Option`, with `WithAssignees(mode)`
    now `WithConfig(ConfigKeyAssignees, mode)`;
  - `SyncOptions.Assignees` stays, set by `bridge.go` from the merged
    configuration, as now;
  - `func Slug(value string) string`;
  - `func OneNative(current string, added, removed []string, native func(label string) bool) (next string, add, remove []string)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"QA Validation": "qa-validation", "Comité Spécialisé": "comite-specialise",
		"S1 - Set up": "s1-set-up", "Preliminary work (S0)": "preliminary-work-s0",
		"Sprint 2": "sprint-2", "  Bug ": "bug",
	} {
		require.Equal(t, want, core.Slug(in), in)
	}
}

func TestOneNative(t *testing.T) {
	native := func(l string) bool { return l == "bug" || l == "feat" }
	for _, c := range []struct {
		name, current          string
		added, removed         []string
		next                   string
		add, remove            []string
	}{
		{"first takes it", "", []string{"bug", "a"}, nil, "bug", []string{"a"}, []string{"bug"}},
		{"held stays, another is a label", "bug", []string{"feat"}, nil, "bug", []string{"feat"}, nil},
		{"replaced", "bug", []string{"feat"}, []string{"bug"}, "feat", nil, []string{"bug", "feat"}},
		{"cleared", "bug", nil, []string{"bug"}, "", nil, []string{"bug"}},
		{"old way label sent again", "bug", []string{"bug"}, nil, "bug", nil, []string{"bug"}},
		{"not native", "", []string{"polish"}, nil, "", []string{"polish"}, nil},
	} {
		next, add, remove := core.OneNative(c.current, c.added, c.removed, native)
		require.Equal(t, c.next, next, c.name)
		require.Equal(t, c.add, add, c.name)
		require.Equal(t, c.remove, remove, c.name)
	}
}
```

`TestSyncModes`: the `TestSyncAssigneesMode` checks, repeated for
`ConfigKeyKinds` and `ConfigKeySprints` through `WithConfig`: native by
default, the key read, the run's value winning, and `"lables"` an error
containing `lables`.

- [ ] **Step 2: Run the tests to check they fail**

Run: `go test ./bridge/core/ -run 'TestSlug|TestOneNative|TestSyncModes'`
Expected: FAIL (`undefined: core.Slug`, `core.OneNative`, `core.WithConfig`).

- [ ] **Step 3: Implement**
  - `Slug`: git-board's `config.Slug` exactly. Lower case, NFD, drop
    `unicode.Mn`, runs of `[^a-z0-9]+` as `-`, then trim `-`.
  - `OneNative`:
    - `next` is `current`, or `""` when `removed` holds `current`;
    - each native label in `added`:
      - takes `next` when `next` is empty;
      - when it equals `next`, it leaves `add` and joins `remove`, as its
        old-way label comes off;
      - otherwise it stays in `add`;
    - `remove` keeps `removed`'s labels in their order, then those appended
      (no duplicates);
    - nil slices when empty.
  - `bridge.go`:
    - merge `opts.Config` over `conf` (cloned);
    - one loop validates `ConfigKeyAssignees`, `ConfigKeyKinds` and
      `ConfigKeySprints` (empty, native or labels);
    - then `opts.Assignees = conf[ConfigKeyAssignees]`, as now.

- [ ] **Step 4: Run the tests to check they pass**

Run: `go test ./bridge/core/...`
Expected: PASS.

- [ ] **Step 5: Commit**

`bridge/core: kinds, sprints and kind-types keys; Slug and OneNative`

### Task 2: GitHub: types come in as kind labels

**Files:**
- Modify:
  - `bridge/github/import_query.go`: three timeline fragments,
    `issueTypesQuery`;
  - `bridge/github/remote.go`:
    - `NewRemote`, `newRemote` fields;
    - `build`, `issueBuilder.timeline`, `issueLabels` and `finish`;
    - new `parseKindTypes`, `kindLabel`, `readTypes`, `typeID`.
- Modify: `bridge/github/remote_test.go` (the fake GitHub: `types`,
  `ghIssue.issueType`, `setType`, `issueTypesQuery`)
- Create: `bridge/github/kinds_test.go`

**Interfaces:**
- Consumes: `core.Slug`, `core.ConfigKeyKinds`, `core.ConfigKeyKindTypes`,
  `core.ModeLabels`.
- Produces, in package `github`:
  - `func parseKindTypes(v string) (map[string]string, error)`: labels by
    lowercased type name, from `bug=Bug,kind:feat=Feature`;
  - fields on `remote`:
    - `kinds bool`, native unless `kinds = labels`;
    - `kindTypes map[string]string`;
    - `types map[string]string`: enabled type ids by label, nil when none,
      guarded by `mu`;
  - `func (r *remote) kindLabel(typeName string) string`: `kindTypes`, else
    `"kind:" + core.Slug(typeName)`;
  - `func (r *remote) readTypes(ctx context.Context) error`: fills `types`
    and sets `typesRead bool`. Called at the start of `Issues` and `Fetch`
    when `kinds`, so each pull sees types made since, and by `changeType`
    when never read;
  - `func (r *remote) typeID(label string) (string, bool)`, from what
    `readTypes` read;
  - `build(mm *importMediator, r *remote, yield …)`: the builder gets `r`
    instead of `labelAssignees`.

- [ ] **Step 1: Write the failing tests** in `kinds_test.go`, with a fake
  GitHub owner that has the types Bug, Feature and Task, `kind-types =
  bug=Bug,feat=Feature`, and a fake that can set an issue's type
  (`g.setType(login, ref, name)`, emitting the three events as GitHub
  does):

```go
// TestGithubTypesAreKinds
g.setType("alice", ref, "Bug")
require.Equal(t, []string{"bug"}, g.Issue(ref.Number).Labels)
g.setType("alice", ref, "Feature") // IssueTypeChangedEvent
ev := last(g.Issue(ref.Number).Events)
require.Equal(t, []string{"feat"}, ev.Added); require.Equal(t, []string{"bug"}, ev.Removed)
g.setType("alice", ref, "Task")
require.Equal(t, []string{"kind:task"}, g.Issue(ref.Number).Labels)
g.setType("alice", ref, "") // IssueTypeRemovedEvent
require.Empty(t, g.Issue(ref.Number).Labels)

// TestGithubTypeLabelsResent: a type's label held as a label
alice.SetLabels(ref, []string{"bug", "polish"})
require.Equal(t, []string{"bug"}, g.Issue(ref.Number).Resend, "no type yet; polish has none")
g.setType("alice", ref, "Bug")
require.Equal(t, []string{"bug", "polish"}, g.Issue(ref.Number).Labels, "one label")
require.Equal(t, []string{"bug"}, g.Issue(ref.Number).Resend)
alice.SetLabels(ref, []string{"feat"})
require.Empty(t, g.Issue(ref.Number).Resend, "a second kind belongs as a label")

// TestGithubKindsOff: kinds = labels
require.Empty(t, g.Issue(ref.Number).Labels, "Bug type ignored"); require.Empty(t, g.Issue(ref.Number).Resend)
// TestGithubUserRepoHasNoTypes: issueTypes is null
// Labels are GitHub's labels only, no error, nothing resent.
// TestGithubDisabledType (Review Focus 1): Feature disabled after it was set
require.Equal(t, []string{"feat"}, g.Issue(ref.Number).Labels)
// TestParseKindTypes
m, err := parseKindTypes(" bug=Bug, kind:feat=Feature ")
require.Equal(t, map[string]string{"bug": "bug", "feature": "kind:feat"}, m)
for bad, entry := range map[string]string{
	"bug": "bug", "=Bug": "=Bug", "bug=": "bug=", "a=Bug,b=Bug": "b=Bug", "bug=Bug,bug=Feature": "bug=Feature",
} {
	_, err := parseKindTypes(bad); require.ErrorContains(t, err, entry, bad)
}
```

- [ ] **Step 2: Run the tests to check they fail**

Run: `go test ./bridge/github/ -run 'Types|Kinds|KindTypes|Disabled'`
Expected: FAIL to compile (`g.setType`, `parseKindTypes` undefined).

- [ ] **Step 3: Implement**
  - Fragments: `IssueTypeAddedEvent`, `IssueTypeChangedEvent` and
    `IssueTypeRemovedEvent`, with `type issueTypeEvent struct { actorEvent;
    IssueType *struct{ Name githubv4.String } }`. A null `IssueType` is a
    deleted type, read as no type.
  - `issueTypesQuery`: `Repository.IssueTypes *struct{ Nodes []struct{ ID,
    Name string; IsEnabled bool } }` with `graphql:"issueTypes(first: 25)"`
    (25 at most per organization).
  - The builder keeps `issueType` (a name) through `relabel`.
  - `issueLabels` adds `r.kindLabel(issueType)` when `r.kinds`.
  - `finish` puts a GitHub label into `Resend` when `typeID` knows it and
    the issue's type is none or that label's.
  - `NewRemote` parses `kind-types` (an error fails the remote) and reads
    `kinds`.

- [ ] **Step 4: Run the tests to check they pass**

Run: `go test ./bridge/github/`
Expected: PASS, conformance included.

- [ ] **Step 5: Commit**

`bridge/github: issue types come in as kind labels`

### Task 3: GitHub: ChangeLabels sets the type

**Files:**
- Modify: `bridge/github/remote.go` (`ChangeLabels`, new `changeType`)
- Modify: `bridge/github/export_mutation.go`:
  - `UpdateIssueIssueTypeInput`, exported because githubv4 names the
    variable's type after the Go type;
  - `updateIssueIssueTypeMutation`;
  - `issueTypeOfQuery`.
- Modify: `bridge/github/remote_test.go` (the fake answers both, and can
  refuse type writes)
- Test: `bridge/github/kinds_test.go`

**Interfaces:**
- Consumes: Task 1's `core.OneNative`, and Task 2's `typeID`, `kindLabel`
  and `kinds`.
- Produces:
  - `type UpdateIssueIssueTypeInput struct { IssueID githubv4.ID
    `json:"issueId"`; IssueTypeID *githubv4.ID `json:"issueTypeId"` }`. It
    has no `omitempty`: `null` clears the type.
  - `func (r *remote) changeType(ctx context.Context, as core.Account, issue core.Ref, added, removed []string) (add, remove []string, refused core.RefusedLabels, err error)`

- [ ] **Step 1: Write the failing tests**

```go
// TestGithubChangeLabelsSetsType (types Bug, Feature; kind-types bug=Bug,feat=Feature)
require.NoError(t, g.ChangeLabels(ctx, bot, ref, []string{"bug", "a"}, nil))
labels, typ := g.heldType(ref); require.Equal(t, []string{"a"}, labels); require.Equal(t, "Bug", typ)
require.NoError(t, g.ChangeLabels(ctx, bot, ref, []string{"feat"}, nil))
labels, typ = g.heldType(ref); require.Equal(t, []string{"a", "feat"}, labels); require.Equal(t, "Bug", typ)
require.NoError(t, g.ChangeLabels(ctx, bot, ref, nil, []string{"bug"}))
_, typ = g.heldType(ref); require.Empty(t, typ)
require.NoError(t, g.ChangeLabels(ctx, bot, ref, []string{"polish"}, nil)) // no Polish type: a label
// TestGithubChangeLabelsMovesKindLabel: GitHub label "bug", no type; ChangeLabels(add bug)
// type Bug, label bug gone from the issue
// TestGithubChangeLabelsKindsOff (no issueTypes read at all) and on a user
// repository (issueTypes null): bug is a label; no issueTypeOfQuery, no type
// mutation (the fake counts both). Helpers g.setType and g.heldType live in
// kinds_test.go, like assignees_test.go's assign and held.
// TestGithubTypeWriteRefused (Review Focus 4): fake refuses updateIssueIssueType
err := g.ChangeLabels(ctx, bot, ref, []string{"bug", "a"}, nil)
require.Equal(t, core.RefusedLabels{"bug"}, err); labels, typ = g.heldType(ref)
require.Equal(t, []string{"a"}, labels); require.Empty(t, typ)
// TestGithubKindsSync: an engine over the fake (as TestGithubLegacyMetadata).
// A bug labeled {bug, polish} is pushed: type Bug, label polish. The next
// pull adds no operation (echoes claimed), and git-bug's labels are unchanged.
// An issue labeled bug the old way is pulled, then pushed (type Bug, label
// gone), then pulled again: nothing new.
```

- [ ] **Step 2: Run the tests to check they fail**

Run: `go test ./bridge/github/ -run 'SetsType|MovesKind|KindsOff|TypeWrite|KindsSync'`
Expected: FAIL (types are never set).

- [ ] **Step 3: Implement `changeType`; `ChangeLabels` calls it first, when `r.kinds`**
  - No label of `added` or `removed` with a `typeID`: return them as they
    are, with no read.
  - Otherwise:
    - read the current type with `issueTypeOfQuery`
      (`node(id:) { ... on Issue { issueType { name } } }`);
    - call `core.OneNative(kindLabel(current), added, removed, hasType)`;
    - if `next` changed, mutate `updateIssueIssueType` with `typeID(next)`,
      or nil to clear it.
  - An `ErrRefused` from that mutation refuses `next`, or the current label
    when clearing: it leaves `add` and `remove`, joins `refused`, and
    `ChangeLabels` returns it with the assignees' refusals.
  - Order: type, then labels added, then assignees, then labels removed, so
    an old-way label comes off only once the type is set.

- [ ] **Step 4: Run the tests to check they pass**

Run: `go test ./bridge/github/ ./bridge/core/...`
Expected: PASS.

- [ ] **Step 5: Commit**

`bridge/github: a kind with a type is set as the issue's type`

### Task 4: GitLab: milestones come in as sprint labels

**Files:**
- Modify: `bridge/gitlab/gitlab_api.go` (`MilestoneEvents`, on the model of
  `LabelEvents`)
- Modify: `bridge/gitlab/event.go` (`type MilestoneEvent struct{
  gitlab.MilestoneEvent }`; `ID()` is `"milestone-<id>"`, as its ids may
  collide with label events')
- Modify: `bridge/gitlab/remote.go`:
  - `NewRemote`, `newRemote`, and the `sprints`, `mu` and `milestones`
    fields;
  - `convert`, `Issues` and `Fetch`;
  - new `readMilestones` and `sprintLabel`.
- Modify: `bridge/gitlab/remote_test.go`, the fake GitLab:
  - its milestones, project and group;
  - `GET /api/v4/projects/{pid}/milestones` and
    `GET …/issues/{iid}/resource_milestone_events`;
  - `setMilestone`.
- Create: `bridge/gitlab/sprints_test.go`

**Interfaces:**
- Consumes: `core.Slug`, `core.ConfigKeySprints`, `core.ModeLabels`.
- Produces:
  - `func sprintLabel(title string) string`, which is `"sprint:" +
    core.Slug(title)`;
  - `func (r *remote) readMilestones(ctx context.Context) error`: fills
    `r.milestones map[string]int64`, ids by label, from all states with
    `IncludeAncestors`, paged by 100. A project's milestone wins over a
    group's with the same label;
  - `func (r *remote) milestoneID(label string) (int64, bool)`, from what
    `readMilestones` read. `ChangeLabels` calls `readMilestones` when it
    was never read;
  - the test helper `f.held(ref) (labels []string, milestone string)`, in
    `sprints_test.go`.

- [ ] **Step 1: Write the failing tests**

```go
// TestGitlabMilestonesAreSprints
f.setMilestone(alice, issue, "Sprint 2")
require.Equal(t, []string{"sprint:sprint-2"}, gl.Issue(ref.Number).Labels)
ev := last(gl.Issue(ref.Number).Events)
require.Equal(t, core.EventLabels, ev.Kind); require.Equal(t, []string{"sprint:sprint-2"}, ev.Added)
require.True(t, strings.HasPrefix(ev.ID, "milestone-"))
f.setMilestone(alice, issue, "") // removed
require.Empty(t, gl.Issue(ref.Number).Labels)
// TestGitlabSprintLabelsResent: label sprint:sprint-2 and milestone "Sprint 2" exists, none set
require.Equal(t, []string{"sprint:sprint-2"}, gl.Issue(ref.Number).Resend)
// label sprint:sprint-9 with no such milestone: not resent; label "sprint:Sprint 2": not resent (exact)
// TestGitlabSprintsOff: sprints = labels: milestone ignored, no milestone request (fake counts)
// TestGitlabSameTitleMilestones (Review Focus 3): group and project "Sprint 2", the project's id is used
// TestGitlabRenamedMilestone (Review Focus 5): rename "Sprint 2" to "Sprint 2b"
require.Equal(t, []string{"sprint:sprint-2b"}, gl.Issue(ref.Number).Labels)
```

- [ ] **Step 2: Run the tests to check they fail**

Run: `go test ./bridge/gitlab/ -run 'Sprint|Milestone'`
Expected: FAIL to compile (`setMilestone` undefined).

- [ ] **Step 3: Implement**
  - `convert`:
    - adds `sprintLabel(gi.Milestone.Title)` to `Labels`;
    - turns each milestone event into an `EventLabels`: `Added` for action
      `add`, `Removed` for `remove`, with its milestone's current title;
    - puts a GitLab label into `Resend` when `milestoneID` knows it and the
      issue's milestone is none or that label's.
  - `Issues` and `Fetch` call `readMilestones` first, when `r.sprints`.

- [ ] **Step 4: Run the tests to check they pass**

Run: `go test ./bridge/gitlab/`
Expected: PASS, conformance included.

- [ ] **Step 5: Commit**

`bridge/gitlab: milestones come in as sprint labels`

### Task 5: GitLab: ChangeLabels sets the milestone

**Files:**
- Modify: `bridge/gitlab/remote.go` (`ChangeLabels`)
- Modify: `bridge/gitlab/remote_test.go` (the fake's `PUT` takes
  `milestone_id`, null or a number)
- Test: `bridge/gitlab/sprints_test.go`

**Interfaces:**
- Consumes: Task 1's `core.OneNative`, and Task 4's `milestoneID` and
  `sprintLabel`.

- [ ] **Step 1: Live check on 42400 (scratch).** This settles the spec's
  open point: can the API assign a closed milestone?
  - Create a milestone `plan-check`, close it, and assign it to a scratch
    issue with `glab api -X PUT projects/42400/issues/<iid>
    -f milestone_id=<id>`.
  - Then remove the milestone and the test issue.
  - Assignable: `milestoneID` keeps closed milestones. Refused: it keeps
    open ones only, for writing. Either way, record the answer in
    sync-decisions.md.

- [ ] **Step 2: Write the failing tests**

```go
// TestGitlabChangeLabelsSetsMilestone (milestones "Sprint 2", "Sprint 3")
require.NoError(t, gl.ChangeLabels(ctx, bot, ref, []string{"sprint:sprint-2", "a"}, nil))
labels, ms := f.held(ref); require.Equal(t, []string{"a"}, labels); require.Equal(t, "Sprint 2", ms)
require.NoError(t, gl.ChangeLabels(ctx, bot, ref, []string{"sprint:sprint-3"}, []string{"sprint:sprint-2"}))
_, ms = f.held(ref); require.Equal(t, "Sprint 3", ms)
require.NoError(t, gl.ChangeLabels(ctx, bot, ref, nil, []string{"sprint:sprint-3"}))
_, ms = f.held(ref); require.Empty(t, ms)
require.NoError(t, gl.ChangeLabels(ctx, bot, ref, []string{"sprint:sprint-9"}, nil)) // no milestone: a label
// TestGitlabSprintsSync: an engine over the fake; push {sprint:sprint-2, a},
// then pull: no new operation; the label and milestone events are claimed.
```

- [ ] **Step 3: Run the tests to check they fail**

Run: `go test ./bridge/gitlab/ -run 'SetsMilestone|SprintsSync'`
Expected: FAIL (the milestone is never set).

- [ ] **Step 4: Implement**
  - When `r.sprints` and a label of `added` or `removed` has a
    `milestoneID`:
    - read the issue with `reader.Issues.GetIssue`;
    - call `core.OneNative(sprintLabel(current title) or "", …)`;
    - set `opt.MilestoneID`, or `opt.ResetMilestoneID`, when `next`
      changed, in the same single `UpdateIssue` as the labels.
  - Without such a label: no read, as today.

- [ ] **Step 5: Run the tests to check they pass**

Run: `go test ./bridge/...`
Expected: PASS.

- [ ] **Step 6: Commit**

`bridge/gitlab: a sprint with a milestone is set as the issue's milestone`

### Task 6: Help text, man pages, decisions

**Files:**
- Modify: `commands/bridge/bridge_sync.go`: the long help, after the
  assignees paragraph;
- Regenerate: `doc/man/git-bug-bridge-sync.1`, `doc/md/git-bug_bridge_sync.md`
  (`go generate`);
- Modify: `doc/design/bridges/sync-decisions.md`, `doc/design/bridges/native-fields.md`
  (status: built).

- [ ] **Step 1:** Write the help paragraph:
  - kinds are GitHub issue types where the organization has one, and
    sprints are GitLab milestones where one exists, labels otherwise;
  - the two keys, `git config git-bug.bridge.<name>.kinds labels` and
    `….sprints labels`;
  - `kind-types` and its format, with co2's flat example;
  - the same every-clone warning and `--no-resume` upgrade step as for
    assignees.
- [ ] **Step 2:** `go generate`, then `git status`: commit `doc/` only. Run
  `git checkout -- api/graphql` if it drifted.
- [ ] **Step 3:** Record in sync-decisions.md the calls made while building:
  - labels match exactly, and prefixes too, unlike assignees;
  - milestone event ids are `milestone-<n>`;
  - a project milestone wins over a group's with the same label;
  - types and milestones are read again at each pull;
  - the closed-milestone answer from Task 5;
  - a refused type write refuses only that label.
- [ ] **Step 4:** Check the whole branch:
  - `go test ./...`;
  - `go vet ./bridge/...`;
  - gofmt with Go 1.26, which prints nothing.
- [ ] **Step 5: Commit**

`doc: kinds and sprints in bridge sync's help, decisions`

### Task 7: Live tests (with the user)

Needs guilbep/git-board#29's scratch repository for GitHub. GitLab can run
first. Build into the scratchpad (`go build -o <scratchpad>/git-bug .`);
don't install. Work in a scratch clone with its own bridges, as earlier live
tests did.

- [ ] **Step 1: GitLab 42400.**
  - Create the milestones `Sprint 2` and `Sprint 3`.
  - A bug labeled `sprint:sprint-2` gets that milestone. A switch to
    `sprint:sprint-3` moves it, and `sprint:sprint-9` stays a label.
  - An old `sprint:sprint-2` label on a GitLab issue moves after
    `bridge pull --no-resume`, then a dry run, then a sync.
  - Run with `sprints = labels` too.
  - A second sync is empty.
- [ ] **Step 2: GitHub, on the EPFL-ENAC scratch repository**, with
  `kind-types = bug=Bug,feat=Feature`:
  - `bug` sets the type Bug, and `polish` stays a label;
  - `feat` added beside it is a label, and `bug` swapped for `feat` makes
    the type Feature;
  - a type set in GitHub's UI comes in as `bug` or `kind:task`;
  - an old `bug` label moves to the type;
  - run with `kinds = labels` too;
  - a second sync is empty.
- [ ] **Step 3:** Record the results in sync-todo.md. Clean the scratch
  issues and milestones, or leave them, as the user says. Report to the
  user, who merges into `bridge-sync` and tags.
