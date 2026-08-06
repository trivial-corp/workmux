package web

import (
	"net/http"
	"sort"
	"sync"

	"github.com/trivial-corp/workmux/internal/project"
	"github.com/trivial-corp/workmux/internal/stack"
)

// StackRow is one running stack and what it costs.
type StackRow struct {
	Slot string `json:"slot"`
	Path string `json:"path"`
	URL  string `json:"url"`
	stack.State
	stack.Usage
}

// StacksView answers "what is running, and what is it costing me" in one read.
type StacksView struct {
	Enabled  bool          `json:"enabled"`
	Stacks   []StackRow    `json:"stacks"`
	Machine  stack.Machine `json:"machine"`
	NextSlot string        `json:"next_slot"`
	Docker   bool          `json:"docker"`
}

// handleStacks is deliberately its own endpoint rather than part of /api/work.
//
// `docker stats` has to watch every container for a moment before it can report a rate,
// so it costs about a second — worth paying when someone is looking at the panel, not
// every six seconds on a poll that the rest of the dashboard is waiting for.
func handleStacks(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	cfg := p.Cfg
	view := StacksView{Enabled: cfg.HasStack(), Stacks: []StackRow{}}
	if !view.Enabled {
		writeJSON(w, http.StatusOK, view)
		return
	}

	running := stack.Running(cfg)
	view.NextSlot = stack.NextFreeSlot(cfg, running)

	// The per-stack read and the machine-wide stats are independent docker round trips.
	var (
		wg     sync.WaitGroup
		states = make([]stack.State, len(running))
		usage  map[string]stack.Usage
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		usage, view.Machine = stack.Stats(cfg)
	}()
	for i, proj := range running {
		wg.Add(1)
		go func(i int, proj stack.Project) {
			defer wg.Done()
			states[i] = stack.Read(proj, cfg)
		}(i, proj)
	}
	wg.Wait()

	// Docker answered at all: with no containers anywhere the stats come back empty, and
	// "nothing is running" reads very differently from "docker isn't there".
	view.Docker = len(running) > 0 || view.Machine.Containers > 0

	for i, proj := range running {
		view.Stacks = append(view.Stacks, StackRow{
			Slot: proj.Slot, Path: proj.Dir, URL: cfg.StackURL(proj.Slot),
			State: states[i], Usage: usage[proj.Slot],
		})
	}
	sort.Slice(view.Stacks, func(i, j int) bool { return view.Stacks[i].Slot < view.Stacks[j].Slot })
	writeJSON(w, http.StatusOK, view)
}
