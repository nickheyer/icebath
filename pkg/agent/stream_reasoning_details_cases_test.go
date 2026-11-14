package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func reasoningDetailsFixture(delivery string) ([]any, []map[string]any) {
	details := []any{
		map[string]any{"type": "opaque.native", "version": 1, "messages": []any{
			map[string]any{"type": "thinking", "signature": " signed+/=\n", "thinking": "α"},
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "redacted_thinking", "data": "AA+/=="}},
			"projection": map[string]any{"content": "hello world", "tool_calls": []any{}}},
		map[string]any{"type": "opaque.future", "index": 7, "unknown": []any{nil, false, map[string]any{"signature": "\r\n"}}},
	}
	if delivery == "absent" {
		details = []any{}
	}
	deltas := []map[string]any{{"role": "assistant", "content": "hello"}, {"content": " world"}}
	switch delivery {
	case "multiple":
		deltas[0]["reasoning_details"] = details[:1]
		deltas = append(deltas, map[string]any{"reasoning_details": []any{}}, map[string]any{"reasoning_details": details[1:]})
	case "final":
		deltas = append(deltas, map[string]any{"reasoning_details": details})
	}
	return details, deltas
}

type reasoningDetailsWire struct {
	mu       sync.Mutex
	requests []map[string]any
	base     string
}

func newReasoningDetailsWire(t *testing.T, deltas []map[string]any) *reasoningDetailsWire {
	t.Helper()
	w := &reasoningDetailsWire{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(rw, r)
			return
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		w.mu.Lock()
		w.requests = append(w.requests, body)
		w.mu.Unlock()
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.WriteHeader(http.StatusOK)
		flusher, _ := rw.(http.Flusher)
		for index, delta := range deltas {
			var finish any
			if index == len(deltas)-1 {
				finish = "stop"
			}
			chunk, _ := json.Marshal(map[string]any{"id": "local-proof", "object": "chat.completion.chunk", "created": 1, "model": "test-model",
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			_, _ = io.WriteString(rw, "data: "+string(chunk)+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(rw, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	w.base = srv.URL + "/v1"
	return w
}

func (w *reasoningDetailsWire) recorded() []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]map[string]any(nil), w.requests...)
}

func reasoningDetailsAgent(t *testing.T, baseURL string) (*AIAgent, context.Context) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	env := &tools.Env{Home: h, Cwd: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{APIKey: "local-test", BaseURL: baseURL, Provider: "custom", Model: "test-model", Platform: "subagent",
		QuietMode: true, SkipMemory: true, SkipContextFiles: true, EnabledToolsets: []string{}, SaveTrajectories: false, Cwd: h, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a, ctx
}

func relayCollectedResponse(t *testing.T, baseURL string, kwargs map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": kwargs["model"], "messages": kwargs["messages"], "stream": true})
	req, err := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer local-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	acc := provider.NewRelayChatAccumulator()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		acc.Observe(chunk)
	}
	recorded := acc.Finalize()
	choices, _ := recorded["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("relay accumulator finalized no choice: %v", recorded)
	}
	choices[0].(map[string]any)["index"] = 0
	response := map[string]any{"id": "local-proof", "created": 1, "object": "chat.completion"}
	for k, v := range recorded {
		response[k] = v
	}
	return response
}

// Replayed session value as canonical JSON data, ordered objects included
func replayedJSON(t *testing.T, v any) any {
	t.Helper()
	if v == nil {
		return nil
	}
	text, err := state.DumpsUnicode(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStreamReasoningDetailsSurviveReplay(t *testing.T) {
	for _, collector := range []string{"main", "relay"} {
		for _, delivery := range []string{"final", "multiple", "absent"} {
			t.Run(collector+"/"+delivery, func(t *testing.T) {
				details, deltas := reasoningDetailsFixture(delivery)
				var want any
				if len(details) > 0 {
					want = parityJSON(t, details)
				}
				wire := newReasoningDetailsWire(t, deltas)
				a, ctx := reasoningDetailsAgent(t, wire.base)
				kwargs := map[string]any{"model": "test-model", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
				var response map[string]any
				if collector == "main" {
					streamed, err := a.interruptibleStreamingAPICall(ctx, kwargs, nil)
					if err != nil {
						t.Fatal(err)
					}
					response, _ = parityJSON(t, streamed).(map[string]any)
				} else {
					response = relayCollectedResponse(t, wire.base, kwargs)
				}
				requests := wire.recorded()
				if len(requests) == 0 {
					t.Fatal("no chat completion request reached the wire")
				}
				for _, request := range requests {
					if request["stream"] != true {
						t.Fatalf("request was not streamed: %v", request)
					}
				}
				if got := parityAt(t, parityJSON(t, response), "choices", 0, "message", "content"); got != "hello world" {
					t.Fatalf("content %v", got)
				}
				if got := parityAt(t, parityJSON(t, response), "choices", 0, "message", "reasoning_details"); !reflect.DeepEqual(got, want) {
					t.Fatalf("collected reasoning_details %v want %v", got, want)
				}
				normalized, err := provider.GetTransport("chat_completions").NormalizeResponse(response, kwargs)
				if err != nil {
					t.Fatal(err)
				}
				if got := parityJSON(t, normalized.Message.ReasoningDetails); !reflect.DeepEqual(got, want) {
					t.Fatalf("normalized reasoning_details %v want %v", got, want)
				}
				message := a.buildAssistantMessage(normalized, normalized.FinishReason)
				if got := parityJSON(t, message["reasoning_details"]); !reflect.DeepEqual(got, want) {
					t.Fatalf("assistant row reasoning_details %v want %v", got, want)
				}
				dbPath := filepath.Join(t.TempDir(), "replay.db")
				db, err := state.NewSessionDB(ctx, dbPath, false)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.CreateSession(state.NewSession{ID: "reasoning-proof", Source: "cli"}); err != nil {
					t.Fatal(err)
				}
				if _, err := db.AppendMessagesBatch(ctx, "reasoning-proof", []map[string]any{message}, state.AppendGuards{}, nil); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				db, err = state.NewSessionDB(ctx, dbPath, false)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				replay, err := db.GetMessagesAsConversation(ctx, "reasoning-proof", state.ConversationOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if len(replay) == 0 {
					t.Fatal("replay returned no message")
				}
				if got := replayedJSON(t, replay[0]["reasoning_details"]); !reflect.DeepEqual(got, want) {
					t.Fatalf("replayed reasoning_details %v want %v", got, want)
				}
			})
		}
	}
}
