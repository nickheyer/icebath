package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Writes files into a fixture checkout and commits them, returning the new HEAD
func ucCommit(t *testing.T, root, message string, files map[string]string) string {
	t.Helper()
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ucGit(t, root, "add", ".")
	ucGit(t, root, "commit", "-qm", message)
	return ucGit(t, root, "rev-parse", "HEAD")
}

// Token of the pause on disk for the fixture checkout
func ucDiskToken(t *testing.T) map[string]any {
	t.Helper()
	record, err := updaterecord.ReadPauseRecord(updaterecord.PauseRecordPath())
	if err != nil || record == nil {
		t.Fatalf("pause record = %v, %v", record, err)
	}
	token, _ := record["token"].(map[string]any)
	return token
}

func TestCheckoutMoveBindsTheMoveBeforeGitWritesAndAdmitsAWholeMove(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(pm.PatchRepoRoot(root))
	ucPatchRoot(t, root)
	ucGit(t, root, "init", "-q", "-b", "main")
	ucGit(t, root, "config", "user.name", "Fixture")
	ucGit(t, root, "config", "user.email", "fixture@example.com")
	a := ucCommit(t, root, "A", map[string]string{"a.py": "A=1\n", "mine.py": "M=1\n"})
	target := ucCommit(t, root, "T", map[string]string{"a.py": "A=2\n"})
	ucGit(t, root, "reset", "-q", "--hard", a)
	ctx := context.Background()
	token, err := updaterecord.StampTree(ctx, map[string]any{"resume_needed": true, "profiles": map[string]any{"default": 999999}}, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := updaterecord.WritePause(ctx, token, nil, ""); err != nil {
		t.Fatal(err)
	}
	ran := false
	err = withCheckoutMove(ctx, movesFor(token), checkoutMoveBind{Targets: []string{target, "", target}}, func(step map[string]any) error {
		ran = true
		var bound map[string]any
		for _, item := range tokenBaselines(ucDiskToken(t)) {
			if baseline, ok := item.(map[string]any); ok && baseline["pre_sha"] == a {
				bound = baseline
			}
		}
		if bound == nil {
			t.Fatalf("no baseline at %s was durable before the move", a)
		}
		if !reflect.DeepEqual(bound["move_targets"], []any{target}) {
			t.Fatalf("move targets = %v", bound["move_targets"])
		}
		ucGit(t, root, "merge", "-q", "--ff-only", target)
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("move ran %v, err %v", ran, err)
	}
	whole, why, err := updaterecord.TreeIsWhole(ctx, ucDiskToken(t), root)
	if err != nil {
		t.Fatal(err)
	}
	if !whole {
		t.Fatalf("a move that landed whole held the paused set: %s", why)
	}
	unbound := false
	if err := withCheckoutMove(ctx, movesFor(nil), checkoutMoveBind{Targets: []string{target}}, func(map[string]any) error {
		unbound = true
		return nil
	}); err != nil || !unbound {
		t.Fatalf("a step with no pause ran %v, err %v", unbound, err)
	}
}
