package term

import (
	"bytes"
	"sort"
	"strconv"
)

// The DEC private modes worth carrying across an attach.
//
// A program sets these once, at startup, and never again. If a viewer arrives without
// being told about them it disagrees with the program about what kind of terminal it is:
// the mouse goes dead, paste stops being bracketed, the alternate screen isn't up. They
// are cheap to track and there is nothing else that knows.
var carried = map[int]bool{
	1:    true, // cursor keys send application sequences
	7:    true, // wrap at the right margin
	12:   true, // cursor blink
	25:   true, // cursor visible
	47:   true, // alternate screen, the old spelling
	1000: true, // report button presses
	1002: true, // ...and drags
	1003: true, // ...and every move
	1004: true, // report focus in and out
	1005: true, // utf-8 mouse coordinates
	1006: true, // SGR mouse coordinates
	1015: true, // urxvt mouse coordinates
	1047: true, // alternate screen
	1049: true, // alternate screen, saving the cursor
	2004: true, // bracketed paste
}

// altModes are the ones that mean "this program has taken over the screen".
var altModes = []int{47, 1047, 1049}

// modeSet is what the program has turned on, read from its own output.
type modeSet struct {
	on   map[int]bool
	tail []byte // an escape can arrive split across two reads
}

func newModeSet() *modeSet { return &modeSet{on: map[int]bool{}} }

// feed reads a chunk of program output for mode changes. It only looks for CSI ? … h/l,
// which is a small enough grammar to scan directly; anything else it steps over.
func (m *modeSet) feed(p []byte) {
	buf := p
	if len(m.tail) > 0 {
		buf = append(append([]byte(nil), m.tail...), p...)
		m.tail = nil
	}
	for i := 0; i+2 < len(buf); i++ {
		if buf[i] != 0x1b || buf[i+1] != '[' || buf[i+2] != '?' {
			continue
		}
		j := i + 3
		for j < len(buf) && (buf[j] == ';' || (buf[j] >= '0' && buf[j] <= '9')) {
			j++
		}
		if j >= len(buf) {
			// Truncated: keep it for the next chunk rather than losing the mode. Bounded,
			// because a run of digits this long is not a mode sequence any more.
			if len(buf)-i < 64 {
				m.tail = append([]byte(nil), buf[i:]...)
			}
			return
		}
		if buf[j] != 'h' && buf[j] != 'l' {
			i = j
			continue
		}
		set := buf[j] == 'h'
		for _, part := range bytes.Split(buf[i+3:j], []byte{';'}) {
			n, err := strconv.Atoi(string(part))
			if err != nil || !carried[n] {
				continue
			}
			if set {
				m.on[n] = true
			} else {
				delete(m.on, n)
			}
		}
		i = j
	}
}

// alt reports whether the program is on the alternate screen, where the buffer holds
// frames rather than scrollback.
func (m *modeSet) alt() bool {
	for _, n := range altModes {
		if m.on[n] {
			return true
		}
	}
	return false
}

// prologue is the sequence that puts a fresh terminal into the state the program thinks
// it is already in.
func (m *modeSet) prologue() []byte {
	if len(m.on) == 0 {
		return nil
	}
	nums := make([]int, 0, len(m.on))
	for n := range m.on {
		nums = append(nums, n)
	}
	// The alternate screen goes first: switching to it after setting everything else
	// would be setting them on the screen the program isn't using.
	sort.Slice(nums, func(i, j int) bool {
		ai, aj := isAlt(nums[i]), isAlt(nums[j])
		if ai != aj {
			return ai
		}
		return nums[i] < nums[j]
	})
	var out []byte
	for _, n := range nums {
		out = append(out, []byte("\x1b[?"+strconv.Itoa(n)+"h")...)
	}
	return out
}

func isAlt(n int) bool {
	for _, a := range altModes {
		if a == n {
			return true
		}
	}
	return false
}
