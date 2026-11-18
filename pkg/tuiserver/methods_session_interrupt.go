package tuiserver

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tuihost"
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
func callerTransport(ctx context.Context) tuihost.Transport {
	if transport := tuihost.CurrentTransport(ctx); transport != nil {
		return transport
	}
	return tuihost.ModuleStdioTransport()
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
	var seen []tuihost.Transport
	for _, owner := range []tuihost.Transport{callerTransport(ctx), voiceOwner} {
		if owner == nil || containsTransport(seen, owner) {
			continue
		}
		seen = append(seen, owner)
		wakeResumeIfOwner(owner, 15*time.Second, time.Second)
	}
}

func containsTransport(seen []tuihost.Transport, owner tuihost.Transport) bool {
	for _, item := range seen {
		if item == owner {
			return true
		}
	}
	return false
}

// Marks the running turn as touched by the user, popped by the turn's own finally
func noteUserInput(session *tuihost.Session) {
	unlock := historyLockOf(session)
	defer unlock()
	if truthy(session.Get("running")) {
		session.Set("_turn_user_input", true)
	}
}

// Stops the running turn of a session, a hosted task mismatch leaving it running
func sessionInterrupt(ctx context.Context, rid any, params map[string]any) (out map[string]any) {
	ttsStreamStop(false)
	resumeWake := true
	defer func() {
		if resumeWake {
			if failure := catchPanic(func() { resumeWakeAfterInterrupt(ctx) }); failure != nil {
				pkgLog().Debug("session.interrupt wake resume failed", "exc_info", errorMessage(failure))
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
	interruptSessionTurn(ctx, sid, session, false)
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
func applyCorrection(rid any, session *tuihost.Session, verb, text, acceptedStatus string) map[string]any {
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
