package plugins

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Load these skills line of the build_blender reminder resolved the way the setup beat result resolves it
func buildSkillsLine(t *testing.T, h string) string {
	t.Helper()
	facts := tools.OnboardingFacts{OS: "windows", Arch: "amd64", MachineKind: "PC", NvidiaPlatform: "discrete"}
	record := tools.NewOnboardingRecord(facts, 0, "run")
	record["picks"].(map[string]any)["installed"] = []any{"blender"}
	env := &tools.Env{Home: h}
	bundled := map[string]bool{}
	if tools.AllSkillNames != nil {
		for _, name := range tools.AllSkillNames(env) {
			bundled[name] = true
		}
	}
	resolve := func(name string) string {
		if strings.Contains(name, ":") {
			return tools.CatalogPluginSkill(env, name)
		}
		if bundled[name] {
			return name
		}
		return ""
	}
	stored := tools.OnboardingFactsFromStored(facts.Stored())
	for _, line := range strings.Split(tools.OnboardingReminder("build_blender", record, stored, "", resolve), "\n") {
		if strings.HasPrefix(line, "Load these skills") {
			return line
		}
	}
	t.Fatal("no skills line")
	return ""
}

// test_setup_beat_plugin_skills.py test_the_build_beat_names_an_installed_plugin_skill_by_its_registered_name
func TestTheBuildBeatNamesAnInstalledPluginSkillByItsRegisteredName(t *testing.T) {
	h, m := skillHostHome(t, "{\"plugins\": {\"enabled\": [\"blender\"]}}\n")
	plugin := filepath.Join(h, "plugins", "blender")
	memWrite(t, filepath.Join(plugin, "plugin.json"), "{\"$schema\": \""+PluginSchemaV1+"\", \"name\": \"blender\"}")
	memWrite(t, filepath.Join(plugin, "skills", "blender", "SKILL.md"), "---\nname: blender\ndescription: Drive Blender.\n---\nBody.\n")
	memWrite(t, filepath.Join(h, "plugins", ".install-metadata.json"), "{\"blender\": {\"source\": \"https://example.com/blender.git\", "+
		"\"catalog\": {\"name\": \"blender\", \"repo\": \"https://example.com/blender.git\", \"sha\": \""+strings.Repeat("0", 40)+"\"}}}")
	m.DiscoverAndLoad(homeCtx(h), true)
	meta := m.ListPluginSkillMetadata()
	if len(meta) != 1 || meta[0]["name"] != "blender:blender" {
		t.Fatalf("registered %v", meta)
	}
	if line := buildSkillsLine(t, h); !strings.Contains(line, "blender:blender") {
		t.Fatalf("line %q", line)
	}
	if dir := tools.CatalogInstalledDir(&tools.Env{Home: h}, "blender"); dir != plugin {
		t.Fatalf("installed dir %q", dir)
	}
}

// test_setup_beat_plugin_skills.py test_a_plugin_skill_that_is_not_installed_is_dropped
func TestAPluginSkillThatIsNotInstalledIsDropped(t *testing.T) {
	h, m := skillHostHome(t, "{\"plugins\": {\"enabled\": [\"blender\"]}}\n")
	m.DiscoverAndLoad(homeCtx(h), true)
	if line := buildSkillsLine(t, h); strings.Contains(line, "blender") {
		t.Fatalf("line %q", line)
	}
	if dir := tools.CatalogInstalledDir(&tools.Env{Home: h}, "blender"); dir != "" {
		t.Fatalf("installed dir %q", dir)
	}
}
