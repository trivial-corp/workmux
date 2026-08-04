# workmux

Run several coding agents at once — one git worktree each — from a browser.

```
workmux
```

It serves the repo you're standing in at <http://127.0.0.1:4315>. A single static
binary, no runtime, no config, and it works from a phone.

> **Status: the Go rewrite is in progress.** The dashboard, the API and the
> agent↔worktree mapping are here and tested. Terminal sessions, the diff pane and
> the MCP panel are being ported from the original (a ~4,500-line Python
> implementation that has been in daily use); see [Roadmap](#roadmap). The `main`
> branch always builds and passes its tests.

## Why

Changes come in different sizes. Some are a backend tweak that needs nothing
running; some need Docker up to test at all. You start three of them, hand each to
an agent, and then spend your day answering the question *which of these wants me
right now* — across terminal tabs, worktrees, and containers that may or may not
be up.

workmux makes **a piece of work** the unit: a worktree + branch, the agents living
in it, the PRs it has produced, its diff, and — only if the change needs it — its
own container stack. The list is ordered by what wants you: needs-input first,
then working, then whatever has containers up, then the rest.

Two details it gets right, because both were bugs first:

- **"Working" means a live process**, not what an agent last wrote about itself.
  Agent state is written at turn boundaries, so the session you are watching work
  reads *idle*.
- **The base checkout sorts last** however busy it looks. It's where you happen to
  stand, not a change in flight — and worktrees live *inside* it, so a naive
  prefix check marks it permanently busy.

## Install

```
go install github.com/trivial-corp/workmux/cmd/workmux@latest
```

Or build it:

```
git clone https://github.com/trivial-corp/workmux && cd workmux && make build
```

Requires git. `docker` matters only if your project has a stack, `gh` only for PR
titles and authors — both optional, and their absence costs you that column and
nothing else.

macOS and Linux.

## Configuration

None required. Drop a `workmux.json` at the repo root when a default is wrong:

```jsonc
{
  "name": "myapp",                        // default: the repo directory's name
  "worktrees": {
    "path": ".claude/worktrees",          // default
    "copy": [".env", "config/*.key"]      // gitignored files each new worktree needs
  },
  "agent": {                              // defaults are Claude Code; null for none
    "command": "claude",                  // what a new agent session runs
    "spawn": "claude --bg {prompt}",      // how New work starts one
    "attach": "claude attach {id}",       // how to take over a running one
    "jobs": "~/.claude/jobs",             // where its state.json files live
    "mcp": "claude mcp"                   // enables the MCP panel
  },
  "stack": {                              // omit to detect; null for "no app"
    "compose": "compose.yaml",            // default: the compose file it finds
    "slots": "myapp{n}",                  // default: "{name}{n}" — one per work
    "url": "http://{slot}.localhost",     // default: none, so no Open button
    "profiles": "app,tools",              // default: $COMPOSE_PROFILES
    "commands": {                         // default: plain `docker compose`
      "up": "docker compose -p {slot} -f {compose} up -d --build",
      "restart": "...", "stop": "...", "logs": "..."
    }
  }
}
```

**Anything left blank is a capability the UI doesn't offer**, rather than a button
that fails:

| you leave out | you get |
| --- | --- |
| a compose file, or `"stack": null` | worktrees, agents, sessions and diffs — no app controls at all |
| `agent.spawn` | New work makes the worktree without starting an agent |
| `agent.mcp` | no MCP panel |
| `"agent": null` | no agents anywhere; worktrees and shells |

`{slot}`, `{compose}`, `{profiles}`, `{path}` and `{base}` are substituted into
stack commands, so a project with its own wrapper script (per-slot databases, a
reverse proxy to attach) keeps using it.

## Options

```
workmux [options]

  -p, --port N       port to listen on (default 4315)
      --host ADDR    interface to bind (default 127.0.0.1)
      --token TOK    pin the token instead of minting one
      --root DIR     repository to serve (default: the current directory)
      --no-terminal  dashboard only — don't serve shells
      --open         open a browser once it's listening
```

Every flag has a `WORKMUX_*` environment variable (`WORKMUX_PORT`, `WORKMUX_HOST`,
`WORKMUX_TOKEN`, `WORKMUX_ROOT`, `WORKMUX_TERMINAL=0`).

## Reaching it from your phone

```
workmux --host 0.0.0.0
```

It prints a LAN URL with a token. Off-loopback requests need it; loopback stays
exempt, so nothing changes on the machine itself. `?t=…` is swapped for an
`HttpOnly` cookie on first load.

> **This hands out a shell over plain HTTP.** That's why it binds loopback only by
> default, refuses cross-origin WebSocket upgrades (CORS doesn't cover
> WebSockets), and requires a token off-box. Put TLS in front before exposing it
> beyond a network you trust, and use `--no-terminal` if you only want the
> dashboard.

## API

The browser is one client; a mobile app is another. Everything the UI renders
comes from the same JSON:

| endpoint | |
| --- | --- |
| `GET /api/work` | the whole dashboard: worktrees, agents, PRs, stacks, sessions, capabilities |
| `GET /api/config` | the resolved project shape |
| `GET /api/health` | liveness, outside the token check so a proxy can probe it |

## Roadmap

- [x] Config resolution with defaults, optional stack, configurable agent
- [x] Worktrees, agents, PRs, stacks, ordering — `GET /api/work`
- [x] Token auth, origin allowlist, embedded UI
- [ ] Terminal sessions: PTY + WebSocket, replay on reattach, size arbitration
- [ ] Changes: status, per-file diffs, commits this branch has that its base doesn't
- [ ] Actions: new work, start/stop a stack, merge the base in, check a PR out
- [ ] MCP panel: reachability, scope, authenticate
- [ ] The frontend as a proper TS build, embedded — shared with a mobile app
- [ ] Release binaries, Homebrew, and an npm package that ships the binary
- [ ] Paired device tokens and TLS, for the remote/mobile case

## Using it on another project

Point it anywhere; nothing has to be installed in the project itself:

```
workmux --root ~/code/my-drupal-site
```

With no `workmux.json` it reads the repo: the name from the directory, a stack
from whatever compose file it finds, and Claude Code as the agent. A stack the
project already has running is recognised whether it's named after the directory
(what `docker compose up` does) or numbered per worktree.

Two things are worth setting for a project that isn't structured around
per-worktree stacks:

```jsonc
{
  "stack": { "url": "http://localhost:8080" },   // so there's an Open button
  "agent": null                                   // if you don't use one here
}
```

## Developing workmux

**Run it from source, against any project. No build, no install, no CI:**

```
make dev ROOT=~/code/my-drupal-site          # or PORT=4321 to sit beside another
```

That's `go run`, so a code change is a Ctrl-C and a re-run — about a second. Two
things make it a debug loop rather than a guessing game:

- **`--dev` serves the frontend from `internal/web/dist` instead of the copy
  embedded in the binary**, with caching off. Editing the UI is a refresh.
- **Every subprocess is logged** with its duration and exit code. Every fact this
  tool reports comes from a `git`, `docker`, `gh` or agent-CLI call, so a wrong
  dashboard is nearly always a surprising command result, and this shows you which
  one:

  ```
  16:24:01    9ms git -C /repo symbolic-ref --quiet --short refs/remotes/origin/HEAD  exit 1:
  16:24:01   16ms git -C /repo worktree list --porcelain
  16:24:02  310ms docker compose ls --all --format json
  ```

  `--verbose` alone gives you that against the embedded frontend.

```
make watch ROOT=~/code/app    # restart on every .go change (watchexec or entr)
make debug ROOT=~/code/app    # under delve, with breakpoints
make install                  # ~/go/bin/workmux — what `bin/dev serve` finds
make test                     # go test ./...
make lint                     # gofmt check + go vet
```

Nothing here needs a push, a tag or a CI run. CI exists to catch what a laptop
misses (Linux, and the four cross-compiled binaries), not to stand between you and
running the code.

Tests use real git repositories in temp directories rather than mocks: every claim
this tool makes about worktrees, branches and commit counts comes from git's own
output, so a fake would only test the fake.

## License

MIT
