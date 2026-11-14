package builtin

import (
	"path/filepath"
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

func TestCompileProviderBlocksAllProviderConfigsUseKnownProviderBlockTags(t *testing.T) {
	for _, config := range impeccableProviders {
		if len(config.ProviderTags) == 0 {
			t.Errorf("%s has no provider tags", config.Provider)
		}
		for _, tag := range config.ProviderTags {
			if !providerBlockTags[tag] {
				t.Errorf("%s uses unknown block tag %s", config.Provider, tag)
			}
		}
	}
}

func providerBlockConfig() impeccableProvider {
	return impeccableProvider{Provider: "cursor", ProviderTags: []string{"cursor"}, ConfigDir: ".test", DisplayName: "Test Provider", FrontmatterFields: []string{}}
}

func TestProviderBlockTransformerIntegrationCompilesProviderBlocksInSkillBodies(t *testing.T) {
	dist := factoryTransform(t, providerBlockConfig(), []impeccableSkill{{Name: "test", Description: "Test",
		Body: lines("Shared guidance.", "<cursor>", "Cursor-only guidance.", "</cursor>", "<codex>", "Codex-only guidance.", "</codex>")}})
	content := readTestFile(t, filepath.Join(dist, "cursor/.test/skills/test/SKILL.md"))
	wantContains(t, content, "Shared guidance.", "Cursor-only guidance.")
	wantLacks(t, content, "Codex-only guidance.", "<cursor>")
}

func TestProviderBlockTransformerIntegrationCompilesProviderBlocksInReferenceFiles(t *testing.T) {
	dist := factoryTransform(t, providerBlockConfig(), []impeccableSkill{{Name: "test", Description: "Test", Body: "Body", References: []impeccableFile{{Name: "ref",
		Content: lines("Shared reference.", "<cursor>", "Cursor reference.", "</cursor>", "<codex>", "Codex reference.", "</codex>")}}}})
	ref := readTestFile(t, filepath.Join(dist, "cursor/.test/skills/test/reference/ref.md"))
	wantContains(t, ref, "Shared reference.", "Cursor reference.")
	wantLacks(t, ref, "Codex reference.", "<cursor>")
}

func TestProviderBlockTransformerIntegrationCompilesProviderBlocksInGeneratedAgents(t *testing.T) {
	config := providerBlockConfig()
	config.Provider, config.ProviderTags, config.AgentFormat = "codex", []string{"codex"}, "codex-toml"
	dist := factoryTransform(t, config, []impeccableSkill{{Name: "test", Description: "Test", Body: "Body", Agents: []impeccableAgent{{
		Name: "review-helper", CodexName: "review_helper", Description: "Review helper",
		Body: lines("Shared agent guidance.", "<codex>", "Codex agent guidance.", "</codex>", "<claude-code>", "Claude agent guidance.", "</claude-code>"),
	}}}})
	agent := readTestFile(t, filepath.Join(dist, "codex/.test/agents/review_helper.toml"))
	wantContains(t, agent, "Shared agent guidance.", "Codex agent guidance.")
	wantLacks(t, agent, "Claude agent guidance.", "<codex>")
}

func TestProviderBlockTransformerIntegrationWritesNestedScriptArtifacts(t *testing.T) {
	dist := factoryTransform(t, providerBlockConfig(), []impeccableSkill{{Name: "test", Description: "Test", Body: "Body", Scripts: []impeccableScript{
		{Name: "detect.mjs", Content: "export {};\n"}, {Name: "detector/detect-antipatterns.mjs", Content: "export const bundled = true;\n"}}}})
	detector := filepath.Join(dist, "cursor/.test/skills/test/scripts/detector/detect-antipatterns.mjs")
	wantExists(t, detector)
	wantContains(t, readTestFile(t, detector), "bundled = true")
}
