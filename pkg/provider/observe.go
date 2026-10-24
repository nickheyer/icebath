package provider

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// What the endpoint reported about its quota on one response, zero where it said nothing
type RateLimit struct {
	Requests          int
	RequestsRemaining int
	Tokens            int
	TokensRemaining   int
	InputTokens       int
	InputRemaining    int
	OutputTokens      int
	OutputRemaining   int
	Reset             time.Duration
}

// Reports whether any field was filled in
func (r RateLimit) Known() bool { return r != RateLimit{} }

// One HTTP attempt of a call with what came back and how long the provider will wait before the next
type Attempt struct {
	Provider string
	Attempt  int
	Method   string
	URL      string
	Started  time.Time
	Duration time.Duration
	Status   int
	// Transport or decoded wire failure, nil on a 2xx
	Err error
	// Pause before the next attempt, zero when this one was the last
	Wait time.Duration
	// Pause the endpoint asked for through Retry-After
	RetryAfter time.Duration
	RequestID  string
	Header     http.Header
	RateLimit  RateLimit
}

// Reports whether the attempt returned a usable response
func (a Attempt) OK() bool { return a.Err == nil && a.Status/100 == 2 }

// Receives every attempt a provider makes
type Observer interface {
	Observe(Attempt)
}

// Observer that also hears each attempt as it starts, before anything comes back
type Starter interface {
	Begin(Attempt)
}

type ObserverFunc func(Attempt)

func (f ObserverFunc) Observe(a Attempt) { f(a) }

// Header names vendors use for the request identifier, in lookup order
var requestIDHeaders = []string{"request-id", "x-request-id", "x-amzn-requestid", "x-amz-request-id", "cf-ray", "x-goog-request-id"}

// Request identifier named by any of the known headers
func RequestID(h http.Header) string {
	for _, name := range requestIDHeaders {
		if v := h.Get(name); v != "" {
			return v
		}
	}
	return ""
}

// Reads the rate limit headers of every supported vendor into one view
func ParseRateLimit(h http.Header) RateLimit {
	var r RateLimit
	get := func(names ...string) int {
		for _, n := range names {
			if v := h.Get(n); v != "" {
				if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					return i
				}
			}
		}
		return 0
	}
	r.Requests = get("x-ratelimit-limit-requests", "anthropic-ratelimit-requests-limit", "x-ratelimit-limit")
	r.RequestsRemaining = get("x-ratelimit-remaining-requests", "anthropic-ratelimit-requests-remaining", "x-ratelimit-remaining")
	r.Tokens = get("x-ratelimit-limit-tokens", "anthropic-ratelimit-tokens-limit")
	r.TokensRemaining = get("x-ratelimit-remaining-tokens", "anthropic-ratelimit-tokens-remaining")
	r.InputTokens = get("anthropic-ratelimit-input-tokens-limit")
	r.InputRemaining = get("anthropic-ratelimit-input-tokens-remaining")
	r.OutputTokens = get("anthropic-ratelimit-output-tokens-limit")
	r.OutputRemaining = get("anthropic-ratelimit-output-tokens-remaining")
	for _, n := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens", "anthropic-ratelimit-requests-reset", "anthropic-ratelimit-tokens-reset", "x-ratelimit-reset"} {
		if d := parseReset(h.Get(n)); d > 0 {
			r.Reset = d
			break
		}
	}
	return r
}

// Reads a reset given as seconds, a duration such as 6m0s, or an RFC 3339 time
func parseReset(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
		return time.Duration(f * float64(time.Second))
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d
	}
	if at, err := time.Parse(time.RFC3339, v); err == nil {
		return max(time.Until(at), 0)
	}
	return 0
}
