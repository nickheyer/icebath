//go:build unix

package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Permission bits of a path
func permOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// Scope with one created skill whose references/example.md holds body at mode
func modeSkill(t *testing.T, name, body string, mode os.FileMode) (*Scope, string, string) {
	t.Helper()
	s, skills := manageScope(t)
	mustObj(s.createSkill(context.Background(), name, improvementSkillContent, nil))
	reference := filepath.Join(skills, name, "references", "example.md")
	if body != "" {
		os.MkdirAll(filepath.Dir(reference), 0o755)
		os.WriteFile(reference, []byte(body), 0o644)
		os.Chmod(reference, mode)
	}
	return s, filepath.Join(skills, name, "SKILL.md"), reference
}

func TestNewDocumentsAreExactly0644UnderRestrictiveUmask(t *testing.T) {
	s, skills := manageScope(t)
	old := syscall.Umask(0o077)
	created := mustObj(s.createSkill(context.Background(), "umask-skill", improvementSkillContent, nil))
	written := mustObj(s.writeFile(context.Background(), "umask-skill", "references/example.md", strp("# Reference\n")))
	syscall.Umask(old)
	if !objSuccess(created) || !objSuccess(written) {
		t.Fatalf("writes %v %v", created.Vals, written.Vals)
	}
	if m := permOf(t, filepath.Join(skills, "umask-skill", "SKILL.md")); m != 0o644 {
		t.Fatalf("SKILL.md mode %o", m)
	}
	if m := permOf(t, filepath.Join(skills, "umask-skill", "references", "example.md")); m != 0o644 {
		t.Fatalf("reference mode %o", m)
	}
}

func TestSuccessfulWritesPreserveExistingMode(t *testing.T) {
	s, skillMD, _ := modeSkill(t, "explicit-skill", "", 0)
	os.Chmod(skillMD, 0o660)
	if res := mustObj(s.patchSkill(context.Background(), "explicit-skill", strp("Step 1: Do the thing."), strp("Step 1: Done!"), strp("SKILL.md"), false)); !objSuccess(res) || !strings.Contains(readText(t, skillMD), "Step 1: Done!") || permOf(t, skillMD) != 0o660 {
		t.Fatalf("explicit SKILL.md patch %v mode %o", res.Vals, permOf(t, skillMD))
	}

	s, skillMD, _ = modeSkill(t, "mode-skill", "", 0)
	os.Chmod(skillMD, 0o600)
	if res := mustObj(s.editSkill(context.Background(), "mode-skill", strings.Replace(improvementSkillContent, "Step 1: Do the thing.", "Step 1: Done!", 1))); !objSuccess(res) || permOf(t, skillMD) != 0o600 {
		t.Fatalf("edit %v mode %o", res.Vals, permOf(t, skillMD))
	}

	s, skillMD, _ = modeSkill(t, "mode-skill", "", 0)
	os.Chmod(skillMD, 0o600)
	if res := mustObj(s.patchSkill(context.Background(), "mode-skill", strp("Step 1: Do the thing."), strp("Step 1: Done!"), nil, false)); !objSuccess(res) || permOf(t, skillMD) != 0o600 {
		t.Fatalf("patch %v mode %o", res.Vals, permOf(t, skillMD))
	}

	s, _, reference := modeSkill(t, "mode-skill", "old\n", 0o600)
	if res := mustObj(s.writeFile(context.Background(), "mode-skill", "references/example.md", strp("new\n"))); !objSuccess(res) || permOf(t, reference) != 0o600 {
		t.Fatalf("support write %v mode %o", res.Vals, permOf(t, reference))
	}

	s, _, reference = modeSkill(t, "mode-skill", "old\n", 0o600)
	if res := mustObj(s.patchSkill(context.Background(), "mode-skill", strp("old"), strp("new"), strp("references/example.md"), false)); !objSuccess(res) || readText(t, reference) != "new\n" || permOf(t, reference) != 0o600 {
		t.Fatalf("support patch %v mode %o", res.Vals, permOf(t, reference))
	}
}

// Direct handler result of fn run while the post write scan is forced to a blocking verdict
func whileScanBlocked(fn func() (*pyjson.Object, error)) *pyjson.Object {
	real := manageSteps.securityScan
	manageSteps.securityScan = func(*Scope, string) string { return "blocked" }
	defer func() { manageSteps.securityScan = real }()
	return mustObj(fn())
}

func TestScanBlockedRollbacksPreserveExistingMode(t *testing.T) {
	s, _, reference := modeSkill(t, "rollback-skill", "original\n", 0o660)
	if res := whileScanBlocked(func() (*pyjson.Object, error) {
		return s.patchSkill(context.Background(), "rollback-skill", strp("original"), strp("blocked"), strp("references/example.md"), false)
	}); objSuccess(res) || readText(t, reference) != "original\n" || permOf(t, reference) != 0o660 {
		t.Fatalf("support patch rollback %v mode %o", res.Vals, permOf(t, reference))
	}

	s, skillMD, _ := modeSkill(t, "rollback-skill", "", 0)
	os.Chmod(skillMD, 0o660)
	if res := whileScanBlocked(func() (*pyjson.Object, error) {
		return s.editSkill(context.Background(), "rollback-skill", strings.Replace(improvementSkillContent, "Step 1: Do the thing.", "blocked edit", 1))
	}); objSuccess(res) || readText(t, skillMD) != improvementSkillContent || permOf(t, skillMD) != 0o660 {
		t.Fatalf("edit rollback %v mode %o", res.Vals, permOf(t, skillMD))
	}

	s, skillMD, _ = modeSkill(t, "rollback-skill", "", 0)
	os.Chmod(skillMD, 0o600)
	if res := whileScanBlocked(func() (*pyjson.Object, error) {
		return s.patchSkill(context.Background(), "rollback-skill", strp("Step 1: Do the thing."), strp("blocked patch"), nil, false)
	}); objSuccess(res) || readText(t, skillMD) != improvementSkillContent || permOf(t, skillMD) != 0o600 {
		t.Fatalf("patch rollback %v mode %o", res.Vals, permOf(t, skillMD))
	}

	s, _, reference = modeSkill(t, "rollback-skill", "original\n", 0o660)
	if res := whileScanBlocked(func() (*pyjson.Object, error) {
		return s.writeFile(context.Background(), "rollback-skill", "references/example.md", strp("blocked\n"))
	}); objSuccess(res) || readText(t, reference) != "original\n" || permOf(t, reference) != 0o660 {
		t.Fatalf("support write rollback %v mode %o", res.Vals, permOf(t, reference))
	}
}
