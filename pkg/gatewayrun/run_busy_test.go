package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Telegram private chat text event from user1 with message id msg1
func busyEvent(text string) *gateway.MessageEvent {
	event := gateway.NewMessageEvent(text)
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "123", ChatType: "private", UserID: strp("user1")}
	event.MessageID = strp("msg1")
	return event
}

// Runner with every Telegram user authorized and the session key of the event's source
func busyRunner(t *testing.T, mode string, event *gateway.MessageEvent) (*GatewayRunner, *restartTestAdapter, string) {
	t.Helper()
	t.Setenv("TELEGRAM_ALLOW_ALL_USERS", "true")
	t.Setenv("ICEBATH_HOME", t.TempDir())
	r, adapter := makeRestartRunner(t)
	r.busyInputMode = mode
	r.busyTextMode = "interrupt"
	sk, err := gateway.BuildSessionKey(context.Background(), event.Source, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	return r, adapter, sk
}

// Steer text the agent holds, read from the out of band user row it delivers after one tool result
func heldSteer(a *agent.AIAgent) string {
	messages := []*state.Message{{Role: "tool", Content: "done"}}
	a.ApplyPendingSteer(&messages, 1)
	if len(messages) < 2 {
		return ""
	}
	row := fmt.Sprint(messages[len(messages)-1].Content)
	_, body, _ := strings.Cut(row, "\n")
	return strings.TrimSuffix(body, "\n[/OUT-OF-BAND USER MESSAGE]")
}

func TestSendsAckWhenAgentRunning(t *testing.T) {
	event := busyEvent("Are you working?")
	r, adapter, sk := busyRunner(t, "interrupt", event)
	a := quietAgent(t)
	r.runningAgents().Set(sk, a)
	r.runningAgentsTS().Set(sk, wallClock()-600)

	handled, err := r.handleActiveSessionBusyMessage(context.Background(), event, sk)
	if err != nil || !handled {
		t.Fatalf("handled %v err %v", handled, err)
	}
	calls := adapter.calls()
	if len(calls) != 1 {
		t.Fatalf("sends %v", calls)
	}
	content := calls[0].content
	if !strings.Contains(content, "Interrupting") && !strings.Contains(content, "respond") {
		t.Fatalf("ack %q", content)
	}
	if strings.Contains(content, "/stop") {
		t.Fatalf("ack names /stop %q", content)
	}
	if !a.InterruptRequested() || fmt.Sprint(a.InterruptMessage()) != "Are you working?" {
		t.Fatalf("interrupt %v %v", a.InterruptRequested(), a.InterruptMessage())
	}
}

func TestSteerModeCallsAgentSteerNoInterruptNoQueue(t *testing.T) {
	t.Setenv("ICEBATH_GATEWAY_BUSY_STEER_ACK_ENABLED", "")
	os.Unsetenv("ICEBATH_GATEWAY_BUSY_STEER_ACK_ENABLED")
	event := busyEvent("also check the tests")
	r, adapter, sk := busyRunner(t, "steer", event)
	a := quietAgent(t)
	r.runningAgents().Set(sk, a)

	if _, err := r.handleActiveSessionBusyMessage(context.Background(), event, sk); err != nil {
		t.Fatal(err)
	}
	injected := heldSteer(a)
	if !strings.HasSuffix(injected, "also check the tests") || !strings.Contains(injected, `"chat_id": "123"`) {
		t.Fatalf("steer %q", injected)
	}
	if a.InterruptRequested() {
		t.Fatal("agent was interrupted")
	}
	if _, held := adapter.PeekPendingMessage(sk); held {
		t.Fatal("a successful steer was queued as the next turn")
	}
	calls := adapter.calls()
	if len(calls) != 1 {
		t.Fatalf("sends %v", calls)
	}
	if content := calls[0].content; !strings.Contains(strings.ToLower(content), "steer") || strings.Contains(content, "Interrupting") {
		t.Fatalf("ack %q", content)
	}
}

func TestSteerModeFallsBackToQueueWhenAgentPending(t *testing.T) {
	event := busyEvent("arrived too early")
	r, adapter, sk := busyRunner(t, "steer", event)
	r.runningAgents().Set(sk, agentPendingSentinel)

	if _, err := r.handleActiveSessionBusyMessage(context.Background(), event, sk); err != nil {
		t.Fatal(err)
	}
	if pending, _ := adapter.PeekPendingMessage(sk); pending != event {
		t.Fatalf("pending %v", pending)
	}
	calls := adapter.calls()
	if len(calls) == 0 || !strings.Contains(calls[len(calls)-1].content, "Queued for the next turn") {
		t.Fatalf("sends %v", calls)
	}
}

func TestInterruptModeTextFollowupsFIFONotMerged(t *testing.T) {
	evt := func(text string) *gateway.MessageEvent {
		event := gateway.NewMessageEvent(text)
		event.Source = &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "123", ChatType: "dm", UserID: strp("user1")}
		event.MessageID = strp("m-" + text[:5])
		return event
	}
	first, second := evt("first message"), evt("second message")
	r, adapter, sk := busyRunner(t, "interrupt", first)
	r.runningAgents().Set(sk, quietAgent(t))

	if _, err := r.handleActiveSessionBusyMessage(context.Background(), first, sk); err != nil {
		t.Fatal(err)
	}
	r.busyAckTS().Clear()
	if _, err := r.handleActiveSessionBusyMessage(context.Background(), second, sk); err != nil {
		t.Fatal(err)
	}
	head, _ := adapter.PeekPendingMessage(sk)
	if head != first || head.Text != "first message" {
		t.Fatalf("head %v", head)
	}
	overflow, _ := r.queuedEvents().Get(sk)
	var texts []string
	for _, e := range overflow {
		texts = append(texts, e.Text)
	}
	if len(texts) != 1 || texts[0] != "second message" {
		t.Fatalf("overflow %v", texts)
	}
}
