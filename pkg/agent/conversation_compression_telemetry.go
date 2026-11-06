package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Caller abort verdicts that defer to the more precise compressor failure class of the same attempt
var compressionGenericAbortVerdicts = map[string]bool{"no_progress": true, "summary_generation_aborted": true}

// Rewritten transcript fields a refused or rolled back candidate never had
var compressionCandidateEffectFields = []string{"messages_after", "tokens_after", "tokens_reclaimed", "items_dropped"}

type compressionAttemptState struct {
	compressionAttemptID string
}

type compressionMetricOwnerKey struct{}

// Context naming the worker whose shared metric lifecycle a compression attempt opened
func withCompressionMetricOwner(ctx context.Context, owner any) context.Context {
	return context.WithValue(ctx, compressionMetricOwnerKey{}, owner)
}

// Owner the attempt lifecycle was opened under, false when none was opened on this worker
func compressionMetricOwner(ctx context.Context) (any, bool) {
	owner := ctx.Value(compressionMetricOwnerKey{})
	return owner, owner != nil
}

// Content only rejoin of the compressed messages and verbatim tail, nil when the snapshot fails
func snapshotCompressionEffectInput(messages, verbatimTail []map[string]any) (out []map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("failed to snapshot compression effect input", "error", r)
			out = nil
		}
	}()
	if len(verbatimTail) == 0 {
		return messages
	}
	tail := make([]map[string]any, len(verbatimTail))
	for i, m := range verbatimTail {
		tail[i], _ = pyjson.Clone(m).(map[string]any)
	}
	return provider.RejoinCompressedHeadAndTail(messages, tail)
}

// Finalizes the effect fields from the transcript shape that crossed the commit boundary
func recordCommittedAttemptEffect(ctx context.Context, a *AIAgent, messagesBefore, messagesAfter, verbatimTail []map[string]any, splitStatus string) {
	if messagesBefore == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("failed to record committed compression effect", "error", r)
		}
	}()
	if len(verbatimTail) > 0 && splitStatus == "not_applicable" {
		messagesAfter = provider.RejoinCompressedHeadAndTail(messagesAfter, verbatimTail)
	}
	if recorder, ok := a.contextCompressor.(interface {
		RecordCommittedEffect(ctx context.Context, messagesBefore, messagesAfter []map[string]any)
	}); ok {
		recorder.RecordCommittedEffect(ctx, messagesBefore, messagesAfter)
	}
}

// Attempt id, starting session and trigger, the attempt's own seed winning over the agent fields
func attemptSeed(a *AIAgent, attemptBegan bool, seed *pyjson.Object) *pyjson.Object {
	if seed != nil && seed.Len() > 0 {
		return seed.Copy()
	}
	attemptID := ""
	if attemptBegan {
		attemptID = a.compressionAttemptID
	}
	if attemptID == "" {
		attemptID = compactionNewAttemptID()
	}
	return pyjson.ObjectOf("attempt_id", attemptID, "session_id", a.sessionID, "trigger_source", "unknown")
}

// Reports whether both values are the same attempt id string
func sameAttemptID(a, b any) bool {
	as, ok := a.(string)
	bs, ok2 := b.(string)
	return ok && ok2 && as == bs
}

// Writes the one attempt log line the micro compaction report parses
func logAttemptRecord(payload *pyjson.Object) {
	text, err := pyjson.Dumps(payload, pyjson.SortKeys(true), pyjson.Separators(",", ":"))
	if err != nil {
		panic(err)
	}
	slog.Info("context compression attempt telemetry: " + text)
}

// attemptTelemetryEmit carries the keyword arguments of one attempt emit, nil pointers reading None
type attemptTelemetryEmit struct {
	StartedAt            time.Time
	CommitStatus         string
	SplitStatus          string
	FailureClass         string
	CommitStartedAt      *time.Time
	ExcludeLastTelemetry bool
	AttemptSeed          *pyjson.Object
	HistoryRewritten     *bool
}

// Milliseconds elapsed on the monotonic clock since the instant
func elapsedMillis(since time.Time) int { return int(time.Since(since) / time.Millisecond) }

// Emits one content free attempt record, publishes it and closes the shared metric lifecycle
func emitCompressionAttemptTelemetry(ctx context.Context, a *AIAgent, e attemptTelemetryEmit) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("failed to emit compression attempt telemetry: %v", r))
		}
	}()
	compressor, _ := a.contextCompressor.(*ContextCompressor)
	var telemetry *pyjson.Object
	if !e.ExcludeLastTelemetry && compressor != nil {
		telemetry = compressor.Last
	}
	seed := attemptSeed(a, !e.ExcludeLastTelemetry, e.AttemptSeed)
	seedID, _ := seed.Get("attempt_id")
	own := false
	if telemetry != nil {
		id, _ := telemetry.Get("attempt_id")
		own = sameAttemptID(id, seedID)
	}
	if !own {
		telemetry = seed.Copy()
		telemetry.Set("method", "none")
	}
	payload := telemetry.Copy()
	sessionID, _ := seed.Get("session_id")
	for _, kv := range []struct {
		key string
		v   any
	}{{"event", "compression_attempt"}, {"route", "hermes"}, {"attempt_id", seedID}, {"session_id", sessionID}} {
		if _, ok := payload.Get(kv.key); !ok {
			payload.Set(kv.key, kv.v)
		}
	}
	payload.Set("total_duration_ms", elapsedMillis(e.StartedAt))
	payload.Set("commit_status", e.CommitStatus)
	payload.Set("split_status", e.SplitStatus)
	if e.HistoryRewritten != nil {
		payload.Set("history_rewritten", *e.HistoryRewritten)
	}
	if e.CommitStatus != "committed" && (e.HistoryRewritten == nil || !*e.HistoryRewritten) {
		for _, key := range compressionCandidateEffectFields {
			payload.Delete(key)
		}
	}
	if e.CommitStartedAt != nil {
		commitMS := max(0, elapsedMillis(*e.CommitStartedAt))
		telemetry.Set("commit_ms", commitMS)
		payload.Set("commit_ms", commitMS)
	}
	prior, _ := payload.Get("failure_class")
	if e.FailureClass != "" && !(compressionGenericAbortVerdicts[e.FailureClass] && own && pyTruthy(prior)) {
		payload.Set("failure_class", e.FailureClass)
	}
	if _, ok := payload.Get("chunking"); !ok {
		payload.Set("chunking", false)
	}
	if _, ok := payload.Get("chunk_count"); !ok {
		payload.Set("chunk_count", 0)
	}
	fallback, _ := payload.Get("fallback_used")
	payload.Set("fallback_used", pyTruthy(fallback) || own && compressor != nil && (compressor.LastSummaryFallbackUsed || compressor.LastAuxModelFailureModel != ""))
	logAttemptRecord(payload)
	record, _ := pyjson.Plain(payload).(map[string]any)
	PublishCompactionAttempt(ctx, a, record)
	failureClass, _ := payload.Get("failure_class")
	var contextLength any
	if compressor != nil {
		contextLength = compressor.ContextLength()
	} else if a.contextCompressor != nil {
		contextLength = a.contextCompressor.Base().ContextLength
	}
	if owner, ok := compressionMetricOwner(ctx); ok {
		observability.FinishCompressionAttempt(ctx, owner, e.CommitStatus, failureClass, contextLength, a)
	}
	preparedSummary, _ := payload.Get("prepared_summary")
	spliced := pyTruthy(preparedSummary)
	if !spliced && compressor != nil {
		if prepared := compressor.PreparedCompactionState(); prepared != nil {
			splicedID := prepared.SplicedAttemptID()
			spliced = splicedID != nil && sameAttemptID(splicedID, seedID)
		}
	}
	if spliced && e.CommitStatus != "committed" {
		class, _ := failureClass.(string)
		observability.RecordPreparedCompaction(ctx, "compaction_failed", "", "", class, nil)
	}
}

// Emits the record of an attempt the caller aborted before it committed
func emitAbortedAttemptTelemetry(ctx context.Context, a *AIAgent, startedAt time.Time, failureClass string, seed *pyjson.Object) {
	emitCompressionAttemptTelemetry(ctx, a, attemptTelemetryEmit{StartedAt: startedAt, CommitStatus: "aborted", SplitStatus: "aborted", FailureClass: failureClass, AttemptSeed: seed})
}

// attemptBypass carries the keyword arguments of an attempt that never reached the local compressor
type attemptBypass struct {
	CommitStatus string
	FailureClass any
	ApproxTokens any
	Route        string
	Method       string
	AttemptSeed  *pyjson.Object
}

// Logs and publishes an attempt the gate, Codex or a lease holder took, then retires its metric lifecycle
func emitBypassedAttemptTelemetry(ctx context.Context, a *AIAgent, startedAt time.Time, b attemptBypass) {
	defer func() {
		if owner, ok := compressionMetricOwner(ctx); ok {
			observability.DiscardCompressionAttempt(owner)
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("failed to emit compression attempt telemetry: %v", r))
		}
	}()
	if b.Route == "" {
		b.Route = "hermes"
	}
	if b.Method == "" {
		b.Method = "none"
	}
	payload := pyjson.ObjectOf("event", "compression_attempt", "route", b.Route, "method", b.Method, "failure_class", b.FailureClass)
	payload.Update(attemptSeed(a, true, b.AttemptSeed))
	var contextLimit, threshold any
	if compressor, ok := a.contextCompressor.(*ContextCompressor); ok {
		if compressor.resolvedContextLength != nil {
			contextLimit = *compressor.resolvedContextLength
		}
		if compressor.thresholdTokens != nil {
			threshold = *compressor.thresholdTokens
		}
	}
	var current any
	switch n := b.ApproxTokens.(type) {
	case int, int64:
		current = n
	}
	for _, kv := range []struct {
		key string
		v   any
	}{{"main_provider", a.provider}, {"main_model", a.model}, {"main_context_limit", contextLimit}, {"effective_threshold", threshold},
		{"current_estimated_tokens", current}, {"total_duration_ms", elapsedMillis(startedAt)}, {"commit_status", b.CommitStatus},
		{"split_status", "not_applicable"}, {"fallback_used", false}} {
		payload.Set(kv.key, kv.v)
	}
	logAttemptRecord(payload)
	record, _ := pyjson.Plain(payload).(map[string]any)
	PublishCompactionAttempt(ctx, a, record)
}

// Records an automatic attempt the breaker gate refused, the class keeping only the guard name
func emitBlockedAttemptTelemetry(ctx context.Context, a *AIAgent, startedAt time.Time, approxTokens any, seed *pyjson.Object, reason string) {
	guard, _, _ := strings.Cut(reason, ":")
	emitBypassedAttemptTelemetry(ctx, a, startedAt, attemptBypass{CommitStatus: "blocked", FailureClass: "blocked:" + guard, ApproxTokens: approxTokens, AttemptSeed: seed})
}
