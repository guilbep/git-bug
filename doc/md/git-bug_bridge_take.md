## git-bug bridge take

Replace an issue's unsent changes with its version on a forge

### Synopsis

Replace what git-bug hasn't sent to a bridge's forge yet, for one issue, with
the forge's version: the title, description, comments' texts, status and
labels the forge has now. What changed on both sides is the forge's too.

Nothing is written to the forge, and nothing is lost: git-bug keeps every
version in the issue's history. Comments made in git-bug stay, and the next
sync sends them. The issue's other forges get this version at the next
sync.

```
git-bug bridge take BUG_ID BRIDGE [flags]
```

### Examples

```
# an edit made in git-bug by mistake, not synced yet: take GitLab's back
git bug bridge take 4f2a1c9 gl

# after linking, keep the forge's version instead of git-bug's
git bug bridge link 4f2a1c9 gl 12
git bug bridge take 4f2a1c9 gl
```

### Options

```
  -h, --help   help for take
```

### SEE ALSO

* [git-bug bridge](git-bug_bridge.md)	 - List bridges to other bug trackers

