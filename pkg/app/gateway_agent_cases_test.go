package app

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Completion frame of a finished reply
func completeFrame(text string) map[string]any {
	return scriptFrame("message.complete", map[string]any{"text": text, "outcome": "completed"})
}

// Copy of the session usage the cases publish with some keys replaced
func sessionUsage(replace map[string]any) map[string]any {
	usage := map[string]any{"input_tokens": 500, "output_tokens": 80, "cache_read_tokens": 0, "cache_write_tokens": 0,
		"prompt_tokens": 500, "completion_tokens": 80, "total_tokens": 580, "reasoning_tokens": 0,
		"api_calls": 5, "last_prompt_tokens": 229, "context_tokens": 229, "context_length": 128000,
		"config_context_length": 128000, "context_source": "local_estimate", "compression_count": 0,
		"avg_latency_s": 0.2, "tokens_per_s": 116.0, "last_turn_tokens": 30, "rate_limit": nil,
		"active_subagents": 0, "model": "fake-model", "provider": "custom", "fallback_model": nil}
	for k, v := range replace {
		usage[k] = v
	}
	return usage
}

// Makes the double answer session.stats with stats, recording the call when asked to
func (c *scriptedClient) answerStats(stats map[string]any, record bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.override = func(method string, params map[string]any) (any, bool, error) {
		if method != "session.stats" {
			return nil, false, nil
		}
		if record {
			c.record(method, params)
		}
		return stats, true, nil
	}
}

func TestStreamedReplyReachesStreamDeltaCallback(t *testing.T) {
	var deltas []any
	a, client := makeScriptedAgent(t, &scriptedClient{
		script: []any{scriptFrame("message.delta", map[string]any{"text": "Hel"}), scriptFrame("message.delta", map[string]any{"text": "lo"}),
			scriptFrame("message.delta", map[string]any{"text": nil}),
			scriptFrame("message.complete", map[string]any{"text": "Hello", "outcome": "completed", "status": "complete"})},
		result: map[string]any{"final_response": "Hello", "api_calls": 1, "completed": true,
			"messages": []any{map[string]any{"role": "assistant", "content": "Hello"}}, "_messages_are_turn_suffix": true}},
		gatewayAgentOptions{StreamDeltaCallback: func(text any) { deltas = append(deltas, text) }})
	result := runTurn(t, a, "hi", map[string]any{"role": "user", "content": "old"})
	if !reflect.DeepEqual(deltas, []any{"Hel", "lo", nil}) {
		t.Fatalf("deltas %v", deltas)
	}
	if result["final_response"] != "Hello" {
		t.Fatalf("final %v", result["final_response"])
	}
	var contents []any
	for _, m := range result["messages"].([]map[string]any) {
		contents = append(contents, m["content"])
	}
	if !reflect.DeepEqual(contents, []any{"old", "hi", "Hello"}) {
		t.Fatalf("messages %v", contents)
	}
	if submit := client.lastCall("prompt.submit"); submit["text"] != "hi" || submit["session_id"] != scriptSID {
		t.Fatalf("submit %v", submit)
	}
}

type progressCall struct {
	args  []any
	extra map[string]any
}

func TestToolEventsDriveProgressAndCompleteCallbacks(t *testing.T) {
	var progress []progressCall
	var completes [][]any
	a, _ := makeScriptedAgent(t, &scriptedClient{script: []any{
		scriptFrame("tool.generating", map[string]any{"name": "read_file"}),
		scriptFrame("tool.start", map[string]any{"tool_id": "c1", "name": "read_file", "args": map[string]any{"path": "notes.txt"}, "context": ""}),
		scriptFrame("tool.complete", map[string]any{"tool_id": "c1", "name": "read_file", "args": map[string]any{"path": "notes.txt"},
			"result": "{}", "is_error": false, "duration_s": 0.25, "inline_diff": nil}),
		completeFrame("done")}},
		gatewayAgentOptions{
			ToolProgressCallback: func(eventType, name, preview string, args map[string]any, extra map[string]any) {
				progress = append(progress, progressCall{[]any{eventType, name, preview, args}, extra})
			},
			ToolCompleteCallback: func(id, name string, args map[string]any, result string) {
				completes = append(completes, []any{id, name, args, result})
			},
			ToolGenCallback: func(name string) { progress = append(progress, progressCall{[]any{"gen", name}, nil}) }})
	runTurn(t, a, "read it")
	if len(progress) != 3 || !reflect.DeepEqual(progress[0].args, []any{"gen", "read_file"}) {
		t.Fatalf("progress %v", progress)
	}
	started := progress[1].args
	if started[0] != "tool.started" || started[1] != "read_file" || !sameJSON(started[3], map[string]any{"path": "notes.txt"}) {
		t.Fatalf("started %v", started)
	}
	done := progress[2]
	if done.args[0] != "tool.completed" || done.args[1] != "read_file" {
		t.Fatalf("done %v", done.args)
	}
	if done.extra["duration"] != 0.25 || done.extra["is_error"] != false || done.extra["result"] != "{}" {
		t.Fatalf("done extra %v", done.extra)
	}
	if !sameJSON(completes, []any{[]any{"c1", "read_file", map[string]any{"path": "notes.txt"}, "{}"}}) {
		t.Fatalf("completes %v", completes)
	}
}

func TestApprovalRequestCallsCliApprovalCallbackThenResponds(t *testing.T) {
	type ask struct {
		command, description string
		allowPermanent       bool
	}
	var seen []ask
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{
		scriptFrame("approval.request", map[string]any{"kind": "approval", "prompt_id": "p1", "execution_generation": 7,
			"command": "rm -rf x", "description": "dangerous", "choices": []any{"once", "deny", "session", "always"}}),
		completeFrame("ok")}},
		gatewayAgentOptions{ApprovalCallback: func(_ context.Context, command, description string, allowPermanent, _, _ bool) (string, error) {
			seen = append(seen, ask{command, description, allowPermanent})
			return "once", nil
		}})
	runTurn(t, a, "do it")
	if len(seen) == 0 || seen[0].command != "rm -rf x" || !seen[0].allowPermanent {
		t.Fatalf("seen %v", seen)
	}
	want := map[string]any{"session_id": scriptSID, "execution_generation": 7, "prompt_id": "p1", "choice": "once"}
	if respond := client.lastCall("approval.respond"); !sameJSON(respond, want) {
		t.Fatalf("respond %v", respond)
	}
}

func TestConnectionCardEventsReachTheCliCard(t *testing.T) {
	var opened, updates []any
	request := map[string]any{"op_id": "op1", "seq": 1, "deadline_at": 0, "timeout_seconds": 300,
		"targets": []any{map[string]any{"name": "linear", "kind": "mcp", "action": "install", "state": "pending"}}}
	update := copyMap(request)
	update["seq"], update["settled"] = 2, false
	a, _ := makeScriptedAgent(t, &scriptedClient{script: []any{scriptFrame("connection.request", request),
		scriptFrame("connection.update", update), completeFrame("ok")}},
		gatewayAgentOptions{ConnectionCallback: func(args ...any) { opened = append(opened, args...) },
			ConnectionUpdateCallback: func(snapshot map[string]any) { updates = append(updates, snapshot) }})
	runTurn(t, a, "connect linear")
	if !sameJSON(opened, []any{request}) || !sameJSON(updates, []any{update}) {
		t.Fatalf("opened %v updates %v", opened, updates)
	}
}

func TestInterruptSendsSessionInterruptAndSettlesInterrupted(t *testing.T) {
	script := []any{scriptFrame("message.delta", map[string]any{"text": "w001 "})}
	for range 20 {
		script = append(script, "wait")
	}
	a, client := makeScriptedAgent(t, &scriptedClient{script: script,
		onInterrupt: scriptFrame("message.complete", map[string]any{"text": "", "outcome": "cancelled", "status": "interrupted"})},
		gatewayAgentOptions{StreamDeltaCallback: func(any) {}})
	var out map[string]any
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, _ = a.RunConversation(context.Background(), "story", agent.RunConversationOptions{})
	}()
	waitUntil(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.generation != nil && a.admission != nil && a.admission != ""
	})
	a.Interrupt()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not settle after the interrupt")
	}
	if interrupt := client.lastCall("session.interrupt"); !sameJSON(interrupt, map[string]any{"session_id": scriptSID, "execution_generation": 7}) {
		t.Fatalf("interrupt %v", interrupt)
	}
	if out["interrupted"] != true || out["completed"] != false {
		t.Fatalf("out %v", out)
	}
}

func TestResultDictHasEveryKeyHermesCliReads(t *testing.T) {
	a, _ := makeScriptedAgent(t, &scriptedClient{script: []any{completeFrame("x")},
		result: map[string]any{"final_response": "x", "api_calls": 2, "input_tokens": 10, "output_tokens": 3},
		usage:  map[string]any{"last_prompt_tokens": 42, "context_length": 1000}}, gatewayAgentOptions{})
	result := runTurn(t, a, "q")
	for _, key := range []string{"final_response", "messages", "failed", "partial", "error", "failure_reason", "completed",
		"interrupted", "interrupt_message", "api_calls", "pending_steer", "last_reasoning",
		"response_previewed", "response_transformed", "pre_transform_response", "billing_block",
		"session_id", "auxiliary_usage"} {
		if _, has := result[key]; !has {
			t.Fatalf("result lacks %s", key)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.compressor.lastPromptTokens != 42 || a.compressor.contextLength != 1000 || a.counters["session_api_calls"] != 2 {
		t.Fatalf("compressor %+v calls %d", a.compressor, a.counters["session_api_calls"])
	}
}

func TestDisconnectReturnsFailedResult(t *testing.T) {
	a, _ := makeScriptedAgent(t, &scriptedClient{script: []any{scriptFrame("message.delta", map[string]any{"text": "par"}), "disconnect"}},
		gatewayAgentOptions{StreamDeltaCallback: func(any) {}})
	result := runTurn(t, a, "q")
	if result["failed"] != true || result["completed"] != false {
		t.Fatalf("result %v", result)
	}
	if text, _ := result["error"].(string); !strings.Contains(strings.ToLower(text), "disconnected") {
		t.Fatalf("error %v", result["error"])
	}
}

func TestOtherAdmissionsFramesAreIgnored(t *testing.T) {
	var deltas []any
	a, _ := makeScriptedAgent(t, &scriptedClient{script: []any{
		scriptFrame("message.delta", map[string]any{"text": "theirs"}, "adm-9"),
		scriptFrame("message.delta", map[string]any{"text": "ours"}), completeFrame("ours")}},
		gatewayAgentOptions{StreamDeltaCallback: func(text any) { deltas = append(deltas, text) }})
	runTurn(t, a, "q")
	if !reflect.DeepEqual(deltas, []any{"ours"}) {
		t.Fatalf("deltas %v", deltas)
	}
}

func TestAgentPrintRoutesToPrintFn(t *testing.T) {
	var printed []string
	a, _ := makeScriptedAgent(t, &scriptedClient{script: []any{
		scriptFrame("agent.print", map[string]any{"text": "⚡ Interrupted during API call."}), completeFrame("")}}, gatewayAgentOptions{})
	a.SetPrintFn(func(text string) { printed = append(printed, text) })
	runTurn(t, a, "q")
	if !reflect.DeepEqual(printed, []string{"⚡ Interrupted during API call."}) {
		t.Fatalf("printed %v", printed)
	}
}

func TestHardInterruptIsARealMethod(t *testing.T) {
	var _ interface{ HardInterrupt() } = (*GatewayAgent)(nil)
	if _, has := reflect.TypeOf((*GatewayAgent)(nil)).MethodByName("HardInterrupt"); !has {
		t.Fatal("GatewayAgent has no HardInterrupt")
	}
}

func TestSessionUsageEventFeedsMainsStatusBarAttributes(t *testing.T) {
	a, _ := makeScriptedAgent(t, &scriptedClient{script: []any{
		scriptFrame("session.usage", map[string]any{"usage": sessionUsage(nil)}, ""), completeFrame("x")}}, gatewayAgentOptions{})
	runTurn(t, a, "q")
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.counters
	if c["session_input_tokens"] != 500 || c["session_output_tokens"] != 80 || c["session_total_tokens"] != 580 ||
		c["session_prompt_tokens"] != 500 || c["session_completion_tokens"] != 80 || c["session_api_calls"] != 5 {
		t.Fatalf("counters %v", c)
	}
	comp := a.compressor
	if comp.lastPromptTokens != 229 || comp.contextLength != 128000 {
		t.Fatalf("compressor %+v", comp)
	}
	if comp.configContextLength == nil || !agent.IsContextPinned(comp.contextLength, *comp.configContextLength) {
		t.Fatalf("compressor pin %v", comp.configContextLength)
	}
	if a.configContextLength == nil || !agent.IsContextPinned(comp.contextLength, *a.configContextLength) {
		t.Fatalf("agent pin %v", a.configContextLength)
	}
	if comp.lastRealPromptTokens == nil || *comp.lastRealPromptTokens == comp.lastPromptTokens {
		t.Fatalf("estimate marker %v", comp.lastRealPromptTokens)
	}
	if len(a.apiLatencyHistory) != 1 {
		t.Fatalf("latency history %v", a.apiLatencyHistory)
	}
	if rate := a.apiOutputHistory[0] / a.apiLatencyHistory[0]; int(rate+0.5) != 116 {
		t.Fatalf("rate %v", rate)
	}
	if a.rateLimitState != nil {
		t.Fatalf("rate limit state %v", a.rateLimitState)
	}
}

func TestGatewaySessionUsageFrameFeedsTheStatusBarMidTurn(t *testing.T) {
	usage := sessionUsage(map[string]any{"compression_count": 2})
	type reading struct {
		cached      map[string]any
		calls       int
		length      int
		compression int
		latency     []float64
	}
	var mu sync.Mutex
	var seen []reading
	published := wireValue(t, usage).(*pyjson.Object)
	a, _ := makeScriptedAgent(t, &scriptedClient{script: []any{
		scriptFrame("session.usage", pyjson.Plain(gateway.UsageFrame(published)).(map[string]any), ""), completeFrame("x")}}, gatewayAgentOptions{})
	a.mu.Lock()
	a.compressor.contextLength = 0
	a.mu.Unlock()
	state := a.state()
	state.Lock()
	watch := gatewaycli.SessionListener(func(_ context.Context, cached map[string]any) {
		a.mu.Lock()
		defer a.mu.Unlock()
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, reading{copyMap(cached), a.counters["session_api_calls"], a.compressor.contextLength,
			a.compressor.compressionCount, append([]float64{}, a.apiLatencyHistory...)})
	})
	state.UsageListeners = append(state.UsageListeners, &watch)
	state.Unlock()
	runTurn(t, a, "q")
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the listener saw no frame")
	}
	first := seen[0]
	if first.calls != 5 || first.length != 128000 || first.compression != 2 || !reflect.DeepEqual(first.latency, []float64{0.2}) {
		t.Fatalf("bar %+v", first)
	}
	if _, nested := first.cached["context_max"]; nested || !sameJSON(first.cached["api_calls"], 5) {
		t.Fatalf("cached %v", first.cached)
	}
}

func TestTurnEndReadsSessionStatsCumulative(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{completeFrame("x")},
		result: map[string]any{"final_response": "x", "api_calls": 1, "input_tokens": 100, "output_tokens": 10}}, gatewayAgentOptions{})
	client.answerStats(sessionUsage(map[string]any{"rate_limit": map[string]any{
		"requests_min": map[string]any{"limit": 10, "remaining": 9}, "captured_at": 5.0}}), true)
	runTurn(t, a, "q")
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.counters["session_input_tokens"] != 500 || a.counters["session_api_calls"] != 5 {
		t.Fatalf("counters %v", a.counters)
	}
	if a.rateLimitState == nil || !a.rateLimitState.HasData() || a.rateLimitState.RequestsMin.Limit != 10 {
		t.Fatalf("rate limit %+v", a.rateLimitState)
	}
}

// Anchor base count of the agent, minus one when it holds no anchor
func anchorBaseCount(a *GatewayAgent) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.turnBaseUsageAnchor == nil {
		return -1
	}
	return a.turnBaseUsageAnchor.BaseCount
}

func TestStatusBarContextIsTheGatewaysAnchoredFigureWithMainsEstimateMarker(t *testing.T) {
	usage := sessionUsage(map[string]any{"context_tokens": 700, "api_calls": 2})
	suffix := []any{
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
			"function": map[string]any{"name": "read_file", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": strings.Repeat("x", 400)},
		map[string]any{"role": "assistant", "content": "done"}}
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{completeFrame("done")},
		result: map[string]any{"final_response": "done", "api_calls": 2, "messages": suffix}}, gatewayAgentOptions{})
	client.answerStats(usage, false)
	runTurn(t, a, "read it")
	a.mu.Lock()
	msgs, anchor := a.sessionMessages, a.turnBaseUsageAnchor
	a.mu.Unlock()
	if anchor == nil {
		t.Fatal("no anchor after the turn")
	}
	if got, ok := provider.AnchoredContextTokens(context.Background(), agent.RowsToMessages(msgs), anchor, false); !ok || got != 700 {
		t.Fatalf("anchored %d %v", got, ok)
	}
	if len(msgs) <= anchor.BaseCount+1 {
		t.Fatalf("rows %d base %d", len(msgs), anchor.BaseCount)
	}
	b, client2 := makeScriptedAgent(t, &scriptedClient{script: []any{
		scriptFrame("message.complete", map[string]any{"text": "", "outcome": "cancelled", "status": "interrupted"})}}, gatewayAgentOptions{})
	client2.answerStats(usage, false)
	b.mu.Lock()
	b.counters["session_api_calls"] = 2
	b.mu.Unlock()
	runTurn(t, b, "story", map[string]any{"role": "user", "content": "a"}, map[string]any{"role": "assistant", "content": "b"})
	if got := anchorBaseCount(b); got != 2 {
		t.Fatalf("interrupted base count %d", got)
	}
}

func TestFirstToolOfABatchReadsSessionStatsForMainsSpinnerTokenFlow(t *testing.T) {
	type reading struct {
		kind   string
		output int
	}
	var seen []reading
	var a *GatewayAgent
	client := &scriptedClient{script: []any{
		scriptFrame("tool.start", map[string]any{"tool_id": "c1", "name": "terminal", "args": map[string]any{"command": "ls"}}),
		scriptFrame("tool.start", map[string]any{"tool_id": "c2", "name": "terminal", "args": map[string]any{"command": "pwd"}}),
		scriptFrame("tool.complete", map[string]any{"tool_id": "c1", "name": "terminal", "args": map[string]any{}, "result": "{}"}),
		scriptFrame("tool.complete", map[string]any{"tool_id": "c2", "name": "terminal", "args": map[string]any{}, "result": "{}"}),
		completeFrame("x")}}
	a, _ = makeScriptedAgent(t, client, gatewayAgentOptions{ToolProgressCallback: func(kind, _, _ string, _ map[string]any, _ map[string]any) {
		a.mu.Lock()
		defer a.mu.Unlock()
		seen = append(seen, reading{kind, a.counters["session_output_tokens"]})
	}})
	client.answerStats(sessionUsage(map[string]any{"output_tokens": 10}), true)
	runTurn(t, a, "q")
	if len(seen) == 0 || seen[0] != (reading{"tool.started", 10}) {
		t.Fatalf("seen %v", seen)
	}
	if got := len(client.callsOf("session.stats")); got != 2 {
		t.Fatalf("session.stats calls %d", got)
	}
}

func TestMoaReferenceMapsIndexAndCountToMainsKwargs(t *testing.T) {
	var seen []progressCall
	a, _ := makeScriptedAgent(t, &scriptedClient{script: []any{
		scriptFrame("moa.reference", map[string]any{"label": "gpt-x", "text": "ref", "index": 1, "count": 2}), completeFrame("x")}},
		gatewayAgentOptions{ToolProgressCallback: func(eventType, name, preview string, args map[string]any, extra map[string]any) {
			seen = append(seen, progressCall{[]any{eventType, name, preview}, extra})
		}})
	runTurn(t, a, "q")
	if len(seen) == 0 || !reflect.DeepEqual(seen[0].args, []any{"moa.reference", "gpt-x", "ref"}) {
		t.Fatalf("seen %v", seen)
	}
	if !sameJSON(seen[0].extra, map[string]any{"moa_index": 1, "moa_count": 2}) {
		t.Fatalf("extra %v", seen[0].extra)
	}
}

func TestAgentFollowsMainsSessionIdSwitch(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{})
	other := a.connection.State("local-other")
	other.Lock()
	other.Info = map[string]any{"model": "other-model"}
	other.Unlock()
	a.SetSessionID("local-other")
	if a.SessionID() != "local-other" {
		t.Fatalf("session id %q", a.SessionID())
	}
	resumed := false
	for _, params := range client.callsOf("session.resume") {
		resumed = resumed || sameJSON(params, map[string]any{"session_id": "local-other"})
	}
	if !resumed {
		t.Fatalf("resume calls %v", client.callsOf("session.resume"))
	}
	other.ApplyUsage(context.Background(), map[string]any{"api_calls": 3, "input_tokens": 9})
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.counters["session_api_calls"] != 3 {
		t.Fatalf("api calls %d", a.counters["session_api_calls"])
	}
}

func TestTurnAfterARewindAnchorsOnItsFirstResponse(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{script: []any{completeFrame("done")},
		result: map[string]any{"final_response": "done", "api_calls": 1}}, gatewayAgentOptions{})
	client.answerStats(sessionUsage(map[string]any{"context_tokens": 120, "api_calls": 1}), false)
	a.mu.Lock()
	a.counters["session_api_calls"] = 4
	a.mu.Unlock()
	runTurn(t, a, "again", map[string]any{"role": "user", "content": "a"}, map[string]any{"role": "assistant", "content": "b"})
	a.mu.Lock()
	rows := len(a.sessionMessages)
	a.mu.Unlock()
	if got := anchorBaseCount(a); got != rows-1 {
		t.Fatalf("base count %d of %d rows", got, rows)
	}
}

// Parameters of every config.set call for one key
func (c *scriptedClient) configSets(key string) []map[string]any {
	var out []map[string]any
	for _, params := range c.callsOf("config.set") {
		if key == "" || params["key"] == key {
			out = append(out, params)
		}
	}
	return out
}

func TestVerboseModeReachesTheGatewaySession(t *testing.T) {
	a, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{})
	a.SetToolProgressMode("verbose")
	a.SetToolProgressMode("verbose")
	a.SetToolProgressMode("off")
	want := []any{map[string]any{"session_id": scriptSID, "key": "verbose", "value": "verbose"},
		map[string]any{"session_id": scriptSID, "key": "verbose", "value": "off"}}
	if sets := client.configSets("verbose"); !sameJSON(sets, want) {
		t.Fatalf("sets %v", sets)
	}
}

func TestANonDefaultVerboseModeIsSentWhenTheAgentIsBuilt(t *testing.T) {
	_, client := makeScriptedAgent(t, &scriptedClient{}, gatewayAgentOptions{ToolProgressMode: "new"})
	sent := false
	for _, params := range client.configSets("") {
		sent = sent || sameJSON(params, map[string]any{"session_id": scriptSID, "key": "verbose", "value": "new"})
	}
	if !sent {
		t.Fatalf("config.set calls %v", client.configSets(""))
	}
}
