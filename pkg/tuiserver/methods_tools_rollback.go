package tuiserver

import (
	"context"
	"fmt"
	"math"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var toolsRollbackRegistry = &handlerRegistry{}

func init() {
	guardedRPC(toolsRollbackRegistry, "rollback.list", 5020, rpcShape{liveSession: true}, rollbackList)
	guardedRPC(toolsRollbackRegistry, "rollback.restore", 5021, rpcShape{liveSession: true}, rollbackRestore)
	guardedRPC(toolsRollbackRegistry, "rollback.diff", 5022, rpcShape{liveSession: true}, rollbackDiff)
	toolsRollbackRegistry.install()
}

// Checkpoints of the session working directory with their reason as the message
func rollbackList(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) (map[string]any, error) {
	return withCheckpoints(ctx, session, func(mgr *tools.CheckpointManager, cwd string) map[string]any {
		result := tuigateway.RollbackListResult{Enabled: mgr.Enabled, Checkpoints: []tuigateway.RollbackCheckpoint{}}
		if mgr.Enabled {
			for _, c := range mgr.ListCheckpoints(cwd) {
				result.Checkpoints = append(result.Checkpoints, tuigateway.RollbackCheckpoint{Hash: c.Hash, Timestamp: c.Timestamp, Message: c.Reason})
			}
		}
		return ok(rid, tuigateway.Plain(result))
	}), nil
}

// Why host checkpoints are off limits for a container backed session, read with the identity and scope a turn binds
func containerCheckpointRefusal(ctx context.Context, session *tuihost.Session, mgr *tools.CheckpointManager, cwd string) string {
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
func rollbackRestore(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.RollbackRestore.Params(params)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	if p.Hash == "" {
		return rpcErr(rid, 4014, "hash required", nil), nil
	}
	if p.FilePath == "" && truthy(session.Get("running")) {
		return rpcErr(rid, 4009, busyMessage("rollback restore", truthy(session.Get("_manual_compress_active"))), nil), nil
	}
	result := withCheckpoints(ctx, session, func(mgr *tools.CheckpointManager, cwd string) tuigateway.RollbackRestoreResult {
		if reason := containerCheckpointRefusal(ctx, session, mgr, cwd); reason != "" {
			return tuigateway.RollbackRestoreResult{Error: reason}
		}
		hash, err := resolveCheckpointHash(mgr, cwd, p.Hash)
		if err != nil {
			return tuigateway.RollbackRestoreResult{Error: err.Error()}
		}
		res, err := gateway.RestoreOutcome(mgr.Restore(cwd, hash, p.FilePath, p.FilePath == "" && (p.All == nil || !*p.All)))
		panicIf(err)
		if res.Success && p.FilePath == "" {
			removed := int64(0)
			func() {
				unlock := lockHistory(session)
				defer unlock()
				if _, userIndices := userTurnIndices(session); len(userIndices) > 0 {
					_, _, count, err := rewindActiveSessionHistory(ctx, session, len(userIndices)-1, false)
					if err != nil {
						panic(fmt.Errorf("checkpoint restored, but session history rewind failed: %s", errorMessage(err)))
					}
					removed = int64(count)
				}
			}()
			res.HistoryRemoved = &removed
		}
		return res
	})
	return ok(rid, tuigateway.Plain(result)), nil
}

// Diff of a checkpoint against the working tree cut to 4000 characters with its rendered copy when one renders
func rollbackDiff(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.RollbackDiff.Params(params)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	if p.Hash == "" {
		return rpcErr(rid, 4014, "hash required", nil), nil
	}
	return withCheckpoints(ctx, session, func(mgr *tools.CheckpointManager, cwd string) map[string]any {
		if reason := containerCheckpointRefusal(ctx, session, mgr, cwd); reason != "" {
			return rpcErr(rid, 5022, reason, nil)
		}
		hash, err := resolveCheckpointHash(mgr, cwd, p.Hash)
		if err != nil {
			return rpcErr(rid, 4014, err.Error(), nil)
		}
		r := mgr.Diff(cwd, hash)
		payload := tuigateway.RollbackDiffResult{}
		if diff, has := r["diff"]; has {
			payload.Diff = pystr.Slice(pyrepr.Str(diff), 0, 4000)
		}
		if stat, has := r["stat"]; has {
			payload.Stat = pyrepr.Str(stat)
		}
		cols, has := session.Lookup("cols")
		if !has {
			cols = 80
		}
		width, err := pyint.Int64(cols)
		panicIf(err)
		if rendered, found := renderDiff(payload.Diff, int(min(width, math.MaxInt32))); found {
			payload.Rendered = rendered
		}
		return ok(rid, tuigateway.Plain(payload))
	}), nil
}
