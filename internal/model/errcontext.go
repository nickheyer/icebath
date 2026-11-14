package model

import (
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/wire"
)

type ErrorContext struct {
	Reason  string
	Message string
	ResetAt time.Time
}

var vendorResetHeaders = []string{
	"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens",
	"anthropic-ratelimit-requests-reset", "anthropic-ratelimit-tokens-reset",
}

var durationComponent = regexp.MustCompile(`(\d+(?:\.\d+)?)(ms|h|m|s)`)

type resetSlot struct {
	at  time.Time
	set bool
}

func (s *resetSlot) put(at time.Time, ok bool) {
	s.set = true
	if ok {
		s.at = at
	}
}

func ContextOf(err error) ErrorContext {
	now := time.Now()
	var out ErrorContext
	var reset resetSlot
	var header http.Header
	var ae *wire.APIError
	var se *wire.StreamError
	var payload wire.Value
	switch {
	case errors.As(err, &ae):
		header = ae.Header
		payload, _ = wire.ParseValue(ae.Body())
	case errors.As(err, &se):
		payload = streamErrorBody(se)
	}
	if obj := payload.Get("error"); obj.IsObject() {
		payload = obj
	}
	if payload.IsObject() {
		contextFromPayload(&out, &reset, payload, now)
	}
	if len(header) > 0 {
		if !reset.set {
			if d, ok := httpclient.RetryAfter(header, now); ok {
				reset.put(now.Add(d), true)
			}
		}
		if raw := header.Get("X-Ratelimit-Reset"); raw != "" && !reset.set {
			reset.put(absoluteText(raw))
		}
		if !reset.set {
			vendorReset(&reset, header, now)
		}
	}
	if out.Message == "" {
		if text := strings.TrimSpace(safeText(err)); text != "" {
			out.Message = truncateRunes(text, 500)
		}
	}
	if !reset.set && out.Message != "" {
		if d, ok := ResetDelayFromMessage(out.Message); ok {
			reset.put(now.Add(d), true)
		}
	}
	out.ResetAt = reset.at
	return out
}

func contextFromPayload(out *ErrorContext, reset *resetSlot, payload wire.Value, now time.Time) {
	reason := payload.Get("code")
	if !reason.Truthy() {
		reason = payload.Get("type")
	}
	if !reason.Truthy() {
		reason = payload.Get("error")
	}
	if s, ok := reason.Str(); ok && strings.TrimSpace(s) != "" {
		out.Reason = strings.TrimSpace(s)
	}
	message := payload.Get("message")
	if !message.Truthy() {
		message = payload.Get("error_description")
	}
	if !message.Truthy() && payload.Get("error").IsString() {
		message = payload.Get("error")
	}
	if s, ok := message.Str(); ok && strings.TrimSpace(s) != "" {
		out.Message = strings.TrimSpace(s)
	}
	switch {
	case presentValue(payload.Get("resets_at")):
		reset.put(absoluteValue(payload.Get("resets_at")))
	case presentValue(payload.Get("reset_at")):
		reset.put(absoluteValue(payload.Get("reset_at")))
	default:
		if f, ok := numberValue(payload.Get("resets_in_seconds")); ok {
			reset.put(now.Add(secondsDuration(f)), true)
		}
	}
	if !reset.set {
		if seconds, ok := retryAfterValue(payload.Get("retry_after"), now); ok {
			reset.put(now.Add(secondsDuration(seconds)), true)
		}
	}
}

func vendorReset(reset *resetSlot, header http.Header, now time.Time) {
	for _, name := range vendorResetHeaders {
		value := header.Get(name)
		if strings.TrimSpace(value) == "" {
			continue
		}
		seconds, ok := durationSeconds(value)
		if !ok {
			if at, found := absoluteText(value); found {
				seconds, ok = at.Sub(now).Seconds(), true
			}
		}
		if ok && seconds > 0 {
			reset.put(now.Add(secondsDuration(seconds)), true)
			return
		}
	}
}

func durationSeconds(text string) (float64, bool) {
	raw := strings.ToLower(strings.TrimSpace(text))
	if raw == "" {
		return 0, false
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f, true
	}
	parts := durationComponent.FindAllStringSubmatch(raw, -1)
	if len(parts) == 0 {
		return 0, false
	}
	var joined strings.Builder
	total := 0.0
	for _, p := range parts {
		joined.WriteString(p[1] + p[2])
		n, _ := strconv.ParseFloat(p[1], 64)
		switch p[2] {
		case "h":
			total += n * 3600
		case "m":
			total += n * 60
		case "s":
			total += n
		case "ms":
			total += n * 0.001
		}
	}
	if joined.String() != raw {
		return 0, false
	}
	return total, true
}

func numberValue(v wire.Value) (float64, bool) {
	switch v.Kind() {
	case wire.KindNumber:
		return v.Float()
	case wire.KindBool:
		if v.True() {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func retryAfterValue(v wire.Value, now time.Time) (float64, bool) {
	switch v.Kind() {
	case wire.KindNumber:
		f, ok := v.Float()
		if !ok {
			return 0, false
		}
		return max(0, f), true
	case wire.KindString:
		d, ok := httpclient.ParseRetryAfter(v.Text(), now)
		return d.Seconds(), ok
	}
	return 0, false
}

func epochTime(seconds float64) (time.Time, bool) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || math.Abs(seconds) > 1e15 {
		return time.Time{}, false
	}
	whole, frac := math.Modf(seconds)
	return time.Unix(int64(whole), int64(frac*1e9)), true
}

func epochSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

func absoluteValue(v wire.Value) (time.Time, bool) {
	switch v.Kind() {
	case wire.KindNumber, wire.KindBool:
		f, _ := numberValue(v)
		if f <= 0 {
			return time.Time{}, false
		}
		if f > 1_000_000_000_000 {
			f /= 1000
		}
		return epochTime(f)
	case wire.KindString:
		return absoluteText(v.Text())
	}
	return time.Time{}, false
}

func absoluteText(text string) (time.Time, bool) {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return time.Time{}, false
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		if f > 1_000_000_000_000 {
			f /= 1000
		}
		return epochTime(f)
	}
	return auth.ParseISOTimestamp(raw)
}

func secondsDuration(seconds float64) time.Duration {
	if seconds <= 0 || math.IsNaN(seconds) {
		return 0
	}
	if seconds >= math.MaxInt64/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(seconds * float64(time.Second))
}

var (
	quotaResetDelay    = regexp.MustCompile(`(?i)quotaResetDelay[:\s"]+(\d+(?:\.\d+)?)(ms|s)`)
	resetsIn           = regexp.MustCompile(`(?i)resets?\s+in\s+(?:(\d+(?:\.\d+)?)\s*(?:h|hr|hrs|hour|hours)\b\s*)?(?:(\d+(?:\.\d+)?)\s*(?:m|min|mins|minute|minutes)\b\s*)?(?:(\d+(?:\.\d+)?)\s*(?:s|sec|secs|second|seconds)\b)?`)
	retryAfterSeconds  = regexp.MustCompile(`(?i)retry\s+(?:after\s+)?(\d+(?:\.\d+)?)\s*(?:sec|secs|seconds|s\b)`)
	resetsInSecondsKey = regexp.MustCompile(`(?i)resets_in_seconds\W{1,4}(\d+(?:\.\d+)?)`)
)

func ResetDelayFromMessage(message string) (time.Duration, bool) {
	if message == "" {
		return 0, false
	}
	if m := quotaResetDelay.FindStringSubmatch(message); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		if strings.EqualFold(m[2], "ms") {
			v /= 1000
		}
		return secondsDuration(v), true
	}
	if m := retryAfterSeconds.FindStringSubmatch(message); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return secondsDuration(v), true
	}
	if m := resetsInSecondsKey.FindStringSubmatch(message); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return secondsDuration(v), true
	}
	if m := resetsIn.FindStringSubmatch(message); m != nil && (m[1] != "" || m[2] != "" || m[3] != "") {
		h, _ := strconv.ParseFloat(m[1], 64)
		mins, _ := strconv.ParseFloat(m[2], 64)
		s, _ := strconv.ParseFloat(m[3], 64)
		return secondsDuration(h*3600 + mins*60 + s), true
	}
	return 0, false
}

func FormatResetWindow(d time.Duration) string {
	seconds := int64(d / time.Second)
	if seconds >= 3600 {
		return "~" + strconv.FormatInt(ceilDiv(seconds, 3600), 10) + "h"
	}
	return "~" + strconv.FormatInt(ceilDiv(seconds, 60), 10) + " min"
}

func ceilDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) == (b < 0) {
		q++
	}
	return q
}
