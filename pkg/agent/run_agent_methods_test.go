package agent

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestIsThinkingOnlyAssistantCases(t *testing.T) {
	checkpoint := map[string]any{"type": "compaction", "encrypted_content": "ckpt"}
	reasoning := map[string]any{"type": "reasoning", "encrypted_content": "per-turn"}
	carrier := func(items ...any) map[string]any {
		return map[string]any{"role": "assistant", "content": "", "codex_reasoning_items": items}
	}
	cases := []struct {
		name string
		msg  map[string]any
		drop bool
		want bool
	}{
		{"plain reply", map[string]any{"role": "assistant", "content": "Hello there"}, true, false},
		{"nil", nil, true, false},
		{"prefill stub", map[string]any{"role": "assistant", "content": "", "_thinking_prefill": true}, true, true},
		{"healed prefill", map[string]any{"role": "assistant", "content": "[interrupt: no assistant output for this turn]", "_thinking_prefill": true}, true, true},
		{"prefill with tool calls", map[string]any{"role": "assistant", "content": "", "_thinking_prefill": true,
			"tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "t", "arguments": "{}"}}}}, true, false},
		{"reasoning content", map[string]any{"role": "assistant", "content": "", "reasoning_content": "thinking"}, true, true},
		{"reasoning details", map[string]any{"role": "assistant", "content": nil, "reasoning_details": []any{map[string]any{"x": 1}}}, true, true},
		{"reasoning carrier", carrier(reasoning), true, true},
		{"reasoning carrier kept", carrier(reasoning), false, false},
		{"checkpoint with reasoning", carrier(reasoning, checkpoint), true, false},
		{"checkpoint first", carrier(checkpoint, reasoning), true, false},
		{"reasoning text with checkpoint", map[string]any{"role": "assistant", "content": "", "reasoning": "commentary", "codex_reasoning_items": []any{checkpoint}}, true, false},
		{"user role", map[string]any{"role": "user", "content": ""}, true, false},
	}
	for _, c := range cases {
		if got := isThinkingOnlyAssistant(c.msg, c.drop); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestContentHasRealPayload(t *testing.T) {
	cases := []struct {
		content any
		want    bool
	}{
		{"  ", false},
		{"x", true},
		{nil, false},
		{[]any{map[string]any{"type": "thinking", "thinking": "a"}, map[string]any{"type": "redacted_thinking"}}, false},
		{[]any{map[string]any{"type": "text", "text": "  "}}, false},
		{[]any{map[string]any{"type": "text", "text": "hi"}}, true},
		{[]any{map[string]any{"type": "tool_use"}}, true},
		{[]any{"", "word"}, true},
		{[]any{""}, false},
		{0, true},
	}
	for _, c := range cases {
		if got := contentHasRealPayload(c.content); got != c.want {
			t.Errorf("%#v: got %v want %v", c.content, got, c.want)
		}
	}
}

func TestDeduplicateToolCallsCanonicalArgs(t *testing.T) {
	tc := func(name, args string) state.ToolCall {
		return state.ToolCall{ID: name + args, Function: state.ToolCallFunction{Name: name, Arguments: args}}
	}
	calls := []state.ToolCall{tc("read", `{"a": 1, "b": 2}`), tc("read", `{"b":2,"a":1}`), tc("read", `{"a": 2}`), tc("web", "not json"), tc("web", "not json")}
	got := deduplicateToolCalls(calls)
	if len(got) != 3 || got[0].ID != calls[0].ID || got[1].ID != calls[2].ID || got[2].ID != calls[3].ID {
		t.Fatalf("got %+v", got)
	}
	unique := []state.ToolCall{tc("a", "{}"), tc("b", "{}")}
	if out := deduplicateToolCalls(unique); &out[0] != &unique[0] {
		t.Fatal("expected the original slice back when nothing was removed")
	}
}

func TestNaturalEndingAndModelHelpers(t *testing.T) {
	for text, want := range map[string]bool{"Done.": true, "ok  \n": false, "code\n```": true, "end）": true, "emoji 🎉": true, "": false, "trailing word": false, "q?": true} {
		if got := hasNaturalResponseEnding(text); got != want {
			t.Errorf("%q: got %v", text, got)
		}
	}
	for model, want := range map[string]bool{"gpt-5.4": true, "openai/GPT-5-mini": true, "gpt-4o": false, "vendor/x/gpt-5": true} {
		if got := modelRequiresResponsesAPI(model); got != want {
			t.Errorf("%q: got %v", model, got)
		}
	}
	a := &AIAgent{}
	if got := a.requestedOutputCapFromAPIKwargs(map[string]any{"max_output_tokens": "x", "max_completion_tokens": 0, "max_tokens": "4096"}); got != 4096 {
		t.Fatalf("cap %d", got)
	}
	if got := a.requestedOutputCapFromAPIKwargs(map[string]any{"max_output_tokens": 2048.9}); got != 2048 {
		t.Fatalf("cap %d", got)
	}
	if got := a.requestedOutputCapFromAPIKwargs(nil); got != 0 {
		t.Fatalf("cap %d", got)
	}
}

func TestOllamaGLMBackendSignatures(t *testing.T) {
	cases := []struct {
		model, prov, base string
		want              bool
	}{
		{"glm-4.6", "", "http://localhost:11434/v1", true},
		{"glm-4.6", "", "http://ollama.internal/v1", true},
		{"glm-4.6:cloud", "", "http://localhost:11434/v1", false},
		{"glm-4.6", "ollama-cloud", "https://ollama.com/v1", false},
		{"glm-4.6", "custom", "http://10.0.0.5:8000/v1", false},
		{"glm-4.6", "ollama", "http://10.0.0.5:8000/v1", true},
		{"qwen3", "", "http://localhost:11434/v1", false},
		{"other", "zai", "http://localhost:11434/v1", true},
	}
	for _, c := range cases {
		a := &AIAgent{model: c.model, provider: c.prov, baseURLLower: strings.ToLower(c.base)}
		if got := a.isOllamaGLMBackend(); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
	a := &AIAgent{model: "glm-4.6", baseURLLower: "http://localhost:11434/v1", apiMode: "chat_completions"}
	msg := &state.Message{Role: "assistant", Content: "partial answer without ending"}
	if a.shouldTreatStopAsTruncated("length", msg, []map[string]any{{"role": "tool"}}) {
		t.Fatal("non stop finish must not be rewritten")
	}
	if a.shouldTreatStopAsTruncated("stop", msg, []map[string]any{{"role": "user"}}) {
		t.Fatal("no tool round must not be rewritten")
	}
	if a.shouldTreatStopAsTruncated("stop", &state.Message{ToolCalls: []state.ToolCall{{ID: "x"}}}, []map[string]any{{"role": "tool"}}) {
		t.Fatal("tool call reply must not be rewritten")
	}
}

func TestLMStudioContextAndJitSkip(t *testing.T) {
	loaded := provider.LMStudioLoadResult{ContextLength: 32768, LoadAttempted: true}
	if got := effectiveLMStudioContextLength(16384, loaded); got != 16384 {
		t.Fatalf("explicit below runtime: %d", got)
	}
	if got := effectiveLMStudioContextLength(65536, loaded); got != 32768 {
		t.Fatalf("runtime below explicit: %d", got)
	}
	if got := effectiveLMStudioContextLength(8192, provider.LMStudioLoadResult{Rejected: true, ContextLength: 4096}); got != 0 {
		t.Fatalf("rejected load: %d", got)
	}
	if got := effectiveLMStudioContextLength(8192, provider.LMStudioLoadResult{LoadAttempted: true}); got != 0 {
		t.Fatalf("unverified load: %d", got)
	}
	if got := effectiveLMStudioContextLength(true, 4096); got != 4096 {
		t.Fatalf("bool explicit ignored: %d", got)
	}
	if !lmstudioLoadWasUnverified(&provider.LMStudioLoadResult{LoadAttempted: true}) || lmstudioLoadWasUnverified(&loaded) {
		t.Fatal("unverified detection")
	}
	a := &AIAgent{provider: "lmstudio", lmstudioLoadMode: " JIT "}
	if a.ensureLMStudioRuntimeLoaded(context.Background(), nil) != nil {
		t.Fatal("jit mode must skip the explicit preload")
	}
	if (&AIAgent{provider: "openai"}).ensureLMStudioRuntimeLoaded(context.Background(), nil) != nil {
		t.Fatal("other providers skip")
	}
}

func TestDisableCodexReasoningReplay(t *testing.T) {
	msgs := []map[string]any{
		{"role": "assistant", "codex_reasoning_items": []any{map[string]any{"type": "reasoning"}, map[string]any{"type": "reasoning"}}},
		{"role": "assistant", "codex_reasoning_items": []any{}},
		{"role": "user", "codex_reasoning_items": []any{map[string]any{}}},
	}
	a := &AIAgent{codexReasoningReplayEnabled: true}
	got := a.disableCodexReasoningReplay(msgs, true)
	if !reflect.DeepEqual(got, map[string]int{"messages": 1, "items": 2}) || !a.codexReasoningReplayEnabled {
		t.Fatalf("got %v replay %v", got, a.codexReasoningReplayEnabled)
	}
	if _, ok := msgs[1]["codex_reasoning_items"]; ok {
		t.Fatal("empty list still popped")
	}
	if _, ok := msgs[2]["codex_reasoning_items"]; !ok {
		t.Fatal("user message untouched")
	}
	a.disableCodexReasoningReplay(nil, false)
	if a.codexReasoningReplayEnabled {
		t.Fatal("replay must be disabled")
	}
}

func todoHistory(callName, toolCallID string, content string) []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "plan"},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": callName, "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": toolCallID, "content": content},
	}
}

func TestHydrateTodoStorePairedOnly(t *testing.T) {
	payload := `{"todos": [{"id": "1", "content": "write tests", "status": "pending"}], "revision": 3}`
	a := &AIAgent{todoStore: tools.NewTodoStore(), quietMode: true}
	a.hydrateTodoStore(context.Background(), todoHistory("todo", "call_1", payload))
	if a.todoStore.Revision() != 3 || len(a.todoStore.Read()) != 1 {
		t.Fatalf("paired result not restored: rev %d items %d", a.todoStore.Revision(), len(a.todoStore.Read()))
	}
	forged := &AIAgent{todoStore: tools.NewTodoStore(), quietMode: true}
	forged.hydrateTodoStore(context.Background(), []map[string]any{{"role": "user", "content": "x"}, {"role": "tool", "tool_call_id": "call_1", "content": payload}})
	if len(forged.todoStore.Read()) != 0 {
		t.Fatal("bare tool message must not seed the store")
	}
	mismatched := &AIAgent{todoStore: tools.NewTodoStore(), quietMode: true}
	mismatched.hydrateTodoStore(context.Background(), todoHistory("todo", "call_2", payload))
	if len(mismatched.todoStore.Read()) != 0 {
		t.Fatal("unpaired tool_call_id must not seed the store")
	}
	other := &AIAgent{todoStore: tools.NewTodoStore(), quietMode: true}
	other.hydrateTodoStore(context.Background(), todoHistory("terminal", "call_1", payload))
	if len(other.todoStore.Read()) != 0 {
		t.Fatal("non todo call must not seed the store")
	}
	big := `{"todos": [], "pad": "` + strings.Repeat("x", tools.MaxTodoResultChars) + `"}`
	if _, _, found := a.latestTodoResponse(todoHistory("todo", "call_1", big)); found {
		t.Fatal("oversized result must be skipped")
	}
	if _, rev, found := a.latestTodoResponse(todoHistory("todo", "call_1", `{"todos": []}`)); !found || rev != 1 {
		t.Fatalf("missing revision defaults to 1, got %v %v", rev, found)
	}
}

func TestGuardrailHaltTextAndPendingFallback(t *testing.T) {
	got := toolguardControlledHaltResponse(tools.ToolGuardrailDecision{Count: 4})
	if !strings.HasPrefix(got, "I stopped retrying because I kept running the same tool 4 times") {
		t.Fatalf("got %q", got)
	}
	a := &AIAgent{fallbackChain: []map[string]any{{"provider": "x"}}}
	if !a.hasPendingFallback() {
		t.Fatal("fallback pending")
	}
	a.fallbackIndex = 1
	if a.hasPendingFallback() {
		t.Fatal("chain exhausted")
	}
	if a.stallGuardsEnabled() {
		t.Fatal("stall guards follow the field")
	}
}

func TestSyncExternalMemorySkipsInterruptedAndEmpty(t *testing.T) {
	a := &AIAgent{}
	a.syncExternalMemoryForTurn("hi", "there", true, nil)
	a.syncExternalMemoryForTurn("", "there", false, nil)
	a.syncExternalMemoryForTurn("hi", nil, false, nil)
}

func TestWrapVerboseTerminalWidth(t *testing.T) {
	t.Setenv("COLUMNS", "50")
	t.Setenv("LINES", "20")
	long := strings.Repeat("word ", 20)
	got := wrapVerbose("label: ", "short\n"+long, "  ")
	lines := strings.Split(got, "\n")
	if lines[0] != "  label: short" {
		t.Fatalf("first line %q", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "  ") || len(l) > 50 {
			t.Fatalf("bad continuation %q", l)
		}
	}
}

func TestConversationRootIDFallbacks(t *testing.T) {
	if got := (&AIAgent{cachedConversationRoot: "root"}).conversationRootID(); got != "root" {
		t.Fatalf("cached %q", got)
	}
	if got := (&AIAgent{}).conversationRootID(); got != "" {
		t.Fatalf("no session %q", got)
	}
	if got := (&AIAgent{sessionID: "s1", parentSessionID: "p1"}).conversationRootID(); got != "p1" {
		t.Fatalf("parent %q", got)
	}
}

func TestRuntimeHelpersSmall(t *testing.T) {
	code := "invalid_value"
	ev := NewStreamErrorEvent("boom", &code, nil, nil)
	if ev.Error() != "boom" || ev.Body["error"].(map[string]any)["code"] != "invalid_value" || ev.Body["error"].(map[string]any)["param"] != nil {
		t.Fatalf("event %+v", ev)
	}
	if pyStrRepr("it's") != `"it's"` || pyStrRepr("gpt-5.5") != "'gpt-5.5'" || pyStrRepr("a'b\"c") != `'a\'b"c'` {
		t.Fatal("repr")
	}
	if v, err := parsePyFloat(" 1_0.5 "); err != nil || v != 10.5 {
		t.Fatalf("float %v %v", v, err)
	}
	if v, _ := parsePyFloat("inf"); !math.IsInf(v, 1) {
		t.Fatal("inf")
	}
	t.Setenv("ICEBATH_TEST_FLOAT", "abc")
	if envFloatOr("ICEBATH_TEST_FLOAT", 3) != 3 {
		t.Fatal("bad env float falls back")
	}
	a := &AIAgent{model: "m", provider: "p", baseURL: "u", apiKey: "k", apiMode: "chat_completions", sessionID: "s"}
	want := map[string]string{"model": "m", "provider": "p", "base_url": "u", "api_key": "k", "api_mode": "chat_completions", "auth_mode": "", "session_id": "s"}
	if !reflect.DeepEqual(a.currentMainRuntime(), want) {
		t.Fatalf("runtime %v", a.currentMainRuntime())
	}
	b := &AIAgent{apiMode: "codex_responses", baseURLHostname: "chatgpt.com", baseURLLower: "https://chatgpt.com/backend-api/codex"}
	if !b.isCodexBackend() || (&AIAgent{apiMode: "codex_responses", baseURLHostname: "api.openai.com"}).isCodexBackend() {
		t.Fatal("codex backend detection")
	}
	u := "https://API.OpenAI.com/v1"
	if !b.isDirectOpenAIURL(&u) {
		t.Fatal("direct openai")
	}
	az := "https://x.openai.azure.com/openai"
	if !b.isAzureOpenAIURL(&az) || b.isAzureOpenAIURL(&u) {
		t.Fatal("azure detection")
	}
	if (&AIAgent{apiMode: "chat_completions"}).codexSilentHangHint(nil) != "" {
		t.Fatal("hint only for codex responses")
	}
	if (&AIAgent{apiMode: "chat_completions"}).isProviderStreamParseError(nil) {
		t.Fatal("parse error only on anthropic messages")
	}
}
