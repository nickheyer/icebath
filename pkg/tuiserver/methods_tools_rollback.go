package tuiserver

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var toolsRollbackRegistry = &handlerRegistry{}

func init() {
	scopedRPC(toolsRollbackRegistry, "insights.get", 5017, rpcShape{}, insightsGet)
	guardedRPC(toolsRollbackRegistry, "rollback.list", 5020, rpcShape{liveSession: true}, rollbackList)
	guardedRPC(toolsRollbackRegistry, "rollback.restore", 5021, rpcShape{liveSession: true}, rollbackRestore)
	guardedRPC(toolsRollbackRegistry, "rollback.diff", 5022, rpcShape{liveSession: true}, rollbackDiff)
	toolsRollbackRegistry.install()
}

// Seconds in a number of days, raising the TypeError Python raises for a value that is not a number
func daysSeconds(days any) float64 {
	switch days.(type) {
	case nil, map[string]any:
		panic(fmt.Errorf("unsupported operand type(s) for *: '%s' and 'int'", pyrepr.TypeName(days)))
	case string, []any:
		panic(fmt.Errorf("unsupported operand type(s) for -: 'float' and '%s'", pyrepr.TypeName(days)))
	}
	n, err := pyint.Float(days)
	panicIf(err)
	return n * 86400
}

// Session and message counts of the profile store over the last days
func insightsGet(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	days, present := params["days"]
	if !present {
		days = 30
	}
	db, release, err := profileDB(ctx, params, false)
	if err != nil {
		return nil, err
	}
	defer release()
	if db == nil {
		return dbUnavailableError(ctx, rid, 5017), nil
	}
	cutoff := float64(time.Now().UnixNano())/1e9 - daysSeconds(days)
	limit := 500
	rows, err := db.ListSessionsRich(ctx, state.ListSessionsRichOptions{Limit: &limit, CompactRows: true})
	if err != nil {
		return nil, err
	}
	sessions, messages := 0, int64(0)
	for _, s := range rows {
		started := s.GetOr("started_at", nil)
		if !truthy(started) {
			started = 0
		}
		at, err := pyint.Float(started)
		panicIf(err)
		if at < cutoff {
			continue
		}
		sessions++
		count, err := pyint.Int64(s.GetOr("message_count", 0))
		panicIf(err)
		messages += count
	}
	return ok(rid, map[string]any{"days": days, "sessions": sessions, "messages": messages}), nil
}

// Checkpoints of the session working directory with their reason as the message
func rollbackList(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) (map[string]any, error) {
	return withCheckpoints(ctx, session, func(mgr *tools.CheckpointManager, cwd string) map[string]any {
		if !mgr.Enabled {
			return ok(rid, map[string]any{"enabled": false, "checkpoints": []any{}})
		}
		rows := []map[string]any{}
		for _, c := range mgr.ListCheckpoints(cwd) {
			rows = append(rows, map[string]any{"hash": c.Hash, "timestamp": c.Timestamp, "message": c.Reason})
		}
		return ok(rid, map[string]any{"enabled": true, "checkpoints": rows})
	}), nil
}

// Why host checkpoints are off limits for a container backed session, read with the identity and scope a turn binds
func containerCheckpointRefusal(ctx context.Context, session *tuigateway.Session, mgr *tools.CheckpointManager, cwd string) string {
	taskID := strOr(session.Get("session_key"))
	if taskID == "" {
		taskID = "default"
	}
	scoped := sessionProfileRuntimeScope(ctx, session, true)
	bound, env := setSessionContext(scoped, taskID, &cwd, "")
	defer clearSessionContext(env)
	return mgr.UnsupportedBackendReasonIn(tools.ToolEnvFrom(bound), taskID)
}

// Restores a checkpoint, a whole tree restore also rewinding the session history to before its last user turn
func rollbackRestore(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) (map[string]any, error) {
	target, present := params["hash"]
	if !present {
		target = ""
	}
	filePath := strOr(params["file_path"])
	if !truthy(target) {
		return rpcErr(rid, 4014, "hash required", nil), nil
	}
	if all, has := params["all"]; has && all != nil {
		if _, isBool := all.(bool); !isBool {
			return rpcErr(rid, 4001, "invalid_params", map[string]any{"reason": "invalid_params", "field": "all",
				"detail": "must be a boolean"}), nil
		}
	}
	if filePath == "" && truthy(session.Get("running")) {
		return rpcErr(rid, 4009, busyMessage("rollback restore", truthy(session.Get("_manual_compress_active"))), nil), nil
	}
	result := withCheckpoints(ctx, session, func(mgr *tools.CheckpointManager, cwd string) map[string]any {
		if reason := containerCheckpointRefusal(ctx, session, mgr, cwd); reason != "" {
			return map[string]any{"success": false, "error": reason}
		}
		hash, err := resolveCheckpointHash(mgr, cwd, target)
		panicIf(err)
		res := mgr.Restore(cwd, hash, filePath, filePath == "" && params["all"] != true)
		if truthy(res["success"]) && filePath == "" {
			removed := 0
			func() {
				unlock := lockHistory(session)
				defer unlock()
				if _, userIndices := userTurnIndices(session); len(userIndices) > 0 {
					_, _, count, err := rewindActiveSessionHistory(ctx, session, len(userIndices)-1, false)
					if err != nil {
						panic(fmt.Errorf("checkpoint restored, but session history rewind failed: %s", errorMessage(err)))
					}
					removed = count
				}
			}()
			res["history_removed"] = removed
		}
		return res
	})
	return ok(rid, result), nil
}

// Diff of a checkpoint against the working tree cut to 4000 characters with its rendered copy when one renders
func rollbackDiff(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) (map[string]any, error) {
	target, present := params["hash"]
	if !present {
		target = ""
	}
	if !truthy(target) {
		return rpcErr(rid, 4014, "hash required", nil), nil
	}
	return withCheckpoints(ctx, session, func(mgr *tools.CheckpointManager, cwd string) map[string]any {
		if reason := containerCheckpointRefusal(ctx, session, mgr, cwd); reason != "" {
			return rpcErr(rid, 5022, reason, nil)
		}
		hash, err := resolveCheckpointHash(mgr, cwd, target)
		panicIf(err)
		r := mgr.Diff(cwd, hash)
		diff, has := r["diff"]
		if !has {
			diff = ""
		}
		raw := pystr.Slice(pyrepr.Str(diff), 0, 4000)
		stat, has := r["stat"]
		if !has {
			stat = ""
		}
		payload := map[string]any{"stat": stat, "diff": raw}
		cols, has := session.Lookup("cols")
		if !has {
			cols = 80
		}
		width, err := pyint.Int64(cols)
		panicIf(err)
		if rendered, found := renderDiff(raw, int(min(width, math.MaxInt32))); found && rendered != "" {
			payload["rendered"] = rendered
		}
		return ok(rid, payload)
	}), nil
}
