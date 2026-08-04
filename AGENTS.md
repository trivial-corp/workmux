# Instructions for agents

Two jobs live here: setting workmux up for a project, and working on workmux
itself. They have almost nothing in common — read the one you're doing.

---

## Setting workmux up for a project

**Run `workmux init` first and read what it prints.** It reports the name, base
branch, worktree location, stack, agent and the gitignored files a new worktree
would be missing, and it writes config only for that last one. Most repos need
nothing else.

```
workmux init --dry-run     # look first
workmux init               # write it, if there's anything to write
workmux                    # serve the repo you're in
```

Then **start it and check the dashboard is right** — worktrees listed, the running
stack recognised, agents attached to the right worktrees. `--verbose` logs every
subprocess with its exit code, which is where a wrong answer always comes from.

### What to add, and only when it's true

| symptom | fix |
| --- | --- |
| the app has a URL but no Open button | `"stack": {"url": "http://localhost:8080"}` |
| the project has containers but they're not this repo's work | `"stack": null` |
| `docker compose` isn't how this project starts | `stack.commands` — see the README |
| no coding agent is used here | `"agent": null` |
| a different agent CLI | `agent.command` plus whichever of `spawn` / `attach` / `jobs` / `mcp` it supports |
| new worktrees are missing a file | add it to `worktrees.copy` |

### Rules

- **Never write a key whose value equals the default.** A config file of restated
  defaults goes stale silently and hides which decisions were real. If `init` says
  "nothing to configure", the correct outcome is no file.
- **Don't guess `stack.url`.** A wrong link is worse than no button. Ask, or read
  the compose file's published ports and confirm.
- **Don't add an `agent` block to use Claude Code.** That's the default.
- Config lives at the **repo root** (`workmux.json`), next to the code it
  describes — not in a dotfile directory, and not in workmux's own repo.
- `worktrees.copy` patterns read like `.gitignore`: no slash matches that basename
  at any depth, a slash anchors to the repo root.

---

## Working on workmux itself

```
make dev ROOT=~/path/to/some-repo    # go run, frontend from disk, subprocesses logged
make test                            # go test ./...
make lint                            # gofmt -l must be empty, then go vet
```

Run `make lint && make test` before committing. Both are what CI runs.

### How this code is built

- **The standard library first.** The server has no dependencies and the binary
  embeds its frontend, so `workmux` is one file that works offline. A dependency
  needs to earn itself against that. (PTY and WebSocket handling will each take
  one, and that's the expected total.)
- **Every subprocess goes through `internal/run`.** Always a timeout, stdout and
  stderr together, never a shell unless a shell is the point. It's also where the
  `--verbose` trace hooks in, so a call that bypasses it is invisible when
  debugging.
- **Degrade, don't fail.** `gh` may be absent, docker may be stopped, an agent may
  never have run. Each of those costs you one column and nothing else. A dashboard
  that renders is worth more than one that's complete.
- **Absent capability, absent affordance.** When config leaves something blank the
  UI must not offer it. A button that answers "not configured" is worse than no
  button, and it's what makes the tool honest on a repo that isn't trip1.
- **Comments explain why, not what.** Especially for anything that looks
  arbitrary: it's usually a bug that was expensive to find. Don't delete one
  without understanding it.
- **JSON field names are a contract.** A browser is one client; a mobile app is
  another and can't update in lockstep. Empty lists serialise as `[]`, never
  `null` — clients iterate without checking.

### Tests

Real git repositories in temp directories, never mocks. Every claim this tool
makes about worktrees, branches and commit counts comes from git's own output, so
a fake would only test the fake. `internal/testrepo` builds them: `New`,
`Worktree`, `Commit`, `FakeOrigin`.

Two invariants have their own tests because both were bugs, and both look like
nothing:

- **Ownership is longest-prefix, not first-match.** Worktrees live *inside* the
  primary checkout, so "is this directory under that worktree" is true of the
  primary for every worktree — which marked the base checkout permanently busy.
- **Paths are resolved through symlinks before anything compares them.** git
  prints resolved paths; on macOS `/var` is `/private/var`, so a repo reached by
  the unresolved path never matches its own worktree.

### Layout

```
cmd/workmux        flags, banner, wiring
internal/config    workmux.json + every default derived from the repo
internal/gitx      worktrees, base branch, drift, origin URL
internal/agents    which agents live where, which are working now
internal/stack      compose projects, service health, slots
internal/prs       pull requests via gh (optional)
internal/work      the assembly and the ordering
internal/web       HTTP, auth, the embedded frontend
internal/initcmd   the bootstrap
internal/testrepo  real git repos for tests
```

### Roadmap order

Terminal sessions (PTY + WebSocket) → changes and diffs → actions → MCP panel →
the frontend as a TS build → release binaries → paired device tokens and TLS. The
`python-original` branch is the previous implementation, kept runnable because it
still has the features `main` doesn't; read it when porting one of them.
