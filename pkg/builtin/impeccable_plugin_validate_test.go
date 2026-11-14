package builtin

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// One source agent of a manifest fixture, its file name and extra frontmatter lines
type manifestAgent struct {
	File        string
	Frontmatter string
}

// Manifest fixture, a nil manifest meaning the good one and nil shipped agents meaning every source agent
type manifestFixture struct {
	Manifest      any
	SourceAgents  []manifestAgent
	ShippedAgents []string
}

func goodPluginManifest(extra ...any) *pyjson.Object {
	manifest := pyjson.ObjectOf(
		"name", "impeccable",
		"description", "Test plugin",
		"version", "4.0.4",
		"author", pyjson.ObjectOf("name", "Paul Bakaus"),
		"homepage", "https://impeccable.style",
		"repository", "https://github.com/pbakaus/impeccable",
		"skills", "./skills/")
	for i := 0; i+1 < len(extra); i += 2 {
		manifest.Set(extra[i].(string), extra[i+1])
	}
	return manifest
}

func writeManifestFixture(t *testing.T, root string, f manifestFixture) {
	t.Helper()
	switch m := f.Manifest.(type) {
	case nil:
		writeAt(t, root, "plugin/.claude-plugin/plugin.json", jsonStringify(goodPluginManifest(), "  "))
	case string:
		writeAt(t, root, "plugin/.claude-plugin/plugin.json", m)
	default:
		writeAt(t, root, "plugin/.claude-plugin/plugin.json", jsonStringify(m, "  "))
	}
	for _, agent := range f.SourceAgents {
		frontmatter := agent.Frontmatter
		if frontmatter != "" {
			frontmatter += "\n"
		}
		writeAt(t, root, "skill/agents/"+agent.File, "---\n"+frontmatter+"description: t\n---\nBody.\n")
	}
	shipped := f.ShippedAgents
	if shipped == nil {
		for _, agent := range f.SourceAgents {
			shipped = append(shipped, agent.File)
		}
	}
	for _, file := range shipped {
		writeAt(t, root, "plugin/agents/"+file, "---\ndescription: t\n---\nBody.\n")
	}
}

func manifestFindings(t *testing.T, root string) []pluginFinding {
	t.Helper()
	findings, err := collectPluginManifestFindings(root)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

func wantFindingCount(t *testing.T, findings []pluginFinding, n int) {
	t.Helper()
	if len(findings) != n {
		t.Fatalf("%d findings, want %d: %+v", len(findings), n, findings)
	}
}

func wantReason(t *testing.T, reason string, patterns ...string) {
	t.Helper()
	for _, p := range patterns {
		if !regexp.MustCompile(p).MatchString(reason) {
			t.Errorf("reason %q does not match %s", reason, p)
		}
	}
}

func TestCollectPluginManifestFindingsACleanManifestWithShippedAgentsProducesNoFindings(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, manifestFixture{SourceAgents: []manifestAgent{{File: "reviewer.md"}, {File: "producer.md"}}})
	wantFindingCount(t, manifestFindings(t, root), 0)
}

func TestCollectPluginManifestFindingsFlagsAnAgentsKeyGivenAsAnArrayOfFilePaths(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, manifestFixture{
		Manifest:     goodPluginManifest("agents", []any{"./agents/reviewer.md", "./agents/producer.md"}),
		SourceAgents: []manifestAgent{{File: "reviewer.md"}, {File: "producer.md"}},
	})
	findings := manifestFindings(t, root)
	wantFindingCount(t, findings, 1)
	if findings[0].RelPath != "plugin/.claude-plugin/plugin.json" {
		t.Fatal(findings[0].RelPath)
	}
	wantReason(t, findings[0].Reason, `"agents" key`, `zero agents`)
}

func TestCollectPluginManifestFindingsFlagsAnAgentsKeyOfAnyOtherShapeToo(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, manifestFixture{Manifest: goodPluginManifest("agents", "./agents")})
	if !slices.ContainsFunc(manifestFindings(t, root), func(f pluginFinding) bool { return strings.Contains(f.Reason, `"agents" key`) }) {
		t.Fatal("no finding names the agents key")
	}
}

func TestCollectPluginManifestFindingsFlagsAManifestKeyOutsideTheVerifiedLoaderContract(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, manifestFixture{Manifest: goodPluginManifest("mcpServers", "./mcp.json")})
	findings := manifestFindings(t, root)
	wantFindingCount(t, findings, 1)
	wantReason(t, findings[0].Reason, `unverified manifest key "mcpServers"`, `KNOWN_LOADER_KEYS`)
}

func TestCollectPluginManifestFindingsFlagsASkillsPathWithoutTheTrailingSlash(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, manifestFixture{Manifest: goodPluginManifest("skills", "./skills")})
	findings := manifestFindings(t, root)
	wantFindingCount(t, findings, 1)
	wantReason(t, findings[0].Reason, `trailing-slash`)
}

func TestCollectPluginManifestFindingsFlagsASourceAgentMissingFromTheShippedSubtree(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, manifestFixture{
		SourceAgents:  []manifestAgent{{File: "reviewer.md"}, {File: "producer.md"}},
		ShippedAgents: []string{"reviewer.md"},
	})
	findings := manifestFindings(t, root)
	wantFindingCount(t, findings, 1)
	if findings[0].RelPath != "plugin/agents/producer.md" {
		t.Fatal(findings[0].RelPath)
	}
	wantReason(t, findings[0].Reason, `never load`, `skill/agents/producer\.md`)
}

func TestCollectPluginManifestFindingsAClaudeNameRenameExpectsTheEmittedFilenameNotTheSourceBasename(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, manifestFixture{
		SourceAgents:  []manifestAgent{{File: "reviewer.md", Frontmatter: "claude-name: impeccable-reviewer"}},
		ShippedAgents: []string{"impeccable-reviewer.md"},
	})
	wantFindingCount(t, manifestFindings(t, root), 0)
}

func TestCollectPluginManifestFindingsAClaudeNameRenameThatIsNotShippedIsReportedUnderTheEmittedFilename(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, manifestFixture{
		SourceAgents:  []manifestAgent{{File: "reviewer.md", Frontmatter: "claude-name: impeccable-reviewer"}},
		ShippedAgents: []string{},
	})
	findings := manifestFindings(t, root)
	wantFindingCount(t, findings, 1)
	if findings[0].RelPath != "plugin/agents/impeccable-reviewer.md" {
		t.Fatal(findings[0].RelPath)
	}
	wantReason(t, findings[0].Reason, `skill/agents/reviewer\.md`)
}

func TestCollectPluginManifestFindingsAFrontmatterNameOverridesTheSourceBasenameLikeTheBuildDoes(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, manifestFixture{
		SourceAgents:  []manifestAgent{{File: "reviewer.md", Frontmatter: "name: custom-reviewer"}},
		ShippedAgents: []string{"custom-reviewer.md"},
	})
	wantFindingCount(t, manifestFindings(t, root), 0)
}

func TestCollectPluginManifestFindingsAnAgentWhoseProvidersListExcludesClaudeCodeOwesNoShippedCopy(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, manifestFixture{
		SourceAgents:  []manifestAgent{{File: "codex-only.md", Frontmatter: "providers: codex"}, {File: "reviewer.md"}},
		ShippedAgents: []string{"reviewer.md"},
	})
	wantFindingCount(t, manifestFindings(t, root), 0)
}

func TestCollectPluginManifestFindingsReportsEveryProblemAtOnce(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, manifestFixture{
		Manifest:      goodPluginManifest("agents", []any{"./agents/reviewer.md"}, "skills", "./skills", "commands", "./commands/"),
		SourceAgents:  []manifestAgent{{File: "reviewer.md"}},
		ShippedAgents: []string{},
	})
	wantFindingCount(t, manifestFindings(t, root), 4)
}

func TestCollectPluginManifestFindingsAnAbsentPluginSubtreeProducesNoFindings(t *testing.T) {
	wantFindingCount(t, manifestFindings(t, t.TempDir()), 0)
}

func TestCollectPluginManifestFindingsAMalformedManifestIsAFindingNotAThrownStack(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, manifestFixture{Manifest: "{ not json"})
	findings := manifestFindings(t, root)
	wantFindingCount(t, findings, 1)
	wantReason(t, findings[0].Reason, `parse`)
}

func TestCollectPluginManifestFindingsValidJSONThatIsNotAnObjectIsAFindingNotAThrownStack(t *testing.T) {
	root := t.TempDir()
	for _, raw := range []string{"null", `"impeccable"`, "42", `["./agents/reviewer.md"]`} {
		writeManifestFixture(t, root, manifestFixture{Manifest: raw})
		findings := manifestFindings(t, root)
		wantFindingCount(t, findings, 1)
		wantReason(t, findings[0].Reason, `not a JSON object`)
	}
}

func TestCollectPluginManifestFindingsKnownLoaderKeysNeverReAdmitsAgents(t *testing.T) {
	if slices.Contains(knownLoaderKeys, "agents") {
		t.Fatal("KNOWN_LOADER_KEYS holds agents")
	}
}

func TestCommittedPluginSubtreeTheShippedManifestHonorsTheVerifiedLoaderContract(t *testing.T) {
	wantFindingCount(t, manifestFindings(t, impeccablePayloadRoot), 0)
}

func versionSkillMd(version string) string {
	return "---\nname: impeccable\nversion: " + version + "\nuser-invocable: true\n---\n\nBody.\n"
}

// Version fixture, an empty field leaving that file out
type versionFixture struct {
	Plugin, Marketplace, SubtreePlugin, CodexPlugin, Skill string
}

func writeVersionFixture(t *testing.T, root string, f versionFixture) {
	t.Helper()
	if f.Plugin != "" {
		writeAt(t, root, ".claude-plugin/plugin.json", jsonStringify(pyjson.ObjectOf("name", "impeccable", "version", f.Plugin), "  "))
	}
	if f.Marketplace != "" {
		writeAt(t, root, ".claude-plugin/marketplace.json", jsonStringify(pyjson.ObjectOf("plugins", []any{pyjson.ObjectOf("name", "impeccable", "version", f.Marketplace)}), "  "))
	}
	if f.SubtreePlugin != "" {
		writeAt(t, root, "plugin/.claude-plugin/plugin.json", jsonStringify(pyjson.ObjectOf("name", "impeccable", "version", f.SubtreePlugin, "skills", "./skills/"), "  "))
	}
	if f.CodexPlugin != "" {
		writeAt(t, root, "dist/openai/impeccable/.codex-plugin/plugin.json", jsonStringify(pyjson.ObjectOf("name", "impeccable", "version", f.CodexPlugin, "skills", "./skills/"), "  "))
	}
	if f.Skill != "" {
		writeAt(t, root, "plugin/skills/impeccable/SKILL.md", versionSkillMd(f.Skill))
	}
}

func wantMismatches(t *testing.T, got []pluginVersionMismatch, want ...pluginVersionMismatch) {
	t.Helper()
	if len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
		t.Fatalf("mismatches %+v, want %+v", got, want)
	}
}

func cliPackageJSON(version string) string {
	return jsonStringify(pyjson.ObjectOf("name", "impeccable", "version", version,
		"optionalDependencies", pyjson.ObjectOf("@impeccable/cli-darwin-arm64", "0.1.14")), "")
}

func TestCollectPluginVersionsNoMismatchesWhenEveryVersionAgrees(t *testing.T) {
	root := t.TempDir()
	writeVersionFixture(t, root, versionFixture{Plugin: "3.7.1", Marketplace: "3.7.1", SubtreePlugin: "3.7.1", CodexPlugin: "3.7.1", Skill: "3.7.1"})
	versions := collectPluginVersions(root)
	if versions.Source != "3.7.1" {
		t.Fatalf("source %v", versions.Source)
	}
	wantMismatches(t, versions.Mismatches)
}

func TestCollectPluginVersionsFlagsAnNpmPackageJSONWhoseVersionDriftedFromTheSkill(t *testing.T) {
	root := t.TempDir()
	writeVersionFixture(t, root, versionFixture{Plugin: "3.7.1", Marketplace: "3.7.1"})
	writeAt(t, root, "package.json", cliPackageJSON("3.6.0"))
	wantMismatches(t, collectPluginVersions(root).Mismatches, pluginVersionMismatch{"package.json", "3.6.0", "3.7.1"})

	writeAt(t, root, "package.json", cliPackageJSON("3.7.1"))
	wantMismatches(t, collectPluginVersions(root).Mismatches)
}

func TestCollectPluginVersionsLeavesAPackageJSONThatIsNotTheImpeccableCLIAlone(t *testing.T) {
	root := t.TempDir()
	writeVersionFixture(t, root, versionFixture{Plugin: "3.7.1", Marketplace: "3.7.1"})
	writeAt(t, root, "package.json", jsonStringify(pyjson.ObjectOf("name", "impeccable", "version", "3.2.1"), ""))
	versions := collectPluginVersions(root)
	wantMismatches(t, versions.Mismatches)
	if slices.ContainsFunc(versions.Checked, func(c pluginVersionCheck) bool { return c.RelPath == "package.json" }) {
		t.Fatalf("package.json was checked: %+v", versions.Checked)
	}
}

func TestCollectPluginVersionsFlagsALaggingMarketplaceJSON(t *testing.T) {
	root := t.TempDir()
	writeVersionFixture(t, root, versionFixture{Plugin: "3.7.1", Marketplace: "3.1.1", SubtreePlugin: "3.7.1", Skill: "3.7.1"})
	wantMismatches(t, collectPluginVersions(root).Mismatches, pluginVersionMismatch{".claude-plugin/marketplace.json", "3.1.1", "3.7.1"})
}

func TestCollectPluginVersionsFlagsAStalePluginSubtreeManifest(t *testing.T) {
	root := t.TempDir()
	writeVersionFixture(t, root, versionFixture{Plugin: "3.7.1", Marketplace: "3.7.1", SubtreePlugin: "3.6.0", Skill: "3.7.1"})
	wantMismatches(t, collectPluginVersions(root).Mismatches, pluginVersionMismatch{"plugin/.claude-plugin/plugin.json", "3.6.0", "3.7.1"})
}

func TestCollectPluginVersionsFlagsAStaleCodexPluginManifest(t *testing.T) {
	root := t.TempDir()
	writeVersionFixture(t, root, versionFixture{Plugin: "3.7.1", Marketplace: "3.7.1", SubtreePlugin: "3.7.1", CodexPlugin: "3.6.0", Skill: "3.7.1"})
	wantMismatches(t, collectPluginVersions(root).Mismatches, pluginVersionMismatch{"dist/openai/impeccable/.codex-plugin/plugin.json", "3.6.0", "3.7.1"})
}

func TestCollectPluginVersionsFlagsAStaleBundledSkillMdFrontmatterVersion(t *testing.T) {
	root := t.TempDir()
	writeVersionFixture(t, root, versionFixture{Plugin: "3.7.1", Marketplace: "3.7.1", SubtreePlugin: "3.7.1", Skill: "3.1.1"})
	wantMismatches(t, collectPluginVersions(root).Mismatches, pluginVersionMismatch{"plugin/skills/impeccable/SKILL.md", "3.1.1", "3.7.1"})
}

func TestCollectPluginVersionsReportsEveryDriftedFileAtOnce(t *testing.T) {
	root := t.TempDir()
	writeVersionFixture(t, root, versionFixture{Plugin: "3.7.1", Marketplace: "3.1.1", SubtreePlugin: "3.5.0", Skill: "3.1.1"})
	var rels []string
	for _, m := range collectPluginVersions(root).Mismatches {
		rels = append(rels, m.RelPath)
	}
	want := []string{".claude-plugin/marketplace.json", "plugin/.claude-plugin/plugin.json", "plugin/skills/impeccable/SKILL.md"}
	if !slices.Equal(rels, want) {
		t.Fatalf("drifted %v, want %v", rels, want)
	}
}

func TestCollectPluginVersionsSkipsFilesThatDoNotExistInsteadOfThrowing(t *testing.T) {
	root := t.TempDir()
	writeVersionFixture(t, root, versionFixture{Plugin: "3.7.1"})
	versions := collectPluginVersions(root)
	if versions.Source != "3.7.1" || len(versions.Checked) != 0 || len(versions.Mismatches) != 0 || len(versions.Errors) != 0 {
		t.Fatalf("%+v", versions)
	}
}

func TestCollectPluginVersionsReturnsANullSourceWithNoErrorsWhenRootPluginJSONIsAbsent(t *testing.T) {
	versions := collectPluginVersions(t.TempDir())
	if versions.Source != nil || len(versions.Mismatches) != 0 || len(versions.Errors) != 0 {
		t.Fatalf("%+v", versions)
	}
}

func TestCollectPluginVersionsReportsAMalformedCheckedManifestAsAnErrorInsteadOfThrowing(t *testing.T) {
	root := t.TempDir()
	writeVersionFixture(t, root, versionFixture{Plugin: "3.7.1", SubtreePlugin: "3.7.1", Skill: "3.7.1"})
	writeAt(t, root, ".claude-plugin/marketplace.json", `{ "plugins": [ { "version": `)
	versions := collectPluginVersions(root)
	wantMismatches(t, versions.Mismatches)
	if len(versions.Errors) != 1 || versions.Errors[0].RelPath != ".claude-plugin/marketplace.json" {
		t.Fatalf("errors %+v", versions.Errors)
	}
	wantReason(t, versions.Errors[0].Reason, `(?i)parse`)
}

func TestCollectPluginVersionsReportsAMalformedRootPluginJSONAsAnErrorNotAThrownStack(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, ".claude-plugin/plugin.json", "{ not json")
	versions := collectPluginVersions(root)
	if versions.Source != nil || len(versions.Errors) != 1 || versions.Errors[0].RelPath != ".claude-plugin/plugin.json" {
		t.Fatalf("%+v", versions)
	}
	wantReason(t, versions.Errors[0].Reason, `(?i)parse`)
}

func TestCollectPluginVersionsFlagsARootPluginJSONThatExistsButHasNoVersionField(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, ".claude-plugin/plugin.json", jsonStringify(pyjson.ObjectOf("name", "impeccable"), ""))
	versions := collectPluginVersions(root)
	want := []pluginFinding{{".claude-plugin/plugin.json", `missing "version" field`}}
	if versions.Source != nil || !reflect.DeepEqual(versions.Errors, want) {
		t.Fatalf("%+v", versions)
	}
}

func wantSkillVersion(t *testing.T, content, want string) {
	t.Helper()
	if got, ok := readSkillFrontmatterVersion(content); !ok || got != want {
		t.Fatalf("version %q %v, want %q", got, ok, want)
	}
}

func TestReadSkillFrontmatterVersionReadsAnUnquotedVersion(t *testing.T) {
	wantSkillVersion(t, versionSkillMd("3.7.1"), "3.7.1")
}

func TestReadSkillFrontmatterVersionStripsSurroundingQuotes(t *testing.T) {
	wantSkillVersion(t, "---\nversion: \"3.7.1\"\n---\n", "3.7.1")
}

func TestReadSkillFrontmatterVersionPrefersMetadataVersionWhileAcceptingTheLegacyTopLevelKey(t *testing.T) {
	wantSkillVersion(t, "---\nname: impeccable\nversion: 3.0.0\nmetadata:\n  version: 3.7.1\n---\n", "3.7.1")
}

func TestReadSkillFrontmatterVersionReadsMetadataVersionAfterNestedMetadataFields(t *testing.T) {
	wantSkillVersion(t, "---\nname: impeccable\nmetadata:\n  interface:\n    display_name: Impeccable\n  version: \"3.7.1\"\n---\n", "3.7.1")
}

func TestReadSkillFrontmatterVersionReturnsNullWhenThereIsNoFrontmatterBlock(t *testing.T) {
	if got, ok := readSkillFrontmatterVersion("no frontmatter here"); ok {
		t.Fatalf("version %q from text without frontmatter", got)
	}
}

func TestReadSkillFrontmatterVersionReadsAVersionFromCRLFEncodedFrontmatter(t *testing.T) {
	wantSkillVersion(t, "---\r\nname: impeccable\r\nversion: 3.7.1\r\nuser-invocable: true\r\n---\r\n\r\nBody.\r\n", "3.7.1")
}

func TestCollectPluginVersionsWithCRLFLineEndingsACRLFSavedSkillMdAtTheRightVersionIsNotAFalseMismatch(t *testing.T) {
	root := t.TempDir()
	writeVersionFixture(t, root, versionFixture{Plugin: "3.7.1", Marketplace: "3.7.1", SubtreePlugin: "3.7.1", Skill: "3.7.1"})
	skillPath := filepath.Join(root, "plugin", "skills", "impeccable", "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(strings.ReplaceAll(readTestFile(t, skillPath), "\n", "\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	versions := collectPluginVersions(root)
	wantMismatches(t, versions.Mismatches)
	if len(versions.Errors) != 0 {
		t.Fatalf("errors %+v", versions.Errors)
	}
}
