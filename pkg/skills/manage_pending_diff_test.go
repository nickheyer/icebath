package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Probe SKILL.md text of the pending diff tests
func probeSkillText(name string) string {
	return "---\nname: " + name + "\ndescription: Probe skill for pending-diff tests.\n---\n# Probe\nStep 1.\n"
}

func TestStagedBatchDiffResolvesSkillsThroughTheInstalledLookup(t *testing.T) {
	s, skills := manageScope(t)
	os.MkdirAll(filepath.Join(skills, "probe"), 0o755)
	os.WriteFile(filepath.Join(skills, "probe", "SKILL.md"), []byte(probeSkillText("probe")), 0o644)
	rec := tools.StageWrite(context.Background(), s.Env, "skills", map[string]any{"action": "batch", "operations": []any{
		map[string]any{"action": "create", "name": "newprobe", "content": probeSkillText("newprobe")},
		map[string]any{"action": "patch", "name": "probe", "old_string": "Step 1.", "new_string": "Step ONE."},
		map[string]any{"action": "write_file", "name": "newprobe", "file_path": "ref.md", "file_content": "x\n"},
		map[string]any{"action": "patch", "name": "newprobe", "file_path": "ref.md", "old_string": "x", "new_string": "y"},
	}}, "batch(4 ops) on newprobe, probe", "foreground")
	pending := tools.GetPending(s.Env, "skills", rec["id"].(string))
	if pending == nil {
		t.Fatal("staged record not found")
	}
	out := tools.SkillPendingDiff(s.Env, pending, nil)
	for _, want := range []string{"## op 1/4:", "-Step 1.", "+Step ONE.", "a/ref.md", "-x", "+y"} {
		if !strings.Contains(out, want) {
			t.Fatalf("diff lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "(batch on") || strings.Contains(out, "(patch") {
		t.Fatalf("diff fell back:\n%s", out)
	}
	single := tools.SkillPendingDiff(s.Env, map[string]any{"payload": map[string]any{"action": "patch", "name": "probe", "old_string": "Step 1.", "new_string": "Step ONE."}}, nil)
	if !strings.Contains(single, "-Step 1.") || !strings.Contains(single, "+Step ONE.") {
		t.Fatalf("single patch diff:\n%s", single)
	}
}
