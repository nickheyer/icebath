// HTTP steps every part of icebath takes the same way
package httpx

import (
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

// Wait a Retry-After value asks for: seconds, or an HTTP or mail date to wait until, never less than nothing; false when it asks none
func RetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		return time.Duration(max(seconds, 0) * float64(time.Second)), true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at), 0), true
	}
	if at, err := mail.ParseDate(value); err == nil {
		return max(time.Until(at), 0), true
	}
	return 0, false
}

// Whether a failed status of an OpenAI style API is worth another try: as its x-should-retry header says, else a timeout, conflict, rate
// limit or failure of the service
func Retried(status int, h http.Header) bool {
	switch h.Get("x-should-retry") {
	case "true":
		return true
	case "false":
		return false
	}
	return status == 408 || status == 409 || status == 429 || status >= 500
}

// Wait before the next try of an OpenAI style API: what Retry-After asks within a minute, else half a second doubling with each try up to 8
// seconds, a quarter of it random
func Backoff(attempt int, h http.Header) time.Duration {
	if h != nil {
		if ms, err := strconv.ParseFloat(h.Get("retry-after-ms"), 64); err == nil && ms > 0 && ms <= 60000 {
			return time.Duration(ms * float64(time.Millisecond))
		}
		if after, ok := RetryAfter(h.Get("retry-after")); ok && after > 0 && after <= time.Minute {
			return after
		}
	}
	wait := math.Min(0.5*math.Pow(2, float64(min(attempt, 1000))), 8) * (1 - 0.25*rand.Float64())
	return time.Duration(wait * float64(time.Second))
}

// Whether the host is this machine's loopback: localhost, a name under .localhost or a loopback address, brackets around one dropped
func Loopback(host string) bool {
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
