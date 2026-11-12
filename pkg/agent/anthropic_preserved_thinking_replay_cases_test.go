package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

type thinkingRoute struct{ provider, baseURL string }

var (
	routeAnthropic  = thinkingRoute{"anthropic", "https://api.anthropic.com"}
	routeNous       = thinkingRoute{"nous", "https://inference-api.nousresearch.com/v1/messages"}
	routeOpenRouter = thinkingRoute{"openrouter", "https://openrouter.ai/api/v1"}
	routeKimi       = thinkingRoute{"anthropic", "https://api.kimi.com/coding"}
)

func signedTurn(question, answer, sig, thinking string) []map[string]any {
	if thinking == "" {
		thinking = "thought-" + sig
	}
	return []map[string]any{
		{"role": "user", "content": question},
		{"role": "assistant", "content": answer, "reasoning": thinking,
			"reasoning_details": []any{map[string]any{"type": "thinking", "thinking": thinking, "signature": sig}}},
	}
}

// Assistant row carrying the same thinking in every field the stores write
func thinkingCarrier(thinking, opaque string) []map[string]any {
	signed := func() map[string]any {
		return map[string]any{"type": "thinking", "thinking": thinking, "signature": "sig_bad" + strings.Repeat("s", len(opaque))}
	}
	redacted := func() map[string]any { return map[string]any{"type": "redacted_thinking", "data": "red_bad" + opaque} }
	return []map[string]any{
		{"role": "user", "content": "Q"},
		{"role": "assistant", "content": "answer", "reasoning": thinking, "reasoning_content": thinking, "timestamp": 1234567890.0,
			"finish_reason": "tool_calls", "reasoning_details": []any{signed(), redacted()},
			"anthropic_content_blocks": []any{signed(), map[string]any{"type": "text", "text": "answer"},
				map[string]any{"type": "tool_use", "id": "tool_1", "name": "search", "input": map[string]any{"q": "x"}}, redacted()},
			"tool_calls": []any{map[string]any{"id": "tool_1", "type": "function", "function": map[string]any{"name": "search", "arguments": `{"q":"x"}`}}}},
		{"role": "tool", "tool_call_id": "tool_1", "content": "result"},
	}
}

// A turn stored by the real producer, Anthropic response through the transport into the assistant row
func nativeThinkingTurn(t *testing.T, a *AIAgent, question string, size int, sig string) []map[string]any {
	t.Helper()
	resp, err := provider.GetTransport("anthropic_messages").NormalizeResponse(map[string]any{
		"content":     []any{map[string]any{"type": "thinking", "thinking": strings.Repeat("x", size), "signature": sig}, map[string]any{"type": "text", "text": "A"}},
		"stop_reason": "end_turn", "stop_details": nil}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return []map[string]any{{"role": "user", "content": question}, a.buildAssistantMessage(resp, resp.FinishReason)}
}

func thinkingDB(t *testing.T, sessionIDs ...string) *state.SessionDB {
	t.Helper()
	db := restoreRealDB(t, sessionIDs[0], "cli")
	for _, id := range sessionIDs[1:] {
		if err := db.CreateSession(state.NewSession{ID: id, Source: "cli", Model: "claude-opus-4-6"}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// Engine whose context selection hands back a deep copy of the canonical conversation
type canonicalCloneEngine struct{ *ContextCompressor }

func (e *canonicalCloneEngine) SelectContext(_, conversationMessages []map[string]any, _ map[string]any, _ int) []map[string]any {
	data, err := json.Marshal(conversationMessages)
	if err != nil {
		panic(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}

// Anthropic route agent on the store with a bound compressor, its selection cloning the canonical rows when asked
func thinkingAgent(t *testing.T, db *state.SessionDB, sessionID, model string, route thinkingRoute, cloneSelection bool) (*AIAgent, *ContextCompressor) {
	t.Helper()
	a := newFixtureAgent(t, AIAgentOptions{APIMode: "anthropic_messages", Provider: route.provider, Model: model, BaseURL: route.baseURL,
		SessionDB: db, SessionID: sessionID, EnabledToolsets: []string{}})
	t.Cleanup(func() { a.Close() })
	a.persistDisabled, a.ephemeralSystemPrompt, a.prefillMessages, a.tools, a.usePromptCaching, a.usageAnchor = false, "", nil, nil, false, nil
	opts := DefaultContextCompressorOptions(model)
	window := 200_000
	opts.Provider, opts.BaseURL, opts.APIMode, opts.QuietMode, opts.ConfigContextLength = route.provider, route.baseURL, "anthropic_messages", true, &window
	c := NewContextCompressor(context.Background(), opts)
	if err := c.BindSessionState(db, sessionID); err != nil {
		t.Fatal(err)
	}
	a.contextCompressor = c
	if cloneSelection {
		a.contextCompressor = &canonicalCloneEngine{c}
	}
	return a, c
}

func toStateMessages(rows []map[string]any) []*state.Message {
	out := make([]*state.Message, len(rows))
	for i, r := range rows {
		out[i] = state.MessageFromDict(r)
	}
	return out
}

func anthropicWire(t *testing.T, rows []map[string]any, baseURL, model string) []map[string]any {
	t.Helper()
	out, err := provider.GetTransport("anthropic_messages").ConvertMessages(rows, map[string]any{"base_url": baseURL, "model": model})
	if err != nil {
		t.Fatal(err)
	}
	return out.([]any)[1].([]map[string]any)
}

func wireBlocks(wire []map[string]any, keep func(string) bool) []map[string]any {
	var out []map[string]any
	for _, m := range wire {
		if m["role"] != "assistant" {
			continue
		}
		var blocks []any
		switch content := m["content"].(type) {
		case []any:
			blocks = content
		case []map[string]any:
			for _, b := range content {
				blocks = append(blocks, b)
			}
		}
		for _, raw := range blocks {
			if b, ok := raw.(map[string]any); ok && keep(fmt.Sprint(b["type"])) {
				out = append(out, b)
			}
		}
	}
	return out
}

func wireThinkingBlocks(wire []map[string]any) []map[string]any {
	return wireBlocks(wire, func(kind string) bool { return kind == "thinking" || kind == "redacted_thinking" })
}

func wireTextBlocks(wire []map[string]any) []map[string]any {
	return wireBlocks(wire, func(kind string) bool { return kind == "text" })
}

// Production signature rejection recovery, the canonical history never mutated
func rejectThinkingSignatures(t *testing.T, a *AIAgent, history []map[string]any) {
	t.Helper()
	before, request := deepCopyRows(t, history), deepCopyRows(t, history)
	canonical := toStateMessages(history)
	retry := &TurnRetryState{}
	apiMessages := anyRows(request)
	ok, err := recoverFormatErrors(a, errors.New("Invalid signature in thinking block"), &provider.Classified{Reason: provider.ReasonThinkingSignature}, retry, canonical, apiMessages)
	if err != nil || !ok {
		t.Fatalf("recovery %v %v", ok, err)
	}
	for i, m := range canonical {
		if !retry.ThinkingSigRetryAttempted || !persistJSONEqual(t, state.MessageToDict(m), state.MessageToDict(state.MessageFromDict(before[i]))) || !persistJSONEqual(t, history[i], before[i]) {
			t.Fatalf("canonical row %d changed %v", i, history[i])
		}
	}
	if blocks := wireThinkingBlocks(anthropicWire(t, mapRows(apiMessages), "", a.model)); len(blocks) != 0 {
		t.Fatalf("rejected thinking still on the repaired request %v", blocks)
	}
}

// Production request assembly then the native converter, the loop state and the wire messages
func assembleAndWire(t *testing.T, a *AIAgent, history []map[string]any) (*LoopState, []map[string]any) {
	t.Helper()
	s := &LoopState{Messages: toStateMessages(deepCopyRows(t, history)), CurrentTurnUserIdx: len(history) - 1, OriginalUserMessage: history[len(history)-1]["content"]}
	if err := assembleAPIRequest(context.Background(), a, s); err != nil {
		t.Fatal(err)
	}
	return s, anthropicWire(t, deepCopyRows(t, mapRows(s.APIMessages)), a.baseURL, a.model)
}

func concatRows(parts ...[]map[string]any) []map[string]any {
	var out []map[string]any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

type thinkingAccountingCase struct {
	model    string
	route    thinkingRoute
	replayed int
}

var preservedThinkingModels = []struct {
	model     string
	preserved bool
}{
	{"claude-opus-4-4", false}, {"claude-opus-4-5", true}, {"claude-opus-4-6", true},
	{"claude-sonnet-4-5", false}, {"claude-sonnet-4-6", true}, {"claude-haiku-4-5", false},
	{"claude-opus-4-20250514", false}, {"claude-sonnet-4-20250514", false},
	{"claude-opus-4-5-20251101", true}, {"claude-opus-4-6-20250414", true},
	{"claude-sonnet-4-5-20250929", false}, {"claude-opus-5", true}, {"claude-sonnet-5-5", true},
	{"claude-fable-5-1", true}, {"claude-mythos-5", true}, {"claude-mythos-preview", true},
	{"claude-fable-5-2", true}, {"claude-mythos-6", true}, {"claude-newfamily-7", true},
	{"claude-haiku-5", false}, {"claude-3-7-sonnet-20250219", false}, {"claude-sonnet-4", false},
	{"hermes-4-405b", false},
}

func thinkingAccountingCases() ([]string, map[string]thinkingAccountingCase) {
	var names []string
	cases := map[string]thinkingAccountingCase{}
	add := func(name string, c thinkingAccountingCase) {
		names = append(names, name)
		cases[name] = c
	}
	for _, p := range preservedThinkingModels {
		replayed := 0
		if p.preserved {
			replayed = 1
		}
		add("route:"+p.model, thinkingAccountingCase{p.model, routeAnthropic, replayed})
	}
	add("route:nous", thinkingAccountingCase{"claude-opus-4-6", routeNous, 1})
	add("route:openrouter", thinkingAccountingCase{"claude-opus-4-6", routeOpenRouter, 0})
	add("producer", thinkingAccountingCase{"claude-opus-4-6", routeAnthropic, 2})
	for name, replayed := range map[string]int{"context_selection_clone": 1, "usage_anchor": 1, "carrier_dedupe": 1, "opaque_bytes": 0, "rejected": 0,
		"rejected_unpersisted": 0, "reasoning_only": 0, "invalid_ordered_then_details": 1, "assistant_api_content": 1} {
		cases[name] = thinkingAccountingCase{"claude-opus-4-6", routeAnthropic, replayed}
	}
	names = append(names, "context_selection_clone", "usage_anchor", "carrier_dedupe", "opaque_bytes", "rejected", "rejected_unpersisted",
		"reasoning_only", "invalid_ordered_then_details", "assistant_api_content")
	return names, cases
}

func TestAnthropicPreservedThinkingReplay(t *testing.T) {
	caseHome(t)
	t.Run("test_estimates_charge_exactly_the_thinking_the_wire_replays", func(t *testing.T) {
		names, cases := thinkingAccountingCases()
		for _, name := range names {
			tc := cases[name]
			t.Run(name, func(t *testing.T) { runThinkingAccountingCase(t, name, tc) })
		}
	})
	t.Run("test_rejected_thinking_never_returns_and_nothing_else_is_suppressed", func(t *testing.T) {
		for _, boundary := range []string{"same_agent", "rebuild", "session_switch", "compression_child", "context_selection",
			"carrier", "unfingerprintable", "kimi_route", "future_family"} {
			t.Run(boundary, func(t *testing.T) { runRejectedThinkingBoundary(t, boundary) })
		}
	})
}

// Growing thinking moves the wire, preflight, tail walk and assembled pressure by the same amount
func runThinkingAccountingCase(t *testing.T, name string, tc thinkingAccountingCase) {
	a, compressor := thinkingAgent(t, thinkingDB(t, "s1"), "s1", tc.model, tc.route, name == "context_selection_clone")
	a.persistDisabled = name == "rejected_unpersisted"
	if name == "usage_anchor" {
		prev := assemblyAnchoredContextTokens
		assemblyAnchoredContextTokens = func(context.Context, []*state.Message, *provider.UsageAnchor, bool) (int, bool) { return 1234, true }
		t.Cleanup(func() { assemblyAnchoredContextTokens = prev })
		a.usageAnchor = &provider.UsageAnchor{}
	}
	history := func(size int) []map[string]any {
		var body []map[string]any
		switch name {
		case "producer":
			body = concatRows(nativeThinkingTurn(t, a, "Q1", size, "sig_1"), nativeThinkingTurn(t, a, "Q2", size, "sig_2"))
		case "carrier_dedupe":
			body = thinkingCarrier(strings.Repeat("x", size), "")
		case "opaque_bytes":
			body = thinkingCarrier(strings.Repeat("x", 8000), strings.Repeat("r", size))
		default:
			body = signedTurn("Q1", "A1", "sig_1", strings.Repeat("x", size))
			switch name {
			case "reasoning_only":
				body[1] = map[string]any{"role": "assistant", "content": "A1", "reasoning": strings.Repeat("x", size)}
			case "invalid_ordered_then_details":
				body[1]["anthropic_content_blocks"] = []any{map[string]any{"type": "redacted_thinking"}}
			case "assistant_api_content":
				body = signedTurn("Q1", "", "sig_1", strings.Repeat("t", 4000))
				body[1]["api_content"] = strings.Repeat("x", size)
			}
			body = concatRows(body, signedTurn("Q2", "A2", "sig_2", ""))
		}
		return concatRows(body, []map[string]any{{"role": "user", "content": "continue"}})
	}
	if strings.HasPrefix(name, "rejected") {
		rejectThinkingSignatures(t, a, history(1))
	}
	measure := func(size int) map[string]int {
		canonical := history(size)
		preflight := preflightRequestTokens(context.Background(), a, toStateMessages(deepCopyRows(t, canonical)), "")
		s, wire := assembleAndWire(t, a, canonical)
		replayed := 0
		for _, b := range wireThinkingBlocks(wire) {
			if b["type"] == "thinking" {
				replayed += provider.EstimateTokens(fmt.Sprint(b["thinking"]))
			} else {
				text, _ := b["text"].(string)
				replayed += provider.EstimateTokens(text)
			}
		}
		for _, b := range wireTextBlocks(wire) {
			text, _ := b["text"].(string)
			replayed += provider.EstimateTokens(text)
		}
		_, tail := compressor.walkTailBudget(canonical, 0, 1_000_000_000, 0, false, nil)
		return map[string]int{"wire": replayed, "preflight": preflight, "tail_walk": tail, "assembled": s.ApproxTokens, "pressure": s.RequestPressureTokens}
	}
	small, large := measure(1), measure(8000)
	delta := map[string]int{}
	for k := range small {
		delta[k] = large[k] - small[k]
	}
	expected := tc.replayed * (provider.EstimateTokens(strings.Repeat("x", 8000)) - provider.EstimateTokens("x"))
	if name == "usage_anchor" {
		if small["pressure"] != 1234 || large["pressure"] != 1234 || !a.requestPressureAnchored {
			t.Fatalf("anchored pressure %d %d anchored %v", small["pressure"], large["pressure"], a.requestPressureAnchored)
		}
		delta["pressure"] = delta["assembled"]
	}
	for _, v := range delta {
		if v != expected {
			t.Fatalf("delta %v want every key %d (small %v large %v)", delta, expected, small, large)
		}
	}
	if !strings.HasPrefix(name, "route:") {
		return
	}
	if got := staleThinkingReachesWire(a.apiMode, tc.route.provider, tc.model, tc.route.baseURL); got != (tc.replayed > 0) {
		t.Fatalf("stale thinking reaches wire %v", got)
	}
	full := history(1)
	prefix := full[:len(full)-1]
	short := anthropicWire(t, prefix, tc.route.baseURL, tc.model)
	longer := anthropicWire(t, concatRows(prefix, signedTurn("Q3", "A3", "sig_3", "")), tc.route.baseURL, tc.model)
	if tc.replayed > 0 {
		if !reflect.DeepEqual(longer[3], short[3]) {
			t.Fatalf("formerly latest turn changed\n%v\n%v", longer[3], short[3])
		}
	} else if blocks := wireThinkingBlocks([]map[string]any{longer[3]}); len(blocks) != 0 {
		t.Fatalf("formerly latest turn kept thinking %v", blocks)
	}
}

// Once a signature is rejected every later request replays only never rejected signed thinking
func runRejectedThinkingBoundary(t *testing.T, boundary string) {
	ctx := context.Background()
	db := thinkingDB(t, "s1", "s2")
	clone := boundary == "context_selection"
	var a *AIAgent
	switch boundary {
	case "kimi_route":
		a, _ = thinkingAgent(t, db, "s1", "kimi-k2.5", routeKimi, clone)
	case "future_family":
		a, _ = thinkingAgent(t, db, "s1", "claude-fable-5-2", routeAnthropic, clone)
	default:
		a, _ = thinkingAgent(t, db, "s1", "claude-opus-4-6", routeAnthropic, clone)
	}
	a.usageAnchor = &provider.UsageAnchor{PromptTokens: 999}
	rejected := concatRows(signedTurn("Q1", "A1", "sig_rejected_1", ""), signedTurn("Q2", "A2", "sig_rejected_2", ""))
	switch boundary {
	case "unfingerprintable":
		rejected = []map[string]any{{"role": "user", "content": "Q1"}, {"role": "assistant", "content": "A1"}}
	case "carrier":
		rejected = thinkingCarrier("secret chain", "")
	}
	rejectThinkingSignatures(t, a, rejected)
	if boundary != "kimi_route" && a.usageAnchor != nil {
		t.Fatalf("stale usage anchor survived the rejection %v", a.usageAnchor)
	}
	reader := a
	switch boundary {
	case "rebuild", "context_selection", "carrier":
		reader, _ = thinkingAgent(t, db, "s1", "claude-opus-4-6", routeAnthropic, clone)
	case "session_switch":
		rejected = signedTurn("Q1", "A1", "sig_rejected_s2", "")
		other, _ := thinkingAgent(t, db, "s2", "claude-opus-4-6", routeAnthropic, clone)
		rejectThinkingSignatures(t, other, rejected)
		a.sessionID = "s2"
	}
	later := concatRows(rejected, signedTurn("Q3", "A3", "sig_kept", ""), signedTurn("Q4", "A4", "sig_new", ""), []map[string]any{{"role": "user", "content": "continue"}})
	if boundary == "compression_child" {
		model := a.model
		if err := db.PublishCompressionChild(ctx, state.CompressionChildPublish{ParentSessionID: "s1", ChildSessionID: "s1-child", Source: "cli",
			Model: &model, ModelConfig: pyjson.NewObject(), Messages: later[2:]}); err != nil {
			t.Fatal(err)
		}
		a.sessionID = "s1-child"
		carrySessionStateToChild(ctx, a, "s1", "")
		reader, _ = thinkingAgent(t, db, "s1-child", "claude-opus-4-6", routeAnthropic, clone)
		later = later[2:]
	}
	_, wire := assembleAndWire(t, reader, later)
	want := []any{"sig_kept", "sig_new"}
	if boundary == "kimi_route" {
		want = append([]any{"sig_rejected_1", "sig_rejected_2"}, want...)
	}
	var got []any
	for _, b := range wireThinkingBlocks(wire) {
		got = append(got, b["signature"])
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed signatures %v want %v", got, want)
	}
	rendered := fmt.Sprint(wire)
	if strings.Contains(rendered, "tool_use") != (boundary == "carrier") {
		t.Fatalf("tool_use on the wire %v for %s", strings.Contains(rendered, "tool_use"), boundary)
	}
	if strings.Contains(rendered, "secret chain") {
		t.Fatal("rejected carrier thinking reached the wire")
	}
}
