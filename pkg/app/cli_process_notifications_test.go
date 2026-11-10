package app

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/tools"
)

// A finished background process event of the display session
func processEvent(sid string, exitCode any) map[string]any {
	return map[string]any{"type": "completion", "session_id": sid, "session_key": "display-session", "command": "cd /tmp && bash long-build.sh",
		"exit_code": exitCode, "completion_reason": "exited", "output": "web tsc=0\nSECRET_OUTPUT_LINE"}
}

// A CLI of the display session draining the given events, every claim granted and each acknowledgement counted
func notificationCLI(t *testing.T, mode string, events ...map[string]any) (*CLI, *int) {
	t.Helper()
	display := map[string]any{}
	if mode != "" {
		display["background_process_notifications"] = mode
	}
	SetCLIConfig(map[string]any{"display": display})
	if tools.DiagnosticProcessEvent == nil {
		tools.DiagnosticProcessEvent = agent.DiagnosticProcessEvent
		t.Cleanup(func() { tools.DiagnosticProcessEvent = nil })
	}
	prevDrain, prevClaim, prevComplete := drainRegistryNotifications, claimEventDelivery, completeEventDelivery
	acked := 0
	drainRegistryNotifications = func(*CLI) []tools.DrainedNotification {
		out := make([]tools.DrainedNotification, len(events))
		for i, e := range events {
			out[i] = tools.DrainedNotification{Event: e, Text: tools.FormatProcessNotification(e)}
		}
		return out
	}
	claimEventDelivery = func(*tools.Env, map[string]any, string) (string, bool, error) { return "claimed", true, nil }
	completeEventDelivery = func(*tools.Env, map[string]any, string) error { acked++; return nil }
	t.Cleanup(func() {
		drainRegistryNotifications, claimEventDelivery, completeEventDelivery = prevDrain, prevClaim, prevComplete
		SetCLIConfig(nil)
	})
	return &CLI{ctx: context.Background(), sessionID: "display-session", pendingInput: &inputQueue{}}, &acked
}

func TestDrainHonorsBackgroundProcessNotificationsOff(t *testing.T) {
	for mode, injected := range map[string]bool{"off": false, "concise": true, "all": true} {
		c, acked := notificationCLI(t, mode, processEvent("proc_off", 0))
		c.drainProcessNotifications("cli-post-turn")
		if *acked == 0 {
			t.Fatalf("mode %s: events were not claimed and acknowledged", mode)
		}
		if got := c.pendingInput.Len() > 0; got != injected {
			t.Fatalf("mode %s: injected %v", mode, got)
		}
	}
}

func TestOffMutesProcessWakesButSubagentResultsStillLand(t *testing.T) {
	delegation := map[string]any{"type": "async_delegation", "delegation_id": "d1", "session_key": "display-session",
		"results": []any{map[string]any{"status": "completed", "summary": "done", "goal": "audit"}}}
	heartbeat := processEvent("proc_b", 0)
	heartbeat["type"], heartbeat["seq"], heartbeat["output"] = "heartbeat", 1, "line"
	c, _ := notificationCLI(t, "off", processEvent("proc_a", 0), heartbeat, delegation)
	c.drainProcessNotifications("cli-post-turn")
	queued, ok := c.pendingInput.GetNoWait()
	if !ok || c.pendingInput.Len() != 0 {
		t.Fatalf("queued %v ok %v, %d left", queued, ok, c.pendingInput.Len())
	}
	if n, isNotification := queued.(tools.TimelineNotification); !isNotification || n.DisplayKind != "async_delegation_complete" {
		t.Fatalf("queued %#v", queued)
	}
}

func TestHeartbeatWakePaintsOneLineAndPersistsHidden(t *testing.T) {
	beat := processEvent("proc_hb", nil)
	beat["type"], beat["seq"], beat["elapsed"], beat["interval"] = "heartbeat", 3, 422, 60
	c, _ := notificationCLI(t, "", beat)
	c.drainProcessNotifications("cli-idle")
	queued, _ := c.pendingInput.GetNoWait()
	n, ok := queued.(tools.TimelineNotification)
	if !ok || n.Text != tools.FormatProcessNotification(beat) {
		t.Fatalf("queued %#v", queued)
	}
	if n.DisplayKind != tools.HeartbeatDisplayKind || tools.HeartbeatDisplayKind != "hidden" || n.DisplayText != tools.HeartbeatDisplayText(beat) {
		t.Fatalf("display kind %q text %q", n.DisplayKind, n.DisplayText)
	}
	if strings.Contains(n.DisplayText, "SECRET_OUTPUT_LINE") || !strings.Contains(n.DisplayText, "long-build.sh") {
		t.Fatalf("display text %q", n.DisplayText)
	}
}
