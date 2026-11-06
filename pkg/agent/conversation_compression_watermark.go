package agent

import (
	"context"
	"fmt"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Store that names held rows whose stored payload changed since they were loaded
type heldRowRewriteChecker interface {
	RewrittenHeldRowIDs(ctx context.Context, sessionID string, held []map[string]any) ([]int64, error)
}

// Positive integer row id of a held row, trusted only while the row still carries its persist marker unless copied
func exactHeldRowID(m map[string]any, copied bool) (int64, bool) {
	var rid int64
	switch v := m["_row_id"].(type) {
	case int:
		rid = int64(v)
	case int64:
		rid = v
	case int32:
		rid = int64(v)
	default:
		return 0, false
	}
	if rid <= 0 || (!copied && !pyrepr.Truthy(m[dbPersistedMarker])) {
		return 0, false
	}
	return rid, true
}

// Archive watermark capped at the newest exact held row, stale held history raised for lease less callers
func heldArchiveWatermark(ctx context.Context, db archiveWatermarkDB, sessionID string, watermark int64, messages, verbatimTail []map[string]any, staleRaises bool) (*int64, error) {
	type slot struct {
		id    int64
		exact bool
	}
	var ids []slot
	var held []int64
	var exactRows []map[string]any
	for _, m := range messages {
		if m == nil {
			continue
		}
		id, ok := exactHeldRowID(m, false)
		ids = append(ids, slot{id, ok})
		if ok {
			held = append(held, id)
			exactRows = append(exactRows, m)
		}
	}
	for _, m := range verbatimTail {
		if m == nil {
			continue
		}
		id, ok := exactHeldRowID(m, true)
		ids = append(ids, slot{id, ok})
		if ok {
			held = append(held, id)
		}
	}
	if staleRaises && len(held) > 0 {
		if checker, ok := db.(heldRowRewriteChecker); ok {
			rewritten, err := checker.RewrittenHeldRowIDs(ctx, sessionID, exactRows)
			if err != nil {
				return nil, err
			}
			if len(rewritten) > 0 {
				return nil, &StaleHeldHistory{Msg: fmt.Sprintf("held row %d of session %s was rewritten in place", slices.Min(rewritten), sessionID)}
			}
		}
	}
	if len(ids) == 0 || !ids[len(ids)-1].exact {
		return &watermark, nil
	}
	newestHeld := slices.Max(held)
	if newestHeld >= watermark && !staleRaises {
		return &watermark, nil
	}
	_, live, err := db.GetMessageRole(ctx, sessionID, newestHeld)
	if err != nil {
		return nil, err
	}
	if !live {
		if staleRaises {
			return nil, &StaleHeldHistory{Msg: fmt.Sprintf("held row %d of session %s is no longer active", newestHeld, sessionID)}
		}
		return &watermark, nil
	}
	capped := min(newestHeld, watermark)
	return &capped, nil
}
