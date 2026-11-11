package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// End reason stamped on an ACP session its last viewer left
const acpEndReason = "acp_disconnect"

// True while a live session still has a viewer subscribed
func hasViewers(a *SessionAuthority, sessionID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	live := a.Sessions[sessionID]
	return live != nil && len(live.Subscribers) > 0
}

// Stored receipt of a local session with its policy source and physical target, false for any other session
func localReceiptTarget(ctx context.Context, a *SessionAuthority, sessionID string) (source any, target any, found bool, err error) {
	receipt, err := a.DB.LocalReceipt(ctx, sessionID)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	if policy, ok := rowGet(receipt, "policy"); ok {
		if obj, isObj := policy.(*pyjson.Object); isObj {
			source, _ = obj.Get("source")
		}
	}
	if entry, ok := rowGet(receipt, "entry"); ok {
		if obj, isObj := entry.(*pyjson.Object); isObj {
			target, _ = obj.Get("session_id")
		}
	}
	return source, target, true, nil
}

// Ends the idle row of a local session with a reason, a refusal or malformed receipt logged and reading false
func endIdleLocalRow(ctx context.Context, a *SessionAuthority, sessionID string, target any, reason, label string) (bool, error) {
	targetID, ok := target.(string)
	if !ok {
		slog.Warn(label + " session " + sessionID + " not ended")
		return false, nil
	}
	changed, err := a.DB.EndIdleLocalSession(ctx, a.Epoch, sessionID, targetID, reason)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			slog.Warn(label+" session "+sessionID+" not ended", "error", err)
			return false, nil
		}
		return false, err
	}
	return changed != 0, nil
}

// Ends an ACP created local session with no viewer and an idle FIFO
func endIdleACPSession(ctx context.Context, a *SessionAuthority, sessionID string) (bool, error) {
	if hasViewers(a, sessionID) {
		return false, nil
	}
	source, target, found, err := localReceiptTarget(ctx, a, sessionID)
	if err != nil || !found || source != "acp" {
		return false, err
	}
	return endIdleLocalRow(ctx, a, sessionID, target, acpEndReason, "ACP")
}

// Ends an idle local session with the reason its last viewer gave, an ACP row keeping its own reason
func endIdleViewerSession(ctx context.Context, a *SessionAuthority, sessionID, reason string) (bool, error) {
	if hasViewers(a, sessionID) {
		return false, nil
	}
	source, target, found, err := localReceiptTarget(ctx, a, sessionID)
	if err != nil || !found {
		return false, err
	}
	if source == "acp" {
		return endIdleACPSession(ctx, a, sessionID)
	}
	return endIdleLocalRow(ctx, a, sessionID, target, reason, "local")
}

// Reopens a session a viewer attached to while its close committed
func reopenIfReattached(ctx context.Context, a *SessionAuthority, sessionID string, ended bool) (bool, error) {
	if !ended || !hasViewers(a, sessionID) {
		return ended, nil
	}
	receipt, err := a.DB.LocalReceipt(ctx, sessionID)
	if err != nil {
		return ended, err
	}
	entry, ok := receipt.Get("entry")
	if !ok {
		return ended, &pyKeyError{"entry"}
	}
	target, ok := rowGet(entry.(*pyjson.Object), "session_id")
	if !ok {
		return ended, &pyKeyError{"session_id"}
	}
	return ended, a.DB.ReopenSession(ctx, target.(string))
}

// Ends every ACP session nobody is viewing at gateway shutdown, returning how many it ended
func endIdleACPSessions(ctx context.Context, a *SessionAuthority) (int, error) {
	var ids []string
	err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		ids = ids[:0]
		rows, err := conn.QueryContext(ctx, "SELECT key FROM state_meta WHERE key LIKE ?", state.LocalPolicyPrefix+"%")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			ids = append(ids, strings.TrimPrefix(key, state.LocalPolicyPrefix))
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	ended := 0
	for _, sid := range ids {
		done, err := endIdleACPSession(ctx, a, sid)
		if err != nil {
			return ended, err
		}
		if done {
			ended++
		}
	}
	return ended, nil
}
