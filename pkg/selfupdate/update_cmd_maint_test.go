package selfupdate

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/skills"
)

func maintWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func maintSQLite(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

// Realistic home tree of the backup cases under a temp user home, with the backup mode written to its config
func maintBackupHome(t *testing.T, mode string) string {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	maintWrite(t, filepath.Join(root, ".env"), "OPENROUTER_API_KEY=sk-test-123\n")
	for _, name := range []string{"memory_store.db", "hermes_state.db"} {
		maintSQLite(t, filepath.Join(root, name), "CREATE TABLE sample (value TEXT)", "INSERT INTO sample VALUES ('test')")
	}
	maintWrite(t, filepath.Join(root, "sessions", "abc123.json"), "{}")
	maintWrite(t, filepath.Join(root, "skills", "my-skill", "SKILL.md"), "# My Skill\n")
	maintWrite(t, filepath.Join(root, "skins", "cyber.yaml"), "name: cyber\n")
	maintWrite(t, filepath.Join(root, "cron", "jobs.json"), "[]")
	maintWrite(t, filepath.Join(root, "memories", "notes.json"), "{}")
	maintWrite(t, filepath.Join(root, "profiles", "coder", "config.yaml"), "model:\n  provider: anthropic\n")
	maintWrite(t, filepath.Join(root, "profiles", "coder", ".env"), "ANTHROPIC_API_KEY=sk-ant-123\n")
	maintWrite(t, filepath.Join(root, "icebath-agent", "run_agent.py"), "# big file\n")
	if err := os.MkdirAll(filepath.Join(root, "icebath-agent", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	maintWrite(t, filepath.Join(root, "config.yaml"), "_config_version: 22\nupdates:\n  pre_update_backup: "+mode+"\n")
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("HOME", tmp)
	return root
}

func maintZips(t *testing.T, root string) []string {
	t.Helper()
	zips, err := filepath.Glob(filepath.Join(root, "backups", "pre-update-*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	return zips
}

func maintSnaps(root string) []string {
	var dirs []string
	entries, _ := os.ReadDir(filepath.Join(root, "state-snapshots"))
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	return dirs
}

func TestRunPreUpdateBackupConfigOffDisablesEverythingSilently(t *testing.T) {
	root := maintBackupHome(t, "'off'")
	var out bytes.Buffer
	snapID := runPreUpdateBackup(context.Background(), &out, map[string]any{"no_backup": false, "backup": false})
	if snapID != nil {
		t.Fatalf("snapshot %q", *snapID)
	}
	if out.String() != "" {
		t.Fatalf("output %q", out.String())
	}
	if len(maintSnaps(root)) != 0 || len(maintZips(t, root)) != 0 {
		t.Fatalf("snaps %v zips %v", maintSnaps(root), maintZips(t, root))
	}
}

func TestRunPreUpdateBackupConfigFullMode(t *testing.T) {
	root := maintBackupHome(t, "full")
	var out bytes.Buffer
	snapID := runPreUpdateBackup(context.Background(), &out, map[string]any{"no_backup": false, "backup": false})
	if snapID == nil {
		t.Fatalf("no snapshot, output %q", out.String())
	}
	if zips := maintZips(t, root); len(zips) != 1 {
		t.Fatalf("zips %v output %q", zips, out.String())
	}
}

// Home of the integrity guard cases with a real state.db
func maintGuardHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	maintWrite(t, filepath.Join(root, "config.yaml"), "model:\n  provider: openrouter\n")
	maintSQLite(t, filepath.Join(root, "state.db"), "CREATE TABLE sessions (id INTEGER PRIMARY KEY)")
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("HOME", tmp)
	return root
}

func TestPreUpdateBackupIntegrityGuardHealthyDbStaysQuiet(t *testing.T) {
	maintGuardHome(t)
	var out bytes.Buffer
	snapID := runPreUpdateBackup(context.Background(), &out, map[string]any{"no_backup": false, "backup": false})
	if snapID == nil {
		t.Fatalf("no snapshot, output %q", out.String())
	}
	if !strings.Contains(out.String(), "Pre-update snapshot") || strings.Contains(out.String(), "integrity check FAILED") {
		t.Fatalf("output %q", out.String())
	}
}

func TestPreUpdateBackupIntegrityGuardZeroedDbAfterSnapshotIsLoud(t *testing.T) {
	root := maintGuardHome(t)
	real := maintCreateQuickSnapshot
	t.Cleanup(func() { maintCreateQuickSnapshot = real })
	maintCreateQuickSnapshot = func(label *string, h string, keep *int, maxFileSize *int64) (string, error) {
		snapID, err := real(label, h, keep, maxFileSize)
		live := filepath.Join(root, "state.db")
		info, statErr := os.Stat(live)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if writeErr := os.WriteFile(live, make([]byte, info.Size()), 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
		return snapID, err
	}
	var out bytes.Buffer
	snapID := runPreUpdateBackup(context.Background(), &out, map[string]any{"no_backup": false, "backup": false})
	if snapID == nil {
		t.Fatalf("no snapshot, output %q", out.String())
	}
	if !strings.Contains(out.String(), "integrity check FAILED") || !strings.Contains(out.String(), "Snapshot copy is valid") {
		t.Fatalf("output %q", out.String())
	}
}

func TestPreUpdateBackupIntegrityGuardFailedSnapshotIsLoudAndUpdateContinues(t *testing.T) {
	maintGuardHome(t)
	real := maintCreateQuickSnapshot
	t.Cleanup(func() { maintCreateQuickSnapshot = real })
	maintCreateQuickSnapshot = func(*string, string, *int, *int64) (string, error) {
		return "", &pyPermissionError{"state-snapshots is read-only"}
	}
	var out bytes.Buffer
	snapID := runPreUpdateBackup(context.Background(), &out, map[string]any{"no_backup": false, "backup": false})
	if snapID != nil {
		t.Fatalf("snapshot %q", *snapID)
	}
	for _, want := range []string{"Pre-update snapshot FAILED", "state-snapshots is read-only", "Continuing with update"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q lacks %q", out.String(), want)
		}
	}
}

// PermissionError raised with a bare message, the failure the snapshot stand-in reports
type pyPermissionError struct{ msg string }

func (e *pyPermissionError) Error() string { return e.msg }

func (e *pyPermissionError) Is(target error) bool { return target == fs.ErrPermission }

func TestUpdateCopiesBundledSkillBytesToDefaultActiveAndSibling(t *testing.T) {
	bundled := skills.BundledFS()
	rel := ""
	_ = fs.WalkDir(bundled, ".", func(path string, entry fs.DirEntry, err error) error {
		if err == nil && rel == "" && !entry.IsDir() && entry.Name() == "SKILL.md" {
			rel = path
		}
		return nil
	})
	if rel == "" {
		t.Fatal("premise: no bundled SKILL.md")
	}
	want, err := fs.ReadFile(bundled, rel)
	if err != nil {
		t.Fatal(err)
	}
	for _, named := range []bool{false, true} {
		id := "False"
		if named {
			id = "True"
		}
		t.Run(id, func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, ".icebath")
			homes := []string{root}
			if named {
				homes = append(homes, filepath.Join(root, "profiles", "active"), filepath.Join(root, "profiles", "sibling"))
			}
			for _, profile := range homes {
				maintWrite(t, filepath.Join(profile, "config.yaml"), "{}\n")
			}
			t.Setenv("HOME", tmp)
			t.Setenv("ICEBATH_BUNDLED_SKILLS", "")
			active := root
			if named {
				active = homes[1]
			}
			t.Setenv("ICEBATH_HOME", active)
			var out bytes.Buffer
			if err := syncProfilesAfterUpdate(context.Background(), &out); err != nil {
				t.Fatal(err)
			}
			for _, profile := range homes {
				got, err := os.ReadFile(filepath.Join(profile, "skills", filepath.FromSlash(rel)))
				if err != nil && errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("%s lacks the bundled skill, output %q", profile, out.String())
				}
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s holds other bytes (%v)", profile, err)
				}
			}
		})
	}
}
