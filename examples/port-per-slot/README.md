# One port per piece of work

The simplest stack setup that actually works with several changes in flight: each
worktree's app gets its own port, and nothing else has to exist. No reverse proxy,
no hostnames, no DNS.

```
http://localhost:8081        the app for slot 1
http://localhost:8082        a *different* worktree's app, at the same time
```

Start here. Move to [`../localhost-routing`](../localhost-routing) when one port per
stack stops being enough — when the app has an API and an admin on their own
hostnames, or when a cookie domain or an OAuth callback cares what the host is.

## The idea

A **slot** is one running copy of your app, belonging to one worktree. workmux names
them from the `slots` pattern — `myapp1`, `myapp2` — and that name is the docker
compose project name, which is what keeps two stacks from colliding: separate
containers, separate networks, separate volumes, for free.

The only thing left to separate by hand is the published port, because that's the
one resource docker can't scope to a project. So the slot number becomes the port:

```
slot myapp1  →  PORT=8081  →  http://localhost:8081
slot myapp2  →  PORT=8082  →  http://localhost:8082
```

## The pieces

| file | |
| --- | --- |
| `compose.yaml` | your app. Publishes `${PORT}`, defaults to 8080 so a bare `docker compose up` still works |
| `workmux.json` | the slot pattern, the URL, and the four commands that carry `PORT` in |

## Try it

```sh
PORT=8081 docker compose -p myapp1 up -d
open http://localhost:8081

# a second one, at the same time, from another worktree
PORT=8082 docker compose -p myapp2 up -d
open http://localhost:8082
```

Then `workmux` gives you ▶ start app / ■ stop / ↻ restart per worktree, an ↗ open
button pointing at that slot's port, and Logs tailing that slot's containers.

## How the number gets in

`{n}` is the slot number, substituted into `url` and into every stack command:

```jsonc
{
  "stack": {
    "slots": "myapp{n}",                    // myapp1, myapp2, …
    "url": "http://localhost:808{n}",       // the ↗ open button
    "commands": {
      "up": "PORT=808{n} docker compose -p {slot} -f {compose} up -d --build"
    }
  }
}
```

The same substitution in both places is the point: the port scheme is written once
and the Open button cannot drift from what actually got started.

`stop` and `logs` carry `PORT` too. They don't need it to do their job, but compose
interpolates every variable in the file before it does anything, and an unset one is
a warning on every invocation.

**This holds for slots 1–9.** `808{n}` is string substitution, not arithmetic, so
slot 10 would be port 80810 — which is not a port. If you genuinely run ten stacks
at once, use a wider stride (`"url": "http://localhost:8{n}00"`, ports 8100…8900) or
do the arithmetic in a script, as below.

## If your stack needs more than compose can do

Point the four commands at your own script. Per-slot databases, migrations, seeding,
a proxy to attach to — none of that belongs in a URL template:

```jsonc
"commands": {
  "up":      "bin/dev up {slot} {path}",
  "restart": "bin/dev restart {slot}",
  "stop":    "bin/dev stop {slot}",
  "logs":    "bin/dev logs {slot}"
}
```

`{slot}`, `{n}`, `{path}`, `{compose}`, `{profiles}` and `{base}` are all available,
and `$STACK` and `$COMPOSE_PROFILES` are in the environment. The command runs through
a login shell, from the worktree, so a script that expects a real PATH gets one.

## Why not just one stack?

Because then a second piece of work has to wait for the first, and the whole reason
for a worktree per change is not waiting. Two slots means you can leave a branch
running while you review another — and a stack is optional per piece of work, so
most changes never start one at all.
