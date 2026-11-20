package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Chat completion shaped response whose first choice carries the given message
func summaryResponse(message any) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"message": message}}}
}

func contentResponse(content any) map[string]any {
	return summaryResponse(map[string]any{"content": content})
}

// Scripted responses or errors served in order, recording each call's model keyword
func scriptedCallLLM(t *testing.T, steps ...any) *[]any {
	models := &[]any{}
	n := 0
	patchCallLLM(t, func(kwargs map[string]any) (any, error) {
		*models = append(*models, kwargs["model"])
		step := steps[min(n, len(steps)-1)]
		n++
		if err, ok := step.(error); ok {
			return nil, err
		}
		return step, nil
	})
	return models
}

func twoTurns() []map[string]any {
	return []map[string]any{{"role": "user", "content": "do something"}, {"role": "assistant", "content": "ok"}}
}

func TestGenerateSummarySuccessShapes(t *testing.T) {
	t.Run("none content does not crash", func(t *testing.T) {
		c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model = "test" })
		scriptedCallLLM(t, contentResponse("[CONTEXT SUMMARY]: tool calls happened"))
		turns := []map[string]any{
			{"role": "user", "content": "do something"},
			{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"function": map[string]any{"name": "search"}}}},
			{"role": "tool", "content": "result"},
			{"role": "assistant", "content": nil},
			{"role": "user", "content": "thanks"},
		}
		summary, err := c.generateSummary(context.Background(), turns, nil, "", false)
		if err != nil || summary == nil || !strings.HasPrefix(*summary, state.SummaryPrefix) {
			t.Fatalf("summary %v %v", summary, err)
		}
	})
	t.Run("none content treated as failure not empty summary", func(t *testing.T) {
		c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model = "test" })
		scriptedCallLLM(t, contentResponse(nil))
		summary, _ := c.generateSummary(context.Background(), twoTurns(), nil, "", false)
		if summary != nil || c.summaryFailureCooldownUntil <= 0 {
			t.Fatalf("summary %v cooldown %v", summary, c.summaryFailureCooldownUntil)
		}
	})
	t.Run("string message coerced to summary content", func(t *testing.T) {
		c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model, o.TailMode = "test", "legacy" })
		scriptedCallLLM(t, summaryResponse("plain summary text"))
		summary, _ := c.generateSummary(context.Background(), twoTurns(), nil, "", false)
		if summary == nil || !strings.HasPrefix(*summary, state.SummaryPrefix+"\n"+state.HistoricalTaskHeading+"\n") ||
			!strings.Contains(*summary, "do something") || !strings.HasSuffix(*summary, "plain summary text") {
			t.Fatalf("summary %v", summary)
		}
	})
	t.Run("generate summary flags empty content failure", func(t *testing.T) {
		c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model = "test" })
		scriptedCallLLM(t, contentResponse("   "))
		summary, _ := c.generateSummary(context.Background(), msgRows(10), nil, "", false)
		if summary != nil || !c.lastSummaryEmptyContentFailure || c.lastSummaryAuthFailure || c.lastSummaryNetworkFailure {
			t.Fatalf("flags empty=%v auth=%v net=%v", c.lastSummaryEmptyContentFailure, c.lastSummaryAuthFailure, c.lastSummaryNetworkFailure)
		}
	})
	t.Run("empty content summary aborts compression and preserves messages", func(t *testing.T) {
		c := abortCompressor(t, false)
		msgs := msgRows(12)
		scriptedCallLLM(t, contentResponse(""))
		result, err := c.CompressBypassingCooldown(context.Background(), msgs, iptr(999999), nil, true, "", false)
		if err != nil || !sameRows(result, msgs) || !c.lastSummaryEmptyContentFailure || !c.lastCompressAborted || c.LastSummaryFallbackUsed || c.LastSummaryDroppedCount != 0 {
			t.Fatalf("first %v aborted=%v", err, c.lastCompressAborted)
		}
		second, _ := c.Compress(msgs, iptr(999999), nil, false, "")
		if !sameRows(second, msgs) || !c.lastCompressAborted || c.LastSummaryFallbackUsed {
			t.Fatalf("cooldown re-entry aborted=%v fallback=%v", c.lastCompressAborted, c.LastSummaryFallbackUsed)
		}
	})
}

func TestSummaryFallbackToMainModelSucceeds(t *testing.T) {
	cases := []struct {
		name, aux, want, errText string
		first                    any
	}{
		{"model not found 404", "broken-aux-model", "summary via main model", "404", providerStatusError("404 model_not_found: no such model", 404)},
		{"empty content", "flaky-aux-model", "summary via main model after empty aux", "empty content", contentResponse("   ")},
		{"json decode error", "aux-via-broken-proxy", "summary via main model", "", &provider.AuxTransportError{Class: "JSONDecodeError", Message: "Expecting value: line 1 column 1 (char 0)"}},
	}
	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model, o.SummaryModelOverride = "main-model", k.aux })
			models := scriptedCallLLM(t, k.first, contentResponse(k.want))
			summary, _ := c.generateSummary(context.Background(), twoTurns(), nil, "", false)
			if len(*models) != 2 || (*models)[0] != k.aux || (*models)[1] != "main-model" {
				t.Fatalf("models %v", *models)
			}
			if summary == nil || !strings.Contains(*summary, k.want) || c.LastAuxModelFailureModel != k.aux || c.LastAuxModelFailureError == "" {
				t.Fatalf("summary %v aux %q %q", summary, c.LastAuxModelFailureModel, c.LastAuxModelFailureError)
			}
			if !strings.Contains(strings.ToLower(c.LastAuxModelFailureError), k.errText) || len([]rune(c.LastAuxModelFailureError)) > 220 {
				t.Fatalf("aux error %q", c.LastAuxModelFailureError)
			}
		})
	}
}

// The eight row system led transcript the fallback and abort classes compress
func sysLedRows() []map[string]any {
	return append([]map[string]any{{"role": "system", "content": "sys"}}, alternating(7, "msg %d")...)
}

// Session store holding s1 with a persisted summary failure cooldown far in the future
func cooldownStore(t *testing.T) *state.SessionDB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	reason := "timeout"
	db.RecordCompressionFailureCooldown("s1", float64(time.Now().UnixNano())/1e9+999.0, &reason)
	return db
}

func cooldownCleared(t *testing.T, db *state.SessionDB) bool {
	row, err := db.GetCompressionFailureCooldown("s1")
	if err != nil {
		t.Fatal(err)
	}
	return row == nil
}

func TestAuxModelFallbackSurfacedToCallers(t *testing.T) {
	fallbackCompressor := func(t *testing.T) *ContextCompressor {
		return newCompressorWith(t, 100000, func(o *ContextCompressorOptions) {
			o.Model, o.SummaryModelOverride, o.ProtectFirstN, o.ProtectLastN = "main-model", "broken-aux-model", 2, 2
		})
	}
	t.Run("compress exposes aux failure fields after successful fallback", func(t *testing.T) {
		c := fallbackCompressor(t)
		scriptedCallLLM(t, providerStatusError("400 provider rejected configured model", 400), contentResponse("summary via main"))
		result, _ := c.Compress(sysLedRows(), nil, nil, false, "")
		if c.LastSummaryFallbackUsed || c.LastAuxModelFailureModel != "broken-aux-model" || !strings.Contains(c.LastAuxModelFailureError, "400") {
			t.Fatalf("fallback=%v aux %q %q", c.LastSummaryFallbackUsed, c.LastAuxModelFailureModel, c.LastAuxModelFailureError)
		}
		if !anyContains(result, "summary via main") {
			t.Fatalf("summary missing %v", result)
		}
	})
	t.Run("compress clears aux failure fields at start of next call", func(t *testing.T) {
		c := fallbackCompressor(t)
		scriptedCallLLM(t, providerStatusError("400 aux model busted", 400), contentResponse("summary via main"))
		c.Compress(sysLedRows(), nil, nil, false, "")
		if c.LastAuxModelFailureModel != "broken-aux-model" {
			t.Fatalf("aux model %q", c.LastAuxModelFailureModel)
		}
		scriptedCallLLM(t, contentResponse("summary via main"))
		c.Compress(sysLedRows(), nil, nil, false, "")
		if c.LastAuxModelFailureModel != "" || c.LastAuxModelFailureError != "" {
			t.Fatalf("aux fields kept %q %q", c.LastAuxModelFailureModel, c.LastAuxModelFailureError)
		}
	})
}

func TestAbortOnSummaryFailureClearsPersistedCooldown(t *testing.T) {
	abortingCompressor := func(t *testing.T) *ContextCompressor {
		return newCompressorWith(t, 100000, func(o *ContextCompressorOptions) {
			o.Model, o.ProtectFirstN, o.ProtectLastN, o.AbortOnSummaryFailure = "test", 2, 2, true
		})
	}
	t.Run("aux fallback clears persisted session cooldown before retry", func(t *testing.T) {
		db := cooldownStore(t)
		c := abortingCompressor(t)
		c.bindSessionState(db, "s1")
		c.summaryModel = "aux/model"
		c.fallbackToMainForCompression(errors.New("provider down"), "failed", "")
		if c.summaryModel != "" || c.summaryFailureCooldownUntil != 0 || !cooldownCleared(t, db) {
			t.Fatalf("model %q cooldown %v", c.summaryModel, c.summaryFailureCooldownUntil)
		}
	})
	t.Run("success clears persisted session cooldown", func(t *testing.T) {
		db := cooldownStore(t)
		c := abortingCompressor(t)
		c.bindSessionState(db, "s1")
		c.summaryFailureCooldownUntil = 0
		msgs := sysLedRows()
		calls := patchCallLLM(t, func(map[string]any) (any, error) { return contentResponse("summary text"), nil })
		result, _ := c.Compress(msgs, iptr(999999), nil, false, "")
		if *calls == 0 || c.lastCompressAborted || len(result) >= len(msgs) || !cooldownCleared(t, db) {
			t.Fatalf("calls=%d aborted=%v len=%d", *calls, c.lastCompressAborted, len(result))
		}
	})
}
