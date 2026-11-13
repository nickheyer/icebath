package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	CompactionSchemaName    = "icebath.compaction"
	CompactionSchemaVersion = "1"
	CompactionMark          = "compaction"
	CompactionAttemptMark   = "compaction.attempt"
)

var (
	compactionKinds              = observability.Set("summarize", "micro_summarize", "prune_tool_results", "provider_native")
	compactionScopes             = observability.Set("history", "provider")
	compactionMethods            = observability.Set("llm_summary", "aux_fallback_main", "deterministic_fallback", "deterministic_prune", "provider", "none")
	compactionTriggers           = observability.Set("manual", "turn_start_threshold", "pre_api", "post_tool", "idle", "engine_preflight", "overflow", "gateway_hygiene", "between_turns", "proactive_prune", "provider", "unknown")
	compactionTriggerClasses     = map[string]string{"manual": "manual", "overflow": "overflow", "provider": "provider", "unknown": "unknown", "other": "unknown"}
	compactionTriggerClassValues = observability.Set("auto", "manual", "overflow", "provider", "unknown")
	compactionOverflowReasons    = observability.Set("context_overflow", "payload_too_large", "long_context_tier")
	compactionOutcomes           = observability.Set("committed", "aborted", "failed", "skipped", "blocked")
	compactionSplitStatuses      = observability.Set("not_applicable", "in_place_committed", "rotated_committed", "failed_not_indexed", "aborted")
	compactionTokenCountMethods  = observability.Set("estimate_rough")
	compactionExtraFailures      = observability.Set("blocked:cooldown", "blocked:structural_backoff", "blocked:ineffective", "blocked:unknown",
		"codex_auto_native", "codex_auto_off", "codex_no_thread", "codex_compaction_failed",
		"session_ownership_lost", "session_ownership_unreadable", "cooldown_state_unreadable", "empty_transcript",
		"summary_model_benched",
		"summarize_failed", "exchange_skipped", "defrag_failed", "stale_generation", "watermark_unavailable")
	compactionAbortedClasses = observability.Set("would_grow", "no_progress", "commit_fence_cancelled")
	compactionCommitModes    = observability.Set("in_place_committed", "rotated_committed")
	compactionMicroOutcomes  = map[string]string{"absorbed": "committed", "defrag": "committed", "stale_generation": "skipped", "watermark_unavailable": "skipped"}
	compactionFailureClasses = observability.CompressionFailureClasses.Union(compactionExtraFailures)
)

// CompactionFields is every v1 key in schema order, so consumers see a stable shape
var CompactionFields = []string{
	"harness", "kind", "scope", "official", "method", "trigger", "trigger_class", "overflow_reason", "outcome",
	"failure_class", "attempt_id", "session_id", "tokens_before", "tokens_after", "tokens_reclaimed",
	"token_count_method", "messages_before", "messages_after", "items_dropped", "context_limit", "threshold_tokens",
	"protected_head_tokens", "middle_window_tokens", "protected_tail_tokens", "duration_ms",
	"summary_generation_ms", "aux_call_duration_ms", "queue_wait_ms", "commit_ms", "summarizer_provider",
	"summarizer_model", "tool_results_pruned", "reasoning_items_pruned", "summary_input_omitted_chars",
	"compression_count", "in_place", "session_rotated", "split_status", "cache_break", "has_focus_topic",
}

// Integer attempt record keys copied as is, record key first and payload key second
var compactionAttemptInts = [][2]string{
	{"tokens_before", "tokens_before"}, {"tokens_after", "tokens_after"}, {"tokens_reclaimed", "tokens_reclaimed"},
	{"messages_before", "messages_before"}, {"messages_after", "messages_after"}, {"items_dropped", "items_dropped"},
	{"main_context_limit", "context_limit"}, {"effective_threshold", "threshold_tokens"},
	{"protected_head_tokens", "protected_head_tokens"}, {"middle_window_tokens", "middle_window_tokens"},
	{"protected_tail_tokens", "protected_tail_tokens"}, {"total_duration_ms", "duration_ms"},
	{"summary_generation_ms", "summary_generation_ms"}, {"aux_call_duration_ms", "aux_call_duration_ms"},
	{"queue_wait_ms", "queue_wait_ms"}, {"commit_ms", "commit_ms"}, {"tool_results_pruned", "tool_results_pruned"},
	{"reasoning_items_pruned", "reasoning_items_pruned"}, {"summary_input_omitted_chars", "summary_input_omitted_chars"},
}

// Seams over the lifecycle dispatch, the relay emitter and the first failure latch the publish path uses
var (
	compactionHasHook         = lifecycleHasHook
	compactionInvokeHook      = lifecycleInvokeHook
	compactionTargetAvailable = CompactionTargetAvailable
	compactionEmitMark        = EmitCompactionMark
	compactionNewAttemptID    = func() string { return fmt.Sprintf("%x", [16]byte(uuid.New())) }
	compactionWarnedMu        sync.Mutex
	compactionWarned          bool
)

// The value when it is a non empty string in allowed, def for other strings, nil for anything else
func compactionClosed(value any, allowed observability.ValueSet, def any) any {
	s, ok := value.(string)
	if !ok || s == "" {
		return nil
	}
	if allowed.Has(s) {
		return s
	}
	return def
}

// The value as an int when it is an integer other than a bool, nil otherwise
func compactionInt(value any) any {
	switch v := value.(type) {
	case int:
		return v
	case int8:
		return int(v)
	case int16:
		return int(v)
	case int32:
		return int(v)
	case int64:
		return int(v)
	case uint8:
		return int(v)
	case uint16:
		return int(v)
	case uint32:
		return int(v)
	case uint64:
		return int(v)
	case uint:
		return int(v)
	}
	return nil
}

// Closed failure classes of a compaction event, the shared metrics contract set joined with the compaction extras
func compactionFailureClassSet() observability.ValueSet { return compactionFailureClasses }

// Closed skip classes of a compaction event, the shared metrics contract set
func compactionSkipClassSet() observability.ValueSet { return observability.CompressionSkipClasses }

// CompactionFailureClass closes a failure class, exception and rollback keep only their prefix, a missing class is none
func CompactionFailureClass(value any) string {
	s, ok := value.(string)
	if !ok || s == "" {
		return "none"
	}
	for _, prefix := range []string{"exception:", "rollback:"} {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			s = prefix[:len(prefix)-1]
			break
		}
	}
	if compactionFailureClassSet().Has(s) {
		return s
	}
	return "other"
}

// CompactionEventName is the reserved compaction mark only for a committed history or provider rewrite
func CompactionEventName(payload map[string]any) string {
	scope, _ := payload["scope"].(string)
	if outcome, _ := payload["outcome"].(string); outcome == "committed" && (scope == "history" || scope == "provider") {
		return CompactionMark
	}
	return CompactionAttemptMark
}

// Builds the full v1 record from values, closing every categorical field and defaulting the identifiers
func compactionPayload(values map[string]any) map[string]any {
	p := make(map[string]any, len(CompactionFields))
	for _, k := range CompactionFields {
		p[k] = nil
	}
	for k, v := range values {
		p[k] = v
	}
	p["harness"] = "icebath"
	p["kind"] = compactionClosed(p["kind"], compactionKinds, "other")
	p["scope"] = compactionClosed(p["scope"], compactionScopes, "other")
	p["method"] = orDefault(compactionClosed(p["method"], compactionMethods, "other"), "none")
	p["trigger"] = orDefault(compactionClosed(p["trigger"], compactionTriggers, "other"), "unknown")
	if tc := compactionClosed(p["trigger_class"], compactionTriggerClassValues, nil); tc != nil {
		p["trigger_class"] = tc
	} else if cls, ok := compactionTriggerClasses[p["trigger"].(string)]; ok {
		p["trigger_class"] = cls
	} else {
		p["trigger_class"] = "auto"
	}
	p["outcome"] = orDefault(compactionClosed(p["outcome"], compactionOutcomes, "other"), "other")
	p["token_count_method"] = compactionClosed(p["token_count_method"], compactionTokenCountMethods, "other")
	if !config.PyTruthy(p["attempt_id"]) {
		p["attempt_id"] = compactionNewAttemptID()
	}
	if !config.PyTruthy(p["session_id"]) {
		p["session_id"] = ""
	}
	if p["cache_break"] == nil && p["scope"] == "history" {
		p["cache_break"] = p["outcome"] == "committed"
	}
	return p
}

// Returns def when v is nil, v otherwise
func orDefault(v any, def string) any {
	if v == nil {
		return def
	}
	return v
}

// Returns v when it is truthy, nil otherwise
func truthyOrNil(v any) any {
	if config.PyTruthy(v) {
		return v
	}
	return nil
}

// Committed when history was rewritten, else the commit status, an aborted attempt split into skipped, aborted or failed
func compactionAttemptOutcome(commitStatus any, failure string, historyRewritten any) string {
	if b, ok := historyRewritten.(bool); ok && b {
		return "committed"
	}
	if s, ok := commitStatus.(string); !ok || s != "aborted" {
		if ok && compactionOutcomes.Has(s) {
			return s
		}
		return "other"
	}
	if compactionSkipClassSet().Has(failure) {
		return "skipped"
	}
	if compactionAbortedClasses.Has(failure) {
		return "aborted"
	}
	return "failed"
}

// The trigger and trigger class, an unlabelled automatic caller is an unknown trigger of the auto class
func compactionAttemptTrigger(triggerSource any) (any, any) {
	if triggerSource == "auto" {
		return "unknown", "auto"
	}
	if config.PyTruthy(triggerSource) {
		return triggerSource, nil
	}
	return "unknown", nil
}

// CompactionAttemptPayload maps one attempt telemetry record onto the v1 schema
func CompactionAttemptPayload(record map[string]any, compressionCount any) map[string]any {
	codex := record["route"] == "codex_app_server"
	failure := CompactionFailureClass(record["failure_class"])
	splitStatus := compactionClosed(record["split_status"], compactionSplitStatuses, "other")
	outcome := compactionAttemptOutcome(record["commit_status"], failure, record["history_rewritten"])
	trigger, triggerClass := compactionAttemptTrigger(record["trigger_source"])
	values := map[string]any{}
	for _, kv := range compactionAttemptInts {
		values[kv[1]] = compactionInt(record[kv[0]])
	}
	kind, scope := "summarize", "history"
	if codex {
		kind, scope = "provider_native", "provider"
	}
	var inPlace, rotated any
	if s, ok := splitStatus.(string); ok && compactionCommitModes.Has(s) {
		inPlace, rotated = s == "in_place_committed", s == "rotated_committed"
	}
	var focus any
	if b, ok := record["has_focus_topic"].(bool); ok {
		focus = b
	}
	for k, v := range map[string]any{
		"kind": kind, "scope": scope, "official": true, "method": record["method"], "trigger": trigger,
		"trigger_class": triggerClass, "overflow_reason": compactionClosed(record["overflow_reason"], compactionOverflowReasons, "other"),
		"outcome": outcome, "failure_class": failure, "attempt_id": record["attempt_id"], "session_id": record["session_id"],
		"token_count_method": record["token_count_method"], "summarizer_provider": truthyOrNil(record["aux_provider"]),
		"summarizer_model": truthyOrNil(record["aux_model"]), "compression_count": compactionInt(compressionCount),
		"split_status": splitStatus, "in_place": inPlace, "session_rotated": rotated, "has_focus_topic": focus,
	} {
		values[k] = v
	}
	if codex {
		values["cache_break"] = nil
	}
	return compactionPayload(values)
}

// Difference of two optional ints, nil when either is missing
func compactionDelta(before, after any) any {
	b, ok1 := before.(int)
	a, ok2 := after.(int)
	if !ok1 || !ok2 {
		return nil
	}
	return b - a
}

// MicroCompactionPayload maps one micro compaction telemetry record onto the v1 schema
func MicroCompactionPayload(record map[string]any) map[string]any {
	outcome := "failed"
	if s, ok := record["outcome"].(string); ok {
		if o, ok := compactionMicroOutcomes[s]; ok {
			outcome = o
		}
	}
	method, failure := "none", CompactionFailureClass(record["outcome"])
	if outcome == "committed" {
		method, failure = "llm_summary", "none"
	}
	before, after := compactionInt(record["tokens_before"]), compactionInt(record["tokens_after"])
	return compactionPayload(map[string]any{
		"kind": "micro_summarize", "scope": "history", "official": false, "trigger": "between_turns", "outcome": outcome,
		"method": method, "failure_class": failure, "session_id": record["session_id"], "tokens_before": before,
		"tokens_after": after, "tokens_reclaimed": compactionDelta(before, after), "token_count_method": "estimate_rough",
		"messages_before": compactionInt(record["messages_before"]), "messages_after": compactionInt(record["messages_after"]),
		"duration_ms": compactionInt(record["duration_ms"]), "threshold_tokens": compactionInt(record["threshold_tokens"]),
		"context_limit": compactionInt(record["context_limit"]), "summarizer_model": truthyOrNil(record["aux_model"]),
	})
}

// PruneCompactionPayload is a committed proactive tool result prune, deterministic with the message count unchanged
func PruneCompactionPayload(sessionID string, tokensBefore, tokensAfter, messages, toolResultsPruned int) map[string]any {
	return compactionPayload(map[string]any{
		"kind": "prune_tool_results", "scope": "history", "official": false, "trigger": "proactive_prune", "outcome": "committed",
		"method": "deterministic_prune", "failure_class": "none", "session_id": sessionID, "tokens_before": tokensBefore,
		"tokens_after": tokensAfter, "tokens_reclaimed": tokensBefore - tokensAfter, "token_count_method": "estimate_rough",
		"messages_before": messages, "messages_after": messages, "tool_results_pruned": toolResultsPruned,
	})
}

// ProviderNativeCompactionPayload is a compaction the provider ran on its own thread and the agent only observed
func ProviderNativeCompactionPayload(sessionID string, compressionCount any) map[string]any {
	return compactionPayload(map[string]any{
		"kind": "provider_native", "scope": "provider", "official": false, "trigger": "provider", "outcome": "committed",
		"method": "provider", "failure_class": "none", "session_id": sessionID, "compression_count": compactionInt(compressionCount),
	})
}

// FirePostCompaction invokes the post_compaction plugin hook when a listener is registered
func FirePostCompaction(ctx context.Context, sessionID, oldSessionID string, kind, reason, beforeTokens, afterTokens, messagesRemoved any) {
	if compactionHasHook(ctx, "post_compaction") {
		compactionInvokeHook(ctx, "post_compaction", map[string]any{
			"session_id": sessionID, "old_session_id": oldSessionID, "kind": kind, "reason": reason,
			"before_tokens": beforeTokens, "after_tokens": afterTokens, "messages_removed": messagesRemoved,
		})
	}
}

// Fires post_compaction for a committed record, naming the record's session as old when the transcript moved
func compactionFireFor(ctx context.Context, payload map[string]any, sessionID string) {
	old := ""
	if s, _ := payload["session_id"].(string); s != sessionID {
		old = s
	}
	FirePostCompaction(ctx, sessionID, old, payload["kind"], payload["trigger"], payload["tokens_before"],
		payload["tokens_after"], compactionDelta(payload["messages_before"], payload["messages_after"]))
}

// Builds one record, fires post_compaction for a committed one and emits the relay mark, never failing compaction
func compactionPublish(ctx context.Context, build func() map[string]any, turnSessionID string, requireTargetSessionID *string, liveSessionID string) {
	var failure any
	defer func() {
		if r := recover(); r != nil {
			switch r.(type) {
			case plugins.UserInterrupt, *plugins.UserInterrupt:
				panic(r)
			}
			failure = r
		}
		if failure == nil {
			return
		}
		compactionWarnedMu.Lock()
		first := !compactionWarned
		compactionWarned = true
		compactionWarnedMu.Unlock()
		if first {
			slog.Warn("compaction event publish failed", "err", failure)
		} else {
			slog.Debug("compaction event publish failed", "err", failure)
		}
	}()
	relay := true
	if requireTargetSessionID != nil {
		ok, err := compactionTargetAvailable(ctx, *requireTargetSessionID, turnSessionID)
		if err != nil {
			failure = err
			return
		}
		relay = ok
	}
	hook := compactionHasHook(ctx, "post_compaction")
	if !relay && !hook {
		return
	}
	payload := build()
	name := CompactionEventName(payload)
	sessionID, _ := payload["session_id"].(string)
	if hook && name == CompactionMark {
		live := liveSessionID
		if live == "" {
			live = sessionID
		}
		compactionFireFor(ctx, payload, live)
	}
	if !relay {
		return
	}
	if _, err := compactionEmitMark(ctx, sessionID, name, payload, turnSessionID); err != nil {
		failure = err
	}
}

// Reports whether the agent publishes, a persistence detached fork rewrites only its own transcript
func compactionPublishesFor(a *AIAgent) bool { return !a.persistDisabled }

// The context engine's compression count, nil without an engine
func compactionCount(a *AIAgent) any {
	if a.contextCompressor == nil {
		return nil
	}
	return a.contextCompressor.Base().CompressionCount
}

// Session the current turn started in, empty outside a turn since a rotating commit moves the live session off it
func compactionTurnSessionID(a *AIAgent) string { return a.inflightTurnSessionID }

// PublishCompactionAttempt publishes one compress_context attempt record from the attempt telemetry seam
func PublishCompactionAttempt(ctx context.Context, a *AIAgent, record map[string]any) {
	if compactionPublishesFor(a) {
		count := compactionCount(a)
		compactionPublish(ctx, func() map[string]any { return CompactionAttemptPayload(record, count) }, compactionTurnSessionID(a), nil, a.sessionID)
	}
}

// PublishMicroCompaction publishes one micro compaction pass record for the session its turn started in
func PublishMicroCompaction(ctx context.Context, record map[string]any, turnSessionID string) {
	compactionPublish(ctx, func() map[string]any { return MicroCompactionPayload(record) }, turnSessionID, nil, "")
}

// PublishCompactionPrune publishes one committed proactive tool result prune measured by the rough token estimate
func PublishCompactionPrune(ctx context.Context, a *AIAgent, messages, pruned []*state.Message, toolResultsPruned int) {
	sessionID, turnSessionID := a.sessionID, compactionTurnSessionID(a)
	if compactionPublishesFor(a) {
		compactionPublish(ctx, func() map[string]any {
			return PruneCompactionPayload(sessionID, compactionEstimateRough(ctx, messages), compactionEstimateRough(ctx, pruned), len(pruned), toolResultsPruned)
		}, turnSessionID, &sessionID, "")
	}
}

// Rough token estimate of a message list at the learned per image price
var compactionEstimateRough = func(ctx context.Context, msgs []*state.Message) int {
	return provider.EstimateMessagesTokens(msgs, provider.CurrentImageTokenCost(ctx), true)
}

// PublishProviderNativeCompaction publishes one provider native compaction the agent observed but did not run
func PublishProviderNativeCompaction(ctx context.Context, a *AIAgent) {
	if compactionPublishesFor(a) {
		sessionID, count := a.sessionID, compactionCount(a)
		compactionPublish(ctx, func() map[string]any { return ProviderNativeCompactionPayload(sessionID, count) }, compactionTurnSessionID(a), nil, "")
	}
}
