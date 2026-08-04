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

	"github.com/trivial-corp/workmux/internal/term"
)

// Sessions is what the web layer needs to hand out terminals. Nil disables them
// entirely (--no-terminal), and then none of these routes exist rather than
// answering "disabled" — an absent capability shouldn't look like a broken one.
type Sessions struct {
	Reg *term.Registry
	// Presets turns a requested kind into something runnable, given a worktree.
	// It lives outside this package because what "the agent" means is config.
	Presets func(kind term.Kind, cwd, agentID string) (term.Spec, error)
	// KnownDir reports whether a directory is one of this repo's worktrees. A
	// session is a shell, so the caller decides where one may be opened — never
	// the request.
	KnownDir func(string) bool
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

func (s *Server) handleSessionNew(w http.ResponseWriter, r *http.Request) {
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
	// directory has to be one of the repo's own worktrees.
	if !s.Sessions.KnownDir(req.CWD) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a worktree of this repository"})
		return
	}

	kind := term.Kind(req.Kind)
	// "resume" is a question — give me this worktree's agent — answered here so the
	// answer is the same from the dock, a keystroke or curl.
	if req.Kind == "resume" {
		k, agent := s.resolveResume(req.CWD)
		kind, req.Agent = k, agent
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

	spec, err := s.Sessions.Presets(kind, req.CWD, req.Agent)
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
