package agent

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Integer coercion for telemetry fields, nil when int of the value raises
func telemetrySafeInt(value any) any {
	n, err := pyint.Of(value)
	if err != nil || !n.IsInt64() {
		return nil
	}
	return int(n.Int64())
}

// Integer reading of a telemetry field, zero for None
func telemetryInt(t *pyjson.Object, key string) int {
	v, _ := t.Get(key)
	n, _ := telemetrySafeInt(v).(int)
	return n
}

// Dict message rows as live messages for the shared rough token estimator
func rowsToMessages(rows []map[string]any) []*state.Message {
	out := make([]*state.Message, len(rows))
	for i, row := range rows {
		out[i] = state.MessageFromDict(row)
	}
	return out
}

// Rough message only token estimate of dict rows at the learned per image price
func estimateRowsRough(ctx context.Context, rows []map[string]any) int {
	return compactionEstimateRough(ctx, rowsToMessages(rows))
}

// CompressionTelemetry is the content free per attempt telemetry a compressor embeds and each phase fills
type CompressionTelemetry struct {
	Seed                     *pyjson.Object
	Active                   *pyjson.Object
	Last                     *pyjson.Object
	LastAuxModelFailureModel string
	LastAuxModelFailureError string
	LastSummaryFallbackUsed  bool
	LastSummaryDroppedCount  int
}

// TelemetryAttempt carries the attempt keywords and the compressor main route a telemetry record is seeded from
type TelemetryAttempt struct {
	CurrentTokens   any
	AttemptID       string
	SessionID       string
	TriggerSource   string
	Provider        string
	Model           string
	ContextLength   any
	ThresholdTokens any
}

// Seeds the active and last telemetry record of one compression attempt
func (t *CompressionTelemetry) Begin(at TelemetryAttempt) *pyjson.Object {
	seed := t.Seed
	if seed == nil {
		seed = &pyjson.Object{}
	}
	seeded := func(explicit, key string) any {
		if explicit != "" {
			return explicit
		}
		v, _ := seed.Get(key)
		return v
	}
	attemptID, sessionID, triggerSource := seeded(at.AttemptID, "attempt_id"), seeded(at.SessionID, "session_id"), seeded(at.TriggerSource, "trigger_source")
	if !pyrepr.Truthy(attemptID) {
		attemptID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if !pyrepr.Truthy(sessionID) {
		sessionID = ""
	}
	if !pyrepr.Truthy(triggerSource) {
		triggerSource = "unknown"
	}
	overflowReason, _ := seed.Get("overflow_reason")
	telemetry := &pyjson.Object{}
	for _, kv := range []struct {
		k string
		v any
	}{
		{"event", "compression_attempt"}, {"attempt_id", attemptID}, {"session_id", sessionID},
		{"trigger_source", triggerSource}, {"overflow_reason", overflowReason},
		{"main_provider", at.Provider}, {"main_model", at.Model},
		{"main_context_limit", telemetrySafeInt(at.ContextLength)},
		{"current_estimated_tokens", telemetrySafeInt(at.CurrentTokens)},
		{"effective_threshold", telemetrySafeInt(at.ThresholdTokens)}, {"protected_head_tokens", nil},
		{"protected_tail_tokens", nil}, {"middle_window_tokens", nil}, {"prellm_skip_count", 0},
		{"aux_prompt_tokens", nil}, {"aux_output_reservation", nil}, {"aux_provider", ""}, {"aux_model", ""},
		{"effective_aux_context", nil}, {"fit_margin", nil}, {"chunking", false}, {"chunk_count", 0},
		{"total_duration_ms", nil}, {"aux_call_duration_ms", nil}, {"queue_wait_ms", nil}, {"prompt_build_ms", nil},
		{"time_to_first_progress_ms", nil}, {"summary_generation_ms", nil}, {"commit_ms", nil},
		{"fallback_used", false}, {"commit_status", "unknown"}, {"split_status", "unknown"}, {"failure_class", nil},
		{"summary_input_chars", nil}, {"summary_input_sampled_chars", nil}, {"summary_input_omitted_chars", nil},
		{"summary_input_record_count", nil}, {"summary_input_sampled_record_count", nil},
		{"summary_input_elided_record_count", nil},
		{"method", "none"}, {"items_dropped", 0}, {"messages_before", nil}, {"messages_after", nil},
		{"tokens_before", nil}, {"tokens_after", nil}, {"tokens_reclaimed", nil}, {"token_count_method", nil},
		{"tool_results_pruned", nil}, {"reasoning_items_pruned", nil}, {"has_focus_topic", false},
		{"prepared_summary", false},
	} {
		telemetry.Set(kv.k, kv.v)
	}
	t.Active, t.Last = telemetry, telemetry
	return telemetry
}

// Records the rough token size of the protected head, middle window and protected tail
func (t *CompressionTelemetry) RecordRegions(ctx context.Context, head, middle, tail []map[string]any) {
	if t.Active == nil {
		return
	}
	t.Active.Set("protected_head_tokens", estimateRowsRough(ctx, head))
	t.Active.Set("middle_window_tokens", estimateRowsRough(ctx, middle))
	t.Active.Set("protected_tail_tokens", estimateRowsRough(ctx, tail))
}

// AuxCompressionCall describes one summary call, empty strings and nil values standing for absent keywords
type AuxCompressionCall struct {
	PromptMessages      []map[string]any
	MaxTokens           any
	DurationMs          int
	AuxProvider         string
	AuxModel            string
	EffectiveAuxContext any
	PhaseTimings        map[string]any
}

// Records one summary call with its prompt size, fit margin and accumulated phase timings
func (t *CompressionTelemetry) RecordAuxCall(ctx context.Context, c AuxCompressionCall) {
	telemetry := t.Active
	if telemetry == nil {
		return
	}
	prompt := rowsToMessages(c.PromptMessages)
	telemetry.Set("aux_prompt_tokens", compactionEstimateRough(ctx, prompt))
	provider.ForgetMessageTokenMemo(prompt, nil)
	telemetry.Set("aux_output_reservation", telemetrySafeInt(c.MaxTokens))
	if c.AuxProvider != "" {
		telemetry.Set("aux_provider", c.AuxProvider)
	}
	if c.AuxModel != "" {
		telemetry.Set("aux_model", c.AuxModel)
	}
	if c.EffectiveAuxContext != nil {
		telemetry.Set("effective_aux_context", telemetrySafeInt(c.EffectiveAuxContext))
	}
	effective, _ := telemetry.Get("effective_aux_context")
	promptTokens, _ := telemetry.Get("aux_prompt_tokens")
	if effective != nil && promptTokens != nil {
		telemetry.Set("fit_margin", telemetryInt(telemetry, "effective_aux_context")-telemetryInt(telemetry, "aux_prompt_tokens")-
			telemetryInt(telemetry, "aux_output_reservation"))
	}
	telemetry.Set("aux_call_duration_ms", telemetryInt(telemetry, "aux_call_duration_ms")+max(0, c.DurationMs))
	for _, key := range []string{"queue_wait_ms", "prompt_build_ms", "time_to_first_progress_ms", "summary_generation_ms", "commit_ms"} {
		raw, ok := c.PhaseTimings[key]
		if c.PhaseTimings == nil || !ok {
			continue
		}
		value := telemetrySafeInt(raw)
		if (key == "queue_wait_ms" || key == "summary_generation_ms") && value != nil {
			telemetry.Set(key, telemetryInt(telemetry, key)+value.(int))
		} else {
			telemetry.Set(key, value)
		}
	}
}

// Records a summary answered by a fallback route and names the broken compression model for the main agent route
func (t *CompressionTelemetry) NoteAuxRouteFallback(route map[string]string) {
	failedModel := route["failed_model"]
	if failedModel == "" {
		return
	}
	if telemetry := t.Active; telemetry != nil {
		telemetry.Set("fallback_used", true)
		if v, _ := telemetry.Get("failure_class"); !pyrepr.Truthy(v) {
			telemetry.Set("failure_class", "aux_model_fallback")
		}
	}
	if strings.HasPrefix(route["fallback_label"], "main-agent(") {
		t.LastAuxModelFailureModel = failedModel
		failure := route["failure"]
		if failure == "" {
			failure = "unknown error"
		}
		if rs := []rune(failure); len(rs) > 220 {
			failure = pystr.Rstrip(string(rs[:217])) + "..."
		}
		t.LastAuxModelFailureError = failure
	}
}

// How the summary that replaced the middle window was produced
func (t *CompressionTelemetry) Method() string {
	if t.LastSummaryFallbackUsed {
		return "deterministic_fallback"
	}
	if t.LastAuxModelFailureError != "" {
		return "aux_fallback_main"
	}
	return "llm_summary"
}

// Records what a finished rewrite did with rough message only token counts before and after
func (t *CompressionTelemetry) RecordEffect(ctx context.Context, messagesBefore int, compressed []map[string]any, tokensBefore, toolResultsPruned, reasoningItemsPruned int) {
	telemetry := t.Active
	if telemetry == nil {
		return
	}
	tokensAfter := estimateRowsRough(ctx, compressed)
	telemetry.Set("method", t.Method())
	telemetry.Set("items_dropped", t.LastSummaryDroppedCount)
	telemetry.Set("messages_before", messagesBefore)
	telemetry.Set("messages_after", len(compressed))
	telemetry.Set("tokens_before", tokensBefore)
	telemetry.Set("tokens_after", tokensAfter)
	telemetry.Set("tokens_reclaimed", tokensBefore-tokensAfter)
	telemetry.Set("token_count_method", "estimate_rough")
	telemetry.Set("tool_results_pruned", toolResultsPruned)
	telemetry.Set("reasoning_items_pruned", reasoningItemsPruned)
}

// Replaces candidate counts with the exact transcript shape that crossed the commit boundary
func (t *CompressionTelemetry) RecordCommittedEffect(ctx context.Context, messagesBefore, messagesAfter []map[string]any) {
	telemetry := t.Active
	if telemetry == nil {
		return
	}
	tokensBefore, tokensAfter := estimateRowsRough(ctx, messagesBefore), estimateRowsRough(ctx, messagesAfter)
	telemetry.Set("messages_before", len(messagesBefore))
	telemetry.Set("messages_after", len(messagesAfter))
	telemetry.Set("tokens_before", tokensBefore)
	telemetry.Set("tokens_after", tokensAfter)
	telemetry.Set("tokens_reclaimed", tokensBefore-tokensAfter)
	telemetry.Set("token_count_method", "estimate_rough")
}
