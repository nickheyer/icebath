//go:build unix

package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Checkout at A with T fetched as origin main, returning root, A and T
func siRepo(t *testing.T) (string, string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ucGit(t, root, "init", "-q", "-b", "main")
	ucGit(t, root, "config", "user.name", "Fixture")
	ucGit(t, root, "config", "user.email", "fixture@example.com")
	ucPatchRoot(t, root)
	t.Cleanup(pm.PatchRepoRoot(root))
	a := ucCommit(t, root, "A", map[string]string{"a.py": "A=1\n", "mine.py": "M=1\n", "package-lock.json": "{}\n"})
	target := ucCommit(t, root, "T", map[string]string{"a.py": "A=2\n"})
	ucGit(t, root, "reset", "-q", "--hard", a)
	ucGit(t, root, "update-ref", "refs/remotes/origin/main", target)
	return root, a, target
}

// Git that on the match subcommand snapshots the record, runs real git, tears a file and interrupts the updater
func siInterruptingGit(t *testing.T, dir, match, tear string) []string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(dir, "git-interrupted")
	script := "#!/bin/sh\n" +
		fmt.Sprintf("case \" $* \" in *\" %s \"*) cp \"%s\" \"%s\"\n", match, updaterecord.PauseRecordPath(), filepath.Join(dir, "at-start.json")) +
		fmt.Sprintf("  \"%s\" \"$@\"\n", git) +
		fmt.Sprintf("  printf 'M=' > \"%s\"; kill -INT $PPID; sleep 5; exit 130;;\n", tear) +
		fmt.Sprintf("esac\nexec \"%s\" \"$@\"\n", git)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{wrapper}
}

// Context the interrupting git cancels the way the operator's interrupt reaches the updater
func siInterruptible(t *testing.T) context.Context {
	t.Helper()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	t.Cleanup(stop)
	return ctx
}

// Baseline at head in the record as it was on disk the moment git started
func siAtStart(t *testing.T, dir, head string) map[string]any {
	t.Helper()
	record, err := updaterecord.ReadPauseRecord(filepath.Join(dir, "at-start.json"))
	if err != nil || record == nil {
		t.Fatalf("record at start = %v, %v", record, err)
	}
	for _, item := range record["token"].(map[string]any)["baselines"].([]any) {
		if baseline := item.(map[string]any); baseline["pre_sha"] == head {
			return baseline
		}
	}
	t.Fatalf("no baseline at %s in the record at start", head)
	return nil
}

func siStrings(v any) []string {
	var out []string
	for _, item := range v.([]any) {
		out = append(out, item.(string))
	}
	return out
}

func TestAnInterruptedStashPushStaysHeld(t *testing.T) {
	root, a, _ := siRepo(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "mine.py"), []byte("M=2  # my edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	token := mhPause(t, root)
	ctx := siInterruptible(t)
	_, err := stashLocalChangesIfNeeded(ctx, io.Discard, siInterruptingGit(t, dir, "stash push", filepath.Join(root, "mine.py")), root,
		movesFor(token), nil, false)
	if !isKeyboardInterrupt(err) {
		t.Fatalf("the stash push was not interrupted: %v", err)
	}
	started := siAtStart(t, dir, a)
	if !slices.Contains(siStrings(started["move_paths"]), "mine.py") || !reflect.DeepEqual(started["dirty_at_pause"], []any{"mine.py"}) {
		t.Fatalf("baseline at start = %v", started)
	}
	if mhGate(t, root) {
		t.Fatal("a stash push interrupted mid-write was admitted")
	}
}

func TestAnInterruptedLockfileChurnCleanupStaysHeld(t *testing.T) {
	root, a, _ := siRepo(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package-lock.json"), []byte("{\"npm\": \"rewrote me\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	token := mhPause(t, root)
	ctx := siInterruptible(t)
	err := discardLockfileChurn(ctx, io.Discard, siInterruptingGit(t, dir, "checkout --", filepath.Join(root, "package-lock.json")), root,
		movesFor(token))
	if !isKeyboardInterrupt(err) {
		t.Fatalf("the lockfile cleanup was not interrupted: %v", err)
	}
	if paths := siAtStart(t, dir, a)["move_paths"]; !reflect.DeepEqual(paths, []any{"package-lock.json"}) {
		t.Fatalf("move paths at start = %v", paths)
	}
	if mhGate(t, root) {
		t.Fatal("a lockfile cleanup interrupted mid-write was admitted")
	}
}

func TestAStashRestoreAtTheMovedHeadIsGatedFromThatHead(t *testing.T) {
	for _, interrupted := range []bool{true, false} {
		t.Run(map[bool]string{true: "True", false: "False"}[interrupted], func(t *testing.T) {
			root, _, target := siRepo(t)
			dir := t.TempDir()
			mine := filepath.Join(root, "mine.py")
			if err := os.WriteFile(mine, []byte("M=2  # my edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			token := mhPause(t, root)
			moves := movesFor(token)
			ctx := siInterruptible(t)
			stash, err := stashLocalChangesIfNeeded(ctx, io.Discard, []string{"git"}, root, moves, nil, false)
			if err != nil || stash == nil {
				t.Fatalf("stash = %v, %v", stash, err)
			}
			if err := withCheckoutMove(ctx, moves, checkoutMoveBind{Targets: []string{target}}, func(map[string]any) error {
				ucGit(t, root, "merge", "-q", "--ff-only", target)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if interrupted {
				gitCmd := siInterruptingGit(t, dir, "stash apply", mine)
				if _, err := restoreStashedChanges(ctx, io.Discard, gitCmd, root, *stash, false, nil, moves); !isKeyboardInterrupt(err) {
					t.Fatalf("the stash restore was not interrupted: %v", err)
				}
				started := siAtStart(t, dir, target)
				if !reflect.DeepEqual(started["move_targets"], []any{*stash}) || !reflect.DeepEqual(started["dirty_at_pause"], []any{}) {
					t.Fatalf("baseline at start = %v", started)
				}
				if mhGate(t, root) {
					t.Fatal("a stash restore interrupted mid-write at the moved HEAD was admitted")
				}
				return
			}
			restored, err := restoreStashedChanges(ctx, io.Discard, []string{"git"}, root, *stash, false, nil, moves)
			if err != nil || !restored {
				t.Fatalf("restore = %v, %v", restored, err)
			}
			if got := mhRead(t, mine); got != "M=2  # my edit\n" {
				t.Fatalf("mine.py = %q", got)
			}
			if !mhGate(t, root) {
				t.Fatal("a stash restore that applied cleanly held the paused set")
			}
		})
	}
}
