package provider

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Header tag per rate limit window in state field order
var bucketTags = [4]struct{ attr, tag string }{{"requests_min", "requests"}, {"requests_hour", "requests-1h"}, {"tokens_min", "tokens"}, {"tokens_hour", "tokens-1h"}}

// RateLimitBucket is one rate limit window such as requests per minute
type RateLimitBucket struct {
	Limit        int
	Remaining    int
	ResetSeconds float64
	CapturedAt   time.Time
}

// Used is the consumed share of the window clamped at zero
func (b RateLimitBucket) Used() int {
	if u := b.Limit - b.Remaining; u > 0 {
		return u
	}
	return 0
}

// UsagePct is the consumed percentage or zero without a limit
func (b RateLimitBucket) UsagePct() float64 {
	if b.Limit > 0 {
		return float64(b.Used()) / float64(b.Limit) * 100
	}
	return 0
}

// RemainingSecondsNow estimates seconds until reset adjusted for time since capture
func (b RateLimitBucket) RemainingSecondsNow(now time.Time) float64 {
	left := b.ResetSeconds - now.Sub(b.CapturedAt).Seconds()
	if left < 0 {
		return 0
	}
	return left
}

// RateLimitState is the full rate limit picture parsed from response headers
type RateLimitState struct {
	RequestsMin  RateLimitBucket
	RequestsHour RateLimitBucket
	TokensMin    RateLimitBucket
	TokensHour   RateLimitBucket
	CapturedAt   time.Time
	Provider     string
}

// HasData reports whether any headers were ever captured
func (s *RateLimitState) HasData() bool { return s != nil && !s.CapturedAt.IsZero() }

// AgeSeconds is the time since capture or infinity without data
func (s *RateLimitState) AgeSeconds(now time.Time) float64 {
	if !s.HasData() {
		return 1e308
	}
	return now.Sub(s.CapturedAt).Seconds()
}

func (s *RateLimitState) buckets() [4]*RateLimitBucket {
	return [4]*RateLimitBucket{&s.RequestsMin, &s.RequestsHour, &s.TokensMin, &s.TokensHour}
}

func safeFloat(v string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return f, err == nil
}

func safeInt(v string) (int, bool) {
	f, ok := safeFloat(v)
	return int(f), ok
}

// LowerHeaders flattens a header set to lowercase names with first values
func LowerHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if len(vals) > 0 {
			out[strings.ToLower(k)] = vals[0]
		}
	}
	return out
}

// HasRateLimitHeaders reports any x-ratelimit prefixed header
func HasRateLimitHeaders(lowered map[string]string) bool {
	for k := range lowered {
		if strings.HasPrefix(k, "x-ratelimit-") {
			return true
		}
	}
	return false
}

// ParseRateLimitHeaders reads the four x-ratelimit windows or returns nil when none are present
func ParseRateLimitHeaders(h http.Header, provider string, now time.Time) *RateLimitState {
	lowered := LowerHeaders(h)
	if !HasRateLimitHeaders(lowered) {
		return nil
	}
	s := &RateLimitState{CapturedAt: now, Provider: provider}
	bs := s.buckets()
	for i, bt := range bucketTags {
		limit, _ := safeInt(lowered["x-ratelimit-limit-"+bt.tag])
		remaining, _ := safeInt(lowered["x-ratelimit-remaining-"+bt.tag])
		reset, _ := safeFloat(lowered["x-ratelimit-reset-"+bt.tag])
		*bs[i] = RateLimitBucket{Limit: limit, Remaining: remaining, ResetSeconds: reset, CapturedAt: now}
	}
	return s
}

// FormatCount renders 7999856 as 8.0M and 33599 as 33.6K
func FormatCount(n int) string {
	switch {
	case n >= 1000000:
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	case n >= 1000:
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return strconv.Itoa(n)
}

// FormatSeconds renders 58s, 2m 14s, or 1h 2m
func FormatSeconds(seconds float64) string {
	s := int(seconds)
	if s < 0 {
		s = 0
	}
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		m, sec := s/60, s%60
		if sec != 0 {
			return fmt.Sprintf("%dm %ds", m, sec)
		}
		return fmt.Sprintf("%dm", m)
	}
	h, m := s/3600, (s%3600)/60
	if m != 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dh", h)
}

func bar(pct float64, width int) string {
	filled := int(pct / 100 * float64(width))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

func bucketLine(label string, b RateLimitBucket, now time.Time) string {
	if b.Limit <= 0 {
		return fmt.Sprintf("  %-14s  (no data)", label)
	}
	pct := b.UsagePct()
	return fmt.Sprintf("  %-14s %s %5.1f%%  %s/%s used  (%s left, resets in %s)", label, bar(pct, 20), pct,
		FormatCount(b.Used()), FormatCount(b.Limit), FormatCount(b.Remaining), FormatSeconds(b.RemainingSecondsNow(now)))
}

// FormatRateLimitDisplay renders the multi line usage view with warnings at eighty percent
func FormatRateLimitDisplay(s *RateLimitState, now time.Time) string {
	if !s.HasData() {
		return "No rate limit data yet. Make an API request first."
	}
	age := s.AgeSeconds(now)
	freshness := "just now"
	if age >= 60 {
		freshness = FormatSeconds(age) + " ago"
	} else if age >= 5 {
		freshness = fmt.Sprintf("%ds ago", int(age))
	}
	label := "Provider"
	if s.Provider != "" {
		label = strings.ToUpper(s.Provider[:1]) + s.Provider[1:]
	}
	labeled := []struct {
		name string
		b    RateLimitBucket
	}{{"Requests/min", s.RequestsMin}, {"Requests/hr", s.RequestsHour}, {"Tokens/min", s.TokensMin}, {"Tokens/hr", s.TokensHour}}
	lines := []string{fmt.Sprintf("%s Rate Limits (captured %s):", label, freshness), ""}
	lines = append(lines, bucketLine(labeled[0].name, labeled[0].b, now), bucketLine(labeled[1].name, labeled[1].b, now), "")
	lines = append(lines, bucketLine(labeled[2].name, labeled[2].b, now), bucketLine(labeled[3].name, labeled[3].b, now))
	var warnings []string
	for _, l := range labeled {
		if l.b.Limit > 0 && l.b.UsagePct() >= 80 {
			warnings = append(warnings, fmt.Sprintf("  ! %s at %.0f%%, resets in %s", strings.ToLower(l.name), l.b.UsagePct(), FormatSeconds(l.b.RemainingSecondsNow(now))))
		}
	}
	if len(warnings) > 0 {
		lines = append(append(lines, ""), warnings...)
	}
	return strings.Join(lines, "\n")
}

// FormatRateLimitCompact renders the one line RPM RPH TPM TPH summary for status bars
func FormatRateLimitCompact(s *RateLimitState, now time.Time) string {
	if !s.HasData() {
		return "No rate limit data."
	}
	raw := func(n int) string { return strconv.Itoa(n) }
	windows := []struct {
		tag   string
		b     RateLimitBucket
		fmt   func(int) string
		reset bool
	}{{"RPM", s.RequestsMin, raw, false}, {"RPH", s.RequestsHour, FormatCount, true}, {"TPM", s.TokensMin, FormatCount, false}, {"TPH", s.TokensHour, FormatCount, true}}
	var parts []string
	for _, w := range windows {
		if w.b.Limit <= 0 {
			continue
		}
		part := fmt.Sprintf("%s: %s/%s", w.tag, w.fmt(w.b.Remaining), w.fmt(w.b.Limit))
		if w.reset {
			part += fmt.Sprintf(" (resets %s)", FormatSeconds(w.b.RemainingSecondsNow(now)))
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " | ")
}
