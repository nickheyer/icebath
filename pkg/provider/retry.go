package provider

import (
	"math"
	"math/rand/v2"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var jitterCounter atomic.Uint64

// Z.AI Coding Plan GLM-5.2 overload widens retries past the short attempts to these waits
var zaiCodingOverloadLongBackoff = []time.Duration{30 * time.Second, 60 * time.Second, 90 * time.Second, 120 * time.Second}

const zaiCodingOverloadShortAttempts = 3

// LiveRetryWaitCap is the wait a person feels so longer non rate limit waits are announced when they start
const LiveRetryWaitCap = 60 * time.Second

// RetryAfterCap rejects pathological provider waits while covering Anthropic Tier 1 bucket resets
const RetryAfterCap = 600 * time.Second

// ParseRetryAfter reads a numeric or HTTP date Retry-After value into a duration clamped at zero
func ParseRetryAfter(value string) (time.Duration, bool) {
	text := strings.TrimSpace(value)
	if text == "" {
		return 0, false
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return time.Duration(math.Max(0, f) * float64(time.Second)), true
	}
	when, err := mail.ParseDate(text)
	if err != nil {
		if t, err2 := time.Parse(time.RFC850, text); err2 == nil {
			when = t
		} else if t, err3 := time.Parse(time.ANSIC, text); err3 == nil {
			when = t.UTC()
		} else {
			return 0, false
		}
	}
	d := time.Until(when)
	if d < 0 {
		d = 0
	}
	return d, true
}

// RetryAfterFromHeaders reads Retry-After from a header set trying both casings
func RetryAfterFromHeaders(h http.Header) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	raw := h.Get("Retry-After")
	if raw == "" {
		raw = h.Get("retry-after")
	}
	return ParseRetryAfter(raw)
}

var (
	quotaResetDelayRe    = regexp.MustCompile(`(?i)quotaResetDelay[:\s"]+(\d+(?:\.\d+)?)(ms|s)`)
	resetsInRe           = regexp.MustCompile(`(?i)resets?\s+in\s+(?:(\d+(?:\.\d+)?)\s*(?:h|hr|hrs|hour|hours)\b\s*)?(?:(\d+(?:\.\d+)?)\s*(?:m|min|mins|minute|minutes)\b\s*)?(?:(\d+(?:\.\d+)?)\s*(?:s|sec|secs|second|seconds)\b)?`)
	retryAfterSecondsRe  = regexp.MustCompile(`(?i)retry\s+(?:after\s+)?(\d+(?:\.\d+)?)\s*(?:sec|secs|seconds|s\b)`)
	resetsInSecondsFldRe = regexp.MustCompile(`(?i)resets_in_seconds\W{1,4}(\d+(?:\.\d+)?)`)
)

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// ResetDelayFromMessage parses seconds until reset out of free text provider error copy
func ResetDelayFromMessage(message string) (time.Duration, bool) {
	if message == "" {
		return 0, false
	}
	if m := quotaResetDelayRe.FindStringSubmatch(message); m != nil {
		v := atof(m[1])
		if strings.EqualFold(m[2], "ms") {
			v /= 1000
		}
		return time.Duration(v * float64(time.Second)), true
	}
	if m := retryAfterSecondsRe.FindStringSubmatch(message); m != nil {
		return time.Duration(atof(m[1]) * float64(time.Second)), true
	}
	if m := resetsInSecondsFldRe.FindStringSubmatch(message); m != nil {
		return time.Duration(atof(m[1]) * float64(time.Second)), true
	}
	if m := resetsInRe.FindStringSubmatch(message); m != nil && (m[1] != "" || m[2] != "" || m[3] != "") {
		secs := atof(m[1])*3600 + atof(m[2])*60 + atof(m[3])
		return time.Duration(secs * float64(time.Second)), true
	}
	return 0, false
}

// FormatResetWindow renders a ceilinged ~9h or ~45 min label for quota reopening copy
func FormatResetWindow(d time.Duration) string {
	seconds := int64(d / time.Second)
	if seconds >= 3600 {
		return "~" + strconv.FormatInt((seconds+3599)/3600, 10) + "h"
	}
	return "~" + strconv.FormatInt((seconds+59)/60, 10) + " min"
}

// BackoffOptions tunes the exponential schedule and its jitter ratio
type BackoffOptions struct {
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	JitterRatio float64
}

// DefaultBackoff matches the 5s base, 120s cap, half jitter schedule
var DefaultBackoff = BackoffOptions{BaseDelay: 5 * time.Second, MaxDelay: 120 * time.Second, JitterRatio: 0.5}

// JitteredBackoff returns min(base*2^(attempt-1), max) plus uniform jitter up to ratio times that delay
func JitteredBackoff(attempt int, opts BackoffOptions) time.Duration {
	tick := jitterCounter.Add(1)
	exponent := attempt - 1
	if exponent < 0 {
		exponent = 0
	}
	var delay float64
	if exponent >= 63 || opts.BaseDelay <= 0 {
		delay = float64(opts.MaxDelay)
	} else {
		delay = math.Min(float64(opts.BaseDelay)*math.Pow(2, float64(exponent)), float64(opts.MaxDelay))
	}
	seed := (uint64(time.Now().UnixNano()) ^ (tick * 0x9E3779B9)) & 0xFFFFFFFF
	r := rand.New(rand.NewPCG(seed, tick))
	return time.Duration(delay + r.Float64()*opts.JitterRatio*delay)
}

func errorText(e *APIError) string {
	if e == nil {
		return ""
	}
	return strings.ToLower(e.Message + " " + e.Body + " " + e.Code)
}

// IsZAICodingOverload matches only the narrow 429 code 1305 shape on the Z.AI coding GLM-5.2 endpoint
func IsZAICodingOverload(baseURL, model string, e *APIError) bool {
	if e == nil || e.StatusCode != 429 {
		return false
	}
	text := errorText(e)
	return strings.Contains(strings.ToLower(baseURL), "api.z.ai/api/coding/paas/v4") &&
		strings.Contains(strings.ToLower(model), "glm-5.2") &&
		(strings.Contains(text, "1305") || strings.Contains(text, "temporarily overloaded"))
}

// AdaptiveRateLimitBackoff keeps the default wait except Z.AI overloads which widen to 30 60 90 120s after the short attempts
func AdaptiveRateLimitBackoff(attempt int, baseURL, model string, e *APIError, defaultWait time.Duration) (time.Duration, string) {
	if !IsZAICodingOverload(baseURL, model, e) {
		return defaultWait, ""
	}
	if attempt <= zaiCodingOverloadShortAttempts {
		return defaultWait, "zai_coding_overload_short"
	}
	idx := attempt - zaiCodingOverloadShortAttempts - 1
	if idx > len(zaiCodingOverloadLongBackoff)-1 {
		idx = len(zaiCodingOverloadLongBackoff) - 1
	}
	base := zaiCodingOverloadLongBackoff[idx]
	return JitteredBackoff(1, BackoffOptions{BaseDelay: base, MaxDelay: base, JitterRatio: 0.2}), "zai_coding_overload_long"
}

// ZAICodingOverloadRetryCeiling is one past the last long entry so the loop exhausts the full schedule
func ZAICodingOverloadRetryCeiling() int {
	return zaiCodingOverloadShortAttempts + len(zaiCodingOverloadLongBackoff) + 1
}

// ProviderRetryAfter returns the positive provider declared cooldown from header or retry_after body field
func ProviderRetryAfter(e *APIError) (time.Duration, bool) {
	if e == nil {
		return 0, false
	}
	if e.RetryAfterHeader != nil {
		return *e.RetryAfterHeader, *e.RetryAfterHeader > 0
	}
	if body := decodeJSONObject(e.Body); body != nil {
		target := body
		if nested, ok := body["error"].(map[string]any); ok {
			target = nested
		}
		if d, ok := retryAfterValue(target["retry_after"]); ok {
			return d, d > 0
		}
		return 0, false
	}
	if e.RetryAfter > 0 {
		return e.RetryAfter, true
	}
	return 0, false
}

func retryAfterValue(v any) (time.Duration, bool) {
	switch t := v.(type) {
	case nil, bool:
		return 0, false
	case float64:
		return time.Duration(math.Max(0, t) * float64(time.Second)), true
	case string:
		return ParseRetryAfter(t)
	}
	return 0, false
}
