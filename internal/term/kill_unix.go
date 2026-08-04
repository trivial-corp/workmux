//go:build unix

package term

import "syscall"

// syscallKillGroup kills the whole process group.
//
// pty.Start puts the child in its own session, so the shell is a group leader and
// its children share the group. Signalling only the shell leaves whatever it
// started — a test run, a dev server, an agent — alive and holding the PTY, which
// looks like a session that won't die.
func syscallKillGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGHUP); err != nil {
		return syscall.Kill(pid, syscall.SIGKILL)
	}
	return nil
}
