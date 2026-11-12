package provider

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"
)

// Error carrying response headers the way an SDK error exposes error.response.headers
type headeredError struct {
	msg     string
	headers http.Header
}

func (e *headeredError) Error() string                { return e.msg }
func (e *headeredError) ResponseHeaders() http.Header { return e.headers }

// Codex usage limit 429 whose body error object carries the extra fields
func codex429(t *testing.T, fields map[string]any) error {
	t.Helper()
	inner := map[string]any{"type": "usage_limit_reached", "message": "The usage limit has been reached"}
	for k, v := range fields {
		inner[k] = v
	}
	body, err := json.Marshal(map[string]any{"error": inner})
	if err != nil {
		t.Fatal(err)
	}
	return &APIError{Message: "HTTP 429: The usage limit has been reached", Body: string(body)}
}

func nowSecs() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func TestRelativeResetsInSecondsBecomesResetAtWhenNoEpochIsGiven(t *testing.T) {
	ctx := ExtractAPIErrorContext(codex429(t, map[string]any{"resets_in_seconds": 756}))
	if at, _ := ctx["reset_at"].(float64); math.Abs(at-(nowSecs()+756)) >= 5 {
		t.Fatalf("reset_at %v", ctx["reset_at"])
	}
	ctx = ExtractAPIErrorContext(codex429(t, map[string]any{"resets_at": 1_900_000_000, "resets_in_seconds": 756}))
	if ctx["reset_at"] != float64(1_900_000_000) {
		t.Fatalf("explicit epoch must win, got %v", ctx["reset_at"])
	}
	if ResetHint(&APIError{Message: "HTTP 429"}, time.Now()) != "" || ResetHint(codex429(t, map[string]any{"resets_in_seconds": -5}), time.Now()) != "" {
		t.Fatal("no signal must give no hint")
	}
}

func TestVendorResetHeadersFeedResetAt(t *testing.T) {
	iso := time.Now().UTC().Add(900 * time.Second).Format("2006-01-02T15:04:05Z")
	cases := []struct {
		headers  map[string]string
		expected float64
	}{
		{map[string]string{"x-ratelimit-reset-requests": "6m0s"}, 360},
		{map[string]string{"x-ratelimit-reset-tokens": "1.5s"}, 1.5},
		{map[string]string{"x-ratelimit-reset-requests": "20ms"}, 0.02},
		{map[string]string{"anthropic-ratelimit-requests-reset": iso}, 900},
		{map[string]string{"retry-after": "30", "x-ratelimit-reset-requests": "6m0s"}, 30},
	}
	for _, c := range cases {
		h := http.Header{}
		for k, v := range c.headers {
			h.Set(k, v)
		}
		ctx := ExtractAPIErrorContext(&headeredError{msg: "HTTP 429: rate limit", headers: h})
		if at, _ := ctx["reset_at"].(float64); math.Abs(at-(nowSecs()+c.expected)) >= 5 {
			t.Fatalf("%v gave reset_at %v", c.headers, ctx["reset_at"])
		}
	}
	past := http.Header{}
	past.Set("anthropic-ratelimit-tokens-reset", "2020-01-01T00:00:00Z")
	if ctx := ExtractAPIErrorContext(&headeredError{msg: "HTTP 429", headers: past}); ctx["reset_at"] != nil {
		t.Fatalf("a past reset must be ignored, got %v", ctx["reset_at"])
	}
}

func TestHttpDateHeaderParsedIdenticallyAtFormerlyDivergentSites(t *testing.T) {
	header := time.Now().UTC().Add(90 * time.Second).Format(http.TimeFormat)
	canonical, ok := ParseRetryAfter(header)
	if !ok || canonical.Seconds() < 85 || canonical.Seconds() > 90 {
		t.Fatalf("canonical %v %v", canonical, ok)
	}
	h := http.Header{}
	h.Set("Retry-After", header)
	guard, ok := parseResetSeconds(h)
	if !ok || math.Abs(guard.Seconds()-canonical.Seconds()) >= 2 {
		t.Fatalf("guard %v %v", guard, ok)
	}
	ctx := ExtractAPIErrorContext(&headeredError{msg: "rate limited", headers: h})
	if d := ctx["reset_at"].(float64) - nowSecs(); d < 85 || d > 91 {
		t.Fatalf("context reset in %v", d)
	}
}
