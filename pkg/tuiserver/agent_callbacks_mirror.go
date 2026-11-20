package tuiserver

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Profile home and child stored id a mirror and its liveness entry are keyed on, an empty home for the launch profile
type childMirrorKey struct{ home, child string }

// Live mirror state of one child watch window
type childMirror struct {
	seq      int
	openTool map[string]any
	started  bool
}

// Child watch mirrors and the child runs in flight by profile scoped key
var (
	childMirrorsMu  sync.Mutex
	childMirrors    = map[childMirrorKey]*childMirror{}
	activeChildRuns = struct {
		sync.Mutex
		at map[childMirrorKey]float64
	}{at: map[childMirrorKey]float64{}}
)

// Seconds a silent child run keeps reporting running before it is treated as lost
var childRunStaleS = 3600.0

// Relayed subagent events that become plain deltas on the child sid
var childDeltaEvents = map[string]string{"subagent.thinking": "reasoning.delta", "subagent.text": "message.delta", "subagent.start": "message.delta"}

// Profile home key of a caller's resolved home, empty for the launch profile
func childHomeKey(profileHome any) string {
	if !truthy(profileHome) {
		return ""
	}
	return pyrepr.Str(profileHome)
}

// Whether a child of the profile has a run in flight that relayed an event within the stale window
func childRunActive(childKey string, profileHome any) bool {
	activeChildRuns.Lock()
	ts, ok := activeChildRuns.at[childMirrorKey{childHomeKey(profileHome), childKey}]
	activeChildRuns.Unlock()
	return ok && nowSeconds()-ts < childRunStaleS
}

// Translates a relayed subagent event into native stream events on the child's live watch session
func mirrorSubagentToChild(ctx context.Context, eventType string, payload map[string]any, profileHome any) {
	childKey := strOr(payload["child_session_id"])
	if childKey == "" {
		return
	}
	key := childMirrorKey{childHomeKey(profileHome), childKey}
	activeChildRuns.Lock()
	if eventType == "subagent.complete" {
		delete(activeChildRuns.at, key)
	} else {
		activeChildRuns.at[key] = nowSeconds()
	}
	activeChildRuns.Unlock()
	csid, live, found := findLiveSessionByKey(childKey, key.home, false)
	if !found || live.Get("agent") != nil {
		childMirrorsMu.Lock()
		delete(childMirrors, key)
		childMirrorsMu.Unlock()
		return
	}
	text := strOr(payload["text"])
	childMirrorsMu.Lock()
	defer childMirrorsMu.Unlock()
	st := childMirrors[key]
	if st == nil {
		st = &childMirror{}
		childMirrors[key] = st
	}
	if !st.started {
		st.started = true
		emitEvent(ctx, "message.start", csid, nil)
	}
	if mapped, delta := childDeltaEvents[eventType]; delta {
		if text != "" {
			if mapped == "reasoning.delta" && !sessionShowReasoning(ctx, csid) {
				return
			}
			if eventType == "subagent.start" {
				text += "\n"
			}
			emitEvent(ctx, mapped, csid, map[string]any{"text": text})
		}
		return
	}
	if eventType != "subagent.tool" && eventType != "subagent.complete" {
		return
	}
	if st.openTool != nil {
		openTool := st.openTool
		st.openTool = nil
		if toolProgressEnabled(csid) || toolLifecycleRequiredForUI(strOr(openTool["name"])) {
			emitEvent(ctx, "tool.complete", csid, openTool)
		}
	}
	if eventType == "subagent.tool" {
		st.seq++
		toolName := strOr(payload["tool_name"])
		if toolName == "" {
			toolName = "tool"
		}
		tool := map[string]any{"name": toolName, "tool_id": fmt.Sprintf("submirror:%s:%d", childKey, st.seq), "args": map[string]any{}}
		preview := strOr(payload["tool_preview"])
		if preview == "" {
			preview = strOr(payload["text"])
		}
		if preview != "" {
			tool["preview"] = preview
		}
		if !toolProgressEnabled(csid) && !toolLifecycleRequiredForUI(toolName) {
			return
		}
		st.openTool = tool
		emitEvent(ctx, "tool.start", csid, tool)
		return
	}
	summary := strOr(payload["summary"])
	if summary == "" {
		summary = strOr(payload["text"])
	}
	emitEvent(ctx, "message.complete", csid, map[string]any{"text": summary})
	delete(childMirrors, key)
}
