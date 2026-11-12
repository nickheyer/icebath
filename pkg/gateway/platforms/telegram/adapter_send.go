package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python TypeError raised where hermes code unpacks or calls with the wrong shape
type typeError struct{ msg string }

func (e *typeError) Error() string      { return e.msg }
func (e *typeError) PyTypeName() string { return "TypeError" }

// Lowercased str() of an error, empty for nil
func errLower(err error) string {
	if err == nil {
		return ""
	}
	return pystr.Lower(pyrepr.ExceptionMessage(err))
}

// Server retry_after of a RetryAfter error, nil for every other error
func retryAfterOf(err error) *float64 {
	var te *TelegramError
	if errors.As(err, &te) && te != nil && isPTBError(te, "RetryAfter") {
		wait := te.RetryAfter
		return &wait
	}
	return nil
}

// Send fields of one MarkdownV2 or plain sendMessage call
type textSend struct {
	chatID      any
	replyTo     *int
	thread      threadKwargs
	disableNote bool
	replyMarkup models.ReplyMarkup
}

// SDK params of a sendMessage with the routing, link preview and notification fields
func (a *TelegramAdapter) sendMessageParams(kw textSend, text string, parseMode models.ParseMode) *bot.SendMessageParams {
	params := &bot.SendMessageParams{ChatID: kw.chatID, Text: text, ParseMode: parseMode, LinkPreviewOptions: linkPreviewKwargs(a.disableLinkPreviews),
		DisableNotification: kw.disableNote, ReplyMarkup: kw.replyMarkup}
	if kw.replyTo != nil {
		params.ReplyParameters = &models.ReplyParameters{MessageID: *kw.replyTo}
	}
	if kw.thread.MessageThreadID != nil {
		params.MessageThreadID = *kw.thread.MessageThreadID
	}
	if kw.thread.DirectMessagesTopicID != nil {
		params.DirectMessagesTopicID = *kw.thread.DirectMessagesTopicID
	}
	return params
}

// sendMessage bounded by the text send deadline
func (a *TelegramAdapter) boundedSendMessage(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
	b := a.currentBot()
	if b == nil {
		return nil, &runtimeError{msg: "Not connected"}
	}
	v, err := awaitWithThreadDeadline(ctx, func(c context.Context) (any, error) {
		m, sendErr := b.SendMessage(c, params)
		return m, ptbError(sendErr)
	}, textSendDeadline, nil, "telegram-send", false)
	if err != nil {
		return nil, err
	}
	m, _ := v.(*models.Message)
	return m, nil
}

// Sends a chunk as MarkdownV2, resending it as stripped plain text on a parse rejection
func (a *TelegramAdapter) sendChunkMarkdownOrPlain(ctx context.Context, chunk string, kw textSend) (*models.Message, error) {
	msg, err := a.boundedSendMessage(ctx, a.sendMessageParams(kw, chunk, models.ParseModeMarkdown))
	if err == nil {
		return msg, nil
	}
	low := errLower(err)
	if strings.Contains(low, "parse") || strings.Contains(low, "markdown") {
		slog.Warn(fmt.Sprintf("[%s] MarkdownV2 parse failed, falling back to plain text: %s", a.Name(), pyrepr.ExceptionMessage(err)))
		return a.boundedSendMessage(ctx, a.sendMessageParams(kw, stripMdv2(chunk), ""))
	}
	return nil, err
}

// One chunk delivered with routing, three attempts and the thread, anchor and flood recoveries
func (a *TelegramAdapter) sendChunkWithRetries(ctx context.Context, chatID, chunk string, index int, replyTo *string,
	metadata map[string]any, threadID *string, usedThreadFallback bool) (*models.Message, bool, *gateway.SendResult, error) {
	retriedThreadNotFound := false
	privateDMTopicSend, dmTopicReplyToOff, replyToID := chunkReplyRouting(replyTo, metadata, threadID, index, a.replyToMode)
	if privateDMTopicSend && replyToID == nil && !dmTopicReplyToOff {
		return nil, usedThreadFallback, &gateway.SendResult{Success: false, Error: dmTopicMissingAnchorError, Retryable: false}, nil
	}
	thread, err := threadKwargsForSend(threadID, metadata, replyToID, a.replyToMode)
	if err != nil {
		return nil, usedThreadFallback, nil, err
	}
	if usedThreadFallback && thread.MessageThreadID != nil {
		thread.MessageThreadID = nil
	}
	effectiveThreadID := thread.MessageThreadID
	for attempt := 0; attempt < 3; attempt++ {
		kw := textSend{chatID: NormalizeTelegramChatID(chatID), replyTo: replyToID, thread: thread,
			disableNote: notificationKwargs(a.notificationsMode, metadata)}
		msg, sendErr := a.sendChunkMarkdownOrPlain(ctx, chunk, kw)
		if sendErr == nil {
			return msg, usedThreadFallback, nil, nil
		}
		if errors.Is(sendErr, context.Canceled) {
			return nil, usedThreadFallback, nil, sendErr
		}
		if isPTBError(sendErr, "NetworkError") {
			if isPTBError(sendErr, "BadRequest") {
				if isThreadNotFoundError(sendErr) && effectiveThreadID != nil {
					if privateDMTopicSend || pyTruthy(metadata["telegram_dm_topic_created_for_send"]) {
						return nil, usedThreadFallback, &gateway.SendResult{Success: false, Error: pyrepr.ExceptionMessage(sendErr)}, nil
					}
					if !retriedThreadNotFound {
						retriedThreadNotFound = true
						slog.Warn(fmt.Sprintf("[%s] Thread %d not found, retrying once with same thread_id", a.Name(), *effectiveThreadID))
						continue
					}
					slog.Warn(fmt.Sprintf("[%s] Thread %d not found, retrying without message_thread_id", a.Name(), *effectiveThreadID))
					a.core.pruneStaleDMTopicBinding(ctx, chatID, *effectiveThreadID, metadata)
					usedThreadFallback = true
					effectiveThreadID = nil
					thread = threadKwargs{}
					continue
				}
				if strings.Contains(errLower(sendErr), "message to be replied not found") && replyToID != nil {
					safe := redactTelegramErrorText(sendErr)
					if privateDMTopicSend {
						return nil, usedThreadFallback, &gateway.SendResult{Success: false, Error: safe}, nil
					}
					slog.Warn(fmt.Sprintf("[%s] Reply target deleted, retrying without reply_to: %s", a.Name(), safe))
					replyToID = nil
					if dmTopicFallback(metadata) {
						thread = threadKwargs{}
					} else if thread, err = threadKwargsForSend(threadID, metadata, nil, a.replyToMode); err != nil {
						return nil, usedThreadFallback, nil, err
					}
					effectiveThreadID = thread.MessageThreadID
					continue
				}
				return nil, usedThreadFallback, nil, sendErr
			}
			isPoolTimeout := looksLikePoolTimeout(sendErr)
			if isPTBError(sendErr, "TimedOut") && !looksLikeConnectTimeout(sendErr) && !isPoolTimeout {
				return nil, usedThreadFallback, nil, sendErr
			}
			if isPoolTimeout {
				a.polling.drainGeneralConnectionsAfterPoolTimeout(ctx)
			}
			if attempt >= 2 {
				return nil, usedThreadFallback, nil, sendErr
			}
			wait := math.Pow(2, float64(attempt))
			slog.Warn(fmt.Sprintf("[%s] Network error on send (attempt %d/3), retrying in %ds: %s", a.Name(), attempt+1, int(wait),
				redactTelegramErrorText(sendErr)))
			if !sleepCtx(ctx, wait) {
				return nil, usedThreadFallback, nil, ctx.Err()
			}
			continue
		}
		retryAfter := retryAfterOf(sendErr)
		if retryAfter == nil && !strings.Contains(errLower(sendErr), "retry after") {
			return nil, usedThreadFallback, nil, sendErr
		}
		wait := 1.0
		if retryAfter != nil {
			wait = *retryAfter
		}
		safe := redactTelegramErrorText(sendErr)
		if wait > floodInlineWaitCapSecs {
			slog.Warn(fmt.Sprintf("[%s] Telegram flood control on send (retry_after=%.1fs > %.0fs); failing closed instead of sleeping: %s",
				a.Name(), wait, floodInlineWaitCapSecs, safe))
			result := a.pacer.recordSendFloodCooldown(chatID, wait)
			return nil, usedThreadFallback, &result, nil
		}
		if attempt < 2 {
			slog.Warn(fmt.Sprintf("[%s] Telegram flood control on send (attempt %d/3), retrying in %.1fs: %s", a.Name(), attempt+1, wait, safe))
			if !sleepCtx(ctx, wait) {
				return nil, usedThreadFallback, nil, ctx.Err()
			}
			continue
		}
		slog.Warn(fmt.Sprintf("[%s] Telegram flood control on send persisted across %d attempts; failing closed so the delivery ledger owns the wait: %s",
			a.Name(), attempt+1, safe))
		result := a.pacer.recordSendFloodCooldown(chatID, wait)
		return nil, usedThreadFallback, &result, nil
	}
	return nil, usedThreadFallback, nil, &typeError{msg: "cannot unpack non-iterable NoneType object"}
}

// Re-arms typing after an intermediate send, scheduled once per chat and rate limited, never on the final reply
func (a *TelegramAdapter) retriggerTyping(ctx context.Context, chatID string, metadata map[string]any) {
	if pyTruthy(metadata["notify"]) || (a.Config != nil && !a.Config.TypingIndicator) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if inFlight := a.retriggerTasks[chatID]; inFlight != nil && !inFlight.Done() {
		return
	}
	now := monotonicNow()
	if last, ok := a.retriggerAt[chatID]; ok && now-last < a.typingRetriggerInterval {
		return
	}
	a.retriggerAt[chatID] = now
	var task *gateway.AdapterTask
	task = gateway.SpawnAdapterTask(context.WithoutCancel(ctx), func(c context.Context) error {
		_ = a.SendTyping(c, chatID, metadata)
		return nil
	})
	a.retriggerTasks[chatID] = task
	task.AddDoneCallback(func() {
		a.mu.Lock()
		if a.retriggerTasks[chatID] == task {
			delete(a.retriggerTasks, chatID)
		}
		a.mu.Unlock()
	})
	a.TrackBackgroundTask(task)
}

// Sends a message, waiting briefly for a reconnect and delegating to a replacement adapter
func (a *TelegramAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if a.currentBot() == nil {
		if live := a.replacementTelegramAdapter(); live != nil {
			return live.Send(ctx, chatID, content, replyTo, metadata)
		}
		if a.IsPermanentFatal() {
			return gateway.SendResult{Success: false, Error: "Not connected", Retryable: false}, nil
		}
		reconnected, err := a.waitForReconnection(ctx)
		if err != nil {
			return gateway.SendResult{}, err
		}
		if !reconnected {
			return gateway.SendResult{Success: false, Error: "Not connected", Retryable: !a.IsPermanentFatal()}, nil
		}
		live := a.replacementTelegramAdapter()
		if a.currentBot() == nil && live != nil {
			return live.Send(ctx, chatID, content, replyTo, metadata)
		}
		if a.currentBot() == nil {
			return gateway.SendResult{Success: false, Error: "Not connected", Retryable: true}, nil
		}
	}
	if a.SendPathDegraded() {
		return gateway.SendResult{Success: false, Error: "send_path_degraded", Retryable: true}, nil
	}
	if content == "" || pystr.Strip(content) == "" {
		return gateway.SendResult{Success: true, MessageID: nil}, nil
	}
	var result gateway.SendResult
	err := a.pacer.withChatSendLock(ctx, chatID, func(c context.Context) error {
		var lockedErr error
		result, lockedErr = a.sendTextLocked(c, chatID, content, optional(replyTo), metadata)
		return lockedErr
	})
	return result, err
}

// Pointer to a non empty string, nil for empty
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

var chunkIndicatorSuffixRE = regexp.MustCompile(` \((\d+)/(\d+)\)$`)

// Send body under the chat lock, the rich fast path first then MarkdownV2 chunks
func (a *TelegramAdapter) sendTextLocked(ctx context.Context, chatID, content string, replyTo *string, metadata map[string]any) (gateway.SendResult, error) {
	if cooldown := a.pacer.sendFloodCooldownRemaining(chatID); cooldown != nil {
		slog.Warn(fmt.Sprintf("[%s] Telegram flood control still active for chat %s (%.0fs left); refusing locally without an API call",
			a.Name(), chatID, *cooldown))
		return floodCapResult(*cooldown), nil
	}
	if remaining := a.pacer.chatOutboundSlotRemaining(chatID); remaining > 0 {
		slog.Debug(fmt.Sprintf("[%s] pacing send for chat %s (shared send+edit budget: slot in %.1fs)", a.Name(), chatID, remaining))
		if !sleepCtx(ctx, remaining) {
			return gateway.SendResult{}, ctx.Err()
		}
	}
	a.pacer.holdChatOutboundSlot(chatID)
	var chunks, delivered []string
	if a.shouldAttemptRich(content, metadata) {
		richResult, err := a.trySendRich(ctx, chatID, content, replyTo, metadata)
		if err != nil {
			return a.classifiedPartial(err, chunks, delivered), nil
		}
		if richResult != nil {
			if richResult.Success {
				a.retriggerTyping(ctx, chatID, metadata)
			}
			return *richResult, nil
		}
	}
	chunks = gateway.TruncateMessage(a.FormatMessage(content), maxMessageLength, gateway.UTF16Len)
	if len(chunks) > 1 {
		for i, chunk := range chunks {
			chunks[i] = separateChunkIndicatorFromFence(chunkIndicatorSuffixRE.ReplaceAllString(chunk, ` \($1/$2\)`))
		}
	}
	result, err := a.sendChunks(ctx, chatID, chunks, &delivered, replyTo, metadata)
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return gateway.SendResult{}, err
		}
		return a.classifiedPartial(err, chunks, delivered), nil
	}
	return result, nil
}

// Classified failure of an escaped send error marked with the partial delivery contract
func (a *TelegramAdapter) classifiedPartial(err error, chunks, delivered []string) gateway.SendResult {
	classified := a.classifySendException(err)
	var undelivered []string
	if len(delivered) <= len(chunks) {
		undelivered = chunks[len(delivered):]
	}
	return withPartialSend(classified, undelivered, delivered, classified.Retryable)
}

// Failed result of an exception escaping the chunk loop, retryable meaning non delivery is certain
func (a *TelegramAdapter) classifySendException(err error) gateway.SendResult {
	safe := redactTelegramErrorText(err)
	slog.Error(fmt.Sprintf("[%s] Failed to send Telegram message: %s", a.Name(), safe))
	low := errLower(err)
	kind := gateway.ClassifySendError(err, "")
	if strings.Contains(low, "message_too_long") || strings.Contains(low, "too long") {
		slog.Debug(fmt.Sprintf("[%s] send() content too long, falling back to new-message continuation", a.Name()))
		tooLong := "too_long"
		return gateway.SendResult{Success: false, Error: "message_too_long", ErrorKind: &tooLong}
	}
	isTimeout := isPTBError(err, "TimedOut") || strings.Contains(low, "timed out")
	return gateway.SendResult{Success: false, Error: safe, Retryable: looksLikeConnectTimeout(err) || looksLikePoolTimeout(err) || !isTimeout,
		ErrorKind: &kind}
}

// Delivers formatted chunks in order, appending each landed id, a mid loop refusal returning the partial result
func (a *TelegramAdapter) sendChunks(ctx context.Context, chatID string, chunks []string, delivered *[]string, replyTo *string,
	metadata map[string]any) (gateway.SendResult, error) {
	threadID := metadataThreadID(metadata)
	requestedThreadID, err := messageThreadIDForSend(threadID)
	if err != nil {
		return gateway.SendResult{}, err
	}
	usedThreadFallback := false
	prior := len(*delivered)
	for _, chunk := range chunks {
		msg, fallback, refusal, sendErr := a.sendChunkWithRetries(ctx, chatID, chunk, len(*delivered), replyTo, metadata, threadID, usedThreadFallback)
		if sendErr != nil {
			return gateway.SendResult{}, sendErr
		}
		if refusal != nil {
			return withPartialSend(*refusal, chunks[len(*delivered)-prior:], *delivered, true), nil
		}
		usedThreadFallback = fallback
		*delivered = append(*delivered, strconv.Itoa(msg.ID))
	}
	a.retriggerTyping(ctx, chatID, metadata)
	var first any
	if len(*delivered) > 0 {
		first = (*delivered)[0]
	}
	var requested any
	if requestedThreadID != nil {
		requested = *requestedThreadID
	}
	return gateway.SendResult{Success: true, MessageID: first, RawResponse: map[string]any{
		"message_ids": append([]string(nil), *delivered...), "requested_thread_id": requested, "thread_fallback": usedThreadFallback}}, nil
}

// Marks a split send failure after earlier chunks landed with the partial overflow contract
func withPartialSend(result gateway.SendResult, undelivered, delivered []string, tailCertain bool) gateway.SendResult {
	if len(delivered) == 0 {
		return result
	}
	raw := map[string]any{}
	if existing, ok := result.RawResponse.(map[string]any); ok {
		for k, v := range existing {
			raw[k] = v
		}
	}
	continuation := make([]any, 0, len(delivered))
	for _, id := range delivered[1:] {
		continuation = append(continuation, id)
	}
	raw["partial_overflow"] = true
	raw["delivered_chunks"] = len(delivered)
	raw["total_chunks"] = len(delivered) + len(undelivered)
	raw["last_message_id"] = delivered[len(delivered)-1]
	raw["continuation_message_ids"] = pyrepr.Tuple(continuation)
	if tailCertain && len(undelivered) > 0 {
		tail := make(pyrepr.Tuple, 0, len(undelivered))
		for _, c := range undelivered {
			tail = append(tail, c)
		}
		ids := make(pyrepr.Tuple, 0, len(delivered))
		for _, id := range delivered {
			ids = append(ids, id)
		}
		raw["undelivered_chunks"] = tail
		raw["delivered_message_ids"] = ids
	}
	result.RawResponse = raw
	return result
}

// Strings of a tuple or list value
func tupleStrings(v any) []string {
	var items []any
	switch t := v.(type) {
	case pyrepr.Tuple:
		items = t
	case []any:
		items = t
	case []string:
		return append([]string(nil), t...)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, pyrepr.Str(item))
	}
	return out
}

// Sends only the chunks a partial send could not deliver, nil when the tail was not certain undelivered
func (a *TelegramAdapter) ResumePartialSend(ctx context.Context, chatID string, result gateway.SendResult, replyTo string,
	metadata map[string]any) (*gateway.SendResult, error) {
	raw, _ := result.RawResponse.(map[string]any)
	undelivered := tupleStrings(raw["undelivered_chunks"])
	if len(undelivered) == 0 || a.currentBot() == nil {
		return nil, nil
	}
	delivered := tupleStrings(raw["delivered_message_ids"])
	prior := len(delivered)
	var out gateway.SendResult
	err := a.pacer.withChatSendLock(ctx, chatID, func(c context.Context) error {
		if cooldown := a.pacer.sendFloodCooldownRemaining(chatID); cooldown != nil {
			out = withPartialSend(floodCapResult(*cooldown), undelivered, delivered, true)
			return nil
		}
		sent, sendErr := a.sendChunks(c, chatID, undelivered, &delivered, optional(replyTo), metadata)
		if sendErr != nil {
			if errors.Is(sendErr, context.Canceled) && c.Err() != nil {
				return sendErr
			}
			classified := a.classifySendException(sendErr)
			out = withPartialSend(classified, undelivered[len(delivered)-prior:], delivered, classified.Retryable)
			return nil
		}
		out = sent
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Sends a status message or edits the previous one of the same chat and status key
func (a *TelegramAdapter) SendOrUpdateStatus(ctx context.Context, chatID, statusKey, content string, metadata map[string]any) (gateway.SendResult, error) {
	key := [2]string{chatID, statusKey}
	a.mu.Lock()
	cachedID, cached := a.statusMessageIDs.Get(key)
	a.mu.Unlock()
	if cached {
		result, err := a.editMessage(ctx, chatID, cachedID, content, true, metadata)
		if err != nil {
			return result, err
		}
		if result.Success {
			a.mu.Lock()
			if current, ok := a.statusMessageIDs.Get(key); ok && current == cachedID && pyTruthy(result.MessageID) {
				a.statusMessageIDs.Set(key, pyrepr.Str(result.MessageID))
			}
			a.mu.Unlock()
			return result, nil
		}
		a.mu.Lock()
		a.statusMessageIDs.Delete(key)
		a.mu.Unlock()
	}
	result, err := a.Send(ctx, chatID, content, "", metadata)
	if err != nil {
		return result, err
	}
	if result.Success && pyTruthy(result.MessageID) {
		a.mu.Lock()
		if a.statusMessageIDs.Len() >= statusMessageIDsMax {
			keys := a.statusMessageIDs.Keys()
			for _, stale := range keys[:statusMessageIDsMax/2] {
				a.statusMessageIDs.Delete(stale)
			}
		}
		a.statusMessageIDs.Set(key, pyrepr.Str(result.MessageID))
		a.mu.Unlock()
	}
	return result, nil
}

// editMessageText with normalized ids, an empty parse mode sending plain text
func (a *TelegramAdapter) editText(ctx context.Context, chatID, messageID, text string, parseMode models.ParseMode) error {
	id, err := pyIntOf(messageID)
	if err != nil {
		return err
	}
	b := a.currentBot()
	if b == nil {
		return &runtimeError{msg: "Not connected"}
	}
	_, err = awaitWithThreadDeadline(ctx, func(c context.Context) (any, error) {
		m, editErr := b.EditMessageText(c, &bot.EditMessageTextParams{ChatID: NormalizeTelegramChatID(chatID), MessageID: id, Text: text,
			ParseMode: parseMode})
		return m, ptbError(editErr)
	}, textSendDeadline, nil, "telegram-send", false)
	return err
}

// MarkdownV2 edit with a plain fallback, true on a not modified no op
func (a *TelegramAdapter) editMarkdownOrPlain(ctx context.Context, chatID, messageID, formatted, plain, warnFmt string) (bool, error) {
	err := a.editText(ctx, chatID, messageID, formatted, models.ParseModeMarkdown)
	if err == nil {
		return false, nil
	}
	if strings.Contains(errLower(err), "not modified") {
		return true, nil
	}
	slog.Warn(fmt.Sprintf(warnFmt, a.Name(), redactTelegramErrorText(err)))
	return false, a.editText(ctx, chatID, messageID, plain, "")
}

// Edits a sent message, splitting an oversized final edit into continuations
func (a *TelegramAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	return a.editMessage(ctx, chatID, messageID, content, finalize, nil)
}

// Edit body with the routing metadata of the stream
func (a *TelegramAdapter) editMessage(ctx context.Context, chatID, messageID, content string, finalize bool, metadata map[string]any) (gateway.SendResult, error) {
	if a.currentBot() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if !finalize && gateway.UTF16Len(content) <= maxMessageLength && a.pacer.chatOutboundSlotRemaining(chatID) > 0 {
		slog.Debug(fmt.Sprintf("[%s] skipping interim edit for chat %s (shared send+edit budget: slot busy)", a.Name(), chatID))
		return gateway.SendResult{Success: true, MessageID: messageID, RawResponse: map[string]any{"skipped": true}}, nil
	}
	a.pacer.holdChatOutboundSlot(chatID)
	if finalize && a.richEligible(content) {
		richResult, err := a.tryEditRich(ctx, chatID, messageID, content)
		if err != nil {
			return gateway.SendResult{}, err
		}
		if richResult != nil {
			return *richResult, nil
		}
	}
	previewKey := [2]string{chatID, messageID}
	saturated := false
	if finalize {
		a.mu.Lock()
		delete(a.lastOverflowPreview, previewKey)
		a.mu.Unlock()
	}
	if gateway.UTF16Len(content) > maxMessageLength {
		if finalize {
			return a.editOverflowSplit(ctx, chatID, messageID, content, finalize, metadata)
		}
		content = a.truncateStreamOverflowPreview(content)
		saturated = true
		a.mu.Lock()
		same := a.lastOverflowPreview[previewKey] == content
		_, has := a.lastOverflowPreview[previewKey]
		a.mu.Unlock()
		if has && same {
			return gateway.SendResult{Success: true, MessageID: messageID}, nil
		}
	} else if !finalize {
		a.mu.Lock()
		delete(a.lastOverflowPreview, previewKey)
		a.mu.Unlock()
	}
	var editErr error
	if !finalize {
		editErr = a.editText(ctx, chatID, messageID, content, "")
		if editErr == nil {
			if saturated {
				a.mu.Lock()
				a.lastOverflowPreview[previewKey] = content
				a.mu.Unlock()
			}
			return gateway.SendResult{Success: true, MessageID: messageID}, nil
		}
	} else {
		plain := content
		if content != "" {
			plain = stripMdv2(content)
		}
		_, editErr = a.editMarkdownOrPlain(ctx, chatID, messageID, a.FormatMessage(content), plain,
			"[%s] MarkdownV2 edit failed, falling back to plain text: %s")
		if editErr == nil {
			return gateway.SendResult{Success: true, MessageID: messageID}, nil
		}
	}
	if errors.Is(editErr, context.Canceled) && ctx.Err() != nil {
		return gateway.SendResult{}, editErr
	}
	return a.editFailure(ctx, chatID, messageID, content, finalize, metadata, previewKey, editErr)
}

// Recovery ladder of a failed edit, the not modified, too long, flood and transient branches
func (a *TelegramAdapter) editFailure(ctx context.Context, chatID, messageID, content string, finalize bool, metadata map[string]any,
	previewKey [2]string, e error) (gateway.SendResult, error) {
	low := errLower(e)
	if strings.Contains(low, "not modified") {
		return gateway.SendResult{Success: true, MessageID: messageID}, nil
	}
	if strings.Contains(low, "message_too_long") || strings.Contains(low, "too long") {
		slog.Debug(fmt.Sprintf("[%s] edit_message overflow (%d UTF-16 > %d), splitting", a.Name(), gateway.UTF16Len(content), maxMessageLength))
		if finalize {
			return a.editOverflowSplit(ctx, chatID, messageID, content, finalize, metadata)
		}
		truncated := a.truncateStreamOverflowPreview(content)
		a.mu.Lock()
		prior, has := a.lastOverflowPreview[previewKey]
		a.mu.Unlock()
		if has && prior == truncated {
			return gateway.SendResult{Success: true, MessageID: messageID}, nil
		}
		if err := a.editText(ctx, chatID, messageID, truncated, ""); err != nil {
			return a.editFailure(ctx, chatID, messageID, truncated, finalize, metadata, previewKey, err)
		}
		a.mu.Lock()
		a.lastOverflowPreview[previewKey] = truncated
		a.mu.Unlock()
		return gateway.SendResult{Success: true, MessageID: messageID}, nil
	}
	retryAfter := retryAfterOf(e)
	if retryAfter != nil || strings.Contains(low, "retry after") {
		wait := 1.0
		if retryAfter != nil && *retryAfter != 0 {
			wait = *retryAfter
		}
		if wait > floodInlineWaitCapSecs {
			slog.Warn(fmt.Sprintf("[%s] Telegram flood control, refusing edit (retry_after %.1fs > %.0fs inline cap)", a.Name(), wait, floodInlineWaitCapSecs))
			return floodCapResult(wait), nil
		}
		slog.Warn(fmt.Sprintf("[%s] Telegram flood control, waiting %.1fs", a.Name(), wait))
		if !sleepCtx(ctx, wait) {
			return gateway.SendResult{}, ctx.Err()
		}
		retryErr := a.editText(ctx, chatID, messageID, content, "")
		if retryErr == nil {
			return gateway.SendResult{Success: true, MessageID: messageID}, nil
		}
		safeRetry := redactTelegramErrorText(retryErr)
		slog.Error(fmt.Sprintf("[%s] Edit retry failed after flood wait: %s", a.Name(), safeRetry))
		retryWait := retryAfterOf(retryErr)
		if retryWait != nil || strings.Contains(errLower(retryErr), "retry after") {
			if retryWait != nil {
				return floodCapResult(*retryWait), nil
			}
			return floodCapResult(wait), nil
		}
		return gateway.SendResult{Success: false, Error: safeRetry}, nil
	}
	safe := redactTelegramErrorText(e)
	for _, marker := range []string{"connecterror", "connect error", "connection error", "networkerror", "network error", "timed out", "readtimeout",
		"writetimeout", "server disconnected", "temporarily unavailable", "temporary failure", "httpx"} {
		if strings.Contains(low, marker) {
			slog.Warn(fmt.Sprintf("[%s] Transient network error editing message %s (will retry): %s", a.Name(), messageID, safe))
			return gateway.SendResult{Success: false, Error: safe, Retryable: true}, nil
		}
	}
	slog.Error(fmt.Sprintf("[%s] Failed to edit Telegram message %s: %s", a.Name(), messageID, safe))
	return gateway.SendResult{Success: false, Error: safe}, nil
}

// One message preview of an oversized streaming edit
func (a *TelegramAdapter) truncateStreamOverflowPreview(content string) string {
	return gateway.TruncateMessage(content, maxMessageLength, gateway.UTF16Len)[0]
}

// Sends one continuation chunk, MarkdownV2 then plain on finalize and raw while streaming
func (a *TelegramAdapter) sendOverflowContinuation(ctx context.Context, chatID, chunk string, replyToID *int, thread threadKwargs,
	threadID *string, metadata map[string]any, finalize bool) *models.Message {
	modes := []bool{false}
	if finalize {
		modes = []bool{true, false}
	}
	kw := textSend{chatID: NormalizeTelegramChatID(chatID), replyTo: replyToID, thread: thread, disableNote: notificationKwargs(a.notificationsMode, metadata)}
	for _, useMarkdown := range modes {
		var text string
		var mode models.ParseMode
		switch {
		case useMarkdown:
			text, mode = separateChunkIndicatorFromFence(a.FormatMessage(chunk)), models.ParseModeMarkdown
		case finalize:
			text = stripMdv2(chunk)
		default:
			text = chunk
		}
		msg, err := a.boundedSendMessage(ctx, a.sendMessageParams(kw, text, mode))
		if err == nil {
			return msg
		}
		if strings.Contains(errLower(err), "reply message not found") {
			retryThread := threadKwargs{}
			if !dmTopicFallback(metadata) {
				retryThread, err = threadKwargsForSend(threadID, metadata, nil, "first")
				if err != nil {
					slog.Warn(fmt.Sprintf("[%s] Overflow continuation no-reply retry failed: %s", a.Name(), redactTelegramErrorText(err)))
					return nil
				}
			}
			retryText := chunk
			if finalize {
				retryText = stripMdv2(chunk)
			}
			retryKw := textSend{chatID: kw.chatID, thread: retryThread, disableNote: kw.disableNote}
			retryMsg, retryErr := a.boundedSendMessage(ctx, a.sendMessageParams(retryKw, retryText, ""))
			if retryErr != nil {
				slog.Warn(fmt.Sprintf("[%s] Overflow continuation no-reply retry failed: %s", a.Name(), redactTelegramErrorText(retryErr)))
				return nil
			}
			return retryMsg
		}
		if useMarkdown {
			continue
		}
		slog.Warn(fmt.Sprintf("[%s] Overflow continuation send failed: %s", a.Name(), redactTelegramErrorText(err)))
		return nil
	}
	return nil
}

var chunkSuffixStripRE = regexp.MustCompile(` \(\d+/\d+\)$`)

// Splits an oversized edit across the message and reply continuations, returning the last chunk id
func (a *TelegramAdapter) editOverflowSplit(ctx context.Context, chatID, messageID, content string, finalize bool, metadata map[string]any) (gateway.SendResult, error) {
	chunks := gateway.TruncateMessage(content, maxMessageLength, gateway.UTF16Len)
	if len(chunks) <= 1 {
		chunks = []string{content}
	}
	first := chunks[0]
	var err error
	if finalize {
		_, err = a.editMarkdownOrPlain(ctx, chatID, messageID, separateChunkIndicatorFromFence(a.FormatMessage(first)), stripMdv2(first),
			"[%s] Overflow split: MarkdownV2 first-chunk edit failed, falling back to plain text: %s")
	} else {
		err = a.editText(ctx, chatID, messageID, first, "")
	}
	if err != nil && !strings.Contains(errLower(err), "not modified") {
		slog.Error(fmt.Sprintf("[%s] Overflow split: first-chunk edit failed: %s", a.Name(), redactTelegramErrorText(err)))
		return gateway.SendResult{Success: false, Error: redactTelegramErrorText(err)}, nil
	}
	var continuationIDs []string
	deliveredChunks := []string{first}
	prevID := messageID
	threadID := metadataThreadID(metadata)
	for _, chunk := range chunks[1:] {
		var replyToID *int
		if prevID != "" {
			id, convErr := pyIntOf(prevID)
			if convErr != nil {
				return gateway.SendResult{}, convErr
			}
			replyToID = &id
		}
		thread, threadErr := threadKwargsForSend(threadID, metadata, replyToID, "first")
		if threadErr != nil {
			return gateway.SendResult{}, threadErr
		}
		sent := a.sendOverflowContinuation(ctx, chatID, chunk, replyToID, thread, threadID, metadata, finalize)
		if sent == nil {
			slog.Warn(fmt.Sprintf("[%s] Overflow split: stopped at %d/%d chunks delivered", a.Name(), 1+len(continuationIDs), len(chunks)))
			var prefix strings.Builder
			for _, d := range deliveredChunks {
				prefix.WriteString(chunkSuffixStripRE.ReplaceAllString(d, ""))
			}
			ids := make(pyrepr.Tuple, 0, len(continuationIDs))
			anyIDs := make([]any, 0, len(continuationIDs))
			for _, id := range continuationIDs {
				ids = append(ids, id)
				anyIDs = append(anyIDs, id)
			}
			return gateway.SendResult{Success: false, MessageID: prevID, Error: "overflow_continuation_failed", Retryable: true,
				RawResponse: map[string]any{"partial_overflow": true, "delivered_chunks": 1 + len(continuationIDs), "total_chunks": len(chunks),
					"last_message_id": prevID, "delivered_prefix": prefix.String(), "continuation_message_ids": ids},
				ContinuationMessageIDs: anyIDs}, nil
		}
		newID := strconv.Itoa(sent.ID)
		if newID == "" {
			newID = prevID
		}
		continuationIDs = append(continuationIDs, newID)
		deliveredChunks = append(deliveredChunks, chunk)
		prevID = newID
	}
	lastID := messageID
	if len(continuationIDs) > 0 {
		lastID = continuationIDs[len(continuationIDs)-1]
	}
	slog.Debug(fmt.Sprintf("[%s] Overflow split delivered %d chunks; last_id=%s", a.Name(), 1+len(continuationIDs), lastID))
	anyIDs := make([]any, 0, len(continuationIDs))
	for _, id := range continuationIDs {
		anyIDs = append(anyIDs, id)
	}
	return gateway.SendResult{Success: true, MessageID: lastID, ContinuationMessageIDs: anyIDs}, nil
}

// Deletes a bot posted message, failures non fatal
func (a *TelegramAdapter) DeleteMessage(ctx context.Context, chatID, messageID string) (bool, error) {
	b := a.currentBot()
	if b == nil {
		return false, nil
	}
	id, err := pyIntOf(messageID)
	if err == nil {
		_, err = b.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: NormalizeTelegramChatID(chatID), MessageID: id})
		err = ptbError(err)
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] Failed to delete Telegram message %s: %s", a.Name(), messageID, redactTelegramErrorText(err)))
		return false, nil
	}
	return true, nil
}

// Draft streaming works in private chats only
func (a *TelegramAdapter) SupportsDraftStreaming(chatType string, metadata map[string]any, chatID string) bool {
	if a.currentBot() == nil {
		return false
	}
	switch pystr.Lower(chatType) {
	case "dm", "private":
		return true
	}
	return false
}

// Streams a partial message as a rich draft when enabled, else a MarkdownV2 then plain sendMessageDraft
func (a *TelegramAdapter) SendDraft(ctx context.Context, chatID string, draftID int, content string, metadata map[string]any) (gateway.SendResult, error) {
	b := a.currentBot()
	if b == nil {
		return gateway.SendResult{Success: false, Error: "not_connected"}, nil
	}
	if a.shouldAttemptRichDraft(content) && a.trySendRichDraft(ctx, chatID, draftID, content, metadata) {
		return gateway.SendResult{Success: true, MessageID: nil}, nil
	}
	text := content
	if len([]rune(content)) > maxMessageLength {
		text = gateway.TruncateMessage(content, maxMessageLength, gateway.UTF16Len)[0]
	}
	plainRichPreview := a.richMessagesEnabled && !a.richDraftsEnabled && needsRichRendering(text)
	thread, err := a.threadKwargsForDraft(metadata)
	modes := []bool{true, false}
	if plainRichPreview {
		modes = []bool{false}
	}
	for _, useMarkdown := range modes {
		draftText := text
		var mode models.ParseMode
		if useMarkdown {
			draftText, mode = a.FormatMessage(text), models.ParseModeMarkdown
		}
		sendErr := err
		ok := false
		if sendErr == nil && thread.DirectMessagesTopicID != nil {
			sendErr = &typeError{msg: "Bot.send_message_draft() got an unexpected keyword argument 'direct_messages_topic_id'"}
		}
		if sendErr == nil {
			params := &bot.SendMessageDraftParams{ChatID: NormalizeTelegramChatID(chatID), DraftID: strconv.Itoa(draftID), Text: draftText, ParseMode: mode}
			if thread.MessageThreadID != nil {
				params.MessageThreadID = *thread.MessageThreadID
			}
			var v any
			v, sendErr = awaitWithThreadDeadline(ctx, func(c context.Context) (any, error) {
				sent, draftErr := b.SendMessageDraft(c, params)
				return sent, ptbError(draftErr)
			}, textSendDeadline, nil, "telegram-send", false)
			ok, _ = v.(bool)
		}
		if sendErr == nil {
			if ok {
				return gateway.SendResult{Success: true, MessageID: nil}, nil
			}
			return gateway.SendResult{Success: false, Error: "draft_rejected"}, nil
		}
		if useMarkdown && isBadRequestError(sendErr) {
			slog.Debug(fmt.Sprintf("[%s] sendMessageDraft MarkdownV2 rejected, retrying as plain text (chat=%s draft_id=%d): %s", a.Name(), chatID,
				draftID, redactTelegramErrorText(sendErr)))
			continue
		}
		slog.Debug(fmt.Sprintf("[%s] sendMessageDraft failed (chat=%s draft_id=%d): %s", a.Name(), chatID, draftID, pyrepr.ExceptionMessage(sendErr)))
		return gateway.SendResult{Success: false, Error: redactTelegramErrorText(sendErr)}, nil
	}
	return gateway.SendResult{Success: false, Error: "draft_rejected"}, nil
}

// Routing of sendMessageDraft and sendRichMessageDraft with an integer DM topic thread id
func (a *TelegramAdapter) threadKwargsForDraft(metadata map[string]any) (threadKwargs, error) {
	return threadKwargsForSend(metadataThreadID(metadata), metadata, replyToMessageIDForSend(nil, metadata, a.replyToMode), a.replyToMode)
}

// Whether the bot can carry rich messages, a live bot speaking the raw Bot API
func (a *TelegramAdapter) botSupportsRich() bool { return a.currentBot() != nil }

// Whether rich delivery is opted in
func (a *TelegramAdapter) richDeliveryEnabled() bool { return a.richMessagesEnabled }

// Shape checks shared by rich sends and rich drafts
func (a *TelegramAdapter) richContentOK(content string) bool {
	return content != "" && pystr.Strip(content) != "" && !hasTelegramDesktopDetailsMathCrashShape(content) &&
		(a.allowCJKRichMessages || !hasTelegramDesktopCJKRichGarbleShape(content)) && contentFitsRichLimits(content) && a.botSupportsRich()
}

// Rich eligibility ignoring expect_edits so a streamed final edit still upgrades
func (a *TelegramAdapter) richEligible(content string) bool {
	return a.richDeliveryEnabled() && !a.richSendDisabled.Load() && content != "" && pystr.Strip(content) != "" &&
		needsRichRendering(content) && a.richContentOK(content)
}

// Whether a fresh send tries sendRichMessage
func (a *TelegramAdapter) shouldAttemptRich(content string, metadata map[string]any) bool {
	return !pyTruthy(metadata["expect_edits"]) && a.richEligible(content)
}

// Replaces a streamed preview with a fresh rich final in DM topics only
func (a *TelegramAdapter) PrefersFreshFinalStreaming(content string, metadata map[string]any, chatID string) bool {
	if !(pyTruthy(metadata["telegram_dm_topic_reply_fallback"]) || metadataDirectMessagesTopicID(metadata) != nil) {
		return false
	}
	return a.richEligible(content)
}

// Whether the rich transport can carry a reply at all
func (a *TelegramAdapter) richTransportAvailable() bool {
	return a.richMessagesEnabled && !a.richSendDisabled.Load() && a.botSupportsRich()
}

// Stream accumulation cap, the rich cap while rich delivery is available
func (a *TelegramAdapter) StreamingOverflowLimit() (int, bool) {
	if a.richTransportAvailable() {
		return richMessageMaxChars, true
	}
	return 0, false
}

// Failed result of a transient rich failure the caller must not resend through the legacy path
func (a *TelegramAdapter) richTransientResult(err error, what string, retryAfter *float64) gateway.SendResult {
	safe := redactTelegramErrorText(err)
	slog.Warn(fmt.Sprintf("[%s] %s transient failure (no legacy resend): %s", a.Name(), what, safe))
	return gateway.SendResult{Success: false, Error: safe, Retryable: looksLikeConnectTimeout(err) || !isTimedOut(err), RetryAfter: retryAfter}
}

// Indexes sent rich content since Telegram never echoes it in reply_to_message
func recordRichSent(ctx context.Context, chatID, messageID any, content string) {
	gateway.RichSentRecord(ctx, pyrepr.Str(chatID), pyrepr.Str(messageID), content)
}

var retryInRE = regexp.MustCompile(`(?i)retry\s+(?:in\s+)?(\d+)`)

// Attempts one sendRichMessage, nil meaning fall back to the legacy MarkdownV2 path
func (a *TelegramAdapter) trySendRich(ctx context.Context, chatID, content string, replyTo *string, metadata map[string]any) (*gateway.SendResult, error) {
	threadID := metadataThreadID(metadata)
	replyToID, thread, ok, err := computeSingleSendRouting(replyTo, metadata, threadID, a.replyToMode)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	payload := a.richPayloadBase(chatID, content)
	if thread.MessageThreadID != nil {
		payload["message_thread_id"] = *thread.MessageThreadID
	}
	if thread.DirectMessagesTopicID != nil {
		payload["direct_messages_topic_id"] = *thread.DirectMessagesTopicID
	}
	payload["disable_notification"] = notificationKwargs(a.notificationsMode, metadata)
	if replyToID != nil {
		payload["reply_parameters"] = map[string]any{"message_id": *replyToID}
	}
	msg, sendErr := awaitWithThreadDeadline(ctx, func(c context.Context) (any, error) { return a.doAPIRequest(c, "sendRichMessage", payload) },
		textSendDeadline, nil, "telegram-send", false)
	if sendErr != nil {
		if errors.Is(sendErr, context.Canceled) && ctx.Err() != nil {
			return nil, sendErr
		}
		if a.richRejected(sendErr, "sendRichMessage", "MarkdownV2") {
			return nil, nil
		}
		retryAfter := retryAfterOf(sendErr)
		if retryAfter == nil {
			if m := retryInRE.FindStringSubmatch(errLower(sendErr)); m != nil {
				v, _ := strconv.ParseFloat(m[1], 64)
				retryAfter = &v
			}
		}
		result := a.richTransientResult(sendErr, "sendRichMessage", retryAfter)
		return &result, nil
	}
	var messageID any
	if m, isMap := msg.(map[string]any); isMap {
		messageID = m["message_id"]
		if messageID == nil {
			if inner, ok := m["result"].(map[string]any); ok {
				messageID = inner["message_id"]
			}
		}
	}
	if messageID != nil {
		messageID = jsonIDText(messageID)
		recordRichSent(ctx, chatID, messageID, content)
		return &gateway.SendResult{Success: true, MessageID: messageID}, nil
	}
	return &gateway.SendResult{Success: true, MessageID: nil}, nil
}

// Python str() of a decoded JSON message id
func jsonIDText(v any) string {
	if f, ok := v.(float64); ok && f == math.Trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return pyrepr.Str(v)
}

// Rich payload with the normalized chat id and the link preview switch
func (a *TelegramAdapter) richPayloadBase(chatID, content string) map[string]any {
	payload := map[string]any{"chat_id": NormalizeTelegramChatID(chatID), "rich_message": richMessagePayload(content, false)}
	if a.disableLinkPreviews {
		payload["link_preview_options"] = map[string]any{"is_disabled": true}
	}
	return payload
}

// True for a permanent or capability rich failure, a capability error latching rich sends off
func (a *TelegramAdapter) richRejected(err error, what, fallback string) bool {
	if !isRichFallbackError(err) {
		return false
	}
	if isRichCapabilityError(err) {
		a.richSendDisabled.Store(true)
	}
	slog.Debug(fmt.Sprintf("[%s] %s rejected (%s) — falling back to %s", a.Name(), what, redactTelegramErrorText(err), fallback))
	return true
}

// Edits a message in place as rich so a streamed preview finalizes without send and delete
func (a *TelegramAdapter) tryEditRich(ctx context.Context, chatID, messageID, content string) (*gateway.SendResult, error) {
	id, err := pyIntOf(messageID)
	if err != nil {
		return nil, err
	}
	payload := a.richPayloadBase(chatID, content)
	payload["message_id"] = id
	_, editErr := awaitWithThreadDeadline(ctx, func(c context.Context) (any, error) { return a.doAPIRequest(c, "editMessageText", payload) },
		textSendDeadline, nil, "telegram-send", false)
	if editErr != nil {
		if errors.Is(editErr, context.Canceled) && ctx.Err() != nil {
			return nil, editErr
		}
		if strings.Contains(errLower(editErr), "not modified") {
			if isRichFallbackError(editErr) && isRichCapabilityError(editErr) {
				a.richSendDisabled.Store(true)
			}
			return &gateway.SendResult{Success: true, MessageID: messageID}, nil
		}
		if a.richRejected(editErr, "rich editMessageText", "MarkdownV2 edit") {
			return nil, nil
		}
		result := a.richTransientResult(editErr, "rich editMessageText", nil)
		return &result, nil
	}
	recordRichSent(ctx, chatID, messageID, content)
	return &gateway.SendResult{Success: true, MessageID: messageID}, nil
}

// Whether a draft frame tries sendRichMessageDraft
func (a *TelegramAdapter) shouldAttemptRichDraft(content string) bool {
	return a.richMessagesEnabled && a.richDraftsEnabled && !a.richSendDisabled.Load() && !a.richDraftDisabled.Load() && a.richContentOK(content)
}

// Emits one sendRichMessageDraft frame, any failure falling back to the legacy draft and capability errors latching off
func (a *TelegramAdapter) trySendRichDraft(ctx context.Context, chatID string, draftID int, content string, metadata map[string]any) bool {
	payload := map[string]any{"chat_id": NormalizeTelegramChatID(chatID), "draft_id": draftID, "rich_message": richMessagePayload(content, false)}
	thread, err := a.threadKwargsForDraft(metadata)
	if err == nil {
		if thread.MessageThreadID != nil {
			payload["message_thread_id"] = *thread.MessageThreadID
		}
		if thread.DirectMessagesTopicID != nil {
			payload["direct_messages_topic_id"] = *thread.DirectMessagesTopicID
		}
		var v any
		v, err = awaitWithThreadDeadline(ctx, func(c context.Context) (any, error) { return a.doAPIRequest(c, "sendRichMessageDraft", payload) },
			textSendDeadline, nil, "telegram-send", false)
		if err == nil {
			return pyTruthy(v)
		}
	}
	if isRichCapabilityError(err) {
		a.richDraftDisabled.Store(true)
		slog.Debug(fmt.Sprintf("[%s] sendRichMessageDraft unsupported (%s) — using legacy drafts", a.Name(), redactTelegramErrorText(err)))
	} else {
		slog.Debug(fmt.Sprintf("[%s] sendRichMessageDraft transient failure (%s) — legacy draft this frame", a.Name(), redactTelegramErrorText(err)))
	}
	return false
}

// Sends the typing action, retrying a DM topic lane without its thread and backing off transient failures
func (a *TelegramAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	b := a.currentBot()
	if b == nil || a.pacer.typingInCooldown(chatID) {
		return nil
	}
	action := func(threadID *int) error {
		params := &bot.SendChatActionParams{ChatID: NormalizeTelegramChatID(chatID), Action: models.ChatActionTyping}
		if threadID != nil {
			params.MessageThreadID = *threadID
		}
		if _, err := b.SendChatAction(ctx, params); err != nil {
			return ptbError(err)
		}
		a.pacer.clearTypingCooldown(chatID)
		return nil
	}
	isDMTopic := dmTopicFallback(metadata)
	threadID, err := messageThreadIDForTyping(metadataThreadID(metadata))
	if err == nil {
		err = action(threadID)
	}
	if err == nil {
		return nil
	}
	if isDMTopic && threadID != nil {
		fallbackErr := action(nil)
		if fallbackErr == nil {
			return nil
		}
		if isTransientTypingError(fallbackErr) {
			a.pacer.recordTypingCooldown(chatID, fallbackErr)
		}
	} else if isTransientTypingError(err) {
		a.pacer.recordTypingCooldown(chatID, err)
	}
	slog.Debug(fmt.Sprintf("[%s] Failed to send Telegram typing indicator: %s", a.Name(), redactTelegramErrorText(err)))
	return nil
}

// Name, type, username and forum flag of a chat
func (a *TelegramAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	b := a.currentBot()
	if b == nil {
		return map[string]any{"name": "Unknown", "type": "dm"}, nil
	}
	chat, err := b.GetChat(ctx, &bot.GetChatParams{ChatID: NormalizeTelegramChatID(chatID)})
	if err != nil {
		err = ptbError(err)
		slog.Error(fmt.Sprintf("[%s] Failed to get Telegram chat info for %s: %s", a.Name(), chatID, redactTelegramErrorText(err)))
		return map[string]any{"name": chatID, "type": "dm", "error": pyrepr.ExceptionMessage(err)}, nil
	}
	chatType := "dm"
	switch chat.Type {
	case models.ChatTypeGroup:
		chatType = "group"
	case models.ChatTypeSupergroup:
		chatType = "group"
		if chat.IsForum {
			chatType = "forum"
		}
	case models.ChatTypeChannel:
		chatType = "channel"
	}
	name := chat.Title
	if name == "" {
		name = fullName(chat.FirstName, chat.LastName)
	}
	if name == "" {
		name = chatID
	}
	var username any
	if chat.Username != "" {
		username = chat.Username
	}
	return map[string]any{"name": name, "type": chatType, "username": username, "is_forum": chat.IsForum}, nil
}

// Whether processing reactions are on, the scoped env first then the profile extra
func (a *TelegramAdapter) ReactionsEnabled(ctx context.Context) bool {
	configured, err := gateway.ExtraOrSecret(ctx, a.Config.Extra, "reactions", "TELEGRAM_REACTIONS", nil, true)
	if err != nil || configured == nil {
		return false
	}
	switch pystr.Lower(pyrepr.Str(configured)) {
	case "false", "0", "no":
		return false
	}
	return true
}

// Sets one emoji reaction, an empty emoji clearing every bot reaction
func (a *TelegramAdapter) setReaction(ctx context.Context, chatID, messageID, emoji string) bool {
	b := a.currentBot()
	if b == nil {
		return false
	}
	id, err := pyIntOf(messageID)
	if err == nil {
		params := &bot.SetMessageReactionParams{ChatID: NormalizeTelegramChatID(chatID), MessageID: id, Reaction: []models.ReactionType{}}
		if emoji != "" {
			params.Reaction = []models.ReactionType{{Type: models.ReactionTypeTypeEmoji, ReactionTypeEmoji: &models.ReactionTypeEmoji{
				Type: models.ReactionTypeTypeEmoji, Emoji: emoji}}}
		}
		_, err = b.SetMessageReaction(ctx, params)
		err = ptbError(err)
	}
	if err != nil {
		if emoji == "" {
			slog.Debug(fmt.Sprintf("[%s] clear reactions failed: %s", a.Name(), redactTelegramErrorText(err)))
		} else {
			slog.Debug(fmt.Sprintf("[%s] set_message_reaction failed (%s): %s", a.Name(), emoji, redactTelegramErrorText(err)))
		}
		return false
	}
	return true
}

// Clears every bot set reaction
func (a *TelegramAdapter) clearReactions(ctx context.Context, chatID, messageID string) bool {
	return a.setReaction(ctx, chatID, messageID, "")
}

// Adds the in progress reaction when processing begins
func (a *TelegramAdapter) OnProcessingStart(ctx context.Context, event *gateway.MessageEvent) error {
	if !a.ReactionsEnabled(ctx) {
		return nil
	}
	if event.Source != nil && event.Source.ChatID != "" && event.MessageID != nil && *event.MessageID != "" {
		a.setReaction(ctx, event.Source.ChatID, *event.MessageID, "\U0001f440")
	}
	return nil
}

// Swaps the in progress reaction for the outcome, a cancelled turn clearing it
func (a *TelegramAdapter) OnProcessingComplete(ctx context.Context, event *gateway.MessageEvent, outcome gateway.ProcessingOutcome) error {
	if !a.ReactionsEnabled(ctx) {
		return nil
	}
	if event.Source == nil || event.Source.ChatID == "" || event.MessageID == nil || *event.MessageID == "" {
		return nil
	}
	chatID, messageID := event.Source.ChatID, *event.MessageID
	switch outcome {
	case gateway.ProcessingCancelled:
		a.clearReactions(ctx, chatID, messageID)
	case gateway.ProcessingSuccess:
		a.setReaction(ctx, chatID, messageID, "\U0001f44d")
	default:
		a.setReaction(ctx, chatID, messageID, "\U0001f44e")
	}
	return nil
}
