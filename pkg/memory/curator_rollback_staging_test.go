package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func curPermErr(msg string) error { return &os.PathError{Op: "move", Path: msg, Err: os.ErrPermission} }

func TestCuratorFailedRollbackKeepsUnrestoredMetadata(t *testing.T) {
	for _, failure := range []string{"staging", "carry"} {
		for _, kind := range []string{"directory", "pointer"} {
			t.Run(failure+"-"+kind, func(t *testing.T) {
				ctx, dir := curBackupHome(t, nil)
				for _, n := range []string{"alpha", "beta"} {
					curWrite(t, filepath.Join(dir, n, "SKILL.md"), "# "+n+"\nSnapshot content\n")
				}
				target := curSnap(t, ctx, "manual")
				expected := map[string]string{}
				rel := func(n string) string {
					if kind == "directory" {
						return filepath.Join(n, ".git", "local-history")
					}
					return filepath.Join(n, ".git")
				}
				for _, n := range []string{"alpha", "beta"} {
					expected[n] = "unpublished metadata for " + n
					curWrite(t, filepath.Join(dir, rel(n)), expected[n])
					curWrite(t, filepath.Join(dir, n, "SKILL.md"), "Current content\n")
				}
				real := curatorMove
				var staged []string
				curatorMove = func(src, dst string) (string, error) {
					if failure == "carry" && filepath.Base(src) == ".git" {
						return "", curPermErr("injected metadata move failure")
					}
					if failure == "staging" {
						if filepath.Dir(src) == dir {
							if len(staged) > 0 {
								return "", curPermErr("injected staging failure")
							}
							res, err := real(src, dst)
							staged = append(staged, filepath.Base(src))
							return res, err
						}
						if strings.HasPrefix(filepath.Base(filepath.Dir(src)), ".rollback-staging-") {
							return "", curPermErr("injected recovery move failure")
						}
					}
					return real(src, dst)
				}
				t.Cleanup(func() { curatorMove = real })
				ok, msg, restored := RollbackCuratorSnapshot(ctx, filepath.Base(target))
				if ok || restored != "" {
					t.Fatalf("rollback %v %q %q", ok, msg, restored)
				}
				staging, _ := filepath.Glob(filepath.Join(dir, ".curator_backups", ".rollback-*"))
				if len(staging) != 1 || !strings.Contains(msg, staging[0]) {
					t.Fatalf("staging %v message %q", staging, msg)
				}
				if _, ok, err := SnapshotSkills(ctx, "manual", nil); !ok || err != nil {
					t.Fatalf("next snapshot failed %v", err)
				}
				for n, content := range expected {
					found := false
					for _, root := range []string{dir, staging[0]} {
						if b, err := os.ReadFile(filepath.Join(root, rel(n))); err == nil && string(b) == content {
							found = true
						}
					}
					if !found {
						t.Fatalf("%s metadata lost", n)
					}
				}
			})
		}
	}
}

func TestCuratorRetainStagingNeverRaises(t *testing.T) {
	for _, mode := range []string{"rename_fails", "collision"} {
		t.Run(mode, func(t *testing.T) {
			ctx, dir := curBackupHome(t, nil)
			curWrite(t, filepath.Join(dir, "alpha", "SKILL.md"), "snap\n")
			target := curSnap(t, ctx, "manual")
			curWrite(t, filepath.Join(dir, "alpha", ".git"), "meta")
			realMove, realRename := curatorMove, curatorRename
			curatorMove = func(src, dst string) (string, error) {
				if filepath.Base(src) == ".git" {
					return "", curPermErr("injected carry failure")
				}
				return realMove(src, dst)
			}
			t.Cleanup(func() { curatorMove, curatorRename = realMove, realRename })
			backups := filepath.Join(dir, ".curator_backups")
			if mode == "rename_fails" {
				curatorRename = func(string, string) error { return &os.LinkError{Op: "rename", Err: fmt.Errorf("injected rename")} }
			} else {
				curatorUTCIDNow = func(time.Time) string { return "2020-01-01T00-00-00Z" }
				if err := os.MkdirAll(filepath.Join(backups, ".rollback-unrestored-2020-01-01T00-00-00Z"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			ok, msg, restored := RollbackCuratorSnapshot(ctx, filepath.Base(target))
			if ok || restored != "" || !strings.Contains(msg, "could not restore alpha/.git") {
				t.Fatalf("rollback %v %q", ok, msg)
			}
			if mode == "rename_fails" {
				if !strings.Contains(msg, "rename failed") || !strings.Contains(msg, ".rollback-staging-") {
					t.Fatalf("message %q", msg)
				}
				return
			}
			kept := filepath.Join(backups, ".rollback-unrestored-2020-01-01T00-00-00Z-1")
			if st, err := os.Stat(kept); err != nil || !st.IsDir() || !strings.Contains(msg, kept) {
				t.Fatalf("kept %s message %q", kept, msg)
			}
		})
	}
}
