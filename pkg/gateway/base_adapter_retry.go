package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Longest server requested retry wait the send path sleeps inline before handing redelivery to the ledger
const sendRetryInlineWaitCapSecs = 60.0

// Backoff sleep of the send retry loop, swapped by tests the way hermes tests patch asyncio.sleep
var sendRetrySleep = func(ctx context.Context, seconds float64) error {
	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Uniform jitter in [0, 1) added to every retry backoff
var sendRetryJitter = func() float64 { return rand.Float64() }

// Runner seam resolving the routed home and terminal policy scope of post handler delivery
type MediaDeliveryScopeRunner interface {
	MediaDeliveryScopeForSource(ctx context.Context, source *SessionSource) (context.Context, func(), error)
}

// Runner seam naming the live adapter of a source after a reconnect swapped the registry entry
type DeliveryAdapterRunner interface {
	DeliveryAdapterFor(ctx context.Context, source *SessionSource) (PlatformAdapter, error)
}

// Text and ttl of a handler response, the ttl zero where the adapter cannot delete messages
func (b *BasePlatformAdapter) unwrapEphemeral(ctx context.Context, response any) (any, int) {
	reply, ok := response.(*EphemeralReply)
	if !ok {
		return response, 0
	}
	ttl := 0
	if reply.TTLSeconds != nil {
		ttl = *reply.TTLSeconds
	} else {
		ttl = b.ephemeralSystemTTLDefault(ctx)
	}
	if _, deletes := b.Self().(MessageDeleter); ttl > 0 && !deletes {
		ttl = 0
	}
	return reply.Text(), ttl
}

// Delivery scope of the source through the runner, the caller's context when absent or failing
func (b *BasePlatformAdapter) mediaDeliveryScope(ctx context.Context, source *SessionSource) (context.Context, func()) {
	resolver, ok := b.GatewayRunner.(MediaDeliveryScopeRunner)
	if !ok || source == nil {
		return ctx, func() {}
	}
	scoped, release, err := resolver.MediaDeliveryScopeForSource(ctx, source)
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] Failed to resolve media delivery scope: %v", b.Self().Name(), err))
		return ctx, func() {}
	}
	if release == nil {
		release = func() {}
	}
	return scoped, release
}

// Runner's current adapter of the same platform for a new final send, this adapter otherwise
func (b *BasePlatformAdapter) finalDeliveryAdapter(ctx context.Context, source *SessionSource) PlatformAdapter {
	self := b.Self()
	resolver, ok := b.GatewayRunner.(DeliveryAdapterRunner)
	if !ok {
		return self
	}
	live, err := resolver.DeliveryAdapterFor(ctx, source)
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] Failed to resolve live adapter for final delivery", self.Name()))
		return self
	}
	if live != nil && live.Base().Platform == b.Platform {
		return live
	}
	return self
}

// Text of a handler response, empty when the handler produced nothing to send
func handlerResponseText(response any) string {
	switch v := response.(type) {
	case nil:
		return ""
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	}
	return fmt.Sprint(response)
}

// Runs the handler and sends its reply inline with retry, threading and ephemeral deletion, no session lifecycle
func (b *BasePlatformAdapter) dispatchInlineReply(ctx context.Context, event *MessageEvent, logCmd *string) error {
	threadMeta := threadMetadataForEvent(event)
	handler := b.MessageHandler()
	if handler == nil {
		return errors.New("'NoneType' object is not callable")
	}
	response, err := handler(ctx, event)
	if err != nil {
		return err
	}
	unwrapped, ephTTL := b.unwrapEphemeral(ctx, response)
	text := handlerResponseText(unwrapped)
	if text == "" {
		return nil
	}
	if logCmd != nil {
		slog.Info(fmt.Sprintf("[%s] Sending command '/%s' response (%d chars) to %s", b.Self().Name(), *logCmd,
			pystr.Len(text), event.Source.ChatID))
	}
	replyTo := ""
	if anchor := replyAnchorForEvent(event); anchor != nil {
		replyTo = *anchor
	}
	result, err := b.SendWithRetry(ctx, event.Source.ChatID, text, replyTo, markNotifyMetadata(threadMeta), 2, 2.0)
	if err != nil {
		return err
	}
	if ephTTL > 0 && result.Success && pyrepr.Truthy(result.MessageID) {
		b.ScheduleEphemeralDelete(ctx, event.Source.ChatID, fmt.Sprint(result.MessageID), ephTTL)
	}
	return nil
}

// Error text of a send result, empty when the result carries none
func sendResultErrorText(result SendResult) string {
	switch v := result.Error.(type) {
	case nil:
		return ""
	case string:
		return v
	case error:
		return v.Error()
	}
	if !pyrepr.Truthy(result.Error) {
		return ""
	}
	return fmt.Sprint(result.Error)
}

// Whether a failed send classifies as transient, retried with backoff instead of the plain text fallback
func sendFailureIsTransient(result SendResult, errorText string) bool {
	return result.Retryable || isRateLimitedError(errorText) || result.RetryAfter != nil || isRetryableError(errorText)
}

// Sends with exponential backoff on transient errors, plain text fallback on permanent ones, a notice after exhaustion
func (b *BasePlatformAdapter) SendWithRetry(ctx context.Context, chatID, content, replyTo string, metadata map[string]any, maxRetries int, baseDelay float64) (SendResult, error) {
	self := b.Self()
	send := func(text string) (SendResult, error) {
		return self.Send(ctx, chatID, text, replyTo, metadata)
	}
	sendAgain := func(previous SendResult) (*SendResult, error) {
		if !IsPartialDelivery(previous) {
			r, err := send(content)
			return &r, err
		}
		return self.ResumePartialSend(ctx, chatID, previous, replyTo, metadata)
	}
	result, err := send(content)
	if err != nil {
		return result, err
	}
	if result.Success || self.SendRetryIsFinal(result) {
		return result, nil
	}
	errorStr := sendResultErrorText(result)
	isNetwork := sendFailureIsTransient(result, errorStr)
	if !isNetwork && isTimeoutError(errorStr) {
		return result, nil
	}
	if isNetwork {
		serverRetryAfter := result.RetryAfter
		exhausted := true
		for attempt := 1; attempt <= maxRetries; attempt++ {
			var backoff float64
			if serverRetryAfter == nil {
				backoff = baseDelay * math.Pow(2, float64(attempt-1))
			} else if backoff = *serverRetryAfter; backoff > sendRetryInlineWaitCapSecs {
				slog.Error(fmt.Sprintf("[%s] Server asked to retry after %.0fs (> %.0fs inline cap); returning "+
					"typed failure for redelivery instead of sleeping: %s", self.Name(), backoff, sendRetryInlineWaitCapSecs, errorStr))
				return result, nil
			}
			delay := backoff + sendRetryJitter()
			serverRetryAfter = nil
			slog.Warn(fmt.Sprintf("[%s] Send failed (attempt %d/%d, retrying in %.1fs): %s", self.Name(), attempt, maxRetries, delay, errorStr))
			if err := sendRetrySleep(ctx, delay); err != nil {
				return result, err
			}
			resumed, err := sendAgain(result)
			if err != nil {
				return result, err
			}
			if resumed == nil {
				slog.Warn(fmt.Sprintf("[%s] Split send partly delivered and the remainder cannot be resumed safely; "+
					"not re-sending the whole payload (would duplicate the visible head): %s", self.Name(), errorStr))
				return result, nil
			}
			result = *resumed
			if result.Success {
				slog.Info(fmt.Sprintf("[%s] Send succeeded on retry %d", self.Name(), attempt))
				return result, nil
			}
			errorStr = sendResultErrorText(result)
			if self.SendRetryIsFinal(result) {
				return result, nil
			}
			if result.RetryAfter != nil {
				serverRetryAfter = result.RetryAfter
			}
			if !sendFailureIsTransient(result, errorStr) {
				exhausted = false
				break
			}
		}
		if exhausted {
			if isRateLimitedError(errorStr) || result.RetryAfter != nil {
				slog.Error(fmt.Sprintf("[%s] Rate-limited send exhausted retries; returning typed failure "+
					"for redelivery (no notice sent inside active flood penalty): %s", self.Name(), errorStr))
				return result, nil
			}
			slog.Error(fmt.Sprintf("[%s] Failed to deliver response after %d retries: %s", self.Name(), maxRetries, errorStr))
			notice := i18n.T(ctx, "gateway.notify.delivery_failed_retry", "", nil)
			if _, nerr := send(notice); nerr != nil {
				slog.Debug(fmt.Sprintf("[%s] Could not send delivery-failure notice: %v", self.Name(), nerr))
			}
			return result, nil
		}
	}
	if IsPartialDelivery(result) {
		slog.Warn(fmt.Sprintf("[%s] Send failed after partial delivery: %s — not re-sending as plain text", self.Name(), errorStr))
		return result, nil
	}
	slog.Warn(fmt.Sprintf("[%s] Send failed: %s — trying plain-text fallback", self.Name(), errorStr))
	fallback, err := self.SendPlainFallback(ctx, chatID, content, replyTo, metadata)
	if err != nil {
		return fallback, err
	}
	if !fallback.Success {
		slog.Error(fmt.Sprintf("[%s] Fallback send also failed: %v", self.Name(), fallback.Error))
	}
	return fallback, nil
}

// Whether a failed send must be returned as is, never retried nor sent as plain text, false by default
func (b *BasePlatformAdapter) SendRetryIsFinal(result SendResult) bool { return false }

// Delivers only the undelivered remainder of a split payload, nil when the adapter cannot resume
func (b *BasePlatformAdapter) ResumePartialSend(ctx context.Context, chatID string, result SendResult, replyTo string, metadata map[string]any) (*SendResult, error) {
	return nil, nil
}

// Last resort send after a permanent failure, the content as plain text under the fallback banner
func (b *BasePlatformAdapter) SendPlainFallback(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (SendResult, error) {
	self := b.Self()
	head := pystr.Slice(content, 0, 3500)
	visible := i18n.T(ctx, "gateway.notify.plain_fallback_prefix", "", map[string]any{"content": head})
	text := self.WarningText(ctx, visible, &head, "", chatID, metadata)
	return self.Send(ctx, chatID, *text, replyTo, metadata)
}
