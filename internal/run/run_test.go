package run

import (
	"strings"
	"testing"
	"time"
)

func TestCmdCapturesBothStreams(t *testing.T) {
	res := Cmd("", 5*time.Second, "sh", "-c", "echo out; echo err >&2")
	if !res.OK() {
		t.Fatalf("code = %d", res.Code)
	}
	// The reason a command refused is usually on stderr, so both are kept.
	if !strings.Contains(res.Out, "out") || !strings.Contains(res.Out, "err") {
		t.Errorf("out = %q, want both streams", res.Out)
	}
}

func TestExitCodeSurvives(t *testing.T) {
	res := Cmd("", 5*time.Second, "sh", "-c", "echo nope >&2; exit 3")
	if res.Code != 3 {
		t.Errorf("code = %d, want 3", res.Code)
	}
	if res.OK() {
		t.Error("OK() must be false")
	}
	if got := res.LastLine("fallback"); got != "nope" {
		t.Errorf("LastLine = %q, want the complaint", got)
	}
}

// A hung command must not hang the dashboard.
func TestTimeout(t *testing.T) {
	start := time.Now()
	res := Cmd("", 300*time.Millisecond, "sleep", "5")
	if res.Code != 124 {
		t.Errorf("code = %d, want 124", res.Code)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v — the timeout didn't fire", elapsed)
	}
}

// A missing binary is the normal case for optional tools (gh, docker, lsof).
func TestMissingBinaryIsNotAPanic(t *testing.T) {
	res := Cmd("", 5*time.Second, "definitely-not-installed-xyz")
	if res.Code != 127 {
		t.Errorf("code = %d, want 127", res.Code)
	}
	if res.Out == "" {
		t.Error("want some explanation")
	}
}

func TestEmptyArgv(t *testing.T) {
	if res := Cmd("", time.Second); res.OK() {
		t.Error("no command should not succeed")
	}
}

func TestLastLineFallback(t *testing.T) {
	if got := (Result{}).LastLine("nothing said"); got != "nothing said" {
		t.Errorf("got %q", got)
	}
}

func TestRunsInDir(t *testing.T) {
	dir := t.TempDir()
	res := Cmd(dir, 5*time.Second, "pwd")
	// macOS reports /private/var for /var, so compare on the suffix.
	if !strings.HasSuffix(res.Out, strings.TrimPrefix(dir, "/private")) {
		t.Errorf("pwd = %q, want %q", res.Out, dir)
	}
}

func TestLinesSkipsBlanks(t *testing.T) {
	res := Result{Out: "a\n\nb\n"}
	if got := res.Lines(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("lines = %v", got)
	}
}
