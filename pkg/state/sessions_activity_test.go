package state

import (
	"context"
	"testing"
)

func activityRow(t *testing.T, db *SessionDB, sid string) (any, any, any) {
	t.Helper()
	row, err := db.GetSession(context.Background(), sid)
	if err != nil || row == nil {
		t.Fatalf("get session %v %v", row, err)
	}
	return rowValue(row, "last_activity_at"), rowValue(row, "last_activity_description"), rowValue(row, "last_activity_provenance")
}

func activityLastActive(t *testing.T, db *SessionDB) float64 {
	t.Helper()
	list, err := db.ListSessions(ListFilter{})
	if err != nil || len(list) == 0 {
		t.Fatalf("list sessions %v %v", list, err)
	}
	return list[0].LastActive
}

func TestLastActivePrefersSessionActivityHeartbeat(t *testing.T) {
	db := usageDB(t)
	ctx := context.Background()
	if err := db.CreateSession(NewSession{ID: "s1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, "s1", map[string]any{"role": "user", "content": "hello"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if err := db.writeSQL(ctx, "UPDATE messages SET timestamp=? WHERE session_id=? AND role=?", []any{1_700_000_000.0, "s1", "user"}, false, nil); err != nil {
		t.Fatal(err)
	}
	before := activityLastActive(t, db)
	heartbeat := 1_700_000_500.0
	if err := db.TouchSessionActivity("s1", &heartbeat, "starting API call #1", ActivityProvenanceUnknown); err != nil {
		t.Fatal(err)
	}
	after := activityLastActive(t, db)
	if after != heartbeat || after <= before {
		t.Fatalf("last active %v before %v", after, before)
	}
	at, desc, prov := activityRow(t, db, "s1")
	if at != heartbeat || desc != "starting API call #1" || prov != "unknown" {
		t.Fatalf("row %v %v %v", at, desc, prov)
	}
	older := heartbeat - 100
	if err := db.TouchSessionActivity("s1", &older, "ignored", ""); err != nil {
		t.Fatal(err)
	}
	if at, desc, _ := activityRow(t, db, "s1"); at != heartbeat || desc != "starting API call #1" {
		t.Fatalf("moved backwards %v %v", at, desc)
	}
	if err := db.TouchSessionActivity("", nil, "x", ""); err != nil {
		t.Fatal(err)
	}
}

func TestClearSessionActivityLabelsKeepsTimestamp(t *testing.T) {
	db := usageDB(t)
	if err := db.CreateSession(NewSession{ID: "s1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	heartbeat := 1_700_000_500.0
	if err := db.TouchSessionActivity("s1", &heartbeat, "compressing context", ActivityProvenanceAgentCompression); err != nil {
		t.Fatal(err)
	}
	if at, desc, prov := activityRow(t, db, "s1"); at != heartbeat || desc != "compressing context" || prov != "agent.compression" {
		t.Fatalf("row %v %v %v", at, desc, prov)
	}
	if err := db.ClearSessionActivityLabels("s1"); err != nil {
		t.Fatal(err)
	}
	if at, desc, prov := activityRow(t, db, "s1"); at != heartbeat || desc != "" || prov != "unknown" {
		t.Fatalf("cleared row %v %v %v", at, desc, prov)
	}
	if err := db.ClearSessionActivityLabels("s1"); err != nil {
		t.Fatal(err)
	}
	unknown := "  bogus "
	later := heartbeat + 1
	if err := db.TouchSessionActivity("s1", &later, "  tool ", ActivityProvenance(unknown)); err != nil {
		t.Fatal(err)
	}
	if _, desc, prov := activityRow(t, db, "s1"); desc != "tool" || prov != "unknown" {
		t.Fatalf("normalized %v %v", desc, prov)
	}
}
