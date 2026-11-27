package worktrees

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Hands each running process's folder and the files it holds open to see until it says it found what it looked for
func scan(see func(pid int, place string) bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", entry.Name())
		links := []string{filepath.Join(dir, "cwd")}
		if fds, err := os.ReadDir(filepath.Join(dir, "fd")); err == nil {
			for _, fd := range fds {
				links = append(links, filepath.Join(dir, "fd", fd.Name()))
			}
		}
		for _, link := range links {
			target, err := os.Readlink(link)
			if err == nil && filepath.IsAbs(target) && see(pid, strings.TrimSuffix(target, " (deleted)")) {
				break
			}
		}
	}
}
