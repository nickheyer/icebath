package state

import (
	"context"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func objText(t *testing.T, o *pyjson.Object) string {
	t.Helper()
	s, err := pyjson.Dumps(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSearchProgressPayloads(t *testing.T) {
	if got := objText(t, (&backfillProgress{phase: "backfill", total: 3, indexed: 2}).info()); got != `{"phase": "backfill", "percent": 66, "indexed": 2, "total": 3}` {
		t.Fatalf("backfill = %s", got)
	}
	if got := objText(t, (&backfillProgress{phase: "backfill_cjk", total: 0, indexed: 5}).info()); got != `{"phase": "backfill_cjk", "percent": 100, "indexed": 0, "total": 0}` {
		t.Fatalf("empty backfill = %s", got)
	}
	td := newTeardownProgress()
	if !td.counted {
		t.Fatal("teardown counts by default")
	}
	td.table, td.tableIndex, td.tableCount, td.total, td.drained = "fts_v22_trash_x", 1, 2, 7, 9
	if got := objText(t, td.info()); got != `{"phase": "teardown", "percent": 100, "indexed": 9, "total": 7, "table": "fts_v22_trash_x", "table_index": 1, "table_count": 2}` {
		t.Fatalf("teardown = %s", got)
	}
	saved := monotonicSeconds
	now := 10.0
	monotonicSeconds = func() float64 { return now }
	defer func() { monotonicSeconds = saved }()
	v := newVacuumProgress(4096, 1024)
	now = 12.5
	if got := objText(t, v.info()); got != `{"phase": "vacuum", "percent": 0, "indexed": 0, "total": 0, "elapsed": 2.5, "size_bytes": 4096, "estimated_bytes": 1024}` {
		t.Fatalf("vacuum = %s", got)
	}
	if floorDiv(-7, 2) != -4 || floorDiv(7, 2) != 3 {
		t.Fatal("floor division")
	}
}

func TestHolderWatchRescansOnTheSideGoroutine(t *testing.T) {
	var held atomic.Bool
	var scans atomic.Int32
	held.Store(true)
	w, err := newHolderWatch(func() (bool, error) { scans.Add(1); return held.Load(), nil }, 0.01).enter()
	if err != nil {
		t.Fatal(err)
	}
	if !w.held() {
		t.Fatal("a holder present at entry paces from the first chunk")
	}
	held.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for w.held() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	w.exit()
	if w.held() || scans.Load() < 2 {
		t.Fatalf("the side scan refreshes the verdict: held=%v scans=%d", w.held(), scans.Load())
	}
	after := scans.Load()
	time.Sleep(30 * time.Millisecond)
	if scans.Load() != after {
		t.Fatal("exit stops the side scan")
	}
}

func TestSearchSlowMSReadsTheSessionsSetting(t *testing.T) {
	ctx := context.Background()
	for value, want := range map[string]float64{"": 1000, "  ": 1000, "250": 250, " 1_5.5 ": 15.5, "abc": 1000, "0": 0, "inf": math.Inf(1)} {
		t.Setenv("ICEBATH_SEARCH_SLOW_MS", value)
		if got, err := searchSlowMS(ctx); err != nil || got != want {
			t.Errorf("ICEBATH_SEARCH_SLOW_MS=%q = %v %v, want %v", value, got, err, want)
		}
	}
}

func TestSearchQueryHelpers(t *testing.T) {
	if !fts5SpecialRE.MatchString(`a\b`) || !fts5SpecialRE.MatchString("x'y") || fts5SpecialRE.MatchString("a%b*c-d.e") {
		t.Fatal("special character class")
	}
	if got := likeTokenRE.FindAllString("\"two words\" alpha　beta gamma", -1); !slices.Equal(got, []string{`"two words"`, "alpha", "beta", "gamma"}) {
		t.Fatalf("like tokens split on python whitespace: %q", got)
	}
	if got := nonOperatorTokens("cats AND dogs or\tnot birds"); !slices.Equal(got, []string{"cats", "dogs", "birds"}) {
		t.Fatalf("non operator tokens = %q", got)
	}
	if got := quoteFTSTokens(`say "hi" and c++`); got != `"say" """hi""" and "c++"` {
		t.Fatalf("quoted = %s", got)
	}
	if got := likeParams(`50%_off\`); !slices.Equal(got, []string{`%50\%\_off\\%`, `%50\%\_off\\%`, `%50\%\_off\\%`}) {
		t.Fatalf("like params = %q", got)
	}
	for in, want := range map[string]string{
		"nimb":         "nimb",
		"일본* AND 구글**": "일본 AND 구글",
		"*** 항목*":      "*** 항목",
		"   ":          "   ",
		"*":            "*",
	} {
		if got := stripCJKWildcards(in); got != want {
			t.Errorf("stripCJKWildcards(%q) = %q, want %q", in, got, want)
		}
	}
	if !searchIsCJK(0x3000) || !searchIsCJK(0x2A6DF) || searchIsCJK(0x2A6E0) || searchIsCJK('a') {
		t.Fatal("search cjk ranges")
	}
	decoded, err := pyjson.Loads(`[{"type": "text", "text": " hello "}, {"type": "image_url"}, {"type": "text", "text": ""}, {"type": "text", "text": "world"}, "loose"]`)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := flattenText(decoded); err != nil || got != "hello  world" {
		t.Fatalf("flatten = %q %v", got, err)
	}
	images, _ := pyjson.Loads(`[{"type": "image_url"}]`)
	if got, _ := flattenText(images); got != "[multimodal content]" {
		t.Fatalf("marker = %q", got)
	}
	if got, _ := flattenText("plain"); got != "plain" {
		t.Fatal("strings pass through")
	}
	if got, _ := flattenText(int64(3)); got != "" {
		t.Fatal("other values flatten to empty")
	}
	bad, _ := pyjson.Loads(`[{"type": "text", "text": "a"}, {"type": "text", "text": 5}]`)
	if _, err := flattenText(bad); err == nil || !strings.Contains(err.Error(), "sequence item 1: expected str instance, int found") {
		t.Fatalf("join type error = %v", err)
	}
	for _, c := range []struct {
		value any
		want  string
	}{{5, ""}, {int64(1), ""}, {true, "limit must be an integer"}, {"5", "limit must be an integer"}, {0, "limit must be greater than zero"}, {-3, "limit must be greater than zero"}} {
		err := positiveInt("limit", c.value)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("positiveInt(%v) = %v, want %q", c.value, err, c.want)
		}
	}
}

func TestSearchSQLBuilders(t *testing.T) {
	after, before := int64(100), int64(200)
	where, params := searchFilterClauses([]string{"x = 1"}, []any{"q"}, searchFilter{
		sourceFilter: []string{"cli", "telegram"}, excludeSources: []string{}, roleFilter: []string{"user"}, afterTS: &after, beforeTS: &before,
	})
	wantWhere := []string{"x = 1", "(m.active = 1 OR m.compacted = 1)", "COALESCE(m.display_kind, '') <> 'hidden'",
		"s.source IN (?,?)", "s.source NOT IN ()", "m.role IN (?)", "s.started_at >= ?", "s.started_at < ?"}
	if !slices.Equal(where, wantWhere) || !slices.Equal(params, []any{"q", "cli", "telegram", "user", int64(100), int64(200)}) {
		t.Fatalf("filters = %q %v", where, params)
	}
	where, params = searchFilterClauses(nil, nil, searchFilter{includeInactive: true, roleFilter: []string{}})
	if !slices.Equal(where, []string{"COALESCE(m.display_kind, '') <> 'hidden'"}) || len(params) != 0 {
		t.Fatalf("minimal filters = %q %v", where, params)
	}
	got := searchSelectSQL(likeSnippetSQL, "messages m", []string{"a", "b"}, ftsOrderBy["newest"], "LIMIT ?")
	if !strings.Contains(got, "SELECT m.id, m.session_id, m.role,\n                   substr(m.content") ||
		!strings.Contains(got, "WHERE a AND b\n            ORDER BY m.timestamp DESC, rank\n            LIMIT ?\n        ") {
		t.Fatalf("select = %q", got)
	}
	conn, _ := schemaConn(t)
	ctx := context.Background()
	mustExecConn(t, conn, "INSERT INTO sessions (id, source, started_at) VALUES ('s1', 'cli', 150)")
	addMessage(t, conn, "user", "50% off", "")
	addMessage(t, conn, "user", "before", "")
	addMessage(t, conn, "user", "after", "")
	where, params = searchFilterClauses([]string{likeAnyColumnSQL}, []any{}, searchFilter{sourceFilter: []string{"cli"}, afterTS: &after})
	args := append([]any{"%"}, stringArgs(likeParams("50%"))...)
	rows, err := fetchOne(ctx, conn, "SELECT COUNT(*) FROM ("+searchSelectSQL(likeSnippetSQL, "messages m", where, "", "")+")", append(args, params...)...)
	if err != nil || rows[0] != int64(1) {
		t.Fatalf("like route runs: %v %v", rows, err)
	}
	ctxRows, err := fetchOne(ctx, conn, "SELECT COUNT(*) FROM ("+strings.Replace(contextWindowSQL, "{ids}", "2", 1)+")")
	if err != nil || ctxRows[0] != int64(3) {
		t.Fatalf("context window = %v %v", ctxRows, err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta (key, value) VALUES ('a', '1'), ('b', '2'), ('c', '3')"); err != nil {
		t.Fatal(err)
	}
	if err := deleteMeta(ctx, conn, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if row, err := metaRow(ctx, conn, "a"); err != nil || row != nil {
		t.Fatalf("deleted = %v %v", row, err)
	}
	if row, err := metaRow(ctx, conn, "c"); err != nil || row[0] != "3" {
		t.Fatalf("kept = %v %v", row, err)
	}
	if !strings.Contains(likeCoalescedColumnSQL, `COALESCE(m.tool_calls, '') LIKE ? ESCAPE '\')`) || !slices.Contains(likeSkipTokens, "NEAR") {
		t.Fatal("coalesced LIKE columns and skip tokens")
	}
}
