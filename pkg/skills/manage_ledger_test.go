package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ledgerSkillContent = "---\nname: my-skill\ndescription: Ledger test skill.\n---\n\n# My Skill\n\nOriginal body.\n"

// Ledger rows of one skill newest first
func manageLedgerRows(t *testing.T, s *Scope, skill string) []map[string]any {
	t.Helper()
	rows, err := s.LedgerListEntries(skill, -1)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m := map[string]any{}
		for _, k := range r.Keys {
			m[k] = r.Vals[k]
		}
		out = append(out, m)
	}
	return out
}

func TestSkillManageLedgersBackgroundAndForegroundPatches(t *testing.T) {
	for _, origin := range []string{BackgroundReview, "foreground"} {
		s, skills := manageScope(t)
		SetCurrentWriteOrigin(s.Env, origin)
		if res := manageCall(t, s, map[string]any{"action": "create", "name": "my-skill", "content": ledgerSkillContent}); res["success"] != true {
			t.Fatalf("create %v", res)
		}
		skillMD := filepath.Join(skills, "my-skill", "SKILL.md")
		original, _ := os.ReadFile(skillMD)
		markBackgroundReviewSkillRead(s.Env, skillMD)
		if res := manageCall(t, s, map[string]any{"action": "patch", "name": "my-skill", "old_string": "Original body.", "new_string": "Updated body."}); res["success"] != true {
			t.Fatalf("patch %v", res)
		}
		var patch map[string]any
		for _, r := range manageLedgerRows(t, s, "my-skill") {
			if r["action"] == "patch" {
				patch = r
			}
		}
		want := "agent"
		if origin == BackgroundReview {
			want = "curator"
		}
		if patch == nil || patch["actor"] != want {
			t.Fatalf("%s patch row %v", origin, patch)
		}
		ok, msg, err := s.LedgerRollbackEntry(context.Background(), patch["id"].(string))
		after, _ := os.ReadFile(skillMD)
		if err != nil || !ok || string(after) != string(original) {
			t.Fatalf("rollback %v %s %v", ok, msg, err)
		}
	}
}

func TestSkillManageLedgersEditDeleteAndRestoresDeletedSkill(t *testing.T) {
	s, skills := manageScope(t)
	manageCall(t, s, map[string]any{"action": "create", "name": "my-skill", "content": ledgerSkillContent})
	skillMD := filepath.Join(skills, "my-skill", "SKILL.md")
	original, _ := os.ReadFile(skillMD)
	manageCall(t, s, map[string]any{"action": "edit", "name": "my-skill", "content": strings.Replace(ledgerSkillContent, "Original body.", "Edited body.", 1)})
	manageCall(t, s, map[string]any{"action": "edit", "name": "my-skill", "content": ledgerSkillContent})
	if res := manageCall(t, s, map[string]any{"action": "delete", "name": "my-skill", "absorbed_into": ""}); res["success"] != true {
		t.Fatalf("delete %v", res)
	}
	rows := manageLedgerRows(t, s, "my-skill")
	var actions []string
	for _, r := range rows {
		actions = append(actions, r["action"].(string))
	}
	if strings.Join(actions, ",") != "delete,edit,edit,create" {
		t.Fatalf("actions %v", actions)
	}
	ev := rows[0]["evidence"]
	absorbed, _ := ev.(interface{ Get(string) (any, bool) }).Get("absorbed_into")
	archived, _ := ev.(interface{ Get(string) (any, bool) }).Get("archived")
	if absorbed != "" || archived != false || len(rows[0]["after"].([]any)) != 0 || len(rows[0]["before"].([]any)) == 0 {
		t.Fatalf("delete row %v", rows[0])
	}
	ok, msg, err := s.LedgerRollbackEntry(context.Background(), rows[0]["id"].(string))
	restored, _ := os.ReadFile(skillMD)
	if err != nil || !ok || string(restored) != string(original) {
		t.Fatalf("restore %v %s %v", ok, msg, err)
	}
}

func TestSkillManageRollbackRemovesCreatedFilesAndGateOffWritesNothing(t *testing.T) {
	s, skills := manageScope(t)
	manageCall(t, s, map[string]any{"action": "create", "name": "my-skill", "content": ledgerSkillContent})
	if res := manageCall(t, s, map[string]any{"action": "write_file", "name": "my-skill", "file_path": "references/extra.md", "file_content": "extra\n"}); res["success"] != true {
		t.Fatalf("write %v", res)
	}
	extra := filepath.Join(skills, "my-skill", "references", "extra.md")
	row := manageLedgerRows(t, s, "my-skill")[0]
	if ok, msg, err := s.LedgerRollbackEntry(context.Background(), row["id"].(string)); err != nil || !ok || lexists(extra) {
		t.Fatalf("rollback %v %s %v", ok, msg, err)
	}
	off, offSkills := manageScope(t)
	off.Config["skills"] = map[string]any{"ledger": false}
	manageCall(t, off, map[string]any{"action": "create", "name": "my-skill", "content": ledgerSkillContent})
	res := manageCall(t, off, map[string]any{"action": "patch", "name": "my-skill", "old_string": "Original body.", "new_string": "New."})
	if res["success"] != true || lexists(off.LedgerPath()) || lexists(off.LedgerBlobsDir()) || !lexists(filepath.Join(offSkills, "my-skill")) {
		t.Fatalf("gate off %v", res)
	}
}
