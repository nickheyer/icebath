package gateway

import (
	"context"
	"strings"
	"testing"
)

type retryAdapter struct {
	*BasePlatformAdapter
	results    []SendResult
	calls      [][2]string
	finalCheck func(SendResult) bool
}

func (a *retryAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *retryAdapter) Disconnect(context.Context) error            { return nil }
func (a *retryAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": "test", "type": "direct", "chat_id": chatID}, nil
}
func (a *retryAdapter) Send(_ context.Context, chatID, content, _ string, _ map[string]any) (SendResult, error) {
	a.calls = append(a.calls, [2]string{chatID, content})
	if len(a.results) > 0 {
		r := a.results[0]
		a.results = a.results[1:]
		return r, nil
	}
	return SendResult{Success: true, MessageID: "ok"}, nil
}
func (a *retryAdapter) SendRetryIsFinal(result SendResult) bool {
	if a.finalCheck != nil {
		return a.finalCheck(result)
	}
	return a.BasePlatformAdapter.SendRetryIsFinal(result)
}

func newRetryAdapter(results ...SendResult) *retryAdapter {
	a := &retryAdapter{results: results}
	a.BasePlatformAdapter = NewBasePlatformAdapter(NewPlatformConfig(), PlatformTelegram, a)
	return a
}

func recordSleeps(t *testing.T) *[]float64 {
	slept := &[]float64{}
	oldSleep := sendRetrySleep
	sendRetrySleep = func(_ context.Context, d float64) error { *slept = append(*slept, d); return nil }
	t.Cleanup(func() { sendRetrySleep = oldSleep })
	return slept
}

func floatPtr(v float64) *float64 { return &v }

func TestSendRetryClassifiersRejectNone(t *testing.T) {
	if isRetryableError("") || isRetryableError("Forbidden: bot was blocked by the user") || isTimeoutError("") {
		t.Fatal("none and permission errors are neither retryable nor timeouts")
	}
}

func TestSendWithRetryOutcomes(t *testing.T) {
	ctx := context.Background()
	connErr := SendResult{Error: "httpx.ConnectError: host unreachable"}
	flood := SendResult{Error: "flood control exceeded, retry in 60 seconds"}
	parse := SendResult{Error: "Bad Request: can't parse entities"}
	cases := []struct {
		name      string
		results   []SendResult
		content   string
		retries   int
		base      float64
		calls     int
		success   bool
		sleeps    int
		lastCheck func(string) bool
	}{
		{"success first attempt", []SendResult{{Success: true, MessageID: "123"}}, "hello", 2, 2, 1, true, 0, nil},
		{"retries on connect error and succeeds", []SendResult{{Error: "httpx.ConnectError: connection refused"}, {Success: true, MessageID: "ok"}}, "hello", 2, 0, 2, true, 1, nil},
		{"timeout not retried to prevent duplicates", []SendResult{{Error: "ReadTimeout: request timed out"}}, "hello", 3, 0, 1, false, 0, nil},
		{"sends user notice after exhaustion", []SendResult{connErr, connErr, connErr, {Success: true}}, "hello", 2, 0, 4, false, 2,
			func(s string) bool { return strings.Contains(strings.ToLower(s), "delivery failed") }},
		{"non network error falls back immediately", []SendResult{parse, {Success: true, MessageID: "fallback_ok"}}, "**bold**", 2, 0, 2, true, 0,
			func(s string) bool { return strings.Contains(strings.ToLower(s), "plain text") }},
		{"rate limited exhausted returns typed failure not fallback", []SendResult{flood, flood, flood}, "hello", 2, 0, 3, false, 2,
			func(s string) bool {
				return !strings.Contains(strings.ToLower(s), "plain text") && !strings.Contains(strings.ToLower(s), "delivery failed")
			}},
		{"rate limited then formatting error stops retrying and falls back", []SendResult{flood, parse}, "**bold**", 3, 0, 3, true, 1,
			func(s string) bool {
				return strings.Contains(strings.ToLower(s), "plain text") && !strings.Contains(strings.ToLower(s), "delivery failed")
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slept := recordSleeps(t)
			a := newRetryAdapter(tc.results...)
			res, err := a.SendWithRetry(ctx, "chat1", tc.content, "", nil, tc.retries, tc.base)
			if err != nil || res.Success != tc.success || len(a.calls) != tc.calls || len(*slept) != tc.sleeps {
				t.Fatalf("res %+v err %v calls %d sleeps %v", res, err, len(a.calls), *slept)
			}
			if tc.lastCheck != nil && !tc.lastCheck(a.calls[len(a.calls)-1][1]) {
				t.Fatalf("last send %q", a.calls[len(a.calls)-1][1])
			}
		})
	}
}

func TestSendWithRetryHonoursServerRetryAfter(t *testing.T) {
	ctx := context.Background()
	slept := recordSleeps(t)
	a := newRetryAdapter(SendResult{Error: "Flood control exceeded. Retry in 37 seconds", Retryable: true, RetryAfter: floatPtr(37)},
		SendResult{Success: true, MessageID: "ok"})
	if res, _ := a.SendWithRetry(ctx, "chat1", "hello", "", nil, 2, 2.0); !res.Success || (*slept)[0] < 36 {
		t.Fatalf("first retry waits the server hint %v", *slept)
	}
	slept = recordSleeps(t)
	a = newRetryAdapter(SendResult{Error: "ConnectError", Retryable: true},
		SendResult{Error: "Flood control exceeded. Retry in 30 seconds", Retryable: true, RetryAfter: floatPtr(30)},
		SendResult{Success: true, MessageID: "ok"})
	if res, _ := a.SendWithRetry(ctx, "chat1", "hello", "", nil, 3, 2.0); !res.Success || (*slept)[1] < 29 {
		t.Fatalf("second retry waits the later hint %v", *slept)
	}
}

func TestLongServerRetryAfterReturnsTypedFailureWithoutSleeping(t *testing.T) {
	slept := recordSleeps(t)
	a := newRetryAdapter(SendResult{Error: "flood_control:5820", RetryAfter: floatPtr(5820)})
	res, err := a.SendWithRetry(context.Background(), "c", "hello", "", nil, 2, 2.0)
	if err != nil || res.Success || res.RetryAfter == nil || *res.RetryAfter != 5820 || len(*slept) != 0 || len(a.calls) != 1 {
		t.Fatalf("res %+v sleeps %v calls %d", res, *slept, len(a.calls))
	}
}

func TestFinalFailureSkipsRetryAndPlainTextFallback(t *testing.T) {
	slept := recordSleeps(t)
	refusal := SendResult{Error: "Target not allowed", Retryable: true,
		RawResponse: map[string]any{"error_class": "target_not_allowed", "retryable": false}}
	a := newRetryAdapter(refusal)
	a.finalCheck = func(r SendResult) bool {
		m, ok := r.RawResponse.(map[string]any)
		return ok && m["error_class"] == "target_not_allowed"
	}
	res, _ := a.SendWithRetry(context.Background(), "c", "hello", "", nil, 2, 2.0)
	if res.Error != "Target not allowed" || len(a.calls) != 1 || a.calls[0][1] != "hello" || len(*slept) != 0 {
		t.Fatalf("res %+v calls %v sleeps %v", res, a.calls, *slept)
	}
}

func TestServerRetryAfterIsHonouredByEveryAdapter(t *testing.T) {
	slept := recordSleeps(t)
	oldJitter := sendRetryJitter
	sendRetryJitter = func() float64 { return 0 }
	t.Cleanup(func() { sendRetryJitter = oldJitter })
	a := newRetryAdapter(SendResult{Error: "429 rate limited", Retryable: true, RetryAfter: floatPtr(7)})
	a.finalCheck = func(SendResult) bool { return false }
	res, _ := a.SendWithRetry(context.Background(), "c", "hello", "", nil, 2, 2.0)
	if !res.Success || len(a.calls) != 2 || a.calls[0][1] != "hello" || a.calls[1][1] != "hello" || len(*slept) != 1 || (*slept)[0] != 7 {
		t.Fatalf("res %+v calls %v sleeps %v", res, a.calls, *slept)
	}
}
