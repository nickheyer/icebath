package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	// Seconds a disbanded replica stays readable before the next sync prunes it
	replicaDisbandedKeepSeconds = 24 * 60 * 60
	// Seconds a refused queued operation stays listed for the user
	replicaFailedKeepSeconds = 60 * 60
)

// Creates the tables a peer keeps beside its replicas, the home link, queued operations, uploads and fetched files
func replicaViewInitializeSchema(ctx context.Context, conn state.SQLiteConn) error {
	if err := replicaInitializeSchema(ctx, conn); err != nil {
		return err
	}
	for _, ddl := range []string{`CREATE TABLE IF NOT EXISTS hosted_room_replica_links (
            room_id TEXT PRIMARY KEY, home_gateway_id TEXT NOT NULL, home_label TEXT NOT NULL,
            status_json TEXT, disbanded_at REAL, synced_at REAL NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS hosted_room_replica_outbox (
            outbox_id INTEGER PRIMARY KEY AUTOINCREMENT, room_id TEXT NOT NULL, op TEXT NOT NULL,
            params_json TEXT NOT NULL, created_at REAL NOT NULL, acked INTEGER NOT NULL DEFAULT 0, error TEXT)`,
		`CREATE TABLE IF NOT EXISTS hosted_room_replica_uploads (
            room_id TEXT NOT NULL, attachment_id TEXT NOT NULL, upload_id TEXT NOT NULL, kind TEXT NOT NULL,
            name TEXT NOT NULL, mime TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, data BLOB NOT NULL,
            created_at REAL NOT NULL, PRIMARY KEY (room_id, attachment_id))`,
		`CREATE TABLE IF NOT EXISTS hosted_room_replica_files (
            room_id TEXT NOT NULL, event_id TEXT NOT NULL, attachment_id TEXT NOT NULL, kind TEXT NOT NULL,
            name TEXT NOT NULL, mime TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, data BLOB NOT NULL,
            created_at REAL NOT NULL, PRIMARY KEY (room_id, event_id, attachment_id))`} {
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	return nil
}

// Runs body in one transaction over the room store with the replica tables in place
func replicaViewTransaction(ctx context.Context, dbPath string, immediate bool, body func(conn state.SQLiteConn) error) error {
	return hostedTransaction(ctx, dbPath, immediate, func(conn state.SQLiteConn) error {
		if err := replicaViewInitializeSchema(ctx, conn); err != nil {
			return err
		}
		return body(conn)
	})
}

// Home link of a replicated room, nil when the room is no replica here
func ReplicaLink(ctx context.Context, dbPath, roomID string) (map[string]any, error) {
	var row *pyjson.Object
	err := replicaViewTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) (err error) {
		row, err = state.QueryRowObject(ctx, conn, `SELECT l.home_gateway_id, l.home_label, l.status_json, l.disbanded_at, l.synced_at
            FROM hosted_room_replica_links l JOIN hosted_room_replicas r ON r.room_id=l.room_id WHERE l.room_id=?`, roomID)
		return err
	})
	if err != nil || row == nil {
		return nil, err
	}
	link := map[string]any{"home_gateway_id": hrStr(row, "home_gateway_id"), "home": hrStr(row, "home_label"),
		"synced_at": hrFloat(row, "synced_at"), "disbanded_at": nil, "status": nil}
	if row.Vals["disbanded_at"] != nil {
		link["disbanded_at"] = hrFloat(row, "disbanded_at")
	}
	if row.Vals["status_json"] != nil {
		if link["status"], err = roomLoads(hrStr(row, "status_json")); err != nil {
			return nil, err
		}
	}
	return link, nil
}

// Room view of a stored replica, marked as a replica of its home
func replicaRoomView(row *pyjson.Object) (map[string]any, error) {
	members, err := roomLoads(hrStr(row, "members_json"))
	if err != nil {
		return nil, err
	}
	room := map[string]any{"room_id": hrStr(row, "room_id"), "name": hrStr(row, "name"), "members": members,
		"authority_gateway_id": hrStr(row, "authority_gateway_id"), "authority_epoch": hrInt(row, "authority_epoch"),
		"revision": hrInt(row, "last_seq"), "created_at": hrFloat(row, "created_at"), "updated_at": hrFloat(row, "updated_at"),
		"idempotent": false, "latest_seq": hrInt(row, "last_seq"), "replica": true, "home": hrStr(row, "home_label")}
	if row.Vals["disbanded_at"] != nil {
		room["disbanded_at"] = hrFloat(row, "disbanded_at")
	}
	return room, nil
}

// Columns of a replica joined with its home link
const replicaViewSelect = `SELECT r.room_id, r.name, r.members_json, r.authority_gateway_id, r.authority_epoch, r.last_seq,
       r.created_at, r.updated_at, l.home_label, l.disbanded_at
  FROM hosted_room_replicas r JOIN hosted_room_replica_links l ON l.room_id=r.room_id`

// Replicated rooms most recently changed first, disbanded ones only when asked
func ReplicaRooms(ctx context.Context, dbPath string, includeDisbanded bool) ([]any, error) {
	var rows []*pyjson.Object
	err := replicaViewTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) (err error) {
		rows, err = state.QueryObjects(ctx, conn, replicaViewSelect+" WHERE l.disbanded_at IS NULL OR ? ORDER BY r.updated_at DESC, r.room_id",
			hostedBoolInt(includeDisbanded))
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		room, err := replicaRoomView(row)
		if err != nil {
			return nil, err
		}
		out = append(out, room)
	}
	return out, nil
}

// One replicated room, a not found error when absent or disbanded and not asked for
func ReplicaRoom(ctx context.Context, dbPath, roomID string, includeDisbanded bool) (map[string]any, error) {
	var row *pyjson.Object
	err := replicaViewTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) (err error) {
		row, err = state.QueryRowObject(ctx, conn, replicaViewSelect+" WHERE r.room_id=? AND (l.disbanded_at IS NULL OR ?)", roomID, hostedBoolInt(includeDisbanded))
		return err
	})
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, roomRefusal("invalid_params", "room not found")
	}
	return replicaRoomView(row)
}

// Monotonic replica log delta after sinceSeq, the page shape the home serves
func ReplicaReadEvents(ctx context.Context, dbPath, roomID string, sinceValue, limitValue any) (map[string]any, error) {
	since, err := hostedNonNegative(sinceValue, "since_seq")
	if err != nil {
		return nil, err
	}
	limit, err := hostedBoundedLimit(limitValue, MaxRoomLogLimit)
	if err != nil {
		return nil, err
	}
	var replica *pyjson.Object
	var rows []*pyjson.Object
	err = replicaViewTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) (err error) {
		if replica, err = state.QueryRowObject(ctx, conn, replicaSelect, roomID); err != nil || replica == nil {
			return err
		}
		rows, err = state.QueryObjects(ctx, conn, "SELECT "+hostedEventColumns+` FROM hosted_room_replica_events
            WHERE room_id=? AND seq>? ORDER BY seq ASC LIMIT ?`, roomID, since, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	if replica == nil {
		return nil, roomRefusal("invalid_params", "room not found")
	}
	events := make([]any, len(rows))
	for i, row := range rows {
		if events[i], err = hostedEventFromRow(row, false); err != nil {
			return nil, err
		}
	}
	cursor := since
	if len(events) > 0 {
		cursor = events[len(events)-1].(map[string]any)["seq"].(int64)
	}
	stored := hrInt(replica, "last_seq")
	return map[string]any{"events": events, "cursor": cursor, "latest_seq": stored, "has_more": cursor < stored,
		"authority": map[string]any{"gateway_id": hrStr(replica, "authority_gateway_id"), "epoch": hrInt(replica, "authority_epoch")}}, nil
}

// Queues one operation of a local user for the room's home, which applies it on its next sync
func QueueReplicaOp(ctx context.Context, dbPath, roomID, op string, params map[string]any) (int64, error) {
	encoded, err := pyjson.Dumps(params, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
	if err != nil {
		return 0, err
	}
	var id int64
	err = replicaViewTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		result, err := conn.ExecContext(ctx, "INSERT INTO hosted_room_replica_outbox (room_id, op, params_json, created_at) VALUES (?, ?, ?, ?)",
			roomID, op, encoded, hostedClock(nil))
		if err == nil {
			id, err = result.LastInsertId()
		}
		return err
	})
	return id, err
}

// Coverage, home, sync time, queued operations and recent refusals of one replica
func ReplicaStateView(ctx context.Context, dbPath, roomID string) (map[string]any, error) {
	out, err := ReplicaState(ctx, dbPath, roomID)
	if err != nil {
		return nil, err
	}
	link, err := ReplicaLink(ctx, dbPath, roomID)
	if err != nil {
		return nil, err
	}
	var pending int64
	var failed []*pyjson.Object
	err = replicaViewTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) error {
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM hosted_room_replica_outbox WHERE room_id=? AND acked=0", roomID).Scan(&pending); err != nil {
			return err
		}
		var err error
		failed, err = state.QueryObjects(ctx, conn, `SELECT outbox_id, op, error FROM hosted_room_replica_outbox
            WHERE room_id=? AND acked=1 ORDER BY outbox_id`, roomID)
		return err
	})
	if err != nil {
		return nil, err
	}
	errorsOut := make([]any, 0, len(failed))
	for _, row := range failed {
		errorsOut = append(errorsOut, map[string]any{"outbox_id": hrInt(row, "outbox_id"), "op": hrStr(row, "op"), "error": hrStr(row, "error")})
	}
	out["pending"], out["errors"] = pending, errorsOut
	if link != nil {
		out["home"], out["synced_at"], out["disbanded_at"] = link["home"], link["synced_at"], link["disbanded_at"]
	}
	return out, nil
}

// Attachment id a replica gives a local upload, stable per upload id
func replicaUploadID(uploadID string) string {
	sum := sha256.Sum256([]byte(uploadID))
	return "rup-" + hex.EncodeToString(sum[:16])
}

// Keeps one composer file of a local user until the home takes it with the message that names it
func ReplicaUpload(ctx context.Context, dbPath, roomID string, params map[string]any) (map[string]any, error) {
	uploadID, err := roomIdentifier(params["upload_id"], roomIdentifierSpec{label: "upload_id", maxChars: maxEventIDChars}, hostedRoomError)
	if err != nil {
		return nil, err
	}
	encoded, ok := params["data_base64"].(string)
	if !ok || len(encoded) > ((maxAttachmentBytes+2)/3)*4 {
		return nil, roomRefusal("invalid_params", "attachment is larger than the per-file limit")
	}
	data, err := pybase64.B64DecodeValidate(encoded)
	if err != nil || len(data) == 0 {
		return nil, roomRefusal("invalid_params", "attachment data is not valid base64")
	}
	kind, _ := params["kind"].(string)
	if !attachmentKinds[kind] {
		return nil, roomRefusal("invalid_params", "attachment kind must be image, pdf, or file")
	}
	name, _ := params["name"].(string)
	mime, _ := params["mime"].(string)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	attachmentID, now := replicaUploadID(uploadID), hostedClock(nil)
	idempotent := false
	err = replicaViewTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		existing, err := state.QueryRowObject(ctx, conn, "SELECT created_at FROM hosted_room_replica_uploads WHERE room_id=? AND attachment_id=?", roomID, attachmentID)
		if err != nil {
			return err
		}
		if existing != nil {
			idempotent, now = true, hrFloat(existing, "created_at")
			return nil
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO hosted_room_replica_uploads (room_id, attachment_id, upload_id, kind, name, mime, size, sha256, data, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, roomID, attachmentID, uploadID, kind, name, mime, len(data), digest, data, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"attachment_id": attachmentID, "kind": kind, "name": name, "size": int64(len(data)), "mime": mime, "sha256": digest,
		"state": "pending", "created_at": now, "idempotent": idempotent, "event_id": nil}, nil
}

// A fetched file of a replicated event, else a fetch queued for the home and a refusal saying it is on its way
func ReplicaDownload(ctx context.Context, dbPath, roomID string, params map[string]any) (map[string]any, error) {
	eventID, _ := params["event_id"].(string)
	attachmentID, _ := params["attachment_id"].(string)
	if eventID == "" || attachmentID == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	saved, err := replicaFetchedFile(ctx, dbPath, roomID, eventID, attachmentID)
	if err != nil || saved != nil {
		return saved, err
	}
	var queued *pyjson.Object
	err = replicaViewTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) (err error) {
		queued, err = state.QueryRowObject(ctx, conn, `SELECT 1 AS hit FROM hosted_room_replica_outbox WHERE room_id=? AND op='fetch' AND acked=0
            AND json_extract(params_json, '$.event_id')=? AND json_extract(params_json, '$.attachment_id')=?`, roomID, eventID, attachmentID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if queued == nil {
		if _, err := QueueReplicaOp(ctx, dbPath, roomID, "fetch", map[string]any{"event_id": eventID, "attachment_id": attachmentID}); err != nil {
			return nil, err
		}
	}
	return nil, roomRefusal("attachment_fetching", "the file is on its way from the room's home")
}

// A complete fetched file with its bytes, nil when it has not fully arrived
func replicaFetchedFile(ctx context.Context, dbPath, roomID, eventID, attachmentID string) (map[string]any, error) {
	var row *pyjson.Object
	err := replicaViewTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) (err error) {
		row, err = state.QueryRowObject(ctx, conn, `SELECT attachment_id, kind, name, mime, size, sha256, data, created_at
            FROM hosted_room_replica_files WHERE room_id=? AND event_id=? AND attachment_id=?`, roomID, eventID, attachmentID)
		return err
	})
	if err != nil || row == nil {
		return nil, err
	}
	data, _ := row.Vals["data"].([]byte)
	if int64(len(data)) != hrInt(row, "size") {
		return nil, nil
	}
	return map[string]any{"attachment_id": attachmentID, "kind": hrStr(row, "kind"), "name": hrStr(row, "name"), "size": hrInt(row, "size"),
		"mime": hrStr(row, "mime"), "sha256": hrStr(row, "sha256"), "state": "committed", "created_at": hrFloat(row, "created_at"),
		"idempotent": false, "event_id": eventID, "data_base64": base64.StdEncoding.EncodeToString(data)}, nil
}

// Appends one pushed chunk of a fetched file in order, a repeated chunk ignored and a corrupt file dropped
func replicaStoreChunk(ctx context.Context, conn state.SQLiteConn, chunk map[string]any, now float64) error {
	roomID, eventID, attachmentID := pyrepr.Str(chunk["room_id"]), pyrepr.Str(chunk["event_id"]), pyrepr.Str(chunk["attachment_id"])
	offset, _ := hostedStrictInt(chunk["offset"])
	size, _ := hostedStrictInt(chunk["size"])
	data, err := base64.StdEncoding.DecodeString(pyrepr.Str(chunk["data_base64"]))
	if err != nil || size <= 0 || size > maxAttachmentBytes {
		return nil
	}
	existing, err := state.QueryRowObject(ctx, conn, "SELECT data FROM hosted_room_replica_files WHERE room_id=? AND event_id=? AND attachment_id=?", roomID, eventID, attachmentID)
	if err != nil {
		return err
	}
	var have []byte
	if existing != nil {
		have, _ = existing.Vals["data"].([]byte)
	}
	if offset != int64(len(have)) {
		return nil
	}
	whole := append(append([]byte{}, have...), data...)
	if int64(len(whole)) > size {
		_, err := conn.ExecContext(ctx, "DELETE FROM hosted_room_replica_files WHERE room_id=? AND event_id=? AND attachment_id=?", roomID, eventID, attachmentID)
		return err
	}
	if int64(len(whole)) == size {
		sum := sha256.Sum256(whole)
		if hex.EncodeToString(sum[:]) != pyrepr.Str(chunk["sha256"]) {
			_, err := conn.ExecContext(ctx, "DELETE FROM hosted_room_replica_files WHERE room_id=? AND event_id=? AND attachment_id=?", roomID, eventID, attachmentID)
			return err
		}
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO hosted_room_replica_files (room_id, event_id, attachment_id, kind, name, mime, size, sha256, data, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(room_id, event_id, attachment_id) DO UPDATE SET data=excluded.data`,
		roomID, eventID, attachmentID, pyrepr.Str(chunk["kind"]), pyrepr.Str(chunk["name"]), pyrepr.Str(chunk["mime"]), size, pyrepr.Str(chunk["sha256"]), whole, now)
	return err
}

// Drops a replica with everything kept beside it
func replicaDrop(ctx context.Context, conn state.SQLiteConn, roomID string) error {
	for _, table := range []string{"hosted_room_replica_events", "hosted_room_replicas", "hosted_room_replica_links", "hosted_room_replica_outbox",
		"hosted_room_replica_uploads", "hosted_room_replica_files"} {
		if _, err := conn.ExecContext(ctx, "DELETE FROM "+table+" WHERE room_id=?", roomID); err != nil {
			return err
		}
	}
	return nil
}

// Error a home sent the sync route that the client should see as a bad request
type RoomShareSyncError struct{ Msg string }

func (e *RoomShareSyncError) Error() string { return e.Msg }

// Takes one sync of a room home, storing its pages and status, answering cursors and the operations local users queued
func RoomShareSync(ctx context.Context, runner any, body *pyjson.Object) (map[string]any, error) {
	authority, err := ActiveAuthority(ctx, runner)
	if err != nil {
		return nil, err
	}
	if authority == nil {
		return nil, &RoomShareSyncError{"this gateway serves no session authority for the profile"}
	}
	dbPath := authority.DB.Path()
	fields, _ := pyjson.Plain(body).(map[string]any)
	homeID, err := roomIdentifier(fields["home_gateway_id"], roomIdentifierSpec{label: "home_gateway_id", maxChars: maxActorIDChars}, hostedRoomError)
	if err != nil {
		return nil, &RoomShareSyncError{err.Error()}
	}
	label, _ := fields["home_label"].(string)
	if label == "" || len([]rune(label)) > maxActorLabelChars {
		label = homeID
	}
	local, err := LocalAuthorityGatewayID(ctx)
	if err != nil {
		return nil, err
	}
	if homeID == local {
		return nil, &RoomShareSyncError{"a gateway cannot share rooms with itself"}
	}
	now := hostedClock(nil)
	removedOut := []any{}
	err = replicaViewTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		acks, _ := fields["acks"].([]any)
		for _, raw := range acks {
			ack, _ := raw.(map[string]any)
			text, failed := ack["error"].(string)
			var err error
			if failed {
				_, err = conn.ExecContext(ctx, `UPDATE hosted_room_replica_outbox SET acked=1, error=? WHERE outbox_id=?
                    AND room_id IN (SELECT room_id FROM hosted_room_replica_links WHERE home_gateway_id=?)`, text, ack["outbox_id"], homeID)
			} else {
				_, err = conn.ExecContext(ctx, `DELETE FROM hosted_room_replica_outbox WHERE outbox_id=?
                    AND room_id IN (SELECT room_id FROM hosted_room_replica_links WHERE home_gateway_id=?)`, ack["outbox_id"], homeID)
			}
			if err != nil {
				return err
			}
		}
		removed, _ := fields["removed"].([]any)
		for _, raw := range removed {
			roomID, _ := raw.(string)
			link, err := state.QueryRowObject(ctx, conn, "SELECT home_gateway_id FROM hosted_room_replica_links WHERE room_id=?", roomID)
			if err != nil {
				return err
			}
			if link != nil && hrStr(link, "home_gateway_id") != homeID {
				continue
			}
			if err := replicaDrop(ctx, conn, roomID); err != nil {
				return err
			}
			removedOut = append(removedOut, roomID)
		}
		files, _ := fields["files"].([]any)
		for _, raw := range files {
			chunk, _ := raw.(map[string]any)
			link, err := state.QueryRowObject(ctx, conn, "SELECT 1 AS hit FROM hosted_room_replica_links WHERE room_id=? AND home_gateway_id=?", pyrepr.Str(chunk["room_id"]), homeID)
			if err != nil {
				return err
			}
			if link != nil {
				if err := replicaStoreChunk(ctx, conn, chunk, now); err != nil {
					return err
				}
			}
		}
		stale, err := state.QueryObjects(ctx, conn, "SELECT room_id FROM hosted_room_replica_links WHERE disbanded_at IS NOT NULL AND disbanded_at < ?", now-replicaDisbandedKeepSeconds)
		if err != nil {
			return err
		}
		for _, row := range stale {
			if err := replicaDrop(ctx, conn, hrStr(row, "room_id")); err != nil {
				return err
			}
		}
		_, err = conn.ExecContext(ctx, "DELETE FROM hosted_room_replica_outbox WHERE acked=1 AND created_at < ?", now-replicaFailedKeepSeconds)
		return err
	})
	if err != nil {
		return nil, err
	}
	results := []any{}
	rooms, _ := fields["rooms"].([]any)
	for _, raw := range rooms {
		room, _ := raw.(map[string]any)
		results = append(results, replicaSyncRoom(ctx, dbPath, local, homeID, label, room, now))
	}
	outbox, err := replicaOutboxFor(ctx, dbPath, homeID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"gateway_id": local, "rooms": results, "removed": removedOut, "outbox": outbox}, nil
}

// Stores one pushed room, answering its stored cursor, this gateway's newer authority or the refusal
func replicaSyncRoom(ctx context.Context, dbPath, local, homeID, label string, room map[string]any, now float64) map[string]any {
	roomID, _ := room["room_id"].(string)
	result := map[string]any{"room_id": roomID}
	if own, err := RoomState(ctx, dbPath, roomID, true); err == nil {
		if gatewayID, epoch, err := hostedRoomAuthority(own); err == nil && gatewayID == local {
			result["authority"] = map[string]any{"gateway_id": gatewayID, "epoch": epoch}
			return result
		}
		result["error"] = "room id is already used by a room of this gateway"
		return result
	}
	var link *pyjson.Object
	if err := replicaViewTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) (err error) {
		link, err = state.QueryRowObject(ctx, conn, "SELECT home_gateway_id FROM hosted_room_replica_links WHERE room_id=?", roomID)
		return err
	}); err != nil {
		result["error"] = err.Error()
		return result
	}
	if link != nil && hrStr(link, "home_gateway_id") != homeID {
		result["error"] = "room id is already replicated from another gateway"
		return result
	}
	ingested, err := IngestReplicaPage(ctx, dbPath, roomID, room["name"], room["members"], room["page"], nil)
	if err != nil {
		result["error"] = err.Error()
		if state, stateErr := ReplicaState(ctx, dbPath, roomID); stateErr == nil {
			result["stored_seq"] = state["last_seq"]
		} else if errors.As(err, new(*ReplicaGapError)) {
			result["stored_seq"] = int64(0)
		}
		return result
	}
	var statusJSON any
	if status, has := room["status"]; has && status != nil {
		if encoded, err := pyjson.Dumps(status, pyjson.EnsureASCII(false), pyjson.Separators(",", ":")); err == nil {
			statusJSON = encoded
		}
	}
	err = replicaViewTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_replica_links (room_id, home_gateway_id, home_label, status_json, disbanded_at, synced_at)
            VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(room_id) DO UPDATE SET home_label=excluded.home_label, status_json=excluded.status_json,
            disbanded_at=excluded.disbanded_at, synced_at=excluded.synced_at`, roomID, homeID, label, statusJSON, room["disbanded_at"], now)
		return err
	})
	if err != nil {
		result["error"] = err.Error()
		return result
	}
	result["stored_seq"] = ingested["stored_seq"]
	return result
}

// Operations local users queued on rooms of one home, oldest first, sends carrying their files
func replicaOutboxFor(ctx context.Context, dbPath, homeID string) ([]any, error) {
	var rows []*pyjson.Object
	uploads := map[string]*pyjson.Object{}
	err := replicaViewTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) error {
		var err error
		rows, err = state.QueryObjects(ctx, conn, `SELECT o.outbox_id, o.room_id, o.op, o.params_json FROM hosted_room_replica_outbox o
            JOIN hosted_room_replica_links l ON l.room_id=o.room_id WHERE l.home_gateway_id=? AND o.acked=0 ORDER BY o.outbox_id`, homeID)
		if err != nil {
			return err
		}
		stored, err := state.QueryObjects(ctx, conn, `SELECT u.room_id, u.attachment_id, u.upload_id, u.kind, u.name, u.mime, u.data
            FROM hosted_room_replica_uploads u JOIN hosted_room_replica_links l ON l.room_id=u.room_id WHERE l.home_gateway_id=?`, homeID)
		for _, row := range stored {
			uploads[hrStr(row, "room_id")+"\x00"+hrStr(row, "attachment_id")] = row
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		params, err := roomLoads(hrStr(row, "params_json"))
		if err != nil {
			return nil, err
		}
		fields, _ := params.(map[string]any)
		roomID := hrStr(row, "room_id")
		if hrStr(row, "op") == "send" {
			payload, _ := fields["payload"].(map[string]any)
			items, _ := payload["attachments"].([]any)
			files := []any{}
			for _, raw := range items {
				item, _ := raw.(map[string]any)
				upload := uploads[roomID+"\x00"+pyrepr.Str(item["attachment_id"])]
				if upload == nil {
					continue
				}
				data, _ := upload.Vals["data"].([]byte)
				files = append(files, map[string]any{"attachment_id": hrStr(upload, "attachment_id"), "upload_id": hrStr(upload, "upload_id"),
					"kind": hrStr(upload, "kind"), "name": hrStr(upload, "name"), "mime": hrStr(upload, "mime"),
					"data_base64": base64.StdEncoding.EncodeToString(data)})
			}
			fields["uploads"] = files
		}
		out = append(out, map[string]any{"outbox_id": hrInt(row, "outbox_id"), "room_id": roomID, "op": hrStr(row, "op"), "params": fields})
	}
	return out, nil
}

// Continues a replicated room here, claims it for the actor and applies what local users had queued for the old home
func (c *CanonicalHostedRoomService) PromoteReplicaRoom(actor Principal, roomID string, reason any) (map[string]any, error) {
	link, err := ReplicaLink(c.ctx, c.DBPath, roomID)
	if err != nil {
		return nil, err
	}
	if link == nil {
		return nil, roomRefusal("invalid_params", "room is no replica of another gateway")
	}
	queued, err := replicaOutboxFor(c.ctx, c.DBPath, pyrepr.Str(link["home_gateway_id"]))
	if err != nil {
		return nil, err
	}
	promoted, err := PromoteReplica(c.ctx, c.DBPath, roomID, reason, nil)
	if err != nil {
		return nil, err
	}
	if err := c.Authority.DB.ExecuteWrite(c.ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(c.ctx, "INSERT OR REPLACE INTO state_meta(key,value) VALUES(?,?)", hostedRoomOwnerKey+roomID, actor.Subject)
		return err
	}); err != nil {
		return nil, err
	}
	local := map[string]any{"kind": "user", "id": "local-user"}
	for _, raw := range queued {
		op, _ := raw.(map[string]any)
		kind, _ := op["op"].(string)
		if op["room_id"] != roomID || kind == "fetch" {
			continue
		}
		params, _ := op["params"].(map[string]any)
		if err := c.applyRoomOp(roomID, kind, params, local); err != nil {
			pkgLog().Warn(fmt.Sprintf("hosted room %s: a %s queued before the promotion was refused", roomID, kind), "exc_info", err)
		}
	}
	if err := replicaViewTransaction(c.ctx, c.DBPath, true, func(conn state.SQLiteConn) error {
		for _, table := range []string{"hosted_room_replica_links", "hosted_room_replica_outbox", "hosted_room_replica_uploads"} {
			if _, err := conn.ExecContext(c.ctx, "DELETE FROM "+table+" WHERE room_id=?", roomID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	c.Wakeup()
	return promoted, nil
}
