package state

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"golang.org/x/crypto/blake2b"
)

// agent/session_persistence.py marker for a message persisted after an admission interrupt
const PersistAfterAdmissionInterrupt = "_persist_after_admission_interrupt"

var nonPayloadColumns = map[string]bool{"session_id": true, "role": true, "timestamp": true, "active": true,
	"display_identity": true, MessageUID: true, "tool_call_uid": true}

// Durable payload columns a row addressed rewrite may change
var repairColumns = func() []string {
	var out []string
	for _, c := range messageWriteColumns {
		if !nonPayloadColumns[c] {
			out = append(out, c)
		}
	}
	return out
}()

var metadataColumns = map[string]bool{"display_kind": true, "display_metadata": true, "api_content": true,
	"codex_reasoning_items": true, "platform_message_id": true}

// Ownership version columns, the payload minus same process metadata writes
var ownedColumns = func() []string {
	var out []string
	for _, c := range repairColumns {
		if !metadataColumns[c] {
			out = append(out, c)
		}
	}
	return out
}()

// Columns a replay of the row sends, the owned payload plus api_content and codex reasoning
var replayColumns = append(append([]string{}, ownedColumns...), "api_content", "codex_reasoning_items")

// Replayed reasoning carriers a compaction claim compares
var ReplaySidecars = []string{"reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items", "codex_message_items"}

var liveMissingMetadata = []string{"display_kind", "display_metadata"}

var syncFields = append([]string{"role"}, repairColumns...)

const (
	canonicalMetadataOnly = "_metadata_only"
	canonicalContentOnly  = "_content_only"
)

// Owned columns a restore projection may leave off the dict, column paired with its live key
var restoreMayOmit = [][2]string{{"token_count", "token_count"}, {"_compressed_summary", "_compressed_summary"},
	{"tool_call_uids", ToolCallUIDs}, {"absorbed_message_uids", AbsorbedMessageUIDs}}

// Value of a Python int typed field, bool counting as int
func pyIntID(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Fixed size BLAKE2b digest of a stored row's owned columns, the compare and swap version
func TranscriptRowSnapshot(row map[string]any) string {
	digest, _ := blake2b.New(16, nil)
	for _, column := range ownedColumns {
		value := row[column]
		if value == nil {
			digest.Write([]byte("N"))
			continue
		}
		var tag string
		var data []byte
		switch v := value.(type) {
		case []byte:
			tag, data = "B", v
		case string:
			tag, data = "S", []byte(v)
		case float64:
			tag, data = "F", []byte(pyrepr.Float(v))
		case float32:
			tag, data = "F", []byte(pyrepr.Float(float64(v)))
		case int64:
			tag, data = "I", []byte(strconv.FormatInt(v, 10))
		case int:
			tag, data = "I", []byte(strconv.Itoa(v))
		case bool:
			tag, data = "I", []byte(pyrepr.Repr(v))
		case *big.Int:
			tag, data = "I", []byte(v.String())
		default:
			tag, data = "I", []byte(pyrepr.Repr(v))
		}
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(data)))
		digest.Write([]byte(tag))
		digest.Write(size[:])
		digest.Write(data)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// Rows of a SELECT on the caller connection as plain maps
func connRows(ctx context.Context, conn SQLiteConn, query string, args ...any) ([]map[string]any, error) {
	objs, err := queryObjects(ctx, conn, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(objs))
	for i, o := range objs {
		out[i] = objMap(o)
	}
	return out, nil
}

// Stamps the stored row digest on freshly inserted dicts with one SELECT per id chunk
func stampInsertedRowSnapshots(ctx context.Context, conn SQLiteConn, sessionID string, messages []map[string]any) error {
	byID := map[int64]map[string]any{}
	var ids []int64
	for _, msg := range messages {
		if id, ok := pyIntID(msg["_row_id"]); ok {
			if _, seen := byID[id]; !seen {
				ids = append(ids, id)
			}
			byID[id] = msg
		}
	}
	for _, chunk := range idChunks(ids, sqlInChunk) {
		args := []any{sessionID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := connRows(ctx, conn, fmt.Sprintf("SELECT * FROM messages WHERE session_id = ? AND id IN (%s)", placeholders(len(chunk))), args...)
		if err != nil {
			return err
		}
		for _, row := range rows {
			id, _ := pyIntID(row["id"])
			byID[id][DBRowSnapshot] = TranscriptRowSnapshot(row)
		}
	}
	return nil
}

// Whether decoded content is None, whitespace only, or carries no visible text parts
func IsContentBlank(content any) (bool, error) {
	switch c := content.(type) {
	case nil:
		return true, nil
	case string:
		return pyStrip(c) == "", nil
	case []any:
		var b strings.Builder
		index := 0
		for _, p := range c {
			part, ok := asPyObject(p)
			if !ok {
				continue
			}
			if kind, _ := part.get("type"); !pyrepr.Equal(kind, "text") {
				continue
			}
			value, has := part.get("text")
			text, isStr := value.(string)
			if has && !isStr {
				return false, fmt.Errorf("sequence item %d: expected str instance, %s found", index, pyrepr.TypeName(value))
			}
			b.WriteString(text)
			index++
		}
		return pyStrip(b.String()) == "", nil
	}
	return false, nil
}

// Newest active physical row for one durable logical message uid
func activeLogicalMessageRow(ctx context.Context, conn SQLiteConn, sessionID, role string, uid string, hasUID bool) (map[string]any, error) {
	if !hasUID {
		return nil, nil
	}
	return connRow(ctx, conn, "SELECT * FROM messages WHERE session_id = ? AND active = 1 AND role = ? AND message_uid = ? ORDER BY id DESC LIMIT 1",
		sessionID, role, uid)
}

// Active durable rows by uid among restored dicts without a snapshot or row address, the re insert guard
func activeUIDDedupe(ctx context.Context, conn SQLiteConn, sessionID string, messages []map[string]any) (map[string][]map[string]any, error) {
	seen := map[string]bool{}
	var candidates []string
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		if restored, ok := msg[RestoredFromStore].(bool); !ok || !restored {
			continue
		}
		if _, ok := pyIntID(msg["_row_id"]); ok {
			continue
		}
		if _, ok := msg[DBRowSnapshot].(string); ok {
			continue
		}
		if uid, ok := MessageUIDOrNone(msg); ok && uid != "" && !seen[uid] {
			seen[uid] = true
			candidates = append(candidates, uid)
		}
	}
	sort.Strings(candidates)
	durable := map[string][]map[string]any{}
	for _, chunk := range idChunks(candidates, sqlInChunk) {
		args := []any{sessionID}
		for _, uid := range chunk {
			args = append(args, uid)
		}
		rows, err := connRows(ctx, conn, fmt.Sprintf("SELECT * FROM messages WHERE session_id = ? AND active = 1 AND message_uid IN (%s)", placeholders(len(chunk))), args...)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			uid, _ := row["message_uid"].(string)
			durable[uid] = append(durable[uid], row)
		}
	}
	return durable, nil
}

// Content encoder, decoder, live serializer and repair row decoder a batch repair works through
type transcriptCodec struct {
	encode    func(any) any
	decode    func(any) any
	serialize func(map[string]any, float64) (map[string]any, error)
	decodeRow func(map[string]any) (map[string]any, error)
}

// Whether msg serializes to the replayed columns of one of its uid's active rows seen through the restore lens
func sameAsStored(msg map[string]any, rows []map[string]any, codec transcriptCodec) (bool, error) {
	for _, row := range rows {
		ts, err := pyFloatOf(row["timestamp"])
		if err != nil {
			return false, err
		}
		serialized, err := codec.serialize(msg, ts)
		if err != nil {
			return false, err
		}
		for _, pair := range restoreMayOmit {
			if !pyTruthy(msg[pair[1]]) {
				serialized[pair[0]] = row[pair[0]]
			}
		}
		role, _ := row["role"].(string)
		if pyrepr.Equal(serialized["content"], codec.encode(loadedViewContent(role, codec.decode(row["content"])))) {
			serialized["content"] = row["content"]
		}
		same := true
		for _, column := range replayColumns {
			if !pyrepr.Equal(row[column], serialized[column]) {
				same = false
				break
			}
		}
		if same {
			return true, nil
		}
	}
	return false, nil
}

// Resolves row addressed rewrites without duplicate appends or replacing concurrent winners, returning rows to insert
func resolveAndRepairTranscriptBatch(ctx context.Context, conn SQLiteConn, sessionID string, messages []map[string]any, codec transcriptCodec) ([]map[string]any, error) {
	inserted := []map[string]any{}
	durableUIDs, err := activeUIDDedupe(ctx, conn, sessionID, messages)
	if err != nil {
		return nil, err
	}
	for _, msg := range messages {
		role := "unknown"
		var expected any
		var existing any
		if msg != nil {
			existing = msg["_row_id"]
			if r, ok := msg["role"]; ok {
				role, _ = r.(string)
			}
			expected = msg[DBRowSnapshot]
		}
		expectedStr, hasExpected := expected.(string)
		var target map[string]any
		if id, ok := pyIntID(existing); ok {
			if target, err = activeMessageRow(ctx, conn, sessionID, id, role); err != nil {
				return nil, err
			}
		} else if hasExpected {
			uid, hasUID := MessageUIDOrNone(msg)
			if target, err = activeLogicalMessageRow(ctx, conn, sessionID, role, uid, hasUID); err != nil {
				return nil, err
			}
		}
		if target == nil {
			if msg != nil && !pyTruthy(msg[PersistAfterAdmissionInterrupt]) {
				uid, _ := MessageUIDOrNone(msg)
				if stored := durableUIDs[uid]; len(stored) > 0 {
					same, err := sameAsStored(msg, stored, codec)
					if err != nil {
						return nil, err
					}
					if same {
						msg[DBPersistedMarker] = true
						continue
					}
				}
			}
			inserted = append(inserted, msg)
			continue
		}
		targetID, _ := pyIntID(target["id"])
		msg["_row_id"] = targetID
		fillMissingToolCallUIDs(target, msg)
		var canonical map[string]any
		adopt, wrote := false, false
		if hasExpected {
			adopt = TranscriptRowSnapshot(target) != expectedStr
			if !adopt {
				ts, err := pyFloatOf(target["timestamp"])
				if err != nil {
					return nil, err
				}
				serialized, err := codec.serialize(msg, ts)
				if err != nil {
					return nil, err
				}
				if serialized["token_count"] == nil {
					serialized["token_count"] = target["token_count"]
				}
				for _, column := range ownedColumns {
					if !pyrepr.Equal(target[column], serialized[column]) {
						if err := rewriteTranscriptRow(ctx, conn, sessionID, target, serialized); err != nil {
							return nil, err
						}
						wrote = true
						break
					}
				}
				var missing []string
				anySet := false
				for _, c := range liveMissingMetadata {
					if msg[c] == nil {
						missing = append(missing, c)
						anySet = anySet || target[c] != nil
					}
				}
				if anySet {
					decoded, err := codec.decodeRow(target)
					if err != nil {
						return nil, err
					}
					canonical = map[string]any{}
					for _, c := range missing {
						if v, ok := decoded[c]; ok {
							canonical[c] = v
						}
					}
					canonical[canonicalMetadataOnly] = true
				}
			}
		} else if blank, err := IsContentBlank(codec.decode(target["content"])); err != nil && role == "assistant" {
			return nil, err
		} else if role == "assistant" && blank {
			res, err := conn.ExecContext(ctx, "UPDATE messages SET content = ? WHERE id = ? AND session_id = ? AND content IS ?",
				codec.encode(msg["content"]), targetID, sessionID, target["content"])
			if err != nil {
				return nil, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return nil, err
			}
			wrote = n > 0
		} else if role == "assistant" {
			canonical = map[string]any{"content": codec.decode(target["content"]), canonicalContentOnly: true}
		}
		final := target
		if wrote {
			if final, err = connRow(ctx, conn, "SELECT * FROM messages WHERE id = ? AND session_id = ?", targetID, sessionID); err != nil {
				return nil, err
			}
		}
		msg["timestamp"] = final["timestamp"]
		restoreRowIdentity(final, msg)
		msg[DBRowSnapshot] = TranscriptRowSnapshot(final)
		if adopt {
			if canonical, err = codec.decodeRow(final); err != nil {
				return nil, err
			}
		}
		if len(canonical) > 0 {
			msg[CanonicalRow] = canonical
		} else {
			delete(msg, CanonicalRow)
		}
	}
	return inserted, nil
}

// Rewrites one durable payload whose digest matched the live dict's last committed version
func rewriteTranscriptRow(ctx context.Context, conn SQLiteConn, sessionID string, target, serialized map[string]any) error {
	oldIdentity := target["display_identity"]
	var oldPeers []int64
	if oldIdentity != nil {
		rows, err := connRows(ctx, conn, "SELECT id FROM messages WHERE session_id = ? AND id != ? AND (active = 1 OR compacted = 1) AND display_identity IS ?",
			sessionID, target["id"], oldIdentity)
		if err != nil {
			return err
		}
		for _, r := range rows {
			id, _ := pyIntID(r["id"])
			oldPeers = append(oldPeers, id)
		}
	}
	assignments := make([]string, len(ownedColumns))
	args := make([]any, 0, len(ownedColumns)+2)
	for i, column := range ownedColumns {
		assignments[i] = column + " = ?"
		args = append(args, serialized[column])
	}
	targetID, _ := pyIntID(target["id"])
	args = append(args, targetID, sessionID)
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("UPDATE messages SET %s WHERE id = ? AND session_id = ?", strings.Join(assignments, ", ")), args...); err != nil {
		return err
	}
	return restoreDisplayIndex(ctx, conn, sessionID, target, serialized, oldIdentity, oldPeers)
}

// Restores display identities and orders the payload update trigger invalidated
func restoreDisplayIndex(ctx context.Context, conn SQLiteConn, sessionID string, target, serialized map[string]any, oldIdentity any, oldPeers []int64) error {
	if len(oldPeers) > 0 {
		oldOrder := oldPeers[0]
		args := []any{oldIdentity, nil, sessionID}
		for _, id := range oldPeers {
			oldOrder = min(oldOrder, id)
			args = append(args, id)
		}
		args[1] = oldOrder
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("UPDATE messages SET display_identity = ?, display_order = ? WHERE session_id = ? AND id IN (%s)",
			placeholders(len(oldPeers))), args...); err != nil {
			return err
		}
	}
	targetID, _ := pyIntID(target["id"])
	newIdentity := serialized["display_identity"]
	if !pyTruthy(target["active"]) && !pyTruthy(target["compacted"]) {
		_, err := conn.ExecContext(ctx, "UPDATE messages SET display_identity = ?, display_order = ? WHERE id = ? AND session_id = ?",
			newIdentity, targetID, targetID, sessionID)
		return err
	}
	peers, err := connRows(ctx, conn, "SELECT id, display_order FROM messages WHERE session_id = ? AND id != ? AND (active = 1 OR compacted = 1) AND display_identity IS ?",
		sessionID, targetID, newIdentity)
	if err != nil {
		return err
	}
	newOrder := targetID
	for _, peer := range peers {
		order, ok := pyIntID(peer["display_order"])
		if !ok {
			order, _ = pyIntID(peer["id"])
		}
		newOrder = min(newOrder, order)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE messages SET display_identity = ?, display_order = ? WHERE id = ? AND session_id = ?",
		newIdentity, newOrder, targetID, sessionID); err != nil {
		return err
	}
	for _, peer := range peers {
		if _, err := conn.ExecContext(ctx, "UPDATE messages SET display_order = ? WHERE id = ? AND session_id = ?", newOrder, peer["id"], sessionID); err != nil {
			return err
		}
	}
	return nil
}

// Same role active clone of a row id, or the addressed inactive row when no unique clone exists
func activeMessageRow(ctx context.Context, conn SQLiteConn, sessionID string, rowID int64, role string) (map[string]any, error) {
	row, err := connRow(ctx, conn, "SELECT * FROM messages WHERE id = ? AND session_id = ?", rowID, sessionID)
	if err != nil || row == nil || !pyrepr.Equal(row["role"], role) {
		return nil, err
	}
	if active, _ := pyIntID(row["active"]); active == 1 {
		return row, nil
	}
	if row["display_identity"] == nil {
		return row, nil
	}
	predicates := make([]string, len(repairColumns))
	args := []any{sessionID, role, row["display_identity"], row["timestamp"], row["id"]}
	for i, column := range repairColumns {
		predicates[i] = column + " IS ?"
		args = append(args, row[column])
	}
	clones, err := connRows(ctx, conn, "SELECT * FROM messages WHERE session_id = ? AND active = 1 AND role = ? AND display_identity IS ? AND timestamp IS ? AND id != ? AND "+
		strings.Join(predicates, " AND ")+" ORDER BY id DESC LIMIT 2", args...)
	if err != nil {
		return nil, err
	}
	if len(clones) == 1 {
		return clones[0], nil
	}
	return row, nil
}

// Stamps persistence markers and syncs canonical durable fields onto live dicts after commit
func SyncFlushedMessageMarkers(batchMsgs, batchRows []map[string]any) {
	for i := 0; i < len(batchMsgs) && i < len(batchRows); i++ {
		written, row := batchMsgs[i], batchRows[i]
		written[DBPersistedMarker] = true
		if id, ok := pyIntID(row["_row_id"]); ok {
			written["_row_id"] = id
		}
		CopyIdentityFields(row, written)
		switch row["timestamp"].(type) {
		case int, int64, float64, bool:
			written["timestamp"] = row["timestamp"]
		}
		if snap, ok := row[DBRowSnapshot].(string); ok {
			written[DBRowSnapshot] = snap
		}
		canonical, ok := row[CanonicalRow].(map[string]any)
		switch {
		case ok && pyTruthy(canonical[canonicalMetadataOnly]):
			for _, key := range liveMissingMetadata {
				if canonical[key] != nil && written[key] == nil {
					written[key] = canonical[key]
				}
			}
		case ok && pyTruthy(canonical[canonicalContentOnly]):
			written["content"] = canonical["content"]
		case ok:
			for _, key := range syncFields {
				if v, has := canonical[key]; has && v != nil {
					written[key] = v
				} else if key != "role" && key != "content" {
					delete(written, key)
				}
			}
		}
	}
}
