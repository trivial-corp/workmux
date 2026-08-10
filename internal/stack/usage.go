package stack

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/run"
)

// Usage is what one stack is costing the machine right now.
type Usage struct {
	CPU        float64 `json:"cpu"`        // percent of one core, summed over containers
	MemBytes   int64   `json:"mem_bytes"`  // resident, summed
	Containers int     `json:"containers"` // how many are running
}

// Machine is everything docker is running, so a stack's cost can be read against
// something. Without the total, "3.4 GB" is a number with no opinion attached.
type Machine struct {
	Usage
	Others   int   `json:"others"`    // running containers that are not this repo's stacks
	MemTotal int64 `json:"mem_total"` // what docker says it has, 0 if it won't say
}

type statsCache struct {
	mu   sync.Mutex
	at   time.Time
	slot map[string]Usage
	mach Machine
}

var stats statsCache

// Stats reports per-slot and machine-wide usage.
//
// Cached for ten seconds: `docker stats --no-stream` has to sample every container for a
// moment before it can report a rate, so it costs about a second every time, and the
// panel that shows it is polled.
func Stats(cfg *config.Config) (map[string]Usage, Machine) {
	stats.mu.Lock()
	if time.Since(stats.at) < 10*time.Second && stats.slot != nil {
		defer stats.mu.Unlock()
		return stats.slot, stats.mach
	}
	stats.mu.Unlock()

	bySlot, mach := readStats(cfg)

	stats.mu.Lock()
	stats.slot, stats.mach, stats.at = bySlot, mach, time.Now()
	stats.mu.Unlock()
	return bySlot, mach
}

func readStats(cfg *config.Config) (map[string]Usage, Machine) {
	bySlot := map[string]Usage{}
	var mach Machine

	// Which project each container belongs to comes from the compose label, not from the
	// container's name: a name only looks like "slot-service-1" until someone sets
	// container_name, and then the whole table is wrong.
	res := run.Cmd("", 12*time.Second, "docker", "ps", "--format", "{{.ID}}\t{{.Label \"com.docker.compose.project\"}}")
	if !res.OK() {
		return bySlot, mach
	}
	project := map[string]string{}
	for _, ln := range strings.Split(strings.TrimSpace(res.Out), "\n") {
		id, proj, ok := strings.Cut(strings.TrimSpace(ln), "\t")
		if !ok || id == "" {
			continue
		}
		project[id] = strings.TrimSpace(proj)
	}
	if len(project) == 0 {
		return bySlot, mach
	}

	res = run.Cmd("", 25*time.Second, "docker", "stats", "--no-stream", "--format", "{{json .}}")
	if !res.OK() {
		return bySlot, mach
	}
	for _, ln := range strings.Split(strings.TrimSpace(res.Out), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var row struct {
			ID       string `json:"ID"`
			CPUPerc  string `json:"CPUPerc"`
			MemUsage string `json:"MemUsage"`
		}
		if json.Unmarshal([]byte(ln), &row) != nil {
			continue
		}
		cpu := parsePercent(row.CPUPerc)
		mem := parseMem(row.MemUsage)
		mach.CPU += cpu
		mach.MemBytes += mem
		mach.Containers++

		name := project[row.ID]
		if name == "" {
			mach.Others++
			continue
		}
		// Keyed by compose project, ours or not. Counting only our own slots left every
		// other project in the panel reading 0% and 0 B — which is exactly the claim
		// the panel exists to disprove.
		if !cfg.IsSlot(name) {
			mach.Others++
		}
		u := bySlot[name]
		u.CPU += cpu
		u.MemBytes += mem
		u.Containers++
		bySlot[name] = u
	}
	mach.MemTotal = memTotal()
	return bySlot, mach
}

// memTotal is what docker believes the machine has. On Docker Desktop that's the VM's
// allowance rather than the host's RAM, which is the number that actually constrains a
// stack, so it's the right one to show.
func memTotal() int64 {
	res := run.Cmd("", 10*time.Second, "docker", "info", "--format", "{{.MemTotal}}")
	if !res.OK() {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(res.Out), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func parsePercent(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%")), 64)
	if err != nil {
		return 0
	}
	return f
}

// parseMem reads the left side of "1.234GiB / 7.653GiB". Docker writes binary units with
// an i and decimal ones without, and means it.
func parseMem(s string) int64 {
	left, _, _ := strings.Cut(s, "/")
	left = strings.TrimSpace(left)
	if left == "" {
		return 0
	}
	units := []struct {
		suffix string
		mult   float64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"TiB", 1 << 40},
		{"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"KB", 1e3}, {"TB", 1e12}, {"B", 1},
	}
	for _, u := range units {
		if !strings.HasSuffix(left, u.suffix) {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(left, u.suffix)), 64)
		if err != nil {
			return 0
		}
		return int64(n * u.mult)
	}
	return 0
}
