package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyzip"
)

// Home tree for the automatic backup cases
func autoHome(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	makeHomeTree(t, h)
	return tmp, h
}

// Makes ZipFile.write finalize a large partial member for one arcname, then surface a source read failure
func failZipWriteAfter(t *testing.T, member string, byteCount int) {
	t.Helper()
	real := zipWrite
	swap(t, &zipWrite, func(zf *pyzip.ZipFile, path, arcname string) error {
		if arcname != member {
			return real(zf, path, arcname)
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		dst, err := zf.Open(arcname)
		if err != nil {
			return err
		}
		buf := make([]byte, byteCount)
		n, _ := io.ReadFull(src, buf)
		dst.Write(buf[:n])
		if err := dst.Close(); err != nil {
			return err
		}
		return &os.PathError{Op: "read", Path: path, Err: syscall.EIO}
	})
}

// Writes random bytes to a file
func randomFile(t *testing.T, path string, n int) {
	t.Helper()
	b := make([]byte, n)
	rand.Read(b)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFailedZipMemberRecoveryAutomaticBackupOmitsCrcValidPartialMember(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	writeTree(t, h, map[string]string{"config.yaml": "model: test\n"})
	randomFile(t, filepath.Join(h, "flaky.bin"), 300_000)
	archive := filepath.Join(tmp, "automatic.zip")
	failZipWriteAfter(t, "flaky.bin", 120_000)
	if got, err := writeFullZipBackup(context.Background(), archive, h); got != "" || err != nil {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := os.Stat(archive); err == nil {
		t.Fatal("the incomplete archive was published at the final path")
	}
	salvage := filepath.Join(tmp, "automatic.incomplete.zip")
	raw, err := os.ReadFile(salvage)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("flaky.bin")) {
		t.Fatal("the dropped member's bytes remain")
	}
	bodies := zipBodies(t, salvage)
	if _, ok := bodies["flaky.bin"]; ok || string(bodies["config.yaml"]) != "model: test\n" {
		t.Fatalf("members %v", bodies)
	}
}

func TestFailedZipMemberRecoveryIncompletePreUpdateBackupDoesNotRotateLastComplete(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	writeTree(t, h, map[string]string{"config.yaml": "model: test\n"})
	var clock time.Duration
	good, err := CreatePreUpdateBackup(h, 2)
	if err != nil || good == "" || !isFile(good) {
		t.Fatalf("%q %v", good, err)
	}
	real := zipWrite
	randomFile(t, filepath.Join(h, "flaky.bin"), 300_000)
	failZipWriteAfter(t, "flaky.bin", 120_000)
	for range 2 {
		advanceBackupClock(t, &clock)
		if got, err := CreatePreUpdateBackup(h, 2); got != "" || err != nil {
			t.Fatalf("%q %v", got, err)
		}
	}
	if !isFile(good) {
		t.Fatal("an incomplete generation rotated out the last complete backup")
	}
	backupDir := filepath.Join(h, "backups")
	salvages, _ := filepath.Glob(filepath.Join(backupDir, "pre-update-*.incomplete.zip"))
	if len(salvages) != 1 {
		t.Fatalf("repeated incomplete runs must not pile up: %v", salvages)
	}
	bodies := zipBodies(t, salvages[0])
	if _, ok := bodies["flaky.bin"]; ok || string(bodies["config.yaml"]) != "model: test\n" {
		t.Fatalf("members %v", bodies)
	}
	zipWrite = real
	os.Remove(filepath.Join(h, "flaky.bin"))
	advanceBackupClock(t, &clock)
	newest, err := CreatePreUpdateBackup(h, 2)
	if err != nil || newest == "" || !isFile(newest) {
		t.Fatalf("%q %v", newest, err)
	}
	if !isFile(good) {
		t.Fatal("a complete run pruned complete backups in favour of salvage")
	}
	if salvages, _ := filepath.Glob(filepath.Join(backupDir, "pre-update-*.incomplete.zip")); len(salvages) != 1 {
		t.Fatalf("salvages %v", salvages)
	}
}

func TestPreUpdateBackupBackupContentsMatchFullBackup(t *testing.T) {
	_, h := autoHome(t)
	out, err := CreatePreUpdateBackup(h, PreUpdateDefaultKeep)
	if err != nil || out == "" {
		t.Fatalf("%q %v", out, err)
	}
	names := zipNames(t, out)
	for _, want := range []string{"config.yaml", ".env", "sessions/abc123.json", "skills/my-skill/SKILL.md", "profiles/coder/config.yaml"} {
		if !slices.Contains(names, want) {
			t.Fatalf("%s missing from %v", want, names)
		}
	}
	for _, n := range names {
		if strings.HasPrefix(n, "icebath-agent/") || strings.Contains(n, "__pycache__") || n == "gateway.pid" {
			t.Fatalf("archived %s", n)
		}
	}
}

func TestPreUpdateBackupPreUpdateZipDoesNotNestThePreUpdateSnapshot(t *testing.T) {
	_, h := autoHome(t)
	c := testConn(t, filepath.Join(h, "state.db"), "")
	mustExec(t, c, "CREATE TABLE sessions (id TEXT PRIMARY KEY)")
	closeQuietly(c)
	snapID, err := CreateQuickSnapshot(ptr("pre-update"), h, nil, nil)
	if err != nil || snapID == "" || !isFile(filepath.Join(h, quickSnapshotsDir, snapID, "state.db")) {
		t.Fatalf("%q %v", snapID, err)
	}
	out, err := CreatePreUpdateBackup(h, PreUpdateDefaultKeep)
	if err != nil || out == "" {
		t.Fatalf("%q %v", out, err)
	}
	names := zipNames(t, out)
	if !slices.Contains(names, "state.db") {
		t.Fatalf("names %v", names)
	}
	for _, n := range names {
		if strings.HasPrefix(n, quickSnapshotsDir+"/") {
			t.Fatalf("nested %v", names)
		}
	}
}

func TestPreUpdateBackupRotationKeepsOnlyN(t *testing.T) {
	_, h := autoHome(t)
	var clock time.Duration
	var created []string
	for range 5 {
		out, err := CreatePreUpdateBackup(h, 3)
		if err != nil || out == "" {
			t.Fatalf("%q %v", out, err)
		}
		created = append(created, filepath.Base(out))
		advanceBackupClock(t, &clock)
	}
	entries, _ := os.ReadDir(filepath.Join(h, "backups"))
	var remaining []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pre-update-") {
			remaining = append(remaining, e.Name())
		}
	}
	sort.Strings(remaining)
	if len(remaining) != 3 || slices.Contains(remaining, created[0]) || slices.Contains(remaining, created[1]) || !slices.Contains(remaining, created[4]) {
		t.Fatalf("remaining %v created %v", remaining, created)
	}
}

func TestPreUpdateBackupSkipsSymlinkedFiles(t *testing.T) {
	tmp, h := autoHome(t)
	outside := filepath.Join(tmp, "outside-secret.txt")
	os.WriteFile(outside, []byte("outside secret\n"), 0o644)
	if err := os.Symlink(outside, filepath.Join(h, "skills", "outside-link.txt")); err != nil {
		t.Skipf("symlinks unavailable in test environment: %v", err)
	}
	out, err := CreatePreUpdateBackup(h, PreUpdateDefaultKeep)
	if err != nil || out == "" {
		t.Fatalf("%q %v", out, err)
	}
	for name, b := range zipBodies(t, out) {
		if name == "skills/outside-link.txt" || string(b) == "outside secret\n" {
			t.Fatalf("archived %s", name)
		}
	}
}

func TestPreMigrationBackupRestorableWithHermesImport(t *testing.T) {
	_, h := autoHome(t)
	out, err := CreatePreMigrationBackup(h, PreMigrationDefaultKeep)
	if err != nil || out == "" {
		t.Fatalf("%q %v", out, err)
	}
	zr, err := pyzip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if valid, reason := validateBackupZip(zr); !valid {
		t.Fatalf("pre-migration zip failed _validate_backup_zip: %s", reason)
	}
}

func TestPreMigrationBackupDoesNotTouchPreUpdateBackups(t *testing.T) {
	_, h := autoHome(t)
	var clock time.Duration
	update, err := CreatePreUpdateBackup(h, 5)
	if err != nil || update == "" || !isFile(update) {
		t.Fatalf("%q %v", update, err)
	}
	for range 3 {
		out, err := CreatePreMigrationBackup(h, 1)
		if err != nil || out == "" {
			t.Fatalf("%q %v", out, err)
		}
		advanceBackupClock(t, &clock)
	}
	if !isFile(update) {
		t.Fatal("pre-migration rotation wrongly pruned the pre-update backup")
	}
	migrations, _ := filepath.Glob(filepath.Join(h, "backups", "pre-migration-*.zip"))
	if len(migrations) != 1 {
		t.Fatalf("migrations %v", migrations)
	}
}

func TestPreMigrationBackupDefaultKeepRetainsFive(t *testing.T) {
	_, h := autoHome(t)
	var clock time.Duration
	for range 6 {
		if out, err := CreatePreMigrationBackup(h, PreMigrationDefaultKeep); err != nil || out == "" {
			t.Fatalf("%q %v", out, err)
		}
		advanceBackupClock(t, &clock)
	}
	migrations, _ := filepath.Glob(filepath.Join(h, "backups", "pre-migration-*.zip"))
	if len(migrations) != 5 {
		t.Fatalf("migrations %v", migrations)
	}
}

func TestFailedAutomaticBackupPreservesPreviousArchive(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	writeTree(t, h, map[string]string{"state.db": "not-a-database"})
	archive := filepath.Join(tmp, "automatic.zip")
	os.WriteFile(archive, []byte("previous-valid-backup"), 0o644)
	swap(t, &copySQLite, func(context.Context, string, string) (bool, error) { return false, nil })
	if got, err := writeFullZipBackup(context.Background(), archive, h); got != "" || err != nil {
		t.Fatalf("%q %v", got, err)
	}
	if b, _ := os.ReadFile(archive); string(b) != "previous-valid-backup" {
		t.Fatalf("archive %q", b)
	}
	if partials, _ := filepath.Glob(filepath.Join(tmp, ".*.partial")); len(partials) != 0 {
		t.Fatalf("partials %v", partials)
	}
}

func TestAutomaticBackupSkipsWhileTheSlotIsHeld(t *testing.T) {
	tmp, h := autoHome(t)
	err := backupOperationLock(h, 0.25, func() error {
		got, err := writeFullZipBackup(context.Background(), filepath.Join(tmp, "a.zip"), h)
		if got != "" || err != nil {
			t.Fatalf("%q %v", got, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrefixedBackupOfAMissingHomeIsNone(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if got, err := CreatePreUpdateBackup(missing, 5); got != "" || err != nil {
		t.Fatalf("%q %v", got, err)
	}
}

func TestPruneOldestFollowsPythonSlicesForNegativeKeep(t *testing.T) {
	var removed []string
	remove := func(p string) error { removed = append(removed, p); return nil }
	if n := pruneOldest([]string{"c", "b", "a"}, -1, remove, "x"); n != 1 || !slices.Equal(removed, []string{"a"}) {
		t.Fatalf("%d %v", n, removed)
	}
	removed = nil
	if n := pruneOldest([]string{"c", "b", "a"}, 5, remove, "x"); n != 0 || len(removed) != 0 {
		t.Fatalf("%d %v", n, removed)
	}
	failing := func(p string) error { return &os.PathError{Op: "remove", Path: p, Err: syscall.EACCES} }
	if n := pruneOldest([]string{"c", "b", "a"}, 1, failing, "x"); n != 0 {
		t.Fatalf("%d", n)
	}
}
