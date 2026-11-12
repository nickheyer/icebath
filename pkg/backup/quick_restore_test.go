package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Snapshot id of a home through the public entry point
func snapshotOf(t *testing.T, h string, label string, keep int) string {
	t.Helper()
	var l *string
	if label != "" {
		l = &label
	}
	var k *int
	if keep != 0 {
		k = &keep
	}
	id, err := CreateQuickSnapshot(l, h, k, nil)
	if err != nil || id == "" {
		t.Fatalf("snapshot %q %v", id, err)
	}
	return id
}

func restoreSnapshot(t *testing.T, id, h string) bool {
	t.Helper()
	ok, err := RestoreQuickSnapshot(context.Background(), id, h)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// Routes the default logger into a buffer for the test
func captureLog(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func sqliteExec(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestQuickSnapshotRestoreStateDbLiveConnection(t *testing.T) {
	h := quickHome(t)
	t.Setenv("ICEBATH_HOME", h)
	snapID := snapshotOf(t, h, "", 0)
	live, err := sql.Open("sqlite", filepath.Join(h, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	live.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode=wal", "INSERT INTO sessions VALUES ('s2', 'new-data')"} {
		if _, err := live.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if live.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n); n != 2 {
		t.Fatalf("before %d", n)
	}
	if !restoreSnapshot(t, snapID, h) {
		t.Fatal("restore failed")
	}
	if live.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n); n != 1 {
		t.Fatalf("Live connection still sees %d rows after restore (expected 1); the extra row 's2' should have been reverted.", n)
	}
}

func seedJobs(t *testing.T, path string, ids ...string) {
	t.Helper()
	jobs := []map[string]string{}
	for _, id := range ids {
		jobs = append(jobs, map[string]string{"id": id})
	}
	raw, _ := json.Marshal(map[string]any{"jobs": jobs})
	writeText(t, path, string(raw))
}

func jobCount(t *testing.T, path string) int {
	t.Helper()
	var doc struct{ Jobs []any }
	if err := json.Unmarshal([]byte(readText(t, path)), &doc); err != nil {
		t.Fatal(err)
	}
	return len(doc.Jobs)
}

func TestRestoreCronJobsIfEmptiedRestoresWhenEmptiedAfterMigration(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	jobs := filepath.Join(h, "cron", "jobs.json")
	seedJobs(t, jobs, "a", "b", "c")
	snapID := snapshotOf(t, h, "pre-update", 5)
	writeText(t, jobs, `{"jobs": []}`)
	result, err := RestoreCronJobsIfEmptied(snapID, h)
	if err != nil || result == nil {
		t.Fatalf("%v %v", result, err)
	}
	if v, _ := result.Get("restored"); v != true {
		t.Fatal(result)
	}
	if v, _ := result.Get("job_count"); v != int64(3) {
		t.Fatal(result)
	}
	if v, _ := result.Get("snapshot_id"); v != snapID {
		t.Fatal(result)
	}
	if jobCount(t, jobs) != 3 {
		t.Fatal("jobs not restored")
	}
}

func TestRestoreCronJobsIfEmptiedRestoresWhenPartialJobLoss(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	jobs := filepath.Join(h, "cron", "jobs.json")
	var ids []string
	for i := range 19 {
		ids = append(ids, "job-"+strconv.Itoa(i))
	}
	seedJobs(t, jobs, ids...)
	snapID := snapshotOf(t, h, "pre-update", 5)
	seedJobs(t, jobs, "desktop-watchdog")
	result, err := RestoreCronJobsIfEmptied(snapID, h)
	if err != nil || result == nil {
		t.Fatalf("%v %v", result, err)
	}
	if v, _ := result.Get("restored"); v != true {
		t.Fatal(result)
	}
	if v, _ := result.Get("job_count"); v != int64(19) {
		t.Fatal(result)
	}
	if jobCount(t, jobs) != 19 {
		t.Fatal("jobs not restored")
	}
}

const userConfig = "_config_version: 39\n" +
	"model:\n" +
	"  provider: custom\n" +
	"  default: zyphra/zamba-3-large\n" +
	"  base_url: https://api.zyphra.example/v1\n" +
	"moa:\n" +
	"  enabled: true\n" +
	"  presets:\n" +
	"    council:\n" +
	"      aggregator: {provider: custom, model: zyphra/zamba-3-large}\n" +
	"custom_unknown_key:\n" +
	"  hello: world\n"

func seedConfig(t *testing.T) (string, string) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	cfg := filepath.Join(h, "config.yaml")
	writeText(t, cfg, userConfig)
	return h, cfg
}

func restoreConfig(t *testing.T, id, h string) *pyjson.Object {
	t.Helper()
	result, err := RestoreConfigModelSettingsIfRewritten(context.Background(), id, h)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func yamlDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	doc, err := config.FastSafeLoad([]byte(readText(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	return doc.(map[string]any)
}

func TestRestoreConfigModelSettingsIfRewrittenRestoresRewrittenProviderAndDroppedMoa(t *testing.T) {
	h, cfg := seedConfig(t)
	snapID := snapshotOf(t, h, "pre-update", 5)
	writeText(t, cfg, "_config_version: 39\nmodel:\n  provider: deepseek\n  default: deepseek-chat\n")
	result := restoreConfig(t, snapID, h)
	if result == nil {
		t.Fatal("no restore")
	}
	restored, _ := result.Get("restored")
	id, _ := result.Get("snapshot_id")
	keysAny, _ := result.Get("keys")
	var keys []string
	for _, k := range keysAny.([]any) {
		keys = append(keys, k.(string))
	}
	if restored != true || id != snapID || !slices.Contains(keys, "model.provider") || !slices.Contains(keys, "model.default") || !slices.Contains(keys, "moa") {
		t.Fatal(result)
	}
	after := yamlDoc(t, cfg)
	model := after["model"].(map[string]any)
	moa := after["moa"].(map[string]any)
	aggregator := moa["presets"].(map[string]any)["council"].(map[string]any)["aggregator"].(map[string]any)
	if model["provider"] != "custom" || model["default"] != "zyphra/zamba-3-large" || model["base_url"] != "https://api.zyphra.example/v1" ||
		moa["enabled"] != true || aggregator["model"] != "zyphra/zamba-3-large" {
		t.Fatal(after)
	}
}

func TestRestoreConfigModelSettingsIfRewrittenNoopWhenConfigUntouched(t *testing.T) {
	h, cfg := seedConfig(t)
	snapID := snapshotOf(t, h, "pre-update", 5)
	before := readText(t, cfg)
	if result := restoreConfig(t, snapID, h); result != nil {
		t.Fatal(result)
	}
	if readText(t, cfg) != before {
		t.Fatal("config changed")
	}
}

func TestRestoreConfigModelSettingsIfRewrittenPreservesLegitimateUpdateWrites(t *testing.T) {
	h, cfg := seedConfig(t)
	snapID := snapshotOf(t, h, "pre-update", 5)
	writeText(t, cfg, "_config_version: 40\nnew_section:\n  added: true\nmodel:\n  provider: deepseek\n")
	if result := restoreConfig(t, snapID, h); result == nil {
		t.Fatal("no restore")
	}
	after := yamlDoc(t, cfg)
	section, _ := after["new_section"].(map[string]any)
	if after["model"].(map[string]any)["provider"] != "custom" || after["_config_version"] != 40 || len(section) != 1 || section["added"] != true {
		t.Fatal(after)
	}
}

func TestRestoreConfigModelSettingsIfRewrittenNoopWhenUserNeverSetProtectedKeys(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	cfg := filepath.Join(h, "config.yaml")
	writeText(t, cfg, "_config_version: 39\nagent: {}\n")
	snapID := snapshotOf(t, h, "pre-update", 5)
	writeText(t, cfg, "_config_version: 39\nmodel:\n  provider: deepseek\n")
	if result := restoreConfig(t, snapID, h); result != nil {
		t.Fatal(result)
	}
}

func TestRestoreConfigModelSettingsIfRewrittenNoopOnUnreadableLiveConfig(t *testing.T) {
	h, cfg := seedConfig(t)
	snapID := snapshotOf(t, h, "pre-update", 5)
	writeText(t, cfg, ": not [valid yaml")
	if result := restoreConfig(t, snapID, h); result != nil {
		t.Fatal(result)
	}
	if readText(t, cfg) != ": not [valid yaml" {
		t.Fatal("unreadable config rewritten")
	}
}

func TestRestoreConfigModelSettingsIfRewrittenNoopWithoutSnapshotId(t *testing.T) {
	if result := restoreConfig(t, "", filepath.Join(t.TempDir(), ".icebath")); result != nil {
		t.Fatal(result)
	}
}

func TestRestoreConfigModelSettingsIsANoOpWithoutFileTooling(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	cfg := filepath.Join(h, "config.yaml")
	writeText(t, cfg, "_config_version: 39\nmodel:\n  provider: custom\n  default: zamba\n")
	snapID := snapshotOf(t, h, "pre-update", 5)
	writeText(t, cfg, "_config_version: 39\nmodel:\n  provider: deepseek\n")
	swap(t, &supportsFileTooling, func(context.Context) (bool, error) { return false, nil })
	if result := restoreConfig(t, snapID, h); result != nil {
		t.Fatal(result)
	}
	if !strings.Contains(readText(t, cfg), "deepseek") {
		t.Fatal("the local snapshot value was written")
	}
}

func preUpdateSnaps(t *testing.T, invoking string) *pyjson.Object {
	t.Helper()
	keep := 1
	snaps, err := CreatePreUpdateSnapshotsAllProfiles(invoking, &keep, nil)
	if err != nil {
		t.Fatal(err)
	}
	return snaps
}

func TestPerProfileCronRestoreLostJobsRestoredFromOwnSnapshot(t *testing.T) {
	profiles := profilesFixture(t)
	snaps := preUpdateSnaps(t, profiles["default"])
	jobs := filepath.Join(profiles["work"], "cron", "jobs.json")
	writeText(t, jobs, `{"jobs": []}`)
	restored := RestoreCronJobsAllProfiles(snaps, profiles["default"])
	if len(restored) != 1 {
		t.Fatalf("restored %v", restored)
	}
	if p, _ := restored[0].Get("profile"); p != "work" {
		t.Fatal(restored[0])
	}
	if n, _ := restored[0].Get("job_count"); n != int64(5) {
		t.Fatal(restored[0])
	}
	if jobCount(t, jobs) != 5 {
		t.Fatal("work jobs not restored")
	}
}

func TestPerProfileCronRestoreHealthyProfilesUntouched(t *testing.T) {
	profiles := profilesFixture(t)
	snaps := preUpdateSnaps(t, profiles["default"])
	if restored := RestoreCronJobsAllProfiles(snaps, profiles["default"]); len(restored) != 0 {
		t.Fatalf("restored %v", restored)
	}
}

func TestPerProfileCronRestoreEmptyMapIsNoop(t *testing.T) {
	profilesFixture(t)
	if restored := RestoreCronJobsAllProfiles(pyjson.NewObject(), ""); restored == nil || len(restored) != 0 {
		t.Fatalf("restored %v", restored)
	}
}

func oauthRow(provider, token string) map[string]any {
	access := "access-" + token
	if provider == "anthropic" {
		access = "sk-ant-oat01-" + token
	}
	return map[string]any{"id": "shared", "label": "shared", "auth_type": "oauth", "priority": 0, "source": "manual",
		"access_token": access, "refresh_token": "refresh-" + token}
}

func authStore(token, static string) map[string]any {
	return map[string]any{
		"version": 1,
		"providers": map[string]any{"openai-codex": map[string]any{"tokens": map[string]any{
			"access_token": "access-" + token, "refresh_token": "refresh-" + token}}},
		"credential_pool": map[string]any{
			"openai-codex": []any{oauthRow("openai-codex", token)},
			"openrouter":   []any{map[string]any{"id": "static", "auth_type": "api_key", "priority": 0, "api_key": static}},
		},
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	writeText(t, path, string(raw))
}

// Manifest listing the named snapshot files with their sizes
func writeManifest(t *testing.T, snapDir string, extra map[string]any, names ...string) {
	t.Helper()
	files := map[string]any{}
	for _, n := range names {
		st, err := os.Stat(filepath.Join(snapDir, n))
		if err != nil {
			t.Fatal(err)
		}
		files[n] = st.Size()
	}
	doc := map[string]any{"files": files}
	for k, v := range extra {
		doc[k] = v
	}
	writeJSON(t, filepath.Join(snapDir, "manifest.json"), doc)
}

func TestQuickSnapshotRestoreKeepsLiveOauthAndRestoresStaticAuth(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	writeJSON(t, filepath.Join(h, "auth.json"), authStore("rotated", "live-static"))
	snapDir := filepath.Join(h, "state-snapshots", "20260928-before-rotation")
	writeJSON(t, filepath.Join(snapDir, "auth.json"), authStore("spent", "snapshot-static"))
	writeManifest(t, snapDir, nil, "auth.json")
	if !restoreSnapshot(t, "20260928-before-rotation", h) {
		t.Fatal("restore refused")
	}
	var restored map[string]any
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(h, "auth.json"))), &restored); err != nil {
		t.Fatal(err)
	}
	pool := restored["credential_pool"].(map[string]any)
	codex := pool["openai-codex"].([]any)[0].(map[string]any)
	tokens := restored["providers"].(map[string]any)["openai-codex"].(map[string]any)["tokens"].(map[string]any)
	static := pool["openrouter"].([]any)[0].(map[string]any)
	if codex["refresh_token"] != "refresh-rotated" || tokens["refresh_token"] != "refresh-rotated" || static["api_key"] != "snapshot-static" {
		t.Fatal(restored)
	}
	if st, _ := os.Stat(filepath.Join(h, "auth.json")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestSnapshotAuthRefusalFailsTheWholeRestore(t *testing.T) {
	snapID := "20260101-000000-pre-update"
	h := filepath.Join(t.TempDir(), "home")
	snapDir := filepath.Join(h, "state-snapshots", snapID)
	t.Setenv("ICEBATH_HOME", h)
	livePath := filepath.Join(h, "auth.json")
	writeJSON(t, livePath, map[string]any{"providers": map[string]any{}, "sentinel": "live"})
	before := readText(t, livePath)
	writeText(t, filepath.Join(h, "config.yaml"), "model: live\n")
	writeText(t, filepath.Join(snapDir, "config.yaml"), "model: snapshot\n")
	writeText(t, filepath.Join(snapDir, "auth.json"), "{invalid-json")
	writeManifest(t, snapDir, map[string]any{"id": snapID}, "auth.json", "config.yaml")
	result := restoreSnapshot(t, snapID, h)
	if readText(t, livePath) != before || readText(t, filepath.Join(h, "config.yaml")) != "model: snapshot\n" || result {
		t.Fatal("refused auth store hidden by the config restore")
	}
	os.Remove(filepath.Join(snapDir, "auth.json"))
	writeText(t, filepath.Join(h, "config.yaml"), "model: live\n")
	if restoreSnapshot(t, snapID, h) {
		t.Fatal("missing listed auth.json reported success")
	}
	if readText(t, filepath.Join(h, "config.yaml")) != "model: snapshot\n" || readText(t, livePath) != before {
		t.Fatal("state after missing auth.json")
	}
}

func makeMarkerDB(t *testing.T, path, marker string) {
	t.Helper()
	sqliteExec(t, path, "PRAGMA journal_mode=WAL", "CREATE TABLE t (v TEXT)", "INSERT INTO t VALUES ('"+marker+"')")
}

func readMarker(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow("SELECT v FROM t LIMIT 1").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// This process's descriptors pointing at deleted files whose path contains needle
func ownDeletedFDs(needle string) []string {
	var out []string
	entries, _ := os.ReadDir("/proc/self/fd")
	for _, e := range entries {
		target, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err == nil && strings.Contains(target, needle) && strings.Contains(target, "(deleted)") {
			out = append(out, target)
		}
	}
	return out
}

func TestSafeRestoreFallbackRefusesUnderOwnLiveConnection(t *testing.T) {
	tmp := t.TempDir()
	dst, src := filepath.Join(tmp, "state.db"), filepath.Join(tmp, "snap.db")
	makeMarkerDB(t, src, "snapshot-good")
	makeMarkerDB(t, dst, "live-old")
	held, err := state.ConnectTracked(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	ctx := context.Background()
	for _, q := range []string{"PRAGMA journal_mode=WAL", "INSERT INTO t VALUES ('held-write')"} {
		if _, err := held.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	wal := dst + "-wal"
	walBefore, err := os.Stat(wal)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(dst, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(make([]byte, 100))
	f.Close()
	if ok, err := safeRestoreDB(ctx, src, dst); ok || err != nil {
		t.Fatalf("restore %v %v", ok, err)
	}
	walAfter, err := os.Stat(wal)
	if err != nil || !os.SameFile(walBefore, walAfter) {
		t.Fatal("held WAL generation replaced")
	}
	if ghosts := ownDeletedFDs(dst); len(ghosts) != 0 {
		t.Fatalf("deleted descriptors %v", ghosts)
	}
}

func TestSafeRestoreFallbackStillWorksWithoutHolder(t *testing.T) {
	tmp := t.TempDir()
	dst, src := filepath.Join(tmp, "projects.db"), filepath.Join(tmp, "snap.db")
	makeMarkerDB(t, src, "snapshot-good")
	makeMarkerDB(t, dst, "live-old")
	f, _ := os.OpenFile(dst, os.O_RDWR, 0)
	f.Write(make([]byte, 100))
	f.Close()
	if ok, err := safeRestoreDB(context.Background(), src, dst); !ok || err != nil {
		t.Fatalf("restore %v %v", ok, err)
	}
	if readMarker(t, dst) != "snapshot-good" {
		t.Fatal("marker not restored")
	}
}

func TestSafeRestoreRefusesUnreadableCanonicalEpoch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	logs := captureLog(t)
	dst, src := filepath.Join(tmp, "state.db"), filepath.Join(tmp, "snapshot.db")
	makeMarkerDB(t, src, "snapshot-good")
	writeText(t, dst, "unreadable canonical epoch")
	srcBefore := readText(t, src)
	if ok, err := safeRestoreDB(context.Background(), src, dst); ok || err != nil {
		t.Fatalf("restore %v %v", ok, err)
	}
	if readText(t, dst) != "unreadable canonical epoch" || readText(t, src) != srcBefore {
		t.Fatal("files changed")
	}
	if !strings.Contains(logs.String(), "separate output") {
		t.Fatal(logs.String())
	}
	os.Remove(dst)
	makeMarkerDB(t, dst, "snapshot-good")
	cfg := filepath.Join(tmp, "config.yaml")
	writeText(t, cfg, "model: snapshot-model\n")
	snapID := snapshotOf(t, tmp, "", 0)
	writeText(t, cfg, "model: current-model\n")
	writeText(t, dst, "unreadable canonical epoch")
	if restoreSnapshot(t, snapID, tmp) {
		t.Fatal("restore reported success over an unreadable epoch")
	}
	if readText(t, cfg) != "model: current-model\n" || readText(t, dst) != "unreadable canonical epoch" {
		t.Fatal("profile partly rolled back")
	}
}

// Home whose name needs URI escaping beside an existing default install that keeps the gateway alone
func integrityHome(t *testing.T, name string) (string, string) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, name)
	os.Mkdir(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	native := filepath.Join(tmp, "native")
	writeText(t, filepath.Join(native, "config.yaml"), "{}")
	useNativeDefault(t, native)
	noRealGatewayService(t, false, nil)
	return tmp, h
}

func integrityDatabase(t *testing.T, path string, count int) {
	t.Helper()
	stmts := []string{"CREATE TABLE sessions(id INTEGER PRIMARY KEY)", "CREATE TABLE messages(id INTEGER PRIMARY KEY)"}
	for n := range count {
		stmts = append(stmts, "INSERT INTO sessions VALUES ("+strconv.Itoa(n)+")", "INSERT INTO messages VALUES ("+strconv.Itoa(n)+")")
	}
	sqliteExec(t, path, stmts...)
}

// Restores source into the home through the snapshot or import entry, true on success
func integrityRestore(t *testing.T, entry, h, snapID, source string) (bool, string) {
	t.Helper()
	if entry == "snapshot" {
		return restoreSnapshot(t, snapID, h), ""
	}
	archive := filepath.Join(filepath.Dir(h), "backup.zip")
	writeZipFiles(t, archive, [2]string{source, "state.db"})
	code, out := runImport(t, archive, true)
	return code != 1, out
}

func assertNoDecoy(t *testing.T, tmp, name string) {
	t.Helper()
	allowed := map[string]bool{name: true, "native": true, "backup.zip": true, "hermes_test": true}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if !allowed[e.Name()] {
			t.Fatalf("decoy %s", e.Name())
		}
	}
}

var integrityHomeNames = []string{"x#y", "x%23y", "x y"}

func TestRestoreUsesLiteralPathsAndPreservesLiveConnection(t *testing.T) {
	for _, name := range integrityHomeNames {
		for _, entry := range []string{"snapshot", "import"} {
			t.Run(name+"/"+entry, func(t *testing.T) {
				tmp, h := integrityHome(t, name)
				target := filepath.Join(h, "state.db")
				integrityDatabase(t, target, 3)
				snapDir := filepath.Join(h, "state-snapshots", "saved")
				source := filepath.Join(snapDir, "state.db")
				os.MkdirAll(snapDir, 0o755)
				integrityDatabase(t, source, 1)
				writeManifest(t, snapDir, nil, "state.db")
				holder, err := state.ConnectTracked(target)
				if err != nil {
					t.Fatal(err)
				}
				func() {
					defer holder.Close()
					ok, out := integrityRestore(t, entry, h, "saved", source)
					if !ok {
						t.Fatalf("restore failed\n%s", out)
					}
					if entry == "import" && !strings.Contains(out, "3 session(s) / 3 message(s) -> 1 / 1") {
						t.Fatal(out)
					}
					var ids []int
					rows, err := holder.QueryContext(context.Background(), "SELECT id FROM messages")
					if err != nil {
						t.Fatal(err)
					}
					for rows.Next() {
						var id int
						rows.Scan(&id)
						ids = append(ids, id)
					}
					rows.Close()
					if !slices.Equal(ids, []int{0}) {
						t.Fatalf("holder sees %v", ids)
					}
					if got := tableRows(t, target, "SELECT id FROM messages"); !slices.Equal(got, []string{"0"}) {
						t.Fatalf("reopened sees %v", got)
					}
				}()
				assertNoDecoy(t, tmp, name)
			})
		}
	}
}

func TestCorruptSourceCannotReplaceAHealthyDatabase(t *testing.T) {
	for _, name := range integrityHomeNames {
		for _, entry := range []string{"snapshot", "import"} {
			for _, damage := range []string{"header", "btree", "truncated"} {
				t.Run(name+"/"+entry+"/"+damage, func(t *testing.T) {
					tmp, h := integrityHome(t, name)
					live := filepath.Join(h, "state.db")
					sqliteExec(t, live, "CREATE TABLE evidence(value TEXT)", "INSERT INTO evidence VALUES ('snapshot')")
					snapID := snapshotOf(t, h, "", 0)
					source := filepath.Join(h, "state-snapshots", snapID, "state.db")
					sqliteExec(t, live, "UPDATE evidence SET value='live'")
					pageSize := tableRows(t, source, "PRAGMA page_size")[0]
					rootPage := tableRows(t, source, "SELECT rootpage FROM sqlite_master WHERE name='evidence'")[0]
					ps, rp := atoiT(t, pageSize), atoiT(t, rootPage)
					contents := []byte(readText(t, source))
					switch damage {
					case "header":
						copy(contents[:16], "not a database!!")
					case "btree":
						contents[(rp-1)*ps] = 0
					default:
						contents = contents[:ps]
					}
					if err := os.WriteFile(source, contents, 0o644); err != nil {
						t.Fatal(err)
					}
					if VerifySQLiteIntegrity(source, IntegrityOptions{}).Valid {
						t.Fatal("damaged source verified")
					}
					before := readText(t, live)
					stBefore, _ := os.Stat(live)
					ok, out := integrityRestore(t, entry, h, snapID, source)
					if ok {
						t.Fatal("damaged source restored")
					}
					if entry == "import" && !strings.Contains(out, "failed its integrity check") {
						t.Fatal(out)
					}
					stAfter, _ := os.Stat(live)
					if readText(t, live) != before || !os.SameFile(stBefore, stAfter) {
						t.Fatal("live database touched")
					}
					if got := tableRows(t, live, "SELECT value FROM evidence"); !slices.Equal(got, []string{"live"}) {
						t.Fatalf("evidence %v", got)
					}
					assertNoDecoy(t, tmp, name)
				})
			}
		}
	}
}

func atoiT(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRoundTripBackupThenImport(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "source", ".icebath")
	os.MkdirAll(src, 0o755)
	makeHomeTree(t, src)
	useHomes(t, src, filepath.Join(tmp, "source"))
	home.ResetKeyCache()
	noRealGatewayService(t, false, nil)
	outZip := filepath.Join(tmp, "roundtrip.zip")
	if ok, out, err := runBackupNS(t, map[string]any{"output": outZip}); !ok || err != nil {
		t.Fatalf("backup %v %v\n%s", ok, err, out)
	}
	dst := filepath.Join(tmp, "dest", ".icebath")
	os.MkdirAll(dst, 0o755)
	useHomes(t, dst, filepath.Join(tmp, "dest"))
	home.ResetKeyCache()
	runImport(t, outZip, true)
	if readText(t, filepath.Join(dst, "config.yaml")) != "model:\n  provider: openrouter\n" || readText(t, filepath.Join(dst, ".env")) != "OPENROUTER_API_KEY=sk-test-123\n" {
		t.Fatal("config not restored")
	}
	for _, rel := range []string{"skills/my-skill/SKILL.md", "profiles/coder/config.yaml", "sessions/abc123.json", "logs/agent.log"} {
		if !exists(filepath.Join(dst, rel)) {
			t.Fatalf("%s missing", rel)
		}
	}
	for _, rel := range []string{"icebath-agent", "plugins/__pycache__", "gateway.pid"} {
		if exists(filepath.Join(dst, rel)) {
			t.Fatalf("%s present", rel)
		}
	}
}

func TestImportRestoresTheSessionStoreWithItsMessageUids(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	makeHomeTree(t, h)
	ctx := context.Background()
	messageUIDs := func() []string {
		db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		msgs, err := db.GetMessagesAsConversation(ctx, "s", state.ConversationOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var uids []string
		for _, m := range msgs {
			uid, _ := m["message_uid"].(string)
			uids = append(uids, uid)
		}
		return uids
	}
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "cli", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []map[string]any{{"role": "user", "content": "q"}, {"role": "assistant", "content": "a"}} {
		if _, err := db.AppendMessage(ctx, "s", m, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	uids := messageUIDs()
	if len(uids) != 2 || len(uids[0]) != 32 || len(uids[1]) != 32 {
		t.Fatalf("uids %v", uids)
	}
	useHomes(t, h, tmp)
	home.ResetKeyCache()
	noRealGatewayService(t, false, nil)
	outZip := filepath.Join(tmp, "backup.zip")
	if ok, out, err := runBackupNS(t, map[string]any{"output": outZip}); !ok || err != nil {
		t.Fatalf("backup %v %v\n%s", ok, err, out)
	}
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm"} {
		os.Remove(filepath.Join(h, name))
	}
	if code, out := runImport(t, outZip, true); code != 0 {
		t.Fatalf("import %d\n%s", code, out)
	}
	if got := messageUIDs(); !slices.Equal(got, uids) {
		t.Fatalf("uids %v, want %v", got, uids)
	}
}
