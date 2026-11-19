package builtin

import (
	"slices"
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

// The icebath render of a root whose skill/SKILL.src.md holds the content, keyed by path below the skill directory
func sourceSkillOf(t *testing.T, root, skillContent string) map[string]string {
	t.Helper()
	writeAt(t, root, "skill/SKILL.src.md", skillContent)
	return renderSourceSkill(t, root)
}

func TestReadSourceFilesShouldReadSkillWithQuotedUserInvocableFlag(t *testing.T) {
	skill := sourceSkillOf(t, t.TempDir(), "---\nname: audit\ndescription: Run technical quality checks\nuser-invocable: 'true'\n---\n\nAudit the code. Then offer {{available_commands}}.")
	if !strings.HasSuffix(skill["SKILL.md"], "\n\nAudit the code. Then offer /audit.") {
		t.Fatalf("quoted user-invocable flag lost\n%s", skill["SKILL.md"])
	}
}

func TestReadSourceFilesShouldFallBackToImpeccableWhenFrontmatterHasNoName(t *testing.T) {
	if skill := sourceSkillOf(t, t.TempDir(), "Just body, no frontmatter."); !strings.HasPrefix(skill["SKILL.md"], "---\nname: impeccable\n") {
		t.Fatalf("rendered skill\n%s", skill["SKILL.md"])
	}
}

func TestReadSourceFilesShouldIgnoreNonMdFilesInSkillReference(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "skill/reference/readme.txt", "Not a markdown file")
	writeAt(t, root, "skill/reference/typography.md", "Valid reference")
	skill := sourceSkillOf(t, root, "---\nname: test-skill\n---\nBody")
	if references := renderedUnder(skill, "reference/"); !slices.Equal(references, []string{"typography.md"}) || skill["reference/typography.md"] != "Valid reference" {
		t.Fatalf("references %v, typography %q", references, skill["reference/typography.md"])
	}
}

func TestReadSourceFilesShouldReadNestedSkillScriptFilesWithPortableRelativeNames(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "skill/scripts/context.mjs", "export const context = true;\n")
	writeAt(t, root, "skill/scripts/live/session-store.mjs", "export const nested = true;\n")
	writeAt(t, root, "skill/scripts/config.json", "{\"local\":true}\n")
	skill := sourceSkillOf(t, root, "---\nname: test-skill\n---\nBody")
	names, nested := renderedUnder(skill, "scripts/"), skill["scripts/live/session-store.mjs"]
	if !slices.Equal(names, []string{"context.mjs", "live/session-store.mjs"}) || !strings.Contains(nested, "nested = true") {
		t.Fatalf("scripts %v, nested %q", names, nested)
	}
}

func wantTokens(t *testing.T, content string, commandNames []string, want string) {
	t.Helper()
	if got := replacePlaceholders(content, commandNames); got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}

func TestReplacePlaceholdersShouldReplaceAvailableCommandsWithCommandList(t *testing.T) {
	wantTokens(t, "Commands: {{available_commands}}", []string{"audit", "polish", "optimize"}, "Commands: /audit, /polish, /optimize")
}

func TestReplacePlaceholdersShouldExcludeImpeccableFromAvailableCommands(t *testing.T) {
	wantTokens(t, "Commands: {{available_commands}}", []string{"audit", "impeccable", "polish"}, "Commands: /audit, /polish")
}

func TestReplacePlaceholdersShouldExcludeLegacyTeachImpeccableFromAvailableCommands(t *testing.T) {
	wantTokens(t, "Commands: {{available_commands}}", []string{"audit", "teach-impeccable", "polish"}, "Commands: /audit, /polish")
}

func TestReplacePlaceholdersListsImpeccableSubCommandsForAvailableCommandsWhenNoCommandNamesArePassed(t *testing.T) {
	result := replacePlaceholders("Commands: {{available_commands}}", []string{})
	if !strings.HasPrefix(result, "Commands: /impeccable ") {
		t.Fatal(result)
	}
	wantContains(t, result, "/impeccable audit", "/impeccable polish")
}

func TestReplacePlaceholdersShouldReplaceMultiplePlaceholdersInTheSameString(t *testing.T) {
	wantTokens(t, "{{model}} uses {{config_file}} and {{ask_instruction}}", nil,
		"the model uses AGENTS.md and Ask the user directly to clarify what you cannot infer.")
}

func TestReplacePlaceholdersShouldReplaceMultipleOccurrencesOfTheSamePlaceholder(t *testing.T) {
	wantTokens(t, "{{model}} and {{model}} again.", nil, "the model and the model again.")
}

func TestReplaceScriptProviderMarkerRendersOnlyTheExplicitProviderDeclarations(t *testing.T) {
	source := strings.Join([]string{
		"export const IMPECCABLE_COMMAND_PREFIX = '/'; // @impeccable-provider-command-prefix",
		"export const IMPECCABLE_PROVIDER_ID = 'source'; // @impeccable-provider-id",
		`const regex = /impeccable\b/gi;`,
		"const runtime = '/src/lib/impeccable/__runtime.js';",
		"const text = 'Run /impeccable audit';",
	}, "\n")
	want := strings.Join([]string{
		`export const IMPECCABLE_COMMAND_PREFIX = "/";`,
		`export const IMPECCABLE_PROVIDER_ID = "icebath";`,
		`const regex = /impeccable\b/gi;`,
		"const runtime = '/src/lib/impeccable/__runtime.js';",
		"const text = 'Run /impeccable audit';",
	}, "\n")
	if result := replaceScriptProviderMarker(source); result != want {
		t.Fatalf("rendered script\n%s", result)
	}
}
