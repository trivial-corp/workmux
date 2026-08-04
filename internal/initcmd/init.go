// Package initcmd is the bootstrap: it looks at a repository, says what workmux
// will do with it, and writes config only for the things it cannot infer.
//
// The point is not to generate a file. Most repos need no config at all, and a
// generated file full of restated defaults is a liability — it goes stale, and it
// hides which decisions were actually made. So this reports what was detected and
// writes the minimum, usually just the gitignored files a new worktree needs,
// which is the one thing no amount of runtime detection can work out.
package initcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/gitx"
	"github.com/trivial-corp/workmux/internal/run"
)

// Options controls a run of init.
type Options struct {
	Root   string
	DryRun bool // print the file instead of writing it
	Force  bool // overwrite an existing workmux.json
}

// candidates are files a fresh worktree usually can't work without. Only ones
// that exist *and* are gitignored are offered: anything tracked already arrives
// with the checkout.
var candidates = []string{
	".env", ".env.local", ".env.development",
	"config/master.key", "config/credentials.yml.enc",
	".envrc", "secrets.yaml", "terraform.tfvars",
}

// Run inspects Root and writes config if there's anything worth writing.
func Run(out io.Writer, o Options) error {
	root := o.Root
	if !gitx.IsRepo(root) {
		return fmt.Errorf("%s is not a git repository — workmux's unit of work is a worktree", root)
	}
	root = gitx.PrimaryRoot(root)

	existing := ""
	for _, name := range []string{"workmux.json", ".workmux.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			existing = name
			break
		}
	}
	if existing != "" && !o.Force && !o.DryRun {
		return fmt.Errorf("%s already exists — inspect it, or re-run with --force", existing)
	}

	cfg, err := config.Load(root)
	if err != nil {
		return fmt.Errorf("existing config is not valid json: %w", err)
	}

	fmt.Fprintf(out, "\n  \033[1m%s\033[0m\n", root)
	fmt.Fprintf(out, "  %-12s %s\n", "name", cfg.Name)
	fmt.Fprintf(out, "  %-12s %s\n", "base branch", gitx.DefaultBranch(root))

	trees := gitx.Worktrees(root)
	fmt.Fprintf(out, "  %-12s %s\n", "worktrees", worktreeSummary(cfg, trees))

	if cfg.HasStack() {
		fmt.Fprintf(out, "  %-12s %s · slots %s\n", "stack", cfg.Stack.Compose, cfg.Stack.Slots)
		if cfg.Stack.URL == "" {
			fmt.Fprintf(out, "  %-12s \033[2mnot set — add stack.url for an Open button\033[0m\n", "")
		}
	} else {
		fmt.Fprintf(out, "  %-12s \033[2mnone found — worktrees, agents and sessions only\033[0m\n", "stack")
	}

	fmt.Fprintf(out, "  %-12s %s\n", "agent", agentSummary(cfg))

	copies := detectCopies(root)
	if len(copies) > 0 {
		fmt.Fprintf(out, "  %-12s %s\n", "carry over", strings.Join(copies, ", "))
	}

	// Config is only worth writing for what can't be inferred at runtime.
	if len(copies) == 0 {
		fmt.Fprintf(out, "\n  \033[32mNothing to configure.\033[0m Every default fits this repo — just run \033[1mworkmux\033[0m.\n\n")
		return nil
	}

	doc := map[string]any{
		"worktrees": map[string]any{"copy": copies},
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	if o.DryRun {
		fmt.Fprintf(out, "\n  \033[2mworkmux.json (not written — this is --dry-run)\033[0m\n\n%s\n", body)
		return nil
	}
	path := filepath.Join(root, "workmux.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n  wrote \033[1mworkmux.json\033[0m — the gitignored files each new worktree needs.\n"+
		"  Everything else is derived from the repo. Run \033[1mworkmux\033[0m.\n\n")
	return nil
}

// worktreeSummary describes where new work will go and what's already there.
func worktreeSummary(cfg *config.Config, trees []gitx.Worktree) string {
	s := cfg.Worktrees.Path
	switch n := len(trees) - 1; {
	case n == 1:
		s += fmt.Sprintf(" · 1 besides the primary checkout")
	case n > 1:
		s += fmt.Sprintf(" · %d besides the primary checkout", n)
	default:
		s += " · none yet"
	}
	return s
}

// agentSummary reports whether the configured agent is actually reachable —
// registration and availability being different things, and the difference being
// silent until you click something.
func agentSummary(cfg *config.Config) string {
	if cfg.Agent.Command == "" {
		return "none configured"
	}
	exe := strings.Fields(cfg.Agent.Command)[0]
	if _, err := exec.LookPath(exe); err != nil {
		return fmt.Sprintf("%s \033[33m(not on PATH — install it, or set \"agent\": null)\033[0m", cfg.Agent.Command)
	}
	jobs := cfg.JobsDir()
	if jobs == "" {
		return cfg.Agent.Command + " \033[2m(no state directory, so no agent list)\033[0m"
	}
	if _, err := os.Stat(jobs); err != nil {
		return fmt.Sprintf("%s \033[2m(no agents yet)\033[0m", cfg.Agent.Command)
	}
	return cfg.Agent.Command
}

// detectCopies finds the files a new worktree would be missing: they exist here,
// and git ignores them, so a fresh checkout won't have them.
func detectCopies(root string) []string {
	var found []string
	for _, rel := range candidates {
		matches, err := filepath.Glob(filepath.Join(root, rel))
		if err != nil || len(matches) == 0 {
			continue
		}
		if ignored(root, rel) {
			found = append(found, rel)
		}
	}
	// Rails-shaped credential keys live under a directory that varies per app, so
	// they're worth a shallow look rather than a fixed path.
	res := run.Cmd(root, 10*time.Second, "git", "ls-files", "--others", "--ignored",
		"--exclude-standard", "--directory")
	for _, ln := range res.Lines() {
		if strings.HasSuffix(ln, ".key") && strings.Count(ln, "/") <= 4 {
			found = append(found, ln)
		}
	}
	sort.Strings(found)
	return dedupe(found)
}

func ignored(root, rel string) bool {
	// check-ignore exits 0 when the path *is* ignored.
	return run.Cmd(root, 8*time.Second, "git", "check-ignore", "-q", rel).OK()
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
