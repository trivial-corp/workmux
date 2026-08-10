package stack

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/trivial-corp/workmux/internal/run"
)

// Foreign is a compose project docker is running that this repo did not start.
//
// The dashboard used to show only its own slots, which is right for "what is this branch
// running" and wrong for "why is my machine busy" — the containers a project depends on
// are usually a second compose project with a name that matches no slot pattern, and
// they were invisible here while being the reason the app wouldn't come up.
type Foreign struct {
	Name       string `json:"name"`
	ConfigFile string `json:"config_file"`
	Dir        string `json:"dir"`
	Running    bool   `json:"running"`
}

// AllProjects lists every compose project docker knows, ours and everyone else's.
func AllProjects() []Foreign {
	res := run.Cmd("", 12*time.Second, "docker", "compose", "ls", "--all", "--format", "json")
	if !res.OK() {
		return nil
	}
	var raw []struct {
		Name        string `json:"Name"`
		Status      string `json:"Status"`
		ConfigFiles string `json:"ConfigFiles"`
	}
	if json.Unmarshal([]byte(res.Out), &raw) != nil {
		return nil
	}
	out := make([]Foreign, 0, len(raw))
	for _, p := range raw {
		// The first config file is the one compose was invoked with; the rest are the
		// same file seen from other worktrees, which are not more projects.
		cf := strings.TrimSpace(strings.Split(p.ConfigFiles, ",")[0])
		out = append(out, Foreign{
			Name: p.Name, ConfigFile: cf, Dir: filepath.Dir(cf),
			Running: strings.Contains(p.Status, "running"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ReadNamed reports the services of a compose project by name, for one that belongs to
// no project of ours and so has no config to read it with.
func ReadNamed(p Foreign) State {
	if p.ConfigFile == "" {
		return State{Services: []Service{}}
	}
	res := run.Cmd(p.Dir, 15*time.Second, "docker", "compose", "-p", p.Name,
		"-f", p.ConfigFile, "ps", "--all", "--format", "json")
	if !res.OK() {
		return State{Services: []Service{}}
	}
	return parsePS(res.Out)
}

// Do runs a compose action against a project by name. Used for the ones no project of
// ours owns: theirs is whatever docker reported, so it is invoked the plain way rather
// than through a command some other repository configured.
func Do(p Foreign, action string) (string, error) {
	var args []string
	switch action {
	case "stop":
		args = []string{"down", "--remove-orphans"}
	case "restart":
		args = []string{"restart"}
	case "logs":
		args = []string{"logs", "--tail", "200", "--no-color"}
	default:
		return "", errString("unknown action")
	}
	if p.ConfigFile == "" {
		return "", errString("docker didn't say which compose file " + p.Name + " came from")
	}
	argv := append([]string{"docker", "compose", "-p", p.Name, "-f", p.ConfigFile}, args...)
	res := run.Cmd(p.Dir, 5*time.Minute, argv...)
	if !res.OK() {
		return res.Out, errString(res.LastLine(action + " failed"))
	}
	return res.Out, nil
}

type errString string

func (e errString) Error() string { return string(e) }
