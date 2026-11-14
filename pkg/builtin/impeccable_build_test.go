package builtin

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

func transformProvider(t *testing.T, provider string, skills []impeccableSkill, distDir string) {
	t.Helper()
	config, ok := impeccableProviderByName(provider)
	if !ok {
		t.Fatalf("no provider %s", provider)
	}
	if err := config.transform(skills, distDir, "", io.Discard); err != nil {
		t.Fatalf("%s transform: %v", provider, err)
	}
}

func readSourceSkills(t *testing.T, root string) []impeccableSkill {
	t.Helper()
	skills, err := readImpeccableSource(root)
	if err != nil {
		t.Fatal(err)
	}
	return skills
}

func wantExists(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if !pathExists(p) {
			t.Errorf("%s does not exist", p)
		}
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

var ruleMarkerOpenRe = regexp.MustCompile(`<!--\s*rule:`)

func TestBuildOrchestrationIntegrationFullBuildCreatesAllExpectedOutputs(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "skill/SKILL.src.md", "---\nname: test-skill\ndescription: A test skill\nlicense: MIT\n---\n\nThis is a test skill body.")
	dist := filepath.Join(root, "dist")
	skills := readSourceSkills(t, root)
	for _, provider := range []string{"cursor", "claude-code", "gemini", "codex", "omp", "devin", "antigravity"} {
		transformProvider(t, provider, skills, dist)
	}
	wantExists(t,
		filepath.Join(dist, "cursor/.cursor/skills/test-skill/SKILL.md"),
		filepath.Join(dist, "claude-code/.claude/skills/test-skill/SKILL.md"),
		filepath.Join(dist, "gemini/.gemini/skills/test-skill/SKILL.md"),
		filepath.Join(dist, "codex/.codex/skills/test-skill/SKILL.md"),
		filepath.Join(dist, "antigravity/.agent/skills/test-skill/SKILL.md"))
}

func TestBuildOrchestrationIntegrationEmitsNativeSubagentFilesForCodexClaudeCodeGitHubCopilotAndCursor(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "skill/SKILL.src.md", "---\nname: test-skill\ndescription: A test skill\n---\n\nThis is a test skill body.")
	writeAt(t, root, "skill/agents/asset-producer.md", "---\nname: asset-producer\ncodex-name: asset_producer\ndescription: Produces assets from approved crops\n"+
		"tools: Read, Write\nmodel: inherit\neffort: medium\nmax-turns: 8\nnickname-candidates:\n  - Asset Plate\n---\n\nDo not redesign the approved crop.")
	dist := filepath.Join(root, "dist")
	skills := readSourceSkills(t, root)
	for _, provider := range []string{"claude-code", "codex", "github", "cursor"} {
		transformProvider(t, provider, skills, dist)
	}
	claudeAgentPath := filepath.Join(dist, "claude-code/.claude/agents/asset-producer.md")
	codexAgentPath := filepath.Join(dist, "codex/.codex/skills/test-skill/agents/asset_producer.toml")
	copilotAgentPath := filepath.Join(dist, "github/.github/agents/asset-producer.agent.md")
	cursorAgentPath := filepath.Join(dist, "cursor/.cursor/agents/asset-producer.md")
	wantExists(t, claudeAgentPath, codexAgentPath, copilotAgentPath, cursorAgentPath)

	wantContains(t, readTestFile(t, claudeAgentPath), "name: asset-producer", "tools: Read, Write", "maxTurns: 8")
	wantContains(t, readTestFile(t, codexAgentPath), `name = "asset_producer"`, `model_reasoning_effort = "medium"`,
		`nickname_candidates = ["Asset Plate"]`, "developer_instructions =")

	copilotAgent := readTestFile(t, copilotAgentPath)
	wantContains(t, copilotAgent, "name: asset-producer", "description: Produces assets from approved crops", "Do not redesign the approved crop.")
	wantLacks(t, copilotAgent, "tools:", "model:", "effort:", "maxTurns:")

	cursorAgent := readTestFile(t, cursorAgentPath)
	wantContains(t, cursorAgent, "name: asset-producer", "description: Produces assets from approved crops", "model: inherit",
		"is_background: false", "Do not redesign the approved crop.")
	wantLacks(t, cursorAgent, "readonly:", "tools:", "effort:", "maxTurns:")
}

func TestBuildOrchestrationIntegrationVerifyTransformationsAreCorrect(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "skill/SKILL.src.md", "---\nname: audit\ndescription: Run technical quality checks\nuser-invocable: true\n"+
		"argument-hint: \"[TARGET=<value>]\"\n---\n\nPlease audit {{target}} for technical quality. Ask {{model}} for help.")
	dist := filepath.Join(root, "dist")
	skills := readSourceSkills(t, root)
	for _, provider := range []string{"cursor", "claude-code", "gemini", "codex"} {
		transformProvider(t, provider, skills, dist)
	}
	wantContains(t, readTestFile(t, filepath.Join(dist, "cursor/.cursor/skills/audit/SKILL.md")), "---", "name: audit", "{{target}}", "the model")
	wantContains(t, readTestFile(t, filepath.Join(dist, "claude-code/.claude/skills/audit/SKILL.md")),
		"---", "name: audit", "user-invocable: true", "{{target}}", "Claude")
	wantContains(t, readTestFile(t, filepath.Join(dist, "gemini/.gemini/skills/audit/SKILL.md")), "{{target}}", "Gemini")
	wantContains(t, readTestFile(t, filepath.Join(dist, "codex/.codex/skills/audit/SKILL.md")), "{{target}}", "GPT")
}

func TestBuildOrchestrationShouldIncludeAgentsOmpDevinAndKiroTransformers(t *testing.T) {
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	skills := readSourceSkills(t, root)
	for _, provider := range []string{"agents", "omp", "devin", "github", "kiro"} {
		transformProvider(t, provider, skills, dist)
	}
	wantExists(t, filepath.Join(dist, "agents/.agents/skills"), filepath.Join(dist, "omp/.omp/skills"), filepath.Join(dist, "devin/.devin/skills"),
		filepath.Join(dist, "github/.github/skills"), filepath.Join(dist, "kiro/.kiro/skills"))
}

func TestBuildOrchestrationAntigravityTransformerEmitsSkillsUnderAgent(t *testing.T) {
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	transformProvider(t, "antigravity", readSourceSkills(t, root), dist)
	wantExists(t, filepath.Join(dist, "antigravity/.agent/skills"))
}

func sourceScripts(t *testing.T) map[string]impeccableScript {
	t.Helper()
	skills := readSourceSkills(t, impeccablePayloadRoot)
	if len(skills) != 1 {
		t.Fatalf("%d skills in the source", len(skills))
	}
	scripts := map[string]impeccableScript{}
	for _, s := range skills[0].Scripts {
		scripts[s.Name] = s
	}
	return scripts
}

func TestSkillScriptsPayloadShipsTheLauncherVersionPageJSAndCommandMetadata(t *testing.T) {
	scripts := sourceScripts(t)
	for _, expected := range []string{"impeccable", "impeccable.cmd", "VERSION", "command-metadata.json",
		"live-browser.js", "live-browser-dom.js", "live-browser-session.js", "modern-screenshot.umd.js"} {
		if _, ok := scripts[expected]; !ok {
			t.Errorf("scripts payload lacks %s", expected)
		}
	}
}

func TestSkillScriptsPayloadShipsTheWindowsLauncherWithCRLFLineEndings(t *testing.T) {
	cmd, ok := sourceScripts(t)["impeccable.cmd"]
	if !ok || !strings.Contains(cmd.Content, "\r\n") || regexp.MustCompile(`(^|[^\r])\n`).MatchString(cmd.Content) {
		t.Fatalf("impeccable.cmd present %v holds a bare LF or no CRLF", ok)
	}
}

func TestSkillScriptsPayloadShipsNoEngineEntryPointsAndNoBundledDetector(t *testing.T) {
	allowedNodeScripts := []string{"build-phase.mjs", "comp-diff.mjs", "comp-spec.mjs", "font-match.mjs", "lib/font-fingerprint.mjs",
		"lib/font-index.mjs", "lib/hero-checks.mjs", "lib/image-metrics.mjs", "lib/png.mjs", "lib/raster.mjs"}
	var stray []string
	for name := range sourceScripts(t) {
		if (strings.HasSuffix(name, ".mjs") || strings.HasPrefix(name, "detector/") || strings.HasPrefix(name, "lib/")) && !slices.Contains(allowedNodeScripts, name) {
			stray = append(stray, name)
		}
	}
	if len(stray) != 0 {
		t.Fatalf("stray scripts %v", stray)
	}
}

func TestSkillScriptsPayloadNeverReadsPlatformBinariesAsSource(t *testing.T) {
	for name := range sourceScripts(t) {
		if strings.HasPrefix(name, "bin/") {
			t.Errorf("platform binary %s read as source", name)
		}
	}
	root := t.TempDir()
	writeAt(t, root, "skill/SKILL.src.md", "---\nname: impeccable\n---\n\nBody.")
	writeAt(t, root, "skill/scripts/bin/linux-x64/impeccable", "binary")
	writeAt(t, root, "skill/scripts/config.json", "{}")
	writeAt(t, root, "skill/scripts/data/bin/kept.json", "{}")
	var names []string
	for _, s := range readSourceSkills(t, root)[0].Scripts {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"data/bin/kept.json"}) {
		t.Fatalf("scripts read %v", names)
	}
}

func degradedDist(t *testing.T) string {
	t.Helper()
	dist := filepath.Join(t.TempDir(), "dist")
	skills := readSourceSkills(t, impeccablePayloadRoot)
	transformProvider(t, "claude-code", skills, dist)
	transformProvider(t, "codex", skills, dist)
	return dist
}

func readDegraded(t *testing.T, dist, provider, configDir, role string) string {
	t.Helper()
	return readTestFile(t, filepath.Join(dist, provider, configDir, "skills", "impeccable", "reference", "degraded", role+".md"))
}

var degradedRoles = []string{"asset-producer.md", "documenter.md", "finish-reviewer.md", "manual-edit-applier.md"}

func TestDegradedModeFallbackReferenceGenerationABuildEmitsReferenceDegradedRoleMdForEveryAgentPrefixStripped(t *testing.T) {
	dist := degradedDist(t)
	files := dirNames(t, filepath.Join(dist, "codex", ".codex", "skills", "impeccable", "reference", "degraded"))
	if !slices.Equal(files, degradedRoles) {
		t.Fatalf("degraded files %v", files)
	}
}

func TestDegradedModeFallbackReferenceGenerationFinishReviewerFallbackOpensWithThePreambleAndCarriesADistinctiveBodyPhrase(t *testing.T) {
	content := readDegraded(t, degradedDist(t), "codex", ".codex", "finish-reviewer")
	if !strings.HasPrefix(content, "<!-- Generated from skill/agents/ at build time. Do not edit; edit the agent definition. -->") {
		t.Fatalf("fallback opens with %q", content[:min(len(content), 120)])
	}
	wantContains(t, content, "This harness has no subagent capability, so you are running this role inline.", "material_fixes")
}

func TestDegradedModeFallbackReferenceGenerationTheDisclosureStaysOutOfTheOutputContract(t *testing.T) {
	wantContains(t, readDegraded(t, degradedDist(t), "codex", ".codex", "manual-edit-applier"), "outside the output contract")
}

func TestDegradedModeFallbackReferenceGenerationGeneratedFallbacksPassThroughProviderBlockCompilation(t *testing.T) {
	synthetic := impeccableSkill{
		Name:        "impeccable",
		Description: "synthetic",
		Body:        "Synthetic skill body.",
		Agents: []impeccableAgent{{
			Name: "impeccable-synthetic",
			Body: "Shared body line.\n\n<codex>\nCODEX_ONLY_MARKER for the codex target.\n</codex>\n\nMore shared body.",
		}},
	}
	synthDist := filepath.Join(t.TempDir(), "synth")
	transformProvider(t, "codex", []impeccableSkill{synthetic}, synthDist)
	transformProvider(t, "claude-code", []impeccableSkill{synthetic}, synthDist)
	codex := readDegraded(t, synthDist, "codex", ".codex", "synthetic")
	claude := readDegraded(t, synthDist, "claude-code", ".claude", "synthetic")
	wantContains(t, codex, "CODEX_ONLY_MARKER")
	wantLacks(t, claude, "CODEX_ONLY_MARKER")
	if !strings.HasPrefix(codex, "<!-- Generated from skill/agents/") {
		t.Fatalf("codex fallback opens with %q", codex[:min(len(codex), 80)])
	}
	wantContains(t, claude, "More shared body.")
}

func providerAgentsDir(t *testing.T, provider, configDir string) (string, string) {
	t.Helper()
	dist := filepath.Join(t.TempDir(), "dist")
	transformProvider(t, provider, readSourceSkills(t, impeccablePayloadRoot), dist)
	return dist, filepath.Join(dist, provider, configDir, "agents")
}

func TestGitHubCopilotCustomAgentGenerationEmitsGithubAgentsNameAgentMdForEveryShippedAgent(t *testing.T) {
	_, agentsDir := providerAgentsDir(t, "github", ".github")
	want := []string{"impeccable-asset-producer.agent.md", "impeccable-documenter.agent.md", "impeccable-finish-reviewer.agent.md", "impeccable-manual-edit-applier.agent.md"}
	if files := dirNames(t, agentsDir); !slices.Equal(files, want) {
		t.Fatalf("agent files %v", files)
	}
}

func TestGitHubCopilotCustomAgentGenerationBodiesAreCompiledPlaceholdersResolvedRuleMarkersStripped(t *testing.T) {
	_, agentsDir := providerAgentsDir(t, "github", ".github")
	for _, name := range dirNames(t, agentsDir) {
		content := readTestFile(t, filepath.Join(agentsDir, name))
		if strings.Contains(content, "{{") || ruleMarkerOpenRe.MatchString(content) {
			t.Errorf("%s keeps a brace token or rule marker", name)
		}
	}
	wantContains(t, readTestFile(t, filepath.Join(agentsDir, "impeccable-asset-producer.agent.md")), ".github/skills/impeccable/scripts")
	wantContains(t, readTestFile(t, filepath.Join(agentsDir, "impeccable-finish-reviewer.agent.md")), "material_fixes")
}

func TestGitHubCopilotCustomAgentGenerationDegradedFallbacksStillShipForTheGithubProviderAlongsideTheRealAgents(t *testing.T) {
	dist, _ := providerAgentsDir(t, "github", ".github")
	files := dirNames(t, filepath.Join(dist, "github", ".github", "skills", "impeccable", "reference", "degraded"))
	if !slices.Equal(files, degradedRoles) {
		t.Fatalf("degraded files %v", files)
	}
}

func TestCursorSubagentGenerationEmitsCursorAgentsNameMdForEveryShippedAgent(t *testing.T) {
	_, agentsDir := providerAgentsDir(t, "cursor", ".cursor")
	want := []string{"impeccable-asset-producer.md", "impeccable-documenter.md", "impeccable-finish-reviewer.md", "impeccable-manual-edit-applier.md"}
	if files := dirNames(t, agentsDir); !slices.Equal(files, want) {
		t.Fatalf("agent files %v", files)
	}
}

func TestCursorSubagentGenerationFrontmatterMapsNameDescriptionModelInheritIsBackgroundFalseReadonlyOnlyOnTheReviewer(t *testing.T) {
	_, agentsDir := providerAgentsDir(t, "cursor", ".cursor")
	for _, name := range dirNames(t, agentsDir) {
		frontmatter := strings.Split(readTestFile(t, filepath.Join(agentsDir, name)), "---")[1]
		wantContains(t, frontmatter, "name: "+strings.TrimSuffix(name, ".md"), "description: ", "model: inherit", "is_background: false")
		wantLacks(t, frontmatter, "tools:", "effort:", "maxTurns:")
		if name == "impeccable-finish-reviewer.md" {
			wantContains(t, frontmatter, "readonly: true")
		} else {
			wantLacks(t, frontmatter, "readonly:")
		}
	}
}

func TestCursorSubagentGenerationBodiesAreCompiledPlaceholdersResolvedRuleMarkersStripped(t *testing.T) {
	_, agentsDir := providerAgentsDir(t, "cursor", ".cursor")
	for _, name := range dirNames(t, agentsDir) {
		content := readTestFile(t, filepath.Join(agentsDir, name))
		if strings.Contains(content, "{{") || ruleMarkerOpenRe.MatchString(content) {
			t.Errorf("%s keeps a brace token or rule marker", name)
		}
	}
	wantContains(t, readTestFile(t, filepath.Join(agentsDir, "impeccable-asset-producer.md")), ".cursor/skills/impeccable/scripts")
}

var agentSurfaces = [][2]string{
	{"codex/.codex/skills/impeccable/agents/impeccable_asset_producer.toml", ".codex/skills/impeccable/scripts"},
	{"agents/.agents/skills/impeccable/agents/impeccable_asset_producer.toml", ".agents/skills/impeccable/scripts"},
	{"claude-code/.claude/agents/impeccable-asset-producer.md", ".claude/skills/impeccable/scripts"},
	{"github/.github/agents/impeccable-asset-producer.agent.md", ".github/skills/impeccable/scripts"},
	{"grok/.grok/agents/impeccable-asset-producer.md", ".grok/skills/impeccable/scripts"},
	{"codex/.codex/skills/impeccable/reference/degraded/asset-producer.md", ".codex/skills/impeccable/scripts"},
}

func agentSurfacesDist(t *testing.T) string {
	t.Helper()
	dist := filepath.Join(t.TempDir(), "dist")
	skills := readSourceSkills(t, impeccablePayloadRoot)
	for _, provider := range []string{"codex", "agents", "claude-code", "github", "grok"} {
		transformProvider(t, provider, skills, dist)
	}
	return dist
}

func TestAgentBodiesResolvePlaceholdersOnEverySurfaceThatShipsThemTheAssetProducerShipsARunnableEmbedPromptCommandNeverTheRawToken(t *testing.T) {
	dist := agentSurfacesDist(t)
	for _, surface := range agentSurfaces {
		content := readTestFile(t, filepath.Join(dist, filepath.FromSlash(surface[0])))
		wantContains(t, content, surface[1]+"/impeccable embed-prompt")
		wantLacks(t, content, "{{scripts_path}}")
	}
}

func TestAgentBodiesResolvePlaceholdersOnEverySurfaceThatShipsThemTheAssetProducerLinksToComponentReviewMdFromEverySurface(t *testing.T) {
	dist := agentSurfacesDist(t)
	for _, surface := range agentSurfaces {
		file := filepath.Join(dist, filepath.FromSlash(surface[0]))
		m := regexp.MustCompile(`\[component-review\.md\]\(([^)]+)\)`).FindStringSubmatch(readTestFile(t, file))
		if m == nil {
			t.Fatalf("%s holds no component-review.md link", surface[0])
		}
		wantExists(t, filepath.Join(filepath.Dir(file), filepath.FromSlash(m[1])))
	}
}

func TestAgentBodiesResolvePlaceholdersOnEverySurfaceThatShipsThemNoEmittedAgentBodyCarriesAnUnresolvedPlaceholderOrARuleMarker(t *testing.T) {
	synthetic := impeccableSkill{
		Name:        "impeccable",
		Description: "synthetic",
		Body:        "Synthetic skill body.",
		Agents: []impeccableAgent{{
			Name:        "impeccable-synthetic",
			CodexName:   "impeccable_synthetic",
			Description: "synthetic agent",
			Body:        "Run `{{scripts_path}}/impeccable embed-prompt` and ask {{model}}. See [x.md]({{reference_path}}/x.md). <!-- rule:synthetic-marker -->",
		}},
	}
	synthDist := filepath.Join(t.TempDir(), "synth")
	transformProvider(t, "codex", []impeccableSkill{synthetic}, synthDist)
	transformProvider(t, "claude-code", []impeccableSkill{synthetic}, synthDist)
	for _, rel := range []string{
		"codex/.codex/skills/impeccable/agents/impeccable_synthetic.toml",
		"codex/.codex/skills/impeccable/reference/degraded/synthetic.md",
		"claude-code/.claude/agents/impeccable-synthetic.md",
	} {
		content := readTestFile(t, filepath.Join(synthDist, filepath.FromSlash(rel)))
		if strings.Contains(content, "{{") || ruleMarkerOpenRe.MatchString(content) {
			t.Errorf("%s keeps a brace token or rule marker\n%s", rel, content)
		}
	}
	codexToml := readTestFile(t, filepath.Join(synthDist, "codex/.codex/skills/impeccable/agents/impeccable_synthetic.toml"))
	wantContains(t, codexToml, ".codex/skills/impeccable/scripts/impeccable embed-prompt")
	if !regexp.MustCompile(`and ask \S+\.`).MatchString(codexToml) {
		t.Fatalf("model token unresolved\n%s", codexToml)
	}
}

func TestBatchLineEndingsToCrlfConvertsLFAndLeavesCRLFAlone(t *testing.T) {
	if got := toCrlf("a\nb\r\nc\n"); got != "a\r\nb\r\nc\r\n" {
		t.Fatalf("%q", got)
	}
}

func TestBatchLineEndingsFindBatchFilesWithBareLfFlagsOnlyBatchFilesWithABareLF(t *testing.T) {
	dir := t.TempDir()
	writeAt(t, dir, "a/scripts/good.cmd", "@echo off\r\ncall :x\r\n")
	writeAt(t, dir, "a/scripts/bad.cmd", "@echo off\r\ncall :x\n")
	writeAt(t, dir, "a/scripts/impeccable", "#!/bin/sh\nexit 0\n")
	bad, err := findBatchFilesWithBareLf([]string{dir, filepath.Join(dir, "missing")})
	if err != nil || !slices.Equal(bad, []string{filepath.Join(dir, "a", "scripts", "bad.cmd")}) {
		t.Fatalf("bad %v, err %v", bad, err)
	}
}

var orchestrationProviders = []string{"cursor", "claude-code", "gemini", "codex"}

func TestBuildOrchestrationShouldCallReadSourceFilesWithRootDirectory(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	writeAt(t, rootA, "skill/SKILL.src.md", "---\nname: from-a\n---\n\nBody A.")
	writeAt(t, rootB, "skill/SKILL.src.md", "---\nname: from-b\n---\n\nBody B.")
	a, b := readSourceSkills(t, rootA), readSourceSkills(t, rootB)
	if len(a) != 1 || len(b) != 1 || a[0].Name != "from-a" || b[0].Name != "from-b" {
		t.Fatalf("%+v %+v", a, b)
	}
	if a[0].FilePath != filepath.Join(rootA, "skill", "SKILL.src.md") {
		t.Fatalf("skill read from %s", a[0].FilePath)
	}
}

func TestBuildOrchestrationShouldCallAllTransformersWithCorrectArguments(t *testing.T) {
	skills := []impeccableSkill{{Name: "skill1", Description: "Skill 1", License: "MIT", Body: "Skill body 1"}}
	dist := filepath.Join(t.TempDir(), "dist")
	for _, provider := range orchestrationProviders {
		transformProvider(t, provider, skills, dist)
		config, _ := impeccableProviderByName(provider)
		wantContains(t, readTestFile(t, filepath.Join(dist, provider, config.ConfigDir, "skills", "skill1", "SKILL.md")), "name: skill1", "Skill body 1")
	}
}

func TestBuildOrchestrationShouldHandleEmptySourceFiles(t *testing.T) {
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	skills := readSourceSkills(t, root)
	if len(skills) != 0 {
		t.Fatalf("%d skills read from an empty root", len(skills))
	}
	for _, provider := range orchestrationProviders {
		transformProvider(t, provider, skills, dist)
		config, _ := impeccableProviderByName(provider)
		if names := dirNames(t, filepath.Join(dist, provider, config.ConfigDir, "skills")); len(names) != 0 {
			t.Fatalf("%s wrote %v for no skills", provider, names)
		}
	}
}

func TestBuildOrchestrationShouldCallTransformersInCorrectOrder(t *testing.T) {
	dist := filepath.Join(t.TempDir(), "dist")
	var log strings.Builder
	var position []int
	for _, provider := range orchestrationProviders {
		config, _ := impeccableProviderByName(provider)
		if err := config.transform(nil, dist, "", &log); err != nil {
			t.Fatal(err)
		}
		position = append(position, slices.IndexFunc(impeccableProviders, func(c impeccableProvider) bool { return c.Provider == provider }))
	}
	want := "✓ Cursor: 0 skills (hooks.json)\n✓ Claude Code: 0 skills (settings.json)\n✓ Gemini: 0 skills (settings.json)\n✓ Codex: 0 skills (hooks.json)\n"
	if log.String() != want {
		t.Fatalf("call order log\n%s", log.String())
	}
	if !slices.IsSorted(position) {
		t.Fatalf("PROVIDERS lists %v at %v", orchestrationProviders, position)
	}
}
