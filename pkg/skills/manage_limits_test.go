package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

func sizedSkillContent(bodyChars int) string {
	return "---\nname: test-skill\ndescription: A test skill\n---\n" + "# Test Skill\n\n" + strings.Repeat("x", max(0, bodyChars-15))
}

func TestValidateContentSize(t *testing.T) {
	if e := validateContentSize(strings.Repeat("a", 1000), "SKILL.md"); e != "" {
		t.Fatal(e)
	}
	if e := validateContentSize(strings.Repeat("a", MaxSkillContentChars+1), "references/api.md"); !strings.Contains(e, "references/api.md") {
		t.Fatal(e)
	}
}

func TestSkillSizeLimits(t *testing.T) {
	s, skills := manageScope(t)
	ok := func(r map[string]any) bool { return r["success"] == true }
	if r := manageCall(t, s, map[string]any{"action": "create", "name": "small-skill", "content": sizedSkillContent(5000)}); !ok(r) {
		t.Fatal(r)
	}
	front := "---\nname: edge-skill\ndescription: Edge case\n---\n# Edge\n\n"
	edge := front + strings.Repeat("x", MaxSkillContentChars-pystr.Len(front))
	if r := manageCall(t, s, map[string]any{"action": "create", "name": "edge-skill", "content": edge}); !ok(r) {
		t.Fatal(r)
	}
	manageCall(t, s, map[string]any{"action": "create", "name": "grow-me", "content": sizedSkillContent(1000)})
	big := strings.Replace(sizedSkillContent(MaxSkillContentChars+100), "name: test-skill", "name: grow-me", 1)
	if r := manageCall(t, s, map[string]any{"action": "edit", "name": "grow-me", "content": big}); ok(r) {
		t.Fatal("edit over limit succeeded")
	}
	manageCall(t, s, map[string]any{"action": "create", "name": "near-limit", "content": sizedSkillContent(MaxSkillContentChars - 50)})
	if r := manageCall(t, s, map[string]any{"action": "patch", "name": "near-limit", "old_string": "# Test Skill", "new_string": "# Test Skill\n" + strings.Repeat("y", 200)}); ok(r) {
		t.Fatal("patch over limit succeeded")
	}
	manageCall(t, s, map[string]any{"action": "create", "name": "with-ref", "content": sizedSkillContent(1000)})
	manageCall(t, s, map[string]any{"action": "write_file", "name": "with-ref", "file_path": "references/data.md", "file_content": "# Data\n\nSmall content."})
	r := manageCall(t, s, map[string]any{"action": "patch", "name": "with-ref", "old_string": "Small content.", "new_string": strings.Repeat("x", MaxSkillContentChars+100), "file_path": "references/data.md"})
	if ok(r) || !strings.Contains(r["error"].(string), "references/data.md") {
		t.Fatal(r)
	}
	manageCall(t, s, map[string]any{"action": "create", "name": "file-test", "content": sizedSkillContent(1000)})
	if r := manageCall(t, s, map[string]any{"action": "write_file", "name": "file-test", "file_path": "references/huge.md", "file_content": strings.Repeat("x", MaxSkillContentChars+1)}); ok(r) {
		t.Fatal("write_file over limit succeeded")
	}
	manageCall(t, s, map[string]any{"action": "create", "name": "file-ok", "content": sizedSkillContent(1000)})
	if r := manageCall(t, s, map[string]any{"action": "write_file", "name": "file-ok", "file_path": "references/normal.md", "file_content": "# Normal\n\n" + strings.Repeat("x", 5000)}); !ok(r) {
		t.Fatal(r)
	}
	giant := filepath.Join(skills, "manual-giant")
	os.MkdirAll(giant, 0o755)
	os.WriteFile(filepath.Join(giant, "SKILL.md"), []byte(strings.Replace(sizedSkillContent(200_000), "name: test-skill", "name: manual-giant", 1)), 0o644)
	var view map[string]any
	decodeJSON(t, s.SkillView(context.Background(), "manual-giant", "", "", false), &view)
	if c, _ := view["content"].(string); pystr.Len(c) <= MaxSkillContentChars {
		t.Fatal(pystr.Len(c))
	}
}
