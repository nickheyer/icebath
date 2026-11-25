package gateway

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Local labels, a cron run being a canonical local session whose last viewer is its own fire
var pickerLocalSources = []string{"cli", "tui", "gui", state.ClientHistorySource, "local", "cron"}

// Sources the stored picker never lists, the internal ones and the API transport
func pickerExcludedSources() []string {
	return append(append([]string(nil), InternalListingSources...), "api_server")
}

// One stored picker entry of a row under its compression tip
func pickerRow(row, tip *jsonutil.Object, sid string) *jsonutil.Object {
	title := row.ValueOr("title", nil)
	if !anyutil.IsTrue(title) {
		title = ""
	}
	return jsonutil.ObjectOf("id", row.ValueOr("id", nil), "session_id", sid, "resolved_id", sid, "title", title,
		"source", tip.ValueOr("source", nil), "started_at", row.ValueOr("started_at", nil),
		"message_count", row.ValueOr("message_count", int64(0)), "model", tip.ValueOr("model", nil), "running", false,
		"preview", listingPreview(row), "pinned", anyutil.IsTrue(row.ValueOr("pinned", nil)), "archived", anyutil.IsTrue(row.ValueOr("archived", nil)),
		"last_active", row.ValueOr("last_active", nil))
}

// Opening words of a stored session's listing row, empty when it has none
func listingPreview(row *jsonutil.Object) string {
	text, _ := row.ValueOr("preview", "").(string)
	return text
}

// Legacy local and mountable messaging rows offered only to the native transport that may resume them
func storedPickerSessions(ctx context.Context, a *SessionAuthority, actor Principal, limit int, includeHidden, includeArchived bool) (*jsonutil.Object, error) {
	if actor.ProfileID != a.ProfileID {
		return nil, runtimeRefusal("profile_mismatch")
	}
	a.mu.Lock()
	legacy, known := a.nativeLegacyTransports[actor.TransportID]
	a.mu.Unlock()
	empty := jsonutil.ObjectOf("sessions", []any{}, "scope", "stored")
	if !known || !samePrincipal(legacy, actor) || !actor.Capabilities["session:read"] || !actor.Capabilities["session:create"] {
		return empty, nil
	}
	db := a.DB
	ownProfile := db.OwnProfileName()
	if ownProfile == "" {
		ownProfile = "default"
	}
	sessions, offset, seen := []any{}, 0, map[string]bool{}
	pageSize := 200
	for len(sessions) < limit {
		rows, err := db.ListSessionsRich(ctx, state.ListSessionsRichOptions{ExcludeSources: pickerExcludedSources(), Limit: &pageSize,
			Offset: offset, OrderByLastActive: true, CompactRows: true, IncludeHidden: includeHidden, IncludeArchived: includeArchived})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			source, _ := row.ValueOr("source", nil).(string)
			local := slices.Contains(pickerLocalSources, source) || state.ClientHistoryRow(row)
			if !local {
				if _, messaging := messagingPlatform(ctx, row.ValueOr("source", nil)); !messaging {
					continue
				}
			}
			id, _ := row.ValueOr("id", "").(string)
			sid, err := db.GetCompressionTip(ctx, id)
			if err != nil {
				return nil, err
			}
			if sid == "" {
				sid = id
			}
			tip, err := db.GetSession(ctx, sid)
			if err != nil {
				return nil, err
			}
			if tip == nil {
				continue
			}
			if !local {
				root, _ := row.ValueOr("_lineage_root_id", nil).(string)
				if root == "" {
					root = id
				}
				profile := row.ValueOr("profile_name", nil)
				owned := profile == nil || profile == "" || profile == ownProfile
				if owned && !seen[root] {
					open, err := mountable(ctx, a, root)
					if err != nil {
						return nil, err
					}
					if open {
						seen[root] = true
						rooted := row.Clone()
						rooted.Set("id", root)
						sessions = append(sessions, pickerRow(rooted, tip, root))
					}
				}
				if len(sessions) == limit {
					break
				}
				continue
			}
			closed, err := closedLocalSession(ctx, a, actor, row, tip)
			if err != nil {
				return nil, err
			}
			if closed != nil {
				closedID, _ := closed.ValueOr("id", "").(string)
				if !seen[closedID] {
					seen[closedID] = true
					sessions = append(sessions, closed)
				}
				if len(sessions) == limit {
					break
				}
				continue
			}
			allowed, err := legacyHistoryAllowed(ctx, db, actor, tip, sid)
			if err != nil {
				return nil, err
			}
			if !allowed {
				continue
			}
			sessions = append(sessions, pickerRow(row, tip, sid))
			if len(sessions) == limit {
				break
			}
		}
		if len(rows) < pageSize {
			break
		}
		offset += len(rows)
	}
	return jsonutil.ObjectOf("sessions", sessions, "scope", "stored"), nil
}

// Whether a legacy row validates for the actor and its history authorizes, a not found or denied refusal skipping it
func legacyHistoryAllowed(ctx context.Context, db *state.SessionDB, actor Principal, tip *jsonutil.Object, sid string) (bool, error) {
	err := db.ValidateLegacyRow(tip, actor.Subject)
	allowed := false
	if err == nil {
		err = db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			ok, err := state.AuthorizeHistory(ctx, conn, historyActor(actor), sid, false)
			allowed = ok
			return err
		})
	}
	if err != nil {
		switch refusalReasonOf(err) {
		case "not_found", "permission_denied":
			return false, nil
		}
		return false, err
	}
	return allowed, nil
}

// A canonical local session its last viewer closed listed as resumable, nil for anything else
func closedLocalSession(ctx context.Context, a *SessionAuthority, actor Principal, row, tip *jsonutil.Object) (*jsonutil.Object, error) {
	owner, _ := tip.ValueOr("chat_id", nil).(string)
	var live *LiveSession
	if owner != "" {
		live, _ = a.live(owner)
	}
	if live == nil {
		if owner == "" {
			return nil, nil
		}
		closed, err := storedClosed(ctx, a, actor, owner)
		if err != nil || !closed {
			return nil, err
		}
	} else {
		handle, err := a.handle(ctx, SessionRef{ProfileID: a.ProfileID, SessionID: owner})
		if err == nil {
			err = a.Authorize(ctx, actor, handle.Ref, "session:read")
		}
		if err != nil {
			if runtimeStoreErrorOf(err) != nil {
				return nil, nil
			}
			return nil, err
		}
		closed, err := closedAndUnviewed(ctx, a, owner, handle)
		if err != nil || !closed {
			return nil, err
		}
	}
	title := tip.ValueOr("title", nil)
	if !anyutil.IsTrue(title) {
		title = row.ValueOr("title", nil)
	}
	if !anyutil.IsTrue(title) {
		title = ""
	}
	return jsonutil.ObjectOf("id", owner, "session_id", owner, "resolved_id", owner, "title", title,
		"source", tip.ValueOr("source", nil), "started_at", row.ValueOr("started_at", nil),
		"message_count", row.ValueOr("message_count", int64(0)), "model", tip.ValueOr("model", nil), "running", false,
		"preview", listingPreview(row), "pinned", anyutil.IsTrue(row.ValueOr("pinned", nil)), "archived", anyutil.IsTrue(row.ValueOr("archived", nil)),
		"last_active", row.ValueOr("last_active", nil)), nil
}

// Whether an unrestored ended session's stored receipt reads as closed for the actor with no admission left
func storedClosed(ctx context.Context, a *SessionAuthority, actor Principal, owner string) (bool, error) {
	if actor.ProfileID != a.ProfileID || !actor.Capabilities["session:read"] {
		return false, nil
	}
	receipt, err := a.DB.LocalReceipt(ctx, owner)
	if err != nil {
		if runtimeStoreErrorOf(err) != nil {
			return false, nil
		}
		return false, err
	}
	if receipt == nil {
		return false, nil
	}
	entry, isDict := receipt.ValueOr("entry", nil).(*jsonutil.Object)
	if !isDict {
		return false, nil
	}
	target, isText := entry.ValueOr("session_id", nil).(string)
	if !isText {
		return false, nil
	}
	row, err := a.DB.GetSession(ctx, target)
	if err != nil {
		if runtimeStoreErrorOf(err) != nil {
			return false, nil
		}
		return false, err
	}
	if receipt.ValueOr("profile_id", nil) != a.ProfileID {
		return false, nil
	}
	if !SameOwner(receipt.ValueOr("principal_id", nil), actor.Subject) && !actor.Capabilities["session:operator"] {
		return false, nil
	}
	if row == nil || row.ValueOr("ended_at", nil) == nil {
		return false, nil
	}
	admissions, err := a.DB.ListSessionAdmissions(ctx, owner, true)
	if err != nil {
		return false, err
	}
	return len(admissions) == 0, nil
}
