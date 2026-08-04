# workmux — the original python implementation

This branch is the implementation that came before the Go rewrite on `main`: one
~4,500-line stdlib python file that has been in daily use, and which still does
things the Go version hasn't caught up to yet.

```
python3 workmux.py --root /path/to/your/repo
```

Needs python 3.9+ (it runs on the one macOS ships) and git. Nothing else — xterm.js
is vendored in `assets/`.

**Use `main` unless you need something below.** This branch exists so that nothing
was lost in the move out of the trip1 monorepo, and so there is a working dashboard
during the port. It will be deleted once `main` reaches parity.

| | main (Go) | this branch (python) |
| --- | --- | --- |
| worktrees · agents · PRs · stacks · ordering | ✓ | ✓ |
| config, optional stack, configurable agent | ✓ | ✓ |
| terminal sessions (PTY over WebSocket, replay on reattach) | — | ✓ |
| changes: file status, per-file diffs, commits vs base | — | ✓ |
| actions: new work, start/stop a stack, merge base, check out a PR | — | ✓ |
| MCP panel: reachability, scope, authenticate | — | ✓ |
| mobile layout, key bar, font size | — | ✓ |
| single static binary, no runtime | ✓ | — |

See `main`'s README for the `workmux.json` schema; both read the same file.
