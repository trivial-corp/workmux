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
	if o.port != 4315 || o.host != "127.0.0.1" || o.root != "." {
		t.Errorf("defaults = %+v", o)
	}
	if o.noTerm || o.open || o.tokenSet {
		t.Errorf("defaults = %+v", o)
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
