package agent

import (
	"os"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Mirrors agent activity onto the board of a dispatched kanban worker, then folds new operator notes into the turn
var kanbanActivityBridge = func(a *AIAgent) {
	ctx := tools.WithToolEnv(a.homeContext(), a.env)
	if a.delegateDepth > 0 {
		ctx = tools.WithDelegatedChildContext(ctx, true)
	}
	kanban.HeartbeatCurrentWorkerFromEnv(ctx)
	kanban.InjectNewCommentsFromEnv(ctx, a)
}

var (
	activityProcessStart = time.Now()
	activityWallNow      = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	activityMonoNow      = func() float64 { return time.Since(activityProcessStart).Seconds() }
)

// Lock guarding the activity clock and generation counter, the installed locker when one is set
func (a *AIAgent) livenessActivityLock() sync.Locker {
	if a.turnLivenessActivityLocker != nil {
		return a.turnLivenessActivityLocker
	}
	return &a.turnLivenessActivityLock
}

// Stamps the activity clock without provenance or a forced persist
func (a *AIAgent) TouchActivity(desc string) { a.touchActivity(desc, "", false) }

// Stamps time, description and provenance under the lock, bridges kanban and persists when due
func (a *AIAgent) touchActivity(desc string, provenance string, forcePersist bool) {
	resolved := state.NormalizeActivityProvenance(provenance)
	lock := a.livenessActivityLock()
	lock.Lock()
	a.turnLivenessActivityGeneration++
	a.lastActivityTS = activityWallNow()
	a.lastActivityDesc = state.BoundActivityDescription(desc)
	a.lastActivityProvenance = resolved
	a.turnLivenessAbortClaim = nil
	lock.Unlock()
	if os.Getenv("ICEBATH_KANBAN_TASK") != "" {
		func() {
			defer func() { _ = recover() }()
			kanbanActivityBridge(a)
		}()
	}
	if forcePersist || IsTerminalCompressionProvenance(string(resolved)) {
		ResetSessionActivityPersistWindow(a)
	}
	a.persistSessionActivityIfDue()
}

// Rate limited durable activity heartbeat, failures logged at debug and never raised
func (a *AIAgent) persistSessionActivityIfDue() {
	if a.sessionID == "" || a.sessionDB == nil {
		return
	}
	nowMono := activityMonoNow()
	last := a.sessionActivityLastPersistMono
	if last != 0 && nowMono-last < SessionActivityHeartbeatMinIntervalSeconds {
		return
	}
	a.sessionActivityLastPersistMono = nowMono
	var at *float64
	if a.lastActivityTS != 0 {
		ts := a.lastActivityTS
		at = &ts
	}
	if err := a.sessionDB.TouchSessionActivity(a.sessionID, at, a.lastActivityDesc,
		state.NormalizeActivityProvenance(string(a.lastActivityProvenance))); err != nil {
		runAgentLog().Debug("session activity heartbeat write failed (ignored)", "error", err)
	}
}

// Clears mid turn activity labels and their durable copy, keeping the activity timestamp
func (a *AIAgent) resetActivityLabelsAfterTurn() {
	lock := a.livenessActivityLock()
	lock.Lock()
	a.lastActivityDesc = ""
	a.lastActivityProvenance = state.ActivityProvenanceUnknown
	lock.Unlock()
	if a.sessionID == "" || a.sessionDB == nil {
		return
	}
	_ = a.sessionDB.ClearSessionActivityLabels(a.sessionID)
}

// Diagnostic activity snapshot with the current tool, call count and iteration budget
func (a *AIAgent) GetActivitySummary() map[string]any {
	lock := a.livenessActivityLock()
	lock.Lock()
	var at *float64
	if a.lastActivityTS != 0 {
		ts := a.lastActivityTS
		at = &ts
	}
	provenance := string(a.lastActivityProvenance)
	desc := a.lastActivityDesc
	currentTool := a.currentTool
	lock.Unlock()
	if provenance == "" {
		provenance = string(state.ActivityProvenanceUnknown)
	}
	extra := pyjson.NewObject()
	if currentTool != "" {
		extra.Set("current_tool", currentTool)
	} else {
		extra.Set("current_tool", nil)
	}
	extra.Set("api_call_count", a.apiCallCount)
	extra.Set("max_iterations", a.maxIterations)
	budgetUsed, budgetMax := 0, 0
	if a.iterationBudget != nil {
		budgetUsed, budgetMax = a.iterationBudget.Used(), a.iterationBudget.MaxTotal
	}
	extra.Set("budget_used", budgetUsed)
	extra.Set("budget_max", budgetMax)
	return BuildActivitySnapshot(at, desc, provenance, nil, extra).Vals
}
