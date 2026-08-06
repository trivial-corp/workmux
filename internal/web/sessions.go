package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/trivial-corp/workmux/internal/bg"
	"github.com/trivial-corp/workmux/internal/project"
	"github.com/trivial-corp/workmux/internal/term"
)

// Sessions is what the web layer needs to hand out terminals. Nil disables them
// entirely (--no-terminal), and then none of these routes exist rather than
// answering "disabled" — an absent capability shouldn't look like a broken one.
//
// One registry for the whole server, not one per project: a session is a process
// this process is holding, and which repository it was opened in is a property of
// the session rather than a reason to keep separate books.
type Sessions struct {
	Reg *term.Registry
}

type newSessionRequest struct {
	Kind  string `json:"kind"`
	CWD   string `json:"cwd"`
	Agent string `json:"agent"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
}

func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":  true,
		"sessions": s.Sessions.Reg.List(),
	})
}

func handleSessionNew(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "post only"})
		return
	}
	var req newSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	// A shell in an arbitrary directory is the one thing this must never do, so the
	// directory has to be one of this project's own worktrees. Scoping it to the
	// project in the path, rather than to any project the server holds, is what
	// keeps the check meaningful once there are several.
	if !p.Owns(req.CWD) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a worktree of " + p.Name()})
		return
	}

	kind := term.Kind(req.Kind)
	// "resume" is a question — give me this worktree's agent — answered here so the
	// answer is the same from the dock, a keystroke or curl.
	if req.Kind == "resume" {
		k, agent := resolveResume(p, req.CWD)
		kind, req.Agent = k, agent
	}
	if kind == term.KindMCPAuth && !mcpNameRe.MatchString(req.Agent) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad server name"})
		return
	}
	if kind == term.KindAttach {
		if !agentIDRe.MatchString(req.Agent) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad agent id"})
			return
		}
		// One viewer per agent: clicking resume twice must land on the session you
		// have, not start a second attach against the same agent.
		if existing, ok := s.Sessions.Reg.FindAgent(req.Agent); ok {
			writeJSON(w, http.StatusOK, existing.Info())
			return
		}
	}

	spec, err := p.Spec(kind, req.CWD, req.Agent)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	spec.Cols, spec.Rows = req.Cols, req.Rows
	sess, err := s.Sessions.Reg.Start(spec)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, term.ErrTooMany) {
			code = http.StatusTooManyRequests
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	Journal.Note("session %s (%s) started in %s", sess.ID, sess.Kind, p.Where(sess.CWD))
	writeJSON(w, http.StatusOK, sess.Info())
}

func (s *Server) handleSessionKill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "post only"})
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req)
	sess, ok := s.Sessions.Reg.Get(req.ID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such session"})
		return
	}
	sess.Kill()
	// Explicitly killed means gone. The grace period exists so a session that ended on
	// its own leaves its last output readable; a session you closed shouldn't come back
	// in the next poll looking like the close didn't work.
	s.Sessions.Reg.Forget(req.ID)
	Journal.Note("session %s killed", req.ID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSessionSocket streams one session both ways.
//
// The protocol is deliberately two-shaped: binary frames are raw PTY bytes in
// either direction, and text frames are JSON control messages. That means
// keystrokes need no encoding and no escaping — the bytes a terminal emits are the
// bytes the program receives — while resize and exit still have somewhere to live.
func (s *Server) handleSessionSocket(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/session/socket/")
	sess, ok := s.Sessions.Reg.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such session"})
		return
	}
	// CORS does not cover WebSockets: without this check any page you had open
	// could connect to localhost and get a shell.
	if !s.OriginOK(r.Header.Get("Origin")) {
		Journal.Note("refused a websocket from origin %q", r.Header.Get("Origin"))
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Checked above, against the addresses this instance is reachable at.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled, // terminal output is tiny and latency-sensitive
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	// A megabyte is a paste, not a keystroke; anything larger is a mistake.
	conn.SetReadLimit(1 << 20)

	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	viewer, replay := sess.Attach(cols, rows)
	defer viewer.Detach()

	ctx := r.Context()
	if len(replay) > 0 {
		// Everything the session already said, before anything new — otherwise a
		// reattach shows an empty pane until the program next writes.
		if err := conn.Write(ctx, websocket.MessageBinary, replay); err != nil {
			return
		}
	}
	// Then ask the program to paint its own screen. A replayed byte log can't
	// reconstruct a full-screen UI, and the viewer's first resize wipes the alternate
	// buffer anyway — so attaching used to leave a pane with only a cursor in it.
	// Delayed a little, so it lands after the client has applied the replay and settled
	// on its size.
	bg.Go(func() {
		time.Sleep(250 * time.Millisecond)
		sess.Nudge()
	})

	// Reader: keystrokes and control messages.
	go func() {
		defer conn.CloseNow()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if err := sess.Write(data); err != nil {
					return
				}
			case websocket.MessageText:
				var msg struct {
					T    string `json:"t"`
					Cols int    `json:"cols,omitempty"`
					Rows int    `json:"rows,omitempty"`
					Data string `json:"data,omitempty"`
				}
				if json.Unmarshal(data, &msg) != nil {
					continue
				}
				switch msg.T {
				case "size":
					viewer.Resize(msg.Cols, msg.Rows)
				case "input": // for clients that would rather not send binary
					if err := sess.Write([]byte(msg.Data)); err != nil {
						return
					}
				}
			}
		}
	}()

	// Writer: PTY output, plus a final note about how it ended.
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, open := <-viewer.Out:
			if !open {
				exit, _ := json.Marshal(map[string]string{"t": "exit"})
				writeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = conn.Write(writeCtx, websocket.MessageText, exit)
				cancel()
				_ = conn.Close(websocket.StatusNormalClosure, "session ended")
				return
			}
			if err := conn.Write(ctx, websocket.MessageBinary, chunk); err != nil {
				return
			}
		}
	}
}
