package tuiserver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLearningGraphAnswersTheProfileGraph(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.MkdirAll(filepath.Join(h, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "memories", "MEMORY.md"), []byte("Deploys go through staging\n§\nUser prefers short answers"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := lookupMethod("learning.graph")(t.Context(), "r1", map[string]any{})
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("response %v", resp)
	}
	stats, _ := result["stats"].(map[string]any)
	if stats["memory_nodes"] != 2 {
		t.Fatalf("stats %v", stats)
	}
	for _, key := range []string{"nodes", "edges", "clusters", "memory"} {
		if _, ok := result[key]; !ok {
			t.Fatalf("result lacks %s: %v", key, result)
		}
	}
}
