package tuiserver

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Tour and preview action deadlines, the short probe until a session's client proved it answers
const (
	tourTimeoutS                  = 45.0
	tourProbeTimeoutS             = 10.0
	previewActionTimeoutS         = 45.0
	previewActionProbeTimeoutS    = 10.0
	tourBridgeUnavailableError    = "No Icebath Desktop window answered the tour request. The tour is driven by the desktop app's renderer, which updates separately from this backend, so an app build older than the tour tool has nothing listening. Update the Icebath Desktop app and start a new session. Do not retry tour in this session."
	previewActionUnavailableError = "No Icebath Desktop window answered the preview action request. The drive_preview / annotate_preview bridge is served by the desktop app's renderer, which updates separately from this backend, so an app build older than the tool has nothing listening. Update the Icebath Desktop app, open a page with open_preview, and try again in this session after a short cooldown."
)

// Tool results a bridge returns when no desktop renderer answered
var (
	tourBridgeUnavailable          = bridgeUnavailable(tourBridgeUnavailableError)
	previewActionBridgeUnavailable = bridgeUnavailable(previewActionUnavailableError)
	previewActionReprobeMu         sync.Mutex
	previewActionReprobe           = map[string]bool{}
	previewActionStateMu           sync.Mutex
	// Window after an unanswered probe during which preview actions fail fast
	previewActionReprobeCooldownS = 30.0
)

// json dumps of a failed bridge result
func bridgeUnavailable(message string) string {
	obj := pyjson.NewObject()
	obj.Set("success", false)
	obj.Set("error", message)
	encoded, err := pyjson.Dumps(obj)
	if err != nil {
		panic(err)
	}
	return encoded
}

// Tour tool callback bridged onto a tour server request, the short probe first and an unanswered bridge failing fast
func tourRequest(ctx context.Context, sid string, payload map[string]any) string {
	session := tuigateway.SessionRecord(sid)
	if session == nil {
		session = tuigateway.NewSession(nil)
	}
	state := session.Get("tour_bridge")
	if state == "unanswered" {
		return tourBridgeUnavailable
	}
	timeout := tourProbeTimeoutS
	if state == "answered" {
		timeout = tourTimeoutS
	}
	answer := askRequest(ctx, "tour", sid, copyDict(payload), &timeout)
	if answer != "" {
		session.Set("tour_bridge", "answered")
	} else if state != "answered" {
		session.Set("tour_bridge", "unanswered")
	}
	if answer == "" {
		return tourBridgeUnavailable
	}
	return answer
}

// Preview action callback bridged onto a preview.act request with a probe deadline and a cooldown gated reprobe
func previewActionRequest(ctx context.Context, sid string, payload map[string]any) string {
	previewActionStateMu.Lock()
	session := tuigateway.SessionRecord(sid)
	if session == nil {
		session = tuigateway.NewSession(nil)
	}
	state := session.Get("preview_action_bridge")
	now := pythread.Monotonic()
	retryAt, _ := session.Get("preview_action_bridge_retry_at").(float64)
	if state == "unanswered" && now < retryAt {
		previewActionStateMu.Unlock()
		return previewActionBridgeUnavailable
	}
	if state == "unanswered" {
		previewActionReprobeMu.Lock()
		if previewActionReprobe[sid] {
			previewActionReprobeMu.Unlock()
			previewActionStateMu.Unlock()
			return previewActionBridgeUnavailable
		}
		previewActionReprobe[sid] = true
		previewActionReprobeMu.Unlock()
		session.Set("preview_action_bridge_retry_at", now+previewActionReprobeCooldownS)
	}
	previewActionStateMu.Unlock()
	timeout := previewActionProbeTimeoutS
	if state == "answered" {
		timeout = previewActionTimeoutS
	}
	answer := func() string {
		defer func() {
			if state == "unanswered" {
				previewActionReprobeMu.Lock()
				delete(previewActionReprobe, sid)
				previewActionReprobeMu.Unlock()
			}
		}()
		return askRequest(ctx, "preview.act", sid, copyDict(payload), &timeout)
	}()
	previewActionStateMu.Lock()
	if answer != "" {
		session.Set("preview_action_bridge", "answered")
	} else if session.Get("preview_action_bridge") != "answered" {
		session.Set("preview_action_bridge", "unanswered")
		session.Set("preview_action_bridge_retry_at", pythread.Monotonic()+previewActionReprobeCooldownS)
	}
	previewActionStateMu.Unlock()
	if answer == "" {
		return previewActionBridgeUnavailable
	}
	return answer
}
