//go:build unix

package selfupdate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Commits files named with __ as a directory separator and returns the new HEAD
func mhCommit(t *testing.T, root, message string, files map[string]string) string {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(root, filepath.FromSlash(strings.ReplaceAll(name, "__", "/")))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ucGit(t, root, "add", ".")
	ucGit(t, root, "commit", "-qm", message)
	return ucGit(t, root, "rev-parse", "HEAD")
}

// Parks the checkout on a merged feature at A with local and origin main at B rewriting locked/z.py
func mhParked(t *testing.T) (string, string, string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores read-only dirs")
	}
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
	a := mhCommit(t, root, "A", map[string]string{"a.py": "A=1\n", "locked__z.py": "Z=1\n", "package-lock.json": "{}\n"})
	ucGit(t, root, "branch", "-q", "feature")
	b := mhCommit(t, root, "B", map[string]string{"a.py": "A=2\n", "locked__z.py": "Z=2\n", "package-lock.json": "{\"v\": 2}\n"})
	ucGit(t, root, "update-ref", "refs/remotes/origin/main", b)
	ucGit(t, root, "checkout", "-q", "feature")
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "locked"), 0o755) })
	return root, a, b
}

// Stamps and durably records a paused set for root
func mhPause(t *testing.T, root string) map[string]any {
	t.Helper()
	ctx := context.Background()
	token, err := updaterecord.StampTree(ctx, map[string]any{"resume_needed": true, "profiles": map[string]any{"default": 999999}}, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := updaterecord.WritePause(ctx, token, nil, ""); err != nil {
		t.Fatal(err)
	}
	return token
}

// Verdict recovery reaches from the durable record as the next launch reads it
func mhGate(t *testing.T, root string) bool {
	t.Helper()
	record, err := updaterecord.ReadPauseRecord(updaterecord.PauseRecordPath())
	if err != nil || record == nil {
		t.Fatalf("pause record = %v, %v", record, err)
	}
	whole, _, err := updaterecord.TreeIsWhole(context.Background(), record["token"].(map[string]any), root)
	if err != nil {
		t.Fatal(err)
	}
	return whole
}

func mhRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestASwitchThatMovedHeadPastAnUnwrittenFileStaysHeld(t *testing.T) {
	for _, unlinkFails := range []bool{true, false} {
		t.Run(map[bool]string{true: "True", false: "False"}[unlinkFails], func(t *testing.T) {
			root, _, b := mhParked(t)
			locked := filepath.Join(root, "locked")
			token := mhPause(t, root)
			if unlinkFails {
				if err := os.Chmod(locked, 0o555); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := prepareCheckoutForUpdate(context.Background(), io.Discard, []string{"git"}, "main", "feature",
				prepareOptions{AssumeYes: true, WindowsGatewayResume: token}); err != nil {
				t.Fatal(err)
			}
			if head := ucGit(t, root, "rev-parse", "HEAD"); head != b {
				t.Fatalf("fixture: the switch did not move HEAD (%s)", head)
			}
			if got := mhRead(t, filepath.Join(root, "a.py")); got != "A=2\n" {
				t.Fatalf("a.py = %q", got)
			}
			if err := os.WriteFile(filepath.Join(root, "package-lock.json"), []byte("{\"v\": 2, \"churn\": 1}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if unlinkFails {
				if got := mhRead(t, filepath.Join(locked, "z.py")); got != "Z=1\n" {
					t.Fatalf("fixture: git wrote the file (%q)", got)
				}
				if mhGate(t, root) {
					t.Fatal("a moved HEAD whose tree is not its commit's was admitted")
				}
				if err := os.Chmod(locked, 0o755); err != nil {
					t.Fatal(err)
				}
				ucGit(t, root, "checkout", "--", "locked/z.py")
			}
			if !mhGate(t, root) {
				t.Fatal("a switch that left the tree whole held the paused set")
			}
		})
	}
}

func TestAMoveTheUpdaterDiedBeforeJudgingIsJudgedAtResume(t *testing.T) {
	root, _, b := mhParked(t)
	token := mhPause(t, root)
	if err := updaterecord.MarkMove(context.Background(), token, b); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "locked"), 0o555); err != nil {
		t.Fatal(err)
	}
	ucGit(t, root, "checkout", "-q", "main")
	if head := ucGit(t, root, "rev-parse", "HEAD"); head != b || mhRead(t, filepath.Join(root, "locked", "z.py")) != "Z=1\n" {
		t.Fatalf("fixture: HEAD %s or z.py was written", head)
	}
	if mhGate(t, root) {
		t.Fatal("an unjudged move to a recorded target was admitted torn")
	}
}
