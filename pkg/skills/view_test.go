package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Temp home env with one demo skill holding a reference file
func viewFixture(t *testing.T) (*tools.Env, string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	skillDir := filepath.Join(home.SkillsDir(h), "demo-dedup-skill")
	os.MkdirAll(filepath.Join(skillDir, "references"), 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: demo-dedup-skill\ndescription: Demo skill\n---\n\n# Demo\n\nStep one\n"), 0o644)
	os.WriteFile(filepath.Join(skillDir, "references", "api.md"), []byte("Detailed reference API docs here\n"), 0o644)
	os.WriteFile(filepath.Join(h, ".env"), []byte("OPENAI_API_KEY=sk-do-not-leak\n"), 0o600)
	ClearSkillsCache()
	ResetSkillViewDedup("")
	return &tools.Env{Home: h, Config: config.Defaults(), Values: &sync.Map{}}, skillDir
}

// Runs the skill_view handler and decodes its JSON result
func viewCall(t *testing.T, env *tools.Env, taskID string, args map[string]any) map[string]any {
	t.Helper()
	out := skillViewHandler(context.Background(), &tools.Call{Name: "skill_view", Args: args, TaskID: taskID, Env: env})
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	return r
}

func TestSkillViewTraversalGuards(t *testing.T) {
	env, skillDir := viewFixture(t)
	r := viewCall(t, env, "", map[string]any{"name": "../../.env"})
	if r["success"] != false || !strings.Contains(strings.ToLower(r["error"].(string)), "traversal") {
		t.Fatalf("dotdot name %v", r)
	}
	r = viewCall(t, env, "", map[string]any{"name": filepath.Join(env.Home, ".env")})
	if r["success"] != false || !strings.Contains(strings.ToLower(r["error"].(string)), "relative path") {
		t.Fatalf("absolute name %v", r)
	}
	for _, fp := range []string{"../../.env", "references/../../../.env"} {
		r = viewCall(t, env, "", map[string]any{"name": "demo-dedup-skill", "file_path": fp})
		b, _ := json.Marshal(r)
		if r["success"] != false || !strings.Contains(strings.ToLower(r["error"].(string)), "traversal") || strings.Contains(string(b), "sk-do-not-leak") {
			t.Fatalf("file path %s %v", fp, r)
		}
	}
	r = viewCall(t, env, "", map[string]any{"name": "demo-dedup-skill", "file_path": "references/api.md"})
	if r["success"] != true || !strings.Contains(r["content"].(string), "API docs here") {
		t.Fatalf("legit file %v", r)
	}
	r = viewCall(t, env, "", map[string]any{"name": "demo-dedup-skill"})
	if r["success"] != true {
		t.Fatalf("legit skill %v", r)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("TOP SECRET DATA"), 0o644)
	os.Symlink(secret, filepath.Join(skillDir, "references", "leak.md"))
	r = viewCall(t, env, "", map[string]any{"name": "demo-dedup-skill", "file_path": "references/leak.md"})
	b, _ := json.Marshal(r)
	if r["success"] != false || !strings.Contains(strings.ToLower(r["error"].(string)), "escapes") || strings.Contains(string(b), "TOP SECRET DATA") {
		t.Fatalf("symlink escape %v", r)
	}
	r = viewCall(t, env, "", map[string]any{"name": 7})
	if r["error"] != "Skill name must be a string." {
		t.Fatalf("non string name %v", r)
	}
}

func TestSkillViewDedup(t *testing.T) {
	env, skillDir := viewFixture(t)
	args := map[string]any{"name": "demo-dedup-skill"}
	r := viewCall(t, env, "t1", args)
	if r["success"] != true || !strings.Contains(r["content"].(string), "Step one") {
		t.Fatalf("first view %v", r)
	}
	r2 := viewCall(t, env, "t1", args)
	if r2["success"] != true || r2["dedup"] != true || r2["content_returned"] != false || r2["content"] != nil {
		t.Fatalf("repeat view %v", r2)
	}
	ref := map[string]any{"name": "demo-dedup-skill", "file_path": "references/api.md"}
	if r := viewCall(t, env, "t1", ref); !strings.Contains(r["content"].(string), "Detailed reference") {
		t.Fatalf("linked first %v", r)
	}
	if r := viewCall(t, env, "t1", ref); r["dedup"] != true {
		t.Fatalf("linked repeat %v", r)
	}
	if r := viewCall(t, env, "t2", args); !strings.Contains(fmString(r["content"]), "Step one") {
		t.Fatalf("other task %v", r)
	}
	ResetSkillViewDedup("t1")
	if r := viewCall(t, env, "t1", args); !strings.Contains(fmString(r["content"]), "Step one") {
		t.Fatalf("after reset %v", r)
	}
	for i := 0; i < 2; i++ {
		if r := viewCall(t, env, "", args); !strings.Contains(fmString(r["content"]), "Step one") {
			t.Fatalf("no task id %v", r)
		}
	}
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: demo-dedup-skill\ndescription: Demo skill\n---\n\n# Demo\n\nStep 111\n"), 0o644)
	if r := viewCall(t, env, "t1", args); r["dedup"] != nil || !strings.Contains(fmString(r["content"]), "Step 111") {
		t.Fatalf("same size rewrite %v", r)
	}
	if r := viewCall(t, env, "t1", args); r["dedup"] != true {
		t.Fatalf("unchanged after rewrite %v", r)
	}
}

func TestSkillViewBackgroundReviewSkipsDedupAndMarksRead(t *testing.T) {
	env, skillDir := viewFixture(t)
	args := map[string]any{"name": "demo-dedup-skill"}
	SetCurrentWriteOrigin(env, BackgroundReview)
	for i := 0; i < 2; i++ {
		r := viewCall(t, env, "t1", args)
		if r["success"] != true || !strings.Contains(fmString(r["content"]), "Step one") || r["dedup"] != nil || r["content_returned"] != nil {
			t.Fatalf("review view %v", r)
		}
	}
	if !backgroundReviewHasRead(env, filepath.Join(skillDir, "SKILL.md")) {
		t.Fatal("review read mark missing")
	}
	fg := &tools.Env{Home: env.Home, Config: env.Config, Values: &sync.Map{}}
	if r := viewCall(t, fg, "t1", args); !strings.Contains(fmString(r["content"]), "Step one") {
		t.Fatalf("foreground polluted %v", r)
	}
	if r := viewCall(t, fg, "t1", args); r["dedup"] != true || r["content_returned"] != false {
		t.Fatalf("foreground repeat %v", r)
	}
}

func TestSkillProvenanceOrigins(t *testing.T) {
	env := &tools.Env{Values: &sync.Map{}}
	if CurrentWriteOrigin(env) != "foreground" || IsBackgroundReview(env) || IsUnattendedReview(env) {
		t.Fatal("default origin is not foreground")
	}
	SetCurrentWriteOrigin(env, BackgroundReview)
	if !IsBackgroundReview(env) || !IsUnattendedReview(env) {
		t.Fatal("background review not detected")
	}
	SetReviewAttended(env, true)
	if IsUnattendedReview(env) {
		t.Fatal("attended review reported unattended")
	}
	SetCurrentWriteOrigin(env, "")
	if CurrentWriteOrigin(env) != "foreground" {
		t.Fatal("empty origin did not fall back to foreground")
	}
	if CurrentWriteOrigin(nil) != "foreground" {
		t.Fatal("nil env origin")
	}
}

func TestSkillsListAndCatalogCache(t *testing.T) {
	env, _ := viewFixture(t)
	scope := ScopeFromEnv(env)
	var r map[string]any
	json.Unmarshal([]byte(scope.SkillsList(context.Background(), "")), &r)
	if r["success"] != true || r["count"] != float64(1) {
		t.Fatalf("listing %v", r)
	}
	second := filepath.Join(home.SkillsDir(env.Home), "mlops", "axolotl")
	os.MkdirAll(second, 0o755)
	os.WriteFile(filepath.Join(second, "SKILL.md"), []byte("---\nname: axolotl\ndescription: Fine tune\n---\nbody\n"), 0o644)
	all := scope.FindAllSkills(false)
	if len(all) != 2 {
		t.Fatalf("install not seen through the signature %v", all)
	}
	sorted := SortSkills(all)
	if sorted[0]["name"] != "demo-dedup-skill" || sorted[1]["category"] != "mlops" {
		t.Fatalf("sort order %v", sorted)
	}
	json.Unmarshal([]byte(scope.SkillsList(context.Background(), "mlops")), &r)
	if r["count"] != float64(1) {
		t.Fatalf("category filter %v", r)
	}
	os.RemoveAll(second)
	if got := scope.FindAllSkills(false); len(got) != 1 {
		t.Fatalf("uninstall not seen %v", got)
	}
}

func TestParseTagsForms(t *testing.T) {
	for _, c := range []struct {
		in   any
		want string
	}{{[]any{"a", "b"}, "a,b"}, {"[a, 'b']", "a,b"}, {"a, b", "a,b"}, {nil, ""}, {"", ""}} {
		if got := strings.Join(parseTags(c.in), ","); got != c.want {
			t.Errorf("parseTags(%v) = %q", c.in, got)
		}
	}
}
