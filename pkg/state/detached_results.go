package state

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	lineageMatchSQL = `WITH RECURSIVE lineage(id) AS (SELECT ? UNION
    SELECT s.parent_session_id FROM sessions s JOIN lineage l ON s.id = l.id
    JOIN sessions p ON p.id = s.parent_session_id WHERE p.end_reason = 'compression'
) SELECT m.id FROM messages m JOIN lineage l ON m.session_id = l.id WHERE `
	delegationDeliveryMatch = "m.display_kind IN ('async_delegation_complete', 'hidden') AND json_extract(m.display_metadata, " +
		"'$.delegation_id') = ? AND coalesce(json_extract(m.display_metadata, '$.delivery_notice'), '') = ?"
)

// Records a detached API delegation result once, keyed by its delegation id
func (d *SessionDB) AppendDelegationDelivery(ctx context.Context, sessionID, content string, metadata *pyjson.Object) (int64, error) {
	meta := toPyObject(metadata)
	delegationID, _ := meta.get("delegation_id")
	if !pyTruthy(delegationID) {
		return 0, errors.New("Delegation delivery requires a stable delegation_id")
	}
	kind := "async_delegation_complete"
	if suppressed, _ := meta.get("presentation_suppressed"); pyTruthy(suppressed) {
		kind = "hidden"
	}
	notice, ok := meta.get("delivery_notice")
	if !ok {
		notice = ""
	}
	return d.appendDetachedResult(ctx, sessionID, content, kind, meta, delegationDeliveryMatch, delegationID, notice)
}

// Records an unviewed completion note once, keyed by its admission stored as completion_admission
func (d *SessionDB) AppendCompletionNote(ctx context.Context, sessionID, admissionID, content, displayKind string, metadata *pyjson.Object) (int64, error) {
	meta := pyObjectSet(toPyObject(metadata), "completion_admission", admissionID)
	return d.appendDetachedResult(ctx, sessionID, content, displayKind, meta,
		"json_extract(m.display_metadata, '$.completion_admission') = ?", admissionID)
}

// Inserts a between turns result as a user row once across the compression lineage, refused under a live turn
func (d *SessionDB) appendDetachedResult(ctx context.Context, sessionID, content, displayKind string, metadata pyObject, matchSQL string, params ...any) (int64, error) {
	msg := map[string]any{"content": content, "display_kind": displayKind, "display_metadata": metadata}
	StampMessageUID(msg)
	row, err := messageRowParams(sessionID, "user", msg, nil, now(), true)
	if err != nil {
		return 0, err
	}
	var msgID int64
	patience := sessionTranscriptWritePatienceS
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		existing, err := connRow(ctx, conn, lineageMatchSQL+matchSQL+" LIMIT 1", append([]any{sessionID}, params...)...)
		if err != nil {
			return err
		}
		if existing != nil {
			msgID, _ = existing["id"].(int64)
			return nil
		}
		if err := d.checkTranscriptWriteGuards(ctx, conn, sessionID, transcriptGuards{rejectActiveTurnLease: true}); err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, hermesInsertMessageSQL, row...)
		if err != nil {
			return err
		}
		if msgID, err = res.LastInsertId(); err != nil {
			return err
		}
		return bumpSessionCounters(ctx, conn, sessionID, 1, 0, true)
	}, &patience)
	return msgID, err
}
