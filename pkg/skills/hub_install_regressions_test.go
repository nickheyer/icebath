package skills

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Hub over tmp/skills with a victim directory beside it and a lock holding the given entries
func traversalHub(t *testing.T, entries map[string]any) (*Hub, string, string) {
	t.Helper()
	h, tmp := newTestHub(t)
	skillsDir := filepath.Join(tmp, "skills")
	victim := filepath.Join(tmp, "do-not-delete")
	writeTestFile(t, filepath.Join(victim, "important.txt"), "data")
	data, _ := json.Marshal(map[string]any{"version": 1, "installed": entries})
	writeTestFile(t, h.LockFilePath(), string(data))
	return h, skillsDir, victim
}

func TestUninstallPathTraversal(t *testing.T) {
	entry := func(p string) map[string]any {
		return map[string]any{"install_path": p, "source": "https://example.com", "version": "1.0"}
	}
	t.Run("parent segments rejected", func(t *testing.T) {
		h, _, victim := traversalHub(t, map[string]any{"evil": entry("../do-not-delete")})
		if ok, _, _ := h.UninstallSkill("evil"); ok {
			t.Fatal("traversal uninstall succeeded")
		}
		if _, err := os.Stat(filepath.Join(victim, "important.txt")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("absolute path rejected", func(t *testing.T) {
		h, _, victim := traversalHub(t, nil)
		data, _ := json.Marshal(map[string]any{"version": 1, "installed": map[string]any{"evil": entry(victim)}})
		writeTestFile(t, h.LockFilePath(), string(data))
		if ok, _, _ := h.UninstallSkill("evil"); ok {
			t.Fatal("absolute uninstall succeeded")
		}
		if _, err := os.Stat(victim); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("symlink escape rejected", func(t *testing.T) {
		h, skillsDir, victim := traversalHub(t, map[string]any{"trap": entry("trapdoor")})
		if err := os.Symlink(victim, filepath.Join(skillsDir, "trapdoor")); err != nil {
			t.Fatal(err)
		}
		if ok, _, _ := h.UninstallSkill("trap"); ok {
			t.Fatal("symlink uninstall succeeded")
		}
		if _, err := os.Stat(filepath.Join(victim, "important.txt")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("legitimate uninstall still works", func(t *testing.T) {
		e := entry("category/my-skill")
		e["trust_level"] = "community"
		h, skillsDir, _ := traversalHub(t, map[string]any{"my-skill": e})
		legit := filepath.Join(skillsDir, "category", "my-skill")
		writeTestFile(t, filepath.Join(legit, "SKILL.md"), "test")
		if ok, msg, err := h.UninstallSkill("my-skill"); !ok || err != nil {
			t.Fatal(ok, msg, err)
		}
		if _, err := os.Stat(legit); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("legitimate skill kept")
		}
	})
}

func TestBundleHashFilenameSensitivity(t *testing.T) {
	a, _ := bundleContentHash(textBundle("test", "SKILL.md", "hello", "scripts/run.sh", "world"))
	b, _ := bundleContentHash(textBundle("test", "SKILL.md", "world", "scripts/run.sh", "hello"))
	if a == b {
		t.Fatal("swapped file contents hashed identically")
	}
	dir := filepath.Join(t.TempDir(), "skill")
	writeTestFile(t, filepath.Join(dir, "SKILL.md"), "hello")
	writeTestFile(t, filepath.Join(dir, "scripts", "run.sh"), "world")
	if a != ContentHash(dir) {
		t.Fatalf("bundle %s disk %s", a, ContentHash(dir))
	}
}

func TestSkillHashSymmetry(t *testing.T) {
	skill := filepath.Join(t.TempDir(), "demo-skill")
	writeTestFile(t, filepath.Join(skill, "SKILL.md"), "---\nname: demo\n---\nbody\n")
	writeTestFile(t, filepath.Join(skill, "references", "methods", "x.md"), "x\n")
	writeTestFile(t, filepath.Join(skill, "Zeta.md"), "z\n")
	b := &SkillBundle{Name: "demo-skill", Source: "test", Identifier: "test/demo-skill", TrustLevel: "community"}
	filepath.WalkDir(skill, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(skill, p)
			data, _ := os.ReadFile(p)
			b.Files = append(b.Files, BundleFile{Path: rel, Data: data})
		}
		return nil
	})
	if got, _ := bundleContentHash(b); got != ContentHash(skill) {
		t.Fatalf("bundle %s disk %s", got, ContentHash(skill))
	}
	posix := &SkillBundle{Name: "s", Files: []BundleFile{{Path: "references/a.md", Data: []byte("a")}, {Path: "SKILL.md", Data: []byte("s")}}}
	windows := &SkillBundle{Name: "s", Files: []BundleFile{{Path: `references\a.md`, Data: []byte("a")}, {Path: "SKILL.md", Data: []byte("s")}}}
	p, _ := bundleContentHash(posix)
	w, _ := bundleContentHash(windows)
	if p != w {
		t.Fatalf("posix %s windows %s", p, w)
	}
}
