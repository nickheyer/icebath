package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var wireSnapshotReasoning = []struct {
	label     string
	reasoning map[string]any
}{
	{"unset", nil},
	{"high", map[string]any{"enabled": true, "effort": "high"}},
	{"off", map[string]any{"enabled": false}},
}

const wireSnapshotTools = `[{"type": "function", "function": {"name": "terminal", "description": "Run a shell command.",
 "parameters": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}}]`

const wireSnapshotMessages = `[{"role": "system", "content": "You are a test agent."}, {"role": "user", "content": "list files"},
 {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "terminal", "arguments": "{\"command\": \"ls\"}"}}]},
 {"role": "tool", "tool_call_id": "call_1", "content": "a.txt\nb.txt"}, {"role": "user", "content": "thanks, now summarize"}]`

var wireSnapshotModels = map[string]string{
	"nous": "anthropic/claude-opus-5.5", "openai-codex": "gpt-5.5", "openai-api": "gpt-5.5",
	"xai-oauth": "grok-4", "qwen-oauth": "qwen3-coder-plus", "lmstudio": "qwen/qwen3-8b",
	"gemini": "gemini-3-pro", "kimi-coding": "kimi-k3", "kimi-coding-cn": "kimi-k2.5",
	"stepfun": "step-3", "arcee": "trinity-large", "actual": "actual-1", "minimax": "MiniMax-M3",
	"minimax-oauth": "MiniMax-M3", "minimax-cn": "MiniMax-M3", "anthropic": "claude-opus-5.5",
	"alibaba": "qwen3.5-plus", "alibaba-coding-plan": "qwen3-coder-plus", "xai": "grok-4",
	"ai-gateway": "anthropic/claude-opus-5.5", "opencode-zen": "claude-opus-5.5", "opencode-go": "kimi-k2.5",
	"kilocode": "anthropic/claude-opus-5.5", "xiaomi": "mimo-v2-pro", "tencent-tokenhub": "hunyuan-t1",
	"tencent-tokenplan": "hunyuan-t1", "ollama-cloud": "gpt-oss:120b",
	"bedrock": "us.anthropic.claude-opus-5-5-v1:0", "vertex": "gemini-3-pro", "azure-foundry": "gpt-5.5",
	"alibaba-cn": "qwen3.5-plus", "alibaba-token-plan": "qwen3.5-plus", "alibaba-token-plan-cn": "qwen3.5-plus",
	"alibaba-coding-plan-cn": "qwen3-coder-plus", "deepinfra": "deepseek-ai/DeepSeek-V4",
	"router": "openai/gpt-5.5", "openrouter": "anthropic/claude-opus-5.5", "custom": "my-local-model",
}

var wireSnapshotBaseURLs = map[string]string{
	"vertex":        "https://aiplatform.googleapis.com/v1/projects/p/locations/global/endpoints/openapi",
	"azure-foundry": "https://example.openai.azure.com/openai/v1",
	"openrouter":    "https://openrouter.ai/api/v1",
	"custom":        "http://127.0.0.1:8080/v1",
}

var wireSnapshotBaseURLOnly = [][3]string{
	{"base-url:openrouter", "anthropic/claude-opus-5.5", "https://openrouter.ai/api/v1"},
	{"base-url:nous", "anthropic/claude-opus-5.5", "https://inference-api.nousresearch.com/v1"},
	{"base-url:kimi", "kimi-k3", "https://api.moonshot.ai/v1"},
	{"base-url:kimi-coding", "kimi-k3", "https://api.kimi.com/coding/v1"},
	{"base-url:tokenhub", "hunyuan-t1", "https://tokenhub.tencentmaas.com/v1"},
	{"base-url:nvidia-nim", "nvidia/nemotron-3-super", "https://integrate.api.nvidia.com/v1"},
	{"base-url:qwen-portal", "qwen3-coder-plus", "https://portal.qwen.ai/v1"},
	{"base-url:lmstudio-port", "qwen/qwen3-8b", "http://127.0.0.1:1234/v1"},
	{"base-url:ollama", "qwen3:8b", "http://127.0.0.1:11434/v1"},
	{"base-url:generic", "my-model", "https://llm.example.com/v1"},
}

var wireSnapshotVolatile = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`client=icebath-client-v[^"]*`), "client=icebath-client-v<VERSION>"},
	{regexp.MustCompile(`"promptId": ?"[0-9a-f-]{36}"`), `"promptId": "<UUID>"`},
}

// Provider cells of the auth registry, one per profile name, external process providers left out
func wireSnapshotCells(ctx context.Context) [][3]string {
	seen := map[string]bool{}
	var cells [][3]string
	for _, key := range provider.RegistryKeys(ctx) {
		row := provider.RegistryRow(ctx, key)
		if row == nil {
			continue
		}
		profile, ok := provider.GetProviderProfile(ctx, key)
		name := key
		if ok {
			name = profile.Name
		}
		if seen[name] || row.AuthType == provider.AuthTypeExternalProcess {
			continue
		}
		seen[name] = true
		fallback := "test-model"
		if ok && len(profile.FallbackModels) > 0 {
			fallback = profile.FallbackModels[0]
		}
		model, has := wireSnapshotModels[name]
		if !has {
			model = fallback
		}
		baseURL := wireSnapshotBaseURLs[name]
		if baseURL == "" {
			baseURL, _ = provider.RegistryInferenceBaseURL(key)
		}
		cells = append(cells, [3]string{name, model, baseURL})
	}
	for _, name := range []string{"openrouter", "custom"} {
		if !seen[name] {
			cells = append(cells, [3]string{name, wireSnapshotModels[name], wireSnapshotBaseURLs[name]})
		}
	}
	return cells
}

func wireSnapshotJSON(t *testing.T, raw string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// No network and no ambient AWS profile, so capability probes take their offline answer
func wireSnapshotOffline(t *testing.T) {
	t.Helper()
	caseHome(t)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)
}

// Request kwargs of the production assembly path for one route under each reasoning setting
func wireSnapshotWire(t *testing.T, providerName, model, baseURL string) map[string]any {
	t.Helper()
	toolDefs := []map[string]any{}
	for _, d := range wireSnapshotJSON(t, wireSnapshotTools).([]any) {
		toolDefs = append(toolDefs, d.(map[string]any))
	}
	prev := loadToolDefinitions
	loadToolDefinitions = func(context.Context, *tools.Env, tools.DefinitionOptions) []map[string]any { return toolDefs }
	defer func() { loadToolDefinitions = prev }()
	env := promptTestEnv(t)
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{Provider: providerName, Model: model, BaseURL: baseURL, APIKey: "sk-test-0000000000",
		QuietMode: true, SkipContextFiles: true, SkipMemory: true, SessionID: "wire-snapshot", Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	applyPerTurnResetState(a)
	out := map[string]any{"api_mode": a.apiMode}
	for _, r := range wireSnapshotReasoning {
		a.reasoningConfig = r.reasoning
		var messages []map[string]any
		for _, m := range wireSnapshotJSON(t, wireSnapshotMessages).([]any) {
			messages = append(messages, m.(map[string]any))
		}
		system := messages[0]["content"].(string)
		s := &LoopState{APIMessages: anyRows(messages), ToolsForAPI: toolSurface(a.tools), SystemMessage: &system, Messages: toStateMessages(messages),
			OriginalUserMessage: messages[len(messages)-1]["content"], APICallCount: 1, APIRequestID: "req-1", EffectiveTaskID: "task-1", TurnID: "turn-1"}
		if err := buildAPIRequest(ctx, a, s); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(wireSnapshotPlain(s.APIKwargs))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, v := range wireSnapshotVolatile {
			text = v.pattern.ReplaceAllString(text, v.replacement)
		}
		out[r.label] = wireSnapshotJSON(t, text)
	}
	return wireSnapshotJSON(t, mustJSON(t, out)).(map[string]any)
}

// JSON safe form of the kwargs, a value JSON cannot hold rendered as its repr the way default=repr does
func wireSnapshotPlain(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = wireSnapshotPlain(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = wireSnapshotPlain(val)
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = wireSnapshotPlain(val)
		}
		return out
	case []*state.Message:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = wireSnapshotPlain(state.MessageToDict(val))
		}
		return out
	}
	if _, err := json.Marshal(v); err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return v
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Path of the first key where the recorded and the built wire differ
func wireFirstDivergence(expected, actual any, path string) string {
	short := func(v any) string {
		data, _ := json.Marshal(v)
		if len(data) > 200 {
			data = data[:200]
		}
		return string(data)
	}
	if e, ok := expected.(map[string]any); ok {
		if a, ok := actual.(map[string]any); ok {
			keys := map[string]bool{}
			for k := range e {
				keys[k] = true
			}
			for k := range a {
				keys[k] = true
			}
			sorted := make([]string, 0, len(keys))
			for k := range keys {
				sorted = append(sorted, k)
			}
			sort.Strings(sorted)
			for _, k := range sorted {
				ev, inE := e[k]
				av, inA := a[k]
				switch {
				case !inA:
					return fmt.Sprintf("%s.%s: missing (expected %s)", path, k, short(ev))
				case !inE:
					return fmt.Sprintf("%s.%s: unexpected %s", path, k, short(av))
				case !reflect.DeepEqual(ev, av):
					return wireFirstDivergence(ev, av, path+"."+k)
				}
			}
		}
	}
	if e, ok := expected.([]any); ok {
		if a, ok := actual.([]any); ok && len(e) == len(a) {
			for i := range e {
				if !reflect.DeepEqual(e[i], a[i]) {
					return wireFirstDivergence(e[i], a[i], fmt.Sprintf("%s[%d]", path, i))
				}
			}
		}
	}
	return fmt.Sprintf("%s: expected %s got %s", path, short(expected), short(actual))
}

func wireSnapshotGolden(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/provider_wire_snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	return wireSnapshotJSON(t, string(data)).(map[string]any)
}

func wireSnapshotCheck(t *testing.T, golden map[string]any, key string, actual map[string]any) {
	t.Helper()
	recorded, ok := golden[key].(map[string]any)
	if !ok {
		t.Fatalf("%s has no recorded wire", key)
	}
	expected := map[string]any{}
	for k, v := range recorded {
		if k != "model" && k != "base_url" {
			expected[k] = v
		}
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("%s wire changed: %s", key, wireFirstDivergence(expected, actual, ""))
	}
}

func TestProviderWireSnapshot(t *testing.T) {
	golden := wireSnapshotGolden(t)
	t.Run("test_provider_wire_matches_snapshot", func(t *testing.T) {
		wireSnapshotOffline(t)
		for _, cell := range wireSnapshotCells(context.Background()) {
			t.Run(cell[0], func(t *testing.T) {
				wireSnapshotOffline(t)
				wireSnapshotCheck(t, golden, cell[0], wireSnapshotWire(t, cell[0], cell[1], cell[2]))
			})
		}
	})
	t.Run("test_base_url_only_wire_matches_snapshot", func(t *testing.T) {
		for _, cell := range wireSnapshotBaseURLOnly {
			t.Run(cell[0], func(t *testing.T) {
				wireSnapshotOffline(t)
				wireSnapshotCheck(t, golden, cell[0], wireSnapshotWire(t, "", cell[1], cell[2]))
			})
		}
	})
	t.Run("test_snapshot_covers_every_provider", func(t *testing.T) {
		wireSnapshotOffline(t)
		current := map[string]bool{}
		for _, cell := range wireSnapshotCells(context.Background()) {
			current[cell[0]] = true
		}
		for _, cell := range wireSnapshotBaseURLOnly {
			current[cell[0]] = true
		}
		var unrecorded, stale []string
		for k := range current {
			if _, ok := golden[k]; !ok {
				unrecorded = append(unrecorded, k)
			}
		}
		for k := range golden {
			if !current[k] {
				stale = append(stale, k)
			}
		}
		slices.Sort(unrecorded)
		slices.Sort(stale)
		if len(unrecorded) > 0 || len(stale) > 0 {
			t.Fatalf("providers without a recorded wire %v, snapshot entries for providers that no longer exist %v", unrecorded, stale)
		}
	})
}
