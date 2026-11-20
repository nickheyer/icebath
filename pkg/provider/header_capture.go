package provider

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HeaderCapture holds the last rate limit headers and the OpenRouter cache hits of a session
type HeaderCapture struct {
	mu sync.Mutex
	// Live provider the rate limit headers are parsed for
	Provider string
	Log      *slog.Logger
	now      func() time.Time

	rateLimit   *RateLimitState
	orCacheHits int
}

// NewHeaderCapture returns an empty capture bound to a provider
func NewHeaderCapture(provider string) *HeaderCapture {
	return &HeaderCapture{Provider: provider, Log: pkgLog(), now: time.Now}
}

func (c *HeaderCapture) logger() *slog.Logger {
	if c.Log == nil {
		return pkgLog()
	}
	return c.Log.With("logger", logName)
}

func (c *HeaderCapture) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

// Moves the capture onto the live provider after a fallback or model switch
func (c *HeaderCapture) SetProvider(provider string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Provider = provider
}

// Parses x-ratelimit headers into the cached state keeping the last known on a miss
func (c *HeaderCapture) CaptureRateLimits(h http.Header) {
	if len(h) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if state := ParseRateLimitHeaders(h, c.Provider, c.clock()); state != nil {
		c.rateLimit = state
	}
}

// Last captured rate limit state or nil
func (c *HeaderCapture) RateLimit() *RateLimitState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rateLimit
}

// Logs X-OpenRouter-Cache-Status and counts HITs
func (c *HeaderCapture) CheckOpenRouterCacheStatus(h http.Header) {
	if len(h) == 0 {
		return
	}
	status := strings.TrimSpace(LowerHeaders(h)["x-openrouter-cache-status"])
	if status == "" {
		return
	}
	upper := strings.ToUpper(status)
	if upper == "HIT" {
		c.mu.Lock()
		c.orCacheHits++
		hits := c.orCacheHits
		c.mu.Unlock()
		c.logger().Info(fmt.Sprintf("OpenRouter response cache HIT (total: %d)", hits))
		return
	}
	c.logger().Debug("OpenRouter response cache " + upper)
}

// How many HIT responses the session saw
func (c *HeaderCapture) OpenRouterCacheHits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.orCacheHits
}
