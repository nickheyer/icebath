package tuiserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Applies a model switch queued while a turn ran, warning instead of switching when it needs confirmation
func applyPendingModelSwitch(ctx context.Context, sid string, session *tuigateway.Session) {
	raw, _ := session.Pop("pending_model_switch")
	pending, _ := raw.(map[string]any)
	if len(pending) == 0 || sessionAgent(session) == nil {
		return
	}
	model := pending["display_model"]
	if !truthy(model) {
		model = pending["raw"]
	}
	text := ""
	if failure := catchPanic(func() {
		result, err := applyModelSwitch(ctx, sid, session, strOr(pending["raw"]), modelSwitchOptions{confirmExpensiveModel: truthy(pending["confirm_expensive_model"]),
			pinSessionOverride: true, countSwitch: true})
		panicIf(err)
		if !truthy(result["confirm_required"]) {
			return
		}
		detail := strOr(orEmpty(result["confirm_message"]))
		if detail == "" {
			detail = strOr(orEmpty(result["warning"]))
		}
		logger().Warn(fmt.Sprintf("Queued model switch to %s dropped for session %s: %s", strOr(model), sid, strings.SplitN(detail, "\n", 2)[0]))
		current := sessionAgent(session).Model()
		if current == "" {
			current = "the current model"
		}
		text = fmt.Sprintf("Stayed on %s: switching to %s needs confirmation. Pick it again to confirm.", current, strOr(model))
	}); failure != nil {
		logger().Warn(fmt.Sprintf("Queued model switch to %s failed for session %s: %s", strOr(model), sid, errorMessage(failure)))
		text = "Could not switch model: " + errorMessage(failure)
	}
	if text == "" {
		return
	}
	tuigateway.Emit(ctx, "notification.show", sid, map[string]any{"text": strings.TrimRight(text, " \t\n\r\v\f"), "level": "warn",
		"kind": "ttl", "ttl_ms": 12000, "key": "model_switch.dropped", "id": "model_switch.dropped"})
	emitSessionInfoForSession(ctx, sid, session)
}

// Compression skipped because a concurrent compression lock row is held
type CompressionLockHeld struct{ Holder string }

func (e *CompressionLockHeld) Error() string {
	holder := e.Holder
	if holder == "" {
		holder = "unknown"
	}
	return "Compression lock held: " + holder
}

// Manual compaction core of every compress route, replaced only where a hermes case replaces compress_now
var compressNow = agent.CompressNow

// Manual compression refused because a turn already holds the session
type CompressionBusy struct{ Msg string }

func (e *CompressionBusy) Error() string { return e.Msg }

// Holds the session busy for a whole manual compaction, draining the queued prompt on release
func manualCompressTurn(ctx context.Context, sid string, session *tuigateway.Session, body func() error) error {
	unlock := lockHistory(session)
	if truthy(session.Get("running")) {
		compressing := truthy(session.Get("_manual_compress_active"))
		unlock()
		return &CompressionBusy{busyMessage("compress", compressing)}
	}
	session.Set("running", true)
	session.Set("_manual_compress_active", true)
	unlock()
	defer func() {
		unlock := lockHistory(session)
		session.Pop("_manual_compress_active")
		session.Set("running", false)
		unlock()
		emitSessionInfoForSession(ctx, sid, session)
		drainQueuedPrompt(ctx, "__compress__", sid, session)
	}()
	return body()
}

// Single choke point of every manual compress route, the removed count and usage after the commit
func compressSessionHistory(ctx context.Context, session *tuigateway.Session, focusTopic string, beforeMessages []any, historyVersion *int64) (int, map[string]any, error) {
	a := sessionAgent(session)
	if beforeMessages == nil || historyVersion == nil {
		unlock := lockHistory(session)
		beforeMessages = historyList(session.Get("history"))
		v := intOr(session.Get("history_version"))
		historyVersion = &v
		unlock()
	}
	if len(beforeMessages) < agent.MinCompressMessages {
		return 0, getUsage(ctx, a), nil
	}
	request := agent.ParseCompressArgs(focusTopic)
	if request.Aggressive {
		return 0, nil, &attachmentError{agent.AggressiveUnsupported}
	}
	cwd := sessionCwd(ctx, session)
	scoped, env := setSessionContext(ctx, strOr(session.Get("session_key")), &cwd, "")
	taskID := strOr(session.Get("session_key"))
	if taskID == "" {
		taskID = "default"
	}
	result, err := func() (agent.CompressResult, error) {
		defer clearSessionContext(env)
		return compressNow(scoped, a, historyAsMessages(beforeMessages), request, agent.CompressNowOptions{TaskID: taskID,
			SnapshotIsCurrent: func() (bool, error) {
				unlock := lockHistory(session)
				defer unlock()
				return intOr(session.Get("history_version")) == *historyVersion, nil
			}})
	}()
	if err != nil {
		return 0, nil, err
	}
	switch result.Status {
	case "preview":
		return 0, getUsage(ctx, a), nil
	case "lock_skipped":
		holder := ""
		if result.LockHolder != nil {
			holder = *result.LockHolder
		}
		return 0, nil, &CompressionLockHeld{holder}
	case "compressed":
	default:
		return 0, getUsage(ctx, a), nil
	}
	unlock := lockHistory(session)
	if intOr(session.Get("history_version")) != *historyVersion {
		unlock()
		agent.FinalizeContextEngineCompressionNotification(a, false)
		return 0, getUsage(ctx, a), nil
	}
	session.Set("history", messagesAsHistory(result.AfterMessages))
	session.Set("history_version", *historyVersion+1)
	unlock()
	return result.Removed(), getUsage(ctx, a), nil
}

// Re anchors the session key on a compression rotated agent session id with its lease, approvals and yolo state
func syncSessionKeyAfterCompress(ctx context.Context, sid string, session *tuigateway.Session, clearPendingTitle, restartWorker bool) {
	a := sessionAgent(session)
	newSessionID := ""
	if a != nil {
		newSessionID = a.SessionID()
	}
	oldKey := strOr(session.Get("session_key"))
	if newSessionID == "" || newSessionID == oldKey {
		return
	}
	if !transferActiveSessionSlot(ctx, sid, session, newSessionID) {
		logger().Warn(fmt.Sprintf("Compression session lease did not re-anchor: sid=%s old_session_id=%s new_session_id=%s", sid, oldKey, newSessionID))
	}
	session.Set("session_key", newSessionID)
	suppress(func() {
		suppress(func() { tools.UnregisterGatewayNotify(oldKey) })
		tools.TransferSessionYolo(oldKey, newSessionID)
		suppress(func() {
			tools.RegisterGatewayNotify(newSessionID, func(data map[string]any) error {
				emitApprovalRequest(ctx, sid, data)
				return nil
			})
		})
	})
	session.Set("_queued_prompt_generation", intOr(session.Get("_queued_prompt_generation"))+1)
	if clearPendingTitle {
		session.Set("pending_title", nil)
	}
	if restartWorker {
		suppress(func() { restartSlashWorker(ctx, sid, session) })
	}
}
