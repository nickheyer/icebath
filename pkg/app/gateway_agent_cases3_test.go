package app

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Result of one turn against a gateway whose prompt.submit raises the given error
func submitAgainst(t *testing.T, refusal error) (map[string]any, *GatewayAgent) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	client := &scriptedClient{}
	client.override = func(method string, params map[string]any) (any, bool, error) {
		if method != "prompt.submit" {
			return nil, false, nil
		}
		client.record(method, params)
		return nil, true, refusal
	}
	conn := scriptedConnection(t, client)
	a := scriptedAgentOn(t, conn, gatewayAgentOptions{})
	return runTurn(t, a, "q"), a
}

func TestASessionBusyRefusalAtSubmitIsTheSessionBusyVerdict(t *testing.T) {
	result, _ := submitAgainst(t, &gateway.GatewayRPCError{Msg: "session_busy"})
	if result["failed"] != true || result["completed"] != false {
		t.Fatalf("result %v", result)
	}
	if result["failure_reason"] != "session_busy" || result["failure_retryable"] != true {
		t.Fatalf("verdict %v %v", result["failure_reason"], result["failure_retryable"])
	}
}

func TestOtherOwnerRefusalsAtSubmitAreNotALostConnection(t *testing.T) {
	for _, reason := range []string{"permission_denied", "runtime_draining", "invalid_params"} {
		t.Run(reason, func(t *testing.T) {
			result, _ := submitAgainst(t, &gateway.GatewayRPCError{Msg: reason})
			if result["failed"] != true || result["error"] != reason {
				t.Fatalf("result %v", result)
			}
			if result["failure_reason"] != nil {
				t.Fatalf("failure reason %v", result["failure_reason"])
			}
			if _, has := result["failure_retryable"]; has {
				t.Fatal("a retry verdict was given")
			}
		})
	}
}

func TestATransportLossAtSubmitIsStillGatewayConnection(t *testing.T) {
	result, _ := submitAgainst(t, &gateway.GatewayClientError{Msg: gateway.GatewayDisconnected})
	if result["failed"] != true || result["error"] != gateway.GatewayDisconnected {
		t.Fatalf("result %v", result)
	}
	if result["failure_reason"] != "gateway_connection" || result["failure_retryable"] != false {
		t.Fatalf("verdict %v %v", result["failure_reason"], result["failure_retryable"])
	}
}

func TestASubmitThatNeverLeftKeepsTheTurnForTheResend(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	client := &scriptedClient{}
	client.override = func(method string, params map[string]any) (any, bool, error) {
		if method != "prompt.submit" {
			return nil, false, nil
		}
		return nil, true, &gateway.GatewayClientError{Msg: gateway.GatewayNotSent}
	}
	a := scriptedAgentOn(t, scriptedConnection(t, client), gatewayAgentOptions{})
	pick := map[string]any{"model": "picked-model", "provider": "openrouter"}
	a.mu.Lock()
	a.turnRoute, a.turnReasoning = copyMap(pick), ""
	a.mu.Unlock()
	result := runTurn(t, a, "q")
	if result["failed"] != true || result["error"] != gateway.GatewayNotSent {
		t.Fatalf("result %v", result)
	}
	if result["admitted"] != false || len(result["messages"].([]map[string]any)) != 0 {
		t.Fatalf("admitted %v messages %v", result["admitted"], result["messages"])
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !reflect.DeepEqual(a.turnRoute, pick) {
		t.Fatalf("turn route %v", a.turnRoute)
	}
}

func TestModelSwitchIsTheSessionModelMutationAndARebuiltAgentShowsIt(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{Model: "old-model", Provider: "openrouter"})
	if _, err := a.SwitchModel("new-model", "anthropic", false, false, ""); err != nil {
		t.Fatal(err)
	}
	mutations := client.callsOf("session.mutate")
	if len(mutations) != 1 || mutations[0]["operation"] != "model" {
		t.Fatalf("mutations %v", mutations)
	}
	if !sameJSON(mutations[0]["payload"], map[string]any{"model": "new-model", "provider": "anthropic"}) {
		t.Fatalf("payload %v", mutations[0]["payload"])
	}
	if !sameJSON([]any{mutations[0]["expected_revision"], mutations[0]["expected_generation"]}, []any{1, 7}) {
		t.Fatalf("fence %v %v", mutations[0]["expected_revision"], mutations[0]["expected_generation"])
	}
	if a.Model() != "new-model" || a.Provider() != "anthropic" {
		t.Fatalf("agent route %s %s", a.Model(), a.Provider())
	}
	rebuilt := scriptedAgentOn(t, a.connection, gatewayAgentOptions{Model: "old-model", Provider: "openrouter"})
	if rebuilt.Model() != "new-model" || rebuilt.Provider() != "anthropic" {
		t.Fatalf("rebuilt route %s %s", rebuilt.Model(), rebuilt.Provider())
	}
}

func TestAGuardedModelTargetCommitsOnlyOnTheUsersYesToTheOwnersWarning(t *testing.T) {
	refusal := map[string]any{"status": "confirmation_required", "confirm": "tok-1", "confirm_message": "Expensive model"}
	var asked []any
	a, client := makeScriptedAgent(t, &scriptedClient{mutateReplies: []any{refusal, map[string]any{"status": "committed", "model": "pricey-model"}}},
		gatewayAgentOptions{Model: "old-model", ModelConfirmCallback: func(r map[string]any) bool {
			asked = append(asked, r["confirm_message"])
			return true
		}})
	if ok, err := a.SwitchModel("pricey-model", "openrouter", false, false, ""); err != nil || !ok {
		t.Fatalf("switch %v %v", ok, err)
	}
	if !reflect.DeepEqual(asked, []any{"Expensive model"}) {
		t.Fatalf("asked %v", asked)
	}
	want := []any{map[string]any{"model": "pricey-model", "provider": "openrouter"},
		map[string]any{"model": "pricey-model", "provider": "openrouter", "confirm": "tok-1"}}
	if sent := client.mutatePayloads(); !sameJSON(sent, want) {
		t.Fatalf("sent %v", sent)
	}
	if a.Model() != "pricey-model" {
		t.Fatalf("model %s", a.Model())
	}
	declined, declinedClient := makeScriptedAgent(t, &scriptedClient{mutateReplies: []any{refusal}},
		gatewayAgentOptions{Model: "old-model", ModelConfirmCallback: func(map[string]any) bool { return false }})
	unattended, unattendedClient := makeScriptedAgent(t, &scriptedClient{mutateReplies: []any{refusal}}, gatewayAgentOptions{Model: "old-model"})
	if ok, err := declined.SwitchModel("pricey-model", "openrouter", false, false, ""); err != nil || ok {
		t.Fatalf("declined switch %v %v", ok, err)
	}
	if _, err := unattended.SwitchModel("pricey-model", "openrouter", false, false, ""); !isGatewayClientError(err) || !strings.Contains(err.Error(), "interactive session") {
		t.Fatalf("unattended switch %v", err)
	}
	for _, refused := range []struct {
		a      *GatewayAgent
		client *scriptedClient
	}{{declined, declinedClient}, {unattended, unattendedClient}} {
		if sent := refused.client.mutatePayloads(); !sameJSON(sent, []any{map[string]any{"model": "pricey-model", "provider": "openrouter"}}) {
			t.Fatalf("refused sent %v", sent)
		}
		if refused.a.Model() != "old-model" {
			t.Fatalf("refused model %s", refused.a.Model())
		}
	}
	second := copyMap(refusal)
	second["confirm"] = "tok-2"
	other, _ := makeScriptedAgent(t, &scriptedClient{mutateReplies: []any{refusal, second}},
		gatewayAgentOptions{Model: "old-model", ModelConfirmCallback: func(map[string]any) bool { return true }})
	if _, err := other.SwitchModel("pricey-model", "openrouter", false, false, ""); !isGatewayClientError(err) || !strings.Contains(err.Error(), "Model not switched") {
		t.Fatalf("moved session switch %v", err)
	}
	if other.Model() != "old-model" {
		t.Fatalf("model %s", other.Model())
	}
}

// Writes the scripted session's model and provider into the viewer's cached info
func setSessionRoute(a *GatewayAgent, model, provider string) {
	state := a.connection.State(scriptSID)
	state.Lock()
	defer state.Unlock()
	if state.Info == nil {
		state.Info = map[string]any{}
	}
	state.Info["model"], state.Info["provider"] = model, provider
}

func TestMoaOneShotPresetRidesThisTurnsSubmitAsItsModelOverride(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{completeFrame("ok")}}, gatewayAgentOptions{})
	setSessionRoute(a, "claude-sonnet", "anthropic")
	moa := scriptedAgentOn(t, a.connection, gatewayAgentOptions{Model: "default", Provider: "moa"})
	runTurn(t, moa, "compare these designs")
	if override := client.lastCall("prompt.submit")["model_override"]; !sameJSON(override, map[string]any{"model": "default", "provider": "moa"}) {
		t.Fatalf("override %v", override)
	}
	client.clearCalls()
	regular := scriptedAgentOn(t, a.connection, gatewayAgentOptions{Model: "claude-sonnet", Provider: "anthropic"})
	runTurn(t, regular, "next")
	if _, has := client.lastCall("prompt.submit")["model_override"]; has {
		t.Fatalf("regular submit %v", client.lastCall("prompt.submit"))
	}
}

func TestMoaOneShotOverridesAMoaSessionRunningAnotherPreset(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{completeFrame("ok")}}, gatewayAgentOptions{})
	setSessionRoute(a, "fast", "moa")
	runTurn(t, scriptedAgentOn(t, a.connection, gatewayAgentOptions{Model: "deep", Provider: "moa"}), "go")
	if override := client.lastCall("prompt.submit")["model_override"]; !sameJSON(override, map[string]any{"model": "deep", "provider": "moa"}) {
		t.Fatalf("override %v", override)
	}
	client.clearCalls()
	runTurn(t, scriptedAgentOn(t, a.connection, gatewayAgentOptions{Model: "fast", Provider: "moa"}), "again")
	if _, has := client.lastCall("prompt.submit")["model_override"]; has {
		t.Fatalf("same preset submit %v", client.lastCall("prompt.submit"))
	}
}

func TestACommittedModelSwitchStandsWhenTheRefreshAfterItFails(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{mutateReplies: []any{map[string]any{"status": "committed", "model": "claude-sonnet-4", "provider": "anthropic"}}},
		gatewayAgentOptions{Model: "old-model", Provider: "openrouter"})
	client.mu.Lock()
	client.override = func(method string, params map[string]any) (any, bool, error) {
		if method == "session.resume" && len(client.callsOf("session.mutate")) > 0 {
			return nil, true, &gateway.GatewayClientError{Msg: gateway.GatewayDisconnected}
		}
		return nil, false, nil
	}
	client.mu.Unlock()
	if _, err := a.SwitchModel("sonnet", "", false, false, ""); err != nil {
		t.Fatal(err)
	}
	if got := len(client.callsOf("session.mutate")); got != 1 {
		t.Fatalf("mutations %d", got)
	}
	if a.Model() != "claude-sonnet-4" || a.Provider() != "anthropic" {
		t.Fatalf("agent route %s %s", a.Model(), a.Provider())
	}
	rebuilt := scriptedAgentOn(t, a.connection, gatewayAgentOptions{Model: "old-model", Provider: "openrouter"})
	if rebuilt.Model() != "claude-sonnet-4" || rebuilt.Provider() != "anthropic" {
		t.Fatalf("rebuilt route %s %s", rebuilt.Model(), rebuilt.Provider())
	}
}

func TestABackgroundReviewSummaryDuringATurnIsPrintedLikeMains(t *testing.T) {
	var printed []string
	a, _ := makeScriptedAgent(t, &scriptedClient{script: []any{
		scriptFrame("review.summary", map[string]any{"text": "💾 Self-improvement review: Memory updated"}, ""), completeFrame("ok")}}, gatewayAgentOptions{})
	a.SetPrintFn(func(text string) { printed = append(printed, text) })
	runTurn(t, a, "hi")
	if !reflect.DeepEqual(printed, []string{"  💾 Self-improvement review: Memory updated"}) {
		t.Fatalf("printed %q", printed)
	}
}

func TestAnotherViewerCompletingTheClarifyBatchReleasesMainsPanel(t *testing.T) {
	panel, _ := clarifyBatchCLI(t, 60)
	request := serverRequest("srq-clr", "clarify", map[string]any{"questions": []any{
		map[string]any{"qid": "q0", "question": "colour?", "choices": []any{"red", "blue"}},
		map[string]any{"qid": "q1", "question": "size?", "choices": []any{"s", "l"}}}})
	withdrawn := scriptFrame("request.cancel", map[string]any{"id": "srq-clr", "method": "clarify", "reason": "resolved"}, "")
	client := &scriptedClient{script: []any{request, "wait", withdrawn, completeFrame("ok")}}
	a := scriptedAgentOn(t, scriptedConnection(t, client), gatewayAgentOptions{ClarifyCallback: panel.clarifyCallback, ModalWithdrawCallback: panel.withdrawGatewayModal})
	var out map[string]any
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, _ = a.RunConversation(context.Background(), "ask", agent.RunConversationOptions{})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the CLI stayed blocked in its clarify panel")
	}
	if panel.clarifyModal() != nil {
		t.Fatal("the clarify panel stayed open")
	}
	if out["completed"] != true && out["final_response"] == nil {
		t.Fatalf("out %v", out)
	}
	want := []any{[]any{"srq-clr", map[string]any{"answers": map[string]any{}, "outcome": "cancelled"}}}
	if got := client.replies(); !sameJSON(got, want) {
		t.Fatalf("replies %v", got)
	}
}

func TestAnotherViewerAnsweringAVaultPromptReleasesMainsPanel(t *testing.T) {
	for method, params := range map[string]map[string]any{
		"vault.code":          {"site": "example.com", "hint": ""},
		"vault.unlock_prompt": {"backend": "bitwarden", "display_name": "Bitwarden"},
		"vault.save_login":    {"origin": "https://example.com", "site": "example.com"}} {
		t.Run(method, func(t *testing.T) {
			panel, _ := clarifyBatchCLI(t, 60)
			withdrawn := scriptFrame("request.cancel", map[string]any{"id": "srq-vlt", "method": method, "reason": "resolved"}, "")
			client := &scriptedClient{script: []any{serverRequest("srq-vlt", method, params), "wait", withdrawn, completeFrame("ok")}}
			a := scriptedAgentOn(t, scriptedConnection(t, client), gatewayAgentOptions{ModalWithdrawCallback: panel.withdrawGatewayModal})
			done := make(chan struct{})
			go func() {
				defer close(done)
				ctx := tools.WithThreadLocals(context.Background())
				browser.SetCodePromptCallback(ctx, panel.vaultCodeCallback)
				browser.SetUnlockPromptCallback(ctx, panel.vaultUnlockCallback)
				browser.SetSaveLoginPromptCallback(ctx, panel.vaultSaveLoginCallback)
				_, _ = a.RunConversation(ctx, "sign in", agent.RunConversationOptions{})
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the CLI stayed blocked in its vault panel")
			}
			panel.uiMu.Lock()
			open := panel.sudoState
			panel.uiMu.Unlock()
			if open != nil {
				t.Fatal("the vault panel stayed open")
			}
			if got := client.replies(); !sameJSON(got, []any{[]any{"srq-vlt", map[string]any{"value": ""}}}) {
				t.Fatalf("replies %v", got)
			}
		})
	}
}

func TestAWithdrawalForAnotherRequestLeavesThePanelOpen(t *testing.T) {
	var closed []string
	a, _ := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{ModalWithdrawCallback: func(method string) { closed = append(closed, method) }})
	a.mu.Lock()
	a.openRequest = &gatewayOpenRequest{id: "srq-mine", method: "clarify"}
	a.mu.Unlock()
	a.onRequestWithdrawn(map[string]any{"id": "srq-other", "method": "clarify", "reason": "resolved"})
	if len(closed) != 0 {
		t.Fatalf("closed %v", closed)
	}
	a.onRequestWithdrawn(map[string]any{"id": "srq-mine", "method": "clarify", "reason": "resolved"})
	if !reflect.DeepEqual(closed, []string{"clarify"}) {
		t.Fatalf("closed %v", closed)
	}
}

func TestCtrlCDuringTheFirstApiCallKeepsMainsEstimateMarker(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{
		scriptFrame("message.complete", map[string]any{"text": "", "outcome": "cancelled", "status": "interrupted"})},
		result: map[string]any{"final_response": "", "api_calls": 1, "interrupted": true, "messages": []any{}}}, gatewayAgentOptions{})
	client.answerStats(sessionUsage(map[string]any{"context_tokens": 700, "api_calls": 2}), false)
	a.mu.Lock()
	a.counters["session_api_calls"] = 2
	a.mu.Unlock()
	runTurn(t, a, "story", map[string]any{"role": "user", "content": "a"}, map[string]any{"role": "assistant", "content": "b"})
	if got := anchorBaseCount(a); got != 2 {
		t.Fatalf("base count %d", got)
	}
	a.mu.Lock()
	msgs, anchor := a.sessionMessages, a.turnBaseUsageAnchor
	a.mu.Unlock()
	if got, ok := provider.AnchoredContextTokens(context.Background(), agent.RowsToMessages(msgs), anchor, false); !ok || got != 700 {
		t.Fatalf("anchored %d %v", got, ok)
	}
}

func TestAVoiceStoppedReplyReturnsWithoutWaitingForATurn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	client := &scriptedClient{}
	client.override = func(method string, params map[string]any) (any, bool, error) {
		if method != "prompt.submit" {
			return nil, false, nil
		}
		client.record(method, params)
		return map[string]any{"voice_stopped": true}, true, nil
	}
	a := scriptedAgentOn(t, scriptedConnection(t, client), gatewayAgentOptions{})
	var printed []string
	a.SetPrintFn(func(text string) { printed = append(printed, text) })
	var result map[string]any
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, _ = a.RunConversation(context.Background(), "stop", agent.RunConversationOptions{ConversationHistory: []map[string]any{{"role": "user", "content": "hi"}}})
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("RunConversation hung waiting for a turn that was never admitted")
	}
	if result["voice_stopped"] != true || result["failed"] != false || result["final_response"] != "" {
		t.Fatalf("result %v", result)
	}
	if !sameJSON(result["messages"], []any{map[string]any{"role": "user", "content": "hi"}}) {
		t.Fatalf("messages %v", result["messages"])
	}
	if len(client.callsOf("prompt.receipt")) != 0 {
		t.Fatal("a receipt was read for a turn that was never admitted")
	}
	notice := i18n.T(context.Background(), "cli.voice.stop_phrase_typed", "", nil)
	if !slices.ContainsFunc(printed, func(line string) bool { return strings.Contains(line, notice) }) {
		t.Fatalf("printed %q", printed)
	}
}

func TestAVoiceStoppedCorrectionIsConsumedNotRequeued(t *testing.T) {
	for mode, verb := range map[string]string{"steer": "session.steer", "interrupt": "session.redirect"} {
		t.Run(mode, func(t *testing.T) {
			recordPrints(t)
			a, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{})
			client.mu.Lock()
			client.override = func(method string, params map[string]any) (any, bool, error) {
				if method != "session.steer" && method != "session.redirect" {
					return nil, false, nil
				}
				client.record(method, params)
				return map[string]any{"voice_stopped": true}, true, nil
			}
			client.mu.Unlock()
			var printed []string
			a.SetPrintFn(func(text string) { printed = append(printed, text) })
			ask := 3
			a.mu.Lock()
			a.generation, a.turnAskIndex = 7, &ask
			a.mu.Unlock()
			shell := bareCLI()
			shell.busyInputMode = mode
			shell.setAgent(a)
			shell.pendingInput, shell.interruptQueue = &inputQueue{}, &inputQueue{}
			shell.tuiEnterWhileBusy("stop", nil, "stop", "")
			if !shell.pendingInput.Empty() || !shell.interruptQueue.Empty() {
				t.Fatal("consumed stop was re-queued")
			}
			var sent []string
			client.mu.Lock()
			for _, call := range client.calls {
				if strings.HasPrefix(call.method, "session.") {
					sent = append(sent, call.method)
				}
			}
			client.mu.Unlock()
			if !reflect.DeepEqual(sent, []string{verb}) {
				t.Fatalf("sent %v", sent)
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.turnAskIndex == nil || *a.turnAskIndex != 3 {
				t.Fatalf("turn ask index %v", a.turnAskIndex)
			}
		})
	}
}

func TestASessionSwitchDropsThePreviousSessionsFigures(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{})
	ctx := context.Background()
	a.connection.State(scriptSID).ApplyUsage(ctx, sessionUsage(nil))
	a.mu.Lock()
	calls, history := a.counters["session_api_calls"], len(a.apiLatencyHistory)
	a.mu.Unlock()
	if calls != 5 || history == 0 {
		t.Fatalf("calls %d latency samples %d", calls, history)
	}
	client.mu.Lock()
	client.override = func(method string, params map[string]any) (any, bool, error) {
		if method == "session.stats" && params["session_id"] == "local-other" {
			return map[string]any{"api_calls": 3, "input_tokens": 9}, true, nil
		}
		return nil, false, nil
	}
	client.mu.Unlock()
	other := a.connection.State("local-other")
	other.Lock()
	other.Messages = []any{map[string]any{"role": "user", "content": "earlier"}}
	other.Unlock()
	a.SetSessionID("local-other")
	a.ResetSessionState()
	a.mu.Lock()
	c := a.counters
	if c["session_api_calls"] != 3 || c["session_input_tokens"] != 9 || c["session_output_tokens"] != 0 {
		t.Fatalf("counters %v", c)
	}
	if a.compressor.lastPromptTokens != 0 || len(a.apiLatencyHistory) != 0 {
		t.Fatalf("compressor %+v latency %v", a.compressor, a.apiLatencyHistory)
	}
	a.mu.Unlock()
	a.connection.State(scriptSID).ApplyUsage(ctx, sessionUsage(map[string]any{"api_calls": 6}))
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.counters["session_api_calls"] != 3 {
		t.Fatalf("a frame of the session left behind repainted the figures: %d", a.counters["session_api_calls"])
	}
	left := a.connection.State(scriptSID)
	left.Lock()
	defer left.Unlock()
	if slices.Contains(left.RequestListeners, a.requestListener) {
		t.Fatal("the request listener stayed on the session left behind")
	}
}

func TestNewSessionReadsZeroWithoutAStatsRoundTrip(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{})
	a.connection.State(scriptSID).ApplyUsage(context.Background(), sessionUsage(nil))
	a.SetSessionID("local-fresh")
	client.clearCalls()
	a.ResetSessionState()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.counters["session_api_calls"] != 0 || len(a.apiLatencyHistory) != 0 {
		t.Fatalf("counters %v latency %v", a.counters, a.apiLatencyHistory)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.calls) != 0 {
		t.Fatalf("calls %v", client.calls)
	}
}

// Todo rows of a store as plain dicts
func todoRows(store *tools.TodoStore) any {
	snapshot := store.Snapshot()
	return snapshot["todos"]
}

func TestTodoUpdatesAfterASessionSwitchReachTheCliTodoStore(t *testing.T) {
	todos := []any{map[string]any{"id": "1", "content": "ship it", "status": "completed"}}
	a, _ := makeScriptedAgent(t, &scriptedClient{script: []any{scriptFrame("todo.updated", map[string]any{"todos": todos, "revision": 1}), completeFrame("x")}}, gatewayAgentOptions{})
	host := bareCLI()
	host.setAgent(a)
	host.syncAgentToSession(scriptSID)
	runTurn(t, a, "plan")
	if got := todoRows(a.TodoStore()); !sameJSON(got, todos) {
		t.Fatalf("todos %v", got)
	}
}

// Makes session.resume answer as the gateway does for a session with a plan
func resumeWithTodos(client *scriptedClient, todos []any, revision int) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.override = func(method string, params map[string]any) (any, bool, error) {
		if method != "session.resume" {
			return nil, false, nil
		}
		client.record(method, params)
		return map[string]any{"session_id": params["session_id"], "revision": 1, "execution_generation": 7, "messages": []any{},
			"stored_session_id": params["session_id"], "todo_state": map[string]any{"todos": todos, "revision": revision}}, true, nil
	}
}

func TestResumeRestoresTheSessionsTodosIntoTheCliTodoStore(t *testing.T) {
	todos := []any{map[string]any{"id": "1", "content": "ship it", "status": "in_progress"}}
	a, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{})
	resumeWithTodos(client, todos, 4)
	a.sessionDB = gatewaycli.NewGatewaySessionStore(a.connection)
	host := bareCLI()
	host.setAgent(a)
	host.syncAgentToSession("local-other")
	if got := a.TodoStore().Snapshot(); !sameJSON(got, map[string]any{"todos": todos, "revision": 4}) {
		t.Fatalf("snapshot %v", got)
	}
}

func TestStartupResumeBuildsTheAgentWithTheSessionsTodos(t *testing.T) {
	todos := []any{map[string]any{"id": "1", "content": "ship it", "status": "completed"}}
	a, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{})
	resumeWithTodos(client, todos, 2)
	store := gatewaycli.NewGatewaySessionStore(a.connection)
	if _, err := store.Attach(context.Background(), "local-resumed", false); err != nil {
		t.Fatal(err)
	}
	resumed, err := newGatewayAgent(context.Background(), gatewayAgentOptions{Connection: a.connection, SessionDB: store, SessionID: "local-resumed",
		MaxIterations: 90, QuietMode: true, ToolProgressMode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resumed.TodoStore().Snapshot(); !sameJSON(got, map[string]any{"todos": todos, "revision": 2}) {
		t.Fatalf("snapshot %v", got)
	}
	later := []any{map[string]any{"id": "1", "content": "ship it", "status": "in_progress"}}
	a.connection.State("local-resumed").ApplyEvent(context.Background(), "todo.updated", map[string]any{"todos": later, "revision": 3})
	resumed.setTodoStore(tools.NewTodoStore())
	if got := resumed.TodoStore().Snapshot(); !sameJSON(got, map[string]any{"todos": later, "revision": 3}) {
		t.Fatalf("snapshot after the swap %v", got)
	}
}

func TestAShutdownSignalRaisesTheKeyboardInterruptOutOfTheTurn(t *testing.T) {
	script := []any{scriptFrame("message.delta", map[string]any{"text": "w001 "})}
	for range 20 {
		script = append(script, "wait")
	}
	a, _ := makeScriptedAgent(t, &scriptedClient{script: script}, gatewayAgentOptions{StreamDeltaCallback: func(any) {}})
	ctx, cancel := context.WithCancelCause(context.Background())
	type outcome struct {
		result map[string]any
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := a.RunConversation(ctx, "story", agent.RunConversationOptions{})
		done <- outcome{result, err}
	}()
	waitUntil(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.generation != nil
	})
	cancel(tools.ErrKeyboardInterrupt)
	select {
	case got := <-done:
		if got.result != nil || !tools.IsKeyboardInterrupt(ctx, got.err) {
			t.Fatalf("result %v err %v", got.result, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not unwind")
	}
}
