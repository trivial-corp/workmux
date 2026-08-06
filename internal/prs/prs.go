// Package prs reads pull requests through the gh CLI.
//
// Optional by design: without gh you lose PR titles and the open-PR list, and
// everything else works. That's why every failure here returns empty rather than
// an error.
package prs

import (
	"encoding/json"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/trivial-corp/workmux/internal/run"
)

// PR is a pull request, reduced to what the dashboard shows.
type PR struct {
	Number int    `json:"number"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Author string `json:"author"`
	Draft  bool   `json:"draft"`
}

// The cache is keyed by repository. One server serves several, and a single set of
// package-level variables handed project B the pull requests of whichever project
// polled first — every branch matched against the wrong repo's PRs.
type entry struct {
	byBranch map[string]PR
	open     []PR
	fetched  time.Time
}

var (
	mu    sync.Mutex
	cache = map[string]entry{}
)

// Data returns PRs indexed by head branch, plus the open ones newest first.
// Cached for 20s — it's a network round on every dashboard poll otherwise.
func Data(root string) (map[string]PR, []PR) {
	mu.Lock()
	if e, ok := cache[root]; ok && time.Since(e.fetched) < 20*time.Second {
		defer mu.Unlock()
		return e.byBranch, e.open
	}
	mu.Unlock()

	index, openList := load(root)

	mu.Lock()
	cache[root] = entry{byBranch: index, open: openList, fetched: time.Now()}
	mu.Unlock()
	return index, openList
}

func load(root string) (map[string]PR, []PR) {
	index, openList := map[string]PR{}, []PR{}
	if _, err := exec.LookPath("gh"); err != nil {
		return index, openList
	}
	res := run.Cmd(root, 20*time.Second, "gh", "pr", "list", "--state", "all",
		"--limit", "400", "--json",
		"number,headRefName,baseRefName,state,title,author,isDraft")
	if !res.OK() || res.Out == "" {
		return index, openList
	}
	var raw []struct {
		Number      int    `json:"number"`
		HeadRefName string `json:"headRefName"`
		BaseRefName string `json:"baseRefName"`
		State       string `json:"state"`
		Title       string `json:"title"`
		IsDraft     bool   `json:"isDraft"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal([]byte(res.Out), &raw); err != nil {
		return index, openList
	}
	for _, p := range raw {
		row := PR{
			Number: p.Number, Branch: p.HeadRefName, Base: p.BaseRefName,
			State: p.State, Title: p.Title, Author: p.Author.Login, Draft: p.IsDraft,
		}
		// gh lists newest first, and a branch can carry several PRs over its life
		// (a reopened one, a revert). The newest is the one that's about now.
		if row.Branch != "" {
			if _, seen := index[row.Branch]; !seen {
				index[row.Branch] = row
			}
		}
		if row.State == "OPEN" {
			openList = append(openList, row)
		}
	}
	sort.Slice(openList, func(i, j int) bool { return openList[i].Number > openList[j].Number })
	return index, openList
}

// Invalidate forces the next Data call for this repository to ask gh again, for
// right after something has opened or merged a PR.
func Invalidate(root string) {
	mu.Lock()
	delete(cache, root)
	mu.Unlock()
}
