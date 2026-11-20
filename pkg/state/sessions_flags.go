package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Pins or unpins a whole compression lineage, a pin also unhiding it unless it is the canonical Bot Chat
func (d *SessionDB) SetSessionPinned(ctx context.Context, sessionID string, pinned bool) (bool, error) {
	result, err := d.setLineageColumn(ctx, "pinned", sessionID, boolInt(pinned), "")
	if err != nil || !pinned {
		return result, err
	}
	row, err := d.GetSession(ctx, sessionID)
	if err != nil {
		return result, err
	}
	canonicalBotChat := row != nil && pyTruthy(rowValue(row, "hidden")) && rowTitle(row) == CanonicalBotChatTitle
	if !canonicalBotChat {
		if _, err := d.setLineageColumn(ctx, "hidden", sessionID, 0, ""); err != nil {
			return result, err
		}
	}
	return result, nil
}

// Hides or unhides a whole compression lineage from the default listing, leaving it resumable
func (d *SessionDB) SetSessionHidden(ctx context.Context, sessionID string, hidden bool) (bool, error) {
	return d.setLineageColumn(ctx, "hidden", sessionID, boolInt(hidden), "")
}

// Marks a whole compression lineage read at now or unread through a zero watermark
func (d *SessionDB) SetSessionRead(ctx context.Context, sessionID string, read bool) (bool, error) {
	mark := 0.0
	if read {
		mark = now()
	}
	return d.setLineageColumn(ctx, "last_read_at", sessionID, mark, "")
}

// Archives every lineage whose tip matches the prune filters, unarchived rows only unless told otherwise
func (d *SessionDB) ArchiveSessions(ctx context.Context, olderThanDays *float64, source string, f PruneFilters) (int, error) {
	if f.Archived == nil {
		unarchived := false
		f.Archived = &unarchived
	}
	f.LineageTipsOnly = true
	rows, err := d.ListPruneCandidates(ctx, olderThanDays, source, false, f)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if _, err := d.SetSessionArchived(ctx, pyStr(rowValue(row, "id")), true, true); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// Title of a session row, empty when unset
func rowTitle(row *pyjson.Object) string {
	if v := rowValue(row, "title"); v != nil {
		return pyStr(v)
	}
	return ""
}
