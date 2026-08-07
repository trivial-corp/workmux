package actions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/testrepo"
)

func runner(t *testing.T, r *testrepo.Repo) *Runner {
	t.Helper()
	cfg, err := config.Load(r.Root)
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{Cfg: cfg}
}

// Nobody should have to invent a branch name to start working.
func TestBranchNameComesFromTheTask(t *testing.T) {
	cases := map[string]string{
		"hotel search times out on long stays": "hotel-search-times-out",
		"we should fix the flaky login test":   "flaky-login-test",
		"Add support for WebP images":          "support-webp-images",
		// A screenshot pasted into the task leaves its path in the text, usually ahead of
		// the words. The branch is named after the work, not after the picture.
		"/var/folders/t/9z/T/workmux-pastes-501/paste-1712.png the header wraps on iPhone": "header-wraps-iphone",
	}
	for prompt, want := range cases {
		if got := slugify(nameFromTask(prompt)); got != want {
			t.Errorf("%q → %q, want %q", prompt, got, want)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Fix The Thing":     "fix-the-thing",
		"feature/sub-thing": "feature/sub-thing",
		"  spaces  here  ":  "spaces-here",
		"!!!":               "",
		"emoji 🎉 here":      "emoji-here",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewWorkCreatesAWorktreeOffTheRemote(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.FakeOrigin()
	// A commit only origin has, so "branched off the remote" is checkable.
	other := r.Worktree("temp")
	r.Commit(other, "remote-only.txt", "x\n", "remote only")
	r.GitIn(other, "push", "-q", "origin", "HEAD:main")

	out, err := runner(t, r).NewWork("", "hotel search times out on long stays", "main")
	if err != nil {
		t.Fatal(err)
	}
	if out.Branch != "hotel-search-times-out" {
		t.Errorf("branch = %q", out.Branch)
	}
	if _, err := os.Stat(out.Path); err != nil {
		t.Fatalf("no worktree at %s", out.Path)
	}
	// Off origin/main, so it has the commit the primary checkout doesn't.
	if _, err := os.Stat(filepath.Join(out.Path, "remote-only.txt")); err != nil {
		t.Error("new work didn't start level with the remote")
	}
	if out.AgentStarting {
		t.Error("no spawn configured here, so no agent should be claimed")
	}
}

func TestNewWorkNeedsSomethingToGoOn(t *testing.T) {
	r := testrepo.New(t, "proj")
	if _, err := runner(t, r).NewWork("", "", ""); err == nil {
		t.Fatal("an empty task must be refused")
	}
	if _, err := runner(t, r).NewWork("", "the a of in", ""); err == nil {
		t.Error("a task of nothing but stopwords must be refused, not become a branch called ''")
	}
}

// Two goes at the same task must not collide.
func TestNewWorkAvoidsNamesInUse(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.FakeOrigin()
	run := runner(t, r)

	first, err := run.NewWork("", "flaky login test", "main")
	if err != nil {
		t.Fatal(err)
	}
	second, err := run.NewWork("", "flaky login test", "main")
	if err != nil {
		t.Fatal(err)
	}
	if first.Branch == second.Branch {
		t.Errorf("both got %q", first.Branch)
	}
	if second.Branch != first.Branch+"-2" {
		t.Errorf("second = %q, want %s-2", second.Branch, first.Branch)
	}
}

// The files a fresh worktree can't work without, which is the one thing runtime
// detection can never work out.
func TestNewWorkCarriesGitignoredFilesOver(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.FakeOrigin()
	r.Write(".gitignore", ".env\n*.key\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "ignores")
	r.Write("workmux.json", `{"worktrees":{"copy":[".env","config/*.key"]}}`)
	r.Write(".env", "SECRET=1\n")
	r.Write("api/.env", "NESTED=1\n")
	r.Write("config/prod.key", "k\n")
	r.Write("node_modules/pkg/.env", "junk\n")

	out, err := runner(t, r).NewWork("with-files", "", "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".env", "api/.env", "config/prod.key"} {
		if _, err := os.Stat(filepath.Join(out.Path, want)); err != nil {
			t.Errorf("%s was not carried over", want)
		}
	}
	if _, err := os.Stat(filepath.Join(out.Path, "node_modules/pkg/.env")); err == nil {
		t.Error("node_modules should be pruned, not copied")
	}
	if out.Copied != 3 {
		t.Errorf("copied = %d, want 3", out.Copied)
	}
}

func TestMergeBase(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.FakeOrigin()
	wt := r.Worktree("feature")
	// origin/main moves on.
	r.Commit(r.Root, "on-main.txt", "1\n", "main moved")
	r.Git("push", "-q", "origin", "main")

	if _, err := runner(t, r).MergeBase(wt, "main"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "on-main.txt")); err != nil {
		t.Error("the base wasn't merged in")
	}
}

// A button must not leave a half-merged worktree behind.
func TestMergeBaseAbortsOnConflict(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.FakeOrigin()
	wt := r.Worktree("feature")
	r.Commit(wt, "clash.txt", "theirs\n", "ours")
	r.Commit(r.Root, "clash.txt", "mine\n", "theirs")
	r.Git("push", "-q", "origin", "main")

	_, err := runner(t, r).MergeBase(wt, "main")
	if err == nil {
		t.Fatal("a conflicting merge should be refused")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Errorf("err = %v, want it to say conflict", err)
	}
	// And the tree is clean, not mid-merge.
	if out := r.GitIn(wt, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("worktree left dirty:\n%s", out)
	}
}

func TestMergeBaseRefusesDirtyWorktree(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.FakeOrigin()
	wt := r.Worktree("feature")
	if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runner(t, r).MergeBase(wt, "main")
	if err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("err = %v, want a complaint about uncommitted changes", err)
	}
}

// Stack actions are configuration; a project without them refuses rather than
// improvising a docker command.
func TestStackRefusals(t *testing.T) {
	r := testrepo.New(t, "proj") // no compose file
	run := runner(t, r)
	if _, err := run.Stack("up", "", ""); err == nil {
		t.Error("no stack should mean no up")
	}

	r2 := testrepo.New(t, "hasstack")
	r2.Write("compose.yaml", "services: {}\n")
	run2 := runner(t, r2)
	if _, err := run2.Stack("nonsense", "hasstack1", ""); err == nil {
		t.Error("an unknown action must be refused")
	}
	if _, err := run2.Stack("up", "someone-elses-project", ""); err == nil {
		t.Error("a slot that isn't ours must be refused")
	}
}

func TestCheckoutPRNeedsANumber(t *testing.T) {
	r := testrepo.New(t, "proj")
	if _, err := runner(t, r).CheckoutPR("not a pr"); err == nil {
		t.Error("a ref with no number must be refused")
	}
}
