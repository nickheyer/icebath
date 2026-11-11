package tuiserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// turn.start frame of an isolated turn carrying the session state the compute host child rebuilds
func computeHostTurnFrame(ctx context.Context, rid any, sid string, session *tuigateway.Session, text any, imagePaths []string, queuedPromptGeneration *int64, displayKind string, displayMetadata map[string]any) map[string]any {
	unlock := historyLockOf(session)
	history := append([]any{}, historyList(session.Get("history"))...)
	historyVersion := intOr(session.Get("history_version"))
	var attached []any
	if imagePaths != nil {
		for _, p := range imagePaths {
			attached = append(attached, p)
		}
	} else {
		attached = append(attached, historyList(session.Get("attached_images"))...)
	}
	unlock()
	if attached == nil {
		attached = []any{}
	}
	sessionKey := strOr(session.Get("session_key"))
	if sessionKey == "" {
		sessionKey = sid
	}
	cols := intOr(session.Get("cols"))
	if cols == 0 {
		cols = 80
	}
	var queued any
	if queuedPromptGeneration != nil {
		queued = *queuedPromptGeneration
	}
	var authUserID any
	if owner := tuigateway.SessionAuthUserID(session); owner != nil {
		authUserID = *owner
	}
	frame := map[string]any{"type": "turn.start", "sid": sid, "request_id": rid, "session_key": sessionKey, "text": text,
		"history": history, "history_version": historyVersion, "cols": cols, "cwd": sessionCwd(ctx, session),
		"explicit_cwd": truthy(session.Get("explicit_cwd")), "context_cwd_is_launch_artifact": contextCwdIsLaunchArtifact(session),
		"profile_home": strOr(session.Get("profile_home")), "model_override": session.Get("model_override"),
		"pending_model_switch": session.Get("pending_model_switch"), "reasoning_config_override": session.Get("create_reasoning_override"),
		"service_tier_override": session.Get("create_service_tier_override"), "source": sessionSource(session),
		"attached_images": attached, "auth_user_id": authUserID, "queued_prompt_generation": queued,
		"active_session_lease": activeSessionLeaseVouch(session)}
	if displayKind != "" {
		frame["display_kind"] = displayKind
	}
	if len(displayMetadata) > 0 {
		frame["display_metadata"] = displayMetadata
	}
	return frame
}

// Lease id and session id of the real registry lease held for the current stored id, nil otherwise
func activeSessionLeaseVouch(session *tuigateway.Session) any {
	lease, _ := session.Get("active_session_lease").(*activesessions.Lease)
	if lease == nil || lease.Released() || !lease.Enabled {
		return nil
	}
	if lease.SessionID() != strOr(session.Get("session_key")) {
		return nil
	}
	return map[string]any{"lease_id": lease.LeaseID, "session_id": lease.SessionID()}
}

// Session info of a session whose agent lives in the compute host
func computeHostSessionInfo(ctx context.Context, session *tuigateway.Session) map[string]any {
	return sessionInfo(ctx, sessionAgent(session), session)
}

// Adopts a host frame's rotated session key and history version, the caller holding the history lock
func computeHostAdoptFrameMeta(ctx context.Context, session *tuigateway.Session, frame map[string]any) {
	newKey := strOr(frame["session_key"])
	if newKey != "" && newKey != strOr(session.Get("session_key")) {
		if !transferActiveSessionSlot(ctx, strOr(frame["sid"]), session, newKey) {
			logger().Warn(fmt.Sprintf("Compression session lease did not re-anchor: sid=%s old_session_id=%s new_session_id=%s",
				strOr(frame["sid"]), strOr(session.Get("session_key")), newKey))
		}
		session.Set("session_key", newKey)
	}
	if frame["history_version"] != nil {
		suppress(func() {
			incoming, err := pyIntOf(frame["history_version"])
			if err != nil {
				panic(err)
			}
			session.Set("history_version", max(intOr(session.Get("history_version")), int64(incoming)))
		})
	}
}

// Withdraws a session's open requests of one method here and in the compute host child that minted them
func cancelSessionRequests(ctx context.Context, sid, reason, method string) {
	tuigateway.Cancel(&sid, reason, &method)
	session := tuigateway.SessionRecord(sid)
	if session == nil {
		return
	}
	unlock := historyLockOf(session)
	mirrored, isDict := session.Get("_compute_host_open_request").(map[string]any)
	hosted := isDict && mirrored["method"] == method
	unlock()
	if !hosted || !sessionUsesComputeHost(ctx, session, nil) {
		return
	}
	sup, err := getComputeHostSupervisor(ctx, nil)
	if err == nil {
		_, err = sup.Respond(sid, objectOf(map[string]any{"cancel": map[string]any{"method": method, "reason": reason}}), 15*time.Second)
	}
	if err != nil {
		logger().Warn(fmt.Sprintf("compute-host request cancel failed sid=%s method=%s", sid, method), "error", err)
	}
}

// Proxies a batch clarify lock into the child owning the request, nil when no child owns it
func lockComputeHostClarify(ctx context.Context, rid any, requestID, questionID string, answer any) map[string]any {
	sid, session, found := computeHostRequestSession(requestID)
	if !found || !sessionUsesComputeHost(ctx, session, nil) {
		return nil
	}
	sup, err := getComputeHostSupervisor(ctx, nil)
	var ackObj *pyjson.Object
	if err == nil {
		ackObj, err = sup.Respond(sid, objectOf(map[string]any{"lock": map[string]any{"request_id": requestID, "question_id": questionID, "answer": answer}}), 15*time.Second)
	}
	if err != nil {
		return rpcErr(rid, 5019, fmt.Sprintf("compute-host clarify lock failed: %s", err), nil)
	}
	ack := plainObject(ackObj)
	if ack["type"] == "respond.error" {
		message := strOr(ack["message"])
		if message == "" {
			message = "compute-host clarify lock failed"
		}
		return rpcErr(rid, 5019, message, nil)
	}
	response, isDict := ack["response"].(map[string]any)
	if !isDict {
		return rpcErr(rid, 5019, "compute-host clarify lock returned an invalid response", nil)
	}
	if failure, present := response["error"]; present {
		errObj, _ := failure.(map[string]any)
		code, _ := pyIntOf(errObj["code"])
		if code == 0 {
			code = 5000
		}
		message := strOr(errObj["message"])
		if message == "" {
			message = "clarify lock failed"
		}
		return rpcErr(rid, code, message, nil)
	}
	result, _ := response["result"].(map[string]any)
	if result == nil {
		result = map[string]any{}
	}
	unlock := historyLockOf(session)
	if openRequestMatches(session, requestID) {
		mirrored, _ := session.Get("_compute_host_open_request").(map[string]any)
		remaining, isList := result["remaining"].([]any)
		if result["status"] == "expired" || (isList && len(remaining) == 0) {
			session.Pop("_compute_host_open_request")
		} else if params, ok := mirrored["params"].(map[string]any); ok {
			answers := map[string]any{}
			if prior, ok := params["answers"].(map[string]any); ok {
				for k, v := range prior {
					answers[k] = v
				}
			}
			answers[questionID] = answer
			params["answers"] = answers
		}
	}
	unlock()
	return ok(rid, result)
}

// Mirrors host owned session metadata so UI reads never build a second agent
func applyComputeHostMetadataMirror(ctx context.Context, session *tuigateway.Session, frame map[string]any) {
	if frame == nil {
		return
	}
	unlock := historyLockOf(session)
	computeHostAdoptFrameMeta(ctx, session, frame)
	if frame["message_count"] != nil {
		suppress(func() {
			count, err := pyIntOf(frame["message_count"])
			if err != nil {
				panic(err)
			}
			session.Set("_metadata_message_count", count)
		})
	}
	unlock()
	if info, isDict := frame["session_info"].(map[string]any); isDict {
		merged := map[string]any{}
		for k, v := range metadataMirror(session) {
			merged[k] = v
		}
		for k, v := range info {
			merged[k] = v
		}
		session.Set("_metadata_mirror", merged)
		session.Set("_metadata_mirror_updated_at", nowSeconds())
	}
}

// Settles an isolated turn, adopting its metadata, releasing a deferred lease and draining the queue
func onComputeHostTurnDone(ctx context.Context, rid any, sid string, session *tuigateway.Session, frame map[string]any) {
	unlock := historyLockOf(session)
	computeHostAdoptFrameMeta(ctx, session, frame)
	session.Set("running", false)
	session.Set("last_active", nowSeconds())
	clearInflightTurn(session)
	session.Pop("_compute_host_open_request")
	unlock()
	if frame["type"] != "turn.error" {
		session.Pop("pending_model_switch")
	} else {
		message := strOr(frame["message"])
		if message == "" {
			message = "compute host turn failed"
		}
		tuigateway.Emit(ctx, "message.complete", sid, map[string]any{"text": "Error: " + message, "status": "error"})
	}
	applyComputeHostMetadataMirror(ctx, session, frame)
	releaseDeferredActiveSessionLease(session)
	info := computeHostSessionInfo(ctx, session)
	if !truthy(frame["session_info_emitted"]) {
		tuigateway.Emit(ctx, "session.info", sid, info)
	}
	drainQueuedPrompt(ctx, rid, sid, session)
}

// Dispatches a turn to the compute host child, a 5019 error when the dispatch fails so the caller can fail open
func submitPromptToComputeHost(ctx context.Context, rid any, sid string, session *tuigateway.Session, text any, queuedPromptGeneration *int64, imagePaths []string, displayKind string, displayMetadata map[string]any) map[string]any {
	cfg := loadDashboardProcessIsolationConfig(ctx, nil)
	frame := computeHostTurnFrame(ctx, rid, sid, session, text, imagePaths, queuedPromptGeneration, displayKind, displayMetadata)
	turnID := strings.ReplaceAll(uuid.NewString(), "-", "")
	frame["turn_id"], frame["request_id"] = turnID, turnID
	unlock := historyLockOf(session)
	session.Set("_compute_host_turn_id", turnID)
	session.Pop("_compute_host_activity_ns")
	unlock()
	complete := func(doneObj *pyjson.Object) {
		done := plainObject(doneObj)
		if done["reason"] == "send_failed" {
			return
		}
		unlock := historyLockOf(session)
		if session.Get("_compute_host_turn_id") != turnID {
			unlock()
			return
		}
		session.Pop("_compute_host_turn_id")
		session.Pop("_compute_host_activity_ns")
		unlock()
		onComputeHostTurnDone(ctx, rid, sid, session, done)
	}
	sup, err := getComputeHostSupervisor(ctx, cfg)
	if err == nil {
		_, err = sup.SubmitTurn(objectOf(frame), complete)
	}
	if err != nil {
		unlock := historyLockOf(session)
		if session.Get("_compute_host_turn_id") == turnID {
			session.Pop("_compute_host_turn_id")
			session.Pop("_compute_host_activity_ns")
		}
		unlock()
		return rpcErr(rid, 5019, fmt.Sprintf("compute-host dispatch failed: %s", err), nil)
	}
	unlock = historyLockOf(session)
	session.Set("_compute_host_active", true)
	if imagePaths == nil {
		session.Set("attached_images", []any{})
	}
	unlock()
	return ok(rid, map[string]any{"status": "streaming", "turn_isolation": true})
}

// Sends a control frame for one session to the compute host and returns its ack
func sendComputeHostControl(ctx context.Context, sid, routeName, command string, payload map[string]any, wait bool, timeout float64, onLateAck func(*pyjson.Object)) (*pyjson.Object, error) {
	frame := map[string]any{}
	for k, v := range payload {
		frame[k] = v
	}
	if _, present := frame["type"]; !present {
		frame["type"] = "control"
	}
	if _, present := frame["command"]; !present {
		frame["command"] = command
	}
	sup, err := getComputeHostSupervisor(ctx, nil)
	if err != nil {
		return nil, err
	}
	return sup.Control(sid, routeName, objectOf(frame), wait, time.Duration(timeout*float64(time.Second)), onLateAck)
}

// Error response of a control.error or error ack, nil otherwise
func computeHostAckError(rid any, ack map[string]any, code int, def string) map[string]any {
	if ack["type"] == "control.error" || ack["type"] == "error" {
		message := strOr(ack["message"])
		if message == "" {
			message = def
		}
		return rpcErr(rid, code, message, nil)
	}
	return nil
}

// RPC wait of a compute host compress, the compression ceiling plus slack capped under the desktop deadline
func computeHostCompressWaitSeconds(ctx context.Context, cfg map[string]any) float64 {
	var compression map[string]any
	suppress(func() {
		if cfg == nil {
			cfg = loadCfg(ctx)
		}
		compression, _ = cfg["compression"].(map[string]any)
	})
	if compression == nil {
		compression = map[string]any{}
	}
	_, ceiling := agent.ResolveContextCompressionTimeouts(ctx, compression)
	return min(max(ceiling+30.0, 120.0), computeHostCompressWaitCapSecs)
}

// Adopts a compress ack that arrived after its RPC answered pending, the client's only completion signal
func adoptLateComputeHostCompressAck(ctx context.Context, sid string, session *tuigateway.Session, ack map[string]any, routeName string) {
	sessionsLock.Lock()
	live := tuigateway.SessionRecord(sid) == session
	sessionsLock.Unlock()
	if !live {
		return
	}
	if ack == nil || ack["type"] == "control.error" || ack["type"] == "error" {
		message := strOr(ack["message"])
		if message == "" {
			message = "compute-host " + routeName + " failed"
		}
		tuigateway.Emit(ctx, "error", sid, map[string]any{"message": "compression failed: " + message})
		statusUpdate(ctx, sid, "ready", nil)
		return
	}
	applyComputeHostMetadataMirror(ctx, session, ack)
	tuigateway.Emit(ctx, "session.info", sid, computeHostSessionInfo(ctx, session))
	done := "✓ Context compression complete"
	statusUpdate(ctx, sid, "compacted", &done)
}

// Plain nested map of an ordered JSON object, empty for nil
func plainObject(o *pyjson.Object) map[string]any {
	if o == nil {
		return map[string]any{}
	}
	m, _ := pyjson.Plain(o).(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
