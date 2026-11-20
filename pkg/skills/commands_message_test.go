package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSkillMessageLayoutAndStablePrefix(t *testing.T) {
	s, dir := usageHome(t)
	skillDir := writeUsageSkill(t, dir, "demo", "tools")
	os.MkdirAll(filepath.Join(skillDir, "scripts", "sub"), 0o755)
	os.WriteFile(filepath.Join(skillDir, "scripts", "sub", "b.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(skillDir, "scripts", "a.js"), []byte("x"), 0o644)
	os.Symlink(filepath.Join(skillDir, "scripts", "a.js"), filepath.Join(skillDir, "scripts", "link.js"))
	var registered []string
	RegisterStablePrefixSink(func(p string) { registered = append(registered, p) })
	t.Cleanup(func() { RegisterStablePrefixSink(nil) })
	loaded := map[string]any{"content": "  Body for ${ICEBATH_SKILL_DIR}  ", "setup_skipped": true}
	msg := s.buildSkillMessage(context.Background(), loaded, skillDir, "[activation]", "do it", "late", "sess")
	want := strings.Join([]string{
		"[activation]", "", "Body for " + skillDir, "",
		"[Skill directory: " + skillDir + "]", skillDirNote, "",
		"[Skill setup note: " + setupSkippedNote + "]", "",
		"[This skill has supporting files (paths relative to the skill directory above):]",
		"- scripts/a.js", "- scripts/sub/b.js",
		"\nLoad any of these with skill_view(name=\"" + filepath.Join("tools", "demo") + "\", file_path=\"<path>\"), or run scripts directly by absolute path (e.g. `node " + skillDir + "/scripts/foo.js`).",
		"", singleSkillInstruction + "do it", "", "[Runtime note: late]",
	}, "\n")
	if msg != want {
		t.Fatalf("message mismatch\n%s\n---\n%s", msg, want)
	}
	if len(registered) != 1 || !strings.HasPrefix(msg, registered[0]) || !strings.HasSuffix(registered[0], singleSkillInstruction) {
		t.Fatalf("stable prefix %q", registered)
	}
	linked := map[string]any{"linked_files": map[string]any{"scripts": []any{"./run.sh"}, "references": []any{"api.md"}},
		"gateway_setup_hint": "hint", "content": "c", "name": "n"}
	msg = s.buildSkillMessage(context.Background(), linked, filepath.Join(t.TempDir(), "elsewhere"), "[a]", "", "", "")
	if !strings.Contains(msg, "[Skill setup note: hint]") || !strings.Contains(msg, "- api.md\n- run.sh\n") ||
		!strings.Contains(msg, `skill_view(name="n"`) {
		t.Fatalf("linked files message %s", msg)
	}
}

func TestScaffoldHeaderLines(t *testing.T) {
	got := scaffoldHeader("demo skill bundle", []string{"a", "b"}, []string{"lead"}, []string{"m"}, []string{"d"}, "extra", "user")
	want := "[IMPORTANT: The user has invoked the demo skill bundle, loading 2 skills together. Treat every skill below as active guidance for this turn.]\n\nlead\nSkills loaded: a, b\nSkills missing (skipped): m\nSkills disabled for this platform (skipped): d\n\nBundle instruction: extra\n\nUser instruction: user"
	if got != want {
		t.Fatalf("%q", got)
	}
	if note := setupNote(map[string]any{"setup_needed": true, "setup_note": "x"}); note != "x" {
		t.Fatal(note)
	}
	if note := setupNote(map[string]any{"setup_note": "x"}); note != "" {
		t.Fatal(note)
	}
}
