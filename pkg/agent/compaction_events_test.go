package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

type compactionMark struct {
	sessionID, name string
	data            map[string]any
}

// Installs a relay target and recorder for every mark plus a lifecycle with no post_compaction listener
func compactionPublished(t *testing.T) *[]compactionMark {
	t.Helper()
	marks := &[]compactionMark{}
	prevTarget, prevEmit, prevHas, prevInvoke := compactionTargetAvailable, compactionEmitMark, compactionHasHook, compactionInvokeHook
	t.Cleanup(func() {
		compactionTargetAvailable, compactionEmitMark, compactionHasHook, compactionInvokeHook = prevTarget, prevEmit, prevHas, prevInvoke
	})
	compactionTargetAvailable = func(context.Context, string, string) (bool, error) { return true, nil }
	compactionEmitMark = func(_ context.Context, sessionID, name string, data map[string]any, _ string) (bool, error) {
		*marks = append(*marks, compactionMark{sessionID, name, data})
		return true, nil
	}
	compactionHasHook = func(context.Context, string) bool { return false }
	compactionInvokeHook = func(context.Context, string, map[string]any) []any { return nil }
	return marks
}

func TestCompactionPayloadCarriesEveryV1Field(t *testing.T) {
	payload := CompactionAttemptPayload(map[string]any{"session_id": "s", "commit_status": "committed", "trigger_source": "pre_api",
		"method": "llm_summary", "tokens_before": 900, "tokens_after": 300, "aux_provider": "p", "aux_model": "m", "has_focus_topic": false}, 2)
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	want := append([]string(nil), CompactionFields...)
	sort.Strings(keys)
	sort.Strings(want)
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys %v", keys)
	}
	if CompactionEventName(payload) != CompactionMark || payload["harness"] != "icebath" || payload["kind"] != "summarize" ||
		payload["scope"] != "history" || payload["official"] != true || payload["trigger"] != "pre_api" || payload["trigger_class"] != "auto" ||
		payload["outcome"] != "committed" || payload["failure_class"] != "none" || payload["cache_break"] != true ||
		payload["compression_count"] != 2 || payload["summarizer_model"] != "m" || payload["has_focus_topic"] != false {
		t.Fatalf("payload %v", payload)
	}
	if id, _ := payload["attempt_id"].(string); len(id) != 32 {
		t.Fatalf("attempt id %q", payload["attempt_id"])
	}
}

func TestCompactionAttemptOutcomesAndTriggers(t *testing.T) {
	cases := []struct {
		record                               map[string]any
		outcome, failure, trigger, triggerCl string
		name                                 string
	}{
		{map[string]any{"commit_status": "aborted", "failure_class": "summary_generation_failed", "trigger_source": "auto"}, "failed", "summary_generation_failed", "unknown", "auto", CompactionAttemptMark},
		{map[string]any{"commit_status": "aborted", "failure_class": "would_grow", "trigger_source": "manual"}, "aborted", "would_grow", "manual", "manual", CompactionAttemptMark},
		{map[string]any{"commit_status": "aborted", "failure_class": "lock_contended", "trigger_source": "overflow", "overflow_reason": "payload_too_large"}, "skipped", "lock_contended", "overflow", "overflow", CompactionAttemptMark},
		{map[string]any{"commit_status": "blocked", "failure_class": "blocked:cooldown"}, "blocked", "blocked:cooldown", "unknown", "unknown", CompactionAttemptMark},
		{map[string]any{"commit_status": "aborted", "failure_class": "exception:PluginError", "history_rewritten": true, "split_status": "failed_not_indexed"}, "committed", "exception", "unknown", "unknown", CompactionMark},
		{map[string]any{"commit_status": "weird", "failure_class": "rollback:X", "trigger_source": "made_up"}, "other", "rollback", "other", "unknown", CompactionAttemptMark},
	}
	for i, c := range cases {
		p := CompactionAttemptPayload(c.record, nil)
		if p["outcome"] != c.outcome || p["failure_class"] != c.failure || p["trigger"] != c.trigger || p["trigger_class"] != c.triggerCl || CompactionEventName(p) != c.name {
			t.Fatalf("case %d payload %v", i, p)
		}
	}
	overflow := CompactionAttemptPayload(cases[2].record, nil)
	if overflow["overflow_reason"] != "payload_too_large" {
		t.Fatalf("overflow reason %v", overflow["overflow_reason"])
	}
	if CompactionFailureClass(nil) != "none" || CompactionFailureClass("nope") != "other" || CompactionFailureClass("summarize_failed") != "summarize_failed" {
		t.Fatal("failure class closing")
	}
}

func TestCompactionSplitModesAndCodexRoute(t *testing.T) {
	inPlace := CompactionAttemptPayload(map[string]any{"commit_status": "committed", "split_status": "in_place_committed"}, nil)
	rotated := CompactionAttemptPayload(map[string]any{"commit_status": "committed", "split_status": "rotated_committed"}, nil)
	failed := CompactionAttemptPayload(map[string]any{"commit_status": "committed", "split_status": "failed_not_indexed"}, nil)
	if inPlace["in_place"] != true || inPlace["session_rotated"] != false || rotated["in_place"] != false || rotated["session_rotated"] != true ||
		failed["in_place"] != nil || failed["session_rotated"] != nil || failed["split_status"] != "failed_not_indexed" {
		t.Fatalf("split modes %v %v %v", inPlace, rotated, failed)
	}
	codex := CompactionAttemptPayload(map[string]any{"route": "codex_app_server", "commit_status": "committed", "method": "provider"}, 1)
	if codex["kind"] != "provider_native" || codex["scope"] != "provider" || codex["cache_break"] != nil || CompactionEventName(codex) != CompactionMark {
		t.Fatalf("codex %v", codex)
	}
}

func TestCompactionMicroCompactionPassPublishesOneRecord(t *testing.T) {
	cases := []struct {
		outcome, name       string
		want0, want1, want2 string
	}{
		{"absorbed", "compaction", "committed", "llm_summary", "none"},
		{"defrag", "compaction", "committed", "llm_summary", "none"},
		{"stale_generation", "compaction.attempt", "skipped", "none", "stale_generation"},
		{"summarize_failed", "compaction.attempt", "failed", "none", "summarize_failed"},
	}
	for _, c := range cases {
		marks := compactionPublished(t)
		PublishMicroCompaction(context.Background(), map[string]any{"session_id": "micro-session", "outcome": c.outcome,
			"messages_before": 12, "messages_after": 10, "tokens_before": 900, "tokens_after": 600, "duration_ms": 5}, "")
		if len(*marks) != 1 {
			t.Fatalf("%s marks %v", c.outcome, *marks)
		}
		m := (*marks)[0]
		p := m.data
		if m.sessionID != "micro-session" || m.name != c.name || p["outcome"] != c.want0 || p["method"] != c.want1 || p["failure_class"] != c.want2 ||
			p["kind"] != "micro_summarize" || p["official"] != false || p["trigger"] != "between_turns" || p["tokens_reclaimed"] != 300 {
			t.Fatalf("%s mark %v", c.outcome, m)
		}
	}
}

func TestCompactionProviderNativeIsAnUnofficialProviderCompaction(t *testing.T) {
	marks := compactionPublished(t)
	a := &AIAgent{sessionID: "codex-session"}
	PublishProviderNativeCompaction(context.Background(), a)
	if len(*marks) != 1 {
		t.Fatalf("marks %v", *marks)
	}
	p := (*marks)[0].data
	if (*marks)[0].name != "compaction" || p["kind"] != "provider_native" || p["official"] != false || p["trigger"] != "provider" ||
		p["trigger_class"] != "provider" || p["compression_count"] != nil {
		t.Fatalf("payload %v", p)
	}
}

func TestCompactionCommittedProactivePrunePublishesOneRecord(t *testing.T) {
	marks := compactionPublished(t)
	prev := compactionEstimateRough
	t.Cleanup(func() { compactionEstimateRough = prev })
	compactionEstimateRough = func(_ context.Context, msgs []*state.Message) int { return 1000 * len(msgs) }
	a := &AIAgent{sessionID: "prune-session"}
	msgs := []*state.Message{{Role: "system"}, {Role: "tool"}, {Role: "tool"}}
	PublishCompactionPrune(context.Background(), a, msgs, msgs[:2], 2)
	if len(*marks) != 1 {
		t.Fatalf("marks %v", *marks)
	}
	m := (*marks)[0]
	p := m.data
	if m.sessionID != "prune-session" || m.name != "compaction" || p["kind"] != "prune_tool_results" || p["method"] != "deterministic_prune" ||
		p["trigger"] != "proactive_prune" || p["tool_results_pruned"] != 2 || p["tokens_reclaimed"] != 1000 ||
		p["token_count_method"] != "estimate_rough" || p["messages_before"] != 2 || p["messages_after"] != 2 {
		t.Fatalf("mark %v", m)
	}
}

func TestCompactionPruneWithoutRelayTargetSkipsTranscriptEstimates(t *testing.T) {
	compactionPublished(t)
	compactionTargetAvailable = func(context.Context, string, string) (bool, error) { return false, nil }
	prev := compactionEstimateRough
	t.Cleanup(func() { compactionEstimateRough = prev })
	compactionEstimateRough = func(context.Context, []*state.Message) int { t.Fatal("estimate called"); return 0 }
	msgs := []*state.Message{{Role: "user"}, {Role: "assistant"}}
	PublishCompactionPrune(context.Background(), &AIAgent{sessionID: "s"}, msgs, msgs[:1], 1)
}

func TestCompactionPersistenceDetachedForkPublishesNothing(t *testing.T) {
	marks := compactionPublished(t)
	a := &AIAgent{sessionID: "fork", persistDisabled: true}
	PublishCompactionAttempt(context.Background(), a, map[string]any{"session_id": "fork", "commit_status": "committed"})
	PublishCompactionPrune(context.Background(), a, nil, nil, 1)
	PublishProviderNativeCompaction(context.Background(), a)
	if len(*marks) != 0 {
		t.Fatalf("marks %v", *marks)
	}
}

func TestCompactionRotationFiresPostCompactionForTheLiveSession(t *testing.T) {
	marks := compactionPublished(t)
	var fired []map[string]any
	compactionHasHook = func(_ context.Context, name string) bool { return name == "post_compaction" }
	compactionInvokeHook = func(_ context.Context, name string, kw map[string]any) []any { fired = append(fired, kw); return nil }
	a := &AIAgent{sessionID: "child", inflightTurnSessionID: "parent"}
	PublishCompactionAttempt(context.Background(), a, map[string]any{"session_id": "parent", "commit_status": "committed",
		"trigger_source": "pre_api", "messages_before": 21, "messages_after": 5, "tokens_before": 800, "tokens_after": 100})
	if len(*marks) != 1 || (*marks)[0].sessionID != "parent" || len(fired) != 1 {
		t.Fatalf("marks %v fired %v", *marks, fired)
	}
	want := map[string]any{"session_id": "child", "old_session_id": "parent", "kind": "summarize", "reason": "pre_api",
		"before_tokens": 800, "after_tokens": 100, "messages_removed": 16}
	if !reflect.DeepEqual(fired[0], want) {
		t.Fatalf("fired %v", fired[0])
	}
	PublishCompactionAttempt(context.Background(), a, map[string]any{"session_id": "parent", "commit_status": "failed"})
	if len(fired) != 1 || len(*marks) != 2 || (*marks)[1].name != CompactionAttemptMark {
		t.Fatalf("attempt fired %v marks %v", fired, *marks)
	}
}

func TestCompactionARaisingEmitterNeverChangesTheResult(t *testing.T) {
	compactionPublished(t)
	compactionEmitMark = func(context.Context, string, string, map[string]any, string) (bool, error) {
		return false, errors.New("relay down")
	}
	compactionWarnedMu.Lock()
	compactionWarned = false
	compactionWarnedMu.Unlock()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	for range 2 {
		PublishMicroCompaction(context.Background(), map[string]any{"session_id": "s", "outcome": "absorbed"}, "")
	}
	compactionPublish(context.Background(), func() map[string]any { panic(errors.New("builder bug")) }, "", nil, "")
	var levels []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, `msg="compaction event publish failed"`) {
			levels = append(levels, line[strings.Index(line, "level=")+6:strings.Index(line, " msg=")])
		}
	}
	if !reflect.DeepEqual(levels, []string{"WARN", "DEBUG", "DEBUG"}) || !strings.Contains(logs.String(), "relay down") {
		t.Fatalf("levels %v logs %s", levels, logs.String())
	}
}
