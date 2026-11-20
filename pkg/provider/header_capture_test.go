package provider

import (
	"net/http"
	"testing"
)

func TestCheckOpenRouterCacheStatus(t *testing.T) {
	c := NewHeaderCapture("openrouter")
	c.CheckOpenRouterCacheStatus(http.Header{})
	if c.OpenRouterCacheHits() != 0 {
		t.Fatal("no header is a noop")
	}
	h := http.Header{}
	h.Set("x-openrouter-cache-status", "hit")
	c.CheckOpenRouterCacheStatus(h)
	if c.OpenRouterCacheHits() != 1 {
		t.Fatal("case insensitive HIT counts")
	}
	h.Set("x-openrouter-cache-status", "MISS")
	c.CheckOpenRouterCacheStatus(h)
	if c.OpenRouterCacheHits() != 1 {
		t.Fatal("MISS must not count")
	}
	h.Set("X-OpenRouter-Cache-Status", "HIT")
	c.CheckOpenRouterCacheStatus(h)
	if c.OpenRouterCacheHits() != 2 {
		t.Fatal("second HIT counts")
	}
}

func TestCaptureRateLimitsKeepsLastKnown(t *testing.T) {
	c := NewHeaderCapture("openrouter")
	c.CaptureRateLimits(headersFrom(rateLimitHeaders))
	state := c.RateLimit()
	if state == nil || state.RequestsMin.Limit != 800 {
		t.Fatalf("state %+v", state)
	}
	c.CaptureRateLimits(http.Header{})
	if c.RateLimit() != state {
		t.Fatal("a miss keeps the last known state")
	}
	plain := http.Header{}
	plain.Set("content-type", "application/json")
	c.CaptureRateLimits(plain)
	if c.RateLimit() != state {
		t.Fatal("headers without rate limits keep the last known state")
	}
}
