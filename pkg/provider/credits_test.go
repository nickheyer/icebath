package provider

import (
	"net/http"
	"testing"
	"time"
)

func TestCheckOpenRouterCacheStatus(t *testing.T) {
	c := NewCreditsCapture(nil, "openrouter", "m", "https://openrouter.ai/api/v1", "s")
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
	c := NewCreditsCapture(nil, "nous", "m", "https://inference-api.nousresearch.com/v1", "s")
	c.CaptureRateLimits(headersFrom(nousHeaders))
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

func TestNoteModelSwitch(t *testing.T) {
	c := NewCreditsCapture(nil, "nous", GuestModel, "https://welcome-api.nousresearch.com/v1", "s")
	if _, ok := c.NoteModelSwitch(nil); ok {
		t.Fatal("nil headers")
	}
	h := http.Header{}
	h.Set(ModelSwitchHeader, GuestModel)
	if _, ok := c.NoteModelSwitch(h); ok {
		t.Fatal("same model is not a switch")
	}
	h.Set(ModelSwitchHeader, "hermes-4-70b")
	if backing, ok := c.NoteModelSwitch(h); !ok || backing != "hermes-4-70b" {
		t.Fatalf("switch %q %v", backing, ok)
	}
	requested, backing, ok := c.TakePendingModelSwitch()
	if !ok || requested != GuestModel || backing != "hermes-4-70b" {
		t.Fatalf("pending %q %q %v", requested, backing, ok)
	}
	if _, _, ok := c.TakePendingModelSwitch(); ok {
		t.Fatal("pending switch is consumed once")
	}
	c.CaptureNousModelSwitch(h)
	if _, _, ok := c.TakePendingModelSwitch(); !ok {
		t.Fatal("capture records the header")
	}
}

func TestIsNousRouteAndNotices(t *testing.T) {
	if !IsNousRoute("nous", "") || !IsNousRoute("custom", "https://inference-api.nousresearch.com/v1") || IsNousRoute("openai", "https://api.openai.com/v1") {
		t.Fatal("nous route")
	}
	c := NewCreditsCapture(nil, "nous", "m", "", "s")
	if !c.CreditsNoticesEnabled() {
		t.Fatal("notices default on")
	}
	if _, ok := c.CreditsSpentMicros(); ok {
		t.Fatal("no data means no spend")
	}
	if c.Credits() != nil || c.Latch() == nil {
		t.Fatal("empty capture")
	}
	c.SetRoute("openrouter", "m2", "https://openrouter.ai/api/v1")
	if c.Provider != "openrouter" || c.Model != "m2" {
		t.Fatal("set route")
	}
	c.CaptureCredits(http.Header{}, false)
	if c.Credits() != nil {
		t.Fatal("no headers keeps nil")
	}
	_ = time.Now()
}
