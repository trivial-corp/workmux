// Package instance is what workmux knows between runs: where a server is
// listening, and which repositories it was serving.
//
// The tool is used from wherever you happen to be standing: you're in a repo, you
// run workmux. Doing that in a second repo used to give you a second server, a
// second port and a second page — three tabs to keep track of, none of which can
// tell you what the others are doing. So a running server leaves a note saying
// where it is, and a later invocation reads that note, hands over the repository it
// was started in, and exits.
//
// Deliberately a file and an HTTP call, not a socket protocol: the endpoint it
// posts to is the same one the browser uses, so there is one way to add a project
// and not two.
//
// The remembered project list is here for the same reason the address is: a set of
// repositories you assembled over a week shouldn't evaporate because the process
// restarted. `workmux` on its own picks up where you left off.
package instance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Info is the note a running server leaves behind.
type Info struct {
	PID     int    `json:"pid"`
	URL     string `json:"url"`
	Started string `json:"started"`
}

// Path is where the note lives: XDG state, because this is a fact about a running
// process rather than configuration, and it is expected to be deleted.
func Path() string { return statePath("server.json") }

// ProjectsPath is where the remembered repositories live. Beside the address, and
// deletable in the same breath: rm -r ~/.local/state/workmux forgets everything
// workmux knows about you.
func ProjectsPath() string { return statePath("projects.json") }

func statePath(name string) string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "workmux", name)
}

// SaveProjects records the repositories being served, so the next bare `workmux`
// serves them again. Written on every change rather than at exit: a server that is
// killed, or that dies, must not take the list with it.
func SaveProjects(roots []string) error {
	path := ProjectsPath()
	if path == "" {
		return fmt.Errorf("no home directory to write state to")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		Roots []string `json:"roots"`
	}{roots})
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// LoadProjects is the set the last server was serving. Missing is the first-run
// case and not an error.
func LoadProjects() []string {
	path := ProjectsPath()
	if path == "" {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out struct {
		Roots []string `json:"roots"`
	}
	if json.Unmarshal(body, &out) != nil {
		return nil
	}
	return out.Roots
}

// Save records this server. Failure is ignored by callers: not being findable is a
// smaller problem than not starting.
func Save(url string) error {
	path := Path()
	if path == "" {
		return fmt.Errorf("no home directory to write state to")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(Info{
		PID: os.Getpid(), URL: url, Started: time.Now().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// Clear removes the note, but only if it is still ours — a server that outlived us
// must not have its address deleted by our shutdown.
func Clear() {
	if info, ok := Load(); ok && info.PID == os.Getpid() {
		_ = os.Remove(Path())
	}
}

// Load reads the note. Missing or unreadable means "nothing is running", which is
// the ordinary case and not an error.
func Load() (Info, bool) {
	path := Path()
	if path == "" {
		return Info{}, false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Info{}, false
	}
	var info Info
	if json.Unmarshal(body, &info) != nil || info.URL == "" {
		return Info{}, false
	}
	return info, true
}

// Health is what a running workmux says about itself.
//
// More than liveness, because the interesting question when a port is taken is
// *which* workmux has it. Nearly always it's one of your own, and being told
// "a dev build, serving homelab" is the difference between recognising it and
// going looking for it.
type Health struct {
	OK       bool     `json:"ok"`
	Server   string   `json:"server"`
	Dev      bool     `json:"dev"`
	Projects []string `json:"projects"`
	Names    []string `json:"names"`
}

// Describe is this server in a phrase, for an error message about a busy port.
func (h Health) Describe() string {
	what := "a workmux"
	if h.Dev {
		what = "a --dev workmux"
	}
	if len(h.Names) > 0 {
		return what + " serving " + strings.Join(h.Names, ", ")
	}
	return what
}

// client keeps the timeouts short. Everything here happens between a person
// pressing enter and the banner appearing, and a stale note must not cost seconds.
var client = &http.Client{Timeout: 2 * time.Second}

// Probe asks what is answering at this URL, and whether it is a workmux at all.
//
// The identity check matters: port 4315 might be anything, and handing a repository
// to something that merely returns 200 is how you get a very confusing bug.
func Probe(url string) (Health, bool) {
	res, err := client.Get(url + "/api/health")
	if err != nil {
		return Health{}, false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Health{}, false
	}
	var h Health
	if json.NewDecoder(res.Body).Decode(&h) != nil {
		return Health{}, false
	}
	return h, h.OK && h.Server == "workmux"
}

// Running reports whether a workmux is answering at this URL.
func Running(url string) bool {
	_, ok := Probe(url)
	return ok
}

// Joined is what a running server says after taking a repository on.
type Joined struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Root     string `json:"root"`
	Added    bool   `json:"added"`
	Error    string `json:"error"`
	Projects []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"projects"`
}

// Join hands a repository to the server at url.
//
// No token: this is a loopback client on the same machine as the server, which the
// auth rule already exempts — and a process that can start a shell here can start
// one anyway.
func Join(url, root string) (Joined, error) {
	body, _ := json.Marshal(map[string]string{"root": root})
	res, err := client.Post(url+"/api/projects", "application/json", bytes.NewReader(body))
	if err != nil {
		return Joined{}, err
	}
	defer res.Body.Close()
	var out Joined
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return Joined{}, fmt.Errorf("the server at %s answered something unexpected", url)
	}
	if out.Error != "" {
		return out, fmt.Errorf("%s", out.Error)
	}
	return out, nil
}
