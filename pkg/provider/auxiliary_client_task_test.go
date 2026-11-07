package provider

import (
	"context"
	"testing"
)

func TestGetTaskMaxConcurrency(t *testing.T) {
	cases := []struct {
		yaml, task string
		want       int
		ok         bool
	}{
		{"auxiliary:\n  vision:\n    max_concurrency: 1\n", "vision", 0, false},
		{"auxiliary:\n  compression:\n    max_concurrency: 3\n", "compression", 3, true},
		{"auxiliary:\n  compression:\n    max_concurrency: not-a-number\n", "compression", 0, false},
		{"auxiliary:\n  compression:\n    max_concurrency: 0\n", "compression", 0, false},
		{"auxiliary:\n  compression:\n    max_concurrency: -2\n", "compression", 0, false},
	}
	for _, c := range cases {
		got, ok, err := auxGetTaskMaxConcurrency(auxMainTestHome(t, c.yaml), c.task)
		if err != nil || got != c.want || ok != c.ok {
			t.Fatalf("%q %s: got %d %v %v", c.yaml, c.task, got, ok, err)
		}
	}
}

func TestSemaphoreCacheSyncAsyncAndRebuild(t *testing.T) {
	auxResetAuxSemaphores()
	t.Cleanup(auxResetAuxSemaphores)
	if s, err := auxAcquireSyncAuxSemaphore(auxMainTestHome(t, ""), "title_generation"); s != nil || err != nil {
		t.Fatalf("unset limit gave %v %v", s, err)
	}
	ctx := auxMainTestHome(t, "auxiliary:\n  compression:\n    max_concurrency: 2\n")
	if s, _ := auxAcquireAsyncAuxSemaphore(ctx, "compression"); s != nil {
		t.Fatal("async semaphore without a running loop")
	}
	s1, _ := auxAcquireSyncAuxSemaphore(ctx, "compression")
	s2, _ := auxAcquireSyncAuxSemaphore(ctx, "compression")
	if s1 == nil || s1 != s2 || s1.limit != 2 {
		t.Fatalf("sync semaphore not cached %v %v", s1, s2)
	}
	if err := s1.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s1.Release(); err != nil {
		t.Fatal(err)
	}
	if err := s1.Release(); err == nil {
		t.Fatal("bounded semaphore released past its limit")
	}
	dead, live := &auxTestLoop{}, &auxTestLoop{}
	a, _ := auxAcquireAsyncAuxSemaphore(WithAuxiliaryEventLoop(ctx, dead), "compression")
	dead.closed = true
	b, _ := auxAcquireAsyncAuxSemaphore(WithAuxiliaryEventLoop(ctx, live), "compression")
	if a == nil || b == nil || a == b {
		t.Fatal("async semaphores must be per loop")
	}
	auxSemMu.Lock()
	_, deadKept := auxAsyncSemaphores[any(dead)]
	auxSemMu.Unlock()
	if deadKept {
		t.Fatal("closed loop table survived")
	}
}

func TestBuildCallKwargsMaxTokensForwarding(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	msgs := []any{map[string]any{"role": "user", "content": "test"}}
	hasCap := func(kw map[string]any, want int) bool {
		return kw["max_tokens"] == want || kw["max_completion_tokens"] == want
	}
	cases := []struct {
		provider, model, base string
		forwards              bool
	}{
		{"openrouter", "openai/gpt-4o-mini", "", true},
		{"openai", "gpt-4o-mini", "https://openrouter.ai/api/v1", true},
		{"openai", "gpt-4o-mini", "", false},
		{"minimax", "MiniMax-Text-01", "", true},
		{"nvidia", "nemotron", "", true},
	}
	for _, c := range cases {
		kw, err := auxBuildCallKwargs(ctx, c.provider, c.model, msgs, auxCallKwargsOptions{MaxTokens: 2000, BaseURL: c.base, Timeout: 30})
		if err != nil {
			t.Fatal(err)
		}
		if hasCap(kw, 2000) != c.forwards {
			t.Fatalf("%s %s forwards %v kwargs %v", c.provider, c.base, hasCap(kw, 2000), kw)
		}
		kw, _ = auxBuildCallKwargs(ctx, c.provider, "test-model", msgs, auxCallKwargsOptions{BaseURL: c.base, Timeout: 30})
		if _, ok := kw["max_tokens"]; ok {
			t.Fatalf("None max_tokens forwarded for %s", c.provider)
		}
	}
}

func TestBuildCallKwargsToolDedupAndExtraBody(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	tool := func(name string) map[string]any {
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	}
	kw, err := auxBuildCallKwargs(ctx, "openai", "gpt-4o", []any{}, auxCallKwargsOptions{Tools: []any{tool("a"), tool("b"), tool("a")}, Timeout: 30})
	if err != nil {
		t.Fatal(err)
	}
	if tools := kw["tools"].([]any); len(tools) != 2 {
		t.Fatalf("duplicate tools kept %v", tools)
	}
	off := false
	kw, _ = auxBuildCallKwargs(ctx, "deepinfra", "m", []any{}, auxCallKwargsOptions{Timeout: 30,
		ExtraBody: map[string]any{"reasoning": map[string]any{"enabled": true, "effort": "high"}}, ReasoningConfig: map[string]any{"enabled": off}})
	if eb, _ := kw["extra_body"].(map[string]any); eb != nil {
		if r, _ := eb["reasoning"].(map[string]any); r != nil && r["effort"] == "high" {
			t.Fatalf("caller disable lost to task reasoning %v", eb)
		}
	}
	kw, _ = auxBuildCallKwargs(ctx, "nous", "Hermes-4", []any{}, auxCallKwargsOptions{Timeout: 30})
	eb, _ := kw["extra_body"].(map[string]any)
	if eb == nil || eb["tags"] == nil {
		t.Fatalf("nous tags missing %v", kw)
	}
	kw, _ = auxBuildCallKwargs(ctx, "anthropic", "claude-sonnet-4", []any{}, auxCallKwargsOptions{Timeout: 30, ReasoningConfig: map[string]any{"enabled": true, "effort": "low"}})
	if kw["_reasoning_config"] == nil {
		t.Fatalf("anthropic private reasoning missing %v", kw)
	}
	kw, _ = auxBuildCallKwargs(ctx, "openai", "gpt-4o", []any{}, auxCallKwargsOptions{Timeout: 30, ReasoningConfig: map[string]any{"enabled": true, "effort": "low"}})
	if kw["_reasoning_config"] != nil {
		t.Fatal("private reasoning kwarg leaked to a plain OpenAI route")
	}
}

func TestGetTaskExtraBodyFoldsReasoningEffort(t *testing.T) {
	ctx := auxMainTestHome(t, "auxiliary:\n  compression:\n    reasoning_effort: low\n  title_generation:\n    reasoning_effort: bogus\n    extra_body:\n      user: me\n  moa_reference:\n    reasoning_effort: high\n  vision:\n    reasoning_effort: high\n    extra_body:\n      reasoning:\n        effort: max\n")
	if body, _ := auxGetTaskExtraBody(ctx, "compression"); body["reasoning"] == nil {
		t.Fatalf("effort not folded %v", body)
	}
	if body, _ := auxGetTaskExtraBody(ctx, "title_generation"); body["reasoning"] != nil || body["user"] != "me" {
		t.Fatalf("invalid effort handling %v", body)
	}
	if body, _ := auxGetTaskExtraBody(ctx, "moa_reference"); body["reasoning"] != nil {
		t.Fatalf("moa task accepted reasoning %v", body)
	}
	if body, _ := auxGetTaskExtraBody(ctx, "vision"); body["reasoning"].(map[string]any)["effort"] != "max" {
		t.Fatalf("explicit reasoning lost %v", body)
	}
}

func TestCustomEndpointExtraBodyInheritance(t *testing.T) {
	ctx := auxMainTestHome(t, "custom_providers:\n  - name: gw\n    base_url: https://gw.example/v1/\n    extra_body:\n      user: svc\n      tier: a\n")
	got := auxWithCustomEndpointExtraBody(ctx, map[string]any{"tier": "b"}, "custom", "m", "https://gw.example/v1")
	if got["user"] != "svc" || got["tier"] != "b" {
		t.Fatalf("inherited body %v", got)
	}
	if other := auxWithCustomEndpointExtraBody(ctx, nil, "openrouter", "m", "https://gw.example/v1"); other != nil {
		t.Fatalf("other destination inherited %v", other)
	}
}

func TestCompressionFastLaneCertification(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	cfg := map[string]any{"provider": "openrouter", "model": "fast/m", "reasoning_effort": "none"}
	lane, err := ResolveCompressionFastLane(ctx, "fallback_chain[0](openrouter)", "fast/m", "", "", cfg)
	if err != nil || !lane.CertifiedNonReasoning || lane.ReasoningConfig["effort"] != "none" {
		t.Fatalf("lane %+v %v", lane, err)
	}
	if lane, _ := ResolveCompressionFastLane(ctx, "nous", "fast/m", "", "", cfg); lane.CertifiedNonReasoning {
		t.Fatal("certified on a different destination")
	}
	_, body, _ := auxCompressionFastLaneControls(ctx, "compression", "nous", "other", "", "", cfg, cfg, nil, map[string]any{"reasoning": map[string]any{"effort": "low"}})
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("fast only reasoning leaked %v", body)
	}
	if mt, body, _ := auxCompressionFastLaneControls(ctx, "compression", "openrouter", "fast/m", "", "", cfg, cfg, 100, map[string]any{}); mt != 100 || len(body) != 0 {
		t.Fatal("explicit cap must bypass the lane")
	}
}

func TestValidateLLMResponseShapes(t *testing.T) {
	ctx := context.Background()
	if _, err := auxValidateLLMResponse(ctx, nil, "title", "", ""); err == nil || err.Error() != "Auxiliary title: LLM returned None response" {
		t.Fatalf("none response %v", err)
	}
	ok := map[string]any{"model": "m", "choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}}}
	if got, err := auxValidateLLMResponse(ctx, ok, "", "", ""); err != nil || got == nil {
		t.Fatalf("valid response rejected %v", err)
	}
	bad := map[string]any{"error": map[string]any{"message": "upstream down"}}
	_, err := auxValidateLLMResponse(ctx, bad, "", "", "")
	rerr, isRuntime := err.(*AuxRuntimeError)
	if !isRuntime || rerr.ProviderError["message"] != "upstream down" {
		t.Fatalf("embedded provider error not kept %v", err)
	}
	shim := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": routerTimeoutShim}}}, "usage": map[string]any{"completion_tokens": 0}}
	if _, err := auxValidateLLMResponse(ctx, shim, "compression", "", ""); err == nil || err.Error() != "Auxiliary compression: provider returned a timeout shim instead of a completion" {
		t.Fatalf("router shim accepted %v", err)
	}
	shim["usage"] = map[string]any{"completion_tokens": 9}
	if _, err := auxValidateLLMResponse(ctx, shim, "compression", "", ""); err != nil {
		t.Fatalf("generated sentinel rejected %v", err)
	}
}

func TestMergeSessionAffinityHeaders(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	kw := MergeSessionAffinityHeaders(WithPortalConversationContext(ctx, "conv-1"), map[string]any{}, "opencode-go", "", "")
	if h := kw["extra_headers"].(map[string]any); h[OpencodeSessionHeader] != "conv-1" {
		t.Fatalf("opencode affinity %v", h)
	}
	kw = MergeSessionAffinityHeaders(ctx, map[string]any{}, "opencode-zen", "", "")
	if h := kw["extra_headers"].(map[string]any)[OpencodeSessionHeader].(string); len(h) != len("oneshot-")+16 {
		t.Fatalf("oneshot key %q", h)
	}
	pinned := MergeSessionAffinityHeaders(ctx, map[string]any{"extra_headers": map[string]any{OpencodeSessionHeader: "mine"}}, "opencode-go", "", "s")
	if pinned["extra_headers"].(map[string]any)[OpencodeSessionHeader] != "mine" {
		t.Fatal("caller pinned header replaced")
	}
	if kw := MergeSessionAffinityHeaders(ctx, map[string]any{}, "openai", "https://api.openai.com/v1", "s"); kw["extra_headers"] != nil {
		t.Fatal("non opencode target got headers")
	}
}

func TestCoerceLLMMessageAndMessageField(t *testing.T) {
	msg := map[string]any{"content": "hi"}
	if got := CoerceLLMMessage(map[string]any{"choices": []any{map[string]any{"message": msg}}}); got.(map[string]any)["content"] != "hi" {
		t.Fatalf("response message %v", got)
	}
	if got := CoerceLLMMessage(msg); got.(map[string]any)["content"] != "hi" {
		t.Fatal("bare message not returned")
	}
	if CoerceLLMMessage(map[string]any{"choices": []any{}}) != nil || CoerceLLMMessage(nil) != nil || CoerceLLMMessage("text") != "text" {
		t.Fatal("empty, nil or string handling")
	}
	type wire struct {
		ReasoningContent string `json:"reasoning_content"`
		Content          string
	}
	if MessageField(&wire{ReasoningContent: "r", Content: "c"}, "reasoning_content") != "r" || MessageField(wire{Content: "c"}, "content") != "c" || MessageField(wire{}, "missing") != nil {
		t.Fatal("attribute reads")
	}
	var marker interface{ PyRuntimeError() } = &AuxRuntimeError{}
	_ = marker
}

func TestOpencodeTransportRederivesPerModel(t *testing.T) {
	ctx := auxMainTestHome(t, "providers:\n  opencode-go-pinned:\n    base_url: https://opencode.ai/zen/go/v1\n    api_mode: chat_completions\n")
	if mode, url := OpencodeTransport(ctx, "openai", "gpt-4o", "https://api.openai.com/v1"); mode != "" || url != "https://api.openai.com/v1" {
		t.Fatalf("non opencode target rederived %q %q", mode, url)
	}
	wantMode := OpenCodeModelMode("opencode-zen", "claude-sonnet-4")
	mode, url := OpencodeTransport(ctx, "opencode-zen", "claude-sonnet-4", "https://opencode.ai/zen/v1")
	if mode != wantMode || url != NormalizeOpenCodeBaseURL("opencode-zen", wantMode, "https://opencode.ai/zen/v1") {
		t.Fatalf("opencode zen transport %q %q", mode, url)
	}
	if mode, _ := OpencodeTransport(ctx, "opencode-go-pinned", "gpt-5", "https://opencode.ai/zen/go/v1"); mode != "" {
		t.Fatalf("entry declared api_mode overridden by %q", mode)
	}
	if !IsOpencodeTarget("custom", "https://opencode.ai/zen/v1") || IsOpencodeTarget("openai", "https://api.openai.com") {
		t.Fatal("opencode target detection")
	}
}

func TestResolveAuxTaskProviderExportReadsConfiguredProvider(t *testing.T) {
	ctx := auxCompressionTaskHome(t)
	if got := ResolveAuxTaskProvider(ctx, "compression"); got != "openrouter" {
		t.Fatalf("configured compression provider %q", got)
	}
	if got := ResolveAuxTaskProvider(ctx, "title_generation"); got != "auto" {
		t.Fatalf("an unconfigured task resolves %q, want auto", got)
	}
}

func TestIsGeminiNativeProviderNameReadsTheNativeSet(t *testing.T) {
	for name, want := range map[string]bool{"gemini": true, "google": true, "google-gemini": true, "google-ai-studio": true, "openrouter": false, "": false} {
		if got := IsGeminiNativeProviderName(name); got != want {
			t.Fatalf("%q: got %v", name, got)
		}
	}
}
