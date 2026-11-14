package tuiserver

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Launch store whose resume cap is three messages, so a four row transcript is a runaway one
func cappedResumeDB(t *testing.T) *state.DB {
	t.Helper()
	db := resumeDB(t)
	cfg := "model:\n  default: test-model\nsessions:\n  max_resume_messages: 3\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return db
}

// Stored session holding the given messages in order
func storeTranscript(t *testing.T, db *state.DB, sid, parent string, messages ...map[string]any) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "tui", ParentSessionID: parent}); err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if _, err := db.AppendMessage(context.Background(), sid, m, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
}

// Compressed root of four rows behind a one row tip, a lineage over the cap with a tip under it
func runawayLineage(t *testing.T, db *state.DB, tip string) {
	t.Helper()
	root := tip + "-root"
	var rows []map[string]any
	for _, text := range []string{"a", "b", "c", "d"} {
		rows = append(rows, map[string]any{"role": "user", "content": text})
	}
	storeTranscript(t, db, root, "", rows...)
	reaperExec(t, db, "UPDATE sessions SET ended_at=started_at+1, end_reason='compression' WHERE id=?", root)
	storeTranscript(t, db, tip, root, map[string]any{"role": "user", "content": "tip"})
	reaperExec(t, db, "UPDATE sessions SET started_at=started_at+2 WHERE id=?", tip)
}

func TestSessionResumeReturnsHydratedMessages(t *testing.T) {
	db := resumeDB(t)
	storeTranscript(t, db, "20260409_010101_abc123", "",
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "yo", "reasoning": "thoughts"},
		map[string]any{"role": "tool", "content": "searched"},
		map[string]any{"role": "assistant", "content": "   "},
		map[string]any{"role": "assistant", "content": nil},
		map[string]any{"role": "narrator", "content": "skip"})
	resp := resumeCall(t, map[string]any{"session_id": "20260409_010101_abc123", "cols": 100, "eager_build": true})
	result, isResult := resp["result"].(map[string]any)
	if !isResult {
		t.Fatalf("resume %v", resp)
	}
	want := []any{map[string]any{"role": "user", "text": "hello"}, map[string]any{"role": "assistant", "text": "yo", "reasoning": "thoughts"},
		map[string]any{"role": "tool", "name": "tool", "context": ""}}
	for _, m := range result["messages"].([]any) {
		delete(m.(map[string]any), "row_id")
		delete(m.(map[string]any), "timestamp")
	}
	if result["message_count"] != 3 || !reflect.DeepEqual(result["messages"], want) {
		t.Fatalf("count %v messages %#v", result["message_count"], result["messages"])
	}
}

func TestSessionResumeRejectsRunawayTranscriptBeforeHistoryLoad(t *testing.T) {
	db := cappedResumeDB(t)
	var rows []map[string]any
	for _, text := range []string{"a", "b", "c", "d"} {
		rows = append(rows, map[string]any{"role": "user", "content": text})
	}
	storeTranscript(t, db, "runaway-session", "", rows...)
	if err := db.EndSession("runaway-session", "tui_close"); err != nil {
		t.Fatal(err)
	}
	resp := resumeCall(t, map[string]any{"session_id": "runaway-session", "omit_messages": true})
	if resumeErrCode(resp) != 4130 {
		t.Fatalf("resume %v", resp)
	}
	row, err := db.GetSession(context.Background(), "runaway-session")
	if err != nil || row.GetOr("ended_at", nil) == nil || row.GetOr("end_reason", nil) != "tui_close" {
		t.Fatalf("oversized session reopened %v %v", row, err)
	}
}

func TestSessionResumeDeferredAndOmittedPathsGuardTheTipOnly(t *testing.T) {
	db := cappedResumeDB(t)
	for _, key := range []string{"ICEBATH_GATEWAY_SESSION", "ICEBATH_EXEC_ASK", "ICEBATH_INTERACTIVE"} {
		t.Setenv(key, "")
	}
	runawayLineage(t, db, "deep-lineage")
	for _, params := range []map[string]any{
		{"defer_history": true, "omit_messages": true, "source": "desktop"},
		{"omit_messages": true},
		{"lazy": true},
	} {
		params["session_id"] = "deep-lineage"
		resp := handleRequest(context.Background(), map[string]any{"id": "r-tip", "method": "session.resume", "params": params})
		if resumeErrCode(resp) == 4130 {
			t.Fatalf("tip path %v refused %v", params, resp)
		}
	}
	resp := handleRequest(context.Background(), map[string]any{"id": "r-full", "method": "session.resume",
		"params": map[string]any{"session_id": "deep-lineage"}})
	if resumeErrCode(resp) != 4130 {
		t.Fatalf("full resume %v", resp)
	}
}

func TestSessionResumeGuardFailureFailsOpen(t *testing.T) {
	db := resumeDB(t)
	storeTranscript(t, db, "transient-guard-session", "")
	reaperExec(t, db, "ALTER TABLE messages RENAME TO messages_away")
	logs := captureLogs(t)
	resp := resumeCall(t, map[string]any{"session_id": "transient-guard-session", "omit_messages": true})
	if resumeErrCode(resp) == 4130 {
		t.Fatalf("guard blocked %v", resp)
	}
	if got := logs.take("resume safety check failed for transient-guard-session (proceeding without guard)"); len(got) != 1 {
		t.Fatalf("guard log %v resp %v", got, resp)
	}
	errObj, _ := resp["error"].(map[string]any)
	if !strings.Contains(strOr(errObj["message"]), "no such table: messages") {
		t.Fatalf("history read never ran %v", resp)
	}
}

func TestDeferredHydrationFallsBackToTipWhenLineageExceedsLimit(t *testing.T) {
	db := cappedResumeDB(t)
	runawayLineage(t, db, "deep-lineage")
	session := deferredSessionRecord(context.Background(), "deep-lineage", deferredRecordOptions{cols: 80, cwd: t.TempDir(), history: []any{}})
	ready := &pythread.Event{}
	session.Set("resume_history_ready", ready)
	session.Set("resume_hydrating", true)
	session.Set("resume_message_count", 28_730)
	swapSessions(t, map[string]*tuigateway.Session{"hyd": session})
	scheduleResumeHydration(context.Background(), "hyd", "deep-lineage", db, false, false)
	if !ready.Wait(5) {
		t.Fatal("hydration never finished")
	}
	built, _ := session.Get("agent_ready").(*pythread.Event)
	if built == nil || !built.Wait(30) {
		t.Fatal("agent build never ran")
	}
	unlock := lockHistory(session)
	history, _ := session.Get("history").([]any)
	prefix, _ := session.Get("display_history_prefix").([]any)
	count, failure := session.Get("resume_message_count"), session.Get("resume_history_error")
	unlock()
	if failure != nil || len(history) != 1 || len(prefix) != 0 || count != 1 {
		t.Fatalf("history %v prefix %v count %v error %v", history, prefix, count, failure)
	}
	if m, _ := history[0].(map[string]any); m["role"] != "user" || m["content"] != "tip" {
		t.Fatalf("history %v", history)
	}
}

func TestSessionBranchStoredAcceptsIdempotencyKey(t *testing.T) {
	db := resumeDB(t)
	swapSessions(t, map[string]*tuigateway.Session{})
	storeTranscript(t, db, "parent", "",
		map[string]any{"role": "user", "content": "first question", "timestamp": 1},
		map[string]any{"role": "assistant", "content": "first answer", "timestamp": 2})
	params := map[string]any{"cols": 96, "parent_session_id": "parent", "source": "desktop", "idempotency_key": "branch-stored-retry-abc"}
	call := func(rid string) string {
		t.Helper()
		return createdSID(t, handleRequest(context.Background(), map[string]any{"id": rid, "method": "session.branch_stored", "params": maps.Clone(params)}))
	}
	first := call("b1")
	if n := len(tuigateway.LiveSessions()); n != 1 {
		t.Fatalf("%d live sessions", n)
	}
	if second := call("b2"); second != first {
		t.Fatalf("retry created %s, first %s", second, first)
	}
	if n := len(tuigateway.LiveSessions()); n != 1 {
		t.Fatalf("%d live sessions after retry", n)
	}
}
