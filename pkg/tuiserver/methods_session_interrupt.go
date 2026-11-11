package tuiserver

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var sessionInterruptRegistry = &handlerRegistry{}

func init() {
	sessionInterruptRegistry.method("session.interrupt", sessionInterrupt)
	correctionMethod("session.steer", "steer", "queued", func(a any) bool {
		_, steers := a.(interface{ Steer(string) bool })
		return steers
	}, "agent does not support steer")
	correctionMethod("session.redirect", "redirect", "redirected", func(a any) bool {
		r, redirects := a.(interface {
			Redirect(string) bool
			SupportsActiveTurnRedirect() bool
		})
		return redirects && r.SupportsActiveTurnRedirect()
	}, "agent does not support active-turn redirect")
	sessionInterruptRegistry.install()
}

// Transport of the current request, stdio when none is bound
func callerTransport(ctx context.Context) tuigateway.Transport {
	if transport := tuigateway.CurrentTransport(ctx); transport != nil {
		return transport
	}
	return tuigateway.ModuleStdioTransport()
}

// Trimmed text of a param, the default for a missing or falsy value
func strParam(params map[string]any, key, def string) string {
	raw := params[key]
	if !truthy(raw) {
		raw = ""
	}
	if text := pystr.Strip(pyrepr.Str(raw)); text != "" {
		return text
	}
	return def
}

// Re-arms a wake lease held by the interrupt caller or a voice capture, each owner once
func resumeWakeAfterInterrupt(ctx context.Context) {
	voiceSidMu.Lock()
	voiceOwner := voiceWakeOwner
	voiceSidMu.Unlock()
	var seen []tuigateway.Transport
	for _, owner := range []tuigateway.Transport{callerTransport(ctx), voiceOwner} {
		if owner == nil || containsTransport(seen, owner) {
			continue
		}
		seen = append(seen, owner)
		wakeResumeIfOwner(owner, 15*time.Second, time.Second)
	}
}

func containsTransport(seen []tuigateway.Transport, owner tuigateway.Transport) bool {
	for _, item := range seen {
		if item == owner {
			return true
		}
	}
	return false
}

// Marks the running turn as touched by the user, popped by the turn's own finally
func noteUserInput(session *tuigateway.Session) {
	unlock := historyLockOf(session)
	defer unlock()
	if truthy(session.Get("running")) {
		session.Set("_turn_user_input", true)
	}
}

// Whether two values are the same Python object, maps compared by identity
func sameObject(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Kind() != vb.Kind() {
		return false
	}
	switch va.Kind() {
	case reflect.Map, reflect.Pointer, reflect.Slice:
		return va.UnsafePointer() == vb.UnsafePointer()
	}
	return a == b
}

// Clears a stopped turn whose compute host is gone, keeping a successor turn and one the supervisor still owes
func recoverUnreachableComputeHostTurn(ctx context.Context, sid string, session *tuigateway.Session, inflightAtInterrupt any) {
	pending := true
	if failure := catchPanic(func() {
		supervisor, err := getComputeHostSupervisor(ctx, nil)
		panicIf(err)
		pending = supervisor.HasPendingTurn(sid)
	}); failure != nil {
		logger().Warn(fmt.Sprintf("session.interrupt: compute-host pending-turn probe failed for %s: %s", sid, errorMessage(failure)))
		pending = true
	}
	func() {
		unlock := historyLockOf(session)
		defer unlock()
		session.Set("_turn_cancel_requested", true)
		session.Set("queued_prompt", nil)
		session.Pop("queued_prompts")
		generation, _ := pyIntOf(session.Get("_queued_prompt_generation"))
		session.Set("_queued_prompt_generation", generation+1)
		inflightNow := session.Get(inflightTurnKey)
		if !pending && truthy(session.Get("running")) && inflightNow != nil && sameObject(inflightNow, inflightAtInterrupt) {
			session.Set("running", false)
			clearInflightTurn(session)
		}
	}()
	clearPending(&sid)
	suppress(func() {
		announceCancelledGatewayApprovals(session, "interrupt", sid)
		tools.ResolveGatewayApproval(strOr(session.Get("session_key")), "deny", true, "", "")
	})
}

// Stops the running turn of a session, a hosted task mismatch leaving it running
func sessionInterrupt(ctx context.Context, rid any, params map[string]any) (out map[string]any) {
	ttsStreamStop(false)
	resumeWake := true
	defer func() {
		if resumeWake {
			if failure := catchPanic(func() { resumeWakeAfterInterrupt(ctx) }); failure != nil {
				logger().Debug("session.interrupt wake resume failed", "exc_info", errorMessage(failure))
			}
		}
	}()
	session, errResp := sessNowait(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	if expected := strParam(params, "expected_hosted_task_id", ""); expected != "" {
		unlock := historyLockOf(session)
		task, isTask := session.Get("_hosted_room_task").(map[string]any)
		matches := truthy(session.Get("running")) && isTask && task["task_id"] == expected
		unlock()
		if !matches {
			resumeWake = false
			return ok(rid, map[string]any{"status": "not_interrupted", "interrupted": false})
		}
	}
	sid := ""
	if truthy(params["session_id"]) {
		sid = pyrepr.Str(params["session_id"])
	}
	noteUserInput(session)
	if sessionUsesComputeHost(ctx, session, nil) {
		unlock := historyLockOf(session)
		inflightAtInterrupt := session.Get(inflightTurnKey)
		unlock()
		if failure := catchPanic(func() {
			interruptSessionTurn(ctx, sid, session, fmt.Sprintf("interrupt-%s", pyrepr.Str(rid)), false)
		}); failure != nil {
			logger().Warn(fmt.Sprintf("session.interrupt: compute-host interrupt failed for %s: %s", sid, errorMessage(failure)))
			recoverUnreachableComputeHostTurn(ctx, sid, session, inflightAtInterrupt)
		}
		return ok(rid, map[string]any{"status": "interrupted", "turn_isolation": true})
	}
	interruptSessionTurn(ctx, sid, session, "", false)
	unlock := historyLockOf(session)
	popped, _ := session.Pop("_active_turn_marker_key")
	unlock()
	activeMarkerKey := ""
	if truthy(popped) {
		activeMarkerKey = pyrepr.Str(popped)
	}
	retireTurnMarker(ctx, session, activeMarkerKey)
	return ok(rid, map[string]any{"status": "interrupted"})
}

// Runs steer or redirect on the live agent, recording an accepted correction on the turn and purging its queued copies
func applyCorrection(rid any, session *tuigateway.Session, verb, text, acceptedStatus string) map[string]any {
	var accepted bool
	if failure := catchPanic(func() {
		switch a := session.Get("agent").(type) {
		case interface{ Steer(string) bool }:
			if verb == "steer" {
				accepted = a.Steer(text)
				return
			}
			accepted = a.(interface{ Redirect(string) bool }).Redirect(text)
		case interface{ Redirect(string) bool }:
			accepted = a.Redirect(text)
		default:
			panic(fmt.Sprintf("'%s' object has no attribute '%s'", pyrepr.TypeName(a), verb))
		}
	}); failure != nil {
		return rpcErr(rid, 5000, verb+" failed: "+errorMessage(failure), nil)
	}
	if accepted {
		unlock := historyLockOf(session)
		recordInflightCorrection(session, text)
		dropQueuedDuplicatesOfInflightUser(session)
		session.Set("last_active", nowSeconds())
		unlock()
	}
	status := "rejected"
	if accepted {
		status = acceptedStatus
	}
	return ok(rid, map[string]any{"status": status, "text": text})
}

// Registers a steer or redirect RPC, text checked before the session and the agent gate answering 4010
func correctionMethod(name, verb, acceptedStatus string, supported func(any) bool, unsupported string) {
	sessionInterruptRegistry.method(name, func(ctx context.Context, rid any, params map[string]any) map[string]any {
		text := ""
		if raw := params["text"]; truthy(raw) {
			s, isText := raw.(string)
			if !isText {
				panic(&pyrepr.FormatError{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(raw))})
			}
			text = pystr.Strip(s)
		}
		if text == "" {
			return rpcErr(rid, 4002, "text is required", nil)
		}
		session, errResp := sessNowait(ctx, params, rid)
		if errResp != nil {
			return errResp
		}
		noteUserInput(session)
		a := session.Get("agent")
		if verb == "redirect" && a == nil && truthy(session.Get("running")) {
			enqueuePrompt(ctx, session, text, callerTransport(ctx), nil)
			session.Set("last_active", nowSeconds())
			return ok(rid, map[string]any{"status": "queued", "text": text})
		}
		if sessionCompressionInFlight(ctx, session) {
			enqueuePrompt(ctx, session, text, callerTransport(ctx), nil)
			session.Set("last_active", nowSeconds())
			return ok(rid, map[string]any{"status": "queued", "text": text})
		}
		if !supported(a) {
			return rpcErr(rid, 4010, unsupported, nil)
		}
		if verb == "steer" && !truthy(session.Get("running")) {
			return ok(rid, map[string]any{"status": "rejected", "text": text})
		}
		return applyCorrection(rid, session, verb, text, acceptedStatus)
	})
}
