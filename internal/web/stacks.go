package web

import (
	"net/http"
	"sort"
	"sync"

	"github.com/trivial-corp/workmux/internal/project"
	"github.com/trivial-corp/workmux/internal/stack"
)

// StackRow is one running compose project and what it costs.
type StackRow struct {
	Slot    string `json:"slot"`
	Project string `json:"project"` // the workmux project that owns it, "" for anyone else's
	Path    string `json:"path"`
	URL     string `json:"url"`
	Ours    bool   `json:"ours"`
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
//
// It reports every compose project docker is running, not only this repo's slots. The
// containers an app depends on are usually a second project whose name matches no slot
// pattern; they were invisible here while being the reason the app wouldn't start.
func handleStacks(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	cfg := p.Cfg
	view := StacksView{Enabled: cfg.HasStack(), Stacks: []StackRow{}}

	all := stack.AllProjects()
	running := stack.Running(cfg)
	if view.Enabled {
		view.NextSlot = stack.NextFreeSlot(cfg, running)
	}
	ours := map[string]stack.Project{}
	for _, x := range running {
		ours[x.Slot] = x
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		states = map[string]stack.State{}
		usage  map[string]stack.Usage
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		usage, view.Machine = stack.Stats(cfg)
	}()
	for _, f := range all {
		if !f.Running {
			continue
		}
		wg.Add(1)
		go func(f stack.Foreign) {
			defer wg.Done()
			st := stack.State{Services: []stack.Service{}}
			if own, mine := ours[f.Name]; mine {
				st = stack.Read(own, cfg)
			} else {
				st = stack.ReadNamed(f)
			}
			mu.Lock()
			states[f.Name] = st
			mu.Unlock()
		}(f)
	}
	wg.Wait()

	// Docker answered at all: with no containers anywhere the stats come back empty, and
	// "nothing is running" reads very differently from "docker isn't there".
	view.Docker = len(all) > 0 || view.Machine.Containers > 0

	for _, f := range all {
		if !f.Running {
			continue
		}
		own, mine := ours[f.Name]
		row := StackRow{
			Slot: f.Name, Path: f.Dir, Ours: mine,
			State: states[f.Name], Usage: usage[f.Name],
		}
		if mine {
			row.Project, row.Path, row.URL = p.ID, own.Dir, cfg.StackURL(own.Slot)
		}
		view.Stacks = append(view.Stacks, row)
	}
	sort.Slice(view.Stacks, func(i, j int) bool {
		if view.Stacks[i].Ours != view.Stacks[j].Ours {
			return view.Stacks[i].Ours // this repo's first; the rest are context
		}
		return view.Stacks[i].Slot < view.Stacks[j].Slot
	})
	writeJSON(w, http.StatusOK, view)
}

// handleStackOther acts on a compose project that belongs to no project of ours.
//
// Ours run as sessions, because starting one is a build you want to watch. These are
// somebody else's containers: stopping or restarting them is quick, there is no build,
// and the only thing worth showing is what compose said.
func handleStackOther(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Action string `json:"action"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	// The name has to be one docker is actually running: this takes a project name from
	// a request and runs docker with it, so the set of acceptable names is the set
	// docker reports, never whatever was asked for.
	var found *stack.Foreign
	for _, f := range stack.AllProjects() {
		if f.Name == req.Name {
			cp := f
			found = &cp
			break
		}
	}
	if found == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "docker isn't running a project called " + req.Name})
		return
	}
	if p.Cfg.IsSlot(found.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": found.Name + " is this project's own stack"})
		return
	}
	Journal.Note("compose %s %s", req.Action, found.Name)
	out, err := stack.Do(*found, req.Action)
	if err != nil {
		Journal.Note("compose %s %s failed: %s", req.Action, found.Name, err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "out": tail(out, 4000)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1", "out": tail(out, 4000)})
}
