package gateway

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Sends a new chunk threaded to replyToID, the new message id or replyToID when nothing landed
func (s *GatewayStreamConsumer) sendNewChunk(ctx context.Context, text, replyToID string, final bool) string {
	text = cleanForDisplay(text)
	if pystr.Strip(text) == "" {
		return replyToID
	}
	result, err := s.adapter.Send(ctx, s.chatID, text, replyToID, s.metadataForSend(final, !final))
	if err != nil {
		streamLog().Error("Stream send chunk error: " + pyrepr.ExceptionMessage(err))
		return replyToID
	}
	newID := sendResultID(result.MessageID)
	if !result.Success || newID == "" {
		s.editSupported = false
		return replyToID
	}
	s.messageID = newID
	s.trackPreviewIDsFromResult(result)
	s.alreadySent = true
	s.lastSentText = text
	s.notifyNewMessage()
	return newID
}

// Visible text already shown in the streamed message
func (s *GatewayStreamConsumer) visiblePrefix() string {
	prefix := s.lastSentText
	if s.cfg.Cursor != "" && strings.HasSuffix(prefix, s.cfg.Cursor) {
		prefix = strings.TrimSuffix(prefix, s.cfg.Cursor)
	}
	return cleanForDisplay(prefix)
}

// Part of the final text the user has not seen, the cut backed up to a word boundary
func (s *GatewayStreamConsumer) continuationText(finalText string) string {
	prefix := s.fallbackPrefix
	if prefix == "" {
		prefix = s.visiblePrefix()
	}
	if prefix == "" || !strings.HasPrefix(finalText, prefix) {
		return finalText
	}
	runes := []rune(finalText)
	cut := len([]rune(prefix))
	if cut < len(runes) && !unicode.IsSpace(runes[cut]) && !unicode.IsSpace(runes[cut-1]) {
		boundary := max(lastRuneIndex(runes[:cut], ' '), lastRuneIndex(runes[:cut], '\n'))
		if boundary >= 0 {
			cut = boundary + 1
		}
	}
	return pystr.Lstrip(string(runes[cut:]))
}

// Fallback chunks split by newline and balanced across code fences
func splitTextChunks(text string, limit int, lenFn func(string) int) []string {
	return SplitTextFenceAware(text, limit, lenFn, false, true)
}

// Overflow split through the adapter's own truncator, else the base platform rules
func (s *GatewayStreamConsumer) truncateForStream(text string, limit int, lenFn func(string) int) []string {
	if truncater, ok := s.adapter.(StreamTruncater); ok {
		return truncater.TruncateMessage(text, limit, lenFn)
	}
	return TruncateMessage(text, limit, lenFn)
}

// Sends the final continuation after streaming edits stopped working, one flood retry per chunk
func (s *GatewayStreamConsumer) sendFallbackFinal(ctx context.Context, text string) {
	if s.egressDeclined {
		streamLog().Warn("suppressing the fallback continuation: the connector already declined this destination for this run")
		return
	}
	finalText := EnsureClosedCodeFences(cleanForDisplay(text))
	continuation := s.continuationText(finalText)
	s.fallbackFinalSend = false
	if pystr.Strip(continuation) == "" {
		next, send := s.fallbackWhenNothingUnseen(ctx, finalText)
		if !send {
			return
		}
		continuation = next
	}
	lenFn, rawLimit := s.fallbackLenBudget()
	chunks := splitTextChunks(continuation, max(500, rawLimit-100), lenFn)
	staleMessageID := s.messageID
	lastMessageID := ""
	lastSuccessfulChunk := ""
	sentAnyChunk := false
	anchor := ""
	if continuation == finalText {
		anchor = s.initialReplyToID
	}
	for _, chunk := range chunks {
		replyTo := anchor
		if sentAnyChunk {
			replyTo = ""
		}
		result, err := s.sendWithFloodRetry(ctx, chunk, replyTo, "Flood control on fallback send, retrying in %.1fs")
		if err != nil {
			streamLog().Error("Stream consumer error: " + pyrepr.ExceptionMessage(err))
		}
		if err != nil || result == nil || !result.Success {
			s.alreadySent = sentAnyChunk
			s.messageID = lastMessageID
			s.lastSentText = lastSuccessfulChunk
			s.fallbackPrefix = ""
			return
		}
		sentAnyChunk = true
		lastSuccessfulChunk = chunk
		if id := sendResultID(result.MessageID); id != "" {
			lastMessageID = id
		}
		s.notifyNewMessage()
	}
	if staleMessageID != "" && staleMessageID != lastMessageID && !s.fallbackPreservePartialMessages && continuation == finalText {
		s.deletePreviews(ctx, idSet{staleMessageID}, "", "Fallback partial", false, false)
	}
	s.messageID = lastMessageID
	s.alreadySent = true
	s.markFinalDelivered(&finalText)
	if len(chunks) > 0 {
		s.lastSentText = chunks[len(chunks)-1]
	}
	s.fallbackPrefix = ""
	s.fallbackPreservePartialMessages = false
}

// Continuation to send when the visible prefix already covers the final, false when the turn settles here
func (s *GatewayStreamConsumer) fallbackWhenNothingUnseen(ctx context.Context, finalText string) (string, bool) {
	visible := s.visiblePrefix()
	resender, resends := s.adapter.(EmptyStreamFallbackResender)
	if pystr.Strip(finalText) != "" && finalText == visible && resends && resender.ResendFinalOnEmptyStreamFallback() {
		delivery := s.sendEmptyFallbackFinal(ctx, finalText)
		if delivery == "delivered" {
			return "", false
		}
		s.alreadySent = true
		s.fallbackPrefix = ""
		s.fallbackPreservePartialMessages = false
		s.finalContentDelivered = delivery == "ambiguous" || delivery == "preview"
		switch delivery {
		case "preview":
			s.recordTurnFinalPayload(finalText)
		case "ambiguous":
			s.deliveryAmbiguous = true
		default:
			s.finalResponseSent = false
		}
		return "", false
	}
	if pystr.Strip(finalText) != "" && finalText != visible {
		return finalText, true
	}
	if s.messageID != "" && s.lastSentText != "" && s.cfg.Cursor != "" && strings.HasSuffix(s.lastSentText, s.cfg.Cursor) {
		cleanText := strings.TrimSuffix(s.lastSentText, s.cfg.Cursor)
		if result, err := s.editMessage(ctx, s.messageID, cleanText, false); err == nil && result.Success {
			s.lastSentText = cleanText
		}
	}
	s.alreadySent = true
	s.markFinalDelivered(&finalText)
	return "", false
}

// Length function and raw per chat limit for fallback chunking
func (s *GatewayStreamConsumer) fallbackLenBudget() (func(string) int, int) {
	rawLimit := 4096
	lenFn := pyLen
	if s.adapter == nil || s.adapter.Base() == nil {
		return lenFn, rawLimit
	}
	if limit := s.adapter.Base().Traits.MaxMessageLength; limit > 0 {
		rawLimit = limit
	}
	if fn := s.adapter.MessageLenFn(); fn != nil {
		lenFn = fn
	}
	limit, err := s.adapter.MaxMessageLengthForChat(s.chatID)
	if err == nil {
		var fn func(string) int
		fn, err = s.adapter.MessageLenFnForChat(s.chatID)
		if err == nil {
			rawLimit, lenFn = limit, fn
		}
	}
	if err != nil {
		streamLog().Debug("per-chat limit resolution failed: " + pyrepr.ExceptionMessage(err))
	}
	return lenFn, rawLimit
}

// Final send with one bounded flood retry, returning the last result
func (s *GatewayStreamConsumer) sendWithFloodRetry(ctx context.Context, content, replyTo, retryLog string) (*SendResult, error) {
	var result *SendResult
	for attempt := range 2 {
		sent, err := s.adapter.Send(ctx, s.chatID, content, replyTo, s.metadataForSend(true, false))
		if err != nil {
			return nil, err
		}
		result = &sent
		if sent.Success {
			break
		}
		delay, ok := s.fallbackFloodRetryDelay(sent)
		if attempt > 0 || !ok {
			break
		}
		if raw, isMap := sent.RawResponse.(map[string]any); isMap && pyrepr.Truthy(raw["partial_overflow"]) {
			break
		}
		streamLog().Debug(fmt.Sprintf(retryLog, delay))
		if !streamSleep(ctx, delay) {
			return result, ctx.Err()
		}
	}
	return result, nil
}

// Commits a completed answer after a failed finalization as delivered, failed, ambiguous or preview
func (s *GatewayStreamConsumer) sendEmptyFallbackFinal(ctx context.Context, finalText string) string {
	staleIDs := s.stalePreviewIDs(true)
	result, err := s.sendWithFloodRetry(ctx, finalText, s.initialReplyToID, "Flood control on empty fallback final send; retrying in %.1fs")
	if err != nil {
		streamLog().Debug("Empty fallback final send failed: " + pyrepr.ExceptionMessage(err))
		if sendErrorMayHaveDelivered(err) {
			return "ambiguous"
		}
		return "failed"
	}
	if result == nil || !result.Success {
		if result != nil && isFloodError(*result) {
			return "preview"
		}
		if result != nil && sendResultMayHaveDelivered(*result) {
			return "ambiguous"
		}
		return "failed"
	}
	newID := sendResultID(result.MessageID)
	s.deletePreviews(ctx, staleIDs, newID, "Empty fallback", true, true)
	s.segmentPreviewMessageIDs = nil
	s.messageID = newID
	if s.messageID == "" {
		s.messageID = "__no_edit__"
	}
	s.alreadySent = true
	s.markFinalDelivered(nil)
	payload := s.displayPayload(finalText)
	s.deliveredFinalText = &payload
	s.lastSentText = finalText
	s.fallbackPrefix = ""
	s.fallbackPreservePartialMessages = false
	s.notifyNewMessage()
	return "delivered"
}

// Whether a timed out failed send result may still have landed
func sendResultMayHaveDelivered(result SendResult) bool {
	if result.Retryable {
		return false
	}
	text := strings.ToLower(pyrepr.Str(pyrepr.Or(result.Error, "")))
	return strings.Contains(text, "timeout") || strings.Contains(text, "timed out")
}

// Whether a raised send error is a timeout that may still have landed
func sendErrorMayHaveDelivered(err error) bool {
	text := strings.ToLower(pyrepr.ExceptionMessage(err))
	name := strings.ToLower(pyrepr.TypeName(err))
	return strings.Contains(text, "timeout") || strings.Contains(text, "timed out") || strings.Contains(name, "timeout")
}

// Bounded retry delay of a flood refused fallback send, false when a retry is not safe
func (s *GatewayStreamConsumer) fallbackFloodRetryDelay(result SendResult) (float64, bool) {
	if !isFloodError(result) {
		return 0, false
	}
	delay := 3.0
	if result.RetryAfter != nil && *result.RetryAfter != 0 {
		delay = *result.RetryAfter
	}
	if delay > s.maxFallbackFloodRetrySeconds {
		streamLog().Debug(fmt.Sprintf("Flood control requests %.1fs; leaving final delivery to the gateway", delay))
		return 0, false
	}
	return max(0.0, delay), true
}

// Whether a failed send result is flood control or rate limiting
func isFloodError(result SendResult) bool {
	errLower := strings.ToLower(pyrepr.Str(pyrepr.Or(result.Error, "")))
	return strings.Contains(errLower, "flood") || strings.Contains(errLower, "retry after") || strings.Contains(errLower, "rate")
}

// Sends the unseen tail as a new message before a segment reset and strips the stuck cursor
func (s *GatewayStreamConsumer) flushSegmentTailOnEditFailure(ctx context.Context) {
	if s.egressDeclined {
		return
	}
	if !s.fallbackFinalSend {
		s.tryStripCursor(ctx)
	}
	visible := s.fallbackPrefix
	if visible == "" {
		visible = s.visiblePrefix()
	}
	tail := cleanForDisplay(s.accumulated)
	if visible != "" && strings.HasPrefix(tail, visible) {
		tail = pystr.Lstrip(tail[len(visible):])
	}
	if pystr.Strip(tail) == "" {
		return
	}
	md := map[string]any{}
	for k, v := range s.metadata {
		md[k] = v
	}
	md["_interim_send"] = true
	result, err := s.adapter.Send(ctx, s.chatID, tail, "", md)
	if err != nil {
		streamLog().Error("Segment-break tail flush error: " + pyrepr.ExceptionMessage(err))
		return
	}
	if result.Success {
		s.alreadySent = true
	}
}

// Edit removing a cursor stuck on the preview when entering fallback mode
func (s *GatewayStreamConsumer) tryStripCursor(ctx context.Context) {
	prefix := s.visiblePrefix()
	if !s.hasRealPreview() || pystr.Strip(prefix) == "" {
		return
	}
	if result, err := s.editMessage(ctx, s.messageID, prefix, false); err == nil && result.Success {
		s.lastSentText = prefix
	}
}

// Platforms whose commentary threads under the user's message by reply anchor
var replyAnchoredCommentaryPlatforms = map[string]bool{"buzz": true, "slack": true, "mattermost": true, "feishu": true}

// Sends a completed interim commentary message, recording its text once it landed
func (s *GatewayStreamConsumer) sendCommentary(ctx context.Context, text string) bool {
	text = cleanForDisplay(text)
	if pystr.Strip(text) == "" {
		return false
	}
	md := s.metadataForSend(false, false)
	if md == nil {
		md = map[string]any{}
	}
	md["_interim_send"] = true
	platformName := ""
	if base := s.adapter.Base(); base != nil && base.Platform != "" {
		platformName = strings.ToLower(string(base.Platform))
	} else {
		platformName = strings.ToLower(s.adapter.Name())
	}
	replyTo := ""
	if replyAnchoredCommentaryPlatforms[platformName] {
		replyTo = s.initialReplyToID
	}
	result, err := s.adapter.Send(ctx, s.chatID, text, replyTo, md)
	if err != nil {
		streamLog().Error("Commentary send error: " + pyrepr.ExceptionMessage(err))
		return false
	}
	if result.Success {
		s.notifyNewMessage()
		s.deliveredCommentaryTexts = append(s.deliveredCommentaryTexts, text)
	}
	return result.Success
}

// Per chat length budget before overflow splits, raised by the adapter's streaming overflow limit
func (s *GatewayStreamConsumer) rawMessageLimit() int {
	base := 4096
	if s.adapter == nil || s.adapter.Base() == nil {
		return base
	}
	if limit := s.adapter.Base().Traits.MaxMessageLength; limit > 0 {
		base = limit
	}
	if limit, err := s.adapter.MaxMessageLengthForChat(s.chatID); err != nil {
		streamLog().Debug("max_message_length_for_chat failed: " + pyrepr.ExceptionMessage(err))
	} else {
		base = limit
	}
	if limit, ok := s.adapter.StreamingOverflowLimit(); ok && limit > base {
		return limit
	}
	return base
}

// Retracts any streamed preview when the final reply is a bare silence marker
func (s *GatewayStreamConsumer) suppressSilenceMarker(ctx context.Context) {
	if s.nativeStreamOpened.Load() {
		s.closeEmptyNativeBubble(ctx, "Silence-marker native stream close failed: %s")
	}
	s.deletePreviews(ctx, s.stalePreviewIDs(false), "", "Silence-marker", false, true)
	s.previewMessageIDs = nil
	s.messageID = ""
	s.accumulated, s.streamLedger, s.lastSentText = "", "", ""
	s.alreadySent = false
	s.clearTurnFinalFlags()
	streamLog().Info(fmt.Sprintf("Suppressed streamed intentional-silence marker (chat=%s)", s.chatID))
}
