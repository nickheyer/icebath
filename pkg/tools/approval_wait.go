package tools

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// Outcome of one blocking gateway approval round trip
type gatewayDecision struct {
	Resolved     bool
	Choice       string
	Reason       string
	NotifyFailed bool
	Cancelled    string
	Coalesced    bool
}

// Waits on the entry until it fires, the turn is interrupted, or the timeout elapses
func pollEvent(ctx context.Context, env *Env, entry *ApprovalEntry, sessionKey, interruptLog string, timeout time.Duration) string {
	if timeout < 0 {
		timeout = 0
	}
	deadline := time.Now().Add(timeout)
	heartbeat := activityHeartbeat(env, "waiting for user approval")
	release := humanWaitWindow(env, sessionKey)
	defer release()
	for {
		if ctx.Err() != nil {
			envLog(env).Info(interruptLog, "session", sessionKey)
			return "interrupted"
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "timeout"
		}
		slice := time.Second
		if remaining < slice {
			slice = remaining
		}
		timer := time.NewTimer(slice)
		select {
		case <-entry.Done():
			timer.Stop()
			return "set"
		case <-ctx.Done():
			timer.Stop()
			envLog(env).Info(interruptLog, "session", sessionKey)
			return "interrupted"
		case <-timer.C:
			heartbeat()
		}
	}
}

// Why the wait ended with nobody answering, empty for a real answer or a plain timeout
func cancelCause(ctx context.Context, state string, entry *ApprovalEntry) string {
	if state == "interrupted" {
		if cause := context.Cause(ctx); cause != nil && cause != context.Canceled && cause != context.DeadlineExceeded {
			return cause.Error()
		}
		return "turn interrupted"
	}
	if state == "set" && entry.Result == "" {
		if entry.Cancelled != "" {
			return entry.Cancelled
		}
		return "the turn ended before the prompt was answered"
	}
	return ""
}

// Observer facing outcome of a wait, the choice else cancelled or timeout
func hookChoice(resolved bool, choice, cancelled string) string {
	if cancelled != "" {
		return "cancelled"
	}
	if resolved && choice != "" {
		return choice
	}
	return "timeout"
}

// Fires the post hook and builds the decision
func finishDecision(env *Env, payload map[string]any, resolved bool, choice, reason, cancelled string, coalesced bool) gatewayDecision {
	post := copyMap(payload)
	post["choice"] = hookChoice(resolved, choice, cancelled)
	if cancelled != "" {
		post["cancelled"] = cancelled
	}
	if coalesced {
		post["coalesced"] = true
	}
	fireApprovalHook(env, "post_approval_response", post)
	return gatewayDecision{Resolved: resolved, Choice: choice, Reason: reason, Cancelled: cancelled, Coalesced: coalesced}
}

// Timeout stamped on a published entry
func entryTimeout(entry *ApprovalEntry, fallback time.Duration) time.Duration {
	switch v := entry.Data["timeout_s"].(type) {
	case int:
		return time.Duration(v) * time.Second
	case float64:
		return time.Duration(v * float64(time.Second))
	}
	return fallback
}

// Waits on an identical pending approval and adopts its decision, ok false means prompt afresh
func awaitCoalescedLeader(ctx context.Context, env *Env, sessionKey string, leader *ApprovalEntry, payload map[string]any) (gatewayDecision, bool) {
	pre := copyMap(payload)
	pre["coalesced"] = true
	fireApprovalHook(env, "pre_approval_request", pre)
	state := pollEvent(ctx, env, leader, sessionKey, "Coalesced approval wait interrupted — returning deny for session",
		entryTimeout(leader, time.Duration(getApprovalTimeout(env))*time.Second))
	approvalState.Lock()
	result, expired, reason := leader.Result, leader.Expired, leader.Reason
	approvalState.Unlock()
	if state == "set" && result == "" && expired {
		state = "timeout"
	}
	cancelled := cancelCause(ctx, state, leader)
	var choice string
	var resolved bool
	switch state {
	case "interrupted":
		choice, resolved = "deny", true
	case "timeout":
		choice, resolved = "", false
	default:
		choice = result
		resolved = choice != ""
	}
	if choice == "once" {
		return gatewayDecision{}, false
	}
	return finishDecision(env, payload, resolved, choice, reason, cancelled, true), true
}

// Enqueues the approval, notifies the user, and blocks until resolved, withdrawn, or timed out
func awaitGatewayDecision(ctx context.Context, env *Env, sessionKey string, notify GatewayNotifyFunc, approvalData map[string]any, surface string) gatewayDecision {
	primaryKey, _ := approvalData["pattern_key"].(string)
	if surface == "gateway" {
		surface = gatewayApprovalSurface(env)
	}
	command, _ := approvalData["command"].(string)
	description, _ := approvalData["description"].(string)
	keys := stringList(approvalData["pattern_keys"])
	payloadKeys := keys
	if approvalData["pattern_keys"] == nil {
		payloadKeys = []string{primaryKey}
	}
	payload := map[string]any{"command": command, "description": description, "pattern_key": primaryKey,
		"pattern_keys": append([]string{}, payloadKeys...), "session_key": sessionKey, "surface": surface}
	entry := newApprovalEntry(approvalData)
	timeout := time.Duration(getApprovalTimeout(env)) * time.Second
	entry.Data["timeout_s"] = int(timeout / time.Second)
	slot := preparedApprovalSlot(ctx)

	for {
		approvalState.Lock()
		var leader *ApprovalEntry
		if slot == nil || !slot.Preparing() {
			for _, e := range approvalState.gatewayQueues[sessionKey] {
				if e.Data["command"] == approvalData["command"] && sameStrings(stringList(e.Data["pattern_keys"]), keys) {
					leader = e
					break
				}
			}
		}
		if leader == nil {
			if slot != nil {
				slot.RegisterPreparedApproval(sessionKey, entry)
			}
			approvalState.gatewayQueues[sessionKey] = append(approvalState.gatewayQueues[sessionKey], entry)
			approvalState.Unlock()
			break
		}
		approvalState.Unlock()
		if adopted, ok := awaitCoalescedLeader(ctx, env, sessionKey, leader, payload); ok {
			return adopted
		}
	}

	dropEntry := func(state string) string {
		approvalState.Lock()
		choice := entry.Result
		queue := approvalState.gatewayQueues[sessionKey]
		rest := queue[:0:0]
		for _, e := range queue {
			if e != entry {
				rest = append(rest, e)
			}
		}
		if len(rest) == 0 {
			delete(approvalState.gatewayQueues, sessionKey)
		} else {
			approvalState.gatewayQueues[sessionKey] = rest
		}
		settle, observers := entry.Settle, entry.Observers
		entry.Settle, entry.Observers = nil, nil
		if choice == "" && (state == "timeout" || state == "notify_failed") {
			entry.Expired = state == "timeout"
			if state == "notify_failed" {
				entry.Cancelled = "the approval prompt could not be delivered"
			}
			entry.set()
		}
		approvalState.Unlock()
		if settle != nil || len(observers) > 0 {
			var reason string
			switch {
			case state == "timeout" && choice != "":
				reason = "resolved"
			case state == "set" && choice != "":
				reason = "resolved"
			case state == "set":
				reason = "session_closed"
			default:
				reason = state
			}
			callbacks := observers
			if settle != nil {
				callbacks = append([]func(string){settle}, observers...)
			}
			for _, cb := range callbacks {
				func() {
					defer func() {
						if r := recover(); r != nil {
							envLog(env).Debug("approval settle hook failed", "error", r)
						}
					}()
					cb(reason)
				}()
			}
		}
		return choice
	}

	human := humanInputRequest(env, "approval", command, sessionKey)
	fireApprovalHook(env, "pre_approval_request", payload)
	if err := notify(copyMap(entry.Data)); err != nil {
		envLog(env).Warn("Gateway approval notify failed", "error", err)
		dropEntry("notify_failed")
		post := copyMap(payload)
		post["choice"] = "notify_failed"
		fireApprovalHook(env, "post_approval_response", post)
		human.finish(env, "notify_failed")
		return gatewayDecision{NotifyFailed: true}
	}
	if slot != nil {
		slot.ApprovalPublished()
	}
	state := pollEvent(ctx, env, entry, sessionKey, "Approval wait interrupted — returning deny for session", timeout)
	cancelled := cancelCause(ctx, state, entry)
	if state == "interrupted" {
		approvalState.Lock()
		entry.Cancelled = cancelled
		approvalState.Unlock()
		entry.set()
	}
	choice := dropEntry(state)
	if state == "interrupted" {
		choice = "deny"
	}
	resolved := state != "timeout" || choice != ""
	human.finish(env, hookChoice(resolved, choice, cancelled))
	return finishDecision(env, payload, resolved, choice, entry.Reason, cancelled, false)
}

// Strings of a list value in a payload map
func stringList(v any) []string {
	switch items := v.(type) {
	case []string:
		return append([]string{}, items...)
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Reports whether two string lists are equal in order
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Per session accounting of time verifiably blocked on a human prompt
type humanWaitState struct {
	pending          int
	windowStarted    time.Time
	windowOpen       bool
	completedSeconds float64
}

var humanWait = struct {
	sync.Mutex
	states map[string]*humanWaitState
	order  []string
}{states: map[string]*humanWaitState{}}

const humanWaitMaxSessions = 256

// HumanWaitMarginSeconds is added above approvals.timeout when clamping a window's contribution
const HumanWaitMarginSeconds = 60.0

// Max seconds a single window may contribute, approvals.timeout plus the margin
func humanWaitCeiling(env *Env) float64 {
	ceiling := float64(getApprovalTimeout(env)) + HumanWaitMarginSeconds
	if ceiling > MaxSafeTimeoutSeconds {
		return MaxSafeTimeoutSeconds
	}
	return ceiling
}

// Seconds an open window contributes, floored at zero and capped at the ceiling
func clampedWindowSeconds(started, now time.Time, ceiling float64) float64 {
	elapsed := now.Sub(started).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > ceiling {
		return ceiling
	}
	return elapsed
}

// Wait state of a session, evicting idle entries past the cap, caller holds the lock
func humanWaitStateFor(sessionKey string) *humanWaitState {
	state := humanWait.states[sessionKey]
	if state != nil {
		return state
	}
	kept := humanWait.order[:0]
	for _, key := range humanWait.order {
		if len(humanWait.states) < humanWaitMaxSessions {
			kept = append(kept, key)
			continue
		}
		if humanWait.states[key].pending == 0 {
			delete(humanWait.states, key)
			continue
		}
		kept = append(kept, key)
	}
	humanWait.order = kept
	state = &humanWaitState{}
	humanWait.states[sessionKey] = state
	humanWait.order = append(humanWait.order, sessionKey)
	return state
}

// Session key for wait accounting, the current session when none is given
func resolveWaitKey(env *Env, sessionKey string) string {
	if sessionKey != "" {
		return sessionKey
	}
	return getCurrentSessionKey(env, "default")
}

// Callable that pings the host activity listener at most every ten seconds while a wait is parked
func activityHeartbeat(env *Env, label string) func() {
	start := time.Now()
	last := start
	return func() {
		now := time.Now()
		if now.Sub(last) < 10*time.Second {
			return
		}
		last = now
		env.Emit(Event{Kind: "activity", Text: fmt.Sprintf("%s (%ds elapsed)", label, int(now.Sub(start).Seconds()))})
	}
}

// Opens a human wait window for the session and returns the closer that accrues the clamped time
func humanWaitWindow(env *Env, sessionKey string) func() {
	key := resolveWaitKey(env, sessionKey)
	now := time.Now()
	humanWait.Lock()
	state := humanWaitStateFor(key)
	if state.pending == 0 {
		state.windowStarted = now
		state.windowOpen = true
	}
	state.pending++
	humanWait.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			now := time.Now()
			ceiling := humanWaitCeiling(env)
			humanWait.Lock()
			defer humanWait.Unlock()
			state := humanWait.states[key]
			if state == nil {
				return
			}
			state.pending--
			if state.pending == 0 {
				if state.windowOpen {
					state.completedSeconds += clampedWindowSeconds(state.windowStarted, now, ceiling)
				}
				state.windowOpen = false
			}
		})
	}
}

// HumanWaitSeconds returns the total human wait recorded for the session including the open window
func HumanWaitSeconds(env *Env, sessionKey string) float64 {
	key := resolveWaitKey(env, sessionKey)
	now := time.Now()
	ceiling := humanWaitCeiling(env)
	humanWait.Lock()
	defer humanWait.Unlock()
	state := humanWait.states[key]
	if state == nil {
		return 0
	}
	total := state.completedSeconds
	if state.windowOpen {
		total += clampedWindowSeconds(state.windowStarted, now, ceiling)
	}
	return total
}

// Handle of one human input observer request, finish fires the resolved hook once
type humanInputHandle struct {
	payload map[string]any
	once    sync.Once
}

// Session identity carried by human input hook payloads
func humanInputIdentity(env *Env) map[string]any {
	platform := os.Getenv("ICEBATH_PLATFORM")
	if platform == "" {
		platform = getSessionPlatform(env)
	}
	if platform == "" {
		platform = sessionValue(env, "source", "ICEBATH_SESSION_SOURCE")
	}
	if platform == "" {
		platform = "cli"
	}
	return map[string]any{"session_id": sessionValue(env, "id", "ICEBATH_SESSION_ID"),
		"session_key": getCurrentSessionKey(env, ""), "platform": platform}
}

// Fires the request hook for a human prompt and returns the handle to resolve it
func humanInputRequest(env *Env, kind, prompt, sessionKey string) *humanInputHandle {
	payload := map[string]any{"kind": kind, "request_id": newRequestID()}
	for k, v := range humanInputIdentity(env) {
		payload[k] = v
	}
	payload["prompt"] = redactApprovalDisplay(env, prompt, true)
	if sessionKey != "" {
		payload["session_key"] = sessionKey
	}
	fireApprovalHook(env, "on_human_input_request", payload)
	return &humanInputHandle{payload: payload}
}

// Fires the resolved hook exactly once with the outcome, error when none was set
func (h *humanInputHandle) finish(env *Env, outcome string) {
	h.once.Do(func() {
		if outcome == "" {
			outcome = "error"
		}
		resolved := copyMap(h.payload)
		resolved["outcome"] = outcome
		fireApprovalHook(env, "on_human_input_resolved", resolved)
	})
}
