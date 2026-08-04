// Package changes answers "what has this piece of work actually done".
//
// It's the question you ask a worktree most often, and a git TUI answers it badly
// at 400px: a TUI can't scroll a diff independently of a file list. So this is the
// same information as structured data, and the browser lays it out.
package changes

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trivial-corp/workmux/internal/run"
)

// diffMax bounds one diff. A generated lockfile can be megabytes, and nobody reads
// that in a browser; the point is to see what changed, not to receive all of it.
const diffMax = 400 << 10

// File is one changed path.
type File struct {
	Path      string `json:"path"`
	X         string `json:"x"` // index status
	Y         string `json:"y"` // worktree status
	Staged    bool   `json:"staged"`
	Untracked bool   `json:"untracked"`
	Add       int    `json:"add"`
	Del       int    `json:"del"`
}

// Commit is one commit this branch has that its base doesn't.
type Commit struct {
	SHA    string `json:"sha"`
	Msg    string `json:"msg"`
	Pushed bool   `json:"pushed"`
}

// View is the whole answer for one worktree.
type View struct {
	Branch  string   `json:"branch"`
	Base    string   `json:"base"`
	Files   []File   `json:"files"`
	Commits []Commit `json:"commits"`
}

// Read collects status and commits for a worktree, measured against base.
func Read(path, base string) View {
	v := View{Base: base, Files: []File{}, Commits: []Commit{}}

	res := run.Git(path, 20*time.Second, "status", "--porcelain")
	for _, line := range res.Lines() {
		if len(line) < 4 {
			continue
		}
		xy, rest := line[:2], line[3:]
		// A rename reads "old -> new"; the destination is the file you want to look at.
		if i := strings.Index(rest, " -> "); i >= 0 {
			rest = rest[i+4:]
		}
		v.Files = append(v.Files, File{
			Path:      strings.Trim(rest, `"`),
			X:         string(xy[0]),
			Y:         string(xy[1]),
			Staged:    xy[0] != ' ' && xy[0] != '?',
			Untracked: xy == "??",
		})
	}

	// Line counts from both sides, because a file can be partly staged and the row
	// should show the whole change.
	counts := numstat(path, false)
	for f, c := range numstat(path, true) {
		cur := counts[f]
		counts[f] = [2]int{cur[0] + c[0], cur[1] + c[1]}
	}
	for i := range v.Files {
		if c, ok := counts[v.Files[i].Path]; ok {
			v.Files[i].Add, v.Files[i].Del = c[0], c[1]
		}
	}
	// Staged first, then alphabetical: what you're about to commit is what you're
	// looking for.
	sort.SliceStable(v.Files, func(i, j int) bool {
		if v.Files[i].Staged != v.Files[j].Staged {
			return v.Files[i].Staged
		}
		return v.Files[i].Path < v.Files[j].Path
	})

	v.Commits = commits(path, base)
	if res := run.Git(path, 8*time.Second, "branch", "--show-current"); res.OK() {
		v.Branch = strings.TrimSpace(res.Out)
	}
	return v
}

// commits lists what this branch has that its base doesn't.
//
// Against the base, not @{upstream}: listing upstream..HEAD showed nothing the
// moment you pushed, which is exactly when a branch has the most to show.
func commits(path, base string) []Commit {
	var out string
	specs := []string{}
	if base != "" {
		specs = append(specs, "origin/"+base+"..HEAD")
	}
	specs = append(specs, "@{upstream}..HEAD", "")
	for _, spec := range specs {
		args := []string{"log", "--oneline", "--no-decorate", "-40"}
		if spec != "" {
			args = append(args, spec)
		}
		res := run.Git(path, 15*time.Second, args...)
		if res.OK() && strings.TrimSpace(res.Out) != "" {
			out = res.Out
			break
		}
	}

	// Which of them are still local, so "pushed" is read rather than guessed. No
	// upstream at all means it's unknowable, and claiming either way would be wrong.
	res := run.Git(path, 12*time.Second, "log", "--format=%h", "-40", "@{upstream}..HEAD")
	tracked := res.OK()
	local := map[string]bool{}
	if tracked {
		for _, sha := range strings.Fields(res.Out) {
			local[sha] = true
		}
	}

	list := []Commit{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		sha, msg, _ := strings.Cut(line, " ")
		list = append(list, Commit{SHA: sha, Msg: msg, Pushed: tracked && !local[sha]})
	}
	return list
}

var numstatLine = regexp.MustCompile(`^(\d+|-)\t(\d+|-)\t(.*)$`)

// numstat maps path → [added, deleted].
func numstat(path string, staged bool) map[string][2]int {
	args := []string{"diff", "--numstat", "--no-color"}
	if staged {
		args = append(args, "--cached")
	}
	out := map[string][2]int{}
	for _, line := range run.Git(path, 20*time.Second, args...).Lines() {
		m := numstatLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		add, _ := strconv.Atoi(m[1]) // "-" for a binary file, and 0 is the right answer
		del, _ := strconv.Atoi(m[2])
		name := m[3]
		// Renames appear as "old => new" inside the path; take the destination.
		if i := strings.Index(name, " => "); i >= 0 {
			name = strings.TrimRight(name[i+4:], "}")
		}
		out[name] = [2]int{add, del}
	}
	return out
}

// FileDiff is the unified diff for one file: the side asked for first, then the
// other side, and the whole contents only for a file git doesn't track.
//
// The order matters and the last step especially: falling back to --no-index
// whenever the diff came back empty reported every *unmodified* file as brand new.
func FileDiff(path, rel string, staged bool) string {
	if !safeRel(path, rel) {
		return ""
	}
	sides := [][]string{{}, {"--cached"}}
	if staged {
		sides = [][]string{{"--cached"}, {}}
	}
	for _, side := range sides {
		args := append([]string{"diff", "--no-color"}, side...)
		args = append(args, "--", rel)
		if res := run.Git(path, 25*time.Second, args...); strings.TrimSpace(res.Out) != "" {
			return clip(res.Out)
		}
	}
	// Tracked and unchanged: nothing to show, which is different from "new".
	if run.Git(path, 10*time.Second, "ls-files", "--error-unmatch", "--", rel).OK() {
		return ""
	}
	res := run.Git(path, 25*time.Second, "diff", "--no-color", "--no-index", "--", os.DevNull, rel)
	return clip(res.Out)
}

// CommitDiff is one commit, for reading what a past change actually did.
func CommitDiff(path, rev string) string {
	if !safeRev(rev) {
		return ""
	}
	res := run.Git(path, 25*time.Second, "show", "--no-color", "--stat", "--patch", rev)
	if !res.OK() {
		return ""
	}
	return clip(res.Out)
}

var revOK = regexp.MustCompile(`^[0-9a-fA-F]{4,40}$`)

// safeRev bounds what can be handed to `git show`. A revision is a hex sha here and
// nothing else — no ranges, no refs, no options.
func safeRev(rev string) bool { return revOK.MatchString(rev) }

// safeRel keeps a diff request inside the worktree. git would happily resolve
// ../../ out of it, and the answer would be a file the caller has no business
// reading through this.
func safeRel(root, rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "-") {
		return false
	}
	if filepath.IsAbs(rel) {
		return false
	}
	clean := filepath.Clean(filepath.Join(root, rel))
	return clean == root || strings.HasPrefix(clean, root+string(filepath.Separator))
}

func clip(s string) string {
	if len(s) > diffMax {
		return s[:diffMax] + "\n… truncated\n"
	}
	return s
}
