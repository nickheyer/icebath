package signal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Paths grouped into sends of at most the per message attachment cap
func attachmentBatches(paths []string) [][]string {
	var batches [][]string
	for start := 0; start < len(paths); start += maxAttachmentsPerMessage {
		batches = append(batches, paths[start:min(start+maxAttachmentsPerMessage, len(paths))])
	}
	return batches
}

// Result of one attachment batch, outcome set on delivery
type batchResult struct {
	outcome     *sendOutcome
	err         error
	rateLimited bool
	permanent   bool
}

// Sends one batch under the scheduler, retrying once after a rate limit or a transient failure
func sendBatch(ctx context.Context, sched *attachmentScheduler, c *rpcClient, params map[string]any, n int, label string) batchResult {
	for attempt := 1; ; attempt++ {
		if _, err := sched.acquire(ctx, n); err != nil {
			return batchResult{err: err, permanent: true}
		}
		start := time.Now()
		raw, err := c.call(ctx, "send", params, "", sendTimeout(n))
		duration := time.Since(start)
		if ctx.Err() != nil {
			return batchResult{err: ctx.Err(), permanent: true}
		}
		if err == nil {
			outcome := decodeSendOutcome(raw)
			delivered, failure, rl := outcome.verdict()
			if delivered {
				sched.reportSend(duration, n)
				if partial := outcome.partialRateLimit(); partial != nil {
					sched.feedback(partial.retryAfter)
				}
				if failure != "" {
					slog.Warn(fmt.Sprintf("Signal batch %s: delivered with some recipients failing: %s", label, failure))
				}
				slog.Info(fmt.Sprintf("Signal batch %s: %d attachments sent in %.1fs (attempt %d/%d)", label, n, duration.Seconds(), attempt,
					rateLimitMaxAttempts))
				return batchResult{outcome: outcome}
			}
			if rl != nil {
				err = rl
			} else {
				err = errors.New(failure)
			}
		}
		if rl := rateLimitFromError(err); rl != nil {
			sched.feedback(rl.retryAfter)
			if attempt >= rateLimitMaxAttempts {
				slog.Error(fmt.Sprintf("Signal: rate-limit retries exhausted on batch %s (%d attachments lost, server retry_after=%s)", label, n,
					rl.retryLabel()))
				return batchResult{err: rl, rateLimited: true}
			}
			slog.Warn(fmt.Sprintf("Signal: rate-limited on batch %s (attempt %d/%d, server retry_after=%s), scheduler will pace the retry",
				label, attempt, rateLimitMaxAttempts, rl.retryLabel()))
			continue
		}
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) {
			slog.Error(fmt.Sprintf("Signal: RPC error on batch %s (%d attachments): %v", label, n, err))
			return batchResult{err: err, permanent: true}
		}
		slog.Error(fmt.Sprintf("Signal: send failed for batch %s (%d attachments, attempt %d/%d, rpc_duration=%.1fs): %v", label, n, attempt,
			rateLimitMaxAttempts, duration.Seconds(), err))
		if attempt >= rateLimitMaxAttempts {
			return batchResult{err: err}
		}
		backoff := time.Duration(1<<attempt) * time.Second
		slog.Info(fmt.Sprintf("Signal: retrying batch %s after %.1fs backoff", label, backoff.Seconds()))
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return batchResult{err: ctx.Err(), permanent: true}
		}
	}
}
