package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func createDirSkillMD(name string) string {
	return "---\nname: " + name + "\ndescription: Use when testing create dir routing. One-line behavior.\n---\n\n# " + name + "\n\nBody.\n"
}

func TestGetSkillCreateDir(t *testing.T) {
	s, skills := manageScope(t)
	h := filepath.Dir(skills)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	brain := filepath.Join(t.TempDir(), "brain-skills")
	cases := []struct {
		value any
		want  string
	}{
		{nil, ""},
		{brain, resolvePath(brain)},
		{"brain", resolvePath(filepath.Join(h, "brain"))},
		{"~/brain-skills", resolvePath(filepath.Join(userHome, "brain-skills"))},
		{skills, ""},
		{"", ""},
	}
	for _, c := range cases {
		cfg := map[string]any{"external_dirs": []any{}}
		if c.value != nil {
			cfg = map[string]any{"create_dir": c.value}
		}
		s.Config["skills"] = cfg
		if got := s.SkillCreateDir(); got != c.want {
			t.Fatalf("%v: got %q want %q", c.value, got, c.want)
		}
	}
}

func TestDisplaySkillCreateDir(t *testing.T) {
	s, _ := manageScope(t)
	s.Config["skills"] = map[string]any{}
	if got := s.DisplaySkillCreateDir(); !strings.HasSuffix(got, "/skills/") {
		t.Fatal(got)
	}
	s.Config["skills"] = map[string]any{"create_dir": filepath.Join(t.TempDir(), "opt-brain")}
	if got := s.DisplaySkillCreateDir(); !strings.Contains(got, "opt-brain") {
		t.Fatal(got)
	}
}

func TestSkillManageSchemaStableAcrossCreateDirChanges(t *testing.T) {
	s, _ := manageScope(t)
	s.Config["skills"] = map[string]any{"create_dir": filepath.Join(t.TempDir(), "first-brain")}
	first := tools.Default.Get("skill_manage").Schema
	s.Config["skills"] = map[string]any{"create_dir": filepath.Join(t.TempDir(), "second-brain")}
	second := tools.Default.Get("skill_manage").Schema
	if !reflect.DeepEqual(first, second) || first["description"] != SkillManageSchema()["description"] {
		t.Fatal("schema changed with create_dir")
	}
}

func TestCreateDirDiscovery(t *testing.T) {
	s, skills := manageScope(t)
	brain := filepath.Join(t.TempDir(), "brain-skills")
	os.MkdirAll(brain, 0o755)
	s.Config["skills"] = map[string]any{"create_dir": brain}
	dirs := s.AllSkillsDirs()
	if resolvePath(dirs[0]) != resolvePath(skills) || !containsResolved(dirs, brain) {
		t.Fatal(dirs)
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	s.Config["skills"] = map[string]any{"create_dir": missing}
	if containsResolved(s.AllSkillsDirs(), missing) {
		t.Fatal("missing create_dir scanned")
	}
	s.Config["skills"] = map[string]any{"create_dir": brain, "external_dirs": []any{brain}}
	n := 0
	for _, d := range s.AllSkillsDirs() {
		if resolvePath(d) == resolvePath(brain) {
			n++
		}
	}
	if n != 1 {
		t.Fatal(s.AllSkillsDirs())
	}
}

func containsResolved(dirs []string, want string) bool {
	for _, d := range dirs {
		if resolvePath(d) == resolvePath(want) {
			return true
		}
	}
	return false
}

func TestCreateRoutingToCreateDir(t *testing.T) {
	s, skills := manageScope(t)
	brain := filepath.Join(t.TempDir(), "brain-skills")
	s.Config["skills"] = map[string]any{"create_dir": brain}
	op := func(m map[string]any) map[string]any { return manageCall(t, s, map[string]any{"operations": []any{m}}) }
	if r := op(map[string]any{"action": "create", "name": "routed-skill", "content": createDirSkillMD("routed-skill")}); r["success"] != true {
		t.Fatal(r)
	}
	if !lexists(filepath.Join(brain, "routed-skill", "SKILL.md")) || lexists(filepath.Join(skills, "routed-skill")) {
		t.Fatal("create did not route to create_dir")
	}
	flat := manageCall(t, s, map[string]any{"action": "create", "name": "routed-skill-flat", "content": createDirSkillMD("routed-skill-flat")})
	if flat["success"] != true || flat["path"] != filepath.Join(brain, "routed-skill-flat") {
		t.Fatal(flat)
	}
	if r := op(map[string]any{"action": "create", "name": "cat-skill", "category": "devops", "content": createDirSkillMD("cat-skill")}); r["success"] != true || !lexists(filepath.Join(brain, "devops", "cat-skill", "SKILL.md")) {
		t.Fatal(r)
	}
	op(map[string]any{"action": "create", "name": "patchable-skill", "content": createDirSkillMD("patchable-skill")})
	if _, ok := s.FindSkill("patchable-skill"); !ok {
		t.Fatal("created skill not findable")
	}
	if r := op(map[string]any{"action": "patch", "name": "patchable-skill", "old_string": "Body.", "new_string": "Patched."}); r["success"] != true {
		t.Fatal(r)
	}
	if data, _ := os.ReadFile(filepath.Join(brain, "patchable-skill", "SKILL.md")); !strings.Contains(string(data), "Patched.") {
		t.Fatal(string(data))
	}
	s.Config["skills"] = map[string]any{}
	if r := op(map[string]any{"action": "create", "name": "local-skill", "content": createDirSkillMD("local-skill")}); r["success"] != true || !lexists(filepath.Join(skills, "local-skill", "SKILL.md")) {
		t.Fatal(r)
	}
}
