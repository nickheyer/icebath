//go:build linux

package worktree

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func scanLivePIDs(roots []string, exclude map[int]bool) (map[string][]int, error) {
	dir, err := os.Open("/proc")
	if err != nil {
		return nil, err
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return nil, err
	}
	hits := map[string][]int{}
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || strings.TrimLeft(name, "0123456789") != "" || exclude[pid] {
			continue
		}
		links := []string{filepath.Join("/proc", name, "cwd")}
		if fds, err := os.ReadDir(filepath.Join("/proc", name, "fd")); err == nil {
			for _, fd := range fds {
				links = append(links, filepath.Join("/proc", name, "fd", fd.Name()))
			}
		}
		for _, link := range links {
			target, err := os.Readlink(link)
			if err != nil {
				continue
			}
			if root := insideRoot(strings.TrimSuffix(target, " (deleted)"), roots); root != "" {
				hits[root] = append(hits[root], pid)
				break
			}
		}
	}
	return hits, nil
}
