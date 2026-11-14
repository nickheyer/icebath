package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
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

func TestSwapWalkReadDirSkipsASubtreeThatVanishesAfterItsListing(t *testing.T) {
	root := t.TempDir()
	for _, s := range []string{"s0", "s1"} {
		p := filepath.Join(root, "cat", s, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	listed := []string{}
	restore := SwapWalkReadDir(func(dir string) ([]os.DirEntry, error) {
		listed = append(listed, dir)
		entries, err := os.ReadDir(dir)
		if dir == filepath.Join(root, "cat") {
			os.RemoveAll(filepath.Join(root, "cat", "s1"))
		}
		return entries, err
	})
	got := IterSkillIndexFiles(root, "SKILL.md")
	restore()
	if !reflect.DeepEqual(got, []string{filepath.Join(root, "cat", "s0", "SKILL.md")}) {
		t.Fatalf("got %v", got)
	}
	if want := []string{root, filepath.Join(root, "cat"), filepath.Join(root, "cat", "s0"), filepath.Join(root, "cat", "s1")}; !reflect.DeepEqual(listed, want) {
		t.Fatalf("listed %v", listed)
	}
}

func TestIterSkillIndexFilesListsEverySymlinkedPathLikeOSWalk(t *testing.T) {
	root := t.TempDir()
	skill := filepath.Join(root, "real", "skill")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := filepath.Join(root, "cat")
	if err := os.MkdirAll(cat, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{"a", "b"} {
		if err := os.Symlink(filepath.Join("..", "real", "skill"), filepath.Join(cat, link)); err != nil {
			t.Skip("symlinks unavailable: " + err.Error())
		}
	}
	got := IterSkillIndexFiles(root, "SKILL.md")
	want := []string{filepath.Join(cat, "a", "SKILL.md"), filepath.Join(cat, "b", "SKILL.md"), filepath.Join(skill, "SKILL.md")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("two links to one skill listed %v want %v", got, want)
	}
	manifest := buildSkillsManifest(root)
	for _, rel := range []string{filepath.Join("cat", "a", "SKILL.md"), filepath.Join("cat", "b", "SKILL.md"), filepath.Join("real", "skill", "SKILL.md")} {
		if _, ok := manifest.Get(rel); !ok {
			t.Fatalf("manifest lacks %s, has %v", rel, manifest.Keys)
		}
	}
	if err := os.Symlink("..", filepath.Join(cat, "loop")); err != nil {
		t.Fatal(err)
	}
	looped := IterSkillIndexFiles(root, "SKILL.md")
	if len(looped) <= len(want) {
		t.Fatalf("the walk did not descend through the link cycle: %d paths", len(looped))
	}
	for _, p := range looped {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("listed path %s does not resolve: %v", p, err)
		}
	}
	if !slices.IsSorted(looped) {
		t.Fatal("paths are not sorted")
	}
	if runtime.GOOS == "linux" && len(looped) != 121 {
		t.Fatalf("link cycle listed %d paths, os.walk lists 121 before the kernel refuses the link depth", len(looped))
	}
}
