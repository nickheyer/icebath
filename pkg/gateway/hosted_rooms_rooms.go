package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	HostedRoomsProtocolVersion    = 2
	maxRoomIDChars                = 128
	maxEventIDChars               = 128
	maxRoomNameChars              = 200
	maxEventKindChars             = 64
	maxActorIDChars               = 128
	maxActorLabelChars            = 200
	maxRoomMembers                = 128
	maxMembersJSONBytes           = 128 * 1024
	maxEventJSONBytes             = 256 * 1024
	MaxRoomLogLimit               = 500
	MaxRoomListLimit              = 500
	disbandedRoomRetentionSeconds = 90 * 24 * 60 * 60
	maxEventsPerRoom              = 50_000
	controlEventCountReserve      = 64
	controlEventByteReserve       = 1024 * 1024
	hostedEventColumns            = "room_id, seq, event_id, kind, actor_json, authority_epoch, payload_json, created_at"
	hostedSelectEvent             = "SELECT " + hostedEventColumns + " FROM hosted_room_events WHERE room_id=? AND event_id=?"
	hostedInsertEvent             = "INSERT INTO hosted_room_events (" + hostedEventColumns + ") VALUES (?, ?, ?, ?, ?, ?, ?, ?)"
	hostedRoomColumns             = "room_id, name, members_json, authority_gateway_id, authority_epoch, next_seq, revision, created_at, updated_at, disbanded_at"
	hostedRoomColumnsWithBytes    = "room_id, name, members_json, authority_gateway_id, authority_epoch, next_seq, event_bytes, revision, created_at, updated_at, disbanded_at"
	hostedSelectRoom              = "SELECT " + hostedRoomColumns + " FROM hosted_rooms WHERE room_id=?"
	hostedSelectRoomWithBytes     = "SELECT " + hostedRoomColumnsWithBytes + " FROM hosted_rooms WHERE room_id=?"
	hostedSumEventBytes           = "SELECT COALESCE(SUM(event_bytes), 0) FROM hosted_rooms"
	hostedInsertRetired           = "INSERT OR IGNORE INTO hosted_room_retired_ids (room_id, retired_at) VALUES (?, ?)"
	hostedSelectLiveReservation   = "SELECT 1 FROM hosted_room_peer_reservations " + hostedLiveReservationWhere + " LIMIT 1"
)

// Room store bounds, package variables like the module globals they port
var (
	maxLogPageBytes            int64 = 2 * 1024 * 1024
	maxActiveRooms             int64 = 256
	maxDisbandedRoomTombstones int64 = 512
	maxRoomEventBytes          int64 = 256 * 1024 * 1024
	MaxGatewayEventBytes       int64 = 16 * 1024 * 1024
)

var (
	hostedEventKindRE        = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)
	hostedControlEventKinds  = map[string]bool{"authority.claimed": true, "authority.lost": true, "room.disbanded": true, "room.stop_requested": true}
	hostedEventKindsByActor  = map[string]map[string]bool{"user": {"message.user": true}, "member": {"message.member": true}, "gateway": {"member.unavailable": true, "room.activity": true, "room.stop_requested": true, "turn.deferred": true, "turn.reassigned": true, "turn.cancelled": true, "turn.failed": true, "turn.settled": true, "turn.started": true}, "system": {"authority.claimed": true, "authority.lost": true, "room.created": true, "room.disbanded": true, "room.members_changed": true, "room.renamed": true}}
	hostedOptionalActorField = []struct {
		name     string
		maxChars int
	}{{"display_name", maxActorLabelChars}, {"profile", maxActorIDChars}, {"connection_id", maxActorIDChars}}
	hostedActorFields    = map[string]bool{"kind": true, "id": true, "display_name": true, "profile": true, "connection_id": true}
	hostedLinkColumns    = []string{"room_id", "member_id", "target_url", "target_profile", "grant", "catalog_json", "cancellation_scope_id", "trace_id", "transport_security", "status", "updated_at"}
	hostedRemoteRunWhere = func() string {
		return strings.Join(mapStrings(hostedRemoteRunIdentityColumns, func(c string) string { return c + "=?" }), " AND ")
	}()
	hostedSelectRemoteRun     = "SELECT * FROM hosted_room_remote_runs WHERE " + hostedRemoteRunWhere
	hostedRoomDependentTables = []string{"hosted_room_policy_transcript_state", "hosted_room_policy_transcript", "hosted_room_policy_publications", "hosted_room_policy_watermarks", "hosted_room_policy_events", "hosted_room_policy_threads", "hosted_room_policy_cursors", "hosted_room_driver_tasks", "hosted_room_driver_leases", "hosted_room_remote_runs", "hosted_room_links", "hosted_room_peer_reservations", "hosted_room_events"}
)

// Each string mapped through fn
func mapStrings(values []string, fn func(string) string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = fn(value)
	}
	return out
}

// A room that does not exist or has been disbanded
type RoomNotFoundError struct{ *HostedRoomError }

func (e *RoomNotFoundError) Unwrap() error { return e.HostedRoomError }

// Python class name of the error
func (e *RoomNotFoundError) PyTypeName() string { return "RoomNotFoundError" }

// A retired room id whose history compaction removed its log
type RoomHistoryExpiredError struct{ *RoomNotFoundError }

func (e *RoomHistoryExpiredError) Unwrap() error { return e.RoomNotFoundError }

// Python class name of the error
func (e *RoomHistoryExpiredError) PyTypeName() string { return "RoomHistoryExpiredError" }

// Stable wire reason of the error
func (e *RoomHistoryExpiredError) Reason() string { return "room_history_expired" }

// An idempotency key reused for different room state
type RoomConflictError struct{ *HostedRoomError }

func (e *RoomConflictError) Unwrap() error { return e.HostedRoomError }

// Python class name of the error
func (e *RoomConflictError) PyTypeName() string { return "RoomConflictError" }

// A non blocking ownership probe that cannot read the room store
type RoomProbeUnavailableError struct{ *HostedRoomError }

func (e *RoomProbeUnavailableError) Unwrap() error { return e.HostedRoomError }

// Python class name of the error
func (e *RoomProbeUnavailableError) PyTypeName() string { return "RoomProbeUnavailableError" }

// An event id reused with different immutable content
type EventConflictError struct{ *HostedRoomError }

func (e *EventConflictError) Unwrap() error { return e.HostedRoomError }

// Python class name of the error
func (e *EventConflictError) PyTypeName() string { return "EventConflictError" }

// New room events that invalidate an uncommitted publication plan
type EventCursorConflictError struct{ *HostedRoomError }

func (e *EventCursorConflictError) Unwrap() error { return e.HostedRoomError }

// Python class name of the error
func (e *EventCursorConflictError) PyTypeName() string { return "EventCursorConflictError" }

// A publication that lost its file commitments, retried with a fresh plan
type EventAttachmentConflictError struct{ *EventCursorConflictError }

func (e *EventAttachmentConflictError) Unwrap() error { return e.EventCursorConflictError }

// Python class name of the error
func (e *EventAttachmentConflictError) PyTypeName() string { return "EventAttachmentConflictError" }

// A successful authority claim that a later claim superseded
type AuthoritySupersededError struct{ *AuthorityConflictError }

func (e *AuthoritySupersededError) Unwrap() error { return e.AuthorityConflictError }

// Python class name of the error
func (e *AuthoritySupersededError) PyTypeName() string { return "AuthoritySupersededError" }

// Stable wire reason of a stale authority
func (e *AuthorityConflictError) Reason() string { return "authority_conflict" }

func roomNotFound(msg string) error  { return &RoomNotFoundError{&HostedRoomError{msg}} }
func roomConflict(msg string) error  { return &RoomConflictError{&HostedRoomError{msg}} }
func eventConflict(msg string) error { return &EventConflictError{&HostedRoomError{msg}} }
func eventCursorConflict(msg string) error {
	return &EventCursorConflictError{&HostedRoomError{msg}}
}
func eventAttachmentConflict(msg string) error {
	return &EventAttachmentConflictError{&EventCursorConflictError{&HostedRoomError{msg}}}
}
func authorityConflict(msg string) *AuthorityConflictError {
	return &AuthorityConflictError{&HostedRoomError{msg}}
}

// Validated room id
func hostedRoomID(value any) (string, error) {
	return roomIdentifier(value, roomIdentifierSpec{label: "room_id", maxChars: maxRoomIDChars}, hostedRoomError)
}

// Validated event id
func hostedEventID(value any) (string, error) {
	return roomIdentifier(value, roomIdentifierSpec{label: "event_id", maxChars: maxEventIDChars}, hostedRoomError)
}

// Bounded room display name of any shape
func hostedRoomName(value any) (string, error) {
	return roomIdentifier(value, roomIdentifierSpec{label: "name", maxChars: maxRoomNameChars, anyShape: true, invalid: "invalid room name"}, hostedRoomError)
}

// Lowercase dotted event kind
func hostedEventKind(value any) (string, error) {
	return roomIdentifier(value, roomIdentifierSpec{label: "kind", maxChars: maxEventKindChars, pattern: hostedEventKindRE, invalid: "invalid event kind"}, hostedRoomError)
}

// Canonical actor JSON bounded to 4 KiB
func hostedActorJSON(value any) (string, error) {
	return roomCanonicalJSON(value, "actor", 4*1024, false, hostedRoomError)
}

// Canonical event payload JSON bounded to the event limit
func hostedPayloadJSON(value any) (string, error) {
	return roomCanonicalJSON(value, "payload", maxEventJSONBytes, false, hostedRoomError)
}

// Positive integer of a hosted room field
func hostedRequirePositiveInt(value any, label string) (int64, error) {
	n, err := roomBoundedInt(value, hostedRoomError, label+" must be a positive integer", 1, nil)
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// Page limit between one and maximum
func hostedBoundedLimit(value any, maximum int64) (int64, error) {
	n, err := roomBoundedInt(value, hostedRoomError, fmt.Sprintf("limit must be between 1 and %d", maximum), 1, big.NewInt(maximum))
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// Non negative integer of a hosted room field
func hostedNonNegative(value any, label string) (int64, error) {
	n, err := roomBoundedInt(value, hostedRoomError, label+" must be a non-negative integer", 0, nil)
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// Canonical actor JSON of a system actor
func hostedSystemActorJSON(actorID string) string {
	encoded, _ := hostedActorJSON(map[string]any{"kind": "system", "id": actorID})
	return encoded
}

// Canonical payload of an authority transfer
func hostedClaimPayloadJSON(previousGatewayID, newGatewayID string, epoch int64) string {
	encoded, _ := hostedPayloadJSON(map[string]any{"previous_gateway_id": previousGatewayID, "authority_gateway_id": newGatewayID, "authority_epoch": epoch})
	return encoded
}

// Server owned user event id a client retry key maps to
func UserEventID(clientEventID any) (string, error) {
	eventID, err := hostedEventID(clientEventID)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(eventID))
	return "user:" + hex.EncodeToString(sum[:]), nil
}

// Copied member objects and their canonical JSON
func hostedValidateMembers(value any) ([]map[string]any, string, error) {
	list, ok := pyjson.Plain(value).([]any)
	if !ok {
		return nil, "", hostedRoomError("members must be a list")
	}
	if len(list) > maxRoomMembers {
		return nil, "", hostedRoomError("too many room members")
	}
	members := make([]map[string]any, len(list))
	for i, member := range list {
		fields, ok := member.(map[string]any)
		if !ok {
			return nil, "", hostedRoomError("each room member must be an object")
		}
		members[i] = fields
	}
	encoded, err := roomCanonicalJSON(members, "members", maxMembersJSONBytes, false, hostedRoomError)
	if err != nil {
		return nil, "", err
	}
	return members, encoded, nil
}

// Whether adoption only adds routing targets an older room could not store
func hostedLegacyMembersMatch(existingJSON string, proposed []map[string]any) bool {
	decoded, err := roomLoads(existingJSON)
	if err != nil {
		return false
	}
	existing, ok := decoded.([]any)
	if !ok || len(existing) != len(proposed) {
		return false
	}
	for i, item := range existing {
		previous, ok := item.(map[string]any)
		if !ok {
			return false
		}
		previous = pyjson.Plain(previous).(map[string]any)
		current := pyjson.Plain(proposed[i]).(map[string]any)
		previousTarget, currentTarget := previous["target"], current["target"]
		delete(previous, "target")
		delete(current, "target")
		emptyTarget := previousTarget == nil || pyjson.Equal(previousTarget, map[string]any{})
		if !pyjson.Equal(previous, current) || (!emptyTarget && !pyjson.Equal(previousTarget, currentTarget)) {
			return false
		}
	}
	return true
}

// Normalized actor allowed to append kind, and its canonical JSON
func hostedValidateActor(value any, kind string) (map[string]any, string, error) {
	fields, ok := hostedMapping(value)
	if !ok {
		return nil, "", hostedRoomError("actor must be an object")
	}
	var unknown []string
	for key := range fields {
		if !hostedActorFields[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, "", hostedRoomError("unknown actor fields: " + strings.Join(unknown, ", "))
	}
	actorKind, ok := fields["kind"].(string)
	if !ok || hostedEventKindsByActor[actorKind] == nil {
		return nil, "", hostedRoomError("invalid actor.kind")
	}
	if !hostedEventKindsByActor[actorKind][kind] {
		return nil, "", hostedRoomError(fmt.Sprintf("actor kind '%s' cannot append '%s'", actorKind, kind))
	}
	actorID, err := hostedActorID(fields["id"], "actor.id")
	if err != nil {
		return nil, "", err
	}
	actor := map[string]any{"kind": actorKind, "id": actorID}
	for _, field := range hostedOptionalActorField {
		fieldValue := fields[field.name]
		if fieldValue == nil {
			continue
		}
		text, ok := fieldValue.(string)
		if !ok {
			return nil, "", hostedRoomError("actor." + field.name + " must be a string")
		}
		text = pystr.Strip(text)
		if utf8.RuneCountInString(text) > field.maxChars {
			return nil, "", hostedRoomError("actor." + field.name + " is too long")
		}
		if text != "" {
			actor[field.name] = text
		}
	}
	encoded, err := hostedActorJSON(actor)
	if err != nil {
		return nil, "", err
	}
	return actor, encoded, nil
}

// Room store connection with no steady state journal or migration writes
func hostedReadConnection(ctx context.Context, dbPath string) (state.SQLiteConn, error) {
	if !hostedIsFile(dbPath) {
		conn, err := hostedRoomsConnect(ctx, dbPath)
		if err != nil {
			return nil, err
		}
		conn.Close()
	}
	conn, err := hostedOpenSQLite(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	ready, err := hostedStoreReady(ctx, conn, dbPath)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !ready {
		conn.Close()
		initialized, err := hostedRoomsConnect(ctx, dbPath)
		if err != nil {
			return nil, err
		}
		initialized.Close()
		return hostedOpenSQLite(ctx, dbPath)
	}
	return conn, nil
}

// Whether the room id is permanently retired
func hostedIsRetired(ctx context.Context, conn state.SQLiteConn, roomID string) (bool, error) {
	row, err := state.QueryRowObject(ctx, conn, "SELECT 1 FROM hosted_room_retired_ids WHERE room_id=?", roomID)
	return row != nil, err
}

// One hosted_rooms row, or the precise not found or history expired error
func hostedRoomRow(ctx context.Context, conn state.SQLiteConn, query string, args []any, roomID string) (*pyjson.Object, error) {
	row, err := state.QueryRowObject(ctx, conn, query, args...)
	if err != nil || row != nil {
		return row, err
	}
	retained, err := state.QueryRowObject(ctx, conn, "SELECT 1 FROM hosted_rooms WHERE room_id=?", roomID)
	if err != nil {
		return nil, err
	}
	if retained == nil {
		retired, err := hostedIsRetired(ctx, conn, roomID)
		if err != nil {
			return nil, err
		}
		if retired {
			return nil, &RoomHistoryExpiredError{&RoomNotFoundError{&HostedRoomError{"Group Chat history expired; room_id remains permanently retired"}}}
		}
	}
	return nil, roomNotFound("hosted room not found")
}

// Refuses a room row whose authority is not gatewayID at epoch
func hostedRequireAuthority(room *pyjson.Object, gatewayID string, epoch int64, message string) error {
	if fmt.Sprint(room.Vals["authority_gateway_id"]) != gatewayID || hrInt(room, "authority_epoch") != epoch {
		return authorityConflict(message)
	}
	return nil
}

// Row this transaction just wrote, a miss being an invariant violation
func hostedReload(ctx context.Context, conn state.SQLiteConn, query string, args []any, missing string) (*pyjson.Object, error) {
	row, err := state.QueryRowObject(ctx, conn, query, args...)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errors.New(missing)
	}
	return row, nil
}

// Public room view of a stored row
func hostedRoomFromRow(row *pyjson.Object, idempotent bool) (map[string]any, error) {
	members, err := roomLoads(hrStr(row, "members_json"))
	if err != nil {
		return nil, err
	}
	room := map[string]any{"room_id": row.Vals["room_id"], "name": row.Vals["name"], "members": members,
		"authority_gateway_id": row.Vals["authority_gateway_id"], "authority_epoch": hrInt(row, "authority_epoch"),
		"revision": hrInt(row, "revision"), "created_at": hrFloat(row, "created_at"), "updated_at": hrFloat(row, "updated_at"),
		"idempotent": idempotent}
	if value, has := row.Vals["disbanded_at"]; has && value != nil {
		room["disbanded_at"] = hrFloat(row, "disbanded_at")
	}
	if _, has := row.Vals["next_seq"]; has {
		room["latest_seq"] = hrInt(row, "next_seq") - 1
	}
	return room, nil
}

// Public event view of a stored row
func hostedEventFromRow(row *pyjson.Object, idempotent bool) (map[string]any, error) {
	actor, err := roomLoads(hrStr(row, "actor_json"))
	if err != nil {
		return nil, err
	}
	payload, err := roomLoads(hrStr(row, "payload_json"))
	if err != nil {
		return nil, err
	}
	var epoch any
	if row.Vals["authority_epoch"] != nil {
		epoch = hrInt(row, "authority_epoch")
	}
	return map[string]any{"room_id": row.Vals["room_id"], "seq": hrInt(row, "seq"), "event_id": row.Vals["event_id"],
		"kind": row.Vals["kind"], "actor": actor, "authority_epoch": epoch, "payload": payload,
		"created_at": hrFloat(row, "created_at"), "idempotent": idempotent}, nil
}

// Stored event of one id, nil when absent
func hostedLoadEvent(ctx context.Context, conn state.SQLiteConn, roomID, eventID string) (*pyjson.Object, error) {
	return state.QueryRowObject(ctx, conn, hostedSelectEvent, roomID, eventID)
}

// Whether a stored event carries exactly this immutable kind, actor, epoch and payload
func hostedEventContentEquals(row *pyjson.Object, kind, actorJSON string, epoch int64, payloadJSON string) bool {
	storedEpoch, isInt := row.Vals["authority_epoch"].(int64)
	return hrStr(row, "kind") == kind && hrStr(row, "actor_json") == actorJSON && isInt && storedEpoch == epoch &&
		hrStr(row, "payload_json") == payloadJSON
}

// Event bytes every room of the gateway accounts
func hostedGatewayEventBytes(ctx context.Context, conn state.SQLiteConn) (int64, error) {
	var total int64
	err := conn.QueryRowContext(ctx, hostedSumEventBytes).Scan(&total)
	return total, err
}

// Capacity checked insert of one event at seq, returning its accounted bytes
func hostedInsertEventRow(ctx context.Context, conn state.SQLiteConn, room *pyjson.Object, roomID string, seq int64, eventID, kind, actorJSON string,
	epoch int64, payloadJSON string, now float64, allowControl bool) (int64, error) {
	eventBytes, err := hostedPrepareEvent(ctx, conn, room, eventID, kind, actorJSON, payloadJSON, allowControl)
	if err != nil {
		return 0, err
	}
	_, err = conn.ExecContext(ctx, hostedInsertEvent, roomID, seq, eventID, kind, actorJSON, epoch, payloadJSON, now)
	return eventBytes, err
}

// Sizes one pending event and enforces room and gateway capacity, returning its bytes
func hostedPrepareEvent(ctx context.Context, conn state.SQLiteConn, room *pyjson.Object, eventID, kind, actorJSON, payloadJSON string, allowControl bool) (int64, error) {
	additional := int64(roomUTF8Len(eventID, kind, actorJSON, payloadJSON))
	var countReserve, byteReserve int64
	if allowControl {
		countReserve, byteReserve = controlEventCountReserve, controlEventByteReserve
	}
	gatewayLimit := MaxGatewayEventBytes + byteReserve
	if hrInt(room, "next_seq")-1 >= maxEventsPerRoom+countReserve {
		return 0, hostedRoomError("This Group Chat reached its history limit. Start a new Group Chat to continue.")
	}
	if hrInt(room, "event_bytes")+additional > maxRoomEventBytes+byteReserve {
		return 0, hostedRoomError("This Group Chat reached its storage limit. Start a new Group Chat to continue.")
	}
	gatewayBytes, err := hostedGatewayEventBytes(ctx, conn)
	if err != nil {
		return 0, err
	}
	if gatewayBytes+additional > gatewayLimit {
		budget := max(0, gatewayLimit-additional)
		if _, err := hostedPruneDisbandedRoomsLocked(ctx, conn, nil, &budget); err != nil {
			return 0, err
		}
		if gatewayBytes, err = hostedGatewayEventBytes(ctx, conn); err != nil {
			return 0, err
		}
	}
	if gatewayBytes+additional > gatewayLimit {
		return 0, hostedRoomError("Group Chat storage is full on this host. Delete an old Group Chat and try again.")
	}
	return additional, nil
}

// Room ids one query returns
func hostedRoomIDs(ctx context.Context, conn state.SQLiteConn, query string, args ...any) ([]string, error) {
	rows, err := state.QueryObjects(ctx, conn, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = fmt.Sprint(row.Vals["room_id"])
	}
	return out, nil
}

// Purges expired, excess or byte budget evicted disbanded rooms, retiring their ids
func hostedPruneDisbandedRoomsLocked(ctx context.Context, conn state.SQLiteConn, now *float64, maxGatewayBytes *int64) (int, error) {
	candidates := map[string]bool{}
	if now != nil {
		ids, err := hostedRoomIDs(ctx, conn, `SELECT room_id FROM hosted_rooms
                     WHERE disbanded_at IS NOT NULL AND disbanded_at<=?`, *now-disbandedRoomRetentionSeconds)
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			candidates[id] = true
		}
	}
	ids, err := hostedRoomIDs(ctx, conn, `SELECT room_id FROM hosted_rooms WHERE disbanded_at IS NOT NULL
                ORDER BY disbanded_at DESC, room_id ASC LIMIT -1 OFFSET ?`, maxDisbandedRoomTombstones)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		candidates[id] = true
	}
	if maxGatewayBytes != nil {
		retained, err := hostedGatewayEventBytes(ctx, conn)
		if err != nil {
			return 0, err
		}
		if retained > *maxGatewayBytes {
			rows, err := state.QueryObjects(ctx, conn, `SELECT room_id, event_bytes FROM hosted_rooms WHERE disbanded_at IS NOT NULL
                    ORDER BY disbanded_at ASC, room_id ASC`)
			if err != nil {
				return 0, err
			}
			for _, row := range rows {
				candidates[fmt.Sprint(row.Vals["room_id"])] = true
				retained -= hrInt(row, "event_bytes")
				if retained <= *maxGatewayBytes {
					break
				}
			}
		}
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	roomIDs := make([]string, 0, len(candidates))
	for id := range candidates {
		roomIDs = append(roomIDs, id)
	}
	sort.Strings(roomIDs)
	binds := strings.TrimSuffix(strings.Repeat("?,", len(roomIDs)), ",")
	args := make([]any, len(roomIDs))
	for i, id := range roomIDs {
		args[i] = id
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(hostedRetireFromRooms, "room_id IN ("+binds+") AND disbanded_at IS NOT NULL"), args...); err != nil {
		return 0, err
	}
	for _, table := range hostedRoomDependentTables {
		exists, err := hostedTableExists(ctx, conn, table)
		if err != nil {
			return 0, err
		}
		if exists {
			if _, err := conn.ExecContext(ctx, "DELETE FROM "+table+" WHERE room_id IN ("+binds+")", args...); err != nil {
				return 0, err
			}
		}
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM hosted_rooms WHERE room_id IN ("+binds+")", args...); err != nil {
		return 0, err
	}
	return len(roomIDs), nil
}

// Purges deleted Group Chat payloads while reserving their identities
func PruneDisbandedRooms(ctx context.Context, dbPath string, now *float64) (int, error) {
	var pruned int
	err := hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) (err error) {
		timestamp := hostedClock(now)
		pruned, err = hostedPruneDisbandedRoomsLocked(ctx, conn, &timestamp, nil)
		return err
	})
	return pruned, err
}

// Private RoomLink records, grants unformatted
func ListRoomLinkRecords(ctx context.Context, dbPath string) ([]map[string]any, error) {
	var out []map[string]any
	err := hostedTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) error {
		rows, err := state.QueryObjects(ctx, conn, `SELECT room_id, member_id, target_url, target_profile, grant,
                      catalog_json, cancellation_scope_id, trace_id,
                      transport_security, status, updated_at
                 FROM hosted_room_links
             ORDER BY room_id, member_id`)
		out = make([]map[string]any, len(rows))
		for i, row := range rows {
			out[i] = hrDict(row)
		}
		return err
	})
	return out, err
}

// Atomically inserts or replaces one private RoomLink record
func UpsertRoomLinkRecord(ctx context.Context, dbPath string, record map[string]any, maxLinks int) error {
	return hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		roomID, okRoom := record["room_id"]
		memberID, okMember := record["member_id"]
		if !okRoom {
			return &HostedKeyError{"room_id"}
		}
		if !okMember {
			return &HostedKeyError{"member_id"}
		}
		existing, err := state.QueryRowObject(ctx, conn, "SELECT 1 FROM hosted_room_links WHERE room_id=? AND member_id=?", roomID, memberID)
		if err != nil {
			return err
		}
		if existing == nil {
			count, err := state.QueryRowObject(ctx, conn, "SELECT COUNT(*) AS n FROM hosted_room_links")
			if err != nil {
				return err
			}
			if hrInt(count, "n") >= int64(maxLinks) {
				return hostedRoomError("too many stored room links")
			}
		}
		values, err := recordValues(record, hostedLinkColumns)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO hosted_room_links(
                   room_id, member_id, target_url, target_profile, grant,
                   catalog_json, cancellation_scope_id, trace_id,
                   transport_security, status, updated_at
               ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
               ON CONFLICT(room_id, member_id) DO UPDATE SET
                   target_url=excluded.target_url,
                   target_profile=excluded.target_profile,
                   grant=excluded.grant,
                   catalog_json=excluded.catalog_json,
                   cancellation_scope_id=excluded.cancellation_scope_id,
                   trace_id=excluded.trace_id,
                   transport_security=excluded.transport_security,
                   status=excluded.status,
                   updated_at=excluded.updated_at`, values...)
		return err
	})
}

// Persists a non secret route health classification
func UpdateRoomLinkStatus(ctx context.Context, dbPath, roomID, memberID, status string, now *float64) (bool, error) {
	var updated bool
	err := hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		changed, err := roomExecCount(ctx, conn, "UPDATE hosted_room_links SET status=?, updated_at=? WHERE room_id=? AND member_id=?",
			status, hostedClock(now), roomID, memberID)
		updated = changed == 1
		return err
	})
	return updated, err
}

// Deletes persisted peer routes after their target grants are revoked
func DeleteRoomLinkRecords(ctx context.Context, dbPath, roomID string) (int64, error) {
	var deleted int64
	err := hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) (err error) {
		deleted, err = roomExecCount(ctx, conn, "DELETE FROM hosted_room_links WHERE room_id=?", roomID)
		return err
	})
	return deleted, err
}

// Whether a live target side RoomLink reservation fences Desktop
func PeerRoomIsReserved(ctx context.Context, dbPath string, roomID, targetProfile any, now *float64) (bool, error) {
	room, err := hostedRoomID(roomID)
	if err != nil {
		return false, err
	}
	profile, err := hostedActorID(targetProfile, "target_profile")
	if err != nil {
		return false, err
	}
	row, err := hostedReadOne(ctx, dbPath, hostedSelectLiveReservation, room, profile, hostedClock(now))
	return row != nil, err
}

// Identity column values of one remote run record in key order
func hostedRemoteRunIdentity(record map[string]any) ([]any, error) {
	return recordValues(record, hostedRemoteRunIdentityColumns)
}

// Durably binds one logical peer task attempt to its remote run handle
func UpsertRemoteRunReceipt(ctx context.Context, dbPath string, record map[string]any, now *float64) error {
	timestamp := hostedClock(now)
	identity, err := hostedRemoteRunIdentity(record)
	if err != nil {
		return err
	}
	tail, err := recordValues(record, []string{"run_id", "session_id"})
	if err != nil {
		return err
	}
	immutable := append(append([]any{}, identity...), tail...)
	return hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		existing, err := state.QueryRowObject(ctx, conn, hostedSelectRemoteRun, identity...)
		if err != nil {
			return err
		}
		if existing != nil {
			stored, _ := recordValues(hrDict(existing), append(append([]string{}, hostedRemoteRunIdentityColumns...), "run_id", "session_id"))
			if !pyjson.Equal(stored, immutable) {
				return hostedRoomError("remote run receipt conflicts with its logical task")
			}
			_, err := conn.ExecContext(ctx, "UPDATE hosted_room_remote_runs SET updated_at=? WHERE "+hostedRemoteRunWhere, append([]any{timestamp}, identity...)...)
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_remote_runs(
                   room_id, home_install_id, authority_gateway_id,
                   authority_epoch, member_id, target_install_id,
                   target_profile, task_id, execution_generation, run_id,
                   session_id, created_at, updated_at
               ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append(immutable, timestamp, timestamp)...); err != nil {
			return err
		}
		return captureTransitionLocked(ctx, conn, fmt.Sprint(record["room_id"]))
	})
}

// Remote run handles in durable task order, filtered by any given column
func ListRemoteRunReceipts(ctx context.Context, dbPath string, roomID, targetProfile, sessionID *string) ([]map[string]any, error) {
	var where []string
	var args []any
	for _, filter := range []struct {
		column string
		value  *string
	}{{"room_id", roomID}, {"target_profile", targetProfile}, {"session_id", sessionID}} {
		if filter.value != nil {
			where = append(where, filter.column+"=?")
			args = append(args, *filter.value)
		}
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	var out []map[string]any
	err := hostedTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) error {
		rows, err := state.QueryObjects(ctx, conn, "SELECT * FROM hosted_room_remote_runs"+clause+" ORDER BY created_at, task_id, execution_generation", args...)
		out = make([]map[string]any, len(rows))
		for i, row := range rows {
			out[i] = hrDict(row)
		}
		return err
	})
	return out, err
}

// Exact durable remote run handle of one task attempt, nil when none
func RemoteRunReceipt(ctx context.Context, dbPath string, record map[string]any) (map[string]any, error) {
	identity, err := hostedRemoteRunIdentity(record)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = hostedTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) error {
		row, err := state.QueryRowObject(ctx, conn, hostedSelectRemoteRun, identity...)
		if row != nil {
			out = hrDict(row)
		}
		return err
	})
	return out, err
}
