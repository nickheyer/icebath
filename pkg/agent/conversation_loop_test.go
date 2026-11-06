package agent

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const canonUNI = "日本語テキスト🎉 café Ω ≈ 中文字符串"

type shutdownRuntimeError struct{ msg string }

func (e *shutdownRuntimeError) Error() string   { return e.msg }
func (e *shutdownRuntimeError) PyRuntimeError() {}

func TestIsInterpreterShutdownError(t *testing.T) {
	for msg, want := range map[string]bool{
		"cannot schedule new futures after interpreter shutdown": true,
		"cannot schedule new futures after shutdown":             true,
		"Cannot Schedule New Futures After Interpreter Shutdown": true,
		"connection reset by peer":                               false,
	} {
		if got := isInterpreterShutdownError(&shutdownRuntimeError{msg}); got != want {
			t.Fatalf("%q: got %v want %v", msg, got, want)
		}
	}
	if isInterpreterShutdownError(errors.New("cannot schedule new futures after shutdown")) {
		t.Fatal("a non RuntimeError with the shutdown text must not match")
	}
	if !isInterpreterShutdownError(fmt.Errorf("submit: %w", tools.ErrPoolShutdown)) {
		t.Fatal("the pool shutdown error is the RuntimeError the pool raises")
	}
}

func resetCanonCache(t *testing.T) {
	reset := func() {
		canonArgsCache.Lock()
		canonArgsCache.order.Init()
		canonArgsCache.index = map[string]*list.Element{}
		canonArgsCache.bytes = 0
		canonArgsCache.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func canonCacheLen() int {
	canonArgsCache.Lock()
	defer canonArgsCache.Unlock()
	return canonArgsCache.order.Len()
}

func mustDumps(t *testing.T, v any) string {
	t.Helper()
	s, err := pyjson.Dumps(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCanonCacheBoundedByBytes(t *testing.T) {
	resetCanonCache(t)
	big := mustDumps(t, pyjson.ObjectOf("path", "/tmp/big.py", "content", strings.Repeat("y", 200_000)))
	for i := range 300 {
		if _, err := canonicalizeToolCallArguments(big[:len(big)-1] + fmt.Sprintf(`,"n":%d}`, i)); err != nil {
			t.Fatal(err)
		}
	}
	canonArgsCache.Lock()
	bytes := canonArgsCache.bytes
	canonArgsCache.Unlock()
	if bytes > canonArgsCacheMaxBytes || canonCacheLen() < 1 {
		t.Fatalf("cache holds %d bytes over %d entries", bytes, canonCacheLen())
	}
}

func TestCanonMemoSemantics(t *testing.T) {
	resetCanonCache(t)
	raw := mustDumps(t, pyjson.ObjectOf("b", 2, "a", canonUNI, "nested", pyjson.ObjectOf("z", []any{3, 2, 1})))
	canon, err := canonicalizeToolCallArguments(raw)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := canonicalizeToolCallArguments(raw)
	twice, _ := canonicalizeToolCallArguments(canon)
	parsed, _ := pyjson.Loads(raw)
	want, _ := pyjson.Dumps(parsed, pyjson.Separators(",", ":"), pyjson.SortKeys(true))
	if canon != again || twice != canon || canon != want {
		t.Fatalf("canonical form unstable %q %q %q %q", canon, again, twice, want)
	}
	for _, r := range canon {
		if r > 127 {
			t.Fatalf("canonical form must be ASCII %q", canon)
		}
	}

	resetCanonCache(t)
	if _, err := canonicalizeToolCallArguments(`{"broken": tru`); err == nil || canonCacheLen() != 0 {
		t.Fatal("malformed arguments must raise and never be memoized")
	}
	for i := range canonArgsCacheMax + 100 {
		_, _ = canonicalizeToolCallArguments(mustDumps(t, pyjson.ObjectOf("i", i)))
	}
	if canonCacheLen() > canonArgsCacheMax {
		t.Fatalf("cache length %d over the count bound", canonCacheLen())
	}
}

func buildToolHistory(t *testing.T, n int) []any {
	filler := strings.Repeat("x", 2048-200)
	var msgs []any
	for i := range n {
		args := mustDumps(t, pyjson.ObjectOf("path", fmt.Sprintf("/tmp/file_%d.py", i), "content", filler, "u", canonUNI, "n", i, "mode", "write"))
		switch {
		case i%9 == 8:
			args = `{"broken": tru`
		case i%6 == 5:
			args = ""
		}
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
			"id": fmt.Sprintf("call_%d", i), "type": "function", "function": map[string]any{"name": "write_file", "arguments": args},
		}}}, map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i), "name": "write_file", "content": fmt.Sprintf("result %d %s", i, canonUNI)})
	}
	return msgs
}

func shallowRows(history []any) []any {
	out := make([]any, len(history))
	for i, m := range history {
		cp := map[string]any{}
		for k, v := range m.(map[string]any) {
			cp[k] = v
		}
		out[i] = cp
	}
	return out
}

func firstArgs(row any) any {
	return row.(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]
}

func TestCanonicalizeAPIToolCallsParity(t *testing.T) {
	resetCanonCache(t)
	history := buildToolHistory(t, 40)
	before := cloneMessageForSend(history)
	api := shallowRows(history)
	canonicalizeAPIToolCalls(api)
	if !reflect.DeepEqual(history, before) {
		t.Fatal("the send path must never write into the persisted history")
	}
	for i, row := range api {
		if row.(map[string]any)["role"] != "assistant" {
			continue
		}
		raw := firstArgs(history[i]).(string)
		want := provider.RepairToolCallArguments(raw, "write_file")
		if parsed, err := pyjson.Loads(raw); err == nil {
			want, _ = pyjson.Dumps(parsed, pyjson.Separators(",", ":"), pyjson.SortKeys(true))
		}
		if got := firstArgs(row); got != want {
			t.Fatalf("row %d: %q want %q", i, got, want)
		}
	}

	nonString := []any{map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
		"id": "c1", "type": "function", "function": map[string]any{"name": "t", "arguments": map[string]any{"a": 1}},
	}}}}
	canonicalizeAPIToolCalls(nonString)
	if got := firstArgs(nonString[0]); got != "{}" {
		t.Fatalf("non string arguments take the repair path, got %v", got)
	}
}

func TestCanonicalizeLoadsLinear(t *testing.T) {
	resetCanonCache(t)
	calls := 0
	orig := canonArgsLoads
	canonArgsLoads = func(s string) (any, error) { calls++; return orig(s) }
	t.Cleanup(func() { canonArgsLoads = orig })
	history := buildToolHistory(t, 60)
	canonicalizeAPIToolCalls(shallowRows(history))
	first := calls
	calls = 0
	canonicalizeAPIToolCalls(shallowRows(history))
	malformed := 0
	for _, row := range history {
		if row.(map[string]any)["role"] == "assistant" {
			if _, err := pyjson.Loads(firstArgs(row).(string)); err != nil {
				malformed++
			}
		}
	}
	if first != 60 || calls != malformed {
		t.Fatalf("first pass parsed %d, second pass %d, want 60 then only the %d malformed rows", first, calls, malformed)
	}
}

func TestUnrepairableArgsAreNotWrittenBackToHistory(t *testing.T) {
	resetCanonCache(t)
	truncated := "{\"content\": \"# chapter draft\nline one\nline two"
	valid := mustDumps(t, pyjson.ObjectOf("path", "a.txt"))
	history := []any{map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
		map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "write_file", "arguments": truncated}},
		map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "read_file", "arguments": valid}},
	}}}
	for range 3 {
		api := shallowRows(history)
		canonicalizeAPIToolCalls(api)
		sent := api[0].(map[string]any)["tool_calls"].([]any)
		if got := sent[0].(map[string]any)["function"].(map[string]any)["arguments"]; got != provider.RepairToolCallArguments(truncated, "write_file") {
			t.Fatalf("send copy must be repaired, got %q", got)
		}
	}
	calls := history[0].(map[string]any)["tool_calls"].([]any)
	if calls[0].(map[string]any)["function"].(map[string]any)["arguments"] != truncated ||
		calls[1].(map[string]any)["function"].(map[string]any)["arguments"] != valid {
		t.Fatal("history must keep the model's original arguments after repeated sends")
	}
}

func TestSerializedMessagesBytesImagePayloads(t *testing.T) {
	toolMsg := func(size int) []any {
		return []any{map[string]any{"role": "tool", "tool_call_id": "call_abc123", "content": []any{
			map[string]any{"type": "text", "text": "screenshot captured"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("A", size)}},
		}}}
	}
	if d := serializedMessagesBytes(toolMsg(3_000_000)) - serializedMessagesBytes(toolMsg(1_000)); d < 3_000_000-1_000-64 || d > 3_000_000-1_000+64 {
		t.Fatalf("inline data url delta %d", d)
	}
	if serializedMessagesBytes([]any{map[string]any{"role": "user", "content": "éééé"}}) <= serializedMessagesBytes([]any{map[string]any{"role": "user", "content": "aaaa"}}) {
		t.Fatal("utf8 bytes, not code points")
	}
	if serializedMessagesBytes([]any{map[string]any{"role": "tool", "content": make(chan int)}}) <= 0 {
		t.Fatal("non serializable content must still size")
	}
}

type selectingEngine struct {
	stubEngine
	selectFn   func(req, convo []map[string]any, incoming map[string]any) []map[string]any
	onComplete func(messages []map[string]any, usage, kwargs map[string]any)
}

func (e *selectingEngine) SelectContext(req, convo []map[string]any, incoming map[string]any, budget int) []map[string]any {
	return e.selectFn(req, convo, incoming)
}

func (e *selectingEngine) OnTurnComplete(messages []map[string]any, usage, kwargs map[string]any) {
	e.onComplete(messages, usage, kwargs)
}

func TestContextEngineSelection(t *testing.T) {
	request := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "q"}}
	history := []map[string]any{{"role": "user", "content": "hello"}}

	base := &AIAgent{contextCompressor: newStubEngine(1000, 0.75)}
	if out := applyContextEngineSelection(base, request, history, history[0]); &out[0] != &request[0] {
		t.Fatal("an engine inheriting the base hook is short circuited and keeps the request")
	}

	empty := &AIAgent{contextCompressor: &selectingEngine{stubEngine: *newStubEngine(1000, 0.75), selectFn: func(_, _ []map[string]any, _ map[string]any) []map[string]any {
		return []map[string]any{}
	}}}
	if out := applyContextEngineSelection(empty, request, history, history[0]); &out[0] != &request[0] {
		t.Fatal("an empty selection keeps the original request")
	}

	snapshot := cloneMessageForSend(history)
	tamper := &AIAgent{contextCompressor: &selectingEngine{stubEngine: *newStubEngine(1000, 0.75), selectFn: func(_, convo []map[string]any, incoming map[string]any) []map[string]any {
		convo[0]["content"] = "TAMPERED"
		incoming["content"] = "TAMPERED"
		return nil
	}}}
	applyContextEngineSelection(tamper, request, history, history[0])
	if !reflect.DeepEqual(any(history), snapshot) {
		t.Fatal("engine edits must not reach persisted history")
	}

	unusual := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "a"}, {"role": "user", "content": "b"}}
	passthrough := &AIAgent{contextCompressor: &selectingEngine{stubEngine: *newStubEngine(1000, 0.75), selectFn: func(_, _ []map[string]any, _ map[string]any) []map[string]any {
		return unusual
	}}}
	if out := applyContextEngineSelection(passthrough, request, history, history[0]); &out[0] != &unusual[0] {
		t.Fatal("a structurally valid replacement passes through verbatim")
	}

	var gotMsgs []map[string]any
	var gotUsage, gotKw map[string]any
	notify := &AIAgent{contextCompressor: &selectingEngine{stubEngine: *newStubEngine(1000, 0.75), onComplete: func(m []map[string]any, u, kw map[string]any) {
		gotMsgs, gotUsage, gotKw = m, u, kw
	}}}
	notifyContextEngineTurnComplete(notify, history, map[string]any{"total_tokens": 12}, map[string]any{"turn_id": "t1", "api_call_count": 1})
	if !reflect.DeepEqual(gotMsgs, history) || &gotMsgs[0] == &history[0] || gotUsage["total_tokens"] != 12 || gotKw["turn_id"] != "t1" || gotKw["api_call_count"] != 1 {
		t.Fatalf("turn complete snapshot %v %v %v", gotMsgs, gotUsage, gotKw)
	}
}

func classifiedBilling(t *testing.T, message string) *provider.Classified {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": message}})
	c := provider.ClassifyError(provider.ClassifyInput{Err: errors.New(message), Status: 400, Body: string(body), Provider: "anthropic"})
	if c == nil || c.Reason != provider.ReasonBilling {
		t.Fatalf("classified %+v", c)
	}
	return c
}

func TestBillingFailureResultUnverifiedCarrythrough(t *testing.T) {
	ctx := context.Background()
	unverified := classifiedBilling(t, "You're out of extra usage. Add more at claude.ai/settings/usage and keep going.")
	res := billingFailureResult(ctx, unverified, "HTTP 400", nil, 1, "anthropic", "https://api.anthropic.com", "claude-opus-4", nil, UsageWindowRefusal{})
	final := res["final_response"].(string)
	if strings.HasPrefix(final, "Billing or credits exhausted") || !strings.Contains(final, "unverified") ||
		!(strings.Contains(final, "content-filter") || strings.Contains(final, "content filter")) {
		t.Fatalf("unverified final %q", final)
	}
	if res["failed"] != true || res["failure_reason"] != "billing" || res["billing_unverified"] != true || res["billing_block"].(map[string]any)["unverified"] != true {
		t.Fatalf("unverified fields %v", res)
	}
	confirmed := classifiedBilling(t, "Your credit balance is too low to access the Anthropic API.")
	res = billingFailureResult(ctx, confirmed, "HTTP 400", nil, 1, "anthropic", "https://api.anthropic.com", "claude-opus-4", nil, UsageWindowRefusal{})
	if !strings.HasPrefix(res["final_response"].(string), "Billing or credits exhausted") || res["billing_unverified"] != false {
		t.Fatalf("confirmed %v", res)
	}
	if _, ok := res["billing_block"].(map[string]any)["unverified"]; ok {
		t.Fatal("a confirmed block carries no unverified flag")
	}
}

func TestJoinTruncatedPartsAndContinuationPrompt(t *testing.T) {
	seam := strings.Repeat("abcdefghij", 5)
	got := joinTruncatedParts([]TruncatedPart{{Text: "start " + seam, CutStream: true}, {Text: seam + " end"}})
	if got != "start "+seam+" end" {
		t.Fatalf("cut stream overlap must dedupe, got %q", got)
	}
	if joinTruncatedParts([]TruncatedPart{{Text: "one"}, {Text: "two"}}) != "one\ntwo" {
		t.Fatal("non space seam joins with a newline")
	}
	if !strings.Contains(getContinuationPrompt(true, []string{"a", "b", "c", "d"}), "(a, b, c) was too large") ||
		getContinuationPrompt(true, nil) != lengthContinuationNetworkStub || getContinuationPrompt(false, []string{"a"}) != lengthContinuationOutputLimit {
		t.Fatal("continuation prompt selection")
	}
}

func TestCodexSessionDBFor(t *testing.T) {
	if codexSessionDBFor(nil) != nil {
		t.Fatal("no session DB is a nil store")
	}
	db, err := state.OpenHome(filepath.Join(t.TempDir(), "home"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	store := codexSessionDBFor(db)
	if err := store.PatchModelConfig("s1", map[string]any{"codex_thread_id": "th"}); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := store.GetModelConfigValue("s1", "codex_thread_id"); err != nil || !ok || v != "th" {
		t.Fatalf("model config round trip %v %v %v", v, ok, err)
	}
	if err := store.QueueTokenCounts("s1", "codex_app_server", map[string]any{"input_tokens": 5, "output_tokens": 2}); err != nil {
		t.Fatal(err)
	}
}
