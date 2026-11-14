//go:build !linux

package worktree

import "github.com/nickheyer/icebath/internal/proc"

func scanLivePIDs(roots []string, exclude map[int]bool) (map[string][]int, error) {
	table, err := proc.ReadTable()
	if err != nil {
		return nil, err
	}
	hits := map[string][]int{}
	for _, entry := range table {
		if exclude[entry.PID] {
			continue
		}
		cwd, err := proc.Cwd(entry.PID)
		if err != nil || cwd == "" {
			continue
		}
		if root := insideRoot(resolvePath(cwd), roots); root != "" {
			hits[root] = append(hits[root], entry.PID)
		}
	}
	return hits, nil
}
