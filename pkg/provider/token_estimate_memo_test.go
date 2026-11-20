package provider

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const parityUnicode = "日本語テキスト🎉 café Ω ≈ 中文字符串"

// Empties the memo and restores its bounds, clock and compute seam after the test
func isolateTokenMemo(t *testing.T) {
	t.Helper()
	clearMessageTokenMemo()
	prevMax, prevBytes, prevCompute := msgTokensCacheMax, msgTokensCacheMaxBytes, messageTextTokens
	t.Cleanup(func() {
		msgTokensCacheMax, msgTokensCacheMaxBytes, messageTextTokens = prevMax, prevBytes, prevCompute
		msgTokensCache.mu.Lock()
		msgTokensCache.clock = time.Now
		msgTokensCache.mu.Unlock()
		clearMessageTokenMemo()
	})
}

// Counts the text estimates the memo computes on a miss
func countTextComputes(t *testing.T) *int {
	t.Helper()
	computed := 0
	real := messageTextTokens
	messageTextTokens = func(msg any) int {
		computed++
		return real(msg)
	}
	return &computed
}

// Memo clock held at a settable instant
func fakeMemoClock() *time.Time {
	now := time.Unix(1000, 0)
	msgTokensCache.mu.Lock()
	msgTokensCache.clock = func() time.Time { return now }
	msgTokensCache.mu.Unlock()
	return &now
}

func memoEntries() int {
	msgTokensCache.mu.Lock()
	defer msgTokensCache.mu.Unlock()
	return msgTokensCache.order.Len()
}

func shallowCopies(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		out[i] = maps.Clone(row)
	}
	return out
}

func tokenHistory(tag string, n, size int) []map[string]any {
	rows := make([]map[string]any, n)
	for i := range rows {
		role := "assistant"
		if i%2 == 1 {
			role = "user"
		}
		rows[i] = map[string]any{"role": role, "content": fmt.Sprintf("%s-%d-", tag, i) + strings.Repeat("x", size)}
	}
	return rows
}

func TestPreflightEstimateIgnoresReasoningDetails(t *testing.T) {
	isolateTokenMemo(t)
	base := map[string]any{"role": "assistant", "content": "hello there"}
	loaded := maps.Clone(base)
	loaded["reasoning_details"] = []any{map[string]any{"type": "thinking", "thinking": strings.Repeat("t", 10_000), "signature": strings.Repeat("s", 40_000)}}
	if delta := estimateMessageTokensWithoutImages(loaded) - estimateMessageTokensWithoutImages(base); delta >= 50 {
		t.Fatalf("the preflight estimator counted the reasoning_details payload, delta %d tokens", delta)
	}
}

func TestMessageTextIsEstimatedOnThePythonStrOfTheShadow(t *testing.T) {
	isolateTokenMemo(t)
	row := map[string]any{"role": "tool", "content": "ok", "tool_call_id": "c1", "n": int64(3), "ratio": 1.5, "flag": true, "none": nil}
	if got, want := estimateMessageTokensWithoutImages(row), EstimateTokens(pyrepr.Str(mapShadow(row))); got != want {
		t.Fatalf("got %d want %d", got, want)
	}
	if !strings.Contains(pyrepr.Str(mapShadow(row)), "True") || !strings.Contains(pyrepr.Str(mapShadow(row)), "None") {
		t.Fatal("the shadow renders as a Python dict")
	}
	if estimateMessageTokensWithoutImages("not-a-dict") != EstimateTokens("not-a-dict") {
		t.Fatal("a non dict row is estimated on its str")
	}
}

func TestShadowCarriesTheMultimodalSummaryAndCountsEveryImageCarrier(t *testing.T) {
	isolateTokenMemo(t)
	unconverted := map[string]any{"role": "tool", "content": map[string]any{"_multimodal": true, "text_summary": "a chart",
		"content": []any{map[string]any{"type": "image_url"}, map[string]any{"type": "image"}, map[string]any{"type": "input_image"}}}}
	if got := mapShadow(unconverted)["content"]; got != "a chart" {
		t.Fatalf("an unconverted multimodal result is priced by its summary, got %v", got)
	}
	if got := countImageParts(unconverted); got != 2 {
		t.Fatalf("only image and image_url count inside an unconverted result, got %d", got)
	}
	noSummary := map[string]any{"role": "tool", "content": map[string]any{"_multimodal": true}}
	if got := mapShadow(noSummary)["content"]; got != "" {
		t.Fatalf("a missing summary is the empty string, got %v", got)
	}
	sidecar := map[string]any{"role": "user", "api_content": "described", "content": []any{map[string]any{"type": "image_url"}},
		"_anthropic_content_blocks": []any{map[string]any{"type": "image"}, map[string]any{"type": "text"}},
		"output":                    []any{map[string]any{"type": "input_image"}}}
	if got := countImageParts(sidecar); got != 3 {
		t.Fatalf("content images count even when the sidecar wins, plus stashed blocks and output, got %d", got)
	}
	if countImageParts("raw") != 0 {
		t.Fatal("a non dict row carries no images")
	}
}

func buildParityHistory(rng *rand.Rand, n int) []map[string]any {
	var rows []map[string]any
	for i := 0; len(rows) < n; i++ {
		rows = append(rows, map[string]any{"role": "user", "content": fmt.Sprintf("question %d %s ", i, parityUnicode) + strings.Repeat("x", 10+rng.Intn(391))})
		if i%3 == 0 {
			raw, _ := json.Marshal(map[string]any{"q": fmt.Sprintf("val %d", i), "u": parityUnicode, "n": i})
			args := string(raw)
			if i%9 == 0 {
				args = `{"broken": tru`
			} else if i%6 == 0 {
				args = ""
			}
			rows = append(rows,
				map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": fmt.Sprintf("call_%d", i), "type": "function",
					"function": map[string]any{"name": "web_search", "arguments": args}}}},
				map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i), "name": "web_search", "content": fmt.Sprintf("result %d %s", i, parityUnicode)})
		} else {
			rows = append(rows, map[string]any{"role": "assistant", "content": fmt.Sprintf("answer %d ", i) + strings.Repeat("y", 10+rng.Intn(591)),
				"reasoning_content": fmt.Sprintf("thinking %d", i)})
		}
		if i%7 == 0 {
			rows[len(rows)-1]["content"] = []any{map[string]any{"type": "text", "text": fmt.Sprintf("part %d", i)},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}}
		}
	}
	return rows[:n]
}

// Reference estimate computed per row without the memo
func referenceRowsTokens[R any](rows []R) int {
	total := 0
	for _, row := range rows {
		total += estimateMessageTokensWithoutImages(any(row)) + countImageParts(any(row))*DefaultImageTokenCost
	}
	return total
}

// Middle of the history rewritten as fresh dict copies around a summary
func compressParityHistory(rows []map[string]any) []map[string]any {
	head, tail := rows[:2], rows[len(rows)-6:]
	out := shallowCopies(head)
	out = append(out, map[string]any{"role": "user", "content": "SUMMARY OF DROPPED CONTEXT " + parityUnicode})
	return append(out, shallowCopies(tail)...)
}

func TestParityTokenMemo(t *testing.T) {
	rng := rand.New(rand.NewSource(1234))
	for _, n := range []int{50, 200, 500} {
		isolateTokenMemo(t)
		rows := buildParityHistory(rng, n)
		for iteration := range 3 {
			api := shallowCopies(rows)
			if old, memoized := referenceRowsTokens(api), estimateRowsTokens(api, DefaultImageTokenCost); old != memoized {
				t.Fatalf("n=%d iteration %d reference %d memoized %d", n, iteration, old, memoized)
			}
			rows = append(rows, map[string]any{"role": "user", "content": fmt.Sprintf("followup %d %s", iteration, parityUnicode)})
			if iteration == 1 {
				rows = compressParityHistory(rows)
			}
		}
		rows[0]["content"] = "EDITED " + parityUnicode
		api := shallowCopies(rows)
		if referenceRowsTokens(api) != estimateRowsTokens(api, DefaultImageTokenCost) {
			t.Fatalf("n=%d an in place edit misses the memo", n)
		}
		weird := []any{
			map[string]any{"role": "user", "content": map[string]any{"_multimodal": true, "text_summary": "s"}},
			map[string]any{"role": "user", "content": nil}, "not-a-dict",
			map[string]any{"role": "tool", "content": []any{map[string]any{"type": "text", "text": parityUnicode}, "raw"}, "meta": pyrepr.Tuple{int64(1), int64(2)}},
		}
		for range 2 {
			if referenceRowsTokens(weird) != estimateRowsTokens(weird, DefaultImageTokenCost) {
				t.Fatalf("n=%d odd row types estimate the same through the memo", n)
			}
		}
	}
}

func TestStateMessagesEstimateThroughTheMemo(t *testing.T) {
	isolateTokenMemo(t)
	computed := countTextComputes(t)
	msgs := []*state.Message{{Role: "user", Content: "hello " + parityUnicode}, {Role: "assistant", Content: "hi", Reasoning: "plan", ReasoningContent: "plan"}}
	first := EstimateMessagesTokens(msgs, DefaultImageTokenCost, true)
	if *computed != 2 {
		t.Fatalf("each message computes once, got %d", *computed)
	}
	if again := EstimateMessagesTokens(msgs, DefaultImageTokenCost, true); again != first || *computed != 2 {
		t.Fatalf("a repeat estimate is served by the memo, got %d after %d computes", again, *computed)
	}
	want := 0
	for _, m := range msgs {
		row := messageRow(m)
		want += estimateMessageTokensWithoutImages(row) + countImageParts(row)*DefaultImageTokenCost
	}
	if first != want {
		t.Fatalf("the memo never changes the estimate, got %d want %d", first, want)
	}
}

func TestTwoLargeRunningConversationsBothKeepTheirMemoHits(t *testing.T) {
	isolateTokenMemo(t)
	computed := countTextComputes(t)
	first, second := tokenHistory("a", 2500, 200), tokenHistory("b", 2500, 200)
	expected := []int{estimateRowsTokens(first, DefaultImageTokenCost), estimateRowsTokens(second, DefaultImageTokenCost)}
	*computed = 0
	for range 3 {
		for i, history := range [][]map[string]any{first, second} {
			if got := estimateRowsTokens(shallowCopies(history), DefaultImageTokenCost); got != expected[i] {
				t.Fatalf("history %d estimate changed, got %d want %d", i, got, expected[i])
			}
		}
	}
	if *computed != 0 {
		t.Fatalf("interleaved running conversations never recompute, got %d", *computed)
	}
}

func TestDeadBodiesPastTheByteBudgetGoFirstAndEstimatesNeverChange(t *testing.T) {
	isolateTokenMemo(t)
	msgTokensCacheMaxBytes = 3_500_000
	now := fakeMemoClock()
	activeBody := "active " + strings.Repeat("y", 100_000)
	active := map[string]any{"role": "user", "content": activeBody}
	activeTokens := estimateRowsTokens([]map[string]any{active}, DefaultImageTokenCost)
	computed := countTextComputes(t)
	var deadBodies []string
	for i := range 10 {
		*now = now.Add(time.Hour)
		deadBodies = append(deadBodies, fmt.Sprintf("dead %d ", i)+strings.Repeat("z", 1_000_000))
		estimateRowsTokens([]map[string]any{{"role": "tool", "content": deadBodies[i]}}, DefaultImageTokenCost)
		*computed--
		if got := estimateRowsTokens([]map[string]any{maps.Clone(active)}, DefaultImageTokenCost); got != activeTokens {
			t.Fatalf("the running turn estimate changed, got %d want %d", got, activeTokens)
		}
	}
	if *computed != 0 {
		t.Fatalf("every active lookup was a hit, got %d recomputes", *computed)
	}
	*now = now.Add(time.Hour)
	estimateRowsTokens([]map[string]any{{"role": "tool", "content": "huge " + strings.Repeat("w", 4_000_000)}}, DefaultImageTokenCost)
	pinned := map[string]bool{}
	msgTokensCache.mu.Lock()
	for el := msgTokensCache.order.Front(); el != nil; el = el.Next() {
		for _, s := range el.Value.(*msgTokensItem).entry.pins.Strings {
			pinned[s] = true
		}
	}
	msgTokensCache.mu.Unlock()
	if !pinned[activeBody] {
		t.Fatal("the running conversation stays pinned")
	}
	kept := 0
	for _, body := range deadBodies {
		if pinned[body] {
			kept++
		}
	}
	if kept > 3 || !pinned[deadBodies[len(deadBodies)-1]] {
		t.Fatalf("dead bodies past the budget go first, %d kept", kept)
	}
}

func TestARunningConversationLargerThanBothBoundsNeverMissesThenShedsWhenIdle(t *testing.T) {
	isolateTokenMemo(t)
	msgTokensCacheMaxBytes, msgTokensCacheMax = 1_000_000, 1000
	now := fakeMemoClock()
	computed := countTextComputes(t)
	history := tokenHistory("big", 5000, 1000)
	expected := estimateRowsTokens(history, DefaultImageTokenCost)
	*computed = 0
	for range 3 {
		*now = now.Add(5 * time.Second)
		history = append(history, map[string]any{"role": "user", "content": fmt.Sprintf("turn %d ", len(history)) + strings.Repeat("q", 1000)})
		expected = estimateRowsTokens(shallowCopies(history), DefaultImageTokenCost)
	}
	if *computed != 3 {
		t.Fatalf("only each pass's new message is computed, got %d", *computed)
	}
	if got := estimateRowsTokens(shallowCopies(history), DefaultImageTokenCost); got != expected {
		t.Fatalf("got %d want %d", got, expected)
	}
	*now = now.Add(time.Hour)
	estimateRowsTokens([]map[string]any{{"role": "user", "content": "next " + strings.Repeat("w", 1000)}}, DefaultImageTokenCost)
	if memoEntries() > 1000 || RetainedBytes() > 1_000_000 {
		t.Fatalf("an idle conversation sheds back under the bounds, %d entries %d bytes", memoEntries(), RetainedBytes())
	}
}

func TestAHitRacingEvictionNeverMakesItDropADifferentActiveEntry(t *testing.T) {
	isolateTokenMemo(t)
	msgTokensCacheMax = 2
	for range 200 {
		clearMessageTokenMemo()
		now := fakeMemoClock()
		rememberMessageTokens("idle", messageTokenEntry{pins: &MemoPins{Strings: []string{"idle text"}}, text: 1})
		*now = now.Add(time.Hour)
		rememberMessageTokens("active", messageTokenEntry{pins: &MemoPins{Strings: []string{"active text"}}, text: 1})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			lookupMessageTokens("idle")
		}()
		go func() {
			defer wg.Done()
			rememberMessageTokens("new", messageTokenEntry{pins: &MemoPins{Strings: []string{"new text"}}, text: 1})
		}()
		wg.Wait()
		_, hasActive := lookupMessageTokens("active")
		_, hasNew := lookupMessageTokens("new")
		if !hasActive || !hasNew {
			t.Fatal("a concurrent hit on the evicted entry never drops the active or new entry")
		}
	}
}

func TestIdleEntriesNeverOutnumberTheMemoMainAlwaysHad(t *testing.T) {
	isolateTokenMemo(t)
	now := fakeMemoClock()
	for i := range 6000 {
		*now = now.Add(61 * time.Second)
		estimateRowsTokens([]map[string]any{{"role": "tool", "content": fmt.Sprintf("closed-%d ", i) + strings.Repeat("q", 300)}}, DefaultImageTokenCost)
	}
	if memoEntries() > 4096 {
		t.Fatalf("idle entries stay within 4096, got %d", memoEntries())
	}
}

func TestEstimateRowsTokensPricesTheDurabilityMarkerOfADictRow(t *testing.T) {
	plain := map[string]any{"role": "user", "content": "Audit the auth module."}
	marked := map[string]any{"role": "user", "content": "Audit the auth module.", "_db_persisted": true, "_row_id": 7, "timestamp": 1.5}
	if got := EstimateRowsTokens([]map[string]any{plain}, DefaultImageTokenCost); got != 14 {
		t.Fatalf("plain row = %d, want 14", got)
	}
	if got := EstimateRowsTokens([]map[string]any{marked}, DefaultImageTokenCost); got != 19 {
		t.Fatalf("marked row = %d, want 19", got)
	}
}
