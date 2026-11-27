package provider

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// One window of a provider's rate limits, such as its requests a minute
type Bucket struct {
	Limit     int
	Remaining int
	// Seconds until the window resets, as the provider said when it answered
	Reset    float64
	Captured time.Time
}

// What the window has used, never below zero
func (b Bucket) Used() int {
	return max(b.Limit-b.Remaining, 0)
}

// Share of the window used, in percent, 0 without a limit
func (b Bucket) Percent() float64 {
	if b.Limit <= 0 {
		return 0
	}
	return float64(b.Used()) / float64(b.Limit) * 100
}

// Seconds left until the window resets, counted from when it was captured
func (b Bucket) Left(now time.Time) float64 {
	return max(b.Reset-now.Sub(b.Captured).Seconds(), 0)
}

// Rate limits a provider's answer stated in its x-ratelimit headers
type RateLimits struct {
	RequestsMinute Bucket
	RequestsHour   Bucket
	TokensMinute   Bucket
	TokensHour     Bucket
	Captured       time.Time
	// Provider that answered, by its name in the profile
	Provider string
}

// The windows in the order they are shown, each with its header tag and its name
func (l *RateLimits) windows() []struct {
	bucket    *Bucket
	tag, name string
} {
	return []struct {
		bucket    *Bucket
		tag, name string
	}{
		{&l.RequestsMinute, "requests", "Requests/min"},
		{&l.RequestsHour, "requests-1h", "Requests/hr"},
		{&l.TokensMinute, "tokens", "Tokens/min"},
		{&l.TokensHour, "tokens-1h", "Tokens/hr"},
	}
}

// Rate limits of an answer's headers, nil when it states none
func ParseRateLimits(header http.Header, provider string, now time.Time) *RateLimits {
	stated := false
	for name := range header {
		if strings.HasPrefix(strings.ToLower(name), "x-ratelimit-") {
			stated = true
			break
		}
	}
	if !stated {
		return nil
	}
	number := func(name string) float64 {
		n, _ := strconv.ParseFloat(strings.TrimSpace(header.Get(name)), 64)
		return n
	}
	limits := &RateLimits{Captured: now, Provider: provider}
	for _, window := range limits.windows() {
		*window.bucket = Bucket{Limit: int(number("x-ratelimit-limit-" + window.tag)), Remaining: int(number("x-ratelimit-remaining-" + window.tag)),
			Reset: number("x-ratelimit-reset-" + window.tag), Captured: now}
	}
	return limits
}

// Lines of the windows that have a limit under a title saying whose they are and how old: how much each has used and when it resets,
// then a warning for each nearly spent; none before any answer stated limits
func (l *RateLimits) Lines(now time.Time) []string {
	if l == nil || l.Captured.IsZero() {
		return nil
	}
	var rows, warnings []string
	for _, window := range l.windows() {
		b := *window.bucket
		if b.Limit <= 0 {
			continue
		}
		resets := span(b.Left(now))
		rows = append(rows, fmt.Sprintf("%s: %.1f%% · %s/%s used · %s left · resets in %s", window.name, b.Percent(), shortCount(b.Used()), shortCount(b.Limit),
			shortCount(b.Remaining), resets))
		if b.Percent() >= 80 {
			warnings = append(warnings, fmt.Sprintf("⚠ %s at %.0f%% — resets in %s", window.name, b.Percent(), resets))
		}
	}
	if len(rows) == 0 {
		return nil
	}
	freshness := "just now"
	if age := now.Sub(l.Captured).Seconds(); age >= 5 {
		freshness = span(age) + " ago"
	}
	name := "Provider"
	if first, size := utf8.DecodeRuneInString(l.Provider); size > 0 {
		name = string(unicode.ToUpper(first)) + l.Provider[size:]
	}
	return append(append([]string{fmt.Sprintf("⏱️ **%s rate limits** (captured %s)", name, freshness)}, rows...), warnings...)
}

// Count shortened as 33.6K or 8.0M
func shortCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return strconv.Itoa(n)
}

// Seconds as 58s, 2m 14s or 1h 2m
func span(seconds float64) string {
	s := max(int(seconds), 0)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600 && s%60 == 0:
		return fmt.Sprintf("%dm", s/60)
	case s < 3600:
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	case s%3600/60 == 0:
		return fmt.Sprintf("%dh", s/3600)
	}
	return fmt.Sprintf("%dh %dm", s/3600, s%3600/60)
}
