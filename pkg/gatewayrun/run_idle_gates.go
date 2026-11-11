package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Goals cached session database of profileHome read with only its home override installed, nil when unavailable
func profileSessionDBProbe(ctx context.Context, profileHome string) (db *state.SessionDB) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("session-db probe failed for %s", profileHome), "error", rec)
			db = nil
		}
	}()
	return goals.SessionDB(home.WithOverride(ctx, profileHome))
}

// Whether the profile's store holds work, failing open when the store or the read is unavailable
func idleGate(ctx context.Context, profileHome string, storeHasWork func(ctx context.Context, db *state.SessionDB) (bool, error)) (hasWork bool) {
	db := profileSessionDBProbe(ctx, profileHome)
	if db == nil {
		return true
	}
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("idle probe failed for %s; keeping the full sweep", profileHome), "error", rec)
			hasWork = true
		}
	}()
	found, err := storeHasWork(ctx, db)
	if err != nil {
		runLog().Debug(fmt.Sprintf("idle probe failed for %s; keeping the full sweep", profileHome), "error", pyrepr.ExceptionMessage(err))
		return true
	}
	return found
}

// Whether the profile's store holds an active heartbeat or cannot prove it holds none
func profileHasActiveHeartbeat(ctx context.Context, profileHome string) bool {
	return idleGate(ctx, profileHome, agent.StoreHasActiveHeartbeat)
}

// Whether the profile's store holds an active loop or cannot prove it holds none
func profileHasActiveLoop(ctx context.Context, profileHome string) bool {
	return idleGate(ctx, profileHome, agent.StoreHasActiveLoop)
}

// Whether the profile's store holds a pending handoff or cannot prove it holds none
func profileHasPendingHandoff(ctx context.Context, profileHome string) bool {
	return idleGate(ctx, profileHome, func(ctx context.Context, db *state.SessionDB) (bool, error) {
		return db.HasPendingHandoffs(ctx)
	})
}

// Runs a blocking gate through the runner's executor hop and answers its verdict
func (r *GatewayRunner) offLoopGate(ctx context.Context, probe func(ctx context.Context) bool) (bool, error) {
	out, err := r.runInExecutorWithContext(ctx, func(ctx context.Context) (any, error) { return probe(ctx), nil })
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(out), nil
}
