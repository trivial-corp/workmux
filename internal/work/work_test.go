package work

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trivial-corp/workmux/internal/agents"
	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/testrepo"
)

// The ordering the whole dashboard leans on.
func TestRank(t *testing.T) {
	cases := []struct {
		name                             string
		isDefault, live, stack, hasAgent bool
		tempo                            string
		want                             int
	}{
		// The base checkout goes last however busy it looks: it's where you happen
		// to stand, not a change in flight. Ranking it by activity pinned it to the
		// top pretending to be work.
		{"base checkout with a live agent", true, true, false, true, "active", 6},
		{"needs input", false, false, false, true, "blocked", 0},
		{"live process", false, true, false, true, "idle", 1},
		{"claims active", false, false, false, true, "active", 1},
		{"containers up", false, false, true, false, "", 2},
		{"has agents", false, false, false, true, "idle", 3},
		{"nothing going on", false, false, false, false, "", 5},
	}
	for _, c := range cases {
		if got := rank(c.isDefault, c.tempo, c.live, c.stack, c.hasAgent); got != c.want {
			t.Errorf("%s: rank = %d, want %d", c.name, got, c.want)
		}
	}
}

// A live process beats a stale state file: state is written at turn boundaries, so
// the agent you are watching work reads idle.
func TestLiveBeatsTempoForOrdering(t *testing.T) {
	if rank(false, "idle", true, false, true) >= rank(false, "idle", false, false, true) {
		t.Error("a live process must rank above an idle claim")
	}
}

func TestSplitProfiles(t *testing.T) {
	cases := map[string]int{"": 0, "app": 1, "app,tools": 2, "app,,tools,": 2}
	for in, want := range cases {
		if got := splitProfiles(in); len(got) != want {
			t.Errorf("splitProfiles(%q) = %v, want %d entries", in, got, want)
		}
	}
}

// End to end against a real repository: two worktrees, one with an agent that
// needs input, and the base checkout must still sort last.
func TestBuildOrdersWorkAndAttachesAgents(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.FakeOrigin()
	quiet := r.Worktree("quiet-change")
	blocked := r.Worktree("needs-me")
	r.Commit(blocked, "x.txt", "1\n", "one")

	jobs := t.TempDir()
	writeState(t, jobs, "ag1", `{"name":"asking a question","tempo":"blocked",
		"updatedAt":"2026-08-04T10:00:00Z","cwd":"`+blocked+`",
		"detail":"needs a decision","children":[{"kind":"pr","href":"https://x/pull/42"}]}`)
	// An agent sitting in the base checkout must not drag it up the list.
	writeState(t, jobs, "ag2", `{"name":"in the base","tempo":"active",
		"updatedAt":"2026-08-04T11:00:00Z","cwd":"`+r.Root+`"}`)

	cfg, err := config.Load(r.Root)
	if err != nil {
		t.Fatal(err)
	}
	b := &Builder{
		Cfg:      cfg,
		Agents:   &agents.Reader{JobsDir: jobs}, // no Process: don't scan the machine
		Terminal: true,
		Sessions: func() []Session {
			return []Session{{ID: "1", Kind: "shell", CWD: quiet, Alive: true},
				{ID: "2", Kind: "shell", CWD: "/elsewhere", Alive: true}}
		},
	}
	v := b.Build()

	if v.Name != "proj" || v.Base != "main" {
		t.Errorf("name/base = %q/%q", v.Name, v.Base)
	}
	if v.StackEnabled {
		t.Error("no compose file → stack disabled")
	}
	if !v.Agent.Spawn || v.Agent.Name != "claude" {
		t.Errorf("agent caps = %+v", v.Agent)
	}
	if len(v.Work) != 3 {
		t.Fatalf("got %d items, want 3: %+v", len(v.Work), v.Work)
	}
	if v.Work[0].Branch != "needs-me" {
		t.Errorf("first = %q, want needs-me (blocked sorts first)", v.Work[0].Branch)
	}
	if last := v.Work[len(v.Work)-1]; !last.IsDefault {
		t.Errorf("last = %q, want the base checkout", last.Branch)
	}
	// The agent's PR is attached even though gh isn't available in tests, because
	// it came from the agent's own state.
	if len(v.Work[0].Agents) != 1 || v.Work[0].Tempo != "blocked" {
		t.Errorf("blocked item = %+v", v.Work[0])
	}
	if got := v.Work[0].Ahead; got != 1 {
		t.Errorf("ahead = %d, want 1", got)
	}
	// Sessions are matched to the worktree they run in, and only that one.
	for _, w := range v.Work {
		want := 0
		if w.Path == quiet {
			want = 1
		}
		if len(w.Sessions) != want {
			t.Errorf("%s has %d sessions, want %d", w.Branch, len(w.Sessions), want)
		}
	}
}

// A project with no agent at all still shows its worktrees.
func TestBuildWithoutAnAgent(t *testing.T) {
	r := testrepo.New(t, "proj")
	if err := os.WriteFile(filepath.Join(r.Root, "workmux.json"), []byte(`{"agent": null}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(r.Root)
	if err != nil {
		t.Fatal(err)
	}
	v := (&Builder{Cfg: cfg, Agents: &agents.Reader{}}).Build()
	if len(v.Work) != 1 {
		t.Fatalf("got %d items, want the primary checkout", len(v.Work))
	}
	caps := v.Agent
	if caps.Run || caps.Spawn || caps.Attach || caps.Jobs || caps.MCP {
		t.Errorf("agent caps = %+v, want all false", caps)
	}
	if caps.Name != "agent" {
		t.Errorf("name = %q, want the neutral fallback", caps.Name)
	}
}

func writeState(t *testing.T, jobs, id, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(jobs, id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobs, id, "state.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
