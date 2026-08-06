// Command workmux serves a dashboard over one or more repositories.
//
// Flags first, environment second, defaults last — and the defaults are chosen so
// that running it with no arguments in any git repo does something useful. That
// includes running it in a second repo while the first is still up: it joins the
// server that's already listening rather than starting a rival on another port.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/trivial-corp/workmux/internal/bg"
	"github.com/trivial-corp/workmux/internal/gitx"
	"github.com/trivial-corp/workmux/internal/initcmd"
	"github.com/trivial-corp/workmux/internal/instance"
	"github.com/trivial-corp/workmux/internal/project"
	"github.com/trivial-corp/workmux/internal/run"
	stackpkg "github.com/trivial-corp/workmux/internal/stack"
	"github.com/trivial-corp/workmux/internal/term"
	"github.com/trivial-corp/workmux/internal/web"
	"github.com/trivial-corp/workmux/internal/work"
)

// version is stamped at build time (-ldflags "-X main.version=…"); dev otherwise.
var version = "dev"

const usage = `workmux — run several coding agents at once, one git worktree each, from a browser.

usage: workmux [options] [DIR…]
       workmux init [--dry-run] [--yes] [--force]   look at this repo and set it up

Run it in a repository to serve that repository. Run it in a second one and it
hands that repo to the workmux already running instead of starting another
server — both projects, one page, one port.

  -p, --port N       port to listen on (default 4315)
      --host ADDR    interface to bind (default 127.0.0.1). 0.0.0.0 reaches it
                     from a phone; off-loopback requests then need a token
      --token TOK    pin the token instead of minting one (empty to disable it,
                     when something in front already authenticates)
      --root DIR     repository to serve. Repeat it, or list directories after
                     the options, to serve several (default: this directory)
      --standalone   start a server of my own instead of joining one that is
                     already running. It also doesn't become the server later
                     invocations look for first
      --no-terminal  dashboard only — don't serve shells
      --open         open a browser once it's listening
      --dev [DIR]    serve the frontend from disk (default internal/web/dist)
                     instead of the copy baked into the binary, and log as if
                     --verbose. Editing the UI becomes a refresh.
      --verbose      log every request, every subprocess and what it returned
  -h, --help         this
  -V, --version      print the version

workmux.json at each repo root is optional — every key falls back to what that
repo already says. See https://github.com/trivial-corp/workmux for the schema.
`

type options struct {
	port       int
	host       string
	token      string
	tokenSet   bool
	roots      []string
	noTerm     bool
	open       bool
	standalone bool
	dev        string
	devSet     bool
	verbose    bool
}

func main() {
	// One subcommand, and it's the bootstrap: `workmux init` reports what this
	// repo looks like and writes config only for what can't be inferred.
	if len(os.Args) > 1 && os.Args[1] == "init" {
		runInit(os.Args[2:])
		return
	}

	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "workmux: %v (try --help)\n", err)
		os.Exit(2)
	}

	roots := resolveRoots(opts.roots)
	for _, root := range roots {
		if !gitx.IsRepo(root) {
			fmt.Fprintf(os.Stderr, "workmux: %s is not a git repository.\n"+
				"The unit of work here is a worktree, so it needs one — run it inside "+
				"your project, or pass --root DIR.\n", root)
			os.Exit(1)
		}
	}

	// Somebody else may already be serving. Hand them these repositories and stop:
	// one page with everything on it is the whole point, and two servers can't do
	// that however many tabs you open.
	if !opts.standalone {
		if url, ok := runningServer(opts); ok {
			join(url, roots, opts.open)
			return
		}
	}

	projects, err := project.New(roots)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workmux: %v\n", err)
		os.Exit(1)
	}
	projects.Log = web.Journal.Note

	// git is required; docker only matters when something has a stack, and demanding
	// it otherwise turned "this project has no containers" into "won't start".
	needed := []string{"git"}
	if projects.HasStack() {
		needed = append(needed, "docker")
	}
	for _, tool := range needed {
		if _, err := exec.LookPath(tool); err != nil {
			fmt.Fprintf(os.Stderr, "workmux: '%s' is required.\n", tool)
			os.Exit(1)
		}
	}

	token := opts.token
	if !opts.tokenSet {
		token = os.Getenv("WORKMUX_TOKEN")
		if _, set := os.LookupEnv("WORKMUX_TOKEN"); !set {
			if isLoopback(opts.host) {
				token = ""
			} else {
				token = mintToken()
			}
		}
	}

	// Every fact workmux reports comes from a subprocess, so a wrong dashboard is
	// nearly always a surprising command result — print them.
	if opts.verbose || opts.devSet {
		log.SetFlags(log.Ltime)
		run.Trace = func(argv []string, dur time.Duration, code int, out string) {
			status := ""
			if code != 0 {
				status = fmt.Sprintf("  \033[31mexit %d\033[0m: %s", code, firstLine(out))
			}
			log.Printf("\033[2m%6s\033[0m %s%s", dur.Round(time.Millisecond), strings.Join(argv, " "), status)
		}
	}

	devDir := ""
	if opts.devSet {
		if abs, err := filepath.Abs(opts.dev); err == nil {
			devDir = abs
		}
		if _, err := os.Stat(filepath.Join(devDir, "index.html")); err != nil {
			fmt.Fprintf(os.Stderr, "workmux: --dev wants a directory with index.html in it; %s has none.\n"+
				"Run it from the workmux checkout, or pass --dev=path/to/frontend.\n", devDir)
			os.Exit(1)
		}
	}

	srv := &web.Server{Projects: projects, Token: token, Terminal: !opts.noTerm,
		Origins: origins(opts.host, opts.port), DevDir: devDir,
		Verbose: opts.verbose || opts.devSet}

	// Terminals hand out a shell, so they're wired only when asked for — and then
	// the routes exist; otherwise they don't, rather than answering "disabled".
	if !opts.noTerm {
		reg := term.NewRegistry()
		defer reg.Shutdown()
		srv.Sessions = &web.Sessions{Reg: reg}
		// The work list shows which sessions belong to which worktree, so it reads
		// them from the same registry rather than keeping its own idea. One registry
		// for every project: worktree paths are unique across repositories, so each
		// project's items keep only their own.
		sessions := func() []work.Session {
			var out []work.Session
			for _, i := range reg.List() {
				out = append(out, work.Session{ID: i.ID, Kind: string(i.Kind), Title: i.Title,
					CWD: i.CWD, Agent: i.Agent, Alive: i.Alive})
			}
			return out
		}
		for _, p := range projects.List() {
			p.Builder.Sessions = sessions
		}
		// A project added later needs the same wiring, and it arrives over HTTP long
		// after this line has run.
		projects.OnAdd = func(p *project.Project) { p.Builder.Sessions = sessions }

		// Ctrl-C should take the sessions with it: a PTY whose server has gone is a
		// process nobody can reach.
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-stop
			fmt.Fprint(os.Stderr, "\n  stopping sessions…\n")
			reg.Shutdown()
			instance.Clear() // stop advertising an address nothing is listening on
			bg.Wait()        // don't leave a background fetch running
			os.Exit(0)
		}()
	}

	addr := net.JoinHostPort(opts.host, strconv.Itoa(opts.port))
	shown := opts.host
	if opts.host == "0.0.0.0" || opts.host == "::" {
		shown = "127.0.0.1"
	}
	where := fmt.Sprintf("http://%s:%d", shown, opts.port)

	fmt.Fprintf(os.Stderr, "\n  \033[1mworkmux\033[0m \033[2m%s\033[0m — %s\n  \033[36m%s\033[0m\n",
		version, projectNames(projects), where)
	if token != "" {
		lan := lanAddress()
		if lan == "" {
			lan = shown
		}
		fmt.Fprintf(os.Stderr, "  \033[36mhttp://%s:%d/?t=%s\033[0m  \033[2m← open this on your phone\033[0m\n",
			lan, opts.port, token)
		fmt.Fprint(os.Stderr, "  \033[2mtoken auth on (loopback exempt). Terminals are a shell: put TLS in\n"+
			"  front before exposing this beyond a trusted network.\033[0m\n")
	} else if isLoopback(opts.host) {
		fmt.Fprint(os.Stderr, "  \033[2mloopback only — no token needed\033[0m\n")
	}
	if !projects.HasStack() {
		fmt.Fprint(os.Stderr, "  \033[2mno stack configured — worktrees, agents and sessions only\033[0m\n")
	}
	if devDir != "" {
		fmt.Fprintf(os.Stderr, "  \033[33mdev\033[0m: frontend from %s — edit and refresh\n", devDir)
	}
	// One line per repository. With several of them "which checkout is this?" is a
	// question the banner should already have answered.
	for _, p := range projects.List() {
		fmt.Fprintf(os.Stderr, "  \033[2m%-12s\033[0m %s\n", p.ID, p.Root())
	}
	if !opts.standalone {
		fmt.Fprint(os.Stderr, "  \033[2mrun workmux in another repo to add it here\033[0m\n")
	}
	fmt.Fprint(os.Stderr, "  Ctrl-C to stop.\n\n")

	// Seed the log with what this instance is. It opened on "Nothing logged yet"
	// until you happened to do something, which made it look broken rather than
	// quiet — and this is the information you want first when it misbehaves.
	web.Journal.Note("workmux %s listening on %s · terminals %s · token %s", version, addr,
		onOff(!opts.noTerm), onOff(token != ""))
	for _, p := range projects.List() {
		cfg := p.Cfg
		web.Journal.Note("serving %s (%s)", cfg.Name, cfg.Root)
		if cfg.HasStack() {
			web.Journal.Note("%s: stack %s · slots %s · next %s", cfg.Name, cfg.Stack.Compose,
				cfg.Stack.Slots, stackpkg.NextFreeSlot(cfg, stackpkg.Running(cfg)))
		}
		if cfg.Agent.Command == "" {
			web.Journal.Note("%s: no agent configured", cfg.Name)
		} else {
			web.Journal.Note("%s: agent %s · state in %s · mcp %s", cfg.Name, cfg.Agent.Command,
				or(cfg.JobsDir(), "nowhere"), onOff(cfg.Agent.MCP != ""))
		}
	}
	// Every fact this dashboard shows comes from a subprocess, so a command that
	// fails is the explanation for a wrong number. Those always get logged; the full
	// trace stays behind --verbose.
	prevTrace := run.Trace
	run.Trace = func(argv []string, dur time.Duration, code int, out string) {
		if prevTrace != nil {
			prevTrace(argv, dur, code, out)
		}
		if code != 0 {
			web.Journal.Note("%s exited %d after %s: %s", argv[0], code,
				dur.Round(time.Millisecond), firstLine(out))
		}
	}

	if opts.open {
		openBrowser(where + tokenQuery(token))
	}
	// Leave the address where the next invocation will look. A standalone server
	// doesn't: it was asked for explicitly, so it shouldn't take over as the one
	// everything else joins. (It can still be found by a later invocation aiming
	// at the port it happens to be on — one server per port is the rule, and this
	// flag is about which one this process becomes, not about hiding.)
	if !opts.standalone {
		if err := instance.Save(where); err != nil && (opts.verbose || opts.devSet) {
			log.Printf("could not record this server's address: %v", err)
		}
		defer instance.Clear()
	}
	if err := srv.Listen(addr); err != nil {
		instance.Clear()
		fmt.Fprintf(os.Stderr, "workmux: %v\n", err)
		os.Exit(1)
	}
}

// resolveRoots turns what was on the command line into repository roots.
func resolveRoots(given []string) []string {
	if len(given) == 0 {
		given = []string{"."}
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range given {
		root, err := filepath.Abs(r)
		if err != nil {
			fmt.Fprintf(os.Stderr, "workmux: %v\n", err)
			os.Exit(1)
		}
		// Through symlinks first. git reports resolved paths, and agent ownership is
		// decided by path prefix — so with /var vs /private/var (every macOS temp dir,
		// and any symlinked checkout) nothing would ever match its own worktree.
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		// Resolve to the primary checkout unless told otherwise, so running it from a
		// subdirectory — or from inside one of the worktrees it manages — works.
		if !flagGiven("root") && os.Getenv("WORKMUX_ROOT") == "" {
			root = gitx.PrimaryRoot(root)
		}
		if !seen[root] {
			seen[root] = true
			out = append(out, root)
		}
	}
	return out
}

// runningServer finds a workmux already listening, if there is one to find.
//
// Two places to look, and the order matters. An explicitly requested address is a
// statement about where the server should be, so only that is checked — asking for
// port 4400 and silently joining one on 4315 would be the wrong answer to a clear
// question. Otherwise the note a running server left behind is authoritative, and
// the default port is the fallback for when that note has been lost.
func runningServer(opts options) (string, bool) {
	if flagGiven("port") || flagGiven("host") {
		url := fmt.Sprintf("http://%s:%d", displayHost(opts.host), opts.port)
		return url, instance.Running(url)
	}
	if info, ok := instance.Load(); ok && instance.Running(info.URL) {
		return info.URL, true
	}
	url := fmt.Sprintf("http://%s:%d", displayHost(opts.host), opts.port)
	return url, instance.Running(url)
}

// join hands these repositories to the server that is already running, says what
// happened, and leaves.
func join(url string, roots []string, open bool) {
	fmt.Fprintf(os.Stderr, "\n  \033[1mworkmux\033[0m — already running at \033[36m%s\033[0m\n", url)
	failed := false
	for _, root := range roots {
		res, err := instance.Join(url, root)
		if err != nil {
			failed = true
			fmt.Fprintf(os.Stderr, "  \033[31m%s\033[0m: %v\n", filepath.Base(root), err)
			continue
		}
		what := "already there"
		if res.Added {
			what = "added"
		}
		fmt.Fprintf(os.Stderr, "  \033[2m%s\033[0m %s \033[2m(%s)\033[0m\n", what, res.Name, res.ID)
	}
	fmt.Fprint(os.Stderr, "  \033[2mopen the page above — everything is on it.\n"+
		"  workmux --standalone starts a separate server instead.\033[0m\n\n")
	if open {
		openBrowser(url)
	}
	if failed {
		os.Exit(1)
	}
}

// projectNames is the banner's one-line answer to "what is this serving".
func projectNames(set *project.Set) string {
	var names []string
	for _, p := range set.List() {
		names = append(names, p.Name())
	}
	return strings.Join(names, ", ")
}

// displayHost is the address to actually talk to: a server bound to everything is
// still reached over loopback from the same machine.
func displayHost(host string) string {
	if host == "0.0.0.0" || host == "::" || host == "" {
		return "127.0.0.1"
	}
	return host
}

func runInit(argv []string) {
	o := initcmd.Options{Root: "."}
	// Interactive only when there's a person there. A pipe, a CI job or an agent
	// gets the same run without questions, and takes the defaults.
	if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		o.In = os.Stdin
	}
	for _, a := range argv {
		switch a {
		case "--dry-run", "-n":
			o.DryRun = true
		case "--yes", "-y":
			o.Yes = true
		case "--force", "-f":
			o.Force = true
		case "-h", "--help":
			fmt.Print(usage)
			return
		default:
			fmt.Fprintf(os.Stderr, "workmux init: unknown option %s\n", a)
			os.Exit(2)
		}
	}
	root, err := filepath.Abs(o.Root)
	if err == nil {
		if resolved, e := filepath.EvalSymlinks(root); e == nil {
			root = resolved
		}
		o.Root = root
	}
	if err := initcmd.Run(os.Stdout, o); err != nil {
		fmt.Fprintf(os.Stderr, "workmux init: %v\n", err)
		os.Exit(1)
	}
}

// given tracks which flags were passed, so environment fallbacks only apply when
// the flag wasn't.
var given = map[string]bool{}

func flagGiven(name string) bool { return given[name] }

// parseArgs accepts the long and short forms people actually type, including
// --port=8080. flag.Parse is avoided because it can't do "-p" and "--port" both.
func parseArgs(argv []string) (options, error) {
	o := options{port: 4315, host: "127.0.0.1"}
	if v := os.Getenv("WORKMUX_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			o.port = n
		}
	}
	if v := os.Getenv("WORKMUX_HOST"); v != "" {
		o.host = v
	}
	// A list, because the environment has to be able to say what the flag can:
	// separated the way PATH is, so WORKMUX_ROOT=~/a:~/b serves both.
	if v := os.Getenv("WORKMUX_ROOT"); v != "" {
		for _, part := range filepath.SplitList(v) {
			if part != "" {
				o.roots = append(o.roots, part)
			}
		}
	}
	if os.Getenv("WORKMUX_TERMINAL") == "0" {
		o.noTerm = true
	}

	for i := 0; i < len(argv); i++ {
		a := argv[i]
		name, inline, hasInline := strings.Cut(a, "=")
		value := func() (string, error) {
			if hasInline {
				return inline, nil
			}
			if i+1 >= len(argv) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return argv[i], nil
		}
		switch name {
		case "-h", "--help":
			fmt.Print(usage)
			os.Exit(0)
		case "-V", "--version":
			fmt.Printf("workmux %s\n", version)
			os.Exit(0)
		case "--no-terminal":
			o.noTerm = true
		case "--open":
			o.open = true
		case "--standalone":
			o.standalone = true
		case "--verbose":
			o.verbose = true
		case "--dev":
			// A bare --dev means "the frontend in this checkout"; --dev=DIR points
			// somewhere else (a built frontend, another branch, a scratch copy).
			o.devSet = true
			o.dev = defaultDevDir
			if hasInline {
				o.dev = inline
			}
		case "-p", "--port":
			v, err := value()
			if err != nil {
				return o, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 65535 {
				return o, fmt.Errorf("--port wants a number")
			}
			o.port, given["port"] = n, true
		case "--host":
			v, err := value()
			if err != nil {
				return o, err
			}
			o.host, given["host"] = v, true
		case "--token":
			v, err := value()
			if err != nil {
				return o, err
			}
			o.token, o.tokenSet, given["token"] = v, true, true
		case "--root":
			v, err := value()
			if err != nil {
				return o, err
			}
			// Repeatable: several roots is the normal case now, and "--root a --root b"
			// reads better than inventing a separator.
			if !given["root"] {
				o.roots = nil // an explicit root replaces WORKMUX_ROOT rather than adding to it
			}
			o.roots, given["root"] = append(o.roots, v), true
		default:
			// A bare directory is a root. `workmux ~/code/a ~/code/b` is what people
			// try first, and refusing it to protect a flag nobody typed is pedantry.
			if strings.HasPrefix(name, "-") {
				return o, fmt.Errorf("unknown option %s", name)
			}
			if !given["root"] {
				o.roots = nil
			}
			o.roots, given["root"] = append(o.roots, a), true
		}
	}
	return o, nil
}

// defaultDevDir is where the frontend lives in the source tree, relative to the
// repo root — which is where `go run ./cmd/workmux` puts you.
const defaultDevDir = "internal/web/dist"

func isLoopback(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost", "0:0:0:0:0:0:0:1":
		return true
	}
	return false
}

func mintToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Without randomness a token would be a false promise; better to refuse.
		fmt.Fprintln(os.Stderr, "workmux: no source of randomness for a token")
		os.Exit(1)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func tokenQuery(token string) string {
	if token == "" {
		return ""
	}
	return "/?t=" + token
}

// origins is the allowlist for WebSocket upgrades: the addresses this instance is
// actually reachable at, and nothing else.
func origins(host string, port int) []string {
	hosts := []string{"127.0.0.1", "localhost", "[::1]"}
	if !isLoopback(host) {
		hosts = append(hosts, host)
		if lan := lanAddress(); lan != "" {
			hosts = append(hosts, lan)
		}
	}
	var out []string
	for _, h := range hosts {
		out = append(out, fmt.Sprintf("http://%s:%d", h, port), fmt.Sprintf("https://%s:%d", h, port))
	}
	return out
}

// lanAddress is this machine's address on the local network, for the phone URL.
func lanAddress() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if v4 := ipnet.IP.To4(); v4 != nil {
				return v4.String()
			}
		}
	}
	return ""
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// firstLine keeps a failure to one line in the trace; the full output is still
// what the caller sees.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func openBrowser(url string) {
	for _, opener := range []string{"open", "xdg-open"} {
		if path, err := exec.LookPath(opener); err == nil {
			_ = exec.Command(path, url).Start()
			return
		}
	}
}
