package gateway

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Local labels, a cron run being a canonical local session whose last viewer is its own fire
var pickerLocalSources = []string{"cli", "tui", "gui", "desktop", "local", "cron"}

// Sources the stored picker never lists, the internal ones and the API transport
func pickerExcludedSources() []string {
	return append(append([]string(nil), InternalListingSources...), "api_server")
}

// One stored picker entry of a row under its compression tip
func pickerRow(row, tip *pyjson.Object, sid string) *pyjson.Object {
	title := row.GetOr("title", nil)
	if !pyTruthy(title) {
		title = ""
	}
	return pyjson.ObjectOf("id", row.GetOr("id", nil), "session_id", sid, "resolved_id", sid, "title", title,
		"source", tip.GetOr("source", nil), "started_at", row.GetOr("started_at", nil),
		"message_count", row.GetOr("message_count", int64(0)), "model", tip.GetOr("model", nil), "running", false)
}

// Legacy local and mountable messaging rows offered only to the native transport that may resume them
func storedPickerSessions(ctx context.Context, a *SessionAuthority, actor Principal, limit int, includeHidden bool) (*pyjson.Object, error) {
	if actor.ProfileID != a.ProfileID {
		return nil, runtimeRefusal("profile_mismatch")
	}
	a.mu.Lock()
	legacy, known := a.nativeLegacyTransports[actor.TransportID]
	a.mu.Unlock()
	empty := pyjson.ObjectOf("sessions", []any{}, "scope", "stored")
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
			Offset: offset, OrderByLastActive: true, CompactRows: true, IncludeHidden: includeHidden})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			source, _ := row.GetOr("source", nil).(string)
			local := slices.Contains(pickerLocalSources, source) || state.ClientHistoryRow(row)
			if !local {
				if _, messaging := messagingPlatform(ctx, row.GetOr("source", nil)); !messaging {
					continue
				}
			}
			id, _ := row.GetOr("id", "").(string)
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
				root, _ := row.GetOr("_lineage_root_id", nil).(string)
				if root == "" {
					root = id
				}
				profile := row.GetOr("profile_name", nil)
				owned := profile == nil || profile == "" || profile == ownProfile
				if owned && !seen[root] {
					open, err := mountable(ctx, a, root)
					if err != nil {
						return nil, err
					}
					if open {
						seen[root] = true
						rooted := row.Copy()
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
				closedID, _ := closed.GetOr("id", "").(string)
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
	return pyjson.ObjectOf("sessions", sessions, "scope", "stored"), nil
}

// Whether a legacy row validates for the actor and its history authorizes, a not found or denied refusal skipping it
func legacyHistoryAllowed(ctx context.Context, db *state.SessionDB, actor Principal, tip *pyjson.Object, sid string) (bool, error) {
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
func closedLocalSession(ctx context.Context, a *SessionAuthority, actor Principal, row, tip *pyjson.Object) (*pyjson.Object, error) {
	owner, _ := tip.GetOr("chat_id", nil).(string)
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
	title := tip.GetOr("title", nil)
	if !pyTruthy(title) {
		title = row.GetOr("title", nil)
	}
	if !pyTruthy(title) {
		title = ""
	}
	return pyjson.ObjectOf("id", owner, "session_id", owner, "resolved_id", owner, "title", title,
		"source", tip.GetOr("source", nil), "started_at", row.GetOr("started_at", nil),
		"message_count", row.GetOr("message_count", int64(0)), "model", tip.GetOr("model", nil), "running", false), nil
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
	entry, isDict := receipt.GetOr("entry", nil).(*pyjson.Object)
	if !isDict {
		return false, nil
	}
	target, isText := entry.GetOr("session_id", nil).(string)
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
	if receipt.GetOr("profile_id", nil) != a.ProfileID {
		return false, nil
	}
	if !SameOwner(receipt.GetOr("principal_id", nil), actor.Subject) && !actor.Capabilities["session:operator"] {
		return false, nil
	}
	if row == nil || row.GetOr("ended_at", nil) == nil {
		return false, nil
	}
	admissions, err := a.DB.ListSessionAdmissions(ctx, owner, false)
	if err != nil {
		return false, err
	}
	return len(admissions) == 0, nil
}
