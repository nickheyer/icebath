package files

import (
	"os"
	"path/filepath"
)

// Nearest folder at or above start holding a .git entry, empty after 50 levels or at the root
func FindGitRoot(start string) string {
	dir := start
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// Git root of a renderer path, nil for a refused path or no repository
func GitRootForIpc(startPath any) *string {
	resolved, err := ResolveRequestedPathForIpc(startPath, "Git root", "")
	if err != nil {
		return nil
	}
	start := resolved
	if st, err := os.Stat(resolved); err == nil {
		if !st.IsDir() {
			start = filepath.Dir(resolved)
		}
	}
	if root := FindGitRoot(start); root != "" {
		return &root
	}
	return nil
}
