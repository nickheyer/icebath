package gateway

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var (
	hrReplicaUser    = map[string]any{"kind": "user", "id": "tek"}
	hrReplicaMembers = []any{map[string]any{"kind": "bot", "id": "planner"}, map[string]any{"kind": "bot", "id": "coder"}}
	hrAuthA          = "install:" + strings.Repeat("a", 32)
	hrAuthB          = "install:" + strings.Repeat("b", 32)
)

// Points the local authority gateway id at the given install through a home holding that install identity
func hrUseInstall(t *testing.T, id string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "install_id"), []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	if got := hrOK(LocalAuthorityGatewayID(context.Background())); got != "install:"+id {
		t.Fatalf("local authority %s", got)
	}
}

func hrSeedRoom(t *testing.T, db string, events int) map[string]any {
	t.Helper()
	ctx := context.Background()
	hrOK(CreateRoom(ctx, db, CreateRoomInput{RoomID: "room-1", Name: "Field Room", Members: hrReplicaMembers, AuthorityGatewayID: hrAuthA}))
	for index := range events {
		hrOK(AppendEvent(ctx, db, AppendEventInput{RoomID: "room-1", EventID: fmt.Sprintf("e%d", index), Kind: "message.user", Actor: hrReplicaUser,
			Payload: map[string]any{"text": fmt.Sprintf("msg %d \U0001F600", index)}, AuthorityGatewayID: hrAuthA, AuthorityEpoch: 1}))
	}
	return hrOK(ReadEvents(ctx, db, "room-1", 0, 100, false))
}

func hrIngest(db string, page any) (map[string]any, error) {
	return IngestReplicaPage(context.Background(), db, "room-1", "Field Room", hrReplicaMembers, page, nil)
}

func hrReplicaDBs(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	return filepath.Join(dir, "authority.db"), filepath.Join(dir, "replica.db")
}

func hrStaleAppend(db, eventID string) error {
	_, err := AppendEvent(context.Background(), db, AppendEventInput{RoomID: "room-1", EventID: eventID, Kind: "message.user", Actor: hrReplicaUser,
		Payload: map[string]any{"text": "stale"}, AuthorityGatewayID: hrAuthA, AuthorityEpoch: 1})
	return err
}

func TestIngestPagePersistsEventsAndLineage(t *testing.T) {
	adb, rdb := hrReplicaDBs(t)
	page := hrSeedRoom(t, adb, 3)
	result := hrOK(hrIngest(rdb, page))
	if result["ingested"] != 3 || result["stored_seq"] != int64(3) || result["caught_up"] != true {
		t.Fatalf("result %v", result)
	}
	replica := hrOK(ReplicaState(context.Background(), rdb, "room-1"))
	if replica["last_seq"] != int64(3) || !pyjson.Equal(replica["authority"], page["authority"]) || !pyjson.Equal(replica["members"], hrReplicaMembers) {
		t.Fatalf("replica %v", replica)
	}
}

func TestIngestPageIsIdempotent(t *testing.T) {
	adb, rdb := hrReplicaDBs(t)
	page := hrSeedRoom(t, adb, 3)
	hrOK(hrIngest(rdb, page))
	if again := hrOK(hrIngest(rdb, page)); again["ingested"] != 0 || again["stored_seq"] != int64(3) {
		t.Fatalf("again %v", again)
	}
}

func TestIngestRejectsSequenceGap(t *testing.T) {
	adb, rdb := hrReplicaDBs(t)
	hrSeedRoom(t, adb, 5)
	later := hrOK(ReadEvents(context.Background(), adb, "room-1", 2, 100, false))
	_, err := hrIngest(rdb, later)
	hrRequireErr[ReplicaGapError](t, err, "")
}

func TestIngestRejectsEpochRegression(t *testing.T) {
	adb, rdb := hrReplicaDBs(t)
	page := hrSeedRoom(t, adb, 3)
	newer := pyjson.Plain(page).(map[string]any)
	newer["authority"].(map[string]any)["epoch"] = 3
	hrOK(hrIngest(rdb, newer))
	stale := pyjson.Plain(page).(map[string]any)
	stale["authority"].(map[string]any)["epoch"] = 2
	_, err := hrIngest(rdb, stale)
	hrRequireErr[ReplicaEpochRegressionError](t, err, "")
}

func TestIngestRequiresAuthorityStamp(t *testing.T) {
	adb, rdb := hrReplicaDBs(t)
	page := hrSeedRoom(t, adb, 3)
	delete(page, "authority")
	_, err := hrIngest(rdb, page)
	hrRequireErr[ReplicaError](t, err, "")
}

func TestPromoteReplicaContinuesRoomAtNextEpoch(t *testing.T) {
	adb, rdb := hrReplicaDBs(t)
	ctx := context.Background()
	hrOK(hrIngest(rdb, hrSeedRoom(t, adb, 3)))
	hrUseInstall(t, strings.Repeat("b", 32))
	promoted := hrOK(PromoteReplica(ctx, rdb, "room-1", "authority-unreachable", nil))
	if promoted["authority_gateway_id"] != hrAuthB || promoted["authority_epoch"] != int64(2) || promoted["previous_gateway_id"] != hrAuthA || promoted["claim_seq"] != int64(4) {
		t.Fatalf("promoted %v", promoted)
	}
	replay := hrOK(ReadEvents(ctx, rdb, "room-1", 0, 100, false))
	if !pyjson.Equal(hrSeqs(replay), []any{1, 2, 3, 4}) {
		t.Fatalf("replay %v", replay)
	}
	events := replay["events"].([]any)
	claim := events[len(events)-1].(map[string]any)
	payload := claim["payload"].(map[string]any)
	if claim["kind"] != "authority.claimed" || payload["previous_gateway_id"] != hrAuthA || !pyjson.Equal(payload["authority_epoch"], 2) ||
		!pyjson.Equal(replay["authority"], map[string]any{"gateway_id": hrAuthB, "epoch": 2}) {
		t.Fatalf("claim %v replay %v", claim, replay["authority"])
	}
	hrOK(AppendEvent(ctx, rdb, AppendEventInput{RoomID: "room-1", EventID: "post-takeover", Kind: "message.user", Actor: hrReplicaUser,
		Payload: map[string]any{"text": "continuing"}, AuthorityGatewayID: hrAuthB, AuthorityEpoch: 2}))
	hrRequireErr[HostedRoomError](t, hrStaleAppend(rdb, "stale-write"), "")
	_, err := ReplicaState(ctx, rdb, "room-1")
	hrRequireErr[ReplicaError](t, err, "")
}

func TestPromoteRefusesWhenRoomExistsLocally(t *testing.T) {
	adb, _ := hrReplicaDBs(t)
	hrOK(hrIngest(adb, hrSeedRoom(t, adb, 3)))
	hrUseInstall(t, strings.Repeat("b", 32))
	_, err := PromoteReplica(context.Background(), adb, "room-1", "authority-unreachable", nil)
	hrRequireErr[RoomConflictError](t, err, "")
}

func TestPromoteRefusesWhenAlreadyAuthority(t *testing.T) {
	adb, rdb := hrReplicaDBs(t)
	hrOK(hrIngest(rdb, hrSeedRoom(t, adb, 3)))
	hrUseInstall(t, strings.Repeat("a", 32))
	_, err := PromoteReplica(context.Background(), rdb, "room-1", "authority-unreachable", nil)
	hrRequireErr[ReplicaError](t, err, "")
}

func TestDemoteFencesStaleLocalAuthority(t *testing.T) {
	adb, _ := hrReplicaDBs(t)
	ctx := context.Background()
	hrSeedRoom(t, adb, 3)
	hrUseInstall(t, strings.Repeat("a", 32))
	result := hrOK(DemoteRoom(ctx, adb, "room-1", hrAuthB, 2, nil))
	if result["idempotent"] != false || result["authority_gateway_id"] != hrAuthB || result["authority_epoch"] != int64(2) {
		t.Fatalf("result %v", result)
	}
	replay := hrOK(ReadEvents(ctx, adb, "room-1", 0, 100, false))
	events := replay["events"].([]any)
	lost := events[len(events)-1].(map[string]any)
	if lost["kind"] != "authority.lost" || lost["payload"].(map[string]any)["authority_gateway_id"] != hrAuthB ||
		!pyjson.Equal(replay["authority"], map[string]any{"gateway_id": hrAuthB, "epoch": 2}) {
		t.Fatalf("lost %v", lost)
	}
	hrRequireErr[HostedRoomError](t, hrStaleAppend(adb, "after-demote"), "")
	if again := hrOK(DemoteRoom(ctx, adb, "room-1", hrAuthB, 2, nil)); again["idempotent"] != true {
		t.Fatalf("again %v", again)
	}
}

func TestDemoteRejectsNonSupersedingEpoch(t *testing.T) {
	adb, _ := hrReplicaDBs(t)
	hrSeedRoom(t, adb, 3)
	hrUseInstall(t, strings.Repeat("a", 32))
	_, err := DemoteRoom(context.Background(), adb, "room-1", hrAuthB, 1, nil)
	hrRequireErr[ReplicaEpochRegressionError](t, err, "")
}

func TestFullFailoverRoundTrip(t *testing.T) {
	adb, rdb := hrReplicaDBs(t)
	ctx := context.Background()
	hrOK(hrIngest(rdb, hrSeedRoom(t, adb, 4)))
	hrUseInstall(t, strings.Repeat("b", 32))
	promoted := hrOK(PromoteReplica(ctx, rdb, "room-1", "authority-unreachable", nil))
	hrOK(AppendEvent(ctx, rdb, AppendEventInput{RoomID: "room-1", EventID: "b-work", Kind: "message.user", Actor: hrReplicaUser,
		Payload: map[string]any{"text": "work continues on B"}, AuthorityGatewayID: hrAuthB, AuthorityEpoch: promoted["authority_epoch"]}))
	hrUseInstall(t, strings.Repeat("a", 32))
	hrOK(DemoteRoom(ctx, adb, "room-1", hrAuthB, promoted["authority_epoch"], nil))
	hrRequireErr[HostedRoomError](t, hrStaleAppend(adb, "a-stale"), "")
	replay := hrOK(ReadEvents(ctx, rdb, "room-1", 0, 100, false))
	var kinds []any
	for _, event := range replay["events"].([]any) {
		kinds = append(kinds, event.(map[string]any)["kind"])
	}
	want := []any{"message.user", "message.user", "message.user", "message.user", "authority.claimed", "message.user"}
	if !pyjson.Equal(kinds, want) || replay["authority"].(map[string]any)["gateway_id"] != hrAuthB {
		t.Fatalf("kinds %v authority %v", kinds, replay["authority"])
	}
}
