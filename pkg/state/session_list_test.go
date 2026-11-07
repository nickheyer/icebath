package state

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func projBuildChain(t *testing.T, db *SessionDB, t0 float64) {
	t.Helper()
	readSession(t, db, NewSession{ID: "root1", Source: "cli"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0, "root1")
	readAppend(t, db, "root1", map[string]any{"role": "user", "content": "help me refactor auth"})
	readSession(t, db, NewSession{ID: "delegate1", Source: "cli", ParentSessionID: "root1"})
	profileExec(t, db, "UPDATE sessions SET started_at=?, ended_at=? WHERE id=?", t0+600, t0+650, "delegate1")
	readAppend(t, db, "delegate1", map[string]any{"role": "user", "content": "delegate task"})
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason=? WHERE id=?", t0+1800, "compression", "root1")
	readSession(t, db, NewSession{ID: "mid1", Source: "cli", ParentSessionID: "root1"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0+1801, "mid1")
	readAppend(t, db, "mid1", map[string]any{"role": "user", "content": "continuing"})
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason=? WHERE id=?", t0+3600, "compression", "mid1")
	readSession(t, db, NewSession{ID: "tip1", Source: "cli", ParentSessionID: "mid1"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0+3601, "tip1")
	readAppend(t, db, "tip1", map[string]any{"role": "user", "content": "latest message"})
}

func projSecondChain(t *testing.T, db *SessionDB, t0 float64, tipText string) {
	t.Helper()
	readSession(t, db, NewSession{ID: "root2", Source: "cli"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0+100, "root2")
	readAppend(t, db, "root2", map[string]any{"role": "user", "content": "second conversation start"})
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason=? WHERE id=?", t0+200, "compression", "root2")
	readSession(t, db, NewSession{ID: "tip2", Source: "cli", ParentSessionID: "root2"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0+201, "tip2")
	readAppend(t, db, "tip2", map[string]any{"role": "user", "content": tipText})
}

func projSolo(t *testing.T, db *SessionDB) {
	t.Helper()
	readSession(t, db, NewSession{ID: "solo", Source: "cli"})
	readAppend(t, db, "solo", map[string]any{"role": "user", "content": "standalone"})
}

func projList(t *testing.T, db *SessionDB, o ListSessionsRichOptions) []*pyjson.Object {
	t.Helper()
	rows, err := db.ListSessionsRich(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func projCLI(t *testing.T, db *SessionDB) []*pyjson.Object {
	t.Helper()
	limit := 20
	return projList(t, db, ListSessionsRichOptions{Source: "cli", Limit: &limit})
}

func projRow(t *testing.T, rows []*pyjson.Object, id string) *pyjson.Object {
	t.Helper()
	for _, r := range rows {
		if rowValue(r, "id") == id {
			return r
		}
	}
	t.Fatalf("row %s missing from %v", id, projIDs(rows))
	return nil
}

func projIDs(rows []*pyjson.Object) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		out[pyStr(rowValue(r, "id"))] = true
	}
	return out
}

func projAbsent(t *testing.T, row *pyjson.Object, key string) {
	t.Helper()
	if v, ok := row.Get(key); ok && v != nil {
		t.Fatalf("%s = %v on %v", key, v, rowValue(row, "id"))
	}
}

func TestListServesFullLineageIDsForProjectedRows(t *testing.T) {
	db := usageDB(t)
	projBuildChain(t, db, ago(1))
	projSolo(t, db)
	rows := projCLI(t, db)
	if got := rowValue(projRow(t, rows, "tip1"), "_lineage_ids"); !reflect.DeepEqual(got, []any{"root1", "mid1", "tip1"}) {
		t.Fatalf("lineage ids %v", got)
	}
	projAbsent(t, projRow(t, rows, "solo"), "_lineage_ids")
}

func TestListLabelsProjectedContinuationKind(t *testing.T) {
	db := usageDB(t)
	projBuildChain(t, db, ago(1))
	projSolo(t, db)
	readSession(t, db, NewSession{ID: "branchy", Source: "cli", ParentSessionID: "root1", ModelConfig: map[string]any{"_branched_from": "root1"}})
	rows := projCLI(t, db)
	if got := rowValue(projRow(t, rows, "tip1"), "continuation_kind"); got != "compression" {
		t.Fatalf("continuation kind %v", got)
	}
	projAbsent(t, projRow(t, rows, "solo"), "continuation_kind")
	projAbsent(t, projRow(t, rows, "branchy"), "continuation_kind")
}

func TestListKeepsLiveTipCarryingParentLink(t *testing.T) {
	db := usageDB(t)
	t0 := ago(1)
	readSession(t, db, NewSession{ID: "seg-a", Source: "cli"})
	readAppend(t, db, "seg-a", map[string]any{"role": "user", "content": "earlier days"})
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason='compression' WHERE id=?", t0+10, "seg-a")
	readSession(t, db, NewSession{ID: "seg-prev", Source: "cli", ParentSessionID: "seg-a"})
	readAppend(t, db, "seg-prev", map[string]any{"role": "user", "content": "restored segment"})
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason='compression' WHERE id=?", t0+20, "seg-prev")
	readSession(t, db, NewSession{ID: "live-tip", Source: "cli", ParentSessionID: "seg-prev"})
	readAppend(t, db, "live-tip", map[string]any{"role": "user", "content": "still talking here"})
	ids := projIDs(projCLI(t, db))
	if !ids["live-tip"] || ids["seg-a"] {
		t.Fatalf("listed %v", ids)
	}
}

func TestListSurfacesTipForCompressedRoot(t *testing.T) {
	db := usageDB(t)
	projBuildChain(t, db, ago(1))
	projSolo(t, db)
	rows := projCLI(t, db)
	ids := projIDs(rows)
	if !ids["tip1"] || !ids["solo"] || ids["root1"] || ids["mid1"] || ids["delegate1"] {
		t.Fatalf("listed %v", ids)
	}
	tip := projRow(t, rows, "tip1")
	if rowValue(tip, "_lineage_root_id") != "root1" || !strings.HasPrefix(pyStr(rowValue(tip, "preview")), "latest message") {
		t.Fatalf("tip row %v", tip)
	}
	if rowValue(tip, "ended_at") != nil || rowValue(tip, "end_reason") != nil {
		t.Fatalf("tip not live %v %v", rowValue(tip, "ended_at"), rowValue(tip, "end_reason"))
	}
}

func TestListProjectsMultipleIndependentChainsInOneCall(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	t0 := ago(2)
	projBuildChain(t, db, t0)
	projSecondChain(t, db, t0, "second conversation continuation")
	if _, err := db.UpdateSessionCwdClaim(ctx, "tip2", "/tmp/workspaces/second", "", "", false); err != nil {
		t.Fatal(err)
	}
	rows := projCLI(t, db)
	ids := projIDs(rows)
	if ids["root1"] || ids["root2"] || !ids["tip1"] || !ids["tip2"] {
		t.Fatalf("listed %v", ids)
	}
	tip1, tip2 := projRow(t, rows, "tip1"), projRow(t, rows, "tip2")
	if rowValue(tip1, "_lineage_root_id") != "root1" || !strings.HasPrefix(pyStr(rowValue(tip1, "preview")), "latest message") {
		t.Fatalf("tip1 %v", tip1)
	}
	if rowValue(tip2, "_lineage_root_id") != "root2" || !strings.HasPrefix(pyStr(rowValue(tip2, "preview")), "second conversation continuation") {
		t.Fatalf("tip2 %v", tip2)
	}
	if rowValue(tip2, "cwd") != "/tmp/workspaces/second" {
		t.Fatalf("tip2 cwd %v", rowValue(tip2, "cwd"))
	}
}

func TestListBatchesTipRowFetchIntoOneQuery(t *testing.T) {
	db := usageDB(t)
	t0 := ago(2)
	projBuildChain(t, db, t0)
	projSecondChain(t, db, t0, "second continuation")
	var batchCalls [][]string
	var singleCalls []string
	db.richRowSeams.batch = func(ctx context.Context, ids []string, compactRows bool) (map[string]*pyjson.Object, error) {
		batchCalls = append(batchCalls, append([]string(nil), ids...))
		return db.readSessionRichRowsBatch(ctx, ids, compactRows)
	}
	db.richRowSeams.single = func(ctx context.Context, id string, compactRows bool) (*pyjson.Object, error) {
		singleCalls = append(singleCalls, id)
		return db.readSessionRichRow(ctx, id, compactRows)
	}
	if rows := projCLI(t, db); len(rows) < 2 {
		t.Fatalf("rows %d", len(rows))
	}
	if len(batchCalls) != 1 || len(singleCalls) != 0 {
		t.Fatalf("batch %v single %v", batchCalls, singleCalls)
	}
	got := append([]string(nil), batchCalls[0]...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"tip1", "tip2"}) {
		t.Fatalf("batched %v", got)
	}
}

func TestListHandlesBrokenChainGracefully(t *testing.T) {
	db := usageDB(t)
	t0 := ago(100.0 / 3600)
	readSession(t, db, NewSession{ID: "orphan", Source: "cli"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0, "orphan")
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason=? WHERE id=?", t0+10, "compression", "orphan")
	limit := 10
	row := projRow(t, projList(t, db, ListSessionsRichOptions{Source: "cli", Limit: &limit}), "orphan")
	if _, ok := row.Get("_lineage_root_id"); ok {
		t.Fatalf("projected %v", row)
	}
	if rowValue(row, "end_reason") != "compression" {
		t.Fatalf("end reason %v", rowValue(row, "end_reason"))
	}
}

func TestProjectedTipInheritsRootTitleWhenUntitled(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	t0 := ago(1)
	readSession(t, db, NewSession{ID: "root", Source: "cli"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0, "root")
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason='compression' WHERE id=?", t0+100, "root")
	readSession(t, db, NewSession{ID: "tip", Source: "cli", ParentSessionID: "root"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0+200, "tip")
	if _, err := db.SetSessionTitle(ctx, "root", "Bot Chat"); err != nil {
		t.Fatal(err)
	}
	limit := 50
	rows := projList(t, db, ListSessionsRichOptions{Limit: &limit, OrderByLastActive: true, SearchQuery: "Bot Chat"})
	var got [][3]any
	for _, r := range rows {
		got = append(got, [3]any{rowValue(r, "id"), rowValue(r, "title"), rowValue(r, "_lineage_root_id")})
	}
	if !reflect.DeepEqual(got, [][3]any{{"tip", "Bot Chat", "root"}}) {
		t.Fatalf("rows %v", got)
	}
}

func TestRichListSessionKeyScopesSearchAndProjectsCompression(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	lane := "agent:main:telegram:dm:lane"
	title := func(id, v string) {
		if _, err := db.SetSessionTitle(ctx, id, v); err != nil {
			t.Fatal(err)
		}
	}
	readSession(t, db, NewSession{ID: "lane_root", Source: "telegram", SessionKey: lane, UserID: "lane-user", ChatID: "lane"})
	title("lane_root", "Needle root")
	if err := db.EndSession("lane_root", "compression"); err != nil {
		t.Fatal(err)
	}
	readSession(t, db, NewSession{ID: "lane_tip", Source: "telegram", SessionKey: lane, UserID: "lane-user", ChatID: "lane", ParentSessionID: "lane_root"})
	title("lane_tip", "Needle continuation")
	readAppend(t, db, "lane_tip", map[string]any{"role": "user", "content": "latest lane activity"})
	readSession(t, db, NewSession{ID: "foreign_match", Source: "telegram", SessionKey: "agent:main:telegram:dm:foreign", UserID: "foreign-user", ChatID: "foreign"})
	title("foreign_match", "Needle foreign")
	limit := 1
	rows := projList(t, db, ListSessionsRichOptions{Source: "telegram", SessionKey: lane, SearchQuery: "needle", OrderByLastActive: true, Limit: &limit})
	if len(rows) != 1 || rowValue(rows[0], "id") != "lane_tip" || rowValue(rows[0], "_lineage_root_id") != "lane_root" {
		t.Fatalf("rows %v", rows)
	}
}

func TestCompressionTipProjectionThreadsCompactRows(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	t0 := ago(1)
	readSession(t, db, NewSession{ID: "rootc", Source: "cli"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0, "rootc")
	readAppend(t, db, "rootc", map[string]any{"role": "user", "content": "start"})
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason=? WHERE id=?", t0+100, "compression", "rootc")
	readSession(t, db, NewSession{ID: "tipc", Source: "cli", ParentSessionID: "rootc"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0+101, "tipc")
	readAppend(t, db, "tipc", map[string]any{"role": "user", "content": "continuation"})
	prompt := strings.Repeat("big blob ", 500)
	if err := db.UpdateSystemPrompt(ctx, "tipc", &prompt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateSessionCwdClaim(ctx, "tipc", "/tmp/w2", "dev", "/tmp/w2", false); err != nil {
		t.Fatal(err)
	}
	tip := projRow(t, projList(t, db, ListSessionsRichOptions{Source: "cli", CompactRows: true}), "tipc")
	if _, ok := tip.Get("system_prompt"); ok {
		t.Fatal("compact tip carries system_prompt")
	}
	if rowValue(tip, "_lineage_root_id") != "rootc" || rowValue(tip, "git_branch") != "dev" || rowValue(tip, "git_repo_root") != "/tmp/w2" {
		t.Fatalf("tip %v", tip)
	}
}

func subagentListingDB(t *testing.T) *SessionDB {
	t.Helper()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "parent", Source: "desktop", Model: "m"})
	readSession(t, db, NewSession{ID: "sub", Source: "desktop", Model: "m", ParentSessionID: "parent", ModelConfig: map[string]any{"_delegate_from": "parent"}})
	readSession(t, db, NewSession{ID: "parent-cont", Source: "desktop", Model: "m", ParentSessionID: "parent"})
	if err := db.EndSession("parent", "compression"); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSubagentRunsStayHiddenByDefault(t *testing.T) {
	db := subagentListingDB(t)
	limit, project := 50, false
	if ids := projIDs(projList(t, db, ListSessionsRichOptions{Limit: &limit, ProjectCompressionTips: &project})); ids["sub"] {
		t.Fatalf("listed %v", ids)
	}
	if n, err := db.SessionCount(context.Background(), SessionCountOptions{ExcludeChildren: true}); err != nil || n != 1 {
		t.Fatalf("count %d %v", n, err)
	}
}

func TestIncludeSubagentsListsDelegateRunsWithAMatchingCount(t *testing.T) {
	db := subagentListingDB(t)
	limit, project := 50, false
	rows := projList(t, db, ListSessionsRichOptions{Limit: &limit, IncludeSubagents: true, ProjectCompressionTips: &project})
	if ids := projIDs(rows); !reflect.DeepEqual(ids, map[string]bool{"parent": true, "sub": true}) {
		t.Fatalf("listed %v", ids)
	}
	if got := rowValue(projRow(t, rows, "sub"), "parent_session_id"); got != "parent" {
		t.Fatalf("parent %v", got)
	}
	if n, err := db.SessionCount(context.Background(), SessionCountOptions{ExcludeChildren: true, IncludeSubagents: true}); err != nil || n != 2 {
		t.Fatalf("count %d %v", n, err)
	}
}

func projTip(t *testing.T, db *SessionDB, id, want string) {
	t.Helper()
	got, err := db.GetCompressionTip(context.Background(), id)
	if err != nil || got != want {
		t.Fatalf("tip of %s = %q, %v want %q", id, got, err, want)
	}
}

func projLineage(t *testing.T, db *SessionDB, id string, want ...string) {
	t.Helper()
	got, err := db.GetCompressionLineage(context.Background(), id)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage of %s = %v, %v want %v", id, got, err, want)
	}
}

func projOrder(rows []*pyjson.Object) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, pyStr(rowValue(r, "id")))
	}
	return out
}

func projCount(ids []string, id string) int {
	n := 0
	for _, x := range ids {
		if x == id {
			n++
		}
	}
	return n
}

func projIndex(ids []string, id string) int {
	for i, x := range ids {
		if x == id {
			return i
		}
	}
	return -1
}

func TestGetCompressionTipWalksFullChain(t *testing.T) {
	db := usageDB(t)
	projBuildChain(t, db, ago(1))
	projTip(t, db, "root1", "tip1")
	projTip(t, db, "mid1", "tip1")
	projTip(t, db, "tip1", "tip1")
}

func TestResetForkSiblingDoesNotStealTipProjection(t *testing.T) {
	db := usageDB(t)
	t0 := ago(1)
	readSession(t, db, NewSession{ID: "root1", Source: "cli"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0, "root1")
	readAppend(t, db, "root1", map[string]any{"role": "user", "content": "help me refactor auth"})
	compressRoot := t0 + 1800
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason=? WHERE id=?", compressRoot, "compression", "root1")
	readSession(t, db, NewSession{ID: "mid1", Source: "cli", ParentSessionID: "root1"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", compressRoot+1, "mid1")
	readAppend(t, db, "mid1", map[string]any{"role": "user", "content": "continuing"})
	compressMid := compressRoot + 1800
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason=? WHERE id=?", compressMid, "compression", "mid1")
	readSession(t, db, NewSession{ID: "tip1", Source: "cli", ParentSessionID: "mid1"})
	profileExec(t, db, "UPDATE sessions SET started_at=?, ended_at=?, end_reason=?, last_activity_at=? WHERE id=?",
		compressMid+1, compressMid+600, "startup_orphan_reap", compressMid+600, "tip1")
	readAppend(t, db, "tip1", map[string]any{"role": "user", "content": "latest message"})
	readSession(t, db, NewSession{ID: "reset1", Source: "cli", ParentSessionID: "mid1", ModelConfig: map[string]any{"_reset_from": "mid1"}})
	profileExec(t, db, "UPDATE sessions SET started_at=?, ended_at=?, end_reason=?, last_activity_at=? WHERE id=?",
		compressMid+2, compressMid+900, "session_reset", compressMid+900, "reset1")
	readAppend(t, db, "reset1", map[string]any{"role": "user", "content": "post reset talk"})

	projTip(t, db, "root1", "tip1")
	projTip(t, db, "mid1", "tip1")
	rows := projCLI(t, db)
	ids := projOrder(rows)
	if projCount(ids, "reset1") != 1 || projIndex(ids, "tip1") < 0 || projIndex(ids, "root1") >= 0 || projIndex(ids, "mid1") >= 0 {
		t.Fatalf("projected ids %v", ids)
	}
	tip := projRow(t, rows, "tip1")
	if rowValue(tip, "_lineage_root_id") != "root1" || !strings.HasPrefix(pyStr(rowValue(tip, "preview")), "latest message") {
		t.Fatalf("tip row %v", tip)
	}

	readSession(t, db, NewSession{ID: "solo", Source: "cli"})
	profileExec(t, db, "UPDATE sessions SET started_at=?, last_activity_at=? WHERE id=?", compressMid+300, compressMid+700, "solo")
	readAppend(t, db, "solo", map[string]any{"role": "user", "content": "standalone"})
	limit := 20
	ordered := projOrder(projList(t, db, ListSessionsRichOptions{Source: "cli", Limit: &limit, OrderByLastActive: true}))
	if projCount(ordered, "reset1") != 1 || projIndex(ordered, "solo") < 0 || projIndex(ordered, "solo") > projIndex(ordered, "tip1") {
		t.Fatalf("ordered ids %v", ordered)
	}
}

func TestResetForkOfCompressedParentIsNotALineageMember(t *testing.T) {
	db := usageDB(t)
	ctx := context.Background()
	t0 := ago(1)
	readSession(t, db, NewSession{ID: "root1", Source: "cli"})
	profileExec(t, db, "UPDATE sessions SET ended_at=?, end_reason='compression' WHERE id=?", t0+10, "root1")
	readSession(t, db, NewSession{ID: "reset1", Source: "cli", ParentSessionID: "root1", ModelConfig: map[string]any{"_reset_from": "root1"}})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0+11, "reset1")
	readSession(t, db, NewSession{ID: "tip1", Source: "cli", ParentSessionID: "root1"})
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0+20, "tip1")

	row, err := db.GetSession(ctx, "reset1")
	if err != nil {
		t.Fatal(err)
	}
	if child, err := db.isCompressionChildRow(ctx, row); err != nil || child {
		t.Fatalf("reset fork is a compression child %v %v", child, err)
	}
	projLineage(t, db, "root1", "root1", "tip1")
	projLineage(t, db, "tip1", "root1", "tip1")
	projLineage(t, db, "reset1", "reset1")
}
