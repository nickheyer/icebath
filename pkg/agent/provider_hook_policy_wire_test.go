package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const hookPolicyHost = "https://api.hookpolicy.test/v1"

// Registers a host claiming profile with the given hooks for one test, the registry restored afterwards
func makeHookPolicyProfile(t *testing.T, p provider.Profile) {
	t.Helper()
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	p.Name, p.BaseURL, p.Hostnames = "hookpolicy", hookPolicyHost, []string{"hookpolicy.test"}
	p.MessageCarrierOpaque, p.MessageCarrierText = "hp_opaque", "hp_text"
	provider.RegisterProvider(p)
}

// Real agent on a custom route pointed at the hook policy host
func hookPolicyAgent(t *testing.T) (context.Context, *AIAgent) {
	t.Helper()
	env := &tools.Env{Home: t.TempDir()}
	t.Setenv("ICEBATH_HOME", env.Home)
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{APIKey: "k", BaseURL: hookPolicyHost, Provider: "custom", APIMode: "chat_completions", Model: "m1",
		QuietMode: true, SkipContextFiles: true, SkipMemory: true, EnabledToolsets: []string{}, ReasoningConfig: map[string]any{"enabled": true, "effort": "high"}, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return ctx, a
}

// History of turns whose assistant rows carry the native carrier record, text on the chosen turns
func hookPolicyHistory(turns int, textEvery int, textAt ...int) []map[string]any {
	hist := []map[string]any{{"role": "system", "content": "s"}}
	for i := range turns {
		record := map[string]any{"type": "icebath.native_assistant", "hp_opaque": fmt.Sprintf("OPQ%d", i)}
		if (textEvery > 0 && i%textEvery == 0) || containsInt(textAt, i) {
			record["hp_text"] = fmt.Sprintf("T%d", i)
		}
		hist = append(hist, map[string]any{"role": "user", "content": fmt.Sprintf("u%d", i)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("a%d", i), "reasoning_details": []any{record}})
	}
	return append(hist, map[string]any{"role": "user", "content": "next"})
}

func containsInt(list []int, n int) bool {
	for _, x := range list {
		if x == n {
			return true
		}
	}
	return false
}

// Deep copy through JSON so the request builder never sees the test's own rows
func deepCopyRows(t *testing.T, rows []map[string]any) []map[string]any {
	t.Helper()
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Wire messages and the extra_body reasoning the agent builds for the history
func hookPolicyWire(t *testing.T, a *AIAgent, hist []map[string]any) ([]map[string]any, any) {
	t.Helper()
	kwargs, err := a.BuildAPIKwargs(deepCopyRows(t, hist))
	if err != nil {
		t.Fatal(err)
	}
	var messages []map[string]any
	switch rows := kwargs["messages"].(type) {
	case []map[string]any:
		messages = rows
	case []any:
		for _, r := range rows {
			messages = append(messages, r.(map[string]any))
		}
	default:
		t.Fatalf("messages %T", kwargs["messages"])
	}
	var reasoning any
	if extra, ok := kwargs["extra_body"].(map[string]any); ok {
		reasoning = extra["reasoning"]
	}
	return messages, reasoning
}

// The replay answer every hook policy test expects, the carrier values the record holds
func hookPolicyReplay(values map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"hp_opaque", "hp_text"} {
		if v, ok := values[k]; ok && v != nil && v != "" {
			out[k] = v
		}
	}
	return out
}

func captureHookWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

func TestLongHistoryAsksTheReplayHookOncePerCarrierKeySet(t *testing.T) {
	var calls []string
	makeHookPolicyProfile(t, provider.Profile{ReplayMessageCarriersHook: func(values map[string]any, model, baseURL string) map[string]any {
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		calls = append(calls, strings.Join(sortedStrings(keys), ","))
		return hookPolicyReplay(values)
	}})
	_, a := hookPolicyAgent(t)
	hist := hookPolicyHistory(50, 5)
	messages, _ := hookPolicyWire(t, a, hist)
	if len(calls) != 2 {
		t.Fatalf("hook asked %d times %v, want two key sets", len(calls), calls)
	}
	var replayed [][2]any
	for _, m := range messages {
		if m["role"] == "assistant" {
			replayed = append(replayed, [2]any{m["hp_opaque"], m["hp_text"]})
		}
	}
	var want [][2]any
	for i := range 50 {
		var text any
		if i%5 == 0 {
			text = fmt.Sprintf("T%d", i)
		}
		want = append(want, [2]any{fmt.Sprintf("OPQ%d", i), text})
	}
	if !reflect.DeepEqual(replayed, want) {
		t.Fatalf("replayed %v", replayed)
	}
	hookPolicyWire(t, a, hist)
	if len(calls) != 2 {
		t.Fatalf("the next request asked %d times", len(calls))
	}
}

func sortedStrings(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestFailingReasoningHookKeepsTheSessionOnTheGenericAnswer(t *testing.T) {
	fail, calls := true, 0
	makeHookPolicyProfile(t, provider.Profile{
		ReasoningPayloadHook: func(model string, r provider.Reasoning, baseURL string) map[string]any {
			calls++
			if fail {
				panic("plugin host went away")
			}
			return map[string]any{"effort": "high"}
		},
		ReplayMessageCarriersHook: func(values map[string]any, model, baseURL string) map[string]any { return hookPolicyReplay(values) },
	})
	ctx, a := hookPolicyAgent(t)
	hist := hookPolicyHistory(3, 0)
	type request struct {
		messages  []map[string]any
		reasoning any
		supports  bool
		payload   any
	}
	do := func() request {
		messages, reasoning := hookPolicyWire(t, a, hist)
		return request{messages, reasoning, a.supportsReasoningExtraBody(ctx), a.endpointReasoningPayload(ctx)}
	}
	logs := captureHookWarnings(t)
	first := do()
	fail = false
	later := []request{do(), do()}
	if first.supports || first.payload != nil {
		t.Fatalf("first request gates %v %v, want the generic answer", first.supports, first.payload)
	}
	if first.messages[2]["hp_opaque"] != "OPQ0" {
		t.Fatalf("other hooks stopped working: %v", first.messages[2])
	}
	for _, r := range later {
		if !reflect.DeepEqual(r, first) {
			t.Fatalf("a later request flipped: %v vs %v", r, first)
		}
	}
	if calls != 1 {
		t.Fatalf("hook asked %d times", calls)
	}
	if n := strings.Count(logs.String(), "reasoning_payload"); n != 1 {
		t.Fatalf("%d reasoning_payload warnings:\n%s", n, logs.String())
	}
	_, fresh := hookPolicyAgent(t)
	if got := fresh.endpointReasoningPayload(ctx); !reflect.DeepEqual(got, map[string]any{"effort": "high"}) {
		t.Fatalf("a new session answered %v", got)
	}
}

func TestReplayHookFailingOnALaterCallNeverFlipsBytesAfterThatTurn(t *testing.T) {
	calls := 0
	makeHookPolicyProfile(t, provider.Profile{ReplayMessageCarriersHook: func(values map[string]any, model, baseURL string) map[string]any {
		calls++
		if calls >= 2 {
			panic("plugin bug")
		}
		return hookPolicyReplay(values)
	}})
	_, a := hookPolicyAgent(t)
	first, _ := hookPolicyWire(t, a, hookPolicyHistory(4, 0))
	if first[2]["hp_opaque"] != "OPQ0" {
		t.Fatalf("first wire %v", first[2])
	}
	hist := hookPolicyHistory(4, 0, 3)
	logs := captureHookWarnings(t)
	second, _ := hookPolicyWire(t, a, hist)
	third, _ := hookPolicyWire(t, a, hist)
	if !reflect.DeepEqual(second, third) {
		t.Fatalf("bytes flipped between requests:\n%v\n%v", second, third)
	}
	for _, m := range second {
		if _, ok := m["hp_opaque"]; ok {
			t.Fatalf("carrier survived the generic answer: %v", m)
		}
		if _, ok := m["hp_text"]; ok {
			t.Fatalf("carrier survived the generic answer: %v", m)
		}
	}
	if n := strings.Count(logs.String(), "replay_message_carriers"); n != 1 {
		t.Fatalf("%d replay_message_carriers warnings:\n%s", n, logs.String())
	}
}
