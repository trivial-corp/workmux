package instance

import (
	"os"
	"path/filepath"
	"testing"
)

// The set you assembled over a week has to survive a restart, which is the whole
// reason `workmux` on its own is worth typing.
func TestProjectsRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if got := LoadProjects(); len(got) != 0 {
		t.Errorf("first run = %v, want nothing remembered", got)
	}
	want := []string{"/code/trip1", "/code/homelab"}
	if err := SaveProjects(want); err != nil {
		t.Fatal(err)
	}
	got := LoadProjects()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("LoadProjects = %v, want %v (order included)", got, want)
	}

	// Beside the address, so forgetting everything is one rm.
	if filepath.Dir(ProjectsPath()) != filepath.Dir(Path()) {
		t.Errorf("%s and %s should share a directory", ProjectsPath(), Path())
	}
	if st, err := os.Stat(ProjectsPath()); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
}

// Removing the last project leaves an empty list, not a missing file: "I serve
// nothing" and "I have never run" are different answers.
func TestProjectsCanBeEmptied(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := SaveProjects([]string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ProjectsPath()); err != nil {
		t.Errorf("the file should exist: %v", err)
	}
	if got := LoadProjects(); len(got) != 0 {
		t.Errorf("LoadProjects = %v, want empty", got)
	}
}

// Junk on disk is the first-run case, not a crash.
func TestProjectsIgnoresGarbage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "workmux"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ProjectsPath(), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadProjects(); got != nil {
		t.Errorf("LoadProjects = %v, want nil", got)
	}
}
