## git-bug bridge link

Sync a git-bug issue with an issue already on a forge

### Synopsis

Sync a git-bug issue with an issue already on a bridge's forge, instead of
creating it there again. ISSUE is the forge issue's number or URL.

Use it when the same issue is on both sides, for example copied by hand, or
by a sync from another repository. Nothing is written to the forge now. The
next sync makes them one:

  - the title, description and status are git-bug's: they are sent as
    changes not sent yet (to keep the forge's instead, run
    "git bug bridge take" right after linking);
  - the comments and labels of each side are added to the other.

It refuses a forge issue already synced with another git-bug issue, and a
git-bug issue that has, or had, an issue on that forge.

```
git-bug bridge link BUG_ID BRIDGE ISSUE [flags]
```

### Examples

```
git bug bridge link 4f2a1c9 gl 12
git bug bridge link 4f2a1c9 gh https://github.com/example-owner/example-repo/issues/12

# what the next sync would send, then send it
git bug bridge sync --issue 4f2a1c9 --dry-run
git bug bridge sync --issue 4f2a1c9
```

### Options

```
  -h, --help   help for link
```

### SEE ALSO

* [git-bug bridge](git-bug_bridge.md)	 - List bridges to other bug trackers

