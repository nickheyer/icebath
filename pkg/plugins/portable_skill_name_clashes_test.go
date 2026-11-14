package plugins

import (
	"path/filepath"
	"strings"
	"testing"
)

// Home holding the portable plugins foo.bar and foo-bar, each with a workflow skill, after one forced discovery
func clashingPortableHome(t *testing.T, cfg string) (string, *Manager) {
	t.Helper()
	h, m := skillHostHome(t, cfg)
	for _, name := range []string{"foo.bar", "foo-bar"} {
		plugin := filepath.Join(h, "plugins", name)
		memWrite(t, filepath.Join(plugin, "plugin.json"), "{\"$schema\": \""+PluginSchemaV1+"\", \"name\": \""+name+"\"}")
		memWrite(t, filepath.Join(plugin, "skills", "workflow", "SKILL.md"), "---\nname: workflow\ndescription: "+name+" workflow.\n---\nBody of "+name+".\n")
	}
	m.DiscoverAndLoad(homeCtx(h), true)
	return h, m
}

func TestAClashingPortableSkillHasOneOwnerAndOldNamesReachOnlyTheirOwn(t *testing.T) {
	h, m := clashingPortableHome(t, "{\"plugins\": {\"enabled\": [\"foo.bar\", \"foo-bar\"], \"disabled\": []}}\n")
	m.mu.RLock()
	skill := m.pluginSkills["foo-bar:workflow"]
	m.mu.RUnlock()
	if skill == nil {
		t.Fatalf("no plugin owns foo-bar:workflow, registered %v", m.ListPluginSkillMetadata())
	}
	owner, loser := skill.PluginKey, "foo.bar"
	if owner == "foo.bar" {
		loser = "foo-bar"
	} else if owner != "foo-bar" {
		t.Fatalf("owner %q is neither clashing plugin", owner)
	}
	scope := skillHostScope(t, h)
	current := skillViewJSON(t, scope, "foo-bar:workflow")
	content, _ := current["content"].(string)
	if current["success"] != true || !strings.Contains(content, "Body of "+owner+".") {
		t.Fatalf("current %v", current)
	}
	if owned := skillViewJSON(t, scope, portableDataNamespace(owner)+":workflow"); owned["success"] != true {
		t.Fatalf("the owner's hashed name %v", owned)
	}
	if lost := skillViewJSON(t, scope, portableDataNamespace(loser)+":workflow"); lost["success"] != false {
		t.Fatalf("the loser's hashed name reached a skill: %v", lost)
	}
}

func TestDisablingOnePluginDoesNotBlockTheOtherWithTheSamePrefix(t *testing.T) {
	h, _ := clashingPortableHome(t, "{\"plugins\": {\"enabled\": [\"foo.bar\"], \"disabled\": [\"foo-bar\"]}}\n")
	result := skillViewJSON(t, skillHostScope(t, h), "foo-bar:workflow")
	content, _ := result["content"].(string)
	if result["success"] != true || !strings.Contains(content, "Body of foo.bar.") {
		t.Fatalf("result %v", result)
	}
}
