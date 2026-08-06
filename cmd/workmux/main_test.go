package main

import (
	"os"
	"testing"

	"github.com/trivial-corp/workmux/internal/instance"
	"github.com/trivial-corp/workmux/internal/testrepo"
)

func reset() {
	given = map[string]bool{}
	for _, k := range []string{"WORKMUX_PORT", "WORKMUX_HOST", "WORKMUX_ROOT", "WORKMUX_TERMINAL"} {
		os.Unsetenv(k)
	}
}

func TestDefaults(t *testing.T) {
	reset()
	o, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.port != 4315 || o.host != "127.0.0.1" || len(o.roots) != 0 {
		t.Errorf("defaults = %+v", o)
	}
	if o.noTerm || o.open || o.tokenSet || o.standalone {
		t.Errorf("defaults = %+v", o)
	}
	// No root given means "here", resolved later — the parser leaves it empty so
	// the environment still gets a say.
	if got := resolveRoots(o.roots); len(got) != 1 {
		t.Errorf("resolveRoots(nil) = %v, want one root", got)
	}
}

// Several repositories in one server is the ordinary case, so both ways of asking
// for them have to work — and an explicit root replaces the environment rather
// than piling onto it.
func TestSeveralRoots(t *testing.T) {
	reset()
	o, err := parseArgs([]string{"--root", "/tmp/a", "--root=/tmp/b", "/tmp/c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.roots) != 3 || o.roots[0] != "/tmp/a" || o.roots[2] != "/tmp/c" {
		t.Errorf("roots = %v", o.roots)
	}

	reset()
	defer reset()
	os.Setenv("WORKMUX_ROOT", "/tmp/from-env")
	if o, err = parseArgs(nil); err != nil || len(o.roots) != 1 || o.roots[0] != "/tmp/from-env" {
		t.Errorf("environment root: %+v %v", o, err)
	}
	if o, err = parseArgs([]string{"--root", "/tmp/flag"}); err != nil ||
		len(o.roots) != 1 || o.roots[0] != "/tmp/flag" {
		t.Errorf("a flag must replace the environment, not add to it: %+v %v", o, err)
	}
}

// The same repository named twice — by flag and by the directory you're in — is
// one project, not two.
func TestRootsAreDeduplicated(t *testing.T) {
	reset()
	dir := t.TempDir()
	got := resolveRoots([]string{dir, dir})
	if len(got) != 1 {
		t.Errorf("resolveRoots = %v, want one", got)
	}
}

func TestFlagForms(t *testing.T) {
	reset()
	o, err := parseArgs([]string{"--port=8080", "--host", "0.0.0.0", "--no-terminal", "--open"})
	if err != nil {
		t.Fatal(err)
	}
	if o.port != 8080 || o.host != "0.0.0.0" || !o.noTerm || !o.open {
		t.Errorf("got %+v", o)
	}
	reset()
	if o, err = parseArgs([]string{"-p", "9000"}); err != nil || o.port != 9000 {
		t.Errorf("short form: %+v %v", o, err)
	}
}

// Flags beat the environment; the environment beats the defaults.
func TestFlagsBeatEnvironment(t *testing.T) {
	reset()
	defer reset()
	os.Setenv("WORKMUX_PORT", "7000")
	os.Setenv("WORKMUX_HOST", "10.0.0.5")
	os.Setenv("WORKMUX_TERMINAL", "0")

	o, err := parseArgs([]string{"--port", "8080"})
	if err != nil {
		t.Fatal(err)
	}
	if o.port != 8080 {
		t.Errorf("port = %d, want the flag to win", o.port)
	}
	if o.host != "10.0.0.5" {
		t.Errorf("host = %q, want the environment", o.host)
	}
	if !o.noTerm {
		t.Error("WORKMUX_TERMINAL=0 should turn terminals off")
	}
}

// An empty --token is meaningful: it disables the check. That has to be
// distinguishable from not passing one at all.
func TestEmptyTokenIsStillSet(t *testing.T) {
	reset()
	o, err := parseArgs([]string{"--token", ""})
	if err != nil {
		t.Fatal(err)
	}
	if !o.tokenSet || o.token != "" {
		t.Errorf("got %+v, want an explicitly empty token", o)
	}
}

func TestBadInput(t *testing.T) {
	for _, argv := range [][]string{
		{"--nope"},
		{"-x"},
		{"--port"},          // missing value
		{"--port", "abc"},   // not a number
		{"--port", "70000"}, // out of range
		{"--host"},
	} {
		reset()
		if _, err := parseArgs(argv); err == nil {
			t.Errorf("parseArgs(%v) should fail", argv)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for _, h := range []string{"127.0.0.1", "::1", "localhost"} {
		if !isLoopback(h) {
			t.Errorf("%s should be loopback", h)
		}
	}
	for _, h := range []string{"0.0.0.0", "192.168.1.9", ""} {
		if isLoopback(h) {
			t.Errorf("%s should not be loopback", h)
		}
	}
}

// The WebSocket allowlist must cover exactly where this instance is reachable.
func TestOrigins(t *testing.T) {
	got := origins("127.0.0.1", 4315)
	if !has(got, "http://127.0.0.1:4315") || !has(got, "http://localhost:4315") {
		t.Errorf("loopback origins = %v", got)
	}
	if has(got, "http://192.168.1.9:4315") {
		t.Error("a loopback bind must not allow LAN origins")
	}
	got = origins("192.168.1.9", 4315)
	if !has(got, "http://192.168.1.9:4315") {
		t.Errorf("bound origins = %v", got)
	}
}

func TestMintTokenIsRandomAndURLSafe(t *testing.T) {
	a, b := mintToken(), mintToken()
	if a == b {
		t.Error("two tokens must differ")
	}
	if len(a) < 20 {
		t.Errorf("token %q is too short to be worth having", a)
	}
	for _, c := range a {
		if c == '+' || c == '/' || c == '=' {
			t.Errorf("token %q needs escaping in a URL", a)
		}
	}
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// Running workmux in a second repository adds it to the one already running. That
// is the whole feature, and no flag turns it into an error about a port — it
// briefly did, and "add this repo" failing because something else is listening is
// exactly the wrong shape. Flags a join can't honour get said out loud instead.
func TestFlagsAJoinCannotHonour(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want []string
	}{
		{[]string{"--dev"}, []string{"--dev"}},
		{[]string{"--dev=some/dir"}, []string{"--dev"}},
		{[]string{"--no-terminal"}, []string{"--no-terminal"}},
		{[]string{"--token", "abc"}, []string{"--token"}},
		{[]string{"--token", ""}, []string{"--token"}}, // an empty token is still a decision
		{[]string{"--dev", "--no-terminal"}, []string{"--dev", "--no-terminal"}},
		{[]string{}, nil},
		{[]string{"--open"}, nil},
		{[]string{"--port", "4400"}, nil},
		{[]string{"--host", "0.0.0.0"}, nil},
		{[]string{"--root", "/tmp/x"}, nil},
	} {
		reset()
		o, err := parseArgs(tc.argv)
		if err != nil {
			t.Fatalf("%v: %v", tc.argv, err)
		}
		got := ignoredByJoining(o)
		if len(got) != len(tc.want) {
			t.Errorf("ignoredByJoining(%v) = %v, want %v", tc.argv, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ignoredByJoining(%v) = %v, want %v", tc.argv, got, tc.want)
				break
			}
		}
	}
}

// The environment is not a flag. WORKMUX_TERMINAL=0 in a shell profile shouldn't
// put a warning on every invocation about something the user didn't type.
func TestTheEnvironmentIsNotAFlag(t *testing.T) {
	reset()
	defer reset()
	os.Setenv("WORKMUX_TERMINAL", "0")
	o, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !o.noTerm {
		t.Fatal("WORKMUX_TERMINAL=0 should still turn terminals off")
	}
	if got := ignoredByJoining(o); len(got) != 0 {
		t.Errorf("ignoredByJoining = %v, want nothing to warn about", got)
	}
}

// Naming a root adds to the remembered set. It replaced it for one commit, and
// `make dev ROOT=x` — which always passes --root — silently deleted every other
// repository from the list.
func TestNamingARootDoesNotForgetTheRest(t *testing.T) {
	reset()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	kept := testrepo.New(t, "kept").Root
	named := testrepo.New(t, "named").Root
	if err := instance.SaveProjects([]string{kept}); err != nil {
		t.Fatal(err)
	}

	o, err := parseArgs([]string{"--root", named})
	if err != nil {
		t.Fatal(err)
	}
	got := startingRoots(o, namedRoots(o))
	if len(got) != 2 || got[0] != kept || got[1] != named {
		t.Fatalf("startingRoots = %v, want [%s %s]", got, kept, named)
	}

	// --standalone is the way to say "only this", and it leaves the list alone.
	reset()
	o, err = parseArgs([]string{"--standalone", "--root", named})
	if err != nil {
		t.Fatal(err)
	}
	if got = startingRoots(o, namedRoots(o)); len(got) != 1 || got[0] != named {
		t.Errorf("standalone = %v, want just %s", got, named)
	}
	if remembered := instance.LoadProjects(); len(remembered) != 1 || remembered[0] != kept {
		t.Errorf("the remembered list changed: %v", remembered)
	}
}
