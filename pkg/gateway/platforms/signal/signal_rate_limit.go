package signal

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	SignalMaxAttachmentsPerMsg         = 32
	SignalRateLimitBucketCapacity      = 50
	SignalRateLimitDefaultRetryAfter   = 4
	SignalRateLimitMaxAttempts         = 2
	SignalBatchPacingNoticeThreshold   = 10.0
	SignalRPCErrorRatelimit            = -5
	signalDefaultSendTimeoutSeconds    = 30.0
	signalSendTimeoutFloorSeconds      = 60.0
	signalSendTimeoutPerAttachmentSecs = 5.0
)

// Rate limit response from the signal-cli RPC, RetryAfter the per token refill window when the server sent one
type SignalRateLimitError struct {
	Message    string
	RetryAfter *float64
}

func (e *SignalRateLimitError) Error() string { return e.Message }

// Scheduler request larger than the bucket capacity
type SignalSchedulerError struct{ Message string }

func (e *SignalSchedulerError) Error() string { return e.Message }

var retryAfterRe, _ = pyre.Compile(`(?i)Retry after (\d+(?:\.\d+)?)\s*second`)

var processStart = time.Now()

// Monotonic seconds and context aware sleep, swappable in tests
var (
	monotonic   = func() float64 { return time.Since(processStart).Seconds() }
	signalSleep = func(ctx context.Context, seconds float64) error {
		timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
)

// Message text of an RPC error object or of any other error value
func errorMessage(err any) string {
	if m, ok := err.(map[string]any); ok {
		v, present := m["message"]
		if !present {
			return ""
		}
		return pyrepr.Str(v)
	}
	if e, ok := err.(error); ok {
		return e.Error()
	}
	return pyrepr.Str(err)
}

// Mapping value of a nested error field, an error where Python attribute access on a non dict raises
func dictOrEmpty(v any) (map[string]any, error) {
	if !pyrepr.Truthy(v) {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(v))
	}
	return m, nil
}

// Per token Retry-After from the result entries of a rate limit error, else from its message text
func extractRetryAfterSeconds(err any) (float64, bool, error) {
	if m, ok := err.(map[string]any); ok {
		data, e := dictOrEmpty(m["data"])
		if e != nil {
			return 0, false, e
		}
		response, e := dictOrEmpty(data["response"])
		if e != nil {
			return 0, false, e
		}
		best, found := 0.0, false
		switch results := response["results"].(type) {
		case []any:
			for _, r := range results {
				rm, isDict := r.(map[string]any)
				if !isDict || !pyrepr.Truthy(rm["retryAfterSeconds"]) {
					continue
				}
				if c, ok := pyhttpx.RetryAfterValue(rm["retryAfterSeconds"]); ok && (!found || c > best) {
					best, found = c, true
				}
			}
		case string, map[string]any:
		default:
			if pyrepr.Truthy(results) {
				return 0, false, fmt.Errorf("TypeError: '%s' object is not iterable", pyrepr.TypeName(results))
			}
		}
		if found {
			return best, true, nil
		}
	}
	match, _ := retryAfterRe.FindStringMatch(errorMessage(err))
	if match == nil {
		return 0, false, nil
	}
	v, ok := pyhttpx.RetryAfterValue(match.Groups()[1].String())
	return v, ok, nil
}

// Whether a signal-cli RPC error is a rate limit by typed code, legacy substrings or a leaked retry later
func isSignalRateLimitError(err any) bool {
	if m, ok := err.(map[string]any); ok && pyrepr.Equal(m["code"], SignalRPCErrorRatelimit) {
		return true
	}
	message := errorMessage(err)
	lower := pystr.Lower(message)
	return strings.Contains(message, "[429]") || strings.Contains(lower, "ratelimit") || strings.Contains(lower, "retrylaterexception") || strings.Contains(lower, "retry after")
}

// Localized wait label, seconds under ninety and whole minutes above
func formatWait(ctx context.Context, seconds float64) string {
	s := math.Max(0, seconds)
	if s < 90 {
		return i18n.T(ctx, "platform.signal.wait_seconds", "", map[string]any{"count": int(math.RoundToEven(s))})
	}
	return i18n.T(ctx, "platform.signal.wait_minutes", "", map[string]any{"count": max(1, int(math.RoundToEven(s/60)))})
}

// Send RPC timeout of thirty seconds without attachments, else five seconds each with a sixty second floor
func signalSendTimeout(numAttachments int) float64 {
	if numAttachments <= 0 {
		return signalDefaultSendTimeoutSeconds
	}
	return math.Max(signalSendTimeoutFloorSeconds, signalSendTimeoutPerAttachmentSecs*float64(numAttachments))
}

// Process wide token bucket model of the Signal server attachment budget, acquires serialized through a lock
type SignalAttachmentScheduler struct {
	mu         sync.Mutex
	Capacity   float64
	Tokens     float64
	RefillRate float64
	LastRefill float64
}

// Full bucket of the given capacity refilling one token per default retry after
func NewSignalAttachmentScheduler(capacity, defaultRetryAfter float64) *SignalAttachmentScheduler {
	return &SignalAttachmentScheduler{Capacity: capacity, Tokens: capacity, RefillRate: 1.0 / defaultRetryAfter, LastRefill: monotonic()}
}

// Tokens the bucket would hold at now without changing it
func (s *SignalAttachmentScheduler) projectedTokens(now float64) float64 {
	elapsed := now - s.LastRefill
	if elapsed > 0 && s.Tokens < s.Capacity {
		return math.Min(s.Capacity, s.Tokens+elapsed*s.RefillRate)
	}
	return s.Tokens
}

// Credits the refill since the last update
func (s *SignalAttachmentScheduler) refill() {
	now := monotonic()
	s.Tokens = s.projectedTokens(now)
	s.LastRefill = now
}

// Seconds until n tokens would be available, informational and lock free
func (s *SignalAttachmentScheduler) EstimateWait(n int) float64 {
	deficit := float64(n) - s.projectedTokens(monotonic())
	if deficit <= 0 {
		return 0
	}
	return deficit / s.RefillRate
}

// Blocks until n tokens are available without deducting them and returns the seconds slept
func (s *SignalAttachmentScheduler) Acquire(ctx context.Context, n int) (float64, error) {
	if n <= 0 {
		return 0, nil
	}
	if float64(n) > s.Capacity {
		return 0, &SignalSchedulerError{fmt.Sprintf("Signal scheduler was called requesting %d tokens (max is %s)", n, pyrepr.Str(s.Capacity))}
	}
	totalSlept, firstPass := 0.0, true
	for {
		s.mu.Lock()
		s.refill()
		if s.Tokens >= float64(n) {
			if !firstPass || totalSlept > 0 {
				slog.Debug(fmt.Sprintf("Signal scheduler: tokens sufficient for %d (remaining=%.1f, total_slept=%.1fs)", n, s.Tokens, totalSlept))
			}
			s.mu.Unlock()
			return totalSlept, nil
		}
		deficit := float64(n) - s.Tokens
		tokens, rate := s.Tokens, s.RefillRate
		s.mu.Unlock()
		wait := deficit / rate
		if firstPass {
			slog.Info(fmt.Sprintf("Signal scheduler: pausing %.1fs for %d tokens (available=%.1f, deficit=%.1f, refill=%.4f/s ≈ %.1fs/token)", wait, n, tokens, deficit, rate, 1.0/rate))
			firstPass = false
		}
		if err := signalSleep(ctx, wait); err != nil {
			return totalSlept, err
		}
		totalSlept += wait
	}
}

// Deducts the attachments of a completed send RPC without crediting refill for the upload window
func (s *SignalAttachmentScheduler) ReportRPCDuration(rpcDuration float64, nAttachments int) {
	if nAttachments <= 0 {
		return
	}
	s.mu.Lock()
	before := s.Tokens
	s.Tokens = math.Max(0, before-float64(nAttachments))
	s.LastRefill = monotonic()
	after, rate := s.Tokens, s.RefillRate
	s.mu.Unlock()
	msg := fmt.Sprintf("Signal scheduler: RPC for %d att took %.1fs — tokens %.1f → %.1f (deducted=%d, no upload refill credited, refill=%.4fs⁻¹)", nAttachments, rpcDuration, before, after, nAttachments, rate)
	if rpcDuration > 10 && nAttachments > 5 {
		slog.Info(msg)
	} else {
		slog.Debug(msg)
	}
}

// Empties the bucket after a 429 and calibrates the refill rate from the per token retry after
func (s *SignalAttachmentScheduler) Feedback(retryAfter *float64, nAttempted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if retryAfter != nil && *retryAfter > 0 {
		if newRate := 1.0 / *retryAfter; newRate != s.RefillRate {
			slog.Info(fmt.Sprintf("Signal scheduler: calibrating refill_rate to %.4f tokens/sec (server retry_after=%.1fs per token)", newRate, *retryAfter))
			s.RefillRate = newRate
		}
	}
	s.Tokens = 0
	s.LastRefill = monotonic()
}

// Read only scheduler state for diagnostic logging
func (s *SignalAttachmentScheduler) State() *pyjson.Object {
	perToken := math.Inf(1)
	if s.RefillRate > 0 {
		perToken = pyint.Round(1.0/s.RefillRate, 1)
	}
	return pyjson.ObjectOf("tokens", pyint.Round(s.projectedTokens(monotonic()), 1), "capacity", int(s.Capacity),
		"refill_rate", pyint.Round(s.RefillRate, 4), "refill_seconds_per_token", perToken)
}

var scheduler struct {
	sync.Mutex
	s *SignalAttachmentScheduler
}

// Process wide scheduler created on first access
func GetScheduler() *SignalAttachmentScheduler {
	scheduler.Lock()
	defer scheduler.Unlock()
	if scheduler.s == nil {
		scheduler.s = NewSignalAttachmentScheduler(SignalRateLimitBucketCapacity, SignalRateLimitDefaultRetryAfter)
		slog.Info(fmt.Sprintf("Signal scheduler: created (capacity=%d tokens, refill=%.4f/s ≈ %.1fs/token)", int(scheduler.s.Capacity), scheduler.s.RefillRate, 1.0/scheduler.s.RefillRate))
	}
	return scheduler.s
}

// Drops the cached scheduler so the next access builds a fresh one
func resetScheduler() {
	scheduler.Lock()
	defer scheduler.Unlock()
	scheduler.s = nil
}
