package agents

import (
	"os"
	"path/filepath"
	"testing"
)

// The bug this guards: worktrees live *inside* the primary checkout, so
// "is this directory under that worktree" is true of the primary for every
// worktree — which marked the base checkout permanently busy.
func TestLongestOwnerPrefersTheDeepestMatch(t *testing.T) {
	primary := "/repo"
	wt := "/repo/.claude/worktrees/feature"
	trees := []string{primary, wt, "/repo/.claude/worktrees/other"}

	cases := []struct {
		dir  string
		want string
	}{
		{wt, wt},                             // the worktree itself
		{wt + "/src/deep", wt},               // inside it
		{primary, primary},                   // the primary itself
		{primary + "/app", primary},          // inside the primary, not any worktree
		{"/repo-other", ""},                  // a sibling that merely shares a prefix
		{"/repo/.claude/worktrees", primary}, // the container is not a worktree
		{"", ""},
		{"/elsewhere", ""},
	}
	for _, c := range cases {
		if got := LongestOwner(c.dir, trees); got != c.want {
			t.Errorf("LongestOwner(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

func TestLongestOwnerIgnoresEmptyEntries(t *testing.T) {
	if got := LongestOwner("/a/b", []string{"", "/a"}); got != "/a" {
		t.Errorf("got %q, want /a", got)
	}
}

// jobs writes a fake agent-state directory, the shape an agent CLI leaves behind.
func jobs(t *testing.T, states map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for id, body := range states {
		if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id, "state.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Both ways an agent lands in a worktree record it differently: spawned there
// (cwd) versus moved there itself (worktreePath). Either one must count.
func TestSnapshotOwnershipFromEitherField(t *testing.T) {
	primary, wt := "/repo", "/repo/.claude/worktrees/feature"
	dir := jobs(t, map[string]string{
		"aaa111": `{"name":"spawned here","cwd":"` + wt + `","tempo":"idle","updatedAt":"2026-08-01T10:00:00Z"}`,
		"bbb222": `{"name":"moved here","worktreePath":"` + wt + `","cwd":"` + primary + `","tempo":"active","updatedAt":"2026-08-02T10:00:00Z"}`,
		"ccc333": `{"name":"someone else's repo","cwd":"/other/place","tempo":"active"}`,
		"ddd444": `not json at all`,
	})
	r := &Reader{JobsDir: dir}
	got := r.Snapshot([]string{primary, wt})

	if len(got) != 2 {
		t.Fatalf("got %d agents, want 2: %+v", len(got), got)
	}
	for _, a := range got {
		if a.Home != wt {
			t.Errorf("agent %s home = %q, want %q", a.ID, a.Home, wt)
		}
	}
	// worktreePath is the more specific claim, so it wins over cwd.
	if got[0].Tempo != "active" {
		t.Errorf("expected the active agent first, got %+v", got)
	}
}

func TestSnapshotOrdersByAttentionThenRecency(t *testing.T) {
	wt := "/repo"
	dir := jobs(t, map[string]string{
		"i1": `{"cwd":"/repo","tempo":"idle","updatedAt":"2026-08-03T10:00:00Z"}`,
		"a1": `{"cwd":"/repo","tempo":"active","updatedAt":"2026-08-01T10:00:00Z"}`,
		"b1": `{"cwd":"/repo","tempo":"blocked","updatedAt":"2026-07-01T10:00:00Z"}`,
		"i2": `{"cwd":"/repo","tempo":"idle","updatedAt":"2026-08-04T10:00:00Z"}`,
	})
	got := (&Reader{JobsDir: dir}).Snapshot([]string{wt})
	want := []string{"b1", "a1", "i2", "i1"} // blocked, active, then newest idle
	if len(got) != len(want) {
		t.Fatalf("got %d agents, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("position %d = %s, want %s (full: %+v)", i, got[i].ID, id, got)
		}
	}
}

// One task commonly yields several PRs (a fix, its follow-up, the revert), from
// both structured children and the agent's own status line.
func TestSnapshotCollectsPRs(t *testing.T) {
	dir := jobs(t, map[string]string{
		"x1": `{"cwd":"/repo","detail":"opened PR #812, superseding PR 799",
		        "children":[{"kind":"pr","href":"https://github.com/o/r/pull/803"},
		                    {"href":"https://github.com/o/r/pull/803"},
		                    {"href":"https://example.com/artifact","title":"a page"}]}`,
	})
	got := (&Reader{JobsDir: dir}).Snapshot([]string{"/repo"})
	if len(got) != 1 {
		t.Fatalf("got %d agents", len(got))
	}
	want := []int{803, 812, 799}
	if len(got[0].PRs) != len(want) {
		t.Fatalf("prs = %v, want %v", got[0].PRs, want)
	}
	for i, n := range want {
		if got[0].PRs[i] != n {
			t.Errorf("prs = %v, want %v", got[0].PRs, want)
		}
	}
}

// No agent configured must mean no agent list, and no attempt to read anything.
func TestNoJobsDirMeansNoAgents(t *testing.T) {
	r := &Reader{}
	if got := r.Snapshot([]string{"/repo"}); len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
	if got := r.LiveDirs([]string{"/repo"}); len(got) != 0 {
		t.Errorf("live = %v, want none", got)
	}
}

func TestMissingJobsDirIsNotFatal(t *testing.T) {
	r := &Reader{JobsDir: filepath.Join(t.TempDir(), "nope")}
	if got := r.Snapshot([]string{"/repo"}); len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

func TestInvalidateForcesAReread(t *testing.T) {
	dir := jobs(t, map[string]string{"a": `{"cwd":"/repo"}`})
	r := &Reader{JobsDir: dir}
	if len(r.Snapshot([]string{"/repo"})) != 1 {
		t.Fatal("setup")
	}
	// A second agent appears; the cache would hide it for a few seconds.
	if err := os.MkdirAll(filepath.Join(dir, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b", "state.json"), []byte(`{"cwd":"/repo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := len(r.Snapshot([]string{"/repo"})); got != 1 {
		t.Errorf("cache not in effect: got %d", got)
	}
	r.Invalidate()
	if got := len(r.Snapshot([]string{"/repo"})); got != 2 {
		t.Errorf("after Invalidate got %d agents, want 2", got)
	}
}
