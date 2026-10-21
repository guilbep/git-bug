## git-bug bridge sync

Sync issues two-way with every bridge that has sync enabled

### Synopsis

Sync issues two-way with every bridge that has sync enabled, or the given ones.

Every bridge is pulled first, then git-bug pushes to each of them, so that an
issue, comment or change made on one forge reaches all the others in one run.

A bridge takes part when sync is enabled for it, by "git bug bridge new --sync"
or later with:

    git config git-bug.bridge.<name>.sync true

With sync, everything is pushed, whichever forge it comes from. What was
written by someone without a token on a forge is posted there with the
bridge's default-login token, under a header naming its author.

Each forge account is a git-bug identity, so the same person on two forges is
two identities, except token owners. To make someone one identity, link their
login on each bridge to it:

    git config git-bug.bridge.<name>.login-<identity-id> <login>

Run it from a single place, for example a CI job:

    git bug pull && git bug bridge sync && git bug push

git-bug's assignee:<name> labels name a person, by their identity's login
or the login "bridge new" tagged them with. On GitHub and GitLab, they are
the assignees of that person's own account there: the one linked as above,
or the one they were seen as. Someone with no account on a forge, or a
name several people share, isn't assigned there, with a warning; a
--dry-run lists them. GitLab's free edition takes one assignee per issue.
To hold them as labels on every forge instead, for a project that wants
several or people without access, set it in every clone that syncs, a CI
job's included: clones holding them differently undo each other's syncs.
git-board gives its own runs the project's setting, but this command reads
only the key:

    git config git-bug.bridge.<name>.assignees labels

After a change, "git bug bridge pull <name> --no-resume" reads every issue
once, so that the next sync moves the assignees (a --dry-run lists them).
A value other than native or labels stops the sync of every bridge, even
those of forges that don't map assignees yet (Gitea).

Comments written for a pull request (line comments, replies and reviews,
anchored by their review:* metadata, as git-board writes them) go to that
pull request on GitHub, never to an issue. This command doesn't know which
pull requests are an issue's, so it brings none of their comments in:
git-board does.

The first sync of a repository mirrors every issue. --dry-run reads every
forge and lists what would be pulled and pushed, and changes nothing, locally
or on the forges.

--issue syncs one issue only: its changes on each forge it is on are
pulled, then its changes pushed to every forge, creating it where a sync
would. Nothing else is pulled or written, and the next full sync still
reads everything changed since the last one. On a forge it isn't known
on yet, the issues changed since it was created are read, to find one
that a sync from another clone created for it.

For one issue on one forge, see also "git bug bridge link" (it already is an
issue there), "git bug bridge take" (drop git-bug's changes not sent there)
and "git bug bridge unlink" (stop syncing it there).

```
git-bug bridge sync [NAME]... [flags]
```

### Examples

```
# In a repository already holding git-bug issues, get them and adopt your
# identity first, so that the bridges tag it with your logins
git bug pull origin
git bug user adopt <your-identity-id>

# One bridge per forge, with sync, each using its stored token
git bug bridge new --name gh --url https://github.com/example-owner/example-repo --sync
git bug bridge new --name gl --target gitlab --url https://gitlab.example.com/example-org/example-repo --sync

# Alice is alice on GitHub and alice-ex on GitLab: one identity, 22ea975
git config git-bug.bridge.gh.login-22ea975 alice
git config git-bug.bridge.gl.login-22ea975 alice-ex

git bug bridge sync --dry-run
git bug bridge sync
git bug push origin

# Only one issue, right now
git bug bridge sync --issue 4f2a1c9
```

### Options

```
      --dry-run        Report what would be pulled and pushed, and change nothing
      --issue string   Sync only this issue (an id prefix)
  -h, --help           help for sync
```

### SEE ALSO

* [git-bug bridge](git-bug_bridge.md)	 - List bridges to other bug trackers

