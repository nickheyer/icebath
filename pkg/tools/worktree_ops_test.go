package tools

import (
	"testing"
	"time"
)

func TestWorktreeWrappersOutsideARepositoryReportNothing(t *testing.T) {
	dir := t.TempDir()
	if WorktreeIsDirty(dir, dir, 5*time.Second) != worktreeIsDirty(dir, dir, 5*time.Second) {
		t.Fatal("dirty wrapper disagrees")
	}
	if WorktreeHasUnpushedCommits(dir, 5*time.Second) != worktreeHasUnpushedCommits(dir, 5*time.Second) {
		t.Fatal("unpushed wrapper disagrees")
	}
	if RepoIsShallow(dir, 5*time.Second) != repoIsShallow(dir, 5*time.Second) {
		t.Fatal("shallow wrapper disagrees")
	}
	WorktreeGitQuiet([]string{"status"}, dir, 5*time.Second, "status outside a repository", nil)
}
