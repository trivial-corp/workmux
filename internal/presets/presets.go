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

	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/term"
)

// Deps is what presets need to answer.
type Deps struct {
	Cfg *config.Config
	// SlotFor names the stack running in a worktree, "" when none is.
	SlotFor func(cwd string) string
}

// Spec describes the session to start, or says why this project can't.
func (d Deps) Spec(kind term.Kind, cwd, agentID string) (term.Spec, error) {
	base := term.Spec{Kind: kind, CWD: cwd, Agent: agentID}
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
		cmd := d.Cfg.AttachCmd(agentID)
		if cmd == "" {
			return base, errors.New("this project's agent has no way to attach to a running session")
		}
		base.Title = d.Cfg.Agent.Name + " " + short(agentID)
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

	case term.KindGit:
		// A git TUI beats anything this dashboard would grow for the same job, but
		// it isn't installed everywhere — so fall back to something readable rather
		// than an error, and leave a shell behind so the pane stays useful.
		base.Title = "git"
		base.Command = "exec lazygit 2>/dev/null || exec tig 2>/dev/null || " +
			"{ git -c color.ui=always log --oneline --graph -20; exec $SHELL -l; }"
		return base, nil
	}
	return base, fmt.Errorf("unknown session kind %q", kind)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
