package config

import "testing"

func runtimeOwners(manifests ...PluginConfigKeys) (RuntimeKeyOwners, *int) {
	scans := 0
	return RuntimeKeyOwners{
		IsPresetFieldPath: func(parts []string) bool {
			return len(parts) == 4 && parts[0] == "moa" && parts[1] == "presets" && parts[3] == "fanout"
		},
		SelectionConfigKeys: func() map[string]bool { return map[string]bool{"browser.cloud_provider": true, "stt.provider": true} },
		ActiveManifests:     func() []PluginConfigKeys { scans++; return manifests },
	}, &scans
}

func TestRuntimeReadKeyOwnersInOrder(t *testing.T) {
	owners, scans := runtimeOwners()
	if !IsRuntimeReadKey([]string{"moa", "presets", "review.v2", "fanout"}, owners) || *scans != 0 {
		t.Fatal("moa preset field with a dotted name refused")
	}
	if !IsRuntimeReadKey([]string{"browser", "cloud_provider"}, owners) || *scans != 0 {
		t.Fatal("selection key refused")
	}
	for _, key := range [][]string{{"computer_use", "backend"}, {"dashboard", "kanban", "columns"}} {
		if !IsRuntimeReadKey(key, owners) {
			t.Fatalf("core runtime key %v refused", key)
		}
	}
	if IsRuntimeReadKey([]string{"browser", "cloud_providerz"}, owners) || *scans != 1 {
		t.Fatal("typo accepted or plugin scan skipped")
	}
	if IsRuntimeReadKey([]string{"computer_use", "backendz"}, owners) {
		t.Fatal("sibling of a core runtime key accepted")
	}
}

func TestPluginConfigKeysCoverThemselvesAndBelow(t *testing.T) {
	owners, _ := runtimeOwners(PluginConfigKeys{Name: "retaindb", ConfigKeys: []string{"memory.retaindb"}})
	for _, key := range [][]string{{"memory", "retaindb"}, {"memory", "retaindb", "url"}} {
		if !IsRuntimeReadKey(key, owners) {
			t.Fatalf("%v refused", key)
		}
	}
	if IsRuntimeReadKey([]string{"memory", "retaindbx"}, owners) {
		t.Fatal("prefix sibling accepted")
	}
	none, _ := runtimeOwners()
	if IsRuntimeReadKey([]string{"memory", "retaindb"}, none) {
		t.Fatal("key counted without an active plugin")
	}
}

func TestPluginCannotSilenceTyposInAWholeCoreSection(t *testing.T) {
	got := PluginDeclaredConfigKeys([]PluginConfigKeys{{Name: "hostile", ConfigKeys: []string{"agent", "agent.max_turns", "memory.retaindb"}}})
	if got["agent"] || got["agent.max_turns"] || !got["memory.retaindb"] || len(got) != 1 {
		t.Fatalf("declared = %v", got)
	}
	owners, _ := runtimeOwners(PluginConfigKeys{Name: "hostile", ConfigKeys: []string{"agent"}})
	if IsRuntimeReadKey([]string{"agent", "max_turnz"}, owners) {
		t.Fatal("whole core section silenced a typo")
	}
}
