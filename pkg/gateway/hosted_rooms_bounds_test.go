package gateway

import (
	"context"
	"fmt"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Sets a store bound for one test, restoring it after
func hrSetBound(t *testing.T, bound *int64, value int64) {
	t.Helper()
	previous := *bound
	*bound = value
	t.Cleanup(func() { *bound = previous })
}

func hrPageLen(t *testing.T, page map[string]any) int64 {
	t.Helper()
	encoded := hrOK(pyjson.Dumps(page, pyjson.EnsureASCII(false), pyjson.Separators(",", ":")))
	return int64(len(encoded))
}

func hrAssertRetiredIdentityStaysReserved(t *testing.T, db, roomID, freshID string) {
	t.Helper()
	if hrRawRow(t, db, "SELECT 1 FROM hosted_rooms WHERE room_id=?", roomID) != nil {
		t.Fatal("room payload survived pruning")
	}
	if hrRawRow(t, db, "SELECT retired_at FROM hosted_room_retired_ids WHERE room_id=?", roomID) == nil {
		t.Fatal("room id is not reserved")
	}
	_, err := hrCreate(db, roomID)
	hrRequireErr[RoomConflictError](t, err, "disbanded")
	_, err = hrAppend(db, AppendEventInput{RoomID: roomID, EventID: "stale-send", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "stale"}})
	if expired := hrRequireErr[RoomHistoryExpiredError](t, err, ""); expired.Reason() != "room_history_expired" {
		t.Fatal(expired.Reason())
	}
	_, err = hrRead(db, roomID, 0, 100, true)
	if expired := hrRequireErr[RoomHistoryExpiredError](t, err, ""); expired.Reason() != "room_history_expired" {
		t.Fatal(expired.Reason())
	}
	repeated := hrOK(hrDisband(db, roomID, nil, nil, hrFloatPtr(999)))
	if !pyjson.Equal(repeated, map[string]any{"room_id": roomID, "disbanded_at": 20.0, "idempotent": true, "history_expired": true}) {
		t.Fatalf("repeated %v", repeated)
	}
	if created := hrOK(hrCreate(db, freshID)); created["room_id"] != freshID {
		t.Fatalf("created %v", created)
	}
}

func TestRoomLogPagesAreBoundedBySerializedEventBytes(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	text := ""
	for range 180 {
		text += "x"
	}
	for index := range 4 {
		hrOK(hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: fmt.Sprintf("message-%d", index), Kind: "message.user", Actor: hrUser,
			Payload: map[string]any{"text": text, "index": index}, Now: hrFloatPtr(1_700_000_000.5)}))
	}
	oneEvent := hrOK(hrRead(db, "room-1", 0, 1, false))
	hrSetBound(t, &maxLogPageBytes, hrPageLen(t, oneEvent)+1)
	first := hrOK(hrRead(db, "room-1", 0, 4, false))
	if len(first["events"].([]any)) != 1 || first["has_more"] != true {
		t.Fatalf("first %v", first)
	}
	second := hrOK(hrRead(db, "room-1", first["cursor"].(int64), 4, false))
	if second["events"].([]any)[0].(map[string]any)["seq"] != first["cursor"].(int64)+1 {
		t.Fatalf("second %v", second)
	}
}

func TestRoomLogPageBoundIncludesJsonStructureOverhead(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	for index := range 20 {
		hrOK(hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: fmt.Sprintf("small-%02d", index), Kind: "message.user", Actor: hrUser,
			Payload: map[string]any{"text": "ok", "index": index}}))
	}
	hrSetBound(t, &maxLogPageBytes, 1_000)
	var cursor int64
	var replayed []any
	for {
		page := hrOK(hrRead(db, "room-1", cursor, 20, false))
		if len(page["events"].([]any)) == 0 || hrPageLen(t, page) > 1_000 || page["cursor"].(int64) <= cursor {
			t.Fatalf("page %v", page)
		}
		replayed = append(replayed, hrSeqs(page)...)
		cursor = page["cursor"].(int64)
		if page["has_more"] != true {
			break
		}
	}
	for i, seq := range replayed {
		if seq != int64(i+1) || len(replayed) != 20 {
			t.Fatalf("replayed %v", replayed)
		}
	}
}

func TestActiveRoomsAndEventStorageAreBounded(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	hrSetBound(t, &maxActiveRooms, 1)
	_, err := hrCreate(db, "room-2")
	hrRequireErr[HostedRoomError](t, err, "too many active")
	hrOK(hrDisband(db, "room-1", nil, nil, hrFloatPtr(20)))
	hrOK(hrCreate(db, "room-2"))
	first := hrOK(hrAppend(db, AppendEventInput{RoomID: "room-2", EventID: "message-1", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "first"}}))
	current := hrRawRow(t, db, "SELECT event_bytes FROM hosted_rooms WHERE room_id='room-2'")
	hrSetBound(t, &maxRoomEventBytes, hrInt(current, "event_bytes"))
	_, err = hrAppend(db, AppendEventInput{RoomID: "room-2", EventID: "message-2", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "second"}})
	hrRequireErr[HostedRoomError](t, err, "storage limit")
	if events := hrOK(hrRead(db, "room-2", 0, 100, false))["events"]; !pyjson.Equal(events, []any{first}) {
		t.Fatalf("events %v", events)
	}
	if kind := hrOK(hrDisband(db, "room-2", nil, nil, nil))["event"].(map[string]any)["kind"]; kind != "room.disbanded" {
		t.Fatal(kind)
	}
}

func TestRoomListingIsPagedAndOldTombstonesArePruned(t *testing.T) {
	db := hrDB(t)
	ctx := context.Background()
	hrSetBound(t, &maxDisbandedRoomTombstones, 1)
	for index := range 3 {
		roomID := fmt.Sprintf("room-%d", index)
		hrOK(hrCreate(db, roomID))
		if index < 2 {
			hrOK(hrDisband(db, roomID, nil, nil, hrFloatPtr(float64(20+index))))
		}
	}
	firstPage := hrOK(ListRooms(ctx, db, true, int64(1), int64(0)))
	secondPage := hrOK(ListRooms(ctx, db, true, int64(1), int64(1)))
	if len(firstPage) != 1 || len(secondPage) != 1 || firstPage[0]["room_id"] != "room-1" || secondPage[0]["room_id"] != "room-2" {
		t.Fatalf("pages %v %v", firstPage, secondPage)
	}
	_, err := RoomState(ctx, db, "room-0", true)
	hrRequireErr[RoomNotFoundError](t, err, "")
	hrAssertRetiredIdentityStaysReserved(t, db, "room-0", "room-new")
}

func TestRetentionPruningKeepsRetiredRoomIdReserved(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-old"))
	hrOK(hrDisband(db, "room-old", nil, nil, hrFloatPtr(20)))
	if pruned := hrOK(PruneDisbandedRooms(context.Background(), db, hrFloatPtr(20+disbandedRoomRetentionSeconds+1))); pruned != 1 {
		t.Fatalf("pruned %d", pruned)
	}
	hrAssertRetiredIdentityStaysReserved(t, db, "room-old", "room-new")
}

func TestBytePressurePruningKeepsRetiredRoomIdReserved(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-full"))
	hrSetBound(t, &MaxGatewayEventBytes, 0)
	hrOK(hrDisband(db, "room-full", nil, nil, hrFloatPtr(20)))
	hrAssertRetiredIdentityStaysReserved(t, db, "room-full", "room-new")
}

func TestPeerReservationRejectsStaleOrConflictingAuthority(t *testing.T) {
	db := hrDB(t)
	ctx := context.Background()
	claims := func(gateway string, epoch int64) *pyjson.Object {
		return pyjson.ObjectOf("room_id", "room-peer", "member_id", "member-peer", "target_profile", "reviewer", "authority_gateway_id", gateway, "authority_epoch", epoch)
	}
	if err := ReservePeerRoom(ctx, db, claims("gateway-current", 2), 300, hrFloatPtr(100)); err != nil {
		t.Fatal(err)
	}
	err := ReservePeerRoom(ctx, db, claims("gateway-stale", 1), 300, hrFloatPtr(100))
	hrRequireErr[AuthorityConflictError](t, err, "authority changed")
	err = ReservePeerRoom(ctx, db, claims("gateway-conflict", 2), 300, hrFloatPtr(100))
	hrRequireErr[AuthorityConflictError](t, err, "authority changed")
	if !hrOK(PeerRoomIsReserved(ctx, db, "room-peer", "reviewer", hrFloatPtr(200))) {
		t.Fatal("reservation is not live")
	}
}
