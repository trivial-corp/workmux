// Package stack reads the project's containers — one stack per piece of work.
//
// Everything here degrades to "nothing running" rather than failing: docker may
// be absent (a project with no stack at all), stopped, or slow, and none of that
// should take the dashboard down with it.
package stack

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/run"
)

// Project is a running compose project that belongs to this repo.
type Project struct {
	Slot       string `json:"slot"`
	ConfigFile string `json:"config_file"`
	Dir        string `json:"dir"`
}

// Service is one container, reduced to what the UI shows.
type Service struct {
	Name     string   `json:"name"`
	State    string   `json:"state"`
	Health   string   `json:"health"`
	ExitCode int      `json:"exit_code"`
	Ports    []string `json:"ports"`
}

// State is one stack's health.
type State struct {
	Services     []Service `json:"services"`
	Up           int       `json:"up"`
	Total        int       `json:"total"`
	StartedEpoch int64     `json:"started_epoch"`
}

// Running lists this repo's running stacks, by slot.
//
// Filtered by the configured slot pattern: other projects on the same machine
// are not this repo's work, and claiming them would be worse than missing them.
func Running(cfg *config.Config) []Project {
	if !cfg.HasStack() {
		return nil
	}
	res := run.Cmd("", 12*time.Second, "docker", "compose", "ls", "--all", "--format", "json")
	if !res.OK() {
		return nil
	}
	var raw []struct {
		Name        string `json:"Name"`
		Status      string `json:"Status"`
		ConfigFiles string `json:"ConfigFiles"`
	}
	if err := json.Unmarshal([]byte(res.Out), &raw); err != nil {
		return nil
	}
	var out []Project
	for _, p := range raw {
		if !cfg.IsSlot(p.Name) || !strings.Contains(p.Status, "running") {
			continue
		}
		cf := strings.TrimSpace(strings.Split(p.ConfigFiles, ",")[0])
		if cf == "" {
			continue
		}
		out = append(out, Project{Slot: p.Name, ConfigFile: cf, Dir: filepath.Dir(cf)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

var createdAt = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{4})`)

// Read reports one stack's services and how long it has been up.
func Read(p Project, cfg *config.Config) State {
	res := run.Cmd(p.Dir, 15*time.Second, "docker", "compose", "-p", p.Slot,
		"-f", p.ConfigFile, "ps", "--all", "--format", "json")
	if !res.OK() {
		return State{Services: []Service{}}
	}
	return parsePS(res.Out)
}

// parsePS turns `docker compose ps` output into a State. Split out from Read so
// the shapes docker actually emits can be tested without docker.
func parsePS(out string) State {
	st := State{Services: []Service{}}
	type container struct {
		Service    string `json:"Service"`
		Name       string `json:"Name"`
		State      string `json:"State"`
		Health     string `json:"Health"`
		ExitCode   int    `json:"ExitCode"`
		CreatedAt  string `json:"CreatedAt"`
		Publishers []struct {
			PublishedPort int    `json:"PublishedPort"`
			TargetPort    int    `json:"TargetPort"`
			Protocol      string `json:"Protocol"`
		} `json:"Publishers"`
	}
	// `docker compose ps --format json` emits one object per line in some
	// versions and a single array in others. Accept both.
	var list []container
	trimmed := strings.TrimSpace(out)
	if strings.HasPrefix(trimmed, "[") {
		_ = json.Unmarshal([]byte(trimmed), &list)
	} else {
		for _, ln := range strings.Split(trimmed, "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue
			}
			var c container
			if json.Unmarshal([]byte(ln), &c) == nil {
				list = append(list, c)
			}
		}
	}

	bySvc := map[string]Service{}
	var earliest int64
	for _, c := range list {
		name := c.Service
		if name == "" {
			name = c.Name
		}
		if name == "" {
			name = "?"
		}
		state := strings.ToLower(c.State)
		svc := Service{
			Name: name, State: state, Health: strings.ToLower(c.Health),
			ExitCode: c.ExitCode, Ports: []string{},
		}
		for _, pub := range c.Publishers {
			if pub.PublishedPort != 0 {
				svc.Ports = append(svc.Ports, itoa(pub.PublishedPort)+"→"+itoa(pub.TargetPort))
			}
		}
		// A service can have a dead container beside a live one (a restart, a
		// one-shot job). Keep the most alive of them, or the row lies.
		if prev, ok := bySvc[name]; !ok || (state == "running" && prev.State != "running") {
			bySvc[name] = svc
		}
		if state == "running" && c.CreatedAt != "" {
			if m := createdAt.FindStringSubmatch(c.CreatedAt); m != nil {
				if t, err := time.Parse("2006-01-02 15:04:05 -0700", m[1]); err == nil {
					if earliest == 0 || t.Unix() < earliest {
						earliest = t.Unix()
					}
				}
			}
		}
	}
	for _, s := range bySvc {
		st.Services = append(st.Services, s)
		if s.State == "running" {
			st.Up++
		}
	}
	sort.Slice(st.Services, func(i, j int) bool { return st.Services[i].Name < st.Services[j].Name })
	st.Total = len(st.Services)
	st.StartedEpoch = earliest
	return st
}

// NextFreeSlot is the lowest slot number not currently running, so "start the
// app" doesn't silently land on a slot that's already busy.
func NextFreeSlot(cfg *config.Config, running []Project) string {
	if !cfg.HasStack() {
		return ""
	}
	taken := map[string]bool{}
	for _, p := range running {
		taken[p.Slot] = true
	}
	for n := 1; n <= 32; n++ {
		if s := cfg.SlotName(n); !taken[s] {
			return s
		}
	}
	return cfg.SlotName(1)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
