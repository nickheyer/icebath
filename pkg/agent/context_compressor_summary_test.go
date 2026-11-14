package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestGenerateSummaryNoneContentInSystemMessageCompress(t *testing.T) {
	c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model, o.ProtectFirstN, o.ProtectLastN = "test", 2, 2 })
	msgs := append([]map[string]any{{"role": "system", "content": nil}}, alternating(10, "msg %d")...)
	failingCallLLM(t, "no provider")
	result, err := c.Compress(msgs, nil, nil, false, "")
	if err != nil || len(result) >= len(msgs) {
		t.Fatalf("compress %v %d", err, len(result))
	}
}

func TestSummaryFailureEntersCooldownAndSkipsRetry(t *testing.T) {
	c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model = "test" })
	messages := []map[string]any{{"role": "user", "content": "do something"}, {"role": "assistant", "content": "ok"}}
	calls := failingCallLLM(t, "boom")
	first, _ := c.generateSummary(context.Background(), messages, nil, "", false)
	second, _ := c.generateSummary(context.Background(), messages, nil, "", false)
	if first != nil || second != nil || *calls != 1 {
		t.Fatalf("cooldown %v %v %d", first, second, *calls)
	}
}

func TestAuthFailureAborts(t *testing.T) {
	aborts := func(t *testing.T, err error, wantErrText bool) *ContextCompressor {
		c := abortCompressor(t, false)
		msgs := msgRows(12)
		failing := patchCallLLM(t, func(map[string]any) (any, error) { return nil, err })
		result, cerr := c.CompressBypassingCooldown(context.Background(), msgs, iptr(999999), nil, true, "", false)
		if cerr != nil || !sameRows(result, msgs) || !c.lastCompressAborted || c.LastSummaryFallbackUsed || c.LastSummaryDroppedCount != 0 {
			t.Fatalf("abort %v %d aborted=%v fallback=%v dropped=%d", cerr, len(result), c.lastCompressAborted, c.LastSummaryFallbackUsed, c.LastSummaryDroppedCount)
		}
		if wantErrText && (c.lastSummaryError == nil || *c.lastSummaryError != err.Error()) {
			t.Fatalf("error text %v", c.lastSummaryError)
		}
		_ = failing
		return c
	}
	t.Run("400 out of extra usage aborts instead of dropping context", func(t *testing.T) {
		c := aborts(t, providerStatusError("Error code: 400 - {'error': {'message': 'out of extra usage'}}", 400), false)
		if !c.lastSummaryAuthFailure {
			t.Fatal("auth flag")
		}
	})
	t.Run("missing provider api key preserves original messages", func(t *testing.T) {
		c := aborts(t, errors.New("Provider 'opencode-zen' is set in config.yaml but no API key was found. Set the OPENCODE-ZEN_API_KEY environment variable, or switch to a different provider with icebath model."), true)
		if !c.lastSummaryAuthFailure {
			t.Fatal("auth flag")
		}
	})
	t.Run("402 quota with retry uses existing fallback", func(t *testing.T) {
		c := abortCompressor(t, false)
		msgs := msgRows(12)
		patchCallLLM(t, func(map[string]any) (any, error) {
			return nil, providerStatusError("quota exceeded, please retry after the window resets", 402)
		})
		result, _ := c.CompressBypassingCooldown(context.Background(), msgs, iptr(999999), nil, true, "", false)
		if sameRows(result, msgs) || c.lastSummaryAuthFailure || c.lastCompressAborted || !c.LastSummaryFallbackUsed {
			t.Fatalf("quota fallback auth=%v aborted=%v fallback=%v", c.lastSummaryAuthFailure, c.lastCompressAborted, c.LastSummaryFallbackUsed)
		}
	})
	t.Run("provider overload aborts instead of dropping context", func(t *testing.T) {
		c := abortCompressor(t, false)
		c.summaryModel = "test/auxiliary"
		msgs := msgRows(12)
		calls := patchCallLLM(t, func(map[string]any) (any, error) {
			return nil, providerStatusError("Our servers are currently overloaded. Please try again later.", 503)
		})
		result, _ := c.CompressBypassingCooldown(context.Background(), msgs, iptr(999999), nil, true, "", false)
		if *calls != 2 || !sameRows(result, msgs) || !c.lastCompressAborted || c.LastSummaryFallbackUsed || c.LastSummaryDroppedCount != 0 {
			t.Fatalf("overload calls=%d aborted=%v", *calls, c.lastCompressAborted)
		}
		if fc, _ := c.Last.Get("failure_class"); fc != "summary_overload_failure" {
			t.Fatalf("failure class %v", fc)
		}
	})
	t.Run("403 also flags auth failure", func(t *testing.T) {
		c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model = "test" })
		patchCallLLM(t, func(map[string]any) (any, error) {
			return nil, providerStatusError("Error code: 403 - {'status': 401, 'message': 'Your API key is invalid, blocked or out of funds.'}", 403)
		})
		c.generateSummary(context.Background(), msgRows(10), nil, "", false)
		if !c.lastSummaryAuthFailure {
			t.Fatal("403 auth flag")
		}
	})
	t.Run("generate summary flags network failure", func(t *testing.T) {
		c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model = "test" })
		patchCallLLM(t, func(map[string]any) (any, error) {
			return nil, &provider.AuxTransportError{Class: "ConnectionError", Message: "Connection error."}
		})
		result, _ := c.generateSummary(context.Background(), msgRows(10), nil, "", false)
		if result != nil || !c.lastSummaryNetworkFailure || c.lastSummaryAuthFailure {
			t.Fatalf("network %v %v %v", result, c.lastSummaryNetworkFailure, c.lastSummaryAuthFailure)
		}
	})
	for _, text := range []string{"Auxiliary compression: LLM returned None response",
		"Auxiliary compression: LLM returned invalid response (type=str): 'oops'. Expected object with .choices[0].message — check provider adapter or custom endpoint compatibility."} {
		t.Run("auxiliary terminal response aborts compression", func(t *testing.T) {
			c := aborts(t, &provider.AuxRuntimeError{Message: text}, false)
			if !c.lastSummaryEmptyContentFailure {
				t.Fatalf("empty content flag for %q", text)
			}
		})
	}
}

func TestSummaryFallbackToMainModelNotWhenSummaryModelEqualsMain(t *testing.T) {
	c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model, o.SummaryModelOverride = "main-model", "main-model" })
	calls := failingCallLLM(t, "500 internal error")
	result, _ := c.generateSummary(context.Background(), []map[string]any{{"role": "user", "content": "do something"}, {"role": "assistant", "content": "ok"}}, nil, "", false)
	if *calls != 1 || result != nil || c.summaryModelFallenBack {
		t.Fatalf("calls=%d result=%v fallen=%v", *calls, result, c.summaryModelFallenBack)
	}
}

func TestStreamingClosedFailure(t *testing.T) {
	failOnMain := func(t *testing.T, err error) *ContextCompressor {
		c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model = "main-model" })
		patchCallLLM(t, func(map[string]any) (any, error) { return nil, err })
		prev := compressorMonotonic
		compressorMonotonic = func() float64 { return 1000.0 }
		defer func() { compressorMonotonic = prev }()
		result, _ := c.generateSummary(context.Background(), []map[string]any{{"role": "user", "content": "do something"}, {"role": "assistant", "content": "ok"}}, nil, "", false)
		if result != nil {
			t.Fatalf("summary %v", *result)
		}
		return c
	}
	for _, message := range []string{"RemoteProtocolError: incomplete chunked read", "RemoteProtocolError: response ended prematurely"} {
		closed := failOnMain(t, errors.New(message))
		generic := failOnMain(t, errors.New("something unexpected broke"))
		if !closed.lastSummaryNetworkFailure || generic.lastSummaryNetworkFailure ||
			!(1000.0 < closed.summaryFailureCooldownUntil && closed.summaryFailureCooldownUntil < generic.summaryFailureCooldownUntil) {
			t.Fatalf("%s closed=%v generic=%v cooldowns %v %v", message, closed.lastSummaryNetworkFailure, generic.lastSummaryNetworkFailure,
				closed.summaryFailureCooldownUntil, generic.summaryFailureCooldownUntil)
		}
	}
	stall := fmt.Sprintf("Codex auxiliary Responses %s: no new output for %.1fs (%.1fs elapsed)", provider.CodexStreamStallMarker, 300.0, 0.0)
	c := failOnMain(t, &provider.AuxTimeoutError{Message: stall})
	if c.lastSummaryNetworkFailure || c.consecutiveTimeoutFailures != 1 {
		t.Fatalf("codex stall network=%v timeouts=%d", c.lastSummaryNetworkFailure, c.consecutiveTimeoutFailures)
	}
	for _, err := range []error{&provider.AuxSDKError{Class: "APITimeoutError", Message: "Request timed out."},
		&provider.AuxSDKError{Class: "APIConnectionError", Message: "upstream " + provider.CodexStreamStallMarker}} {
		if !failOnMain(t, err).lastSummaryNetworkFailure {
			t.Fatalf("transport error %v", err)
		}
	}
}

func TestSummaryFailureTrackingForGatewayWarning(t *testing.T) {
	t.Run("records fallback and dropped count on summary failure", func(t *testing.T) {
		c := abortCompressor(t, false)
		failingCallLLM(t, "404 model not found")
		result, _ := c.Compress(eightRows(), nil, nil, false, "")
		if !c.LastSummaryFallbackUsed || c.LastSummaryDroppedCount <= 0 || c.lastSummaryError == nil || c.lastCompressAborted {
			t.Fatalf("fallback=%v dropped=%d", c.LastSummaryFallbackUsed, c.LastSummaryDroppedCount)
		}
		found := false
		for _, m := range result {
			found = found || truthyField(m, state.CompressedSummaryMetadataKey)
		}
		if !found {
			t.Fatal("no summary row")
		}
	})
	t.Run("fallback preserves tool paths and redacts secret context", func(t *testing.T) {
		c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model, o.ProtectFirstN, o.ProtectLastN = "test", 1, 1 })
		secret := "ghp_" + strings.Repeat("a", 36)
		msgs := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "Fix /tmp/project/app.py and never leak " + secret},
			{"role": "assistant", "content": "I will inspect it.", "tool_calls": []any{map[string]any{"id": "call-1",
				"function": map[string]any{"name": "read_file", "arguments": `{"path":"/tmp/project/app.py"}`}}}},
			{"role": "tool", "tool_call_id": "call-1", "content": "read /tmp/project/app.py with token " + secret},
			{"role": "assistant", "content": "Found the bug in /tmp/project/app.py"}, {"role": "user", "content": "Patch it after this"},
			{"role": "assistant", "content": "Ready to patch"}, {"role": "user", "content": "current live request should stay in tail"}}
		failingCallLLM(t, "timeout")
		result, _ := c.Compress(msgs, nil, nil, false, "")
		fallback := ""
		for _, m := range result {
			if truthyField(m, state.CompressedSummaryMetadataKey) {
				fallback, _ = m["content"].(string)
				break
			}
		}
		if !strings.Contains(fallback, "Called tool(s): read_file") || !strings.Contains(fallback, "/tmp/project/app.py") ||
			strings.Contains(fallback, secret) || strings.Contains(fallback, "ghp_") {
			t.Fatalf("fallback %q", fallback)
		}
	})
}

func TestAbortOnSummaryFailurePreservesMessages(t *testing.T) {
	c := abortCompressor(t, true)
	msgs := eightRows()
	failingCallLLM(t, "404 model not found")
	result, _ := c.Compress(msgs, nil, nil, false, "")
	if !c.lastCompressAborted || c.lastSummaryError == nil || c.LastSummaryFallbackUsed || c.LastSummaryDroppedCount != 0 || !sameRows(result, msgs) {
		t.Fatalf("aborted=%v fallback=%v dropped=%d", c.lastCompressAborted, c.LastSummaryFallbackUsed, c.LastSummaryDroppedCount)
	}
}

func TestCooldownReentryAbort(t *testing.T) {
	for name, err := range map[string]error{
		"network failure cooldown reentry still aborts": &provider.AuxTransportError{Class: "ConnectionError", Message: "Connection error."},
		"auth failure cooldown reentry still aborts":    providerStatusError("Error code: 401 - invalid api key", 401),
	} {
		t.Run(name, func(t *testing.T) {
			c := abortCompressor(t, false)
			msgs := msgRows(12)
			patchCallLLM(t, func(map[string]any) (any, error) { return nil, err })
			first, _ := c.CompressBypassingCooldown(context.Background(), msgs, iptr(999999), nil, true, "", false)
			if !sameRows(first, msgs) || !c.lastCompressAborted || !(c.lastSummaryNetworkFailure || c.lastSummaryAuthFailure) {
				t.Fatalf("first aborted=%v network=%v auth=%v", c.lastCompressAborted, c.lastSummaryNetworkFailure, c.lastSummaryAuthFailure)
			}
			second, _ := c.Compress(msgs, iptr(999999), nil, false, "")
			if !sameRows(second, msgs) || !c.lastCompressAborted || c.LastSummaryFallbackUsed {
				t.Fatalf("second aborted=%v fallback=%v", c.lastCompressAborted, c.LastSummaryFallbackUsed)
			}
		})
	}
}

func TestCompressSummaryPaths(t *testing.T) {
	t.Run("compress strips db persisted from assembled messages", func(t *testing.T) {
		c := fixtureCompressor(t)
		msgs := alternating(10, "m%d")
		for _, m := range msgs {
			m["_db_persisted"] = true
		}
		failingCallLLM(t, "no provider")
		result, err := c.Compress(msgs, nil, nil, false, "")
		if err != nil || len(result) >= len(msgs) {
			t.Fatalf("compress %v %d", err, len(result))
		}
		for _, m := range result {
			if _, ok := m["_db_persisted"]; ok {
				t.Fatalf("flush marker survived %v", m)
			}
		}
	})
	t.Run("protect first n 0 preserves only system prompt", func(t *testing.T) {
		c := newCompressorWith(t, 100_000, func(o *ContextCompressorOptions) { o.Model, o.ProtectFirstN, o.ProtectLastN = "test", 0, 2 })
		msgs := append([]map[string]any{{"role": "system", "content": "System prompt"}}, msgRows(8)...)
		failingCallLLM(t, "no provider")
		result, _ := c.Compress(msgs, nil, nil, false, "")
		content, _ := result[0]["content"].(string)
		if result[0]["role"] != "system" || !strings.HasPrefix(content, "System prompt") || result[1]["content"] == "msg 0" ||
			result[len(result)-1]["content"] != msgs[len(msgs)-1]["content"] {
			t.Fatalf("result %v", result)
		}
	})
}

func TestPreLlmFeasibilityCheckSummaryPaths(t *testing.T) {
	for name, tc := range map[string]struct {
		strikes int
		force   bool
	}{"no skip when force true": {1, true}, "no skip when no prior strikes": {0, false}} {
		t.Run(name, func(t *testing.T) {
			c := fixtureCompressor(t)
			c.ineffectiveCompressionCount = tc.strikes
			calls := failingCallLLM(t, "no provider")
			c.Compress(feasibilityRows(10, "short reply"), nil, nil, tc.force, "")
			if *calls != 1 || c.prellmSkipCount != 0 {
				t.Fatalf("calls %d skips %d", *calls, c.prellmSkipCount)
			}
		})
	}
	t.Run("real fallback still feeds streak", func(t *testing.T) {
		c := fixtureCompressor(t)
		c.ineffectiveCompressionCount = 1
		failingCallLLM(t, "no provider")
		c.Compress(feasibilityRows(10, strings.Repeat("filler ", 3000)), nil, nil, false, "")
		if c.lastFeasibilitySkip || !c.LastSummaryFallbackUsed {
			t.Fatalf("feasibility %v fallback %v", c.lastFeasibilitySkip, c.LastSummaryFallbackUsed)
		}
		c.recordCompletedCompaction(c.LastSummaryFallbackUsed, c.lastFeasibilitySkip)
		if c.fallbackCompressionStreak != 1 {
			t.Fatalf("streak %d", c.fallbackCompressionStreak)
		}
	})
}
