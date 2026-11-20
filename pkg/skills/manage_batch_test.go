package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func batchSK(n string) string {
	return "---\nname: " + n + "\ndescription: Use when probing batch ops. Behavior.\n---\n# Probe\nStep 1.\n"
}

// Runs one operations batch with every op defaulting to the skill name
func batchCall(t *testing.T, s *Scope, name string, ops ...map[string]any) map[string]any {
	t.Helper()
	list := make([]any, len(ops))
	for i, op := range ops {
		if _, ok := op["name"]; !ok {
			op["name"] = name
		}
		list[i] = op
	}
	return manageCall(t, s, map[string]any{"action": "", "name": "", "operations": list})
}

func readText(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type op = map[string]any

func TestBatchCreatePlusFilesAtomic(t *testing.T) {
	s, skills := manageScope(t)
	r := batchCall(t, s, "probe", op{"action": "create", "content": batchSK("probe")},
		op{"action": "write_file", "file_path": "references/a.md", "file_content": "a"},
		op{"action": "write_file", "file_path": "scripts/r.py", "file_content": "pass"})
	if r["success"] != true || r["operations_applied"] != float64(3) {
		t.Fatal(r)
	}
	for _, rel := range []string{"SKILL.md", "references/a.md", "scripts/r.py"} {
		if !lexists(filepath.Join(skills, "probe", rel)) {
			t.Fatal(rel)
		}
	}
}

func TestBatchAdvisoryFindingsRideOnTheOpResult(t *testing.T) {
	s, _ := manageScope(t)
	batchCall(t, s, "probe", op{"action": "create", "content": batchSK("probe")})
	r := batchCall(t, s, "probe", op{"action": "patch", "old_string": "Step 1.", "new_string": strings.Repeat("- rule; why.\n", lintBodySoftBudgetChars/12+1)})
	if r["success"] != true {
		t.Fatal(r)
	}
	found := false
	for _, w := range r["results"].([]any)[0].(map[string]any)["lint_warnings"].([]any) {
		found = found || w.(map[string]any)["rule"] == "oversized-body"
	}
	if !found {
		t.Fatal(r)
	}
}

func TestBatchMidFailureRollsBackAndFailedCreateRemovesPartial(t *testing.T) {
	s, skills := manageScope(t)
	batchCall(t, s, "probe", op{"action": "create", "content": batchSK("probe")})
	r := batchCall(t, s, "probe", op{"action": "patch", "old_string": "Step 1.", "new_string": "Step ONE."},
		op{"action": "write_file", "file_path": "bad/nope.md", "file_content": "x"})
	if r["success"] != false || r["failed_index"] != float64(1) {
		t.Fatal(r)
	}
	if c := readText(t, filepath.Join(skills, "probe", "SKILL.md")); !strings.Contains(c, "Step 1.") || strings.Contains(c, "Step ONE.") {
		t.Fatal(c)
	}
	r = batchCall(t, s, "fresh", op{"action": "create", "content": batchSK("fresh")},
		op{"action": "write_file", "file_path": "../escape.md", "file_content": "x"})
	if r["success"] != false || lexists(filepath.Join(skills, "fresh")) {
		t.Fatal(r)
	}
}

func TestBatchValidationRules(t *testing.T) {
	s, skills := manageScope(t)
	batchCall(t, s, "probe", op{"action": "create", "content": batchSK("probe")})
	if r := batchCall(t, s, "probe", op{"action": "delete"}); r["success"] != true || lexists(filepath.Join(skills, "probe")) {
		t.Fatal(r)
	}
	batchCall(t, s, "probe", op{"action": "create", "content": batchSK("probe")})
	if r := batchCall(t, s, "probe", op{"action": "patch", "old_string": "Step 1.", "new_string": "X."}, op{"action": "delete"}); r["success"] != false || !strings.Contains(r["error"].(string), "SOLE") {
		t.Fatal(r)
	}
	if r := batchCall(t, s, "x", op{"action": "write_file", "file_path": "references/a.md", "file_content": "a"}, op{"action": "create", "content": batchSK("x")}); r["success"] != false {
		t.Fatal(r)
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	if r := batchCall(t, s, "x", op{"action": "create", "content": batchSK("x"), "category": 5}); r["success"] != false || !strings.Contains(r["error"].(string), "Category must be a string") {
		t.Fatal(r)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatal(left)
	}
	if r := batchCall(t, s, "x"); r["success"] != false {
		t.Fatal(r)
	}
	many := make([]map[string]any, 21)
	for i := range many {
		many[i] = op{"action": "patch"}
	}
	if r := batchCall(t, s, "x", many...); r["success"] != false || !strings.Contains(r["error"].(string), "capped") {
		t.Fatal(r)
	}
}

func TestBatchIntraBatchConflictGuard(t *testing.T) {
	s, skills := manageScope(t)
	batchCall(t, s, "probe", op{"action": "create", "content": batchSK("probe")})
	batchCall(t, s, "probe", op{"action": "write_file", "file_path": "references/c.md", "file_content": "seed"})
	patchC := func() op {
		return op{"action": "patch", "file_path": "references/c.md", "old_string": "seed", "new_string": "edited"}
	}
	for _, ops := range [][]map[string]any{
		{{"action": "write_file", "file_path": "references/a.md", "file_content": "1"}, {"action": "write_file", "file_path": "references/a.md", "file_content": "2"}},
		{{"action": "write_file", "file_path": "references/b.md", "file_content": "x"}, {"action": "remove_file", "file_path": "references/b.md"}},
		{patchC(), {"action": "write_file", "file_path": "references/c.md", "file_content": "CLOB"}},
		{patchC(), {"action": "remove_file", "file_path": "references/c.md"}},
		{{"action": "write_file", "file_path": "references/d.md", "file_content": "1"}, {"action": "write_file", "file_path": "./references//d.md", "file_content": "2"}},
	} {
		if r := batchCall(t, s, "probe", ops...); r["success"] != false || !strings.Contains(r["error"].(string), "discard") {
			t.Fatal(ops, r)
		}
	}
	if c := readText(t, filepath.Join(skills, "probe", "references", "c.md")); c != "seed" {
		t.Fatal(c)
	}
	if r := batchCall(t, s, "probe", op{"action": "write_file", "file_path": "references/e.md", "file_content": "base"}, op{"action": "patch", "file_path": "references/e.md", "old_string": "base", "new_string": "base+"}); r["success"] != true {
		t.Fatal(r)
	}
	if r := batchCall(t, s, "probe", op{"action": "patch", "old_string": "Step 1.", "new_string": "P."}, op{"action": "patch", "content": batchSK("probe")}); r["success"] != false || !strings.Contains(r["error"].(string), "rewrite") {
		t.Fatal(r)
	}
	if r := batchCall(t, s, "probe", op{"action": "patch", "content": strings.Replace(batchSK("probe"), "Step 1.", "F.", 1)}, op{"action": "patch", "old_string": "F.", "new_string": "G."}); r["success"] != true {
		t.Fatal(r)
	}
	if r := batchCall(t, s, "probe", op{"action": "patch", "old_string": "G.", "new_string": "H."}, op{"action": "patch", "old_string": "H.", "new_string": "I."}); r["success"] != true {
		t.Fatal(r)
	}
}

func TestBatchCrossSkillRollbackKeepsPreExistingDirContent(t *testing.T) {
	s, skills := manageScope(t)
	batchCall(t, s, "alpha", op{"action": "create", "content": batchSK("alpha")})
	gamma := filepath.Join(skills, "gamma")
	os.Mkdir(gamma, 0o755)
	real := batchSteps.applyOp
	batchSteps.applyOp = func(sc *Scope, ctx context.Context, payload map[string]any, taskID, sessionID string) (*pyjson.Object, error) {
		if payload["action"] == "write_file" {
			os.WriteFile(filepath.Join(gamma, "dropped.txt"), []byte("keep me"), 0o644)
		}
		return real(sc, ctx, payload, taskID, sessionID)
	}
	r := batchCall(t, s, "", op{"name": "alpha", "action": "patch", "old_string": "Step 1.", "new_string": "Step A."},
		op{"name": "beta", "action": "create", "content": batchSK("beta")},
		op{"name": "gamma", "action": "create", "content": batchSK("gamma")},
		op{"name": "gamma", "action": "write_file", "file_path": "references/a.md", "file_content": "a"},
		op{"name": "beta", "action": "write_file", "file_path": "bad/nope.md", "file_content": "x"})
	batchSteps.applyOp = real
	if r["success"] != false || r["failed_index"] != float64(4) {
		t.Fatal(r)
	}
	if c := readText(t, filepath.Join(skills, "alpha", "SKILL.md")); !strings.Contains(c, "Step 1.") || strings.Contains(c, "Step A.") {
		t.Fatal(c)
	}
	if lexists(filepath.Join(skills, "beta")) || readText(t, filepath.Join(gamma, "dropped.txt")) != "keep me" {
		t.Fatal("beta survived or gamma lost its foreign file")
	}
	if left, _ := os.ReadDir(gamma); len(left) != 1 || left[0].Name() != "dropped.txt" {
		t.Fatal(left)
	}
	delta := filepath.Join(skills, "delta")
	os.Mkdir(delta, 0o755)
	r = batchCall(t, s, "delta", op{"action": "create", "content": batchSK("delta")},
		op{"action": "write_file", "file_path": "references/a.md", "file_content": "a"},
		op{"action": "write_file", "file_path": "bad/nope.md", "file_content": "x"})
	if r["success"] != false || lexists(delta) {
		t.Fatal(r)
	}
	if r := batchCall(t, s, "delta", op{"action": "create", "content": batchSK("delta")}); r["success"] != true {
		t.Fatal(r)
	}
}

func TestBatchFailedRestoreNeverDestroysTheSkill(t *testing.T) {
	s, skills := manageScope(t)
	batchCall(t, s, "probe", op{"action": "create", "content": batchSK("probe")})
	real, n := batchSteps.copyTree, 0
	batchSteps.copyTree = func(src, dst string) error {
		if n++; n == 2 {
			return errors.New("disk full")
		}
		return real(src, dst)
	}
	r := batchCall(t, s, "probe", op{"action": "patch", "old_string": "Step 1.", "new_string": "Step ONE."},
		op{"action": "write_file", "file_path": "bad/nope.md", "file_content": "x"})
	batchSteps.copyTree = real
	if r["success"] != false || !strings.Contains(r["error"].(string), "ROLLBACK FAILED") {
		t.Fatal(r)
	}
	if c := readText(t, filepath.Join(skills, "probe", "SKILL.md")); !strings.Contains(c, "Step ONE.") {
		t.Fatal(c)
	}
}

func TestBatchSingleOpPathUnchanged(t *testing.T) {
	s, _ := manageScope(t)
	batchCall(t, s, "probe", op{"action": "create", "content": batchSK("probe")})
	if r := manageCall(t, s, map[string]any{"action": "patch", "name": "probe", "old_string": "Step 1.", "new_string": "Step 1 (single)."}); r["success"] != true {
		t.Fatal(r)
	}
}

func TestBatchStagesAsOnePendingWriteWhenGated(t *testing.T) {
	s, skills := manageScope(t)
	cfgPath := filepath.Join(filepath.Dir(skills), "config.yaml")
	os.WriteFile(cfgPath, []byte("skills:\n  write_approval: true\n"), 0o644)
	r := batchCall(t, s, "probe", op{"action": "create", "content": batchSK("probe")},
		op{"action": "write_file", "file_path": "references/a.md", "file_content": "a"})
	if r["staged"] != true || lexists(filepath.Join(skills, "probe")) {
		t.Fatal(r)
	}
	pending := pendingSkillRecords(t, s)
	if len(pending) != 1 {
		t.Fatal(pending)
	}
	payload := pending[0]["payload"].(map[string]any)
	if payload["action"] != "batch" || len(payload["operations"].([]any)) != 2 {
		t.Fatal(payload)
	}
	os.Remove(cfgPath)
	var out map[string]any
	decodeJSON(t, ApplySkillPending(context.Background(), s.Env, payload), &out)
	if out["success"] != true || out["operations_applied"] != float64(2) {
		t.Fatal(out)
	}
}

// Pending skill write records with payloads decoded through JSON
func pendingSkillRecords(t *testing.T, s *Scope) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range tools.ListPending(s.Env, tools.WriteApprovalSkills) {
		text, err := pyjson.Dumps(rec)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		decodeJSON(t, text, &m)
		out = append(out, m)
	}
	return out
}
