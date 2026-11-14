package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	hrUser     = map[string]any{"kind": "user", "id": "desktop-user", "display_name": "User"}
	hrGatewayA = map[string]any{"kind": "gateway", "id": "gateway-a"}
	hrGatewayB = map[string]any{"kind": "gateway", "id": "gateway-b"}
)

func hrFloatPtr(v float64) *float64 { return &v }

// Fresh room store path under a hermetic home
func hrDB(t *testing.T) string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return filepath.Join(t.TempDir(), "state.db")
}

func hrCreate(db, roomID string) (map[string]any, error) {
	return CreateRoom(context.Background(), db, CreateRoomInput{RoomID: roomID, Name: "Release room",
		Members: []any{map[string]any{"profile": "ops", "handle": "ops"}}, AuthorityGatewayID: "gateway-a", Now: hrFloatPtr(10)})
}

func hrAppend(db string, in AppendEventInput) (map[string]any, error) {
	if in.Kind == "message.user" {
		if in.AuthorityGatewayID == nil {
			in.AuthorityGatewayID = "gateway-a"
		}
		if in.AuthorityEpoch == nil {
			in.AuthorityEpoch = int64(1)
		}
	}
	return AppendEvent(context.Background(), db, in)
}

func hrDisband(db, roomID string, gatewayID any, epoch any, now *float64) (map[string]any, error) {
	if gatewayID == nil {
		gatewayID = "gateway-a"
	}
	if epoch == nil {
		epoch = int64(1)
	}
	return DisbandRoom(context.Background(), db, roomID, gatewayID, epoch, now)
}

func hrRead(db, roomID string, since, limit int64, includeDisbanded bool) (map[string]any, error) {
	return ReadEvents(context.Background(), db, roomID, since, limit, includeDisbanded)
}

// Value of a call that must succeed, a panic failing the test otherwise
func hrOK[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func hrRequireErr[T any, PT interface {
	*T
	error
}](t *testing.T, err error, contains string) PT {
	t.Helper()
	var target PT
	if !errors.As(err, &target) {
		t.Fatalf("error %v (%T) is not %T", err, err, target)
	}
	if contains != "" && !strings.Contains(err.Error(), contains) {
		t.Fatalf("error %q does not contain %q", err, contains)
	}
	return target
}

func hrSeqs(page map[string]any) []any {
	var out []any
	for _, event := range page["events"].([]any) {
		out = append(out, event.(map[string]any)["seq"])
	}
	return out
}

func hrClaim(db, roomID, expected string, epoch int64, next, eventID string, now *float64) (map[string]any, error) {
	return ClaimAuthority(context.Background(), db, roomID, expected, epoch, next, eventID, now)
}

func TestCreateRoomIsIdempotentButConflictsFailClosed(t *testing.T) {
	db := hrDB(t)
	first := hrOK(hrCreate(db, "room-1"))
	second := hrOK(hrCreate(db, "room-1"))
	if first["idempotent"] != false || second["idempotent"] != true || first["room_id"] != "room-1" || second["room_id"] != "room-1" {
		t.Fatalf("first %v second %v", first, second)
	}
	_, err := CreateRoom(context.Background(), db, CreateRoomInput{RoomID: "room-1", Name: "A different room", Members: []any{}, AuthorityGatewayID: "gateway-a"})
	hrRequireErr[RoomConflictError](t, err, "")
}

func TestConcurrentFirstDatabaseOpenKeepsEveryRoom(t *testing.T) {
	db := hrDB(t)
	var wg sync.WaitGroup
	ids := make([]string, 16)
	errs := make([]error, 16)
	sem := make(chan struct{}, 8)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			room, err := hrCreate(db, fmt.Sprintf("room-%d", i))
			errs[i] = err
			if err == nil {
				ids[i] = room["room_id"].(string)
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		if ids[i] != fmt.Sprintf("room-%d", i) {
			t.Fatalf("room %d is %q", i, ids[i])
		}
	}
	if listed := hrOK(ListRooms(context.Background(), db, false, int64(MaxRoomListLimit), int64(0))); len(listed) != 16 {
		t.Fatalf("listed %d rooms", len(listed))
	}
}

func TestAuthorityClaimFencesStaleGatewayEvents(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	turn := func(eventID string, actor any, gateway string, epoch int64, kind string, payload map[string]any) (map[string]any, error) {
		return hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: eventID, Kind: kind, Actor: actor, AuthorityGatewayID: gateway, AuthorityEpoch: epoch, Payload: payload})
	}
	first := hrOK(turn("turn-1", hrGatewayA, "gateway-a", 1, "turn.started", map[string]any{"member": "ops"}))
	claimed := hrOK(hrClaim(db, "room-1", "gateway-a", 1, "gateway-b", "claim-gateway-b", hrFloatPtr(30)))
	retried := hrOK(hrClaim(db, "room-1", "gateway-a", 1, "gateway-b", "claim-gateway-b", hrFloatPtr(40)))
	claimEvent := claimed["claim_event"].(map[string]any)
	if claimed["authority_gateway_id"] != "gateway-b" || claimed["authority_epoch"] != int64(2) || claimed["idempotent"] != false ||
		claimEvent["kind"] != "authority.claimed" || claimEvent["authority_epoch"] != int64(2) {
		t.Fatalf("claimed %v", claimed)
	}
	if !pyjson.Equal(claimEvent["payload"], map[string]any{"previous_gateway_id": "gateway-a", "authority_gateway_id": "gateway-b", "authority_epoch": 2}) {
		t.Fatalf("claim payload %v", claimEvent["payload"])
	}
	retriedEvent := retried["claim_event"].(map[string]any)
	if retried["authority_epoch"] != int64(2) || retried["idempotent"] != true || retriedEvent["seq"] != claimEvent["seq"] || retriedEvent["idempotent"] != true {
		t.Fatalf("retried %v", retried)
	}
	roomState := hrOK(RoomState(context.Background(), db, "room-1", false))
	authorityClaim := roomState["authority_claim"].(map[string]any)
	if authorityClaim["event_id"] != "claim-gateway-b" || authorityClaim["payload"].(map[string]any)["previous_gateway_id"] != "gateway-a" {
		t.Fatalf("state %v", roomState)
	}
	repeated := hrOK(turn("turn-1", hrGatewayA, "gateway-a", 1, "turn.started", map[string]any{"member": "ops"}))
	if repeated["seq"] != first["seq"] || repeated["idempotent"] != true {
		t.Fatalf("repeated %v", repeated)
	}
	_, err := turn("turn-2-stale", hrGatewayA, "gateway-a", 1, "turn.started", map[string]any{"member": "ops"})
	hrRequireErr[AuthorityConflictError](t, err, "stale")
	_, err = turn("member-2-stale", map[string]any{"kind": "member", "id": "ops"}, "gateway-a", 1, "message.member", map[string]any{"text": "stale result"})
	hrRequireErr[AuthorityConflictError](t, err, "stale")
	current := hrOK(turn("turn-2-current", hrGatewayB, "gateway-b", 2, "turn.started", map[string]any{"member": "ops"}))
	member := hrOK(turn("member-2-current", map[string]any{"kind": "member", "id": "ops"}, "gateway-b", 2, "message.member", map[string]any{"text": "current result"}))
	if current["authority_epoch"] != int64(2) || member["authority_epoch"] != int64(2) {
		t.Fatalf("current %v member %v", current, member)
	}
}

func TestConcurrentAuthorityClaimHasOneWinner(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	results := make([]map[string]any, 2)
	var wg sync.WaitGroup
	for i, gateway := range []string{"gateway-b", "gateway-c"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := hrClaim(db, "room-1", "gateway-a", 1, gateway, "claim-"+gateway, nil)
			var conflict *AuthorityConflictError
			if err != nil && !errors.As(err, &conflict) {
				t.Error(err)
			}
			results[i] = claimed
		}()
	}
	wg.Wait()
	var winners []map[string]any
	for _, result := range results {
		if result != nil {
			winners = append(winners, result)
		}
	}
	if len(winners) != 1 || winners[0]["authority_epoch"] != int64(2) {
		t.Fatalf("winners %v", winners)
	}
	owner := hrOK(RoomState(context.Background(), db, "room-1", false))["authority_gateway_id"]
	if owner != "gateway-b" && owner != "gateway-c" {
		t.Fatalf("owner %v", owner)
	}
}

func TestRetryOfSuccessfulButSupersededClaimIsDistinct(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	hrOK(hrClaim(db, "room-1", "gateway-a", 1, "gateway-b", "claim-b", nil))
	hrOK(hrClaim(db, "room-1", "gateway-b", 2, "gateway-c", "claim-c", nil))
	_, err := hrClaim(db, "room-1", "gateway-a", 1, "gateway-b", "claim-b", nil)
	hrRequireErr[AuthoritySupersededError](t, err, "later superseded")
}

func TestAuthorityScopedEventsRequireGatewayAndEpoch(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	_, err := AppendEvent(context.Background(), db, AppendEventInput{RoomID: "room-1", EventID: "turn-1", Kind: "member.unavailable", Actor: hrGatewayA, Payload: map[string]any{"member": "ops"}})
	hrRequireErr[HostedRoomError](t, err, "authority_gateway_id")
	_, err = hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: "turn-2", Kind: "turn.started", Actor: hrGatewayB, AuthorityGatewayID: "gateway-a", AuthorityEpoch: int64(1), Payload: map[string]any{"member": "ops"}})
	hrRequireErr[HostedRoomError](t, err, "actor.id")
	_, err = AppendEvent(context.Background(), db, AppendEventInput{RoomID: "room-1", EventID: "message-1", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "hello"}})
	hrRequireErr[HostedRoomError](t, err, "authority_gateway_id")
	appended := hrOK(hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: "message-1", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "hello"}}))
	if appended["authority_epoch"] != int64(1) {
		t.Fatalf("appended %v", appended)
	}
}

func TestAppendIsIdempotentAndConflictingEventIdIsRejected(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	in := AppendEventInput{RoomID: "room-1", EventID: "event-1", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "hello"}, Now: hrFloatPtr(20)}
	first := hrOK(hrAppend(db, in))
	in.Now = hrFloatPtr(30)
	repeated := hrOK(hrAppend(db, in))
	if first["seq"] != int64(1) || repeated["seq"] != int64(1) || repeated["idempotent"] != true {
		t.Fatalf("first %v repeated %v", first, repeated)
	}
	if latest := hrOK(hrRead(db, "room-1", 0, 100, false))["latest_seq"]; latest != int64(1) {
		t.Fatalf("latest %v", latest)
	}
	in.Payload, in.Now = map[string]any{"text": "changed"}, nil
	_, err := hrAppend(db, in)
	hrRequireErr[EventConflictError](t, err, "")
}

func TestSinceSeqReturnsOrderedDeltasAndStableCursor(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	for index := 1; index < 5; index++ {
		hrOK(hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: fmt.Sprintf("event-%d", index), Kind: "message.user", Actor: hrUser,
			Payload: map[string]any{"index": index}, Now: hrFloatPtr(float64(20 + index))}))
	}
	first := hrOK(hrRead(db, "room-1", 0, 2, false))
	if !pyjson.Equal(hrSeqs(first), []any{1, 2}) || first["cursor"] != int64(2) || first["latest_seq"] != int64(4) || first["has_more"] != true ||
		!pyjson.Equal(first["authority"], map[string]any{"gateway_id": "gateway-a", "epoch": 1}) || len(first) != 5 {
		t.Fatalf("first %v", first)
	}
	second := hrOK(hrRead(db, "room-1", first["cursor"].(int64), 2, false))
	if !pyjson.Equal(hrSeqs(second), []any{3, 4}) || second["cursor"] != int64(4) || second["has_more"] != false {
		t.Fatalf("second %v", second)
	}
	settled := hrOK(hrRead(db, "room-1", 4, 100, false))
	if len(settled["events"].([]any)) != 0 || settled["cursor"] != int64(4) || settled["latest_seq"] != int64(4) {
		t.Fatalf("settled %v", settled)
	}
}

func TestRoomLogSurvivesStoreReopen(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	hrOK(hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: "event-1", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "persist me"}}))
	if rooms := hrOK(ListRooms(context.Background(), db, false, int64(MaxRoomListLimit), int64(0))); rooms[0]["name"] != "Release room" {
		t.Fatalf("rooms %v", rooms)
	}
	replay := hrOK(hrRead(db, "room-1", 0, 100, false))
	if !pyjson.Equal(replay["events"].([]any)[0].(map[string]any)["payload"], map[string]any{"text": "persist me"}) {
		t.Fatalf("replay %v", replay)
	}
}

func TestRoomTablesCoexistWithSessionDbSchema(t *testing.T) {
	for _, sessionFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(sessionFirst), func(t *testing.T) {
			db := hrDB(t)
			ctx := context.Background()
			if sessionFirst {
				hrOK(state.NewSessionDB(ctx, db, false)).Close()
			}
			hrOK(hrCreate(db, "room-1"))
			hrOK(hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: "event-1", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "shared database"}}))
			hrOK(state.NewSessionDB(ctx, db, false)).Close()
			replay := hrOK(hrRead(db, "room-1", 0, 100, false))
			if replay["latest_seq"] != int64(1) || replay["events"].([]any)[0].(map[string]any)["payload"].(map[string]any)["text"] != "shared database" {
				t.Fatalf("replay %v", replay)
			}
		})
	}
}

func TestConcurrentAppendsAllocateOneMonotonicSequence(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	seqs := make([]int64, 24)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			event, err := hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: fmt.Sprintf("event-%d", i), Kind: "message.user", Actor: hrUser, Payload: map[string]any{"index": i}})
			if err != nil {
				t.Error(err)
				return
			}
			seqs[i] = event["seq"].(int64)
		}()
	}
	wg.Wait()
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("seqs %v", seqs)
		}
	}
	replay := hrSeqs(hrOK(hrRead(db, "room-1", 0, 100, false)))
	for i, seq := range replay {
		if seq != int64(i+1) || len(replay) != 24 {
			t.Fatalf("replay %v", replay)
		}
	}
}

// Raw sqlite connection the way sqlite3.connect opens one
func hrRawConn(t *testing.T, db string) state.SQLiteConn {
	t.Helper()
	return hrOK(state.ConnectSQLite(db, state.SQLiteConfig{Timeout: 5}))
}

func TestRolledBackAppendDoesNotConsumeSequence(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	ctx := context.Background()
	conn := hrRawConn(t, db)
	for _, statement := range []string{"BEGIN IMMEDIATE", `INSERT INTO hosted_room_events
               (room_id, seq, event_id, kind, actor_json, payload_json, created_at)
               VALUES ('room-1', 1, 'crash-event', 'message.user',
                       '{"id":"desktop-user","kind":"user"}', '{}', 20)`, "UPDATE hosted_rooms SET next_seq=2 WHERE room_id='room-1'", "ROLLBACK"} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	event := hrOK(hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: "after-restart", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "safe"}}))
	if event["seq"] != int64(1) {
		t.Fatalf("event %v", event)
	}
}

func TestInvalidEventContractIsRejected(t *testing.T) {
	cases := []struct {
		mutate  func(in *AppendEventInput)
		message string
	}{
		{func(in *AppendEventInput) { in.RoomID = "../escape" }, "invalid room_id"},
		{func(in *AppendEventInput) { in.EventID = "event id" }, "invalid event_id"},
		{func(in *AppendEventInput) { in.Kind = "Message Created" }, "invalid event kind"},
		{func(in *AppendEventInput) { in.Kind = "directive.run" }, "cannot append"},
		{func(in *AppendEventInput) { in.Actor = map[string]any{"kind": "member", "id": "ops"} }, "cannot append"},
		{func(in *AppendEventInput) { in.Payload = []any{} }, "payload must be an object"},
	}
	for _, tc := range cases {
		t.Run(tc.message, func(t *testing.T) {
			db := hrDB(t)
			hrOK(hrCreate(db, "room-1"))
			in := AppendEventInput{RoomID: "room-1", EventID: "event-1", Kind: "message.user", Actor: hrUser, Payload: map[string]any{}}
			tc.mutate(&in)
			_, err := hrAppend(db, in)
			hrRequireErr[HostedRoomError](t, err, tc.message)
		})
	}
}

func TestUnknownRoomAndInvalidCursorFailClosed(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	_, err := hrRead(db, "missing", 0, 100, false)
	hrRequireErr[RoomNotFoundError](t, err, "")
	_, err = hrRead(db, "room-1", -1, 100, false)
	hrRequireErr[HostedRoomError](t, err, "since_seq")
	_, err = hrRead(db, "room-1", 1, 100, false)
	hrRequireErr[HostedRoomError](t, err, "ahead")
	_, err = hrRead(db, "room-1", 0, 0, false)
	hrRequireErr[HostedRoomError](t, err, "limit")
}

func TestActorIsPartOfEventIdempotencyAndReplay(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	event := hrOK(hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: "event-1", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "hello"}}))
	if !pyjson.Equal(event["actor"], hrUser) {
		t.Fatalf("actor %v", event["actor"])
	}
	if replayed := hrOK(hrRead(db, "room-1", 0, 100, false))["events"].([]any)[0].(map[string]any)["actor"]; !pyjson.Equal(replayed, hrUser) {
		t.Fatalf("replayed actor %v", replayed)
	}
	_, err := hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: "event-1", Kind: "message.user", Actor: map[string]any{"kind": "user", "id": "another-user"}, Payload: map[string]any{"text": "hello"}})
	hrRequireErr[EventConflictError](t, err, "")
}

func TestDisbandIsIdempotentAndRoomIdCannotBeReused(t *testing.T) {
	db := hrDB(t)
	ctx := context.Background()
	hrOK(hrCreate(db, "room-1"))
	first := hrOK(hrDisband(db, "room-1", nil, nil, hrFloatPtr(50)))
	repeated := hrOK(hrDisband(db, "room-1", nil, nil, hrFloatPtr(60)))
	firstEvent := first["event"].(map[string]any)
	if first["room_id"] != "room-1" || first["disbanded_at"] != 50.0 || first["idempotent"] != false || firstEvent["kind"] != "room.disbanded" ||
		firstEvent["seq"] != int64(1) || !pyjson.Equal(firstEvent["payload"], map[string]any{"room_id": "room-1"}) {
		t.Fatalf("first %v", first)
	}
	if repeated["idempotent"] != true || repeated["event"].(map[string]any)["seq"] != firstEvent["seq"] {
		t.Fatalf("repeated %v", repeated)
	}
	if live := hrOK(ListRooms(ctx, db, false, int64(MaxRoomListLimit), int64(0))); len(live) != 0 {
		t.Fatalf("live %v", live)
	}
	deleted := hrOK(ListRooms(ctx, db, true, int64(MaxRoomListLimit), int64(0)))
	if deleted[0]["disbanded_at"] != 50.0 || deleted[0]["latest_seq"] != int64(1) {
		t.Fatalf("deleted %v", deleted)
	}
	roomState := hrOK(RoomState(ctx, db, "room-1", true))
	if roomState["disbanded_at"] != 50.0 || roomState["latest_seq"] != int64(1) {
		t.Fatalf("state %v", roomState)
	}
	replay := hrOK(hrRead(db, "room-1", 0, 100, true))
	if events := replay["events"].([]any); len(events) != 1 || events[0].(map[string]any)["kind"] != "room.disbanded" {
		t.Fatalf("replay %v", replay)
	}
	_, err := hrCreate(db, "room-1")
	hrRequireErr[RoomConflictError](t, err, "")
	_, err = hrRead(db, "room-1", 0, 100, false)
	hrRequireErr[RoomNotFoundError](t, err, "")
	var expired *RoomHistoryExpiredError
	if errors.As(err, &expired) {
		t.Fatal("a retained tombstone is not history expired")
	}
}

func TestDisbandRejectsStaleAuthority(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	hrOK(hrClaim(db, "room-1", "gateway-a", 1, "gateway-b", "claim-b", nil))
	_, err := hrDisband(db, "room-1", nil, nil, nil)
	hrRequireErr[AuthorityConflictError](t, err, "stale")
	tombstone := hrOK(hrDisband(db, "room-1", "gateway-b", int64(2), nil))
	if tombstone["event"].(map[string]any)["authority_epoch"] != int64(2) {
		t.Fatalf("tombstone %v", tombstone)
	}
}

// Row of one raw query, nil when it returns none
func hrRawRow(t *testing.T, db, query string, args ...any) *pyjson.Object {
	t.Helper()
	conn := hrRawConn(t, db)
	defer conn.Close()
	row, err := state.QueryRowObject(context.Background(), conn, query, args...)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return row
}
