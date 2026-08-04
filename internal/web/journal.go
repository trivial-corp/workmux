package web

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Journal is this process's own log, kept in memory so the UI can show it.
//
// It existed only in the terminal you launched the server from, which is nowhere
// you'll look — least of all from a phone. Bounded, because a long-running dashboard
// would otherwise hold every line it ever wrote.
var Journal = &journal{max: 500}

type journal struct {
	mu    sync.Mutex
	lines []Line
	max   int
}

// Line is one entry. Level lets the UI filter to problems only.
type Line struct {
	At    string `json:"at"`
	Level string `json:"level"` // info | warn | bad
	Msg   string `json:"msg"`
}

// Note records something worth seeing later: a session started, work created, an
// action refused.
func (j *journal) Note(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	level := "info"
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "fail") || strings.Contains(low, "error") ||
		strings.Contains(low, "refused") || strings.Contains(low, "could not"):
		level = "bad"
	case strings.Contains(low, "warn") || strings.Contains(low, "no ") ||
		strings.Contains(low, "gone"):
		level = "warn"
	}
	j.mu.Lock()
	j.lines = append(j.lines, Line{At: time.Now().Format("15:04:05"), Level: level, Msg: msg})
	if len(j.lines) > j.max {
		j.lines = j.lines[len(j.lines)-j.max:]
	}
	j.mu.Unlock()
}

// Lines is the log, oldest first.
func (j *journal) Lines() []Line {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Line, len(j.lines))
	copy(out, j.lines)
	return out
}
