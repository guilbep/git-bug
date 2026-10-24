## git-bug bridge unlink

Stop syncing a git-bug issue with a forge

### Synopsis

Stop syncing a git-bug issue with its issue on a bridge's forge.

Both keep what they have: nothing is deleted, here or on the forge. From now
on, sync doesn't send the git-bug issue to that forge, nor create it there
again, nor pull the forge issue, which can be linked to another git-bug
issue with "git bug bridge link". The issue's other bridges still sync it.

This can't be undone: once unlinked from a forge, a git-bug issue can't be
linked to an issue of that forge again.

```
git-bug bridge unlink BUG_ID BRIDGE [flags]
```

### Examples

```
git bug bridge unlink 4f2a1c9 gl
```

### Options

```
  -h, --help   help for unlink
```

### SEE ALSO

* [git-bug bridge](git-bug_bridge.md)	 - List bridges to other bug trackers

