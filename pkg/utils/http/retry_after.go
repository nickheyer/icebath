package httputil

import (
	"encoding/json"
	"math"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

// Wait the Retry-After header asks for, as delay seconds or an HTTP date, never negative; false when the header is absent or unreadable
func RetryAfter(h http.Header) (time.Duration, bool) { return retryAfterText(h.Get("Retry-After")) }

// Wait a Retry-After value of any decoded type asks for: a number of seconds, or text read as the header is; false for nil, booleans and anything else
func RetryAfterValue(v any) (time.Duration, bool) {
	switch n := v.(type) {
	case int:
		return delaySeconds(float64(n))
	case int64:
		return delaySeconds(float64(n))
	case int32:
		return delaySeconds(float64(n))
	case uint:
		return delaySeconds(float64(n))
	case uint64:
		return delaySeconds(float64(n))
	case uint32:
		return delaySeconds(float64(n))
	case float64:
		return delaySeconds(n)
	case float32:
		return delaySeconds(float64(n))
	case json.Number:
		return retryAfterText(n.String())
	case string:
		return retryAfterText(n)
	}
	return 0, false
}

// Wait of a Retry-After text, decimal seconds or an HTTP date
func retryAfterText(text string) (time.Duration, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, false
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return delaySeconds(f)
	}
	when, err := http.ParseTime(text)
	if err != nil {
		if when, err = mail.ParseDate(text); err != nil {
			return 0, false
		}
	}
	return max(0, time.Until(when)), true
}

// Duration of a delay in seconds, a negative delay as none and a delay past the range of time.Duration as the longest one; false for NaN and infinities
func delaySeconds(f float64) (time.Duration, bool) {
	switch {
	case math.IsNaN(f) || math.IsInf(f, 0):
		return 0, false
	case f <= 0:
		return 0, true
	case f >= float64(math.MaxInt64)/float64(time.Second):
		return time.Duration(math.MaxInt64), true
	}
	return time.Duration(f * float64(time.Second)), true
}
