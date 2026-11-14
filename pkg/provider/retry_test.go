package provider

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestBackoffIsExponential(t *testing.T) {
	for _, attempt := range []int{1, 2, 3, 4} {
		expected := math.Min(5*math.Pow(2, float64(attempt-1)), 120)
		total := 0.0
		for i := 0; i < 100; i++ {
			total += JitteredBackoff(attempt, BackoffOptions{BaseDelay: 5 * time.Second, MaxDelay: 120 * time.Second, JitterRatio: 0}).Seconds()
		}
		if mean := total / 100; math.Abs(mean-expected) > 0.01 {
			t.Fatalf("attempt %d: expected %v, got %v", attempt, expected, mean)
		}
	}
}

func TestBackoffRespectsMaxDelay(t *testing.T) {
	for _, attempt := range []int{10, 20, 100} {
		if d := JitteredBackoff(attempt, BackoffOptions{BaseDelay: 5 * time.Second, MaxDelay: 60 * time.Second, JitterRatio: 0}); d > 60*time.Second {
			t.Fatalf("attempt %d: delay %v exceeds max 60s", attempt, d)
		}
	}
}

func TestBackoffThreadSafety(t *testing.T) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	results := map[time.Duration]bool{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := JitteredBackoff(1, BackoffOptions{BaseDelay: 10 * time.Second, MaxDelay: 120 * time.Second, JitterRatio: 0.5})
			mu.Lock()
			results[d] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(results) < 6 {
		t.Fatalf("expected mostly unique delays, got %d/8 unique", len(results))
	}
}

func zaiOverloadError() *APIError {
	return &APIError{StatusCode: 429, Body: `{"error":{"code":"1305","message":"The service may be temporarily overloaded, please try again later"}}`, Code: "1305", Message: "The service may be temporarily overloaded, please try again later"}
}

func TestZaiOverloadRetryCeilingExceedsShortAttempts(t *testing.T) {
	ceiling := ZAICodingOverloadRetryCeiling()
	if ceiling <= zaiCodingOverloadShortAttempts {
		t.Fatalf("ceiling %d must exceed short attempts %d", ceiling, zaiCodingOverloadShortAttempts)
	}
	if (ceiling-1)-zaiCodingOverloadShortAttempts < len(zaiCodingOverloadLongBackoff) {
		t.Fatalf("ceiling %d leaves no headroom for the long backoff tier", ceiling)
	}
}

func TestZaiOverloadCeilingMakesLongTierReachable(t *testing.T) {
	err := zaiOverloadError()
	var longWaits []time.Duration
	for attempt := 1; attempt < ZAICodingOverloadRetryCeiling(); attempt++ {
		wait, policy := AdaptiveRateLimitBackoff(attempt, "https://api.z.ai/api/coding/paas/v4", "glm-5.2", err, time.Second)
		if policy == "zai_coding_overload_long" {
			longWaits = append(longWaits, wait)
		}
	}
	if len(longWaits) != len(zaiCodingOverloadLongBackoff) {
		t.Fatalf("long tier waits %v do not cover the schedule %v", longWaits, zaiCodingOverloadLongBackoff)
	}
	for i, base := range zaiCodingOverloadLongBackoff {
		if longWaits[i] < base || longWaits[i] > base+base/5+time.Millisecond {
			t.Fatalf("long wait %d = %v outside [%v, %v]", i, longWaits[i], base, base+base/5)
		}
	}
	if wait, policy := AdaptiveRateLimitBackoff(1, "https://api.z.ai/api/coding/paas/v4", "glm-5.2", err, 7*time.Second); wait != 7*time.Second || policy != "zai_coding_overload_short" {
		t.Fatalf("short attempt should keep the default wait, got %v %q", wait, policy)
	}
	if wait, policy := AdaptiveRateLimitBackoff(5, "https://api.openai.com/v1", "gpt-5", err, 7*time.Second); wait != 7*time.Second || policy != "" {
		t.Fatalf("non zai route should keep the default wait, got %v %q", wait, policy)
	}
}

func httpDate(ahead time.Duration) string {
	return time.Now().Add(ahead).UTC().Format(http.TimeFormat)
}

func TestParseRetryAfterSeconds(t *testing.T) {
	if d, ok := ParseRetryAfter("120"); !ok || d != 120*time.Second {
		t.Fatalf("numeric string: %v %v", d, ok)
	}
	if d, ok := ParseRetryAfter(" 4.5 "); !ok || d != 4500*time.Millisecond {
		t.Fatalf("decimal string: %v %v", d, ok)
	}
	if d, ok := retryAfterValue(float64(45)); !ok || d != 45*time.Second {
		t.Fatalf("numeric value: %v %v", d, ok)
	}
	if d, ok := retryAfterValue(3.25); !ok || d != 3250*time.Millisecond {
		t.Fatalf("float value: %v %v", d, ok)
	}
	if d, ok := ParseRetryAfter(httpDate(90 * time.Second)); !ok || d < 80*time.Second || d > 91*time.Second {
		t.Fatalf("future http date: %v %v", d, ok)
	}
	if d, ok := ParseRetryAfter(httpDate(-90 * time.Second)); !ok || d != 0 {
		t.Fatalf("past http date: %v %v", d, ok)
	}
	if _, ok := ParseRetryAfter("garbage"); ok {
		t.Fatal("garbage parsed")
	}
	if _, ok := retryAfterValue(true); ok {
		t.Fatal("bool parsed")
	}
}

type headerErr struct {
	msg string
	h   http.Header
}

func (e *headerErr) Error() string                { return e.msg }
func (e *headerErr) ResponseHeaders() http.Header { return e.h }

func TestHTTPDateHeaderParsedIdenticallyAcrossSites(t *testing.T) {
	header := httpDate(90 * time.Second)
	canonical, ok := ParseRetryAfter(header)
	if !ok || canonical < 85*time.Second || canonical > 90*time.Second {
		t.Fatalf("canonical %v %v", canonical, ok)
	}
	h := http.Header{}
	h.Set("Retry-After", header)
	guard, ok := parseResetSeconds(h)
	if !ok || math.Abs(guard.Seconds()-canonical.Seconds()) >= 2 {
		t.Fatalf("guard %v %v", guard, ok)
	}
	err := &APIError{Message: "rate limited", Err: &headerErr{msg: "rate limited", h: h}}
	ctx := ExtractAPIErrorContext(err)
	resetAt, _ := ctx["reset_at"].(float64)
	delta := resetAt - float64(time.Now().UnixNano())/1e9
	if delta < 85 || delta > 91 {
		t.Fatalf("reset_at delta %v", delta)
	}
}

func TestResetDelayOneTable(t *testing.T) {
	cases := []struct {
		message string
		seconds float64
	}{
		{"Weekly usage limit reached. Resets in 6hr 29min.", 6*3600 + 29*60},
		{"resets in 2 hours 5 minutes", 2*3600 + 5*60},
		{"Limit hit; resets in 45s", 45},
		{`"quotaResetDelay": "1500ms"`, 1.5},
		{"please retry after 12 seconds", 12},
		{"Rate limited. Retry after 30s; resets in 4hr", 30},
	}
	for _, c := range cases {
		d, ok := ResetDelayFromMessage(c.message)
		if !ok || math.Abs(d.Seconds()-c.seconds) > 0.001 {
			t.Fatalf("%q: got %v %v want %v", c.message, d, ok, c.seconds)
		}
		ctx := ExtractAPIErrorContext(&APIError{Message: c.message})
		resetAt, _ := ctx["reset_at"].(float64)
		if math.Abs(resetAt-float64(time.Now().UnixNano())/1e9-c.seconds) > 2 {
			t.Fatalf("%q: error context reset_at %v", c.message, resetAt)
		}
	}
	if _, ok := ResetDelayFromMessage("resets in the future, maybe"); ok {
		t.Fatal("no grammar should mean no reset")
	}
	if _, has := ExtractAPIErrorContext(&APIError{Message: "resets in the future, maybe"})["reset_at"]; has {
		t.Fatal("no grammar should mean no reset_at in context")
	}
}

func TestExtractAPIErrorContextBodyFields(t *testing.T) {
	body := `{"error":{"code":"usage_limit_reached","message":"Weekly limit","resets_in_seconds":120}}`
	ctx := ExtractAPIErrorContext(&APIError{StatusCode: 429, Body: body, Message: "Weekly limit"})
	if ctx["reason"] != "usage_limit_reached" || ctx["message"] != "Weekly limit" {
		t.Fatalf("context %v", ctx)
	}
	resetAt, _ := ctx["reset_at"].(float64)
	if math.Abs(resetAt-float64(time.Now().UnixNano())/1e9-120) > 2 {
		t.Fatalf("resets_in_seconds reset_at %v", resetAt)
	}
	ctx = ExtractAPIErrorContext(&APIError{StatusCode: 429, Body: `{"error":{"message":"m","resets_at":1700000000}}`})
	if ctx["reset_at"] != float64(1700000000) {
		t.Fatalf("resets_at %v", ctx["reset_at"])
	}
	h := http.Header{}
	h.Set("x-ratelimit-reset-requests", "6m0s")
	ctx = ExtractAPIErrorContext(&APIError{StatusCode: 429, Message: "x", Err: &headerErr{msg: "x", h: h}})
	resetAt, _ = ctx["reset_at"].(float64)
	if math.Abs(resetAt-float64(time.Now().UnixNano())/1e9-360) > 2 {
		t.Fatalf("vendor duration reset_at %v", resetAt)
	}
	if secs, ok := durationStringSeconds("1h2m3s"); !ok || secs != 3723 {
		t.Fatalf("duration string %v %v", secs, ok)
	}
	if _, ok := durationStringSeconds("1h junk"); ok {
		t.Fatal("junk duration parsed")
	}
}

var nousHeaders = map[string]string{
	"x-ratelimit-limit-requests": "800", "x-ratelimit-limit-requests-1h": "33600",
	"x-ratelimit-limit-tokens": "8000000", "x-ratelimit-limit-tokens-1h": "336000000",
	"x-ratelimit-remaining-requests": "795", "x-ratelimit-remaining-requests-1h": "33590",
	"x-ratelimit-remaining-tokens": "7999500", "x-ratelimit-remaining-tokens-1h": "335999000",
	"x-ratelimit-reset-requests": "45.5", "x-ratelimit-reset-requests-1h": "3500.0",
	"x-ratelimit-reset-tokens": "42.3", "x-ratelimit-reset-tokens-1h": "3490.0",
}

func headersFrom(m map[string]string) http.Header {
	h := http.Header{}
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

func TestParseRateLimitHeadersBasic(t *testing.T) {
	state := ParseRateLimitHeaders(headersFrom(nousHeaders), "nous", time.Now())
	if state == nil || state.Provider != "nous" || !state.HasData() {
		t.Fatalf("state %+v", state)
	}
	if state.RequestsMin.Limit != 800 || state.RequestsMin.Remaining != 795 || state.RequestsMin.ResetSeconds != 45.5 {
		t.Fatalf("requests min %+v", state.RequestsMin)
	}
	if state.RequestsHour.Limit != 33600 || state.RequestsHour.Remaining != 33590 {
		t.Fatalf("requests hour %+v", state.RequestsHour)
	}
	if state.TokensMin.Limit != 8000000 || state.TokensMin.Remaining != 7999500 {
		t.Fatalf("tokens min %+v", state.TokensMin)
	}
	if state.TokensHour.Limit != 336000000 || state.TokensHour.Remaining != 335999000 || state.TokensHour.ResetSeconds != 3490 {
		t.Fatalf("tokens hour %+v", state.TokensHour)
	}
	if ParseRateLimitHeaders(http.Header{}, "", time.Now()) != nil {
		t.Fatal("no headers should give nil")
	}
}

func TestRateLimitBucket(t *testing.T) {
	b := RateLimitBucket{Limit: 100, Remaining: 20, ResetSeconds: 30, CapturedAt: time.Now()}
	if math.Abs(b.UsagePct()-80) > 0.001 {
		t.Fatalf("usage pct %v", b.UsagePct())
	}
	now := time.Now()
	b = RateLimitBucket{Limit: 800, Remaining: 795, ResetSeconds: 60, CapturedAt: now.Add(-10 * time.Second)}
	if left := b.RemainingSecondsNow(now); left < 49 || left > 51 {
		t.Fatalf("remaining seconds %v", left)
	}
}

func TestProviderRetryAfterBodyField(t *testing.T) {
	d, ok := ProviderRetryAfter(&APIError{StatusCode: 503, Body: `{"error":{"message":"down","retry_after":12}}`})
	if !ok || d != 12*time.Second {
		t.Fatalf("body retry_after %v %v", d, ok)
	}
	if _, ok := ProviderRetryAfter(&APIError{StatusCode: 503}); ok {
		t.Fatal("no retry_after should report none")
	}
	if _, ok := ProviderRetryAfter(nil); ok {
		t.Fatal("nil error should report none")
	}
	wrapped := fmt.Errorf("wrap: %w", &APIError{RetryAfter: 3 * time.Second})
	var apiErr *APIError
	if !errors.As(wrapped, &apiErr) {
		t.Fatal("unwrap failed")
	}
	if d, ok := ProviderRetryAfter(apiErr); !ok || d != 3*time.Second {
		t.Fatalf("field retry after %v %v", d, ok)
	}
}
