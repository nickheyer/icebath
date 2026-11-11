package gatewayrun

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Fails handoffs a dead gateway left running in the scoped store
func (r *GatewayRunner) reclaimStaleHandoffs(ctx context.Context) {
	db := r.syncSessionDB(ctx)
	if db == nil {
		return
	}
	ids := db.ReclaimStaleRunningHandoffs(ctx, "gateway stopped mid-handoff; state reclaimed at startup. Re-run /handoff to try again.")
	if len(ids) > 0 {
		runLog().Warn(fmt.Sprintf("Reclaimed %d handoff(s) stranded in 'running' by a previous gateway: %s", len(ids), strings.Join(ids, ", ")))
	}
}

// Handoff scope of one profile home, the async profile scope for a home and the launch scope for the root
func handoffScopeContext(ctx context.Context, scope handoffScope) context.Context {
	var h *routedHome
	if scope.home != "" {
		h = &routedHome{path: scope.home}
	}
	return asyncScopeOrNull(ctx, h)
}

// Claims pending CLI to gateway session handoffs and dispatches each as a full turn until the runner stops
func (r *GatewayRunner) handoffWatcher(ctx context.Context, interval, drainTimeout float64) error {
	if err := sleepCtx(ctx, 5); err != nil {
		return err
	}
	var mu sync.Mutex
	inflight := map[string]*gatewayTask{}
	dispatch := func(c context.Context, row *pyjson.Object, sessionID string, db *state.SessionDB, profileName *string) error {
		defer func() {
			mu.Lock()
			delete(inflight, sessionID)
			mu.Unlock()
		}()
		err := r.processHandoff(c, row, profileName)
		if err == nil {
			err = db.CompleteHandoff(c, sessionID)
		}
		if err == nil {
			return nil
		}
		if isCancellation(err) || c.Err() != nil {
			return err
		}
		runLog().Warn(fmt.Sprintf("Handoff for session %s failed: %s", sessionID, err.Error()), "exc_info", err)
		if _, ferr := db.FailHandoff(c, sessionID, err.Error(), nil); ferr != nil {
			runLog().Debug("Could not record handoff failure", "exc_info", ferr)
		}
		return nil
	}
	tick := func(c context.Context, profileName *string) error {
		db := r.syncSessionDB(c)
		if db == nil {
			return nil
		}
		for _, row := range db.ListPendingHandoffs(c) {
			raw, _ := row.Get("id")
			sessionID := ""
			if pyrepr.Truthy(raw) {
				sessionID = pyrepr.Str(raw)
			}
			mu.Lock()
			_, busy := inflight[sessionID]
			mu.Unlock()
			if sessionID == "" || busy {
				continue
			}
			claimed, err := db.ClaimHandoff(c, sessionID)
			if err != nil {
				return err
			}
			if !claimed {
				continue
			}
			row, id := row, sessionID
			mu.Lock()
			inflight[id] = spawnGatewayTask(context.WithoutCancel(c), "handoff-dispatch:"+id, func(tc context.Context) (any, error) {
				return nil, dispatch(tc, row, id, db, profileName)
			})
			mu.Unlock()
		}
		return nil
	}
	scopes, err := r.resolveHandoffWatchScopes(ctx)
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug("Stale-handoff reclaim failed", "error", rec)
				}
			}()
			r.reclaimStaleHandoffs(handoffScopeContext(ctx, scope))
		}()
	}
	defer func() {
		mu.Lock()
		var pending []*gatewayTask
		for _, task := range inflight {
			if !task.isDone() {
				pending = append(pending, task)
			}
		}
		mu.Unlock()
		if len(pending) == 0 {
			return
		}
		deadline := time.NewTimer(time.Duration(drainTimeout * float64(time.Second)))
		defer deadline.Stop()
	wait:
		for _, task := range pending {
			select {
			case <-task.done:
			case <-deadline.C:
				break wait
			}
		}
		for _, task := range pending {
			if !task.isDone() {
				task.Cancel()
			}
		}
	}()
	for r.running.Load() {
		err := func() (err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("%v", rec)
				}
			}()
			scopes, err := r.resolveHandoffWatchScopes(ctx)
			if err != nil {
				return err
			}
			for _, scope := range scopes {
				if scope.home != "" {
					home := scope.home
					pending, err := r.offLoopGate(ctx, func(c context.Context) bool { return profileHasPendingHandoff(c, home) })
					if err != nil {
						return err
					}
					if !pending {
						continue
					}
				}
				if err := tick(handoffScopeContext(ctx, scope), scope.name); err != nil {
					return err
				}
			}
			return nil
		}()
		if err != nil {
			if isCancellation(err) || ctx.Err() != nil {
				return err
			}
			runLog().Debug(fmt.Sprintf("Handoff watcher tick error: %s", err.Error()), "exc_info", err)
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return err
		}
	}
	return nil
}
