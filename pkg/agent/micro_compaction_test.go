package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Micro compaction enabled compressor whose summarizer answers with the given text
func microCompressor(t *testing.T, summary string) *ContextCompressor {
	c := newCompressorWith(t, 40960, func(o *ContextCompressorOptions) {
		o.Model, o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN, o.Provider = "test-model", 0.75, 1, 2, "test"
		o.ConfigContextLength = iptr(40960)
	})
	c.microCompactEnabled = true
	patchMicroSummarizer(t, func(string) *string { s := summary; return &s })
	return c
}

func patchMicroSummarizer(t *testing.T, fn func(text string) *string) {
	prev := compressorMicroSummarizeOne
	compressorMicroSummarizeOne = func(_ *ContextCompressor, text string) (*string, error) { return fn(text), nil }
	t.Cleanup(func() { compressorMicroSummarizeOne = prev })
}

// System prompt then question and answer pairs
func microConversation(exchanges int) []map[string]any {
	msgs := []map[string]any{{"role": "system", "content": "system prompt"}}
	for i := range exchanges {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("question %d", i)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("answer %d ", i) + strings.Repeat("z", 400)})
	}
	return msgs
}

func summaryMarkers(msgs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range msgs {
		if isSummaryMarkerEntry(m) {
			out = append(out, m)
		}
	}
	return out
}

func mustMicro(t *testing.T, c *ContextCompressor, msgs []map[string]any) []map[string]any {
	t.Helper()
	out, err := c.microCompact(msgs, "")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func copyRows(msgs []map[string]any) []map[string]any { return append([]map[string]any(nil), msgs...) }

func anyContains(msgs []map[string]any, text string) bool {
	for _, m := range msgs {
		if strings.Contains(fmt.Sprint(m["content"]), text) {
			return true
		}
	}
	return false
}

func TestMicroAbsorbsOneExchangeAndLeavesASummaryMarker(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	messages := microConversation(6)
	result := mustMicro(t, c, copyRows(messages))
	markers := summaryMarkers(result)
	if !anyContains(messages, "answer 0") || anyContains(result, "answer 0") || len(markers) != 1 ||
		!strings.Contains(markers[0]["content"].(string), "ROLLING SUMMARY") || markers[0]["role"] != "assistant" {
		t.Fatalf("absorb failed: %d markers", len(markers))
	}
}

func TestMicroDisabledIsANoOp(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	c.microCompactEnabled = false
	messages := microConversation(6)
	if !sameRows(mustMicro(t, c, copyRows(messages)), messages) {
		t.Fatal("disabled pass changed the transcript")
	}
}

func TestMicroIsOffUnlessExplicitlyEnabled(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fresh := newCompressorWith(t, 40960, func(o *ContextCompressorOptions) {
		o.Model, o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN, o.Provider = "test-model", 0.75, 1, 2, "test"
		o.ConfigContextLength = iptr(40960)
	})
	patchMicroSummarizer(t, func(string) *string { s := "ROLLING SUMMARY"; return &s })
	messages := microConversation(6)
	if fresh.microCompactEnabled || !sameRows(mustMicro(t, fresh, copyRows(messages)), messages) {
		t.Fatal("micro compaction on by default")
	}
}

func TestMicroCadenceOfOneRunsEveryTurn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	c.microCompactEveryNTurns = 1
	first := mustMicro(t, c, microConversation(8))
	second := mustMicro(t, c, copyRows(first))
	if c.microCompactCursor <= 0 || len(summaryMarkers(first)) != 1 || len(second) >= len(first) {
		t.Fatalf("cadence one: cursor %d rows %d -> %d", c.microCompactCursor, len(first), len(second))
	}
}

func TestMicroCadenceSkipsTurnsUntilAPassIsDue(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	c.microCompactEveryNTurns = 3
	first := mustMicro(t, c, microConversation(8))
	second := mustMicro(t, c, copyRows(first))
	if len(summaryMarkers(first)) != 0 || len(summaryMarkers(second)) != 0 || c.microCompactCursor != 0 {
		t.Fatal("pass ran before it was due")
	}
	third := mustMicro(t, c, copyRows(second))
	if len(summaryMarkers(third)) != 1 || anyContains(third, "answer 0") || c.microCompactTurnsSincePass != 0 {
		t.Fatal("due pass did not run")
	}
}

func TestMicroCadenceIsClampedToAtLeastOne(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, bogus := range []int{0, -5} {
		c := microCompressor(t, "ROLLING SUMMARY")
		c.microCompactEveryNTurns = bogus
		if len(summaryMarkers(mustMicro(t, c, microConversation(8)))) != 1 {
			t.Fatalf("cadence %d did not run", bogus)
		}
	}
}

func TestMicroCursorAdvancesAcrossSuccessiveTurns(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	first := mustMicro(t, c, microConversation(8))
	after := c.microCompactCursor
	second := mustMicro(t, c, copyRows(first))
	if after <= 0 || c.microCompactCursor < after || len(summaryMarkers(second)) != 1 {
		t.Fatalf("cursor %d then %d", after, c.microCompactCursor)
	}
}

func TestMicroProtectedHeadAndTailSurvive(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	messages := microConversation(6)
	result := mustMicro(t, c, copyRows(messages))
	if fmt.Sprint(result[0]) != fmt.Sprint(messages[0]) || fmt.Sprint(result[len(result)-1]) != fmt.Sprint(messages[len(messages)-1]) {
		t.Fatal("protected head or tail touched")
	}
}

func TestMicroUserMessagesAreNeverAbsorbed(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	messages := microConversation(10)
	var originals []string
	for _, m := range messages {
		if m["role"] == "user" {
			originals = append(originals, m["content"].(string))
		}
	}
	for range 5 {
		messages = mustMicro(t, c, messages)
	}
	var surviving []string
	for _, m := range messages {
		if m["role"] == "user" && !isSummaryMarkerEntry(m) {
			surviving = append(surviving, m["content"].(string))
		}
	}
	joined := strings.Join(surviving, "\n\n")
	for _, o := range originals {
		if !strings.Contains(joined, o) {
			t.Fatalf("user text %q lost", o)
		}
	}
}

// Question rows each answered by an assistant with the given tool calls and tool bodies
func microToolConversation(rounds, calls, bodyChars int, followup bool) []map[string]any {
	msgs := []map[string]any{{"role": "system", "content": "sys"}}
	for i := range rounds {
		var tcs []any
		for j := range calls {
			tcs = append(tcs, map[string]any{"id": fmt.Sprintf("c%d-%d", i, j), "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("q%d", i)}, map[string]any{"role": "assistant", "content": fmt.Sprintf("a%d", i), "tool_calls": tcs})
		for j := range calls {
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("c%d-%d", i, j), "content": strings.Repeat("T", bodyChars)})
		}
		if followup {
			msgs = append(msgs, map[string]any{"role": "assistant", "content": fmt.Sprintf("followup %d ", i) + strings.Repeat("y", 200)})
		}
	}
	return msgs
}

func TestMicroCursorIsDerivedFromTheSplicedList(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	msgs := microToolConversation(8, 3, 500, false)
	for range 4 {
		msgs = mustMicro(t, c, msgs)
		idx := -1
		for i, m := range msgs {
			if isSummaryMarkerEntry(m) {
				idx = i
				break
			}
		}
		if c.microCompactCursor != idx+1 {
			t.Fatalf("cursor %d marker %d", c.microCompactCursor, idx)
		}
	}
}

func TestMicroResumeDoesNotDestroyTheAccumulatedSummary(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	msgs := microConversation(10)
	first := microCompressor(t, "IMPORTANT HISTORY: decisions and paths")
	for range 3 {
		msgs = mustMicro(t, first, msgs)
	}
	if !anyContains(summaryMarkers(msgs), "IMPORTANT HISTORY") {
		t.Fatal("history marker missing")
	}
	resumed := microCompressor(t, "MERGED: history plus newest exchange")
	if resumed.microCompactRollingSummary != "" {
		t.Fatal("fresh compressor carries a summary")
	}
	markers := summaryMarkers(mustMicro(t, resumed, msgs))
	if len(markers) != 1 || !strings.Contains(markers[0]["content"].(string), "MERGED") {
		t.Fatalf("resume markers %d", len(markers))
	}
}

func TestMicroResumeKeepsTheOldMarkerWhenRehydrationFails(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	msgs := microConversation(10)
	first := microCompressor(t, "IMPORTANT HISTORY: decisions and paths")
	for range 3 {
		msgs = mustMicro(t, first, msgs)
	}
	resumed := microCompressor(t, "BRAND NEW SUMMARY")
	prev := compressorRollingSummaryFromMarker
	compressorRollingSummaryFromMarker = func(any) string { return "" }
	t.Cleanup(func() { compressorRollingSummaryFromMarker = prev })
	markers := summaryMarkers(mustMicro(t, resumed, msgs))
	if len(markers) != 2 || !anyContains(markers, "IMPORTANT HISTORY") {
		t.Fatalf("un-carried history lost: %d markers", len(markers))
	}
}

func TestMicroRollingSummaryRoundTripsThroughAMarker(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	c.microCompactRollingSummary = "decisions: use the existing helper"
	marker := summaryMarkers(mustMicro(t, c, microConversation(6)))[0]
	if rollingSummaryFromMarker(marker["content"]) != c.microCompactRollingSummary {
		t.Fatal("rolling summary does not round trip")
	}
}

func TestMicroShortConversationIsUntouched(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	messages := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "hi"}, {"role": "assistant", "content": "hello"}}
	if !sameRows(mustMicro(t, c, copyRows(messages)), messages) {
		t.Fatal("short conversation changed")
	}
}

func TestMicroSummarizerFailureLeavesConversationIntact(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	patchMicroSummarizer(t, func(string) *string { return nil })
	messages := microConversation(6)
	if !sameRows(mustMicro(t, c, copyRows(messages)), messages) || c.microCompactConsecutiveFailures != 1 {
		t.Fatalf("failure changed the transcript or count %d", c.microCompactConsecutiveFailures)
	}
}

func TestMicroPoisonExchangeIsSkippedAfterRepeatedFailures(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	patchMicroSummarizer(t, func(string) *string { return nil })
	messages := microConversation(6)
	for range microCompactMaxConsecutiveFailures {
		mustMicro(t, c, copyRows(messages))
	}
	if c.microCompactCursor <= 0 || c.microCompactConsecutiveFailures != 0 {
		t.Fatalf("poison exchange not skipped: cursor %d", c.microCompactCursor)
	}
}

func TestMicroRepeatedCompactionShrinksContextAndKeepsOneMarker(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "")
	n := 0
	patchMicroSummarizer(t, func(string) *string {
		n++
		var parts []string
		for i := range n {
			parts = append(parts, fmt.Sprintf("ex%d", i))
		}
		s := "SUMMARY " + strings.Join(parts, " ")
		return &s
	})
	messages := microConversation(12)
	before := estimateRowsRough(context.Background(), messages)
	for range 6 {
		messages = mustMicro(t, c, messages)
	}
	if after := estimateRowsRough(context.Background(), messages); len(summaryMarkers(messages)) != 1 || after >= before {
		t.Fatalf("context grew: %d -> %d", before, after)
	}
}

// Captures info and above records of the default logger
func captureInfoLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// Decoded payloads of every micro compaction telemetry line in the captured log
func microTelemetryPayloads(t *testing.T, logs *bytes.Buffer) []*pyjson.Object {
	var out []*pyjson.Object
	for line := range strings.SplitSeq(logs.String(), "\n") {
		_, rest, ok := strings.Cut(line, "micro compaction telemetry: ")
		if !ok {
			continue
		}
		rest = strings.TrimSuffix(rest, `"`)
		rest = strings.ReplaceAll(rest, `\"`, `"`)
		v, err := pyjson.Loads(rest)
		if err != nil {
			t.Fatalf("telemetry line %q: %v", rest, err)
		}
		out = append(out, v.(*pyjson.Object))
	}
	return out
}

func objInt(o *pyjson.Object, key string) int {
	v, _ := o.Get(key)
	n, _ := telemetrySafeInt(v).(int)
	return n
}

func TestMicroEmitsContentFreeTokenTelemetry(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	logs := captureInfoLogs(t)
	result := mustMicro(t, c, microConversation(8))
	payloads := microTelemetryPayloads(t, logs)
	if len(payloads) != 1 {
		t.Fatalf("%d telemetry lines", len(payloads))
	}
	p := payloads[0]
	event, _ := p.Get("event")
	outcome, _ := p.Get("outcome")
	blob, _ := pyjson.Dumps(p)
	if event != "micro_compaction" || outcome != "absorbed" || objInt(p, "tokens_saved_total") != -objInt(p, "tokens_delta") ||
		objInt(p, "passes_total") != 1 || objInt(p, "messages_after") != len(result) || objInt(p, "exchange_tokens") <= 0 ||
		strings.Contains(blob, "answer 0") || strings.Contains(blob, "question 0") {
		t.Fatalf("telemetry payload %s", blob)
	}
}

func TestMicroEmitterNeverForcesWindowResolution(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	c.thresholdTokens, c.resolvedContextLength = nil, nil
	prev := compressorModelContextLength
	compressorModelContextLength = func(context.Context, provider.ContextLengthRequest) int {
		panic("telemetry forced context-length resolution")
	}
	t.Cleanup(func() { compressorModelContextLength = prev })
	logs := captureInfoLogs(t)
	c.emitMicroCompactionTelemetry(microTelemetry{outcome: "absorbed", messagesBefore: 10, messagesAfter: 9, tokensBefore: iptr(500), tokensAfter: iptr(400)})
	p := microTelemetryPayloads(t, logs)[0]
	occ, _ := p.Get("occupancy_pct")
	thr, _ := p.Get("threshold_tokens")
	if occ != nil || thr != nil {
		t.Fatalf("occupancy %v threshold %v", occ, thr)
	}
}

func TestMicroCumulativeSavingsAccumulateAcrossPasses(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	messages := microConversation(10)
	for range 6 {
		messages = mustMicro(t, c, messages)
	}
	if c.microCompactPasses != 6 || c.microCompactTokensSavedTotal <= 0 {
		t.Fatalf("passes %d saved %d", c.microCompactPasses, c.microCompactTokensSavedTotal)
	}
}

func TestMicroDefragTriggersOnceTheRollingSummaryGrows(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "FRESH DEFRAGGED SUMMARY")
	messages := mustMicro(t, c, microConversation(8))
	c.microCompactRollingSummary = strings.Repeat("x", 40_000)
	cursor := c.microCompactCursor
	var shape []any
	for _, m := range messages {
		shape = append(shape, m["role"])
	}
	if !c.needsDefrag() {
		t.Fatal("defrag not needed")
	}
	result := mustMicro(t, c, copyRows(messages))
	var after []any
	for _, m := range result {
		after = append(after, m["role"])
	}
	markers := summaryMarkers(result)
	if c.microCompactRollingSummary != "FRESH DEFRAGGED SUMMARY" || len(markers) != 1 || !strings.Contains(markers[0]["content"].(string), "FRESH DEFRAGGED SUMMARY") ||
		fmt.Sprint(after) != fmt.Sprint(shape) || c.microCompactCursor != cursor {
		t.Fatal("defrag changed the shape or missed the marker")
	}
}

func TestMicroDefragNeverAbsorbsUserMessages(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "DEFRAGGED")
	messages := []map[string]any{{"role": "system", "content": "sys"}}
	for i := range 10 {
		messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("UNIQUE-USER-PROMPT-%d", i)}, map[string]any{"role": "assistant", "content": fmt.Sprintf("answer %d ", i) + strings.Repeat("z", 400)})
	}
	c.microCompactRollingSummary = strings.Repeat("x", 40_000)
	result := mustMicro(t, c, copyRows(messages))
	for i := range 10 {
		if !anyContains(result, fmt.Sprintf("UNIQUE-USER-PROMPT-%d", i)) {
			t.Fatalf("user prompt %d absorbed", i)
		}
	}
}

func TestMicroDefragSummarizesOnlyTheSummaryText(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	captured := ""
	patchMicroSummarizer(t, func(text string) *string { captured = text; s := "DEFRAGGED"; return &s })
	c.microCompactRollingSummary = "OLD-SUMMARY " + strings.Repeat("x", 40_000)
	mustMicro(t, c, microConversation(8))
	if !strings.Contains(captured, "OLD-SUMMARY") || strings.Contains(captured, "[USER]") {
		t.Fatal("defrag serialized transcript turns")
	}
}

func TestMicroSplicedTranscriptHasNoConsecutiveSameRoleMessages(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	msgs := microToolConversation(8, 2, 400, true)
	for range 4 {
		msgs = mustMicro(t, c, msgs)
		for i := 1; i < len(msgs); i++ {
			if ra, rb := msgs[i-1]["role"], msgs[i]["role"]; ra == rb && (ra == "user" || ra == "assistant") {
				t.Fatalf("consecutive %v messages", ra)
			}
		}
	}
}

func TestMicroMarkerReportsNoUserProvenance(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	marker := summaryMarkers(mustMicro(t, c, microConversation(6)))[0]
	if v, ok := marker[state.CompressedSummaryHasUserTurnKey].(bool); !ok || v {
		t.Fatal("marker claims user provenance")
	}
}

func TestMicroSupersedeNeverDropsABatchCompactionMarker(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "MICRO SUMMARY (exchanges 1..k only)")
	msgs := mustMicro(t, c, microConversation(8))
	if c.microCompactRollingSummary == "" {
		t.Fatal("no rolling summary")
	}
	batch := map[string]any{"role": "user", "content": "[batch summary] CRITICAL HISTORY: exchanges 1..m", state.CompressedSummaryMetadataKey: true}
	idx := 0
	for i, m := range msgs {
		if isSummaryMarkerEntry(m) {
			idx = i
			break
		}
	}
	msgs = append(append(append([]map[string]any{}, msgs[:idx]...), batch), msgs[idx+3:]...)
	if !anyContains(mustMicro(t, c, msgs), "CRITICAL HISTORY") {
		t.Fatal("batch-compaction summary destroyed by micro supersede")
	}
}

func TestMicroDefragNeverRewritesABatchCompactionMarker(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "DEFRAGGED")
	msgs := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "[batch summary] CRITICAL HISTORY: exchanges 1..m", state.CompressedSummaryMetadataKey: true}}
	for i := range 6 {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("q%d", i)}, map[string]any{"role": "assistant", "content": fmt.Sprintf("a%d ", i) + strings.Repeat("z", 400)})
	}
	c.microCompactRollingSummary = strings.Repeat("x", 40_000)
	if !anyContains(mustMicro(t, c, copyRows(msgs)), "CRITICAL HISTORY") {
		t.Fatal("batch marker content overwritten by defrag")
	}
}

func TestMicroBatchCompressResetsMicroState(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	msgs := mustMicro(t, c, microConversation(8))
	if c.microCompactRollingSummary == "" || c.microCompactCursor <= 0 {
		t.Fatal("no micro state")
	}
	patchCallLLM(t, func(map[string]any) (any, error) { return nil, fmt.Errorf("no provider") })
	prev := compressorGenerateSummary
	compressorGenerateSummary = func(*ContextCompressor, context.Context, []map[string]any, *string, string, bool) (*string, error) {
		return nil, nil
	}
	t.Cleanup(func() { compressorGenerateSummary = prev })
	c.Compress(msgs, nil, nil, true, "")
	if c.microCompactRollingSummary != "" || c.microCompactCursor != 0 {
		t.Fatalf("micro state survived: %q %d", c.microCompactRollingSummary, c.microCompactCursor)
	}
}

// Store recording the archive and compact call options
type microCaptureDB struct {
	sessionID string
	messages  []map[string]any
	opts      state.ArchiveCompactOptions
}

func (d *microCaptureDB) ArchiveAndCompact(_ context.Context, sessionID string, compacted []map[string]any, o state.ArchiveCompactOptions) (int, error) {
	d.sessionID, d.messages, d.opts = sessionID, compacted, o
	return len(compacted), nil
}

func TestMicroDBSyncPassesExactCarriedMessages(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	db := &microCaptureDB{}
	c.sessionDB, c.sessionID = db, "sess"
	compacted := []map[string]any{
		{"role": "user", "content": "prefix", "_row_id": 11, dbPersistedMarker: true},
		{"role": "assistant", "content": "summary", state.CompressedSummaryMetadataKey: true},
		{"role": "user", "content": "suffix", "_row_id": 15, dbPersistedMarker: true},
		{"role": "assistant", "content": "rewritten", "_row_id": 16},
	}
	c.syncMicroCompactToDB(compacted, nil, nil)
	var ids []any
	for _, m := range db.opts.CarriedMessages {
		ids = append(ids, m["_row_id"])
	}
	if db.sessionID != "sess" || fmt.Sprint(ids) != "[11 15]" || db.opts.TailCount != 0 {
		t.Fatalf("carried %v tail %d", ids, db.opts.TailCount)
	}
}

func TestMicroSplicePreservesDBPersistedStamps(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	messages := microConversation(8)
	for _, m := range messages {
		m[dbPersistedMarker] = true
	}
	for _, m := range mustMicro(t, c, messages) {
		if m[dbPersistedMarker] != true && !isSummaryMarkerEntry(m) {
			t.Fatal("splice stripped a persisted stamp")
		}
	}
}

// Compressor after one pass with the marker stamped persisted and a defrag forced
func microDefragSetup(t *testing.T) (*ContextCompressor, []map[string]any) {
	c := microCompressor(t, "FRESH DEFRAGGED SUMMARY")
	messages := mustMicro(t, c, microConversation(8))
	for _, m := range messages {
		if isSummaryMarkerEntry(m) {
			m[dbPersistedMarker] = true
		}
	}
	c.microCompactRollingSummary = strings.Repeat("x", 40_000)
	return c, messages
}

func TestMicroDefragMarkerPopRaisesInvalidationFlag(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c, messages := microDefragSetup(t)
	if c.flushScanCursorInvalidated {
		t.Fatal("flag raised before defrag")
	}
	markers := summaryMarkers(mustMicro(t, c, copyRows(messages)))
	if _, stamped := markers[0][dbPersistedMarker]; len(markers) != 1 || stamped || !c.flushScanCursorInvalidated {
		t.Fatal("defrag kept the stamp or left the flag down")
	}
}

func TestMicroNoDefragNoFlag(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	mustMicro(t, c, microConversation(8))
	if c.flushScanCursorInvalidated {
		t.Fatal("flag raised without a defrag")
	}
}

func TestMicroMergePopsPersistedMarkerAndRaisesFlag(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	c.microCompactRollingSummary = "ROLLING"
	messages := []map[string]any{
		{"role": "user", "content": "first", dbPersistedMarker: true},
		{"role": "assistant", "content": "SUMMARY", state.CompressedSummaryMetadataKey: true, state.MicroCompactMarkerKey: true, dbPersistedMarker: true},
		{"role": "user", "content": "second", dbPersistedMarker: true},
		{"role": "assistant", "content": "answer", dbPersistedMarker: true},
		{"role": "user", "content": "third", dbPersistedMarker: true},
	}
	var users []map[string]any
	for _, m := range c.spliceMicroCompactResult(messages, 3, 5, true) {
		if m["role"] == "user" {
			users = append(users, m)
		}
	}
	if _, stamped := users[0][dbPersistedMarker]; len(users) != 1 || users[0]["content"] != "first\n\nsecond" || stamped || !c.flushScanCursorInvalidated {
		t.Fatalf("merge result %v", users)
	}
}
