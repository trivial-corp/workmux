#!/usr/bin/env python3
# workmux — run several coding agents at once, one git worktree each, from a
# browser.
#
# The unit is a piece of work: a worktree + branch, the agents living in it, the
# PRs it has produced, its diff, and — only if the change needs the app to test
# it — its own container stack. Sessions are real PTYs streamed to xterm.js, so
# anything you'd do in a terminal (the agent, git, make, tests, a log tail) you
# do from the same screen, including from a phone.
#
# Zero dependencies: the stdlib, on the python3 that ships with macOS. Every
# fact is read straight from git, the agent CLI and docker compose, so there is
# no database and nothing to keep in sync.
#
#   python3 workmux/workmux.py                  → http://127.0.0.1:4315
#   python3 workmux/workmux.py --port 8080 --open
#   python3 workmux/workmux.py --host 0.0.0.0    → prints a URL + token for
#                                                  your phone
#
# Project shape lives in workmux.json at the repo root (--help prints the whole
# schema). Every key is derived from the repo when absent, so a repo needs no
# config at all, and a project with no containers gets the same tool minus the
# app controls rather than buttons that can't work.
#
# Terminals hand out a shell, so it binds loopback only, checks Origin on the
# WebSocket upgrade (CORS doesn't cover WebSockets), and requires a token off
# loopback. WORKMUX_TERMINAL=0 turns terminals off entirely.
#
# Needs: python3 >= 3.9 and git; docker only for the stack, gh only for PR
# titles and authors.

import base64
import collections
import fcntl
import fnmatch
import hashlib
import hmac
import json
import os
import pty
import re
import secrets
import select
import shlex
import shutil
import signal
import struct
import subprocess
import sys
import termios
import threading
import time
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

VERSION = "0.1.0"

USAGE = """workmux — run several coding agents at once, one git worktree each, from
a browser.

usage: workmux [options]

  -p, --port N       port to listen on (default 4315)
      --host ADDR    interface to bind (default 127.0.0.1). 0.0.0.0 reaches it
                     from a phone; off-loopback requests then need a token
      --token TOK    pin the token instead of minting one (empty to disable it,
                     when something in front already authenticates)
      --root DIR     repository to serve (default: the current directory)
      --no-terminal  dashboard only — don't serve shells
      --open         open a browser once it's listening
  -h, --help         this
  -V, --version      print the version

workmux.json at the repo root is optional — each key below falls back to what
the repo already says:

  {
    "name": "myapp",                        // the repo directory's name
    "worktrees": {
      "path": ".claude/worktrees",          // where new worktrees are made
      "copy": [".env", "config/*.key"]      // gitignored files each one needs
    },
    "agent": {                              // null for none; defaults are Claude Code
      "command": "claude",                  // what a new agent session runs
      "spawn": "claude --bg {prompt}",      // how New work starts one
      "attach": "claude attach {id}",       // how to take over a running one
      "jobs": "~/.claude/jobs",             // where its state.json files live
      "mcp": "claude mcp"                   // enables the MCP panel
    },
    "stack": {                              // omit to detect; null for "no app"
      "compose": "compose.yaml",
      "slots": "myapp{n}",                  // one stack per piece of work
      "url": "http://{slot}.localhost",
      "profiles": "app,tools",
      "commands": {                         // default: plain docker compose
        "up": "...", "restart": "...", "stop": "...", "logs": "..."
      }
    }
  }

Anything left blank is a capability the UI doesn't offer rather than a button
that fails: no "spawn" and New work just makes the worktree, no "mcp" and the
MCP panel is gone, no stack and the app controls are gone.
"""

_FLAG_ARG = {"-p": "port", "--port": "port", "--host": "host", "--token": "token",
             "--root": "root"}


def _cli(argv):
    """Flags beat environment; both are optional. Bad input exits, loudly."""
    opts, i = {}, 0
    while i < len(argv):
        a = argv[i]
        if a in ("-h", "--help"):
            sys.stdout.write(USAGE)
            raise SystemExit(0)
        if a in ("-V", "--version"):
            sys.stdout.write("workmux " + VERSION + "\n")
            raise SystemExit(0)
        if a == "--no-terminal":
            opts["no_terminal"] = True
        elif a == "--open":
            opts["open"] = True
        elif a in _FLAG_ARG or a.split("=", 1)[0] in _FLAG_ARG:
            key, _, inline = a.partition("=")
            if inline or "=" in a:
                val = inline
            else:
                i += 1
                if i >= len(argv):
                    sys.stderr.write("workmux: %s needs a value\n" % a)
                    raise SystemExit(2)
                val = argv[i]
            opts[_FLAG_ARG[key]] = val
        else:
            sys.stderr.write("workmux: unknown option %s (try --help)\n" % a)
            raise SystemExit(2)
        i += 1
    if "port" in opts and not opts["port"].isdigit():
        sys.stderr.write("workmux: --port wants a number\n")
        raise SystemExit(2)
    return opts


OPTS = _cli(sys.argv[1:])
HOST = OPTS.get("host") or os.environ.get("WORKMUX_HOST", "127.0.0.1")
LOOPBACK = {"127.0.0.1", "::1", "localhost", "0:0:0:0:0:0:0:1"}
# Binding anywhere but loopback publishes a shell, so it gets a token. Loopback
# stays open: the browser is already on the machine it would be logging into.
# --token / WORKMUX_TOKEN pins it (a proxy config, a bookmark); otherwise one is
# minted per start and printed. An empty value disables the check entirely —
# only sane when something in front is already authenticating.
TOKEN = OPTS["token"] if "token" in OPTS else os.environ.get("WORKMUX_TOKEN")
if TOKEN is None:
    TOKEN = "" if HOST in LOOPBACK else secrets.token_urlsafe(16)
PORT = int(OPTS.get("port") or os.environ.get("WORKMUX_PORT", "4315"))
# ── configuration ─────────────────────────────────────────────────────────────
# Everything project-shaped lives in workmux.json at the repo root, and every
# key has a default worked out from what's actually there. A repo with a compose
# file gets a stack; a repo without one (a homelab, a library, a pile of
# scripts) gets the same tool minus the app controls, rather than a dashboard
# full of buttons that can't work.
DEFAULT_COMPOSE = ["compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"]
# {slot} {compose} {profiles} {path} {base} are substituted.
DEFAULT_COMMANDS = {
    "up": "docker compose -p {slot} -f {compose} up -d --build",
    "restart": "docker compose -p {slot} -f {compose} restart",
    "stop": "docker compose -p {slot} -f {compose} down --remove-orphans",
    "logs": "docker compose -p {slot} -f {compose} logs -f --tail 200 --no-color",
}


# Claude Code is the default because it's what this was built against, but every
# way the tool touches an agent is a template, so another CLI is config rather
# than a patch. Only the presets below can be inferred from a command name;
# anything else has to say how it spawns, attaches and (if it has one) does MCP.
AGENT_PRESETS = {
    "claude": {"spawn": "claude --bg {prompt}", "attach": "claude attach {id}",
               "jobs": "~/.claude/jobs", "mcp": "claude mcp"},
}
AGENT_KEYS = ("spawn", "attach", "jobs", "mcp")


def _load_agent(cfg):
    """The agent block, with each capability defaulted or left off."""
    raw = cfg.get("agent", {})
    if raw is None:                        # "there is no agent here"
        return {"command": "", "process": "", "name": "",
                **{k: "" for k in AGENT_KEYS}}
    agent = dict(raw)
    command = (agent.get("command") or "claude").strip()
    exe = os.path.basename(shlex.split(command)[0]) if command else ""
    preset = AGENT_PRESETS.get(exe, {})
    for k in AGENT_KEYS:
        agent.setdefault(k, preset.get(k, ""))
    agent["command"] = command
    # What "running" means: a live process of this name in a worktree. tempo in
    # state.json is a turn-boundary snapshot, so it lags by a whole turn.
    agent.setdefault("process", exe)
    agent.setdefault("name", exe or "agent")
    return agent


def _load_config(root):
    cfg = {}
    for name in ("workmux.json", ".workmux.json"):
        try:
            with open(os.path.join(root, name)) as f:
                cfg = json.load(f)
            break
        except (OSError, ValueError):
            continue
    cfg.setdefault("name", os.path.basename(root) or "dev")
    wt = cfg.get("worktrees") or {}
    wt.setdefault("path", os.path.join(".claude", "worktrees"))
    # Gitignored files a fresh worktree can't work without: .env, service-account
    # keys, credential masters. Nobody remembers to copy them by hand, and the
    # failure shows up minutes later as a confusing crash.
    wt.setdefault("copy", [])
    cfg["worktrees"] = wt
    cfg["agent"] = _load_agent(cfg)
    stack = cfg.get("stack", {})
    if stack is None:                      # explicitly "this project has no app"
        cfg["stack"] = None
        return cfg
    compose = stack.get("compose")
    if not compose:
        compose = next((c for c in DEFAULT_COMPOSE if os.path.isfile(os.path.join(root, c))), "")
    if not compose:                        # nothing to run → no stack, no buttons
        cfg["stack"] = None
        return cfg
    stack["compose"] = compose
    stack.setdefault("slots", cfg["name"] + "{n}")
    stack.setdefault("url", "")            # no Open button unless someone says where
    stack.setdefault("profiles", os.environ.get("COMPOSE_PROFILES", ""))
    stack["commands"] = dict(DEFAULT_COMMANDS, **(stack.get("commands") or {}))
    cfg["stack"] = stack
    return cfg


def _repo_root(start):
    """The primary checkout, so it works from a subdirectory — or from
    inside one of the worktrees it manages, where config lives one level up."""
    try:
        p = subprocess.run(["git", "worktree", "list", "--porcelain"], cwd=start,
                           stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                           text=True, timeout=8)
        for line in (p.stdout or "").splitlines():
            if line.startswith("worktree "):
                return line[len("worktree "):].strip()
    except (OSError, subprocess.SubprocessError):
        pass
    return start


ROOT = os.path.abspath(OPTS.get("root") or os.environ.get("WORKMUX_ROOT") or os.getcwd())
if not OPTS.get("root") and not os.environ.get("WORKMUX_ROOT"):
    ROOT = _repo_root(ROOT)
CONFIG = _load_config(ROOT)
STACK = CONFIG.get("stack")
AGENT = CONFIG["agent"]
WORKTREES = CONFIG["worktrees"]["path"]
WT_COPY = CONFIG["worktrees"]["copy"]
PROJECT = CONFIG["name"]
PROFILES = (STACK or {}).get("profiles", "")
STACK_URL = ((STACK or {}).get("url") or "").replace("{slot}", PROJECT)
_SLOT_PAT = (STACK or {}).get("slots", PROJECT + "{n}")
STACK_RE = re.compile("^" + re.escape(_SLOT_PAT).replace(r"\{n\}", r"[0-9]+") + "$")


def slot_name(n):
    return _SLOT_PAT.replace("{n}", str(n))


def stack_url(slot):
    return ((STACK or {}).get("url") or "").replace("{slot}", slot)


def stack_cmd(action, slot="", path="", base=""):
    """A shell command for a stack action, from config or the compose default."""
    if not STACK:
        return ""
    tmpl = (STACK.get("commands") or {}).get(action, "")
    return tmpl.format(slot=slot, path=path, base=base, compose=STACK["compose"],
                       profiles=STACK.get("profiles", ""))
# Where user-installed binaries live. Appended to PATH for sessions and health
# checks so a bare-command MCP server resolves the same way it does in a real
# terminal.
BIN_DIRS = ["~/go/bin", "~/.local/bin", "~/bin", "/opt/homebrew/bin", "/usr/local/bin",
            "~/.npm-global/bin", "~/.cargo/bin", "~/.bun/bin"]
HERE = os.path.dirname(os.path.abspath(__file__))
ASSETS = os.path.join(HERE, "assets")
TERMINAL = (not OPTS.get("no_terminal")
            and os.environ.get("WORKMUX_TERMINAL", "1") != "0")

# Actions run PER-SLOT so independent stacks can act at once (e.g. stop trip2
# and trip3 together). One action per slot at a time (same-slot overlaps would
# fight the same compose project). Each runs DETACHED in a background thread,
# output buffered here — NOT tied to the HTTP request that started it, because a
# switch/restart tears down Traefik (the proxy the browser talks through). The
# browser polls /api/action/status?slot=… and reconnects across the blip.
_actions_guard = threading.Lock()   # guards the dicts below
_actions = {}       # slot -> {"id","name","lines","running","code"}
_slot_locks = {}    # slot -> threading.Lock (one action per slot)
_action_seq = 0


def _slot_lock(slot):
    with _actions_guard:
        lk = _slot_locks.get(slot)
        if lk is None:
            lk = _slot_locks[slot] = threading.Lock()
        return lk


def _run_action_thread(slot, argv, cwd, env):
    code = 1
    try:
        proc = subprocess.Popen(argv, cwd=cwd, env=env or compose_env(),
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                text=True, bufsize=1)
        for line in proc.stdout:
            with _actions_guard:
                _actions[slot]["lines"].append(_strip_ansi(line.rstrip("\n")))
        proc.wait()
        code = proc.returncode
    except OSError as e:
        with _actions_guard:
            _actions[slot]["lines"].append("✗ " + str(e))
    finally:
        with _actions_guard:
            _actions[slot]["running"] = False
            _actions[slot]["code"] = code
        note("action on %s finished with %s" % (slot, code))
        _slot_lock(slot).release()


# ── shell helpers ────────────────────────────────────────────────────────────
def run(args, cwd=None, timeout=25, env=None):
    """Run a command, return (returncode, stdout). Never raises on non-zero."""
    try:
        p = subprocess.run(
            args, cwd=cwd, timeout=timeout, env=env,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        return p.returncode, p.stdout
    except (subprocess.TimeoutExpired, FileNotFoundError, OSError):
        return 1, ""


def compose_env():
    env = dict(os.environ)
    env["COMPOSE_PROFILES"] = PROFILES
    return env


def stack_env(slot):
    env = compose_env()
    env["STACK"] = slot
    return env


# ── state: worktrees · PRs · running stack ─────────────────────────────────────
def primary_root():
    rc, out = run(["git", "-C", ROOT, "worktree", "list", "--porcelain"], timeout=8)
    for line in out.splitlines():
        if line.startswith("worktree "):
            return line[len("worktree "):].strip()
    return os.getcwd()


def git_worktrees(root):
    """[{path, dir, branch, detached}] in git's listing order (primary first)."""
    rc, out = run(["git", "-C", root, "worktree", "list", "--porcelain"], timeout=8)
    trees, cur = [], None
    for line in out.splitlines():
        if line.startswith("worktree "):
            if cur:
                trees.append(cur)
            path = line[len("worktree "):].strip()
            cur = {"path": path, "dir": os.path.basename(path), "branch": None, "detached": False}
        elif line.startswith("branch ") and cur is not None:
            cur["branch"] = line[len("branch "):].strip().replace("refs/heads/", "")
        elif line.strip() == "detached" and cur is not None:
            cur["detached"] = True
    if cur:
        trees.append(cur)
    return trees


# ── git sync vs the default branch (origin/main) ──────────────────────────────
# How far each worktree is behind/ahead of origin's default branch drives the
# "Update from main" status + button. Behind/ahead is read from LOCAL refs (no
# network) so /api/state stays fast; a background `git fetch` (kick_fetch) keeps
# origin/<base> fresh, so the numbers track reality without blocking requests.
_fetch_lock = threading.Lock()
_fetch_at = 0.0


def default_branch(root):
    """The remote's default branch (origin/HEAD → e.g. 'main'), 'main' if unset."""
    rc, out = run(["git", "-C", root, "symbolic-ref", "--quiet", "--short",
                   "refs/remotes/origin/HEAD"], timeout=6)
    b = out.strip()
    if b.startswith("origin/"):
        b = b[len("origin/"):]
    return b or "main"


def behind_ahead(path, base):
    """(behind, ahead) vs origin/<base> from local refs; (None, None) if unknown."""
    rc, out = run(["git", "-C", path, "rev-list", "--left-right", "--count",
                   "origin/%s...HEAD" % base], timeout=8)
    parts = out.split()
    if rc != 0 or len(parts) != 2:
        return None, None
    try:
        return int(parts[0]), int(parts[1])   # left = behind, right = ahead
    except ValueError:
        return None, None


def kick_fetch(root, base):
    """Fire-and-forget `git fetch origin <base>`, at most once per 60s."""
    global _fetch_at
    with _fetch_lock:
        if _fetch_at and time.time() - _fetch_at < 60:
            return
        _fetch_at = time.time()
    threading.Thread(
        target=lambda: run(["git", "-C", root, "fetch", "--quiet", "origin", base], timeout=25),
        daemon=True).start()


# gh is slow-ish and rate-limited; cache the branch→PR map briefly.
_pr_cache = {"at": 0.0, "by_branch": {}, "open": []}
_pr_lock = threading.Lock()


def pr_data():
    with _pr_lock:
        if time.time() - _pr_cache["at"] < 20 and _pr_cache["at"]:
            return _pr_cache["by_branch"], _pr_cache["open"]
    by_branch, open_prs = {}, []
    if shutil.which("gh"):
        rc, out = run([
            "gh", "pr", "list", "--state", "all", "--limit", "400",
            "--json", "number,headRefName,baseRefName,state,title,author,isDraft",
        ], timeout=20)
        if rc == 0 and out.strip():
            try:
                for pr in json.loads(out):
                    br = pr.get("headRefName")
                    row = {
                        "number": pr.get("number"),
                        "branch": br,
                        "base": pr.get("baseRefName") or "",
                        "state": pr.get("state"),
                        "title": pr.get("title") or "",
                        "author": (pr.get("author") or {}).get("login") or "",
                        "draft": bool(pr.get("isDraft")),
                    }
                    if br and br not in by_branch:  # newest wins (gh sorts desc)
                        by_branch[br] = row
                    if row["state"] == "OPEN":
                        open_prs.append(row)
            except (ValueError, TypeError):
                pass
    open_prs.sort(key=lambda r: r["number"] or 0, reverse=True)
    with _pr_lock:
        _pr_cache.update(at=time.time(), by_branch=by_branch, open=open_prs)
    return by_branch, open_prs


def _loads_multi(text):
    """docker compose emits either a JSON array or newline-delimited objects."""
    text = text.strip()
    if not text:
        return []
    try:
        v = json.loads(text)
        return v if isinstance(v, list) else [v]
    except ValueError:
        rows = []
        for line in text.splitlines():
            line = line.strip()
            if line:
                try:
                    rows.append(json.loads(line))
                except ValueError:
                    pass
        return rows


def running_projects():
    """Every running dev stack: [{slot, config_file, dir}] for trip1, trip2, …."""
    rc, out = run(["docker", "compose", "ls", "--all", "--format", "json"], timeout=12)
    stacks = []
    for proj in _loads_multi(out):
        name = proj.get("Name") or ""
        if STACK_RE.match(name) and "running" in (proj.get("Status") or ""):
            cfg = (proj.get("ConfigFiles") or "").split(",")[0].strip()
            if cfg:
                stacks.append({"slot": name, "config_file": cfg, "dir": os.path.dirname(cfg)})
    return sorted(stacks, key=lambda s: s["slot"])


def find_stack(slot):
    return next((s for s in running_projects() if s["slot"] == slot), None)


def next_free_slot(running_slots):
    n = 1
    while slot_name(n) in running_slots:
        n += 1
    return slot_name(n)


def _ports_of(c):
    ports = []
    for pub in c.get("Publishers") or []:
        p = pub.get("PublishedPort")
        if p:
            ports.append(int(p))
    return sorted(set(ports))


def stack_state(config_file, running_dir, slot):
    """Services, health, uptime for one running stack."""
    rc, out = run(
        ["docker", "compose", "-p", slot, "-f", config_file, "ps", "--all", "--format", "json"],
        cwd=running_dir, env=stack_env(slot), timeout=15,
    )
    by_service, earliest = {}, None
    for c in _loads_multi(out):
        svc = c.get("Service") or c.get("Name") or "?"
        state = (c.get("State") or "").lower()
        health = (c.get("Health") or "").lower()
        exit_code = c.get("ExitCode")
        ports = _ports_of(c)
        created = c.get("CreatedAt") or ""
        prev = by_service.get(svc)
        # keep the "most alive" container per service
        if prev is None or (state == "running" and prev["state"] != "running"):
            by_service[svc] = {
                "name": svc, "state": state, "health": health,
                "exit_code": exit_code, "ports": ports,
            }
        if state == "running" and created:
            ts = _parse_created(created)
            if ts and (earliest is None or ts < earliest):
                earliest = ts
    services = sorted(by_service.values(), key=lambda s: s["name"])
    up = sum(1 for s in services if s["state"] == "running")
    return {
        "services": services,
        "up": up,
        "total": len(services),
        "started_epoch": earliest,
    }


def _parse_created(s):
    # "2026-07-15 12:00:53 +0300 EEST" → epoch seconds
    m = re.match(r"(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{4})", s)
    if not m:
        return None
    try:
        return datetime.strptime(m.group(1), "%Y-%m-%d %H:%M:%S %z").timestamp()
    except ValueError:
        return None


# ── resource footprint (docker stats) ─────────────────────────────────────────
# docker stats is slow (~1-2s), so it gets its own briefly-cached endpoint
# rather than being folded into /api/state.
_stats_cache = {}  # slot -> {"at": float, "data": {...}}
_stats_lock = threading.Lock()
_BYTE_UNIT = {"B": 1, "kB": 1000, "KB": 1000, "MB": 1000**2, "GB": 1000**3,
              "TB": 1000**4, "KiB": 1024, "MiB": 1024**2, "GiB": 1024**3, "TiB": 1024**4}


def _parse_bytes(s):
    m = re.match(r"\s*([\d.]+)\s*([KMGT]?i?B)", s or "")
    if not m:
        return 0
    return int(float(m.group(1)) * _BYTE_UNIT.get(m.group(2), 1))


def stats_snapshot(slot):
    slot = slot or "trip1"
    with _stats_lock:
        c = _stats_cache.get(slot)
        if c and time.time() - c["at"] < 4:
            return c["data"]
    rc, out = run(["docker", "ps", "--filter",
                   "label=com.docker.compose.project=%s" % slot, "-q"], timeout=8)
    ids = out.split()
    services, cpu_total, mem_used, mem_limit = [], 0.0, 0, 0
    if ids:
        rc, out = run(["docker", "stats", "--no-stream", "--format", "{{json .}}"] + ids,
                      timeout=25)
        for line in out.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                c = json.loads(line)
            except ValueError:
                continue
            name = c.get("Name") or c.get("Container") or ""
            svc = re.sub(r"[-_]\d+$", "", re.sub(r"^%s[-_]" % re.escape(slot), "", name))
            try:
                cpu = float((c.get("CPUPerc") or "0").rstrip("%"))
            except ValueError:
                cpu = 0.0
            mem = c.get("MemUsage") or ""
            used, lim = 0, 0
            if "/" in mem:
                u, l = mem.split("/", 1)
                used, lim = _parse_bytes(u), _parse_bytes(l)
            services.append({"name": svc, "cpu": round(cpu, 1), "mem": used})
            cpu_total += cpu
            mem_used += used
            mem_limit = max(mem_limit, lim)
    data = {
        "services": sorted(services, key=lambda s: s["mem"], reverse=True),
        "cpu_total": round(cpu_total, 1),
        "cpu_count": os.cpu_count() or 1,
        "mem_used": mem_used,
        "mem_limit": mem_limit,
    }
    with _stats_lock:
        _stats_cache[slot] = {"at": time.time(), "data": data}
    return data


# ── Claude agents (background jobs) ───────────────────────────────────────────
# Each agent working in this repo is a background job under ~/.claude/jobs/<id>/
# with a state.json: name, live tempo (active/idle/blocked), current detail line,
# tokens, children (the PRs/artifacts it produced) — and where it works.
#
# "Where" needs both directory fields, because the two ways an agent ends up in a
# worktree record it differently:
#
#   spawned *in* a worktree (`claude --bg` from there) → cwd = the worktree,
#                                                        worktreePath = null
#   entered one itself (EnterWorktree)                 → worktreePath = the
#                                                        worktree, cwd = wherever
#                                                        it was launched
#
# So a worktree owns an agent when it matches *either*. That's exact; the PR
# number stays as a third, weaker signal for agents that sit in the primary
# checkout while working on a branch. `claude agents --json` reports only cwd,
# which is why this reads state.json directly.
#
# agent.jobs points at that directory; blank (another agent CLI, or none) means
# no agent list, and the UI drops to worktrees + sessions.
JOBS_DIR = os.path.expanduser(AGENT["jobs"]) if AGENT["jobs"] else ""
_PR_HREF = re.compile(r"/pull/(\d+)")
_PR_TEXT = re.compile(r"\bPR #?(\d+)")
_agents_cache = {"at": 0.0, "data": None}
_agents_lock = threading.Lock()


def longest_owner(d, paths):
    """The worktree that owns a directory: the longest path containing it.

    .claude/worktrees/* live *inside* the primary checkout, so "is this dir under
    that worktree" is true of the primary for every worktree — which is how the
    primary ended up permanently marked as running.
    """
    best = ""
    for w in paths:
        if d and (d == w or d.startswith(w + os.sep)) and len(w) > len(best):
            best = w
    return best


def agent_home(worktree, cwd, paths):
    """The worktree that owns a directory pair, or "".

    Longest match wins and the directory may be *under* the worktree, so an agent
    started in <worktree>/trip1-frontend belongs to that worktree instead of the
    primary checkout — whose path is a prefix of every worktree, since
    .claude/worktrees lives inside it. worktreePath is tried first: an agent that
    ran EnterWorktree keeps cwd pointing at wherever it was launched.
    """
    for p in (worktree, cwd):
        best = longest_owner(p, paths)
        if best:
            return best
    return ""


def agents_snapshot():
    with _agents_lock:
        c = _agents_cache
        if c["data"] is not None and time.time() - c["at"] < 3:
            return c["data"]
    paths = [w["path"] for w in git_worktrees(primary_root())]
    agents = []
    try:
        ids = os.listdir(JOBS_DIR) if JOBS_DIR else []
    except OSError:
        ids = []
    for jid in ids:
        try:
            with open(os.path.join(JOBS_DIR, jid, "state.json")) as f:
                s = json.load(f)
        except (OSError, ValueError):
            continue
        prs, artifacts = [], []
        for ch in s.get("children") or []:
            href = ch.get("href") or ""
            if ch.get("kind") == "pr" or "/pull/" in href:
                m = _PR_HREF.search(href)
                if m and int(m.group(1)) not in prs:
                    prs.append(int(m.group(1)))
            elif href:
                artifacts.append({"title": ch.get("title") or ch.get("id") or "artifact", "href": href})
        detail = (s.get("detail") or "").strip()
        for m in _PR_TEXT.finditer(detail):
            if int(m.group(1)) not in prs:
                prs.append(int(m.group(1)))
        agents.append({
            "id": s.get("daemonShort") or jid[:8],
            "name": s.get("name") or "(agent)",
            "tempo": s.get("tempo") or "idle",
            "state": s.get("state") or "",
            "detail": detail,
            "tokens": s.get("tokens") or 0,
            "prs": prs,
            "artifacts": artifacts[:4],
            "updated": s.get("updatedAt") or "",
            "session": s.get("sessionId") or "",
            "worktree": s.get("worktreePath") or "",     # set when it entered one
            "branch": s.get("worktreeBranch") or "",
            "cwd": s.get("cwd") or "",                   # set when spawned in one
            # Resolved here, not in the browser, so the drawer and the terminal
            # can't disagree about who owns an agent.
            "home": agent_home(s.get("worktreePath") or "", s.get("cwd") or "", paths),
        })
    rank = {"blocked": 0, "active": 1, "idle": 2}
    agents.sort(key=lambda a: a["updated"], reverse=True)
    agents.sort(key=lambda a: rank.get(a["tempo"], 3))
    data = {"agents": agents}
    with _agents_lock:
        _agents_cache.update(at=time.time(), data=data)
    return data


def build_state():
    root = primary_root()
    base = default_branch(root)
    kick_fetch(root, base)          # refresh origin/<base> in the background
    projs = running_projects()
    by_dir = {p["dir"]: p for p in projs}
    by_branch, open_prs = pr_data()

    worktrees = []
    for wt in git_worktrees(root):
        pr = by_branch.get(wt["branch"]) if wt["branch"] else None
        rp = by_dir.get(wt["path"])
        behind, ahead = behind_ahead(wt["path"], base)
        worktrees.append({
            "path": wt["path"],
            "dir": wt["dir"],
            "branch": wt["branch"] or "(detached)",
            "detached": wt["detached"],
            "is_default": wt["path"] == root,
            "is_running": rp is not None,
            "running_slot": rp["slot"] if rp else None,
            "behind": behind,
            "ahead": ahead,
            "pr": pr and {"number": pr["number"], "state": pr["state"], "base": pr.get("base") or "",
                          "title": pr["title"], "draft": pr["draft"]},
        })
    # running first, then default, then git order
    worktrees.sort(key=lambda w: (0 if w["is_running"] else 1 if w["is_default"] else 2))

    stacks = []
    for p in projs:
        st = stack_state(p["config_file"], p["dir"], p["slot"])
        wt = next((w for w in worktrees if w["path"] == p["dir"]), None)
        stacks.append({
            "slot": p["slot"],
            "dir": os.path.basename(p["dir"]),
            "path": p["dir"],
            "branch": wt["branch"] if wt else "?",
            "behind": wt["behind"] if wt else None,
            "ahead": wt["ahead"] if wt else None,
            "pr": wt["pr"] if wt else None,
            "url": "http://%s.localhost" % p["slot"],
            "profiles": PROFILES.split(","),
            **st,
        })

    running_slots = {p["slot"] for p in projs}
    return {
        "root": root,
        "base": base,
        "stacks": stacks,
        "worktrees": worktrees,
        "open_prs": [p for p in open_prs
                     if not any(w["branch"] == p["branch"] for w in worktrees)][:12],
        "next_slot": next_free_slot(running_slots),
        "profiles": PROFILES,
    }


# ── work: the thing you actually juggle ───────────────────────────────────────
# A piece of work is a worktree + branch (+ PR), the agents living in it, the
# terminal sessions open on it, and — only if this change needs the app running
# — a stack. Docker is an attachment to a task, not the frame around it: most
# backend edits never need it, and starting one costs a minute you don't spend
# unless the change earns it.
#
# Sorted by what wants you: needs-input first, then working, then whatever has a
# stack up, then the rest in git order. With several agents in flight that
# ordering *is* the dashboard.
WORK_ATTENTION = ("blocked", "active")


def _iso_epoch(s):
    try:
        return datetime.strptime((s or "")[:19], "%Y-%m-%dT%H:%M:%S").replace(
            tzinfo=timezone.utc).timestamp()
    except ValueError:
        return 0


def last_activity(path, agents):
    """When this work was last touched: its liveliest agent, or the worktree
    itself. Stat-cheap on purpose — this runs for every worktree every poll."""
    stamps = [_iso_epoch(a["updated"]) for a in agents]
    for candidate in (os.path.join(path, ".git"), path):
        try:
            stamps.append(os.path.getmtime(candidate))
        except OSError:
            pass
    return max(stamps) if stamps else 0


_live_cache = {"at": 0.0, "dirs": set()}
_live_lock = threading.Lock()


def live_agent_dirs():
    """Directories with a live agent process (agent.process).

    state.json's tempo is a turn-boundary snapshot — the session you are talking
    to right now reads "idle", because it only writes when a turn ends. A running
    process is the one thing that is true *now*, so that's what drives "running".
    """
    with _live_lock:
        if time.time() - _live_cache["at"] < 4:
            return _live_cache["dirs"]
    dirs = set()
    if not AGENT["process"]:
        return dirs
    rc, out = run(["pgrep", "-f", AGENT["process"]], timeout=6)
    pids = [w for w in out.split() if w.isdigit()][:80]
    if pids:
        rc, out = run(["lsof", "-a", "-d", "cwd", "-Fn", "-p", ",".join(pids)], timeout=10)
        dirs = {ln[1:] for ln in out.splitlines() if ln.startswith("n/")}
    with _live_lock:
        _live_cache.update(at=time.time(), dirs=dirs)
    return dirs


_repo_cache = {"at": 0.0, "url": None}


def repo_web_url():
    """https URL for the origin remote, for linking PRs. "" when there isn't one."""
    if _repo_cache["url"] is not None and time.time() - _repo_cache["at"] < 300:
        return _repo_cache["url"]
    rc, out = run(["git", "-C", primary_root(), "remote", "get-url", "origin"], timeout=8)
    url = out.strip()
    if url.startswith("git@"):                       # git@host:owner/repo.git
        url = "https://" + url[4:].replace(":", "/", 1)
    if url.startswith("ssh://git@"):
        url = "https://" + url[len("ssh://git@"):]
    url = re.sub(r"\.git$", "", url)
    if not url.startswith("https://"):
        url = ""
    _repo_cache.update(at=time.time(), url=url)
    return url


def build_work():
    state = build_state()
    ags = agents_snapshot()["agents"]
    sessions = term_list()
    homes = {}
    for a in ags:
        if a["home"]:
            homes.setdefault(a["home"], []).append(a)

    by_branch, _open = pr_data()
    known_prs = {p["number"] for p in by_branch.values()}
    wt_paths = [w["path"] for w in state["worktrees"]]
    live_owners = {longest_owner(d, wt_paths) for d in live_agent_dirs()}
    live_owners.discard("")

    items = []
    for w in state["worktrees"]:
        st = next((s for s in state["stacks"] if s["path"] == w["path"]), None)
        mine = homes.get(w["path"], [])
        mine.sort(key=lambda a: a["updated"] or "", reverse=True)
        mine.sort(key=lambda a: TEMPO_RANK.get(a["tempo"], 3))
        tempo = mine[0]["tempo"] if mine else ""
        # A claude process *owned by* this worktree means work is happening here,
        # whatever the last state.json write claimed.
        live = w["path"] in live_owners
        # Every PR this work has produced, not just the one whose head branch
        # matches the worktree: an agent commonly opens several (a fix, its
        # follow-up, the revert) and showing one made the rest invisible.
        prs = [w["pr"]["number"]] if w["pr"] else []
        for a in mine:
            for n in a["prs"]:
                # Agent notes mention other repos' PRs too ("PR #1" from a
                # homelab job). Only keep numbers that are PRs *here*, since
                # that's where the links point.
                if n not in prs and (not known_prs or n in known_prs):
                    prs.append(n)
        # Behind *what*: a PR's own base branch, not an assumption about main. A
        # branch cut from another branch was being measured against the wrong
        # thing, which made the merge button lie.
        base = (w["pr"] or {}).get("base") or state["base"]
        behind, ahead = w["behind"], w["ahead"]
        if base != state["base"]:
            behind, ahead = behind_ahead(w["path"], base)
        items.append({
            "path": w["path"], "dir": w["dir"], "branch": w["branch"],
            "is_default": w["is_default"], "pr": w["pr"], "prs": prs,
            "base": base, "activity": last_activity(w["path"], mine), "live": live,
            "behind": behind, "ahead": ahead,
            "stack": st,                                   # None unless one is up
            "agents": mine,
            "sessions": [s for s in sessions if s["cwd"] == w["path"] and s["alive"]],
            "tempo": tempo,
            "rank": (6 if w["is_default"] else
                     0 if tempo == "blocked" else 1 if (live or tempo == "active")
                     else 2 if st else 3 if mine else 5),
        })
    # Newest first inside each group: with 35 worktrees, "what did I touch" is
    # how you find things again.
    items.sort(key=lambda i: i["activity"], reverse=True)
    items.sort(key=lambda i: i["rank"])
    return {
        "root": state["root"], "base": state["base"], "work": items,
        "name": PROJECT, "stack_enabled": bool(STACK), "repo_url": repo_web_url(),
        "open_prs": state["open_prs"], "next_slot": state["next_slot"],
        "profiles": PROFILES, "terminal": TERMINAL,
        # Capabilities, so the UI offers what this project actually has rather
        # than buttons that answer "not configured".
        "agent": {"name": AGENT["name"] or "agent", "run": bool(AGENT["command"]),
                  "spawn": bool(AGENT["spawn"]), "attach": bool(AGENT["attach"]),
                  "jobs": bool(JOBS_DIR), "mcp": bool(AGENT["mcp"])},
    }


WORK_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9._/-]{0,60}$")


def slugify(s):
    s = re.sub(r"[^a-z0-9]+", "-", (s or "").lower()).strip("-")
    return s[:48] or ""


def unique_slug(root, slug):
    """First free name in the slug, slug-2, slug-3 … family."""
    for n in range(1, 40):
        candidate = slug if n == 1 else "%s-%d" % (slug, n)
        taken = os.path.exists(os.path.join(root, WORKTREES, candidate))
        if not taken:
            rc, _ = run(["git", "-C", root, "rev-parse", "--verify", "--quiet",
                         "refs/heads/" + candidate], timeout=8)
            taken = rc == 0
        if not taken:
            return candidate
    return ""


STOPWORDS = {"the", "a", "an", "and", "or", "to", "of", "in", "on", "for", "with",
             "please", "make", "fix", "add", "when", "that", "this", "is", "are", "it"}


def name_from_task(prompt):
    """A branch name out of the task text: nobody should have to invent one."""
    words = [w for w in re.findall(r"[a-z0-9]+", (prompt or "").lower())
             if w not in STOPWORDS and len(w) > 1]
    return "-".join(words[:4])[:40]


def new_work(name, prompt, base=""):
    """Create a worktree + branch and (optionally) an agent in it. (item, error)

    Deliberately never touches compose: this is the "small change, no app
    needed" path, and waiting a minute for six containers you won't look at is
    the thing that makes people not bother branching.
    """
    root = primary_root()
    # A name is optional: derive one from the task, and only fall back to asking
    # if there's nothing to derive from.
    slug = slugify(name) or slugify(name_from_task(prompt)) or ""
    if not slug or not WORK_NAME_RE.match(slug):
        return None, "describe the task (or give it a short name)"
    slug = unique_slug(root, slug)
    if not slug:
        return None, "couldn't find a free branch name — try a different wording"
    base = base or default_branch(root)
    path = os.path.join(root, WORKTREES, slug)
    # Branch from origin/<base> so new work starts level with the remote rather
    # than on top of whatever the primary checkout happens to have.
    run(["git", "-C", root, "fetch", "--quiet", "origin", base], timeout=40)
    start = "origin/%s" % base
    rc, out = run(["git", "-C", root, "rev-parse", "--verify", "--quiet", start], timeout=8)
    if rc != 0:
        start = base
    rc, out = run(["git", "-C", root, "worktree", "add", "-b", slug, path, start], timeout=90)
    if rc != 0:
        return None, (_strip_ansi(out).strip().splitlines() or ["git worktree add failed"])[-1][:200]
    copied = copy_local_files(root, path)
    if copied:
        note("copied %d local file(s) into %s" % (copied, slug))
    # Return as soon as the worktree exists and start the agent behind it. Waiting
    # on the agent spawn held the request for as long as it took to boot — and if it
    # ever hangs (a trust prompt in a fresh directory, say) the button would just
    # sit there. The agent shows up in /api/work on the next poll either way.
    if (prompt or "").strip():
        threading.Thread(target=lambda: (spawn_agent(path, prompt),
                                         _agents_cache.update(at=0.0)), daemon=True).start()
    _agents_cache["at"] = 0.0
    note("new work %s created off %s" % (slug, start))
    return {"path": path, "dir": slug, "branch": slug,
            "agent_starting": bool((prompt or "").strip())}, None


def copy_local_files(root, dest):
    """Bring worktrees.copy over from the primary checkout.

    A worktree is a clean checkout, so everything gitignored is missing: .env,
    service-account keys, credential masters. The app then fails minutes later
    with something that looks nothing like "you forgot a file", so this copies
    them at creation rather than leaving it to memory.

    Patterns read like .gitignore: no slash matches that basename at any depth,
    a slash anchors it to the repo root. Noisy directories are pruned rather
    than filtered — walking 40 worktrees' node_modules to find .env files took
    five seconds, and the worktree root is one of the pruned ones.
    """
    prune = {".git", ".hg", "node_modules", "vendor", "dist", "build", "target",
             "__pycache__", ".venv", ".next", ".turbo", ".cache", ".terraform"}
    wt_root = os.path.abspath(os.path.join(root, WORKTREES))
    pats = [pat for pat in WT_COPY if not (os.path.isabs(pat) or ".." in pat.split("/"))]
    for pat in set(WT_COPY) - set(pats):
        note("worktrees.copy ignores %s — must be a path inside the repo" % pat)
    if not pats:
        return 0
    anchored = [pat for pat in pats if "/" in pat]
    loose = [pat for pat in pats if "/" not in pat]
    n = 0
    for cur, dirs, files in os.walk(root):
        dirs[:] = [d for d in dirs
                   if d not in prune and os.path.join(cur, d) != wt_root]
        for f in files:
            rel = os.path.relpath(os.path.join(cur, f), root)
            if not (any(fnmatch.fnmatch(f, pat) for pat in loose)
                    or any(fnmatch.fnmatch(rel, pat) for pat in anchored)):
                continue
            tgt = os.path.join(dest, rel)
            if os.path.exists(tgt):
                continue
            try:
                os.makedirs(os.path.dirname(tgt), exist_ok=True)
                shutil.copy2(os.path.join(cur, f), tgt)
                n += 1
            except OSError as e:
                note("could not copy %s: %s" % (rel, e))
    return n


# ── MCP servers ───────────────────────────────────────────────────────────────
# agent.mcp ("claude mcp") health-checks every server, which is the part worth
# surfacing: a server can be registered and still be invisible to an agent
# because its command isn't on PATH, or because it needs an OAuth round. Both
# fail silently unless you go looking, so show the reason next to the name.
MCP_LINE = re.compile(r"^(?P<name>.+?): (?P<target>.*?)\s+-\s+(?P<status>[^\s].*)$")
_mcp_cache = {"at": 0.0, "data": None}
_mcp_lock = threading.Lock()


def mcp_scopes():
    """Which config each server came from — that's what decides who sees it."""
    scopes = {}
    root = primary_root()
    try:
        with open(os.path.expanduser("~/.claude.json")) as f:
            cfg = json.load(f)
    except (OSError, ValueError):
        cfg = {}
    for name in (cfg.get("mcpServers") or {}):
        scopes[name] = "user"
    for path, entry in (cfg.get("projects") or {}).items():
        for name in ((entry or {}).get("mcpServers") or {}):
            scopes.setdefault(name, "local:" + os.path.basename(path))
    try:
        with open(os.path.join(root, ".mcp.json")) as f:
            for name in (json.load(f).get("mcpServers") or {}):
                scopes[name] = "project"
    except (OSError, ValueError):
        pass
    return scopes



ENOENT_CMD = re.compile(r'Executable not found in \$PATH: "([^"]+)"')


def mcp_configs():
    """Stored config per server, so a broken one can be re-added as it was."""
    out = {}
    root = primary_root()
    try:
        with open(os.path.expanduser("~/.claude.json")) as f:
            cfg = json.load(f)
    except (OSError, ValueError):
        cfg = {}
    for name, entry in (cfg.get("mcpServers") or {}).items():
        out[name] = dict(entry or {}, _scope="user")
    for path, proj in (cfg.get("projects") or {}).items():
        for name, entry in ((proj or {}).get("mcpServers") or {}).items():
            out.setdefault(name, dict(entry or {}, _scope="local"))
    try:
        with open(os.path.join(root, ".mcp.json")) as f:
            for name, entry in (json.load(f).get("mcpServers") or {}).items():
                out[name] = dict(entry or {}, _scope="project")
    except (OSError, ValueError):
        pass
    return out


def find_executable(cmd):
    """Where a command actually lives, when the PATH agents use can't see it.

    A stdio server whose command isn't on PATH is the most common way an MCP
    config is correct and still dead, and the fix is always the same: point at
    the absolute path. So find it rather than making someone go looking.
    """
    if not cmd or "/" in cmd:
        return ""
    for d in BIN_DIRS:
        candidate = os.path.join(os.path.expanduser(d), cmd)
        if os.path.isfile(candidate) and os.access(candidate, os.X_OK):
            return candidate
    return ""


def mcp_list(force=False):
    if not AGENT["mcp"]:
        return {"servers": []}
    with _mcp_lock:
        c = _mcp_cache
        if not force and c["data"] is not None and time.time() - c["at"] < 20:
            return c["data"]
    # Health checks are slow (a network round per HTTP server), hence the cache.
    # Through a login shell: an agent session runs `$SHELL -lc`, so its PATH is
    # what decides reachability. Checking with this server's own PATH reported a
    # different answer than the thing that actually connects.
    env = dict(os.environ, PATH=user_path())
    rc, out = run([login_shell(), "-lc", AGENT["mcp"] + " list"], cwd=primary_root(),
                  timeout=120, env=env)
    scopes = mcp_scopes()
    configs = mcp_configs()
    servers = []
    for line in _strip_ansi(out).splitlines():
        m = MCP_LINE.match(line.strip())
        if not m:
            continue
        status = m.group("status")
        # Classify on the words, not the glyph: "connected" ships as ✔ (U+2714)
        # in some builds and ✓ (U+2713) in others, and matching the glyph put
        # seventeen healthy servers in the failed column.
        low = status.lower()
        state = ("auth" if "authentication" in low or "authenticate" in low
                 else "pending" if "pending" in low
                 else "ok" if "connected" in low and "fail" not in low
                 else "fail")
        servers.append({
            "name": m.group("name").strip(),
            "target": m.group("target").strip(),
            "state": state,
            "detail": status.lstrip("✓✔✘✗!⏸ ").strip(),
            "scope": scopes.get(m.group("name").strip(), ""),
        })
        name = servers[-1]["name"]
        cfg = configs.get(name) or {}
        servers[-1]["command"] = cfg.get("command") or ""
        miss = ENOENT_CMD.search(status)
        servers[-1]["suggest"] = find_executable(miss.group(1)) if miss else ""
    servers.sort(key=lambda x: ({"auth": 0, "fail": 1, "pending": 2, "ok": 3}[x["state"]], x["name"].lower()))
    data = {"servers": servers}
    with _mcp_lock:
        _mcp_cache.update(at=time.time(), data=data)
    bad = [x["name"] for x in servers if x["state"] == "fail"]
    note("mcp: %d servers — %d reachable, %d need auth%s"
         % (len(servers), sum(1 for x in servers if x["state"] == "ok"),
            sum(1 for x in servers if x["state"] == "auth"),
            (", failing: " + ", ".join(bad[:4])) if bad else ""))
    return data


MCP_NAME_RE = re.compile(r"^[A-Za-z0-9][\w.-]{0,60}$")


def mcp_add(name, target, transport, scope, env, headers):
    """agent.mcp add, so the agent CLI stays the one source of truth for config."""
    if not AGENT["mcp"]:
        return "no mcp command configured"
    if not MCP_NAME_RE.match(name or ""):
        return "name must be letters, digits, dots or dashes"
    if not (target or "").strip():
        return "give it a URL or a command"
    if scope not in ("user", "project", "local"):
        return "bad scope"
    argv = shlex.split(AGENT["mcp"]) + ["add", "--scope", scope]
    if transport in ("http", "sse"):
        argv += ["--transport", transport]
    for h in headers or []:
        if ":" in h:
            argv += ["--header", h]
    for e in env or []:
        if "=" in e:
            argv += ["-e", e]
    argv += [name]
    parts = shlex.split(target) if not target.strip().startswith(("http://", "https://")) else [target.strip()]
    if transport in ("http", "sse"):
        argv += parts[:1]
    else:
        argv += ["--"] + parts
    try:
        p = subprocess.run(argv, cwd=primary_root(), timeout=60, text=True,
                           env=dict(os.environ, PATH=user_path()),
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    except (subprocess.TimeoutExpired, OSError) as e:
        return str(e)
    if p.returncode != 0:
        return (_strip_ansi(p.stdout or "").strip().splitlines() or ["mcp add failed"])[-1][:200]
    mcp_list(force=True)
    return ""


def mcp_remove(name, scope):
    if not AGENT["mcp"]:
        return "no mcp command configured"
    if not MCP_NAME_RE.match(name or ""):
        return "bad name"
    argv = shlex.split(AGENT["mcp"]) + ["remove", name]
    if scope in ("user", "project", "local"):
        argv += ["--scope", scope]
    rc, out = run(argv, cwd=primary_root(), timeout=60)
    if rc != 0:
        return (_strip_ansi(out).strip().splitlines() or ["mcp remove failed"])[-1][:200]
    mcp_list(force=True)
    return ""


# ── changes: what this work has actually done ─────────────────────────────────
# Agents commit as they go, so "what changed here" is the question you ask a
# worktree most often. A TUI in a PTY answers it badly at 400px; this is the
# same information as structured JSON, so the browser can lay it out.
DIFF_MAX = 400_000          # a diff nobody reads past; keeps a huge file sane


def _numstat(path, cached):
    counts = {}
    args = ["git", "-C", path, "diff", "--numstat"] + (["--cached"] if cached else [])
    rc, out = run(args, timeout=20)
    for line in out.splitlines():
        parts = line.split("\t")
        if len(parts) == 3:
            add, dele, f = parts
            c = counts.setdefault(f, [0, 0])
            c[0] += 0 if add == "-" else int(add or 0)
            c[1] += 0 if dele == "-" else int(dele or 0)
    return counts


def work_changes(path, base=""):
    """Working-tree status + the commits this branch has that its base doesn't."""
    rc, out = run(["git", "-C", path, "status", "--porcelain"], timeout=20)
    files = []
    for line in out.splitlines():
        if len(line) < 4:
            continue
        xy, rest = line[:2], line[3:]
        if " -> " in rest:                       # renames: show the destination
            rest = rest.split(" -> ", 1)[1]
        files.append({
            "path": rest.strip('"'), "x": xy[0], "y": xy[1],
            "staged": xy[0] not in " ?", "untracked": xy == "??",
            "add": 0, "del": 0,
        })
    counts = _numstat(path, False)
    for f, c in _numstat(path, True).items():
        cur = counts.setdefault(f, [0, 0])
        cur[0] += c[0]; cur[1] += c[1]
    for f in files:
        f["add"], f["del"] = counts.get(f["path"], [0, 0])
    files.sort(key=lambda f: (not f["staged"], f["path"]))

    # What this work has done = commits its *base* doesn't have. Listing
    # @{upstream}..HEAD showed nothing the moment you pushed, which is exactly
    # when a branch has the most to show.
    rng = "origin/%s..HEAD" % base if base else ""
    out = ""
    for spec in ([rng] if rng else []) + ["@{upstream}..HEAD", "-30"]:
        rc, out = run(["git", "-C", path, "log", "--oneline", "--no-decorate", "-40"]
                      + ([spec] if spec != "-30" else []), timeout=15)
        if rc == 0 and out.strip():
            break
    # Which of them are still local, so "pushed" isn't a guess.
    rc, unpushed = run(["git", "-C", path, "log", "--format=%h", "-40", "@{upstream}..HEAD"], timeout=12)
    tracked = rc == 0                     # no upstream → "pushed" is unknowable
    local = set(unpushed.split()) if tracked else set()
    commits = []
    for line in out.splitlines():
        sha, _, msg = line.partition(" ")
        commits.append({"sha": sha, "msg": msg, "pushed": tracked and sha not in local})
    rc, branch = run(["git", "-C", path, "branch", "--show-current"], timeout=8)
    return {"branch": branch.strip(), "base": base, "files": files, "commits": commits}


def file_diff(path, rel, staged):
    """Unified diff for one file: the side asked for, then the other side, and
    only for an *untracked* file the whole contents. Falling back to
    --no-index unconditionally reported every unmodified file as brand new."""
    base = ["git", "-C", path, "diff", "--no-color"]
    for extra in ([["--cached"], []] if staged else [[], ["--cached"]]):
        rc, out = run(base + extra + ["--", rel], timeout=25)
        if out.strip():
            return out[:DIFF_MAX]
    rc, _ = run(["git", "-C", path, "ls-files", "--error-unmatch", "--", rel], timeout=10)
    if rc == 0:
        return ""                    # tracked and unchanged
    rc, out = run(["git", "-C", path, "diff", "--no-color", "--no-index",
                   "--", os.devnull, rel], cwd=path, timeout=25)
    return out[:DIFF_MAX]


# ── log line parsing (docker compose logs -t) ──────────────────────────────────
_LINE = re.compile(r"^(?P<src>\S+?)\s+\|\s+(?P<rest>.*)$")
_TS = re.compile(r"^(?P<ts>\d{4}-\d{2}-\d{2}T[\d:.]+Z?)\s+(?P<msg>.*)$")
_BAD = re.compile(r"(error|fatal|panic|exited \(|traceback|✗|\bFAIL\b)", re.I)
_WARN = re.compile(r"(warn|deprecat|⚠|retry)", re.I)


def parse_log(line):
    src, msg = "", line
    m = _LINE.match(line)
    if m:
        src, msg = m.group("src"), m.group("rest")
    ts = ""
    mt = _TS.match(msg)
    if mt:
        raw = mt.group("ts")
        msg = mt.group("msg")
        ts = raw[11:19] if len(raw) >= 19 else raw  # HH:MM:SS
    svc = re.sub(r"^%s[-_]" % re.escape(PROJECT), "", src)
    svc = re.sub(r"[-_]\d+$", "", svc)
    level = "bad" if _BAD.search(msg) else "warn" if _WARN.search(msg) else "info"
    return {"t": ts, "svc": svc or "system", "msg": msg, "level": level}


# ── HTTP handler ───────────────────────────────────────────────────────────────
class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass  # quiet

    # -- helpers --
    def _send(self, code, body, ctype="application/json"):
        data = body.encode() if isinstance(body, str) else body
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        try:
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _sse_open(self):
        # Streamed body is delimited by connection close (no Content-Length),
        # so the client's reader ends cleanly when we're done rather than
        # blocking on a reused keep-alive socket.
        self.close_connection = True
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.send_header("X-Accel-Buffering", "no")
        self.end_headers()

    def _write(self, s):
        self.wfile.write(s.encode())
        self.wfile.flush()

    # -- routing --
    def do_GET(self):
        u = urlparse(self.path)
        if not token_ok(self):
            if u.path == "/":
                return self._send(401, LOGIN_PAGE, "text/html; charset=utf-8")
            return self._send(401, json.dumps({"error": "token required"}))
        # A token in the URL is swapped for a cookie once, so it stops riding
        # along in history, screenshots and Referer headers.
        if TOKEN and (parse_qs(u.query).get("t") or [""])[0]:
            return self.set_token_cookie(u.path)
        if u.path == "/":
            self._send(200, PAGE, "text/html; charset=utf-8")
        elif u.path == "/api/state":
            self._send(200, json.dumps(build_state()))
        elif u.path == "/api/logs":
            self.stream_logs(parse_qs(u.query))
        elif u.path == "/api/action/status":
            self.action_status(parse_qs(u.query))
        elif u.path == "/api/stats":
            slot = (parse_qs(u.query).get("stack") or [""])[0]
            self._send(200, json.dumps(stats_snapshot(slot)))
        elif u.path == "/api/agents":
            self._send(200, json.dumps(agents_snapshot()))
        elif u.path == "/api/work":
            self._send(200, json.dumps(build_work()))
        elif u.path == "/api/serverlog":
            self._send(200, json.dumps(server_log((parse_qs(u.query).get("only") or [""])[0])))
        elif u.path == "/api/mcp":
            self._send(200, json.dumps(mcp_list((parse_qs(u.query).get("refresh") or [""])[0] == "1")))
        elif u.path == "/api/changes":
            self.changes(parse_qs(u.query))
        elif u.path == "/api/diff":
            self.diff(parse_qs(u.query))
        elif u.path.startswith("/vendor/"):
            self.send_asset(u.path[len("/vendor/"):])
        elif u.path == "/api/term/list":
            self._send(200, json.dumps({"enabled": TERMINAL, "sessions": term_list()}))
        elif u.path == "/api/term/attach":
            self.term_attach(parse_qs(u.query))
        else:
            self._send(404, json.dumps({"error": "not found"}))

    def do_POST(self):
        u = urlparse(self.path)
        if not token_ok(self):
            return self._send(401, json.dumps({"error": "token required"}))
        if u.path not in ("/api/action", "/api/term/new", "/api/term/kill",
                          "/api/agents/new", "/api/work/new",
                          "/api/mcp/add", "/api/mcp/remove"):
            return self._send(404, json.dumps({"error": "not found"}))
        length = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(length) or "{}")
        except ValueError:
            return self._send(400, json.dumps({"error": "bad json"}))
        if u.path == "/api/action":
            return self.run_action(body)
        if not origin_ok(self.headers):
            note("refused a cross-origin POST to %s from %s"
                 % (u.path, self.headers.get("Origin", "?")))
            return self._send(403, json.dumps({"error": "cross-origin request refused"}))
        if u.path == "/api/agents/new":
            return self.agent_new(body)
        if u.path in ("/api/mcp/add", "/api/mcp/remove"):
            if u.path.endswith("add"):
                err = mcp_add(str(body.get("name") or ""), str(body.get("target") or ""),
                              str(body.get("transport") or ""), str(body.get("scope") or "user"),
                              body.get("env") or [], body.get("headers") or [])
            else:
                err = mcp_remove(str(body.get("name") or ""), str(body.get("scope") or ""))
            return self._send(400 if err else 200, json.dumps({"error": err} if err else {"ok": True}))
        if u.path == "/api/work/new":
            item, err = new_work(str(body.get("name") or ""), str(body.get("prompt") or ""),
                                 str(body.get("base") or ""))
            return self._send(400 if err else 200, json.dumps({"error": err} if err else item))
        if not TERMINAL:
            return self._send(403, json.dumps({"error": "terminal disabled (WORKMUX_TERMINAL=0)"}))
        if u.path == "/api/term/new":
            return self.term_new(body)
        self.term_kill(body)

    def set_token_cookie(self, path):
        self.send_response(302)
        self.send_header("Location", path or "/")
        # HttpOnly: the page never needs to read it, and this keeps it away from
        # anything that manages to run script in here.
        self.send_header("Set-Cookie",
                         "dev_token=%s; Path=/; HttpOnly; SameSite=Lax; Max-Age=31536000" % TOKEN)
        self.send_header("Content-Length", "0")
        self.end_headers()

    # -- changes --
    def _worktree_arg(self, qs):
        path = (qs.get("path") or [""])[0]
        known = {w["path"] for w in git_worktrees(primary_root())}
        return path if path in known else ""

    def changes(self, qs):
        path = self._worktree_arg(qs)
        if not path:
            return self._send(400, json.dumps({"error": "unknown worktree"}))
        base = (qs.get("base") or [""])[0]
        if not re.match(r"^[\w./-]{0,80}$", base):
            base = ""
        self._send(200, json.dumps(work_changes(path, base or default_branch(primary_root()))))

    def diff(self, qs):
        path = self._worktree_arg(qs)
        if not path:
            return self._send(400, json.dumps({"error": "unknown worktree"}))
        sha = (qs.get("commit") or [""])[0]
        if sha:
            if not re.match(r"^[0-9a-f]{4,40}$", sha):
                return self._send(400, json.dumps({"error": "bad revision"}))
            rc, out = run(["git", "-C", path, "show", "--no-color", "--stat", "--patch",
                           "--format=%H%n%an · %ar%n%n%B", sha], timeout=30)
            return self._send(200, json.dumps({"file": sha, "diff": out[:DIFF_MAX]}))
        rel = (qs.get("file") or [""])[0]
        if not rel or rel.startswith("-") or ".." in rel:
            return self._send(400, json.dumps({"error": "bad request"}))
        staged = (qs.get("staged") or ["0"])[0] == "1"
        self._send(200, json.dumps({"file": rel, "diff": file_diff(path, rel, staged)}))

    # -- vendored assets (xterm.js) --
    ASSET_TYPES = {".js": "application/javascript; charset=utf-8", ".css": "text/css; charset=utf-8"}

    def send_asset(self, name):
        ext = os.path.splitext(name)[1]
        # Flat directory, known extensions: no path to traverse out of.
        if "/" in name or ".." in name or ext not in self.ASSET_TYPES:
            return self._send(404, json.dumps({"error": "not found"}))
        try:
            with open(os.path.join(ASSETS, name), "rb") as f:
                data = f.read()
        except OSError:
            return self._send(404, json.dumps({"error": "missing asset " + name}))
        self.send_response(200)
        self.send_header("Content-Type", self.ASSET_TYPES[ext])
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "max-age=86400")   # version-pinned in-repo
        self.end_headers()
        try:
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass

    # -- terminals --
    def term_new(self, body):
        slot = body.get("stack") or ""
        if slot and not STACK_RE.match(slot):
            return self._send(400, json.dumps({"error": "bad stack"}))
        state = build_state()
        st = next((s for s in state["stacks"] if s["slot"] == slot), None)
        # A terminal doesn't need a running stack — an idle worktree is a fine
        # place to run tests or `claude`. Fall back to the primary checkout.
        cwd = st["path"] if st else state["root"]
        if body.get("cwd"):
            cwd = body["cwd"]
            if cwd not in {w["path"] for w in state["worktrees"]}:
                return self._send(400, json.dumps({"error": "unknown worktree"}))
        cols, rows = _clamp_size(body.get("cols"), body.get("rows"))
        s, err = term_new(str(body.get("kind") or "shell"), cwd, slot, cols, rows,
                          str(body.get("agent") or ""))
        if err:
            return self._send(400, json.dumps({"error": err}))
        self._send(200, json.dumps(s.info()))

    def agent_new(self, body):
        """Spawn a background agent in a worktree — one agent per worktree, on
        purpose: that's the unit the rest of this UI is organised around."""
        target = str(body.get("target") or "")
        state = build_state()
        if target not in {w["path"] for w in state["worktrees"]}:
            return self._send(400, json.dumps({"error": "unknown worktree"}))
        sid, err = spawn_agent(target, str(body.get("prompt") or ""))
        if err:
            return self._send(400, json.dumps({"error": err}))
        _agents_cache["at"] = 0.0        # so the drawer shows it on the next poll
        self._send(200, json.dumps({"id": sid, "target": target}))

    def term_kill(self, body):
        s = term_get(str(body.get("id") or ""))
        if not s:
            return self._send(404, json.dumps({"error": "no such session"}))
        threading.Thread(target=s.kill, daemon=True).start()
        self._send(200, json.dumps({"id": s.id}))

    def term_attach(self, qs):
        """Upgrade to a WebSocket and pipe it to a session's PTY."""
        if not TERMINAL:
            return self._send(403, json.dumps({"error": "terminal disabled"}))
        # CORS doesn't apply to WebSockets; this is the only thing standing
        # between a random page you have open and a shell on this machine.
        if not origin_ok(self.headers):
            return self._send(403, json.dumps({"error": "cross-origin upgrade refused"}))
        key = self.headers.get("Sec-WebSocket-Key")
        if (self.headers.get("Upgrade") or "").lower() != "websocket" or not key:
            return self._send(400, json.dumps({"error": "expected a websocket upgrade"}))
        s = term_get((qs.get("id") or [""])[0])
        if not s:
            return self._send(404, json.dumps({"error": "no such session"}))

        self.close_connection = True        # we own the socket from here on
        try:
            self.wfile.write(
                b"HTTP/1.1 101 Switching Protocols\r\n"
                b"Upgrade: websocket\r\nConnection: Upgrade\r\n"
                b"Sec-WebSocket-Accept: " + ws_accept(key).encode() + b"\r\n\r\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            return                          # gone before the upgrade landed

        ws = WebSocket(self)
        cols, rows = _clamp_size((qs.get("cols") or [None])[0], (qs.get("rows") or [None])[0])
        s.resize(cols, rows, ws)
        ws.send_text(json.dumps({"t": "session", "session": s.info()}))
        s.attach(ws)                         # replays the scrollback
        if not s.alive:
            ws.send_text(json.dumps({"t": "exit", "code": s.code}))
        try:
            while True:
                msg = ws.recv()
                if msg is None:
                    break
                opcode, data = msg
                if opcode == WS_BIN:
                    s.write(data)            # keystrokes, verbatim
                elif opcode == WS_TEXT:
                    try:
                        m = json.loads(data.decode())
                    except (ValueError, UnicodeDecodeError):
                        continue
                    if m.get("t") == "size":
                        c, r = _clamp_size(m.get("cols"), m.get("rows"))
                        s.resize(c, r, ws)
        except (EOFError, OSError):
            pass
        finally:
            s.detach(ws)                     # detach ≠ kill: the session lives on
            ws.closed = True

    # -- live logs (SSE) --
    def stream_logs(self, qs):
        slot = (qs.get("stack") or [""])[0]
        projs = running_projects()
        st = find_stack(slot) if slot else (projs[0] if projs else None)
        self._sse_open()
        if not st:
            try:
                self._write('data: %s\n\n' % json.dumps(
                    {"t": "", "svc": "system", "level": "warn",
                     "msg": "No stack running — add one to see logs."}))
            except (BrokenPipeError, ConnectionResetError):
                pass
            return
        args = ["docker", "compose", "-p", st["slot"], "-f", st["config_file"],
                "logs", "-f", "-t", "--no-color", "--tail", "300"]
        svc = (qs.get("service") or [""])[0]
        if svc and re.match(r"^[A-Za-z0-9_-]+$", svc):
            args.append(svc)
        proc = subprocess.Popen(args, cwd=st["dir"], env=stack_env(st["slot"]),
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                text=True, bufsize=1)
        # Ping often: a disconnected client is only noticed when a write raises
        # BrokenPipeError, and the browser re-opens the stream on every stack
        # switch / worktree reconnect. A long ping interval leaves the orphaned
        # `docker compose logs -f` follower alive (and re-tailing 300 lines)
        # until the next write — they pile up and load an already-busy daemon.
        last_ping = time.monotonic()
        try:
            while True:
                r, _, _ = select.select([proc.stdout], [], [], 2)
                if r:
                    line = proc.stdout.readline()
                    if line == "":
                        break
                    line = line.rstrip("\n")
                    if line:
                        self._write("data: %s\n\n" % json.dumps(parse_log(line)))
                if time.monotonic() - last_ping > 4:
                    self._write(": ping\n\n")
                    last_ping = time.monotonic()
                if proc.poll() is not None and not r:
                    break
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            _kill(proc)

    # -- actions: shell back into bin/dev, run detached, report via /status --
    def run_action(self, body):
        action = body.get("action")
        state = build_state()

        def valid_slot(s):
            return bool(s) and re.match(r"^trip[0-9]+$", s)

        if action in ("restart", "stop"):
            slot = body.get("stack") or ""
            if not valid_slot(slot):
                return self._send(400, json.dumps({"error": "bad stack"}))
            st = next((x for x in state["stacks"] if x["slot"] == slot), None)
            cmd = stack_cmd(action, slot=slot, path=(st or {}).get("path", ""))
            if not cmd:
                return self._send(400, json.dumps({"error": "no stack configured"}))
            argv = [login_shell(), "-lc", cmd]
        elif action == "update":
            # Merge a base branch into a worktree. Git-only, so it does NOT need
            # a running stack — most work that's behind has no containers up. The
            # base comes from the caller (a PR's base branch); bin/dev falls back
            # to origin's default when it's omitted.
            target = body.get("target") or ""
            if target not in {w["path"] for w in state["worktrees"]}:
                return self._send(400, json.dumps({"error": "unknown worktree"}))
            slot = body.get("stack") or ""
            if slot and not valid_slot(slot):
                return self._send(400, json.dumps({"error": "bad stack"}))
            base = str(body.get("base") or "")
            if not re.match(r"^[\w./-]{1,80}$", base or ""):
                base = default_branch(state["root"])
            # Git-only, so it's implemented here rather than shelled out: a
            # project without a bin/dev of its own still gets "merge my base in".
            argv = [login_shell(), "-lc",
                    "set -e; git fetch origin %(b)s; "
                    "git merge --no-edit origin/%(b)s || { git merge --abort; "
                    "echo 'merge conflicts — resolve by hand'; exit 1; }" % {"b": base}]
            slot = slot or target          # lock/console key: the worktree itself
        elif action == "up":
            # Switch a slot's worktree, or spawn a new stack (stack omitted →
            # next free slot). `target` must be a known worktree.
            target = body.get("target") or ""
            if target not in {w["path"] for w in state["worktrees"]}:
                return self._send(400, json.dumps({"error": "unknown worktree"}))
            slot = body.get("stack") or state["next_slot"]
            if not valid_slot(slot):
                return self._send(400, json.dumps({"error": "bad stack"}))
            cmd = stack_cmd("up", slot=slot, path=target)
            if not cmd:
                return self._send(400, json.dumps({"error": "no stack configured"}))
            argv = [login_shell(), "-lc", cmd]
        elif action == "pr":
            ref = str(body.get("ref") or "")
            if not re.match(r"^\d+$", ref):
                return self._send(400, json.dumps({"error": "bad PR ref"}))
            slot = body.get("stack") or state["next_slot"]
            if not valid_slot(slot):
                return self._send(400, json.dumps({"error": "bad stack"}))
            wt = os.path.join(state["root"], WORKTREES, "pr-" + ref)
            argv = [login_shell(), "-lc",
                    "set -e; gh pr checkout %s --force" % shlex.quote(ref)
                    if os.path.isdir(wt) else
                    "set -e; git fetch origin pull/%(n)s/head:pr-%(n)s 2>/dev/null || true; "
                    "git worktree add %(w)s pr-%(n)s 2>/dev/null || git worktree add %(w)s"
                    % {"n": shlex.quote(ref), "w": shlex.quote(wt)}]
        else:
            return self._send(400, json.dumps({"error": "unknown action"}))

        # Per-key lock (a stack slot, or a worktree path for git-only actions):
        # a second action on the SAME key is refused, but
        # different slots run concurrently (stop trip2 + trip3 at once). Held
        # for the whole action, released by the thread when it finishes.
        lock = _slot_lock(slot)
        if not lock.acquire(blocking=False):
            return self._send(409, json.dumps({"error": "an action is already running for " + slot}))
        global _action_seq
        with _actions_guard:
            _action_seq += 1
            aid = _action_seq
            _actions[slot] = {"id": aid, "name": action, "lines": [], "running": True, "code": None}
        # Reply BEFORE any docker work starts, so the browser has the id even
        # though the imminent `compose down` will drop this very connection.
        self._send(200, json.dumps({"id": aid, "slot": slot}))
        note("action %s on %s: %s" % (action, slot, " ".join(argv[1:])))
        threading.Thread(target=_run_action_thread,
                         args=(slot, argv, state["root"], stack_env(slot)),
                         daemon=True).start()

    def action_status(self, qs):
        slot = (qs.get("slot") or [""])[0]
        try:
            since = int((qs.get("since") or ["0"])[0])
        except ValueError:
            since = 0
        with _actions_guard:
            a = _actions.get(slot)
            if not a:
                return self._send(200, json.dumps({"slot": slot, "running": False, "code": None, "total": 0, "lines": []}))
            lines = a["lines"]
            self._send(200, json.dumps({
                "slot": slot, "id": a["id"], "name": a["name"],
                "running": a["running"], "code": a["code"],
                "total": len(lines), "lines": lines[since:],
            }))


_ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]")


def _strip_ansi(s):
    return _ANSI.sub("", s)


def _kill(proc):
    if proc.poll() is None:
        try:
            proc.terminate()
            proc.wait(3)
        except (subprocess.TimeoutExpired, OSError):
            try:
                proc.kill()
            except OSError:
                pass


# ── terminals: a PTY per session, held by this host process ───────────────────
# The browser is a *view* onto a session, not its owner: output is mirrored into
# a scrollback ring so a reload (or a second tab, or the Traefik blip a restart
# causes) re-attaches and replays what it missed, and closing the tab merely
# detaches. That's what makes a web terminal usable for long jobs — `claude`
# thinking for ten minutes doesn't care that you switched worktrees meanwhile.
TERM_SCROLLBACK = 512 * 1024    # bytes of raw PTY output kept per session
TERM_MAX = 16                   # sessions at once, to bound stray shells
TERM_KEEP_DEAD = 600            # seconds a finished session stays readable
_terms = {}                     # id -> TermSession
_terms_lock = threading.Lock()
_term_seq = 0


def user_path(base=""):
    """PATH plus the user bin dirs that exist, deduped.

    An MCP server declared as a bare command (`mcp-grafana`) is only reachable if
    that command is on the PATH the process inherits — and a server started from
    a GUI, launchd or a thin shell doesn't have ~/go/bin or ~/.local/bin on it.
    Fixing each server's config to an absolute path is per-server, per-machine
    busywork; fixing the PATH once covers every server and everything an agent
    shells out to.
    """
    parts = [p for p in (base or os.environ.get("PATH", "")).split(os.pathsep) if p]
    for d in BIN_DIRS:
        full = os.path.expanduser(d)
        if os.path.isdir(full) and full not in parts:
            parts.append(full)
    return os.pathsep.join(parts)


def term_env(slot, cwd):
    """Environment for a session: the CLI's, plus what a TUI needs to render."""
    env = stack_env(slot) if slot else compose_env()
    env["PATH"] = user_path(env.get("PATH", ""))
    env["TERM"] = "xterm-256color"
    env["COLORTERM"] = "truecolor"
    env["TERM_PROGRAM"] = "workmux"
    # Box drawing and spinners in `claude` (and gum, and fzf) are UTF-8; a login
    # shell inherits our environment, and launchd hands GUI processes no locale.
    if not any(env.get(k) for k in ("LC_ALL", "LC_CTYPE", "LANG")):
        env["LANG"] = "en_US.UTF-8"
    env["PWD"] = cwd
    return env


def _clamp_size(cols, rows):
    """A window size we're willing to hand a PTY, whatever the client claimed."""
    def n(v, lo, hi, default):
        try:
            return max(lo, min(hi, int(v)))
        except (TypeError, ValueError):
            return default
    return n(cols, 20, 500, 100), n(rows, 5, 200, 30)


def _set_winsize(fd, cols, rows):
    try:
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    except OSError:
        pass


def _pty_spawn(argv, cwd, env, cols, rows):
    """Fork argv onto a new PTY; return (pid, master_fd).

    pty.fork() (not subprocess) because the child needs the slave as its
    *controlling* terminal — that's what makes ⌃C reach the foreground job
    instead of us, and what tells `claude` it's talking to a human.
    """
    pid, fd = pty.fork()
    if pid == 0:                                  # child
        try:
            _set_winsize(0, cols, rows)           # fd 0 is the slave; size it
            os.chdir(cwd)                         # before exec, so no SIGWINCH race
            os.execvpe(argv[0], argv, env)
        except OSError as e:
            os.write(2, ("workmux: cannot run %s: %s\r\n" % (argv[0], e)).encode())
        os._exit(127)
    return pid, fd


class TermSession:
    def __init__(self, sid, title, kind, argv, cwd, run_in, slot, cols, rows, agent=""):
        self.id, self.title, self.kind = sid, title, kind
        self.agent = agent                  # the background agent this attaches to
        # cwd is the worktree this session is *for* — what the tab is labelled
        # with. run_in is where argv is launched, which differs for the shell
        # preset: `bin/dev shell <worktree>` has to run from the primary
        # checkout and cd's itself.
        self.argv, self.cwd, self.slot = argv, cwd, slot
        self.cols, self.rows = cols, rows
        self.started = time.time()
        self.ended = None
        self.code = None
        self.buf = bytearray()
        self.clients = set()
        self.sizes = {}                     # viewer -> (cols, rows)
        self.lock = threading.Lock()
        self.pid, self.fd = _pty_spawn(argv, run_in, term_env(slot, cwd), cols, rows)
        threading.Thread(target=self._pump, daemon=True).start()

    # -- output: PTY → scrollback + every attached socket --
    def _pump(self):
        while True:
            try:
                data = os.read(self.fd, 65536)
            except OSError:                        # EIO: the slave side closed
                data = b""
            if not data:
                break
            with self.lock:
                self.buf += data
                if len(self.buf) > TERM_SCROLLBACK:
                    # Drop from the front, but resume just after a newline: a
                    # replay that starts mid-escape-sequence paints garbage.
                    cut = len(self.buf) - TERM_SCROLLBACK
                    nl = self.buf.find(b"\n", cut)
                    del self.buf[:nl + 1 if nl != -1 else cut]
                clients = list(self.clients)
            for c in clients:
                c.send(data)
        try:
            _, status = os.waitpid(self.pid, 0)
            self.code = os.waitstatus_to_exitcode(status)
        except (ChildProcessError, OSError, ValueError):
            self.code = -1
        self.ended = time.time()
        note("session %s (%s) ended with %s" % (self.id, self.title, self.code))
        try:
            os.close(self.fd)
        except OSError:
            pass
        with self.lock:
            clients = list(self.clients)
        for c in clients:
            c.send_text(json.dumps({"t": "exit", "code": self.code}))
            c.close()

    @property
    def alive(self):
        return self.ended is None

    def attach(self, ws):
        """Register a viewer and hand it the scrollback so far.

        The replay is written while holding the lock: release it first and the
        pump — which snapshots clients under the lock but sends outside it — can
        land fresh output on this socket *before* the scrollback it belongs after.
        """
        with self.lock:
            self.clients.add(ws)
            if self.buf:
                ws.send(bytes(self.buf))

    def detach(self, ws):
        with self.lock:
            self.clients.discard(ws)
            self.sizes.pop(ws, None)
            remaining = list(self.sizes.values())
        # The viewer that was holding it narrow may have just left.
        if remaining:
            self.resize(min(c for c, _ in remaining), min(r for _, r in remaining))

    def write(self, data):
        try:
            os.write(self.fd, data)
        except OSError:
            pass

    def resize(self, cols, rows, ws=None):
        """Fit the PTY to the *smallest* attached viewer.

        A PTY has one size and can have many viewers. Letting the latest report
        win made two viewers of different widths (a desktop and a phone on the
        same agent) resize it back and forth: a TUI only redraws on SIGWINCH, so
        the wrap points drift out of sync and the screen turns to confetti.
        Smallest-wins is stable, and content that fits the narrow viewer fits
        everyone.
        """
        with self.lock:
            if ws is not None:
                self.sizes[ws] = (cols, rows)
            live = list(self.sizes.values()) or [(cols, rows)]
            cols = min(c for c, _ in live)
            rows = min(r for _, r in live)
            if (cols, rows) == (self.cols, self.rows):
                return
            self.cols, self.rows = cols, rows
        _set_winsize(self.fd, cols, rows)

    def kill(self):
        """SIGHUP the whole process group, SIGKILL what ignores it."""
        try:
            pgid = os.getpgid(self.pid)
        except OSError:
            return
        for sig, delay in ((signal.SIGHUP, 1.5), (signal.SIGKILL, 0)):
            if not self.alive:
                return
            try:
                os.killpg(pgid, sig)
            except OSError:
                return
            if delay:
                time.sleep(delay)

    def info(self):
        return {
            "id": self.id, "title": self.title, "kind": self.kind, "agent": self.agent,
            "cwd": self.cwd, "dir": os.path.basename(self.cwd), "slot": self.slot,
            "alive": self.alive, "code": self.code, "viewers": len(self.clients),
            "cols": self.cols, "rows": self.rows,
            "age": int(time.time() - self.started),
        }


def login_shell():
    return os.environ.get("SHELL") or "/bin/sh"


TEMPO_RANK = {"blocked": 0, "active": 1, "idle": 2}


def agent_for_worktree(path):
    """The agent to resume for a worktree, or None.

    Liveness first, recency second — same order the drawer lists them in. One
    that needs input wants you most, then one that's working; a finished session
    only wins if nothing in the worktree is alive. Recency alone looked right
    until a worktree accumulated a few short-lived sessions, and the freshest
    corpse beat the agent actually running.
    """
    mine = [a for a in agents_snapshot()["agents"] if a["home"] == path]
    # Two stable passes rather than one composite key: descending timestamps and
    # ascending rank don't share a direction, and a hand-rolled inverted string
    # key sorted a *missing* timestamp first instead of last.
    mine.sort(key=lambda a: a["updated"] or "", reverse=True)
    mine.sort(key=lambda a: TEMPO_RANK.get(a["tempo"], 3))
    return mine[0] if mine else None


def agent_label(a):
    """What to call an agent in the UI. Sessions start life as "(agent)" until
    they earn a name, and "❯ (agent)" tells you nothing — fall back to the id."""
    name = (a.get("name") or "").strip()
    return name if name and name != "(agent)" else "claude " + a.get("id", "?")


def term_presets(cwd, slot, agent=""):
    """What a new session can be. Every command that isn't a plain shell comes
    from config, so pointing this at another agent CLI is configuration."""
    sh = login_shell()
    return {
        "shell": {"title": "shell", "argv": [sh, "-l"], "run_in": cwd},
        # -l so a login profile sets PATH (homebrew, nvm, asdf) before the agent
        # runs; exec so ⌃D ends the session instead of dropping to a bare shell.
        "agent": {"title": AGENT["name"], "run_in": cwd,
                  "argv": [sh, "-lc", "exec " + AGENT["command"]] if AGENT["command"] else None},
        # Logs are a session too, not a separate screen: a tail in a PTY gives you
        # grep, less and ⌃C, and one pane to learn.
        "logs": {"title": "logs", "run_in": cwd,
                 "argv": [sh, "-lc", stack_cmd("logs", slot=slot or PROJECT) or "echo 'no stack configured'"]},
        # Same idea for git: a full TUI beats anything this dashboard would grow.
        "git": {"title": "git", "run_in": cwd,
                "argv": [sh, "-lc", "exec lazygit 2>/dev/null || exec tig 2>/dev/null"
                                    " || { git -c color.ui=always log --oneline --graph -20; exec " + sh + "; }"]},
        # Take over a background agent's session in the browser. Detaching (⌃D or
        # closing the tab) leaves the agent running, same as in the CLI.
        "attach": {"title": AGENT["name"] + " " + agent, "run_in": cwd,
                   "argv": [sh, "-lc", "exec " + AGENT["attach"].format(id=shlex.quote(agent))]
                           if AGENT["attach"] else None},
    }


AGENT_ID_RE = re.compile(r"^[0-9a-f]{6,40}$")


def spawn_agent(cwd, prompt):
    """agent.spawn in a worktree: (short_id, error). Returns as soon as the agent
    is backgrounded — it then shows up in /api/agents like any other."""
    if not AGENT["spawn"]:
        return None, "no agent spawn command configured"
    if not (prompt or "").strip():
        return None, "give the agent something to do"
    if not os.path.isdir(cwd):
        return None, "worktree is gone: " + cwd
    # Through a login shell so `claude` resolves the same way it does in a
    # terminal session (this server may have been started by launchd/nohup with a
    # thin PATH that misses ~/.local/bin). stderr folded in: that's where the
    # reason lives when it refuses.
    argv = [login_shell(), "-lc", AGENT["spawn"].format(prompt=shlex.quote(prompt))]
    try:
        p = subprocess.run(argv, cwd=cwd, env=term_env("", cwd), timeout=120,
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        out, rc = _strip_ansi(p.stdout or ""), p.returncode
    except (subprocess.TimeoutExpired, OSError) as e:
        return None, str(e)
    if rc != 0:
        return None, (out.strip().splitlines() or ["agent spawn failed"])[-1][:200]
    m = re.search(r"backgrounded\s*·?\s*([0-9a-f]{6,40})", out)
    note("agent %s spawned in %s" % (m.group(1) if m else "?", os.path.basename(cwd)))
    return (m.group(1) if m else ""), None


def term_gc():
    """Forget finished sessions once their output has been read.

    An attach is dropped the moment it ends, with no grace period: `claude
    attach` exiting means you detached, the agent is still running, and its
    transcript lives in the session rather than in that pane. Keeping the corpse
    listed put a dead tab back in the dock on every poll — next to the live tab
    for the very same agent, if you'd re-attached meanwhile.
    """
    now = time.time()
    with _terms_lock:
        for sid in [s.id for s in _terms.values() if s.ended and not s.clients
                    and (s.kind == "attach" or now - s.ended > TERM_KEEP_DEAD)]:
            _terms.pop(sid, None)


def term_list():
    term_gc()
    with _terms_lock:
        return sorted((s.info() for s in _terms.values()), key=lambda i: i["id"])


def term_new(kind, cwd, slot, cols, rows, agent=""):
    """(session, error). Errors are user-facing strings."""
    if kind == "resume":
        # "Give me this worktree's Claude session." Resolved here so the answer is
        # the same whether it came from the dock, a keystroke or curl. No agent
        # there yet → start one, rather than making the button a dead end.
        a = agent_for_worktree(cwd)
        kind, agent = ("attach", a["id"]) if a else ("agent", "")
    if kind == "attach" and not AGENT_ID_RE.match(agent):
        return None, "bad agent id"
    if kind == "attach":
        # One tab per agent. Clicking ❯ resume twice, or attaching from the drawer
        # to something the dock already shows, must land on the session you have
        # rather than start a second `claude attach` against the same agent —
        # which is confusing to look at and gives it two competing viewers.
        with _terms_lock:
            live = [s for s in _terms.values() if s.agent == agent and s.alive]
        if live:
            return live[0], None
    p = term_presets(cwd, slot, agent).get(kind)
    if not p:
        return None, "unknown kind"
    if not p.get("argv"):
        return None, "no %s configured for this project" % kind
    if kind == "attach":
        named = next((a for a in agents_snapshot()["agents"] if a["id"] == agent), None)
        if named:
            p = dict(p, title=agent_label(named))   # the tab says what it is
    if not os.path.isdir(cwd):
        return None, "worktree is gone: " + cwd
    term_gc()
    global _term_seq
    with _terms_lock:
        if sum(1 for s in _terms.values() if s.alive) >= TERM_MAX:
            return None, "too many terminals open (%d)" % TERM_MAX
        _term_seq += 1
        sid = "t%d" % _term_seq
    try:
        s = TermSession(sid, p["title"], kind, p["argv"], cwd, p["run_in"], slot, cols, rows, agent)
    except OSError as e:
        return None, str(e)
    with _terms_lock:
        _terms[sid] = s
    note("session %s started: %s in %s" % (sid, p["title"], os.path.basename(cwd)))
    return s, None


def term_get(sid):
    with _terms_lock:
        return _terms.get(sid)


# ── minimal WebSocket (RFC 6455) ───────────────────────────────────────────────
# Only what a terminal needs: binary frames carry raw PTY bytes in both
# directions (so a UTF-8 rune split across reads still renders), text frames
# carry JSON control messages (resize, exit). No extensions, no compression.
WS_MAGIC = b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
WS_CONT, WS_TEXT, WS_BIN, WS_CLOSE, WS_PING, WS_PONG = 0x0, 0x1, 0x2, 0x8, 0x9, 0xA
WS_MAX_FRAME = 8 << 20


class WebSocket:
    """One upgraded connection. Reads happen on the request thread; writes can
    come from any thread (the PTY pump), so sends are serialised."""

    def __init__(self, handler):
        self.rfile = handler.rfile
        self.sock = handler.connection
        self._send = threading.Lock()
        self.closed = False

    def send(self, payload, opcode=WS_BIN):
        head = bytearray([0x80 | opcode])
        n = len(payload)
        if n < 126:
            head.append(n)
        elif n < 65536:
            head.append(126)
            head += struct.pack("!H", n)
        else:
            head.append(127)
            head += struct.pack("!Q", n)
        with self._send:
            if self.closed:
                return
            try:
                self.sock.sendall(bytes(head) + payload)
            except OSError:
                self.closed = True

    def send_text(self, s):
        self.send(s.encode(), WS_TEXT)

    def close(self):
        """Send a close frame and unblock the reader waiting on recv()."""
        self.send(b"", WS_CLOSE)
        with self._send:
            self.closed = True
        try:
            self.sock.shutdown(2)
        except OSError:
            pass

    def _exact(self, n):
        buf = self.rfile.read(n)
        if buf is None or len(buf) < n:
            raise EOFError
        return buf

    def recv(self):
        """Next (opcode, payload); None once the peer is done. Joins fragments
        and answers pings inline."""
        frags, first = bytearray(), None
        while True:
            b1, b2 = self._exact(2)
            fin, opcode, masked, n = b1 & 0x80, b1 & 0x0F, b2 & 0x80, b2 & 0x7F
            if n == 126:
                n = struct.unpack("!H", self._exact(2))[0]
            elif n == 127:
                n = struct.unpack("!Q", self._exact(8))[0]
            if n > WS_MAX_FRAME:
                raise EOFError                      # nothing legitimate is this big
            key = self._exact(4) if masked else b""
            data = self._exact(n) if n else b""
            if masked:
                data = bytes(c ^ key[i & 3] for i, c in enumerate(data))
            if opcode == WS_CLOSE:
                return None
            if opcode == WS_PING:
                self.send(data, WS_PONG)
                continue
            if opcode == WS_PONG:
                continue
            if opcode == WS_CONT:
                frags += data
            else:
                first, frags = opcode, bytearray(data)
            if fin:
                return first, bytes(frags)


def ws_accept(key):
    return base64.b64encode(hashlib.sha1(key.encode() + WS_MAGIC).digest()).decode()


def token_ok(handler):
    """True when this request may act. Loopback is trusted; everything else needs
    the token, from a cookie (set once from ?t=), a header, or the query string."""
    if not TOKEN:
        return True
    if (handler.client_address[0] or "") in LOOPBACK:
        return True
    given = ""
    q = parse_qs(urlparse(handler.path).query).get("t")
    if q:
        given = q[0]
    elif handler.headers.get("X-Dev-Token"):
        given = handler.headers["X-Dev-Token"]
    else:
        for part in (handler.headers.get("Cookie") or "").split(";"):
            k, _, v = part.strip().partition("=")
            if k == "dev_token":
                given = v
    return hmac.compare_digest(given, TOKEN)


LOGIN_PAGE = """<!doctype html><meta name=viewport content="width=device-width,initial-scale=1">
<title>dev</title><style>body{background:#0e0e13;color:#e9e9f0;font:15px/1.6 -apple-system,sans-serif;
display:grid;place-items:center;height:100dvh;margin:0}form{display:flex;gap:8px;flex-direction:column;width:min(320px,86vw)}
input{background:#1a1a22;border:1px solid #2f2f3b;border-radius:10px;padding:13px;color:inherit;font:inherit}
button{background:#7c6cf0;border:0;border-radius:10px;padding:13px;color:#100c22;font:600 15px/1 inherit}
p{color:#9b9cad;font-size:13px}</style>
<form><p>This dev server needs its token.<br>It was printed where you started it.</p>
<input name=t type=password autocomplete=current-password placeholder="token" autofocus>
<button>Open</button></form>"""


def origin_ok(headers):
    """False when a request came from another site.

    Same-origin protects the JSON API (a cross-site POST of application/json is
    preflighted, and we answer no CORS headers), but WebSockets are exempt from
    CORS entirely: without this check, any page you happened to have open could
    upgrade against 127.0.0.1:4315 and get a shell. Browsers always send Origin
    on an upgrade, so allowlisting the loopback names this UI is served under is
    enough. No Origin at all means a local CLI client — curl, wscat — which
    already has the shell we'd be handing it.
    """
    origin = headers.get("Origin")
    if not origin:
        return True
    host = (urlparse(origin).hostname or "").lower()
    if host in LOOPBACK or host.endswith(".localhost"):
        return True
    # Served from somewhere else (a LAN address, a tunnel, a real hostname):
    # same-origin is still the rule, so compare against the Host we were asked
    # for. A cross-site page cannot forge Host to match its own Origin.
    served = (headers.get("Host") or "").rsplit(":", 1)[0].strip("[]").lower()
    return bool(served) and host == served


PAGE = r"""<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover, interactive-widget=resizes-content">
<meta name="theme-color" content="#0e0e13">
<meta name="apple-mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">
<title>workmux</title>
<style>
  :root{
    color-scheme:dark;
    --bg:#0e0e13; --panel:#15151c; --elev:#1a1a22; --elev-2:#20202a;
    --line:#262631; --line-2:#2f2f3b;
    --ink:#e9e9f0; --ink-2:#9b9cad; --ink-3:#63647a;
    --acc:#7c6cf0; --acc-ink:#b7adf9; --acc-bg:rgba(124,108,240,.14); --acc-line:rgba(124,108,240,.35);
    --ok:#48c98a; --warn:#e6b25a; --bad:#ec6a5e;
    --sans:-apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif;
    --mono:ui-monospace,"SF Mono","JetBrains Mono","Menlo","Cascadia Code",monospace;
  }
  *{box-sizing:border-box}
  html,body{margin:0;height:100%}
  body{background:var(--bg);color:var(--ink);font-family:var(--sans);
    -webkit-font-smoothing:antialiased;-webkit-text-size-adjust:100%;line-height:1.5;
    height:100vh;height:100dvh;display:flex;flex-direction:column;overflow:hidden}
  ::selection{background:#332d5e;color:#fff}
  button{font-family:inherit}

  /* top bar */
  .bar{display:flex;align-items:center;gap:14px;padding:11px 16px;border-bottom:1px solid var(--line);
    background:#111117;flex:none;position:relative;z-index:20}
  .switch{display:flex;align-items:center;gap:10px;background:var(--elev);border:1px solid var(--line-2);
    border-radius:9px;padding:8px 12px;cursor:pointer;min-width:0;max-width:min(46vw,440px)}
  .switch:hover{border-color:#3a3a49}
  .switch .br{font:600 13px/1 var(--sans);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
  .switch .car{color:var(--ink-3);flex:none}
  .barstat{display:flex;align-items:center;gap:14px;color:var(--ink-2);font:500 12.5px/1 var(--mono);flex-wrap:wrap}
  .barstat b{color:var(--ink);font-weight:600}
  .barstat .sep{color:var(--ink-3)}
  .barbtns{margin-left:auto;display:flex;gap:8px;flex:none}
  .b{font:600 12px/1 var(--sans);padding:9px 12px;border-radius:8px;border:1px solid var(--line-2);
    background:var(--elev);color:var(--ink);cursor:pointer;display:inline-flex;gap:7px;align-items:center;white-space:nowrap}
  .b:hover{border-color:#3a3a49}
  .b:disabled{opacity:.4;cursor:default}
  .b.pri{background:var(--acc);border-color:var(--acc);color:#100c22}
  .b.pri:hover{filter:brightness(1.08)}
  .b.danger:hover{border-color:var(--bad);color:#f2a49c}
  .b.wtpath{font-family:var(--mono);font-weight:600;color:var(--ink-2);max-width:280px}
  .b.wtpath #copy-dir{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .b.wtpath:hover:not(:disabled){color:var(--ink)}

  /* multi-stack tabs strip */
  .stacks{display:flex;align-items:center;gap:7px;padding:9px 16px;border-bottom:1px solid var(--line);background:#0f0f15;flex-wrap:wrap}
  .stacks .lbl{font:600 10px/1 var(--mono);letter-spacing:.14em;text-transform:uppercase;color:var(--ink-3);margin-right:4px}
  .stab{display:inline-flex;align-items:center;gap:8px;padding:7px 11px;border-radius:8px;border:1px solid var(--line-2);background:var(--elev);color:var(--ink-2);cursor:pointer;font:600 12px/1 var(--sans);max-width:300px}
  .stab:hover{border-color:#3a3a49}
  .stab .slot{font:700 10px/1 var(--mono);color:var(--ink-3);letter-spacing:.04em}
  .stab .br{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .stab.active{background:var(--acc-bg);border-color:var(--acc-line);color:#fff}
  .stab.active .slot{color:var(--acc-ink)}
  .addstack{display:inline-flex;align-items:center;gap:6px;padding:7px 11px;border-radius:8px;border:1px dashed var(--line-2);background:transparent;color:var(--ink-2);cursor:pointer;font:600 12px/1 var(--sans)}
  .addstack:hover{border-color:var(--acc-line);color:var(--acc-ink)}
  .sw-slot,.switch .slot{font:700 10px/1 var(--mono);color:var(--acc-ink);letter-spacing:.04em}

  /* agents button + drawer */
  .agents-btn{margin-left:auto;display:inline-flex;align-items:center;gap:7px;padding:7px 11px;border-radius:8px;border:1px solid var(--line-2);background:var(--elev);color:var(--ink-2);cursor:pointer;font:600 12px/1 var(--sans)}
  .agents-btn:hover{border-color:#3a3a49;color:var(--ink)}
  .agents-btn .badge{border-radius:999px;font:700 10px/1 var(--mono);padding:2px 6px;color:#100c22}
  .ag-scrim{position:fixed;inset:0;background:#0007;display:none;z-index:45}
  .ag-scrim.on{display:block}
  .drawer{position:fixed;top:0;right:0;bottom:0;width:min(500px,96vw);background:var(--panel);border-left:1px solid var(--line-2);
    z-index:46;transform:translateX(100%);transition:transform .18s ease;display:flex;flex-direction:column;box-shadow:-20px 0 60px -30px #000}
  .drawer.on{transform:translateX(0)}
  .drawer h3{margin:0;padding:15px 18px;border-bottom:1px solid var(--line);font:600 15px/1 var(--sans);display:flex;align-items:center;gap:10px}
  .drawer h3 .c{font:500 11px/1 var(--mono);color:var(--ink-3)}
  .drawer h3 .ag-scope{margin-left:auto;background:var(--elev);border:1px solid var(--line-2);color:var(--ink-2);border-radius:7px;padding:6px 10px;font:600 11px/1 var(--sans);cursor:pointer}
  .drawer h3 .ag-scope:hover{border-color:var(--acc-line);color:var(--acc-ink)}
  .drawer h3 .x{background:0;border:0;color:var(--ink-3);cursor:pointer;font-size:16px}
  .ag-empty{color:var(--ink-3);font:500 12.5px/1.6 var(--sans);padding:14px 8px}
  .drawer .body{overflow:auto;padding:8px 10px;flex:1}
  .ag-group .gh{display:flex;align-items:center;gap:8px;font:600 11px/1 var(--mono);color:var(--ink-3);text-transform:uppercase;letter-spacing:.04em;padding:12px 8px 4px}
  .ag-group .gh .pr{color:var(--acc-ink);text-transform:none}
  .ag-group .gh .run{color:var(--ok);text-transform:none}
  .ag-card{background:var(--elev);border:1px solid var(--line);border-radius:10px;padding:11px 12px;margin:6px 4px;display:grid;grid-template-columns:10px 1fr;gap:11px}
  .ag-card .nm{font:600 13px/1.2 var(--sans);color:var(--ink)}
  .ag-card .dt{font:500 12px/1.55 var(--sans);color:var(--ink-2);margin-top:6px;word-break:break-word}
  .ag-card .ft{display:flex;flex-wrap:wrap;gap:10px;align-items:center;margin-top:9px;font:500 11px/1 var(--mono);color:var(--ink-3)}
  .ag-card .ft a{color:var(--acc-ink);text-decoration:none}
  .ag-card .ft a:hover{text-decoration:underline}
  .ag-dot{width:9px;height:9px;border-radius:50%;margin-top:3px;flex:none}
  .ag-dot.active{background:var(--ok);box-shadow:0 0 8px #48c98a88}
  .ag-dot.blocked{background:var(--warn);box-shadow:0 0 8px #e6b25a88}
  .ag-dot.idle{background:#3a4250}
  .ag-chip{font:700 9px/1 var(--mono);text-transform:uppercase;letter-spacing:.06em;padding:3px 6px;border-radius:5px}
  .ag-chip.active{background:#12351f;color:#5be08c}
  .ag-chip.blocked{background:#3a2f16;color:#e6b25a}
  .ag-chip.idle{color:var(--ink-3);border:1px solid var(--line)}
  .ag-group .gh .ag-add{margin-left:auto;background:transparent;border:1px dashed var(--line-2);color:var(--ink-2);
    border-radius:6px;padding:4px 7px;cursor:pointer;font:600 10px/1 var(--mono);text-transform:none}
  .ag-group .gh .ag-add:hover{border-color:var(--acc-line);color:var(--acc-ink)}
  .ag-card .ft .ag-attach{background:transparent;border:1px solid var(--line-2);color:var(--ink-2);
    border-radius:6px;padding:4px 7px;cursor:pointer;font:600 10px/1 var(--mono)}
  .ag-card .ft .ag-attach:hover{border-color:var(--acc-line);color:var(--acc-ink)}
  .ag-slim{display:flex;align-items:center;gap:9px;padding:6px 12px 6px 8px;margin:1px 4px;border-radius:7px}
  .ag-slim:hover{background:var(--elev)}
  .ag-slim .sn{font:500 12px/1.3 var(--sans);color:var(--ink-2);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .ag-slim .run{font:600 10px/1 var(--mono);color:var(--ok)}
  .ag-slim .ag-add{margin-left:auto;opacity:0}
  .ag-slim:hover .ag-add{opacity:1}
  .wt .sub .wt-ag{color:var(--ink-3)}
  .wt .sub .wt-ag.working{color:var(--ok)}
  .wt .sub .wt-ag.blocked{color:var(--warn)}
  @media (prefers-reduced-motion:no-preference){ .ag-dot.active{animation:pulse 2.2s ease-in-out infinite} }

  .dot{width:8px;height:8px;border-radius:50%;flex:none;display:inline-block;background:#3a3a46}
  .dot.ok{background:var(--ok);box-shadow:0 0 7px #48c98a66}
  .dot.warn{background:var(--warn);box-shadow:0 0 7px #e6b25a66}
  .dot.bad{background:var(--bad);box-shadow:0 0 7px #ec6a5e66}
  @media (prefers-reduced-motion:no-preference){
    .dot.live{animation:pulse 2.2s ease-in-out infinite}
    @keyframes pulse{0%,100%{opacity:1}50%{opacity:.45}}
    .blink{animation:blink 1.15s steps(1) infinite}@keyframes blink{50%{opacity:0}}
  }

  /* worktree dropdown */
  .pop{position:absolute;top:56px;left:16px;width:min(560px,92vw);max-height:70vh;overflow:auto;
    background:var(--panel);border:1px solid var(--line-2);border-radius:12px;
    box-shadow:0 24px 60px -20px #000,0 0 0 1px #0006;padding:8px;display:none}
  .pop.on{display:block}
  .pop .ph{display:flex;align-items:center;gap:9px;background:var(--elev);border:1px solid var(--line);
    border-radius:8px;padding:9px 11px;margin:4px 4px 8px}
  .pop .ph input{flex:1;background:0;border:0;outline:0;color:var(--ink);font:500 13px/1 var(--sans)}
  .pop .ph input::placeholder{color:var(--ink-3)}
  .pop .sect{font:600 10px/1 var(--mono);letter-spacing:.12em;text-transform:uppercase;color:var(--ink-3);padding:10px 12px 6px}
  .wt{display:grid;grid-template-columns:10px 1fr auto;gap:11px;align-items:center;padding:9px 12px;
    border-radius:8px;cursor:pointer;border:1px solid transparent}
  .wt:hover,.wt.kb{background:var(--elev)}
  .wt .br{font:550 12.5px/1.2 var(--sans);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
  .wt .sub{font:500 11px/1 var(--mono);color:var(--ink-3);margin-top:5px;display:flex;gap:8px;align-items:center}
  .wt .sub .pr{color:var(--acc-ink)}
  .wt.live{background:var(--acc-bg);border-color:var(--acc-line)}
  .wt .go{opacity:0;font:600 10px/1 var(--mono);color:var(--acc-ink)}
  .wt:hover .go,.wt.kb .go{opacity:1}
  .wt.live .go{opacity:1;color:var(--ok)}
  .dot.wtd{margin-top:1px}

  /* log controls */
  .ctl{display:flex;align-items:center;gap:6px;padding:11px 16px 9px;flex:none;flex-wrap:wrap}
  .tabs{display:flex;gap:2px;flex-wrap:wrap;min-width:0}
  .tab{font:550 12.5px/1 var(--sans);color:var(--ink-2);padding:8px 11px;border-radius:8px;cursor:pointer;white-space:nowrap;border:0;background:0}
  .tab:hover{color:var(--ink)}
  .tab.sel{background:var(--elev-2);color:var(--ink)}
  .tab .c{font:600 10px/1 var(--mono);color:var(--ink-3);margin-left:6px}
  .tab.sel.err .c,.tab.err .c{color:var(--bad)}
  .filter{margin-left:auto;display:flex;align-items:center;gap:9px;background:var(--elev);border:1px solid var(--line);
    border-radius:9px;padding:8px 12px;min-width:220px}
  .filter svg{flex:none;color:var(--ink-3)}
  .filter input{background:0;border:0;outline:0;color:var(--ink);font:500 13px/1 var(--sans);width:100%}
  .filter input::placeholder{color:var(--ink-3)}
  .foll{display:inline-flex;align-items:center;gap:7px;font:600 11px/1 var(--mono);color:var(--ink-3);
    border:1px solid var(--line);border-radius:8px;padding:8px 10px;cursor:pointer}
  .foll.on{color:var(--acc-ink);border-color:var(--acc-line)}

  /* terminal dock — a bottom split, so logs stay visible while you work */
  .tdock{display:none;flex-direction:column;flex:none;height:44vh;min-height:150px;
    border-top:1px solid var(--line-2);background:#0c0c11;position:relative}
  .tdock.on{display:flex}
  .tdock.max{height:calc(100vh - 96px)}
  .tgrip{position:absolute;top:-3px;left:0;right:0;height:7px;cursor:ns-resize;z-index:5}
  .tgrip:hover,.tgrip.drag{background:var(--acc-line)}
  .thead{display:flex;align-items:center;gap:7px;padding:8px 12px 8px 16px;border-bottom:1px solid var(--line);flex:none;flex-wrap:wrap}
  .thead .tlbl{font:600 10px/1 var(--mono);letter-spacing:.14em;text-transform:uppercase;color:var(--ink-3);margin-right:2px}
  .tnew{padding:6px 9px;border-radius:7px;border:1px dashed var(--line-2);background:transparent;
    color:var(--ink-2);cursor:pointer;font:600 11.5px/1 var(--mono)}
  .tnew:hover{border-color:var(--acc-line);color:var(--acc-ink)}
  .tright{margin-left:auto;display:flex;align-items:center;gap:7px}
  .tcwd{font:500 11px/1 var(--mono);color:var(--ink-3);max-width:34vw;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .ticon{background:var(--elev);border:1px solid var(--line-2);color:var(--ink-2);border-radius:7px;
    padding:6px 9px;cursor:pointer;font:600 11px/1 var(--mono)}
  .ticon:hover{border-color:var(--acc-line);color:var(--acc-ink)}
  .tbody{flex:1;min-height:0;position:relative}
  .tpane{position:absolute;inset:0;padding:6px 4px 4px 10px;display:none}
  .tpane.on{display:block}
  .tpane .xterm{height:100%}
  .tnote{color:var(--ink-3);font:500 13px/1.7 var(--sans);padding:26px 22px;max-width:60ch}
  .tnote .th{font:600 12px/1 var(--mono);color:var(--ink-2);letter-spacing:.05em;margin-bottom:8px}

  /* resource footprint: aggregate meters in the bar + per-service popover */
  .res{display:flex;align-items:center;gap:16px}
  .meter{display:flex;align-items:center;gap:8px;cursor:pointer}
  .meter .mlabel{font:600 10px/1 var(--mono);letter-spacing:.1em;color:var(--ink-3)}
  .meter .mbar{width:48px;height:5px;border-radius:3px;background:var(--elev-2);overflow:hidden;flex:none}
  .meter .mbar i{display:block;height:100%;width:0;background:var(--acc);border-radius:3px;transition:width .5s ease}
  .meter .mval{font:600 12px/1 var(--mono);color:var(--ink);font-variant-numeric:tabular-nums}
  .meter.hot .mbar i{background:var(--warn)}
  .meter.hot .mval{color:var(--warn)}
  .pop.right{left:auto;right:16px;width:min(380px,92vw)}
  .rtot{display:flex;justify-content:space-between;align-items:baseline;padding:8px 12px 10px;
    color:var(--ink-2);font:500 12px/1 var(--mono)}
  .rtot b{color:var(--ink);font-weight:600}
  .rrow{display:grid;grid-template-columns:130px 1fr 88px;gap:10px;align-items:center;
    padding:7px 12px;font-family:var(--mono);font-size:11.5px}
  .rrow .rn{color:var(--ink);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
  .rrow .rbar{height:6px;border-radius:3px;background:var(--elev-2);overflow:hidden}
  .rrow .rbar i{display:block;height:100%;background:var(--acc);border-radius:3px}
  .rrow .rv{text-align:right;color:var(--ink-2);font-variant-numeric:tabular-nums}

  /* action console overlay */
  /* non-blocking: a floating progress card, not a full-screen modal, so other
     stacks stay usable while one is starting/restarting */
  .scrim{position:fixed;right:16px;bottom:16px;display:none;z-index:40}
  .scrim.on{display:block}
  .console{width:min(460px,92vw);max-height:46vh;display:flex;flex-direction:column;
    background:var(--panel);border:1px solid var(--line-2);border-radius:12px;overflow:hidden;box-shadow:0 20px 60px -20px #000}
  .stab .spin{width:11px;height:11px;border-radius:50%;border:2px solid var(--line-2);border-top-color:var(--acc);flex:none}
  .stab.provisional{border-style:dashed;color:var(--ink-2)}
  @media (prefers-reduced-motion:no-preference){ .stab .spin{animation:spin .7s linear infinite} @keyframes spin{to{transform:rotate(360deg)}} }
  .console h4{margin:0;padding:15px 18px;border-bottom:1px solid var(--line);font:600 14px/1 var(--sans);display:flex;align-items:center;gap:10px}
  .console h4 .x{margin-left:auto;color:var(--ink-3);cursor:pointer;background:0;border:0;font-size:16px}
  .console pre{margin:0;padding:14px 18px;overflow:auto;font-family:var(--mono);font-size:12px;line-height:1.7;color:#c7c9d6;flex:1;white-space:pre-wrap;word-break:break-word}
  .console .ftr{padding:11px 18px;border-top:1px solid var(--line);color:var(--ink-3);font:500 12px/1 var(--mono);display:flex;align-items:center;gap:9px}

  /* ── layout: work list + one pane ─────────────────────────────────────────
     The unit of work is a worktree + its agent; a stack is an optional
     attachment to one. So the list of work is the dashboard, the agent session
     is the main pane, and logs only exist for work that has containers up. */
  .top{display:flex;align-items:center;gap:10px;padding:9px 12px;border-bottom:1px solid var(--line);
    background:#111117;flex:none}
  .top .newwork{font-size:12.5px}
  .topstat{display:flex;gap:12px;align-items:center;color:var(--ink-2);font:500 12px/1 var(--mono);
    min-width:0;overflow:hidden;white-space:nowrap}
  .topstat span{flex:none}
  .topstat b{color:var(--ink);font-weight:600}
  .topstat .w{color:var(--ok)}
  .topstat .n{color:var(--warn)}
  #btn-reload{margin-left:auto}

  .shell{flex:1;min-height:0;display:grid;grid-template-columns:330px minmax(0,1fr)}
  .worklist{border-right:1px solid var(--line);display:flex;flex-direction:column;
    min-height:0;min-width:0;background:#101016}
  .wl-search{display:flex;align-items:center;gap:8px;padding:10px 12px;border-bottom:1px solid var(--line);flex:none}
  .wl-search input{flex:1;background:0;border:0;outline:0;color:var(--ink);font:500 13px/1 var(--sans);min-width:0}
  .wl-search input::placeholder{color:var(--ink-3)}
  .wl-body{flex:1;overflow:auto;padding:6px}
  .wl-sect{font:600 10px/1 var(--mono);letter-spacing:.12em;text-transform:uppercase;color:var(--ink-3);padding:12px 8px 6px}

  .wrow{display:grid;grid-template-columns:10px 1fr auto;gap:10px;align-items:start;padding:10px;
    border:1px solid transparent;border-radius:10px;cursor:pointer}
  .wrow:hover{background:var(--elev)}
  .wrow.sel{background:var(--acc-bg);border-color:var(--acc-line)}
  .wrow .wdot{width:9px;height:9px;border-radius:50%;margin-top:4px;background:#3a4250;flex:none}
  .wrow .wdot.active{background:var(--ok);box-shadow:0 0 8px #48c98a88}
  .wrow .wdot.blocked{background:var(--warn);box-shadow:0 0 8px #e6b25a88}
  .wrow .wdot.up{background:var(--acc)}
  @media (prefers-reduced-motion:no-preference){ .wrow .wdot.active{animation:pulse 2.2s ease-in-out infinite} }
  .wrow .wbr{font:600 12.5px/1.3 var(--sans);color:var(--ink);word-break:break-word}
  .wrow.dim .wbr{color:var(--ink-2);font-weight:500}
  .wrow .wsub{margin-top:4px;font:500 11px/1.45 var(--mono);color:var(--ink-3);display:flex;gap:8px;flex-wrap:wrap}
  .wrow .wsub .pr{color:var(--acc-ink)}
  .wrow .wsub .slot{color:var(--acc-ink)}
  .wrow .wsub .need{color:var(--warn)}
  .wrow .wsub .run{color:var(--ok)}
  .wrow .wdet{margin-top:5px;font:500 11.5px/1.5 var(--sans);color:var(--ink-2);
    display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
  .wrow .wside{display:flex;flex-direction:column;align-items:flex-end;gap:6px}
  .wrow .wgo{opacity:0;font:600 10px/1 var(--mono);color:var(--acc-ink);white-space:nowrap;padding-top:2px}
  .wrow:hover .wgo,.wrow.sel .wgo{opacity:1}
  .wrow .wsub .off{color:var(--ink-3)}
  .wrow .wwho{margin-top:5px;font:500 11.5px/1.5 var(--sans);
    display:-webkit-box;-webkit-line-clamp:3;-webkit-box-orient:vertical;overflow:hidden}
  .wrow .wwho .nm{color:var(--acc-ink);font-weight:600}
  .wrow .wwho .dt{color:var(--ink-2)}
  .wrow .wwho .dt::before{content:" · ";color:var(--ink-3)}
  .wrow .wago{font:500 10.5px/1 var(--mono);color:var(--ink-3);white-space:nowrap}
  .wrow .wstart{background:transparent;border:1px solid var(--line-2);color:var(--ink-2);
    border-radius:7px;padding:6px 8px;font:600 10.5px/1 var(--mono);cursor:pointer;white-space:nowrap}
  .wrow .wstart:hover{border-color:var(--acc-line);color:var(--acc-ink)}
  @media (max-width:820px){
    .wrow .wgo{display:none}
    .wrow .wstart{padding:9px 11px;font-size:11.5px}
  }

  /* min-width:0 all the way down: a Rails backtrace or a row of buttons will
     otherwise set the column's intrinsic width and scroll the whole page
     sideways, which is also what pushed the fixed bottom nav off screen. */
  .pane{display:flex;flex-direction:column;min-height:0;min-width:0}
  .paneview,.tdock,.tbody,.wl-body{min-width:0}
  .wrow{min-width:0}
  .wrow .wbr,.wrow .wdet{overflow-wrap:anywhere}
  /* focus keeps the pane switcher (and, on a phone, the nav) so you can get
     back out — hiding everything was a trap */
  body.focus .worklist,body.focus .top,body.focus .workhead{display:none}
  body.focus .shell{grid-template-columns:1fr}
  .wl-body .empty{padding:26px 12px;font-size:12.5px}
  .workhead{display:flex;align-items:center;gap:9px;padding:10px 14px;border-bottom:1px solid var(--line);
    flex:none;flex-wrap:wrap;background:#0f0f15}
  .workhead .wh-br{font:600 13px/1.2 var(--sans)}
  .workhead .wh-meta{display:flex;gap:10px;align-items:center;color:var(--ink-3);font:500 11.5px/1 var(--mono);flex-wrap:wrap}
  .workhead .wh-meta a{color:var(--acc-ink);text-decoration:none}
  .workhead .wh-acts{margin-left:auto;display:flex;gap:7px;flex-wrap:wrap}
  .workhead .b{padding:8px 10px;font-size:11.5px}
  .paneview{flex:1;min-height:0;display:flex;flex-direction:column}
  .panetabs{display:flex;gap:2px;padding:7px 12px 0;flex:none;align-items:flex-end}
  .panetabs button{font:600 12px/1 var(--sans);color:var(--ink-2);background:0;border:0;
    border-radius:8px 8px 0 0;padding:9px 13px;cursor:pointer;border-bottom:2px solid transparent}
  .panetabs button.on{color:var(--ink);border-bottom-color:var(--acc)}
  .panetabs .cnt{font:600 10px/1 var(--mono);color:var(--ink-3);margin-left:6px}
  body[data-pane="changes"] .tdock{display:none}
  .changes{flex:1;min-height:0;min-width:0;display:none;grid-template-columns:320px minmax(0,1fr)}
  body[data-pane="changes"] .changes{display:grid}
  .chlist{border-right:1px solid var(--line);overflow:auto;min-width:0;background:#101016}
  .chsect{font:600 10px/1 var(--mono);letter-spacing:.12em;text-transform:uppercase;
    color:var(--ink-3);padding:12px 12px 6px;display:flex;gap:8px;align-items:center}
  .chsect .n{color:var(--ink-2)}
  .chrow{display:flex;align-items:baseline;gap:8px;padding:8px 12px;cursor:pointer;min-width:0}
  .chrow:hover{background:var(--elev)}
  .chrow.on{background:var(--acc-bg)}
  .chrow .st{font:700 10px/1 var(--mono);width:18px;flex:none;color:var(--ink-3)}
  .chrow .st.M{color:var(--warn)} .chrow .st.A{color:var(--ok)} .chrow .st.D{color:var(--bad)}
  .chrow .st.U{color:var(--acc-ink)}
  .chrow .fp{flex:1;min-width:0;font:500 12px/1.45 var(--mono);color:var(--ink);
    overflow-wrap:anywhere}
  .chrow .num{font:500 10.5px/1 var(--mono);white-space:nowrap;flex:none}
  .chrow .num .a{color:var(--ok)} .chrow .num .d{color:var(--bad)}
  .chcommit{display:flex;gap:9px;padding:8px 12px;font:500 11.5px/1.5 var(--mono);min-width:0;
    cursor:pointer;align-items:baseline}
  .chcommit:hover{background:var(--elev)}
  .chcommit.on{background:var(--acc-bg)}
  .chcommit .local{font:700 9px/1 var(--mono);text-transform:uppercase;letter-spacing:.06em;
    color:var(--warn);border:1px solid #4a3f22;border-radius:5px;padding:3px 5px;flex:none}
  .chcommit .sha{color:var(--acc-ink);flex:none}
  .chcommit .msg{color:var(--ink-2);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .chdiff{overflow:auto;min-width:0;padding:0 0 24px}
  .chdiff .hd{position:sticky;top:0;background:#0f0f15;border-bottom:1px solid var(--line);
    padding:10px 14px;font:600 12px/1.3 var(--mono);color:var(--ink);z-index:2;
    display:flex;gap:10px;align-items:center}
  .chdiff .hd .back{display:none;background:var(--elev);border:1px solid var(--line-2);
    color:var(--ink-2);border-radius:7px;padding:6px 9px;font:600 11px/1 var(--mono);cursor:pointer}
  .chdiff pre{margin:0;font-family:var(--mono);font-size:12px;line-height:1.55;white-space:pre}
  .chdiff .l{padding:0 14px;display:block;overflow-wrap:anywhere;white-space:pre-wrap}
  .chdiff .l.add{background:#0e2a17;color:#9be8b6}
  .chdiff .l.del{background:#2e1414;color:#f0a9a2}
  .chdiff .l.hunk{background:#141a2c;color:#8fb8e8}
  .chdiff .l.meta{color:var(--ink-3)}
  .sheet.wide{width:min(620px,100%)}
  .sheet h4 .ticon{margin-left:auto}
  .mcplist{display:flex;flex-direction:column;gap:5px}
  .mcprow{display:grid;grid-template-columns:auto 1fr auto;gap:10px;align-items:center;
    padding:10px 12px;border:1px solid var(--line);border-radius:9px;background:var(--elev);min-width:0}
  .mcprow .dot{width:8px;height:8px;border-radius:50%;flex:none}
  .mcprow.ok .dot{background:var(--ok)} .mcprow.auth .dot{background:var(--warn)}
  .mcprow.fail .dot{background:var(--bad)} .mcprow.pending .dot{background:var(--ink-3)}
  .mcprow .nm{font:600 12.5px/1.35 var(--mono);color:var(--ink);overflow-wrap:anywhere}
  .mcprow .meta{font:500 11px/1.5 var(--mono);color:var(--ink-3);overflow-wrap:anywhere}
  .mcprow.fail .meta{color:#eda199}
  .mcprow .scope{font:700 9px/1 var(--mono);text-transform:uppercase;letter-spacing:.06em;
    border:1px solid var(--line-2);border-radius:5px;padding:3px 5px;color:var(--ink-3)}
  .mcprow .acts{display:flex;gap:6px;flex:none}
  .mcprow .acts button{background:transparent;border:1px solid var(--line-2);color:var(--ink-2);
    border-radius:7px;padding:6px 9px;font:600 10.5px/1 var(--mono);cursor:pointer;white-space:nowrap}
  .mcprow .acts button:hover{border-color:var(--acc-line);color:var(--acc-ink)}
  .mcprow2{display:grid;grid-template-columns:1fr 1fr;gap:10px}
  .logpre{margin:0;font-family:var(--mono);font-size:11.5px;line-height:1.6;white-space:pre-wrap;
    overflow-wrap:anywhere;color:#c7c9d6;background:var(--elev);border:1px solid var(--line);
    border-radius:9px;padding:11px 12px;flex:1;min-height:120px;overflow:auto}
  .logpre .bad{color:#eda199}
  .logpre .t{color:var(--ink-3)}
  #mcp-add{display:flex;flex-direction:column;gap:10px;margin-top:10px}
  .sheet select{background:var(--elev);border:1px solid var(--line-2);border-radius:9px;
    padding:11px 12px;color:var(--ink);font:500 13px/1 var(--sans)}
  @media (max-width:820px){ .mcprow{grid-template-columns:auto 1fr}
    .mcprow .acts{grid-column:2;justify-content:flex-start} .mcprow2{grid-template-columns:1fr} }
  .sheet .keys{font:500 11.5px/1.5 var(--sans)}
  .sheet .keys .kh{font:600 10px/1 var(--mono);letter-spacing:.12em;text-transform:uppercase;
    color:var(--ink-3);padding:6px 0 2px}
  .keygrid{display:grid;grid-template-columns:auto 1fr;gap:5px 12px;margin-top:8px}
  .keygrid b{font:600 11px/1.5 var(--mono);color:var(--acc-ink);white-space:nowrap}
  .keygrid span{color:var(--ink-2)}

  /* one control instead of a strip of tabs: which session you're in, and a way
     to switch. A scrolling tab strip never fit a phone and never will. */
  .sesspick{display:flex;align-items:center;gap:8px;min-width:0;flex:1;max-width:420px;
    background:var(--elev);border:1px solid var(--line-2);border-radius:9px;padding:9px 11px;
    color:var(--ink);font:600 12.5px/1 var(--mono);cursor:pointer}
  .sesspick:hover{border-color:#3a3a49}
  .sesspick #sess-name{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0;flex:1;text-align:left}
  .sesspick .car{color:var(--ink-3);flex:none}
  .sesspick .dot{width:7px;height:7px;flex:none}
  .sesslead{display:flex;align-items:center;gap:11px;width:100%;text-align:left;cursor:pointer;
    background:var(--acc-bg);border:1px solid var(--acc-line);border-radius:11px;padding:13px 14px;
    color:var(--ink);font:inherit}
  .sesslead:hover{filter:brightness(1.12)}
  .sesslead.on{background:var(--elev);border-color:var(--line-2)}
  .sesslead>div{flex:1;min-width:0}
  .sesslead .nm{font:600 13px/1.3 var(--sans);overflow-wrap:anywhere}
  .sesslead .sub{font:500 11.5px/1.4 var(--sans);color:var(--ink-2);margin-top:3px}
  .sesslead .go{font:600 11px/1 var(--mono);color:var(--acc-ink);flex:none}
  .sesslist{display:flex;flex-direction:column;gap:5px}
  .sesslist .chsect,.sessnew .chsect{padding:6px 2px 2px}
  .sessrow{display:flex;align-items:center;gap:9px;padding:11px 12px;border-radius:9px;
    border:1px solid var(--line);background:var(--elev);cursor:pointer;min-width:0}
  .sessrow:hover{border-color:var(--acc-line)}
  .sessrow.on{background:var(--acc-bg);border-color:var(--acc-line)}
  .sessrow .nm{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;
    font:600 12.5px/1.3 var(--mono);color:var(--ink)}
  .sessrow .kind{font:600 9.5px/1 var(--mono);text-transform:uppercase;letter-spacing:.07em;
    color:var(--ink-3);border:1px solid var(--line-2);border-radius:5px;padding:3px 5px;flex:none}
  .sessrow .k{background:0;border:0;color:var(--ink-3);cursor:pointer;font:600 13px/1 var(--sans);flex:none;padding:0 2px}
  .sessrow .k:hover{color:var(--bad)}
  .sessnew{display:flex;gap:6px;flex-wrap:wrap}
  .sessnew button{flex:1 0 auto;background:transparent;border:1px dashed var(--line-2);color:var(--ink-2);
    border-radius:9px;padding:11px 12px;cursor:pointer;font:600 12px/1 var(--mono)}
  .sessnew button:hover{border-color:var(--acc-line);color:var(--acc-ink)}
  .sheet h4 .c{font:500 11px/1 var(--mono);color:var(--ink-3);margin-left:8px}

  /* the dock is now a pane, not an overlay */
  .tdock{flex:1;min-height:0;height:auto;border-top:0;display:flex;flex-direction:column}
  .tdock.max{height:auto}
  .tgrip{display:none}

  /* new-work sheet */
  .modal{position:fixed;inset:0;background:#000a;z-index:60;display:none;align-items:center;justify-content:center;padding:16px}
  .modal.on{display:flex}
  /* A sheet grows with its contents — the MCP list plus an expanded add form
     ran off the bottom of the window, taking the buttons with it. Cap it and
     let it scroll instead. */
  .sheet{width:min(460px,100%);background:var(--panel);border:1px solid var(--line-2);border-radius:14px;
    padding:16px 18px;display:flex;flex-direction:column;gap:12px;box-shadow:0 30px 80px -30px #000;
    max-height:88dvh;overflow:hidden;overscroll-behavior:contain}
  /* The title and the buttons are flex rows that never scroll; only the middle
     does. Sticky inside a flex column didn't hold — the buttons ended up 1500px
     below the fold in a full sheet. */
  .sheet > h4,.sheet > .sheetbtns{flex:none}
  .sbody{flex:1;min-height:0;overflow:auto;overscroll-behavior:contain;
    display:flex;flex-direction:column;gap:12px}
  .sheet h4{margin:0;font:600 15px/1 var(--sans)}
  .sheet .hint{margin:0;color:var(--ink-3);font:500 12px/1.6 var(--sans)}
  .sheet .hint b{color:var(--ink-2)}
  .sheet details{font:500 11.5px/1.5 var(--sans)}
  .sheet summary{color:var(--ink-3);cursor:pointer;padding:2px 0}
  .sheet details input{margin-top:7px;width:100%;box-sizing:border-box}
  .sheet label{display:flex;flex-direction:column;gap:6px;font:600 11px/1 var(--mono);
    letter-spacing:.06em;text-transform:uppercase;color:var(--ink-3)}
  .sheet input,.sheet textarea{background:var(--elev);border:1px solid var(--line-2);border-radius:9px;
    padding:11px 12px;color:var(--ink);font:500 13.5px/1.5 var(--sans);outline:0;resize:vertical}
  .sheet input:focus,.sheet textarea:focus{border-color:var(--acc-line)}
  .sheetbtns{display:flex;gap:8px;justify-content:flex-end;margin-top:2px}
  .sheet .err{margin:0;color:#f2a49c;font:500 12.5px/1.5 var(--sans);
    background:#2a1614;border:1px solid #4a3130;border-radius:9px;padding:9px 11px}

  /* ── phones & tablets ─────────────────────────────────────────────────────
     One surface at a time, chosen by the bottom nav: the work list, the agent
     session, or the logs. Desktop keeps list + pane side by side. */
  .mobnav,.keybar{display:none}
  @media (max-width:820px){
    .shell{grid-template-columns:1fr}
    .worklist{border-right:0;display:none}
    .pane{display:none}
    body[data-view="work"] .worklist{display:flex}
    body[data-view="session"] .pane,body[data-view="changes"] .pane{display:flex}
    /* inside a session the terminal is the point: ＋ New work and the counters
       live on the work view, one tap away, so give their pixels to the shell */
    body[data-view="session"] .top,body[data-view="changes"] .top{display:none}
    /* one column: the file list, then the diff with a way back */
    .changes{grid-template-columns:1fr}
    .chdiff{display:none}
    .chlist{border-right:0}
    body.diffopen .chlist{display:none}
    body.diffopen .chdiff{display:block}
    .chdiff .hd .back{display:inline-flex}
    .mobnav{display:flex}
    .top{padding:8px 10px;gap:8px}
    .top .newwork{flex:none}
    .topstat{gap:9px;font-size:12.5px}
    .res{display:none}
    .workhead{padding:8px 10px;gap:7px}
    .workhead .wh-acts{width:100%;overflow-x:auto;scrollbar-width:none;margin-left:0}
    .workhead .wh-acts::-webkit-scrollbar{display:none}
    .workhead .b{flex:none;padding:10px 13px;font-size:14px}
    .workhead{flex-wrap:nowrap;overflow:hidden}
    .workhead .wh-br{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:46vw}
    .workhead .wh-meta{display:none}
    .panetabs{display:none}          /* the bottom nav switches panes here */
    .panetabs button{flex:1;padding:11px 8px}
    .wrow{padding:12px 10px}
    .wl-search input{font-size:15px}      /* iOS zooms anything under 16px… */
    .sheet input,.sheet textarea{font-size:16px}   /* …so form fields stay 16px */


    .thead{padding:7px 8px;gap:6px;overflow-x:auto;flex-wrap:nowrap;scrollbar-width:none}
    .thead::-webkit-scrollbar{display:none}
    .thead .tcwd{display:none}
    .ticon{flex:none;padding:10px 12px}
    .tright{margin-left:auto}
    .tpane{padding:4px 2px 2px 6px}
    .keybar{display:flex}
    .scrim{right:8px;bottom:calc(var(--navtotal) + 8px)}
    body{padding-bottom:var(--navtotal)}
  }
  @media (max-width:1100px) and (min-width:821px){
    .shell{grid-template-columns:280px minmax(0,1fr)}
    .res{display:none}                 /* the meters wrap before anything else */
  }

  :root{--navh:58px;--navtotal:calc(var(--navh) + env(safe-area-inset-bottom))}
  .mobnav{position:fixed;left:0;right:0;bottom:0;z-index:44;background:#101017;box-sizing:border-box;
    border-top:1px solid var(--line-2);height:var(--navtotal);
    padding:4px 8px env(safe-area-inset-bottom);gap:6px;align-items:stretch}
  .mobnav button{flex:1;background:0;border:0;color:var(--ink-3);font:600 10.5px/1.25 var(--sans);
    padding:5px 4px;border-radius:10px;display:flex;flex-direction:column;align-items:center;gap:3px;cursor:pointer}
  .mobnav button .ic{font-size:16px;line-height:1}
  .mobnav button.on{color:var(--acc-ink);background:var(--acc-bg)}
  .mobnav button:disabled{opacity:.4}
  .mobnav .badge{position:absolute;transform:translate(15px,-3px);background:var(--warn);
    color:#100c22;border-radius:999px;font:700 9px/1 var(--mono);padding:2px 5px}

  /* key bar: the keys a phone keyboard doesn't have */
  .keybar{flex:none;gap:5px;padding:6px 8px;border-top:1px solid var(--line);
    background:#101017;flex-wrap:wrap;justify-content:flex-start}
  .keybar button{flex:1 0 auto;min-width:40px;max-width:64px;height:38px;border-radius:9px;border:1px solid var(--line-2);
    background:var(--elev);color:var(--ink);font:600 12px/1 var(--mono);cursor:pointer;
    display:inline-flex;align-items:center;justify-content:center;-webkit-user-select:none;user-select:none}
  .keybar button:active{background:var(--acc-bg);border-color:var(--acc-line)}
  .keybar .sep{width:1px;background:var(--line-2);margin:4px 3px;flex:none}

  .toast{position:fixed;bottom:20px;left:50%;transform:translateX(-50%);background:var(--elev-2);
    border:1px solid var(--line-2);border-radius:10px;padding:11px 16px;color:var(--ink);font:500 13px/1 var(--sans);
    box-shadow:0 16px 40px -18px #000;z-index:50;opacity:0;transition:opacity .2s;pointer-events:none}
  .toast.on{opacity:1}
</style></head>
<body>
  <header class="top">
    <button class="b pri newwork" id="btn-new">＋ New work</button>
    <span class="topstat" id="topstat"></span>
    <span class="res" id="res" title="Resource footprint of the selected work's stack"></span>
    <button class="b" id="btn-mcp" title="MCP servers">◇ MCP</button>
    <button class="b" id="btn-log" title="This server's own log">▤ Log</button>
    <button class="b" id="btn-reload" title="Refresh">↻</button>
  </header>

  <main class="shell">
    <aside class="worklist" id="worklist">
      <div class="wl-search">
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="#63647a" stroke-width="2"><circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/></svg>
        <input id="wl-filter" placeholder="Filter work · ⌘K" autocomplete="off">
      </div>
      <div class="wl-body" id="wl-body"></div>
    </aside>

    <section class="pane">
      <div class="workhead" id="workhead"></div>
      <div class="panetabs" id="panetabs"></div>
      <div class="paneview">
        <section class="tdock on" id="tdock">
          <div class="thead">
            <button class="sesspick" id="sesspick"><span class="dot" id="sess-dot"></span><span id="sess-name">no session</span><span class="car">▾</span></button>
            <button class="ticon" id="sess-add" title="Start a session here">＋</button>
            <div class="tright">
              <button class="ticon" id="pane-keys" title="Keyboard shortcuts — ⌘⏎ for a newline">?</button>
              <button class="ticon" id="pane-full" title="Give the terminal the whole window">⤢</button>
            </div>
          </div>
          <div class="tbody" id="tbody"></div>
          <div class="keybar" id="keybar"></div>
        </section>

        <div class="changes" id="changes">
          <div class="chlist" id="chlist"></div>
          <div class="chdiff" id="chdiff"></div>
        </div>
      </div>
    </section>
  </main>

  <nav class="mobnav" id="mobnav">
    <button data-view="work"><span class="ic">▤</span>Work</button>
    <button data-view="session"><span class="ic">❯</span>Session</button>
    <button data-view="changes"><span class="ic">◧</span>Changes</button>
  </nav>

  <div class="modal" id="sessmodal">
    <div class="sheet" id="sesssheet">
      <h4>Sessions <span class="c" id="sess-where"></span></h4>
      <div class="sbody">
      <div id="sesslead"></div>
      <div class="sesslist" id="sesslist"></div>
      <div class="sessnew" id="sessnew"></div>
      <div class="keys"><div class="kh">Keyboard</div>
        <div class="keygrid">
          <b>⌘⏎</b><span>newline without submitting</span>
          <b>⌘← / ⌘→</b><span>start / end of line</span>
          <b>⌥← / ⌥→</b><span>move by word</span>
          <b>⌘⌫ / ⌥⌫</b><span>kill line / word</span>
          <b>⌃C / ⌃D</b><span>interrupt / end of input</span>
          <b>⌃`</b><span>jump to the session</span>
          <b>⌘K</b><span>filter work</span>
          <b>⌘N</b><span>new work</span>
        </div></div>
      </div>
      <div class="sheetbtns"><button type="button" class="b" id="sess-close">Close</button></div>
    </div>
  </div>

  <div class="modal" id="logmodal">
    <div class="sheet wide" id="logsheet">
      <h4>Server log <span class="c" id="log-count"></span>
        <button type="button" class="ticon" id="log-only" title="Show only errors">problems</button>
        <button type="button" class="ticon" id="log-refresh" title="Reload">↻</button></h4>
      <div class="sbody">
      <p class="hint">What this workmux process has written about itself since it started —
        the thing that used to exist only in the terminal you launched it from.</p>
      <pre class="logpre" id="log-body">loading…</pre>
      </div>
      <div class="sheetbtns"><button type="button" class="b" id="log-close">Close</button></div>
    </div>
  </div>

  <div class="modal" id="mcpmodal">
    <div class="sheet wide" id="mcpsheet">
      <h4>MCP servers <span class="c" id="mcp-count"></span>
        <button type="button" class="ticon" id="mcp-refresh" title="Re-check every server">↻</button></h4>
      <div class="sbody">
      <p class="hint">Registered servers and whether an agent can actually reach them. A server can be
        configured and still be invisible — its command not on <b>PATH</b>, or an OAuth round outstanding.</p>
      <div class="mcplist" id="mcp-list">checking…</div>
      <details id="mcp-add-wrap"><summary>Add a server</summary>
        <form id="mcp-add">
          <label>Name <input id="mcp-name" placeholder="grafana" autocomplete="off" required></label>
          <label>URL or command
            <input id="mcp-target" placeholder="https://mcp.example.com/mcp — or — /Users/you/go/bin/mcp-thing --flag" autocomplete="off" required></label>
          <div class="mcprow2">
            <label>Transport
              <select id="mcp-transport"><option value="">stdio (a command)</option><option value="http">http</option><option value="sse">sse</option></select></label>
            <label>Scope
              <select id="mcp-scope">
                <option value="user">user — every project</option>
                <option value="project">project — committed to .mcp.json</option>
                <option value="local">local — this checkout only</option>
              </select></label>
          </div>
          <label>Environment <input id="mcp-env" placeholder="KEY=value, OTHER=value" autocomplete="off"></label>
          <label>Headers <input id="mcp-headers" placeholder="Authorization: Bearer …" autocomplete="off"></label>
          <p class="err" id="mcp-err" hidden></p>
          <div class="sheetbtns"><button type="submit" class="b pri">Add</button></div>
        </form>
      </details>
      </div>
      <div class="sheetbtns"><button type="button" class="b" id="mcp-close">Close</button></div>
    </div>
  </div>

  <div class="modal" id="modal">
    <form class="sheet" id="newform">
      <h4>New work</h4>
      <p class="hint">Branches off <b id="nw-base">origin/main</b> into its own worktree and starts an agent there. The branch name comes from what you write. No containers are started — add the app later only if the change needs it.</p>
      <label>What should the agent do?
        <textarea id="nw-task" rows="5" placeholder="Cart totals ignore the discount when a coupon is applied twice — Rails only, no need to boot the app." autofocus></textarea></label>
      <details><summary>Name it yourself</summary>
        <input id="nw-name" placeholder="fix-cart-discount" autocomplete="off"></details>
      <p class="err" id="nw-err" hidden></p>
      <div class="sheetbtns">
        <button type="button" class="b" id="nw-cancel">Cancel</button>
        <button type="submit" class="b pri" id="nw-go">Create</button>
      </div>
    </form>
  </div>

  <div class="scrim" id="scrim">
    <div class="console">
      <h4><span class="dot live" id="con-dot"></span><span id="con-title">Working…</span><button class="x" id="con-x">✕</button></h4>
      <pre id="con-log"></pre>
      <div class="ftr" id="con-ftr">running…</div>
    </div>
  </div>
  <div class="toast" id="toast"></div>
  <div class="pop right" id="respop"></div>

<script>
"use strict";
const $ = s => document.querySelector(s);
const el = (t, c, x) => { const e = document.createElement(t); if (c) e.className = c; if (x != null) e.textContent = x; return e; };
const SVC_CLASS = n => n.startsWith("backend") ? "be" : n.startsWith("front") ? "fe" : n === "db" || n.includes("postgres") ? "db" : n === "traefik" ? "tr" : "sy";

let STATE = null, LOGS = [], FILTER = "", SVC = "", FOLLOW = true, ES = null;
let ACTIVE = null;          // selected stack slot (myapp1, myapp2, …)
const MAX = 4000;

// ---- work: the list IS the dashboard ----
// A piece of work is a worktree + its agents (+ a stack, only when the change
// needs the app running). Everything here is a view of /api/work: one poll, one
// ordering — what wants your attention first.
let WORK = [], SEL = null, AGENTS = [], WL_FILTER = "", LOGS_SLOT = null;
function stacks() { return WORK.filter(w => w.stack).map(w => w.stack); }
function selWork() { return WORK.find(w => w.path === SEL) || WORK[0] || null; }
function activeStack() { const w = selWork(); return (w && w.stack) || null; }
function isWorktreePath(p) { return !!p && WORK.some(w => p === w.path || p.startsWith(w.path + "/")); }

// Which session the selected work means. Liveness first, then recency — same
// order the server uses in agent_for_worktree(), which is what actually resolves
// kind=resume; this only decides the label.
const TEMPO_RANK = { blocked: 0, active: 1, idle: 2 };
function agentForTarget() {
  const w = selWork();
  return (w && w.agents[0]) || null;      // /api/work already ranks them
}
// Sessions are called "(agent)" until they earn a name; "❯ (agent)" says nothing.
// PR links follow the repo's own remote — this tool shouldn't know one repo's URL.
function prUrl(n) {
  const base = (STATE && STATE.repo_url) || "";
  return base ? base + "/pull/" + n : "";
}
const stackEnabled = () => !(STATE && STATE.stack_enabled === false);
function fmtAgo(ms) {
  const s = Math.max(0, (Date.now() - ms) / 1000);
  return s < 90 ? Math.round(s) + "s" : s < 5400 ? Math.round(s / 60) + "m"
       : s < 172800 ? Math.round(s / 3600) + "h" : Math.round(s / 86400) + "d";
}
function agentLabel(a) {
  const n = (a.name || "").trim();
  return n && n !== "(agent)" ? n : agentName() + " " + a.id;
}

async function refresh() {
  try {
    const d = await (await fetch("/api/work")).json();
    WORK = d.work || []; STATE = d; TERM_OK = d.terminal !== false;
    // No mcp command in workmux.json → no panel. It's the agent CLI's registry,
    // so without one there is nothing to show and nothing that could add to it.
    $("#btn-mcp").hidden = !AGENT().mcp;
    AGENTS = WORK.reduce((a, w) => a.concat(w.agents), []);
    if (!SEL || !WORK.some(w => w.path === SEL)) SEL = (WORK[0] || {}).path || null;
    syncStats();
    renderWork(); renderWorkHead(); renderSessionBar(); renderNav(); renderPaneTabs();
  } catch (e) { /* server gone; keep the last picture */ }
}

// Logs belong to a stack, and most work has none. Only (re)bind the stream when
// the selected work's stack actually changes, so a slow `docker compose ls`
// can't wipe the view mid-poll.
// Nothing to rebind now that logs are just another session — only the meters
// follow the selection.
function syncStats() {
  const slot = activeStack() ? activeStack().slot : null;
  if (slot === LOGS_SLOT) return;
  LOGS_SLOT = slot; ACTIVE = slot;
  if (slot) pollStats(); else renderRes({});
}

function selectWork(path, quiet) {
  if (!path || path === SEL) { if (!quiet && NARROW()) setView("session"); return; }
  SEL = path;
  syncStats();
  CHANGES = null; CHFILE = null; document.body.classList.remove("diffopen");
  renderWork(); renderWorkHead(); renderSessionBar(); renderNav(); renderPaneTabs();
  if (document.body.dataset.pane === "changes") loadChanges();
  followTerm();                       // bring this work's session forward
  if (!quiet && NARROW()) setView("session");
}

// ---- the work list ----
// Attention only. "App running" was its own group, so a worktree with a running
// app landed under "Working" when it also had a live agent and under "App
// running" when it didn't — the same state in two places depending on something
// unrelated. App state is a chip on every row instead.
const WROWS = [
  [0, "Needs you"], [1, "Running"], [2, "Recent"], [3, "Recent"],
  [4, "Worktrees"], [5, "Worktrees"], [6, "Base checkout"],
];
function wmatch(w) {
  if (!WL_FILTER) return true;
  const hay = [w.branch, w.dir, w.pr && ("#" + w.pr.number), w.pr && w.pr.title,
               ...w.agents.map(a => a.name + " " + a.detail)].join(" ").toLowerCase();
  return hay.includes(WL_FILTER);
}
function workRow(w) {
  const a = w.agents[0];
  // "running" comes from a live agent process here; state.json's tempo only
  // updates when a turn ends, so the session you're watching reads idle.
  const busy = w.live || (a && a.tempo === "active");
  const need = a && a.tempo === "blocked";
  const row = el("div", "wrow" + (w.path === SEL ? " sel" : "") + (a || busy ? "" : " dim"));
  row.appendChild(el("span", "wdot " + (need ? "blocked" : busy ? "active" : w.stack ? "up" : "")));
  const mid = el("div");
  mid.appendChild(el("div", "wbr", w.branch === "(detached)" ? w.dir : w.branch));
  const sub = el("div", "wsub");
  const prs = w.prs || (w.pr ? [w.pr.number] : []);
  if (prs.length) {
    sub.appendChild(el("span", "pr", "#" + prs[0]));
    if (prs.length > 1) {
      const more = el("span", "pr", "+" + (prs.length - 1));
      more.title = "Also " + prs.slice(1).map(n => "#" + n).join(", ");
      sub.appendChild(more);
    }
  }
  // Say the app's state outright on every row. Inferring "not running" from a
  // missing chip meant you couldn't tell at a glance, least of all on a phone.
  if (stackEnabled())
    sub.appendChild(el("span", w.stack ? "slot" : "off", w.stack ? "● app " + w.stack.slot : "○ app off"));
  if (need) sub.appendChild(el("span", "need", "needs input"));
  else if (busy) sub.appendChild(el("span", "run", "running"));
  if (w.agents.length > 1) sub.appendChild(el("span", null, w.agents.length + " agents"));
  if (w.sessions.length) sub.appendChild(el("span", null, w.sessions.length + " session" + (w.sessions.length > 1 ? "s" : "")));
  if (w.is_default) sub.appendChild(el("span", null, "primary"));
  mid.appendChild(sub);
  // The agent's own name, not just its latest line: it's how you recognise a
  // piece of work you started days ago.
  if (a) {
    const who = el("div", "wwho");
    who.appendChild(el("span", "nm", agentLabel(a)));
    if (a.detail) who.appendChild(el("span", "dt", a.detail));
    mid.appendChild(who);
  }
  row.appendChild(mid);

  const side = el("div", "wside");
  if (w.activity) {
    const ago = el("span", "wago", fmtAgo(w.activity * 1000));
    ago.title = "Last activity " + new Date(w.activity * 1000).toLocaleString();
    side.appendChild(ago);
  }
  side.appendChild(el("span", "wgo", "open ▸"));
  row.appendChild(side);
  row.onclick = () => selectWork(w.path);
  return row;
}
function renderWork() {
  const c = $("#wl-body"); c.innerHTML = "";
  const shown = WORK.filter(wmatch);
  let last = null;
  shown.forEach(w => {
    const [, label] = WROWS.find(([r]) => r === w.rank) || [0, "Worktrees"];
    if (label !== last) { c.appendChild(el("div", "wl-sect", label)); last = label; }
    c.appendChild(workRow(w));
  });
  if (!shown.length) c.appendChild(el("div", "empty", WL_FILTER ? "Nothing matches." : "No worktrees yet."));

  // PRs that aren't checked out anywhere — starting one *does* boot a stack,
  // so it's labelled differently from ＋ New work.
  const prs = ((STATE && STATE.open_prs) || []).filter(p =>
    !WL_FILTER || (p.title + " #" + p.number).toLowerCase().includes(WL_FILTER));
  if (prs.length) {
    c.appendChild(el("div", "wl-sect", "Open PRs · not checked out"));
    prs.forEach(p => {
      const row = el("div", "wrow prrow dim");
      row.appendChild(el("span", "wdot"));
      const mid = el("div");
      mid.appendChild(el("div", "wbr", p.title || p.branch));
      const sub = el("div", "wsub");
      sub.append(el("span", "pr", "#" + p.number), el("span", null, p.author || ""));
      mid.appendChild(sub); row.appendChild(mid);
      row.appendChild(el("span", "wgo", "check out ▸"));
      row.onclick = () => {
        if (!confirm("Check out PR #" + p.number + " and start its stack?\n\nThis boots containers — for a change that doesn't need the app, use ＋ New work instead.")) return;
        doAction({ action: "pr", ref: p.number }, "Starting PR #" + p.number);
      };
      c.appendChild(row);
    });
  }
  const need = AGENTS.filter(a => a.tempo === "blocked").length;
  const busy = AGENTS.filter(a => a.tempo === "active").length;
  const up = stacks().length;
  // Name the slots, don't just count them: "2 stacks up" gave no way to know
  // slot 1 was taken, so the next ▶ Start app landing on slot 2 looked like a bug.
  const upWhere = WORK.filter(w => w.stack).map(w => w.stack.slot + " → " + w.dir).join("\n");
  const st = $("#topstat"); st.innerHTML = "";
  // Narrow screens get glyphs, not sentences: "1 need input · 1 stack up" wrapped
  // the bar into four lines at 393px, or got clipped mid-word.
  if (NARROW()) {
    st.append(el("span", null, WORK.length + "▤"));
    if (busy) st.appendChild(el("span", "w", busy + "▶"));
    if (need) st.appendChild(el("span", "n", need + "⚠"));
    if (up) {
      const chip = el("span", null, up + "●");
      chip.title = "Apps up:\n" + upWhere;
      st.appendChild(chip);
    }
    st.title = WORK.length + " worktrees · " + busy + " working · " + need + " need input";
    return;
  }
  st.append(el("span", null, WORK.length + " worktrees"));
  if (busy) st.appendChild(el("span", "w", busy + " working"));
  if (need) st.appendChild(el("span", "n", need + " need input"));
  if (up && stackEnabled()) {
    const chip = el("span", null, "● " + WORK.filter(w => w.stack).map(w => w.stack.slot).join(" ● "));
    chip.title = "Apps up:\n" + upWhere;
    st.appendChild(chip);
  }
}
$("#wl-filter").addEventListener("input", e => { WL_FILTER = e.target.value.toLowerCase().trim(); renderWork(); });

// ---- the selected work: header + its actions ----
function upWhereHead() {
  return WORK.filter(w => w.stack).map(w => w.stack.slot + " → " + w.dir).join("\n");
}
function renderWorkHead() {
  const h = $("#workhead"); h.innerHTML = "";
  const w = selWork();
  if (!w) { h.appendChild(el("span", "wh-meta", "No worktrees")); return; }
  h.appendChild(el("span", "wh-br", w.branch === "(detached)" ? w.dir : w.branch));
  const meta = el("span", "wh-meta");
  (w.prs || (w.pr ? [w.pr.number] : [])).forEach((n, i) => {
    const a = el("a", null, (i ? "" : "PR ") + "#" + n);
    a.href = prUrl(n); a.target = "_blank";
    a.title = i ? "Opened from this work" : "This branch's PR";
    meta.appendChild(a);
  });
  if (w.behind) meta.appendChild(el("span", null, "↓" + w.behind));
  if (w.ahead) meta.appendChild(el("span", null, "↑" + w.ahead));
  meta.appendChild(el("span", null, w.dir));
  h.appendChild(meta);

  // On a phone these are icons: five labelled buttons wrapped onto two rows and
  // cost ~190px of the screen the terminal wants. The title carries the meaning.
  const tight = NARROW();
  const acts = el("div", "wh-acts");
  const noStack = !stackEnabled();
  const btn = (icon, label, title, fn, cls) => {
    const b = el("button", "b" + (cls ? " " + cls : ""), tight ? icon : icon + " " + label);
    b.title = (tight ? label + " — " : "") + (title || ""); b.onclick = fn;
    acts.appendChild(b); return b;
  };
  const base = w.base || (STATE && STATE.base) || "main";
  if (noStack) {
    /* nothing to boot: this project has no compose file (or says stack: null) */
  } else if (w.stack) {
    if (w.stack.url)
      btn("↗", "Open " + w.stack.slot, "Open " + w.stack.url, () => window.open(w.stack.url, "_blank"));
    btn("↻", "Restart", "Restart this stack", () =>
      doAction({ action: "restart", stack: w.stack.slot }, "Restarting " + w.stack.slot));
    btn("◻", "Stop", "Stop the containers", () => {
      if (confirm("Stop stack " + w.stack.slot + "?")) doAction({ action: "stop", stack: w.stack.slot }, "Stopping " + w.stack.slot);
    }, "danger");
  } else {
    // Which slot this will land on, before you press it. Several stacks run side
    // by side, so the answer isn't always the first slot — and silently getting
    // the second reads as a bug when nothing said the first was busy.
    const slot = (STATE && STATE.next_slot) || "";
    const b = el("button", "b pri", tight ? "▶ " + slot : "▶ Start app · " + slot);
    b.title = "Boot containers for this worktree as " + slot
      + (upWhereHead() ? "\n\nAlready up:\n" + upWhereHead() : "")
      + "\n\nOnly needed when the change requires the app.";
    b.onclick = () => doAction({ action: "up", target: w.path },
                               "Starting " + slot + " on " + w.dir);
    acts.appendChild(b);
  }
  if (w.behind) {
    btn("⇣" + w.behind, "from " + base,
        "Merge origin/" + base + " in — this branch's base"
        + (w.pr ? " (PR #" + w.pr.number + " targets it)" : ""), () => {
      if (confirm("Merge origin/" + base + " into " + w.branch + "?"))
        doAction({ action: "update", stack: (w.stack || {}).slot || "", target: w.path,
                   base: base }, "Updating " + w.dir + " from " + base);
    });
  }
  btn("⧉", "Copy path", "Copy this worktree's path", () => {
    navigator.clipboard && navigator.clipboard.writeText(w.path); toast("Copied " + w.path);
  });
  h.appendChild(acts);
}

// ---- this server's own log ----
let LOG_ONLY = "";
const logmodal = $("#logmodal");
function closeLog() { logmodal.classList.remove("on"); }
async function openLog() {
  logmodal.classList.add("on");
  const body = $("#log-body");
  try {
    const d = await (await fetch("/api/serverlog" + (LOG_ONLY ? "?only=" + LOG_ONLY : ""))).json();
    body.innerHTML = "";
    $("#log-count").textContent = d.lines.length + " of " + d.total + (LOG_ONLY ? " · problems only" : "");
    if (!d.lines.length) { body.appendChild(el("span", "t", "Nothing logged yet.")); return; }
    d.lines.forEach(r => {
      const bad = /error|traceback|exception|✗|failed|refused/i.test(r.line);
      const t = new Date(r.t * 1000).toTimeString().slice(0, 8);
      const row = el("span", bad ? "bad" : null, t + "  " + r.line + "\n");
      body.appendChild(row);
    });
    body.scrollTop = body.scrollHeight;
  } catch (e) { body.textContent = "Couldn't reach the server."; }
}
$("#btn-log").onclick = () => openLog();
$("#log-close").onclick = closeLog;
$("#log-refresh").onclick = () => openLog();
$("#log-only").onclick = () => {
  LOG_ONLY = LOG_ONLY ? "" : "problems";
  $("#log-only").textContent = LOG_ONLY ? "all lines" : "problems";
  openLog();
};
logmodal.addEventListener("click", e => { if (e.target === logmodal) closeLog(); });

// ---- MCP servers ----
// Registration is not reachability: a server can be configured and still be
// invisible to every agent because its command isn't on PATH or an OAuth round
// is outstanding. Both were silent, which is how a working grafana config sat
// dead. So: state and reason first, then the actions that fix each state.
let MCP = null;
const mcpmodal = $("#mcpmodal");
function closeMcp() { mcpmodal.classList.remove("on"); }
async function openMcp(refresh) {
  mcpmodal.classList.add("on");
  if (!MCP || refresh) {
    $("#mcp-list").textContent = "checking every server…";
    try { MCP = await (await fetch("/api/mcp" + (refresh ? "?refresh=1" : ""))).json(); }
    catch (e) { $("#mcp-list").textContent = "Couldn't reach the server."; return; }
  }
  renderMcp();
}
function renderMcp() {
  const box = $("#mcp-list"); box.innerHTML = "";
  const list = (MCP && MCP.servers) || [];
  const n = s => list.filter(x => x.state === s).length;
  $("#mcp-count").textContent = list.length + " · " + n("ok") + " reachable · "
    + n("auth") + " need auth" + (n("fail") ? " · " + n("fail") + " broken" : "");
  list.forEach(x => {
    const row = el("div", "mcprow " + x.state);
    row.appendChild(el("span", "dot"));
    const mid = el("div");
    const top = el("div"); top.style.cssText = "display:flex;gap:8px;align-items:baseline;flex-wrap:wrap";
    top.appendChild(el("span", "nm", x.name));
    if (x.scope) top.appendChild(el("span", "scope", x.scope));
    mid.appendChild(top);
    mid.appendChild(el("div", "meta", x.detail + (x.target ? "  ·  " + x.target : "")));
    row.appendChild(mid);
    const acts = el("div", "acts");
    if (x.suggest) {
      const f = el("button", null, "fix path");
      f.title = "Re-add " + x.name + " pointing at " + x.suggest
        + "\n(its command isn't on the PATH agents use)";
      f.onclick = async () => {
        toast("Re-adding " + x.name + " at " + x.suggest);
        const res = await fetch("/api/mcp/fixpath", { method: "POST",
          headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name: x.name }) });
        const j = await res.json();
        toast(res.ok ? "Fixed " + x.name : "✗ " + (j.error || res.status));
        openMcp(true);
      };
      acts.appendChild(f);
    }
    if (x.state === "auth" || x.state === "pending") {
      const b = el("button", null, "authenticate");
      b.title = "Opens an agent session here and runs /mcp, where the OAuth round happens";
      b.onclick = () => authMcp(x.name);
      acts.appendChild(b);
    }
    if (x.scope && !x.scope.startsWith("local:")) {
      const r = el("button", null, "remove");
      r.onclick = async () => {
        if (!confirm("Remove MCP server " + x.name + " (" + x.scope + ")?")) return;
        const res = await fetch("/api/mcp/remove", { method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ name: x.name, scope: x.scope }) });
        const j = await res.json();
        toast(res.ok ? "Removed " + x.name : "✗ " + (j.error || res.status));
        openMcp(true);
      };
      acts.appendChild(r);
    } else if (x.scope.startsWith("local:")) {
      const r = el("button", null, "remove");
      r.onclick = async () => {
        if (!confirm("Remove MCP server " + x.name + " from this checkout?")) return;
        const res = await fetch("/api/mcp/remove", { method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ name: x.name, scope: "local" }) });
        const j = await res.json();
        toast(res.ok ? "Removed " + x.name : "✗ " + (j.error || res.status));
        openMcp(true);
      };
      acts.appendChild(r);
    }
    row.appendChild(acts);
    box.appendChild(row);
  });
  if (!list.length) box.appendChild(el("p", "hint", "No MCP servers registered."));
}
// The OAuth round only exists inside a session, so open one and run /mcp in it.
async function authMcp(name) {
  closeMcp();
  if (NARROW()) setView("session"); else setPane("session");
  toast("Opening a session for /mcp — approve " + name + " there");
  await newTerm("agent");
  const p = TPANES.get(TACTIVE);
  if (!p) return;
  setTimeout(() => wsSend(p, new TextEncoder().encode("/mcp\r")), 4500);
}
$("#btn-mcp").onclick = () => openMcp(false);
$("#mcp-close").onclick = closeMcp;
$("#mcp-refresh").onclick = () => openMcp(true);
mcpmodal.addEventListener("click", e => { if (e.target === mcpmodal) closeMcp(); });
$("#mcp-add").addEventListener("submit", async e => {
  e.preventDefault();
  const split = v => v.split(",").map(x => x.trim()).filter(Boolean);
  const body = {
    name: $("#mcp-name").value.trim(), target: $("#mcp-target").value.trim(),
    transport: $("#mcp-transport").value, scope: $("#mcp-scope").value,
    env: split($("#mcp-env").value), headers: split($("#mcp-headers").value),
  };
  const err = $("#mcp-err"); err.hidden = true;
  try {
    const res = await fetch("/api/mcp/add", { method: "POST",
      headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    const j = await res.json();
    if (!res.ok) { err.textContent = j.error || ("HTTP " + res.status); err.hidden = false; return; }
    toast("Added " + body.name);
    $("#mcp-name").value = ""; $("#mcp-target").value = "";
    $("#mcp-add-wrap").open = false;
    openMcp(true);
  } catch (ex) { err.textContent = ex.message; err.hidden = false; }
});

// ---- changes: what this work has done ----
// Same information a git TUI shows, laid out by the browser: a PTY grid at 400px
// wide is unreadable, and it can't scroll a diff independently of a file list.
let CHANGES = null, CHFILE = null, CHTIMER = null;

function setPane(p) {
  document.body.dataset.pane = p;
  // On a phone the nav is the switcher, so its highlight has to follow the pane
  // — they were drifting apart (nav on Session while the pane showed Changes).
  if (NARROW() && document.body.dataset.view !== "work") {
    document.body.dataset.view = p;
    renderNav();
  }
  renderPaneTabs();
  clearTimeout(CHTIMER);
  if (p === "session") { toggleTerm(true).then(() => setTimeout(refitActive, 60)); }
  else {
    // Entering the pane starts at the file list: on a phone the two are the same
    // column, and arriving on whichever diff you last read is disorienting.
    document.body.classList.remove("diffopen");
    loadChanges();
  }
}
function renderPaneTabs() {
  const c = $("#panetabs"); c.innerHTML = "";
  const w = selWork(), pane = document.body.dataset.pane || "session";
  const mk = (id, label, count) => {
    const b = el("button", pane === id ? "on" : null);
    b.appendChild(document.createTextNode(label));
    if (count) b.appendChild(el("span", "cnt", String(count)));
    b.onclick = () => setPane(id);
    c.appendChild(b);
  };
  mk("session", "❯ Session", w ? w.sessions.length : 0);
  mk("changes", "◧ Changes", CHANGES ? CHANGES.files.length : 0);
}

async function loadChanges(quiet) {
  const w = selWork();
  if (!w) return;
  try {
    const d = await (await fetch("/api/changes?path=" + encodeURIComponent(w.path))).json();
    if (d.error) return;
    CHANGES = d; renderChanges(); renderPaneTabs();
  } catch (e) { /* keep what we have */ }
  clearTimeout(CHTIMER);
  if (document.body.dataset.pane === "changes")
    CHTIMER = setTimeout(() => loadChanges(true), 6000);   // agents commit as they go
}

function renderChanges() {
  const box = $("#chlist"); box.innerHTML = "";
  if (!CHANGES) return;
  const files = CHANGES.files, commits = CHANGES.commits;
  const head = el("div", "chsect");
  head.append(document.createTextNode("Working tree"), el("span", "n", files.length + " file" + (files.length === 1 ? "" : "s")));
  box.appendChild(head);
  if (!files.length) box.appendChild(el("div", "empty", "Nothing uncommitted."));
  files.forEach(f => {
    const code = f.untracked ? "U" : (f.x !== " " ? f.x : f.y);
    const row = el("div", "chrow" + (CHFILE === f.path ? " on" : ""));
    row.appendChild(el("span", "st " + code, code + (f.staged && !f.untracked ? "·" : "")));
    row.appendChild(el("span", "fp", f.path));
    if (f.add || f.del) {
      const n = el("span", "num");
      if (f.add) n.appendChild(el("span", "a", "+" + f.add));
      if (f.del) n.appendChild(el("span", "d", " −" + f.del));
      row.appendChild(n);
    }
    row.onclick = () => showDiff(f);
    box.appendChild(row);
  });
  if (commits.length) {
    const w2 = selWork();
    const h2 = el("div", "chsect");
    h2.append(document.createTextNode("Commits ahead of " + ((CHANGES.base || (w2 && w2.base)) || "base")),
              el("span", "n", String(commits.length)));
    box.appendChild(h2);
    commits.forEach(c => {
      const row = el("div", "chcommit" + (CHFILE === c.sha ? " on" : ""));
      row.append(el("span", "sha", c.sha.slice(0, 9)), el("span", "msg", c.msg));
      if (!c.pushed) row.appendChild(el("span", "local", "local"));
      row.title = c.msg + "\n\n" + (c.pushed ? "pushed" : "not pushed yet");
      row.onclick = () => showCommit(c);            // its diff, like any file
      box.appendChild(row);
    });
  }
}

function showCommit(c) {
  return renderDiffPane(c.sha, c.sha.slice(0, 9) + "  " + c.msg,
                        "commit=" + encodeURIComponent(c.sha));
}
async function showDiff(f) {
  return renderDiffPane(f.path, f.path,
                        "file=" + encodeURIComponent(f.path) + "&staged=" + (f.staged ? "1" : "0"));
}
async function renderDiffPane(key, title, query) {
  CHFILE = key; renderChanges();
  document.body.classList.add("diffopen");
  const host = $("#chdiff"); host.innerHTML = "";
  const hd = el("div", "hd");
  const back = el("button", "back", "‹ files");
  back.onclick = () => { document.body.classList.remove("diffopen"); CHFILE = null; renderChanges(); };
  hd.append(back, el("span", null, title));
  host.appendChild(hd);
  const pre = el("pre"); pre.appendChild(el("span", "l meta", "loading…"));
  host.appendChild(pre);
  const w = selWork();
  try {
    const d = await (await fetch("/api/diff?path=" + encodeURIComponent(w.path) + "&" + query)).json();
    pre.innerHTML = "";
    const lines = (d.diff || "").split("\n");
    if (!d.diff) { pre.appendChild(el("span", "l meta", "No textual diff (binary, or nothing changed).")); return; }
    lines.forEach(line => {
      const cls = line.startsWith("+++") || line.startsWith("---") || line.startsWith("diff ")
                || line.startsWith("index ") || line.startsWith("new file") || line.startsWith("deleted file")
          ? "meta"
          : line.startsWith("@@") ? "hunk"
          : line.startsWith("+") ? "add"
          : line.startsWith("-") ? "del" : "";
      pre.appendChild(el("span", "l " + cls, line || " "));
    });
  } catch (e) {
    pre.innerHTML = ""; pre.appendChild(el("span", "l del", "Couldn't load the diff: " + e.message));
  }
}

// ---- new work: a branch, a worktree and an agent — no containers ----
const modal = $("#modal");
function openNew() {
  $("#nw-base").textContent = "origin/" + ((STATE && STATE.base) || "main");
  $("#nw-name").value = ""; $("#nw-task").value = ""; showNewErr("");
  modal.classList.add("on");
  setTimeout(() => $("#nw-task").focus(), 30);
}
function closeNew() { modal.classList.remove("on"); }
function showNewErr(msg) {
  const e = $("#nw-err"); e.textContent = msg || ""; e.hidden = !msg;
}
$("#btn-new").onclick = openNew;
$("#nw-cancel").onclick = closeNew;
modal.addEventListener("click", e => { if (e.target === modal) closeNew(); });
$("#newform").addEventListener("submit", async e => {
  e.preventDefault();
  const name = $("#nw-name").value.trim(), task = $("#nw-task").value.trim();
  if (!task && !name) return showNewErr("Say what the agent should do.");
  const go = $("#nw-go"); go.disabled = true; go.textContent = "Creating…";
  try {
    const r = await fetch("/api/work/new", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name, prompt: task }),
    });
    const j = await r.json();
    if (!r.ok) { showNewErr(j.error || ("HTTP " + r.status)); return; }
    closeNew();
    toast("Created " + j.dir + (j.agent_starting ? " · starting an agent…" : ""));
    await refresh();
    selectWork(j.path);
    if (j.agent_starting) {
      // The agent is booting behind the response; open its session as soon as
      // /api/work reports it rather than making you watch for it.
      const path = j.path, until = Date.now() + 120000;
      const poll = async () => {
        if (Date.now() > until || SEL !== path) return;
        const w = WORK.find(x => x.path === path);
        if (w && w.agents.length) { newTerm("resume"); return; }
        await refresh(); setTimeout(poll, 2000);
      };
      setTimeout(poll, 1500);
    }
  } catch (err) { showNewErr(err.message); }
  finally { go.disabled = false; go.textContent = "Create"; }
});
$("#btn-reload").onclick = () => { refresh(); toast("Refreshed"); };

// ---- resource footprint (CPU / memory) ----
let STATS = null, RES = null;
function fmtBytes(b) { if (!b) return "0"; const u = ["B", "KiB", "MiB", "GiB", "TiB"]; let i = 0; while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; } return (b < 10 && i > 0 ? b.toFixed(1) : Math.round(b)) + " " + u[i]; }
async function pollStats() { try { const r = await fetch("/api/stats?stack=" + encodeURIComponent(ACTIVE || "")); renderRes(await r.json()); } catch (e) { /* stack down/restarting */ } }
function meterHtml(label, val, pct) { const hot = pct > 85 ? " hot" : ""; return `<span class="meter${hot}"><span class="mlabel">${label}</span><span class="mbar"><i style="width:${Math.round(pct)}%"></i></span><span class="mval">${val}</span></span>`; }
function renderRes(s) {
  const res = $("#res");
  if (!activeStack() || !s.services || !s.services.length) { res.innerHTML = ""; STATS = null; RES = null; toggleRespop(false); return; }
  RES = s; STATS = {}; s.services.forEach(x => STATS[x.name] = x);
  const cpuPct = s.cpu_count ? Math.min(100, s.cpu_total / (s.cpu_count * 100) * 100) : 0;
  const memPct = s.mem_limit ? Math.min(100, s.mem_used / s.mem_limit * 100) : 0;
  res.innerHTML = meterHtml("CPU", Math.round(s.cpu_total) + "%", cpuPct) + meterHtml("MEM", fmtBytes(s.mem_used), memPct);
  if (respopOn()) renderRespop();
}
const respop = $("#respop");
function respopOn() { return respop.classList.contains("on"); }
function toggleRespop(on) { const show = on == null ? !respopOn() : on; respop.classList.toggle("on", show); if (show) renderRespop(); }
function renderRespop() {
  if (!RES) { respop.innerHTML = ""; return; }
  const maxMem = Math.max(1, ...RES.services.map(x => x.mem));
  let html = `<div class="rtot"><span>CPU <b>${Math.round(RES.cpu_total)}%</b> <span style="color:var(--ink-3)">/ ${RES.cpu_count} cores</span></span><span>MEM <b>${fmtBytes(RES.mem_used)}</b> <span style="color:var(--ink-3)">/ ${fmtBytes(RES.mem_limit)}</span></span></div>`;
  html += `<div class="sect">Per service · by memory</div>`;
  RES.services.forEach(x => {
    const w = Math.round(x.mem / maxMem * 100);
    html += `<div class="rrow"><span class="rn">${x.name}</span><span class="rbar"><i style="width:${w}%"></i></span><span class="rv">${x.cpu}% · ${fmtBytes(x.mem)}</span></div>`;
  });
  respop.innerHTML = html;
}
$("#res").addEventListener("click", e => { e.stopPropagation(); if (RES) toggleRespop(); });

// ---- action console ----
// Per-slot actions: several stacks can act at once (e.g. stop trip2 + trip3).
let WORKING = {};       // slot -> true while an action runs (drives tab spinners)
let SLOT_LOG = {};      // slot -> accumulated log lines
let CONSOLE_SLOT = null; // which slot's log the floating console shows
function openConsole() { $("#scrim").classList.add("on"); }
function renderConsole(slot) {
  const lines = SLOT_LOG[slot] || [], log = $("#con-log");
  log.textContent = lines.join("\n") + (lines.length ? "\n" : "");
  log.scrollTop = log.scrollHeight;
}
function viewConsole(slot) {
  CONSOLE_SLOT = slot;
  $("#con-title").textContent = (WORKING[slot] ? "Working — " : "") + slot;
  renderConsole(slot); openConsole();
}
function doAction(payload, title) {
  let slot = payload.stack || (STATE && STATE.next_slot); if (!slot) return;
  SLOT_LOG[slot] = []; WORKING[slot] = true; CONSOLE_SLOT = slot;
  $("#con-title").textContent = title; $("#con-dot").className = "dot live"; $("#con-ftr").textContent = "running…";
  renderConsole(slot); openConsole(); renderWork();
  fetch("/api/action", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) })
    .then(async res => {
      const j = await res.json().catch(() => ({}));
      if (!res.ok) {
        SLOT_LOG[slot].push("✗ " + (j.error || ("HTTP " + res.status))); delete WORKING[slot];
        if (CONSOLE_SLOT === slot) { renderConsole(slot); $("#con-ftr").textContent = "✗ not started"; }
        renderWork(); return;
      }
      if (j.slot && j.slot !== slot) {   // server assigned a different slot (spawn)
        WORKING[j.slot] = true; SLOT_LOG[j.slot] = SLOT_LOG[slot];
        delete WORKING[slot]; if (CONSOLE_SLOT === slot) CONSOLE_SLOT = j.slot; slot = j.slot; renderWork();
      }
      pollSlot(slot, 0, false);
    })
    .catch(err => {
      SLOT_LOG[slot].push("✗ " + err.message); delete WORKING[slot];
      if (CONSOLE_SLOT === slot) { renderConsole(slot); $("#con-ftr").textContent = "✗ failed"; }
      renderWork();
    });
}
// Poll one slot's detached action. Reconnects across the Traefik blip a
// switch/restart causes; the action finishes on the host regardless.
function pollSlot(slot, cursor, reconnecting) {
  fetch("/api/action/status?slot=" + encodeURIComponent(slot) + "&since=" + cursor)
    .then(r => r.json())
    .then(s => {
      if (reconnecting) SLOT_LOG[slot].push("… reconnected");
      if (s.lines && s.lines.length) { SLOT_LOG[slot].push(...s.lines); if (CONSOLE_SLOT === slot) renderConsole(slot); }
      if (s.running) { setTimeout(() => pollSlot(slot, s.total, false), 800); return; }
      if (CONSOLE_SLOT === slot) {
        $("#con-dot").className = "dot " + (s.code === 0 ? "ok" : "bad");
        $("#con-ftr").textContent = s.code === 0 ? "✓ done — Esc/✕ to dismiss" : "✗ exited (" + s.code + ")";
      }
      // Clear the working flag only AFTER the refreshed state (with the now-running
      // stack) arrives — otherwise the tab briefly belongs to neither WORKING nor
      // the stack list and vanishes for a frame.
      refresh().then(() => {
        delete WORKING[slot];
        if (s.code === 0 && stacks().some(x => x.slot === slot)) ACTIVE = slot;
        renderWork(); syncStats();
      });
    })
    .catch(() => {
      if (!reconnecting && CONSOLE_SLOT === slot) { SLOT_LOG[slot].push("… stack restarting — reconnecting …"); renderConsole(slot); $("#con-ftr").textContent = "reconnecting…"; }
      setTimeout(() => pollSlot(slot, cursor, true), 1500);
    });
}
function closeConsole() { $("#scrim").classList.remove("on"); }
$("#con-x").onclick = closeConsole;
$("#scrim").addEventListener("click", e => { if (e.target === $("#scrim")) closeConsole(); });

// ---- toast ----
let toastT = null;
function toast(msg) { const t = $("#toast"); t.textContent = msg; t.classList.add("on"); clearTimeout(toastT); toastT = setTimeout(() => t.classList.remove("on"), 2400); }

async function spawnAgent(w) {
  const task = prompt("Spawn a background agent in " + w.dir + "\n(" + w.branch + ")\n\nWhat should it do?");
  if (!task || !task.trim()) return;
  toast("Starting an agent in " + w.dir + "…");
  try {
    const r = await fetch("/api/agents/new", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ target: w.path, prompt: task }),
    });
    const j = await r.json();
    if (!r.ok) return toast("✗ " + (j.error || ("HTTP " + r.status)));
    toast("Agent " + (j.id || "") + " started in " + w.dir);
    refresh();
  } catch (e) { toast("✗ " + e.message); }
}

// Take over an agent's session in the dock — same as attaching in the CLI.
async function attachAgent(a) {
  if (TPENDING) return;
  await toggleTerm(true);
  const dir = isWorktreePath(a.worktree) ? a.worktree : a.cwd;
  TPENDING = { label: agentLabel(a), dir: dir ? dir.split("/").pop() : "" };
  renderSessionBar();
  try {
    const r = await fetch("/api/term/new", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ kind: "attach", agent: a.id, cwd: isWorktreePath(dir) ? dir : undefined }),
    });
    const j = await r.json();
    if (!r.ok) return toast("✗ " + (j.error || ("HTTP " + r.status)));
    TSESS = TSESS.concat([j]);
    await attachSession(j);
  } catch (e) {
    toast("✗ " + e.message);
  } finally {
    TPENDING = null;
    renderSessionBar();
  }
}

// ---- terminal dock ----
// The session lives on the server; this is only a viewer. Attaching replays the
// scrollback, so a reload — or a restart that drops Traefik mid-command — picks
// up where you left off, and closing a tab detaches instead of killing.
let TSESS = [];              // server's session list (/api/term/list)
let TACTIVE = null;          // which session the dock is showing
let TXTERM = null;           // xterm.js load promise
let TERM_OK = true;          // false when the server ran with WORKMUX_TERMINAL=0
// What this project's agent can do, from workmux.json. Absent capabilities are
// absent buttons: a "＋ claude" that reports "not configured" is worse than no
// button, and this is also what makes the tool honest on a repo with no agent.
const AGENT = () => (STATE && STATE.agent) || {};
const agentName = () => AGENT().name || "agent";
const TPANES = new Map();    // id -> {info, term, fit, wrap, ws, dead, missed, tries}
const TBUSY = new Set();     // ids mid-attach, so a double click makes one pane
const termOpen = () => $("#tdock").classList.contains("on");

const TTHEME = {
  background: "#0c0c11", foreground: "#e9e9f0", cursor: "#b7adf9", cursorAccent: "#0c0c11",
  selectionBackground: "#332d5e", black: "#15151c", red: "#ec6a5e", green: "#48c98a",
  yellow: "#e6b25a", blue: "#8ea2e6", magenta: "#b79af0", cyan: "#6bcabd", white: "#c7c9d6",
  brightBlack: "#63647a", brightRed: "#f2a49c", brightGreen: "#5be08c", brightYellow: "#f0c880",
  brightBlue: "#a9bcf5", brightMagenta: "#cdb6ff", brightCyan: "#8fdcd2", brightWhite: "#fff",
};

// xterm.js is vendored in workmux/assets — no npm, no CDN, works offline.
// Loaded on first open so the dashboard itself stays a single small page.
function loadXterm() {
  if (TXTERM) return TXTERM;
  const tag = (t, attrs) => new Promise((ok, bad) => {
    const e = Object.assign(document.createElement(t), attrs);
    e.onload = ok; e.onerror = () => bad(new Error(attrs.src || attrs.href));
    document.head.appendChild(e);
  });
  TXTERM = tag("link", { rel: "stylesheet", href: "/vendor/xterm.css" })
    .then(() => tag("script", { src: "/vendor/xterm.js" }))
    .then(() => tag("script", { src: "/vendor/xterm-addon-fit.js" }));
  return TXTERM;
}

// New sessions land in the selected stack's worktree; with nothing running, the
// primary checkout (the server resolves that too, this is just the label).
// Sessions belong to the *selected work*, with or without a stack. Deriving this
// from the running stack (as it did when stacks were the unit) opened sessions
// in the primary checkout for every worktree that had no containers up — which
// is most of them, and exactly the case this tool exists for.
function termTarget() {
  const w = selWork();
  if (w) return { slot: (w.stack || {}).slot || "", path: w.path, dir: w.dir };
  return { slot: "", path: (STATE && STATE.root) || "", dir: "" };
}
function termSessions() {
  const path = (selWork() || {}).path || "";
  const mine = s => !path || s.cwd === path;
  const by = new Map(TSESS.filter(mine).map(s => [s.id, s]));
  for (const [id, p] of TPANES) if (!by.has(id) && mine(p.info)) by.set(id, p.info);
  return [...by.values()].sort((a, b) => parseInt(a.id.slice(1)) - parseInt(b.id.slice(1)));
}

async function termList() {
  let j;
  try { j = await (await fetch("/api/term/list")).json(); }
  catch (e) { return; }                       // server blipped; keep what we have
  TERM_OK = j.enabled !== false;
  TSESS = j.sessions || [];
  // Forget panes the server no longer knows about (killed elsewhere, GC'd).
  // Two strikes: a list request in flight when we created a session won't have it.
  for (const [id, p] of [...TPANES]) {
    if (TSESS.some(s => s.id === id)) { p.missed = 0; continue; }
    if (++p.missed >= 2) closePane(id);
  }
  renderSessionBar();
}

// ---- sessions: one picker, not a strip ----
// A horizontally scrolling row of tabs never fitted a phone and never will. This
// is the session you're in plus a way to switch; the list — and how to start
// another — lives in a sheet, which has room to say what each session is.
function renderSessionBar() {
  const list = termSessions();
  renderSessionEmpty(list.length);
  const p = TPANES.get(TACTIVE), cur = p ? p.info : null;
  const nm = $("#sess-name"), dot = $("#sess-dot");
  if (TPENDING) { nm.textContent = TPENDING.label + " · starting…"; dot.className = "dot warn live"; }
  else if (cur) { nm.textContent = cur.title; dot.className = "dot " + (p.dead ? "" : "ok live"); }
  else if (list.length) { nm.textContent = list.length + " sessions"; dot.className = "dot"; }
  else {
    const a = agentForTarget();
    nm.textContent = a ? "resume " + agentLabel(a) : "no session";
    dot.className = "dot" + (a && a.tempo === "blocked" ? " warn" : "");
  }
  $("#sesspick").title = (cur ? cur.title : "Start a session")
    + (list.length > 1 ? "  (" + list.length + " here)" : "") + (SEL ? "\n" + SEL : "");
}

const sessmodal = $("#sessmodal");
function closeSessions() { sessmodal.classList.remove("on"); }
function openSessions() {
  const w = selWork(), list = termSessions();
  $("#sess-where").textContent = w ? "in " + w.dir : "";

  // The agent comes first and reads as the主 action: it's what a worktree is
  // for. Sessions and "start something else" are below it.
  const lead = $("#sesslead"); lead.innerHTML = "";
  const a = agentForTarget();
  if (a) {
    const open = [...TPANES.values()].find(x => x.info.agent === a.id && !x.dead);
    const card = el("button", "sesslead" + (open ? " on" : ""));
    const mid = el("div");
    mid.appendChild(el("div", "nm", agentLabel(a)));
    mid.appendChild(el("div", "sub", a.tempo === "blocked" ? "needs your input"
      : a.tempo === "active" ? "working" : open ? "open in this dock" : "pick up where it left off"));
    card.append(el("span", "ag-dot " + (a.tempo || "idle")), mid,
                el("span", "go", open ? "go ▸" : "❯ resume"));
    card.onclick = () => {
      closeSessions();
      if (open) setTerm(open.info.id); else newTerm("resume");
    };
    lead.appendChild(card);
  } else {
    const card = el("button", "sesslead");
    const mid = el("div");
    mid.appendChild(el("div", "nm", "No agent in this worktree"));
    mid.appendChild(el("div", "sub", "start one to work here"));
    card.append(el("span", "ag-dot idle"), mid, el("span", "go", "＋ " + agentName()));
    card.onclick = () => { closeSessions(); newTerm("agent"); };
    lead.appendChild(card);
  }

  const box = $("#sesslist"); box.innerHTML = "";
  list.forEach(info => {
    const p = TPANES.get(info.id), dead = p ? p.dead : !info.alive;
    const row = el("div", "sessrow" + (info.id === TACTIVE ? " on" : ""));
    row.append(el("span", "kind", info.kind), el("span", "nm", info.title));
    if (dead) row.appendChild(el("span", "kind", "ended"));
    const k = el("button", "k", "✕");
    k.title = dead ? "Close" : "Kill this session";
    k.onclick = e => { e.stopPropagation(); killTerm(info.id, dead); openSessions(); };
    row.appendChild(k);
    row.onclick = () => { closeSessions(); attachSession(info); };
    box.appendChild(row);
  });
  if (list.length) box.insertBefore(el("div", "chsect", "Open sessions"), box.firstChild);

  const nw = $("#sessnew"); nw.innerHTML = "";
  nw.appendChild(el("div", "chsect", "Start something else"));
  const add = (label, title, fn) => {
    const b = el("button", null, label); b.title = title || "";
    b.onclick = () => { closeSessions(); fn(); }; nw.appendChild(b);
  };
  if (AGENT().run) add("＋ " + agentName(), "Another " + agentName() + " session here", () => newTerm("agent"));
  add("＋ shell", "A plain shell here", () => newTerm("shell"));
  add("＋ git", "lazygit here", () => newTerm("git"));
  if (w && w.stack && stackEnabled())
    add("＋ logs", "Tail this stack's containers in a session", () => newTerm("logs"));
  add("↻ redraw", "Nudge the program to repaint — fixes a screen garbled by a resize", () => {
    const p = TPANES.get(TACTIVE);
    if (!p) return;
    wsSend(p, JSON.stringify({ t: "size", cols: Math.max(20, p.term.cols - 1), rows: p.term.rows }));
    setTimeout(() => { fitPane(p); wsSend(p, JSON.stringify({ t: "size", cols: p.term.cols, rows: p.term.rows })); }, 150);
  });
  add("⧉ CLI", "Copy the equivalent bin/dev command", copyCli);
  sessmodal.classList.add("on");
}
$("#sesspick").onclick = openSessions;
$("#sess-add").onclick = openSessions;
$("#sess-close").onclick = closeSessions;
sessmodal.addEventListener("click", e => { if (e.target === sessmodal) closeSessions(); });
function copyCli() {
  const w = selWork(); if (!w) return;
  const cmd = (w.stack ? "STACK=" + w.stack.slot + " " : "") + "bin/dev shell " + (w.pr ? w.pr.number : w.dir);
  navigator.clipboard && navigator.clipboard.writeText(cmd);
  toast("Copied: " + cmd);
}
$("#pane-keys").onclick = () => { openSessions(); setTimeout(() => {
  const k = $("#sesssheet").querySelector(".keys");
  if (k) k.scrollIntoView({ block: "nearest" });
}, 40); };
$("#pane-full").onclick = () => {
  const on = document.body.classList.toggle("focus");
  $("#pane-full").textContent = on ? "⤡" : "⤢";
  setTimeout(refitActive, 60);
};

// Selecting work moves the pane with it: focus a session already open for that
// worktree rather than leaving you typing into the previous one. Only focuses
// what exists — switching work must not silently spawn shells.
function followTerm() {
  if (!termOpen()) return;
  const t = termTarget();
  if (!t.path) return;
  const cur = TPANES.get(TACTIVE);
  if (cur && cur.info.cwd === t.path && !cur.dead) return;      // already there
  const live = s => { const p = TPANES.get(s.id); return p ? !p.dead : s.alive; };
  const s = termSessions().find(x => x.cwd === t.path && live(x));
  if (s) return attachSession(s);
  // Nothing open here: hide the other worktrees' panes and let the empty state
  // say how to start one.
  TACTIVE = null;
  TPANES.forEach(p => p.wrap.classList.remove("on"));
  renderSessionBar();
}

// With no session open the pane is a blank void; say what the buttons do and
// name the agent waiting to be resumed, if there is one.
function renderSessionEmpty(haveSessions) {
  const host = $("#tbody");
  let box = host.querySelector(".tnote");
  if (haveSessions || TPENDING) { if (box) box.remove(); return; }
  if (!box) { box = el("div", "tnote"); host.appendChild(box); }
  const w = selWork(), a = agentForTarget();
  box.innerHTML = "";
  box.appendChild(el("div", "th", w ? w.dir : "—"));
  box.appendChild(el("div", null, a
    ? "This work has an agent — ❯ " + agentLabel(a) + " picks up where it left off."
    : "No agent here yet. ＋ " + agentName() + " starts one in this worktree; ＋ shell gives you a plain shell."));
  return box;
}

function fitPane(p) {
  // Width matters more than height: a short window still needs the right number
  // of columns, or lines clip on the right instead of wrapping.
  if (!termOpen() || !p || !p.wrap.clientWidth) return;
  try { p.fit.fit(); } catch (e) { return; /* not laid out yet */ }
  // fit() can leave the last row taller than the space left for it, so the
  // bottom line (a prompt, or the agent's status bar) sits half-hidden. Give the
  // row back if the rendered screen doesn't fit what we have.
  try {
    const screen = p.wrap.querySelector(".xterm-screen");
    if (screen && p.term.rows > 3) {
      const over = screen.getBoundingClientRect().height - p.wrap.clientHeight;
      if (over > 1) p.term.resize(p.term.cols, Math.max(3, p.term.rows - Math.ceil(over / (screen.getBoundingClientRect().height / p.term.rows))));
    }
  } catch (e) { /* nothing to correct */ }
}
function refitActive() { fitPane(TPANES.get(TACTIVE)); }

function setTerm(id) {
  TACTIVE = id;
  for (const [sid, p] of TPANES) p.wrap.classList.toggle("on", sid === id);
  renderSessionBar();
  const p = TPANES.get(id);
  if (p) { fitPane(p); p.term.focus(); }
}

async function attachSession(info) {
  if (TPANES.has(info.id)) return setTerm(info.id);
  if (TBUSY.has(info.id)) return;
  TBUSY.add(info.id);
  try {
    await loadXterm();
  } catch (e) {
    TBUSY.delete(info.id);
    toast("Could not load xterm.js — is workmux/assets/ there?");
    return;
  }
  const wrap = el("div", "tpane");
  $("#tbody").appendChild(wrap);
  const term = new Terminal({
    fontFamily: '"SF Mono","JetBrains Mono",Menlo,ui-monospace,monospace',
    fontSize: TFONT, lineHeight: 1.25, cursorBlink: true, scrollback: 8000,
    macOptionIsMeta: true, theme: TTHEME, allowProposedApi: true,
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(wrap);
  const p = { info, term, fit, wrap, ws: null, dead: !info.alive, missed: 0, tries: 0 };
  TPANES.set(info.id, p);
  TBUSY.delete(info.id);
  setTerm(info.id);
  // macOS line-editing habits the browser eats before xterm sees them. Mapped to
  // the control codes a readline/TUI actually listens for.
  const CHORDS = [
    [e => e.metaKey && e.key === "Enter", "\x1b\r"],       // newline, don't submit
    [e => e.metaKey && e.key === "ArrowLeft", "\x01"],      // start of line (⌃A)
    [e => e.metaKey && e.key === "ArrowRight", "\x05"],     // end of line (⌃E)
    [e => e.metaKey && e.key === "Backspace", "\x15"],      // kill to start (⌃U)
    [e => e.altKey && e.key === "ArrowLeft", "\x1bb"],      // word left
    [e => e.altKey && e.key === "ArrowRight", "\x1bf"],     // word right
    [e => e.altKey && e.key === "Backspace", "\x1b\x7f"],  // kill word
  ];
  term.attachCustomKeyEventHandler(e => {
    if (e.type !== "keydown") return true;
    for (const [match, bytes] of CHORDS) {
      if (match(e)) { e.preventDefault(); wsSend(p, new TextEncoder().encode(bytes)); return false; }
    }
    return true;
  });
  term.onData(d => wsSend(p, new TextEncoder().encode(d)));          // binary = keystrokes
  term.onResize(({ cols, rows }) => {
    // Only the pane you're looking at reports its size. A hidden pane (or a
    // backgrounded tab) reporting one is how the PTY ended up oscillating.
    if (document.hidden || p.info.id !== TACTIVE) return;
    wsSend(p, JSON.stringify({ t: "size", cols, rows }));
  });
  connectPane(p);
}

function wsSend(p, data) { if (p.ws && p.ws.readyState === 1) p.ws.send(data); }

function connectPane(p) {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(proto + "//" + location.host + "/api/term/attach"
    + "?id=" + encodeURIComponent(p.info.id) + "&cols=" + p.term.cols + "&rows=" + p.term.rows);
  ws.binaryType = "arraybuffer";
  p.ws = ws;
  ws.onopen = () => { p.term.reset(); p.tries = 0; };   // the server replays; don't double-paint
  ws.onmessage = ev => {
    if (typeof ev.data === "string") return termCtl(p, ev.data);
    p.term.write(new Uint8Array(ev.data));
  };
  ws.onerror = () => { /* onclose does the work */ };
  ws.onclose = () => {
    p.ws = null;
    if (p.dead || !TPANES.has(p.info.id)) return;
    // Restarting a stack tears down the Traefik route we came through; the
    // session is untouched on the host, so back off and re-attach.
    const wait = Math.min(8000, 400 * Math.pow(2, p.tries++));
    setTimeout(() => { if (TPANES.has(p.info.id) && !p.dead) connectPane(p); }, wait);
  };
}

function termCtl(p, raw) {
  let m; try { m = JSON.parse(raw); } catch (e) { return; }
  if (m.t === "session") { p.info = m.session; renderSessionBar(); }
  else if (m.t === "exit") {
    p.dead = true;
    // Same reasoning as ✕: an attach that ended (⌃D, or the agent finishing)
    // has nothing left to show, so the tab goes rather than greying out.
    if (p.info.kind === "attach") { closePane(p.info.id); TSESS = TSESS.filter(s => s.id !== p.info.id); return; }
    p.term.write("\r\n\x1b[38;5;244m— exited (" + m.code + ") · ✕ closes this tab —\x1b[0m\r\n");
    renderSessionBar();
  }
}

// Starting a session takes a couple of seconds (fork a PTY, let the shell or
// the attach paint). Show a placeholder tab with a spinner for that gap,
// and refuse a second click: without it the button looked dead and a double
// click opened two shells.
let TPENDING = null;            // {label, dir} while a session is being created
async function newTerm(kind) {
  if (TPENDING) return;
  const t = termTarget();
  const a = kind === "resume" ? agentForTarget() : null;
  TPENDING = { label: kind === "shell" ? "shell" : a ? agentLabel(a) : agentName(), dir: t.dir };
  renderSessionBar();
  try {
    const r = await fetch("/api/term/new", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ kind, stack: t.slot, cwd: t.path || undefined }),
    });
    const j = await r.json();
    if (!r.ok) return toast("✗ " + (j.error || ("HTTP " + r.status)));
    TSESS = TSESS.concat([j]);
    await attachSession(j);
  } catch (e) {
    toast("✗ " + e.message);
  } finally {
    TPENDING = null;
    renderSessionBar();
  }
}

function killTerm(id, dead) {
  const p = TPANES.get(id);
  const kind = (p ? p.info : TSESS.find(s => s.id === id) || {}).kind;
  const stop = () => fetch("/api/term/kill", { method: "POST",
    headers: { "Content-Type": "application/json" }, body: JSON.stringify({ id }) }).catch(() => {});
  // ✕ on an attach means "close this", not "leave me a corpse": detaching leaves
  // nothing to read, and a dead tab beside a live one for the same agent is
  // exactly the confusion this used to cause. Shells keep their last output.
  if (dead || kind === "attach") {
    if (!dead) stop();
    closePane(id); TSESS = TSESS.filter(s => s.id !== id); renderSessionBar(); return;
  }
  stop().then(() => termList());
}

function closePane(id) {
  const p = TPANES.get(id); if (!p) return;
  TPANES.delete(id);
  p.dead = true;                          // stops the reconnect loop
  if (p.ws) { try { p.ws.close(); } catch (e) {} }
  try { p.term.dispose(); } catch (e) {}
  p.wrap.remove();
  if (TACTIVE === id) {
    TACTIVE = null;
    const next = [...TPANES.keys()][0];
    if (next) setTerm(next); else renderSessionBar();
  }
}

async function toggleTerm(force) {
  const open = force == null ? !termOpen() : !!force;
  $("#tdock").classList.toggle("on", open);
  if (!open) return;
  // Before the first /api/work lands there is no selected worktree, and the
  // session would open in the primary checkout instead of the one you picked.
  if (!STATE) await refresh();
  renderSessionBar();
  await termList();
  if (!TERM_OK) { toast("Terminal is off (WORKMUX_TERMINAL=0)"); return toggleTerm(false); }
  // Finished sessions stay listed so you can read how they ended, but opening
  // the dock should land you somewhere you can type — so pick a live one, and
  // start a shell if every session is a corpse.
  const live = termSessions().filter(s => { const p = TPANES.get(s.id); return p ? !p.dead : s.alive; });
  // Nearly all work happens in the agent, so that's what opening a session
  // means; a shell is what you ask for, not what you get.
  if (!live.length) return newTerm(agentForTarget() ? "resume" : "shell");
  const keep = TACTIVE && live.some(s => s.id === TACTIVE);
  attachSession(keep ? live.find(s => s.id === TACTIVE) : live[0]);
}

new ResizeObserver(() => refitActive()).observe($("#tbody"));

// ---- phones & tablets ----
// Below 820px one surface at a time: the work list, the session, or the logs.
// Desktop shows list + pane together and the nav is hidden.
const NARROW = () => matchMedia("(max-width:820px)").matches;
const COARSE = () => matchMedia("(hover:none),(pointer:coarse)").matches;

function setView(v) {
  document.body.dataset.view = v;
  renderNav();
  if (v === "session" || v === "changes") setPane(v === "changes" ? "changes" : "session");
}
function renderNav() {
  const v = document.body.dataset.view || "";
  [...$("#mobnav").children].forEach(b => {
    b.classList.toggle("on", b.dataset.view === v);
    const old = b.querySelector(".badge"); if (old) old.remove();
    if (b.dataset.view === "work") {
      const need = AGENTS.filter(a => a.tempo === "blocked").length;
      if (need) { const t = el("span", "badge", String(need)); b.appendChild(t); }
    }
  });
}
[...$("#mobnav").children].forEach(b => { b.onclick = () => setView(b.dataset.view); });

// The keys a phone keyboard doesn't have but a TUI can't live without. Sent as
// raw bytes down the same channel as typing, so the PTY can't tell the
// difference between these and a real keyboard.
const KEYS = [
  ["esc", "\x1b", "Escape"], ["tab", "\t", "Tab"],
  ["⌃C", "\x03", "Interrupt"], ["⌃D", "\x04", "End of input / detach"],
  ["|sep"],
  ["↑", "\x1b[A"], ["↓", "\x1b[B"], ["←", "\x1b[D"], ["→", "\x1b[C"],
  ["|sep"],
  ["⇧⏎", "\x1b\r", "Newline without submitting"], ["⏎", "\r"],
  ["|sep"],
  ["⌨", null, "Show the keyboard"], ["⧉", null, "Paste from clipboard"],
  ["A−", null, "Smaller"], ["A+", null, "Bigger"],
];
function sendKey(bytes) {
  const p = TPANES.get(TACTIVE);
  if (!p) return;
  wsSend(p, new TextEncoder().encode(bytes));
  p.term.focus();
}
function focusTerm() {
  const p = TPANES.get(TACTIVE);
  if (p) p.term.focus();      // inside a tap handler, so iOS opens the keyboard
}
async function pasteIntoTerm() {
  try {
    const text = await navigator.clipboard.readText();
    if (text) sendKey(text);
  } catch (e) { toast("Clipboard not available — long-press the terminal to paste"); }
}
let TFONT = +(localStorage.getItem("workmux.termFont") || 0) || (COARSE() ? 11 : 12.5);
function setTermFont(px) {
  TFONT = Math.max(8, Math.min(20, px));
  localStorage.setItem("workmux.termFont", TFONT);
  TPANES.forEach(p => { p.term.options.fontSize = TFONT; fitPane(p); });
  toast("Terminal font " + TFONT + "px");
}
function renderKeybar() {
  const bar = $("#keybar"); bar.innerHTML = "";
  KEYS.forEach(([label, bytes, title]) => {
    if (label === "|sep") { bar.appendChild(el("span", "sep")); return; }
    const b = el("button", label.length > 2 ? "wide" : null, label);
    if (title) b.title = title;
    // pointerdown, not click: the terminal must never lose focus, or iOS closes
    // the keyboard between every keypress.
    b.addEventListener("pointerdown", e => {
      e.preventDefault();
      if (bytes) return sendKey(bytes);
      if (label === "⌨") return focusTerm();
      if (label === "⧉") return pasteIntoTerm();
      setTermFont(TFONT + (label === "A+" ? 1 : -1));
    });
    bar.appendChild(b);
  });
}
renderKeybar();

// The on-screen keyboard shrinks the viewport rather than the window, so xterm
// has to be re-fitted against visualViewport or the prompt hides behind it.
if (window.visualViewport) {
  let vt = null;
  const onvv = () => { clearTimeout(vt); vt = setTimeout(refitActive, 120); };
  visualViewport.addEventListener("resize", onvv);
  visualViewport.addEventListener("scroll", onvv);
}
addEventListener("orientationchange", () => setTimeout(refitActive, 250));

// Some rendering is width-dependent (compact counters, which surfaces are on
// screen). Nothing re-ran on resize, so rotating a tablet — or just dragging a
// window narrow — left the previous size's markup in place.
let rzT = null;
addEventListener("resize", () => {
  clearTimeout(rzT);
  rzT = setTimeout(() => {
    renderWork(); renderWorkHead(); renderSessionBar(); renderNav();
    if (!NARROW()) document.body.dataset.view = "";
    else if (!document.body.dataset.view) setView("work");
    refitActive();
  }, 150);
});

// ---- global keys ----
document.addEventListener("keydown", e => {
  if (e.key === "`" && (e.metaKey || e.ctrlKey)) {
    e.preventDefault();
    return setView("session");
  }
  // Inside a terminal, Escape and ⌘K belong to whatever is running there —
  // the agent, vim, less all use them. Don't hijack them.
  if ($("#tdock").contains(e.target)) return;
  if ((e.metaKey || e.ctrlKey) && e.key === "k") {
    e.preventDefault();
    if (NARROW()) setView("work");
    $("#wl-filter").focus(); $("#wl-filter").select();
  } else if ((e.metaKey || e.ctrlKey) && e.key === "n") {
    e.preventDefault(); openNew();
  } else if (e.key === "Escape") {
    if (logmodal.classList.contains("on")) return closeLog();
    if (mcpmodal.classList.contains("on")) return closeMcp();
    if (sessmodal.classList.contains("on")) return closeSessions();
    if (modal.classList.contains("on")) return closeNew();
    closeConsole(); toggleRespop(false);
  }
});

// ---- boot ----
refresh();
termList().then(() => { if (!TERM_OK) toast("Terminal is off (WORKMUX_TERMINAL=0)"); });
if (NARROW()) setView("work");
setPane("session");
setInterval(refresh, 5000);
setInterval(() => { if (activeStack()) pollStats(); }, 4000);
setInterval(() => { if (termOpen()) termList(); }, 5000);
</script>
</body></html>
"""


def lan_address():
    """Best-guess address another device on the network can reach us on."""
    import socket
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.connect(("192.0.2.1", 1))       # reserved, never routed; no packets sent
        return s.getsockname()[0]
    except OSError:
        return ""
    finally:
        s.close()


# Everything this process says about itself, kept where the UI can read it. It
# only ever wrote to the terminal it was started from — which, for a host process
# you launch once and forget, is nowhere you'll look when something misbehaves.
SERVER_LOG = collections.deque(maxlen=800)


class _LogTee:
    """Pass stderr through to the terminal and keep a copy in memory."""

    def __init__(self, stream):
        self.stream = stream
        self._partial = ""

    def write(self, text):
        try:
            self.stream.write(text)
        except Exception:
            pass
        self._partial += text
        while "\n" in self._partial:
            line, self._partial = self._partial.split("\n", 1)
            line = _strip_ansi(line).rstrip()
            if line:
                SERVER_LOG.append({"t": time.time(), "line": line[:2000]})
        return len(text)

    def flush(self):
        try:
            self.stream.flush()
        except Exception:
            pass

    def isatty(self):
        return getattr(self.stream, "isatty", lambda: False)()

    def fileno(self):
        return self.stream.fileno()


def note(msg):
    """One line about something the server did. Goes to stderr, so the terminal
    and the in-UI log see the same thing."""
    sys.stderr.write("%s\n" % msg)


def server_log(level=""):
    rows = list(SERVER_LOG)
    if level == "problems":
        rows = [r for r in rows if re.search(r"error|traceback|exception|✗|failed|refused", r["line"], re.I)]
    return {"lines": rows[-400:], "total": len(SERVER_LOG)}


class DevServer(ThreadingHTTPServer):
    """ThreadingHTTPServer that doesn't shout when a browser hangs up.

    Clients drop sockets all the time and it is never our problem: Chrome opens
    speculative keep-alive connections and resets the unused ones, every stack
    switch tears down an SSE log stream mid-flight, and closing a terminal tab
    kills a WebSocket. The default handle_error prints a 20-line traceback for
    each one, which buries anything that actually matters. Real errors still
    print.
    """
    daemon_threads = True

    def handle_error(self, request, client_address):
        if isinstance(sys.exc_info()[1], (ConnectionResetError, BrokenPipeError,
                                          ConnectionAbortedError, TimeoutError)):
            return
        ThreadingHTTPServer.handle_error(self, request, client_address)


def main():
    # docker is only needed by the stack, and half the projects this runs on
    # don't have one — requiring it up front turned "no containers here" into
    # "won't start".
    for tool in ["git"] + (["docker"] if STACK else []):
        if not shutil.which(tool):
            sys.stderr.write("workmux: '%s' is required.\n" % tool)
            sys.exit(1)
    if not os.path.exists(os.path.join(ROOT, ".git")):
        sys.stderr.write("workmux: %s is not a git repository.\n"
                         "The unit of work here is a worktree, so it needs one — "
                         "run it inside your project, or pass --root DIR.\n" % ROOT)
        sys.exit(1)
    sys.stderr = _LogTee(sys.stderr)      # before anything logs
    httpd = DevServer((HOST, PORT), Handler)
    root = primary_root()
    where = "http://%s:%d" % ("127.0.0.1" if HOST in ("0.0.0.0", "::") else HOST, PORT)
    lines = ["\n  \033[1mworkmux\033[0m \033[2m%s\033[0m — %s\n" % (VERSION, PROJECT),
             "  \033[36m%s\033[0m\n" % where]
    if HOST not in LOOPBACK:
        lines.append("  \033[36m%s/?t=%s\033[0m  \033[2m← open this on your phone\033[0m\n"
                     % (where.replace("127.0.0.1", lan_address() or HOST), TOKEN))
        lines.append("  \033[2mtoken auth on (loopback exempt). Terminals are a shell: put TLS in\n"
                     "  front before exposing this beyond a trusted network.\033[0m\n")
    elif not TOKEN:
        lines.append("  \033[2mloopback only — no token needed\033[0m\n")
    if not STACK:
        lines.append("  \033[2mno stack configured — worktrees, agents and sessions only\033[0m\n")
    lines.append("  root: %s\n  Ctrl-C to stop.\n\n" % root)
    sys.stderr.write("".join(lines))
    if OPTS.get("open"):
        opener = next((c for c in ("open", "xdg-open") if shutil.which(c)), "")
        if opener:
            subprocess.Popen([opener, where + (("/?t=" + TOKEN) if TOKEN else "")],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        sys.stderr.write("\n  stopped.\n")
        httpd.shutdown()


if __name__ == "__main__":
    main()
