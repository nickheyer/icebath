package skills

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

const improvementSkillContent = "---\nname: test-skill\ndescription: A test skill for unit testing.\n---\n\n# Test Skill\n\nStep 1: Do the thing.\nStep 2: Do another thing.\nStep 3: Final step.\n"

func TestFuzzyPatchExactAndWhitespaceTrimmedMatches(t *testing.T) {
	s, skills := manageScope(t)
	mustObj(s.createSkill(context.Background(), "test-skill", improvementSkillContent, nil))
	if res := mustObj(s.patchSkill(context.Background(), "test-skill", strp("Step 1: Do the thing."), strp("Step 1: Done!"), nil, false)); !objSuccess(res) {
		t.Fatalf("exact %v", res.Vals)
	}
	if !strings.Contains(readText(t, filepath.Join(skills, "test-skill", "SKILL.md")), "Step 1: Done!") {
		t.Fatal("exact patch not on disk")
	}
	ws := "---\nname: ws-skill\ndescription: Whitespace test\n---\n\n# Commands\n\n    def hello():\n        print(\"hi\")\n"
	mustObj(s.createSkill(context.Background(), "ws-skill", ws, nil))
	res := mustObj(s.patchSkill(context.Background(), "ws-skill", strp("def hello():\n    print(\"hi\")"), strp("def hello():\n    print(\"hello world\")"), nil, false))
	if !objSuccess(res) || !strings.Contains(readText(t, filepath.Join(skills, "ws-skill", "SKILL.md")), `print("hello world")`) {
		t.Fatalf("whitespace trimmed %v", res.Vals)
	}
}

func TestFuzzyPatchMultipleMatchesBlockedWithoutReplaceAll(t *testing.T) {
	s, _ := manageScope(t)
	mustObj(s.createSkill(context.Background(), "dup-skill", "---\nname: dup-skill\ndescription: Duplicate test\n---\n\nword word word\n", nil))
	res := mustObj(s.patchSkill(context.Background(), "dup-skill", strp("word"), strp("replaced"), nil, false))
	if objSuccess(res) || !strings.Contains(strings.ToLower(objStr(res, "error")), "match") {
		t.Fatalf("ambiguous patch %v", res.Vals)
	}
}

func TestSkillManagePatchUsesFuzzy(t *testing.T) {
	s, _ := manageScope(t)
	mustObj(s.createSkill(context.Background(), "test-skill", improvementSkillContent, nil))
	res := manageCall(t, s, map[string]any{"action": "patch", "name": "test-skill", "old_string": "  Step 1: Do the thing.", "new_string": "Step 1: Updated."})
	if res["success"] != true {
		t.Fatalf("fuzzy dispatcher patch %v", res)
	}
}

func TestScanBlockRollsBackCreateAndNewSupportingFile(t *testing.T) {
	s, skills := manageScope(t)
	mustObj(s.createSkill(context.Background(), "rollback-skill", improvementSkillContent, nil))
	forceScanBlocked(t)
	created := mustObj(s.createSkill(context.Background(), "blocked-skill", improvementSkillContent, nil))
	if objSuccess(created) || lexists(filepath.Join(skills, "blocked-skill")) {
		t.Fatalf("blocked create kept %v", created.Vals)
	}
	reference := filepath.Join(skills, "rollback-skill", "references", "example.md")
	written := mustObj(s.writeFile(context.Background(), "rollback-skill", "references/example.md", strp("blocked\n")))
	if objSuccess(written) || lexists(reference) {
		t.Fatalf("blocked new file kept %v", written.Vals)
	}
}
