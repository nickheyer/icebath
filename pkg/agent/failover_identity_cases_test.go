package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/provider"
)

const failoverPrompt = "You are a helpful assistant.\n\nMemory note at line start:\nModel: decoy-from-memory\n\n" +
	"Conversation started: Wednesday, June 10, 2026\nModel: gpt-5.4-mini\nProvider: openai-codex"

const failoverStatic = "You are a helpful assistant.\n\nStable brief.\n"

func countCacheMarkers(messages []map[string]any) int {
	count := 0
	for _, msg := range messages {
		if _, ok := msg["cache_control"]; ok {
			count++
		}
		for _, part := range anyRows(msg["content"]) {
			if p, ok := part.(map[string]any); ok {
				if _, ok := p["cache_control"]; ok {
					count++
				}
			}
		}
	}
	return count
}

type failoverCacheAgent struct {
	useCaching, native, directToolCache bool
	prompt, static, provider            string
	tools                               []any
}

func (o failoverCacheAgent) build() *AIAgent {
	a := &AIAgent{cachedSystemPrompt: o.prompt, cachedSystemPromptStatic: o.static, usePromptCaching: o.useCaching,
		useNativeCacheLayout: o.native, cacheTTL: "5m", provider: o.provider, model: "gpt-4o", tools: o.tools}
	if o.directToolCache {
		a.apiMode = "anthropic_messages"
		a.setBaseURL("https://api.anthropic.com")
	}
	return a
}

func partMaps(content any) []map[string]any {
	var out []map[string]any
	for _, p := range anyRows(content) {
		if m, ok := p.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func TestSyncFailoverSystemMessage(t *testing.T) {
	caseHome(t)
	t.Run("test_patches_in_flight_system_message", func(t *testing.T) {
		a := &AIAgent{cachedSystemPrompt: failoverPrompt}
		a.rewritePromptModelIdentity("gemma4:e2b-mlx", "custom")
		api := []any{map[string]any{"role": "system", "content": failoverPrompt}, map[string]any{"role": "user", "content": "what model are you?"}}
		result := syncFailoverSystemMessage(a, api, failoverPrompt)
		if !strings.Contains(api[0].(map[string]any)["content"].(string), "Model: gemma4:e2b-mlx") || result != a.cachedSystemPrompt {
			t.Fatalf("system row %v result %q", api[0], result)
		}
	})
	t.Run("test_noop_without_cached_prompt", func(t *testing.T) {
		api := []any{map[string]any{"role": "system", "content": "original"}}
		if result := syncFailoverSystemMessage(&AIAgent{}, api, "active"); api[0].(map[string]any)["content"] != "original" || result != "active" {
			t.Fatalf("system row %v result %q", api[0], result)
		}
	})
}

func TestSyncFailoverPreservesCacheDecoration(t *testing.T) {
	caseHome(t)
	t.Run("test_keeps_breakpoints_and_static_prefix", func(t *testing.T) {
		prompt := failoverStatic + "Model: gpt-5.4-mini\nProvider: openai-codex"
		a := &AIAgent{cachedSystemPrompt: prompt}
		api := provider.ApplyAnthropicCacheControl([]map[string]any{{"role": "system", "content": prompt}, {"role": "user", "content": "what model are you?"}},
			"", true, failoverStatic, false)
		if len(partMaps(api[0]["content"])) == 0 {
			t.Fatalf("the system prompt was not decorated %v", api[0]["content"])
		}
		a.rewritePromptModelIdentity("gemma4:e2b-mlx", "custom")
		syncFailoverSystemMessage(a, rowsAsAny(api), prompt)
		content := partMaps(api[0]["content"])
		if len(content) != 2 || content[0]["cache_control"] == nil || content[1]["cache_control"] == nil {
			t.Fatalf("cache decoration lost %v", api[0]["content"])
		}
		tail, _ := content[1]["text"].(string)
		if content[0]["text"] != failoverStatic || !strings.Contains(tail, "Model: gemma4:e2b-mlx") || !strings.Contains(tail, "Provider: custom") {
			t.Fatalf("blocks %v", content)
		}
	})
	t.Run("test_keeps_single_block_shape", func(t *testing.T) {
		prompt := "Model: gpt-5.4-mini\nProvider: openai-codex"
		a := &AIAgent{cachedSystemPrompt: prompt}
		api := provider.ApplyAnthropicCacheControl([]map[string]any{{"role": "system", "content": prompt}}, "", true, "", false)
		if len(partMaps(api[0]["content"])) != 1 {
			t.Fatalf("single block layout expected %v", api[0]["content"])
		}
		a.rewritePromptModelIdentity("gemma4:e2b-mlx", "custom")
		syncFailoverSystemMessage(a, rowsAsAny(api), prompt)
		content := partMaps(api[0]["content"])
		if len(content) != 1 || content[0]["cache_control"] == nil || !strings.Contains(content[0]["text"].(string), "Model: gemma4:e2b-mlx") {
			t.Fatalf("blocks %v", api[0]["content"])
		}
	})
}

type rebasingMoaClient struct{}

func (rebasingMoaClient) RebasePreparedRequest(prepared map[string]any, messages []map[string]any) map[string]any {
	out := make([]map[string]any, len(messages))
	for i, m := range messages {
		out[i] = shallowRow(m)
	}
	rebased := shallowRow(prepared)
	rebased["messages"] = attachReferenceGuidance(out, prepared["guidance"].(string))
	return rebased
}

func TestRedecoratePromptCacheOnPolicyChange(t *testing.T) {
	caseHome(t)
	ctx := context.Background()
	t.Run("test_optional_tools_return_a_complete_provider_plan", func(t *testing.T) {
		for _, tc := range []struct {
			name               string
			useCaching, native bool
			provider           string
		}{{"cache-off", false, false, "openai"}, {"native", true, true, "anthropic"}} {
			prompt := failoverStatic + "volatile"
			messages := provider.ApplyAnthropicCacheControl([]map[string]any{{"role": "system", "content": prompt}, {"role": "user", "content": "hello"}},
				"", true, failoverStatic, false)
			registered := []any{map[string]any{"type": "function", "function": map[string]any{"name": "registered", "parameters": map[string]any{"type": "object"}},
				"cache_control": map[string]any{"type": "ephemeral"}}}
			a := failoverCacheAgent{useCaching: tc.useCaching, native: tc.native, prompt: prompt, static: failoverStatic, provider: tc.provider,
				tools: registered, directToolCache: tc.native}.build()
			beforeMessages, beforeTools := pycopy.DeepCopy(messages), pycopy.DeepCopy(registered)
			decorated, _, planned := redecoratePromptCacheForProvider(ctx, a, messages, nil, nil, nil)
			if len(planned) != 1 || !reflect.DeepEqual(planned[0]["function"], registered[0].(map[string]any)["function"]) {
				t.Fatalf("%s planned tools %v", tc.name, planned)
			}
			_, toolMarked := planned[0]["cache_control"]
			if (countCacheMarkers(decorated) > 0) != tc.useCaching || toolMarked != tc.native {
				t.Fatalf("%s markers %d tool marked %v", tc.name, countCacheMarkers(decorated), toolMarked)
			}
			if !reflect.DeepEqual(any(messages), beforeMessages) || !reflect.DeepEqual(any(registered), beforeTools) {
				t.Fatalf("%s inputs mutated", tc.name)
			}
		}
	})
	t.Run("test_cache_off_to_cache_on_adds_breakpoints", func(t *testing.T) {
		prompt := failoverStatic + "Model: gpt-5.4-mini\nProvider: openai"
		undecorated := []map[string]any{{"role": "system", "content": prompt}, {"role": "user", "content": "hello"},
			{"role": "assistant", "content": "hi"}, {"role": "user", "content": "again"}}
		a := failoverCacheAgent{useCaching: true, native: true, prompt: prompt, static: failoverStatic, provider: "anthropic"}.build()
		decorated, _, _ := redecoratePromptCacheForProvider(ctx, a, undecorated, nil, nil, nil)
		system := partMaps(decorated[0]["content"])
		if countCacheMarkers(decorated) < 2 || len(system) == 0 || system[0]["text"] != failoverStatic {
			t.Fatalf("decorated %v", decorated)
		}
	})
	t.Run("test_replans_tools_for_the_active_destination", func(t *testing.T) {
		toolDefs := []map[string]any{{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}
		messages := []map[string]any{{"role": "system", "content": failoverStatic + "volatile"}, {"role": "user", "content": "lookup"}}
		sourceMessages, sourceTools := provider.BuildPromptCachePlan(messages, toolDefs, provider.PromptCachePlanOptions{NativeAnthropic: true,
			StaticSystemPrefix: failoverStatic, DirectNativeToolCache: true})
		a := failoverCacheAgent{useCaching: true, prompt: messages[0]["content"].(string), static: failoverStatic, provider: "openrouter",
			tools: []any{toolDefs[0]}}.build()
		fallbackMessages, _, fallbackTools := redecoratePromptCacheForProvider(ctx, a, sourceMessages, nil, nil, sourceTools)
		if _, ok := sourceTools[len(sourceTools)-1]["cache_control"]; !ok {
			t.Fatal("the source plan marks the last tool")
		}
		if _, ok := fallbackTools[len(fallbackTools)-1]["cache_control"]; ok {
			t.Fatal("the fallback destination takes no tool marker")
		}
		if _, ok := toolDefs[0]["cache_control"]; ok {
			t.Fatal("the registered tools were mutated")
		}
		for _, part := range partMaps(fallbackMessages[0]["content"]) {
			if part["cache_control"] == nil {
				t.Fatalf("system part without a breakpoint %v", part)
			}
		}
	})
	t.Run("test_moa_guidance_stays_outside_last_breakpoint", func(t *testing.T) {
		guidance := "[Mixture of Agents context — use this as private guidance for the normal Icebath agent loop.]\nAggregator: agg\n\nadvice"
		base := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "task"}, {"role": "assistant", "content": "ok"},
			{"role": "user", "content": "task"}, {"role": "user", "content": guidance}}
		decorated := provider.ApplyAnthropicCacheControl(base, "", true, "", false)
		a := failoverCacheAgent{useCaching: true, native: true, prompt: "sys", provider: "moa"}.build()
		a.client = rebasingMoaClient{}
		out, newPrepared, planned := redecoratePromptCacheForProvider(ctx, a, decorated, nil, map[string]any{"guidance": guidance, "messages": decorated}, nil)
		if newPrepared == nil || len(planned) != 0 {
			t.Fatalf("prepared %v planned %v", newPrepared, planned)
		}
		last := out[len(out)-1]
		if last["role"] != "user" {
			t.Fatalf("last row %v", last)
		}
		if text, ok := last["content"].(string); ok {
			if !strings.Contains(text, guidance) {
				t.Fatalf("guidance missing from %q", text)
			}
			return
		}
		var marked, guidanceParts int
		for _, p := range partMaps(last["content"]) {
			text, _ := p["text"].(string)
			if _, ok := p["cache_control"]; ok {
				marked++
				if strings.Contains(text, guidance) {
					t.Fatal("the marked part must not contain the guidance body")
				}
			}
			if strings.Contains(text, guidance) {
				guidanceParts++
			}
		}
		if marked == 0 || guidanceParts == 0 {
			t.Fatalf("marked %d guidance parts %d in %v", marked, guidanceParts, last["content"])
		}
	})
}

func TestPeelReferenceGuidanceRoundTrip(t *testing.T) {
	caseHome(t)
	const guidance = "[Mixture of Agents reference context]\nAdvice body."
	t.Run("test_list_part_shape", func(t *testing.T) {
		base := []map[string]any{{"role": "system", "content": "sys"},
			{"role": "user", "content": []any{map[string]any{"type": "text", "text": "task", "cache_control": map[string]any{"type": "ephemeral"}}}}}
		attached := attachReferenceGuidance(pycopy.DeepCopy(base).([]map[string]any), guidance)
		if reflect.DeepEqual(attached, base) {
			t.Fatal("attach must change the transcript")
		}
		if peeled := peelReferenceGuidance(attached, guidance); !reflect.DeepEqual(peeled, base) {
			t.Fatalf("peeled %v", peeled)
		}
	})
	t.Run("test_guidance_only_part_drops_message_not_empty_residue", func(t *testing.T) {
		messages := []map[string]any{{"role": "user", "content": "task"}, {"role": "assistant", "content": "ok"},
			{"role": "user", "content": []any{map[string]any{"type": "text", "text": guidance}}}}
		if peeled := peelReferenceGuidance(messages, guidance); !reflect.DeepEqual(peeled, messages[:2]) {
			t.Fatalf("peeled %v", peeled)
		}
	})
}
