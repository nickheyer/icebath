package provider

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// Chat chunk wire map with a delta, a finish reason and an optional prompt progress payload
func promptTestChunk(content any, finish any, progress map[string]any) map[string]any {
	delta := map[string]any{"role": "assistant", "content": content, "tool_calls": nil, "reasoning_content": nil, "reasoning": nil, "refusal": nil}
	chunk := map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	if progress != nil {
		chunk["prompt_progress"] = progress
	}
	return chunk
}

func TestPromptProgressHelpers(t *testing.T) {
	full := map[string]any{"total": 1000, "cache": 0, "processed": 250, "time_ms": 1}
	if got := promptProgress(promptTestChunk(nil, nil, full)); !reflect.DeepEqual(got, full) {
		t.Fatalf("progress_chunk_is_recognised %v", got)
	}
	wire := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"role": "assistant", "content": nil}, "finish_reason": nil}},
		"prompt_progress": map[string]any{"total": 10, "processed": 5}}
	if got := promptProgress(wire); !reflect.DeepEqual(got, map[string]any{"total": 10, "processed": 5}) {
		t.Fatalf("wire_dict_chunk_is_recognised %v", got)
	}
	if got := promptProgress(promptTestChunk("hi", nil, map[string]any{"total": 10, "processed": 10})); got != nil {
		t.Fatalf("chunk_with_text_is_a_real_chunk %v", got)
	}
	if got := promptProgress(promptTestChunk(nil, "stop", map[string]any{"total": 10, "processed": 10})); got != nil {
		t.Fatalf("chunk_with_finish_reason_is_a_real_chunk %v", got)
	}
	if got := promptProgress(promptTestChunk("hi", nil, nil)); got != nil {
		t.Fatalf("plain_chunk_has_no_progress %v", got)
	}
	kwargs := map[string]any{"model": "m", "extra_body": map[string]any{"cache_prompt": true}}
	out := WithPromptProgressKwargs(kwargs)
	if !reflect.DeepEqual(out["extra_body"], map[string]any{"cache_prompt": true, "return_progress": true}) ||
		!reflect.DeepEqual(kwargs["extra_body"], map[string]any{"cache_prompt": true}) {
		t.Fatalf("with_prompt_progress_merges_extra_body %v %v", out, kwargs)
	}
	if pct, ok := promptProgressPercent(map[string]any{"total": 200, "processed": 50}); !ok || pct != 25 {
		t.Fatalf("percent %d %v", pct, ok)
	}
	if _, ok := promptProgressPercent(map[string]any{"total": 0, "processed": 50}); ok {
		t.Fatal("zero total has no percent")
	}
	if _, ok := promptProgressPercent(map[string]any{"total": "x"}); ok {
		t.Fatal("non numeric total has no percent")
	}
}

func TestPromptReadingClockAndHook(t *testing.T) {
	start := time.Unix(1000, 0)
	now := start
	old := promptReadingNow
	t.Cleanup(func() { promptReadingNow = old })
	promptReadingNow = func() time.Time { return now }
	var heard []float64
	if CurrentPromptReadingHook(context.Background()) != nil {
		t.Fatal("no hook outside an installation")
	}
	ctx := PromptReadingHook(context.Background(), func(s float64) { heard = append(heard, s) })
	clock := NewPromptReadingClock(start)
	now = start.Add(5 * time.Second)
	if !clock.Tick(ctx, map[string]any{"processed": 100, "time_ms": 2000}) || clock.Seconds != 2 {
		t.Fatalf("server time caps the stretch, got %v", clock.Seconds)
	}
	now = now.Add(time.Second)
	if clock.Tick(ctx, map[string]any{"processed": 100, "time_ms": 9000}) {
		t.Fatal("no new tokens read gets no time")
	}
	now = now.Add(time.Second)
	if !clock.Tick(ctx, map[string]any{"processed": 200}) || clock.Seconds != 3 {
		t.Fatalf("without server time the wall stretch counts, got %v", clock.Seconds)
	}
	if clock.Tick(ctx, map[string]any{"total": 5}) {
		t.Fatal("a chunk without processed is not counted")
	}
	if !reflect.DeepEqual(heard, []float64{2, 1}) {
		t.Fatalf("hook heard %v", heard)
	}
	if CurrentPromptReadingHook(PromptReadingHook(ctx, nil)) != nil {
		t.Fatal("a nested nil hook uninstalls it")
	}
}

func TestEffectiveAuxTimeout(t *testing.T) {
	ctx := auxMainTestHome(t, "auxiliary:\n  compression:\n    timeout: 45\n  vision:\n    timeout: \"12.5\"\n    no_progress_timeout: 90\n  web_extract:\n    timeout: bogus\n    no_progress_timeout: -3\n")
	explicit := 7.0
	cases := []struct {
		task    string
		timeout *float64
		want    float64
	}{
		{"vision", &explicit, 7}, {"vision", nil, 12.5}, {"compression", nil, 300}, {"web_extract", nil, 30}, {"", nil, 30}, {"session_search", nil, 30},
	}
	for _, tc := range cases {
		if got := EffectiveAuxTimeout(ctx, tc.task, tc.timeout); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.task, got, tc.want)
		}
	}
	if got := auxGetTaskNoProgressTimeout(ctx, "vision"); got != 90 {
		t.Fatalf("no progress window %v", got)
	}
	if got := auxGetTaskNoProgressTimeout(ctx, "web_extract"); got != 0 {
		t.Fatalf("invalid window falls back to the default, got %v", got)
	}
}
