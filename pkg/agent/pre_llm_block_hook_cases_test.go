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
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	blockMsg    = "Blocked by policy: prompt contains a customer ID."
	blockSecret = "CUSTOMER-ID-4242"
)

// Loopback provider answering MODEL-ANSWER on the stream and plain wires and keeping every chat request
type blockProvider struct {
	mu       sync.Mutex
	requests []map[string]any
	srv      *httptest.Server
}

func newBlockProvider(t *testing.T) *blockProvider {
	p := &blockProvider{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "test-model", "object": "model"}}})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		if _, ok := req["messages"]; ok {
			p.mu.Lock()
			p.requests = append(p.requests, req)
			p.mu.Unlock()
		}
		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, part := range []struct {
				delta  map[string]any
				finish any
			}{{map[string]any{"role": "assistant", "content": "MODEL-ANSWER"}, nil}, {map[string]any{}, "stop"}} {
				chunk, _ := json.Marshal(map[string]any{"id": "m", "choices": []any{map[string]any{"index": 0, "delta": part.delta, "finish_reason": part.finish}}})
				fmt.Fprintf(w, "data: %s\n\n", chunk)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "m", "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11},
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "MODEL-ANSWER"}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *blockProvider) chats() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.requests...)
}

func (p *blockProvider) url() string { return p.srv.URL + "/v1" }

func blockDB(t *testing.T, name string) *state.SessionDB {
	t.Helper()
	db, err := state.NewSessionDB(t.Context(), filepath.Join(t.TempDir(), name), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func blockAgent(t *testing.T, h string, p *blockProvider, db *state.SessionDB) (*AIAgent, context.Context) {
	t.Helper()
	env := &tools.Env{Home: h, Cwd: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{APIKey: "test-key", BaseURL: p.url(), Provider: "openai-compat", Model: "test-model", MaxIterations: 4,
		EnabledToolsets: []string{}, QuietMode: true, SkipContextFiles: true, SkipMemory: true, Platform: "cli", SessionDB: db, SessionID: "s-block",
		Cwd: h, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.saveTrajectories = false
	return a, ctx
}

// Runs one vetoed turn on s-block and returns its result
func blockedSession(t *testing.T, h string, p *blockProvider, db *state.SessionDB) map[string]any {
	t.Helper()
	patchPluginHooks(t, func(hook string, _ map[string]any) []any {
		if hook == "pre_llm_call" {
			return []any{map[string]any{"context": "IGNORED-ON-BLOCK"}, map[string]any{"action": "block", "message": blockMsg}}
		}
		return nil
	})
	a, ctx := blockAgent(t, h, p, db)
	result, err := a.RunConversation(ctx, "look up "+blockSecret, RunConversationOptions{ConversationHistory: []map[string]any{}, TaskID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func blockHome(t *testing.T) string {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func TestBlockedTurnNeverReachesTheProviderAndStoresOnlyThePlaceholder(t *testing.T) {
	h := blockHome(t)
	p := newBlockProvider(t)
	db := blockDB(t, "state.db")
	result := blockedSession(t, h, p, db)
	if len(p.chats()) != 0 || result["final_response"] != blockMsg || result["turn_exit_reason"] != PreLLMBlockExitReason ||
		result["api_calls"] != 0 || result["failed"] != false {
		t.Fatalf("chat requests %d result %v", len(p.chats()), result)
	}
	history := persistConversation(t, db, "s-block", false)
	if len(history) != 2 || history[0]["role"] != "user" || history[0]["content"] != state.PreLLMBlockWithheldText ||
		history[1]["role"] != "assistant" || history[1]["content"] != blockMsg {
		t.Fatalf("history %v", history)
	}
	if typed, ok := state.WithheldPromptText(history[0]); !ok || typed != "look up "+blockSecret {
		t.Fatalf("withheld text %q %v", typed, ok)
	}
	found := tools.SessionSearch(tools.WithToolEnv(context.Background(), &tools.Env{Home: h, Cwd: h}), tools.SessionSearchArgs{Query: "CUSTOMER", DB: db, CurrentSessionID: "other"})
	if strings.Contains(found, blockSecret) {
		t.Fatalf("session search returned the vetoed prompt %s", found)
	}
	patchPluginHooks(t, func(string, map[string]any) []any { return nil })
	followUp, ctx := blockAgent(t, h, p, db)
	next, err := followUp.RunConversation(ctx, "next question", RunConversationOptions{ConversationHistory: history, TaskID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	chats := p.chats()
	if next["final_response"] != "MODEL-ANSWER" || len(chats) != 1 {
		t.Fatalf("follow up %v after %d chat requests", next["final_response"], len(chats))
	}
	wire, _ := json.Marshal(chats[0]["messages"])
	if strings.Contains(string(wire), blockSecret) || !strings.Contains(string(wire), state.PreLLMBlockWithheldText) {
		t.Fatalf("wire %s", wire)
	}
}

func TestCompactionOfABlockedTurnNeverSendsThePrompt(t *testing.T) {
	h := blockHome(t)
	p := newBlockProvider(t)
	cfg := fmt.Sprintf("auxiliary:\n  compression: {provider: custom, base_url: '%s', api_key: k, model: m}\n", p.url())
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	db := blockDB(t, "state.db")
	blockedSession(t, h, p, db)
	blockedPair := persistConversation(t, db, "s-block", false)
	var filler []map[string]any
	for i := range 8 {
		for _, role := range []string{"user", "assistant"} {
			filler = append(filler, map[string]any{"role": role, "content": fmt.Sprintf("%s %d ", role, i) + strings.Repeat("x", 400)})
		}
	}
	a, ctx := blockAgent(t, h, p, db)
	messages := append(append(append([]map[string]any{}, filler[:12]...), blockedPair...), filler...)
	system := "sys"
	approx := 150000
	compressed, _, err := compressContext(ctx, a, messages, &system, compressContextOptions{ApproxTokens: &approx, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	chats := p.chats()
	if len(chats) == 0 {
		t.Fatal("the summarizer was never called")
	}
	if wire, _ := json.Marshal(chats); strings.Contains(string(wire), blockSecret) {
		t.Fatalf("the summary request carried the vetoed prompt %s", wire)
	}
	for _, m := range compressed {
		for _, key := range []string{"content", "api_content"} {
			if text, _ := json.Marshal(m[key]); strings.Contains(string(text), blockSecret) {
				t.Fatalf("the compacted history carries the vetoed prompt in %s of %v", key, m)
			}
		}
	}
}

func TestRetryAndUndoAfterABlockResubmitTheTypedText(t *testing.T) {
	h := blockHome(t)
	p := newBlockProvider(t)
	typed := "look up " + blockSecret
	t.Run("receipt", func(t *testing.T) {
		db := blockDB(t, "receipt.db")
		blockedSession(t, h, p, db)
		epoch, err := db.BeginRuntimeEpoch(t.Context(), "owner")
		if err != nil {
			t.Fatal(err)
		}
		rows, err := db.GetMessages(t.Context(), "s-block", state.GetMessagesOptions{})
		if err != nil || len(rows) == 0 {
			t.Fatalf("rows %v %v", rows, err)
		}
		target, _ := rows[0].Get("id")
		generation := int64(0)
		receipt, err := db.MutateRuntimeSession(t.Context(), state.RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "s-block", RequestID: "r",
			ExpectedRevision: 0, ExpectedGeneration: &generation, Operation: "rewind", Payload: pyjson.ObjectOf("target_message_id", target)})
		if err != nil {
			t.Fatal(err)
		}
		message, _ := receipt.Get("target_message")
		targetMessage, _ := message.(*pyjson.Object)
		if targetMessage == nil {
			t.Fatalf("receipt target message %T %v in %v", message, message, receipt.Keys)
		}
		if got, _ := targetMessage.Get("content"); got != typed {
			t.Fatalf("receipt target content %v", got)
		}
	})
	t.Run("rewind", func(t *testing.T) {
		db := blockDB(t, "rewind.db")
		blockedSession(t, h, p, db)
		history := persistConversation(t, db, "s-block", false)
		outcome, err := db.RewindUserTurn(t.Context(), "s-block", -1, state.RewindUserTurnOptions{WarmHistory: history, HasWarmHistory: true, RequireRetryable: true})
		if err != nil {
			t.Fatal(err)
		}
		if outcome.LiveText != typed || len(outcome.Prefix) != 0 {
			t.Fatalf("live text %q prefix %v", outcome.LiveText, outcome.Prefix)
		}
		patchPluginHooks(t, func(hook string, kwargs map[string]any) []any {
			if message, _ := kwargs["user_message"].(string); hook == "pre_llm_call" && strings.Contains(message, blockSecret) {
				return []any{map[string]any{"action": "block", "message": blockMsg}}
			}
			return nil
		})
		a, ctx := blockAgent(t, h, p, db)
		again, err := a.RunConversation(ctx, outcome.LiveText, RunConversationOptions{ConversationHistory: []map[string]any{}, TaskID: "t"})
		if err != nil {
			t.Fatal(err)
		}
		if again["turn_exit_reason"] != PreLLMBlockExitReason || len(p.chats()) != 0 {
			t.Fatalf("exit %v chat requests %d", again["turn_exit_reason"], len(p.chats()))
		}
	})
}
