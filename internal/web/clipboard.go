package web

import (
	"os/exec"
	"runtime"
	"time"

	"github.com/trivial-corp/workmux/internal/run"
)

// An agent CLI reads images from the *system clipboard* (⌃V — ⌘V belongs to the
// terminal emulator), which is the only way to hand it an image without leaving a
// path sitting in the prompt. So a pasted image is put on the host clipboard here
// and the browser sends ⌃V.
//
// Only possible when the server shares a clipboard with you: on your own machine it
// does, and on a remote box it doesn't — which is why the path is still returned as
// a fallback, and why this reports whether it worked rather than assuming.
func copyImageToClipboard(path string) bool {
	switch runtime.GOOS {
	case "darwin":
		// AppleScript is the only interface to a *typed* clipboard entry on macOS;
		// pbcopy would store the bytes as text, which no reader would recognise.
		script := `set the clipboard to (read (POSIX file "` + path + `") as «class PNGf»)`
		return run.Cmd("", 10*time.Second, "osascript", "-e", script).OK()
	case "linux":
		// Wayland first, then X11; a box with neither has no clipboard to write to.
		if _, err := exec.LookPath("wl-copy"); err == nil {
			return run.Cmd("", 10*time.Second, "sh", "-c",
				"wl-copy --type image/png < "+shellQuote(path)).OK()
		}
		if _, err := exec.LookPath("xclip"); err == nil {
			return run.Cmd("", 10*time.Second,
				"xclip", "-selection", "clipboard", "-t", "image/png", "-i", path).OK()
		}
	}
	return false
}

// shellQuote is for the one place a path reaches a shell.
func shellQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\\', '\'', '\'')
			continue
		}
		out = append(out, s[i])
	}
	return string(append(out, '\''))
}
