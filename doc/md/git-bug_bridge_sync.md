## git-bug bridge sync

Sync issues two-way with every bridge that has sync enabled

### Synopsis

Sync issues two-way with every bridge that has sync enabled, or the given ones.

Every bridge is pulled first, then git-bug pushes to each of them, so that an
issue, comment or change made on one forge reaches all the others in one run.

A bridge takes part when sync is enabled for it:

    git config git-bug.bridge.<name>.sync true

With sync, everything is pushed, whichever forge it comes from. What was
written by someone without a token on a forge is posted there with the
bridge's default-login token, under a header naming its author.

Run it from a single place, for example a CI job:

    git bug pull && git bug bridge sync && git bug push

The first sync of a repository mirrors every issue. Use --dry-run to see what
it would do first.

```
git-bug bridge sync [NAME]... [flags]
```

### Options

```
      --dry-run   Report what would be pulled and pushed, and change nothing
  -h, --help      help for sync
```

### SEE ALSO

* [git-bug bridge](git-bug_bridge.md)	 - List bridges to other bug trackers

