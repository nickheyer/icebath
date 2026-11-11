package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Claims a legacy authority room for a real gateway behind a fenced claim event
func hostedAdoptLegacyRoom(ctx context.Context, conn state.SQLiteConn, existing *pyjson.Object, roomID, membersJSON, gatewayID string, now float64) (map[string]any, error) {
	targetEpoch := hrInt(existing, "authority_epoch") + 1
	seq := hrInt(existing, "next_seq")
	claimBytes, err := hostedInsertEventRow(ctx, conn, existing, roomID, seq, "system:authority-adopted", "authority.claimed",
		hostedSystemActorJSON("authority-control"), targetEpoch, hostedClaimPayloadJSON("legacy", gatewayID, targetEpoch), now, true)
	if err != nil {
		return nil, err
	}
	if err := roomFencedUpdate(ctx, conn, `UPDATE hosted_rooms
            SET members_json=?, authority_gateway_id=?, authority_epoch=?,
                next_seq=next_seq+1, revision=revision+1, event_bytes=event_bytes+?, updated_at=?
            WHERE room_id=? AND authority_gateway_id='legacy' AND authority_epoch=? AND next_seq=?
            AND disbanded_at IS NULL`, []any{membersJSON, gatewayID, targetEpoch, claimBytes, now, roomID, hrInt(existing, "authority_epoch"), seq},
		authorityConflict("legacy room adoption lost its fence")); err != nil {
		return nil, err
	}
	reloaded, err := hostedReload(ctx, conn, hostedSelectRoom, []any{roomID}, "adopted room could not be reloaded")
	if err != nil {
		return nil, err
	}
	claimRow, err := hostedReload(ctx, conn, hostedSelectEvent, []any{roomID, "system:authority-adopted"}, "legacy adoption event could not be reloaded")
	if err != nil {
		return nil, err
	}
	room, err := hostedRoomFromRow(reloaded, true)
	if err != nil {
		return nil, err
	}
	claim, err := hostedEventFromRow(claimRow, false)
	if err != nil {
		return nil, err
	}
	room["adopted"] = true
	room["claim_event"] = claim
	return room, nil
}

// Room creation request, admit running first inside the write transaction
type CreateRoomInput struct {
	RoomID, Name, Members, AuthorityGatewayID any
	Now                                       *float64
	Admit                                     func(conn state.SQLiteConn) error
}

// Creates a room, or returns the identical existing room idempotently
func CreateRoom(ctx context.Context, dbPath string, in CreateRoomInput) (map[string]any, error) {
	roomID, err := hostedRoomID(in.RoomID)
	if err != nil {
		return nil, err
	}
	name, err := hostedRoomName(in.Name)
	if err != nil {
		return nil, err
	}
	members, membersJSON, err := hostedValidateMembers(in.Members)
	if err != nil {
		return nil, err
	}
	gatewayID, err := hostedActorID(in.AuthorityGatewayID, "authority_gateway_id")
	if err != nil {
		return nil, err
	}
	now := hostedClock(in.Now)
	var out map[string]any
	err = hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		if in.Admit != nil {
			if err := in.Admit(conn); err != nil {
				return err
			}
		}
		retired, err := hostedIsRetired(ctx, conn, roomID)
		if err != nil {
			return err
		}
		if retired {
			return roomConflict("room_id belongs to a disbanded room")
		}
		existing, err := state.QueryRowObject(ctx, conn, hostedSelectRoomWithBytes, roomID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Vals["disbanded_at"] != nil {
				return roomConflict("room_id belongs to a disbanded room")
			}
			legacyAdoption := hrStr(existing, "authority_gateway_id") == "legacy" && gatewayID != "legacy"
			membersMatch := hrStr(existing, "members_json") == membersJSON || (legacyAdoption && hostedLegacyMembersMatch(hrStr(existing, "members_json"), members))
			if hrStr(existing, "name") != name || !membersMatch {
				return roomConflict("room_id already exists with different state")
			}
			if legacyAdoption {
				out, err = hostedAdoptLegacyRoom(ctx, conn, existing, roomID, membersJSON, gatewayID, now)
				return err
			}
			if hrStr(existing, "authority_gateway_id") != gatewayID {
				return roomConflict("room_id already belongs to a different authority")
			}
			out, err = hostedRoomFromRow(existing, true)
			return err
		}
		var active int64
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM hosted_rooms WHERE disbanded_at IS NULL").Scan(&active); err != nil {
			return err
		}
		if active >= maxActiveRooms {
			return hostedRoomError("This host has too many active Group Chats. Delete one and try again.")
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO hosted_rooms ("+hostedRoomColumnsWithBytes+`)
                VALUES (?, ?, ?, ?, 1, 1, 0, 1, ?, ?, NULL)`, roomID, name, membersJSON, gatewayID, now, now); err != nil {
			return err
		}
		row, err := hostedReload(ctx, conn, `SELECT room_id, name, members_json, authority_gateway_id, authority_epoch, revision,
                created_at, updated_at FROM hosted_rooms WHERE room_id=?`, []any{roomID}, "created room could not be reloaded")
		if err != nil {
			return err
		}
		if out, err = hostedRoomFromRow(row, false); err != nil {
			return err
		}
		normalized := make([]any, len(members))
		for i, member := range members {
			normalized[i] = member
		}
		out["members"] = normalized
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// One bounded read only page of rooms ordered by most recent change
func ListRooms(ctx context.Context, dbPath string, includeDisbanded bool, limit, offset any) ([]map[string]any, error) {
	pageLimit, err := hostedBoundedLimit(limit, MaxRoomListLimit)
	if err != nil {
		return nil, err
	}
	pageOffset, err := hostedNonNegative(offset, "offset")
	if err != nil {
		return nil, err
	}
	conn, err := hostedReadConnection(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := state.QueryObjects(ctx, conn, "SELECT "+hostedRoomColumns+` FROM hosted_rooms WHERE disbanded_at IS NULL OR ?
                ORDER BY updated_at DESC, room_id ASC LIMIT ? OFFSET ?`, hostedBoolInt(includeDisbanded), pageLimit, pageOffset)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		if out[i], err = hostedRoomFromRow(row, false); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// One or zero for a flag bound into SQL
func hostedBoolInt(flag bool) int {
	if flag {
		return 1
	}
	return 0
}

// Renames a live room and appends its replay event atomically
func RenameRoom(ctx context.Context, dbPath string, roomIDValue, eventIDValue, nameValue any, now *float64) (map[string]any, error) {
	roomID, err := hostedRoomID(roomIDValue)
	if err != nil {
		return nil, err
	}
	eventID, err := hostedEventID(eventIDValue)
	if err != nil {
		return nil, err
	}
	name, err := hostedRoomName(nameValue)
	if err != nil {
		return nil, err
	}
	timestamp := hostedClock(now)
	actorJSON := hostedSystemActorJSON("room-control")
	payloadJSON, err := hostedPayloadJSON(map[string]any{"name": name})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		room, err := hostedRoomRow(ctx, conn, hostedSelectRoomWithBytes, []any{roomID}, roomID)
		if err != nil {
			return err
		}
		if room.Vals["disbanded_at"] != nil {
			return roomNotFound("hosted room not found")
		}
		existing, err := hostedLoadEvent(ctx, conn, roomID, eventID)
		if err != nil {
			return err
		}
		if existing != nil {
			if hrStr(existing, "kind") != "room.renamed" || hrStr(existing, "payload_json") != payloadJSON {
				return eventConflict("event_id already exists with different immutable content")
			}
			if out, err = hostedRoomFromRow(room, true); err != nil {
				return err
			}
			out["event"], err = hostedEventFromRow(existing, true)
			return err
		}
		seq := hrInt(room, "next_seq")
		eventBytes, err := hostedPrepareEvent(ctx, conn, room, eventID, "room.renamed", actorJSON, payloadJSON, false)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE hosted_rooms
                SET name=?, next_seq=?, event_bytes=event_bytes+?, revision=revision+1, updated_at=?
                WHERE room_id=?`, name, seq+1, eventBytes, timestamp, roomID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, hostedInsertEvent, roomID, seq, eventID, "room.renamed", actorJSON, hrInt(room, "authority_epoch"), payloadJSON, timestamp); err != nil {
			return err
		}
		updated, err := state.QueryRowObject(ctx, conn, hostedSelectRoom, roomID)
		if err != nil {
			return err
		}
		if out, err = hostedRoomFromRow(updated, false); err != nil {
			return err
		}
		event, err := hostedLoadEvent(ctx, conn, roomID, eventID)
		if err != nil {
			return err
		}
		out["event"], err = hostedEventFromRow(event, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// One event append, ExpectedLatestSeq nil for no cursor check
type AppendEventInput struct {
	RoomID, EventID, Kind, Actor, Payload any
	AuthorityGatewayID, AuthorityEpoch    any
	Now                                   *float64
	ExpectedLatestSeq                     any
	ControlReserve                        bool
}

// Appends one immutable event under the next per room sequence, an identical repeat returning the original
func AppendEvent(ctx context.Context, dbPath string, in AppendEventInput) (map[string]any, error) {
	roomID, err := hostedRoomID(in.RoomID)
	if err != nil {
		return nil, err
	}
	eventID, err := hostedEventID(in.EventID)
	if err != nil {
		return nil, err
	}
	var expected *int64
	if in.ExpectedLatestSeq != nil {
		n, err := roomBoundedInt(in.ExpectedLatestSeq, hostedRoomError, "expected_latest_seq must be a nonnegative integer", 0, nil)
		if err != nil {
			return nil, err
		}
		value := n.Int64()
		expected = &value
	}
	kind, err := hostedEventKind(in.Kind)
	if err != nil {
		return nil, err
	}
	actor, actorJSON, err := hostedValidateActor(in.Actor, kind)
	if err != nil {
		return nil, err
	}
	gatewayID, err := hostedActorID(in.AuthorityGatewayID, "authority_gateway_id")
	if err != nil {
		return nil, err
	}
	if actor["kind"] == "gateway" && actor["id"] != gatewayID {
		return nil, hostedRoomError("gateway actor.id must match authority_gateway_id")
	}
	epoch, err := hostedRequirePositiveInt(in.AuthorityEpoch, "authority_epoch")
	if err != nil {
		return nil, err
	}
	payload, ok := hostedMapping(in.Payload)
	if !ok {
		return nil, hostedRoomError("payload must be an object")
	}
	payloadJSON, err := hostedPayloadJSON(pyjson.Plain(payload))
	if err != nil {
		return nil, err
	}
	now := hostedClock(in.Now)
	var out map[string]any
	err = hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		existing, err := hostedLoadEvent(ctx, conn, roomID, eventID)
		if err != nil {
			return err
		}
		if existing != nil {
			if !hostedEventContentEquals(existing, kind, actorJSON, epoch, payloadJSON) {
				return eventConflict("event_id already exists with different content")
			}
			out, err = hostedEventFromRow(existing, true)
			return err
		}
		room, err := hostedRoomRow(ctx, conn, `SELECT next_seq, event_bytes, authority_gateway_id, authority_epoch
                FROM hosted_rooms WHERE room_id=? AND disbanded_at IS NULL`, []any{roomID}, roomID)
		if err != nil {
			return err
		}
		if err := hostedRequireAuthority(room, gatewayID, epoch, "stale hosted room authority"); err != nil {
			return err
		}
		seq := hrInt(room, "next_seq")
		if expected != nil && seq-1 != *expected {
			return eventCursorConflict("room changed before event publication")
		}
		if kind == "message.user" || kind == "message.member" {
			decoded, err := roomLoads(payloadJSON)
			if err != nil {
				return err
			}
			manifest, has := decoded.(map[string]any)["attachments"]
			if !has {
				manifest = []any{}
			}
			if err := retainMessageAttachments(ctx, conn, roomID, eventID, manifest, now); err != nil {
				return err
			}
		}
		allowControl := hostedControlEventKinds[kind] || (in.ControlReserve && actor["kind"] == "gateway")
		eventBytes, err := hostedInsertEventRow(ctx, conn, room, roomID, seq, eventID, kind, actorJSON, epoch, payloadJSON, now, allowControl)
		if err != nil {
			return err
		}
		if err := roomFencedUpdate(ctx, conn, `UPDATE hosted_rooms SET next_seq=?, event_bytes=event_bytes+?, updated_at=?
                WHERE room_id=? AND next_seq=?`, []any{seq + 1, eventBytes, now, roomID, seq},
			errors.New("hosted room sequence advance lost its write fence")); err != nil {
			return err
		}
		row, err := hostedReload(ctx, conn, "SELECT "+hostedEventColumns+" FROM hosted_room_events WHERE room_id=? AND seq=?", []any{roomID, seq}, "appended event could not be reloaded")
		if err != nil {
			return err
		}
		if out, err = hostedEventFromRow(row, false); err != nil {
			return err
		}
		out["actor"] = actor
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Non blocking existence probe with a short timeout and no schema creation or migration
func hostedProbe(ctx context.Context, path, table, query string, args []any, unavailable string) (bool, error) {
	if !hostedIsFile(path) {
		return false, nil
	}
	found, err := func() (bool, error) {
		conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 0.05})
		if err != nil {
			return false, err
		}
		defer conn.Close()
		tableRow, err := state.QueryRowObject(ctx, conn, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='"+table+"' LIMIT 1")
		if err != nil || tableRow == nil {
			return false, err
		}
		row, err := state.QueryRowObject(ctx, conn, query, args...)
		return row != nil, err
	}()
	if err != nil {
		return false, &RoomProbeUnavailableError{&HostedRoomError{unavailable}}
	}
	return found, nil
}

// Room ownership check that never creates or migrates the shared store and fails fast under contention
func ProbeHostedRoom(ctx context.Context, dbPath string, roomID any) (bool, error) {
	room, err := hostedRoomID(roomID)
	if err != nil {
		return false, err
	}
	return hostedProbe(ctx, dbPath, "hosted_rooms", "SELECT 1 FROM hosted_rooms WHERE room_id=? AND disbanded_at IS NULL LIMIT 1",
		[]any{room}, "hosted room ownership is temporarily unavailable")
}

// Peer reservation check that never creates or migrates shared state
func ProbePeerRoomReservation(ctx context.Context, dbPath string, roomID, targetProfile any, now *float64) (bool, error) {
	room, err := hostedRoomID(roomID)
	if err != nil {
		return false, err
	}
	profile, err := hostedActorID(targetProfile, "target_profile")
	if err != nil {
		return false, err
	}
	return hostedProbe(ctx, dbPath, "hosted_room_peer_reservations", hostedSelectLiveReservation, []any{room, profile, hostedClock(now)},
		"peer room ownership is temporarily unavailable")
}

// Durable replay and authority state of one room
func RoomState(ctx context.Context, dbPath string, roomIDValue any, includeDisbanded bool) (map[string]any, error) {
	roomID, err := hostedRoomID(roomIDValue)
	if err != nil {
		return nil, err
	}
	var row, claimRow *pyjson.Object
	err = hostedTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) error {
		var err error
		row, err = hostedRoomRow(ctx, conn, "SELECT "+hostedRoomColumns+` FROM hosted_rooms WHERE room_id=? AND (disbanded_at IS NULL
                OR ?)`, []any{roomID, hostedBoolInt(includeDisbanded)}, roomID)
		if err != nil {
			return err
		}
		claimRow, err = state.QueryRowObject(ctx, conn, "SELECT "+hostedEventColumns+` FROM hosted_room_events WHERE room_id=?
                AND kind='authority.claimed' AND authority_epoch=? ORDER BY seq DESC LIMIT 1`, roomID, hrInt(row, "authority_epoch"))
		return err
	})
	if err != nil {
		return nil, err
	}
	room, err := hostedRoomFromRow(row, false)
	if err != nil {
		return nil, err
	}
	if claimRow != nil {
		if room["authority_claim"], err = hostedEventFromRow(claimRow, false); err != nil {
			return nil, err
		}
	}
	return room, nil
}

// Appends an idempotent fence that supersedes earlier user turns
func RequestRoomStop(ctx context.Context, dbPath string, roomID, cancelIDValue, expectedGatewayID, expectedEpoch any) (map[string]any, error) {
	cancelID, err := roomIdentifier(cancelIDValue, roomIdentifierSpec{label: "cancel_id", maxChars: maxEventIDChars}, hostedRoomError)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(cancelID))
	return AppendEvent(ctx, dbPath, AppendEventInput{RoomID: roomID, EventID: "room-stop:" + hex.EncodeToString(sum[:])[:32],
		Kind: "room.stop_requested", Actor: map[string]any{"kind": "gateway", "id": expectedGatewayID},
		Payload: map[string]any{"cancel_id": cancelID}, AuthorityGatewayID: expectedGatewayID, AuthorityEpoch: expectedEpoch})
}

// Fences a verified authority transfer with a compare and swap epoch
func ClaimAuthority(ctx context.Context, dbPath string, roomIDValue, expectedGatewayValue, expectedEpoch, newGatewayValue, eventIDValue any, now *float64) (map[string]any, error) {
	roomID, err := hostedRoomID(roomIDValue)
	if err != nil {
		return nil, err
	}
	expectedGatewayID, err := hostedActorID(expectedGatewayValue, "expected_gateway_id")
	if err != nil {
		return nil, err
	}
	newGatewayID, err := hostedActorID(newGatewayValue, "new_gateway_id")
	if err != nil {
		return nil, err
	}
	eventID, err := hostedEventID(eventIDValue)
	if err != nil {
		return nil, err
	}
	epoch, err := hostedRequirePositiveInt(expectedEpoch, "expected_epoch")
	if err != nil {
		return nil, err
	}
	timestamp := hostedClock(now)
	targetEpoch := epoch + 1
	claimActorJSON := hostedSystemActorJSON("authority-control")
	claimPayloadJSON := hostedClaimPayloadJSON(expectedGatewayID, newGatewayID, targetEpoch)
	var stateRow, existingEvent *pyjson.Object
	idempotent := false
	err = hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		row, err := hostedRoomRow(ctx, conn, `SELECT authority_gateway_id, authority_epoch, next_seq, event_bytes
                FROM hosted_rooms WHERE room_id=? AND disbanded_at IS NULL`, []any{roomID}, roomID)
		if err != nil {
			return err
		}
		if existingEvent, err = hostedLoadEvent(ctx, conn, roomID, eventID); err != nil {
			return err
		}
		idempotent = existingEvent != nil
		if idempotent {
			if !hostedEventContentEquals(existingEvent, "authority.claimed", claimActorJSON, targetEpoch, claimPayloadJSON) {
				return eventConflict("event_id already exists with different content")
			}
			if fmt.Sprint(row.Vals["authority_gateway_id"]) != newGatewayID || hrInt(row, "authority_epoch") != targetEpoch {
				return &AuthoritySupersededError{authorityConflict("authority claim succeeded but was later superseded")}
			}
		} else {
			if err := hostedRequireAuthority(row, expectedGatewayID, epoch, "hosted room authority changed"); err != nil {
				return err
			}
			claimBytes, err := hostedInsertEventRow(ctx, conn, row, roomID, hrInt(row, "next_seq"), eventID, "authority.claimed", claimActorJSON, targetEpoch, claimPayloadJSON, timestamp, true)
			if err != nil {
				return err
			}
			if err := roomFencedUpdate(ctx, conn, `UPDATE hosted_rooms SET authority_gateway_id=?,
            authority_epoch=authority_epoch+1, next_seq=next_seq+1,
            event_bytes=event_bytes+?, revision=revision+1, updated_at=? WHERE room_id=?
            AND disbanded_at IS NULL AND authority_gateway_id=? AND authority_epoch=?`,
				[]any{newGatewayID, claimBytes, timestamp, roomID, expectedGatewayID, epoch}, authorityConflict("hosted room authority changed")); err != nil {
				return err
			}
			if existingEvent, err = hostedLoadEvent(ctx, conn, roomID, eventID); err != nil {
				return err
			}
		}
		stateRow, err = hostedReload(ctx, conn, `SELECT room_id, name, members_json, authority_gateway_id, authority_epoch, next_seq,
                revision, created_at, updated_at FROM hosted_rooms WHERE room_id=?`, []any{roomID}, "claimed room could not be reloaded")
		return err
	})
	if err != nil {
		return nil, err
	}
	if existingEvent == nil {
		return nil, errors.New("authority claim event could not be reloaded")
	}
	room, err := hostedRoomFromRow(stateRow, idempotent)
	if err != nil {
		return nil, err
	}
	if room["claim_event"], err = hostedEventFromRow(existingEvent, idempotent); err != nil {
		return nil, err
	}
	return room, nil
}

// Idempotent replay of a retired or already disbanded room, nil while the room is live
func hostedDisbandReplay(ctx context.Context, conn state.SQLiteConn, roomID string, room *pyjson.Object) (map[string]any, error) {
	if room == nil {
		retired, err := state.QueryRowObject(ctx, conn, "SELECT retired_at FROM hosted_room_retired_ids WHERE room_id=?", roomID)
		if err != nil {
			return nil, err
		}
		if retired == nil {
			return nil, roomNotFound("hosted room not found")
		}
		return map[string]any{"room_id": roomID, "disbanded_at": hrFloat(retired, "retired_at"), "idempotent": true, "history_expired": true}, nil
	}
	if room.Vals["disbanded_at"] == nil {
		return nil, nil
	}
	if _, err := conn.ExecContext(ctx, hostedInsertRetired, roomID, hrFloat(room, "disbanded_at")); err != nil {
		return nil, err
	}
	event, err := hostedLoadEvent(ctx, conn, roomID, "system:room-disbanded")
	if err != nil {
		return nil, err
	}
	replay := map[string]any{"room_id": roomID, "disbanded_at": hrFloat(room, "disbanded_at"), "idempotent": true}
	if event != nil {
		if replay["event"], err = hostedEventFromRow(event, true); err != nil {
			return nil, err
		}
	}
	return replay, nil
}

// Tombstones a room id permanently and idempotently
func DisbandRoom(ctx context.Context, dbPath string, roomIDValue, expectedGatewayValue, expectedEpoch any, now *float64) (map[string]any, error) {
	roomID, err := hostedRoomID(roomIDValue)
	if err != nil {
		return nil, err
	}
	expectedGatewayID, err := hostedActorID(expectedGatewayValue, "expected_gateway_id")
	if err != nil {
		return nil, err
	}
	epoch, err := hostedRequirePositiveInt(expectedEpoch, "expected_epoch")
	if err != nil {
		return nil, err
	}
	timestamp := hostedClock(now)
	var out map[string]any
	err = hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		room, err := state.QueryRowObject(ctx, conn, `SELECT authority_gateway_id, authority_epoch, next_seq, event_bytes, disbanded_at
                FROM hosted_rooms WHERE room_id=?`, roomID)
		if err != nil {
			return err
		}
		replay, err := hostedDisbandReplay(ctx, conn, roomID, room)
		if err != nil || replay != nil {
			out = replay
			return err
		}
		if err := hostedRequireAuthority(room, expectedGatewayID, epoch, "stale hosted room authority"); err != nil {
			return err
		}
		payloadJSON, err := hostedPayloadJSON(map[string]any{"room_id": roomID})
		if err != nil {
			return err
		}
		disbandBytes, err := hostedInsertEventRow(ctx, conn, room, roomID, hrInt(room, "next_seq"), "system:room-disbanded", "room.disbanded",
			hostedSystemActorJSON("room-control"), hrInt(room, "authority_epoch"), payloadJSON, timestamp, true)
		if err != nil {
			return err
		}
		if err := roomFencedUpdate(ctx, conn, `UPDATE hosted_rooms
                SET disbanded_at=?, updated_at=?, revision=revision+1,
                    next_seq=next_seq+1, event_bytes=event_bytes+?
                WHERE room_id=? AND disbanded_at IS NULL AND authority_gateway_id=?
                AND authority_epoch=?`, []any{timestamp, timestamp, disbandBytes, roomID, expectedGatewayID, epoch},
			roomConflict("hosted room disband lost its fence")); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, hostedInsertRetired, roomID, timestamp); err != nil {
			return err
		}
		eventRow, err := hostedReload(ctx, conn, hostedSelectEvent, []any{roomID, "system:room-disbanded"}, "room disband event could not be reloaded")
		if err != nil {
			return err
		}
		budget := MaxGatewayEventBytes
		if _, err := hostedPruneDisbandedRoomsLocked(ctx, conn, &timestamp, &budget); err != nil {
			return err
		}
		event, err := hostedEventFromRow(eventRow, false)
		out = map[string]any{"room_id": roomID, "disbanded_at": timestamp, "idempotent": false, "event": event}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Monotonic room log delta after sinceSeq within the page byte budget
func ReadEvents(ctx context.Context, dbPath string, roomIDValue, sinceSeqValue, limitValue any, includeDisbanded bool) (map[string]any, error) {
	roomID, err := hostedRoomID(roomIDValue)
	if err != nil {
		return nil, err
	}
	sinceSeq, err := hostedNonNegative(sinceSeqValue, "since_seq")
	if err != nil {
		return nil, err
	}
	limit, err := hostedBoundedLimit(limitValue, MaxRoomLogLimit)
	if err != nil {
		return nil, err
	}
	var latestSeq int64
	var authority map[string]any
	var rows []*pyjson.Object
	err = hostedTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) error {
		room, err := hostedRoomRow(ctx, conn, `SELECT next_seq, authority_gateway_id, authority_epoch FROM hosted_rooms
                WHERE room_id=? AND (disbanded_at IS NULL OR ?)`, []any{roomID, hostedBoolInt(includeDisbanded)}, roomID)
		if err != nil {
			return err
		}
		latestSeq = hrInt(room, "next_seq") - 1
		authority = map[string]any{"gateway_id": fmt.Sprint(room.Vals["authority_gateway_id"]), "epoch": hrInt(room, "authority_epoch")}
		if sinceSeq > latestSeq {
			return hostedRoomError("since_seq is ahead of the hosted room log")
		}
		rows, err = state.QueryObjects(ctx, conn, `WITH candidates AS (
                   SELECT `+hostedEventColumns+`,
                          SUM(
                              LENGTH(CAST(event_id AS BLOB)) +
                              LENGTH(CAST(kind AS BLOB)) +
                              LENGTH(CAST(actor_json AS BLOB)) +
                              LENGTH(CAST(payload_json AS BLOB))
                          ) OVER (ORDER BY seq ASC) AS cumulative_bytes
                     FROM hosted_room_events
                    WHERE room_id=? AND seq>?
                    ORDER BY seq ASC LIMIT ?
               )
               SELECT `+hostedEventColumns+`
                 FROM candidates
                WHERE cumulative_bytes<=?
                ORDER BY seq ASC`, roomID, sinceSeq, limit, maxLogPageBytes)
		return err
	})
	if err != nil {
		return nil, err
	}
	events := make([]any, len(rows))
	for i, row := range rows {
		if events[i], err = hostedEventFromRow(row, false); err != nil {
			return nil, err
		}
	}
	buildPage := func(page []any) map[string]any {
		cursor := sinceSeq
		if len(page) > 0 {
			cursor = page[len(page)-1].(map[string]any)["seq"].(int64)
		}
		return map[string]any{"events": page, "cursor": cursor, "latest_seq": latestSeq, "has_more": cursor < latestSeq, "authority": authority}
	}
	fits := func(page []any) (bool, error) {
		encoded, err := pyjson.Dumps(buildPage(page), pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
		return int64(len(encoded)) <= maxLogPageBytes, err
	}
	if len(events) > 0 {
		whole, err := fits(events)
		if err != nil {
			return nil, err
		}
		if !whole {
			low, high := 1, len(events)
			for low < high {
				middle := (low + high + 1) / 2
				ok, err := fits(events[:middle])
				if err != nil {
					return nil, err
				}
				if ok {
					low = middle
				} else {
					high = middle - 1
				}
			}
			events = events[:low]
			ok, err := fits(events)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, hostedRoomError("hosted room event exceeds replay page limit")
			}
		}
	}
	return buildPage(events), nil
}
