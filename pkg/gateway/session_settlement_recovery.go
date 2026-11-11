package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Interrupt reasons a gateway stop records on the turns it cuts short
const (
	interruptReasonGatewayShutdown = "Gateway shutting down"
	interruptReasonGatewayRestart  = "Gateway restarting"
)

// Notices a viewer reads for a turn the owner's restart or shutdown interrupted
const (
	restartInterruptNotice  = "Gateway restarting: this turn was interrupted and will not be run again. Send your message again once Hermes reconnects."
	shutdownInterruptNotice = "Gateway shutting down: this turn was interrupted and will not be run again. Send your message again once Hermes is back."
)

// Pauses in seconds between settlement attempts after a transient storage error
var settleRetryDelaysS = []float64{0.5, 2.0}

// True for a storage failure a settlement retries, an OS error or any sqlite error
func transientSettleFailure(err error) bool {
	if _, ok := state.SQLiteClassOf(err); ok {
		return true
	}
	return pypath.IsOSError(err)
}

// Fences a claim whose settlement failed as unknown, returning the status the row holds afterwards
func markUnknown(ctx context.Context, a *SessionAuthority, row *pyjson.Object) (string, error) {
	live, _ := a.live(rowString(row, "target_session_id"))
	transfers := settlementTransfers(a, live, row)
	admissionID, generation := rowString(row, "admission_id"), rowGeneration(row)
	status := ""
	err := a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		status = ""
		if err := state.RuntimeEpoch(ctx, conn, a.Epoch); err != nil {
			return err
		}
		current, err := state.RuntimeAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		owner := rowInt(current, "owner_epoch")
		if owner == nil || *owner != a.Epoch || rowGeneration(current) != generation {
			return runtimeRefusal("stale_generation")
		}
		switch rowString(current, "status") {
		case AdmissionTerminal, AdmissionUnknown:
			status = rowString(current, "status")
			return nil
		case AdmissionStarted:
		default:
			return runtimeRefusal("stale_generation")
		}
		if _, err := conn.ExecContext(ctx, "UPDATE session_admissions SET status='unknown' WHERE admission_id=?", admissionID); err != nil {
			return err
		}
		if err := state.SettleSteerTransfers(ctx, conn, a.Epoch, generation, transfers, "unknown"); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", rowString(row, "target_session_id")); err != nil {
			return err
		}
		status = AdmissionUnknown
		return nil
	})
	return status, err
}

// Steers handed to a claim, read under the stream lock once its execution returned
func settlementTransfers(a *SessionAuthority, live *LiveSession, row *pyjson.Object) map[string]bool {
	if live == nil {
		return nil
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	return steerTransfers(a, rowString(row, "target_session_id"), rowGeneration(row))
}

// Stamps the recovery fence of a failed settlement, retrying until storage lets it commit
func recoverFailedSettlement(ctx context.Context, a *SessionAuthority, row *pyjson.Object) (string, error) {
	for {
		task := trackMutation(a, func(taskCtx context.Context) (any, error) { return markUnknown(taskCtx, a, row) })
		status, err := task.wait()
		if err == nil {
			return status.(string), nil
		}
		if !transientSettleFailure(err) {
			return "", err
		}
		if err := drainSleep(ctx, 1); err != nil {
			return "", err
		}
	}
}

// Result object of a captured execution, empty when it captured none
func capturedResultOf(captured *pyjson.Object) *pyjson.Object {
	if captured != nil {
		if value, _ := captured.Get("result"); pyTruthy(value) {
			if obj, ok := value.(*pyjson.Object); ok {
				return obj
			}
		}
	}
	return pyjson.NewObject()
}

// Completion frame payload of a settled turn with its outcome, status and the fields a viewer redraws from
func completionPayload(row, settled *pyjson.Object, response any, captured *pyjson.Object) *pyjson.Object {
	outcome := rowString(settled, "outcome")
	published := outcome
	if outcome == "interrupted" {
		published = "cancelled"
	}
	status := "error"
	switch outcome {
	case "completed":
		status = "complete"
	case "interrupted":
		status = "interrupted"
	}
	complete := pyjson.ObjectOf("text", response, "content", response, "admission_id", rowString(row, "admission_id"),
		"outcome", published, "status", status)
	result := capturedResultOf(captured)
	textOf := func(key string) string {
		value, _ := result.Get(key)
		text, _ := value.(string)
		return text
	}
	truthy := func(key string) bool {
		value, _ := result.Get(key)
		return pyTruthy(value)
	}
	surface, _ := result.Get("error_surface")
	if _, isSurface := surface.(*pyjson.Object); !isSurface && status == "error" {
		providerName, model := "", ""
		if truthy("provider") {
			value, _ := result.Get("provider")
			providerName = pyrepr.Str(value)
		}
		if truthy("model") {
			value, _ := result.Get("model")
			model = pyrepr.Str(value)
		}
		surface = buildErrorSurfaceFromResult(result, providerName, model)
	}
	if descriptor, ok := surface.(*pyjson.Object); ok && descriptor != nil {
		complete.Set("error_surface", descriptor)
	}
	if pyTruthy(response) && truthy("response_reused") {
		complete.Set("response_reused", true)
	}
	if truthy("local_viewer") {
		if truthy("response_previewed") {
			complete.Set("response_previewed", true)
		}
		if truthy("response_transformed") {
			complete.Set("response_transformed", true)
			if final, _ := result.Get("final_response"); !pyTruthy(response) && pyTruthy(final) {
				complete.Set("text", final)
				complete.Set("content", final)
			}
		}
		if warning := textOf("warning"); warning != "" {
			complete.Set("warning", warning)
		}
		if billing, _ := result.Get("billing_block"); billing != nil {
			if block, ok := billing.(*pyjson.Object); ok {
				complete.Set("billing", block)
			}
		}
		if reason := textOf("failure_reason"); reason != "" {
			complete.Set("failure_reason", reason)
		}
		if reasoning := pystr.Strip(textOf("last_reasoning")); reasoning != "" {
			complete.Set("reasoning", reasoning)
		}
	}
	if captured != nil {
		if usage, _ := captured.Get("session_usage"); usage != nil {
			if stats, ok := usage.(*pyjson.Object); ok {
				complete.Set("usage", tuiUsage(stats))
			}
		}
	}
	if persisted, _ := result.Get("persisted_turn"); persisted != nil {
		if turn, ok := persisted.(*pyjson.Object); ok {
			bound := turn.Copy()
			requestID, _ := row.Get("request_id")
			bound.Set("submission_id", requestID)
			complete.Set("persisted_turn", bound)
		}
	}
	return complete
}

// Publishes a settled turn's completion frame under the stream lock the caller holds, no storage write
func publishCompletion(ctx context.Context, a *SessionAuthority, live *LiveSession, ref SessionRef, row, settled *pyjson.Object, response any, captured *pyjson.Object) error {
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error(fmt.Sprintf("Terminal control cleanup failed for admission %s", rowString(row, "admission_id")), "error", r)
			}
		}()
		pendingControlsSnapshot(live, ref.SessionID, nil)
	}()
	complete := completionPayload(row, settled, response, captured)
	notice := ownerStopNotice(captured, rowString(settled, "outcome"), row)
	if warning, _ := complete.Get("warning"); notice != "" && !pyTruthy(warning) {
		complete.Set("warning", notice)
	}
	live.EventStream.Publish(ref.SessionID, complete, "message.complete", nil)
	return nil
}

// Named notice for a turn the owner's restart or shutdown interrupted, empty for any other turn
func ownerStopNotice(captured *pyjson.Object, outcome string, row *pyjson.Object) string {
	if outcome != "interrupted" {
		return ""
	}
	cause, _ := capturedResultOf(captured).Get("interrupt_message")
	notice := ""
	switch cause {
	case interruptReasonGatewayRestart:
		notice = restartInterruptNotice
	case interruptReasonGatewayShutdown:
		notice = shutdownInterruptNotice
	}
	if notice != "" && row != nil && isUnattended(row) {
		return automationInterruptedNotice
	}
	return notice
}

// Notice and display metadata of a failed turn the gateway itself refused with a renderable descriptor, nil metadata otherwise
func gatewayRefusalBoundary(captured *pyjson.Object, outcome string) (string, *pyjson.Object) {
	value := capturedResultOf(captured)
	surface, _ := value.Get("error_surface")
	failure, _ := value.Get("error")
	descriptor, isSurface := surface.(*pyjson.Object)
	text, isText := failure.(string)
	if outcome != "failed" || !isSurface || descriptor == nil || !isText || text == "" {
		return "", nil
	}
	return text, pyjson.ObjectOf("error", text, "error_surface", descriptor.Copy())
}

// Releases a settled admission's media in its own write after publication, a failure logged and dropped
func releaseSettledMedia(ctx context.Context, a *SessionAuthority, row *pyjson.Object) {
	if err := releaseAdmissionMedia(ctx, a.DB, rowString(row, "admission_id")); err != nil {
		slog.Error(fmt.Sprintf("Terminal media cleanup failed for admission %s", rowString(row, "admission_id")), "error", err)
	}
}

// Commits a turn's result off the stream lock and publishes its completion, the claim reading started until then
func commitAndPublish(ctx context.Context, a *SessionAuthority, live *LiveSession, ref SessionRef, row *pyjson.Object, response any, outcome string, captured *pyjson.Object, settlement *turnSettlement) error {
	prepared, err := prepareResult(row, response, outcome, captured)
	if err != nil {
		return err
	}
	admissionID := rowString(row, "admission_id")
	live.EventStream.Lock.Lock()
	a.mu.Lock()
	live.Settling[admissionID] = row
	a.mu.Unlock()
	live.EventStream.Lock.Unlock()
	transfers := settlementTransfers(a, live, row)
	unmask := func() {
		a.mu.Lock()
		delete(live.Settling, admissionID)
		a.mu.Unlock()
	}
	err = func() error {
		defer func() {
			live.EventStream.Lock.Lock()
			unmask()
			live.EventStream.Lock.Unlock()
		}()
		settled, committed, err := finishResult(ctx, a.DB, a.Epoch, row, response, outcome, captured, prepared, transfers)
		if err != nil {
			return err
		}
		settlement.Settled, settlement.Response = settled, committed
		live.EventStream.Lock.Lock()
		defer live.EventStream.Lock.Unlock()
		unmask()
		return publishCompletion(ctx, a, live, ref, row, settled, committed, captured)
	}()
	if err != nil {
		return err
	}
	releaseSettledMedia(ctx, a, row)
	return nil
}

// Commits the captured result, retrying a transient storage failure a bounded number of times
func settleWithRetry(ctx context.Context, a *SessionAuthority, live *LiveSession, ref SessionRef, row *pyjson.Object, response any, outcome string, captured *pyjson.Object, settlement *turnSettlement) error {
	for attempt := 0; ; attempt++ {
		task := trackMutation(a, func(taskCtx context.Context) (any, error) {
			return nil, commitAndPublish(taskCtx, a, live, ref, row, response, outcome, captured, settlement)
		})
		_, err := task.wait()
		if err == nil {
			return nil
		}
		if !transientSettleFailure(err) || attempt >= len(settleRetryDelaysS) || settlement.Settled != nil {
			return err
		}
		delay := settleRetryDelaysS[attempt]
		slog.Warn(fmt.Sprintf("Settlement of admission %s deferred by storage (%s); retrying in %.1fs", rowString(row, "admission_id"), pyExceptionRepr(err), delay))
		timer := time.NewTimer(time.Duration(delay * float64(time.Second)))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// Publishes a committed outcome once, then releases its media and publishes the idle state
func publishTerminal(ctx context.Context, a *SessionAuthority, ref SessionRef, row, settled *pyjson.Object, response any, captured *pyjson.Object) error {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return &pyKeyError{ref.SessionID}
	}
	live.EventStream.Lock.Lock()
	err := publishCompletion(ctx, a, live, ref, row, settled, response, captured)
	live.EventStream.Lock.Unlock()
	if err != nil {
		return err
	}
	releaseSettledMedia(ctx, a, row)
	return a.publishPending(ctx, ref)
}
