// Package presets turns "I want a session of this kind here" into something
// runnable, using only what config says.
//
// It exists as its own package because that translation is the entire difference
// between a tool that assumes Claude Code and docker compose, and one that doesn't:
// every command here comes from workmux.json, and a kind the project can't do
// returns a reason instead of a broken session.
package presets

import (
	"errors"
	"fmt"
	"strings"

	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/term"
)

// Deps is what presets need to answer.
type Deps struct {
	// NextSlot is the slot a stack would take if it started now. Only "up" needs it,
	// and only when this worktree has nothing running yet.
	NextSlot func() string

	Cfg *config.Config
	// SlotFor names the stack running in a worktree, "" when none is.
	SlotFor func(cwd string) string
}

// Spec describes the session to start, or says why this project can't.
//
// subject is what the kind is about: an agent id for an attach, an MCP server name
// for an authorization round, and ignored by the kinds that need neither.
func (d Deps) Spec(kind term.Kind, cwd, subject string) (term.Spec, error) {
	base := term.Spec{Kind: kind, CWD: cwd, Agent: subject}
	switch kind {
	case term.KindShell:
		base.Title = "shell"
		return base, nil // no command: an interactive login shell

	case term.KindAgent:
		if d.Cfg.Agent.Command == "" {
			return base, errors.New("no agent is configured for this project")
		}
		base.Title = d.Cfg.Agent.Name
		base.Command = d.Cfg.Agent.Command
		return base, nil

	case term.KindAttach:
		cmd := d.Cfg.AttachCmd(subject)
		if cmd == "" {
			return base, errors.New("this project's agent has no way to attach to a running session")
		}
		base.Title = d.Cfg.Agent.Name + " " + short(subject)
		base.Command = cmd
		return base, nil

	case term.KindLogs:
		slot := ""
		if d.SlotFor != nil {
			slot = d.SlotFor(cwd)
		}
		if slot == "" {
			return base, errors.New("nothing is running here to tail")
		}
		cmd := d.Cfg.StackCmd("logs", slot, cwd, "")
		if cmd == "" {
			return base, errors.New("this project has no logs command")
		}
		base.Title = "logs " + slot
		base.Command = cmd
		return base, nil

	case term.KindStack:
		// Starting an app is a build: minutes of output, and the only interesting part
		// is the reason it stopped. It used to run as a request — a toast, a silent
		// wait, and the output thrown away — so a failure was indistinguishable from a
		// slow success. It is a session now, and you watch it.
		action := subject
		switch action {
		case "up", "restart", "stop":
		default:
			return base, errors.New("unknown stack action")
		}
		slot := ""
		if d.SlotFor != nil {
			slot = d.SlotFor(cwd)
		}
		if slot == "" && action == "up" && d.NextSlot != nil {
			slot = d.NextSlot()
		}
		if slot == "" {
			return base, errors.New("nothing is running here to " + action)
		}
		cmd := d.Cfg.StackCmd(action, slot, cwd, "")
		if cmd == "" {
			return base, errors.New("this project has no " + action + " command")
		}
		base.Title = action + " " + slot
		// The exit status, in words, on the last line. Compose says plenty when it
		// fails and nothing at all when it succeeds, and "did that work" should not be
		// a question you have to read a hundred lines to answer.
		//
		// One command, quoted, because the session runs `exec <command>` — and
		// `exec (…); s=$?` execs the subshell and never reaches the rest, which is how
		// this first shipped saying "number expected" instead of anything useful.
		script := cmd + "; s=$?; echo; " +
			"if [ $s -eq 0 ]; then echo '" + action + " " + slot + " — done'; " +
			"else echo '" + action + " " + slot + " — failed, exit '$s; fi"
		base.Command = "sh -c " + sq(script)
		return base, nil

	case term.KindGit:
		// A git TUI beats anything this dashboard would grow for the same job, but
		// it isn't installed everywhere — so fall back to something readable rather
		// than an error, and leave a shell behind so the pane stays useful.
		base.Title = "git"
		base.Command = "exec lazygit 2>/dev/null || exec tig 2>/dev/null || " +
			"{ git -c color.ui=always log --oneline --graph -20; exec $SHELL -l; }"
		return base, nil

	case term.KindMCPAuth:
		if d.Cfg.Agent.MCP == "" || d.Cfg.Agent.MCPAuth == "" {
			return base, errors.New("this project's agent has no MCP authorization command")
		}
		if subject == "" {
			return base, errors.New("which server?")
		}
		base.Title = "auth " + subject
		// The command *is* the session. Typing it in after the fact means guessing
		// when the program is ready to be typed at, and a guess that's wrong loses
		// the keystrokes silently.
		base.Command = d.Cfg.Agent.MCP + " " +
			strings.ReplaceAll(d.Cfg.Agent.MCPAuth, "{name}", shellQuote(subject))
		return base, nil
	}
	return base, fmt.Errorf("unknown session kind %q", kind)
}

// shellQuote makes one shell argument out of a name that may contain spaces —
// connector names do ("claude.ai Sentry"), and a bare one would arrive as two
// arguments. Spec.Command goes through a login shell, so this can't be skipped.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sq quotes a script for a shell that will run it as one word.
func sq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
