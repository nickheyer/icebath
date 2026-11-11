package gateway

import (
	"context"
	"testing"
)

func TestTombstonePruningOwnsOnlyRoomLogDriverAndPolicyTables(t *testing.T) {
	db := hrDB(t)
	ctx := context.Background()
	hrOK(hrCreate(db, "room-1"))
	at20 := func() any { return 20.0 }
	hrOK(DriverAdmitTask(ctx, db, hrOK(NewTaskIdentity("room-1", "task-1", "thread-1", "turn-1")), map[string]any{"target_profile": "ops", "prompt": "Inspect.", "source_event_seq": 1}, at20))
	hrOK(DriverAcquireLease(ctx, db, "room-1", "gateway-a", 1, "process-a", 30, at20))
	hrOK(NewHostedRoomPolicyCheckpoint(ctx, db, MaxActivePolicyEvents))
	hrOK(hrDisband(db, "room-1", nil, nil, hrFloatPtr(50)))
	hrRawExec(t, db, `INSERT INTO hosted_room_policy_cursors(
                   room_id, through_seq, stopped_through_seq, updated_at
               ) VALUES ('room-1', 1, 0, 50)`, `INSERT INTO hosted_room_policy_threads
               VALUES ('room-1', 'thread-1', 'user-1', 1, 0)`, `INSERT INTO hosted_room_policy_events
               VALUES ('room-1', 'thread-1', 'user-1', 1, '{}')`, `INSERT INTO hosted_room_policy_watermarks
               VALUES ('room-1', 'thread-1', 'ops', 1)`, `INSERT INTO hosted_room_policy_publications
               VALUES ('room-1', 'task-1', 'turn.settled', 0, 1)`, `INSERT INTO hosted_room_policy_transcript
               VALUES (
                   'room-1', 'thread-1', 1, 'message.user', NULL
               )`, `CREATE TABLE hosted_room_messaging_refs (
                room_id TEXT NOT NULL,
                marker TEXT NOT NULL
            )`, `INSERT INTO hosted_room_policy_transcript_state
               VALUES ('room-1', 1)`, `INSERT INTO hosted_room_messaging_refs
               VALUES ('room-1', 'outside-pr-b')`)
	if pruned := hrOK(PruneDisbandedRooms(ctx, db, hrFloatPtr(50+disbandedRoomRetentionSeconds+1))); pruned != 1 {
		t.Fatalf("pruned %d", pruned)
	}
	for _, table := range []string{"hosted_rooms", "hosted_room_events", "hosted_room_driver_tasks", "hosted_room_driver_leases", "hosted_room_policy_cursors",
		"hosted_room_policy_threads", "hosted_room_policy_events", "hosted_room_policy_watermarks", "hosted_room_policy_publications",
		"hosted_room_policy_transcript", "hosted_room_policy_transcript_state"} {
		if count := hrInt(hrRawRow(t, db, "SELECT COUNT(*) AS n FROM "+table), "n"); count != 0 {
			t.Fatalf("%s kept %d rows", table, count)
		}
	}
	if marker := hrStr(hrRawRow(t, db, "SELECT marker FROM hosted_room_messaging_refs"), "marker"); marker != "outside-pr-b" {
		t.Fatal(marker)
	}
}
