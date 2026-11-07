package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Routing identity columns a messaging lane stamps onto every segment of a conversation it takes over
const localLaneIdentity = "session_key, source, user_id, chat_id, chat_type, thread_id, display_name, origin_json, transport_profile"

// Hook into pkg/gateway naming whether a row source label is a messaging platform other than local and the API transport
var MessagingPlatformSource func(source string) bool

type movedReceipt struct {
	Owner  string
	Target string
	Source any
}

// Local receipts whose current target row now carries another lane's route, narrowed to the target ids when given
func movedReceipts(ctx context.Context, conn SQLiteConn, targetIDs map[string]bool) ([]movedReceipt, error) {
	rows, err := connRows(ctx, conn, "SELECT substr(m.key, ?) AS owner, s.id AS target, s.source AS source FROM state_meta m JOIN sessions s "+
		"ON s.id = CASE WHEN json_valid(m.value) THEN json_extract(m.value, '$.entry.session_id') END "+
		"WHERE m.key GLOB ? AND s.session_key IS NOT NULL "+
		"AND s.session_key IS NOT CASE WHEN json_valid(m.value) THEN json_extract(m.value, '$.route') END",
		len(LocalPolicyPrefix)+1, LocalPolicyPrefix+"*")
	if err != nil {
		return nil, err
	}
	var out []movedReceipt
	seen := map[string]int{}
	for _, row := range rows {
		target := pyStr(row["target"])
		if targetIDs != nil && !targetIDs[target] {
			continue
		}
		owner := pyStr(row["owner"])
		m := movedReceipt{Owner: owner, Target: target, Source: row["source"]}
		if i, ok := seen[owner]; ok {
			out[i] = m
			continue
		}
		seen[owner] = len(out)
		out = append(out, m)
	}
	return out, nil
}

// Retires the creation policy and legacy binding of each owner whose row a messaging lane now holds
func releaseReceipts(ctx context.Context, conn SQLiteConn, owners []string) error {
	for _, owner := range owners {
		if _, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key IN (?,?)", LocalPolicyPrefix+owner, LocalLegacyPrefix+owner); err != nil {
			return err
		}
	}
	return nil
}

// Local owner and the row a handoff of the session moves, the receipt target's compression tip for a local conversation
func (d *SessionDB) HandoffHead(ctx context.Context, sessionID string) (string, string, error) {
	var owner string
	var saved map[string]any
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		var err error
		if owner, err = localLineageOwner(ctx, conn, sessionID); err != nil {
			return err
		}
		saved, err = connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", LocalPolicyPrefix+owner)
		return err
	})
	if err != nil {
		return "", "", err
	}
	var target any
	if saved != nil {
		target = localPolicyTarget(saved["value"])
	}
	start := sessionID
	if pyTruthy(target) {
		start = pyStr(target)
	} else {
		owner = ""
	}
	tip, err := d.GetCompressionTip(ctx, start)
	if err != nil {
		return "", "", err
	}
	if tip == "" {
		tip = start
	}
	return owner, tip, nil
}

// Decoded receipt object, nil for anything json loads rejects or a non object
func localReceiptObject(raw any) *pyjson.Object {
	text, ok := raw.(string)
	if !ok {
		b, isBytes := raw.([]byte)
		if !isBytes {
			return nil
		}
		decoded, err := pyJSONDecodeBytes(b)
		if err != nil {
			return nil
		}
		text = decoded
	}
	value, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	o, _ := value.(*pyjson.Object)
	return o
}

// First row of the target's new segment, walking back through the compressions behind it
func localSegmentRoot(ctx context.Context, conn SQLiteConn, segments []string, target string) (string, error) {
	index := -1
	for i, sid := range segments {
		if sid == target {
			index = i
			break
		}
	}
	if index < 0 {
		return target, nil
	}
	for index > 0 {
		row, err := connRow(ctx, conn, "SELECT end_reason FROM sessions WHERE id=?", segments[index-1])
		if err != nil {
			return "", err
		}
		if row == nil || row["end_reason"] != "compression" {
			break
		}
		index--
	}
	return segments[index], nil
}

// Moves the conversation name to its head over a title of lower provenance there, an equal rank title staying
func localTitleFollowsHead(ctx context.Context, conn SQLiteConn, owner, head string) error {
	row, err := connRow(ctx, conn, "SELECT title, title_source, hidden FROM sessions WHERE id=?", owner)
	if err != nil {
		return err
	}
	if head == owner || row == nil || !pyTruthy(row["title"]) {
		return nil
	}
	kept, err := connRow(ctx, conn, "SELECT title, title_source FROM sessions WHERE id=?", head)
	if err != nil {
		return err
	}
	if kept == nil || (pyTruthy(kept["title"]) && titleRankOf(kept["title_source"]) >= titleRankOf(row["title_source"])) {
		return nil
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET title=NULL WHERE id IN (?,?)", owner, head); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "UPDATE sessions SET title=?, title_source=?, hidden=? WHERE id=?", row["title"], row["title_source"], row["hidden"], head)
	return err
}

// Retires the moved receipts and stamps the lane identity onto every other segment, returning the owners handed off
func handOffConversations(ctx context.Context, conn SQLiteConn, moved []movedReceipt) ([]movedReceipt, error) {
	var handed []movedReceipt
	for _, m := range moved {
		saved, err := connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", LocalPolicyPrefix+m.Owner)
		if err != nil {
			return nil, err
		}
		var lineage any = []any{m.Owner}
		if saved != nil {
			if receipt := localReceiptObject(saved["value"]); receipt != nil {
				if v, ok := receipt.Get("lineage"); ok {
					lineage = v
				}
			}
		}
		list, ok := lineage.([]any)
		if !ok {
			continue
		}
		segments := []string{m.Owner}
		seen := map[string]bool{m.Owner: true}
		valid := true
		for _, v := range list {
			sid, isStr := v.(string)
			if !isStr {
				valid = false
				break
			}
			if !seen[sid] {
				seen[sid] = true
				segments = append(segments, sid)
			}
		}
		if !valid {
			continue
		}
		encoded := make([]any, len(segments))
		for i, sid := range segments {
			encoded[i] = sid
		}
		text, err := pyjson.Dumps(encoded)
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET ("+localLaneIdentity+") = (SELECT "+localLaneIdentity+" FROM sessions WHERE id=?) "+
			"WHERE id IN (SELECT value FROM json_each(?)) AND id != ?", m.Target, text, m.Target); err != nil {
			return nil, err
		}
		head, err := localSegmentRoot(ctx, conn, segments, m.Target)
		if err != nil {
			return nil, err
		}
		if err := localTitleFollowsHead(ctx, conn, m.Owner, head); err != nil {
			return nil, err
		}
		handed = append(handed, m)
	}
	owners := make([]string, len(handed))
	for i, m := range handed {
		owners[i] = m.Owner
	}
	return handed, releaseReceipts(ctx, conn, owners)
}

// Stored creation receipt of a local session, storage_unavailable when it is missing or not a JSON object
func (d *SessionDB) LocalReceipt(ctx context.Context, sessionID string) (*pyjson.Object, error) {
	var row map[string]any
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		var err error
		row, err = connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", LocalPolicyPrefix+sessionID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, NewRuntimeStoreError("storage_unavailable", nil, nil)
	}
	receipt := localReceiptObject(row["value"])
	if receipt == nil {
		return nil, NewRuntimeStoreError("storage_unavailable", nil, nil)
	}
	return receipt, nil
}
