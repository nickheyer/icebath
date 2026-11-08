package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	reasonLeaseLost       = "session turn lease lost"
	leaseTTLSeconds       = 300.0
	leaseWaitSeconds      = 1800.0
	refreshWritePatienceS = 20.0
	refreshExpiryMarginS  = 2.0
)

// Agent slots the durable turn lease and its liveness watchdog read across turns
type turnLeaseState struct {
	sessionTurnLeaseRefreshInterval *float64
	sessionDBReserved               bool
	turnExit                        observability.TurnExitState
	executionGoroutineID            atomic.Uint64
}

// Wall clock the lease deadline arithmetic reads, swapped by tests that move time
var leaseWallNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Interrupt publication the lease drives, swapped by tests that intercept or fail the hammer
var turnLeaseInterrupt = func(a *AIAgent, message string, o InterruptOptions) (bool, error) {
	return a.InterruptWith(message, o), nil
}

// Liveness config the watchdog resolves, an empty mapping when it cannot be read
var turnLivenessConfig = func(ctx context.Context) map[string]any {
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil || tree == nil {
		return map[string]any{}
	}
	return tree
}

// Store a lease renews and releases its row through
type turnLeaseStore interface {
	RefreshSessionTurnLease(sessionID, holder string, ttlSeconds float64, patienceS *float64) (bool, error)
	ReleaseSessionTurnLease(sessionID, holder string) error
}

// Store a turn is admitted through, the agent's session db
type turnLeaseAdmissionStore interface {
	turnLeaseStore
	AcquireSessionTurnLease(sessionID, holder string, o state.TurnLeaseWait) (bool, error)
	GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error)
	ResolveResumeSessionID(sessionID string) (string, error)
	GetMessagesAsConversation(ctx context.Context, sessionID string, o state.ConversationOptions) ([]map[string]any, error)
}

// Store exposing the committed expiry of an admitted row
type turnLeaseExpiryReader interface {
	SessionTurnLeaseExpiresAt(sessionID, holder string) (*float64, error)
}

// Admission store of the agent, nil without a session db, swapped by tests that fake the store
var turnLeaseAdmissionStoreOf = func(a *AIAgent) turnLeaseAdmissionStore {
	if a.sessionDB == nil {
		return nil
	}
	return a.sessionDB
}

// Bracket the turn facade runs an admitted lease through
type turnLease interface {
	Start(ctx context.Context)
	StopRefresher()
	JoinThreads()
	ClearInterrupt()
	Release()
}

// Outcome of admission, at most one of lease and early result set
type durableTurnAdmission struct {
	earlyResult         map[string]any
	lease               turnLease
	conversationHistory []map[string]any
}

// Admitted session turn lease with the periodic refresher and liveness watchdog that keep it honest
type durableTurnLease struct {
	agent             *AIAgent
	db                turnLeaseStore
	sessionID         string
	holder            string
	authorityDeadline float64
	stop              *pythread.Event
	refreshInterval   float64
	mu                sync.Mutex
	turnActive        bool
	interruptMessage  string
	watchdog          *turnLivenessWatchdog
	timerHandles      []*cron.ScheduledHandle
}

// Lease on the admitted row with its authority deadline at the committed expiry or a TTL from now
func newDurableTurnLease(a *AIAgent, db turnLeaseStore, sessionID, holder string, expiresAt *float64) *durableTurnLease {
	deadline := leaseWallNow() + leaseTTLSeconds
	if expiresAt != nil {
		deadline = *expiresAt
	}
	interval := 60.0
	if a.sessionTurnLeaseRefreshInterval != nil {
		interval = *a.sessionTurnLeaseRefreshInterval
	}
	return &durableTurnLease{agent: a, db: db, sessionID: sessionID, holder: holder, authorityDeadline: deadline, stop: &pythread.Event{}, refreshInterval: interval}
}

// Live session id of the agent, the admission id when the agent has none
func (l *durableTurnLease) currentSessionID() string {
	if sid := l.agent.sessionID; sid != "" {
		return sid
	}
	return l.sessionID
}

// Creates the liveness watchdog when configured, erroring only when a knob overflows a float
func (l *durableTurnLease) BuildThreads(ctx context.Context) error {
	timeoutS, pollS, err := resolveTurnLivenessSettings(turnLivenessConfig(ctx))
	if err != nil {
		return err
	}
	if timeoutS != nil {
		l.watchdog = newTurnLivenessWatchdog(l.agent, turnLivenessWatchdogConfig{
			sessionID: l.currentSessionID(), timeoutS: *timeoutS, pollS: pollS, stopEvent: l.stop,
			activityLock: l.agent.livenessActivityLock(), isTurnActive: l.IsTurnActive,
			commitAbort: l.CommitLivenessAbort, deactivateTurn: l.StopRefresher,
		})
	}
	return nil
}

// Activates the turn, stamps the activity clock, arms exit metrics and schedules both timers
func (l *durableTurnLease) Start(ctx context.Context) {
	l.mu.Lock()
	l.turnActive = true
	l.mu.Unlock()
	l.agent.TouchActivity("starting new turn")
	observability.ArmTurn(ctx, &l.agent.turnExit)
	timerCtx := context.WithoutCancel(ctx)
	l.timerHandles = append(l.timerHandles, cron.SchedulePeriodic(timerCtx, func(context.Context) bool { return l.RefreshTick() }, time.Duration(l.refreshInterval*float64(time.Second))))
	if l.watchdog != nil {
		l.timerHandles = append(l.timerHandles, l.watchdog.schedule(timerCtx))
	}
}

// Stops renewal and deactivates the turn so an unwound wedge expires through the TTL
func (l *durableTurnLease) StopRefresher() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.turnActive = false
	l.stop.Set()
}

// Deactivation the watchdog runs after a committed abort, the same as stopping the refresher
func (l *durableTurnLease) DeactivateAfterLivenessAbort() { l.StopRefresher() }

// Cancels both timers waiting up to a second for an in flight tick and drops the watchdog
func (l *durableTurnLease) JoinThreads() {
	for _, h := range l.timerHandles {
		h.Cancel(time.Second)
	}
	l.timerHandles, l.watchdog = nil, nil
}

// Releases the row and clears the agent holder slots when they still name this lease
func (l *durableTurnLease) Release() {
	func() {
		defer func() {
			if r := recover(); r != nil {
				runAgentLog().Error("Failed to release session turn lease: "+l.sessionID, "error", r)
			}
		}()
		if err := l.db.ReleaseSessionTurnLease(l.sessionID, l.holder); err != nil {
			runAgentLog().Error("Failed to release session turn lease: "+l.sessionID, "error", err)
		}
	}()
	if h := l.agent.activeSessionTurnLeaseHolder; h != nil && *h == l.holder {
		l.agent.activeSessionTurnLeaseHolder = nil
		l.agent.activeSessionTurnLeaseTTLSeconds = 0
	}
}

// Reports whether the turn is still active under the lease lock
func (l *durableTurnLease) IsTurnActive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.turnActive
}

// Publishes an interrupt through the swappable hammer, a panic surfacing as an error
func (l *durableTurnLease) publishInterrupt(message string, o InterruptOptions) (published bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			published, err = false, fmt.Errorf("%v", r)
		}
	}()
	return turnLeaseInterrupt(l.agent, message, o)
}

// Hard interrupts an active turn for a lost lease, setting the flags directly when the interrupt fails
func (l *durableTurnLease) interruptTurn(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stop.IsSet() || !l.turnActive {
		return
	}
	l.interruptMessage = message
	if _, err := l.publishInterrupt(message, InterruptOptions{HardCancel: true, ToolReason: reasonLeaseLost}); err != nil {
		l.agent.interruptRequested = true
		l.agent.interruptMessage = message
		l.agent.toolInterruptReason = reasonLeaseLost
	}
}

// Commits a watchdog abort only when the observed activity is unchanged and the interrupt publishes
func (l *durableTurnLease) CommitLivenessAbort(snapshot activitySnapshot, message string) bool {
	a := l.agent
	lock := a.livenessActivityLock()
	lock.Lock()
	currentGeneration := a.turnLivenessActivityGeneration
	if currentGeneration != snapshot.generation || a.lastActivityTS != snapshot.activityTS {
		lock.Unlock()
		return false
	}
	lock.Unlock()
	l.mu.Lock()
	if l.stop.IsSet() || !l.turnActive {
		l.mu.Unlock()
		return false
	}
	l.mu.Unlock()
	published, err := l.publishInterrupt(message, InterruptOptions{HardCancel: true, ToolReason: "turn liveness watchdog", RequireGeneration: &currentGeneration})
	if err != nil {
		runAgentLog().Debug("Turn liveness abort interrupt raised; declining the abort", "error", err)
		published = false
	}
	if !published {
		return false
	}
	l.mu.Lock()
	l.interruptMessage = message
	l.mu.Unlock()
	return true
}

// Clears only the interrupt this lease raised, run after the timers are joined
func (l *durableTurnLease) ClearInterrupt() {
	l.mu.Lock()
	message := l.interruptMessage
	l.mu.Unlock()
	if message == "" {
		return
	}
	a := l.agent
	defer icLock(a.pendingRedirectLock)()
	if a.interruptMessage != message {
		return
	}
	a.interruptRequested = false
	a.interruptMessage = ""
	if a.hardInterruptRequested != nil {
		a.hardInterruptRequested.Clear()
	}
	a.interruptThreadSignalPending = false
	if a.executionThreadID != 0 {
		tools.SetInterrupt(tools.WorkerID(a.executionThreadID), false, "")
	}
}

// Stops the turn once no renewal can land before the row expires
func (l *durableTurnLease) stopOnExhaustedAuthority() bool {
	runAgentLog().Warn("Session turn lease refresh stayed locked through its lifetime: " + l.currentSessionID())
	l.interruptTurn("Session turn lease could not be refreshed; stopping to protect the transcript.")
	return false
}

// Renewal call with a panic surfacing as an error
func (l *durableTurnLease) refreshRow(patience float64) (refreshed bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			refreshed, err = false, fmt.Errorf("%v", r)
		}
	}()
	return l.db.RefreshSessionTurnLease(l.currentSessionID(), l.holder, leaseTTLSeconds, &patience)
}

// One renewal inside the authority deadline, false stops the timer after a loss or exhausted authority
func (l *durableTurnLease) RefreshTick() bool {
	if l.stop.IsSet() {
		return false
	}
	started := leaseWallNow()
	patience := l.authorityDeadline - refreshExpiryMarginS - started
	if patience <= 0 {
		return l.stopOnExhaustedAuthority()
	}
	refreshed, err := l.refreshRow(math.Min(refreshWritePatienceS, patience))
	if err == nil {
		if refreshed {
			l.authorityDeadline = started + leaseTTLSeconds
			return true
		}
		if l.stop.IsSet() {
			return false
		}
		runAgentLog().Error("Lost session turn lease while turn is active: " + l.currentSessionID())
		l.interruptTurn("Session turn lease lost; stopping to protect the transcript.")
		return false
	}
	if l.stop.IsSet() {
		return false
	}
	if state.IsSQLiteLockError(err) {
		if leaseWallNow()+l.refreshInterval+refreshExpiryMarginS >= l.authorityDeadline {
			return l.stopOnExhaustedAuthority()
		}
		runAgentLog().Warn("Session turn lease refresh hit a SQLite lock; will retry: " + l.currentSessionID())
		return true
	}
	runAgentLog().Warn("Failed to refresh session turn lease: "+l.currentSessionID(), "error", err)
	l.interruptTurn("Session turn lease could not be refreshed; stopping to protect the transcript.")
	return false
}

// Committed expiry of the admitted row, the pre acquire floor plus TTL when unreadable
func committedLeaseExpiry(db turnLeaseStore, sessionID, holder string, floor float64) float64 {
	if reader, ok := db.(turnLeaseExpiryReader); ok {
		committed, err := func() (c *float64, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			return reader.SessionTurnLeaseExpiresAt(sessionID, holder)
		}()
		if err != nil {
			runAgentLog().Debug("Could not read the admitted turn lease expiry; using the pre-acquire floor", "error", err)
		} else if committed != nil {
			return *committed
		}
	}
	return floor + leaseTTLSeconds
}

// Whether the session row exists, nil when the read failed and the state is unknown
func durableSessionExists(ctx context.Context, db turnLeaseAdmissionStore, sessionID string) *bool {
	row, err := func() (row *pyjson.Object, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return db.GetSession(ctx, sessionID)
	}()
	if err != nil {
		runAgentLog().Warn("Could not check durable session after turn lease admission; row state is unknown", "error", err)
		return nil
	}
	exists := row != nil
	return &exists
}

// Acquires the session turn lease and builds its timers, or returns the early result the facade hands back
func admitDurableTurnLease(ctx context.Context, a *AIAgent, sessionID, relayTurnID string, taskContext map[string]string, conversationHistory []map[string]any) (*durableTurnAdmission, error) {
	admission := &durableTurnAdmission{conversationHistory: conversationHistory}
	db := turnLeaseAdmissionStoreOf(a)
	if db == nil || sessionID == "" || a.persistDisabled {
		return admission, nil
	}
	platform := taskContext["platform"]
	if platform == "" {
		platform = "unknown"
	}
	holder := fmt.Sprintf("pid=%d%s:turn=%s:platform=%s", os.Getpid(), state.HolderNamespaceToken(), relayTurnID, platform)
	reloadNeeded, announced := false, false
	wait := state.NewTurnLeaseWait()
	wait.TTLSeconds, wait.WaitSeconds = leaseTTLSeconds, leaseWaitSeconds
	wait.OnContended = func() error {
		reloadNeeded = true
		return nil
	}
	wait.OnWait = func(elapsed float64) (err error) {
		reloadNeeded, announced = true, true
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		if elapsed < 1.0 {
			a.emitStatus("⏳ Another Icebath process is using this session; waiting for it to finish before starting your turn...")
		} else {
			a.emitStatus(fmt.Sprintf("⏳ Still waiting for the other Icebath process on this session (%ds)...", int64(elapsed)))
		}
		return nil
	}
	wait.ShouldAbort = func() (bool, error) { return a.interruptRequested, nil }
	authorityFloor := leaseWallNow()
	acquired, err := db.AcquireSessionTurnLease(sessionID, holder, wait)
	if err != nil {
		return nil, err
	}
	if !acquired {
		admission.earlyResult = leaseNotAcquiredResult(a, sessionID, conversationHistory, announced)
		return admission, nil
	}
	expiry := committedLeaseExpiry(db, sessionID, holder, authorityFloor)
	lease := newDurableTurnLease(a, db, sessionID, holder, &expiry)
	a.activeSessionTurnLeaseHolder = &holder
	a.activeSessionTurnLeaseTTLSeconds = leaseTTLSeconds
	admitted := false
	defer func() {
		if !admitted {
			lease.Release()
		}
	}()
	durable := durableSessionExists(ctx, db, sessionID)
	if durable != nil && *durable && !a.sessionDBReserved {
		a.sessionDBCreated = true
	}
	if reloadNeeded && (durable == nil || *durable) {
		if announced {
			a.emitStatus("Session is free; loading the latest transcript...")
		}
		latest, err := db.ResolveResumeSessionID(sessionID)
		if err != nil {
			return nil, err
		}
		if latest != "" {
			a.sessionID = latest
			taskContext["session_id"] = latest
		}
		reloaded, err := db.GetMessagesAsConversation(ctx, a.sessionID, state.ConversationOptions{RepairAlternation: true, IncludeRowIDs: true})
		if err != nil {
			return nil, err
		}
		if (durable != nil && *durable) || len(reloaded) > 0 {
			for _, m := range conversationHistory {
				if m == nil || !pyTruthy(m[state.PersistAfterAdmissionInterrupt]) {
					continue
				}
				if _, hasRowID := m["_row_id"]; hasRowID {
					continue
				}
				reloaded = append(reloaded, m)
			}
			admission.conversationHistory = reloaded
		}
	}
	if err := lease.BuildThreads(ctx); err != nil {
		return nil, err
	}
	admitted = true
	admission.lease = lease
	return admission, nil
}

// Carries the input of a follow up that interrupted the lease wait unless a hard stop cancelled it
func carryUnadmittedUserMessage(result map[string]any, userMessage any, persistUserMessage any, timestamp *float64, displayKind string, displayMetadata map[string]any, platformID string) {
	hardInterrupted := pyTruthy(result["_hard_interrupted"])
	delete(result, "_hard_interrupted")
	if hardInterrupted || !pyTruthy(result["interrupted"]) || userMessage == nil || userMessage == "" {
		return
	}
	var durableContent any = userMessage
	_, userIsList := userMessage.([]any)
	_, persistIsList := persistUserMessage.([]any)
	if persistUserMessage != nil && (!userIsList || persistIsList) {
		durableContent = persistUserMessage
	}
	deferredUser := map[string]any{"role": "user", "content": durableContent, state.PersistAfterAdmissionInterrupt: true}
	if text, isText := userMessage.(string); isText && !pyrepr.Equal(text, durableContent) {
		deferredUser["api_content"] = userMessage
	}
	if displayKind != "" {
		deferredUser["display_kind"] = displayKind
	}
	if len(displayMetadata) > 0 {
		deferredUser["display_metadata"] = displayMetadata
	}
	if platformID != "" {
		deferredUser["platform_message_id"] = platformID
	}
	messages, _ := result["messages"].([]map[string]any)
	state.AppendMessage(&messages, deferredUser, timestamp)
	result["messages"] = messages
}

// Early result of a turn that never held the lease, an interrupt notice or a retryable busy failure
func leaseNotAcquiredResult(a *AIAgent, sessionID string, conversationHistory []map[string]any, waited bool) map[string]any {
	messages := append([]map[string]any{}, conversationHistory...)
	if a.interruptRequested {
		runAgentLog().Info("session turn lease wait aborted by interrupt: " + sessionID)
		hardInterrupted := a.hardInterruptRequested != nil && a.hardInterruptRequested.IsSet()
		final := "Stopped before this turn started. Your message was not processed."
		if waited {
			final = "Stopped waiting for another Icebath process on this session. Your message was not processed."
		}
		result := map[string]any{"final_response": final, "messages": messages, "api_calls": 0, "completed": false, "interrupted": true}
		if hardInterrupted {
			result["_hard_interrupted"] = true
		}
		if a.interruptMessage != "" {
			result["interrupt_message"] = a.interruptMessage
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					a.interruptRequested = false
					a.interruptMessage = ""
				}
			}()
			a.clearInterrupt(false)
		}()
		return result
	}
	timeoutMsg := "⏳ Another Icebath process kept this session busy too long. Your message was not " +
		"processed - wait for the other process to finish, then send it again."
	runAgentLog().Error("session turn lease wait timed out for " + sessionID)
	func() {
		defer func() {
			if r := recover(); r != nil {
				runAgentLog().Debug("Failed to emit session turn lease timeout warning", "error", r)
			}
		}()
		a.emitWarning(timeoutMsg)
	}()
	return map[string]any{
		"final_response": timeoutMsg, "messages": messages, "api_calls": 0, "completed": false,
		"failed": true, "error": "session_turn_lease_timeout:" + sessionID,
		"failure_reason": "session_busy", "failure_retryable": true,
	}
}
