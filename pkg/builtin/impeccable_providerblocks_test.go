package builtin

import (
	"strings"
	"testing"
)

func lines(parts ...string) string {
	return strings.Join(parts, "\n")
}

func wantCompiled(t *testing.T, content string, active []string, want string) {
	t.Helper()
	if got := compileProviderBlocks(content, active); got != want {
		t.Fatalf("%v compiled to %q, want %q", active, got, want)
	}
}

func TestCompileProviderBlocksKeepsMatchingProviderBlockBodiesAndRemovesTags(t *testing.T) {
	wantCompiled(t, lines("Before", "<codex>", "Codex-only guidance.", "</codex>", "After"), []string{"codex"}, lines("Before", "Codex-only guidance.", "After"))
}

func TestCompileProviderBlocksCompilesOmpBlocksOnlyForOmpOutput(t *testing.T) {
	content := lines("<omp>", "OMP-only guidance.", "</omp>")
	wantCompiled(t, content, []string{"omp"}, "OMP-only guidance.")
	wantCompiled(t, content, []string{"cursor"}, "")
}

func TestCompileProviderBlocksCompilesDevinBlocksOnlyForDevinOutput(t *testing.T) {
	content := lines("<devin>", "Devin-only guidance.", "</devin>")
	wantCompiled(t, content, []string{"devin"}, "Devin-only guidance.")
	wantCompiled(t, content, []string{"cursor"}, "")
}

func TestCompileProviderBlocksRemovesNonMatchingProviderBlocks(t *testing.T) {
	wantCompiled(t, lines("Before", "<codex>", "Codex-only guidance.", "</codex>", "After"), []string{"claude-code"}, lines("Before", "", "After"))
}

func TestCompileProviderBlocksDoesNotLeaveExtraBlankLinesAroundStrippedBlocks(t *testing.T) {
	wantCompiled(t, lines("Before", "", "<codex>", "Codex-only guidance.", "</codex>", "", "After"), []string{"claude-code"}, lines("Before", "", "After"))
}

func TestCompileProviderBlocksPreservesUnknownStandaloneTags(t *testing.T) {
	content := lines("Before", "<aside>", "Normal markdown HTML.", "</aside>", "After")
	wantCompiled(t, content, []string{"codex"}, content)
}

func TestCompileProviderBlocksKeepsCodexBlocksForTargetsThatOptIntoTheCodexTag(t *testing.T) {
	wantCompiled(t, lines("<codex>", "Codex repo skill guidance.", "</codex>"), []string{"agents", "codex"}, "Codex repo skill guidance.")
}
