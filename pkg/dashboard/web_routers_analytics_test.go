package dashboard

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Log record a case inspects, its level, logger name and whether it carries a traceback
type capturedRecord struct {
	level   slog.Level
	logger  string
	message string
	excInfo bool
}

// Handler collecting every record at or above its level, the pytest caplog fixture
type captureHandler struct {
	mu    *sync.Mutex
	recs  *[]capturedRecord
	level slog.Level
	attrs []slog.Attr
}

func (h *captureHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{level: r.Level, message: r.Message}
	visit := func(a slog.Attr) bool {
		switch a.Key {
		case "logger":
			rec.logger = a.Value.String()
		case "exc_info":
			rec.excInfo = true
		}
		return true
	}
	for _, a := range h.attrs {
		visit(a)
	}
	r.Attrs(visit)
	h.mu.Lock()
	*h.recs = append(*h.recs, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr{}, h.attrs...), as...)
	return &c
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// Captures the default logger records at the level until the case ends
func captureLogs(t *testing.T, level slog.Level) (records func() []capturedRecord, reset func()) {
	var mu sync.Mutex
	recs := []capturedRecord{}
	prev := slog.Default()
	slog.SetDefault(slog.New(&captureHandler{mu: &mu, recs: &recs, level: level}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []capturedRecord {
			mu.Lock()
			defer mu.Unlock()
			return append([]capturedRecord(nil), recs...)
		}, func() {
			mu.Lock()
			recs = recs[:0]
			mu.Unlock()
		}
}

// Warnings of the web server loggers among the records
func webServerWarnings(recs []capturedRecord) []capturedRecord {
	var out []capturedRecord
	for _, r := range recs {
		if r.level >= slog.LevelWarn && strings.HasPrefix(r.logger, "hermes_cli.web_server") {
			out = append(out, r)
		}
	}
	return out
}

// Session store of the home with one session whose image is then overwritten past the header
func malformedStateDB(t *testing.T, h string) string {
	t.Helper()
	dbPath := filepath.Join(h, "state.db")
	db, err := state.NewSessionDB(context.Background(), dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"-wal", "-shm"} {
		os.Remove(dbPath + side)
	}
	f, err := os.OpenFile(dbPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xff}, 4096-100), 100); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

// Fresh corrupt store warning gate restored after the case
func resetCorruptStoreGate(t *testing.T) {
	corruptStoreWarnedMu.Lock()
	saved := corruptStoreWarnedAt
	corruptStoreWarnedAt = map[string]float64{}
	corruptStoreWarnedMu.Unlock()
	t.Cleanup(func() {
		corruptStoreWarnedMu.Lock()
		corruptStoreWarnedAt = saved
		corruptStoreWarnedMu.Unlock()
	})
}

func TestCorruptStorePollsReturnStatusAndWarnOncePerInterval(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	dbPath := malformedStateDB(t, tmp)
	resetCorruptStoreGate(t)
	app := NewFastAPI("Icebath Agent", "0", nil, nil)
	app.IncludeRouter(analyticsRouter(), "")
	client := newTestClient(t, app)

	records, reset := captureLogs(t, slog.LevelDebug)
	responses := []*testResponse{client.get("/api/analytics/usage?days=7"), client.get("/api/analytics/usage?days=7"),
		client.get("/api/analytics/models?days=7")}
	for _, r := range responses {
		if r.Status != 503 {
			t.Fatalf("status %d %s", r.Status, r.Body)
		}
		if detail, _ := jsonObject(t, r)["detail"].(map[string]any); detail["error"] != "state_db_corrupt" {
			t.Fatalf("body %s", r.Body)
		}
	}
	if warnings := webServerWarnings(records()); len(warnings) != 1 {
		t.Fatalf("warnings %v", warnings)
	}
	for _, r := range records() {
		if r.excInfo {
			t.Fatalf("traceback for a known corrupt store: %v", r)
		}
	}
	if st, err := os.Stat(dbPath); err != nil || st.Size() == 0 {
		t.Fatalf("dashboard must never quarantine the file: %v", err)
	}

	corruptStoreWarnedMu.Lock()
	corruptStoreWarnedAt[dbPath] -= corruptStoreWarnIntervalS + 1
	corruptStoreWarnedMu.Unlock()
	reset()
	if r := client.get("/api/analytics/usage?days=7"); r.Status != 503 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if warnings := webServerWarnings(records()); len(warnings) != 1 {
		t.Fatalf("warnings after the interval %v", warnings)
	}
}

// Test application with one session in the store, authorized by the session token as a bearer
func clampClient(t *testing.T) (*testClient, string) {
	app := dashboardTestApp(t)
	db, err := state.NewSessionDB(context.Background(), filepath.Join(os.Getenv("ICEBATH_HOME"), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "clamp-session", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return newTestClient(t, app), "Bearer " + SessionToken()
}

func TestOutOfRangeDaysRejected(t *testing.T) {
	for _, days := range []int{0, -5, 100_000} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			c, auth := clampClient(t)
			if r := c.get(fmt.Sprintf("/api/analytics/usage?days=%d", days), "authorization", auth); r.Status != 422 {
				t.Fatalf("status %d %s", r.Status, r.Body)
			}
		})
	}
}

func TestInRangeDaysAccepted(t *testing.T) {
	c, auth := clampClient(t)
	if r := c.get("/api/analytics/usage?days=30", "authorization", auth); r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
}

// Transport counting the session.info frames a broadcast writes
type infoCountingTransport struct {
	mu    sync.Mutex
	infos int
}

func (tr *infoCountingTransport) Write(obj *pyjson.Object) bool {
	if params, _ := obj.GetOr("params", nil).(*pyjson.Object); params != nil && params.GetOr("type", nil) == "session.info" {
		tr.mu.Lock()
		tr.infos++
		tr.mu.Unlock()
	}
	return true
}

func (tr *infoCountingTransport) Close() {}

func (tr *infoCountingTransport) count() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.infos
}

func (tr *infoCountingTransport) clear() {
	tr.mu.Lock()
	tr.infos = 0
	tr.mu.Unlock()
}

// Live gateway session with a compute host mirror whose transport records each broadcast
func broadcastCalls(t *testing.T) *infoCountingTransport {
	tr := &infoCountingTransport{}
	sid := "broadcast-probe"
	tuigateway.SetSessionRecord(sid, tuigateway.NewSession(map[string]any{
		"session_key": sid, "transport": tr, "_metadata_mirror": map[string]any{"model": "probe"},
	}))
	t.Cleanup(func() { tuigateway.SetSessionRecord(sid, nil) })
	return tr
}

func putRawConfig(t *testing.T, c sessionClient, yamlText string) {
	t.Helper()
	if r := c.send("PUT", "/api/config/raw", map[string]any{"yaml_text": yamlText}); r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
}

func TestRawSaveDeletingApprovalsBlockBroadcasts(t *testing.T) {
	c := newSessionClient(t)
	calls := broadcastCalls(t)
	putRawConfig(t, c, "approvals:\n  mode: 'off'\n")
	calls.clear()
	putRawConfig(t, c, "display:\n  skin: default\n")
	if calls.count() == 0 {
		t.Fatal("deleting the approvals block changes the effective mode and must broadcast")
	}
}

func TestRawSaveApprovalsChangeBroadcasts(t *testing.T) {
	c := newSessionClient(t)
	calls := broadcastCalls(t)
	putRawConfig(t, c, "approvals:\n  mode: 'off'\n")
	if calls.count() != 1 {
		t.Fatalf("broadcasts %d", calls.count())
	}
}

func TestRawSaveWithoutApprovalsChangeDoesNotBroadcast(t *testing.T) {
	c := newSessionClient(t)
	calls := broadcastCalls(t)
	putRawConfig(t, c, "display:\n  skin: default\n")
	if calls.count() != 0 {
		t.Fatalf("broadcasts %d", calls.count())
	}
}

// Session store of the test home
func testHomeSessionDB(t *testing.T) *state.SessionDB {
	db, err := state.NewSessionDB(context.Background(), filepath.Join(os.Getenv("ICEBATH_HOME"), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestAnalyticsUsageIncludesSkillBreakdown(t *testing.T) {
	c := newSessionClient(t)
	db := testHomeSessionDB(t)
	ctx := context.Background()
	if err := db.CreateSession(state.NewSession{ID: "skills-analytics-test", Source: "cli", Model: "anthropic/claude-sonnet-4"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTokenCounts("skills-analytics-test", state.TokenCountDelta{InputTokens: 120, OutputTokens: 45}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, "skills-analytics-test", map[string]any{
		"role": "assistant", "content": "Loading and updating skills.",
		"tool_calls": []any{
			map[string]any{"function": map[string]any{"name": "skill_view", "arguments": `{"name":"github-pr-workflow"}`}},
			map[string]any{"function": map[string]any{"name": "skill_manage", "arguments": `{"name":"github-code-review"}`}},
		},
	}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	r := c.get("/api/analytics/usage?days=7")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	skills := jsonObject(t, r)["skills"].(map[string]any)
	want := map[string]any{"total_skill_loads": int64(1), "total_skill_edits": int64(1), "total_skill_actions": int64(2), "distinct_skills_used": int64(2)}
	if !reflect.DeepEqual(skills["summary"], want) {
		t.Fatalf("summary %v", skills["summary"])
	}
	top := skills["top_skills"].([]any)
	if len(top) != 2 {
		t.Fatalf("top skills %v", top)
	}
	first := top[0].(map[string]any)
	if first["skill"] != "github-pr-workflow" || !pyrepr.Equal(first["view_count"], int64(1)) || !pyrepr.Equal(first["manage_count"], int64(0)) ||
		!pyrepr.Equal(first["total_count"], int64(1)) || first["last_used_at"] == nil {
		t.Fatalf("top skill %v", first)
	}
}

// Daily buckets of sessions started at naive local times of a zone, read with the process zone switched
func dailyForLocalStarts(t *testing.T, c sessionClient, tzName string, localStarts []time.Time) map[string]any {
	t.Helper()
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		t.Fatal(err)
	}
	db := testHomeSessionDB(t)
	ctx := context.Background()
	for i, local := range localStarts {
		id := fmt.Sprintf("day-bucket-%d", i)
		if err := db.CreateSession(state.NewSession{ID: id, Source: "cli"}); err != nil {
			t.Fatal(err)
		}
		at := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), 0, loc)
		if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
			_, err := conn.ExecContext(ctx, "UPDATE sessions SET started_at = ? WHERE id = ?", float64(at.UnixNano())/1e9, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	t.Cleanup(func() {
		if err := pysqlite.Tzset(); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("TZ", tzName)
	if err := pysqlite.Tzset(); err != nil {
		t.Fatal(err)
	}
	r := c.get("/api/analytics/usage?days=365")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	out := map[string]any{}
	for _, row := range jsonObject(t, r)["daily"].([]any) {
		m := row.(map[string]any)
		out[m["day"].(string)] = m["sessions"]
	}
	return out
}

func TestAnalyticsDailyBucketsUseLocalDay(t *testing.T) {
	c := newSessionClient(t)
	y := time.Now().AddDate(0, 0, -1)
	yesterday := time.Date(y.Year(), y.Month(), y.Day(), 2, 0, 0, 0, time.UTC)
	daily := dailyForLocalStarts(t, c, "Asia/Kolkata", []time.Time{yesterday})
	if !reflect.DeepEqual(daily, map[string]any{yesterday.Format("2006-01-02"): int64(1)}) {
		t.Fatalf("daily %v", daily)
	}
}

func TestAnalyticsDailyBucketsFollowDstOffsetPerSession(t *testing.T) {
	c := newSessionClient(t)
	var starts []time.Time
	for _, month := range []time.Month{time.January, time.July} {
		for n := 2; n < 360; n++ {
			d := time.Now().AddDate(0, 0, -n)
			if d.Month() == month {
				starts = append(starts, time.Date(d.Year(), d.Month(), d.Day(), 23, 30, 0, 0, time.UTC))
				break
			}
		}
	}
	daily := dailyForLocalStarts(t, c, "America/New_York", starts)
	want := map[string]any{}
	for _, s := range starts {
		want[s.Format("2006-01-02")] = int64(1)
	}
	if !reflect.DeepEqual(daily, want) {
		t.Fatalf("daily %v want %v", daily, want)
	}
}
