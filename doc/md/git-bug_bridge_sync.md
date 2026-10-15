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

The first sync of a repository mirrors every issue. --dry-run reads every
forge and lists what would be pulled and pushed, and changes nothing, locally
or on the forges.

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
```

### Options

```
      --dry-run   Report what would be pulled and pushed, and change nothing
  -h, --help      help for sync
```

### SEE ALSO

* [git-bug bridge](git-bug_bridge.md)	 - List bridges to other bug trackers

