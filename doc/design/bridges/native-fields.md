# Native fields: kind and sprint

Decision doc for the `native-fields` branch (off `bridge-sync` at
`8227b073`, after `v0.11.1-sync.7`), weighed with the user on 2026-10-21.
Built (2026-10-21). The GitHub live test waits on the scratch repository
of guilbep/git-board#29. The rulings are recorded in
[sync-decisions.md](sync-decisions.md#assignees) with what they cost if
wrong.

## The principle

The user, 2026-10-20: seamless first, so native where the forge has the
concept, labels otherwise; and retro-compatible: what projects hold today
keeps working, and existing labels move to the native field, as the old
`assignee:*` labels moved to GitHub's assignees in sync.7 (`Issue.Resend`,
refusals, the per-project `assignees = labels` switch).

## Facts (checked 2026-10-21, read only)

- **Only organizations have issue types**: a user-owned repository answers
  `issueTypes: null` (guilbep/git-bug-sync-test). EPFL-ENAC has Task, Bug
  and Feature, enabled. An organization can have up to 25, made in its
  Settings › Planning › Issue types.
- **EPFL-ENAC uses them**: 542 typed issues (Bug 80, Feature 263, Task 199)
  in 31 repositories, 65 of them in co2-calculator (28 Bug, 33 Feature,
  4 Task, out of 1067 issues).
- **GitHub's API has them**: `Issue.issueType`, `issueTypeId` on
  `createIssue` and `updateIssue`, `updateIssueIssueType`, and the timeline
  events `IssueTypeAddedEvent`, `IssueTypeRemovedEvent` and
  `IssueTypeChangedEvent` (`issueType`, `prevIssueType`, `actor`,
  `createdAt`). The fork reads none of them today.
- **GitHub's issue fields** (an organization's own fields on issues) can be
  text, a single or multiple select, a date or a number. None is an
  iteration. EPFL-ENAC has Priority, Effort and two dates; out of scope here.
- **gitlab.epfl.ch is 19.4.2, not enterprise**:
  - Iterations and iteration cadences answer 404 for projects and groups.
    GitLab's docs list them as Premium.
  - Milestones are in the free edition. One belongs to a project or to a
    group; a group's milestone serves every project in it. An issue has one
    milestone, with optional start and due dates.
  - `resource_milestone_events` answers.
  - Neither the EPFL-ENAC group (28666) nor the wiki (41960) has milestones.
  - Scratch project 42400 is in a user namespace, so it has project
    milestones only.
  - The fork's client-go v1.46.0 has `MilestonesService`, `Issue.Milestone`
    and `ResourceMilestoneEvents`. The fork ignores milestones.
- **git-board**:
  - `git-board bridge sync` syncs `sprint:` with the Project's Sprint
    iteration, both ways. Titles match slugs through `[[sprints.list]]`,
    then `config.Slug`, so `Sprint 2` is `sprint:sprint-2`.
  - It fills a missing `kind:` from Project 40's Category, one way.
  - It writes the Project only with `[github-project] write = true`, which
    co2 has off.
- **The fork sends `kind:*` and `sprint:*` to GitHub as plain labels**: the
  push filters no namespace.
- **git-board doesn't pass `WithAssignees` yet**: its Settings page writes
  the clone's `assignees` key, and `[repos] assignees` is read but unused.
  Each switch below has the same every-clone constraint.

## kind:

- **On GitHub, a kind is the issue type when the organization has one for
  it, a label otherwise.** It is decided per value, so nothing is refused or
  goes missing on GitHub, and a user-owned repository keeps labels silently.
  Weighed and left:
  - always a type, others refused: six new types org-wide first, and kinds
    that don't show on GitHub until then;
  - labels only: co2's 65 types would never come in.
- **Each enabled type is one label**:
  - By default a type is `kind:<slug of its name>`: Bug is `kind:bug`, Task
    is `kind:task`.
  - The bridge key `kind-types` maps types to labels as git-bug stores them,
    over that default: `bug=Bug,feat=Feature` for co2, whose kinds are
    flat labels (no `[fields.kind]`), or `kind:feat=Feature` with
    git-board's default config. git-board writes the key from its config.
  - Type names match ignoring case. Labels match exactly, as the pull must
    give back the very label pushed for its echo to be recognized, so
    `Kind:bug` stays a label.
  - Revised with the user on 2026-10-21, once co2's flat kinds were found.
    Weighed and left: any label named like a type becomes that type, which
    would turn every synced repository's `documentation` into a type; and
    co2 moving to `kind:` labels first.
- **One type per issue**: the kind GitHub holds stays the type, and another
  goes as a label, so the round trip is stable. A change made on both sides
  follows "the later change wins", as labels do. Project 40's Category
  still only fills a missing kind, in git-board.
- **Existing data**:
  - Types already on GitHub come in as kind labels after the upgrade's full
    pull (`git bug bridge pull <bridge> --no-resume`). A dry run names them.
  - A type's label held as a label on GitHub goes in `Issue.Resend` when
    the issue has no type or that one: the push sets the type, and the
    label comes off.
- **Switch: `kinds = native|labels`**, default `native`, and it works as
  `assignees` does:
  - In `labels` mode the fork still reads types, and moves them to labels:
    the push adds the type's label and clears the type.
  - Switching moves them once each way, after the upgrade's full pull.
  - Decided with the user on 2026-10-21. A mode that ignored types would
    lose kinds: the pull rebuilds labels from what the forge holds now, so
    a kind held as a type would seem removed on GitHub at the first sync
    after switching.
  - The cost: in `labels` mode the fork clears types people set on GitHub.
- **GitLab and Gitea**: there is no equivalent, so `kind:` stays a label.

## sprint:

- **GitHub: the fork changes nothing.**
  - The native home is the Project's Sprint iteration, which git-board
    already syncs. GitHub has no iteration on the issue, and a milestone is
    per repository, with a due date only.
  - `sprint:*` labels keep going to GitHub as labels. With co2's Project
    writes off, they're the only GitHub copy sync writes.
  - Weighed and left: keeping them off GitHub where a Project holds sprints,
    which would be one more key in every clone.
- **GitLab: `sprint:<slug>` is the existing milestone with that slug, and
  stays a label when there is none**, the same rule as kind.
  - The fork looks through the project's milestones and its groups'
    (`include_ancestors`), open or closed.
  - A milestone's label is `sprint:` and git-board's `Slug` of its title:
    lower case, accents dropped, other characters' runs as `-`. It is
    matched exactly, as for kinds, so `sprint:Sprint 2` stays a label.
  - The fork creates no milestone. People set the dates, and choose project
    or group.
  - Weighed and left: creating missing milestones, as the fork knows
    neither their dates nor their titles; labels only.
  - Whether the API assigns a closed milestone isn't checked: the build
    checks it first.
- **Switch: `sprints = native|off`**, default `native` (decided with the
  user on 2026-10-21).
  - The fork never creates a milestone, so a project that wants sprints as
    labels just has none.
  - `off` is for a project that uses milestones for something else, such
    as releases: the fork ignores milestones then.
  - Set it before the project's first sync with this version. In `native`
    mode, milestones already on GitLab issues come in as `sprint:` labels,
    and a dry run names them.
  - Turned off later, git-bug drops the sprints that milestones held: the
    forge seems to have removed them. That also cleans up release
    milestones read as sprints by mistake.
- **One milestone per issue, and existing labels move** (`Issue.Resend`),
  as for kind.

## What to add

1. **Issue types in EPFL-ENAC, optional, by an org owner.** Until they
   exist, these kinds stay labels on GitHub. Each type shows in the type
   picker of every EPFL-ENAC repository, so the organization decides. All
   six make 9 of the 25 allowed.

   | kind | EPFL-ENAC type | To add (name: description) | co2's `kind-types` entry |
   |---|---|---|---|
   | bug | Bug | none | `bug=Bug` |
   | feat | Feature | none | `feat=Feature` |
   | polish | none | Polish: a small improvement to something that works | `polish=Polish` |
   | perf | none | Performance: faster or lighter, same behaviour | `perf=Performance` |
   | refactor | none | Refactor: a code change with no change in behaviour | `refactor=Refactor` |
   | docs | none | Documentation: docs only | `docs=Documentation` |
   | style | none | Style: formatting only | `style=Style` |
   | test | none | Test: tests only | `test=Test` |
   | none | Task | none | none: comes in as `kind:task` |

   co2's kinds are flat labels, so every kind needs an entry. A project
   with `kind:` labels needs one only where the slug of the type differs
   from its kind (`kind:feat=Feature`).
   EPFL-ENAC's Priority issue field (Urgent, High, Medium, Low) could
   hold `prio:p0`–`p3` natively; that's a later build.
2. **A private scratch repository in EPFL-ENAC** for the live test, such as
   `EPFL-ENAC/git-bug-native-fields-test`:
   - guilbep can write to it;
   - the token the bridge uses can reach it. A fine-grained token needs the
     repository, Issues read and write, Metadata read, and an admin's
     approval if the organization asks for one; a classic token needs
     `repo`;
   - it stays out of Project 40 and its automation.
3. **GitLab: nothing.** The live test runs on 42400 and creates its own
   milestones there. Group milestones are tested with the fake only.
4. **With the build, later**:
   - git-board writes `kind-types`, `kinds` and `sprints` into every clone,
     CI's included, as for `assignees`. For `kind-types` it needs each
     kind's type name (feat is Feature), which its config doesn't hold yet;
   - co2 upgrades with a full pull, then a dry run, then the user's sync.

## Building it

- Engine tests with the synctest fake first, then the adapters' fakes.
- Live tests:
  - GitHub types on the EPFL-ENAC scratch repository: Bug, Feature, and a
    kind with no type;
  - GitLab project milestones on 42400.
- Never co2-calculator, never Project 40 (read only).
- gofmt with Go 1.26 and `go generate`; don't commit its `api/graphql`
  drift.
- Never push `bridge-sync` or tags: the user merges and tags.
- Record every new ruling in sync-decisions.md with its *If wrong*.
