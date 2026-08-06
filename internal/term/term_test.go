package term

import (
	"os"
	"strings"
	"testing"
	"time"
)

// waitFor polls until cond holds, so tests don't race a real process starting.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The whole reason this is a PTY and not a pipe: the program gets a terminal, so
// what it writes comes back and what you type reaches it.
func TestRoundTrip(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()

	s, err := r.Start(Spec{Kind: KindShell, CWD: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	v, replay := s.Attach(80, 24)
	if len(replay) != 0 {
		t.Errorf("a new session has nothing to replay, got %q", replay)
	}
	if err := s.Write([]byte("echo round-trip-$((21*2))\n")); err != nil {
		t.Fatal(err)
	}
	got := collect(v.Out, 5*time.Second, "round-trip-42")
	if !strings.Contains(got, "round-trip-42") {
		t.Errorf("shell output missing the result:\n%s", got)
	}
}

// A program only behaves like itself if it believes it's on a terminal.
func TestProcessSeesATTY(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()

	// stty reads the size from *stdin*, which is the pty. `tput cols` would be read
	// through the command substitution's pipe instead and quietly answer 80 — which
	// is what this test asserted before, measuring the harness rather than the code.
	s, err := r.Start(Spec{Kind: KindShell, CWD: t.TempDir(),
		Command: `sh -c 'test -t 0 && echo IS_A_TTY; echo "size=$(stty size)"'`,
		Cols:    100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.Attach(100, 30)
	got := collect(v.Out, 5*time.Second, "size=")
	if !strings.Contains(got, "IS_A_TTY") {
		t.Errorf("stdin was not a terminal:\n%s", got)
	}
	// And the size it was given, not terminfo's default.
	if !strings.Contains(got, "size=30 100") {
		t.Errorf("the program saw the wrong size:\n%s", got)
	}
}

// Reload, switch worktrees, come back: the pane must show what it already said.
func TestReattachReplaysScrollback(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()

	s, err := r.Start(Spec{Kind: KindShell, CWD: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := s.Attach(80, 24)
	_ = s.Write([]byte("echo before-the-reload\n"))
	collect(first.Out, 5*time.Second, "before-the-reload")
	first.Detach()

	second, replay := s.Attach(80, 24)
	if !strings.Contains(string(replay), "before-the-reload") {
		t.Errorf("replay missing earlier output:\n%s", replay)
	}
	// And the session is still live, not a corpse.
	_ = s.Write([]byte("echo after-the-reload\n"))
	got := collect(second.Out, 5*time.Second, "after-the-reload")
	if !strings.Contains(got, "after-the-reload") {
		t.Errorf("session not usable after reattach:\n%s", got)
	}
}

// Two viewers of one session, and the PTY has to be a size both can render.
// Last-writer-wins made a laptop and a phone shred each other's screen.
func TestSmallestViewerWins(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()

	s, err := r.Start(Spec{Kind: KindShell, CWD: t.TempDir(), Cols: 120, Rows: 40})
	if err != nil {
		t.Fatal(err)
	}
	desktop, _ := s.Attach(120, 40)
	phone, _ := s.Attach(40, 20)

	waitFor(t, "the smaller size to win", func() bool {
		c, rw := s.Size()
		return c == 40 && rw == 20
	})

	// The phone goes away: the desktop's size is usable again.
	phone.Detach()
	waitFor(t, "the size to recover", func() bool {
		c, rw := s.Size()
		return c == 120 && rw == 40
	})

	// A viewer that isn't showing the session (0×0) doesn't get a vote — otherwise
	// a hidden pane would pin the terminal to nothing.
	desktop.Resize(0, 0)
	c, rw := s.Size()
	if c != 120 || rw != 40 {
		t.Errorf("size = %dx%d, want the last real size", c, rw)
	}
}

func TestResizeIsBounded(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()
	s, err := r.Start(Spec{Kind: KindShell, CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.Attach(0, 0)
	// A client that hasn't laid out yet, and one that's lying.
	v.Resize(3, 1)
	waitFor(t, "a sane floor", func() bool { c, rw := s.Size(); return c >= 20 && rw >= 4 })
	v.Resize(9000, 9000)
	c, rw := s.Size()
	if c > 500 || rw > 300 {
		t.Errorf("size = %dx%d, want it clamped", c, rw)
	}
}

// Exiting must close the viewer's channel, which is how the UI learns to stop.
func TestExitClosesViewers(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()

	s, err := r.Start(Spec{Kind: KindShell, CWD: t.TempDir(), Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.Attach(80, 24)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, open := <-v.Out:
			if !open {
				if !s.Finished() {
					t.Error("channel closed but the session says it's alive")
				}
				return
			}
		case <-deadline:
			t.Fatal("the viewer channel was never closed")
		}
	}
}

// Killing must take the process group: signalling only the shell leaves whatever
// it started alive and holding the PTY, which looks like a session that won't die.
func TestKillTakesTheWholeGroup(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()

	dir := t.TempDir()
	marker := dir + "/still-running"
	// A child that outlives its parent shell unless the group is signalled.
	s, err := r.Start(Spec{Kind: KindShell, CWD: dir,
		Command: `sh -c 'sh -c "while :; do touch ` + marker + `; sleep 0.2; done" & wait'`})
	if err != nil {
		t.Fatal(err)
	}
	s.Attach(80, 24)
	waitFor(t, "the child to start", func() bool { _, err := os.Stat(marker); return err == nil })

	s.Kill()
	if !s.WaitExit(10 * time.Second) {
		t.Fatal("session did not end")
	}
	// Give the grandchild a chance to prove it's gone: remove the marker and see
	// whether anything recreates it.
	_ = os.Remove(marker)
	time.Sleep(1 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a grandchild survived the kill and is still writing")
	}
}

func TestStartRefusesAMissingDirectory(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()
	if _, err := r.Start(Spec{Kind: KindShell, CWD: "/no/such/place"}); err == nil {
		t.Error("a missing worktree must be refused")
	}
	if _, err := r.Start(Spec{Kind: KindShell}); err == nil {
		t.Error("no directory must be refused")
	}
}

// One viewer per agent: resume twice lands on the session you already have.
func TestFindAgent(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()

	s, err := r.Start(Spec{Kind: KindAttach, CWD: t.TempDir(), Agent: "abc123", Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.FindAgent("abc123")
	if !ok || got.ID != s.ID {
		t.Errorf("FindAgent = %v, %v; want %s", got, ok, s.ID)
	}
	if _, ok := r.FindAgent("nobody"); ok {
		t.Error("found an agent that has no session")
	}
}

// A finished attach is dropped at once — detaching leaves nothing to read, and a
// dead tab beside a live one for the same agent is pure confusion.
func TestFinishedAttachIsForgotten(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()

	s, err := r.Start(Spec{Kind: KindAttach, CWD: t.TempDir(), Agent: "abc123", Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.WaitExit(10 * time.Second) {
		t.Fatal("did not exit")
	}
	waitFor(t, "the attach to be forgotten", func() bool {
		for _, i := range r.List() {
			if i.ID == s.ID {
				return false
			}
		}
		return true
	})
}

// A shell's last output is often the point, so it lingers after exiting.
func TestFinishedShellLingers(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()

	s, err := r.Start(Spec{Kind: KindShell, CWD: t.TempDir(), Command: "echo the-reason-it-failed"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.WaitExit(10 * time.Second) {
		t.Fatal("did not exit")
	}
	found := false
	for _, i := range r.List() {
		if i.ID == s.ID {
			found = true
			if i.Alive {
				t.Error("a finished session should not claim to be alive")
			}
		}
	}
	if !found {
		t.Error("a finished shell was dropped immediately; its output is the point")
	}
	// And its output is still readable.
	if !strings.Contains(s.Drain(), "the-reason-it-failed") {
		t.Errorf("output lost: %q", s.Drain())
	}
	// Attaching to it replays, then closes.
	v, replay := s.Attach(80, 24)
	if !strings.Contains(string(replay), "the-reason-it-failed") {
		t.Error("replay of a finished session lost its output")
	}
	if _, open := <-v.Out; open {
		t.Error("a finished session's channel should be closed")
	}
}

func TestWriteAfterExit(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()
	s, err := r.Start(Spec{Kind: KindShell, CWD: t.TempDir(), Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	s.WaitExit(10 * time.Second)
	if err := s.Write([]byte("hello\n")); err == nil {
		t.Error("writing to a finished session should fail rather than silently vanish")
	}
}

func TestSessionCap(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()
	dir := t.TempDir()
	for i := 0; i < maxSessions; i++ {
		if _, err := r.Start(Spec{Kind: KindShell, CWD: dir, Command: "sleep 30"}); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	if _, err := r.Start(Spec{Kind: KindShell, CWD: dir, Command: "sleep 30"}); err == nil {
		t.Error("the cap did not hold")
	}
}

// collect reads until it sees want, or the timeout passes.
func collect(ch <-chan []byte, d time.Duration, want string) string {
	var sb strings.Builder
	deadline := time.After(d)
	for {
		select {
		case chunk, open := <-ch:
			if !open {
				return sb.String()
			}
			sb.Write(chunk)
			if strings.Contains(sb.String(), want) {
				return sb.String()
			}
		case <-deadline:
			return sb.String()
		}
	}
}

// A full-screen program owns the alternate screen and repaints only when told. Attaching
// used to leave a pane with nothing but a cursor: the replay can't reconstruct someone
// else's screen, and the viewer's own resize wipes the alternate buffer. Nudge makes the
// program redraw its own screen.
func TestNudgeMakesTheProgramRedraw(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()

	// Draws only on SIGWINCH — so anything we see afterwards came from the program.
	s, err := r.Start(Spec{Kind: KindShell, CWD: t.TempDir(), Cols: 90, Rows: 30,
		Command: `sh -c 'trap "printf REDREW-ON-WINCH" WINCH; printf "\033[?1049h\033[2J"; while :; do sleep 0.1; done'`})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.Attach(90, 30)
	// Let it install the trap and clear the screen.
	waitFor(t, "the alternate screen", func() bool { return strings.Contains(s.Drain(), "[?1049h") })
	before := s.Drain()
	if strings.Contains(before, "REDREW-ON-WINCH") {
		t.Fatal("it drew before being asked; the test proves nothing")
	}

	s.Nudge()
	got := collect(v.Out, 5*time.Second, "REDREW-ON-WINCH")
	if !strings.Contains(got, "REDREW-ON-WINCH") {
		t.Errorf("the program was never asked to redraw:\n%q", got)
	}
	// And the size it ends on is the size it started with, not one column short.
	waitFor(t, "the size to come back", func() bool { c, _ := s.Size(); return c == 90 })
}

// Nudging a finished session must not panic or resize a closed pty.
func TestNudgeAfterExit(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()
	s, err := r.Start(Spec{Kind: KindShell, CWD: t.TempDir(), Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.WaitExit(10 * time.Second) {
		t.Fatal("did not exit")
	}
	s.Nudge() // must be a no-op
}

// A full-screen program's buffer is a reel of frames drawn for one width. Replaying it
// into a narrower terminal laid every frame over the others at the wrong offsets, and
// the pane came back unreadable — two renderings of the same text interleaved.
func TestAltScreenIsNotReplayedAtAnotherWidth(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()

	s, err := r.Start(Spec{Kind: KindShell, CWD: t.TempDir(), Cols: 120, Rows: 40})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.Attach(120, 40)
	_ = s.Write([]byte("printf '\\033[?1049h\\033[?1000h\\033[?1006h'; echo FRAME''-CONTENT\n"))
	collect(v.Out, 5*time.Second, "FRAME-CONTENT")
	v.Detach()

	// Even at the same width: the reel spans every width the session has ever had, and
	// the frames from before a resize are as wrong as any others.
	same, replay := s.Attach(120, 40)
	if strings.Contains(string(replay), "FRAME-CONTENT") {
		t.Errorf("a full-screen program's frames are never replayed:\n%q", replay)
	}
	same.Detach()

	narrow, replay := s.Attach(70, 40)
	defer narrow.Detach()
	got := string(replay)
	if strings.Contains(got, "FRAME-CONTENT") {
		t.Errorf("frames drawn for another width must not be painted in:\n%q", got)
	}
	if !strings.Contains(got, "\x1b[2J") {
		t.Errorf("the viewer needs a clear screen to repaint into:\n%q", got)
	}
	for _, mode := range []string{"?1049h", "?1000h", "?1006h"} {
		if !strings.Contains(got, mode) {
			// Without these the new viewer disagrees with the program about what kind
			// of terminal it is: the mouse goes dead and the alternate screen isn't up.
			t.Errorf("attach must restore %s:\n%q", mode, got)
		}
	}
}

// A shell has real scrollback, and a narrower window is no reason to throw it away.
func TestPlainSessionStillReplaysAtAnyWidth(t *testing.T) {
	r := NewRegistry()
	defer r.Shutdown()

	s, err := r.Start(Spec{Kind: KindShell, CWD: t.TempDir(), Cols: 120, Rows: 40})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.Attach(120, 40)
	_ = s.Write([]byte("echo SCROLLBACK''-KEPT\n"))
	collect(v.Out, 5*time.Second, "SCROLLBACK-KEPT")
	v.Detach()

	narrow, replay := s.Attach(70, 40)
	defer narrow.Detach()
	if !strings.Contains(string(replay), "SCROLLBACK-KEPT") {
		t.Errorf("a shell's scrollback survives a resize:\n%q", replay)
	}
}
