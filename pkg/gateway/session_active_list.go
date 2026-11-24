package gateway

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Live state of a turn, waiting on a prompt, working, starting before its claim or idle
func activeStatus(live *LiveSession, handle SessionHandle, pending []*jsonutil.Object) string {
	if handle.ExecutionState == "running" {
		if len(live.SnapshotControls(handle.Ref.SessionID, handle.ExecutionGeneration)) > 0 {
			return "waiting"
		}
		return "working"
	}
	for _, row := range pending {
		if row.ValueOr("status", nil) == "queued" {
			return "starting"
		}
	}
	return "idle"
}

// Live turn states of the sessions this owner holds that the actor may read
func activeList(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
	if paramsOutside(params, "profile", "current_session_id") {
		return nil, runtimeRefusal("invalid_params")
	}
	if !c.actor.Capabilities["session:read"] {
		return nil, runtimeRefusal("permission_denied")
	}
	current := ""
	if value := params.ValueOr("current_session_id", nil); anyutil.IsTrue(value) {
		current = anyutil.String(value)
	}
	entries, err := liveEntries(ctx, c.authority, c.actor)
	if err != nil {
		return nil, err
	}
	rows := []any{}
	for _, entry := range entries {
		row := entry.row
		if source, ok := row["source"].(string); (ok && slices.Contains(InternalListingSources, source)) || entry.closed {
			continue
		}
		started := listingFloat(row["started_at"])
		lastActive := started
		if anyutil.IsTrue(row["ended_at"]) {
			lastActive = listingFloat(row["ended_at"])
		}
		title := row["title"]
		if !anyutil.IsTrue(title) {
			title = ""
		}
		model := ""
		if anyutil.IsTrue(row["model"]) {
			model = anyutil.String(row["model"])
		}
		rows = append(rows, map[string]any{"current": entry.sessionID == current, "id": entry.sessionID,
			"session_key": entry.sessionID, "last_active": lastActive, "started_at": started,
			"message_count": anyInt64(row["message_count"]), "model": model, "preview": state.ShapePreview(row["_preview_raw"]),
			"title": title, "source": row["source"], "status": activeStatus(entry.live, entry.handle, entry.pending)})
	}
	return map[string]any{"sessions": rows}, nil
}

// Float of a numeric column, zero for NULL
func listingFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	}
	return 0
}
