package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func hrWorkCreate(db, roomID string) {
	hrOK(CreateRoom(context.Background(), db, CreateRoomInput{RoomID: roomID, Name: "Evidence", Members: []any{map[string]any{"profile": "ops", "handle": "ops"}},
		AuthorityGatewayID: "owner-a", Now: hrFloatPtr(10)}))
}

// Runs body in one transaction of a fresh raw connection, committed when it succeeds
func hrWithConn(t *testing.T, db string, body func(conn state.SQLiteConn)) {
	t.Helper()
	conn := hrRawConn(t, db)
	if err := state.Transaction(context.Background(), conn, false, func(conn state.SQLiteConn) error {
		body(conn)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func hrIsIntegrityError(err error) bool {
	class, ok := state.SQLiteClassOf(err)
	return ok && class == state.SQLiteIntegrityError
}

func hrTen() any { return 10.0 }

func TestPendingAckSurvivesRestartAndFencesAuthority(t *testing.T) {
	db := hrDB(t)
	ctx := context.Background()
	hrWorkCreate(db, "room")
	hrOK(AppendEvent(ctx, db, AppendEventInput{RoomID: "room", EventID: "input", Kind: "message.user", Actor: map[string]any{"kind": "user", "id": "alice"},
		Payload: map[string]any{"text": "hello"}, AuthorityGatewayID: "owner-a", AuthorityEpoch: 1}))
	identity := hrOK(NewTaskIdentity("room", "task", "thread", "turn"))
	hrOK(DriverAdmitTask(ctx, db, identity, map[string]any{"target_profile": "ops", "prompt": "private prompt", "source_event_seq": 1}, hrTen))
	first := hrOK(CaptureWorkRecord(ctx, db, "room", "owner-a", nil))
	if workList(first["tasks"])[0]["task_id"] != "task" || strings.Contains(hrOK(WorkEncode(first)), "private prompt") || first["version"] != int64(1) {
		t.Fatalf("first %v", first)
	}
	var pending map[string]any
	var raw string
	hrWithConn(t, db, func(conn state.SQLiteConn) {
		pending = hrOK(PrepareWorkDeliveryLocked(ctx, conn, "room", "peer", "route-1", "owner-a", 100))
		raw = hrStr(hrOK(state.QueryRowObject(ctx, conn, "SELECT record_json FROM "+WorkPendingTable)), "record_json")
	})
	hrWithConn(t, db, func(conn state.SQLiteConn) {
		for _, change := range []struct {
			key   string
			value any
		}{{"revision", first["revision"].(int64) + 1}, {"digest", strings.Repeat("0", 64)}, {"authority", map[string]any{"gateway_id": "owner-b", "epoch": 2}}} {
			ack := WorkAcknowledgement(pending)
			ack[change.key] = change.value
			if hrOK(AcknowledgeWorkLocked(ctx, conn, "room", "peer", "route-1", pending, ack)) {
				t.Fatalf("altered %s acknowledged", change.key)
			}
		}
		if hrOK(AcknowledgeWorkLocked(ctx, conn, "room", "peer", "route-2", pending, WorkAcknowledgement(pending))) {
			t.Fatal("wrong route acknowledged")
		}
		for _, statement := range []string{"UPDATE hosted_rooms SET authority_gateway_id='owner-b',authority_epoch=2 WHERE room_id='room'"} {
			hrOK(conn.ExecContext(ctx, statement))
		}
		hrOK(conn.ExecContext(ctx, "INSERT INTO hosted_room_events VALUES ('room',2,'claim','authority.claimed',?,2,?,11)",
			`{"kind": "system", "id": "authority-control"}`, `{"previous_gateway_id": "owner-a", "authority_gateway_id": "owner-b", "authority_epoch": 2}`))
		hrOK(conn.ExecContext(ctx, "UPDATE hosted_rooms SET next_seq=3 WHERE room_id='room'"))
	})
	successor := hrOK(CaptureWorkRecord(ctx, db, "room", "owner-b", nil))
	if successor["version"] != int64(2) || !pyjson.Equal(successor["incompleteness"], []any{"prior_authority_work_unknown"}) {
		t.Fatalf("successor %v", successor)
	}
	hrWithConn(t, db, func(conn state.SQLiteConn) {
		if hrOK(AcknowledgeWorkLocked(ctx, conn, "room", "peer", "route-1", pending, WorkAcknowledgement(pending))) {
			t.Fatal("superseded record acknowledged")
		}
		old := hrOK(state.QueryRowObject(ctx, conn, "SELECT * FROM "+WorkPendingTable))
		if hrStr(old, "record_json") != raw || hrStr(old, "status") != "pending" || hrStr(old, "disposition") != "superseded_authority" {
			t.Fatalf("old %v", old)
		}
		current := hrOK(PrepareWorkDeliveryLocked(ctx, conn, "room", "peer", "route-3", "owner-b", 100))
		ack := WorkAcknowledgement(current)
		wrong := pyjson.Plain(ack).(map[string]any)
		wrong["lineage_sha256"] = strings.Repeat("0", 64)
		if hrOK(AcknowledgeWorkLocked(ctx, conn, "room", "peer", "route-3", current, wrong)) {
			t.Fatal("wrong lineage acknowledged")
		}
		if !hrOK(AcknowledgeWorkLocked(ctx, conn, "room", "peer", "route-3", current, ack)) {
			t.Fatal("exact acknowledgement refused")
		}
		if _, err := conn.ExecContext(ctx, "UPDATE "+WorkPendingTable+" SET room_id='other' WHERE producer_epoch=1"); !hrIsIntegrityError(err) {
			t.Fatalf("historical row mutated: %v", err)
		}
	})
	_, err := CaptureWorkRecord(ctx, db, "room", "owner-a", nil)
	hrRequireErr[WorkRecordError](t, err, "")
}

func TestMigrationPreservesBytesInvalidRowsAndCallerTransaction(t *testing.T) {
	db := hrDB(t)
	ctx := context.Background()
	hrWorkCreate(db, "room")
	hrWorkCreate(db, "healthy")
	record := hrOK(CaptureWorkRecord(ctx, db, "room", "owner-a", nil))
	indented := hrOK(json.MarshalIndent(record, "", "   "))
	raw := string(indented)
	hrRawExec(t, db, "DROP TABLE "+WorkSourceTable, "DROP TRIGGER IF EXISTS trg_work_invalid_insert",
		"CREATE TABLE "+WorkSourceTable+" (room_id TEXT PRIMARY KEY, revision INTEGER, digest TEXT, record_json TEXT)")
	conn := hrRawConn(t, db)
	hrOK(conn.ExecContext(ctx, "INSERT INTO "+WorkSourceTable+" VALUES (?,?,?,?)", "room", record["revision"], record["digest"], raw))
	hrOK(conn.ExecContext(ctx, "INSERT INTO "+WorkSourceTable+" VALUES ('orphan',4,'broken','not-json')"))
	hrOK(conn.ExecContext(ctx, "BEGIN IMMEDIATE"))
	if err := workStorageInitialize(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if !hrOK(state.InTransaction(conn)) {
		t.Fatal("nested initialization ended the caller transaction")
	}
	hrOK(conn.ExecContext(ctx, "ROLLBACK"))
	if hrOK(hostedTableColumns(ctx, conn, WorkSourceTable))["producer_epoch"] {
		t.Fatal("rolled back migration persisted")
	}
	if err := workStorageInitialize(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if got := hrStr(hrOK(state.QueryRowObject(ctx, conn, "SELECT record_json FROM "+WorkSourceTable+" WHERE room_id='room'")), "record_json"); got != raw {
		t.Fatalf("migrated bytes %q", got)
	}
	if got := hrStr(hrOK(state.QueryRowObject(ctx, conn, "SELECT record_json FROM "+workInvalidTable)), "record_json"); got != "not-json" {
		t.Fatal(got)
	}
	hrOK(conn.ExecContext(ctx, "UPDATE "+WorkSourceTable+" SET record_json='bad' WHERE room_id='room'"))
	conn.Close()
	hrOK(CaptureWorkRecord(ctx, db, "healthy", "owner-a", nil))
	if got := hrStr(hrRawRow(t, db, "SELECT disposition FROM "+WorkSourceTable+" WHERE room_id='room'"), "disposition"); got != "current" {
		t.Fatal(got)
	}
	_, err := CaptureWorkRecord(ctx, db, "room", "owner-a", nil)
	hrRequireErr[InvalidStoredWorkRecord](t, err, "")
	row := hrRawRow(t, db, "SELECT disposition,record_json FROM "+WorkSourceTable+" WHERE room_id='room'")
	if hrStr(row, "disposition") != "invalid" || hrStr(row, "record_json") != "bad" {
		t.Fatalf("row %v", row)
	}
	conn = hrRawConn(t, db)
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "DELETE FROM "+workInvalidTable); !hrIsIntegrityError(err) {
		t.Fatalf("invalid evidence deleted: %v", err)
	}
	previous := workMaxStoreBytes
	workMaxStoreBytes = 1
	t.Cleanup(func() { workMaxStoreBytes = previous })
	if err := workStorageInitialize(ctx, conn); err != nil {
		t.Fatal(err)
	}
	_, err = PrepareWorkDeliveryLocked(ctx, conn, "healthy", "peer", "route", "owner-a", 100)
	hrRequireErr[WorkRecordCapacityError](t, err, "")
}

func TestRemoteReceiptIsCapturedWithoutAnotherDriverTransition(t *testing.T) {
	db := hrDB(t)
	ctx := context.Background()
	hrOK(CreateRoom(ctx, db, CreateRoomInput{RoomID: "room", Name: "Room", Members: []any{map[string]any{"profile": "ops", "handle": "ops",
		"target": map[string]any{"kind": "peer", "installation_id": "peer", "profile": "ops"}}}, AuthorityGatewayID: "owner"}))
	hrOK(AppendEvent(ctx, db, AppendEventInput{RoomID: "room", EventID: "input", Kind: "message.user", Actor: map[string]any{"kind": "user", "id": "alice"},
		Payload: map[string]any{"text": "hello"}, AuthorityGatewayID: "owner", AuthorityEpoch: 1}))
	hrOK(DriverAdmitTask(ctx, db, hrOK(NewTaskIdentity("room", "task", "thread", "turn")), map[string]any{"target_profile": "ops", "prompt": "hello", "source_event_seq": 1}, hrTen))
	record := map[string]any{"room_id": "room", "home_install_id": "owner", "authority_gateway_id": "owner", "authority_epoch": 1, "member_id": "ops",
		"target_install_id": "peer", "target_profile": "ops", "task_id": "task", "execution_generation": 1, "run_id": "run", "session_id": "session"}
	if err := UpsertRemoteRunReceipt(ctx, db, record, nil); err != nil {
		t.Fatal(err)
	}
	evidence := hrOK(roomLoads(hrStr(hrRawRow(t, db, "SELECT record_json FROM "+WorkSourceTable+" WHERE disposition='current' ORDER BY revision DESC LIMIT 1"), "record_json")))
	if !pyjson.Equal(evidence.(map[string]any)["receipts"], []any{record}) {
		t.Fatalf("evidence %v", evidence)
	}
}

// Current validated source evidence of the store
func hrSnapshot(t *testing.T, db string) map[string]any {
	t.Helper()
	row := hrRawRow(t, db, "SELECT record_json FROM "+WorkSourceTable+" WHERE disposition='current'")
	if row == nil {
		t.Fatal("no current evidence")
	}
	return hrOK(ValidateWorkRecord(hrOK(roomLoads(hrStr(row, "record_json")))))
}

func hrWorkPhase(record map[string]any) any { return workList(record["tasks"])[0]["phase"] }

// Room with one admitted task under owner, the input event appended
func hrWorkRoom(t *testing.T, prompt string) (string, TaskIdentity, map[string]any) {
	t.Helper()
	db := hrDB(t)
	ctx := context.Background()
	hrOK(CreateRoom(ctx, db, CreateRoomInput{RoomID: "room", Name: "Room", Members: []any{map[string]any{"profile": "ops", "handle": "ops"}}, AuthorityGatewayID: "owner", Now: hrFloatPtr(10)}))
	hrOK(AppendEvent(ctx, db, AppendEventInput{RoomID: "room", EventID: "input", Kind: "message.user", Actor: map[string]any{"kind": "user", "id": "alice"},
		Payload: map[string]any{"text": "hello"}, AuthorityGatewayID: "owner", AuthorityEpoch: 1}))
	identity := hrOK(NewTaskIdentity("room", "task", "thread", "turn"))
	payload := map[string]any{"target_profile": "ops", "prompt": prompt, "source_event_seq": 1}
	hrOK(DriverAdmitTask(ctx, db, identity, payload, hrTen))
	return db, identity, payload
}

func TestDriverTransitionsCaptureWithoutManualPublication(t *testing.T) {
	db, identity, payload := hrWorkRoom(t, "private input")
	ctx := context.Background()
	at21 := func() any { return 21.0 }
	queued := hrSnapshot(t, db)
	if hrWorkPhase(queued) != "queued" || strings.Contains(hrOK(WorkEncode(queued)), "private input") {
		t.Fatalf("queued %v", queued)
	}
	hrOK(DriverAdmitTask(ctx, db, identity, payload, hrTen))
	if !pyjson.Equal(hrSnapshot(t, db), queued) {
		t.Fatal("idempotent admission changed evidence")
	}
	lease := hrOK(DriverAcquireLease(ctx, db, "room", "owner", 1, "process-a", 10, hrTen))
	hrOK(DriverStartTask(ctx, db, identity, lease, 0, hrTen))
	running := hrSnapshot(t, db)
	if running["revision"].(int64) <= queued["revision"].(int64) || hrWorkPhase(running) != "running" {
		t.Fatalf("running %v", running)
	}
	successor := hrOK(DriverAcquireLease(ctx, db, "room", "owner", 1, "process-b", 10, at21))
	hrOK(DriverRecoverRoom(ctx, db, successor, at21))
	unknown := hrSnapshot(t, db)
	if hrWorkPhase(unknown) != "indeterminate" {
		t.Fatalf("unknown %v", unknown)
	}
	_, err := DriverStartTask(ctx, db, identity, successor, 0, at21)
	hrRequireErr[InvalidTaskTransitionError](t, err, "")
	if !pyjson.Equal(hrSnapshot(t, db), unknown) {
		t.Fatal("refused start changed evidence")
	}
	hrOK(DriverBeginTaskCancel(ctx, db, identity, "stop", 0, at21))
	if phase := hrWorkPhase(hrSnapshot(t, db)); phase != "stopping" {
		t.Fatal(phase)
	}
	hrOK(DriverCompleteTaskCancel(ctx, db, identity, "stop", 1, at21))
	if phase := hrWorkPhase(hrSnapshot(t, db)); phase != "cancelled" {
		t.Fatal(phase)
	}
}

func TestBrokenPassiveEvidenceDoesNotAuthorizeOrBlockStop(t *testing.T) {
	db, identity, _ := hrWorkRoom(t, "hello")
	ctx := context.Background()
	hrSnapshot(t, db)
	hrWithConn(t, db, func(conn state.SQLiteConn) {
		hrOK(conn.ExecContext(ctx, "UPDATE "+WorkSourceTable+" SET record_json='broken'"))
	})
	hrOK(DriverCancelTask(ctx, db, identity, "stop", 0, hrTen))
	if status := hrOK(DriverGetTask(ctx, db, identity))["status"]; status != "cancelled" {
		t.Fatal(status)
	}
	row := hrRawRow(t, db, "SELECT disposition,record_json FROM "+WorkSourceTable)
	if hrStr(row, "disposition") != "invalid" || hrStr(row, "record_json") != "broken" {
		t.Fatalf("row %v", row)
	}
}
