package gateway

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var hrImportMembers = []any{map[string]any{"member_id": "a", "profile": "default", "handle": "a"}, map[string]any{"member_id": "b", "profile": "default", "handle": "b"}}

// Standalone source and gateway target stores, the target holding the owner claim table
func hrImportStores(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	source, target := filepath.Join(dir, "shared-state.db"), filepath.Join(dir, "state.db")
	hrRawExec(t, target, "CREATE TABLE state_meta (key TEXT PRIMARY KEY, value TEXT)")
	return source, target
}

func hrStandaloneRoom(t *testing.T, source, roomID string) {
	t.Helper()
	ctx := context.Background()
	local := hrOK(LocalAuthorityGatewayID(ctx))
	hrOK(CreateRoom(ctx, source, CreateRoomInput{RoomID: roomID, Name: roomID, Members: hrImportMembers, AuthorityGatewayID: local}))
	hrOK(AppendEvent(ctx, source, AppendEventInput{RoomID: roomID, EventID: "user:" + roomID, Kind: "message.user", Actor: map[string]any{"kind": "user", "id": "desktop"},
		Payload: map[string]any{"text": "hello"}, AuthorityGatewayID: local, AuthorityEpoch: 1}))
}

func hrListAll(t *testing.T, db string, includeDisbanded bool) []map[string]any {
	t.Helper()
	return hrOK(ListRooms(context.Background(), db, includeDisbanded, int64(MaxRoomListLimit), int64(0)))
}

func TestStandaloneRoomsMoveOnceWithHistoryOwnerAndOneDriver(t *testing.T) {
	source, target := hrImportStores(t)
	ctx := context.Background()
	hrStandaloneRoom(t, source, "room-desk")
	if copied := hrOK(ImportStandaloneRooms(ctx, target, nil)); copied != 1 {
		t.Fatalf("copied %d", copied)
	}
	if copied := hrOK(ImportStandaloneRooms(ctx, target, nil)); copied != 0 {
		t.Fatalf("second import copied %d", copied)
	}
	local := hrOK(LocalAuthorityGatewayID(ctx))
	listed := hrListAll(t, target, false)
	if len(listed) != 1 || listed[0]["room_id"] != "room-desk" || listed[0]["authority_gateway_id"] != local {
		t.Fatalf("listed %v", listed)
	}
	var kinds []any
	users := 0
	for _, event := range hrOK(ReadEvents(ctx, target, "room-desk", 0, 100, false))["events"].([]any) {
		kind := event.(map[string]any)["kind"]
		kinds = append(kinds, kind)
		if kind == "message.user" {
			users++
		}
	}
	if users != 1 || !pyjson.Equal(kinds[len(kinds)-2:], []any{"authority.lost", "authority.claimed"}) {
		t.Fatalf("kinds %v", kinds)
	}
	if owner := hrStr(hrRawRow(t, target, "SELECT value FROM state_meta WHERE key=?", "gateway.hosted.owner.v1:room-desk"), "value"); owner != HostOwnerSubject() {
		t.Fatalf("owner %q", owner)
	}
	kept := hrListAll(t, source, false)
	if len(kept) != 1 || kept[0]["authority_gateway_id"] != hrOK(HandoffGatewayID(ctx)) {
		t.Fatalf("kept %v", kept)
	}
}

func TestARoomMadeStandaloneAfterTheImportMovesOnTheNextStart(t *testing.T) {
	source, target := hrImportStores(t)
	ctx := context.Background()
	hrStandaloneRoom(t, source, "room-one")
	hrOK(ImportStandaloneRooms(ctx, target, nil))
	hrStandaloneRoom(t, source, "room-two")
	if copied := hrOK(ImportStandaloneRooms(ctx, target, nil)); copied != 1 {
		t.Fatalf("copied %d", copied)
	}
	var ids []string
	for _, room := range hrListAll(t, target, false) {
		ids = append(ids, room["room_id"].(string))
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "room-one" || ids[1] != "room-two" {
		t.Fatalf("ids %v", ids)
	}
}

func TestANamedProfileStoreImportsNothing(t *testing.T) {
	source, _ := hrImportStores(t)
	ctx := context.Background()
	hrStandaloneRoom(t, source, "room-desk")
	named := filepath.Join(filepath.Dir(source), "profiles", "work", "state.db")
	if err := os.MkdirAll(filepath.Dir(named), 0o755); err != nil {
		t.Fatal(err)
	}
	if copied := hrOK(ImportStandaloneRooms(ctx, named, nil)); copied != 0 {
		t.Fatalf("copied %d", copied)
	}
	if owner := hrListAll(t, source, false)[0]["authority_gateway_id"]; owner != hrOK(LocalAuthorityGatewayID(ctx)) {
		t.Fatalf("owner %v", owner)
	}
}

func TestARoomDisbandedInTheGatewayNeverComesBack(t *testing.T) {
	source, target := hrImportStores(t)
	ctx := context.Background()
	hrStandaloneRoom(t, source, "room-desk")
	hrOK(ImportStandaloneRooms(ctx, target, nil))
	room := hrListAll(t, target, false)[0]
	hrOK(DisbandRoom(ctx, target, "room-desk", room["authority_gateway_id"], room["authority_epoch"], nil))
	later := hostedClock(nil) + disbandedRoomRetentionSeconds + 10
	if pruned := hrOK(PruneDisbandedRooms(ctx, target, &later)); pruned != 1 {
		t.Fatalf("pruned %d", pruned)
	}
	if copied := hrOK(ImportStandaloneRooms(ctx, target, nil)); copied != 0 {
		t.Fatalf("copied %d", copied)
	}
	if listed := hrListAll(t, target, true); len(listed) != 0 {
		t.Fatalf("listed %v", listed)
	}
}
