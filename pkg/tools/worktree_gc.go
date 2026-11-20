package tools

import (
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Directory size in megabytes through du with a bounded wait, nil on failure or timeout
func TreeSizeMB(path string, timeout time.Duration) *int {
	r := boundedProbeRun([]string{"du", "-sm", path}, timeout, nil, "", nil)
	if r == nil || r.Code != 0 || pystr.Strip(r.Stdout) == "" {
		return nil
	}
	n, ok := pyint.Parse(pystr.SplitWhitespace(r.Stdout, -1)[0])
	if !ok || !n.IsInt64() {
		return nil
	}
	v := int(n.Int64())
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
