package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fakeLlmResponse(text string, prompt, completion int) map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": text, "role": "assistant"}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion},
	}
}

func trustedPolicy(id string) trustPolicy {
	return trustPolicy{pluginID: id, allowProviderOverride: true, allowAnyProvider: true, allowModelOverride: true, allowAnyModel: true,
		allowAgentIDOverride: true, allowProfileOverride: true}
}

func sp(s string) *string { return &s }

func capturingCaller(captured *pluginLlmCall, provider, model string, response any) pluginLlmCaller {
	return func(_ context.Context, kw pluginLlmCall) (string, string, any, error) {
		*captured = kw
		return provider, model, response, nil
	}
}

func isTrustErr(err error, sub string) bool {
	var te *PluginLlmTrustError
	return errors.As(err, &te) && strings.Contains(te.Message, sub)
}

// Installs a services set whose builtin aux task keys and plugin client are the given doubles
func installLlmServices(t *testing.T, builtin []string, client PluginLLMClient) {
	t.Helper()
	previous := installedServices()
	InstallServices(&Services{BuiltinAuxTaskKeys: func() []string { return builtin }, PluginLLM: client})
	t.Cleanup(func() { InstallServices(previous) })
}

// Temp home whose plugin manager holds the given auxiliary task registrations by canonical id
func llmTaskHome(t *testing.T, owners map[string]string) *Manager {
	t.Helper()
	h, m := skillHostHome(t, "plugins: {}\n")
	m.DiscoverAndLoad(homeCtx(h), false)
	for task, key := range owners {
		if _, err := newContext(&Manifest{Name: task + "-owner", Key: key}, m).RegisterAuxiliaryTask(task, AuxTaskOptions{DisplayName: strings.ToTitle(task), Description: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// test_plugin_llm.py TestTrustGate and TestAllowlistCoercion
func TestPluginLlmTrustGate(t *testing.T) {
	modelOnly := trustPolicy{pluginID: "model-only", allowModelOverride: true, allowAnyModel: true}
	if _, m, _, _, err := checkOverrides(modelOnly, nil, sp("gpt-4o"), nil, nil); err != nil || *m != "gpt-4o" {
		t.Fatalf("model alone %v %v", m, err)
	}
	if _, _, _, _, err := checkOverrides(modelOnly, sp("anthropic"), nil, nil, nil); !isTrustErr(err, "cannot override the provider") {
		t.Fatalf("provider alone %v", err)
	}
	restricted := trustPolicy{pluginID: "restricted", allowProviderOverride: true, allowedProviders: map[string]bool{"openrouter": true}}
	if p, _, _, _, err := checkOverrides(restricted, sp("OpenRouter"), nil, nil, nil); err != nil || *p != "OpenRouter" {
		t.Fatalf("allowlist %v %v", p, err)
	}
	if _, _, _, _, err := checkOverrides(restricted, sp("anthropic"), nil, nil, nil); !isTrustErr(err, "is not in plugins.entries.restricted.llm.allowed_providers") {
		t.Fatalf("not listed %v", err)
	}
	if p, m, a, pr, err := checkOverrides(trustPolicy{pluginID: "locked"}, nil, nil, nil, nil); err != nil || p != nil || m != nil || a != nil || pr != nil {
		t.Fatal("no overrides")
	}
	p, m, a, pr, err := checkOverrides(trustedPolicy("trusted-plugin"), sp("openrouter"), sp("anthropic/claude-3-5-sonnet"), sp("ada"), sp("work"))
	if err != nil || *p != "openrouter" || *m != "anthropic/claude-3-5-sonnet" || *a != "ada" || *pr != "work" {
		t.Fatalf("fully trusted %v", err)
	}
	if _, _, _, _, err := checkOverrides(trustPolicy{pluginID: "x"}, nil, nil, sp("ada"), nil); !isTrustErr(err, "run completions against a non-default agent id") {
		t.Fatalf("agent id %v", err)
	}
	set, star := coerceAllowlist([]any{"OpenAI/GPT-4o", "  anthropic/claude  ", 7})
	if star || !reflect.DeepEqual(set, map[string]bool{"openai/gpt-4o": true, "anthropic/claude": true}) {
		t.Fatalf("list %v %v", set, star)
	}
	if set, star := coerceAllowlist([]any{"*"}); !star || len(set) != 0 {
		t.Fatalf("star %v %v", set, star)
	}
	if set, star := coerceAllowlist("gpt"); set != nil || star {
		t.Fatal("non list")
	}
}

// test_plugin_llm.py TestStructuredMessageBuilding and TestJsonParsing
func TestPluginLlmStructuredMessagesAndParsing(t *testing.T) {
	msgs, err := buildStructuredMessages(PluginLlmStructuredRequest{Instructions: "Detect language", Input: []any{PluginLlmTextInput{Text: "hello"}}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("text only %v %v", msgs, err)
	}
	parts := msgs[0].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["text"] != "Detect language" || parts[1].(map[string]any)["text"] != "hello" {
		t.Fatalf("parts %v", parts)
	}
	msgs, _ = buildStructuredMessages(PluginLlmStructuredRequest{Instructions: "Caption", Input: []any{PluginLlmImageInput{Data: []byte("fake-bytes"), MimeType: "image/png"}},
		JSONMode: true, SchemaName: "cap", JSONSchema: map[string]any{"type": "object"}, SystemPrompt: " Be brief. "})
	if sys := msgs[0].(map[string]any)["content"]; sys != "Be brief.\n\nRespond with a single JSON object that matches the requested shape. Do not include prose or markdown fences." {
		t.Fatalf("system %q", sys)
	}
	user := msgs[1].(map[string]any)["content"].([]any)
	if user[0].(map[string]any)["text"] != "Caption\n\nSchema name: cap\n\nJSON schema:\n{\"type\": \"object\"}" {
		t.Fatalf("header %q", user[0])
	}
	if url := user[1].(map[string]any)["image_url"].(map[string]any)["url"]; url != "data:image/png;base64,ZmFrZS1ieXRlcw==" {
		t.Fatalf("data url %v", url)
	}
	msgs, _ = buildStructuredMessages(PluginLlmStructuredRequest{Instructions: "x", Input: []any{map[string]any{"type": "image", "url": "https://example.com/a.png"}}})
	if url := msgs[0].(map[string]any)["content"].([]any)[1].(map[string]any)["image_url"].(map[string]any)["url"]; url != "https://example.com/a.png" {
		t.Fatalf("url %v", url)
	}
	for _, bad := range []any{map[string]any{"type": "video"}, map[string]any{"type": "text"}, map[string]any{"type": "image"}, 5} {
		if _, err := normalizeInputBlock(bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	if got := stripCodeFences("```json\n{\"a\": 1}\n```"); got != "{\"a\": 1}" {
		t.Fatalf("fence %q", got)
	}
	parsed, ct, err := parseStructuredText("{\"language\": \"French\"}", true, nil)
	if err != nil || ct != "json" || !reflect.DeepEqual(parsed, map[string]any{"language": "French"}) {
		t.Fatalf("json mode %v %v %v", parsed, ct, err)
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{"language": map[string]any{"type": "string"}}, "required": []any{"language"}}
	if parsed, ct, err := parseStructuredText("{\"language\": \"French\"}", false, schema); err != nil || ct != "json" || parsed.(map[string]any)["language"] != "French" {
		t.Fatalf("schema match %v %v", parsed, err)
	}
	if _, _, err := parseStructuredText("{}", false, schema); err == nil || err.Error() != "Plugin LLM structured output did not match schema: 'language' is a required property" {
		t.Fatalf("schema mismatch %v", err)
	}
	if parsed, ct, _ := parseStructuredText("not json", true, nil); parsed != nil || ct != "text" {
		t.Fatal("prose")
	}
}

// test_plugin_llm.py TestPluginLlmFacade and TestAsyncSurface
func TestPluginLlmFacade(t *testing.T) {
	ctx := context.Background()
	var captured pluginLlmCall
	llm := makePluginLlmForTest("my-plugin", trustPolicy{pluginID: "my-plugin"}, capturingCaller(&captured, "auto", "default", fakeLlmResponse("Hello world.", 4, 6)), nil)
	result, err := llm.Complete(ctx, []any{map[string]any{"role": "user", "content": "hi"}}, PluginLlmOptions{})
	if err != nil || result.Text != "Hello world." || captured.ProviderOverride != nil || captured.ModelOverride != nil || captured.ProfileOverride != nil ||
		result.Usage.InputTokens != 4 || result.Usage.TotalTokens != 10 || result.AgentID != "default" {
		t.Fatalf("default %+v %v", result, err)
	}
	llm = makePluginLlmForTest("my-plugin", trustedPolicy("my-plugin"), capturingCaller(&captured, "anthropic", "claude-3-opus", fakeLlmResponse("ok", 4, 6)), nil)
	temp, maxTokens, timeout := 0.0, 128, 10.0
	result, err = llm.Complete(ctx, []any{map[string]any{"role": "user", "content": "hi"}}, PluginLlmOptions{Provider: sp("anthropic"), Model: sp("claude-3-opus"),
		Profile: sp("work"), AgentID: sp("ada"), Temperature: &temp, MaxTokens: &maxTokens, Timeout: &timeout, Purpose: sp("extract")})
	if err != nil || result.Provider != "anthropic" || result.Model != "claude-3-opus" || *captured.ProviderOverride != "anthropic" || *captured.ModelOverride != "claude-3-opus" ||
		*captured.ProfileOverride != "work" || *captured.Temperature != 0 || *captured.MaxTokens != 128 || *captured.Timeout != 10 || result.AgentID != "ada" ||
		result.Audit["purpose"] != "extract" || result.Audit["profile"] != "work" {
		t.Fatalf("trusted %+v %v", result, err)
	}
	llm = makePluginLlmForTest("my-plugin", trustPolicy{pluginID: "my-plugin"}, capturingCaller(&captured, "openai", "gpt-4o",
		fakeLlmResponse("{\"language\": \"French\", \"is_question\": true, \"confidence\": 0.99}", 4, 6)), nil)
	sr, err := llm.CompleteStructured(ctx, PluginLlmStructuredRequest{Instructions: "Detect language", Input: []any{PluginLlmTextInput{Text: "Comment ça va?"}}, JSONMode: true}, PluginLlmOptions{})
	if err != nil || sr.ContentType != "json" || !reflect.DeepEqual(sr.Parsed, map[string]any{"language": "French", "is_question": true, "confidence": 0.99}) || sr.Audit["schema_name"] != "" {
		t.Fatalf("structured %+v %v", sr, err)
	}
	if captured.ExtraBody["response_format"].(map[string]any)["type"] != "json_object" {
		t.Fatalf("response format %v", captured.ExtraBody)
	}
	llm = makePluginLlmForTest("my-plugin", trustPolicy{pluginID: "my-plugin"}, capturingCaller(&captured, "openai", "gpt-4o", fakeLlmResponse("{\"caption\": \"ok\"}", 4, 6)), nil)
	if _, err := llm.CompleteStructured(ctx, PluginLlmStructuredRequest{Instructions: "Caption this", Input: []any{PluginLlmImageInput{Data: []byte("fake-bytes"), MimeType: "image/png"}}, JSONMode: true}, PluginLlmOptions{}); err != nil {
		t.Fatal(err)
	}
	var images []any
	for _, m := range captured.Messages {
		if m.(map[string]any)["role"] == "user" {
			for _, p := range m.(map[string]any)["content"].([]any) {
				if p.(map[string]any)["type"] == "image_url" {
					images = append(images, p)
				}
			}
		}
	}
	if len(images) != 1 || !strings.HasPrefix(images[0].(map[string]any)["image_url"].(map[string]any)["url"].(string), "data:image/png;base64,") {
		t.Fatalf("image parts %v", images)
	}
	if _, err := llm.CompleteStructured(ctx, PluginLlmStructuredRequest{Instructions: "  ", Input: []any{PluginLlmTextInput{Text: "x"}}}, PluginLlmOptions{}); err == nil || err.Error() != "complete_structured requires non-empty instructions" {
		t.Fatalf("empty instructions %v", err)
	}
	if _, err := llm.ACompleteStructured(ctx, PluginLlmStructuredRequest{Instructions: "x"}, PluginLlmOptions{}); err == nil || err.Error() != "acomplete_structured requires at least one input block" {
		t.Fatalf("no input %v", err)
	}
	async := makePluginLlmForTest("my-plugin", trustPolicy{pluginID: "my-plugin"}, nil, capturingCaller(&captured, "openai", "gpt-4o", fakeLlmResponse("{\"x\": 42}", 4, 6)))
	ar, err := async.ACompleteStructured(ctx, PluginLlmStructuredRequest{Instructions: "Extract x", Input: []any{PluginLlmTextInput{Text: "data"}}, JSONMode: true}, PluginLlmOptions{})
	if err != nil || ar.ContentType != "json" || !reflect.DeepEqual(ar.Parsed, map[string]any{"x": int64(42)}) {
		t.Fatalf("async %+v %v", ar, err)
	}
}

// test_plugin_llm.py TestConfigDrivenPolicy and TestPluginContextIntegration
func TestPluginLlmConfigPolicyAndContextBinding(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	memWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  entries:\n    my-plugin:\n      llm:\n        allow_provider_override: true\n        allowed_providers: [openrouter, anthropic]\n"+
		"        allow_model_override: true\n        allowed_models:\n          - openai/gpt-4o-mini\n          - anthropic/claude-3-5-haiku\n        allow_profile_override: false\n")
	p := resolveTrustPolicy(homeCtx(h), "my-plugin")
	if !p.allowProviderOverride || !p.allowModelOverride || p.allowProfileOverride ||
		!reflect.DeepEqual(p.allowedProviders, map[string]bool{"openrouter": true, "anthropic": true}) ||
		!reflect.DeepEqual(p.allowedModels, map[string]bool{"openai/gpt-4o-mini": true, "anthropic/claude-3-5-haiku": true}) {
		t.Fatalf("policy %+v", p)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := resolveTrustPolicy(homeCtx(h), "never-configured"); p.allowProviderOverride || p.allowModelOverride || p.allowProfileOverride || p.allowAgentIDOverride {
		t.Fatalf("default deny %+v", p)
	}
	c := newContext(&Manifest{Name: "bare-name", Source: "test", Key: "image_gen/openai"}, NewManager(h))
	if c.LLM().pluginID != "image_gen/openai" || c.LLM() != c.LLM() {
		t.Fatal("ctx.llm identity")
	}
}

type fakeAuxClient struct {
	seen         []*AuxLLMRequest
	async        []bool
	route        map[string]string
	response     any
	mainP, mainM string
}

func (f *fakeAuxClient) call(req *AuxLLMRequest, async bool) (any, error) {
	f.seen, f.async = append(f.seen, req), append(f.async, async)
	if req.RouteInfo != nil {
		for k, v := range f.route {
			req.RouteInfo[k] = v
		}
	}
	return f.response, nil
}
func (f *fakeAuxClient) CallLLM(_ context.Context, req *AuxLLMRequest) (any, error) {
	return f.call(req, false)
}
func (f *fakeAuxClient) AsyncCallLLM(_ context.Context, req *AuxLLMRequest) (any, error) {
	return f.call(req, true)
}
func (f *fakeAuxClient) ReadMainProvider() string { return f.mainP }
func (f *fakeAuxClient) ReadMainModel() string    { return f.mainM }

// test_plugin_llm.py TestAttribution and test_plugin_llm_task_routing.py test_successful_fallback_route_beats_requested_route_for_attribution
func TestPluginLlmAttribution(t *testing.T) {
	response := map[string]any{"model": "gpt-4o-2024-08-06", "choices": []any{}}
	if p, m := resolveAttribution(sp("openrouter"), sp("openai/gpt-4o"), response, nil); p != "openrouter" || m != "gpt-4o-2024-08-06" {
		t.Fatalf("override %s %s", p, m)
	}
	installLlmServices(t, nil, &fakeAuxClient{mainP: "openrouter", mainM: "openai/gpt-4o"})
	if p, m := resolveAttribution(nil, nil, map[string]any{"model": "openai/gpt-4o-2024-08-06", "choices": []any{}}, nil); p != "openrouter" || m != "openai/gpt-4o-2024-08-06" {
		t.Fatalf("main %s %s", p, m)
	}
	if p, m := resolveAttribution(nil, nil, fakeLlmResponse("x", 1, 1), nil); p != "openrouter" || m != "openai/gpt-4o" {
		t.Fatalf("main fallback %s %s", p, m)
	}
	if p, m := resolveAttribution(sp("primary-provider"), sp("primary-model"), fakeLlmResponse("ok", 3, 5), map[string]string{"provider": "fallback-provider", "model": "fallback-model"}); p != "fallback-provider" || m != "fallback-model" {
		t.Fatalf("route %s %s", p, m)
	}
	installLlmServices(t, nil, &fakeAuxClient{})
	if p, m := resolveAttribution(nil, nil, fakeLlmResponse("x", 1, 1), nil); p != "auto" || m != "default" {
		t.Fatalf("defaults %s %s", p, m)
	}
}

// test_plugin_llm.py TestHookMode test_complete_works_from_post_tool_call_hook
func TestPluginLlmFromPostToolCallHook(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "hook-plugin", Source: "test", Key: "hook-plugin"}, m)
	var calls []pluginLlmCall
	c.llm = makePluginLlmForTest("hook-plugin", trustPolicy{pluginID: "hook-plugin"}, func(_ context.Context, kw pluginLlmCall) (string, string, any, error) {
		calls = append(calls, kw)
		return "openrouter", "openai/gpt-4o", fakeLlmResponse("rewrote it", 4, 6), nil
	}, nil)
	var returned []string
	hook := func(_ context.Context, kwargs map[string]any) (any, error) {
		result, _ := kwargs["result"].(string)
		if strings.Contains(result, "Traceback") {
			max := 64
			r, err := c.LLM().Complete(context.Background(), []any{map[string]any{"role": "system", "content": "Rewrite errors plainly."},
				map[string]any{"role": "user", "content": result}}, PluginLlmOptions{MaxTokens: &max, Purpose: sp("hook-plugin.rewrite")})
			if err != nil {
				t.Fatal(err)
			}
			returned = append(returned, r.Text)
		}
		return nil, nil
	}
	c.RegisterHook("post_tool_call", hook, false)
	m.InvokeHook(context.Background(), "post_tool_call", map[string]any{"tool_name": "terminal", "args": map[string]any{"command": "x"}, "result": "Traceback (most recent call last):\n  boom"})
	if len(calls) != 1 || *calls[0].MaxTokens != 64 || len(returned) != 1 || returned[0] != "rewrote it" {
		t.Fatalf("hook calls %v %v", calls, returned)
	}
}

// test_plugin_llm_task_routing.py TestCheckTask
func TestPluginLlmCheckTask(t *testing.T) {
	installLlmServices(t, []string{"vision", "compression", "mine"}, nil)
	ctx := homeCtx(llmTaskHome(t, map[string]string{"classifier": "other-plugin", "mine2": "my-plugin"}).home)
	pol := trustPolicy{pluginID: "my-plugin"}
	for _, raw := range []*string{nil, sp(""), sp("auto"), sp("AUTO"), sp("   ")} {
		if got, err := checkTask(ctx, pol, "my-plugin", raw); got != nil || err != nil {
			t.Fatalf("auto %v %v", got, err)
		}
	}
	if got, err := checkTask(ctx, pol, "my-plugin", sp("  mine2 ")); err != nil || *got != "mine2" {
		t.Fatalf("own key %v %v", got, err)
	}
	logs := captureLogs(t)
	if _, err := checkTask(ctx, pol, "my-plugin", sp("classifier")); !isTrustErr(err, "'my-plugin'") || !isTrustErr(err, "'classifier'") ||
		!strings.Contains(logs.String(), "plugin 'my-plugin' requested auxiliary task 'classifier' it did not register") {
		t.Fatalf("foreign %v %s", err, logs)
	}
	if _, err := checkTask(ctx, pol, "my-plugin", sp("nope")); !isTrustErr(err, "nope") {
		t.Fatalf("unknown %v", err)
	}
	if _, err := checkTask(ctx, pol, "my-plugin", sp("vision")); !isTrustErr(err, "allow_task_override") || !strings.Contains(logs.String(), "built-in auxiliary task 'vision'") {
		t.Fatalf("builtin denied %v", err)
	}
	if got, err := checkTask(ctx, trustPolicy{pluginID: "my-plugin", allowTaskOverride: true}, "my-plugin", sp("vision")); err != nil || *got != "vision" {
		t.Fatalf("builtin allowed %v %v", got, err)
	}
}

// test_plugin_llm_task_routing.py TestRouting, TestForwardsToCallLlm and the plugin halves of TestOwnershipIntegration
func TestPluginLlmTaskRouting(t *testing.T) {
	installLlmServices(t, []string{"vision"}, nil)
	m := llmTaskHome(t, map[string]string{"classifier": "my-plugin"})
	ctx := homeCtx(m.home)
	var captured pluginLlmCall
	llm := makePluginLlmForTest("my-plugin", trustPolicy{pluginID: "my-plugin"}, capturingCaller(&captured, "openrouter", "slot-model", fakeLlmResponse("ok", 3, 5)),
		capturingCaller(&captured, "openrouter", "slot-model", fakeLlmResponse("{\"a\": 1}", 3, 5)))
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	if r, err := llm.Complete(ctx, msgs, PluginLlmOptions{}); err != nil || captured.Task != nil || r.Audit["task"] != "" {
		t.Fatalf("default %v", err)
	}
	if r, err := llm.Complete(ctx, msgs, PluginLlmOptions{Task: sp("classifier")}); err != nil || *captured.Task != "classifier" || r.Model != "slot-model" || r.Audit["task"] != "classifier" {
		t.Fatalf("registered %v", err)
	}
	for _, task := range []string{"vision", "nope"} {
		captured = pluginLlmCall{}
		if _, err := llm.Complete(ctx, msgs, PluginLlmOptions{Task: sp(task)}); err == nil || captured.Messages != nil {
			t.Fatalf("%s reached the caller", task)
		}
	}
	if r, err := llm.CompleteStructured(ctx, PluginLlmStructuredRequest{Instructions: "x", Input: []any{PluginLlmTextInput{Text: "payload"}}}, PluginLlmOptions{Task: sp("classifier")}); err != nil || *captured.Task != "classifier" || r.Audit["task"] != "classifier" {
		t.Fatalf("structured %v", err)
	}
	if r, err := llm.AComplete(ctx, msgs, PluginLlmOptions{Task: sp("classifier")}); err != nil || *captured.Task != "classifier" || r.Audit["task"] != "classifier" {
		t.Fatalf("async %v", err)
	}
	if r, err := llm.ACompleteStructured(ctx, PluginLlmStructuredRequest{Instructions: "x", Input: []any{PluginLlmTextInput{Text: "p"}}, JSONMode: true}, PluginLlmOptions{Task: sp("classifier")}); err != nil || r.Audit["task"] != "classifier" || r.ContentType != "json" {
		t.Fatalf("async structured %v", err)
	}
	client := &fakeAuxClient{route: map[string]string{"provider": "aux-provider", "model": "aux-model"}, response: fakeLlmResponse("ok", 3, 5)}
	installLlmServices(t, []string{"vision"}, client)
	host := makePluginLlmForTest("my-plugin", trustPolicy{pluginID: "my-plugin"}, nil, nil)
	if r, err := host.Complete(ctx, msgs, PluginLlmOptions{Task: sp("classifier")}); err != nil || *client.seen[0].Task != "classifier" || client.async[0] || r.Provider != "aux-provider" || r.Model != "aux-model" {
		t.Fatalf("sync forward %+v %v", r, err)
	}
	if _, err := host.Complete(ctx, msgs, PluginLlmOptions{}); err != nil || client.seen[1].Task != nil || client.seen[1].RouteInfo != nil {
		t.Fatalf("default forward %v", err)
	}
	if r, err := host.AComplete(ctx, msgs, PluginLlmOptions{Task: sp("classifier")}); err != nil || !client.async[2] || r.Provider != "aux-provider" || r.Model != "aux-model" {
		t.Fatalf("async forward %v", err)
	}
	trusted := makePluginLlmForTest("my-plugin", trustedPolicy("my-plugin"), nil, nil)
	if _, err := trusted.CompleteStructured(ctx, PluginLlmStructuredRequest{Instructions: "x", Input: []any{PluginLlmTextInput{Text: "p"}}, JSONSchema: map[string]any{"type": "object"}}, PluginLlmOptions{Profile: sp(" work ")}); err != nil {
		t.Fatal(err)
	}
	extra := client.seen[3].ExtraBody
	if extra["metadata"].(map[string]any)["auth_profile"] != "work" || extra["response_format"].(map[string]any)["json_schema"].(map[string]any)["name"] != "plugin_structured_output" {
		t.Fatalf("extra body %v", extra)
	}
	owned, builtin := resolveTaskOwnership(ctx, "my-plugin")
	if !owned["classifier"] || !builtin["vision"] {
		t.Fatalf("ownership %v %v", owned, builtin)
	}
}

// test_plugin_llm_task_routing.py TestOwnershipIntegration test_owner_stored_as_canonical_id and test_check_task_end_to_end_with_real_registry
func TestPluginLlmTaskOwnerIsCanonicalID(t *testing.T) {
	installLlmServices(t, []string{"vision"}, nil)
	m := llmTaskHome(t, map[string]string{"classifier": "my_key"})
	ctx := homeCtx(m.home)
	if owned, builtin := resolveTaskOwnership(ctx, "my_key"); !owned["classifier"] || !builtin["vision"] {
		t.Fatalf("canonical %v %v", owned, builtin)
	}
	if owned, _ := resolveTaskOwnership(ctx, "classifier-owner"); owned["classifier"] {
		t.Fatal("name owns the task")
	}
	if got, err := checkTask(ctx, trustPolicy{pluginID: "my_key"}, "my_key", sp("classifier")); err != nil || *got != "classifier" {
		t.Fatalf("end to end %v", err)
	}
	if _, err := checkTask(ctx, trustPolicy{pluginID: "other"}, "other", sp("classifier")); err == nil {
		t.Fatal("other plugin passed")
	}
}
