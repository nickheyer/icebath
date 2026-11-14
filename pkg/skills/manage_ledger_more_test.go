package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Scope with my-skill created through skill_manage and its SKILL.md path
func ledgerManagedSkill(t *testing.T) (*Scope, string) {
	t.Helper()
	s, skills := manageScope(t)
	if res := manageCall(t, s, map[string]any{"action": "create", "name": "my-skill", "content": ledgerSkillContent}); res["success"] != true {
		t.Fatalf("create %v", res)
	}
	return s, filepath.Join(skills, "my-skill", "SKILL.md")
}

// Newest ledger row of the skill with the action
func ledgerRowWithAction(t *testing.T, s *Scope, action string) map[string]any {
	t.Helper()
	for _, r := range manageLedgerRows(t, s, "my-skill") {
		if r["action"] == action {
			return r
		}
	}
	t.Fatalf("no %s row", action)
	return nil
}

func TestLedgerMissingBlobAbortsRollbackBeforeAnyChange(t *testing.T) {
	s, skillMD := ledgerManagedSkill(t)
	id := ledgerAppendOK(t, s, "patch", "my-skill", ledgerItems(t, skillMD, strings.Repeat("a", 64)), []any{}, "", nil)
	current := readText(t, skillMD)
	ok, msg, err := s.LedgerRollbackEntry(context.Background(), id)
	if err != nil || ok || !strings.Contains(msg, "missing blob") || readText(t, skillMD) != current {
		t.Fatalf("missing blob rollback %v %q %v", ok, msg, err)
	}
}

func TestLedgerArchiveLandsWithCuratorActor(t *testing.T) {
	ctx := context.Background()
	s, _ := ledgerManagedSkill(t)
	if ok, msg, err := s.ArchiveSkill(WithLedgerActor(ctx, "curator"), "my-skill"); err != nil || !ok {
		t.Fatalf("archive %v %s %v", ok, msg, err)
	}
	var archives []map[string]any
	for _, r := range manageLedgerRows(t, s, "my-skill") {
		if r["action"] == "archive" {
			archives = append(archives, r)
		}
	}
	if len(archives) != 1 || archives[0]["actor"] != "curator" || len(archives[0]["before"].([]any)) == 0 || len(archives[0]["after"].([]any)) == 0 {
		t.Fatalf("archive rows %v", archives)
	}
	if ok, msg, err := s.RestoreSkill(ctx, "my-skill"); err != nil || !ok {
		t.Fatalf("restore %v %s %v", ok, msg, err)
	}
	ledgerRowWithAction(t, s, "restore")
}

func TestLedgerRollbackFailsClosedWhenSafetyCaptureFails(t *testing.T) {
	s, skillMD := ledgerManagedSkill(t)
	if res := manageCall(t, s, map[string]any{"action": "patch", "name": "my-skill", "old_string": "Original body.", "new_string": "Updated body."}); res["success"] != true {
		t.Fatalf("patch %v", res)
	}
	entry := ledgerRowWithAction(t, s, "patch")
	current := readText(t, skillMD)
	real := ledgerSteps.append
	ledgerSteps.append = func(*Scope, context.Context, string, any, []any, []any, string, *pyjson.Object) (string, bool) {
		return "", false
	}
	defer func() { ledgerSteps.append = real }()
	ok, msg, err := s.LedgerRollbackEntry(context.Background(), entry["id"].(string))
	if err != nil || ok || !strings.Contains(msg, "safety capture failed") || readText(t, skillMD) != current {
		t.Fatalf("rollback without safety row %v %q %v", ok, msg, err)
	}
}

func TestLedgerFailureNeverBlocksTheMutation(t *testing.T) {
	real := ledgerSteps.snapshot
	ledgerSteps.snapshot = func(*Scope, string, bool) ([]any, error) { return nil, errors.New("disk full") }
	defer func() { ledgerSteps.snapshot = real }()
	s, _ := ledgerManagedSkill(t)
	if res := manageCall(t, s, map[string]any{"action": "patch", "name": "my-skill", "old_string": "Original body.", "new_string": "Updated body."}); res["success"] != true {
		t.Fatalf("patch blocked by ledger failure %v", res)
	}
}

func TestLedgerDeleteAfterRehomeCapturesFullPackageFromBackup(t *testing.T) {
	s, skillMD := ledgerManagedSkill(t)
	extra := filepath.Join(filepath.Dir(skillMD), "references", "extra.md")
	if res := manageCall(t, s, map[string]any{"action": "write_file", "name": "my-skill", "file_path": "references/extra.md", "file_content": "roadmap body"}); res["success"] != true {
		t.Fatalf("write %v", res)
	}
	writeSkillsTarball(t, s.Home, [][2]string{{"my-skill/SKILL.md", readText(t, skillMD)}, {"my-skill/references/extra.md", "roadmap body"}}, "2026-08-01T00-00-00Z")
	os.Remove(extra)
	os.Remove(filepath.Dir(extra))
	if res := manageCall(t, s, map[string]any{"action": "delete", "name": "my-skill"}); res["success"] != true {
		t.Fatalf("delete %v", res)
	}
	entry := ledgerRowWithAction(t, s, "delete")
	names := ledgerNames(entry["before"])
	if !names["SKILL.md"] || !names["extra.md"] {
		t.Fatalf("delete ledger captured only %v after the support files were re-homed", names)
	}
	if ok, msg, err := s.LedgerRollbackEntry(context.Background(), entry["id"].(string)); err != nil || !ok {
		t.Fatalf("rollback %v %s %v", ok, msg, err)
	}
	if !lexists(skillMD) || readText(t, extra) != "roadmap body" {
		t.Fatal("rollback did not restore the full package")
	}
}

func TestLedgerDeleteRollbackWithoutBackupStillWorks(t *testing.T) {
	s, skillMD := ledgerManagedSkill(t)
	if res := manageCall(t, s, map[string]any{"action": "delete", "name": "my-skill"}); res["success"] != true {
		t.Fatalf("delete %v", res)
	}
	entry := ledgerRowWithAction(t, s, "delete")
	if names := ledgerNames(entry["before"]); len(names) != 1 || !names["SKILL.md"] {
		t.Fatalf("before names %v", names)
	}
	if ok, msg, err := s.LedgerRollbackEntry(context.Background(), entry["id"].(string)); err != nil || !ok {
		t.Fatalf("rollback %v %s %v", ok, msg, err)
	}
	if readText(t, skillMD) != ledgerSkillContent {
		t.Fatal("rollback content differs")
	}
}
