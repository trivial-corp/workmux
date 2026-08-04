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
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
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
	// In is where answers come from. Nil, or a stdout that isn't a terminal, runs
	// it non-interactively — which is what a script or an agent gets.
	In io.Reader
	// Yes takes the default answer to everything.
	Yes bool
}

// ask prints a question and returns the answer, or def when there's nobody to ask.
// Deliberately prompts rather than taking over the screen: this is a handful of
// questions once, and a full-screen TUI for it would be a dependency and a
// surprise — the actual interface is a browser.
type prompter struct {
	out io.Writer
	in  *bufio.Reader
}

func newPrompter(out io.Writer, o Options) *prompter {
	if o.Yes || o.DryRun || o.In == nil {
		return &prompter{out: out}
	}
	return &prompter{out: out, in: bufio.NewReader(o.In)}
}

func (p *prompter) interactive() bool { return p.in != nil }

// yesNo asks a yes/no question. def is the answer for a bare Enter.
func (p *prompter) yesNo(q string, def bool) bool {
	if !p.interactive() {
		return def
	}
	hint := "[Y/n]"
	if !def {
		hint = "[y/N]"
	}
	fmt.Fprintf(p.out, "  \033[1m?\033[0m %s \033[2m%s\033[0m ", q, hint)
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}

// text asks for a value; empty means "leave it out". valid rejects answers that
// would be worse than nothing — an unusable value recorded in config is harder to
// notice than a missing one, and this prompt is right next to a stray "y" from the
// question before it.
func (p *prompter) text(q, placeholder string, valid func(string) string) string {
	if !p.interactive() {
		return ""
	}
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprintf(p.out, "  \033[1m?\033[0m %s \033[2m%s\033[0m ", q, placeholder)
		line, err := p.in.ReadString('\n')
		answer := strings.TrimSpace(line)
		if answer == "" {
			return "" // deliberately skipped, or nothing left to read
		}
		if valid == nil {
			return answer
		}
		if complaint := valid(answer); complaint != "" {
			fmt.Fprintf(p.out, "    \033[33m%s\033[0m\n", complaint)
			if err != nil {
				return "" // no more input to correct it with
			}
			continue
		}
		return answer
	}
	fmt.Fprintf(p.out, "    \033[2mskipping — add stack.url to workmux.json later\033[0m\n")
	return ""
}

// validURL is the check for "where does the app open". Only absolute http(s) URLs:
// the value becomes a link in the dashboard, so a hostname or a stray keystroke
// would render a button that goes nowhere.
func validURL(s string) string {
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return "needs to start with http:// or https://"
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "that isn't a URL I can link to"
	}
	return ""
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

	p := newPrompter(out, o)
	if p.interactive() {
		fmt.Fprintln(out)
	}

	doc := map[string]any{}

	// Only ever ask about things that can't be derived. Everything else is already
	// correct, and a question about it would just be a chance to get it wrong.
	if len(copies) > 0 {
		if p.yesNo("Copy these into every new worktree?", true) {
			doc["worktrees"] = map[string]any{"copy": copies}
		}
	}
	if cfg.HasStack() && cfg.Stack.URL == "" {
		// Never guessed: a wrong link is worse than no button.
		if u := p.text("Where does the app open? (blank to skip)",
			"e.g. http://localhost:8080", validURL); u != "" {
			doc["stack"] = map[string]any{"url": u}
		}
	}
	if cfg.Agent.Command != "" && !onPath(cfg.Agent.Command) {
		if p.yesNo(fmt.Sprintf("%s isn't installed. Record that this project has no agent?",
			strings.Fields(cfg.Agent.Command)[0]), false) {
			doc["agent"] = nil
		}
	}

	// Config is only worth writing for what can't be inferred at runtime.
	if len(doc) == 0 {
		fmt.Fprintf(out, "\n  \033[32mNothing to configure.\033[0m Every default fits this repo — just run \033[1mworkmux\033[0m.\n\n")
		return nil
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
	if p.interactive() {
		fmt.Fprintf(out, "\n%s\n", body)
		if !p.yesNo("Write workmux.json?", true) {
			fmt.Fprintf(out, "\n  nothing written.\n\n")
			return nil
		}
	}
	path := filepath.Join(root, "workmux.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n  wrote \033[1mworkmux.json\033[0m with %s.\n"+
		"  Everything else is derived from the repo. Run \033[1mworkmux\033[0m.\n\n", strings.Join(wrote(doc), " and "))
	return nil
}

// wrote names what ended up in the file, so the closing line says something true
// rather than a fixed sentence.
func wrote(doc map[string]any) []string {
	var out []string
	if _, ok := doc["worktrees"]; ok {
		out = append(out, "the files each new worktree needs")
	}
	if _, ok := doc["stack"]; ok {
		out = append(out, "where the app opens")
	}
	if v, ok := doc["agent"]; ok && v == nil {
		out = append(out, "no agent for this project")
	}
	sort.Strings(out)
	return out
}

func onPath(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	_, err := exec.LookPath(fields[0])
	return err == nil
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
