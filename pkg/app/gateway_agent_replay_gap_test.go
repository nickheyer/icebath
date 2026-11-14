package app

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
)

// Gap frame of the scripted session, carrying no admission
func gapFrame() map[string]any { return scriptFrame("session.replay_gap", map[string]any{}, "") }

// Runs one turn on its own goroutine, nil when it did not finish in time
func runTurnWithin(a *GatewayAgent, text string, timeout time.Duration, o agent.RunConversationOptions) map[string]any {
	done := make(chan map[string]any, 1)
	go func() {
		result, _ := a.RunConversation(context.Background(), text, o)
		done <- result
	}()
	select {
	case result := <-done:
		return result
	case <-time.After(timeout):
		return nil
	}
}

// Sets the settle bound of an interrupted turn for one case
func settleBound(t *testing.T, seconds float64) {
	t.Helper()
	previous := gatewayInterruptSettleSeconds
	gatewayInterruptSettleSeconds = seconds
	t.Cleanup(func() { gatewayInterruptSettleSeconds = previous })
}

func TestIdleGapReattachCannotSubmitAbandonedInput(t *testing.T) {
	for _, change := range []string{"session", "interrupt"} {
		t.Run(change, func(t *testing.T) {
			entered, release := make(chan struct{}, 1), make(chan struct{})
			client := &scriptedClient{}
			client.override = func(method string, params map[string]any) (any, bool, error) {
				if method != "session.resume" || params["session_id"] != scriptSID {
					return nil, false, nil
				}
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-time.After(5 * time.Second):
				}
				return map[string]any{"pending": []any{}}, true, nil
			}
			a, _ := makeScriptedAgent(t, client, gatewayAgentOptions{})
			state := a.connection.State(scriptSID)
			state.Lock()
			state.Gaps = 1
			state.Unlock()
			if change == "interrupt" {
				settleBound(t, 0)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = a.RunConversation(context.Background(), "old question", agent.RunConversationOptions{})
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				close(release)
				t.Fatal("the turn never re-attached")
			}
			if change == "session" {
				a.SetSessionID("new-session")
				a.ResetSessionState()
			} else {
				a.Interrupt()
			}
			close(release)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("the abandoned turn kept running")
			}
			if submits := client.callsOf("prompt.submit"); len(submits) != 0 {
				t.Fatalf("abandoned input was submitted: %v", submits)
			}
		})
	}
}

func TestATurnPastItsInterruptSettleBoundStillReadsItsReceiptAndStats(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{scriptFrame("message.delta", map[string]any{"text": "partial"}), "wait", "wait", "wait", "wait"}}, gatewayAgentOptions{})
	settleBound(t, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.RunConversation(context.Background(), "q", agent.RunConversationOptions{})
	}()
	waitUntil(t, func() bool { return len(client.callsOf("prompt.submit")) > 0 })
	a.Interrupt()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the interrupted turn never returned")
	}
	if len(client.callsOf("prompt.receipt")) == 0 || len(client.callsOf("session.stats")) == 0 {
		client.mu.Lock()
		defer client.mu.Unlock()
		t.Fatalf("calls %v", client.calls)
	}
}

func TestALongStreamedReplyKeepsItsDeltaBuffersLinear(t *testing.T) {
	a := &GatewayAgent{opts: gatewayAgentOptions{StreamDeltaCallback: func(any) {}}, turn: newGatewayTurn(scriptSID)}
	started := time.Now()
	payload := map[string]any{"text": "token-text "}
	for range 200_000 {
		a.onDelta(payload, func(string) {}, map[string]time.Time{})
	}
	if time.Since(started) > 10*time.Second {
		t.Fatalf("200k deltas took %s", time.Since(started))
	}
	want := strings.Repeat("token-text ", 200_000)
	if a.turn.shownText() != want || a.turn.spokenText() != want {
		t.Fatal("the delta buffers lost text")
	}
}

func TestToolStartClosesAPrefixWhoseSeparatorFellInTheHole(t *testing.T) {
	final := "The answer is 42."
	var output []any
	client := &scriptedClient{script: []any{scriptFrame("message.delta", map[string]any{"text": "The answer"}), gapFrame()},
		result: map[string]any{"final_response": final, "completed": true}}
	client.override = func(method string, params map[string]any) (any, bool, error) {
		if method != "session.resume" {
			return nil, false, nil
		}
		client.events.PutNowait(wireValue(client.t, scriptFrame("tool.start", map[string]any{"name": "read_file", "tool_id": "t", "args": map[string]any{}})))
		client.events.PutNowait(wireValue(client.t, completeFrame(final)))
		return map[string]any{"pending": []any{map[string]any{"admission_id": "adm-1", "status": "started"}}}, true, nil
	}
	a, _ := makeScriptedAgent(t, client, gatewayAgentOptions{
		StreamDeltaCallback: func(text any) { output = append(output, text) },
		ToolProgressCallback: func(string, string, string, map[string]any, map[string]any) {
			output = append(output, []string{"<tool>"})
		}})
	if runTurnWithin(a, "q", 5*time.Second, agent.RunConversationOptions{}) == nil {
		t.Fatal("the turn did not finish")
	}
	tool := -1
	for i, item := range output {
		if _, isTool := item.([]string); isTool && tool < 0 {
			tool = i
		}
	}
	if tool < 0 {
		t.Fatalf("no tool line in %v", output)
	}
	after := ""
	for _, item := range output[tool+1:] {
		if text, isText := item.(string); isText {
			after += text
		}
	}
	if after != final {
		t.Fatalf("text after the tool %q of %v", after, output)
	}
}

func TestGapCompletesTheVoiceStreamWithoutTerminalStreaming(t *testing.T) {
	final := "First sentence. Second sentence."
	var spoken []string
	a, _ := makeScriptedAgent(t, &scriptedClient{script: []any{scriptFrame("message.delta", map[string]any{"text": "First sentence. "}), gapFrame()},
		result: map[string]any{"final_response": final, "completed": true}}, gatewayAgentOptions{})
	result, err := a.RunConversation(context.Background(), "q", agent.RunConversationOptions{StreamCallback: func(text string) { spoken = append(spoken, text) }})
	if err != nil {
		t.Fatal(err)
	}
	if result["failed"] != false || result["final_response"] != final {
		t.Fatalf("result %v", result)
	}
	if got := strings.Join(spoken, ""); got != final {
		t.Fatalf("spoken %q", got)
	}
}

func TestGapRecoveryRestoresOrClearsTheConnectionCard(t *testing.T) {
	card := map[string]any{"op_id": "op1", "seq": 3, "targets": []any{map[string]any{"name": "linear", "state": "initiated"}}}
	for name, pending := range map[string]map[string]any{"open operation": card, "none": nil} {
		t.Run(name, func(t *testing.T) {
			final := "done"
			var opened, updates []any
			client := &scriptedClient{script: []any{gapFrame()}, result: map[string]any{"final_response": final, "completed": true}}
			client.override = func(method string, params map[string]any) (any, bool, error) {
				if method != "session.resume" {
					return nil, false, nil
				}
				client.events.PutNowait(wireValue(client.t, completeFrame(final)))
				var connection any
				if pending != nil {
					connection = pending
				}
				return map[string]any{"pending": []any{map[string]any{"admission_id": "adm-1", "status": "started"}}, "pending_connection": connection}, true, nil
			}
			a, _ := makeScriptedAgent(t, client, gatewayAgentOptions{
				ConnectionCallback:       func(args ...any) { opened = append(opened, args...) },
				ConnectionUpdateCallback: func(snapshot map[string]any) { updates = append(updates, snapshot) }})
			if runTurnWithin(a, "connect linear", 5*time.Second, agent.RunConversationOptions{}) == nil {
				t.Fatal("the turn did not finish")
			}
			if pending != nil {
				if !sameJSON(opened, []any{pending}) || len(updates) != 0 {
					t.Fatalf("opened %v updates %v", opened, updates)
				}
			} else if len(opened) != 0 || !reflect.DeepEqual(updates, []any{map[string]any{"settled": true}}) {
				t.Fatalf("opened %v updates %v", opened, updates)
			}
		})
	}
}
