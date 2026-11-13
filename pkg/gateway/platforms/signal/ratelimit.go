package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/i18n"
)

const (
	maxAttachmentsPerMessage   = 32
	rateLimitBucketCapacity    = 50
	rateLimitDefaultRetryAfter = 4.0
	rateLimitMaxAttempts       = 2
	batchPacingNoticeThreshold = 10 * time.Second
	rpcErrorCodeRateLimit      = -5
)

// Rate limit answer of signal-cli, retryAfter the per token refill window in seconds or zero when absent
type rateLimitError struct {
	message    string
	retryAfter float64
}

func (e *rateLimitError) Error() string { return e.message }

// Server retry window label for logs
func (e *rateLimitError) retryLabel() string {
	if e.retryAfter > 0 {
		return fmt.Sprintf("%.0fs", e.retryAfter)
	}
	return "unknown"
}

// Retry window libsignal leaks through attachment upload failures
var retryAfterTextRE = regexp.MustCompile(`(?i)Retry after (\d+(?:\.\d+)?)\s*second`)

// Seconds signal-cli reports as a JSON number or numeric string, negatives clamped to zero
type flexSeconds float64

func (f *flexSeconds) UnmarshalJSON(raw []byte) error {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(v) {
		*f = 0
		return nil
	}
	*f = flexSeconds(math.Max(0, v))
	return nil
}

// Whether a signal-cli error is a rate limit by typed code, legacy substrings or a leaked retry later text
func isRateLimitRPCError(e *rpcError) bool {
	if e.Code == rpcErrorCodeRateLimit {
		return true
	}
	lower := strings.ToLower(e.Message)
	return strings.Contains(e.Message, "[429]") || strings.Contains(lower, "ratelimit") ||
		strings.Contains(lower, "retrylaterexception") || strings.Contains(lower, "retry after")
}

// Largest per token retry window in the error's result entries, else the one named in its message
func retryAfterFromRPCError(e *rpcError) float64 {
	var data struct {
		Response struct {
			Results []recipientResult `json:"results"`
		} `json:"response"`
	}
	best := 0.0
	if len(e.Data) > 0 && json.Unmarshal(e.Data, &data) == nil {
		for _, r := range data.Response.Results {
			best = math.Max(best, float64(r.RetryAfterSeconds))
		}
	}
	if best > 0 {
		return best
	}
	if m := retryAfterTextRE.FindStringSubmatch(e.Message); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			return math.Max(0, v)
		}
	}
	return 0
}

// Rate limit form of a JSON-RPC error, nil for every other failure
func rateLimitFromError(err error) *rateLimitError {
	if rl, ok := err.(*rateLimitError); ok {
		return rl
	}
	e, ok := err.(*rpcError)
	if !ok || !isRateLimitRPCError(e) {
		return nil
	}
	return &rateLimitError{message: e.Message, retryAfter: retryAfterFromRPCError(e)}
}

// Localized wait label, whole seconds under ninety and whole minutes above
func formatWait(ctx context.Context, wait time.Duration) string {
	s := math.Max(0, wait.Seconds())
	if s < 90 {
		return i18n.T(ctx, "platform.signal.wait_seconds", "", map[string]any{"count": int(math.RoundToEven(s))})
	}
	return i18n.T(ctx, "platform.signal.wait_minutes", "", map[string]any{"count": max(1, int(math.RoundToEven(s/60)))})
}

// Send timeout of thirty seconds without attachments, else five seconds per attachment with a minute floor
func sendTimeout(numAttachments int) time.Duration {
	if numAttachments <= 0 {
		return 30 * time.Second
	}
	return max(60*time.Second, time.Duration(numAttachments)*5*time.Second)
}

// Token bucket model of the per account attachment budget the Signal server enforces
type attachmentScheduler struct {
	mu         sync.Mutex
	capacity   float64
	tokens     float64
	refillRate float64
	lastRefill time.Time
}

// Full bucket refilling one token per default retry window
func newAttachmentScheduler(capacity, defaultRetryAfter float64) *attachmentScheduler {
	return &attachmentScheduler{capacity: capacity, tokens: capacity, refillRate: 1 / defaultRetryAfter, lastRefill: time.Now()}
}

// Process wide scheduler shared by the live adapter and standalone sends
var sharedScheduler = sync.OnceValue(func() *attachmentScheduler {
	s := newAttachmentScheduler(rateLimitBucketCapacity, rateLimitDefaultRetryAfter)
	slog.Info(fmt.Sprintf("Signal scheduler: created (capacity=%d tokens, refill=%.4f/s, %.1fs/token)", int(s.capacity), s.refillRate,
		1/s.refillRate))
	return s
})

// Tokens the bucket holds at now without changing it, the caller holding the lock
func (s *attachmentScheduler) projectedLocked(now time.Time) float64 {
	elapsed := now.Sub(s.lastRefill).Seconds()
	if elapsed > 0 && s.tokens < s.capacity {
		return math.Min(s.capacity, s.tokens+elapsed*s.refillRate)
	}
	return s.tokens
}

// Time until n tokens would be available, read before a possibly blocking acquire
func (s *attachmentScheduler) estimateWait(n int) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	deficit := float64(n) - s.projectedLocked(time.Now())
	if deficit <= 0 {
		return 0
	}
	return time.Duration(deficit / s.refillRate * float64(time.Second))
}

// Blocks until n tokens are available without deducting them, returning the time slept
func (s *attachmentScheduler) acquire(ctx context.Context, n int) (time.Duration, error) {
	if n <= 0 {
		return 0, nil
	}
	if float64(n) > s.capacity {
		return 0, fmt.Errorf("signal scheduler asked for %d tokens, capacity is %d", n, int(s.capacity))
	}
	var slept time.Duration
	for first := true; ; first = false {
		s.mu.Lock()
		now := time.Now()
		s.tokens, s.lastRefill = s.projectedLocked(now), now
		if s.tokens >= float64(n) {
			remaining := s.tokens
			s.mu.Unlock()
			if slept > 0 {
				slog.Debug(fmt.Sprintf("Signal scheduler: tokens sufficient for %d (remaining=%.1f, slept=%.1fs)", n, remaining, slept.Seconds()))
			}
			return slept, nil
		}
		available, deficit, rate := s.tokens, float64(n)-s.tokens, s.refillRate
		s.mu.Unlock()
		wait := time.Duration(deficit / rate * float64(time.Second))
		if first {
			slog.Info(fmt.Sprintf("Signal scheduler: pausing %.1fs for %d tokens (available=%.1f, deficit=%.1f, refill=%.4f/s, %.1fs/token)",
				wait.Seconds(), n, available, deficit, rate, 1/rate))
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return slept, ctx.Err()
		}
		slept += wait
	}
}

// Deducts the attachments of a completed send without crediting refill for the upload window
func (s *attachmentScheduler) reportSend(rpcDuration time.Duration, n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	before := s.tokens
	s.tokens = math.Max(0, before-float64(n))
	s.lastRefill = time.Now()
	after, rate := s.tokens, s.refillRate
	s.mu.Unlock()
	msg := fmt.Sprintf("Signal scheduler: RPC for %d attachments took %.1fs, tokens %.1f to %.1f (refill=%.4f/s)", n,
		rpcDuration.Seconds(), before, after, rate)
	if rpcDuration > 10*time.Second && n > 5 {
		slog.Info(msg)
	} else {
		slog.Debug(msg)
	}
}

// Empties the bucket after a rate limit and calibrates the refill rate from the per token retry window
func (s *attachmentScheduler) feedback(retryAfter float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if retryAfter > 0 {
		if rate := 1 / retryAfter; rate != s.refillRate {
			slog.Info(fmt.Sprintf("Signal scheduler: calibrating refill rate to %.4f tokens/s (server retry_after=%.1fs per token)", rate, retryAfter))
			s.refillRate = rate
		}
	}
	s.tokens = 0
	s.lastRefill = time.Now()
}

// Scheduler state for diagnostic logs
func (s *attachmentScheduler) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("tokens=%.1f capacity=%d refill=%.4f/s (%.1fs/token)", s.projectedLocked(time.Now()), int(s.capacity), s.refillRate,
		1/s.refillRate)
}
