package main

import (
	"os"
	"testing"
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

// A flag that describes the server this process would be means "start one".
//
// This was a real bug: `make dev` handed its repository to the workmux already
// running, which then served its own embedded frontend — so --dev did nothing at
// all, and said nothing about it. Joining is only correct when the invocation
// asked for nothing a running server can't already give it.
func TestFlagsThatMeanStartAServer(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"--dev"}, "--dev"},
		{[]string{"--dev=some/dir"}, "--dev"},
		{[]string{"--no-terminal"}, "--no-terminal"},
		{[]string{"--token", "abc"}, "--token"},
		{[]string{"--token", ""}, "--token"}, // an empty token is still a decision
		{[]string{}, ""},
		{[]string{"--open"}, ""},
		{[]string{"--port", "4400"}, ""}, // where, not what: joining one there is right
		{[]string{"--host", "0.0.0.0"}, ""},
		{[]string{"--root", "/tmp/x"}, ""},
	} {
		reset()
		o, err := parseArgs(tc.argv)
		if err != nil {
			t.Fatalf("%v: %v", tc.argv, err)
		}
		if got := ownServerFlag(o); got != tc.want {
			t.Errorf("ownServerFlag(%v) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

// The environment must not turn every invocation into its own server. Someone with
// WORKMUX_TERMINAL=0 in a shell profile would otherwise get a server per repository
// — the exact thing this is all meant to stop.
func TestTheEnvironmentDoesNotForceAServer(t *testing.T) {
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
	if got := ownServerFlag(o); got != "" {
		t.Errorf("ownServerFlag = %q, want it to stay free to join", got)
	}
}
