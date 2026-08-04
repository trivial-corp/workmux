package presets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/term"
)

func load(t *testing.T, files map[string]string) *config.Config {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaultsGiveClaudeAndAShell(t *testing.T) {
	d := Deps{Cfg: load(t, nil)}

	shell, err := d.Spec(term.KindShell, "/w", "")
	if err != nil || shell.Command != "" {
		t.Errorf("shell = %+v, %v — want a bare login shell", shell, err)
	}
	agent, err := d.Spec(term.KindAgent, "/w", "")
	if err != nil || agent.Command != "claude" {
		t.Errorf("agent = %+v, %v", agent, err)
	}
	attach, err := d.Spec(term.KindAttach, "/w", "abc123ef")
	if err != nil || !strings.Contains(attach.Command, "claude attach") {
		t.Errorf("attach = %+v, %v", attach, err)
	}
	if !strings.Contains(attach.Title, "abc123ef") {
		t.Errorf("the tab should name the agent: %q", attach.Title)
	}
}

// The point of the package: a project that can't do something says so, and the
// session is never started.
func TestUnavailableKindsExplainThemselves(t *testing.T) {
	d := Deps{Cfg: load(t, map[string]string{"workmux.json": `{"agent": null}`})}

	for _, kind := range []term.Kind{term.KindAgent, term.KindAttach} {
		spec, err := d.Spec(kind, "/w", "abc123")
		if err == nil {
			t.Errorf("%s should be refused, got %+v", kind, spec)
			continue
		}
		if !strings.Contains(err.Error(), "agent") {
			t.Errorf("%s: %v — should mention the agent", kind, err)
		}
	}
	// A shell has nothing to do with the agent and must still work.
	if _, err := d.Spec(term.KindShell, "/w", ""); err != nil {
		t.Errorf("shell should always work: %v", err)
	}
}

func TestLogsNeedsARunningStack(t *testing.T) {
	cfg := load(t, map[string]string{
		"compose.yaml": "services: {}\n",
		"workmux.json": `{"name":"app","stack":{"slots":"app{n}","commands":{"logs":"bin/dev logs {slot}"}}}`,
	})

	// Nothing up here.
	d := Deps{Cfg: cfg, SlotFor: func(string) string { return "" }}
	if _, err := d.Spec(term.KindLogs, "/w", ""); err == nil {
		t.Error("logs with no stack running should be refused")
	}

	// Something up.
	d = Deps{Cfg: cfg, SlotFor: func(string) string { return "app2" }}
	spec, err := d.Spec(term.KindLogs, "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Command != "bin/dev logs app2" {
		t.Errorf("command = %q", spec.Command)
	}
	if !strings.Contains(spec.Title, "app2") {
		t.Errorf("title should name the slot: %q", spec.Title)
	}
}

// A project with no stack at all can't tail logs, whatever is running elsewhere.
func TestLogsWithoutAStack(t *testing.T) {
	d := Deps{Cfg: load(t, nil), SlotFor: func(string) string { return "whatever" }}
	if _, err := d.Spec(term.KindLogs, "/w", ""); err == nil {
		t.Error("no stack means no logs command")
	}
}

func TestGitFallsBackRatherThanFailing(t *testing.T) {
	d := Deps{Cfg: load(t, nil)}
	spec, err := d.Spec(term.KindGit, "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"lazygit", "tig", "git -c color.ui=always log"} {
		if !strings.Contains(spec.Command, want) {
			t.Errorf("command should try %s: %q", want, spec.Command)
		}
	}
}

func TestUnknownKind(t *testing.T) {
	d := Deps{Cfg: load(t, nil)}
	if _, err := d.Spec(term.Kind("nonsense"), "/w", ""); err == nil {
		t.Error("an unknown kind must be refused")
	}
}

// Another agent CLI: attach comes from its own template, not claude's.
func TestAnotherAgentCLI(t *testing.T) {
	d := Deps{Cfg: load(t, map[string]string{"workmux.json": `{
		"agent": {"command": "mycoder", "attach": "mycoder resume --id {id}"}}`})}
	spec, err := d.Spec(term.KindAttach, "/w", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Command != "mycoder resume --id 'deadbeef'" {
		t.Errorf("command = %q", spec.Command)
	}
}
