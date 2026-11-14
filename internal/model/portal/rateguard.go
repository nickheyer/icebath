package portal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	WelcomeLongWait        = 20 * time.Second
	DefaultRateCooldown    = 300 * time.Second
	minResetForBreaker     = 60.0
	rateLimitHeaderPrefix  = "x-ratelimit-"
	rateLimitResetHourly   = "x-ratelimit-reset-requests-1h"
	rateLimitResetMinutely = "x-ratelimit-reset-requests"
)

var rateBucketTags = []string{"requests", "requests-1h", "tokens", "tokens-1h"}

type RateBucket struct {
	Limit     int
	Remaining int
	ResetIn   time.Duration
}

type RateLimitRecord struct {
	Header          http.Header
	ResetAt         time.Time
	DefaultCooldown time.Duration
	Anonymous       bool
}

func FormatRemaining(d time.Duration) string {
	s := max(0, int(d.Seconds()))
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		if sec := s % 60; sec != 0 {
			return fmt.Sprintf("%dm %ds", s/60, sec)
		}
		return fmt.Sprintf("%dm", s/60)
	}
	h, m := s/3600, (s%3600)/60
	if m != 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dh", h)
}

func (a *Account) rateLimitPath(anonymous bool) string {
	if anonymous {
		return home.PortalAnonymousLimit.In(a.home)
	}
	return home.PortalRateLimit.In(a.home)
}

func headerFloat(h http.Header, name string) (float64, bool) {
	if h == nil {
		return 0, false
	}
	return parseFloat(h.Get(name))
}

func retryAfterSeconds(h http.Header) (float64, bool) {
	if h == nil {
		return 0, false
	}
	wait, ok := httpclient.RetryAfter(h, time.Now())
	if !ok {
		return 0, false
	}
	return wait.Seconds(), true
}

func parseResetSeconds(h http.Header) (float64, bool) {
	for _, key := range []string{rateLimitResetHourly, rateLimitResetMinutely} {
		if v, ok := headerFloat(h, key); ok && v > 0 {
			return v, true
		}
	}
	if wait, ok := retryAfterSeconds(h); ok && wait != 0 {
		return wait, true
	}
	return 0, false
}

func floatText(f float64) string {
	if magnitude := math.Abs(f); magnitude != 0 && (magnitude < 1e-4 || magnitude >= 1e16) {
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	text := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(text, ".") {
		text += ".0"
	}
	return text
}

func (a *Account) RecordRateLimit(ctx context.Context, r RateLimitRecord) {
	now := epochOf(time.Now())
	resetAt, known := 0.0, false
	if !r.ResetAt.IsZero() && epochOf(r.ResetAt) > now {
		resetAt, known = epochOf(r.ResetAt), true
	}
	if !known {
		if seconds, ok := parseResetSeconds(r.Header); ok {
			resetAt, known = now+seconds, true
		}
	}
	if !known {
		cooldown := r.DefaultCooldown
		if cooldown <= 0 {
			cooldown = DefaultRateCooldown
		}
		resetAt = now + cooldown.Seconds()
	}
	text := fmt.Sprintf(`{"reset_at": %s, "recorded_at": %s, "reset_seconds": %s}`, floatText(resetAt), floatText(now), floatText(resetAt-now))
	path := a.rateLimitPath(r.Anonymous)
	ctx = a.bind(ctx)
	err := a.homes.Mkdir(ctx, filepath.Dir(path))
	if err == nil {
		err = fileutil.WriteText(ctx, path, text)
	}
	if err != nil {
		slog.Debug("Failed to write Icebath Portal rate limit state", "error", err)
		return
	}
	slog.Info(fmt.Sprintf("Icebath Portal rate limit recorded: resets in %.0fs (at %.0f)", resetAt-now, resetAt))
}

func (a *Account) RateLimitRemaining(anonymous bool) (time.Duration, bool, error) {
	path := a.rateLimitPath(anonymous)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !utf8.Valid(data) {
		return 0, false, fmt.Errorf("portal: rate limit state %s is not UTF-8 text", path)
	}
	parsed, err := parseValue(data)
	if err != nil {
		return 0, false, nil
	}
	state, _ := parsed.(*object)
	if state == nil {
		return 0, false, fmt.Errorf("portal: rate limit state %s does not hold a JSON object", path)
	}
	resetAt := 0.0
	if state.has("reset_at") {
		value, ok := claimNumber(state.get("reset_at"))
		if !ok {
			return 0, false, nil
		}
		resetAt = value
	}
	remaining := resetAt - epochOf(time.Now())
	if remaining > 0 {
		return time.Duration(remaining * float64(time.Second)), true, nil
	}
	_ = os.Remove(path)
	return 0, false, nil
}

func (a *Account) ClearRateLimit(anonymous bool) {
	if err := os.Remove(a.rateLimitPath(anonymous)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("Failed to clear Icebath Portal rate limit state", "error", err)
	}
}

type headerBucket struct {
	remaining    int
	hasRemaining bool
	reset        float64
	hasReset     bool
}

func hasRateLimitHeaders(h http.Header) bool {
	for name := range h {
		if strings.HasPrefix(strings.ToLower(name), rateLimitHeaderPrefix) {
			return true
		}
	}
	return false
}

func parseBuckets(h http.Header) []headerBucket {
	if !hasRateLimitHeaders(h) {
		return nil
	}
	out := []headerBucket{}
	for _, tag := range rateBucketTags {
		var b headerBucket
		if v, ok := headerFloat(h, "x-ratelimit-remaining-"+tag); ok {
			b.remaining, b.hasRemaining = int(math.Trunc(v)), true
		}
		if v, ok := headerFloat(h, "x-ratelimit-reset-"+tag); ok {
			b.reset, b.hasReset = v, true
		}
		if b.hasRemaining || b.hasReset {
			out = append(out, b)
		}
	}
	return out
}

func bucketExhausted(remaining int, hasRemaining bool, reset float64, hasReset bool) bool {
	return hasRemaining && remaining <= 0 && hasReset && reset >= minResetForBreaker
}

func IsGenuineRateLimit(h http.Header, lastKnown []RateBucket) bool {
	for _, b := range parseBuckets(h) {
		if bucketExhausted(b.remaining, b.hasRemaining, b.reset, b.hasReset) {
			return true
		}
	}
	for _, b := range lastKnown {
		if b.Limit <= 0 {
			continue
		}
		if bucketExhausted(b.Remaining, true, b.ResetIn.Seconds(), true) {
			return true
		}
	}
	return false
}

func IsLongWelcomeRateLimit(r *Refusal) bool {
	return r != nil && r.Reason == RefusalRateLimited && float64(r.RetryAfter) >= WelcomeLongWait.Seconds()
}

func WelcomeRefusalFromHeaders(h http.Header, bodyWait time.Duration) Refusal {
	buckets := parseBuckets(h)
	wait, empty := 0.0, false
	for _, b := range buckets {
		if !b.hasRemaining || b.remaining > 0 {
			continue
		}
		empty = true
		if b.hasReset && b.reset > 0 && b.reset > wait {
			wait = b.reset
		}
	}
	if wait == 0 {
		if seconds, ok := retryAfterSeconds(h); ok && seconds != 0 {
			wait = seconds
		}
	}
	if wait == 0 && bodyWait > 0 {
		wait = bodyWait.Seconds()
	}
	reason := RefusalAtCapacity
	if wait >= WelcomeLongWait.Seconds() || empty {
		reason = RefusalRateLimited
	}
	return Refusal{Reason: reason, RetryAfter: max(0, int(math.Trunc(wait))), Alternates: []string{}}
}
