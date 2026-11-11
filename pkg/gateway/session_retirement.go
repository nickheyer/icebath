package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Platforms whose idle conversations leave the authority's memory
var retirablePlatforms = map[Platform]bool{PlatformLocal: true, PlatformAPIServer: true}

// Execution stamp the last viewer close completed at
type closedStamp struct {
	execution *pyjson.Object
}

// Run token of a cold refine review held on its live session
type reviewRun interface {
	RequestDone() bool
	CancelForLiveTurn(message, toolReason string)
}

type drainTaskKey struct{}

// Context of the drain goroutine of a live session, the current task of its FIFO pump
func withDrainTask(ctx context.Context, live *LiveSession) context.Context {
	return context.WithValue(ctx, drainTaskKey{}, live)
}

// True inside the drain goroutine of the live session
func inDrainTask(ctx context.Context, live *LiveSession) bool {
	held, _ := ctx.Value(drainTaskKey{}).(*LiveSession)
	return held == live
}

// True while another holder has the lock
func lockHeld(lock *sync.Mutex) bool {
	if lock.TryLock() {
		lock.Unlock()
		return false
	}
	return true
}

// True while in memory state still needs this exact live session, authority lock held
func pinnedLocked(ctx context.Context, a *SessionAuthority, sessionID string, live *LiveSession) bool {
	if a.Sessions[sessionID] != live || live.Source == nil || !retirablePlatforms[live.Source.Platform] {
		return true
	}
	if len(live.Subscribers) > 0 || len(live.EventStream.Observers) > 0 || live.Rescan || len(live.Settling) > 0 {
		return true
	}
	if liveDetached(live) || live.Detaching != 0 || isHistoryMount(live) || lockHeld(&live.mutationLock) || lockHeld(&live.claimGate) {
		return true
	}
	if live.drain != nil && !inDrainTask(ctx, live) {
		select {
		case <-live.drain:
		default:
			return true
		}
	}
	_, executing := a.executing[sessionID]
	_, closing := a.pendingCloses[sessionID]
	_, orphaned := a.orphanCloses[sessionID]
	return executing || closing || orphaned || reviewing(live)
}

// True while in memory state still needs this exact live session
func pinned(ctx context.Context, a *SessionAuthority, sessionID string, live *LiveSession) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return pinnedLocked(ctx, a, sessionID, live)
}

// True while a cold refine review started on this live session still runs
func reviewing(live *LiveSession) bool {
	return live.ReviewRun != nil && !live.ReviewRun.RequestDone()
}

// Drops the live session from memory when nothing pins it and its FIFO holds no input, true once dropped
func retireIdle(ctx context.Context, a *SessionAuthority, sessionID string, live *LiveSession, target string) (bool, error) {
	if pinned(ctx, a, sessionID, live) {
		return false, nil
	}
	busy, err := viewerBusy(ctx, a, sessionID, target)
	if err != nil || busy {
		return false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if pinnedLocked(ctx, a, sessionID, live) {
		return false, nil
	}
	delete(a.Sessions, sessionID)
	delete(a.launchSources, sessionID)
	a.retiredRoutes[sessionID] = live.Route
	if live.Source.Platform == PlatformLocal {
		if a.retiredLocal == nil {
			a.retiredLocal = map[string]bool{}
		}
		a.retiredLocal[sessionID] = true
	}
	return true, nil
}

// Retires a closed local conversation nobody views unless a turn claimed since the close found the FIFO idle
func retireClosed(ctx context.Context, a *SessionAuthority, sessionID string, live *LiveSession, target string, idleStamp *pyjson.Object) (bool, error) {
	a.mu.Lock()
	live.ClosedStamp = &closedStamp{execution: idleStamp}
	a.mu.Unlock()
	if live.EventStream.Execution != idleStamp {
		return false, nil
	}
	return retireIdle(ctx, a, sessionID, live, target)
}

// Retires at the drain's idle exit an API conversation between requests or a local one closed while the drain ran
func retireAtIdle(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession) (bool, error) {
	if live.Source == nil {
		return false, nil
	}
	if live.Source.Platform == PlatformAPIServer {
		target, err := a.PhysicalTarget(ctx, ref)
		if err != nil {
			return false, err
		}
		return retireIdle(ctx, a, ref.SessionID, live, target)
	}
	a.mu.Lock()
	stamp := live.ClosedStamp
	a.mu.Unlock()
	if live.Source.Platform == PlatformLocal && stamp != nil && stamp.execution == live.EventStream.Execution {
		target, err := viewerPhysicalTarget(ctx, a, ref.SessionID)
		if err != nil {
			return false, err
		}
		return retireIdle(ctx, a, ref.SessionID, live, target)
	}
	return false, nil
}

// Retires a conversation whose close a cold refine review pinned, once that review ended
func retireAfterReview(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession) error {
	a.mu.Lock()
	closed := a.Sessions[ref.SessionID] == live && live.ClosedStamp != nil
	a.mu.Unlock()
	if !closed {
		return nil
	}
	_, err := retireAtIdle(ctx, a, ref, live)
	return err
}

// Brings back from its receipt a conversation retirement let go, absent ones without a binding staying absent
func restoreRetired(ctx context.Context, a *SessionAuthority, sessionID string) error {
	a.mu.Lock()
	_, live := a.Sessions[sessionID]
	local := a.retiredLocal[sessionID]
	a.mu.Unlock()
	if live {
		return nil
	}
	if local {
		if _, err := restoreLocalSession(ctx, a, sessionID); err != nil {
			return err
		}
	} else if err := restoreAPISession(ctx, a, sessionID); err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return nil
		}
		return err
	}
	touchRestored(ctx, a, sessionID)
	return nil
}

// Retires again a restored conversation nobody watches, checking later while running work pins it
func retireRestored(ctx context.Context, a *SessionAuthority, sessionID string, live *LiveSession) {
	a.mu.Lock()
	live.RetireTimer = nil
	gone := a.Sessions[sessionID] != live || len(live.Subscribers) > 0 || live.Source == nil
	a.mu.Unlock()
	if gone {
		return
	}
	var target string
	var err error
	if live.Source.Platform == PlatformAPIServer {
		target, err = a.PhysicalTarget(ctx, SessionRef{ProfileID: a.ProfileID, SessionID: sessionID})
	} else {
		target, err = viewerPhysicalTarget(ctx, a, sessionID)
	}
	retired := false
	if err == nil {
		retired, err = retireIdle(ctx, a, sessionID, live, target)
	}
	if err != nil {
		slog.Error("Exception in callback retire_restored: " + pyExceptionRepr(err))
		return
	}
	if !retired {
		touchRestored(ctx, a, sessionID)
	}
}

// Runner that evicts the cached agent of a route
type agentEvictingRunner interface {
	EvictCachedAgent(route string)
}

// Drops the route, cached agent, admission source and frozen policy of a deleted retired conversation
func forgetRetired(ctx context.Context, a *SessionAuthority, sessionID string) error {
	a.mu.Lock()
	localRetired := a.retiredLocal[sessionID]
	delete(a.retiredLocal, sessionID)
	route, held := a.retiredRoutes[sessionID]
	delete(a.retiredRoutes, sessionID)
	a.mu.Unlock()
	if !held {
		return nil
	}
	if evicting, ok := a.Runner.(agentEvictingRunner); ok {
		evicting.EvictCachedAgent(route)
	}
	if !localRetired {
		return nil
	}
	adapters, err := localAdapterMap(ctx, a)
	if err != nil {
		return err
	}
	retiring, _ := adapters.Get(PlatformLocal)
	local, ok := retiring.(*LocalSessionAdapter)
	if !ok || local == nil {
		return nil
	}
	if saved := local.savedSource(sessionID); saved != nil {
		local.forget(saved, route)
	}
	return nil
}
