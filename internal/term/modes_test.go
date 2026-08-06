package term

import (
	"strings"
	"testing"
)

func TestModeSetReadsWhatTheProgramAsked(t *testing.T) {
	m := newModeSet()
	m.feed([]byte("hello \x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[?2004h world"))
	if !m.alt() {
		t.Fatal("1049h means the program is on the alternate screen")
	}
	for _, n := range []int{1049, 1000, 1006, 2004} {
		if !m.on[n] {
			t.Errorf("mode %d should be on", n)
		}
	}
	m.feed([]byte("\x1b[?1000l\x1b[?1049l"))
	if m.alt() || m.on[1000] {
		t.Error("a program that turns them off is not still holding them")
	}
	if !m.on[1006] {
		t.Error("turning off one mode must not turn off another")
	}
}

func TestModeSetSemicolonForm(t *testing.T) {
	// xterm accepts several modes in one sequence, and programs use it.
	m := newModeSet()
	m.feed([]byte("\x1b[?1000;1006;2004h"))
	if !m.on[1000] || !m.on[1006] || !m.on[2004] {
		t.Fatalf("all three should be on: %v", m.on)
	}
}

func TestModeSetSurvivesASplitEscape(t *testing.T) {
	// A read boundary can fall anywhere. Losing the mode here is losing the mouse for
	// the rest of the session.
	m := newModeSet()
	m.feed([]byte("noise \x1b[?10"))
	m.feed([]byte("49h more"))
	if !m.alt() {
		t.Fatal("a mode split across two reads is still a mode")
	}
}

func TestModeSetIgnoresWhatItCannotRestore(t *testing.T) {
	m := newModeSet()
	m.feed([]byte("\x1b[?9001h\x1b[?38h"))
	if len(m.on) != 0 {
		t.Fatalf("only the carried modes are tracked, got %v", m.on)
	}
	if m.prologue() != nil {
		t.Error("nothing to say means nothing said")
	}
}

func TestPrologueEntersTheAlternateScreenFirst(t *testing.T) {
	m := newModeSet()
	m.feed([]byte("\x1b[?1006h\x1b[?1000h\x1b[?1049h"))
	got := string(m.prologue())
	alt, mouse := strings.Index(got, "?1049h"), strings.Index(got, "?1000h")
	if alt == -1 || mouse == -1 {
		t.Fatalf("both should be in %q", got)
	}
	if alt > mouse {
		// Setting a mode and then switching screens sets it on the wrong one.
		t.Errorf("the alternate screen must come first: %q", got)
	}
}
