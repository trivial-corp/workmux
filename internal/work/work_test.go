package work

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trivial-corp/workmux/internal/agents"
	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/testrepo"
)

// The ordering the whole dashboard leans on: what you touched last, newest first.
// Status never reorders — a blocked agent on stale work stays where its activity
// puts it — and the base checkout goes last however busy it looks: it's where you
// happen to stand, not a change in flight.
func TestSortWork(t *testing.T) {
	items := []Item{
		{Branch: "main", IsDefault: true, Activity: 400, Tempo: "active", Live: true},
		{Branch: "stale-but-blocked", Activity: 100, Tempo: "blocked"},
		{Branch: "just-touched", Activity: 300},
		{Branch: "yesterday", Activity: 200, Live: true},
	}
	sortWork(items)
	want := []string{"just-touched", "yesterday", "stale-but-blocked", "main"}
	for i, w := range want {
		if items[i].Branch != w {
			t.Fatalf("order = %v, want %v", branches(items), want)
		}
	}
}

func branches(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Branch
	}
	return out
}

func TestSplitProfiles(t *testing.T) {
	cases := map[string]int{"": 0, "app": 1, "app,tools": 2, "app,,tools,": 2}
	for in, want := range cases {
		if got := splitProfiles(in); len(got) != want {
			t.Errorf("splitProfiles(%q) = %v, want %d entries", in, got, want)
		}
	}
}

// End to end against a real repository: two worktrees, the one touched last on
// top, and the base checkout must still sort last.
func TestBuildOrdersWorkAndAttachesAgents(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.FakeOrigin()
	quiet := r.Worktree("quiet-change")
	blocked := r.Worktree("needs-me")
	r.Commit(blocked, "x.txt", "1\n", "one")
	// Both worktrees were made this second; pin the quiet one into the past so the
	// ordering under test is activity, not a mtime coin toss.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(quiet, old, old); err != nil {
		t.Fatal(err)
	}

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
		ID:     "proj",
		Cfg:    cfg,
		Agents: &agents.Reader{JobsDir: jobs}, // no Process: don't scan the machine
		Sessions: func() []Session {
			return []Session{{ID: "1", Kind: "shell", CWD: quiet, Alive: true},
				{ID: "2", Kind: "shell", CWD: "/elsewhere", Alive: true}}
		},
	}
	v := b.Build()

	if v.Name != "proj" || v.Base != "main" {
		t.Errorf("name/base = %q/%q", v.Name, v.Base)
	}
	// Every item says which project it came from: one server merges several, and a
	// branch name alone doesn't identify a worktree any more.
	for _, w := range v.Work {
		if w.Project != "proj" {
			t.Errorf("item %q has project %q", w.Branch, w.Project)
		}
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
		t.Errorf("first = %q, want needs-me (touched last sorts first)", v.Work[0].Branch)
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
