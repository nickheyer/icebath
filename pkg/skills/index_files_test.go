package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestIterSkillIndexFilesPrunesExcludedAndSupportDirs(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"b/SKILL.md", "a/SKILL.md", "a/references/nested/SKILL.md", "cat/c/SKILL.md",
		".git/x/SKILL.md", "node_modules/y/SKILL.md", "plain/scripts/SKILL.md", "a/DESCRIPTION.md",
	} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := IterSkillIndexFiles(root, "SKILL.md")
	want := []string{
		filepath.Join(root, "a", "SKILL.md"), filepath.Join(root, "b", "SKILL.md"),
		filepath.Join(root, "cat", "c", "SKILL.md"), filepath.Join(root, "plain", "scripts", "SKILL.md"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := IterSkillIndexFiles(filepath.Join(root, "missing"), "SKILL.md"); len(got) != 0 {
		t.Fatalf("missing dir listed %v", got)
	}
}
