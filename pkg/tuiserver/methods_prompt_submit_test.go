package tuiserver

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Launch store at a known path holding the session row the submit cases write through
func submitCaseDB(t *testing.T, key string) (*state.DB, string) {
	t.Helper()
	sessionCaseScope(t)
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := state.NewSessionDB(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	useSharedDB(t, db)
	if err := db.CreateSession(state.NewSession{ID: key, Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	return db, path
}

// Replaces the session transcript with rows under the given ids, the second column role and the third content
func seedRowsWithIDs(t *testing.T, db *state.DB, key string, rows ...[3]any) {
	t.Helper()
	err := db.ExecuteWrite(context.Background(), func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(context.Background(), "DELETE FROM messages WHERE session_id = ?", key); err != nil {
			return err
		}
		for i, row := range rows {
			if _, err := conn.ExecContext(context.Background(),
				"INSERT INTO messages (id, session_id, role, content, timestamp) VALUES (?, ?, ?, ?, ?)",
				row[0], key, row[1], row[2], 1000+i); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Active rows of the session as id, role and content triples
func activeRows(t *testing.T, db *state.DB, key string) [][3]any {
	t.Helper()
	rows, err := db.GetMessagesAsConversation(context.Background(), key, state.ConversationOptions{IncludeRowIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	out := [][3]any{}
	for _, row := range rows {
		id, _ := messageRowID(row)
		out = append(out, [3]any{id, row["role"], row["content"]})
	}
	return out
}

// Live record whose accepted turn parks on an unfinished build until the case ends and cancels it
func parkedSubmitSession(t *testing.T, sid string, fields map[string]any) *tuigateway.Session {
	t.Helper()
	ready := &pythread.Event{}
	base := map[string]any{"agent": nil, "session_key": "session-key", "history": []any{}, "history_lock": &sync.Mutex{},
		"history_version": int64(0), "running": false, "attached_images": []any{}, "image_counter": 0, "cols": 80,
		"slash_worker": nil, "show_reasoning": false, "tool_progress_mode": "all", "agent_ready": ready, "agent_build_started": true}
	for key, value := range fields {
		base[key] = value
	}
	session := tuigateway.NewSession(base)
	tuigateway.SetSessionRecord(sid, session)
	t.Cleanup(func() {
		unlock := lockHistory(session)
		session.Set("_turn_cancel_requested", true)
		unlock()
		ready.Set()
		if thread, isThread := session.Get("_run_thread").(*memory.ContextThread); isThread && thread != nil {
			deadline := time.Now().Add(10 * time.Second)
			for thread.Alive() && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
		}
		tuigateway.SetSessionRecord(sid, nil)
	})
	return session
}

// Three user and assistant turns stamped 100 to 105
func threeTurnHistory() ([]any, [][3]any) {
	var history []any
	var rows [][3]any
	for turn := 0; turn < 3; turn++ {
		history = append(history,
			map[string]any{"_row_id": int64(100 + 2*turn), "role": "user", "content": fmt.Sprintf("u%d", turn)},
			map[string]any{"_row_id": int64(101 + 2*turn), "role": "assistant", "content": fmt.Sprintf("a%d", turn)})
		rows = append(rows, [3]any{int64(100 + 2*turn), "user", fmt.Sprintf("u%d", turn)},
			[3]any{int64(101 + 2*turn), "assistant", fmt.Sprintf("a%d", turn)})
	}
	return history, rows
}

func submitRPC(sid string, params map[string]any) map[string]any {
	merged := map[string]any{"session_id": sid}
	for key, value := range params {
		merged[key] = value
	}
	return handleRequest(context.Background(), map[string]any{"id": "1", "method": "prompt.submit", "params": merged})
}

func rpcErrorOf(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	failure, isErr := response["error"].(map[string]any)
	if !isErr {
		t.Fatalf("expected an error, got %v", response)
	}
	return failure
}

// test_tui_gateway_server_deep_truncate.py test_prompt_submit_refuses_deep_truncation_without_confirm
func TestPromptSubmitRefusesDeepTruncationWithoutConfirm(t *testing.T) {
	db, _ := submitCaseDB(t, "session-key")
	swapVar(t, &deepTruncateEnforced, true)
	history, rows := threeTurnHistory()
	seedRowsWithIDs(t, db, "session-key", rows...)
	session := parkedSubmitSession(t, "deep-trunc-sid", map[string]any{"history": append([]any{}, history...)})
	submit := func(extra map[string]any) map[string]any {
		params := map[string]any{"text": "u1", "truncate_before_row_id": 102, "confirm_truncate": true}
		for key, value := range extra {
			params[key] = value
		}
		return submitRPC("deep-trunc-sid", params)
	}
	refused := rpcErrorOf(t, submit(nil))
	if refused["code"] != 4033 || !pyjson.Equal(refused["data"], map[string]any{"archived_messages": 4, "archived_user_turns": 2}) {
		t.Fatalf("refused %v", refused)
	}
	if !pyjson.Equal(session.Get("history"), history) || !reflect.DeepEqual(activeRows(t, db, "session-key"), rows) {
		t.Fatalf("refusal touched the transcript: %v", activeRows(t, db, "session-key"))
	}
	if confirmed := submit(map[string]any{"confirm_deep_truncate": true}); confirmed["error"] != nil {
		t.Fatalf("confirmed %v", confirmed)
	}
	written := activeRows(t, db, "session-key")
	if len(written) != 3 || !reflect.DeepEqual(written[:2], rows[:2]) || written[2][1] != "user" || written[2][2] != "u1" {
		t.Fatalf("the cut must keep exactly the first turn before the resent prompt: %v", written)
	}
	seedRowsWithIDs(t, db, "session-key", [3]any{int64(100), "user", "u0"}, [3]any{int64(101), "user", "u0b"},
		[3]any{int64(102), "assistant", "a0"}, [3]any{int64(103), "user", "u1"}, [3]any{int64(104), "assistant", "a1"},
		[3]any{int64(105), "user", "u2"}, [3]any{int64(106), "assistant", "a2"})
	unlock := lockHistory(session)
	session.Set("running", false)
	session.Set("history", []any{
		map[string]any{"_row_id": int64(100), "_absorbed_row_ids": []any{int64(101)}, "role": "user", "content": "u0\n\nu0b"},
		map[string]any{"_row_id": int64(102), "role": "assistant", "content": "a0"},
		map[string]any{"_row_id": int64(103), "role": "user", "content": "u1"},
		map[string]any{"_row_id": int64(104), "role": "assistant", "content": "a1"},
		map[string]any{"_row_id": int64(105), "role": "user", "content": "u2"},
		map[string]any{"_row_id": int64(106), "role": "assistant", "content": "a2"}})
	unlock()
	carrier := rpcErrorOf(t, submit(map[string]any{"truncate_before_row_id": 101}))
	if !pyjson.Equal(carrier["data"], map[string]any{"archived_messages": 6, "archived_user_turns": 3}) {
		t.Fatalf("carrier %v", carrier)
	}
}

// test_tui_gateway_server_deep_truncate.py test_deep_truncate_counts_every_durable_row_a_merged_carrier_holds
func TestDeepTruncateCountsEveryDurableRowAMergedCarrierHolds(t *testing.T) {
	db, _ := submitCaseDB(t, "session-key")
	swapVar(t, &deepTruncateEnforced, true)
	physical := [][3]any{{int64(101), "user", "a"}, {int64(102), "user", "b"}, {int64(103), "user", "c"}, {int64(104), "assistant", "reply"}}
	seedRowsWithIDs(t, db, "session-key", physical...)
	history := []any{map[string]any{"_row_id": int64(101), "_absorbed_row_ids": []any{int64(102), int64(103)}, "role": "user", "content": "a\n\nb\n\nc"},
		map[string]any{"_row_id": int64(104), "role": "assistant", "content": "reply"}}
	session := parkedSubmitSession(t, "deep-carrier-sid", map[string]any{"history": append([]any{}, history...)})
	refused := rpcErrorOf(t, submitRPC("deep-carrier-sid", map[string]any{"text": "a", "truncate_before_row_id": 101,
		"confirm_truncate": true, "confirm_empty_truncate": true}))
	data, _ := refused["data"].(map[string]any)
	if refused["code"] != 4033 || !pyjson.Equal(data["archived_user_turns"], 3) {
		t.Fatalf("refused %v", refused)
	}
	if !pyjson.Equal(session.Get("history"), history) || !reflect.DeepEqual(activeRows(t, db, "session-key"), physical) {
		t.Fatalf("refusal touched the transcript")
	}
}

// test_tui_gateway_server_deep_truncate.py test_archived_user_turns_counts_a_carrier_without_its_own_row_id
func TestArchivedUserTurnsCountsACarrierWithoutItsOwnRowID(t *testing.T) {
	db, _ := submitCaseDB(t, "session-key")
	seedRowsWithIDs(t, db, "session-key", [3]any{int64(102), "user", "c"})
	history := []map[string]any{{"role": "user", "content": "x"}, {"role": "assistant", "content": "r"},
		{"_absorbed_row_ids": []any{int64(102)}, "role": "user", "content": "b\n\nc"}}
	session := tuigateway.NewSession(map[string]any{"session_key": "session-key", "history": messagesAsHistory(history), "history_lock": &sync.Mutex{}})
	if got := archivedUserTurns(context.Background(), session, "sid", history, 2, map[int64]bool{}); got != 2 {
		t.Fatalf("archived %d", got)
	}
}

// test_tui_gateway_server_deep_truncate.py test_unconfirmed_deep_truncation_is_warn_only_this_release
func TestUnconfirmedDeepTruncationIsWarnOnlyThisRelease(t *testing.T) {
	db, _ := submitCaseDB(t, "session-key")
	history, rows := threeTurnHistory()
	seedRowsWithIDs(t, db, "session-key", rows...)
	parkedSubmitSession(t, "warn-only-sid", map[string]any{"history": append([]any{}, history...)})
	logs := captureLogs(t)
	result := submitRPC("warn-only-sid", map[string]any{"text": "u1", "truncate_before_row_id": 102, "confirm_truncate": true})
	if result["error"] != nil {
		t.Fatalf("result %v", result)
	}
	written := activeRows(t, db, "session-key")
	if len(written) != 3 || !reflect.DeepEqual(written[:2], rows[:2]) {
		t.Fatalf("written %v", written)
	}
	if len(logs.take("ALLOWED (warn-only release) unconfirmed deep truncation")) == 0 {
		t.Fatal("the would-be refusal was not logged")
	}
}

// Parked bot room record titled for a group, the session every fence case submits to
func fenceSession(t *testing.T, title string, profileHome any) *tuigateway.Session {
	t.Helper()
	return parkedSubmitSession(t, "session-1", map[string]any{"id": "session-1", "title": title, "source": "bot_room",
		"profile_home": profileHome, "session_key": ""})
}

func fenceSubmit(rid string) map[string]any {
	return lookupMethod("prompt.submit")(context.Background(), rid, map[string]any{"session_id": "session-1", "text": "continue"})
}

// Whether the submit passed the fence into normal admission and was accepted
func reachedAdmission(result map[string]any) bool {
	accepted, _ := result["result"].(map[string]any)
	return accepted["status"] == "streaming"
}

func createFenceRoom(t *testing.T, roomID string, members []any) {
	t.Helper()
	ctx := context.Background()
	authority, err := gateway.LocalAuthorityGatewayID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.CreateRoom(ctx, gateway.HostedRoomsDefaultDBPath(ctx), gateway.CreateRoomInput{RoomID: roomID, Name: roomID,
		Members: members, AuthorityGatewayID: authority}); err != nil {
		t.Fatal(err)
	}
}

// test_hosted_room_prompt_fence.py test_direct_prompt_to_hosted_group_session_is_rejected
func TestDirectPromptToHostedGroupSessionIsRejected(t *testing.T) {
	submitCaseDB(t, "unused-key")
	createFenceRoom(t, "room-hosted", []any{
		map[string]any{"member_id": "one", "profile": "one", "handle": "one"},
		map[string]any{"member_id": "two", "profile": "two", "handle": "two"}})
	fenceSession(t, "Group: room-hosted", nil)
	if failure := rpcErrorOf(t, fenceSubmit("request-1")); failure["code"] != 4122 {
		t.Fatalf("result %v", failure)
	}
}

// test_hosted_room_prompt_fence.py test_direct_prompt_to_non_hosted_group_reaches_normal_admission
func TestDirectPromptToNonHostedGroupReachesNormalAdmission(t *testing.T) {
	submitCaseDB(t, "unused-key")
	fenceSession(t, "Group: local-only", nil)
	if result := fenceSubmit("request-2"); !reachedAdmission(result) {
		t.Fatalf("result %v", result)
	}
	if _, err := os.Stat(gateway.HostedRoomsDefaultDBPath(context.Background())); !os.IsNotExist(err) {
		t.Fatalf("the probe created the room store: %v", err)
	}
}

// test_hosted_room_prompt_fence.py test_direct_prompt_to_legacy_named_group_reaches_normal_admission
func TestDirectPromptToLegacyNamedGroupReachesNormalAdmission(t *testing.T) {
	for _, legacyName := range []string{"Launch room", "Ceo, Product Designer, Cfo", "Équipe", "Alpha/Beta"} {
		t.Run(legacyName, func(t *testing.T) {
			submitCaseDB(t, "unused-key")
			fenceSession(t, "Group: "+legacyName, nil)
			if result := fenceSubmit("request-legacy"); !reachedAdmission(result) {
				t.Fatalf("result %v", result)
			}
		})
	}
}

// test_hosted_room_prompt_fence.py test_direct_prompt_to_peer_reserved_group_is_rejected_until_revoke
func TestDirectPromptToPeerReservedGroupIsRejectedUntilRevoke(t *testing.T) {
	submitCaseDB(t, "unused-key")
	ctx := context.Background()
	profileHome := filepath.Join(home.Dir(ctx), "profiles", "reviewer")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	now := float64(time.Now().UnixNano()) / 1e9
	claims := pyjson.NewObject()
	for _, kv := range [][2]any{{"room_id", "room-peer"}, {"home_install_id", "install-home"}, {"authority_gateway_id", "install-home"},
		{"authority_epoch", int64(1)}, {"member_id", "member-reviewer"}, {"target_install_id", "install-target"},
		{"target_profile", "reviewer"}, {"issued_at", now}} {
		claims.Set(kv[0].(string), kv[1])
	}
	dbPath := gateway.HostedRoomsDefaultDBPath(ctx)
	if err := gateway.ReservePeerRoom(ctx, dbPath, claims, now+300.0, &now); err != nil {
		t.Fatal(err)
	}
	fenceSession(t, "Group: room-peer", profileHome)
	if failure := rpcErrorOf(t, fenceSubmit("request-peer")); failure["code"] != 4122 {
		t.Fatalf("rejected %v", failure)
	}
	later := now + 150.0
	if err := gateway.RevokeRoomGrantScope(ctx, dbPath, claims, now+300.0, &later); err != nil {
		t.Fatal(err)
	}
	if admitted := fenceSubmit("request-peer-after-revoke"); !reachedAdmission(admitted) {
		t.Fatalf("admitted %v", admitted)
	}
}

// test_hosted_room_prompt_fence.py test_direct_prompt_is_refused_when_room_authority_cannot_be_verified
func TestDirectPromptIsRefusedWhenRoomAuthorityCannotBeVerified(t *testing.T) {
	submitCaseDB(t, "unused-key")
	dbPath := gateway.HostedRoomsDefaultDBPath(context.Background())
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("disk busy: this file is not a database, so every probe read fails"), 0o644); err != nil {
		t.Fatal(err)
	}
	fenceSession(t, "Group: room-unknown", nil)
	if failure := rpcErrorOf(t, fenceSubmit("request-3")); failure["code"] != 5122 {
		t.Fatalf("result %v", failure)
	}
}

// test_hosted_room_prompt_fence.py test_contended_ownership_probe_fails_quickly_without_blocking_socket
func TestContendedOwnershipProbeFailsQuicklyWithoutBlockingSocket(t *testing.T) {
	submitCaseDB(t, "unused-key")
	createFenceRoom(t, "room-busy", []any{})
	fenceSession(t, "Group: room-busy", nil)
	ctx := context.Background()
	blocker, err := sql.Open("sqlite", gateway.HostedRoomsDefaultDBPath(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	conn, err := blocker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, statement := range []string{"PRAGMA journal_mode=DELETE", "BEGIN EXCLUSIVE"} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	result := fenceSubmit("request-busy")
	_, _ = conn.ExecContext(ctx, "ROLLBACK")
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("probe blocked for %v", elapsed)
	}
	if failure := rpcErrorOf(t, result); failure["code"] != 5122 {
		t.Fatalf("result %v", failure)
	}
}

// test_relay_live_author.py test_prompt_submit_refuses_a_client_supplied_author
func TestPromptSubmitRefusesAClientSuppliedAuthor(t *testing.T) {
	submitCaseDB(t, "gw-session-key")
	parkedSubmitSession(t, "sid", map[string]any{"session_key": "gw-session-key", "inflight_turn": nil, "transport": nil})
	author := map[string]any{"id": "bot:coder", "name": "coder", "is_bot": true}
	resp := lookupMethod("prompt.submit")(context.Background(), "r", map[string]any{"session_id": "sid", "text": "hi", "_turn_author": author})
	if failure := rpcErrorOf(t, resp); failure["code"] != 4124 {
		t.Fatalf("resp %v", resp)
	}
}
