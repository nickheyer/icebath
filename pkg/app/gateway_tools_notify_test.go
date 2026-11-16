package app

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	notifyDisplay   = "Background Process Finished: sleep 6; echo BGDONE"
	notifyModelText = "[IMPORTANT: Background process proc_1 completed (exit 0).\nOutput:\nBGDONE]"
)

// Frame of the scripted session for one generation and admission
func admissionFrame(kind string, payload map[string]any, generation int, admission string) map[string]any {
	frame := scriptFrame(kind, payload, admission)
	frame["params"].(map[string]any)["execution_generation"] = generation
	return frame
}

// What the authority publishes for one admitted local automation turn of admission adm-9
func gatewayNotifyFrames() []map[string]any {
	return []map[string]any{
		{"jsonrpc": "2.0", "method": "event", "params": map[string]any{"type": "session.info", "session_id": scriptSID, "payload": map[string]any{
			"running": false, "pending": []any{map[string]any{"admission_id": "adm-9", "status": "queued", "input_id": "x", "text": notifyModelText}}}}},
		admissionFrame("message.start", map[string]any{"display_kind": "process_complete", "display_text": notifyDisplay, "text": notifyModelText}, 9, "adm-9"),
		admissionFrame("message.delta", map[string]any{"text": "STEP13 notified "}, 9, "adm-9"),
		admissionFrame("message.delta", map[string]any{"text": "reply."}, 9, "adm-9"),
		admissionFrame("message.complete", map[string]any{"text": "STEP13 notified reply.", "status": "complete"}, 9, "adm-9"),
	}
}

// Idle CLI on the scripted session with nothing queued
func idleNotifyCLI(t *testing.T) (*CLI, *scriptedClient, *gatewaycli.GatewayConnection, *printRecorder) {
	t.Helper()
	rec := recordPrints(t)
	client := &scriptedClient{}
	conn := scriptedConnection(t, client)
	c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{})
	c.sessionID = scriptSID
	c.pendingInput, c.interruptQueue = &inputQueue{}, &inputQueue{}
	c.sessionDB = gatewaycli.NewGatewaySessionStore(conn)
	t.Cleanup(func() { _ = c.sessionDB.CloseReader() })
	return c, client, conn, rec
}

// Hands frames to the socket and waits until the connection queued them for the viewer
func playFrames(t *testing.T, conn *gatewaycli.GatewayConnection, client *scriptedClient, frames ...map[string]any) {
	t.Helper()
	for _, frame := range frames {
		client.events.PutNowait(wireValue(t, frame))
	}
	waitUntil(t, func() bool { return conn.Events.QSize() >= len(frames) })
}

func queuedNotification(t *testing.T, c *CLI) tools.TimelineNotification {
	t.Helper()
	item, ok := c.pendingInput.GetNoWait()
	if !ok {
		t.Fatal("nothing was queued")
	}
	notification, isNotification := item.(tools.TimelineNotification)
	if !isNotification {
		t.Fatalf("queued %T %v", item, item)
	}
	return notification
}
