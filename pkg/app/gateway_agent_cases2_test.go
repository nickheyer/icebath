package app

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Payload of every recorded session.mutate call
func (c *scriptedClient) mutatePayloads() []any {
	var out []any
	for _, params := range c.callsOf("session.mutate") {
		out = append(out, params["payload"])
	}
	return out
}

func TestReasoningLevelChangeIsTheSessionReasoningMutation(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{ReasoningConfig: map[string]any{"enabled": true, "effort": "high"}, PushReasoning: true})
	mut := client.callsOf("session.mutate")
	if len(mut) != 1 || mut[0]["operation"] != "reasoning" || !sameJSON(mut[0]["payload"], map[string]any{"effort": "high"}) {
		t.Fatalf("mutations %v", mut)
	}
	if !sameJSON(mut[0]["expected_generation"], 7) {
		t.Fatalf("expected generation %v", mut[0]["expected_generation"])
	}
	a.SetReasoningConfig(map[string]any{"enabled": false})
	mut = client.callsOf("session.mutate")
	if !sameJSON(mut[len(mut)-1]["payload"], map[string]any{"effort": "none"}) {
		t.Fatalf("last mutation %v", mut[len(mut)-1])
	}
	state := a.connection.State(scriptSID)
	state.Lock()
	state.Info["reasoning_effort"] = "none"
	state.Unlock()
	second := scriptedAgentOn(t, a.connection, gatewayAgentOptions{ReasoningConfig: map[string]any{"enabled": false}})
	if got := len(client.callsOf("session.mutate")); got != 2 {
		t.Fatalf("mutations after the rebuild %d", got)
	}
	if !reflect.DeepEqual(second.ReasoningConfig(), map[string]any{"enabled": false}) {
		t.Fatalf("reasoning config %v", second.ReasoningConfig())
	}
}

func TestAnExplicitLevelTheSessionAlreadyInheritsIsStillCommitted(t *testing.T) {
	recordPrints(t)
	a, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{})
	state := a.connection.State(scriptSID)
	state.Lock()
	if state.Info == nil {
		state.Info = map[string]any{}
	}
	state.Info["reasoning_effort"] = "high"
	state.Unlock()
	high := map[string]any{"enabled": true, "effort": "high"}
	scriptedAgentOn(t, a.connection, gatewayAgentOptions{ReasoningConfig: high, PushReasoning: true})
	host := bareCLI()
	host.agent = a
	host.applyReasoningAfterSwitch("high", false, false)
	a.SetReasoningConfig(map[string]any{"enabled": true, "effort": "high"})
	want := []any{map[string]any{"effort": "high"}, map[string]any{"effort": "high"}}
	if got := client.mutatePayloads(); !sameJSON(got, want) {
		t.Fatalf("mutations %v", got)
	}
}

func TestAConfigResolvedEffortNeverOverridesTheSessionsLaunchPolicy(t *testing.T) {
	config := map[string]any{"enabled": true, "effort": "none"}
	a, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{ReasoningConfig: config})
	if mut := client.callsOf("session.mutate"); len(mut) != 0 {
		t.Fatalf("mutations %v", mut)
	}
	if !reflect.DeepEqual(a.ReasoningConfig(), config) {
		t.Fatalf("reasoning config %v", a.ReasoningConfig())
	}
}

func TestReasoningSinkSetsTheSessionsShowReasoningFlagOnAttachAndToggle(t *testing.T) {
	var shown []string
	sink := func(text string) { shown = append(shown, text) }
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{
		scriptFrame("reasoning.delta", map[string]any{"text": "thinking hard"}), completeFrame("ok")}},
		gatewayAgentOptions{ReasoningCallback: sink})
	if sets := client.configSets(""); !sameJSON(sets, []any{map[string]any{"session_id": scriptSID, "key": "show_reasoning", "value": "1"}}) {
		t.Fatalf("sets %v", sets)
	}
	runTurn(t, a, "q")
	if !reflect.DeepEqual(shown, []string{"thinking hard"}) {
		t.Fatalf("shown %v", shown)
	}
	a.SetReasoningCallback(nil)
	a.SetReasoningCallback(nil)
	a.SetReasoningCallback(sink)
	var values []any
	for _, params := range client.configSets("show_reasoning") {
		values = append(values, params["value"])
	}
	if !reflect.DeepEqual(values, []any{"1", "0", "1"}) {
		t.Fatalf("values %v", values)
	}
}

// Server request frame the gateway sends a viewer
func serverRequest(id, method string, params map[string]any) map[string]any {
	params["session_id"] = scriptSID
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
}

func TestGatewaySudoRequestOpensMainsSudoPanelAndRepliesWithTheValue(t *testing.T) {
	asked := 0
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{
		serverRequest("srq-abc", "sudo", map[string]any{"command": "sudo true"}), completeFrame("ok")}},
		gatewayAgentOptions{SudoPasswordCallback: func(context.Context) string {
			asked++
			return "pw-from-panel"
		}})
	runTurn(t, a, "run sudo")
	if asked != 1 {
		t.Fatalf("asked %d", asked)
	}
	if got := client.replies(); !sameJSON(got, []any{[]any{"srq-abc", map[string]any{"value": "pw-from-panel"}}}) {
		t.Fatalf("replies %v", got)
	}
}

func TestGatewayVaultRequestsOpenMainsPanelsAndReplyWithTheValue(t *testing.T) {
	ctx := tools.WithThreadLocals(context.Background())
	for _, tc := range []struct {
		name, method string
		params       map[string]any
		install      func() func()
		expected     string
	}{
		{"code", "vault.code", map[string]any{"site": "example.com", "hint": ""}, func() func() {
			browser.SetCodePromptCallback(ctx, func(site, hint string) string { return "123456" })
			return func() { browser.SetCodePromptCallback(ctx, nil) }
		}, "123456"},
		{"unlock", "vault.unlock_prompt", map[string]any{"backend": "bitwarden", "display_name": "Bitwarden"}, func() func() {
			browser.SetUnlockPromptCallback(ctx, func(backend, name string) string { return "master" })
			return func() { browser.SetUnlockPromptCallback(ctx, nil) }
		}, "master"},
		{"save", "vault.save_login", map[string]any{"origin": "https://example.com", "site": "example.com"}, func() func() {
			browser.SetSaveLoginPromptCallback(ctx, func(origin, site string) map[string]string {
				return map[string]string{"identifier": "me@example.com", "password": "pw"}
			})
			return func() { browser.SetSaveLoginPromptCallback(ctx, nil) }
		}, `{"identifier": "me@example.com", "password": "pw"}`},
		{"save declined", "vault.save_login", map[string]any{"origin": "https://example.com", "site": "example.com"}, func() func() {
			browser.SetSaveLoginPromptCallback(ctx, func(origin, site string) map[string]string { return nil })
			return func() { browser.SetSaveLoginPromptCallback(ctx, nil) }
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, client := makeScriptedAgent(t, &scriptedClient{script: []any{serverRequest("srq-vault", tc.method, tc.params), completeFrame("ok")}}, gatewayAgentOptions{})
			defer tc.install()()
			if _, err := a.RunConversation(ctx, "sign in", agent.RunConversationOptions{}); err != nil {
				t.Fatal(err)
			}
			if got := client.replies(); !sameJSON(got, []any{[]any{"srq-vault", map[string]any{"value": tc.expected}}}) {
				t.Fatalf("replies %v", got)
			}
		})
	}
}

func TestConnectionReplySendsAJsonrpcResponseFrame(t *testing.T) {
	client := &scriptedClient{}
	conn := scriptedConnection(t, client)
	if !conn.Reply("srq-1", map[string]any{"value": ""}) {
		t.Fatal("reply was not sent")
	}
	client.mu.Lock()
	sent := append([]string(nil), client.sent...)
	client.mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("sent %v", sent)
	}
	frame := wireValue(t, map[string]any{})
	var err error
	if frame, err = loadsJSON(sent[0]); err != nil {
		t.Fatal(err)
	}
	if !sameJSON(frame, map[string]any{"jsonrpc": "2.0", "id": "srq-1", "result": map[string]any{"value": ""}}) {
		t.Fatalf("frame %s", sent[0])
	}
}

func TestGatewaySecretRequestOpensMainsSecretPanelAndTheGatewaySaves(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{
		serverRequest("srq-sec", "secret", map[string]any{"env_var": "HAB_KEY", "prompt": "Enter it", "metadata": map[string]any{"skill_name": "hab"}}),
		completeFrame("ok")}}, gatewayAgentOptions{})
	var seen []string
	restore := SetStreams(&cliui.Console{In: strings.NewReader(""), Out: io.Discard, StdinTTY: func() bool { return false }, StdoutTTY: func() bool { return false },
		MaskedSecretPrompt: func(prompt string) (string, error) {
			seen = append(seen, prompt)
			return "sekrit", nil
		}}, io.Discard)
	t.Cleanup(restore)
	host := bareCLI()
	previous := skills.SecretCaptureCallback()
	skills.SetSecretCaptureCallback(func(name, prompt string, metadata map[string]any) (map[string]any, error) {
		return promptForSecret(host, name, prompt, metadata)
	})
	t.Cleanup(func() { skills.SetSecretCaptureCallback(previous) })
	runTurn(t, a, "view the skill")
	if len(seen) == 0 || !strings.HasPrefix(seen[0], "Enter it") {
		t.Fatalf("prompts %v", seen)
	}
	if got := client.replies(); !sameJSON(got, []any{[]any{"srq-sec", map[string]any{"value": "sekrit"}}}) {
		t.Fatalf("replies %v", got)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("ICEBATH_HOME"), ".env")); !os.IsNotExist(err) {
		t.Fatalf("this process wrote .env: %v", err)
	}
}

func TestGatewayClarifyBatchRequestOpensMainsBatchPanel(t *testing.T) {
	var shown [][]any
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{
		serverRequest("srq-clr", "clarify", map[string]any{"questions": []any{
			map[string]any{"qid": "q0", "question": "colour?", "choices": []any{"red", "blue"}},
			map[string]any{"qid": "q1", "question": "size?", "choices": []any{"s", "l"}, "multi_select": true}}}),
		completeFrame("ok")}},
		gatewayAgentOptions{ClarifyCallback: func(questions []any) map[string]any {
			shown = append(shown, questions)
			return map[string]any{"answers": map[string]any{"q0": "red", "q1": []any{"s", "l"}}, "outcome": "submitted"}
		}})
	runTurn(t, a, "ask")
	if len(shown) != 1 || len(shown[0]) != 2 {
		t.Fatalf("shown %v", shown)
	}
	first, second := shown[0][0].(map[string]any), shown[0][1].(map[string]any)
	if first["qid"] != "q0" || second["qid"] != "q1" || second["multi_select"] != true {
		t.Fatalf("questions %v", shown[0])
	}
	want := []any{[]any{"srq-clr", map[string]any{"answers": map[string]any{"q0": "red", "q1": `["s", "l"]`}, "outcome": "submitted"}}}
	if got := client.replies(); !sameJSON(got, want) {
		t.Fatalf("replies %v", got)
	}
	if client.lastCall("prompt.submit")["clarify_batch"] != true {
		t.Fatalf("submit %v", client.lastCall("prompt.submit"))
	}
}

func TestAMidTurnSteerRowIsEstimatedDeltaLikeMain(t *testing.T) {
	suffix := []any{
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
			"function": map[string]any{"name": "terminal", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"},
		map[string]any{"role": "user", "content": "[USER STEER] also this", "display_kind": "steer"},
		map[string]any{"role": "assistant", "content": "done"}}
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{completeFrame("done")},
		result: map[string]any{"final_response": "done", "api_calls": 2, "messages": suffix}}, gatewayAgentOptions{})
	client.answerStats(sessionUsage(map[string]any{"context_tokens": 900, "api_calls": 2}), false)
	runTurn(t, a, "slow", map[string]any{"role": "user", "content": "a"}, map[string]any{"role": "assistant", "content": "b"})
	if got := anchorBaseCount(a); got != 3 {
		t.Fatalf("base count %d", got)
	}
}

func TestFirstTurnOnARebuiltGatewayAgentKeepsTheExactFigure(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{completeFrame("arr")},
		result: map[string]any{"final_response": "arr", "api_calls": 1, "messages": []any{map[string]any{"role": "assistant", "content": "arr"}}}}, gatewayAgentOptions{})
	client.answerStats(sessionUsage(map[string]any{"context_tokens": 120, "api_calls": 1}), false)
	a.mu.Lock()
	a.counters["session_api_calls"] = 9
	a.mu.Unlock()
	runTurn(t, a, "who are you", map[string]any{"role": "user", "content": "a"}, map[string]any{"role": "assistant", "content": "b"})
	if got := anchorBaseCount(a); got != 3 {
		t.Fatalf("base count %d", got)
	}
}

func TestNativeImagePartsAreStagedAndSubmittedAsAttachments(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGNg+M8AAAICAQB7CYF4AAAAAElFTkSuQmCC")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pixel.png")
	if err := os.WriteFile(path, png, 0o644); err != nil {
		t.Fatal(err)
	}
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{completeFrame("a pixel")}}, gatewayAgentOptions{})
	parts, skipped, err := tools.BuildNativeContentParts(nil, "what is this?", []string{path}, nil)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("parts skipped %v err %v", skipped, err)
	}
	client.mu.Lock()
	client.override = func(method string, params map[string]any) (any, bool, error) {
		if method != "image.attach_bytes" {
			return nil, false, nil
		}
		client.record(method, params)
		return map[string]any{"attached": true, "path": "/owner/cache/images/upload_1.png", "mime": "image/png"}, true, nil
	}
	client.mu.Unlock()
	runTurn(t, a, parts)
	staged := client.callsOf("image.attach_bytes")
	if len(staged) != 1 || !strings.HasPrefix(staged[0]["content_base64"].(string), "data:image/png;base64,") {
		t.Fatalf("staged %v", staged)
	}
	submit := client.lastCall("prompt.submit")
	if submit["text"] != "what is this?" {
		t.Fatalf("text %v", submit["text"])
	}
	if !sameJSON(submit["attachments"], []any{map[string]any{"path": "/owner/cache/images/upload_1.png", "mime": "image/png"}}) {
		t.Fatalf("attachments %v", submit["attachments"])
	}
}

func TestAfterARedirectTheRedirectRowIsTheTurnsAskLikeMain(t *testing.T) {
	suffix := []any{map[string]any{"role": "assistant", "content": "STEP8 story: w000 w001"},
		map[string]any{"role": "user", "content": "[Context from the interrupted assistant response] redirect now"},
		map[string]any{"role": "assistant", "content": "redirected"}}
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{scriptFrame("message.delta", map[string]any{"text": "w000 "}), "wait", "wait", completeFrame("redirected")},
		result: map[string]any{"final_response": "redirected", "api_calls": 1, "messages": suffix}},
		gatewayAgentOptions{StreamDeltaCallback: func(any) {}})
	usage := sessionUsage(map[string]any{"context_tokens": 300, "api_calls": 1})
	client.mu.Lock()
	client.override = func(method string, params map[string]any) (any, bool, error) {
		switch method {
		case "session.stats":
			return usage, true, nil
		case "session.redirect":
			client.record(method, params)
			return map[string]any{"status": "redirected"}, true, nil
		}
		return nil, false, nil
	}
	client.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.RunConversation(context.Background(), "story", agent.RunConversationOptions{})
	}()
	waitUntil(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.generation != nil
	})
	if got := a.Redirect("redirect now"); got != true {
		t.Fatalf("redirect answered %v", got)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not finish")
	}
	if got := anchorBaseCount(a); got != 3 {
		t.Fatalf("base count %d", got)
	}
}

func TestVoiceTurnFlagRidesPromptSubmit(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{completeFrame("ok")}}, gatewayAgentOptions{})
	a.SetVoiceTurnPending(true)
	if _, err := a.RunConversation(context.Background(), "[Voice input — respond concisely] hi", agent.RunConversationOptions{PersistUserMessage: "hi"}); err != nil {
		t.Fatal(err)
	}
	submit := client.lastCall("prompt.submit")
	if submit["voice_turn"] != true || submit["text"] != "hi" || !strings.HasPrefix(submit["api_text"].(string), "[Voice input") {
		t.Fatalf("submit %v", submit)
	}
	client.clearCalls()
	client.setScript(completeFrame("ok"))
	a.SetVoiceTurnPending(false)
	runTurn(t, a, "typed")
	if _, has := client.lastCall("prompt.submit")["voice_turn"]; has {
		t.Fatalf("typed submit %v", client.lastCall("prompt.submit"))
	}
}

func TestFastTierReachesTheSessionOnEachRebuildAndBackToNormal(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{})
	if sets := client.configSets("service_tier"); len(sets) != 0 {
		t.Fatalf("sets at the first build %v", sets)
	}
	scriptedAgentOn(t, a.connection, gatewayAgentOptions{ServiceTier: "priority"})
	scriptedAgentOn(t, a.connection, gatewayAgentOptions{ServiceTier: "priority"})
	scriptedAgentOn(t, a.connection, gatewayAgentOptions{})
	var values []any
	for _, params := range client.configSets("service_tier") {
		values = append(values, params["value"])
	}
	if !reflect.DeepEqual(values, []any{"priority", "normal"}) {
		t.Fatalf("values %v", values)
	}
}
