package pyhttpx

import (
	"math"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// Header value as httpx Headers.get returns it, repeated values joined by a comma and space
func HeaderGet(h http.Header, name, def string) string {
	vals := h.Values(name)
	if len(vals) == 0 {
		return def
	}
	return strings.Join(vals, ", ")
}

// Seconds a Retry-After header asks for, numeric or HTTP date, clamped at zero, false when absent or unparsable
func RetryAfterSeconds(h http.Header) (float64, bool) {
	vals := h.Values("Retry-After")
	if len(vals) == 0 {
		return 0, false
	}
	text := strings.TrimSpace(strings.Join(vals, ", "))
	if text == "" {
		return 0, false
	}
	if f, err := pyint.ParseFloat(text); err == nil {
		if math.IsNaN(f) || f < 0 {
			return 0, true
		}
		return f, true
	}
	when, err := mail.ParseDate(text)
	if err != nil {
		t, err2 := time.Parse(time.RFC850, text)
		if err2 != nil {
			t, err2 = time.Parse(time.ANSIC, text)
			if err2 != nil {
				return 0, false
			}
		}
		when = t
	}
	return math.Max(0, time.Until(when).Seconds()), true
}
