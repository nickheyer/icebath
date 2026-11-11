package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Pins the model window the compressor resolves for the test
func patchContextLength(t *testing.T, n int) {
	t.Helper()
	prev := compressorModelContextLength
	compressorModelContextLength = func(context.Context, provider.ContextLengthRequest) int { return n }
	t.Cleanup(func() { compressorModelContextLength = prev })
}

// Replaces the summary call and counts its invocations
func patchCallLLM(t *testing.T, fn func(kwargs map[string]any) (any, error)) *int {
	t.Helper()
	calls := 0
	prev := compressorCallLLM
	compressorCallLLM = func(_ context.Context, kwargs map[string]any) (any, error) {
		calls++
		return fn(kwargs)
	}
	t.Cleanup(func() { compressorCallLLM = prev })
	return &calls
}

// Failing summary call raising the given error text
func failingCallLLM(t *testing.T, text string) *int {
	return patchCallLLM(t, func(map[string]any) (any, error) { return nil, errors.New(text) })
}

// Compressor over a pinned window with the option edits applied, window resolved before return
func newCompressorWith(t *testing.T, window int, edit func(*ContextCompressorOptions)) *ContextCompressor {
	t.Helper()
	patchContextLength(t, window)
	o := DefaultContextCompressorOptions("test/model")
	o.QuietMode = true
	if edit != nil {
		edit(&o)
	}
	c := NewContextCompressor(context.Background(), o)
	_ = c.ContextLength()
	return c
}

// The shared fixture with threshold 0.85 and two protected rows at each end
func fixtureCompressor(t *testing.T) *ContextCompressor {
	return newCompressorWith(t, 100000, func(o *ContextCompressorOptions) {
		o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN = 0.85, 2, 2
	})
}

// Alternating user and assistant rows with the given content format
func alternating(n int, format string) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		out = append(out, map[string]any{"role": role, "content": fmt.Sprintf(format, i)})
	}
	return out
}

func iptr(n int) *int { return &n }

func TestShouldCompress(t *testing.T) {
	c := fixtureCompressor(t)
	c.LastPromptTokens = 50000
	if c.shouldCompress(nil) {
		t.Fatal("below threshold")
	}
	c.LastPromptTokens = 90000
	if !c.shouldCompress(nil) {
		t.Fatal("above threshold")
	}
	if !c.shouldCompress(iptr(90000)) || c.shouldCompress(iptr(50000)) {
		t.Fatal("explicit tokens")
	}
}

func TestUpdateFromResponse(t *testing.T) {
	c := fixtureCompressor(t)
	c.awaitingRealUsageAfterCompression = true
	c.UpdateFromResponse(map[string]any{"prompt_tokens": 5000, "completion_tokens": 1000, "total_tokens": 6000})
	if c.LastPromptTokens != 5000 || c.LastCompletionTokens != 1000 || c.lastRealPromptTokens != 5000 || c.awaitingRealUsageAfterCompression {
		t.Fatalf("fields %d %d %d %v", c.LastPromptTokens, c.LastCompletionTokens, c.lastRealPromptTokens, c.awaitingRealUsageAfterCompression)
	}
	c = fixtureCompressor(t)
	c.UpdateFromResponse(map[string]any{})
	if c.LastPromptTokens != 0 {
		t.Fatal("missing fields default zero")
	}
}

func TestPreflightDeferral(t *testing.T) {
	t.Run("rough estimate defers until provider prices it", func(t *testing.T) {
		c := fixtureCompressor(t)
		c.SetContextLength(200_000)
		c.SetThresholdTokens(85_000)
		c.lastRealPromptTokens = 50_000
		if !c.shouldDeferPreflightToRealUsage(100_000) || c.shouldDeferPreflightToRealUsage(80_000) {
			t.Fatal("deferral")
		}
	})
	t.Run("never defers when real usage cannot arrive or is already over", func(t *testing.T) {
		c := fixtureCompressor(t)
		c.SetContextLength(100_000)
		c.SetThresholdTokens(85_000)
		c.lastRealPromptTokens = 90_000
		if c.shouldDeferPreflightToRealUsage(95_000) {
			t.Fatal("real usage already over")
		}
		c.lastRealPromptTokens = 50_000
		for _, rough := range []int{c.ContextLength(), 150_000, 10_000_000} {
			if !c.shouldDeferPreflightToRealUsage(rough) {
				t.Fatalf("rough %d", rough)
			}
		}
		c.noteUsageLessResponse()
		if c.shouldDeferPreflightToRealUsage(95_000) {
			t.Fatal("usage less provider")
		}
		c.UpdateFromResponse(map[string]any{"prompt_tokens": 50_000})
		if !c.shouldDeferPreflightToRealUsage(95_000) {
			t.Fatal("usage returns")
		}
	})
	t.Run("defers immediately after compaction with stale real prompt", func(t *testing.T) {
		c := fixtureCompressor(t)
		c.SetThresholdTokens(85_000)
		c.lastRealPromptTokens = 120_000
		c.awaitingRealUsageAfterCompression = true
		if !c.shouldDeferPreflightToRealUsage(95_000) {
			t.Fatal("awaiting flag")
		}
	})
	t.Run("native checkpoint defers until provider usage reanchors", func(t *testing.T) {
		c := fixtureCompressor(t)
		c.SetThresholdTokens(85_000)
		c.lastRealPromptTokens = 60_000
		c.lastCompressionRoughTokens = 40_000
		c.NoteNativeCompactionCheckpoint()
		if !c.awaitingRealUsageAfterCompression || c.lastCompressionRoughTokens != 0 || !c.shouldDeferPreflightToRealUsage(1_300_000) {
			t.Fatal("checkpoint")
		}
		c.UpdateFromResponse(map[string]any{"prompt_tokens": 65_000})
		if c.awaitingRealUsageAfterCompression {
			t.Fatal("reanchored")
		}
	})
}

func TestCompress(t *testing.T) {
	t.Run("fallback summary does not triplicate latest user ask", func(t *testing.T) {
		c := newCompressorWith(t, 100000, nil)
		ask := "PLEASE_COMPUTE_THE_ARITHMETIC_CHAIN_XYZ"
		summary := c.buildStaticFallbackSummary([]map[string]any{{"role": "user", "content": ask}, {"role": "assistant", "content": "working on it"}}, "provider down")
		if n := strings.Count(summary, "User asked: '"+ask+"'"); n > 1 {
			t.Fatalf("active task line %dx\n%s", n, summary)
		}
	})
	t.Run("threshold floor capped at 85 percent of window", func(t *testing.T) {
		if computeThresholdTokens(65_536, 0.50, nil) != 55_705 || computeThresholdTokens(70_000, 0.50, nil) != 59_500 ||
			computeThresholdTokens(100_000, 0.50, nil) != 64_000 || computeThresholdTokens(372_000, 0.90, nil) != 334_800 {
			t.Fatal("threshold floor")
		}
	})
	t.Run("max tokens coercion treats non int as no reservation", func(t *testing.T) {
		for _, v := range []any{nil, 0, -5, "nope"} {
			if coerceMaxTokens(v) != nil {
				t.Fatalf("coerce %v", v)
			}
		}
		if p := coerceMaxTokens(65536); p == nil || *p != 65536 {
			t.Fatal("coerce 65536")
		}
		c := newCompressorWith(t, 200000, func(o *ContextCompressorOptions) { o.Model, o.MaxTokens = "m", struct{ mock bool }{true} })
		if c.maxTokens != nil || c.thresholdTokensValue() <= 0 {
			t.Fatalf("mocked max tokens %v %d", c.maxTokens, c.thresholdTokensValue())
		}
	})
	t.Run("protect first n decays after first compression", func(t *testing.T) {
		c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model, o.ProtectFirstN = "test", 3 })
		msgs := append([]map[string]any{{"role": "system", "content": "sys"}}, alternating(10, "m%d")...)
		if c.ContextEngineBase.CompressionCount != 0 || c.effectiveProtectFirstN(msgs) != 3 || c.protectHeadSize(msgs) != 4 {
			t.Fatal("first compaction head")
		}
		c.ContextEngineBase.CompressionCount = 1
		if c.effectiveProtectFirstN(msgs) != 0 || c.protectHeadSize(msgs) != 1 {
			t.Fatal("decayed head")
		}
	})
}

func TestTailBudgetCodexReplayFields(t *testing.T) {
	big := strings.Repeat("x", 5_000)
	cases := map[string]any{
		"reasoning":             big,
		"reasoning_content":     big,
		"reasoning_details":     []any{map[string]any{"text": big}},
		"codex_reasoning_items": []any{map[string]any{"type": "reasoning", "encrypted_content": big}},
		"codex_message_items": []any{map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": big}}}},
	}
	for field, value := range cases {
		c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.ProtectFirstN, o.ProtectLastN = 1, 1 })
		messages := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "initial ask"},
			{"role": "assistant", "content": "first answer"}, {"role": "user", "content": "older follow-up"},
			{"role": "assistant", "content": "ok", field: value}}
		messages = append(messages, alternating(14, "tail visible message %d")...)
		if got := c.findTailCutByTokens(messages, 1, iptr(150)); got != 5 {
			t.Fatalf("%s cut %d", field, got)
		}
	}
}

func TestNonStringContentSnapshots(t *testing.T) {
	t.Run("task snapshot skips synthetic user scaffolding", func(t *testing.T) {
		messages := []map[string]any{{"role": "user", "content": "fix the login bug on prod"}, {"role": "assistant", "content": "on it"},
			{"role": "user", "content": "[Your active task list was preserved across context compression]\n- item", "_todo_snapshot_synthetic": true}}
		snapshot := latestUserTaskSnapshot(messages)
		if !strings.Contains(snapshot, "fix the login bug on prod") || strings.Contains(snapshot, "task list was preserved") {
			t.Fatalf("snapshot %q", snapshot)
		}
		if latestUserTaskSnapshot([]map[string]any{{"role": "user", "content": "[System: Your previous response was truncated ...]"}}) != "" {
			t.Fatal("synthetic only")
		}
	})
	t.Run("grounding preserves following sections across regrounding", func(t *testing.T) {
		summary := state.HistoricalTaskHeading + "\nUser asked: stale example\n\n## Historical Remaining Work\n- keep me\n\n## Goal\nfinish"
		turns := []map[string]any{{"role": "user", "content": "real ask"}}
		first := groundHistoricalTaskSnapshot(summary, turns)
		if !regexp.MustCompile(`(?m)^## Historical Remaining Work$`).MatchString(first) || !strings.Contains(first, "- keep me") || !strings.Contains(first, "## Goal") {
			t.Fatalf("first %q", first)
		}
		second := groundHistoricalTaskSnapshot(first, turns)
		if !strings.Contains(second, "- keep me") || !strings.Contains(second, "## Goal") || strings.Count(second, state.HistoricalTaskHeading) != 1 {
			t.Fatalf("second %q", second)
		}
	})
}

// Ten or n alternating msg rows
func msgRows(n int) []map[string]any { return alternating(n, "msg %d") }

// Status carrying provider error rendering as the message alone
func providerStatusError(message string, status int) error {
	return &provider.AuxSDKError{StatusCode: status, Message: message}
}

// Compressor with two protected rows at each end and abort on summary failure as given
func abortCompressor(t *testing.T, abort bool) *ContextCompressor {
	return newCompressorWith(t, 100000, func(o *ContextCompressorOptions) {
		o.Model, o.ProtectFirstN, o.ProtectLastN, o.AbortOnSummaryFailure = "test", 2, 2, abort
	})
}

func sameRows(a, b []map[string]any) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

// Eight row conversation behind a system prompt
func eightRows() []map[string]any {
	return []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "msg 1"}, {"role": "assistant", "content": "msg 2"},
		{"role": "user", "content": "msg 3"}, {"role": "assistant", "content": "msg 4"}, {"role": "user", "content": "msg 5"},
		{"role": "assistant", "content": "msg 6"}, {"role": "user", "content": "msg 7"}}
}

func TestSummaryPrefixNormalization(t *testing.T) {
	if withSummaryPrefix("[CONTEXT SUMMARY]: did work") != state.SummaryPrefix+"\ndid work" {
		t.Fatal("legacy prefix")
	}
	if withSummaryPrefix(state.SummaryPrefix+"\ndid work") != state.SummaryPrefix+"\ndid work" {
		t.Fatal("duplicated prefix")
	}
}

// Compressor over a 200K window at a 100K threshold protecting twenty tail rows
func budgetCompressor(t *testing.T) *ContextCompressor {
	return newCompressorWith(t, 200_000, func(o *ContextCompressorOptions) {
		o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN = 0.50, 2, 20
	})
}

func TestTokenBudgetTailProtection(t *testing.T) {
	t.Run("tiny budget preserves bounded recent turns", func(t *testing.T) {
		c := budgetCompressor(t)
		c.SetTailTokenBudget(10)
		c.ProtectLastN = 20
		contents := []string{"old start", "old ack", "middle work", "middle ack", "middle ask 2", "middle answer 2", "middle ask 3",
			"middle answer 3", "recent ask 1", "recent answer 1", "recent ask 2", "recent answer 2", "latest ask"}
		messages := []map[string]any{{"role": "system", "content": "sys"}}
		for i, text := range contents {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			messages = append(messages, map[string]any{"role": role, "content": text})
		}
		cut := c.findTailCutByTokens(messages, 1, nil)
		if len(messages)-cut < 8 || messages[cut]["content"] != "middle answer 2" || messages[len(messages)-1]["content"] != "latest ask" {
			t.Fatalf("cut %d %v", cut, messages[cut]["content"])
		}
	})
	t.Run("prune short conv protects entire tail", func(t *testing.T) {
		c := budgetCompressor(t)
		big := strings.Repeat("x", 5000)
		messages := []map[string]any{{"role": "tool", "content": big, "tool_call_id": "c0"}, {"role": "assistant", "content": "ack"},
			{"role": "user", "content": "recent"}, {"role": "assistant", "content": "reply"}}
		result, pruned := c.pruneOldToolResults(messages, 4, iptr(1_000_000), pruneMinChars)
		if pruned != 0 || result[0]["content"] != big {
			t.Fatalf("pruned %d", pruned)
		}
	})
	t.Run("multimodal message accumulates text chars not block count", func(t *testing.T) {
		c := budgetCompressor(t)
		multimodal := []any{map[string]any{"type": "text", "text": strings.Repeat("x", 500)},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/img.jpg"}}}
		messages := []map[string]any{{"role": "user", "content": "head1"}, {"role": "user", "content": multimodal},
			{"role": "assistant", "content": "tail1"}, {"role": "user", "content": "tail2"}, {"role": "assistant", "content": "tail3"},
			{"role": "user", "content": "tail4"}}
		c.SetTailTokenBudget(80)
		if cut := c.findTailCutByTokens(messages, 0, nil); len(messages)-cut < 4 {
			t.Fatalf("tail %d cut %d", len(messages)-cut, cut)
		}
	})
}

func TestUpdateModelBudgets(t *testing.T) {
	t.Run("tail budget recalculated", func(t *testing.T) {
		comp := newCompressorWith(t, 200_000, func(o *ContextCompressorOptions) { o.Model, o.ThresholdPercent, o.TailMode = "model-a", 0.50, "legacy" })
		oldTail, oldMax := comp.TailTokenBudget(), comp.maxSummaryTokensValue()
		comp.UpdateModel("model-b", 32_000, "", "", "", "")
		if comp.TailTokenBudget() >= oldTail || comp.maxSummaryTokensValue() == oldMax {
			t.Fatalf("budgets %d %d %d %d", oldTail, comp.TailTokenBudget(), oldMax, comp.maxSummaryTokensValue())
		}
	})
	t.Run("update model preserves lean mode", func(t *testing.T) {
		comp := newCompressorWith(t, 1_000_000, func(o *ContextCompressorOptions) { o.Model, o.ThresholdPercent = "model-a", 0.85 })
		fresh := newCompressorWith(t, 400_000, func(o *ContextCompressorOptions) { o.Model, o.ThresholdPercent = "model-b", 0.85 })
		comp.UpdateModel("model-b", 400_000, "", "", "", "")
		if comp.TailTokenBudget() != fresh.TailTokenBudget() || comp.TailTokenBudget() >= int(float64(comp.thresholdTokensValue())*comp.summaryTargetRatio) {
			t.Fatalf("lean %d fresh %d", comp.TailTokenBudget(), fresh.TailTokenBudget())
		}
	})
	t.Run("explicit legacy still honored", func(t *testing.T) {
		comp := newCompressorWith(t, 1_000_000, func(o *ContextCompressorOptions) { o.Model, o.ThresholdPercent, o.TailMode = "model-a", 0.85, "legacy" })
		if comp.tailMode != "legacy" || comp.TailTokenBudget() != int(float64(comp.thresholdTokensValue())*comp.summaryTargetRatio) {
			t.Fatalf("legacy %d", comp.TailTokenBudget())
		}
	})
}

func TestUpdateModelResetsCalibration(t *testing.T) {
	newComp := func(t *testing.T) *ContextCompressor {
		return newCompressorWith(t, 200_000, func(o *ContextCompressorOptions) { o.Model, o.ThresholdPercent = "big-model", 0.50 })
	}
	t.Run("real usage state cleared", func(t *testing.T) {
		comp := newComp(t)
		comp.LastPromptTokens, comp.lastRealPromptTokens, comp.lastCompressionRoughTokens = 120_000, 120_000, 130_000
		comp.awaitingRealUsageAfterCompression, comp.ineffectiveCompressionCount = true, 2
		comp.UpdateModel("small-model", 65_536, "", "", "", "")
		if comp.LastPromptTokens != 0 || comp.lastRealPromptTokens != 0 || comp.lastCompressionRoughTokens != 0 ||
			comp.awaitingRealUsageAfterCompression || comp.ineffectiveCompressionCount != 0 {
			t.Fatal("calibration survived the switch")
		}
	})
	t.Run("switch waits for new provider evidence", func(t *testing.T) {
		comp := newComp(t)
		comp.lastRealPromptTokens = 50_000
		comp.SetThresholdTokens(85_000)
		if !comp.shouldDeferPreflightToRealUsage(93_000) {
			t.Fatal("pre switch deferral")
		}
		comp.UpdateModel("small-model", 65_536, "", "", "", "")
		if comp.lastRealPromptTokens != 0 || !comp.shouldDeferPreflightToRealUsage(comp.ContextLength()+5_000) {
			t.Fatal("post switch deferral")
		}
		comp.UpdateFromResponse(map[string]any{"prompt_tokens": comp.thresholdTokensValue() + 1})
		if comp.shouldDeferPreflightToRealUsage(comp.ContextLength() + 5_000) {
			t.Fatal("evidence arrived")
		}
	})
}

func TestThresholdTokensCap(t *testing.T) {
	capped := func(t *testing.T, window int, pct float64, cap any) *ContextCompressor {
		return newCompressorWith(t, window, func(o *ContextCompressorOptions) {
			o.Model, o.ThresholdPercent, o.ThresholdTokensCap = "model-a", pct, cap
		})
	}
	if comp := capped(t, 1_000_000, 0.50, nil); comp.thresholdTokensValue() != 500_000 || comp.thresholdTokensCap != nil {
		t.Fatal("no cap uses ratio only")
	}
	compression, _ := config.Defaults()["compression"].(map[string]any)
	defaultPct, _ := compression["threshold"].(float64)
	for _, window := range []int{128_000, 1_000_000} {
		ratioOnly := capped(t, window, defaultPct, nil)
		comp := capped(t, window, defaultPct, compression["threshold_tokens"])
		if comp.thresholdTokensCap != nil || comp.thresholdTokensValue() != ratioOnly.thresholdTokensValue() {
			t.Fatalf("default config at %d", window)
		}
	}
	for _, bad := range []any{0, -100, "not-a-number"} {
		if comp := capped(t, 1_000_000, 0.50, bad); comp.thresholdTokensCap != nil || comp.thresholdTokensValue() != 500_000 {
			t.Fatalf("invalid cap %v", bad)
		}
	}
	comp := capped(t, 1_000_000, 0.50, 200_000)
	if comp.shouldCompress(iptr(150_000)) || !comp.shouldCompress(iptr(200_000)) || !comp.shouldCompress(iptr(250_000)) {
		t.Fatal("cap fires first")
	}
	comp = capped(t, 1_000_000, 0.50, 200_000)
	if comp.thresholdTokensValue() != 200_000 {
		t.Fatal("cap applied")
	}
	comp.UpdateModel("model-b", 2_000_000, "", "", "", "")
	if comp.thresholdTokensValue() != 200_000 {
		t.Fatal("cap survives model switch")
	}
}

func TestOldLargeArgumentsSurvivePruneByteExact(t *testing.T) {
	c := NewContextCompressor(context.Background(), func() ContextCompressorOptions {
		o := DefaultContextCompressorOptions("test/model")
		o.ConfigContextLength, o.ProtectFirstN, o.ProtectLastN, o.QuietMode, o.TailMode = iptr(200_000), 1, 2, true, "legacy"
		return o
	}())
	messages := []map[string]any{{"role": "user", "content": "go"}}
	originals := map[string]string{}
	for k := 0; k < 6; k++ {
		id := fmt.Sprintf("c%d", k)
		args := `{"path": "a.py", "content": "` + strings.Repeat("X", 3000) + `"}`
		originals[id] = args
		messages = append(messages, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "type": "function",
			"function": map[string]any{"name": "write_file", "arguments": args}}}}, map[string]any{"role": "tool", "tool_call_id": id, "content": "ok"},
			map[string]any{"role": "user", "content": fmt.Sprintf("u%d", k)})
	}
	messages = append(messages, map[string]any{"role": "assistant", "content": "done"})
	result, _ := c.pruneOldToolResults(messages, 2, nil, pruneMinChars)
	seen := map[string]string{}
	for _, m := range result {
		if calls, ok := m["tool_calls"].([]any); ok && len(calls) > 0 {
			tc := calls[0].(map[string]any)
			seen[tc["id"].(string)] = tc["function"].(map[string]any)["arguments"].(string)
		}
	}
	if fmt.Sprint(seen) != fmt.Sprint(originals) {
		t.Fatal("arguments rewritten")
	}
	pem := "-----BEGIN RSA PRIVATE KEY-----\n%s\n-----END RSA PRIVATE KEY-----"
	leak := dumps(t, map[string]any{"a": fmt.Sprintf(pem, strings.Repeat("K", 3200)), "b": fmt.Sprintf(pem, strings.Repeat("L", 6500))})
	rendered := renderToolCallForSummary(map[string]any{"function": map[string]any{"name": "write_file", "arguments": leak}})
	if strings.Contains(rendered, "KKKK") || strings.Contains(rendered, "LLLL") || strings.Contains(rendered, "BEGIN RSA") {
		t.Fatalf("rendered %q", rendered)
	}
}

func TestLazyContextResolutionDoesNotProbeWhenNotQuiet(t *testing.T) {
	probes := 0
	prev := compressorModelContextLength
	compressorModelContextLength = func(context.Context, provider.ContextLengthRequest) int { probes++; return 200_000 }
	t.Cleanup(func() { compressorModelContextLength = prev })
	o := DefaultContextCompressorOptions("test/model")
	c := NewContextCompressor(context.Background(), o)
	if probes != 0 {
		t.Fatal("construction probed")
	}
	_ = c.ContextLength()
	_ = c.ContextLength()
	_ = c.thresholdTokensValue()
	if probes != 1 {
		t.Fatalf("probes %d", probes)
	}
}

func TestPreflightSentinelGuard(t *testing.T) {
	for _, tc := range []struct{ before, seed, after int }{{-1, 250_000, -1}, {0, 50_000, 50_000}, {492_000, 685_344, 492_000}} {
		c := fixtureCompressor(t)
		c.LastPromptTokens = tc.before
		c.MaybeSeedPreflightDisplayTokens(tc.seed)
		if c.LastPromptTokens != tc.after {
			t.Fatalf("%+v got %d", tc, c.LastPromptTokens)
		}
	}
}

func TestSanitizerMatchesResponsesCallIDWhenIDDiffers(t *testing.T) {
	c := fixtureCompressor(t)
	msgs := []map[string]any{{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "fc_123", "call_id": "call_123",
		"response_item_id": "fc_123", "type": "function", "function": map[string]any{"name": "search_files", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "call_123", "content": "result"}}
	var ids []any
	for _, m := range c.sanitizeToolPairs(msgs) {
		if m["role"] == "tool" {
			ids = append(ids, m["tool_call_id"])
		}
	}
	if fmt.Sprint(ids) != "[call_123]" {
		t.Fatalf("ids %v", ids)
	}
}

func TestMergedTailSummaryStillDetectedAndStripped(t *testing.T) {
	merged := state.MergedPriorContextHeader + "\nold tail content here\n\n" + state.MergedSummaryDelimiter + "\n\n" + state.SummaryPrefix + "\nTHE_SUMMARY_BODY\n\n" + state.SummaryEndMarker
	if !state.IsContextSummaryContent(merged) || stripSummaryPrefix(merged) != "THE_SUMMARY_BODY" {
		t.Fatalf("merged %q", stripSummaryPrefix(merged))
	}
	standalone := state.SummaryPrefix + "\nSTANDALONE_BODY\n\n" + state.SummaryEndMarker
	if !state.IsContextSummaryContent(standalone) || stripSummaryPrefix(standalone) != "STANDALONE_BODY" {
		t.Fatal("standalone")
	}
}

func TestSummaryTargetRatioHeadSemantics(t *testing.T) {
	t.Run("protect first n semantics stable without system prompt", func(t *testing.T) {
		c := newCompressorWith(t, 100_000, func(o *ContextCompressorOptions) { o.Model, o.ProtectFirstN, o.ProtectLastN = "test", 0, 2 })
		msgs := msgRows(10)
		if c.protectHeadSize(msgs) != 0 {
			t.Fatal("head without system")
		}
		c.ProtectFirstN = 3
		if c.protectHeadSize(msgs) != 3 {
			t.Fatal("head of three")
		}
	})
}

// Assistant row carrying one function call per id with the given content
func callRow(content string, ids ...string) map[string]any {
	calls := make([]any, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, map[string]any{"id": id, "function": map[string]any{"name": "search", "arguments": "{}"}})
	}
	return map[string]any{"role": "assistant", "content": content, "tool_calls": calls}
}

// Ids of the tool calls a row carries
func callIDs(m map[string]any) []string {
	var ids []string
	calls, _ := m["tool_calls"].([]any)
	for _, raw := range calls {
		if tc, ok := raw.(map[string]any); ok {
			ids = append(ids, fmt.Sprint(tc["id"]))
		}
	}
	return ids
}

// First and last rows of the role, nil when absent
func rowsOfRole(rows []map[string]any, role string) []map[string]any {
	var out []map[string]any
	for _, m := range rows {
		if m["role"] == role {
			out = append(out, m)
		}
	}
	return out
}

func TestTurnPairPreservation(t *testing.T) {
	newC := func(t *testing.T) *ContextCompressor {
		return newCompressorWith(t, 100_000, func(o *ContextCompressorOptions) { o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN = 0.85, 1, 0 })
	}
	t.Run("user in compressed region pulled back", func(t *testing.T) {
		msgs := []map[string]any{{"role": "user", "content": "head"}, {"role": "assistant", "content": "hi"}, {"role": "user", "content": "do thing"}, {"role": "assistant", "content": "done"}}
		if got := newC(t).ensureLastUserMessageInTail(msgs, 3, 0); got > 2 {
			t.Fatalf("cut %d", got)
		}
	})
	t.Run("orphan prevented user at head end", func(t *testing.T) {
		msgs := []map[string]any{{"role": "user", "content": "first exchange"}, {"role": "user", "content": "THE ACTIVE ASK"}, {"role": "assistant", "content": "done"},
			{"role": "tool", "tool_call_id": "c1", "content": "toolout"}, {"role": "assistant", "content": "final reply"}}
		got := newC(t).ensureLastUserMessageInTail(msgs, 3, 1)
		if got != 4 || msgs[got]["role"] != "assistant" {
			t.Fatalf("cut %d", got)
		}
	})
	t.Run("no orphan after full compaction cycle", func(t *testing.T) {
		c := newC(t)
		msgs := []map[string]any{{"role": "user", "content": "initial"}, {"role": "assistant", "content": "ok"}}
		for i := 0; i < 5; i++ {
			msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("step %d", i)}, map[string]any{"role": "assistant", "content": fmt.Sprintf("done %d", i)})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": "lights off please"}, map[string]any{"role": "assistant", "content": "lights are off"})
		tail := msgs[c.findTailCutByTokens(msgs, c.ProtectFirstN, nil):]
		if len(tail) > 0 && tail[0]["role"] == "user" && (len(tail) < 2 || tail[1]["role"] != "assistant") {
			t.Fatalf("orphan user turn at tail start %v", tail[0]["content"])
		}
	})
}

func TestSanitizerStripsOrphanedToolCalls(t *testing.T) {
	c := fixtureCompressor(t)
	t.Run("strips orphaned tool calls", func(t *testing.T) {
		sanitized := c.sanitizeToolPairs([]map[string]any{callRow("", "tc_orphan"), {"role": "user", "content": "never mind"}})
		asst := rowsOfRole(sanitized, "assistant")[0]
		if pyTruthy(asst["tool_calls"]) || len(rowsOfRole(sanitized, "tool")) != 0 || !pyTruthy(asst["content"]) {
			t.Fatalf("assistant %v", asst)
		}
	})
	t.Run("strips orphaned keeps valid", func(t *testing.T) {
		sanitized := c.sanitizeToolPairs([]map[string]any{callRow("", "tc_valid", "tc_orphan"), {"role": "tool", "tool_call_id": "tc_valid", "content": "file content"},
			{"role": "assistant", "content": "done"}})
		tools := rowsOfRole(sanitized, "tool")
		if fmt.Sprint(callIDs(rowsOfRole(sanitized, "assistant")[0])) != "[tc_valid]" || len(tools) != 1 || tools[0]["tool_call_id"] != "tc_valid" {
			t.Fatalf("sanitized %v", sanitized)
		}
	})
	t.Run("strips orphaned preserves text content", func(t *testing.T) {
		asst := rowsOfRole(c.sanitizeToolPairs([]map[string]any{callRow("Let me search for that.", "tc_orphan"), {"role": "user", "content": "thanks"}}), "assistant")[0]
		if asst["content"] != "Let me search for that." || pyTruthy(asst["tool_calls"]) {
			t.Fatalf("assistant %v", asst)
		}
	})
	t.Run("keeps valid pair matching on id not call id", func(t *testing.T) {
		row := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "fc_777", "call_id": "call_777", "type": "function",
			"function": map[string]any{"name": "search", "arguments": "{}"}}}}
		sanitized := c.sanitizeToolPairs([]map[string]any{row, {"role": "tool", "tool_call_id": "fc_777", "content": "result"}})
		tools := rowsOfRole(sanitized, "tool")
		if fmt.Sprint(callIDs(rowsOfRole(sanitized, "assistant")[0])) != "[fc_777]" || len(tools) != 1 || tools[0]["tool_call_id"] != "fc_777" {
			t.Fatalf("sanitized %v", sanitized)
		}
	})
	t.Run("still drops genuine orphan with dual ids", func(t *testing.T) {
		row := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "fc_1", "call_id": "call_1", "type": "function", "function": map[string]any{"name": "search", "arguments": "{}"}},
			map[string]any{"id": "fc_2", "call_id": "call_2", "type": "function", "function": map[string]any{"name": "search", "arguments": "{}"}}}}
		sanitized := c.sanitizeToolPairs([]map[string]any{row, {"role": "tool", "tool_call_id": "call_1", "content": "result"}, {"role": "user", "content": "next question"}})
		if fmt.Sprint(callIDs(rowsOfRole(sanitized, "assistant")[0])) != "[fc_1]" {
			t.Fatalf("sanitized %v", sanitized)
		}
	})
}

func TestSanitizerPreservesInFlightToolChain(t *testing.T) {
	c := fixtureCompressor(t)
	t.Run("trailing inflight tool call preserved", func(t *testing.T) {
		sanitized := c.sanitizeToolPairs([]map[string]any{{"role": "user", "content": "summarize"}, callRow("", "call_1"), callRow("", "call_2")})
		last := sanitized[len(sanitized)-1]
		if last["role"] != "assistant" || fmt.Sprint(callIDs(last)) != "[call_2]" || last["content"] == "(tool call removed)" {
			t.Fatalf("last %v", last)
		}
	})
	t.Run("inflight preserved while true orphan still stripped", func(t *testing.T) {
		sanitized := c.sanitizeToolPairs([]map[string]any{callRow("", "call_orphan"), {"role": "user", "content": "interim"}, callRow("", "call_pending")})
		var withCalls [][]string
		for _, m := range rowsOfRole(sanitized, "assistant") {
			if ids := callIDs(m); len(ids) > 0 {
				withCalls = append(withCalls, ids)
			}
		}
		if fmt.Sprint(withCalls) != "[[call_pending]]" {
			t.Fatalf("calls %v", withCalls)
		}
	})
	t.Run("partial batch inflight calls preserved", func(t *testing.T) {
		sanitized := c.sanitizeToolPairs([]map[string]any{{"role": "user", "content": "run the batch"}, callRow("", "c1", "c2", "c3"), {"role": "tool", "tool_call_id": "c1", "content": "done 1"}})
		asst := rowsOfRole(sanitized, "assistant")
		tools := rowsOfRole(sanitized, "tool")
		if fmt.Sprint(callIDs(asst[len(asst)-1])) != "[c1 c2 c3]" || len(tools) != 1 || tools[0]["tool_call_id"] != "c1" {
			t.Fatalf("sanitized %v", sanitized)
		}
	})
}

var elisionMarkerRE = regexp.MustCompile(`\n*\.\.\.\[[^\]]*elided[^\n]*\.\.\.\n*`)

func TestSummaryPromptBoundingSampling(t *testing.T) {
	t.Run("oversized middle record does not evict tail", func(t *testing.T) {
		cap := summaryInputMaxChars
		var records []string
		for i := 0; i < 400; i++ {
			records = append(records, fmt.Sprintf("[USER]: record-%04d ", i)+strings.Repeat("x", 1200))
		}
		records = append(records, "[TOOL RESULT oversized-mid]: "+strings.Repeat("y", cap/2))
		for i := 401; i < 800; i++ {
			records = append(records, fmt.Sprintf("[USER]: record-%04d ", i)+strings.Repeat("x", 1200))
		}
		records = append(records, "[USER]: newest-tail-record")
		sampled, _ := sampleSummaryRecords(records)
		if len([]rune(sampled)) > cap || !strings.HasSuffix(strings.TrimRight(sampled, " \n"), "[USER]: newest-tail-record") ||
			!strings.Contains(sampled, "[TOOL RESULT oversized-mid]: yyyy") || strings.Count(sampled, strings.Repeat("y", 1000)) >= (cap/2)/1000 ||
			strings.Count(sampled, "[USER]: record-") <= 8 {
			t.Fatalf("sampled length %d", len(sampled))
		}
		var perSlice []int
		total := 0
		for _, sec := range elisionMarkerRE.Split(sampled, -1) {
			if n := strings.Count(sec, "[USER]: record-"); strings.Contains(sec, "[USER]: record-") {
				perSlice = append(perSlice, n)
				total += n
			}
		}
		for _, n := range perSlice {
			if float64(n) > float64(total)/float64(len(perSlice))+1 {
				t.Fatalf("per slice %v", perSlice)
			}
		}
	})
	t.Run("extension closes gaps without stray markers", func(t *testing.T) {
		cap := summaryInputMaxChars
		var records []string
		sum := 0
		for i := 0; i < 20; i++ {
			records = append(records, fmt.Sprintf("[USER]: record-%02d ", i)+strings.Repeat("x", 8500))
			sum += len(records[i])
		}
		if sum <= cap {
			t.Fatal("sampling must engage")
		}
		sampled, coverage := sampleSummaryRecords(records)
		if len(sampled) > cap {
			t.Fatalf("sampled %d", len(sampled))
		}
		var shown []int
		for _, m := range regexp.MustCompile(`record-(\d\d) x`).FindAllStringSubmatch(sampled, -1) {
			n, _ := strconv.Atoi(m[1])
			shown = append(shown, n)
		}
		for i := 1; i < len(shown); i++ {
			if shown[i] <= shown[i-1] {
				t.Fatalf("shown %v", shown)
			}
		}
		if shown[0] != 0 || shown[len(shown)-1] != 19 || coverage.sampledRecordCount != len(shown) || strings.Count(sampled, "chars elided") >= sampledInputSlices-1 {
			t.Fatalf("shown %v coverage %+v", shown, coverage)
		}
		for i := 1; i < len(shown); i++ {
			a, b := shown[i-1], shown[i]
			want := records[a] + "\n\n" + records[b]
			if b != a+1 {
				want = records[a] + "\n\n...[records " + groupedInt(a+2) + "-" + groupedInt(b) + ":"
			}
			if !strings.Contains(sampled, want) {
				t.Fatalf("gap %d %d", a, b)
			}
		}
	})
	t.Run("marker does not collide with summary classifier", func(t *testing.T) {
		bounded := boundSummaryInput(strings.Repeat("z", summaryInputMaxChars*2))
		if !strings.Contains(bounded, "summary input truncated") || state.ClassifySummaryContent(bounded) != "" {
			t.Fatal("bounded classified")
		}
		markerOnly := bounded[strings.Index(bounded, "\n\n...[summary input truncated"):]
		if state.ClassifySummaryContent(strings.TrimLeft(markerOnly, " \t\n\r\v\f")) != "" {
			t.Fatal("marker classified")
		}
	})
}

// Compressor over a 200K window protecting one head row with three tail users
func minTailCompressor(t *testing.T) *ContextCompressor {
	return newCompressorWith(t, 200_000, func(o *ContextCompressorOptions) {
		o.ThresholdPercent, o.ProtectFirstN, o.MinTailUserMessages = 0.50, 1, 3
	})
}

// Users of a tail by content
func tailUsers(tail []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, m := range tail {
		if m["role"] == "user" {
			out[fmt.Sprint(m["content"])] = true
		}
	}
	return out
}

func tailBudget(tail []map[string]any) int {
	total := 0
	for _, m := range tail {
		total += estimateMsgBudgetTokens(context.Background(), m, true)
	}
	return total
}

func TestMinTailUserMessages(t *testing.T) {
	t.Run("n3 tool group integrity", func(t *testing.T) {
		c := minTailCompressor(t)
		c.SetTailTokenBudget(200)
		messages := []map[string]any{{"role": "user", "content": "start"},
			{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"function": map[string]any{"name": "read_file", "arguments": "{}"}}}},
			{"role": "tool", "content": "result content", "tool_call_id": "call_1"}, {"role": "user", "content": "user 3rd last"},
			{"role": "assistant", "content": "reply 3rd last"}, {"role": "user", "content": "user 2nd last"}, {"role": "assistant", "content": "reply 2nd last"},
			{"role": "user", "content": "user last"}, {"role": "assistant", "content": "reply last"}}
		cut := c.findTailCutByTokens(messages, c.ProtectFirstN, nil)
		var toolPositions []int
		for i, m := range messages[:cut] {
			if (m["role"] == "assistant" || m["role"] == "tool") && (pyTruthy(m["tool_calls"]) || pyTruthy(m["tool_call_id"])) {
				toolPositions = append(toolPositions, i)
			}
		}
		if len(toolPositions) >= 2 && toolPositions[len(toolPositions)-1]-toolPositions[0] != len(toolPositions)-1 {
			t.Fatal("tool group split")
		}
		users := tailUsers(messages[cut:])
		if !users["user 3rd last"] || !users["user 2nd last"] || !users["user last"] || cut < c.ProtectFirstN+1 {
			t.Fatalf("cut %d users %v", cut, users)
		}
	})
	t.Run("blank echo does not count toward n", func(t *testing.T) {
		c := minTailCompressor(t)
		contents := []string{"head", "head reply", "real oldest", "reply oldest", "real middle", "reply middle", "", "reply to echo", "   ", "another reply", "real latest", "final reply"}
		var messages []map[string]any
		for i, text := range contents {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			messages = append(messages, map[string]any{"role": role, "content": text})
		}
		if got := c.ensureLastNUserMessagesInTail(messages, 10, c.ProtectFirstN, 3); got != 2 {
			t.Fatalf("cut %d", got)
		}
	})
	t.Run("n guarantee wins over tail token budget and floor", func(t *testing.T) {
		c := minTailCompressor(t)
		c.SetTailTokenBudget(150)
		messages := []map[string]any{{"role": "user", "content": "head"}, {"role": "assistant", "content": "head reply"}}
		for _, i := range []int{3, 2, 1} {
			messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("real %d", i)}, map[string]any{"role": "assistant", "content": strings.Repeat("B", 6000)})
		}
		tail := messages[c.findTailCutByTokens(messages, c.ProtectFirstN, nil):]
		users := tailUsers(tail)
		if !users["real 1"] || !users["real 2"] || !users["real 3"] || tailBudget(tail) <= c.TailTokenBudget() {
			t.Fatalf("users %v", users)
		}
	})
}

func TestTailTokenBudgetCeiling(t *testing.T) {
	c := newCompressorWith(t, 200_000, func(o *ContextCompressorOptions) { o.ProtectFirstN, o.ProtectLastN, o.TailMode = 1, 20, "lean" })
	c.SetTailTokenBudget(10_000)
	oversized := strings.Repeat("x", 24_000)
	messages := []map[string]any{{"role": "system", "content": "system"}}
	for i := 0; i < 6; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, map[string]any{"role": role, "content": oversized})
	}
	messages = append(messages, map[string]any{"role": "user", "content": "latest request"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call-latest", "function": map[string]any{"name": "read_file", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "call-latest", "content": "result " + strings.Repeat("r", 20_000)},
		map[string]any{"role": "assistant", "content": "latest answer"})
	tail := messages[c.findTailCutByTokens(messages, 1, nil):]
	hasLatest, hasCall := false, false
	var resultRoles []any
	for _, m := range tail {
		hasLatest = hasLatest || m["content"] == "latest request"
		hasCall = hasCall || fmt.Sprint(callIDs(m)) == "[call-latest]"
		if m["tool_call_id"] == "call-latest" {
			resultRoles = append(resultRoles, m["role"])
		}
	}
	if tailBudget(tail) > int(float64(c.TailTokenBudget())*1.5) || !hasLatest || tail[len(tail)-1]["content"] != "latest answer" || fmt.Sprint(resultRoles) != "[tool]" || !hasCall {
		t.Fatalf("tail %d rows %d tokens", len(tail), tailBudget(tail))
	}
	messages[9]["content"] = "result " + strings.Repeat("r", 80_000)
	oversizedTail := messages[c.findTailCutByTokens(messages, 1, nil):]
	if tailBudget(oversizedTail) <= int(float64(c.TailTokenBudget())*1.5) || !sameRows(messages[7:], oversizedTail) {
		t.Fatalf("oversized tail %d rows", len(oversizedTail))
	}
}

func TestContextLengthSetterCoherence(t *testing.T) {
	t.Run("same value reassignment preserves threshold override", func(t *testing.T) {
		c := newCompressorWith(t, 200_000, func(o *ContextCompressorOptions) { o.Model = "test" })
		c.SetThresholdTokens(42_000)
		c.SetTailTokenBudget(8_400)
		c.SetContextLength(200_000)
		if c.thresholdTokensValue() != 42_000 || c.TailTokenBudget() != 8_400 {
			t.Fatal("override wiped")
		}
	})
	t.Run("new value assignment refloors and invalidates", func(t *testing.T) {
		c := newCompressorWith(t, 1_000_000, func(o *ContextCompressorOptions) { o.Model = "test" })
		fresh := newCompressorWith(t, 200_000, func(o *ContextCompressorOptions) { o.Model = "test" })
		c.SetContextLength(200_000)
		if c.ThresholdPercent != fresh.ThresholdPercent || c.thresholdTokensValue() != fresh.thresholdTokensValue() {
			t.Fatalf("percent %v %v tokens %d %d", c.ThresholdPercent, fresh.ThresholdPercent, c.thresholdTokensValue(), fresh.thresholdTokensValue())
		}
	})
}

// System prompt then question and reply pairs
func feasibilityRows(pairs int, content string) []map[string]any {
	msgs := []map[string]any{{"role": "system", "content": "system prompt"}}
	for i := 0; i < pairs; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("question %d", i)}, map[string]any{"role": "assistant", "content": content})
	}
	return msgs
}

func TestPreLlmFeasibilityCheck(t *testing.T) {
	t.Run("skip does not increment strike counter", func(t *testing.T) {
		c := fixtureCompressor(t)
		c.ineffectiveCompressionCount = 1
		calls := failingCallLLM(t, "summary must not run")
		c.Compress(feasibilityRows(10, "short reply"), nil, nil, false, "")
		if c.ineffectiveCompressionCount != 1 || c.prellmSkipCount < 1 || *calls != 0 {
			t.Fatalf("strikes %d skips %d calls %d", c.ineffectiveCompressionCount, c.prellmSkipCount, *calls)
		}
	})
	t.Run("skip does not trip abort on stale auth failure", func(t *testing.T) {
		c := fixtureCompressor(t)
		c.ineffectiveCompressionCount, c.lastSummaryAuthFailure = 1, true
		calls := failingCallLLM(t, "summary must not run")
		result, _ := c.Compress(feasibilityRows(10, "short reply"), nil, nil, false, "")
		if len(result) == 0 || *calls != 0 {
			t.Fatalf("result %d calls %d", len(result), *calls)
		}
	})
	t.Run("skip fires on fat tail small middle", func(t *testing.T) {
		c := fixtureCompressor(t)
		c.tailMode, c.ineffectiveCompressionCount = "legacy", 1
		msgs := []map[string]any{{"role": "system", "content": "system prompt"}}
		for i := 0; i < 6; i++ {
			msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("early question %d", i)}, map[string]any{"role": "assistant", "content": "brief answer"})
		}
		for i := 0; i < 4; i++ {
			msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("recent request %d", i)}, map[string]any{"role": "assistant", "content": "big result " + strings.Repeat("x", 20000)})
		}
		calls := failingCallLLM(t, "summary must not run")
		result, _ := c.Compress(msgs, nil, nil, false, "")
		if *calls != 0 || c.prellmSkipCount != 1 || !c.lastFeasibilitySkip || len(result) >= len(msgs) {
			t.Fatalf("calls %d skips %d feasibility %v", *calls, c.prellmSkipCount, c.lastFeasibilitySkip)
		}
	})
	t.Run("boundary accounting skip does not feed fallback streak", func(t *testing.T) {
		c := fixtureCompressor(t)
		c.ineffectiveCompressionCount = 1
		msgs := feasibilityRows(10, "short reply")
		calls := failingCallLLM(t, "summary must not run")
		for i := 0; i < 2; i++ {
			c.Compress(append([]map[string]any(nil), msgs...), nil, nil, false, "")
			if *calls != 0 || !c.lastCompressionMadeProgress {
				t.Fatalf("pass %d calls %d progress %v", i, *calls, c.lastCompressionMadeProgress)
			}
			c.recordCompletedCompaction(c.LastSummaryFallbackUsed, c.lastFeasibilitySkip)
		}
		if c.prellmSkipCount != 2 || c.fallbackCompressionStreak != 0 || c.automaticCompressionBlockedLocally(false) {
			t.Fatalf("skips %d streak %d", c.prellmSkipCount, c.fallbackCompressionStreak)
		}
	})
	t.Run("boundary accounting skip does not reset fallback streak", func(t *testing.T) {
		c := fixtureCompressor(t)
		c.recordCompletedCompaction(true, false)
		if c.fallbackCompressionStreak != 1 {
			t.Fatal("real fallback")
		}
		c.recordCompletedCompaction(true, true)
		if c.fallbackCompressionStreak != 1 {
			t.Fatal("skip boundary moved the streak")
		}
	})
}

func TestSanitizeToolPairsWhitespace(t *testing.T) {
	assistant := func(id string) map[string]any {
		return map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}}}}
	}
	c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.ProtectFirstN, o.ProtectLastN = 2, 2 })
	tools := rowsOfRole(c.sanitizeToolPairs([]map[string]any{assistant("call_abc"), {"role": "tool", "tool_call_id": " call_abc", "content": "ok"}}), "tool")
	if len(tools) != 1 || tools[0]["content"] != "ok" {
		t.Fatalf("valid result %v", tools)
	}
	for _, m := range rowsOfRole(c.sanitizeToolPairs([]map[string]any{assistant("call_real"), {"role": "tool", "tool_call_id": " call_orphan ", "content": "stale"}}), "tool") {
		if m["tool_call_id"] == "call_orphan" || m["tool_call_id"] == " call_orphan " {
			t.Fatalf("orphan survived %v", m)
		}
	}
}

func TestSummaryAccessOrQuotaClassifier(t *testing.T) {
	t.Run("missing provider api key is terminal access failure", func(t *testing.T) {
		if !isSummaryAccessOrQuotaError(errors.New("Provider 'opencode-zen' is set in config.yaml but no API key was found. Set the OPENCODE-ZEN_API_KEY environment variable.")) {
			t.Fatal("api key")
		}
		if !isSummaryAccessOrQuotaError(errors.New("Provider 'minimax-oauth' is set in config.yaml but no credentials were found. Run `icebath auth add minimax-oauth` to sign in, or switch to a different provider with `icebath model`.")) {
			t.Fatal("oauth")
		}
	})
	t.Run("unscoped secret read is terminal access failure", func(t *testing.T) {
		if !isSummaryAccessOrQuotaError(&provider.UnscopedSecretError{SecretName: "SURPLUS_API_KEY"}) {
			t.Fatal("unscoped")
		}
	})
}

// Infinite max_tokens raises the OverflowError int() raises in Python rather than reading as no reservation
func TestCoerceMaxTokensInfinityRaisesOverflowError(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	defer func() {
		failure := recover()
		var pyErr *pyint.Error
		if err, isError := failure.(error); !isError || !errors.As(err, &pyErr) || pyErr.Kind != "OverflowError" {
			t.Fatalf("recovered %v", failure)
		}
	}()
	coerceMaxTokens(math.Inf(1))
}
