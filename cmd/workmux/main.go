// Command workmux serves one repository's dashboard.
//
// Flags first, environment second, defaults last — and the defaults are chosen so
// that running it with no arguments in any git repo does something useful.
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

	"github.com/trivial-corp/workmux/internal/actions"
	"github.com/trivial-corp/workmux/internal/agents"
	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/gitx"
	"github.com/trivial-corp/workmux/internal/initcmd"
	"github.com/trivial-corp/workmux/internal/mcp"
	"github.com/trivial-corp/workmux/internal/presets"
	"github.com/trivial-corp/workmux/internal/prs"
	"github.com/trivial-corp/workmux/internal/run"
	"github.com/trivial-corp/workmux/internal/term"
	"github.com/trivial-corp/workmux/internal/web"
	"github.com/trivial-corp/workmux/internal/work"
)

// version is stamped at build time (-ldflags "-X main.version=…"); dev otherwise.
var version = "dev"

const usage = `workmux — run several coding agents at once, one git worktree each, from a browser.

usage: workmux [options]
       workmux init [--dry-run] [--yes] [--force]   look at this repo and set it up

  -p, --port N       port to listen on (default 4315)
      --host ADDR    interface to bind (default 127.0.0.1). 0.0.0.0 reaches it
                     from a phone; off-loopback requests then need a token
      --token TOK    pin the token instead of minting one (empty to disable it,
                     when something in front already authenticates)
      --root DIR     repository to serve (default: the current directory)
      --no-terminal  dashboard only — don't serve shells
      --open         open a browser once it's listening
      --dev [DIR]    serve the frontend from disk (default internal/web/dist)
                     instead of the copy baked into the binary, and log as if
                     --verbose. Editing the UI becomes a refresh.
      --verbose      log every request, every subprocess and what it returned
  -h, --help         this
  -V, --version      print the version

workmux.json at the repo root is optional — each key falls back to what the repo
already says. See https://github.com/trivial-corp/workmux for the schema.
`

type options struct {
	port     int
	host     string
	token    string
	tokenSet bool
	root     string
	noTerm   bool
	open     bool
	dev      string
	devSet   bool
	verbose  bool
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

	root, err := filepath.Abs(opts.root)
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
	if !gitx.IsRepo(root) {
		fmt.Fprintf(os.Stderr, "workmux: %s is not a git repository.\n"+
			"The unit of work here is a worktree, so it needs one — run it inside "+
			"your project, or pass --root DIR.\n", root)
		os.Exit(1)
	}

	cfg, err := config.Load(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workmux: workmux.json is not valid json: %v\n", err)
		os.Exit(1)
	}

	// git is required; docker only matters when there's a stack, and demanding it
	// otherwise turned "this project has no containers" into "won't start".
	needed := []string{"git"}
	if cfg.HasStack() {
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

	reader := &agents.Reader{JobsDir: cfg.JobsDir(), Process: cfg.Agent.Process}
	builder := &work.Builder{Cfg: cfg, Agents: reader, Terminal: !opts.noTerm}
	srv := &web.Server{Cfg: cfg, Builder: builder, Token: token,
		Origins: origins(opts.host, opts.port), DevDir: devDir,
		Verbose: opts.verbose || opts.devSet}

	// Everything that changes something, and the agent's server registry. Both read
	// their commands from config, so a project that can't do one of them offers
	// nothing rather than a button that fails.
	srv.MCP = &mcp.Reader{Cfg: cfg}
	runner := &actions.Runner{Cfg: cfg, Invalidate: func() {
		reader.Invalidate()
		prs.Invalidate()
	}}
	srv.Actions = &web.Actions{Runner: runner}

	// Terminals hand out a shell, so they're wired only when asked for — and then
	// the routes exist; otherwise they don't, rather than answering "disabled".
	if !opts.noTerm {
		reg := term.NewRegistry()
		defer reg.Shutdown()
		p := presets.Deps{Cfg: cfg, SlotFor: builder.SlotFor}
		srv.Sessions = &web.Sessions{
			Reg:      reg,
			Presets:  p.Spec,
			KnownDir: builder.IsWorktree,
		}
		srv.Agents = builder.AgentFor
		// New work starts its agent through a session-less spawn, so it survives the
		// request that asked for it.
		runner.Spawn = func(cwd, prompt string) (string, error) {
			cmd := cfg.SpawnCmd(prompt)
			if cmd == "" {
				return "", nil
			}
			res := run.Env(cwd, append(os.Environ(), "PATH="+mcp.UserPath()),
				3*time.Minute, os.Getenv("SHELL"), "-lc", cmd)
			if !res.OK() {
				web.Journal.Note("agent spawn failed in %s: %s", filepath.Base(cwd),
					res.LastLine("no output"))
				return "", fmt.Errorf("%s", res.LastLine("agent spawn failed"))
			}
			web.Journal.Note("agent started in %s", filepath.Base(cwd))
			return res.Out, nil
		}
		// The work list shows which sessions belong to which worktree, so it reads
		// them from the same registry rather than keeping its own idea.
		builder.Sessions = func() []work.Session {
			var out []work.Session
			for _, i := range reg.List() {
				out = append(out, work.Session{ID: i.ID, Kind: string(i.Kind), Title: i.Title,
					CWD: i.CWD, Agent: i.Agent, Alive: i.Alive})
			}
			return out
		}
		// Ctrl-C should take the sessions with it: a PTY whose server has gone is a
		// process nobody can reach.
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-stop
			fmt.Fprint(os.Stderr, "\n  stopping sessions…\n")
			reg.Shutdown()
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
		version, cfg.Name, where)
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
	if !cfg.HasStack() {
		fmt.Fprint(os.Stderr, "  \033[2mno stack configured — worktrees, agents and sessions only\033[0m\n")
	}
	if devDir != "" {
		fmt.Fprintf(os.Stderr, "  \033[33mdev\033[0m: frontend from %s — edit and refresh\n", devDir)
	}
	fmt.Fprintf(os.Stderr, "  root: %s\n  Ctrl-C to stop.\n\n", root)

	if opts.open {
		openBrowser(where + tokenQuery(token))
	}
	if err := srv.Listen(addr); err != nil {
		fmt.Fprintf(os.Stderr, "workmux: %v\n", err)
		os.Exit(1)
	}
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
	o := options{port: 4315, host: "127.0.0.1", root: "."}
	if v := os.Getenv("WORKMUX_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			o.port = n
		}
	}
	if v := os.Getenv("WORKMUX_HOST"); v != "" {
		o.host = v
	}
	if v := os.Getenv("WORKMUX_ROOT"); v != "" {
		o.root = v
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
			o.root, given["root"] = v, true
		default:
			return o, fmt.Errorf("unknown option %s", name)
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
