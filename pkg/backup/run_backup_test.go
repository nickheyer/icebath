package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyzip"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Temp root holding a .icebath home bound as the process home, the user home at the temp root
func backupHome(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, h)
	t.Cleanup(home.PatchUserHome(tmp))
	home.ResetKeyCache()
	return tmp, h
}

// Runs icebath backup with the namespace and returns its result and printed text
func runBackupNS(t *testing.T, ns map[string]any) (bool, string, error) {
	t.Helper()
	var out bytes.Buffer
	ctx := context.Background()
	ok, err := RunBackup(ctx, home.Dir(ctx), ns, &out)
	return ok, out.String(), err
}

// Members of a finished archive in central directory order
func zipNames(t *testing.T, path string) []string {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	return names
}

// Every member body of an archive, failing on the first member whose CRC does not check like testzip
func zipBodies(t *testing.T, path string) map[string][]byte {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := map[string][]byte{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("testzip %s: %v", f.Name, err)
		}
		out[f.Name] = b
	}
	return out
}

// Member name to CRC of an archive
func zipCRCs(t *testing.T, path string) map[string]uint32 {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := map[string]uint32{}
	for _, f := range r.File {
		out[f.Name] = f.CRC32
	}
	return out
}

// Replaces a package seam until the test ends
func swap[T any](t *testing.T, seam *T, v T) {
	t.Helper()
	old := *seam
	*seam = v
	t.Cleanup(func() { *seam = old })
}

// Skews the backup clock forward instead of sleeping between timestamped ids
func advanceBackupClock(t *testing.T, offset *time.Duration) {
	t.Helper()
	if *offset == 0 {
		swap(t, &backupNow, func() time.Time { return time.Now().Add(*offset) })
	}
	*offset += 1100 * time.Millisecond
}

// Rows of a query on a database file, columns joined by a bar
func tableRows(t *testing.T, path, q string) []string {
	t.Helper()
	c := testConn(t, path, "")
	defer closeQuietly(c)
	rows, err := c.c.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := []string{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out
}

func TestBackupDbSnapshotsStagedBesideOutputZip(t *testing.T) {
	tmp, h := backupHome(t)
	makeHomeTree(t, h)
	outDir := filepath.Join(tmp, "external-drive")
	os.Mkdir(outDir, 0o755)
	var staged []string
	real := stageSQLiteSnapshot
	swap(t, &stageSQLiteSnapshot, func(dir string) (string, error) {
		staged = append(staged, dir)
		return real(dir)
	})
	runBackupNS(t, map[string]any{"output": filepath.Join(outDir, "backup.zip")})
	if len(staged) == 0 {
		t.Fatal("no SQLite snapshot was staged")
	}
	for _, d := range staged {
		if d != outDir {
			t.Fatalf("staged %v", staged)
		}
	}
}

func TestBackupPreUpdateDbSnapshotsStagedBesideOutputZip(t *testing.T) {
	_, h := backupHome(t)
	makeHomeTree(t, h)
	outZip := filepath.Join(h, "backups", "pre-update-test.zip")
	os.MkdirAll(filepath.Dir(outZip), 0o755)
	var staged []string
	real := stageSQLiteSnapshot
	swap(t, &stageSQLiteSnapshot, func(dir string) (string, error) {
		staged = append(staged, dir)
		return real(dir)
	})
	result, err := writeFullZipBackup(context.Background(), outZip, h)
	if err != nil || result == "" {
		t.Fatalf("result %q %v", result, err)
	}
	if len(staged) == 0 {
		t.Fatal("no SQLite snapshot was staged")
	}
	for _, d := range staged {
		if d != filepath.Dir(outZip) {
			t.Fatalf("staged %v", staged)
		}
	}
}

func TestBackupSkipsSymlinkedFiles(t *testing.T) {
	tmp, h := backupHome(t)
	makeHomeTree(t, h)
	outside := filepath.Join(tmp, "outside-secret.txt")
	os.WriteFile(outside, []byte("outside secret\n"), 0o644)
	if err := os.Symlink(outside, filepath.Join(h, "skills", "outside-link.txt")); err != nil {
		t.Skipf("symlinks unavailable in test environment: %v", err)
	}
	outZip := filepath.Join(tmp, "backup.zip")
	runBackupNS(t, map[string]any{"output": outZip})
	bodies := zipBodies(t, outZip)
	if _, ok := bodies["skills/outside-link.txt"]; ok {
		t.Fatal("the symlink was archived")
	}
	for name, b := range bodies {
		if string(b) == "outside secret\n" {
			t.Fatalf("%s carries the outside file", name)
		}
	}
}

func TestBackupStateSnapshotsNotNestedIntoBackup(t *testing.T) {
	tmp, h := backupHome(t)
	makeHomeTree(t, h)
	c := testConn(t, filepath.Join(h, "state.db"), "")
	mustExec(t, c, "CREATE TABLE sessions (id TEXT PRIMARY KEY)", "INSERT INTO sessions VALUES ('s1')")
	closeQuietly(c)
	snapID, err := CreateQuickSnapshot(nil, h, nil, nil)
	if err != nil || snapID == "" {
		t.Fatalf("snapshot %q %v", snapID, err)
	}
	if _, err := os.Stat(filepath.Join(h, quickSnapshotsDir, snapID, "state.db")); err != nil {
		t.Fatal(err)
	}
	outZip := filepath.Join(tmp, "backup.zip")
	runBackupNS(t, map[string]any{"output": outZip})
	names := zipNames(t, outZip)
	var dbs []string
	for _, n := range names {
		if strings.HasPrefix(n, quickSnapshotsDir+"/") {
			t.Fatalf("nested snapshot %v", names)
		}
		if n == "state.db" || strings.HasSuffix(n, "/state.db") {
			dbs = append(dbs, n)
		}
	}
	if !slices.Equal(dbs, []string{"state.db"}) {
		t.Fatalf("state.db members %v", dbs)
	}
}

func TestBackupEdgeCasesIncompleteArchiveIsKeptButReportedAsFailure(t *testing.T) {
	tmp, h := backupHome(t)
	makeHomeTree(t, h)
	unreadable := filepath.Join(h, "skills", "locked.md")
	os.WriteFile(unreadable, []byte("secret\n"), 0o644)
	os.Chmod(unreadable, 0)
	if f, err := os.Open(unreadable); err == nil {
		f.Close()
		t.Skip("running as root: chmod 0 does not make the file unreadable")
	}
	outDir := filepath.Join(tmp, "b")
	os.Mkdir(outDir, 0o755)
	goodOld := filepath.Join(outDir, runBackupPrefix+"old.zip")
	os.WriteFile(goodOld, []byte("PK"), 0o644)
	outZip := filepath.Join(outDir, runBackupPrefix+"new.zip")
	ok, out, err := runBackupNS(t, map[string]any{"output": outZip, "keep": 1})
	if ok || err != nil {
		t.Fatalf("result %v %v", ok, err)
	}
	if _, err := os.Stat(outZip); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(goodOld); err != nil {
		t.Fatal("an incomplete run must not rotate the last complete backup out")
	}
	if !strings.Contains(out, "Archive kept, but 1 file(s) could not be added:") || !strings.Contains(out, "Permission denied") {
		t.Fatalf("output %q", out)
	}
	ok, _, err = runBackupNS(t, map[string]any{"output": filepath.Join(tmp, "out2.zip"), "quick": false})
	if ok || err != nil {
		t.Fatalf("second incomplete run %v %v", ok, err)
	}
	os.Chmod(unreadable, 0o600)
	if ok, _, err := runBackupNS(t, map[string]any{"output": filepath.Join(tmp, "out4.zip")}); !ok || err != nil {
		t.Fatalf("readable run %v %v", ok, err)
	}
}

func TestBackupEdgeCasesEmptyHermesHome(t *testing.T) {
	tmp, h := backupHome(t)
	writeTree(t, h, map[string]string{"__pycache__/foo.pyc": "\x00"})
	ok, out, err := runBackupNS(t, map[string]any{"output": filepath.Join(tmp, "out.zip")})
	if !ok || err != nil || !strings.Contains(out, "No files to back up.") {
		t.Fatalf("result %v %v %q", ok, err, out)
	}
	if _, err := os.Stat(filepath.Join(tmp, "out.zip")); err == nil {
		t.Fatal("a zip was created")
	}
}

func TestBackupEdgeCasesPre1980TimestampSkipped(t *testing.T) {
	tmp, h := backupHome(t)
	writeTree(t, h, map[string]string{"config.yaml": "model: test\n", "ancient.txt": "old data"})
	os.Chtimes(filepath.Join(h, "ancient.txt"), time.Unix(0, 0), time.Unix(0, 0))
	outZip := filepath.Join(tmp, "out.zip")
	ok, out, err := runBackupNS(t, map[string]any{"output": outZip})
	if ok || err != nil || !strings.Contains(out, "ancient.txt: ZIP does not support timestamps before 1980") {
		t.Fatalf("result %v %v %q", ok, err, out)
	}
	names := zipNames(t, outZip)
	if !slices.Contains(names, "config.yaml") || slices.Contains(names, "ancient.txt") {
		t.Fatalf("names %v", names)
	}
}

func TestMemoryProviderExternalPathsBackupSkipsExternalPathsOutsideHome(t *testing.T) {
	tmp, h := backupHome(t)
	writeTree(t, h, map[string]string{"config.yaml": "model:\n  provider: openrouter\n", ".env": "OPENROUTER_API_KEY=sk-test\n", "state.db": "x"})
	outside := filepath.Join(filepath.Dir(tmp), "outside-home-secret")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "leak.json"), []byte(`{"secret":1}`), 0o644)
	t.Cleanup(func() { os.RemoveAll(outside) })
	t.Cleanup(SwapMemoryProviderExternalPaths(func(context.Context, string) []string { return []string{outside} }))
	outZip := filepath.Join(tmp, "backup.zip")
	_, out, err := runBackupNS(t, map[string]any{"output": outZip})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range zipNames(t, outZip) {
		if strings.HasPrefix(n, "_external/") || strings.Contains(n, "leak.json") {
			t.Fatalf("archived %s", n)
		}
	}
	if !strings.Contains(out, "Skipped 1 memory-provider path(s) outside your home directory (not portable):\n    "+outside) {
		t.Fatalf("output %q", out)
	}
	os.Remove(filepath.Join(outside, "leak.json"))
	os.Remove(outside)
}

func TestMemoryProviderExternalFilesInsideHomeArchiveUnderTheExternalPrefix(t *testing.T) {
	tmp, h := backupHome(t)
	writeTree(t, h, map[string]string{"config.yaml": "model: {}\n"})
	writeTree(t, tmp, map[string]string{".honcho/state.json": "{}", ".honcho/__pycache__/x.pyc": "\x00", ".honcho/run.pid": "1", ".honcho/gateway.pid": "1"})
	t.Cleanup(SwapMemoryProviderExternalPaths(func(context.Context, string) []string { return []string{filepath.Join(tmp, ".honcho")} }))
	outZip := filepath.Join(tmp, "backup.zip")
	ok, out, err := runBackupNS(t, map[string]any{"output": outZip})
	if !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	names := zipNames(t, outZip)
	sort.Strings(names)
	if !slices.Equal(names, []string{"_external/.honcho/run.pid", "_external/.honcho/state.json", "config.yaml"}) {
		t.Fatalf("names %v", names)
	}
	if !strings.Contains(out, "Included 2 memory-provider file(s) stored outside ~/.icebath.") {
		t.Fatalf("output %q", out)
	}
}

func TestCollectMemoryProviderExternalPathsReadsNoProviderFromAnUnsetConfig(t *testing.T) {
	_, h := backupHome(t)
	writeTree(t, h, map[string]string{"config.yaml": "model: {}\n"})
	if got := collectMemoryProviderExternalPaths(context.Background(), h); len(got) != 0 {
		t.Fatalf("paths %v", got)
	}
}

func TestBackupUnwritableParentErrorsCleanly(t *testing.T) {
	tmp, h := backupHome(t)
	writeTree(t, h, map[string]string{"config.yaml": "model: {}\n"})
	blocker := filepath.Join(tmp, "blocker")
	os.WriteFile(blocker, []byte("i am a file, not a dir"), 0o644)
	ok, out, err := runBackupNS(t, map[string]any{"output": filepath.Join(blocker, "sub", "backup.zip")})
	var exit *SystemExit
	if ok || !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("result %v %v", ok, err)
	}
	if strings.Contains(out, "Traceback") || !strings.HasPrefix(out, "Error: cannot write backup to "+filepath.Join(blocker, "sub", "backup.zip")+": [Errno 20] Not a directory") {
		t.Fatalf("output %q", out)
	}
}

func TestRunBackupPrunesOlderDefaultNamedZipsButNotOthers(t *testing.T) {
	tmp, h := backupHome(t)
	writeTree(t, h, map[string]string{"config.yaml": "model: x\n"})
	for i := range 4 {
		os.WriteFile(filepath.Join(tmp, fmt.Sprintf("%s2026-01-0%d-000000.zip", runBackupPrefix, i+1)), []byte("old"), 0o644)
	}
	os.WriteFile(filepath.Join(tmp, "my-archive.zip"), []byte("mine"), 0o644)
	ok, out, err := runBackupNS(t, map[string]any{"output": nil, "keep": 2})
	if !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	kept, _ := filepath.Glob(filepath.Join(tmp, runBackupPrefix+"*.zip"))
	sort.Strings(kept)
	if len(kept) != 2 || filepath.Base(kept[0]) != runBackupPrefix+"2026-01-04-000000.zip" {
		t.Fatalf("kept %v", kept)
	}
	if _, err := os.Stat(filepath.Join(tmp, "my-archive.zip")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Pruned 3 older "+runBackupPrefix+"*.zip (keeping 2).") {
		t.Fatalf("output %q", out)
	}
}

func TestBackupRefusesWhileAnotherBackupHoldsTheSlot(t *testing.T) {
	_, h := backupHome(t)
	writeTree(t, h, map[string]string{"config.yaml": "model: x\n"})
	err := backupOperationLock(h, 0.25, func() error {
		ok, out, err := runBackupNS(t, map[string]any{"output": nil})
		var exit *SystemExit
		if ok || !errors.As(err, &exit) || exit.Code != 2 || out != "Error: another Icebath backup is already running\n" {
			t.Fatalf("%v %v %q", ok, err, out)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBackupMissingHomeExitsOne(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(home.EnvHome, filepath.Join(tmp, "missing"))
	t.Cleanup(home.PatchUserHome(tmp))
	ok, out, err := runBackupNS(t, map[string]any{"output": nil})
	var exit *SystemExit
	if ok || !errors.As(err, &exit) || exit.Code != 1 || out != "Error: Icebath home directory not found at "+filepath.Join(tmp, "missing")+"\n" {
		t.Fatalf("%v %v %q", ok, err, out)
	}
}

func TestBackupReportsTheArchiveSummary(t *testing.T) {
	tmp, h := backupHome(t)
	makeHomeTree(t, h)
	outZip := filepath.Join(tmp, "summary.zip")
	ok, out, err := runBackupNS(t, map[string]any{"output": outZip})
	if !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	for _, want := range []string{"Scanning ~/.icebath ...\n", "Backing up 12 files ...\n", "\nBackup complete: " + outZip + "\n  Files:       12\n",
		"\n  Excluded directories:\n    icebath-agent/\n    plugins/__pycache__/\n", "\nRestore with: icebath import summary.zip\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestZipCapturesLiveWalAndCleansFailedStaging(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprintf("automatic=%v", automatic), func(t *testing.T) {
			tmp, h := backupHome(t)
			output := filepath.Join(tmp, "archive")
			os.Mkdir(output, 0o755)
			archive := filepath.Join(output, "backup.zip")
			run := func() {
				if automatic {
					if _, err := writeFullZipBackup(context.Background(), archive, h); err != nil {
						t.Fatal(err)
					}
					return
				}
				if _, _, err := runBackupNS(t, map[string]any{"output": archive}); err != nil {
					t.Fatal(err)
				}
			}
			writer := testConn(t, filepath.Join(h, "state.db"), "")
			mustExec(t, writer, "PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "CREATE TABLE messages (body TEXT)",
				"PRAGMA wal_checkpoint(TRUNCATE)", "INSERT INTO messages VALUES ('WAL-only row')")
			run()
			bodies := zipBodies(t, archive)
			var member string
			for name := range bodies {
				if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
					t.Fatalf("sidecar %s archived", name)
				}
				if strings.HasSuffix(name, "state.db") {
					member = name
				}
			}
			restored := filepath.Join(tmp, "restored.db")
			os.WriteFile(restored, bodies[member], 0o644)
			if rows := tableRows(t, restored, "SELECT body FROM messages"); !slices.Equal(rows, []string{"WAL-only row"}) {
				t.Fatalf("rows %v", rows)
			}
			previous := zipCRCs(t, archive)
			swap(t, &zipWrite, func(*pyzip.ZipFile, string, string) error {
				return &os.PathError{Op: "write", Path: archive, Err: syscall.ENOSPC}
			})
			run()
			entries, _ := os.ReadDir(output)
			if len(entries) != 1 || entries[0].Name() != "backup.zip" {
				t.Fatalf("a failed write must not leak a private database snapshot: %v", entries)
			}
			if automatic {
				if got := zipCRCs(t, archive); !maps.Equal(got, previous) {
					t.Fatalf("an all-failed run must not overwrite the previous good archive: %v %v", got, previous)
				}
			}
		})
	}
}

func TestBackupLockRejectsASecondOperation(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	os.Mkdir(h, 0o755)
	err := backupOperationLock(h, 0.25, func() error {
		err := backupOperationLock(h, 0, func() error {
			t.Fatal("second backup unexpectedly acquired the lock")
			return nil
		})
		var busy *BackupInProgressError
		if !errors.As(err, &busy) {
			t.Fatalf("second lock %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backupOperationLock(h, 0, func() error { return nil }); err != nil {
		t.Fatalf("released slot %v", err)
	}
}

func TestAtomicOutputPublishesOnlyAfterCleanClose(t *testing.T) {
	final := filepath.Join(t.TempDir(), "backup.zip")
	os.WriteFile(final, []byte("previous"), 0o644)
	var partial string
	err := atomicOutputPath(final, nil, func(p string) error {
		partial = p
		os.WriteFile(p, []byte("complete"), 0o644)
		if b, _ := os.ReadFile(final); string(b) != "previous" {
			t.Fatalf("final changed before close: %q", b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(final); string(b) != "complete" {
		t.Fatalf("final %q", b)
	}
	if _, err := os.Stat(partial); err == nil {
		t.Fatal("partial remains")
	}
}

func TestAtomicOutputKeepsPreviousFileAfterFailure(t *testing.T) {
	final := filepath.Join(t.TempDir(), "backup.zip")
	os.WriteFile(final, []byte("previous"), 0o644)
	var partial string
	boom := errors.New("compression failed")
	err := atomicOutputPath(final, nil, func(p string) error {
		partial = p
		os.WriteFile(p, []byte("incomplete"), 0o644)
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error %v", err)
	}
	if b, _ := os.ReadFile(final); string(b) != "previous" {
		t.Fatalf("final %q", b)
	}
	if _, err := os.Stat(partial); err == nil {
		t.Fatal("partial remains")
	}
}

func TestAtomicOutputDiscardsWhenThePublishPathDeclines(t *testing.T) {
	final := filepath.Join(t.TempDir(), "backup.zip")
	var partial string
	err := atomicOutputPath(final, func() (string, bool) { return "", false }, func(p string) error {
		partial = p
		return os.WriteFile(p, []byte("x"), 0o644)
	})
	if err != nil || !strings.HasPrefix(filepath.Base(partial), ".backup.zip.") || !strings.HasSuffix(partial, ".partial") {
		t.Fatalf("%v %s", err, partial)
	}
	for _, p := range []string{final, partial} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s exists", p)
		}
	}
}

func TestDiscardFailedZipMembersRestoresAShadowedDuplicate(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	os.WriteFile(src, []byte("first"), 0o644)
	zf, err := pyzip.Create(filepath.Join(dir, "d.zip"), pyzip.Deflated, ptr(6))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeZipFile(zf, src, "a"); err != nil {
		t.Fatal(err)
	}
	first := zf.NameToInfo["a"]
	swap(t, &zipWrite, func(zf *pyzip.ZipFile, path, arcname string) error {
		w, err := zf.Open(arcname)
		if err != nil {
			return err
		}
		w.Write([]byte("second"))
		w.Close()
		return &os.PathError{Op: "read", Path: path, Err: syscall.EIO}
	})
	if err := writeZipFile(zf, src, "a"); !errors.Is(err, syscall.EIO) {
		t.Fatalf("%v", err)
	}
	if len(zf.Filelist) != 1 || zf.NameToInfo["a"] != first {
		t.Fatalf("filelist %d", len(zf.Filelist))
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
	if b := zipBodies(t, filepath.Join(dir, "d.zip")); string(b["a"]) != "first" || len(b) != 1 {
		t.Fatalf("%v", b)
	}
}

func TestPrintCappedShowsTenLinesAndTheRestCount(t *testing.T) {
	var out bytes.Buffer
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = fmt.Sprintf("l%d", i)
	}
	printCapped(&out, "head", lines, "  ")
	want := "head\n  l0\n  l1\n  l2\n  l3\n  l4\n  l5\n  l6\n  l7\n  l8\n  l9\n  ... and 2 more\n"
	if out.String() != want {
		t.Fatalf("%q", out.String())
	}
}

func TestResolveBackupOutputPathAppendsZipAndNamesDirectories(t *testing.T) {
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	swap(t, &backupNow, func() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.Local) })
	var out bytes.Buffer
	cases := map[string]string{
		"":                                   filepath.Join(tmp, runBackupPrefix+"2026-03-04-050607.zip"),
		tmp:                                  filepath.Join(tmp, runBackupPrefix+"2026-03-04-050607.zip"),
		filepath.Join(tmp, "x.tar"):          filepath.Join(tmp, "x.tar.zip"),
		filepath.Join(tmp, "new", "y"):       filepath.Join(tmp, "new", "y.zip"),
		filepath.Join(tmp, "upper", "Z.ZIP"): filepath.Join(tmp, "upper", "Z.ZIP"),
	}
	for in, want := range cases {
		got, err := resolveBackupOutputPath(&out, in)
		if err != nil || got != want {
			t.Fatalf("%q -> %q %v want %q", in, got, err, want)
		}
	}
	if st, err := os.Stat(filepath.Join(tmp, "new")); err != nil || !st.IsDir() {
		t.Fatal("parent not created")
	}
}

func TestRetiredForeignDBHolderPIDsReportsUnknown(t *testing.T) {
	if got := retiredForeignDBHolderPIDs(filepath.Join(t.TempDir(), "state.db")); got != nil {
		t.Fatalf("%v", got)
	}
}

func TestIterExternalFilesSkipsLinksCachesAndRuntimeNames(t *testing.T) {
	tmp := t.TempDir()
	writeTree(t, tmp, map[string]string{"base/a.json": "{}", "base/node_modules/x.js": "x", "base/sub/b.txt": "b",
		"base/cron.pid": "1", "base/c.pyc": "\x00", "base/d.db-wal": "w", "single.json": "{}"})
	os.Symlink(filepath.Join(tmp, "single.json"), filepath.Join(tmp, "base", "link.json"))
	os.Symlink(filepath.Join(tmp, "single.json"), filepath.Join(tmp, "single-link"))
	got := iterExternalFiles(filepath.Join(tmp, "base"))
	sort.Strings(got)
	want := []string{filepath.Join(tmp, "base", "a.json"), filepath.Join(tmp, "base", "sub", "b.txt")}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
	if got := iterExternalFiles(filepath.Join(tmp, "single.json")); !slices.Equal(got, []string{filepath.Join(tmp, "single.json")}) {
		t.Fatalf("%v", got)
	}
	if got := iterExternalFiles(filepath.Join(tmp, "single-link")); len(got) != 0 {
		t.Fatalf("link base %v", got)
	}
}

func TestSnapshotRecoveryHintNamesTheSlashCommand(t *testing.T) {
	if got := snapshotRecoveryHint(); got != "To restore a newer snapshot, start `icebath` in a terminal and run `/snapshot list`, then `/snapshot restore <id>` (CLI only)." {
		t.Fatal(got)
	}
}

// Memory provider declaring fixed backup paths or failing to
type pathsProvider struct {
	memory.BaseProvider
	paths []string
	err   error
}

func (p *pathsProvider) Name() string                                                 { return "backup-paths-test" }
func (p *pathsProvider) Available(context.Context) bool                               { return true }
func (p *pathsProvider) Initialize(context.Context, string, memory.InitOptions) error { return nil }
func (p *pathsProvider) ToolSchemas(context.Context) ([]map[string]any, error)        { return nil, nil }
func (p *pathsProvider) BackupPaths() ([]string, error)                               { return p.paths, p.err }

func TestCollectMemoryProviderExternalPathsKeepsOneSpellingPerExistingTarget(t *testing.T) {
	tmp, h := backupHome(t)
	t.Setenv("HOME", tmp)
	writeTree(t, h, map[string]string{"config.yaml": "memory:\n  provider: backup-paths-test\n"})
	writeTree(t, tmp, map[string]string{".honcho/state.json": "{}"})
	os.Symlink(filepath.Join(tmp, ".honcho"), filepath.Join(tmp, "honcho-link"))
	current := &pathsProvider{paths: []string{"~/.honcho", filepath.Join(tmp, "honcho-link"), filepath.Join(tmp, "missing"), filepath.Join(tmp, ".honcho") + "/"}}
	memory.RegisterProvider(memory.ProviderRegistration{Name: "backup-paths-test",
		New: func(context.Context, *tools.Env, memory.LoadOptions) (memory.Provider, error) { return current, nil }})
	if got := collectMemoryProviderExternalPaths(context.Background(), h); !slices.Equal(got, []string{filepath.Join(tmp, ".honcho")}) {
		t.Fatalf("paths %v", got)
	}
	current.err = errors.New("provider exploded")
	if got := collectMemoryProviderExternalPaths(context.Background(), h); len(got) != 0 {
		t.Fatalf("failing provider paths %v", got)
	}
}

func TestSwapGatewayServiceReplacesAndRestoresTheReviveSeams(t *testing.T) {
	calls := 0
	restore := SwapGatewayService(func(context.Context) (bool, error) { calls++; return true, nil },
		func(context.Context, io.Writer, gateway.ServiceSetupOptions) (bool, error) {
			calls += 10
			return true, nil
		})
	running, _ := isServiceRunning(context.Background())
	started, _ := ensureGatewayService(context.Background(), io.Discard, gateway.ServiceSetupOptions{})
	restore()
	if !running || !started || calls != 11 {
		t.Fatalf("%v %v %d", running, started, calls)
	}
	if ensureGatewayService == nil || isServiceRunning == nil {
		t.Fatal("restore left the seams empty")
	}
}
