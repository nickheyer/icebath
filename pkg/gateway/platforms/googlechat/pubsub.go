package googlechat

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Records a fatal Pub/Sub failure and hands it to the runner once the pull loop has returned
func (a *GoogleChatAdapter) pubsubFatal(ctx context.Context, code, message string, retryable bool) {
	a.SetFatalError(ctx, code, message, retryable)
	go func() {
		if err := a.NotifyFatalError(context.WithoutCancel(ctx)); err != nil {
			slog.Debug(fmt.Sprintf("[GoogleChat] fatal error handler failed: %v", err))
		}
	}()
}

// Pulls the subscription until stopped, backing off with full jitter and giving up after ten straight failures
func (a *GoogleChatAdapter) runSupervisor(ctx context.Context) {
	a.mu.Lock()
	api, subscription := a.api, a.subscription
	a.mu.Unlock()
	pullSize := max(1, a.maxMessages)
	attempt := 0
	for ctx.Err() == nil {
		batch, err := api.pull(ctx, subscription, pullSize)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var te *tokenError
			switch status := errStatus(err); {
			case status == 401 || (errors.As(err, &te) && !te.retryable):
				a.pubsubFatal(ctx, "pubsub_auth", "Pub/Sub authentication failed; check service-account credentials and gateway logs", false)
				return
			case status == 403:
				a.pubsubFatal(ctx, "pubsub_permission", "SA lacks pubsub.subscriber on the subscription", false)
				return
			}
			attempt++
			slog.Warn(fmt.Sprintf("[GoogleChat] Pub/Sub stream died (attempt %d/%d): %s", attempt, reconnectMaxAttempts, redact(err.Error())))
			if attempt >= reconnectMaxAttempts {
				a.pubsubFatal(ctx, "pubsub_reconnect_exhausted", fmt.Sprintf("Pub/Sub reconnect failed %d times; giving up", attempt), false)
				return
			}
			delay := min(reconnectMaxDelay, reconnectBaseDelay<<(attempt-1))
			if sleepCtx(ctx, time.Duration(rand.Int64N(int64(delay)+1))) != nil {
				return
			}
			continue
		}
		if attempt > 0 {
			slog.Info(fmt.Sprintf("[GoogleChat] Pub/Sub stream reconnected after %d attempts", attempt))
			attempt = 0
		}
		a.handleBatch(ctx, api, subscription, batch)
	}
}

// Handles a pulled batch inside the byte budget, acking what was handled and returning the rest for redelivery
func (a *GoogleChatAdapter) handleBatch(ctx context.Context, api *googleAPI, subscription string, batch []receivedMessage) {
	var ack, nack []string
	used := 0
	for i, rm := range batch {
		data, decodeErr := base64.StdEncoding.DecodeString(rm.Message.Data)
		a.mu.Lock()
		stopping := a.shuttingDown
		a.mu.Unlock()
		if stopping || ctx.Err() != nil || (a.maxBytes > 0 && i > 0 && used+len(data) > a.maxBytes) {
			nack = append(nack, rm.AckID)
			continue
		}
		used += len(data)
		if decodeErr != nil {
			slog.Error(fmt.Sprintf("[GoogleChat] Could not parse Pub/Sub envelope: %v", decodeErr))
		} else {
			a.handlePubSubMessage(ctx, data, rm.Message.Attributes["ce-type"])
		}
		ack = append(ack, rm.AckID)
	}
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
	defer cancel()
	if err := api.acknowledge(settle, subscription, ack); err != nil {
		slog.Warn(fmt.Sprintf("[GoogleChat] Pub/Sub acknowledge failed: %s", redact(err.Error())))
	}
	if err := api.nack(settle, subscription, nack); err != nil {
		slog.Warn(fmt.Sprintf("[GoogleChat] Pub/Sub nack failed: %s", redact(err.Error())))
	}
}

// Routes one Pub/Sub envelope, never failing so a bad envelope is acked instead of redelivered forever
func (a *GoogleChatAdapter) handlePubSubMessage(ctx context.Context, data []byte, ceType string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error(fmt.Sprintf("[GoogleChat] Error in Pub/Sub message handler: %v", r))
		}
	}()
	ev, err := parseEnvelope(data)
	if err != nil {
		slog.Error(fmt.Sprintf("[GoogleChat] Could not parse Pub/Sub envelope: %v", err))
		return
	}
	slog.Debug(fmt.Sprintf("[GoogleChat] Envelope keys=%v, ce-type=%s", ev.keys(), ceType))
	if a.debugRaw {
		dump := redactRaw(string(data))
		if len(dump) > 2000 {
			dump = dump[:2000]
		}
		slog.Debug("[GoogleChat] RAW envelope (redacted): " + dump)
	}
	a.routeEvent(ctx, ev, ceType, true)
}
