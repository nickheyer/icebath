package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Long decode stream event with an optional token and finish
func gemLDEvent(text, token, finish string) string {
	cand := map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}}}
	if token != "" {
		cand["continuationToken"] = token
	}
	if finish != "" {
		cand["finishReason"] = finish
	}
	data, _ := json.Marshal(map[string]any{"candidates": []any{cand}})
	return string(data)
}

// Fake streaming endpoint replaying one script per request, DROP cutting the connection mid body
func gemScriptedStream(t *testing.T, streams [][]string) (*geminiNativeClient, *gemFake) {
	f := newGemFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		script := streams[min(n, len(streams)-1)]
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, item := range script {
			if item == "DROP" {
				w.(http.Flusher).Flush()
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", item)
		}
	})
	return gemClient(t, f, "https://generativelanguage.googleapis.com/v1beta", "AQ.test", "gemini-4-argon"), f
}

// Streams a request collecting text and finish reasons
func gemCollect(c *geminiNativeClient, req *Request) (string, *Response, error) {
	var text strings.Builder
	resp, err := c.Stream(context.Background(), req, func(ev StreamEvent) error {
		if ev.Kind == EventTextDelta {
			text.WriteString(ev.Text)
		}
		return nil
	})
	return text.String(), resp, err
}

func TestGeminiDroppedLongDecodeStreamResumesFromItsLastCheckpoint(t *testing.T) {
	c, f := gemScriptedStream(t, [][]string{{gemLDEvent("1\n", "", ""), gemLDEvent("2\n", "ckpt-2", ""), "DROP"}, {gemLDEvent("3\n", "", "STOP")}})
	text, resp, err := gemCollect(c, gemUserRequest("gemini-4-argon", "count"))
	if err != nil {
		t.Fatal(err)
	}
	sent := f.all()
	want := copyAnyMap(sent[0].Body)
	want["continuationToken"] = "ckpt-2"
	gemEq(t, sent[1].Body, gemCanon(t, want))
	if text != "1\n2\n3\n" || resp.FinishReason != "stop" {
		t.Fatalf("text %q finish %s", text, resp.FinishReason)
	}
}

func TestGeminiDroppedStreamWithoutACleanCheckpointRaises(t *testing.T) {
	for _, tc := range []struct {
		streams  [][]string
		requests int
	}{
		{[][]string{{gemLDEvent("1\n", "", ""), "DROP"}}, 1},
		{[][]string{{gemLDEvent("1\n", "ckpt-1", ""), gemLDEvent("2\n", "", ""), "DROP"}}, 1},
		{[][]string{{gemLDEvent("1\n", "ckpt-1", ""), "DROP"}, {"DROP"}}, 4},
	} {
		c, f := gemScriptedStream(t, tc.streams)
		_, _, err := gemCollect(c, gemUserRequest("gemini-4-argon", "count"))
		var apiErr *APIError
		var gerr *GeminiAPIError
		if !errors.As(err, &apiErr) || apiErr.Code != "gemini_stream_error" || !errors.As(err, &gerr) {
			t.Fatalf("err %v", err)
		}
		if got := len(f.all()); got != tc.requests {
			t.Fatalf("requests %d want %d", got, tc.requests)
		}
	}
}

func TestGeminiLongDecodeHopCeilingEndsAsLength(t *testing.T) {
	hop := func(n int) string {
		data, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": fmt.Sprintf("p%d ", n)}}},
			"finishReason": "CONTINUATION", "continuationToken": fmt.Sprintf("tok-%d", n)}}, "usageMetadata": map[string]any{"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15}})
		return string(data)
	}
	f := newGemFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			gemWriteSSE(w, hop(n+1))
			return
		}
		gemWriteJSON(w, 200, hop(n+1))
	})
	c := gemClient(t, f, "https://generativelanguage.googleapis.com/v1beta", "AQ.test", "gemini-4-argon")
	req := gemUserRequest("gemini-4-argon", "go")
	req.MaxTokens = 65_536
	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.all()) != 3 || resp.FinishReason != "length" || resp.Message.Content != "p1 p2 p3 " {
		t.Fatalf("sent %d finish %s content %q", len(f.all()), resp.FinishReason, resp.Message.Content)
	}
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()
	_, sresp, err := gemCollect(c, req)
	if err != nil {
		t.Fatal(err)
	}
	prompt, _, ok := ResponseInitialPromptTokens(sresp)
	if len(f.all()) != 3 || sresp.FinishReason != "length" || sresp.Usage.InputTokens != 30 || !ok || prompt != 10 {
		t.Fatalf("sent %d finish %s usage %+v initial %d", len(f.all()), sresp.FinishReason, sresp.Usage, prompt)
	}
}

func TestGeminiLongDecodeUsageKeepsCanonicalBucketsOnFirstHop(t *testing.T) {
	merged := mergeHopUsage([]map[string]any{gemMap(`{"promptTokenCount": 100, "cachedContentTokenCount": 40, "candidatesTokenCount": 32768}`),
		gemMap(`{"promptTokenCount": 32868, "cachedContentTokenCount": 32800, "candidatesTokenCount": 10}`)})
	usage := geminiUsageFromMetadata(merged)
	if usage["prompt_tokens"] != 32968 || gemAt(usage, "prompt_tokens_details", "cached_tokens") != 32840 {
		t.Fatalf("usage %v", usage)
	}
	if usage["initial_prompt_tokens"] != 100 || usage["initial_cached_tokens"] != 40 {
		t.Fatalf("initial %v", usage)
	}
}

func TestGeminiLongDecodeBudgetsAndStaleMultiplier(t *testing.T) {
	for _, tc := range []struct {
		model string
		cfg   map[string]any
		want  int
	}{
		{"gemini-4-argon", map[string]any{"effort": "xhigh"}, 262_144},
		{"google/gemini-4-argon", map[string]any{"effort": "max"}, 1_048_576},
		{"gemini-4-argon", map[string]any{"effort": "ULTRA "}, 1_048_576},
		{"gemini-4-argon", map[string]any{"effort": "high"}, 0},
		{"gemini-4-argon", map[string]any{"enabled": false, "effort": "max"}, 0},
		{"gemini-3.8-flash", map[string]any{"effort": "max"}, 0},
		{"gemini-4-argon", nil, 0},
	} {
		if got := longDecodeMaxOutputTokens(tc.model, tc.cfg); got != tc.want {
			t.Errorf("%s %v = %d want %d", tc.model, tc.cfg, got, tc.want)
		}
	}
	if longDecodeStaleMultiplier(map[string]any{"extra_body": map[string]any{"max_output_tokens": 262_144}}) != 8 ||
		longDecodeStaleMultiplier(map[string]any{}) != 1 || longDecodeStaleMultiplier(map[string]any{"extra_body": map[string]any{}}) != 1 {
		t.Fatal("stale multiplier")
	}
	if continuationToken(gemMap(`{"candidates": [{"finishReason": "STOP", "continuationToken": "x"}]}`)) != "" ||
		continuationToken(gemMap(`{"candidates": [{"finishReason": "continuation", "continuationToken": "x"}]}`)) != "x" {
		t.Fatal("continuation token")
	}
}

func TestGeminiSplitStreamContinuationHoldsBackHopFinish(t *testing.T) {
	var hops []map[string]any
	ev, tok := splitStreamContinuation(gemMap(`{"candidates": [{"content": {"parts": [{"text": "a"}]}, "finishReason": "CONTINUATION", "continuationToken": "t1"}], "usageMetadata": {"promptTokenCount": 5}}`), &hops)
	if tok != "t1" || len(hops) != 1 {
		t.Fatalf("token %q hops %d", tok, len(hops))
	}
	gemEq(t, ev, `{"candidates": [{"content": {"parts": [{"text": "a"}]}}]}`)
	ev, tok = splitStreamContinuation(gemMap(`{"candidates": [{"content": {"parts": [{"text": "b"}]}, "finishReason": "STOP"}], "usageMetadata": {"promptTokenCount": 7}}`), &hops)
	if tok != "" || gemAt(ev, "usageMetadata", "promptTokenCount") != 12 || gemAt(ev, "usageMetadata", "initialPromptTokenCount") != 5 {
		t.Fatalf("final %v", ev)
	}
	_, tok = splitStreamContinuation(gemMap(`{"candidates": [{"content": {"parts": [{"text": "c"}]}, "continuationToken": "ck"}]}`), &hops)
	if tok != "ck" || len(hops) != 1 {
		t.Fatalf("checkpoint %q", tok)
	}
}

func TestGeminiCompleteNonStreamTimeoutSpansEveryHop(t *testing.T) {
	f := newGemFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		time.Sleep(30 * time.Millisecond)
		gemWriteJSON(w, 200, `{"candidates": [{"content": {"parts": [{"text": "ok"}]}, "finishReason": "STOP"}]}`)
	})
	c := gemClient(t, f, "https://generativelanguage.googleapis.com/v1beta", "AQ.test", "gemini-2.5-flash")
	req := gemUserRequest("gemini-2.5-flash", "hi")
	req.Timeout = 5 * time.Millisecond
	_, err := c.Complete(context.Background(), req)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != ErrTimeout {
		t.Fatalf("err %v", err)
	}
}

func TestLongDecodeStaleMultiplierExportScalesTheStaleBudget(t *testing.T) {
	if LongDecodeStaleMultiplier(map[string]any{"extra_body": map[string]any{"max_output_tokens": 262_144}}) != 8 || LongDecodeStaleMultiplier(nil) != 1 {
		t.Fatal("long decode multiplier")
	}
}
