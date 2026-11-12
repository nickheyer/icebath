package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Home with the critical state files and a one row sessions database
func quickHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	writeTree(t, h, map[string]string{
		"config.yaml": "model:\n  provider: openrouter\n", ".env": "OPENROUTER_API_KEY=test-key-123\n",
		"auth.json": "{\"providers\": {}}\n", "channel_aliases.json": "{\"whatsapp\": {\"120363408391911677@g.us\": \"general\"}}\n",
		"cron/jobs.json": "{\"jobs\": []}\n",
	})
	c := testConn(t, filepath.Join(h, "state.db"), "")
	mustExec(t, c, "CREATE TABLE sessions (id TEXT PRIMARY KEY, data TEXT)", "INSERT INTO sessions VALUES ('s1', 'hello world')")
	closeQuietly(c)
	return h
}

// Parsed manifest of a published snapshot
func snapshotManifest(t *testing.T, h, snapID string) *pyjson.Object {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, quickSnapshotsDir, snapID, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	obj, err := pyjson.LoadsDict(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

// Keys of a manifest object field
func manifestKeys(t *testing.T, m *pyjson.Object, field string) []string {
	t.Helper()
	v, _ := m.Get(field)
	obj, ok := v.(*pyjson.Object)
	if !ok {
		t.Fatalf("%s is %T", field, v)
	}
	return obj.Keys
}

// Strings of a manifest list field
func manifestList(t *testing.T, m *pyjson.Object, field string) []string {
	t.Helper()
	v, _ := m.Get(field)
	items, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is %T", field, v)
	}
	out := []string{}
	for _, item := range items {
		out = append(out, item.(string))
	}
	return out
}

// Snapshot of a home printing into a buffer
func quickSnapshot(t *testing.T, label *string, h string, keep *int, maxFileSize *int64) (string, string) {
	t.Helper()
	var out bytes.Buffer
	snapID, err := createQuickSnapshot(context.Background(), &out, label, h, keep, maxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	return snapID, out.String()
}

func TestQuickSnapshotStateDbSafelyCopied(t *testing.T) {
	h := quickHome(t)
	snapID, _ := quickSnapshot(t, nil, h, nil, nil)
	dbCopy := filepath.Join(h, quickSnapshotsDir, snapID, "state.db")
	if _, err := os.Stat(dbCopy); err != nil {
		t.Fatal(err)
	}
	if rows := tableRows(t, dbCopy, "SELECT * FROM sessions"); !slices.Equal(rows, []string{"s1|hello world"}) {
		t.Fatalf("rows %v", rows)
	}
}

func TestQuickSnapshotFailedStateDbCopyIsLoud(t *testing.T) {
	h := quickHome(t)
	swap(t, &copySQLite, func(context.Context, string, string) (bool, error) { return false, nil })
	var buf bytes.Buffer
	t.Cleanup(SwapOutput(&buf))
	snapID, err := CreateQuickSnapshot(nil, h, nil, nil)
	if err != nil || snapID == "" {
		t.Fatalf("no snapshot %v", err)
	}
	out := buf.String()
	m := snapshotManifest(t, h, snapID)
	if slices.Contains(manifestKeys(t, m, "files"), "state.db") || !slices.Contains(manifestList(t, m, "failed_dbs"), "state.db") {
		t.Fatalf("manifest %v", m)
	}
	root := filepath.Join(h, quickSnapshotsDir)
	for _, want := range []string{
		"  ⚠ Snapshot: SQLite safe copy FAILED for state.db — file may be locked or corrupted\n",
		"  ⚠ CRITICAL: could not snapshot DB file(s): state.db\n",
		"  ⚠ If sessions disappear after the update, check " + root + ". " + snapshotRecoveryHint() + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestQuickSnapshotSnapshotIncludesPairingDirectories(t *testing.T) {
	h := quickHome(t)
	writeTree(t, h, map[string]string{
		"platforms/pairing/telegram-approved.json": `{"12345": {"user_name": "alice"}}`,
		"platforms/pairing/discord-approved.json":  `{"67890": {"user_name": "bob"}}`,
		"pairing/matrix-approved.json":             `{"@charlie:server": {"user_name": "charlie"}}`,
		"feishu_comment_pairing.json":              `{"doc_abc": {"allow_from": ["user_xyz"]}}`,
	})
	snapID, _ := quickSnapshot(t, nil, h, nil, nil)
	if snapID == "" {
		t.Fatal("no snapshot")
	}
	rels := []string{"platforms/pairing/telegram-approved.json", "platforms/pairing/discord-approved.json",
		"pairing/matrix-approved.json", "feishu_comment_pairing.json"}
	files := manifestKeys(t, snapshotManifest(t, h, snapID), "files")
	for _, rel := range rels {
		if _, err := os.Stat(filepath.Join(h, quickSnapshotsDir, snapID, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(files, rel) {
			t.Fatalf("%s missing from %v", rel, files)
		}
	}
}

func TestQuickSnapshotOversizedDbSuppressesPruning(t *testing.T) {
	h := quickHome(t)
	var clock time.Duration
	firstID, _ := quickSnapshot(t, ptr("complete"), h, nil, nil)
	if firstID == "" {
		t.Fatal("no first snapshot")
	}
	if _, err := os.Stat(filepath.Join(h, quickSnapshotsDir, firstID, "state.db")); err != nil {
		t.Fatal(err)
	}
	advanceBackupClock(t, &clock)
	secondID, out := quickSnapshot(t, ptr("oversized"), h, ptr(1), ptr(int64(1024)))
	if secondID == "" {
		t.Fatal("no second snapshot")
	}
	if _, err := os.Stat(filepath.Join(h, quickSnapshotsDir, secondID, "state.db")); err == nil {
		t.Fatal("oversized state.db copied")
	}
	if !slices.Contains(manifestList(t, snapshotManifest(t, h, secondID), "oversized_skipped"), "state.db") {
		t.Fatal("oversized skip not recorded")
	}
	snaps, err := ListQuickSnapshots(100, h)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, s := range snaps {
		id, _ := s.(*pyjson.Object).Get("id")
		ids[id.(string)] = true
	}
	if !ids[firstID] || !ids[secondID] {
		t.Fatalf("Complete snapshot %s was pruned by an incomplete (oversized) snapshot: %v", firstID, ids)
	}
	if !strings.Contains(out, "  ⚠ Snapshot: skipping state.db (12.0 KB exceeds 1.0 KB limit)\n") ||
		!strings.Contains(out, "  ⚠ Skipping snapshot prune: DB file(s) skipped for size: state.db\n") {
		t.Fatalf("output %q", out)
	}
}

// Home with a config, a projects store and the default kanban board
func kanbanHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	writeTree(t, h, map[string]string{"config.yaml": "model:\n  provider: openrouter\n"})
	for _, db := range [][4]string{{"projects.db", "projects", "p1", "demo"}, {"kanban.db", "tasks", "t1", "todo"}} {
		c := testConn(t, filepath.Join(h, db[0]), "")
		mustExec(t, c, "CREATE TABLE "+db[1]+" (id TEXT PRIMARY KEY, data TEXT)", fmt.Sprintf("INSERT INTO %s VALUES ('%s', '%s')", db[1], db[2], db[3]))
		closeQuietly(c)
	}
	return h
}

func TestQuickSnapshotProjectsKanbanNonDefaultKanbanBoardSnapshotted(t *testing.T) {
	h := kanbanHome(t)
	board := filepath.Join(h, "kanban", "boards", "work")
	os.MkdirAll(board, 0o755)
	boardDB := filepath.Join(board, "kanban.db")
	c := testConn(t, boardDB, "")
	mustExec(t, c, "CREATE TABLE tasks (id TEXT PRIMARY KEY, data TEXT)", "INSERT INTO tasks VALUES ('w1', 'ship')")
	closeQuietly(c)
	snapID, _ := quickSnapshot(t, nil, h, nil, nil)
	if !isFile(filepath.Join(h, quickSnapshotsDir, snapID, "kanban", "boards", "work", "kanban.db")) {
		t.Fatal("non-default board kanban.db was not snapshotted")
	}
	c = testConn(t, boardDB, "")
	mustExec(t, c, "DELETE FROM tasks")
	closeQuietly(c)
	if ok, err := RestoreQuickSnapshot(context.Background(), snapID, h); !ok || err != nil {
		t.Fatalf("restore %v %v", ok, err)
	}
	if rows := tableRows(t, boardDB, "SELECT * FROM tasks"); !slices.Equal(rows, []string{"w1|ship"}) {
		t.Fatalf("rows %v", rows)
	}
}

func TestQuickSnapshotProjectsKanbanBoardDbCopiedWalSafely(t *testing.T) {
	h := kanbanHome(t)
	board := filepath.Join(h, "kanban", "boards", "work")
	os.MkdirAll(board, 0o755)
	c := testConn(t, filepath.Join(board, "kanban.db"), "")
	mustExec(t, c, "PRAGMA journal_mode=WAL", "CREATE TABLE tasks (id TEXT PRIMARY KEY, data TEXT)", "INSERT INTO tasks VALUES ('w1', 'ship')")
	closeQuietly(c)
	var called []string
	real := copySQLite
	swap(t, &copySQLite, func(ctx context.Context, src, dst string) (bool, error) {
		called = append(called, src)
		return real(ctx, src, dst)
	})
	snapID, _ := quickSnapshot(t, nil, h, nil, nil)
	if !slices.ContainsFunc(called, func(s string) bool { return strings.HasSuffix(filepath.ToSlash(s), "boards/work/kanban.db") }) {
		t.Fatalf("copied %v", called)
	}
	copied := filepath.Join(h, quickSnapshotsDir, snapID, "kanban", "boards", "work", "kanban.db")
	if rows := tableRows(t, copied, "SELECT * FROM tasks"); !slices.Equal(rows, []string{"w1|ship"}) {
		t.Fatalf("rows %v", rows)
	}
	files := manifestKeys(t, snapshotManifest(t, h, snapID), "files")
	for _, rel := range []string{"projects.db", "kanban.db", "kanban/boards/work/kanban.db"} {
		if !slices.Contains(files, rel) {
			t.Fatalf("%s missing from %v", rel, files)
		}
	}
}

func TestQuickSnapshotSkipsBoardWorkspacesAndAttachments(t *testing.T) {
	h := kanbanHome(t)
	writeTree(t, h, map[string]string{"kanban/boards/work/workspaces/t1/out.txt": "x", "kanban/boards/work/attachments/a.png": "x",
		"kanban/boards/work/board.json": "{}"})
	snapID, _ := quickSnapshot(t, nil, h, nil, nil)
	files := manifestKeys(t, snapshotManifest(t, h, snapID), "files")
	if !slices.Contains(files, "kanban/boards/work/board.json") || slices.ContainsFunc(files, func(s string) bool {
		return strings.Contains(s, "workspaces") || strings.Contains(s, "attachments")
	}) {
		t.Fatalf("files %v", files)
	}
}

func TestQuickSnapshotIsPublishedWithManifest(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	writeTree(t, h, map[string]string{"config.yaml": "model: {}\n"})
	var published [][2]string
	real := osReplace
	swap(t, &osReplace, func(src, dst string) error {
		if filepath.Dir(dst) == filepath.Join(h, quickSnapshotsDir) {
			if !strings.HasSuffix(filepath.Base(src), ".partial") {
				t.Fatalf("source %s", src)
			}
			if st, err := os.Stat(filepath.Join(src, "manifest.json")); err != nil || !st.Mode().IsRegular() {
				t.Fatal("manifest not staged")
			}
			if _, err := os.Stat(dst); err == nil {
				t.Fatal("destination exists before publish")
			}
			published = append(published, [2]string{src, dst})
		}
		return real(src, dst)
	})
	snapID, err := CreateQuickSnapshot(nil, h, nil, nil)
	if err != nil || snapID == "" || len(published) != 1 {
		t.Fatalf("%q %v %d", snapID, err, len(published))
	}
	m := snapshotManifest(t, h, snapID)
	id, _ := m.Get("id")
	files, _ := m.Get("files")
	want := pyjson.NewObject()
	want.Set("config.yaml", int64(10))
	if id != snapID || !pyjson.Equal(files, want) {
		t.Fatalf("manifest %v", m)
	}
}

func TestQuickSnapshotListingIgnoresPartialDirectories(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	writeTree(t, h, map[string]string{"state-snapshots/.unfinished.1.partial/manifest.json": `{"id":"unfinished"}`})
	snaps, err := ListQuickSnapshots(20, h)
	if err != nil || len(snaps) != 0 {
		t.Fatalf("%v %v", snaps, err)
	}
}

func TestQuickSnapshotManifestRecordsEveryField(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	writeTree(t, h, map[string]string{"config.yaml": "model: {}\n", ".env": "A=1\n"})
	swap(t, &backupNow, func() time.Time { return time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC) })
	snapID, _ := quickSnapshot(t, ptr("tag"), h, nil, nil)
	if snapID != "20260506-070809-tag" {
		t.Fatal(snapID)
	}
	raw, _ := os.ReadFile(filepath.Join(h, quickSnapshotsDir, snapID, "manifest.json"))
	want := "{\n  \"id\": \"20260506-070809-tag\",\n  \"timestamp\": \"20260506-070809\",\n  \"label\": \"tag\",\n  \"file_count\": 2,\n" +
		"  \"total_size\": 14,\n  \"files\": {\n    \"config.yaml\": 10,\n    \".env\": 4\n  },\n  \"failed_dbs\": [],\n  \"oversized_skipped\": []\n}"
	if string(raw) != want {
		t.Fatalf("%s", raw)
	}
	again, _ := quickSnapshot(t, ptr("tag"), h, nil, nil)
	if again != "20260506-070809-tag-2" {
		t.Fatal(again)
	}
}

func TestQuickSnapshotWithNothingToCaptureReturnsNone(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	writeTree(t, h, map[string]string{"skills/x.md": "x"})
	if snapID, _ := quickSnapshot(t, nil, h, nil, nil); snapID != "" {
		t.Fatal(snapID)
	}
	entries, _ := os.ReadDir(filepath.Join(h, quickSnapshotsDir))
	if len(entries) != 0 {
		t.Fatalf("staging left %v", entries)
	}
	os.WriteFile(filepath.Join(h, "state.db"), make([]byte, 200), 0o644)
	snapID, out := quickSnapshot(t, nil, h, nil, nil)
	if snapID != "" || !strings.Contains(out, "  ⚠ Snapshot: state.db looks ZEROED (no SQLite header; 200 bytes)\n") ||
		!strings.Contains(out, "  ⚠ Snapshot aborted: no files captured (failed DBs: state.db)\n") {
		t.Fatalf("%q %q", snapID, out)
	}
}

func TestQuickSnapshotPrunesBeyondKeep(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	writeTree(t, h, map[string]string{"config.yaml": "model: {}\n"})
	var clock time.Duration
	var ids []string
	for range 3 {
		id, _ := quickSnapshot(t, nil, h, ptr(2), nil)
		ids = append(ids, id)
		advanceBackupClock(t, &clock)
	}
	entries, _ := os.ReadDir(filepath.Join(h, quickSnapshotsDir))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, ids[1:]) {
		t.Fatalf("%v want %v", names, ids[1:])
	}
}

func TestQuickSnapshotFlagsCorruptStateDb(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	h := filepath.Join(tmp, "hermes_home")
	writeTree(t, h, map[string]string{"config.yaml": "test: true\n"})
	good := filepath.Join(tmp, "state.db")
	db, err := state.NewSessionDB(context.Background(), good, false)
	if err != nil {
		t.Fatal(err)
	}
	sid := "4a1c4bb0-74b3-4a4b-9d0e-0b6a2f3c9e11"
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		content := fmt.Sprintf("needle-%d ", i) + strings.Repeat("payload ", 40)
		if _, err := db.AppendMessage(context.Background(), sid, map[string]any{"role": "user", "content": content}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	raw, _ := os.ReadFile(good)
	os.WriteFile(filepath.Join(h, "state.db"), raw[:len(raw)-4096-100], 0o644)
	snapID, err := CreateQuickSnapshot(ptr("conformance"), h, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapID == "" {
		return
	}
	m := snapshotManifest(t, h, snapID)
	if slices.Contains(manifestKeys(t, m, "files"), "state.db") {
		if !VerifySQLiteIntegrity(filepath.Join(h, quickSnapshotsDir, snapID, "state.db"), IntegrityOptions{}).Valid {
			t.Fatal("quick snapshot recorded a corrupt state.db as captured")
		}
	} else if !slices.Contains(manifestList(t, m, "failed_dbs"), "state.db") {
		t.Fatal("state.db neither captured nor flagged as failed — silent loss")
	}
}

func TestQuickSnapshotRefusesWhileTheSlotIsHeld(t *testing.T) {
	h := quickHome(t)
	err := backupOperationLock(h, 0.25, func() error {
		_, err := createQuickSnapshot(context.Background(), io.Discard, nil, h, nil, nil)
		var busy *BackupInProgressError
		if !errors.As(err, &busy) {
			t.Fatalf("%v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAllProfileSnapshotsEachSiblingSnapshottedIntoOwnHome(t *testing.T) {
	p := profilesFixture(t)
	result, err := createPreUpdateSnapshotsAllProfiles(context.Background(), io.Discard, p["default"], ptr(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := append([]string{}, result.Keys...)
	slices.Sort(names)
	if !slices.Equal(names, []string{"sparks", "work"}) {
		t.Fatalf("%v", names)
	}
	for _, name := range result.Keys {
		v, _ := result.Get(name)
		snapID := v.(string)
		snapDir := filepath.Join(p[name], quickSnapshotsDir, snapID)
		if !isDir(snapDir) || !isFile(filepath.Join(snapDir, "config.yaml")) || !strings.Contains(snapID, "pre-update") {
			t.Fatalf("%s %s", name, snapID)
		}
	}
	if _, err := os.Stat(filepath.Join(p["default"], quickSnapshotsDir)); err == nil {
		t.Fatal("invoking profile snapshotted")
	}
}

func TestAllProfileSnapshotsSizeCapForwarded(t *testing.T) {
	p := profilesFixture(t)
	os.WriteFile(filepath.Join(p["work"], "state.db"), make([]byte, 4096), 0o644)
	result, err := createPreUpdateSnapshotsAllProfiles(context.Background(), io.Discard, p["default"], ptr(1), ptr(int64(1024)))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := result.Get("work")
	snapDir := filepath.Join(p["work"], quickSnapshotsDir, v.(string))
	if isFile(filepath.Join(snapDir, "state.db")) || !isFile(filepath.Join(snapDir, "config.yaml")) {
		t.Fatalf("snapshot %s", snapDir)
	}
}

func TestAllProfileSnapshotsOneFailingSiblingDoesNotBlockOthers(t *testing.T) {
	p := profilesFixture(t)
	real := quickSnapshotForProfile
	swap(t, &quickSnapshotForProfile, func(ctx context.Context, w io.Writer, label *string, h string, keep *int, maxFileSize *int64) (string, error) {
		if h == p["work"] {
			return "", errors.New("disk full")
		}
		return real(ctx, w, label, h, keep, maxFileSize)
	})
	result, err := CreatePreUpdateSnapshotsAllProfiles(p["default"], nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Get("sparks"); !ok {
		t.Fatal("sparks missing")
	}
	if _, ok := result.Get("work"); ok {
		t.Fatal("work present")
	}
}

func TestRunQuickBackupReportsTheSnapshot(t *testing.T) {
	_, h := backupHome(t)
	writeTree(t, h, map[string]string{"config.yaml": "model: {}\n"})
	swap(t, &backupNow, func() time.Time { return time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC) })
	var out bytes.Buffer
	code, err := RunQuickBackup(context.Background(), h, map[string]any{"label": "manual"}, &out)
	want := "State snapshot created: 20260506-070809-manual\n  1 snapshot(s) stored in ~/.icebath/state-snapshots/\n" +
		"  Restore with: /snapshot restore 20260506-070809-manual\n"
	if code != 0 || err != nil || out.String() != want {
		t.Fatalf("%d %v %q", code, err, out.String())
	}
	empty := filepath.Join(t.TempDir(), "empty")
	os.Mkdir(empty, 0o755)
	out.Reset()
	if code, err := RunQuickBackup(context.Background(), empty, map[string]any{"label": nil}, &out); code != 0 || err != nil || out.String() != "No state files found to snapshot.\n" {
		t.Fatalf("%d %v %q", code, err, out.String())
	}
}
