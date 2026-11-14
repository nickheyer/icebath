package usage

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type RateLimitBucket struct {
	Limit        int64     `json:"limit"`
	Remaining    int64     `json:"remaining"`
	ResetSeconds float64   `json:"reset_seconds"`
	CapturedAt   time.Time `json:"captured_at"`
}

func (b RateLimitBucket) Used() int64 { return max(0, b.Limit-b.Remaining) }

func (b RateLimitBucket) UsagePct() float64 {
	if b.Limit <= 0 {
		return 0
	}
	return float64(b.Used()) / float64(b.Limit) * 100.0
}

func (b RateLimitBucket) secondsLeft(now time.Time) float64 {
	left := b.ResetSeconds - now.Sub(b.CapturedAt).Seconds()
	if !(left > 0) {
		return 0
	}
	return left
}

type RateLimitState struct {
	RequestsMin  RateLimitBucket `json:"requests_min"`
	RequestsHour RateLimitBucket `json:"requests_hour"`
	TokensMin    RateLimitBucket `json:"tokens_min"`
	TokensHour   RateLimitBucket `json:"tokens_hour"`
	CapturedAt   time.Time       `json:"captured_at"`
	Provider     string          `json:"provider"`
}

func (s *RateLimitState) HasData() bool { return s != nil && !s.CapturedAt.IsZero() }

func rateInt(raw string, present bool) (int64, bool) {
	if !present {
		return 0, true
	}
	f, err := parseFloatText(raw)
	if err != nil || math.IsNaN(f) {
		return 0, true
	}
	if math.IsInf(f, 0) {
		return 0, false
	}
	if f >= math.MaxInt64 {
		return math.MaxInt64, true
	}
	if f <= math.MinInt64 {
		return math.MinInt64, true
	}
	return int64(f), true
}

func rateFloat(raw string, present bool) float64 {
	if !present {
		return 0
	}
	f, err := parseFloatText(raw)
	if err != nil {
		return 0
	}
	return f
}

func ParseRateLimitHeaders(h http.Header, provider string) *RateLimitState {
	lowered := lowerHeaders(h)
	found := false
	for k := range lowered {
		if strings.HasPrefix(k, "x-ratelimit-") {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	now := time.Now()
	bucket := func(tag string) (RateLimitBucket, bool) {
		limitRaw, hasLimit := lowered["x-ratelimit-limit-"+tag]
		remainingRaw, hasRemaining := lowered["x-ratelimit-remaining-"+tag]
		resetRaw, hasReset := lowered["x-ratelimit-reset-"+tag]
		limit, ok := rateInt(limitRaw, hasLimit)
		if !ok {
			return RateLimitBucket{}, false
		}
		remaining, ok := rateInt(remainingRaw, hasRemaining)
		if !ok {
			return RateLimitBucket{}, false
		}
		return RateLimitBucket{Limit: limit, Remaining: remaining, ResetSeconds: rateFloat(resetRaw, hasReset), CapturedAt: now}, true
	}
	s := &RateLimitState{CapturedAt: now, Provider: provider}
	for _, b := range []struct {
		dst *RateLimitBucket
		tag string
	}{{&s.RequestsMin, "requests"}, {&s.RequestsHour, "requests-1h"}, {&s.TokensMin, "tokens"}, {&s.TokensHour, "tokens-1h"}} {
		parsed, ok := bucket(b.tag)
		if !ok {
			return nil
		}
		*b.dst = parsed
	}
	return s
}

func fmtCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fixed(float64(n)/1_000_000, 1) + "M"
	case n >= 1_000:
		return fixed(float64(n)/1_000, 1) + "K"
	}
	return strconv.FormatInt(n, 10)
}

func fmtSeconds(seconds float64) string {
	var s int64
	switch {
	case math.IsNaN(seconds) || seconds <= 0:
		s = 0
	case seconds >= math.MaxInt64:
		s = math.MaxInt64
	default:
		s = int64(seconds)
	}
	if s < 60 {
		return strconv.FormatInt(s, 10) + "s"
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

func rateBar(pct float64) string {
	filled := max(0, min(20, int(pct/100.0*20)))
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", 20-filled) + "]"
}

func bucketLine(label string, b RateLimitBucket, now time.Time) string {
	if b.Limit <= 0 {
		return "  " + padRight(label, 14) + "  (no data)"
	}
	pct := b.UsagePct()
	return fmt.Sprintf("  %s %s %5.1f%%  %s/%s used  (%s left, resets in %s)",
		padRight(label, 14), rateBar(pct), pct, fmtCount(b.Used()), fmtCount(b.Limit), fmtCount(b.Remaining), fmtSeconds(b.secondsLeft(now)))
}

func FormatRateLimitDisplay(s *RateLimitState) string {
	if !s.HasData() {
		return "No rate limit data yet — make an API request first."
	}
	now := time.Now()
	age := now.Sub(s.CapturedAt).Seconds()
	freshness := "just now"
	switch {
	case age >= 60:
		freshness = fmtSeconds(age) + " ago"
	case age >= 5:
		freshness = strconv.Itoa(int(age)) + "s ago"
	}
	providerLabel := "Provider"
	if s.Provider != "" {
		providerLabel = titleCase(s.Provider)
	}
	labeled := []struct {
		label  string
		bucket RateLimitBucket
	}{{"Requests/min", s.RequestsMin}, {"Requests/hr", s.RequestsHour}, {"Tokens/min", s.TokensMin}, {"Tokens/hr", s.TokensHour}}
	lines := []string{providerLabel + " Rate Limits (captured " + freshness + "):", ""}
	for i, entry := range labeled {
		if i == 2 {
			lines = append(lines, "")
		}
		lines = append(lines, bucketLine(entry.label, entry.bucket, now))
	}
	var warnings []string
	for _, entry := range labeled {
		if entry.bucket.Limit > 0 && entry.bucket.UsagePct() >= 80 {
			warnings = append(warnings, "  ⚠ "+strings.ToLower(entry.label)+" at "+fixed(entry.bucket.UsagePct(), 0)+
				"% — resets in "+fmtSeconds(entry.bucket.secondsLeft(now)))
		}
	}
	if len(warnings) > 0 {
		lines = append(lines, "")
		lines = append(lines, warnings...)
	}
	return strings.Join(lines, "\n")
}

func FormatRateLimitCompact(s *RateLimitState) string {
	if !s.HasData() {
		return "No rate limit data."
	}
	now := time.Now()
	raw := func(n int64) string { return strconv.FormatInt(n, 10) }
	var parts []string
	for _, w := range []struct {
		tag    string
		bucket RateLimitBucket
		count  func(int64) string
		reset  bool
	}{
		{"RPM", s.RequestsMin, raw, false},
		{"RPH", s.RequestsHour, fmtCount, true},
		{"TPM", s.TokensMin, fmtCount, false},
		{"TPH", s.TokensHour, fmtCount, true},
	} {
		if w.bucket.Limit <= 0 {
			continue
		}
		part := w.tag + ": " + w.count(w.bucket.Remaining) + "/" + w.count(w.bucket.Limit)
		if w.reset {
			part += " (resets " + fmtSeconds(w.bucket.secondsLeft(now)) + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " | ")
}
