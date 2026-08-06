package config

import (
	"os"
	"path/filepath"
	"testing"
)

// write puts a repo-shaped directory on disk. No git needed: config resolution
// only looks at file names.
func write(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func load(t *testing.T, root string) *Config {
	t.Helper()
	c, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// The headline claim: a repo needs no config at all.
func TestBareRepoNeedsNoConfig(t *testing.T) {
	c := load(t, write(t, "my-project", nil))

	if c.Name != "my-project" {
		t.Errorf("name = %q, want the directory name", c.Name)
	}
	if c.HasStack() {
		t.Error("no compose file should mean no stack")
	}
	if c.Worktrees.Path != filepath.Join(".claude", "worktrees") {
		t.Errorf("worktrees.path = %q", c.Worktrees.Path)
	}
	if len(c.Worktrees.Copy) != 0 {
		t.Errorf("worktrees.copy = %v, want empty", c.Worktrees.Copy)
	}
	// Claude Code is the default preset, so today's behaviour is the no-config
	// behaviour.
	want := Agent{
		Command: "claude", Spawn: "claude --bg {prompt}", Attach: "claude attach {id}",
		Jobs: "~/.claude/jobs", MCP: "claude mcp", MCPAuth: "login {name} --no-browser",
		Process: "claude", Name: "claude",
	}
	if c.Agent != want {
		t.Errorf("agent = %+v\nwant %+v", c.Agent, want)
	}
}

func TestComposeFileIsEnoughForAStack(t *testing.T) {
	for _, name := range []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"} {
		t.Run(name, func(t *testing.T) {
			c := load(t, write(t, "shop", map[string]string{name: "services: {}\n"}))
			if !c.HasStack() {
				t.Fatal("stack not detected")
			}
			if c.Stack.Compose != name {
				t.Errorf("compose = %q, want %q", c.Stack.Compose, name)
			}
			if got := c.SlotName(2); got != "shop2" {
				t.Errorf("slot 2 = %q, want shop2", got)
			}
			wantCmd := "docker compose -p shop1 -f " + name + " up -d --build"
			if got := c.StackCmd("up", "shop1", "", ""); got != wantCmd {
				t.Errorf("up = %q\nwant %q", got, wantCmd)
			}
			// No url configured means no Open button, not a broken link.
			if got := c.StackURL("shop1"); got != "" {
				t.Errorf("url = %q, want empty", got)
			}
		})
	}
}

// compose.yaml wins over docker-compose.yml when both exist: it's the name the
// current tooling writes.
func TestComposeDetectionOrder(t *testing.T) {
	c := load(t, write(t, "both", map[string]string{
		"docker-compose.yml": "services: {}\n",
		"compose.yaml":       "services: {}\n",
	}))
	if c.Stack.Compose != "compose.yaml" {
		t.Errorf("compose = %q, want compose.yaml", c.Stack.Compose)
	}
}

func TestStackNullBeatsDetection(t *testing.T) {
	c := load(t, write(t, "nulled", map[string]string{
		"compose.yaml": "services: {}\n",
		"workmux.json": `{"stack": null}`,
	}))
	if c.HasStack() {
		t.Fatal(`"stack": null must win over a compose file on disk`)
	}
	// And every stack action has to refuse rather than improvise.
	for _, action := range []string{"up", "restart", "stop", "logs"} {
		if got := c.StackCmd(action, "x", "", ""); got != "" {
			t.Errorf("%s = %q, want empty", action, got)
		}
	}
	if c.Profiles() != "" || c.StackURL("x") != "" {
		t.Error("no stack should mean no profiles and no url")
	}
}

func TestConfiguredStackOverridesEverything(t *testing.T) {
	c := load(t, write(t, "trip1", map[string]string{
		"compose.yaml": "services: {}\n",
		"workmux.json": `{
			"name": "trip1",
			"stack": {
				"slots": "trip{n}",
				"url": "http://{slot}.localhost",
				"profiles": "app,tools",
				"commands": {"up": "STACK={slot} bin/dev up {path}"}
			}
		}`,
	}))
	if got := c.SlotName(2); got != "trip2" {
		t.Errorf("slot = %q, want trip2", got)
	}
	if got := c.StackURL("trip2"); got != "http://trip2.localhost" {
		t.Errorf("url = %q", got)
	}
	if got := c.StackCmd("up", "trip2", "/w/x", ""); got != "STACK=trip2 bin/dev up /w/x" {
		t.Errorf("up = %q", got)
	}
	// Unspecified commands still fall back, so a partial override isn't a hole.
	if got := c.StackCmd("stop", "trip2", "", ""); got == "" {
		t.Error("stop should fall back to the compose default")
	}
	if !c.IsSlot("trip7") || c.IsSlot("other1") || c.IsSlot("trip") {
		t.Error("slot matching is wrong")
	}
}

// "agent": null means there is no agent here — and every agent-shaped capability
// has to disappear rather than fail.
func TestAgentNull(t *testing.T) {
	c := load(t, write(t, "noagent", map[string]string{"workmux.json": `{"agent": null}`}))
	if c.Agent != (Agent{}) {
		t.Errorf("agent = %+v, want zero", c.Agent)
	}
	if c.JobsDir() != "" {
		t.Errorf("jobs = %q, want empty", c.JobsDir())
	}
	if c.SpawnCmd("do a thing") != "" || c.AttachCmd("abc") != "" {
		t.Error("no agent should mean no spawn and no attach command")
	}
}

func TestAnotherAgentCLI(t *testing.T) {
	c := load(t, write(t, "other", map[string]string{"workmux.json": `{
		"agent": {"command": "mycoder --resume", "spawn": "mycoder run {prompt}"}
	}`}))
	if c.Agent.Command != "mycoder --resume" {
		t.Errorf("command = %q", c.Agent.Command)
	}
	// The process to look for is the executable, not the whole command line.
	if c.Agent.Process != "mycoder" || c.Agent.Name != "mycoder" {
		t.Errorf("process/name = %q/%q, want mycoder", c.Agent.Process, c.Agent.Name)
	}
	// Claude's defaults must not leak onto a different CLI.
	if c.Agent.Attach != "" || c.Agent.Jobs != "" || c.Agent.MCP != "" {
		t.Errorf("claude defaults leaked: %+v", c.Agent)
	}
	if got := c.SpawnCmd("ship it"); got != "mycoder run 'ship it'" {
		t.Errorf("spawn = %q", got)
	}
}

// Prompts are arbitrary user text heading for `sh -lc`, so quoting is the
// boundary that has to hold.
func TestSpawnQuotesTheProompt(t *testing.T) {
	c := load(t, write(t, "quoting", nil))
	got := c.SpawnCmd(`don't; rm -rf /`)
	want := `claude --bg 'don'\''t; rm -rf /'`
	if got != want {
		t.Errorf("spawn = %q\nwant %q", got, want)
	}
}

func TestJobsDirExpandsHome(t *testing.T) {
	c := load(t, write(t, "home", nil))
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if want := filepath.Join(home, ".claude", "jobs"); c.JobsDir() != want {
		t.Errorf("jobs = %q, want %q", c.JobsDir(), want)
	}
}

func TestWorktreesConfig(t *testing.T) {
	c := load(t, write(t, "wt", map[string]string{"workmux.json": `{
		"worktrees": {"path": ".wt", "copy": [".env", "config/*.key"]}
	}`}))
	if c.Worktrees.Path != ".wt" {
		t.Errorf("path = %q", c.Worktrees.Path)
	}
	if len(c.Worktrees.Copy) != 2 {
		t.Errorf("copy = %v", c.Worktrees.Copy)
	}
}

// A typo in config is worth stopping for: silently ignoring it looks like the
// tool ignoring the project.
func TestBrokenJSONIsAnError(t *testing.T) {
	if _, err := Load(write(t, "broken", map[string]string{"workmux.json": `{"name": }`})); err == nil {
		t.Fatal("invalid json should be an error")
	}
}

// A missing file is the zero-config case, not a failure.
func TestDotfileNameAlsoWorks(t *testing.T) {
	c := load(t, write(t, "dotted", map[string]string{".workmux.json": `{"name": "renamed"}`}))
	if c.Name != "renamed" {
		t.Errorf("name = %q, want renamed", c.Name)
	}
}

// `docker compose up` names the project after the directory, so the bare name has
// to count as a slot — otherwise the one stack an existing project already has
// running matches nothing and the dashboard says no app is up.
func TestBareProjectNameIsASlot(t *testing.T) {
	c := load(t, write(t, "my-drupal-site", map[string]string{"docker-compose.yml": "services: {}\n"}))
	if !c.IsSlot("my-drupal-site") {
		t.Error("the bare directory name must be recognised as a slot")
	}
	if !c.IsSlot("my-drupal-site2") {
		t.Error("numbered slots must still match")
	}
	for _, no := range []string{"my-drupal-siteX", "other", "my-drupal-site-2", ""} {
		if c.IsSlot(no) {
			t.Errorf("%q must not match", no)
		}
	}
	// Starting another one still gets its own name.
	if got := c.SlotName(1); got != "my-drupal-site1" {
		t.Errorf("slot 1 = %q", got)
	}
}

// The same rule under a configured pattern: trip1 is both "slot 1" and the bare
// name, and neither reading may swallow another project.
func TestConfiguredPatternStillBounded(t *testing.T) {
	c := load(t, write(t, "trip1", map[string]string{
		"compose.yaml": "services: {}\n",
		"workmux.json": `{"name":"trip1","stack":{"slots":"trip{n}"}}`,
	}))
	for _, yes := range []string{"trip1", "trip9", "trip42"} {
		if !c.IsSlot(yes) {
			t.Errorf("%q should match trip{n}", yes)
		}
	}
	// "trip" is the pattern minus its number, not a project anyone has. Matching it
	// would let this repo claim someone else's stack.
	for _, no := range []string{"trip", "trippy", "atrip1", "trip1x"} {
		if c.IsSlot(no) {
			t.Errorf("%q must not match", no)
		}
	}
}

// A project called trip1 wants slots trip1, trip2 — not trip11. The trailing number
// in the name *is* a slot number, and defaulting to "{name}{n}" read as a bug.
func TestSlotsDropATrailingNumberFromTheName(t *testing.T) {
	c := load(t, write(t, "trip1", map[string]string{"compose.yaml": "services: {}\n"}))
	if got := c.Stack.Slots; got != "trip{n}" {
		t.Errorf("slots = %q, want trip{n}", got)
	}
	if got := c.SlotName(1); got != "trip1" {
		t.Errorf("slot 1 = %q, want trip1", got)
	}
	if got := c.SlotName(2); got != "trip2" {
		t.Errorf("slot 2 = %q, want trip2", got)
	}
	if !c.IsSlot("trip1") || !c.IsSlot("trip7") {
		t.Error("numbered slots must match")
	}
	// A name with no trailing digits is untouched.
	c = load(t, write(t, "shop", map[string]string{"compose.yaml": "services: {}\n"}))
	if got := c.SlotName(2); got != "shop2" {
		t.Errorf("slot = %q, want shop2", got)
	}
	// And a name that is only digits keeps itself, rather than becoming "{n}".
	c = load(t, write(t, "2024", map[string]string{"compose.yaml": "services: {}\n"}))
	if got := c.SlotName(1); got != "20241" {
		t.Errorf("slot = %q", got)
	}
}

// A path that never passed through a shell still has to work: config files, the
// browser's "add a repository" box, a request from a phone. Typing ~/code/thing
// there resolved it against the server's own working directory.
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	for in, want := range map[string]string{
		"~":              home,
		"~/":             home,
		"~/code/thing":   filepath.Join(home, "code/thing"),
		"/abs/path":      "/abs/path",
		"relative/path":  "relative/path",
		"":               "",
		"~otheruser/x":   "~otheruser/x", // not ours to guess at
		"a/~/not-a-home": "a/~/not-a-home",
	} {
		if got := ExpandHome(in); got != want {
			t.Errorf("ExpandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

// A project with no reverse proxy gives each slot a port, which means the slot
// number has to be reachable from both the URL and the commands. Without it the
// simplest possible stack setup can't say where it is.
func TestSlotNumberSubstitution(t *testing.T) {
	c := load(t, write(t, "myapp", map[string]string{
		"compose.yaml": "services: {}\n",
		"workmux.json": `{
			"stack": {
				"slots": "myapp{n}",
				"url": "http://localhost:808{n}",
				"commands": {"up": "PORT=808{n} docker compose -p {slot} up -d"}
			}
		}`,
	}))
	if got := c.SlotNumber("myapp3"); got != "3" {
		t.Errorf("SlotNumber(myapp3) = %q, want 3", got)
	}
	if got := c.StackURL("myapp3"); got != "http://localhost:8083" {
		t.Errorf("url = %q", got)
	}
	if got := c.StackCmd("up", "myapp3", "", ""); got != "PORT=8083 docker compose -p myapp3 up -d" {
		t.Errorf("up = %q", got)
	}
	// The bare project name is a slot too — `docker compose up` with no -p makes one
	// — but it has no number, so a URL that needs one can't be built. No button
	// beats a link to the wrong port.
	if got := c.SlotNumber("myapp"); got != "" {
		t.Errorf("SlotNumber(myapp) = %q, want empty", got)
	}
	if got := c.StackURL("myapp"); got != "" {
		t.Errorf("url for an unnumbered slot = %q, want empty", got)
	}
}

// {n} must not break the hostname style, which doesn't use it.
func TestSlotURLWithoutN(t *testing.T) {
	c := load(t, write(t, "myapp", map[string]string{
		"compose.yaml": "services: {}\n",
		"workmux.json": `{"stack": {"slots": "myapp{n}", "url": "http://{slot}.localhost"}}`,
	}))
	for slot, want := range map[string]string{
		"myapp2": "http://myapp2.localhost",
		"myapp":  "http://myapp.localhost", // unnumbered, and the url doesn't care
	} {
		if got := c.StackURL(slot); got != want {
			t.Errorf("StackURL(%q) = %q, want %q", slot, got, want)
		}
	}
}
