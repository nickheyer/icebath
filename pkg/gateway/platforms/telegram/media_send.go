package telegram

import (
	"context"
	"fmt"
	"log/slog"
)

const (
	mediaSendReadTimeout = 60.0
	mediaSendDeadline    = 300.0
)

// Routing, anchor and notification fields every native media send carries, the kwargs hermes splats into send_*
type mediaSendKwargs struct {
	ChatID              any
	ReplyToMessageID    *int
	Thread              threadKwargs
	DisableNotification bool
	ReadTimeout         float64
}

// Reply anchor and base kwargs shared by every native media send
func buildMediaSendKwargs(chatID string, replyTo *string, metadata map[string]any, replyToMode, notificationsMode string) (*int, mediaSendKwargs, error) {
	replyToID := replyToMessageIDForSend(replyTo, metadata, replyToMode)
	thread, err := threadKwargsForSend(metadataThreadID(metadata), metadata, replyToID, replyToMode)
	if err != nil {
		return nil, mediaSendKwargs{}, err
	}
	return replyToID, mediaSendKwargs{ChatID: NormalizeTelegramChatID(chatID), ReplyToMessageID: replyToID, Thread: thread,
		DisableNotification: notificationKwargs(notificationsMode, metadata), ReadTimeout: mediaSendReadTimeout}, nil
}

// Sends one media payload under the chat lock and retries once without the anchor when a DM topic anchor went stale
func sendWithDMTopicReplyAnchorRetry[T any](ctx context.Context, pacer *sendPacer, name string, send func(ctx context.Context, kw mediaSendKwargs) (T, error),
	kw mediaSendKwargs, metadata map[string]any, replyToMessageID *int, mediaLabel string, resetMedia func()) (T, error) {
	var out T
	bounded := func(ctx context.Context, kw mediaSendKwargs) (T, error) {
		v, err := awaitWithThreadDeadline(ctx, func(ctx context.Context) (any, error) { return send(ctx, kw) }, mediaSendDeadline, nil,
			"telegram-media-send", false)
		if err != nil {
			var zero T
			return zero, err
		}
		return v.(T), nil
	}
	err := pacer.withChatSendLock(ctx, kw.ChatID, func(ctx context.Context) error {
		v, sendErr := bounded(ctx, kw)
		if sendErr == nil {
			out = v
			return nil
		}
		if !shouldRetryWithoutDMTopicReplyAnchor(sendErr, metadata, replyToMessageID) {
			return sendErr
		}
		slog.Warn(fmt.Sprintf("[%s] Reply target deleted for Telegram %s, retrying without reply/topic anchor: %s", name, mediaLabel,
			redactTelegramErrorText(sendErr)))
		if resetMedia != nil {
			resetMedia()
		}
		retry := kw
		retry.ReplyToMessageID = nil
		retry.Thread = threadKwargs{}
		v, sendErr = bounded(ctx, retry)
		out = v
		return sendErr
	})
	return out, err
}

// Sends one native media payload with thread routing and the DM topic anchor retry
func sendMedia[T any](ctx context.Context, pacer *sendPacer, name string, send func(ctx context.Context, kw mediaSendKwargs) (T, error),
	chatID string, replyTo *string, metadata map[string]any, replyToMode, notificationsMode, mediaLabel string, resetMedia func()) (T, error) {
	replyToID, kw, err := buildMediaSendKwargs(chatID, replyTo, metadata, replyToMode, notificationsMode)
	if err != nil {
		var zero T
		return zero, err
	}
	return sendWithDMTopicReplyAnchorRetry(ctx, pacer, name, send, kw, metadata, replyToID, mediaLabel, resetMedia)
}
