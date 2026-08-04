// Package config resolves everything project-shaped from workmux.json — and,
// when it isn't there, from what the repository already says.
//
// The rule that matters: a repo needs no config at all. The name comes from the
// directory, the compose file from looking for one, slots from the name,
// commands from plain `docker compose`. And anything that can't be inferred is
// left blank, which the UI reads as "this project doesn't do that" rather than
// offering a button that fails.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// composeNames are tried in order when stack.compose isn't given.
var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}

// defaultCommands keep a project with no wrapper script working: {slot},
// {compose}, {profiles}, {path} and {base} are substituted at call time.
var defaultCommands = map[string]string{
	"up":      "docker compose -p {slot} -f {compose} up -d --build",
	"restart": "docker compose -p {slot} -f {compose} restart",
	"stop":    "docker compose -p {slot} -f {compose} down --remove-orphans",
	"logs":    "docker compose -p {slot} -f {compose} logs -f --tail 200 --no-color",
}

// agentPresets is the only place a command name implies more than itself.
// Claude Code is here because it's what this was built against; every other CLI
// has to say how it spawns, attaches and (if it has one) does MCP.
var agentPresets = map[string]Agent{
	"claude": {
		Spawn:  "claude --bg {prompt}",
		Attach: "claude attach {id}",
		Jobs:   "~/.claude/jobs",
		MCP:    "claude mcp",
	},
}

// Agent is how the tool touches a coding agent. Each field is a command
// template; an empty one is a capability that isn't offered.
type Agent struct {
	Command string `json:"command"` // what a new agent session runs
	Spawn   string `json:"spawn"`   // how New work starts one, with {prompt}
	Attach  string `json:"attach"`  // how to take over a running one, with {id}
	Jobs    string `json:"jobs"`    // directory of per-agent state
	MCP     string `json:"mcp"`     // subcommand prefix for the MCP registry
	Process string `json:"process"` // process name that means "working here"
	Name    string `json:"name"`    // what to call it in the UI
}

// Worktrees is where new work goes, and what has to follow it there.
type Worktrees struct {
	Path string `json:"path"`
	// Copy lists gitignored files a fresh worktree can't work without, as
	// gitignore-style patterns. Without them the app fails minutes later with
	// something that looks nothing like "you forgot a file".
	Copy []string `json:"copy"`
}

// Stack is the project's containers, if it has any.
type Stack struct {
	Compose  string            `json:"compose"`
	Slots    string            `json:"slots"`
	URL      string            `json:"url"`
	Profiles string            `json:"profiles"`
	Commands map[string]string `json:"commands"`
}

// Config is the resolved shape of a project. Stack is nil when there is nothing
// to run, which is a supported answer rather than a missing feature.
type Config struct {
	Root      string    `json:"-"`
	Name      string    `json:"name"`
	Worktrees Worktrees `json:"worktrees"`
	Agent     Agent     `json:"agent"`
	Stack     *Stack    `json:"stack"`

	slotRe *regexp.Regexp
}

// raw mirrors the file. Stack and Agent are held as RawMessage so that "absent"
// (infer it) stays distinguishable from "null" (this project has none) — the
// whole point of both keys.
type raw struct {
	Name      string          `json:"name"`
	Worktrees *Worktrees      `json:"worktrees"`
	Agent     json.RawMessage `json:"agent"`
	Stack     json.RawMessage `json:"stack"`
}

// Load reads workmux.json from root, filling in everything it doesn't say.
// A missing or unreadable file is not an error: that's the zero-config case.
func Load(root string) (*Config, error) {
	var r raw
	for _, name := range []string{"workmux.json", ".workmux.json"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		if err := json.Unmarshal(b, &r); err != nil {
			// A typo in config is worth stopping for — silently ignoring it
			// would look like the tool ignoring the project.
			return nil, err
		}
		break
	}

	c := &Config{Root: root, Name: r.Name}
	if c.Name == "" {
		c.Name = filepath.Base(root)
	}
	if c.Name == "" || c.Name == "." || c.Name == string(filepath.Separator) {
		c.Name = "dev"
	}

	c.Worktrees = Worktrees{Path: filepath.Join(".claude", "worktrees"), Copy: []string{}}
	if r.Worktrees != nil {
		if r.Worktrees.Path != "" {
			c.Worktrees.Path = r.Worktrees.Path
		}
		if r.Worktrees.Copy != nil {
			c.Worktrees.Copy = r.Worktrees.Copy
		}
	}

	c.Agent = resolveAgent(r.Agent)
	c.Stack = resolveStack(r.Stack, root, c.Name)
	pat := slotStem(c.Name) + "{n}"
	if c.Stack != nil {
		pat = c.Stack.Slots
	}
	// A slot is the pattern with a number — and also the project name exactly,
	// because `docker compose up` names the project after the directory. Without
	// that, the single stack somebody already has running (the common case when
	// pointing this at an existing project) matched nothing and the dashboard
	// claimed no app was up. The name, not the pattern-minus-{n}: "trip{n}" must
	// not start claiming a project called "trip".
	numbered := strings.Replace(regexp.QuoteMeta(pat), `\{n\}`, "[0-9]+", 1)
	c.slotRe = regexp.MustCompile("^(?:" + numbered + "|" + regexp.QuoteMeta(c.Name) + ")$")
	return c, nil
}

func resolveAgent(rm json.RawMessage) Agent {
	if isNull(rm) { // "there is no agent here"
		return Agent{}
	}
	var a Agent
	if len(rm) > 0 {
		_ = json.Unmarshal(rm, &a)
	}
	if a.Command == "" {
		a.Command = "claude"
	}
	a.Command = strings.TrimSpace(a.Command)
	exe := ""
	if fields := strings.Fields(a.Command); len(fields) > 0 {
		exe = filepath.Base(fields[0])
	}
	preset := agentPresets[exe]
	if a.Spawn == "" {
		a.Spawn = preset.Spawn
	}
	if a.Attach == "" {
		a.Attach = preset.Attach
	}
	if a.Jobs == "" {
		a.Jobs = preset.Jobs
	}
	if a.MCP == "" {
		a.MCP = preset.MCP
	}
	if a.Process == "" {
		// What "running" means: a live process of this name inside a worktree.
		// Per-agent state is a turn-boundary snapshot, so it lags a whole turn.
		a.Process = exe
	}
	if a.Name == "" {
		a.Name = exe
		if a.Name == "" {
			a.Name = "agent"
		}
	}
	return a
}

func resolveStack(rm json.RawMessage, root, name string) *Stack {
	if isNull(rm) { // explicitly "this project has no app"
		return nil
	}
	var s Stack
	if len(rm) > 0 {
		_ = json.Unmarshal(rm, &s)
	}
	if s.Compose == "" {
		for _, cand := range composeNames {
			if st, err := os.Stat(filepath.Join(root, cand)); err == nil && !st.IsDir() {
				s.Compose = cand
				break
			}
		}
	}
	if s.Compose == "" {
		return nil // nothing to run → no stack, and so no stack buttons
	}
	if s.Slots == "" {
		// A trailing number in the project name is already a slot number: a repo
		// called trip1 wants trip1, trip2 — not trip11, trip12.
		s.Slots = slotStem(name) + "{n}"
	}
	if s.Profiles == "" {
		s.Profiles = os.Getenv("COMPOSE_PROFILES")
	}
	cmds := map[string]string{}
	for k, v := range defaultCommands {
		cmds[k] = v
	}
	for k, v := range s.Commands {
		if v != "" {
			cmds[k] = v
		}
	}
	s.Commands = cmds
	return &s
}

// isNull separates a present-but-null key from an absent one.
func isNull(rm json.RawMessage) bool {
	return len(rm) > 0 && string(rm) == "null"
}

// slotStem drops a trailing number from a name, so trip1 yields trip1 and trip2
// rather than trip11 and trip12. A name that is nothing but digits keeps itself.
func slotStem(name string) string {
	stem := strings.TrimRight(name, "0123456789")
	if stem == "" {
		return name
	}
	return stem
}

// HasStack reports whether this project has containers at all.
func (c *Config) HasStack() bool { return c.Stack != nil }

// SlotName is the nth stack slot — trip1, trip2, …
func (c *Config) SlotName(n int) string {
	pat := slotStem(c.Name) + "{n}"
	if c.Stack != nil {
		pat = c.Stack.Slots
	}
	return strings.Replace(pat, "{n}", strconv.Itoa(n), 1)
}

// IsSlot reports whether a docker compose project name is one of ours, so other
// projects on the same machine aren't mistaken for this repo's work.
func (c *Config) IsSlot(s string) bool { return c.slotRe.MatchString(s) }

// StackURL is where a slot is reachable, or "" when the project didn't say
// (in which case there is no Open button rather than a broken one).
func (c *Config) StackURL(slot string) string {
	if c.Stack == nil {
		return ""
	}
	return strings.ReplaceAll(c.Stack.URL, "{slot}", slot)
}

// Profiles is the compose profile list, empty when there's no stack.
func (c *Config) Profiles() string {
	if c.Stack == nil {
		return ""
	}
	return c.Stack.Profiles
}

// StackCmd builds a shell command for a stack action. Empty means "this project
// can't do that", and callers must refuse rather than improvise.
func (c *Config) StackCmd(action, slot, path, base string) string {
	if c.Stack == nil {
		return ""
	}
	tmpl := c.Stack.Commands[action]
	if tmpl == "" {
		return ""
	}
	r := strings.NewReplacer(
		"{slot}", slot,
		"{path}", path,
		"{base}", base,
		"{compose}", c.Stack.Compose,
		"{profiles}", c.Stack.Profiles,
	)
	return r.Replace(tmpl)
}

// SpawnCmd is the command that starts an agent on a task, or "" if the project
// has no way to.
func (c *Config) SpawnCmd(prompt string) string {
	if c.Agent.Spawn == "" {
		return ""
	}
	return strings.ReplaceAll(c.Agent.Spawn, "{prompt}", shellQuote(prompt))
}

// AttachCmd is the command that takes over a running agent.
func (c *Config) AttachCmd(id string) string {
	if c.Agent.Attach == "" {
		return ""
	}
	return strings.ReplaceAll(c.Agent.Attach, "{id}", shellQuote(id))
}

// JobsDir is where per-agent state is readable, expanded; "" when there is none.
func (c *Config) JobsDir() string {
	if c.Agent.Jobs == "" {
		return ""
	}
	p := c.Agent.Jobs
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p[1:], string(filepath.Separator)))
		}
	}
	return p
}

// shellQuote wraps a value for a POSIX shell. Prompts are arbitrary user text
// heading for `sh -lc`, so this is the boundary that has to hold.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
