# One hostname per service, one stack per piece of work

This is the setup workmux was built around, and it's the thing that makes several
changes in flight at once bearable: every piece of work gets its own containers under
its own hostnames, so you can have two versions of the app running and talk to both.

```
http://myapp1.localhost              the app for slot 1
http://api.myapp1.localhost          its API
http://admin.myapp1.localhost        its admin
http://myapp2.localhost              a *different* worktree's app, running at the same time
http://dev.localhost                 workmux itself
http://traefik.localhost/dashboard/  the proxy's own dashboard (note the path)
```

**`.localhost` needs no configuration.** Every `*.localhost` name resolves to
127.0.0.1 on macOS and on modern Linux — no `/etc/hosts`, no dnsmasq, no TLS. That's
the whole trick: one reverse proxy on port 80 routes by `Host`, and each stack claims
its own names.

## The pieces

| file | |
| --- | --- |
| `compose.proxy.yaml` | Traefik, started **once** and left running. Owns port 80. |
| `compose.yaml` | your app, started **per slot**. Claims hostnames through labels. |
| `workmux.json` | tells workmux the slot pattern and where to find the app |

## Try it

```sh
# once, and leave it running
docker compose -f compose.proxy.yaml up -d

# a stack for the first piece of work
docker compose -p myapp1 up -d
open http://myapp1.localhost

# a second one, at the same time, from another worktree
docker compose -p myapp2 up -d
open http://myapp2.localhost
```

Then `workmux` gives you ▶ app / ■ stop / ↻ restart per worktree, an ↗ open button
pointing at that slot's URL, and `＋ logs` tailing that slot's containers.

## How the labels work

The interesting line is in `compose.yaml`:

```yaml
labels:
  - traefik.http.routers.web-${COMPOSE_PROJECT_NAME}.rule=Host(`${COMPOSE_PROJECT_NAME}.localhost`)
```

`COMPOSE_PROJECT_NAME` is the slot — `myapp1`, `myapp2` — which docker compose sets
from `-p`. So the *same* compose file produces different hostnames per slot, with
nothing to edit and nothing to keep in sync. Router names include the slot too,
because two routers with one name silently overwrite each other and you get a 404 on
whichever stack lost.

Both stacks join an external `edge` network so the proxy can reach them; each stack
keeps its own internal network for its own services.

## Wiring it to workmux

```jsonc
{
  "name": "myapp",
  "stack": {
    "slots": "myapp{n}",                 // one stack per worktree
    "url": "http://{slot}.localhost",    // the ↗ open button
    "profiles": "app"
  }
}
```

`{slot}` is substituted everywhere, so nothing repeats the hostname scheme.

## If your stack needs more than compose can do

trip1's does — per-slot databases, migrations, attaching to the proxy — so it points
the four stack commands at its own script:

```jsonc
"commands": {
  "up":      "STACK={slot} bin/dev up {path}",
  "restart": "STACK={slot} bin/dev restart",
  "stop":    "STACK={slot} bin/dev stop",
  "logs":    "STACK={slot} bin/dev logs"
}
```

Any of `{slot}`, `{path}`, `{compose}`, `{profiles}` and `{base}` are available.

## Reaching it from a phone

`.localhost` only resolves on the machine running it, so a phone can't use these
names. Two ways round it:

- **An SSH tunnel** — `ssh -L 4315:127.0.0.1:4315 yourbox` — for workmux itself. The
  app's hostnames still won't resolve, so this is for driving agents, not for
  browsing the app.
- **A real domain** for a shared box: point `*.dev.example.com` at it and change one
  line, `Host(`${COMPOSE_PROJECT_NAME}.dev.example.com`)`, plus `stack.url` to match.
  Then add TLS at the proxy — workmux hands out shells, so don't put it on a network
  you don't trust without it.
