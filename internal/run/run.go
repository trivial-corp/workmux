// Package run is the one place this tool shells out.
//
// Everything workmux knows comes from git, docker, gh or the agent CLI, so the
// rules for calling them live here rather than at forty call sites: always a
// timeout (a hung `docker stats` must not hang the dashboard), stdout and stderr
// together (the reason a command refused is usually on stderr), and never a
// shell unless a shell is the point.
package run

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

// Trace, when set, is called after every command with what ran, how long it took
// and what it returned. Nil in normal operation; --verbose points it at the log.
// This is the debug view that matters: every fact workmux reports comes from one
// of these, so a wrong dashboard is nearly always a surprising command result.
var Trace func(argv []string, dur time.Duration, code int, out string)

// Result is what a command did. Out is stdout+stderr, trimmed.
type Result struct {
	Code int
	Out  string
}

// OK reports the boring case: it ran and it was happy.
func (r Result) OK() bool { return r.Code == 0 }

// Lines splits the output, dropping blanks.
func (r Result) Lines() []string {
	var out []string
	for _, ln := range strings.Split(r.Out, "\n") {
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

// LastLine is what to show a user when something failed: the final line of
// output is nearly always the actual complaint.
func (r Result) LastLine(fallback string) string {
	lines := r.Lines()
	if len(lines) == 0 {
		return fallback
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// Cmd runs argv in dir with a timeout. A missing binary or a timeout comes back
// as a non-zero Code, never a panic — callers degrade instead of failing.
func Cmd(dir string, timeout time.Duration, argv ...string) Result {
	return Env(dir, nil, timeout, argv...)
}

// Env is Cmd with extra environment entries ("KEY=value"), for the cases where
// PATH decides the answer.
func Env(dir string, env []string, timeout time.Duration, argv ...string) Result {
	if len(argv) == 0 {
		return Result{Code: 127, Out: "no command"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	started := time.Now()
	err := cmd.Run()
	elapsed := time.Since(started)

	res := Result{Out: strings.TrimRight(buf.String(), "\n")}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.Code = 124 // same convention as timeout(1)
		if res.Out == "" {
			res.Out = argv[0] + " timed out"
		}
	case err == nil:
		res.Code = 0
	default:
		var ee *exec.ExitError
		if errorsAs(err, &ee) {
			res.Code = ee.ExitCode()
		} else {
			res.Code = 127 // couldn't be started at all
			if res.Out == "" {
				res.Out = err.Error()
			}
		}
	}
	if Trace != nil {
		Trace(argv, elapsed, res.Code, res.Out)
	}
	return res
}

// Git is the common case: a git command against a specific tree.
func Git(dir string, timeout time.Duration, args ...string) Result {
	return Cmd("", timeout, append([]string{"git", "-C", dir}, args...)...)
}

// errorsAs is errors.As, kept local so this package imports nothing surprising.
func errorsAs(err error, target **exec.ExitError) bool {
	for err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			*target = ee
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
