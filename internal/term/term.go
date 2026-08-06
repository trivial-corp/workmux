// Package term owns the terminal sessions.
//
// A session is a real PTY — not a pipe. That distinction is the whole feature: a
// process started with a controlling terminal has a foreground job, so ⌃C reaches
// the program you're looking at, `less` pages, and an agent's full-screen UI draws.
// A pipe gives you none of that.
//
// Sessions belong to this process, not to a page. Reload the browser, switch
// worktrees, restart a stack — the session is still here, and a reattaching viewer
// gets replayed what it missed. Closing a tab detaches; killing is explicit.
package term

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

// Kind is what a session is for. The UI labels them; the server only cares that
// each one has a command.
type Kind string

const (
	KindShell  Kind = "shell"  // a login shell in the worktree
	KindAgent  Kind = "agent"  // a fresh agent session
	KindAttach Kind = "attach" // take over an agent that's already running
	KindLogs   Kind = "logs"   // tail this work's containers
	KindGit    Kind = "git"    // a git TUI, for people who want one
)

// Limits chosen for the failure they prevent, not for elegance.
const (
	// scrollback is what a reattaching viewer gets replayed. Big enough for a
	// build log you scroll back through, small enough that fifty idle sessions
	// aren't holding a hundred megabytes.
	scrollback = 256 * 1024
	// maxSessions is a backstop against a loop opening them forever.
	maxSessions = 48
	// keepDead is how long a finished session stays readable, so "the command
	// exited and I want to see why" works. Attaches are dropped immediately —
	// detaching means the transcript lives with the agent, not in a dead pane.
	keepDead = 2 * time.Minute
)

// Info is a session as the API describes it.
type Info struct {
	ID string `json:"id"`
	// Project is the repository this session belongs to. One server serves
	// several, and the dock has to say which repo a shell is a shell in.
	Project string `json:"project"`
	Kind    Kind   `json:"kind"`
	Title   string `json:"title"`
	CWD     string `json:"cwd"`
	Agent   string `json:"agent"`
	Alive   bool   `json:"alive"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
	Viewers int    `json:"viewers"`
}

// Session is one PTY and everyone watching it.
type Session struct {
	ID      string
	Project string
	Kind    Kind
	Title   string
	CWD     string
	Agent   string

	pty  *os.File
	cmd  *exec.Cmd
	done chan struct{}

	mu      sync.Mutex
	buf     []byte // scrollback ring, oldest trimmed
	viewers map[*Viewer]struct{}
	ended   time.Time
	cols    int
	rows    int
}

// Viewer is one attached browser. Out is closed when the session ends.
type Viewer struct {
	Out chan []byte
	// cols/rows this viewer can display, 0 when it isn't showing the session.
	cols, rows int
	sess       *Session
}

// Spec describes a session to start.
type Spec struct {
	Kind    Kind
	Project string
	Title   string
	CWD     string
	Agent   string
	// Command is run through a login shell, so PATH is what a terminal would have
	// (homebrew, nvm, asdf) rather than what this process inherited — which for a
	// launchd- or systemd-started server is close to nothing. Empty means an
	// interactive login shell.
	Command string
	Env     []string
	Cols    int
	Rows    int
}

// Registry holds the sessions for one server.
type Registry struct {
	mu   sync.Mutex
	byID map[string]*Session
	seq  int
}

func NewRegistry() *Registry {
	return &Registry{byID: map[string]*Session{}}
}

var ErrTooMany = errors.New("too many sessions open — close some first")

// Start launches a session. The returned error is user-facing.
func (r *Registry) Start(spec Spec) (*Session, error) {
	if spec.CWD == "" {
		return nil, errors.New("a session needs a directory")
	}
	if st, err := os.Stat(spec.CWD); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("worktree is gone: %s", spec.CWD)
	}
	r.gc()
	r.mu.Lock()
	if len(r.byID) >= maxSessions {
		r.mu.Unlock()
		return nil, ErrTooMany
	}
	r.seq++
	id := fmt.Sprintf("s%d", r.seq)
	r.mu.Unlock()

	shell := loginShell()
	argv := []string{shell, "-l"}
	if spec.Command != "" {
		// exec, so ⌃D ends the session instead of dropping you into a bare shell
		// inside it — which looks like the session hanging.
		argv = []string{shell, "-lc", "exec " + spec.Command}
	}

	cols, rows := sane(spec.Cols, spec.Rows)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = spec.CWD
	cmd.Env = append(os.Environ(), spec.Env...)
	// TERM matters: without it curses programs refuse to draw. xterm-256color is
	// what xterm.js implements.
	cmd.Env = append(cmd.Env, "TERM=xterm-256color", "COLORTERM=truecolor", "TERM_PROGRAM=workmux")

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, fmt.Errorf("could not start a terminal: %w", err)
	}

	s := &Session{
		ID: id, Project: spec.Project, Kind: spec.Kind, Title: spec.Title,
		CWD: spec.CWD, Agent: spec.Agent,
		pty: f, cmd: cmd, done: make(chan struct{}),
		viewers: map[*Viewer]struct{}{}, cols: cols, rows: rows,
	}
	r.mu.Lock()
	r.byID[id] = s
	r.mu.Unlock()

	go s.pump()
	return s, nil
}

// pump moves PTY output to every viewer and into the scrollback.
func (s *Session) pump() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.broadcast(chunk)
		}
		if err != nil {
			break // EIO on close is the normal end of a PTY
		}
	}
	_ = s.cmd.Wait()
	s.mu.Lock()
	s.ended = time.Now()
	// Mark it finished *before* telling anyone. A viewer reacts to its channel
	// closing by asking whether the session ended, and closing the channels first
	// meant it could be told no — the two signals disagreed for a few microseconds,
	// which is exactly long enough for the answer to be wrong.
	close(s.done)
	for v := range s.viewers {
		close(v.Out)
	}
	s.viewers = map[*Viewer]struct{}{}
	s.mu.Unlock()
}

func (s *Session) broadcast(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, chunk...)
	if len(s.buf) > scrollback {
		s.buf = append([]byte(nil), s.buf[len(s.buf)-scrollback:]...)
	}
	for v := range s.viewers {
		select {
		case v.Out <- chunk:
		default:
			// A viewer that can't keep up must not stall the PTY — that would
			// block the program everyone else is watching. Drop this viewer's
			// chunk; it will resync on the next reattach.
		}
	}
}

// Attach registers a viewer and returns the scrollback to paint first, so a
// reload shows what the session already said instead of an empty pane.
func (s *Session) Attach(cols, rows int) (*Viewer, []byte) {
	v := &Viewer{Out: make(chan []byte, 256), cols: cols, rows: rows, sess: s}
	s.mu.Lock()
	replay := append([]byte(nil), s.buf...)
	if s.ended.IsZero() {
		s.viewers[v] = struct{}{}
	} else {
		close(v.Out) // already finished: replay, then EOF
	}
	s.mu.Unlock()
	s.resize()
	return v, replay
}

// Detach removes a viewer without touching the session.
func (v *Viewer) Detach() {
	s := v.sess
	s.mu.Lock()
	if _, ok := s.viewers[v]; ok {
		delete(s.viewers, v)
		close(v.Out)
	}
	s.mu.Unlock()
	s.resize()
}

// Write sends keystrokes to the PTY.
func (s *Session) Write(p []byte) error {
	if s.Finished() {
		return errors.New("session has ended")
	}
	_, err := s.pty.Write(p)
	return err
}

// Resize records what one viewer can display and re-derives the PTY size.
func (v *Viewer) Resize(cols, rows int) {
	v.sess.mu.Lock()
	v.cols, v.rows = cols, rows
	v.sess.mu.Unlock()
	v.sess.resize()
}

// resize sets the PTY to the smallest attached viewer.
//
// Smallest, not last-writer-wins: with a laptop and a phone on the same session,
// last-writer-wins means each resize shreds the other's screen, and the program
// inside redraws at a width somebody can't see. The smallest viewer is the only
// size everyone can render. Viewers reporting 0 aren't showing it and don't vote.
func (s *Session) resize() {
	s.mu.Lock()
	cols, rows := 0, 0
	for v := range s.viewers {
		if v.cols <= 0 || v.rows <= 0 {
			continue
		}
		if cols == 0 || v.cols < cols {
			cols = v.cols
		}
		if rows == 0 || v.rows < rows {
			rows = v.rows
		}
	}
	if cols == 0 || rows == 0 { // nobody watching: keep the last real size
		s.mu.Unlock()
		return
	}
	cols, rows = sane(cols, rows)
	if cols == s.cols && rows == s.rows {
		s.mu.Unlock()
		return
	}
	s.cols, s.rows = cols, rows
	f := s.pty
	s.mu.Unlock()
	_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Nudge makes the program redraw, by changing the window size and putting it back.
//
// A full-screen program (an agent's UI, vim, less) owns the alternate screen and only
// repaints when something tells it to. Two things conspire on attach: a replayed byte
// log can't reliably reconstruct someone else's screen, and resizing an xterm that is in
// the alternate buffer *discards* what was there. The result is a pane with nothing but a
// cursor — which is exactly why changing the font size fixed it, since that resized the
// PTY and the program redrew.
//
// So: one column narrower, then back. Two SIGWINCHes, and the program paints its own
// screen rather than us trying to reproduce it.
func (s *Session) Nudge() {
	s.mu.Lock()
	cols, rows, f := s.cols, s.rows, s.pty
	ended := !s.ended.IsZero()
	s.mu.Unlock()
	if ended || cols < 2 || rows < 1 {
		return
	}
	_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(cols - 1), Rows: uint16(rows)})
	time.Sleep(60 * time.Millisecond)
	_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Size is the current PTY size.
func (s *Session) Size() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

// Finished reports whether the command has exited.
func (s *Session) Finished() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Kill ends the session. The process group goes, not just the shell: killing the
// shell alone leaves whatever it started holding the PTY.
func (s *Session) Kill() {
	if s.cmd.Process != nil {
		_ = syscallKillGroup(s.cmd.Process.Pid)
	}
	_ = s.pty.Close()
}

// Info describes the session.
func (s *Session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Info{
		ID: s.ID, Project: s.Project, Kind: s.Kind, Title: s.Title, CWD: s.CWD, Agent: s.Agent,
		Alive: s.ended.IsZero(), Cols: s.cols, Rows: s.rows, Viewers: len(s.viewers),
	}
}

// Get returns a session by id.
func (r *Registry) Get(id string) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[id]
	return s, ok
}

// FindAgent returns a live session already attached to an agent, so clicking
// resume twice lands on the session you have instead of starting a second viewer
// onto the same agent — which is confusing to look at and gives it two masters.
func (r *Registry) FindAgent(agentID string) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.byID {
		if s.Kind == KindAttach && s.Agent == agentID && !s.Finished() {
			return s, true
		}
	}
	return nil, false
}

// List describes every session, oldest first.
func (r *Registry) List() []Info {
	r.gc()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Info, 0, len(r.byID))
	for _, s := range r.byID {
		out = append(out, s.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// gc forgets finished sessions once nobody is reading them.
func (r *Registry) gc() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.byID {
		s.mu.Lock()
		ended, viewers := s.ended, len(s.viewers)
		s.mu.Unlock()
		if ended.IsZero() || viewers > 0 {
			continue
		}
		// An attach leaves nothing worth reading: the transcript is the agent's.
		// A shell's last output is often the point, so it lingers.
		if s.Kind == KindAttach || time.Since(ended) > keepDead {
			delete(r.byID, id)
		}
	}
}

// Forget drops a session from the registry without waiting for the grace period. It's
// what an explicit close means: the grace period is for sessions that ended on their
// own, whose last output is usually the reason you're looking.
func (r *Registry) Forget(id string) {
	r.mu.Lock()
	delete(r.byID, id)
	r.mu.Unlock()
}

// Shutdown kills everything, for process exit.
func (r *Registry) Shutdown() {
	r.mu.Lock()
	all := make([]*Session, 0, len(r.byID))
	for _, s := range r.byID {
		all = append(all, s)
	}
	r.byID = map[string]*Session{}
	r.mu.Unlock()
	for _, s := range all {
		s.Kill()
	}
}

// sane bounds a requested size. A zero or absurd value from a client that hasn't
// laid out yet would otherwise become the PTY's idea of the screen.
func sane(cols, rows int) (int, int) {
	if cols < 20 {
		cols = 80
	}
	if rows < 4 {
		rows = 24
	}
	if cols > 500 {
		cols = 500
	}
	if rows > 300 {
		rows = 300
	}
	return cols, rows
}

// loginShell is the user's shell, which decides what PATH a session has.
func loginShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	for _, cand := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return "/bin/sh"
}

// Drain reads everything a session has produced so far. Tests use it; nothing
// else should, since viewers get output as it happens.
func (s *Session) Drain() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.buf)
}

// WaitExit blocks until the command exits, or the timeout passes.
func (s *Session) WaitExit(d time.Duration) bool {
	select {
	case <-s.done:
		return true
	case <-time.After(d):
		return false
	}
}

var _ io.Writer = (*sessionWriter)(nil)

// sessionWriter adapts a session to io.Writer, for callers that stream into it.
type sessionWriter struct{ s *Session }

func (w *sessionWriter) Write(p []byte) (int, error) {
	if err := w.s.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Writer returns the session as an io.Writer.
func (s *Session) Writer() io.Writer { return &sessionWriter{s} }

// TitleOr returns the title, or a fallback when the caller didn't set one.
func TitleOr(title string, kind Kind) string {
	if strings.TrimSpace(title) != "" {
		return title
	}
	return string(kind)
}
