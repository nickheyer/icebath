package tools

import "testing"

func TestGitRepoRootExportMatchesTheProbe(t *testing.T) {
	if GitRepoRoot() != gitRepoRoot() {
		t.Fatal("exported repo root disagrees with the probe")
	}
}
