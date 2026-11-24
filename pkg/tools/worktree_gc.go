package tools

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Directory size in megabytes through du with a bounded wait, nil on failure or timeout
func TreeSizeMB(path string, timeout time.Duration) *int {
	r := boundedProbeRun([]string{"du", "-sm", path}, timeout, nil, "", nil)
	if r == nil || r.Code != 0 || strings.TrimSpace(r.Stdout) == "" {
		return nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(strings.Fields(r.Stdout)[0]), 10, 64)
	if err != nil {
		return nil
	}
	v := int(n)
	return &v
}

// Tree count and total megabytes of the worktrees dir, size skipped once the count reaches sizeBelowCount
func WorktreesSummary(repoRoot string, sizeBelowCount *int) (int, *int) {
	worktreesDir := filepath.Join(repoRoot, ".worktrees")
	if _, err := os.Stat(worktreesDir); err != nil {
		return 0, nil
	}
	ents, err := os.ReadDir(worktreesDir)
	if err != nil {
		return 0, nil
	}
	count := 0
	for _, e := range ents {
		if st, err := os.Stat(filepath.Join(worktreesDir, e.Name())); err == nil && st.IsDir() {
			count++
		}
	}
	if sizeBelowCount != nil && count >= *sizeBelowCount {
		return count, nil
	}
	return count, TreeSizeMB(worktreesDir, 20*time.Second)
}
