package tuiserver

import (
	"context"
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Background processes the session key owns, each row carrying the last 4000 characters of its output
func sessionProcesses(ctx context.Context, session *tuigateway.Session) []map[string]any {
	registry := tools.DefaultProcessRegistry()
	env := tools.ToolEnvFrom(ctx)
	key := strOr(session.Get("session_key"))
	owned := []map[string]any{}
	for _, entry := range registry.ListSessions(ctx, env, "", "", false) {
		proc := registry.Get(env, strOr(entry["session_id"]))
		if proc != nil && proc.SessionKey == key {
			entry["output_tail"] = pystr.Slice(proc.DockFields().OutputBuffer, -4000, math.MaxInt)
			owned = append(owned, entry)
		}
	}
	return owned
}
