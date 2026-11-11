package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

// Capped backoff bounds in seconds of a claim refused by transient storage
const (
	claimRetryMinS = 0.25
	claimRetryMaxS = 5.0
)

// Longest in seconds a settled turn's adapter may hold its adopted crash marker before the follower is claimed
const markerReleaseWaitS = 60.0

// Waits out the pause between two claim attempts, an error when the drain is cancelled first
func drainSleep(ctx context.Context, seconds float64) error {
	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Future of one admission's reply that its process local observer awaits
type admissionWaiter struct {
	once   sync.Once
	done   chan struct{}
	result any
	err    error
}

// Unresolved waiter
func newAdmissionWaiter() *admissionWaiter { return &admissionWaiter{done: make(chan struct{})} }

// Whether the waiter was resolved
func (w *admissionWaiter) isDone() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// Resolves the waiter with a reply unless it was resolved before
func (w *admissionWaiter) setResult(result any) {
	w.once.Do(func() {
		w.result = result
		close(w.done)
	})
}

// Resolves the waiter with a refusal unless it was resolved before
func (w *admissionWaiter) setException(err error) {
	w.once.Do(func() {
		w.err = err
		close(w.done)
	})
}

// Reply or refusal of the waiter once resolved, the context error when the caller stops waiting first
func (w *admissionWaiter) wait(ctx context.Context) (any, error) {
	select {
	case <-w.done:
		return w.result, w.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Hosted room service that reauthorizes a queued hosted admission at its claim
type HostedAdmissionChecker interface {
	CheckAdmission(ctx context.Context, ref SessionRef, row *pyjson.Object) error
}

// Runner surface of the classic turn slots a route may hold outside the FIFO
type classicTurnRunner interface {
	IsSessionRunning(route string) bool
	EvictIdleStaleAgent(ctx context.Context, route string)
	EvictReapedAgent(ctx context.Context, route string)
}

// Outcome of one settlement attempt, the committed row and the reply it carries once it landed
type turnSettlement struct {
	Settled  *pyjson.Object
	Response string
}

// Result of one claim attempt with the queued head its checks validated
type claimAttempt struct {
	row         *pyjson.Object
	reread      bool
	classicBusy bool
	first       *pyjson.Object
}

// Claim write result, the claimed row and whether the validated head moved under the write
type claimWrite struct {
	row   *pyjson.Object
	moved bool
}

// Takes a waiter out of the table, nil when none waits on the admission
func (a *SessionAuthority) popWaiter(admissionID string) *admissionWaiter {
	a.mu.Lock()
	defer a.mu.Unlock()
	waiter := a.waiters[admissionID]
	delete(a.waiters, admissionID)
	return waiter
}

// Reruns the drain of a session and of its logical owner once a worker execution on it ended
func (a *SessionAuthority) wakeAfterWorker(ctx context.Context, sessionID string) error {
	owner, err := a.LogicalOwner(ctx, sessionID)
	if err != nil {
		return err
	}
	for i, sid := range []string{sessionID, owner} {
		if i == 1 && sid == sessionID {
			continue
		}
		if _, ok := a.live(sid); ok {
			if err := scheduleSession(ctx, a, SessionRef{a.ProfileID, sid}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Releases the process local waiters of a FIFO that stopped without claiming its head, with the reason
func (a *SessionAuthority) pause(ctx context.Context, ref SessionRef, reason string) error {
	rows, err := a.DB.ListSessionAdmissions(ctx, ref.SessionID, true)
	if err != nil {
		return err
	}
	for _, row := range rows {
		admissionID := rowString(row, "admission_id")
		a.mu.Lock()
		native := a.nativeWaiters[admissionID]
		if reason == "session_busy" && !native {
			a.mu.Unlock()
			continue
		}
		delete(a.nativeWaiters, admissionID)
		waiter := a.waiters[admissionID]
		delete(a.waiters, admissionID)
		a.mu.Unlock()
		if waiter != nil {
			waiter.setException(runtimeRefusal(reason))
		}
	}
	return nil
}

// True while a turn this FIFO did not claim holds the route's running slot, leaked and reaped slots evicted first
func (a *SessionAuthority) classicTurnHolds(ctx context.Context, route string) bool {
	runner, ok := a.Runner.(classicTurnRunner)
	if !ok || !runner.IsSessionRunning(route) {
		return false
	}
	now := pythread.Monotonic()
	a.mu.Lock()
	if a.classicEvictAt == nil {
		a.classicEvictAt = map[string]float64{}
	}
	due := now-a.classicEvictAt[route] >= 1.0
	if due {
		a.classicEvictAt[route] = now
	}
	a.mu.Unlock()
	if due {
		runner.EvictIdleStaleAgent(ctx, route)
		if runner.IsSessionRunning(route) {
			runner.EvictReapedAgent(ctx, route)
		}
	}
	return runner.IsSessionRunning(route)
}

// True when the stored row is gone or no longer queued
func (a *SessionAuthority) headMoved(ctx context.Context, admissionID string) (bool, error) {
	current, err := a.DB.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		return false, err
	}
	return current == nil || rowString(current, "status") != AdmissionQueued, nil
}

// Validates the queued head and claims it, a reread asked when the head changed during preflight
func (a *SessionAuthority) claimNext(ctx context.Context, ref SessionRef, live *LiveSession) (claimAttempt, error) {
	live.claimGate.Lock()
	defer live.claimGate.Unlock()
	live.mutationLock.Lock()
	defer live.mutationLock.Unlock()
	pending, err := a.DB.ListSessionAdmissions(ctx, ref.SessionID, true)
	if err != nil {
		return claimAttempt{}, err
	}
	var first *pyjson.Object
	for _, row := range pending {
		if rowString(row, "status") == AdmissionUnknown {
			return claimAttempt{}, runtimeRefusal("unknown_execution")
		}
	}
	for _, row := range pending {
		if rowString(row, "status") == AdmissionQueued {
			first = row
			break
		}
	}
	a.mu.Lock()
	source, route := live.Source, live.Route
	a.mu.Unlock()
	if source == nil {
		return claimAttempt{}, errors.New("'NoneType' object has no attribute 'platform'")
	}
	platform := source.Platform
	if platform == PlatformLocal {
		if err := applyPendingReasoning(ctx, a, ref, live); err != nil {
			return claimAttempt{}, err
		}
		if err := applyDeferredPick(ctx, a, ref, live); err != nil {
			return claimAttempt{}, err
		}
	}
	if first != nil && platform == PlatformLocal {
		if _, err := restoreLocalSession(ctx, a, ref.SessionID); err != nil {
			return claimAttempt{}, err
		}
		if strings.HasPrefix(rowString(first, "request_id"), "hosted:") {
			remote, err := checkRemoteHostedAdmission(ctx, a, ref, first)
			if err != nil {
				return claimAttempt{}, err
			}
			if !remote {
				if a.HostedRoomService == nil {
					return claimAttempt{}, runtimeRefusal("permission_denied")
				}
				if err := a.HostedRoomService.CheckAdmission(ctx, ref, first); err != nil {
					return claimAttempt{}, err
				}
			}
			moved, err := a.headMoved(ctx, rowString(first, "admission_id"))
			if err != nil {
				return claimAttempt{}, err
			}
			if moved {
				return claimAttempt{reread: true, first: first}, nil
			}
		}
		if _, automation := rowPayload(first).Get("local_automation_v1"); automation {
			if err := checkLocalAutomation(ctx, a, ref, first, ""); err != nil {
				return claimAttempt{}, err
			}
		} else if err := checkLocalInput(a, ref, first, nil); err != nil {
			return claimAttempt{}, err
		}
		if err := refreezeEditorMCP(ctx, a, ref, live); err != nil {
			return claimAttempt{}, err
		}
		if !notedCompletion(first) {
			if _, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
				return nil, reopenLocalSession(taskCtx, a, ref)
			}, nil, false, nil); err != nil {
				return claimAttempt{}, err
			}
		}
	}
	if first != nil {
		if _, native := rowPayload(first).Get("native_text_v1"); native {
			target, err := a.PhysicalTarget(ctx, ref)
			if err != nil {
				return claimAttempt{}, err
			}
			if _, _, err := checkNativeRoute(ctx, a.Runner, rowPayload(first), target, source, a.Runner.AdapterForSource(ctx, source)); err != nil {
				return claimAttempt{}, err
			}
			moved, err := a.headMoved(ctx, rowString(first, "admission_id"))
			if err != nil {
				return claimAttempt{}, err
			}
			if moved {
				return claimAttempt{reread: true, first: first}, nil
			}
		}
	}
	if first != nil && platform == PlatformAPIServer {
		if err := checkAPITurn(ctx, a, ref, rowPayload(first)); err != nil {
			return claimAttempt{}, err
		}
		if _, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
			return nil, reopenAPISession(taskCtx, a, ref)
		}, nil, false, nil); err != nil {
			return claimAttempt{}, err
		}
	} else if first != nil && liveDetached(live) {
		if !notedCompletion(first) {
			if err := reopenForTurn(ctx, a, ref); err != nil {
				return claimAttempt{}, err
			}
		}
	}
	if first != nil && a.classicTurnHolds(ctx, route) {
		return claimAttempt{classicBusy: true, first: first}, nil
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return claimAttempt{}, err
	}
	a.mu.Lock()
	live.Rescan = false
	a.mu.Unlock()
	var head *string
	if first != nil {
		id := rowString(first, "admission_id")
		head = &id
	}
	result, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		row, moved, err := a.DB.ClaimSessionInput(taskCtx, state.ClaimInput{Epoch: a.Epoch, SessionID: ref.SessionID,
			Guard: a.admissionGate(nil, false), CheckHead: true, Head: head})
		return claimWrite{row, moved}, err
	}, func(taskCtx context.Context, written any) (any, error) {
		return written, a.stampClaim(taskCtx, ref, live, written.(claimWrite).row)
	}, false, nil)
	if err != nil {
		return claimAttempt{}, err
	}
	claimed := result.(claimWrite)
	a.mu.Lock()
	rescan := live.Rescan
	a.mu.Unlock()
	return claimAttempt{row: claimed.row, reread: claimed.moved || (claimed.row == nil && rescan), first: first}, nil
}

// Stamps the claim's execution on the stream in the same tracked task as its commit
func (a *SessionAuthority) stampClaim(ctx context.Context, ref SessionRef, live *LiveSession, row *pyjson.Object) error {
	if row == nil {
		return nil
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	generation, _ := row.Get("generation")
	live.EventStream.Execution = pyjson.ObjectOf("authority_epoch", a.Epoch, "execution_generation", generation,
		"admission_id", rowString(row, "admission_id"))
	a.mu.Lock()
	if a.executing == nil {
		a.executing = map[string]int64{}
	}
	if stamped := rowInt(row, "generation"); stamped != nil {
		a.executing[ref.SessionID] = *stamped
	}
	a.mu.Unlock()
	live.EventStream.Publish(ref.SessionID, automationAdmissionDisplay(rowPayload(row)), "message.start", nil)
	return a.publishPending(ctx, ref)
}

// True when a drain about to wait must exit, the runtime draining, the profile retiring or its live entry replaced
func (a *SessionAuthority) drainStopping(ctx context.Context, ref SessionRef, live *LiveSession) (bool, error) {
	current, _ := a.live(ref.SessionID)
	if a.Runner.Draining() || a.Retiring || current != live {
		return true, a.pause(ctx, ref, "runtime_draining")
	}
	return false, nil
}

// Settles the queued head a preclaim check refused as cancelled, its waiters released with the reason
func (a *SessionAuthority) refuseHead(ctx context.Context, ref SessionRef, admissionID, reason string) (bool, error) {
	before, err := a.DB.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		return false, err
	}
	if before != nil && rowString(before, "status") == AdmissionQueued {
		a.mu.Lock()
		a.cancelObligations[admissionID] = true
		a.mu.Unlock()
	}
	discard := func() {
		a.mu.Lock()
		delete(a.cancelObligations, admissionID)
		a.mu.Unlock()
	}
	result, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		return a.DB.CancelSessionInput(taskCtx, a.Epoch, admissionID)
	}, nil, false, nil)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			discard()
			return false, nil
		}
		return false, err
	}
	row := result.(*pyjson.Object)
	if outcome, _ := row.Get("outcome"); rowString(row, "status") != AdmissionTerminal || outcome != "cancelled" {
		discard()
		return false, nil
	}
	a.mu.Lock()
	owed := a.cancelObligations[admissionID]
	a.mu.Unlock()
	if owed {
		if waiter := a.popWaiter(admissionID); waiter != nil {
			waiter.setException(runtimeRefusal(reason))
		}
		if err := a.settleCancelled(ctx, ref, admissionID, "cancelled"); err != nil {
			return false, err
		}
	}
	if err := releaseAdmissionMedia(ctx, a.DB, admissionID); err != nil {
		return false, err
	}
	return true, nil
}

// True for a writer held past its patience or a full disk, the storage failures a claim waits out
func transientClaimFailure(err error) bool {
	if class, ok := state.SQLiteClassOf(err); ok {
		return class == state.SQLiteOperationalError
	}
	return pypath.IsOSError(err)
}

// First queued admission id of a session, empty when nothing is queued
func (a *SessionAuthority) queuedHead(ctx context.Context, ref SessionRef) (string, error) {
	rows, err := a.DB.ListSessionAdmissions(ctx, ref.SessionID, true)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if rowString(row, "status") == AdmissionQueued {
			return rowString(row, "admission_id"), nil
		}
	}
	return "", nil
}

// One claim with its refusal handling, done false asking the retry loop for another pass
func (a *SessionAuthority) claimOnce(ctx context.Context, ref SessionRef, live *LiveSession, conflicted *string, refusing *[2]string) (attempt claimAttempt, done, exit bool, err error) {
	if refusing[0] != "" {
		if _, err := a.refuseHead(ctx, ref, refusing[0], refusing[1]); err != nil {
			return claimAttempt{}, false, false, err
		}
		*refusing = [2]string{}
	}
	attempt, err = a.claimNext(ctx, ref, live)
	if err == nil {
		return attempt, true, false, nil
	}
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) {
		return claimAttempt{}, false, false, err
	}
	if refusal.Reason == "unknown_execution" {
		settled, err := settleUnattendedUnknown(ctx, a, ref)
		if err != nil {
			return claimAttempt{}, false, false, err
		}
		if settled {
			return claimAttempt{}, false, false, nil
		}
	}
	if refusal.Reason == "admission_conflict" {
		head, err := a.queuedHead(ctx, ref)
		if err != nil {
			return claimAttempt{}, false, false, err
		}
		if head != "" && head != *conflicted {
			*conflicted = head
			return claimAttempt{}, false, false, nil
		}
		if head != "" {
			*refusing = [2]string{head, refusal.Reason}
			refused, err := a.refuseHead(ctx, ref, head, refusal.Reason)
			if err != nil {
				return claimAttempt{}, false, false, err
			}
			*refusing = [2]string{}
			if refused {
				return claimAttempt{}, false, false, nil
			}
		}
	}
	slog.Warn(fmt.Sprintf("Session %s paused: %s", ref.SessionID, refusal.Reason))
	if err := a.pause(ctx, ref, refusal.Reason); err != nil {
		return claimAttempt{}, false, false, err
	}
	return claimAttempt{}, false, true, nil
}

// Next claim of the drain, waiting out transient storage and a classic turn, exit true when the drain must end
func (a *SessionAuthority) claimWithRetry(ctx context.Context, ref SessionRef, live *LiveSession) (claimAttempt, bool, error) {
	backoff, conflicted, refusing := 0.0, "", [2]string{}
	for {
		attempt, done, exit, err := a.claimOnce(ctx, ref, live, &conflicted, &refusing)
		if err != nil {
			if !transientClaimFailure(err) {
				return claimAttempt{}, false, err
			}
			stopping, err2 := a.drainStopping(ctx, ref, live)
			if err2 != nil {
				return claimAttempt{}, false, err2
			}
			if stopping {
				return claimAttempt{}, true, nil
			}
			backoff = min(max(backoff*2, claimRetryMinS), claimRetryMaxS)
			slog.Warn(fmt.Sprintf("Session %s claim deferred by storage (%s); retrying in %.1fs", ref.SessionID, pyExceptionRepr(err), backoff))
			if err := drainSleep(ctx, backoff); err != nil {
				return claimAttempt{}, false, err
			}
			continue
		}
		if exit {
			return claimAttempt{}, true, nil
		}
		if !done {
			continue
		}
		backoff = 0.0
		if !attempt.classicBusy {
			return attempt, false, nil
		}
		stopping, err := a.drainStopping(ctx, ref, live)
		if err != nil {
			return claimAttempt{}, false, err
		}
		if stopping {
			return claimAttempt{}, true, nil
		}
		if err := drainSleep(ctx, claimRetryMinS); err != nil {
			return claimAttempt{}, false, err
		}
	}
}

// Drops the stamps and latches of the session's claim once its execution or settlement is over
func (a *SessionAuthority) clearClaim(live *LiveSession, sessionID string) {
	live.EventStream.Lock.Lock()
	live.EventStream.Execution = pyjson.NewObject()
	live.EventStream.Lock.Unlock()
	a.mu.Lock()
	delete(a.pendingStops, sessionID)
	delete(a.pendingStopCauses, sessionID)
	delete(a.pendingSteers, sessionID)
	delete(a.adopted, sessionID)
	a.mu.Unlock()
}

// Runs the claimed admission, its reply and outcome, a lost managed worker settling or pausing the FIFO
func (a *SessionAuthority) executeClaimed(ctx context.Context, ref SessionRef, live *LiveSession, row *pyjson.Object) (string, string, error) {
	if live.ReviewRun != nil && !live.ReviewRun.RequestDone() {
		cancelLiveReviewForTurn(ctx, live)
	}
	response, err := executeFiniteAdmission(ctx, a, ref, row)
	live.EventStream.Lock.Lock()
	a.mu.Lock()
	delete(a.executing, ref.SessionID)
	a.mu.Unlock()
	live.EventStream.Lock.Unlock()
	if err == nil {
		return response, "completed", nil
	}
	var unknown *ManagedExecutionUnknown
	if errors.As(err, &unknown) {
		a.clearClaim(live, ref.SessionID)
		settled, settleErr := settleUnattendedUnknown(ctx, a, ref)
		if settleErr != nil {
			return "", "", settleErr
		}
		if settled {
			return "", "settled_unknown", nil
		}
		if pauseErr := a.pause(ctx, ref, "unknown_execution"); pauseErr != nil {
			return "", "", pauseErr
		}
		return "", "", err
	}
	if errors.Is(err, context.Canceled) {
		return "", "", err
	}
	slog.Error(fmt.Sprintf("Admitted turn %s failed", rowString(row, "admission_id")), "error", err)
	return "The admitted turn failed.", "failed", nil
}

// What the drain does once a claimed turn's settlement block ended
type settleNext int

const (
	settleDeliver settleNext = iota
	settleContinue
	settleReturn
)

// Settles a finished turn and publishes its completion, recovering a settlement that failed after its commit
func (a *SessionAuthority) settleClaimed(ctx context.Context, ref SessionRef, live *LiveSession, row *pyjson.Object, response *string, outcome string, settled **pyjson.Object) (settleNext, error) {
	admissionID := rowString(row, "admission_id")
	err := func() error {
		a.mu.Lock()
		captured := a.PendingResults[admissionID]
		a.mu.Unlock()
		settlement := turnSettlement{}
		err := settleWithRetry(ctx, a, live, ref, row, *response, outcome, captured, &settlement)
		if settlement.Settled != nil {
			*settled, *response = settlement.Settled, settlement.Response
			a.mu.Lock()
			delete(a.PendingResults, admissionID)
			a.mu.Unlock()
		}
		if err != nil {
			return err
		}
		if current, _ := a.live(ref.SessionID); current == live {
			return a.publishPending(ctx, ref)
		}
		return nil
	}()
	if err == nil {
		return settleDeliver, nil
	}
	if errors.Is(err, context.Canceled) {
		return settleDeliver, err
	}
	slog.Error(fmt.Sprintf("Settlement or publication of admission %s failed", admissionID), "error", err)
	if *settled != nil {
		return settleDeliver, nil
	}
	a.mu.Lock()
	if _, held := a.PendingResults[admissionID]; !held {
		a.PendingResults[admissionID] = pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", *response, "failed", outcome == "failed"),
			"usage", pyjson.NewObject())
	}
	a.mu.Unlock()
	if _, err := recoverFailedSettlement(ctx, a, row); err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return settleReturn, a.pause(ctx, ref, "unknown_execution")
		}
		return settleDeliver, err
	}
	current, err := a.DB.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		return settleDeliver, err
	}
	if current == nil {
		return settleDeliver, errors.New("'NoneType' object is not subscriptable")
	}
	if rowString(current, "status") == AdmissionUnknown {
		retainUnsettledDelivery(a, admissionID)
		if err := a.publishPending(ctx, ref); err != nil {
			return settleDeliver, err
		}
		return settleReturn, a.pause(ctx, ref, "unknown_execution")
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	stored, err := admissionResult(ctx, a.DB, admissionID)
	if err != nil {
		return settleDeliver, err
	}
	a.mu.Lock()
	delete(a.PendingResults, admissionID)
	a.mu.Unlock()
	captured, _ := stored.(*pyjson.Object)
	if captured == nil {
		if waiter := a.popWaiter(admissionID); waiter != nil {
			waiter.setException(runtimeRefusal("unknown_execution"))
		}
		return settleContinue, nil
	}
	*settled = current
	*response = ""
	if result, ok := rowGet(captured, "result"); ok {
		if obj, isObj := result.(*pyjson.Object); isObj {
			if text, _ := obj.Get("final_response"); pyTruthy(text) {
				*response, _ = text.(string)
			}
		}
	}
	return settleDeliver, publishTerminal(ctx, a, ref, row, *settled, *response, captured)
}

// Drains the session's FIFO, claiming, executing and settling one admission at a time until it is idle or paused
func drainSessionFIFO(ctx context.Context, a *SessionAuthority, ref SessionRef) error {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return &pyKeyError{ref.SessionID}
	}
	for {
		claimed, exit, err := a.claimWithRetry(ctx, ref, live)
		if err != nil {
			return err
		}
		if exit {
			return nil
		}
		row, first := claimed.row, claimed.first
		if claimed.reread {
			continue
		}
		if row == nil && first != nil {
			return a.pause(ctx, ref, "session_busy")
		}
		a.mu.Lock()
		live.PauseNotified = false
		a.mu.Unlock()
		if row == nil {
			if _, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
				ended, err := endIdleACPSession(taskCtx, a, ref.SessionID)
				return ended, err
			}, nil, false, nil); err != nil {
				return err
			}
			if a.rescanPending(live) {
				continue
			}
			if err := settleViewerClose(ctx, a, ref.SessionID); err != nil {
				return err
			}
			if a.rescanPending(live) {
				continue
			}
			_, err := retireAtIdle(ctx, a, ref, live)
			return err
		}
		admissionID := rowString(row, "admission_id")
		response, outcome, err := a.executeClaimed(ctx, ref, live, row)
		if err == nil && outcome == "settled_unknown" {
			a.takeDeferredReset(live.Route)
			continue
		}
		if err != nil {
			a.takeDeferredReset(live.Route)
			if fireErr := fireAdmissionPostDelivery(context.WithoutCancel(ctx), a, admissionID); fireErr != nil {
				return fireErr
			}
			var unknown *ManagedExecutionUnknown
			if errors.As(err, &unknown) {
				return nil
			}
			return err
		}
		var settled *pyjson.Object
		next, settleErr := a.settleClaimed(ctx, ref, live, row, &response, outcome, &settled)
		a.clearClaim(live, ref.SessionID)
		closing := context.WithoutCancel(ctx)
		generation := int64(0)
		if stamped := rowInt(row, "generation"); stamped != nil {
			generation = *stamped
		}
		if err := publishTransfers(closing, a, ref, generation); err != nil {
			return err
		}
		if settled == nil {
			a.mu.Lock()
			delete(a.pendingDeliveries, admissionID)
			a.mu.Unlock()
			dropProducerTurn(a, admissionID)
			if err := fireAdmissionPostDelivery(closing, a, admissionID); err != nil {
				return err
			}
		}
		delete(waiterReplies(a), admissionID)
		reset := a.takeDeferredReset(live.Route)
		if settleErr != nil {
			return settleErr
		}
		switch next {
		case settleContinue:
			continue
		case settleReturn:
			return nil
		}
		runDeferredReset(ctx, live.Route, reset)
		if err := deliverSettled(ctx, a, admissionID); err != nil {
			return err
		}
		if err := fireAdmissionPostDelivery(ctx, a, admissionID); err != nil {
			return err
		}
		if err := a.resolveSettledWaiter(ctx, admissionID, response); err != nil {
			return err
		}
	}
}

// True when input was scheduled on the still live drain since its last claim read
func (a *SessionAuthority) rescanPending(live *LiveSession) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return live.Rescan
}

// Takes the auto reset a route's turn deferred, nil when it deferred none
func (a *SessionAuthority) takeDeferredReset(route string) func(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	reset := a.deferredResets[route]
	delete(a.deferredResets, route)
	return reset
}

// Runs the auto reset the settled turn deferred before its waiter wakes, a failure logged and dropped
func runDeferredReset(ctx context.Context, route string, reset func(ctx context.Context) error) {
	if reset == nil {
		return
	}
	if err := reset(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error(fmt.Sprintf("Deferred auto-reset of %s failed", route), "error", err)
	}
}

// Hands the settled reply to its waiter and waits a bounded time for its adapter to release the crash marker
func (a *SessionAuthority) resolveSettledWaiter(ctx context.Context, admissionID, response string) error {
	a.mu.Lock()
	adopter := a.markerAdopters[admissionID]
	a.mu.Unlock()
	var released *InterruptEvent
	if adopter != nil && pyTruthy(adopter.GatewayActiveTurnToken()) {
		released = NewInterruptEvent()
		adopter.SetGatewayMarkerReleased(released)
	}
	waiter := a.popWaiter(admissionID)
	if waiter == nil || waiter.isDone() {
		return nil
	}
	waiter.setResult(response)
	if released != nil && !(a.Runner.Draining() || a.Retiring) {
		timer := time.NewTimer(time.Duration(markerReleaseWaitS * float64(time.Second)))
		defer timer.Stop()
		select {
		case <-released.Done():
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
