package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// test_spawn_tree_records.py test_spawn_tree_list_survives_non_dict_snapshot
func TestSpawnTreeListSurvivesNonDictSnapshot(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	d := spawnTreeSessionDir("sess-x")
	if err := os.WriteFile(filepath.Join(d, "bad.json"), []byte(`"not a snapshot"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "good.json"), []byte(`{"session_id": "sess-x", "label": "ok"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	envelope := lookupMethod("spawn_tree.list")(context.Background(), 1, map[string]any{"session_id": "sess-x"})
	if envelope["error"] != nil {
		t.Fatalf("envelope %v", envelope)
	}
	entries := envelope["result"].(map[string]any)["entries"].([]any)
	labels := map[any]bool{}
	for _, entry := range entries {
		labels[entry.(map[string]any)["label"]] = true
	}
	if !labels["ok"] || len(entries) != 2 {
		t.Fatalf("entries %v", entries)
	}
}

// test_spawn_tree_records.py test_spawn_tree_load_rejects_non_dict_snapshot
func TestSpawnTreeLoadRejectsNonDictSnapshot(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	bad := filepath.Join(spawnTreeSessionDir("sess-y"), "bad.json")
	if err := os.WriteFile(bad, []byte("[1, 2]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if envelope := lookupMethod("spawn_tree.load")(context.Background(), 1, map[string]any{"path": bad}); envelope["error"] == nil {
		t.Fatalf("envelope %v", envelope)
	}
}
