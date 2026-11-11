package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	MaxReplicaRooms        = 256
	maxReplicaEventBytes   = 256 * 1024 * 1024
	replicaEventColumns    = "(room_id, seq, event_id, kind, actor_json, authority_epoch, payload_json, created_at)"
	replicaInsertRoomEvent = "INSERT INTO hosted_room_events " + replicaEventColumns + " VALUES (?, ?, ?, ?, ?, ?, ?, ?)"
	replicaInsertEvent     = "INSERT INTO hosted_room_replica_events " + replicaEventColumns + " VALUES (?, ?, ?, ?, ?, ?, ?, ?)"
	replicaSelect          = "SELECT * FROM hosted_room_replicas WHERE room_id=?"
)

// Invalid or conflicting replica operation
type ReplicaError struct{ *HostedRoomError }

func (e *ReplicaError) Unwrap() error      { return e.HostedRoomError }
func (e *ReplicaError) PyTypeName() string { return "ReplicaError" }

// Page that does not start at the replica's next expected sequence
type ReplicaGapError struct{ *ReplicaError }

func (e *ReplicaGapError) Unwrap() error      { return e.ReplicaError }
func (e *ReplicaGapError) PyTypeName() string { return "ReplicaGapError" }

// Page or demotion carrying an older authority epoch than stored
type ReplicaEpochRegressionError struct{ *ReplicaError }

func (e *ReplicaEpochRegressionError) Unwrap() error      { return e.ReplicaError }
func (e *ReplicaEpochRegressionError) PyTypeName() string { return "ReplicaEpochRegressionError" }

func replicaError(msg string) error { return &ReplicaError{&HostedRoomError{msg}} }
func replicaGap(msg string) error   { return &ReplicaGapError{&ReplicaError{&HostedRoomError{msg}}} }
func replicaRegression(msg string) error {
	return &ReplicaEpochRegressionError{&ReplicaError{&HostedRoomError{msg}}}
}

// Creates the replica row and event tables
func replicaInitializeSchema(ctx context.Context, conn state.SQLiteConn) error {
	for _, ddl := range []string{`CREATE TABLE IF NOT EXISTS hosted_room_replicas (
            room_id TEXT PRIMARY KEY, name TEXT NOT NULL, members_json TEXT NOT NULL,
            authority_gateway_id TEXT NOT NULL,
            authority_epoch INTEGER NOT NULL CHECK (authority_epoch >= 1),
            last_seq INTEGER NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
            latest_seq INTEGER NOT NULL DEFAULT 0, event_bytes INTEGER NOT NULL DEFAULT 0,
            created_at REAL NOT NULL, updated_at REAL NOT NULL
        )`, `CREATE TABLE IF NOT EXISTS hosted_room_replica_events (
            room_id TEXT NOT NULL, seq INTEGER NOT NULL CHECK (seq >= 1), event_id TEXT NOT NULL,
            kind TEXT NOT NULL, actor_json TEXT NOT NULL, authority_epoch INTEGER,
            payload_json TEXT NOT NULL, created_at REAL NOT NULL,
            PRIMARY KEY (room_id, seq)
        )`} {
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	return nil
}

// Ensures the replica schema on its own connection, then runs body in an IMMEDIATE transaction that re-runs it
func replicaTransaction(ctx context.Context, dbPath string, body func(conn state.SQLiteConn) error) error {
	conn, err := hostedRoomsConnect(ctx, dbPath)
	if err != nil {
		return err
	}
	if err := state.Transaction(ctx, conn, false, func(conn state.SQLiteConn) error { return replicaInitializeSchema(ctx, conn) }); err != nil {
		return err
	}
	return hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		if err := replicaInitializeSchema(ctx, conn); err != nil {
			return err
		}
		return body(conn)
	})
}

// Positive integer of a replica field
func replicaPositiveInt(value any, message string) (int64, error) {
	n, err := roomBoundedInt(value, replicaError, message, 1, nil)
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// Event id, kind, actor JSON and payload JSON of the system authority control event at epoch
func replicaControlEvent(kind string, epoch int64, payload map[string]any) ([4]string, error) {
	payloadJSON, err := hostedPayloadJSON(payload)
	if err != nil {
		return [4]string{}, err
	}
	return [4]string{fmt.Sprintf("system:authority-%s:%d", kind, epoch), "authority." + kind, hostedSystemActorJSON("authority-control"), payloadJSON}, nil
}

// Inserts one control event at seq under epoch
func replicaAppendControlEvent(ctx context.Context, conn state.SQLiteConn, roomID string, seq, epoch int64, event [4]string, now float64) error {
	_, err := conn.ExecContext(ctx, replicaInsertRoomEvent, roomID, seq, event[0], event[1], event[2], epoch, event[3], now)
	return err
}

// Accounted bytes of one replicated event
func replicaEventBytes(event map[string]any) (int64, error) {
	actor, err := pyjson.Dumps(event["actor"], pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
	if err != nil {
		return 0, err
	}
	payload, err := pyjson.Dumps(event["payload"], pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
	if err != nil {
		return 0, err
	}
	return int64(roomUTF8Len(pyrepr.Str(event["event_id"]), pyrepr.Str(event["kind"]), actor, payload)), nil
}

// Contiguous validated events of a read_events page and its authority
func replicaValidatePage(page any) ([]map[string]any, map[string]any, error) {
	fields, ok := pyjson.Plain(page).(map[string]any)
	if !ok {
		return nil, nil, replicaError("page must be an object")
	}
	events, eventsOK := fields["events"].([]any)
	authority, authorityOK := fields["authority"].(map[string]any)
	if !eventsOK {
		return nil, nil, replicaError("page.events must be a list")
	}
	if !authorityOK {
		return nil, nil, replicaError("page.authority is required for replication")
	}
	gatewayID, err := roomIdentifier(authority["gateway_id"], roomIdentifierSpec{label: "page.authority.gateway_id", maxChars: maxActorIDChars}, hostedRoomError)
	if err != nil {
		return nil, nil, err
	}
	epoch, err := replicaPositiveInt(authority["epoch"], "page.authority.epoch must be a positive integer")
	if err != nil {
		return nil, nil, err
	}
	out := make([]map[string]any, 0, len(events))
	var previous *int64
	for _, item := range events {
		event, ok := item.(map[string]any)
		if !ok {
			return nil, nil, replicaError("page events must be objects")
		}
		seq, err := replicaPositiveInt(event["seq"], "event.seq must be a positive integer")
		if err != nil {
			return nil, nil, err
		}
		if previous != nil && seq != *previous+1 {
			return nil, nil, replicaGap("page events must be contiguous")
		}
		previous = &seq
		for _, field := range []string{"event_id", "kind"} {
			if text, ok := event[field].(string); !ok || text == "" {
				return nil, nil, replicaError("event." + field + " must be a non-empty string")
			}
		}
		if _, ok := event["actor"].(map[string]any); !ok {
			return nil, nil, replicaError("event.actor must be an object")
		}
		if _, has := event["payload"]; !has {
			return nil, nil, replicaError("event.payload is required")
		}
		out = append(out, event)
	}
	return out, map[string]any{"gateway_id": gatewayID, "epoch": epoch}, nil
}

// Persists one verbatim read_events page idempotently, refusing sequence gaps and epoch regressions
func IngestReplicaPage(ctx context.Context, dbPath string, roomIDValue, roomNameValue, members, page any, now *float64) (map[string]any, error) {
	roomID, err := hostedRoomID(roomIDValue)
	if err != nil {
		return nil, err
	}
	roomName, err := hostedRoomName(roomNameValue)
	if err != nil {
		return nil, err
	}
	_, membersJSON, err := hostedValidateMembers(members)
	if err != nil {
		return nil, err
	}
	events, authority, err := replicaValidatePage(page)
	if err != nil {
		return nil, err
	}
	timestamp := hostedClock(now)
	var newLast, latestSeq int64
	var ingested int
	err = replicaTransaction(ctx, dbPath, func(conn state.SQLiteConn) error {
		row, err := state.QueryRowObject(ctx, conn, `SELECT authority_gateway_id, authority_epoch, last_seq, latest_seq, event_bytes
             FROM hosted_room_replicas WHERE room_id=?`, roomID)
		if err != nil {
			return err
		}
		var storedEpoch, lastSeq, storedBytes int64
		if row == nil {
			var count int64
			if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM hosted_room_replicas").Scan(&count); err != nil {
				return err
			}
			if count >= MaxReplicaRooms {
				return replicaError("replica room capacity exhausted")
			}
		} else {
			storedEpoch, lastSeq, storedBytes = hrInt(row, "authority_epoch"), hrInt(row, "last_seq"), hrInt(row, "event_bytes")
		}
		if authority["epoch"].(int64) < storedEpoch {
			return replicaRegression("page authority epoch is older than the stored replica epoch")
		}
		var fresh []map[string]any
		for _, event := range events {
			if seq, _ := hostedStrictInt(event["seq"]); seq > lastSeq {
				fresh = append(fresh, event)
			}
		}
		if len(fresh) > 0 {
			if first, _ := hostedStrictInt(fresh[0]["seq"]); first != lastSeq+1 {
				return replicaGap("page skips sequences the replica has not stored")
			}
		}
		var added int64
		for _, event := range fresh {
			size, err := replicaEventBytes(event)
			if err != nil {
				return err
			}
			if storedBytes+added+size > maxReplicaEventBytes {
				return replicaError("replica event storage exhausted")
			}
			actorJSON, err := hostedActorJSON(event["actor"])
			if err != nil {
				return err
			}
			payloadJSON, err := hostedPayloadJSON(event["payload"])
			if err != nil {
				return err
			}
			createdAt := timestamp
			if pyrepr.Truthy(event["created_at"]) {
				if createdAt, err = pyint.Float(event["created_at"]); err != nil {
					return err
				}
			}
			seq, _ := hostedStrictInt(event["seq"])
			if _, err := conn.ExecContext(ctx, replicaInsertEvent, roomID, seq, event["event_id"], event["kind"], actorJSON, event["authority_epoch"], payloadJSON, createdAt); err != nil {
				return err
			}
			added += size
		}
		newLast = lastSeq
		if len(fresh) > 0 {
			newLast, _ = hostedStrictInt(fresh[len(fresh)-1]["seq"])
		}
		latestSeq = newLast
		if value, isInt := hostedStrictInt(pyjson.Plain(page).(map[string]any)["latest_seq"]); isInt {
			latestSeq = value
		}
		values := []any{roomName, membersJSON, authority["gateway_id"], authority["epoch"], newLast, max(latestSeq, newLast)}
		if row == nil {
			_, err = conn.ExecContext(ctx, `INSERT INTO hosted_room_replicas (room_id, name, members_json,
                authority_gateway_id, authority_epoch, last_seq, latest_seq, event_bytes,
                created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append(append([]any{roomID}, values...), added, timestamp, timestamp)...)
		} else {
			_, err = conn.ExecContext(ctx, `UPDATE hosted_room_replicas SET name=?, members_json=?, authority_gateway_id=?,
                authority_epoch=?, last_seq=?, latest_seq=?, event_bytes=event_bytes+?,
                updated_at=? WHERE room_id=?`, append(values, added, timestamp, roomID)...)
		}
		ingested = len(fresh)
		return err
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"room_id": roomID, "stored_seq": newLast, "ingested": ingested, "authority": authority, "caught_up": newLast >= max(latestSeq, newLast)}, nil
}

// Stored replica coverage and authority lineage
func ReplicaState(ctx context.Context, dbPath string, roomIDValue any) (map[string]any, error) {
	roomID, err := hostedRoomID(roomIDValue)
	if err != nil {
		return nil, err
	}
	var row *pyjson.Object
	if err := replicaTransaction(ctx, dbPath, func(conn state.SQLiteConn) (err error) {
		row, err = state.QueryRowObject(ctx, conn, replicaSelect, roomID)
		return err
	}); err != nil {
		return nil, err
	}
	if row == nil {
		return nil, replicaError("replica not found")
	}
	members, err := roomLoads(hrStr(row, "members_json"))
	if err != nil {
		return nil, err
	}
	return map[string]any{"room_id": row.Vals["room_id"], "name": row.Vals["name"], "members": members,
		"authority": map[string]any{"gateway_id": row.Vals["authority_gateway_id"], "epoch": hrInt(row, "authority_epoch")},
		"last_seq":  hrInt(row, "last_seq"), "latest_seq": hrInt(row, "latest_seq"), "event_bytes": hrInt(row, "event_bytes"),
		"created_at": hrFloat(row, "created_at"), "updated_at": hrFloat(row, "updated_at")}, nil
}

// Continues a replicated room on this gateway at the next epoch behind a lineage proving claim
func PromoteReplica(ctx context.Context, dbPath string, roomIDValue, reason any, now *float64) (map[string]any, error) {
	roomID, err := hostedRoomID(roomIDValue)
	if err != nil {
		return nil, err
	}
	text, ok := reason.(string)
	if !ok || text == "" || len([]rune(text)) > 200 {
		return nil, replicaError("reason must be a non-empty string of at most 200 chars")
	}
	timestamp := hostedClock(now)
	localGateway, err := LocalAuthorityGatewayID(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = replicaTransaction(ctx, dbPath, func(conn state.SQLiteConn) error {
		replica, err := state.QueryRowObject(ctx, conn, replicaSelect, roomID)
		if err != nil {
			return err
		}
		if replica == nil {
			return replicaError("replica not found")
		}
		if hrStr(replica, "authority_gateway_id") == localGateway {
			return replicaError("this gateway already holds the room authority")
		}
		if existing, err := state.QueryRowObject(ctx, conn, "SELECT 1 FROM hosted_rooms WHERE room_id=?", roomID); err != nil || existing != nil {
			if err != nil {
				return err
			}
			return roomConflict("room_id already exists in the local authoritative store")
		}
		if retired, err := hostedIsRetired(ctx, conn, roomID); err != nil || retired {
			if err != nil {
				return err
			}
			return roomConflict("room_id belongs to a disbanded room")
		}
		previousGateway, previousEpoch := fmt.Sprint(replica.Vals["authority_gateway_id"]), hrInt(replica, "authority_epoch")
		targetEpoch, claimSeq := previousEpoch+1, hrInt(replica, "last_seq")+1
		claim, err := replicaControlEvent("claimed", targetEpoch, map[string]any{"previous_gateway_id": previousGateway, "authority_gateway_id": localGateway,
			"authority_epoch": targetEpoch, "promoted_from_replica": true, "reason": text})
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_rooms
               (room_id, name, members_json, authority_gateway_id, authority_epoch, next_seq, event_bytes,
                revision, created_at, updated_at, disbanded_at)
               VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, NULL)`, roomID, replica.Vals["name"], replica.Vals["members_json"], localGateway, targetEpoch,
			claimSeq+1, hrInt(replica, "event_bytes")+int64(roomUTF8Len(claim[:]...)), timestamp, timestamp); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO hosted_room_events "+replicaEventColumns+`
               SELECT room_id, seq, event_id, kind, actor_json, authority_epoch, payload_json, created_at
                 FROM hosted_room_replica_events WHERE room_id=?`, roomID); err != nil {
			return err
		}
		if err := replicaAppendControlEvent(ctx, conn, roomID, claimSeq, targetEpoch, claim, timestamp); err != nil {
			return err
		}
		for _, statement := range []string{"DELETE FROM hosted_room_replica_events WHERE room_id=?", "DELETE FROM hosted_room_replicas WHERE room_id=?"} {
			if _, err := conn.ExecContext(ctx, statement, roomID); err != nil {
				return err
			}
		}
		out = map[string]any{"room_id": roomID, "authority_gateway_id": localGateway, "authority_epoch": targetEpoch, "previous_gateway_id": previousGateway,
			"previous_epoch": previousEpoch, "claim_seq": claimSeq, "latest_seq": claimSeq}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Fences this gateway's stale room authority against a proven newer epoch, idempotent per lineage
func DemoteRoom(ctx context.Context, dbPath string, roomIDValue, observedGatewayValue, observedEpochValue any, now *float64) (map[string]any, error) {
	roomID, err := hostedRoomID(roomIDValue)
	if err != nil {
		return nil, err
	}
	observedGateway, err := roomIdentifier(observedGatewayValue, roomIdentifierSpec{label: "observed_gateway_id", maxChars: maxActorIDChars}, hostedRoomError)
	if err != nil {
		return nil, err
	}
	observedEpoch, err := replicaPositiveInt(observedEpochValue, "observed_epoch must be a positive integer")
	if err != nil {
		return nil, err
	}
	timestamp := hostedClock(now)
	localGateway, err := LocalAuthorityGatewayID(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		row, err := state.QueryRowObject(ctx, conn, `SELECT authority_gateway_id, authority_epoch, next_seq
                 FROM hosted_rooms WHERE room_id=? AND disbanded_at IS NULL`, roomID)
		if err != nil {
			return err
		}
		if row == nil {
			return replicaError("room not found in the local authoritative store")
		}
		currentGateway, currentEpoch := fmt.Sprint(row.Vals["authority_gateway_id"]), hrInt(row, "authority_epoch")
		if currentGateway == observedGateway && currentEpoch == observedEpoch {
			out = map[string]any{"room_id": roomID, "authority_gateway_id": currentGateway, "authority_epoch": currentEpoch, "idempotent": true}
			return nil
		}
		if observedEpoch <= currentEpoch {
			return replicaRegression("observed epoch does not supersede the stored authority")
		}
		if currentGateway != localGateway {
			return replicaError("room is not locally authoritative; nothing to demote")
		}
		lost, err := replicaControlEvent("lost", observedEpoch, map[string]any{"previous_gateway_id": currentGateway, "authority_gateway_id": observedGateway, "authority_epoch": observedEpoch})
		if err != nil {
			return err
		}
		if err := replicaAppendControlEvent(ctx, conn, roomID, hrInt(row, "next_seq"), observedEpoch, lost, timestamp); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE hosted_rooms
                  SET authority_gateway_id=?, authority_epoch=?, next_seq=next_seq+1, revision=revision+1, updated_at=?
                WHERE room_id=?`, observedGateway, observedEpoch, timestamp, roomID); err != nil {
			return err
		}
		out = map[string]any{"room_id": roomID, "authority_gateway_id": observedGateway, "authority_epoch": observedEpoch, "idempotent": false}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
