package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Loopback chat completions provider answering scripted texts and recording every main loop body
type voiceFakeLLM struct {
	mu     sync.Mutex
	script []string
	main   []map[string]any
	srv    *httptest.Server
}

func newVoiceFakeLLM(t *testing.T, script ...string) *voiceFakeLLM {
	t.Helper()
	f := &voiceFakeLLM{script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *voiceFakeLLM) baseURL() string { return f.srv.URL + "/v1" }

func (f *voiceFakeLLM) mainRequests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.main...)
}

// Serves the model list and scripted completions, streamed as SSE chunks when the body asks for a stream
func (f *voiceFakeLLM) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/models") {
			writeVoiceJSON(w, map[string]any{"object": "list", "data": []any{map[string]any{"id": "fake-model", "object": "model", "context_length": 128000}}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	body := map[string]any{}
	if err := json.Unmarshal(raw, &body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	text := "ok"
	f.mu.Lock()
	if tools, _ := body["tools"].([]any); len(tools) > 0 {
		f.main = append(f.main, body)
		if len(f.script) > 0 {
			text, f.script = f.script[0], f.script[1:]
		}
	}
	f.mu.Unlock()
	usage := map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120, "prompt_tokens_details": map[string]any{"cached_tokens": 0}}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "fake-model",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	if stream, _ := body["stream"].(bool); !stream {
		writeVoiceJSON(w, map[string]any{"id": "chatcmpl-fake", "object": "chat.completion", "created": time.Now().Unix(), "model": "fake-model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}}, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	sse := func(payload map[string]any) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	sse(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	for i := 0; i < len(text); i += 8 {
		sse(chunk(map[string]any{"content": text[i:min(i+8, len(text))]}, nil))
	}
	last := chunk(map[string]any{}, "stop")
	last["usage"] = usage
	sse(last)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func writeVoiceJSON(w http.ResponseWriter, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// Temp home whose config routes the main model to baseURL with extra config appended
func writeVoiceHome(t *testing.T, baseURL, extra string) string {
	t.Helper()
	h := t.TempDir()
	cfg := "model:\n  provider: custom\n  base_url: " + baseURL + "\n  default: fake-model\n  context_length: 128000\n" +
		"agent:\n  api_max_retries: 1\ntelemetry:\n  shared_metrics:\n    enabled: false\n    send: false\n" + extra
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("OPENAI_API_KEY=sk-fake-e2e\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func newVoiceAgent(t *testing.T, h, mainURL string) (context.Context, *AIAgent) {
	t.Helper()
	env := &tools.Env{Home: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{Model: "fake-model", Provider: "custom", BaseURL: mainURL, APIKey: "sk-fake-e2e",
		QuietMode: true, SkipContextFiles: true, SkipMemory: true, EnabledToolsets: []string{"memory"}, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, a
}

func TestReasoningOffFallsToTheLowestValidLevel(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cases := []struct{ model, apiMode, expected string }{
		{"gpt-6-astra", "codex_responses", "low"},
		{"claude-opus-5-5", "anthropic_messages", "low"},
		{"gpt-5.6-sol", "codex_responses", ""},
		{"claude-sonnet-4-6", "anthropic_messages", ""},
	}
	for _, tc := range cases {
		baseURL := "https://api.anthropic.com"
		if strings.Contains(tc.model, "gpt") {
			baseURL = "https://api.openai.com/v1"
		}
		a := &AIAgent{model: tc.model, apiMode: tc.apiMode, provider: "custom", baseURL: baseURL}
		got, err := voiceReasoning(context.Background(), a, map[string]any{"enabled": false})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"enabled": false}
		if tc.expected != "" {
			want = map[string]any{"enabled": true, "effort": tc.expected}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s %s got %v want %v", tc.model, tc.apiMode, got, want)
		}
	}
}

func TestSessionRuntimeViewPreservesRequestedProviderDuringVoiceTurn(t *testing.T) {
	main := &AIAgent{model: "voice-model", provider: "custom", requestedProvider: "custom:voice",
		baseURL: "https://voice.example/v1", apiMode: "chat_completions", reasoningConfig: map[string]any{"enabled": false}}
	main.voiceRouteState = &voiceRouteState{
		snapshot: map[string]any{"model": "main-model", "provider": "custom", "requested_provider": "custom:main",
			"base_url": "https://main.example/v1", "api_mode": "chat_completions"},
		reasoningConfig: map[string]any{"enabled": true},
	}
	view := SessionRuntimeView(main)
	if view.Provider() != "custom" || view.RequestedProvider() != "custom:main" {
		t.Fatalf("view %q %q", view.Provider(), view.RequestedProvider())
	}
}

func TestVoiceUsageNeverBecomesTheSessionRoute(t *testing.T) {
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli", Model: "main-model"}); err != nil {
		t.Fatal(err)
	}
	model, billing := "voice-model", "voicep"
	if err := db.UpdateTokenCounts("s1", state.TokenCountDelta{InputTokens: 10, OutputTokens: 5, Model: &model,
		BillingProvider: &billing, APICallCount: 1, Task: "voice_chat"}); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetSession(ctx, "s1")
	if err != nil || row == nil {
		t.Fatalf("row %v err %v", row, err)
	}
	if got, _ := row.Get("model"); got != "main-model" {
		t.Fatalf("session model %v", got)
	}
	if got, _ := row.Get("input_tokens"); fmt.Sprint(got) != "10" {
		t.Fatalf("session input tokens %v", got)
	}
	byTask, err := db.AuxiliaryUsageByTask(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	voiceUsage, _ := byTask.Get("voice_chat")
	usage, ok := voiceUsage.(interface{ Get(string) (any, bool) })
	if !ok {
		t.Fatalf("voice_chat usage %T", voiceUsage)
	}
	if got, _ := usage.Get("input_tokens"); fmt.Sprint(got) != "10" {
		t.Fatalf("voice_chat input tokens %v", got)
	}
	route, err := db.GetRecentSessionModelRoute(ctx, "s1")
	if err != nil || route != nil {
		t.Fatalf("route %v err %v", route, err)
	}
}

func TestBeginAndEndVoiceTurnRouteSwapAndRestoreTheSessionRoute(t *testing.T) {
	for _, tc := range []struct {
		name          string
		separate      bool
		contextLength string
		content       string
		boundModel    string
	}{
		{"main model only", false, "", "hi", "fake-model"},
		{"separate voice model", true, "128000", "hi", "voice-model"},
		{"voice window too small", true, "1000", strings.Repeat("spoken words ", 4000), "fake-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			main, voice := newVoiceFakeLLM(t), newVoiceFakeLLM(t)
			extra := ""
			if tc.separate {
				extra = "auxiliary:\n  voice_chat:\n    provider: custom\n" +
					"    base_url: " + voice.baseURL() + "\n    model: voice-model\n    api_key: sk-fake-voice\n" +
					"custom_providers:\n  - name: voice\n" +
					"    base_url: " + voice.baseURL() + "\n    models:\n      voice-model:\n        context_length: " + tc.contextLength + "\n"
			}
			ctx, a := newVoiceAgent(t, writeVoiceHome(t, main.baseURL(), extra), main.baseURL())
			a.reasoningConfig = map[string]any{"enabled": true, "effort": "low"}
			a.SetVoiceTurnPending(true)
			messages := []*state.Message{{Role: "user", Content: tc.content}}
			if _, err := beginVoiceTurnRoute(ctx, a, messages, "system"); err != nil {
				t.Fatal(err)
			}
			rejected := tc.separate && tc.boundModel != "voice-model"
			turnReasoning := map[string]any{"enabled": false}
			if rejected {
				turnReasoning = map[string]any{"enabled": true, "effort": "low"}
			}
			if a.voiceTurnPending || !reflect.DeepEqual(a.reasoningConfig, turnReasoning) || a.model != tc.boundModel {
				t.Fatalf("voice turn pending %v reasoning %v model %s", a.voiceTurnPending, a.reasoningConfig, a.model)
			}
			routed := tc.boundModel == "voice-model"
			if (a.turnRouteTask == voiceChatTask) != routed || (a.voiceRouteState.snapshot != nil) != routed || a.warnedVoiceRoute != rejected {
				t.Fatalf("route task %q snapshot %v warned %v", a.turnRouteTask, a.voiceRouteState.snapshot != nil, a.warnedVoiceRoute)
			}
			if routed && strings.TrimRight(a.baseURL, "/") != strings.TrimRight(voice.baseURL(), "/") {
				t.Fatalf("voice base url %s", a.baseURL)
			}
			view := SessionRuntimeView(a)
			if view.Model() != "fake-model" || !reflect.DeepEqual(view.ReasoningConfig(), map[string]any{"enabled": true, "effort": "low"}) {
				t.Fatalf("view %s %v", view.Model(), view.ReasoningConfig())
			}
			endVoiceTurnRoute(ctx, a)
			endVoiceTurnRoute(ctx, a)
			if a.model != "fake-model" || strings.TrimRight(a.baseURL, "/") != strings.TrimRight(main.baseURL(), "/") || a.fallbackActivated {
				t.Fatalf("restored route %s %s fallback %v", a.model, a.baseURL, a.fallbackActivated)
			}
			if a.voiceRouteState != nil || a.turnRouteTask != "" || !reflect.DeepEqual(a.reasoningConfig, map[string]any{"enabled": true, "effort": "low"}) {
				t.Fatalf("state %v task %q reasoning %v", a.voiceRouteState, a.turnRouteTask, a.reasoningConfig)
			}
			if got, ok := SessionRuntimeView(a).(*AIAgent); !ok || got != a {
				t.Fatalf("view outside a voice turn %T", SessionRuntimeView(a))
			}
		})
	}
}
