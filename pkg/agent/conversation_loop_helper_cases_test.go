package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func billingMessage(prov, baseURL, model string, unverified bool) string {
	return billingOrEntitlementMessage(context.Background(), "model access", prov, baseURL, model, unverified)
}

func TestAnthropicSubscriptionExhaustedGuidance(t *testing.T) {
	caseHome(t)
	msg := billingMessage("anthropic", "https://api.anthropic.com", "claude-opus-4-7", false)
	for _, want := range []string{"claude.ai/settings/usage", "/model", "claude-opus-4-7"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("%q missing from %q", want, msg)
		}
	}
	if !strings.Contains(strings.ToLower(msg), "reset") {
		t.Fatalf("cycle reset option missing from %q", msg)
	}
}

func TestNonAnthropicBillingGuidanceUnaffected(t *testing.T) {
	caseHome(t)
	msg := billingMessage("openrouter", "https://openrouter.ai/api/v1", "anthropic/claude-opus-4.7", false)
	if strings.Contains(msg, "claude.ai/settings/usage") || !strings.Contains(msg, "openrouter.ai/settings/credits") {
		t.Fatalf("generic guidance %q", msg)
	}
}

func TestUnverifiedGuidanceNamesTheContentFilterAlternative(t *testing.T) {
	caseHome(t)
	if msg := strings.ToLower(billingMessage("anthropic", "https://api.anthropic.com", "claude-opus-5", true)); !strings.Contains(msg, "content filter") {
		t.Fatalf("unverified guidance %q", msg)
	}
}

func TestConfirmedGuidanceStaysAssertiveWithoutTheCaveat(t *testing.T) {
	caseHome(t)
	lowered := strings.ToLower(billingMessage("anthropic", "https://api.anthropic.com", "claude-opus-5", false))
	if strings.Contains(lowered, "content filter") || strings.Contains(lowered, "icebath auth reset") {
		t.Fatalf("confirmed guidance %q", lowered)
	}
}

func TestContentFilterCaveatIsAnthropicOnly(t *testing.T) {
	caseHome(t)
	msg := strings.ToLower(billingMessage("openrouter", "https://openrouter.ai/api/v1", "anthropic/claude-opus-4.7", true))
	if strings.Contains(msg, "content filter") || strings.Contains(msg, "icebath auth reset") {
		t.Fatalf("generic unverified guidance %q", msg)
	}
}

func TestInterpreterShutdownDetection(t *testing.T) {
	caseHome(t)
	check := func(err error, want bool) func(*testing.T) {
		return func(t *testing.T) {
			if got := isInterpreterShutdownError(err); got != want {
				t.Fatalf("got %v want %v", got, want)
			}
		}
	}
	t.Run("test_matches_full_interpreter_shutdown_message", check(&shutdownRuntimeError{"cannot schedule new futures after interpreter shutdown"}, true))
	t.Run("test_matches_short_shutdown_variant", check(&shutdownRuntimeError{"cannot schedule new futures after shutdown"}, true))
	t.Run("test_case_insensitive_match", check(&shutdownRuntimeError{"Cannot Schedule New Futures After Interpreter Shutdown"}, true))
	t.Run("test_does_not_match_unrelated_runtime_error", check(&shutdownRuntimeError{"connection reset by peer"}, false))
	t.Run("test_does_not_match_non_runtime_error", check(errors.New("cannot schedule new futures"), false))
	if !isInterpreterShutdownError(fmt.Errorf("submit: %w", tools.ErrPoolShutdown)) {
		t.Fatal("the pool shutdown error is the RuntimeError the pool raises")
	}
}

func TestNativeClientDoesNotConsumeThePreparedRequest(t *testing.T) {
	caseHome(t)
	if moaClientConsumesPreparedRequest(&fakeOpenAI{}) {
		t.Fatal("a native client has no prepare")
	}
}

func TestRealMoaFacadeConsumesThePreparedRequest(t *testing.T) {
	caseHome(t)
	if !moaClientConsumesPreparedRequest(&MoAClient{}) {
		t.Fatal("the MoA facade exposes prepare")
	}
}

func TestClientWithoutAChatAttributeIsNotAFacade(t *testing.T) {
	caseHome(t)
	if moaClientConsumesPreparedRequest(struct{}{}) || moaClientConsumesPreparedRequest(nil) {
		t.Fatal("an object without chat completions is no facade")
	}
}

func TestPartialStubContinuationSaysToolsRemainAvailable(t *testing.T) {
	caseHome(t)
	for _, dropped := range [][]string{nil, {"write_file"}} {
		prompt := strings.ToLower(getContinuationPrompt(true, dropped))
		if !strings.Contains(prompt, "tools") || !strings.Contains(prompt, "available") || strings.Contains(prompt, "finish the answer directly") {
			t.Fatalf("dropped %v prompt %q", dropped, prompt)
		}
	}
}

func TestNewAndLegacyStubNudgesAreRecognizedAsSynthetic(t *testing.T) {
	caseHome(t)
	for _, dropped := range [][]string{nil, {"write_file"}} {
		for _, text := range []string{getContinuationPrompt(true, dropped), legacyLengthContinuationNetworkStub} {
			if !state.IsSyntheticCompressionUserTurn(map[string]any{"role": "user", "content": text}) {
				t.Fatalf("not recognized as synthetic %q", text)
			}
		}
	}
}

func TestTruncatedPartJoining(t *testing.T) {
	caseHome(t)
	plain := func(texts ...string) []TruncatedPart {
		out := make([]TruncatedPart, len(texts))
		for i, s := range texts {
			out[i] = TruncatedPart{Text: s}
		}
		return out
	}
	t.Run("test_glued_parts_get_a_newline", func(t *testing.T) {
		if got := joinTruncatedParts(plain("Edited index.html", "Review the 5 changes")); got != "Edited index.html\nReview the 5 changes" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_existing_whitespace_is_not_doubled", func(t *testing.T) {
		if got := joinTruncatedParts(plain("line one\n", "line two")); got != "line one\nline two" {
			t.Fatalf("got %q", got)
		}
		if got := joinTruncatedParts(plain("word", " next")); got != "word next" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_degenerate_inputs", func(t *testing.T) {
		if joinTruncatedParts(nil) != "" || joinTruncatedParts(plain("only")) != "only" || joinTruncatedParts(plain("a", "", "b")) != "a\nb" {
			t.Fatal("degenerate joins")
		}
	})
	seam := strings.Repeat("abcdefghij", 5)
	if got := joinTruncatedParts([]TruncatedPart{{Text: "start " + seam, CutStream: true}, {Text: seam + " end"}}); got != "start "+seam+" end" {
		t.Fatalf("cut stream overlap must dedupe, got %q", got)
	}
	if !strings.Contains(getContinuationPrompt(true, []string{"a", "b", "c", "d"}), "(a, b, c) was too large") ||
		getContinuationPrompt(true, nil) != lengthContinuationNetworkStub || getContinuationPrompt(false, []string{"a"}) != lengthContinuationOutputLimit {
		t.Fatal("continuation prompt selection")
	}
}

// Pre memo inline loop of the send path, the byte exact reference the parity cases compare against
func canonicalizePassOld(apiMessages []any) {
	for _, row := range apiMessages {
		am := row.(map[string]any)
		tcs, _ := am["tool_calls"].([]any)
		if len(tcs) == 0 {
			continue
		}
		newTCs := make([]any, 0, len(tcs))
		for _, raw := range tcs {
			tc := raw.(map[string]any)
			fn := tc["function"].(map[string]any)
			text, isStr := fn["arguments"].(string)
			parsed, err := pyjson.Loads(text)
			if !isStr || err != nil {
				if !isStr {
					text = ""
				}
				fn["arguments"] = provider.RepairToolCallArguments(text, fmt.Sprint(fn["name"]))
				newTCs = append(newTCs, tc)
				continue
			}
			canonical, _ := pyjson.Dumps(parsed, pyjson.Separators(",", ":"), pyjson.SortKeys(true))
			newFn := map[string]any{}
			for k, v := range fn {
				newFn[k] = v
			}
			newFn["arguments"] = canonical
			newTC := map[string]any{}
			for k, v := range tc {
				newTC[k] = v
			}
			newTC["function"] = newFn
			newTCs = append(newTCs, newTC)
		}
		am["tool_calls"] = newTCs
	}
}

func deepRows(history []any) []any { return cloneMessageForSend(history).([]any) }

func allValidHistory(t *testing.T, n int, withName bool) []any {
	history := buildToolHistory(t, n)
	for _, row := range history {
		m := row.(map[string]any)
		tcs, _ := m["tool_calls"].([]any)
		if len(tcs) == 0 {
			continue
		}
		tc := tcs[0].(map[string]any)
		fn := tc["function"].(map[string]any)
		if withName {
			fn["arguments"] = mustDumps(t, pyjson.ObjectOf("name", fn["name"], "id", tc["id"], "u", canonUNI))
		} else {
			fn["arguments"] = mustDumps(t, pyjson.ObjectOf("id", tc["id"], "u", canonUNI))
		}
	}
	return history
}

func TestCacheBoundedByBytes(t *testing.T) {
	caseHome(t)
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

func TestByteParity(t *testing.T) {
	caseHome(t)
	t.Run("test_growing_session_every_iteration", func(t *testing.T) {
		resetCanonCache(t)
		n := 60
		history := buildToolHistory(t, n)
		for k := 1; k <= n; k++ {
			oldMsgs, newMsgs := deepRows(history[:2*k]), deepRows(history[:2*k])
			canonicalizePassOld(oldMsgs)
			canonicalizeAPIToolCalls(newMsgs)
			if !reflect.DeepEqual(oldMsgs, newMsgs) {
				t.Fatalf("diverged at iteration %d", k)
			}
		}
	})
	t.Run("test_history_not_mutated", func(t *testing.T) {
		resetCanonCache(t)
		history := allValidHistory(t, 20, false)
		before := deepRows(history)
		canonicalizeAPIToolCalls(shallowRows(history))
		if !reflect.DeepEqual(history, before) {
			t.Fatal("the send path wrote into the persisted history")
		}
	})
	t.Run("test_non_string_arguments_parity", func(t *testing.T) {
		resetCanonCache(t)
		msgs := []any{map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
			"id": "c1", "type": "function", "function": map[string]any{"name": "t", "arguments": map[string]any{"a": 1}},
		}}}}
		oldMsgs, newMsgs := deepRows(msgs), deepRows(msgs)
		canonicalizePassOld(oldMsgs)
		canonicalizeAPIToolCalls(newMsgs)
		if !reflect.DeepEqual(oldMsgs, newMsgs) || firstArgs(newMsgs[0]) != "{}" {
			t.Fatalf("non string arguments diverged %v %v", oldMsgs, newMsgs)
		}
	})
}

func TestMemoSemantics(t *testing.T) {
	caseHome(t)
	t.Run("test_deterministic_and_idempotent", func(t *testing.T) {
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
	})
	t.Run("test_malformed_never_memoized", func(t *testing.T) {
		resetCanonCache(t)
		if _, err := canonicalizeToolCallArguments(`{"broken": tru`); err == nil || canonCacheLen() != 0 {
			t.Fatal("malformed arguments must raise and never be memoized")
		}
	})
	t.Run("test_cache_bounded", func(t *testing.T) {
		resetCanonCache(t)
		for i := range canonArgsCacheMax + 100 {
			_, _ = canonicalizeToolCallArguments(mustDumps(t, pyjson.ObjectOf("i", i)))
		}
		if canonCacheLen() > canonArgsCacheMax {
			t.Fatalf("cache length %d over the count bound", canonCacheLen())
		}
	})
}

func TestComplexityProof(t *testing.T) {
	caseHome(t)
	t.Run("test_json_loads_linear_not_quadratic", func(t *testing.T) {
		resetCanonCache(t)
		n := 40
		history := allValidHistory(t, n, true)
		oldCount := 0
		for k := 1; k <= n; k++ {
			for _, row := range history[:2*k] {
				if tcs, _ := row.(map[string]any)["tool_calls"].([]any); len(tcs) > 0 {
					oldCount++
				}
			}
			canonicalizePassOld(deepRows(history[:2*k]))
		}
		if oldCount != n*(n+1)/2 {
			t.Fatalf("reference pass parsed %d", oldCount)
		}
		newCount := 0
		orig := canonArgsLoads
		canonArgsLoads = func(s string) (any, error) { newCount++; return orig(s) }
		t.Cleanup(func() { canonArgsLoads = orig })
		for k := 1; k <= n; k++ {
			canonicalizeAPIToolCalls(deepRows(history[:2*k]))
		}
		if newCount != n || float64(oldCount) != float64(n+1)/2*float64(newCount) {
			t.Fatalf("memo pass parsed %d of %d unique strings", newCount, n)
		}
	})
}

func truncatedWriteHistory() ([]any, string) {
	truncated := "{\"content\": \"# chapter draft\nline one\nline two"
	return []any{map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
		map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "write_file", "arguments": truncated}},
	}}}, truncated
}

func TestUnrepairableArgsAreNotWrittenBackToHistory(t *testing.T) {
	caseHome(t)
	t.Run("test_history_keeps_the_original_arguments", func(t *testing.T) {
		resetCanonCache(t)
		history, truncated := truncatedWriteHistory()
		before := deepRows(history)
		canonicalizeAPIToolCalls(shallowRows(history))
		if !reflect.DeepEqual(history, before) || firstArgs(history[0]) != truncated {
			t.Fatal("the send path canonicalizer rewrote the persisted history")
		}
	})
	t.Run("test_send_copy_is_still_repaired", func(t *testing.T) {
		resetCanonCache(t)
		history, _ := truncatedWriteHistory()
		api := shallowRows(history)
		canonicalizeAPIToolCalls(api)
		sent := firstArgs(api[0])
		if sent != "{}" {
			t.Fatalf("send copy %q", sent)
		}
		if _, err := pyjson.Loads(sent.(string)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("test_valid_calls_alongside_a_broken_one_are_untouched", func(t *testing.T) {
		resetCanonCache(t)
		good := mustDumps(t, pyjson.ObjectOf("path", "a.txt", "u", canonUNI))
		history := []any{map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": good}},
			map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "write_file", "arguments": `{"content": "cut`}},
		}}}
		before := deepRows(history)
		api := shallowRows(history)
		canonicalizeAPIToolCalls(api)
		if !reflect.DeepEqual(history, before) {
			t.Fatal("a broken call disturbed the history")
		}
		sent := api[0].(map[string]any)["tool_calls"].([]any)
		got, err := pyjson.Loads(sent[0].(map[string]any)["function"].(map[string]any)["arguments"].(string))
		want, _ := pyjson.Loads(good)
		if err != nil || !reflect.DeepEqual(got, want) || sent[1].(map[string]any)["function"].(map[string]any)["arguments"] != "{}" {
			t.Fatalf("sent %v", sent)
		}
	})
	t.Run("test_repeated_sends_do_not_accumulate_damage", func(t *testing.T) {
		resetCanonCache(t)
		history, truncated := truncatedWriteHistory()
		for range 5 {
			api := shallowRows(history)
			canonicalizeAPIToolCalls(api)
			if firstArgs(api[0]) != "{}" {
				t.Fatalf("send copy %v", firstArgs(api[0]))
			}
		}
		if firstArgs(history[0]) != truncated {
			t.Fatal("history lost the model's original arguments")
		}
	})
}

// Binds the case to a private icebath home and returns it
func caseHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	return h
}
