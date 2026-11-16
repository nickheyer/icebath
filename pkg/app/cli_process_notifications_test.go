package app

import (
	"context"
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
	caseHome(t)
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
