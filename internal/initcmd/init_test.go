package initcmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trivial-corp/workmux/internal/testrepo"
)

// The common case: a repo where every default fits. Init must say so and write
// nothing — a file full of restated defaults goes stale and hides real decisions.
func TestNothingToConfigure(t *testing.T) {
	r := testrepo.New(t, "plain")
	var out bytes.Buffer
	if err := Run(&out, Options{Root: r.Root}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Nothing to configure") {
		t.Errorf("output was:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(r.Root, "workmux.json")); err == nil {
		t.Error("init wrote a config file it didn't need")
	}
}

func TestReportsWhatItFound(t *testing.T) {
	r := testrepo.New(t, "shop")
	r.Write("compose.yaml", "services: {}\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "compose")
	r.Worktree("feature-a")

	var out bytes.Buffer
	if err := Run(&out, Options{Root: r.Root}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"shop", "main", "compose.yaml", "shop{n}", "1 besides"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	// It should point out the missing Open button rather than inventing a URL.
	if !strings.Contains(s, "stack.url") {
		t.Errorf("no note about stack.url:\n%s", s)
	}
}

// The one thing runtime detection can never work out: which gitignored files a
// fresh worktree needs. That's what init exists to write.
func TestWritesTheCarryOverList(t *testing.T) {
	r := testrepo.New(t, "app")
	r.Write(".gitignore", ".env\n*.key\n")
	r.Write(".env", "SECRET=1\n")
	r.Write("config/credentials/prod.key", "k\n")
	r.Write("tracked.txt", "in git\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "ignore rules")

	var out bytes.Buffer
	if err := Run(&out, Options{Root: r.Root}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(r.Root, "workmux.json"))
	if err != nil {
		t.Fatalf("no workmux.json written: %v", err)
	}
	var doc struct {
		Worktrees struct {
			Copy []string `json:"copy"`
		} `json:"worktrees"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(doc.Worktrees.Copy, " ")
	if !strings.Contains(got, ".env") {
		t.Errorf("copy = %v, want the gitignored .env", doc.Worktrees.Copy)
	}
	if !strings.Contains(got, "prod.key") {
		t.Errorf("copy = %v, want the gitignored key", doc.Worktrees.Copy)
	}
	// Tracked files arrive with the checkout; suggesting them would be noise.
	if strings.Contains(got, "tracked.txt") {
		t.Errorf("copy = %v, must not include tracked files", doc.Worktrees.Copy)
	}
	// And nothing else: no restated defaults.
	var all map[string]any
	if err := json.Unmarshal(body, &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("wrote %v, want only worktrees", keys(all))
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	r := testrepo.New(t, "app")
	r.Write(".gitignore", ".env\n")
	r.Write(".env", "X=1\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "ignore")

	var out bytes.Buffer
	if err := Run(&out, Options{Root: r.Root, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not written") {
		t.Errorf("dry run should say so:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(r.Root, "workmux.json")); err == nil {
		t.Error("--dry-run wrote the file")
	}
}

// Someone's existing config is not init's to overwrite by accident.
func TestRefusesToClobber(t *testing.T) {
	r := testrepo.New(t, "app")
	r.Write("workmux.json", `{"name":"handwritten"}`)
	r.Write(".gitignore", ".env\n")
	r.Write(".env", "X=1\n")

	var out bytes.Buffer
	err := Run(&out, Options{Root: r.Root})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should name the way out: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(r.Root, "workmux.json"))
	if !strings.Contains(string(body), "handwritten") {
		t.Error("the existing file was modified")
	}
	// With --force it may proceed.
	if err := Run(&out, Options{Root: r.Root, Force: true}); err != nil {
		t.Fatalf("--force: %v", err)
	}
	body, _ = os.ReadFile(filepath.Join(r.Root, "workmux.json"))
	if strings.Contains(string(body), "handwritten") {
		t.Error("--force should have replaced it")
	}
}

func TestNotARepo(t *testing.T) {
	var out bytes.Buffer
	err := Run(&out, Options{Root: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "git repository") {
		t.Errorf("err = %v, want a clear complaint about git", err)
	}
}

// A broken config is a stop, not something to silently replace.
func TestInvalidExistingConfig(t *testing.T) {
	r := testrepo.New(t, "app")
	r.Write("workmux.json", `{"name": }`)
	var out bytes.Buffer
	if err := Run(&out, Options{Root: r.Root, Force: true}); err == nil {
		t.Fatal("expected an error about invalid json")
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Interactive: the answers decide what gets written, and a "no" writes nothing.
func TestInteractiveAcceptsEverything(t *testing.T) {
	r := testrepo.New(t, "app")
	r.Write(".gitignore", ".env\n")
	r.Write(".env", "X=1\n")
	r.Write("compose.yaml", "services: {}\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "setup")

	var out bytes.Buffer
	// copy? yes · url? given · write? yes
	in := strings.NewReader("y\nhttp://localhost:8080\ny\n")
	if err := Run(&out, Options{Root: r.Root, In: in}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(r.Root, "workmux.json"))
	if err != nil {
		t.Fatalf("nothing written: %v\n%s", err, out.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["worktrees"] == nil {
		t.Errorf("no copy list: %s", body)
	}
	st, ok := doc["stack"].(map[string]any)
	if !ok || st["url"] != "http://localhost:8080" {
		t.Errorf("url not recorded: %s", body)
	}
	// It asked, rather than guessing.
	if !strings.Contains(out.String(), "Where does the app open?") {
		t.Errorf("no url question asked:\n%s", out.String())
	}
}

func TestInteractiveDeclineWritesNothing(t *testing.T) {
	r := testrepo.New(t, "app")
	r.Write(".gitignore", ".env\n")
	r.Write(".env", "X=1\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "setup")

	var out bytes.Buffer
	// copy? yes · write? no
	if err := Run(&out, Options{Root: r.Root, In: strings.NewReader("y\nn\n")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Root, "workmux.json")); err == nil {
		t.Error("declining the write still wrote the file")
	}
	if !strings.Contains(out.String(), "nothing written") {
		t.Errorf("should say so:\n%s", out.String())
	}
}

// Declining every question means there's nothing to configure, not an empty file.
func TestInteractiveDeclineEverything(t *testing.T) {
	r := testrepo.New(t, "app")
	r.Write(".gitignore", ".env\n")
	r.Write(".env", "X=1\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "setup")

	var out bytes.Buffer
	if err := Run(&out, Options{Root: r.Root, In: strings.NewReader("n\n")}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Nothing to configure") {
		t.Errorf("output:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(r.Root, "workmux.json")); err == nil {
		t.Error("wrote an empty config")
	}
}

// A bare Enter takes the default: yes to the copy list, skip the url.
func TestInteractiveDefaults(t *testing.T) {
	r := testrepo.New(t, "app")
	r.Write(".gitignore", ".env\n")
	r.Write(".env", "X=1\n")
	r.Write("compose.yaml", "services: {}\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "setup")

	var out bytes.Buffer
	if err := Run(&out, Options{Root: r.Root, In: strings.NewReader("\n\n\n")}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(r.Root, "workmux.json"))
	if err != nil {
		t.Fatalf("nothing written: %v", err)
	}
	if strings.Contains(string(body), "stack") {
		t.Errorf("a skipped url must not be written: %s", body)
	}
	if !strings.Contains(string(body), ".env") {
		t.Errorf("the copy list should default to yes: %s", body)
	}
}

// A pipe or an agent gets no questions and the same result as before.
func TestNonInteractiveIsUnchanged(t *testing.T) {
	r := testrepo.New(t, "app")
	r.Write(".gitignore", ".env\n")
	r.Write(".env", "X=1\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "setup")

	var out bytes.Buffer
	if err := Run(&out, Options{Root: r.Root}); err != nil { // no In
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "?") {
		t.Errorf("asked a question with nobody there:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(r.Root, "workmux.json")); err != nil {
		t.Error("non-interactive should still write the copy list")
	}
}

// --yes is interactive input's opposite: take defaults, ask nothing.
func TestYesSkipsPrompts(t *testing.T) {
	r := testrepo.New(t, "app")
	r.Write(".gitignore", ".env\n")
	r.Write(".env", "X=1\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "setup")

	var out bytes.Buffer
	in := strings.NewReader("n\nn\nn\n") // would decline everything, if it were read
	if err := Run(&out, Options{Root: r.Root, In: in, Yes: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Root, "workmux.json")); err != nil {
		t.Error("--yes should have written the defaults")
	}
}

// A URL that isn't one must not be recorded: it becomes a link in the dashboard,
// and this prompt sits right after a yes/no question whose stray "y" can land here.
func TestURLPromptRejectsNonsense(t *testing.T) {
	if got := validURL("y"); got == "" {
		t.Error(`"y" must be rejected as a URL`)
	}
	if got := validURL("localhost:8080"); got == "" {
		t.Error("a bare host:port must be rejected — it isn't linkable")
	}
	if got := validURL("http://"); got == "" {
		t.Error("a scheme with no host must be rejected")
	}
	for _, ok := range []string{"http://localhost:8080", "https://app.example.com", "http://trip2.localhost"} {
		if got := validURL(ok); got != "" {
			t.Errorf("validURL(%q) = %q, want accepted", ok, got)
		}
	}
}

func TestInteractiveRetriesABadURL(t *testing.T) {
	r := testrepo.New(t, "app")
	r.Write("compose.yaml", "services: {}\n")
	r.Write(".gitignore", ".env\n")
	r.Write(".env", "X=1\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "setup")

	var out bytes.Buffer
	// copy? yes · url? nonsense, then a real one · write? yes
	in := strings.NewReader("y\nnope\nhttp://localhost:3000\ny\n")
	if err := Run(&out, Options{Root: r.Root, In: in}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "needs to start with http") {
		t.Errorf("no complaint about the bad url:\n%s", out.String())
	}
	body, err := os.ReadFile(filepath.Join(r.Root, "workmux.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "http://localhost:3000") {
		t.Errorf("the corrected url wasn't recorded: %s", body)
	}
	if strings.Contains(string(body), "nope") {
		t.Errorf("the rejected value was written anyway: %s", body)
	}
}
