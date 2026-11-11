package gateway

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

// Failure of a member turn carrying the admission proof the room driver books it by
type HostedAttemptError struct {
	Err                 error
	NotAdmitted         bool
	RefusedBeforeSubmit bool
}

func (e *HostedAttemptError) Error() string { return e.Err.Error() }

func (e *HostedAttemptError) Unwrap() error { return e.Err }

func (e *HostedAttemptError) attemptProof() (bool, bool) { return e.NotAdmitted, e.RefusedBeforeSubmit }

type hostedAttemptProof interface {
	attemptProof() (notAdmitted, refusedBeforeSubmit bool)
}

// Not admitted and refused before submit marks of a failed attempt
func hostedAttemptMarks(err error) (bool, bool) {
	var proof hostedAttemptProof
	if errors.As(err, &proof) {
		return proof.attemptProof()
	}
	return false, false
}

// Fixed persisted reason of a failed attempt, never its free form message
func hostedAttemptReason(err error) string {
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) && refusal.Reason == "request_too_large" {
		return "request_too_large"
	}
	return "attempt_failed"
}

func (r *HostedRoomRuntime) workerLoop() {
	defer func() {
		for {
			r.statusLock.Lock()
			alive := []*hostedThread{}
			for _, t := range r.roomThreads {
				if t.alive() {
					alive = append(alive, t)
				}
			}
			r.statusLock.Unlock()
			if len(alive) == 0 {
				break
			}
			for _, t := range alive {
				t.join(r.ActivePollIntervalSeconds)
			}
		}
		if err := r.releaseIdleLeases(); err != nil {
			hostedRuntimeLog().Error("Exception in thread hosted-room-driver-supervisor", "exc_info", err)
		}
	}()
	for !r.stop.IsSet() {
		r.wake.Clear()
		if err := r.RunCycle(); err != nil {
			r.recordError("worker cycle failed: " + err.Error())
		}
		r.statusLock.Lock()
		r.cycle++
		r.statusLock.Unlock()
		r.wake.Wait(r.PollIntervalSeconds)
	}
}

// Runs one scheduling pass, inline outside the supervisor and on bounded room workers inside it
func (r *HostedRoomRuntime) RunCycle() error {
	r.statusLock.Lock()
	supervisor := r.thread
	r.statusLock.Unlock()
	if !supervisor.isCurrent() {
		bindings, err := r.roomsProvider()
		if err != nil {
			return err
		}
		for _, binding := range bindings {
			if r.stop.IsSet() {
				return nil
			}
			r.runRoomOnce(binding)
		}
		return nil
	}
	r.statusLock.Lock()
	for roomID, t := range r.roomThreads {
		if !t.alive() {
			delete(r.roomThreads, roomID)
		}
	}
	available := r.MaxConcurrentRooms - len(r.roomThreads)
	active := map[string]bool{}
	for roomID := range r.roomThreads {
		active[roomID] = true
	}
	r.statusLock.Unlock()
	if available <= 0 {
		return nil
	}
	bindings, err := r.roomsProvider()
	if err != nil {
		return err
	}
	if len(bindings) == 0 {
		return nil
	}
	start := r.roomScheduleCursor % len(bindings)
	r.roomScheduleCursor = (start + 1) % len(bindings)
	ordered := append(append([]HostedRoomBinding{}, bindings[start:]...), bindings[:start]...)
	for _, binding := range ordered {
		if r.stop.IsSet() || available <= 0 {
			return nil
		}
		if active[binding.RoomID] {
			continue
		}
		r.statusLock.Lock()
		gate := make(chan struct{})
		bound := binding
		r.roomThreads[binding.RoomID] = startHostedThread(func(*hostedThread) {
			<-gate
			r.runRoomOnce(bound)
		})
		r.statusLock.Unlock()
		active[binding.RoomID] = true
		available--
		close(gate)
	}
	return nil
}

func (r *HostedRoomRuntime) runRoomOnce(binding HostedRoomBinding) {
	defer func() {
		r.statusLock.Lock()
		if t := r.roomThreads[binding.RoomID]; t.isCurrent() {
			delete(r.roomThreads, binding.RoomID)
		}
		shouldWake := r.roomsNeedingReschedule[binding.RoomID]
		delete(r.roomsNeedingReschedule, binding.RoomID)
		r.statusLock.Unlock()
		if shouldWake {
			r.Wakeup()
		}
	}()
	err := r.processRoom(binding)
	if err == nil || errAs[*LeaseHeldError](err) {
		return
	}
	if errAs[*RoomUnavailableError](err) || errAs[*StaleLeaseError](err) {
		r.dropLease(binding.RoomID)
		r.setBlocked(binding.RoomID, false)
	}
	r.recordError(fmt.Sprintf("room %s: %s", binding.RoomID, err.Error()))
}

func (r *HostedRoomRuntime) processRoom(binding HostedRoomBinding) error {
	if r.prepareRoom != nil {
		if err := r.prepareRoom(binding); err != nil {
			return err
		}
	}
	if err := r.inspectAbandonedAttempts(binding); err != nil {
		return err
	}
	r.statusLock.Lock()
	deferredUntil, deferred := r.ambiguousRooms[binding.RoomID]
	r.statusLock.Unlock()
	if deferred {
		running, err := r.tasks(binding, "running")
		if err != nil {
			return err
		}
		if len(running) > 0 && r.clock() < deferredUntil {
			return nil
		}
		r.statusLock.Lock()
		delete(r.ambiguousRooms, binding.RoomID)
		r.statusLock.Unlock()
	}
	lease, err := r.ensureLease(binding)
	if err != nil {
		return err
	}
	key := hostedLeaseKey{lease.RoomID, lease.LeaseGeneration}
	r.statusLock.Lock()
	recovered := r.recoveredLeases[key]
	r.statusLock.Unlock()
	if !recovered {
		if _, err := DriverRecoverRoom(r.ctx, r.DBPath, lease, r.driverClock()); err != nil {
			return err
		}
		r.statusLock.Lock()
		r.recoveredLeases[key] = true
		r.statusLock.Unlock()
	}
	pending, err := r.retryStoppingTasks(binding, lease)
	if err != nil {
		return err
	}
	if pending {
		r.setBlocked(binding.RoomID, true)
		return nil
	}
	if blocked, err := r.reconcileIndeterminate(binding, lease); err != nil || blocked {
		return err
	}
	queued, err := r.tasks(binding, "queued")
	if err != nil {
		return err
	}
	for _, task := range queued {
		r.statusLock.Lock()
		retry, waiting := r.unavailableRouteRetries[hostedRouteKey{hrTaskIdentity(task).RoomID, hostedMemberID(task)}]
		r.statusLock.Unlock()
		if r.stop.IsSet() || (waiting && r.clock() < retry.nextAttemptAt) {
			return nil
		}
		if lease, err = r.renewLeaseIfNeeded(lease, false); err != nil {
			return err
		}
		attempt, err := DriverStartTask(r.ctx, r.DBPath, hrTaskIdentity(task), lease, hrTaskInt(task, "cancel_generation"), r.driverClock())
		if err != nil {
			return err
		}
		if err := r.executeAttempt(binding, task, attempt); err != nil {
			return err
		}
		current, err := DriverGetTask(r.ctx, r.DBPath, hrTaskIdentity(task))
		if err != nil {
			return err
		}
		if status, _ := current["status"].(string); !DriverTerminalStatuses[status] {
			return nil
		}
	}
	return nil
}

// Backs off the unavailable member route with doubling delay inside the retry bounds
func (r *HostedRoomRuntime) deferUnavailableRoute(task map[string]any) float64 {
	key := hostedRouteKey{hrTaskIdentity(task).RoomID, hostedMemberID(task)}
	now := r.clock()
	r.statusLock.Lock()
	defer r.statusLock.Unlock()
	previous, seen := r.unavailableRouteRetries[key]
	lo, hi := r.UnavailableRetryMinSeconds, r.UnavailableRetryMaxSeconds
	delay := lo
	if seen {
		delay = min(hi, max(lo, previous.delay*2))
	}
	r.unavailableRouteRetries[key] = hostedRouteRetry{delay: delay, nextAttemptAt: now + delay}
	return delay
}

func (r *HostedRoomRuntime) ensureLease(binding HostedRoomBinding) (DriverLease, error) {
	r.statusLock.Lock()
	current, held := r.leases[binding.RoomID]
	r.statusLock.Unlock()
	if held {
		renewed, err := r.renewLeaseIfNeeded(current, false)
		if err == nil {
			return renewed, nil
		}
		if !errAs[*StaleLeaseError](err) {
			return DriverLease{}, err
		}
		r.dropLease(binding.RoomID)
	}
	lease, err := DriverAcquireLease(r.ctx, r.DBPath, binding.RoomID, binding.GatewayID, binding.AuthorityEpoch, r.ProcessGeneration, r.LeaseTTLSeconds, r.driverClock())
	if err != nil {
		return DriverLease{}, err
	}
	r.statusLock.Lock()
	r.leases[binding.RoomID] = lease
	for key := range r.recoveredLeases {
		if key.roomID == binding.RoomID {
			delete(r.recoveredLeases, key)
		}
	}
	r.statusLock.Unlock()
	return lease, nil
}

func (r *HostedRoomRuntime) renewLeaseIfNeeded(lease DriverLease, force bool) (DriverLease, error) {
	if !force && r.clock() < lease.ExpiresAt-(r.LeaseTTLSeconds/2) {
		return lease, nil
	}
	renewed, err := DriverRenewLease(r.ctx, r.DBPath, lease, r.LeaseTTLSeconds, r.driverClock())
	if err != nil {
		return DriverLease{}, err
	}
	r.statusLock.Lock()
	r.leases[lease.RoomID] = renewed
	r.statusLock.Unlock()
	return renewed, nil
}

func (r *HostedRoomRuntime) dropLease(roomID string) {
	r.statusLock.Lock()
	delete(r.leases, roomID)
	r.statusLock.Unlock()
}

func (r *HostedRoomRuntime) releaseIdleLeases() error {
	r.statusLock.Lock()
	held := make(map[string]DriverLease, len(r.leases))
	for roomID, lease := range r.leases {
		held[roomID] = lease
	}
	r.statusLock.Unlock()
	for roomID, lease := range held {
		if _, err := DriverReleaseLease(r.ctx, r.DBPath, lease, r.driverClock()); err != nil {
			if errAs[*DriverStateError](err) {
				continue
			}
			return err
		}
		r.dropLease(roomID)
	}
	return nil
}

func (r *HostedRoomRuntime) setCurrentTask(roomID string, identity *TaskIdentity) {
	r.statusLock.Lock()
	defer r.statusLock.Unlock()
	if _, present := r.currentTasks[roomID]; present {
		delete(r.currentTasks, roomID)
		for i, id := range r.currentOrder {
			if id == roomID {
				r.currentOrder = append(r.currentOrder[:i], r.currentOrder[i+1:]...)
				break
			}
		}
	}
	if identity != nil {
		r.currentTasks[roomID] = *identity
		r.currentOrder = append(r.currentOrder, roomID)
	}
}

// Runs one started attempt to its terminal receipt, booking each failure by the proof it carries
func (r *HostedRoomRuntime) executeAttempt(binding HostedRoomBinding, task map[string]any, attempt TaskAttempt) error {
	profile := hrTaskProfile(task)
	payload, _ := hostedMapping(task["payload"])
	attachments, _ := payload["attachments"].([]any)
	r.setCurrentTask(binding.RoomID, &attempt.Identity)
	defer func() {
		r.setCurrentTask(binding.RoomID, nil)
		r.statusLock.Lock()
		r.roomsNeedingReschedule[binding.RoomID] = true
		r.statusLock.Unlock()
	}()
	submitAttempted, err := r.runAttempt(binding, task, attempt, profile, attachments)
	if err == nil {
		return nil
	}
	if errAs[*StaleLeaseError](err) || errAs[*StaleTaskError](err) {
		r.dropLease(binding.RoomID)
		r.recordTaskError(attempt, "fenced: "+err.Error())
		return nil
	}
	hostedRuntimeLog().Warn(fmt.Sprintf("hosted room task %s: %s", attempt.Identity.TaskID, hostedAttemptReason(err)))
	notAdmitted, refused := hostedAttemptMarks(err)
	switch {
	case refused:
		return r.settleFailureIfCurrent(attempt, err)
	case submitAttempted && notAdmitted:
		if _, fenceErr := DriverRequeueNotAdmittedTask(r.ctx, r.DBPath, attempt, r.driverClock()); fenceErr != nil {
			if !errAs[*StaleLeaseError](fenceErr) && !errAs[*StaleTaskError](fenceErr) {
				return fenceErr
			}
			r.markAmbiguous(binding, attempt)
			r.recordTaskError(attempt, "not-admitted proof lost its fence: "+fenceErr.Error())
			return nil
		}
		delay := r.deferUnavailableRoute(task)
		r.recordTaskError(attempt, "was not admitted; queued for retry in "+strconv.FormatFloat(delay, 'g', 6, 64)+"s")
		return nil
	case submitAttempted:
		r.markAmbiguous(binding, attempt)
		r.recordTaskError(attempt, "observation failed after submit: "+err.Error())
		return nil
	}
	return r.settleFailureIfCurrent(attempt, err)
}

func (r *HostedRoomRuntime) runAttempt(binding HostedRoomBinding, task map[string]any, attempt TaskAttempt, profile string, attachments []any) (bool, error) {
	transport, err := r.transportFor(binding, task)
	if err != nil {
		return false, err
	}
	if len(attachments) > 0 && (transport == nil || !transport.AcceptsAttachments()) {
		return false, &AttachmentsUnsupportedError{Msg: "attachments_unsupported_on_route: this member's route cannot carry attached files"}
	}
	release, err := r.turnLock(profile)
	if err != nil {
		return false, err
	}
	defer release()
	session, err := r.resolveOrCreate(transport, profile, binding.RoomID, true)
	if err != nil {
		return false, err
	}
	sessionID, err := hostedSessionID(session)
	if err != nil {
		return false, err
	}
	deadline := pythread.Monotonic() + r.TurnTimeoutSeconds
	payload, _ := hostedMapping(task["payload"])
	prompt, _ := payload["prompt"].(string)
	memberID := hostedMemberID(task)
	if _, err := transport.Submit(r.ctx, HostedSubmit{
		Profile: profile, SessionID: sessionID, Prompt: prompt, Source: RoomSessionSource, Task: attempt.Identity,
		ExecutionGeneration: attempt.ExecutionGeneration, Attachments: attachments, MemberID: &memberID,
		OnTerminal: func(receipt map[string]any) error { return r.onTerminal(binding, attempt, receipt) },
	}); err != nil {
		return true, err
	}
	r.statusLock.Lock()
	delete(r.unavailableRouteRetries, hostedRouteKey{hrTaskIdentity(task).RoomID, hostedMemberID(task)})
	r.statusLock.Unlock()
	receipt, err := r.waitForTerminal(binding, profile, sessionID, attempt, transport, deadline)
	if err != nil || receipt == nil {
		return true, err
	}
	_, err = DriverSettleTask(r.ctx, r.DBPath, attempt, receipt.settlementID, receipt.status, receipt.result, r.driverClock())
	return true, err
}

func (r *HostedRoomRuntime) markAmbiguous(binding HostedRoomBinding, attempt TaskAttempt) {
	r.dropLease(binding.RoomID)
	r.statusLock.Lock()
	r.ambiguousRooms[binding.RoomID] = attempt.Lease.ExpiresAt
	r.statusLock.Unlock()
}

// Durably commits one in process terminal receipt of the attempt
func (r *HostedRoomRuntime) onTerminal(binding HostedRoomBinding, attempt TaskAttempt, receipt map[string]any) error {
	status := receipt["status"]
	if status == "cancelled" {
		r.Wakeup()
		return nil
	}
	terminal := hostedTerminalReceipt{status: "failed", result: hostedBoundedTerminalResult(receipt)}
	if status == "settled" {
		terminal.status = "settled"
	}
	var settlementID any = receipt["settlement_id"]
	if !pyrepr.Truthy(settlementID) {
		settlementID = fmt.Sprintf("reply:%s:%d", attempt.Identity.TaskID, attempt.ExecutionGeneration)
	}
	settled, err := DriverSettleTask(r.ctx, r.DBPath, attempt, settlementID, terminal.status, terminal.result, r.driverClock())
	if err == nil {
		_, err = r.publish(binding, settled)
	}
	switch {
	case err == nil:
	case errAs[*StaleTaskError](err):
		current, getErr := DriverGetTask(r.ctx, r.DBPath, attempt.Identity)
		if getErr != nil {
			if !errAs[*StaleLeaseError](getErr) && !errAs[*StaleTaskError](getErr) {
				return getErr
			}
			break
		}
		if current["status"] == "stopping" {
			op := func(identity TaskIdentity, lease DriverLease, e, c int64) (map[string]any, error) {
				return DriverSettleStoppingTask(r.ctx, r.DBPath, identity, lease, e, c, settlementID, terminal.status, terminal.result, r.driverClock())
			}
			generation := attempt.ExecutionGeneration
			if _, err := r.fenced(op, &binding, current, attempt.Lease, true, &generation); err != nil &&
				!errAs[*StaleLeaseError](err) && !errAs[*StaleTaskError](err) {
				return err
			}
		}
	case errAs[*StaleLeaseError](err):
	case errAs[*DriverStateError](err):
		if err := r.settleFailureIfCurrent(attempt, errors.New("terminal result could not be committed: "+err.Error())); err != nil {
			return err
		}
	default:
		return err
	}
	r.Wakeup()
	return nil
}

func (r *HostedRoomRuntime) waitForTerminal(binding HostedRoomBinding, profile, sessionID string, attempt TaskAttempt, transport InternalSessionRPC, deadline float64) (*hostedTerminalReceipt, error) {
	lease := attempt.Lease
	for !r.stop.IsSet() {
		task, err := DriverGetTask(r.ctx, r.DBPath, attempt.Identity)
		if err != nil {
			return nil, err
		}
		status, _ := task["status"].(string)
		if DriverTerminalStatuses[status] {
			return nil, nil
		}
		if status == "stopping" {
			finished, err := r.stopWhileWaiting(binding, task, &lease)
			if err != nil {
				r.recordError(fmt.Sprintf(hostedStopPending, err.Error()))
			} else if finished {
				return nil, nil
			}
			r.wake.Wait(r.ActivePollIntervalSeconds)
			r.wake.Clear()
			continue
		}
		if pythread.Monotonic() >= deadline {
			return nil, r.expireAttemptDeadline(binding, task, lease)
		}
		if lease, err = r.renewLeaseIfNeeded(lease, false); err != nil {
			return nil, err
		}
		receipt, err := r.terminalFromHistory(transport, profile, sessionID, task)
		if err != nil || receipt != nil {
			return receipt, err
		}
		info, err := transport.Info(r.ctx, profile, sessionID, RoomSessionSource)
		if err != nil {
			return nil, err
		}
		if err := r.reportPendingAction(task, sessionID, info); err != nil {
			return nil, err
		}
		remaining := max(0.0, deadline-pythread.Monotonic())
		r.wake.Wait(min(r.ActivePollIntervalSeconds, remaining))
		r.wake.Clear()
	}
	return nil, nil
}

func (r *HostedRoomRuntime) stopWhileWaiting(binding HostedRoomBinding, task map[string]any, lease *DriverLease) (bool, error) {
	renewed, err := r.renewLeaseIfNeeded(*lease, false)
	if err != nil {
		return false, err
	}
	*lease = renewed
	return r.finishStop(binding, task, renewed)
}

// Terminalizes an acknowledged Stop, a deadline stop publishing an explicit failure
func (r *HostedRoomRuntime) completeAcknowledgedStop(binding HostedRoomBinding, task map[string]any, lease DriverLease) (map[string]any, error) {
	if !strings.HasPrefix(hostedStrOrEmpty(task["cancel_id"]), "deadline:") {
		return r.completeCancel(task, nil)
	}
	receipt := hostedTerminalReceipt{
		status: "failed", settlementID: fmt.Sprintf("deadline:%d", hrTaskInt(task, "execution_generation")),
		result: map[string]any{
			"error":       "This Group Chat turn exceeded its configured time limit and was stopped.",
			"reason_code": "turn_deadline_exceeded", "timeout_seconds": r.TurnTimeoutSeconds,
		},
	}
	return r.fenced(r.opSettleStopping(receipt), &binding, task, lease, true, nil)
}

// Fences, stops and terminalizes one exact attempt at its deadline
func (r *HostedRoomRuntime) expireAttemptDeadline(binding HostedRoomBinding, task map[string]any, lease DriverLease) error {
	switch task["status"] {
	case "running":
		begun, err := DriverBeginTaskCancel(r.ctx, r.DBPath, hrTaskIdentity(task), fmt.Sprintf("deadline:%d", hrTaskInt(task, "execution_generation")),
			hrTaskInt(task, "cancel_generation"), r.driverClock())
		if err != nil {
			return err
		}
		task = begun
	case "stopping":
	default:
		return nil
	}
	if !strings.HasPrefix(hostedStrOrEmpty(task["cancel_id"]), "deadline:") {
		return nil
	}
	lease, err := r.renewLeaseIfNeeded(lease, true)
	if err != nil {
		return err
	}
	finished, err := r.finishStop(binding, task, lease)
	if err != nil {
		return err
	}
	if !finished {
		r.recordError(fmt.Sprintf("task %s exceeded its deadline; stop remains pending", hrTaskIdentity(task).TaskID))
	}
	return nil
}

func (r *HostedRoomRuntime) isLocal(binding HostedRoomBinding, task map[string]any) (bool, error) {
	transport, err := r.transportFor(binding, task)
	return transport == r.RPC, err
}

func (r *HostedRoomRuntime) inspectAbandonedAttempts(binding HostedRoomBinding) error {
	running, err := r.tasks(binding, "running")
	if err != nil {
		return err
	}
	for _, task := range running {
		if pyrepr.Equal(task["run_process_generation"], r.ProcessGeneration) {
			continue
		}
		local, err := r.isLocal(binding, task)
		if err != nil {
			return err
		}
		var inspection hostedRecoveryInspection
		if local {
			inspection, err = r.inspectLocalRecoverySession(task)
		} else {
			inspection, err = r.inspectRecoverySession(binding, task)
		}
		if err != nil {
			return err
		}
		if inspection.terminal != nil {
			if err := r.harvestPreviousAttempt(binding, task, *inspection.terminal); err != nil {
				return err
			}
		} else if inspection.active {
			return leaseHeld("recovered session turn is still active")
		}
	}
	return nil
}

// Probes one resolved session, the optional terminal receipt from history then live info
func (r *HostedRoomRuntime) inspectSession(transport InternalSessionRPC, task map[string]any, sessionID string, readHistory bool) (hostedRecoveryInspection, error) {
	profile := hrTaskProfile(task)
	var receipt *hostedTerminalReceipt
	if readHistory {
		var err error
		if receipt, err = r.terminalFromHistory(transport, profile, sessionID, task); err != nil {
			return hostedRecoveryInspection{}, err
		}
	}
	info, err := transport.Info(r.ctx, profile, sessionID, RoomSessionSource)
	if err != nil {
		return hostedRecoveryInspection{}, err
	}
	if err := r.reportPendingAction(task, sessionID, info); err != nil {
		return hostedRecoveryInspection{}, err
	}
	return hostedRecoveryInspection{terminal: receipt, active: hostedInfoIsActiveFor(info, hrTaskIdentity(task), false),
		status: hostedStrOrEmpty(info["status"])}, nil
}

func (r *HostedRoomRuntime) inspectRecoverySession(binding HostedRoomBinding, task map[string]any) (hostedRecoveryInspection, error) {
	profile := hrTaskProfile(task)
	transport, err := r.transportFor(binding, task)
	if err != nil {
		return hostedRecoveryInspection{}, err
	}
	release, err := r.turnLock(profile)
	if err != nil {
		return hostedRecoveryInspection{}, err
	}
	defer release()
	sessionID, err := r.resumeExact(transport, hrTaskIdentity(task).RoomID, profile)
	if err != nil || sessionID == "" {
		return hostedRecoveryInspection{}, err
	}
	return r.inspectSession(transport, task, sessionID, true)
}

// Checks only live process state before explicit local recovery, no resume and no history
func (r *HostedRoomRuntime) inspectLocalRecoverySession(task map[string]any) (hostedRecoveryInspection, error) {
	profile := hrTaskProfile(task)
	release, err := r.turnLock(profile)
	if err != nil {
		return hostedRecoveryInspection{}, err
	}
	defer release()
	if r.RPC == nil {
		return hostedRecoveryInspection{}, errors.New("'NoneType' object has no attribute 'resolve_exact'")
	}
	session, err := r.RPC.ResolveExact(r.ctx, profile, RoomSessionTitle(hrTaskIdentity(task).RoomID), RoomSessionSource)
	if err != nil || session == nil {
		return hostedRecoveryInspection{}, err
	}
	sessionID, err := hostedSessionID(session)
	if err != nil {
		return hostedRecoveryInspection{}, err
	}
	return r.inspectSession(r.RPC, task, sessionID, false)
}

func (r *HostedRoomRuntime) inspected(key hostedAttemptKey, mark *bool) bool {
	r.statusLock.Lock()
	defer r.statusLock.Unlock()
	if mark != nil {
		if *mark {
			r.inspectedIndeterminate[key] = true
		} else {
			delete(r.inspectedIndeterminate, key)
		}
	}
	return r.inspectedIndeterminate[key]
}

// Resolves, defers or holds each indeterminate task, true when the room stays blocked
func (r *HostedRoomRuntime) reconcileIndeterminate(binding HostedRoomBinding, lease DriverLease) (bool, error) {
	unresolved, err := r.tasks(binding, "indeterminate")
	if err != nil {
		return false, err
	}
	if len(unresolved) == 0 {
		r.setBlocked(binding.RoomID, false)
		return false, nil
	}
	yes, no := true, false
	for _, task := range unresolved {
		key := hostedAttemptKey{binding.RoomID, hrTaskIdentity(task).TaskID, hrTaskInt(task, "execution_generation")}
		local, err := r.isLocal(binding, task)
		if err != nil {
			return false, err
		}
		if local && !r.inspected(key, nil) {
			inspection, err := r.inspectLocalRecoverySession(task)
			if err != nil {
				return false, err
			}
			r.inspected(key, &yes)
			if inspection.terminal != nil {
				if _, err := r.resolveIndeterminate(&binding, task, lease, *inspection.terminal, true); err != nil {
					return false, err
				}
				r.inspected(key, &no)
				continue
			}
			if inspection.active {
				r.setBlocked(binding.RoomID, true)
				return true, nil
			}
		}
		base := task["indeterminate_at"]
		for _, next := range []any{task["updated_at"], task["created_at"]} {
			if pyrepr.Truthy(base) {
				break
			}
			base = next
		}
		var baseSeconds float64
		if pyrepr.Truthy(base) {
			if baseSeconds, err = pyint.Float(base); err != nil {
				return false, err
			}
		} else {
			baseSeconds = r.clock()
		}
		deadline := r.IndeterminateDeferSeconds + baseSeconds
		inspection := hostedRecoveryInspection{}
		if !r.inspected(key, nil) || r.clock() >= deadline {
			if probed, err := r.probeRemote(binding, task); err != nil {
				r.recordError(fmt.Sprintf("task %s recovery probe failed: %s", hrTaskIdentity(task).TaskID, err.Error()))
			} else {
				inspection = probed
			}
			r.inspected(key, &yes)
		}
		if inspection.status == "cancelled" {
			if _, err := r.fenced(r.opResolveCancellation(fmt.Sprintf("remote-cancel:%d", hrTaskInt(task, "execution_generation"))), &binding, task, lease, false, nil); err != nil {
				return false, err
			}
			r.inspected(key, &no)
			continue
		}
		if inspection.terminal != nil {
			if _, err := r.resolveIndeterminate(&binding, task, lease, *inspection.terminal, false); err != nil {
				return false, err
			}
			r.inspected(key, &no)
			continue
		}
		if r.clock() < deadline {
			r.setBlocked(binding.RoomID, true)
			return true, nil
		}
		deferredTask, err := r.fenced(r.opDefer("member_unavailable"), nil, task, lease, true, nil)
		if err != nil {
			return false, err
		}
		r.inspected(key, &no)
		if _, err := r.publish(binding, deferredTask); err != nil {
			return false, err
		}
	}
	r.setBlocked(binding.RoomID, false)
	return false, nil
}

func (r *HostedRoomRuntime) probeRemote(binding HostedRoomBinding, task map[string]any) (hostedRecoveryInspection, error) {
	local, err := r.isLocal(binding, task)
	if err != nil || local {
		return hostedRecoveryInspection{}, err
	}
	return r.inspectRecoverySession(binding, task)
}

func (r *HostedRoomRuntime) harvestPreviousAttempt(binding HostedRoomBinding, task map[string]any, receipt hostedTerminalReceipt) error {
	gatewayID, _ := task["run_gateway_id"].(string)
	processGeneration, _ := task["run_process_generation"].(string)
	previous := TaskAttempt{
		Identity: hrTaskIdentity(task), ExecutionGeneration: hrTaskInt(task, "execution_generation"),
		CancelGeneration: hrTaskInt(task, "cancel_generation"),
		Lease: DriverLease{RoomID: binding.RoomID, GatewayID: gatewayID, AuthorityEpoch: binding.AuthorityEpoch,
			ProcessGeneration: processGeneration, LeaseGeneration: hrTaskInt(task, "run_lease_generation")},
	}
	_, err := DriverSettleTask(r.ctx, r.DBPath, previous, receipt.settlementID, receipt.status, receipt.result, r.driverClock())
	if err != nil && !errAs[*StaleLeaseError](err) && !errAs[*StaleTaskError](err) {
		return err
	}
	return nil
}

func (r *HostedRoomRuntime) tasks(binding HostedRoomBinding, status string) ([]map[string]any, error) {
	return DriverListTasks(r.ctx, r.DBPath, binding.RoomID, &status)
}

func (r *HostedRoomRuntime) bindingForRoom(roomID string) (*HostedRoomBinding, error) {
	bindings, err := r.roomsProvider()
	if err != nil {
		return nil, err
	}
	for _, b := range bindings {
		if b.RoomID == roomID {
			found := b
			return &found, nil
		}
	}
	return nil, nil
}

func (r *HostedRoomRuntime) transportFor(binding HostedRoomBinding, task map[string]any) (InternalSessionRPC, error) {
	if r.transportResolver != nil {
		return r.transportResolver(binding, task)
	}
	if r.RPC == nil {
		return nil, errors.New("hosted room transport is unavailable")
	}
	return r.RPC, nil
}

// Resolves and resumes the canonical room session, creating it or returning nil when absent
func (r *HostedRoomRuntime) resolveOrCreate(transport InternalSessionRPC, profile, roomID string, create bool) (map[string]any, error) {
	title := RoomSessionTitle(roomID)
	session, err := transport.ResolveExact(r.ctx, profile, title, RoomSessionSource)
	if err != nil {
		return nil, err
	}
	if session == nil {
		if create {
			return transport.Create(r.ctx, profile, title, RoomSessionSource)
		}
		return nil, nil
	}
	sessionID, err := hostedSessionID(session)
	if err != nil {
		return nil, err
	}
	return transport.Resume(r.ctx, profile, sessionID, RoomSessionSource)
}

func (r *HostedRoomRuntime) settleFailureIfCurrent(attempt TaskAttempt, cause error) error {
	_, err := DriverSettleTask(r.ctx, r.DBPath, attempt, fmt.Sprintf("failure:%s:%d", attempt.Identity.TaskID, attempt.ExecutionGeneration),
		"failed", map[string]any{"error": cause.Error()}, r.driverClock())
	if err != nil && !errAs[*DriverStateError](err) && !errAs[*RoomUnavailableError](err) {
		return err
	}
	r.recordTaskError(attempt, "failed: "+cause.Error())
	return nil
}
