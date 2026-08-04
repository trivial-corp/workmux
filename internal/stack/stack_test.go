package stack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trivial-corp/workmux/internal/config"
)

func cfgWithStack(t *testing.T, body string) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		if err := os.WriteFile(filepath.Join(root, "workmux.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNextFreeSlotSkipsWhatIsRunning(t *testing.T) {
	cfg := cfgWithStack(t, `{"name":"trip1","stack":{"slots":"trip{n}"}}`)

	if got := NextFreeSlot(cfg, nil); got != "trip1" {
		t.Errorf("nothing running → %q, want trip1", got)
	}
	// The bug this prevents: slot 1 was taken, so "start the app" landing on slot 2
	// looked like a bug unless something said so first.
	running := []Project{{Slot: "trip1"}, {Slot: "trip2"}}
	if got := NextFreeSlot(cfg, running); got != "trip3" {
		t.Errorf("two running → %q, want trip3", got)
	}
}

func TestNoStackHasNoSlots(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := NextFreeSlot(cfg, nil); got != "" {
		t.Errorf("got %q, want empty for a project with no stack", got)
	}
	if got := Running(cfg); got != nil {
		t.Errorf("got %v, want nil — docker must not even be called", got)
	}
}

// docker emits one object per line in some versions and an array in others.
func TestParsePSAcceptsBothShapes(t *testing.T) {
	lines := `{"Service":"web","State":"running","Health":"healthy","CreatedAt":"2026-08-01 12:00:53 +0300 EEST"}
{"Service":"db","State":"running","CreatedAt":"2026-08-01 11:00:53 +0300 EEST"}`
	array := `[{"Service":"web","State":"running","Health":"healthy","CreatedAt":"2026-08-01 12:00:53 +0300 EEST"},
	           {"Service":"db","State":"running","CreatedAt":"2026-08-01 11:00:53 +0300 EEST"}]`

	for name, out := range map[string]string{"ndjson": lines, "array": array} {
		st := parsePS(out)
		if st.Total != 2 || st.Up != 2 {
			t.Errorf("%s: up/total = %d/%d, want 2/2", name, st.Up, st.Total)
		}
		if st.Services[0].Name != "db" { // sorted by name
			t.Errorf("%s: services not sorted: %+v", name, st.Services)
		}
		// Uptime is the *earliest* running container: a stack is as old as its
		// oldest live piece, not its newest restart.
		if st.StartedEpoch == 0 {
			t.Errorf("%s: no start time parsed", name)
		}
	}
}

// A service can have a dead container beside a live one (a restart, a one-shot
// job). Keeping the wrong one made the row lie about the service being down.
func TestParsePSKeepsTheMostAliveContainerPerService(t *testing.T) {
	out := `{"Service":"web","Name":"web-1","State":"exited","ExitCode":1}
{"Service":"web","Name":"web-2","State":"running"}`
	st := parsePS(out)
	if st.Total != 1 {
		t.Fatalf("got %d services, want 1: %+v", st.Total, st.Services)
	}
	if st.Services[0].State != "running" || st.Up != 1 {
		t.Errorf("service = %+v, want the running container", st.Services[0])
	}
}

func TestParsePSPorts(t *testing.T) {
	out := `{"Service":"web","State":"running","Publishers":[{"PublishedPort":8080,"TargetPort":3000},{"PublishedPort":0,"TargetPort":9}]}`
	st := parsePS(out)
	if len(st.Services[0].Ports) != 1 || st.Services[0].Ports[0] != "8080→3000" {
		t.Errorf("ports = %v", st.Services[0].Ports)
	}
}

func TestParsePSGarbage(t *testing.T) {
	st := parsePS("not json\n\n")
	if st.Total != 0 || st.Services == nil {
		t.Errorf("garbage should give an empty (non-nil) service list, got %+v", st)
	}
}
