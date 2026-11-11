package gateway

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Longest interim edit interval flood backoff may reach
const maxEditBackoffSecs = 30.0

// Shortest visible text that opens a new streamed message
const minNewMsgChars = 4

// Adapter that streams every frame natively, the WeCom stream message transport
type NativeStreamAdapter interface {
	SupportsNativeStreaming(chatType string, metadata map[string]any) bool
	SendStreamFrame(ctx context.Context, text string, finalize bool, chatID, replyTo, turnID string) (bool, error)
}

// Adapter that seals an orphaned draft stream when its turn dies
type DraftAbandoner interface {
	AbandonOpenDraft(ctx context.Context, chatID, text string, metadata map[string]any) (SendResult, error)
}

// Adapter whose turn final edit flood enters the fallback send at once
type FinalEditFloodFallback interface {
	FallbackOnFinalEditFlood() bool
}

// Adapter that commits a fresh final send when the stream fallback has nothing unseen
type EmptyStreamFallbackResender interface {
	ResendFinalOnEmptyStreamFallback() bool
}

// Adapter with its own message splitter for stream overflow
type StreamTruncater interface {
	TruncateMessage(content string, maxLength int, lenFn func(string) int) []string
}

// Message id of a send result as text, empty when absent
func sendResultID(id any) string {
	if !pyrepr.Truthy(id) {
		return ""
	}
	return pyrepr.Str(id)
}

// Edits through the adapter with the consumer's chat routing
func (s *GatewayStreamConsumer) editMessage(ctx context.Context, messageID, content string, finalize bool) (SendResult, error) {
	return s.adapter.EditMessage(ctx, s.chatID, messageID, content, finalize)
}

// Opens a native stream with an empty seed frame, a failure logged and read as false
func (s *GatewayStreamConsumer) trySeedFrame(ctx context.Context, failLog string, excInfo bool) bool {
	native, ok := s.adapter.(NativeStreamAdapter)
	if !ok {
		return false
	}
	sent, err := native.SendStreamFrame(ctx, "", false, s.chatID, s.initialReplyToID, s.turnID)
	return tryFrame(sent, err, failLog, excInfo)
}

// Frame send outcome as a bool, a raised error logged at debug
func tryFrame(sent bool, err error, failLog string, excInfo bool) bool {
	if err != nil {
		if excInfo {
			streamLog().Debug(failLog, "error", err)
		} else {
			streamLog().Debug(strings.Replace(failLog, "%s", pyrepr.ExceptionMessage(err), 1))
		}
		return false
	}
	return sent
}

// One native stream frame with the consumer's chat, reply and turn routing
func (s *GatewayStreamConsumer) sendFrame(ctx context.Context, text string, finalize bool) (bool, error) {
	native, ok := s.adapter.(NativeStreamAdapter)
	if !ok {
		return false, fmt.Errorf("'%T' object has no attribute 'send_stream_frame'", s.adapter)
	}
	return native.SendStreamFrame(ctx, text, finalize, s.chatID, s.initialReplyToID, s.turnID)
}

// Marks the native stream closed so the next content re seeds or falls back
func (s *GatewayStreamConsumer) closeNativeState() {
	s.nativeStreamOpened.Store(false)
	s.nativeLastPushedLen = 0
}

// Empty finalize frame closing an open typing bubble, then the stream marked closed
func (s *GatewayStreamConsumer) closeEmptyNativeBubble(ctx context.Context, failLog string) {
	sent, err := s.sendFrame(ctx, "", true)
	tryFrame(sent, err, failLog, false)
	s.closeNativeState()
	s.reopenSeededEagerly = false
}

// Leaves native mode so post boundary output goes out as one send at the end
func (s *GatewayStreamConsumer) degradeNativeToBufferedSend() {
	s.useNativeStreaming.Store(false)
	s.closeNativeState()
	s.cfg.BufferOnly = true
}

// Draft frame metadata carrying the same reply anchor as the final send
func (s *GatewayStreamConsumer) draftMetadata() map[string]any {
	md := map[string]any{}
	for k, v := range s.metadata {
		md[k] = v
	}
	if s.initialReplyToID != "" {
		if _, ok := md["reply_to_message_id"]; !ok {
			md["reply_to_message_id"] = s.initialReplyToID
		}
	}
	if len(md) == 0 {
		return nil
	}
	return md
}

// Preview ids a fresh final replaces, segmentOnly sparing finalized preambles
func (s *GatewayStreamConsumer) stalePreviewIDs(segmentOnly bool) idSet {
	stale := s.previewMessageIDs.clone()
	if segmentOnly {
		stale = s.segmentPreviewMessageIDs.clone()
	}
	if s.messageID != "" && s.messageID != "__no_edit__" {
		stale.add(s.messageID)
	}
	return stale
}

// Deletes stale previews, never the message just sent
func (s *GatewayStreamConsumer) deletePreviews(ctx context.Context, staleIDs idSet, skip, label string, retryOnFalse, skipSentinel bool) {
	deleter, ok := s.adapter.(MessageDeleter)
	if !ok {
		return
	}
	for _, staleID := range staleIDs {
		if staleID == "" || (skip != "" && staleID == skip) || (skipSentinel && staleID == "__no_edit__") {
			continue
		}
		deleted, err := deleter.DeleteMessage(ctx, s.chatID, staleID)
		if err == nil && retryOnFalse && !deleted {
			if !streamSleep(ctx, 1.0) {
				return
			}
			_, err = deleter.DeleteMessage(ctx, s.chatID, staleID)
		}
		if err != nil {
			streamLog().Debug(fmt.Sprintf("%s preview cleanup failed (%s): %s", label, staleID, pyrepr.ExceptionMessage(err)))
		}
	}
}

// Whether the draft transport applies, the adapter's per chat probe deciding for draft and auto
func (s *GatewayStreamConsumer) resolveDraftStreaming() bool {
	transport := strings.ToLower(s.cfg.Transport)
	if transport == "" {
		transport = "edit"
	}
	if transport == "edit" || transport == "off" || s.adapter == nil {
		return false
	}
	supported := func() (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				streamLog().Debug("supports_draft_streaming probe raised", "error", r)
				ok = false
			}
		}()
		return s.adapter.SupportsDraftStreaming(s.cfg.ChatType, s.metadata, s.chatID)
	}()
	if !supported && transport == "draft" {
		streamLog().Debug(fmt.Sprintf("Draft streaming requested but unsupported (chat=%s, type=%s) — falling back to edit", s.chatID, pyrepr.Repr(s.cfg.ChatType)))
	}
	return supported
}

// Whether the adapter streams natively for this chat
func (s *GatewayStreamConsumer) resolveNativeStreaming() bool {
	native, ok := s.adapter.(NativeStreamAdapter)
	if !ok {
		return false
	}
	return func() (supported bool) {
		defer func() {
			if r := recover(); r != nil {
				streamLog().Debug("supports_native_streaming probe raised", "error", r)
				supported = false
			}
		}()
		return native.SupportsNativeStreaming(s.cfg.ChatType, s.metadata)
	}()
}

// Emits one draft frame, any failure disabling drafts for this run
func (s *GatewayStreamConsumer) sendDraftFrame(ctx context.Context, text string) bool {
	if s.draftID == nil {
		s.useDraftStreaming = false
		return false
	}
	result, err := s.adapter.SendDraft(ctx, s.chatID, int(*s.draftID), text, s.draftMetadata())
	if err != nil {
		streamLog().Debug("send_draft raised, disabling draft transport for this run: " + pyrepr.ExceptionMessage(err))
	} else {
		if result.Success {
			s.lastSentText = text
			return true
		}
		if DeclinedSend(&result) {
			streamLog().Warn("send_draft DECLINED by the connector's egress guard; suppressing every later send for this run (the destination is not approved for this connection)")
			s.egressDeclined = true
		}
		streamLog().Debug("send_draft returned success=False, disabling draft transport: " + pyrepr.Str(pyrepr.Or(result.Error, "unknown")))
	}
	s.draftFailures++
	s.useDraftStreaming = false
	return false
}

// Seals an orphaned draft stream on turn death, never setting delivery flags
func (s *GatewayStreamConsumer) abandonNativeStream(ctx context.Context) {
	if !s.useDraftStreaming {
		return
	}
	abandoner, ok := s.adapter.(DraftAbandoner)
	if !ok {
		return
	}
	text := s.lastSentText
	if text == "" {
		text = cleanForDisplay(s.accumulated)
	}
	if _, err := abandoner.AbandonOpenDraft(ctx, s.chatID, text, s.draftMetadata()); err != nil {
		streamLog().Debug("abandon_open_draft failed (best-effort): " + pyrepr.ExceptionMessage(err))
	}
}

// A real editable and deletable preview message is on screen
func (s *GatewayStreamConsumer) hasRealPreview() bool {
	return s.messageID != "" && s.messageID != "__no_edit__"
}

// Whether fresh final is enabled and a real preview has been visible for the threshold
func (s *GatewayStreamConsumer) shouldSendFreshFinal() bool {
	threshold := s.cfg.FreshFinalAfterSeconds
	if threshold <= 0 || !s.hasRealPreview() || s.messageCreatedTS == nil {
		return false
	}
	return pythread.Monotonic()-*s.messageCreatedTS >= threshold
}

// Records a real preview message id for finalization cleanup
func (s *GatewayStreamConsumer) trackPreviewID(messageID string) {
	if messageID != "" && messageID != "__no_edit__" {
		s.previewMessageIDs.add(messageID)
		s.segmentPreviewMessageIDs.add(messageID)
	}
}

// Records the primary id plus any continuation ids of an oversized split
func (s *GatewayStreamConsumer) trackPreviewIDsFromResult(result SendResult) {
	ids := []any{result.MessageID}
	ids = append(ids, result.ContinuationMessageIDs...)
	if raw, ok := result.RawResponse.(map[string]any); ok {
		if rawIDs, ok := raw["message_ids"].([]any); ok {
			ids = append(ids, rawIDs...)
		}
	}
	for _, id := range ids {
		s.trackPreviewID(sendResultID(id))
	}
}

// Whether the adapter's fresh final hook asks for a new message, false without a real preview
func (s *GatewayStreamConsumer) adapterPrefersFreshFinal(text string) bool {
	if !s.hasRealPreview() {
		return false
	}
	return func() (prefers bool) {
		defer func() {
			if r := recover(); r != nil {
				streamLog().Debug(fmt.Sprintf("prefers_fresh_final_streaming check failed: %v", r))
				prefers = false
			}
		}()
		return s.adapter.PrefersFreshFinalStreaming(text, s.metadata, s.chatID)
	}()
}

// Sends the text fresh and deletes the previews, false on any failure so the caller edits instead
func (s *GatewayStreamConsumer) tryFreshFinal(ctx context.Context, text string, isTurnFinal bool) bool {
	if s.turnSplitDelivery {
		return false
	}
	staleIDs := s.stalePreviewIDs(false)
	result, err := s.adapter.Send(ctx, s.chatID, text, "", s.metadataForSend(true, false))
	if err != nil {
		streamLog().Debug("Fresh-final send failed, falling back to edit: " + pyrepr.ExceptionMessage(err))
		return false
	}
	if !result.Success {
		return false
	}
	newID := sendResultID(result.MessageID)
	s.deletePreviews(ctx, staleIDs, newID, "Fresh-final", false, true)
	s.previewMessageIDs = nil
	s.adoptMessageID(newID)
	s.alreadySent = true
	s.lastSentText = text
	if isTurnFinal {
		s.finalResponseSent = true
		s.recordTurnFinalPayload(text)
	}
	return true
}

// Retargets edits at the id, the no edit sentinel when the platform returned none
func (s *GatewayStreamConsumer) adoptMessageID(messageID string) {
	if messageID != "" {
		s.messageID = messageID
		now := pythread.Monotonic()
		s.messageCreatedTS = &now
		return
	}
	s.messageID = "__no_edit__"
	s.messageCreatedTS = nil
}

// Sends or edits the streaming message through the reply clock, true when delivered
func (s *GatewayStreamConsumer) sendOrEdit(ctx context.Context, text string, finalize, isTurnFinal bool) bool {
	ok, _ := observability.StopsReplyClock(ctx, s, func() (any, error) {
		return s.sendOrEditInner(ctx, text, finalize, isTurnFinal), nil
	})
	delivered, _ := ok.(bool)
	return delivered
}

// Transport ordered send or edit, native frame then draft then edit then first send
func (s *GatewayStreamConsumer) sendOrEditInner(ctx context.Context, text string, finalize, isTurnFinal bool) bool {
	text = cleanForDisplay(text)
	preFenceText := text
	text = EnsureClosedCodeFences(text)
	visible := text
	if s.cfg.Cursor != "" {
		visible = strings.ReplaceAll(text, s.cfg.Cursor, "")
	}
	visibleStripped := pystr.Strip(visible)
	if visibleStripped == "" {
		if finalize && s.useNativeStreaming.Load() && s.nativeStreamOpened.Load() {
			sent, err := s.sendFrame(ctx, "✅", true)
			if tryFrame(sent, err, "Finalize empty stream failed: %s", false) {
				s.markFinalDelivered(nil)
			}
		}
		return true
	}
	preambleFinalize := finalize && !isTurnFinal
	if s.messageID == "" && utf8.RuneCountInString(visibleStripped) < minNewMsgChars &&
		(preambleFinalize || (s.cfg.Cursor != "" && strings.Contains(text, s.cfg.Cursor))) {
		return true
	}
	if s.useNativeStreaming.Load() {
		if ok := s.nativePush(ctx, text, finalize, isTurnFinal); ok != nil {
			return *ok
		}
	}
	if s.useDraftStreaming && s.messageID == "" {
		if ok := s.draftPush(ctx, text, preFenceText, finalize, isTurnFinal); ok != nil {
			return *ok
		}
	}
	s.lastEditOverflowed = false
	if s.messageID == "" {
		if !s.editSupported && !finalize {
			return false
		}
		ok, err := s.firstSend(ctx, text, finalize)
		if err != nil {
			streamLog().Error("Stream send/edit error: " + pyrepr.ExceptionMessage(err))
			return false
		}
		return ok
	}
	if !s.editSupported {
		return false
	}
	ok, err := s.editExisting(ctx, text, finalize, isTurnFinal)
	if err != nil {
		streamLog().Error("Stream send/edit error: " + pyrepr.ExceptionMessage(err))
		return false
	}
	return ok
}

// Native frame push with a lazy re seed after a boundary, nil when native was disabled
func (s *GatewayStreamConsumer) nativePush(ctx context.Context, text string, finalize, isTurnFinal bool) *bool {
	yes := true
	if !s.nativeStreamOpened.Load() && text != "" {
		if !s.trySeedFrame(ctx, "Re-seed failed, disabling native streaming: %s", false) {
			s.useNativeStreaming.Store(false)
			return nil
		}
		s.nativeStreamOpened.Store(true)
		s.awaitingReopenAfterBoundary.Store(false)
		streamLog().Info(fmt.Sprintf("[latency] Re-opened native stream after boundary (turn=%s, waited for first delta)", s.turnID))
	}
	finalize = finalize && isTurnFinal
	if !finalize && text == s.lastSentText {
		return &yes
	}
	if finalize {
		s.markFinalDelivered(&text)
	}
	sent, err := s.sendFrame(ctx, text, finalize)
	if tryFrame(sent, err, "send_stream_frame raised, disabling native streaming: %s", false) {
		s.alreadySent = true
		s.lastSentText = text
		s.nativeLastPushedLen = utf8.RuneCountInString(text)
		if finalize {
			s.markFinalDelivered(nil)
		}
		return &yes
	}
	if finalize {
		s.finalResponseSent = false
		s.finalContentDelivered = false
		s.deliveredFinalText = nil
	}
	s.useNativeStreaming.Store(false)
	if s.nativeStreamOpened.Load() {
		if _, err := s.sendFrame(ctx, text, true); err != nil {
			streamLog().Debug("Native fallback: failed to finalize stream: " + pyrepr.ExceptionMessage(err))
		} else {
			streamLog().Debug("Native fallback: finalized stream (best-effort close)")
		}
	}
	return nil
}

// Draft frame while no message id exists, nil when not applicable or drafts just failed
func (s *GatewayStreamConsumer) draftPush(ctx context.Context, text, preFenceText string, finalize, isTurnFinal bool) *bool {
	streamIsMsg := s.streamIsMessage()
	if finalize && !(streamIsMsg && !isTurnFinal) {
		return nil
	}
	frameText := text
	if streamIsMsg {
		frameText = preFenceText
	}
	if s.cfg.Cursor != "" && strings.HasSuffix(frameText, s.cfg.Cursor) {
		frameText = strings.TrimSuffix(frameText, s.cfg.Cursor)
	}
	yes := true
	if frameText == s.lastSentText {
		return &yes
	}
	if s.sendDraftFrame(ctx, frameText) {
		return &yes
	}
	return nil
}

// First send threaded to the user's message
func (s *GatewayStreamConsumer) firstSend(ctx context.Context, text string, finalize bool) (bool, error) {
	if s.egressDeclined {
		streamLog().Warn("suppressing the plain-send fallback: the connector already declined this destination for this run")
		return false, nil
	}
	result, err := s.adapter.Send(ctx, s.chatID, text, s.initialReplyToID, s.metadataForSend(finalize, !finalize))
	if err != nil {
		return false, err
	}
	if !result.Success {
		s.editSupported = false
		return false, nil
	}
	s.alreadySent = true
	s.lastSentText = text
	if id := sendResultID(result.MessageID); id != "" {
		s.adoptMessageID(id)
		s.trackPreviewIDsFromResult(result)
	} else {
		s.enterFallbackMode(s.visiblePrefix())
		s.messageID = "__no_edit__"
	}
	s.notifyNewMessage()
	return true, nil
}

// Edits the live preview, or replaces it with a fresh final when the adapter prefers one
func (s *GatewayStreamConsumer) editExisting(ctx context.Context, text string, finalize, isTurnFinal bool) (bool, error) {
	if text == s.lastSentText && !(finalize && s.adapterRequiresFinalize) {
		return true, nil
	}
	hasPrefersHook := true
	prefersFresh := s.adapterPrefersFreshFinal(text)
	if finalize && (prefersFresh || (!hasPrefersHook && s.shouldSendFreshFinal())) && s.tryFreshFinal(ctx, text, isTurnFinal) {
		return true, nil
	}
	result, err := s.editMessage(ctx, s.messageID, text, finalize)
	if err != nil {
		return false, err
	}
	if !result.Success {
		return s.onEditFailure(ctx, result, text, finalize, isTurnFinal), nil
	}
	if raw, ok := result.RawResponse.(map[string]any); ok && pyrepr.Truthy(raw["skipped"]) {
		return true, nil
	}
	s.alreadySent = true
	s.trackPreviewIDsFromResult(result)
	if newID := sendResultID(result.MessageID); len(result.ContinuationMessageIDs) > 0 && newID != "" && newID != s.messageID {
		s.lastEditOverflowed = true
		s.turnSplitDelivery = true
		s.adoptMessageID(newID)
		s.lastSentText = ""
		s.notifyNewMessage()
	} else {
		s.lastSentText = text
	}
	s.floodStrikes = 0
	return true, nil
}

// Ends progressive edits for this stream, the missing tail sent at the end
func (s *GatewayStreamConsumer) enterFallbackMode(prefix string) {
	s.fallbackPrefix = prefix
	s.fallbackFinalSend = true
	s.editSupported = false
	s.alreadySent = true
}

// Classifies a failed edit as a decline, a partial overflow, flood backoff or fallback mode, always false
func (s *GatewayStreamConsumer) onEditFailure(ctx context.Context, result SendResult, text string, finalize, isTurnFinal bool) bool {
	if DeclinedSend(&result) {
		streamLog().Warn("edit DECLINED by the connector's egress guard; suppressing every later send for this run (the destination is not approved for this connection)")
		s.egressDeclined = true
		s.editSupported = false
		return false
	}
	turnFinal := finalize && isTurnFinal
	if turnFinal && s.cfg.Cursor != "" && strings.HasSuffix(s.lastSentText, s.cfg.Cursor) && s.visiblePrefix() == text {
		s.finalContentDelivered = true
		s.recordTurnFinalPayload(text)
	}
	if raw, ok := result.RawResponse.(map[string]any); ok && pyrepr.Truthy(raw["partial_overflow"]) {
		messageID := sendResultID(raw["last_message_id"])
		if messageID == "" {
			messageID = sendResultID(result.MessageID)
		}
		if messageID == "" {
			messageID = s.messageID
		}
		s.messageID = messageID
		if prefix, ok := raw["delivered_prefix"].(string); ok && prefix != "" {
			s.lastSentText = prefix
			s.fallbackPreservePartialMessages = strings.HasPrefix(text, prefix)
			s.enterFallbackMode(prefix)
		} else {
			s.fallbackPreservePartialMessages = false
			s.enterFallbackMode(s.visiblePrefix())
		}
		if len(result.ContinuationMessageIDs) > 0 {
			s.notifyNewMessage()
		}
		return false
	}
	immediateFinalFallback := false
	if isFloodError(result) {
		s.floodStrikes++
		backoff := min(s.currentEditInterval*2, 10.0)
		if result.RetryAfter != nil && *result.RetryAfter > 0 {
			backoff = max(backoff, min(*result.RetryAfter, maxEditBackoffSecs))
		}
		s.currentEditInterval = backoff
		streamLog().Debug(fmt.Sprintf("Flood control on edit (strike %d/%d), backoff interval → %.1fs", s.floodStrikes, maxFloodStrikes, s.currentEditInterval))
		if flood, ok := s.adapter.(FinalEditFloodFallback); ok {
			immediateFinalFallback = turnFinal && flood.FallbackOnFinalEditFlood()
		}
		if s.floodStrikes < maxFloodStrikes && !immediateFinalFallback {
			s.lastEditTime = pythread.Monotonic()
			return false
		}
		if immediateFinalFallback {
			streamLog().Debug("Turn-final edit hit flood control; entering fallback immediately")
		}
	}
	streamLog().Debug(fmt.Sprintf("Edit failed (strikes=%d), entering fallback mode", s.floodStrikes))
	s.enterFallbackMode(s.visiblePrefix())
	if !immediateFinalFallback {
		s.tryStripCursor(ctx)
	}
	return false
}
