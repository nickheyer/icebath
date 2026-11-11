package builtin

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func factoryBaseConfig() impeccableProvider {
	return impeccableProvider{Provider: "cursor", ConfigDir: ".test", DisplayName: "Test Provider", FrontmatterFields: []string{}}
}

// Dist directory after one transform of the skills with the config
func factoryTransform(t *testing.T, config impeccableProvider, skills []impeccableSkill) string {
	t.Helper()
	dist := filepath.Join(t.TempDir(), "test-tmp-factory")
	if err := config.transform(skills, dist, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	return dist
}

func factorySkillMd(t *testing.T, dist string) (string, []fmPair, string) {
	t.Helper()
	content := readTestFile(t, filepath.Join(dist, "cursor/.test/skills/test/SKILL.md"))
	fm, body := parseImpeccableFrontmatter(content)
	return content, fm, body
}

func TestCreateTransformerFactoryShouldCreateCorrectDirectoryStructure(t *testing.T) {
	dist := factoryTransform(t, factoryBaseConfig(), nil)
	wantExists(t, filepath.Join(dist, "cursor/.test/skills"))
}

func TestCreateTransformerFactoryShouldAlwaysEmitNameAndDescription(t *testing.T) {
	dist := factoryTransform(t, factoryBaseConfig(), []impeccableSkill{{Name: "test", Description: "A test skill", Body: "Body."}})
	_, fm, body := factorySkillMd(t, dist)
	if fmLookup(fm, "name") != "test" || fmLookup(fm, "description") != "A test skill" || body != "Body." {
		t.Fatalf("%+v %q", fm, body)
	}
}

func TestCreateTransformerFactoryShouldOnlyEmitAllowlistedFields(t *testing.T) {
	config := factoryBaseConfig()
	config.FrontmatterFields = []string{"license"}
	dist := factoryTransform(t, config, []impeccableSkill{{Name: "test", Description: "Test", License: "MIT", Compatibility: "all", Metadata: "meta", Body: "Body"}})
	_, fm, _ := factorySkillMd(t, dist)
	if fmLookup(fm, "license") != "MIT" || fmHas(fm, "compatibility") || fmHas(fm, "metadata") {
		t.Fatalf("%+v", fm)
	}
}

func TestCreateTransformerFactoryShouldSkipEmptyOptionalFields(t *testing.T) {
	config := factoryBaseConfig()
	config.FrontmatterFields = []string{"license"}
	dist := factoryTransform(t, config, []impeccableSkill{{Name: "test", Description: "Test", License: "", Body: "Body"}})
	if _, fm, _ := factorySkillMd(t, dist); fmHas(fm, "license") {
		t.Fatalf("%+v", fm)
	}
}

func TestCreateTransformerFactoryShouldEmitUserInvocableAsTrueWhenSkillIsUserInvocable(t *testing.T) {
	config := factoryBaseConfig()
	config.FrontmatterFields = []string{"user-invocable"}
	dist := factoryTransform(t, config, []impeccableSkill{{Name: "test", Description: "Test", UserInvocable: true, Body: "Body"}})
	if _, fm, _ := factorySkillMd(t, dist); fmLookup(fm, "user-invocable") != true {
		t.Fatalf("%+v", fm)
	}
}

func TestCreateTransformerFactoryShouldNotEmitUserInvocableWhenSkillIsNotUserInvocable(t *testing.T) {
	config := factoryBaseConfig()
	config.FrontmatterFields = []string{"user-invocable"}
	dist := factoryTransform(t, config, []impeccableSkill{{Name: "test", Description: "Test", UserInvocable: false, Body: "Body"}})
	if _, fm, _ := factorySkillMd(t, dist); fmHas(fm, "user-invocable") {
		t.Fatalf("%+v", fm)
	}
}

func TestCreateTransformerFactoryShouldEmitArgumentHintOnlyWhenUserInvocable(t *testing.T) {
	config := factoryBaseConfig()
	config.FrontmatterFields = []string{"argument-hint"}
	dist := factoryTransform(t, config, []impeccableSkill{{Name: "test", Description: "Test", UserInvocable: true, ArgumentHint: "[target]", Body: "Body"}})
	if _, fm, _ := factorySkillMd(t, dist); fmLookup(fm, "argument-hint") != "[target]" {
		t.Fatalf("%+v", fm)
	}
	dist = factoryTransform(t, config, []impeccableSkill{{Name: "test", Description: "Test", UserInvocable: false, ArgumentHint: "[target]", Body: "Body"}})
	if _, fm, _ := factorySkillMd(t, dist); fmHas(fm, "argument-hint") {
		t.Fatalf("%+v", fm)
	}
}

func TestCreateTransformerFactoryShouldApplyBodyTransformAfterPlaceholderReplacement(t *testing.T) {
	config := factoryBaseConfig()
	config.BodyTransform = func(body string, _ impeccableSkill) string {
		return strings.Replace(body, "PLACEHOLDER", "TRANSFORMED", 1)
	}
	dist := factoryTransform(t, config, []impeccableSkill{{Name: "test", Description: "Test", Body: "PLACEHOLDER content"}})
	content, _, _ := factorySkillMd(t, dist)
	wantContains(t, content, "TRANSFORMED content")
}

func TestCreateTransformerFactoryShouldCopyReferenceFiles(t *testing.T) {
	dist := factoryTransform(t, factoryBaseConfig(), []impeccableSkill{{Name: "test", Description: "Test", Body: "Body",
		References: []impeccableFile{{Name: "ref1", Content: "Reference 1 content"}, {Name: "ref2", Content: "Reference 2 content"}}}})
	ref1 := filepath.Join(dist, "cursor/.test/skills/test/reference/ref1.md")
	wantExists(t, ref1, filepath.Join(dist, "cursor/.test/skills/test/reference/ref2.md"))
	if got := readTestFile(t, ref1); got != "Reference 1 content" {
		t.Fatal(got)
	}
}

func TestCreateTransformerFactoryShouldRenderTheExplicitScriptProviderMarkerWithoutRewritingExecutableCode(t *testing.T) {
	config := factoryBaseConfig()
	config.Provider, config.PlaceholderProvider = "codex", "codex"
	dist := factoryTransform(t, config, []impeccableSkill{{Name: "impeccable", Description: "Test", Body: "Body", Scripts: []impeccableScript{{
		Name: "example.mjs",
		Content: strings.Join([]string{
			"export const IMPECCABLE_COMMAND_PREFIX = '/'; // @impeccable-provider-command-prefix",
			"export const IMPECCABLE_PROVIDER_ID = 'source'; // @impeccable-provider-id",
			"const command = `${IMPECCABLE_COMMAND_PREFIX}impeccable polish`;",
			`const hint = "Run /impeccable audit";`,
			`const hook = ".github/hooks/impeccable.json";`,
			`const runtime = "/src/lib/impeccable/__runtime.js";`,
			`const regex = /impeccable\b/gi;`,
		}, "\n"),
	}}}})
	script := readTestFile(t, filepath.Join(dist, "codex/.test/skills/impeccable/scripts/example.mjs"))
	wantContains(t, script, `IMPECCABLE_COMMAND_PREFIX = "$"`, `IMPECCABLE_PROVIDER_ID = "codex"`, "`${IMPECCABLE_COMMAND_PREFIX}impeccable polish`",
		`"Run /impeccable audit"`, `".github/hooks/impeccable.json"`, `"/src/lib/impeccable/__runtime.js"`, `/impeccable\b/gi`)
}

func TestCreateTransformerFactoryShouldCleanExistingDirectoryBeforeWriting(t *testing.T) {
	dist := filepath.Join(t.TempDir(), "test-tmp-factory")
	old := filepath.Join(dist, "cursor/.test/skills/old/SKILL.md")
	writeTestFile(t, old, "old")
	if err := factoryBaseConfig().transform([]impeccableSkill{{Name: "new", Description: "New", Body: "New"}}, dist, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if pathExists(old) {
		t.Fatal("old skill survived the transform")
	}
	wantExists(t, filepath.Join(dist, "cursor/.test/skills/new/SKILL.md"))
}

func TestCreateTransformerFactoryShouldLogCorrectSummary(t *testing.T) {
	var log bytes.Buffer
	skills := []impeccableSkill{{Name: "s1", Description: "Test", UserInvocable: true, Body: "body"}, {Name: "s2", Description: "Test", Body: "body"}}
	if err := factoryBaseConfig().transform(skills, filepath.Join(t.TempDir(), "dist"), "", &log); err != nil {
		t.Fatal(err)
	}
	wantContains(t, log.String(), "✓ Test Provider:", "2 skills")
}

func TestCreateTransformerFactoryShouldHandleEmptySkillsArray(t *testing.T) {
	dist := factoryTransform(t, factoryBaseConfig(), []impeccableSkill{})
	if names := dirNames(t, filepath.Join(dist, "cursor/.test/skills")); len(names) != 0 {
		t.Fatalf("skill dirs %v", names)
	}
}

func TestCreateTransformerFactoryShouldReplaceModelPlaceholder(t *testing.T) {
	dist := factoryTransform(t, factoryBaseConfig(), []impeccableSkill{{Name: "test", Description: "Test", Body: "Ask {{model}} for help."}})
	content, _, _ := factorySkillMd(t, dist)
	wantContains(t, content, "Ask the model for help.")
}

func TestCreateTransformerFactoryShouldReplaceConfigFilePlaceholder(t *testing.T) {
	dist := factoryTransform(t, factoryBaseConfig(), []impeccableSkill{{Name: "test", Description: "Test", Body: "See {{config_file}}."}})
	content, _, _ := factorySkillMd(t, dist)
	wantContains(t, content, "See .cursorrules.")
}

func TestCreateTransformerFactoryShouldHandleMultipleSkills(t *testing.T) {
	dist := factoryTransform(t, factoryBaseConfig(), []impeccableSkill{{Name: "skill1", Description: "Skill 1", Body: "Body 1"}, {Name: "skill2", Description: "Skill 2", Body: "Body 2"}})
	wantExists(t, filepath.Join(dist, "cursor/.test/skills/skill1/SKILL.md"), filepath.Join(dist, "cursor/.test/skills/skill2/SKILL.md"))
}

func TestCreateTransformerFactoryShouldPreserveMultilineBodyContent(t *testing.T) {
	dist := factoryTransform(t, factoryBaseConfig(), []impeccableSkill{{Name: "test", Description: "Test",
		Body: "First paragraph.\n\nSecond paragraph.\n\n- List item 1\n- List item 2"}})
	_, _, body := factorySkillMd(t, dist)
	wantContains(t, body, "First paragraph.", "Second paragraph.", "- List item 1")
}

func TestCreateTransformerFactoryShouldEmitAllSpecFieldsWhenConfigured(t *testing.T) {
	config := factoryBaseConfig()
	config.FrontmatterFields = []string{"user-invocable", "argument-hint", "license", "compatibility", "metadata", "allowed-tools"}
	dist := factoryTransform(t, config, []impeccableSkill{{Name: "test", Description: "Test", UserInvocable: true, ArgumentHint: "[target]",
		License: "MIT", Compatibility: "claude-code", Metadata: "v1", AllowedTools: "Bash,Edit", Body: "Body"}})
	content, _, _ := factorySkillMd(t, dist)
	wantContains(t, content, "user-invocable: true", "argument-hint:", "license: MIT", "compatibility: claude-code", "metadata: v1", "allowed-tools: Bash,Edit")
}
