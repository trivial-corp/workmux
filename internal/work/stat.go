package work

import "os"

// statMtime is the worktree directory's own modification time — a cheap "was
// anything done here" that doesn't depend on an agent having written state.
func statMtime(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.ModTime().Unix(), nil
}
