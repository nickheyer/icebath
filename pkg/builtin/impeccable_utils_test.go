package builtin

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func wantFm(t *testing.T, fm []fmPair, key string, want any) {
	t.Helper()
	if got := fmLookup(fm, key); got != want {
		t.Fatalf("%s is %v, want %v", key, got, want)
	}
}

func TestParseFrontmatterShouldParseBasicFrontmatterWithSimpleKeyValuePairs(t *testing.T) {
	fm, body := parseImpeccableFrontmatter("---\nname: test-skill\ndescription: A test skill\n---\n\nThis is the body content.")
	wantFm(t, fm, "name", "test-skill")
	wantFm(t, fm, "description", "A test skill")
	if body != "This is the body content." {
		t.Fatal(body)
	}
}

func TestParseFrontmatterShouldParseFrontmatterWithArgumentHint(t *testing.T) {
	fm, _ := parseImpeccableFrontmatter("---\nname: test-skill\ndescription: A test skill\nargument-hint: <output> [TARGET=<value>]\n---\n\nBody here.")
	wantFm(t, fm, "name", "test-skill")
	wantFm(t, fm, "argument-hint", "<output> [TARGET=<value>]")
}

func TestParseFrontmatterShouldReturnEmptyFrontmatterWhenNoFrontmatterPresent(t *testing.T) {
	content := "Just some content without frontmatter."
	fm, body := parseImpeccableFrontmatter(content)
	if len(fm) != 0 || body != content {
		t.Fatalf("%+v %q", fm, body)
	}
}

func TestParseFrontmatterShouldHandleEmptyBody(t *testing.T) {
	fm, body := parseImpeccableFrontmatter("---\nname: test\n---\n")
	wantFm(t, fm, "name", "test")
	if body != "" {
		t.Fatalf("body %q", body)
	}
}

func TestParseFrontmatterShouldHandleFrontmatterWithLicenseField(t *testing.T) {
	fm, _ := parseImpeccableFrontmatter("---\nname: skill-name\ndescription: A skill\nlicense: MIT\n---\n\nSkill body.")
	wantFm(t, fm, "license", "MIT")
}

func TestParseFrontmatterShouldParseUserInvocableBoolean(t *testing.T) {
	fm, _ := parseImpeccableFrontmatter("---\nname: test-skill\nuser-invocable: true\n---\n\nBody.")
	wantFm(t, fm, "user-invocable", true)
}

func TestParseFrontmatterShouldParseQuotedUserInvocableBooleanAsTrue(t *testing.T) {
	fm, _ := parseImpeccableFrontmatter("---\nname: test-skill\nuser-invocable: 'true'\n---\n\nBody.")
	wantFm(t, fm, "user-invocable", true)
}

func TestParseFrontmatterShouldKeepQuotedNonUserInvocableBooleansAsPlainStrings(t *testing.T) {
	fm, _ := parseImpeccableFrontmatter("---\nname: test-skill\ndescription: 'true'\n---\n\nBody.")
	wantFm(t, fm, "description", "true")
}

func TestParseFrontmatterShouldParseAllowedToolsField(t *testing.T) {
	fm, _ := parseImpeccableFrontmatter("---\nname: test-skill\nallowed-tools: Bash\n---\n\nBody.")
	wantFm(t, fm, "allowed-tools", "Bash")
}

func TestGenerateYamlFrontmatterShouldGenerateBasicFrontmatter(t *testing.T) {
	wantContains(t, generateYamlFrontmatter([]fmPair{{"name", "test-skill"}, {"description", "A test"}}), "---", "name: test-skill", "description: A test")
}

func TestGenerateYamlFrontmatterShouldGenerateFrontmatterWithArgumentHint(t *testing.T) {
	result := generateYamlFrontmatter([]fmPair{{"name", "test"}, {"description", "Test skill"}, {"argument-hint", "<output> [TARGET=<value>]"}})
	wantContains(t, result, "argument-hint: <output> [TARGET=<value>]")
}

func TestGenerateYamlFrontmatterShouldGenerateFrontmatterWithBoolean(t *testing.T) {
	wantContains(t, generateYamlFrontmatter([]fmPair{{"name", "test"}, {"description", "Test"}, {"user-invocable", true}}), "user-invocable: true")
}

func TestGenerateYamlFrontmatterShouldGenerateNestedMetadata(t *testing.T) {
	wantContains(t, generateYamlFrontmatter([]fmPair{{"name", "test"}, {"metadata", []fmPair{{"version", "1.2.3"}}}}), "metadata:\n  version: 1.2.3")
}

func TestGenerateYamlFrontmatterShouldRoundtripGenerateAndParseBack(t *testing.T) {
	yaml := generateYamlFrontmatter([]fmPair{{"name", "roundtrip-test"}, {"description", "Testing roundtrip"}, {"argument-hint", "<arg1>"}})
	fm, _ := parseImpeccableFrontmatter(yaml + "\n\nBody content")
	wantFm(t, fm, "name", "roundtrip-test")
	wantFm(t, fm, "description", "Testing roundtrip")
	wantFm(t, fm, "argument-hint", "<arg1>")
}

func TestGenerateYamlFrontmatterShouldQuoteStringsContainingColonSpace(t *testing.T) {
	description := "Design fluency. Also handles: critique, audit. Commands: craft, polish."
	result := generateYamlFrontmatter([]fmPair{{"name", "impeccable"}, {"description", description}})
	wantContains(t, result, `description: "`+description+`"`)
	fm, _ := parseImpeccableFrontmatter(result + "\n\nbody")
	wantFm(t, fm, "description", description)
}

func TestGenerateYamlFrontmatterShouldQuoteStringsStartingWithYAMLFlowIndicators(t *testing.T) {
	wantContains(t, generateYamlFrontmatter([]fmPair{{"name", "test"}, {"argument-hint", "[command] [target]"}}), `argument-hint: "[command] [target]"`)
}

func TestGenerateYamlFrontmatterShouldNotQuotePlainStringsWithoutSpecialChars(t *testing.T) {
	result := generateYamlFrontmatter([]fmPair{{"name", "simple"}, {"description", "A plain description with no colons or hashes"}})
	wantContains(t, result, "description: A plain description with no colons or hashes")
	wantLacks(t, result, `"A plain`)
}

func TestWriteFileShouldWriteFileWithContent(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "test-tmp", "test.txt")
	if err := writeBuildFile(filePath, "Hello, world!"); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filePath); got != "Hello, world!" {
		t.Fatal(got)
	}
}

func TestWriteFileShouldCreateParentDirectoriesAutomatically(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "test-tmp", "nested", "deep", "file.txt")
	if err := writeBuildFile(filePath, "content"); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filePath); got != "content" {
		t.Fatal(got)
	}
}

func TestWriteFileShouldOverwriteExistingFile(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "test-tmp", "file.txt")
	for _, content := range []string{"first", "second"} {
		if err := writeBuildFile(filePath, content); err != nil {
			t.Fatal(err)
		}
	}
	if got := readTestFile(t, filePath); got != "second" {
		t.Fatal(got)
	}
}

// The one skill read from a root whose skill/SKILL.src.md holds the content
func sourceSkillOf(t *testing.T, root, skillContent string) impeccableSkill {
	t.Helper()
	writeAt(t, root, "skill/SKILL.src.md", skillContent)
	skills := readSourceSkills(t, root)
	if len(skills) != 1 {
		t.Fatalf("%d skills read", len(skills))
	}
	return skills[0]
}

func TestReadSourceFilesShouldReadAndParseSkillMdFromSkill(t *testing.T) {
	skill := sourceSkillOf(t, t.TempDir(), "---\nname: test-skill\ndescription: A test skill\nlicense: MIT\n---\n\nSkill instructions here.")
	if skill.Name != "test-skill" || skill.Description != "A test skill" || skill.License != "MIT" || skill.Body != "Skill instructions here." {
		t.Fatalf("%+v", skill)
	}
}

func TestReadSourceFilesShouldReadSkillWithUserInvocableFlag(t *testing.T) {
	skill := sourceSkillOf(t, t.TempDir(), "---\nname: audit\ndescription: Run technical quality checks\nuser-invocable: true\n---\n\nAudit the code.")
	if !skill.UserInvocable {
		t.Fatal("user-invocable flag lost")
	}
}

func TestReadSourceFilesShouldReadSkillWithQuotedUserInvocableFlag(t *testing.T) {
	skill := sourceSkillOf(t, t.TempDir(), "---\nname: audit\ndescription: Run technical quality checks\nuser-invocable: 'true'\n---\n\nAudit the code.")
	if !skill.UserInvocable {
		t.Fatal("quoted user-invocable flag lost")
	}
}

func TestReadSourceFilesShouldReadSkillWithReferenceFiles(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "skill/reference/typography.md", "Typography reference content.")
	writeAt(t, root, "skill/reference/color.md", "Color reference content.")
	skill := sourceSkillOf(t, root, "---\nname: impeccable\ndescription: Impeccable design skill\n---\n\nImpeccable design instructions.")
	var refNames []string
	for _, ref := range skill.References {
		refNames = append(refNames, ref.Name)
	}
	sort.Strings(refNames)
	if !slices.Equal(refNames, []string{"color", "typography"}) {
		t.Fatalf("references %v", refNames)
	}
}

func TestReadSourceFilesShouldFallBackToImpeccableWhenFrontmatterHasNoName(t *testing.T) {
	if skill := sourceSkillOf(t, t.TempDir(), "Just body, no frontmatter."); skill.Name != "impeccable" {
		t.Fatalf("name %q", skill.Name)
	}
}

func TestReadSourceFilesShouldIgnoreNonMdFilesInSkillReference(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "skill/reference/readme.txt", "Not a markdown file")
	writeAt(t, root, "skill/reference/typography.md", "Valid reference")
	skill := sourceSkillOf(t, root, "---\nname: test-skill\n---\nBody")
	if len(skill.References) != 1 || skill.References[0].Name != "typography" {
		t.Fatalf("references %+v", skill.References)
	}
}

func TestReadSourceFilesShouldReadNestedSkillScriptFilesWithPortableRelativeNames(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "skill/scripts/context.mjs", "export const context = true;\n")
	writeAt(t, root, "skill/scripts/live/session-store.mjs", "export const nested = true;\n")
	writeAt(t, root, "skill/scripts/config.json", "{\"local\":true}\n")
	skill := sourceSkillOf(t, root, "---\nname: test-skill\n---\nBody")
	var names []string
	nested := ""
	for _, script := range skill.Scripts {
		names = append(names, script.Name)
		if script.Name == "live/session-store.mjs" {
			nested = script.Content
		}
	}
	sort.Strings(names)
	if !slices.Equal(names, []string{"context.mjs", "live/session-store.mjs"}) || !strings.Contains(nested, "nested = true") {
		t.Fatalf("scripts %v, nested %q", names, nested)
	}
}

func TestReadSourceFilesShouldHandleMissingSkillDirectory(t *testing.T) {
	if skills := readSourceSkills(t, t.TempDir()); len(skills) != 0 {
		t.Fatalf("%d skills read from an empty root", len(skills))
	}
}

func TestReadSourceFilesShouldParseAllFrontmatterFieldsCorrectly(t *testing.T) {
	skill := sourceSkillOf(t, t.TempDir(), "---\nname: test-skill\ndescription: A comprehensive test skill\nlicense: Apache-2.0\n"+
		"compatibility: claude-code\nuser-invocable: true\nallowed-tools: Bash,Edit\n---\n\nBody content.")
	if skill.Name != "test-skill" || skill.Description != "A comprehensive test skill" || skill.License != "Apache-2.0" ||
		skill.Compatibility != "claude-code" || !skill.UserInvocable || skill.AllowedTools != "Bash,Edit" {
		t.Fatalf("%+v", skill)
	}
}

func wantTokens(t *testing.T, content, provider string, commandNames, allSkillNames []string, want string) {
	t.Helper()
	if got := replaceProviderTokens(content, provider, commandNames, allSkillNames); got != want {
		t.Fatalf("%s: %q, want %q", provider, got, want)
	}
}

func TestReplacePlaceholdersShouldReplaceModelWithProviderSpecificValue(t *testing.T) {
	for provider, model := range map[string]string{"claude-code": "Claude", "gemini": "Gemini", "codex": "GPT", "cursor": "the model", "agents": "the model", "kiro": "Claude"} {
		wantTokens(t, "Ask {{model}} for help.", provider, nil, nil, "Ask "+model+" for help.")
	}
}

func TestReplacePlaceholdersShouldReplaceConfigFileWithProviderSpecificValue(t *testing.T) {
	for provider, file := range map[string]string{"claude-code": "CLAUDE.md", "cursor": ".cursorrules", "gemini": "GEMINI.md", "codex": "AGENTS.md",
		"agents": ".github/copilot-instructions.md", "kiro": ".kiro/settings.json"} {
		wantTokens(t, "See {{config_file}}.", provider, nil, nil, "See "+file+".")
	}
}

func TestReplacePlaceholdersShouldReplaceAskInstructionWithProviderSpecificValue(t *testing.T) {
	wantTokens(t, "{{ask_instruction}}", "claude-code", nil, nil, "Ask with the AskUserQuestion tool and wait for the answer before continuing.")
	wantTokens(t, "{{ask_instruction}}", "cursor", nil, nil, "Ask the user directly to clarify what you cannot infer.")
}

func TestReplacePlaceholdersShouldReplaceAvailableCommandsWithCommandList(t *testing.T) {
	wantTokens(t, "Commands: {{available_commands}}", "claude-code", []string{"audit", "polish", "optimize"}, nil, "Commands: /audit, /polish, /optimize")
}

func TestReplacePlaceholdersShouldExcludeImpeccableFromAvailableCommands(t *testing.T) {
	wantTokens(t, "Commands: {{available_commands}}", "claude-code", []string{"audit", "impeccable", "polish"}, nil, "Commands: /audit, /polish")
}

func TestReplacePlaceholdersShouldExcludeLegacyTeachImpeccableFromAvailableCommands(t *testing.T) {
	wantTokens(t, "Commands: {{available_commands}}", "claude-code", []string{"audit", "teach-impeccable", "polish"}, nil, "Commands: /audit, /polish")
}

func TestReplacePlaceholdersListsImpeccableSubCommandsForAvailableCommandsWhenNoCommandNamesArePassed(t *testing.T) {
	result := replaceProviderTokens("Commands: {{available_commands}}", "claude-code", []string{}, nil)
	if !strings.HasPrefix(result, "Commands: /impeccable ") {
		t.Fatal(result)
	}
	wantContains(t, result, "/impeccable audit", "/impeccable polish")
}

func TestReplacePlaceholdersShouldReplaceMultiplePlaceholdersInTheSameString(t *testing.T) {
	wantTokens(t, "{{model}} uses {{config_file}} and {{ask_instruction}}", "claude-code", nil, nil,
		"Claude uses CLAUDE.md and Ask with the AskUserQuestion tool and wait for the answer before continuing.")
}

func TestReplacePlaceholdersShouldReplaceMultipleOccurrencesOfTheSamePlaceholder(t *testing.T) {
	wantTokens(t, "{{model}} and {{model}} again.", "gemini", nil, nil, "Gemini and Gemini again.")
}

func TestReplacePlaceholdersShouldFallBackToCursorPlaceholdersForUnknownProvider(t *testing.T) {
	wantTokens(t, "{{model}} {{config_file}}", "unknown-provider", nil, nil, "the model .cursorrules")
}

func TestReplacePlaceholdersShouldReplaceCodexCommandInvocationsWithoutRewritingPaths(t *testing.T) {
	source := strings.Join([]string{
		"Run /impeccable audit.",
		"Use `/impeccable polish` next.",
		".github/hooks/impeccable.json",
		".codex/skills/impeccable/scripts/context.mjs",
		"https://example.com/impeccable",
	}, "\n")
	result := replaceProviderTokens(source, "codex", []string{}, []string{"impeccable"})
	wantContains(t, result, "Run $impeccable audit.", "Use `$impeccable polish` next.", ".github/hooks/impeccable.json",
		".codex/skills/impeccable/scripts/context.mjs", "https://example.com/impeccable")
}

func TestReplaceScriptProviderMarkerRendersOnlyTheExplicitProviderDeclarations(t *testing.T) {
	source := strings.Join([]string{
		"export const IMPECCABLE_COMMAND_PREFIX = '/'; // @impeccable-provider-command-prefix",
		"export const IMPECCABLE_PROVIDER_ID = 'source'; // @impeccable-provider-id",
		`const regex = /impeccable\b/gi;`,
		"const runtime = '/src/lib/impeccable/__runtime.js';",
		"const text = 'Run /impeccable audit';",
	}, "\n")
	result := replaceProviderScriptMarker(source, "codex", "agents")
	wantContains(t, result, `export const IMPECCABLE_COMMAND_PREFIX = "$";`, `export const IMPECCABLE_PROVIDER_ID = "agents";`,
		`const regex = /impeccable\b/gi;`, "const runtime = '/src/lib/impeccable/__runtime.js';", "const text = 'Run /impeccable audit';")
}

func TestEnsureDirShouldCreateDirectoryIfItDoesNotExist(t *testing.T) {
	testPath := filepath.Join(t.TempDir(), "new-dir")
	if err := ensureDir(testPath); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(testPath); err != nil || !info.IsDir() {
		t.Fatalf("%v %v", info, err)
	}
}

func TestEnsureDirShouldCreateNestedDirectories(t *testing.T) {
	testPath := filepath.Join(t.TempDir(), "level1", "level2", "level3")
	if err := ensureDir(testPath); err != nil {
		t.Fatal(err)
	}
	wantExists(t, testPath)
}

func TestEnsureDirShouldNotThrowIfDirectoryAlreadyExists(t *testing.T) {
	testPath := filepath.Join(t.TempDir(), "existing")
	if err := os.MkdirAll(testPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureDir(testPath); err != nil {
		t.Fatal(err)
	}
}

func TestCleanDirShouldRemoveDirectoryAndAllContents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "test-tmp")
	writeAt(t, dir, "test.txt", "content")
	if err := cleanDir(dir); err != nil {
		t.Fatal(err)
	}
	if pathExists(dir) {
		t.Fatal("directory survived")
	}
}

func TestCleanDirShouldNotThrowIfDirectoryDoesNotExist(t *testing.T) {
	if err := cleanDir(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatal(err)
	}
}

func TestCleanDirShouldRemoveNestedDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "test-tmp")
	nested := filepath.Join(dir, "level1", "level2")
	if err := ensureDir(nested); err != nil {
		t.Fatal(err)
	}
	writeAt(t, nested, "file.txt", "content")
	if err := cleanDir(dir); err != nil {
		t.Fatal(err)
	}
	if pathExists(dir) {
		t.Fatal("directory survived")
	}
}

func markdownUnder(t *testing.T, dir string) []string {
	t.Helper()
	files, err := readFilesRecursive(dir)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func wantSuffixes(t *testing.T, files []string, suffixes ...string) {
	t.Helper()
	if len(files) != len(suffixes) {
		t.Fatalf("files %v, want %d", files, len(suffixes))
	}
	for _, suffix := range suffixes {
		if !slices.ContainsFunc(files, func(f string) bool { return strings.HasSuffix(f, suffix) }) {
			t.Errorf("no file ends with %s in %v", suffix, files)
		}
	}
}

func TestReadFilesRecursiveShouldFindAllMarkdownFilesInDirectory(t *testing.T) {
	dir := t.TempDir()
	writeAt(t, dir, "file1.md", "content1")
	writeAt(t, dir, "file2.md", "content2")
	writeAt(t, dir, "file3.txt", "not markdown")
	wantSuffixes(t, markdownUnder(t, dir), "file1.md", "file2.md")
}

func TestReadFilesRecursiveShouldFindMarkdownFilesInNestedDirectories(t *testing.T) {
	dir := t.TempDir()
	writeAt(t, dir, "root.md", "root")
	writeAt(t, dir, "sub/nested.md", "nested")
	writeAt(t, dir, "sub/deep/deeper.md", "deeper")
	wantSuffixes(t, markdownUnder(t, dir), "root.md", "nested.md", "deeper.md")
}

func TestReadFilesRecursiveShouldReturnEmptyArrayForNonExistentDirectory(t *testing.T) {
	wantSuffixes(t, markdownUnder(t, filepath.Join(t.TempDir(), "does-not-exist")))
}

func TestReadFilesRecursiveShouldReturnEmptyArrayForDirectoryWithNoMarkdownFiles(t *testing.T) {
	dir := t.TempDir()
	writeAt(t, dir, "file.txt", "text")
	writeAt(t, dir, "file.js", "code")
	wantSuffixes(t, markdownUnder(t, dir))
}

func patternNames(categories []patternCategory) []string {
	var names []string
	for _, c := range categories {
		names = append(names, c.Name)
	}
	return names
}

func TestReadPatternsReturnsTheCuratedDoDontPatternCategories(t *testing.T) {
	got := readPatterns(t.TempDir())
	if len(got.Patterns) != 6 || len(got.Antipatterns) != 6 {
		t.Fatalf("%d patterns, %d antipatterns", len(got.Patterns), len(got.Antipatterns))
	}
	if got.Patterns[0].Name != "Typography" || len(got.Patterns[0].Items) == 0 || got.Antipatterns[0].Name != "Typography" || len(got.Antipatterns[0].Items) == 0 {
		t.Fatalf("%+v %+v", got.Patterns[0], got.Antipatterns[0])
	}
}

func TestReadPatternsReturnsCategoriesInTheCuratedOrder(t *testing.T) {
	want := []string{"Typography", "Color & Contrast", "Layout & Space", "Visual Details", "Motion", "Interaction"}
	if names := patternNames(readPatterns(t.TempDir()).Patterns); !slices.Equal(names, want) {
		t.Fatalf("order %v", names)
	}
}

func TestReadPatternsIgnoresItsArguments(t *testing.T) {
	fromBogusRoot := readPatterns("/nonexistent/path/xyz")
	fromRealRoot := readPatterns(t.TempDir())
	if !slices.Equal(patternNames(fromBogusRoot.Patterns), patternNames(fromRealRoot.Patterns)) || len(fromBogusRoot.Patterns) != 6 {
		t.Fatalf("%v", patternNames(fromBogusRoot.Patterns))
	}
}
