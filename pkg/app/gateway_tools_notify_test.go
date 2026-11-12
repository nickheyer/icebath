package app

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
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

func TestIdleCliPaintsHeaderAndStreamsTheGatewayAdmittedTurn(t *testing.T) {
	c, client, conn, rec := idleNotifyCLI(t)
	previousDrain := drainRegistryNotifications
	drainRegistryNotifications = func(*CLI) []tools.DrainedNotification {
		t.Error("CLI drained the process registry")
		return nil
	}
	t.Cleanup(func() { drainRegistryNotifications = previousDrain })
	playFrames(t, conn, client, gatewayNotifyFrames()...)
	c.drainProcessNotifications("cli-idle")
	queued := queuedNotification(t, c)
	if !c.pendingInput.Empty() {
		t.Fatal("more than the notification was queued")
	}
	if queued.DisplayKind != "process_complete" || queued.DisplayText != notifyDisplay || queued.Text != notifyModelText {
		t.Fatalf("queued %+v", queued)
	}
	var deltas []any
	a := scriptedAgentOn(t, conn, gatewayAgentOptions{StreamDeltaCallback: func(text any) { deltas = append(deltas, text) }})
	c.agent, c.conversationHistory = a, nil
	var chatted []any
	var result map[string]any
	patchTurn(t, func(ctx context.Context, running *GatewayAgent, message any, o agent.RunConversationOptions) (map[string]any, error) {
		chatted = append(chatted, message)
		ran, err := running.RunConversation(ctx, message, o)
		result = ran
		return ran, err
	})
	c.tuiProcessOneInput(queued)
	if !strings.Contains(printedText(rec), "◈ "+notifyDisplay) {
		t.Fatalf("output %q", printedText(rec))
	}
	if len(chatted) != 1 || chatted[0] != notifyModelText || result == nil {
		t.Fatalf("chat ran with %v result %v", chatted, result)
	}
	if len(client.callsOf("prompt.submit")) != 0 {
		t.Fatalf("the admitted turn was submitted again: %v", client.callsOf("prompt.submit"))
	}
	if !reflect.DeepEqual(deltas, []any{"STEP13 notified ", "reply."}) || result["final_response"] != "STEP13 notified reply." {
		t.Fatalf("deltas %v result %v", deltas, result)
	}
	if result["failed"] != false || result["interrupted"] != false {
		t.Fatalf("result %v", result)
	}
	read := false
	for _, params := range client.callsOf("prompt.receipt") {
		read = read || sameJSON(params, map[string]any{"session_id": scriptSID, "admission_id": "adm-9", "include_result": true})
	}
	if !read {
		t.Fatalf("receipt calls %v", client.callsOf("prompt.receipt"))
	}
	if messages := result["messages"].([]map[string]any); messages[0]["display_kind"] != "process_complete" {
		t.Fatalf("first row %v", messages[0])
	}
}

func TestAdoptionIsOneShotAndNeverHijacksATypedTurn(t *testing.T) {
	c, client, conn, _ := idleNotifyCLI(t)
	playFrames(t, conn, client, gatewayNotifyFrames()[:2]...)
	c.drainProcessNotifications("cli-idle")
	if c.pendingInput.Empty() {
		t.Fatal("the admitted turn was not queued")
	}
	client.setScript(completeFrame("typed"))
	a := scriptedAgentOn(t, conn, gatewayAgentOptions{})
	a.SetPendingCLIUserMessage(map[string]any{"role": "user", "content": "hello"})
	result := runTurn(t, a, "hello")
	if len(client.callsOf("prompt.submit")) == 0 || result["final_response"] != "typed" {
		t.Fatalf("submits %v result %v", client.callsOf("prompt.submit"), result)
	}
	state := conn.State(scriptSID)
	state.Lock()
	defer state.Unlock()
	if state.AdmittedTurn != nil {
		t.Fatalf("admitted turn kept %v", state.AdmittedTurn)
	}
}

func TestPostTurnAdoptionRunsBeforeInputsQueuedBehindTheRunningTurn(t *testing.T) {
	c, client, conn, _ := idleNotifyCLI(t)
	c.pendingInput.Put("typed while the last turn ran")
	playFrames(t, conn, client, gatewayNotifyFrames()[:2]...)
	c.drainProcessNotifications("cli-post-turn")
	if first := queuedNotification(t, c); first.DisplayText != notifyDisplay {
		t.Fatalf("first %+v", first)
	}
	if next, _ := c.pendingInput.GetNoWait(); next != "typed while the last turn ran" {
		t.Fatalf("next %v", next)
	}
}

func TestIdleTickWithoutAnAdmittedTurnQueuesNothing(t *testing.T) {
	c, client, conn, _ := idleNotifyCLI(t)
	playFrames(t, conn, client, scriptFrame("session.usage", map[string]any{"usage": map[string]any{"api_calls": 1}}),
		scriptFrame("message.complete", map[string]any{"text": "late"}, "adm-old"))
	c.drainProcessNotifications("cli-idle")
	if !c.pendingInput.Empty() {
		t.Fatalf("queued %v", c.pendingInput.snapshot())
	}
}

func TestIdleCliShowsAGatewayReviewSubagentsCompletion(t *testing.T) {
	c, client, conn, rec := idleNotifyCLI(t)
	event := map[string]any{"type": "async_delegation", "delegation_id": "deleg_1", "parent_session_id": scriptSID,
		"goals": []any{"Review the work just discussed"}, "results": []any{map[string]any{"task_index": 0, "status": "completed",
			"summary": "Two nits.", "api_calls": 3, "duration_seconds": 12}}}
	text := tools.FormatProcessNotification(event)
	card := map[string]any{"display_kind": "async_delegation_complete", "display_text": "Subagent Task Completed: Review the work just discussed", "text": text}
	playFrames(t, conn, client, admissionFrame("message.start", card, 9, "adm-9"),
		admissionFrame("message.delta", map[string]any{"text": "Review: two nits."}, 9, "adm-9"),
		admissionFrame("message.complete", map[string]any{"text": "Review: two nits.", "status": "complete"}, 9, "adm-9"))
	c.drainProcessNotifications("cli-idle")
	queued := queuedNotification(t, c)
	if queued.DisplayKind != "async_delegation_complete" || queued.DisplayText != "Subagent Task Completed: Review the work just discussed" || queued.Text != text {
		t.Fatalf("queued %+v", queued)
	}
	var deltas []any
	a := scriptedAgentOn(t, conn, gatewayAgentOptions{StreamDeltaCallback: func(text any) { deltas = append(deltas, text) }})
	c.agent, c.conversationHistory = a, nil
	var result map[string]any
	patchTurn(t, func(ctx context.Context, running *GatewayAgent, message any, o agent.RunConversationOptions) (map[string]any, error) {
		ran, err := running.RunConversation(ctx, message, o)
		result = ran
		return ran, err
	})
	c.tuiProcessOneInput(queued)
	if !strings.Contains(printedText(rec), "◈ "+queued.DisplayText) {
		t.Fatalf("output %q", printedText(rec))
	}
	if result == nil {
		t.Fatal("chat ran no turn")
	}
	if len(client.callsOf("prompt.submit")) != 0 {
		t.Fatalf("the admitted turn was submitted again: %v", client.callsOf("prompt.submit"))
	}
	if !reflect.DeepEqual(deltas, []any{"Review: two nits."}) || result["final_response"] != "Review: two nits." {
		t.Fatalf("deltas %v result %v", deltas, result)
	}
}

func TestIdleCliPrintsTheGatewayBackgroundReviewSummary(t *testing.T) {
	c, client, conn, rec := idleNotifyCLI(t)
	playFrames(t, conn, client, scriptFrame("review.summary", map[string]any{"text": "💾 Self-improvement review: Memory updated"}, ""))
	c.drainProcessNotifications("cli-idle")
	if !strings.Contains(printedText(rec), "  💾 Self-improvement review: Memory updated") {
		t.Fatalf("output %q", printedText(rec))
	}
	if !c.pendingInput.Empty() {
		t.Fatalf("a report was queued as a turn: %v", c.pendingInput.snapshot())
	}
}
