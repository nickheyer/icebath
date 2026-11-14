package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Points the default store at path and stills the host wide holder scan for the test
func repairCmdStore(t *testing.T, path string) {
	t.Helper()
	installOwnershipDoubles(t)
	savedPath, savedLive := DefaultDBPath, repairLiveWriterFn
	DefaultDBPath = path
	repairLiveWriterFn = func(context.Context, string) (bool, error) { return false, nil }
	t.Cleanup(func() { DefaultDBPath, repairLiveWriterFn = savedPath, savedLive })
}

// Runs the repair command over a namespace and returns its exit code with the printed output
func runCmdRepair(t *testing.T, ns map[string]any) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code, err := CmdRepair(context.Background(), ns, &out)
	if err != nil {
		t.Fatalf("repair failed %v, output %s", err, out.String())
	}
	return code, out.String()
}

// Contents of the messages of a session read through a read only handle
func readOnlyContents(t *testing.T, db *SessionDB, sid string) []string {
	t.Helper()
	rows, err := db.GetMessages(context.Background(), sid, GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var contents []string
	for _, row := range rows {
		text, _ := rowValue(row, "content").(string)
		contents = append(contents, text)
	}
	return contents
}

func TestCheckOnlyReportsDriftWithoutMutation(t *testing.T) {
	path := legacyStore(t)
	repairCmdStore(t, path)
	before := fileBytes(t, path)
	if code, out := runCmdRepair(t, map[string]any{"check_only": true}); code != 1 || !strings.Contains(out, "does not open cleanly") {
		t.Fatalf("code %d output %s", code, out)
	}
	if !bytes.Equal(fileBytes(t, path), before) {
		t.Fatal("check only changed the store")
	}
}

func TestRefusedReconciliationPreservesOriginal(t *testing.T) {
	for _, blocker := range []string{"owner", "ddl", "incomplete", "backup"} {
		t.Run(blocker, func(t *testing.T) {
			path := legacyStore(t)
			repairCmdStore(t, path)
			before := fileBytes(t, path)
			switch blocker {
			case "owner":
				conflict := errors.New("profile home is reserved by another owner")
				ExclusiveMaintenance = func(ctx context.Context, homes []string, purpose string) (context.Context, func(), error) {
					return ctx, nil, conflict
				}
				IsOwnershipConflict = func(err error) bool { return errors.Is(err, conflict) }
			case "ddl":
				saved := reconcileApplyDeclaredSchema
				reconcileApplyDeclaredSchema = func(ctx context.Context, conn SQLiteConn) error {
					if _, err := conn.ExecContext(ctx, "CREATE TABLE only_in_scratch (id INTEGER)"); err != nil {
						return err
					}
					return operationalError("injected DDL refusal")
				}
				t.Cleanup(func() { reconcileApplyDeclaredSchema = saved })
			case "incomplete":
				saved := reconcileApplyDeclaredSchema
				reconcileApplyDeclaredSchema = func(context.Context, SQLiteConn) error { return nil }
				t.Cleanup(func() { reconcileApplyDeclaredSchema = saved })
			default:
				saved := repairBackupFn
				repairBackupFn = func(string) (string, string) { return "", "injected backup refusal" }
				t.Cleanup(func() { repairBackupFn = saved })
			}
			if code, out := runCmdRepair(t, map[string]any{"check_only": false, "no_backup": false}); code != 1 {
				t.Fatalf("code %d output %s", code, out)
			}
			if !bytes.Equal(fileBytes(t, path), before) {
				t.Fatal("a refused reconciliation changed the store")
			}
			if _, err := os.Lstat(path + ".reconcile-scratch"); err == nil {
				t.Fatal("the reconcile scratch file was left behind")
			}
		})
	}
}

func TestCanonicalStoreIsUnchanged(t *testing.T) {
	for _, withHistory := range []bool{true, false} {
		name := "empty"
		if withHistory {
			name = "history"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db := mustSessionDB(t, path, false)
			if withHistory {
				readSession(t, db, NewSession{ID: "normal", Source: "cli"})
				readAppend(t, db, "normal", map[string]any{"role": "user", "content": "normal"})
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			repairCmdStore(t, path)
			before := fileBytes(t, path)
			if code, out := runCmdRepair(t, map[string]any{"check_only": false}); code != 0 || !strings.Contains(out, "opens cleanly — no repair needed.") {
				t.Fatalf("code %d output %s", code, out)
			}
			if !bytes.Equal(fileBytes(t, path), before) {
				t.Fatal("repair changed a canonical store")
			}
		})
	}
}

func TestRepairRestoresMissingIndexedColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := mustSessionDB(t, path, false)
	readSession(t, db, NewSession{ID: "retained", Source: "cli"})
	readAppend(t, db, "retained", map[string]any{"role": "user", "content": "history survives"})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	repairCmdStore(t, path)
	conn := openRaw(t, path)
	execAll(t, conn, "DROP INDEX idx_async_delegations_delivery", "ALTER TABLE async_delegations DROP COLUMN completed_at")
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if code, out := runCmdRepair(t, map[string]any{"check_only": false, "no_backup": false}); code != 0 {
		t.Fatalf("code %d output %s", code, out)
	}
	reader := mustSessionDB(t, path, true)
	defer reader.Close()
	if got := readOnlyContents(t, reader, "retained"); len(got) != 1 || got[0] != "history survives" {
		t.Fatalf("messages %v", got)
	}
	row, err := reader.readOne(context.Background(), "SELECT name FROM sqlite_master WHERE name='idx_async_delegations_delivery'")
	if err != nil || row == nil {
		t.Fatalf("index row %v %v", row, err)
	}
}

func TestExplicitRepairRestoresReadonlyHistory(t *testing.T) {
	for _, backup := range []bool{true, false} {
		name := "no-backup"
		if backup {
			name = "backup"
		}
		t.Run(name, func(t *testing.T) {
			path := legacyStore(t)
			repairCmdStore(t, path)
			code, out := runCmdRepair(t, map[string]any{"check_only": false, "no_backup": !backup})
			if code != 0 || !strings.Contains(out, "✓ Repaired — 1 sessions recovered.") {
				t.Fatalf("code %d output %s", code, out)
			}
			backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), "state.db.malformed-backup-*"))
			if err != nil || (len(backups) > 0) != backup {
				t.Fatalf("backups %v %v, want any %v", backups, err, backup)
			}
			if strings.Contains(out, "  backup: ") != backup {
				t.Fatalf("backup line in output %s, want %v", out, backup)
			}
			reader := mustSessionDB(t, path, true)
			if got := readOnlyContents(t, reader, "retained"); len(got) != 1 || got[0] != "history survives" {
				t.Fatalf("messages %v", got)
			}
			if n, err := reader.SessionCount(context.Background(), SessionCountOptions{}); err != nil || n != 1 {
				t.Fatalf("session count %d %v", n, err)
			}
			row, err := reader.readOne(context.Background(), "SELECT version FROM schema_version")
			if version, _ := rowInt(row, "version"); err != nil || version != SchemaVersion {
				t.Fatalf("schema version %v %v", row, err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if code, out := runCmdRepair(t, map[string]any{"check_only": true}); code != 0 {
				t.Fatalf("second check code %d output %s", code, out)
			}
		})
	}
}

func TestCheckOnlyExitStatusTracksProbeVerdict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := mustSessionDB(t, path, false).Close(); err != nil {
		t.Fatal(err)
	}
	saved := DefaultDBPath
	DefaultDBPath = path
	t.Cleanup(func() { DefaultDBPath = saved })
	ns := map[string]any{"check_only": true, "no_backup": false}
	if code, out := runCmdRepair(t, ns); code != 0 {
		t.Fatalf("healthy store code %d output %s", code, out)
	}
	fh, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteAt(bytes.Repeat([]byte{0xff}, 4096-100), 100); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"-wal", "-shm"} {
		os.Remove(path + side)
	}
	if code, out := runCmdRepair(t, ns); code != 1 {
		t.Fatalf("damaged store code %d output %s", code, out)
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("check-only must not touch the file: %v %v", info, err)
	}
}
