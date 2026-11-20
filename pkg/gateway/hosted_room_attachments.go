package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	maxAttachmentsPerMessage   = 8
	maxMessageAttachmentBytes  = 25_000_000
	hostedAttachmentSelectByID = "SELECT * FROM hosted_room_attachments WHERE attachment_id=?"
)

// Validated attachment manifest of one message
func ValidateAttachmentManifest(value any) ([]map[string]any, error) {
	list, ok := pyjson.Plain(value).([]any)
	if !ok {
		return nil, &AttachmentError{"attachments must be a list"}
	}
	if len(list) > maxAttachmentsPerMessage {
		return nil, &AttachmentError{fmt.Sprintf("attachments must contain at most %d entries", maxAttachmentsPerMessage)}
	}
	normalized := make([]map[string]any, 0, len(list))
	ids, total := map[string]bool{}, 0
	for _, entry := range list {
		item, err := safeManifestEntry(entry)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, item)
		ids[item["attachment_id"].(string)] = true
		total += item["size"].(int)
	}
	if len(ids) != len(normalized) {
		return nil, &AttachmentError{"attachment ids must be unique within one message"}
	}
	if total > maxMessageAttachmentBytes {
		return nil, &AttachmentError{"attachments exceed the per-message total size limit"}
	}
	return normalized, nil
}

// Validates and retains the exact committed attachments of a message inside the canonical append transaction
func retainMessageAttachments(ctx context.Context, conn state.SQLiteConn, roomID, eventID string, manifest any, now float64) error {
	normalized, err := ValidateAttachmentManifest(manifest)
	if err != nil {
		return err
	}
	if len(normalized) == 0 {
		return nil
	}
	if !pyjson.Equal(toAnyList(normalized), pyjson.Plain(manifest)) {
		return &AttachmentError{"published attachment metadata must be canonical"}
	}
	table, err := state.QueryRowObject(ctx, conn, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='hosted_room_attachments'")
	if err != nil {
		return err
	}
	if table == nil {
		return eventAttachmentConflict("message attachments are not committed")
	}
	for _, entry := range normalized {
		row, err := state.QueryRowObject(ctx, conn, hostedAttachmentSelectByID, entry["attachment_id"])
		if err != nil {
			return err
		}
		changed := row == nil || row.Vals["room_id"] != roomID || row.Vals["event_id"] != eventID || row.Vals["state"] != "committed" ||
			!pyjson.Equal(row.Vals["viewer_access"], int64(1)) || (row.Vals["expires_at"] != nil && hrFloat(row, "expires_at") <= now)
		if !changed {
			for key, value := range entry {
				if !pyjson.Equal(row.Vals[key], value) {
					changed = true
					break
				}
			}
		}
		if changed {
			return eventAttachmentConflict("attachment commitment changed before event publication")
		}
	}
	for _, entry := range normalized {
		if _, err := conn.ExecContext(ctx, `UPDATE hosted_room_attachments SET expires_at=NULL, updated_at=?
           WHERE attachment_id=?`, now, entry["attachment_id"]); err != nil {
			return err
		}
	}
	return nil
}

// Manifest entries as a generic list
func toAnyList(items []map[string]any) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}
