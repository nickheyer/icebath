package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Every update type python-telegram-bot 22 requests with Update.ALL_TYPES
var allUpdateTypes = []string{"message", "edited_message", "channel_post", "edited_channel_post", "inline_query",
	"chosen_inline_result", "callback_query", "shipping_query", "pre_checkout_query", "poll", "poll_answer", "my_chat_member",
	"chat_member", "chat_join_request", "chat_boost", "removed_chat_boost", "message_reaction", "message_reaction_count",
	"business_connection", "business_message", "edited_business_message", "deleted_business_messages", "purchased_paid_media"}

// SDK bot calls of the polling health probes with python-telegram-bot exceptions
type sdkPollingBot struct{ b *bot.Bot }

func (s sdkPollingBot) GetMe(ctx context.Context) (*models.User, error) {
	user, err := s.b.GetMe(ctx)
	return user, ptbError(err)
}

func (s sdkPollingBot) GetWebhookInfo(ctx context.Context) (*models.WebhookInfo, error) {
	info, err := s.b.GetWebhookInfo(ctx)
	return info, ptbError(err)
}

func (s sdkPollingBot) DeleteWebhook(ctx context.Context, dropPendingUpdates bool) (bool, error) {
	ok, err := s.b.DeleteWebhook(ctx, &bot.DeleteWebhookParams{DropPendingUpdates: dropPendingUpdates})
	return ok, ptbError(err)
}

// Long poll updater with the python-telegram-bot 22 lifecycle and retry loop over the getUpdates request
type sdkUpdater struct {
	bot         sdkPollingBot
	request     *ptbRequest
	updatesURL  string
	pollTimeout int
	deliver     func(ctx context.Context, update *models.Update)

	lifecycle    sync.Mutex
	mu           sync.Mutex
	running      bool
	lastUpdateID int64
	stopEvent    *pollEvent
	pollingDone  chan struct{}
	pollingErr   error
	webhook      *webhookServer
}

// Whether the updater is running, true from start until stop even when the polling loop died
func (u *sdkUpdater) Running() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.running
}

// Bootstraps by deleting the webhook then starts the polling loop under the caller's generation context
func (u *sdkUpdater) StartPolling(ctx context.Context, dropPendingUpdates bool, errorCallback func(error)) error {
	u.lifecycle.Lock()
	defer u.lifecycle.Unlock()
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		return &runtimeError{msg: "This Updater is already running!"}
	}
	u.running = true
	u.mu.Unlock()
	if _, err := u.bot.DeleteWebhook(ctx, dropPendingUpdates); err != nil {
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
		return err
	}
	stop := newPollEvent()
	done := make(chan struct{})
	u.mu.Lock()
	u.stopEvent, u.pollingDone, u.pollingErr = stop, done, nil
	u.mu.Unlock()
	go func() {
		err := u.networkRetryLoop(ctx, stop, errorCallback)
		u.mu.Lock()
		u.pollingErr = err
		u.mu.Unlock()
		close(done)
	}()
	return nil
}

// Polling loop that retries timeouts at once, honours flood waits, aborts on an invalid token and reports other errors
func (u *sdkUpdater) networkRetryLoop(ctx context.Context, stop *pollEvent, onErr func(error)) error {
	curInterval := 0.0
	for {
		pollCtx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-stop.ch:
				cancel()
			case <-pollCtx.Done():
			}
		}()
		err := u.pollOnce(pollCtx)
		cancel()
		if stop.isSet() {
			return nil
		}
		switch {
		case err == nil:
			curInterval = 0
		case isPTBError(err, "RetryAfter"):
			slog.Info(fmt.Sprintf("Network Retry Loop (Polling Updates): %s. Adding %v seconds to the specified time.", err.Error(), 0.5))
			curInterval = 0.5 + err.(*TelegramError).RetryAfter
		case isPTBError(err, "TimedOut"):
			slog.Debug(fmt.Sprintf("Network Retry Loop (Polling Updates): Timed out: %s. Retrying immediately.", err.Error()))
			curInterval = 0
		case isPTBError(err, "InvalidToken"):
			slog.Error("Network Retry Loop (Polling Updates): Invalid token. Aborting retry loop.", "error", err)
			return err
		case isPTBError(err, "TelegramError"):
			if onErr != nil {
				onErr(err)
			}
			if curInterval == 0 {
				curInterval = 1
			} else {
				curInterval = math.Min(30, 1.5*curInterval)
			}
		default:
			slog.Error("Something went wrong processing the data received from Telegram. Received data was *not* processed!", "error", err)
			return err
		}
		if curInterval > 0 {
			select {
			case <-stop.ch:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(seconds(curInterval)):
			}
		}
	}
}

// One getUpdates round trip delivering every fetched update in order and confirming the offset
func (u *sdkUpdater) pollOnce(ctx context.Context) error {
	u.mu.Lock()
	offset := u.lastUpdateID
	u.mu.Unlock()
	updates, err := u.getUpdates(ctx, offset, u.pollTimeout)
	if err != nil {
		return err
	}
	if len(updates) == 0 {
		return nil
	}
	if !u.Running() {
		slog.Error("Updater stopped unexpectedly. Pulled updates will be ignored and pulled again on restart.")
		return nil
	}
	for _, update := range updates {
		u.deliver(ctx, update)
	}
	u.mu.Lock()
	u.lastUpdateID = updates[len(updates)-1].ID + 1
	u.mu.Unlock()
	return nil
}

// getUpdates through the instrumented polling request, a missing result standing for the KeyError of hermes
func (u *sdkUpdater) getUpdates(ctx context.Context, offset int64, timeout int) ([]*models.Update, error) {
	body, err := json.Marshal(map[string]any{"offset": offset, "timeout": timeout, "allowed_updates": allUpdateTypes})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.updatesURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.request.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	result, ok := envelope["result"]
	if !ok {
		return nil, &keyError{key: "result"}
	}
	var updates []*models.Update
	if err := json.Unmarshal(result, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// Python KeyError of a missing mapping key
type keyError struct{ key string }

func (e *keyError) Error() string      { return "'" + e.key + "'" }
func (e *keyError) PyTypeName() string { return "KeyError" }

// Stops polling, waits for the loop and confirms fetched updates with a zero timeout getUpdates
func (u *sdkUpdater) Stop(ctx context.Context) error {
	u.lifecycle.Lock()
	defer u.lifecycle.Unlock()
	u.mu.Lock()
	if !u.running {
		u.mu.Unlock()
		return &runtimeError{msg: "This Updater is not running!"}
	}
	u.running = false
	stop, done, server := u.stopEvent, u.pollingDone, u.webhook
	u.webhook = nil
	u.mu.Unlock()
	if server != nil {
		return u.stopWebhook(ctx, server)
	}
	if done == nil {
		return nil
	}
	stop.set()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	u.mu.Lock()
	pollingErr := u.pollingErr
	u.stopEvent, u.pollingDone, u.pollingErr = nil, nil, nil
	offset := u.lastUpdateID
	u.mu.Unlock()
	if pollingErr != nil && pollingErr != context.Canceled {
		return pollingErr
	}
	if _, err := u.getUpdates(ctx, offset, 0); err != nil {
		if isPTBError(err, "TelegramError") {
			slog.Error("Error while calling `get_updates` one more time to mark all fetched updates. Suppressing error to ensure graceful shutdown. When polling for updates is restarted, updates may be fetched again. Please adjust timeouts via `ApplicationBuilder` or the parameter `get_updates_request` of `Bot`.", "error", err)
			return nil
		}
		return err
	}
	return nil
}

// Sets the polling stop event so the retry loop exits on its next tick, leaving running untouched
func (u *sdkUpdater) DisarmRetryLoop() (bool, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stopEvent == nil {
		return false, false
	}
	if u.stopEvent.isSet() {
		return false, true
	}
	u.stopEvent.set()
	return true, true
}
