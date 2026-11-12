package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Seconds the one shot post delivery callback may run before it is abandoned
const postDeliveryCallbackTimeoutSeconds = 30.0

const (
	requeueBackoffInitialSeconds = 0.25
	requeueBackoffMaxSeconds     = 1.0
)

var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}

// Deliverable parts of a handler response
type extractedResponse struct {
	textContent              string
	images                   []ImageRef
	mediaFiles               []MediaRef
	localFiles               []string
	forceDocumentAttachments bool
	preExtract               string
}

// Runner seam clearing the durable crash marker of a turn whose reply is ledgered or owed nothing
type DurableActiveTurnClearer interface {
	ClearDurableActiveTurn(ctx context.Context, event *MessageEvent) error
}

// Runner seam sweeping the failed obligations of a platform once a replacement adapter is live
type FailedObligationRedeliverer interface {
	RedeliverFailedObligationsForPlatform(ctx context.Context, platform Platform, profile *string) int
}

// Runner seam arming the timed redelivery after a refused final reply
type FloodRedeliveryScheduler interface {
	ScheduleFloodRedelivery(ctx context.Context, platform Platform, profile *string)
}

// Runner seam naming the routed profile of a source, a refused route raising *ProfileRouteRejected
type SourceProfileResolver interface {
	ProfileNameForSource(ctx context.Context, source *SessionSource, adapterProfile *string) (*string, error)
}

// Keyword fields of BuildSource beyond the chat id, unset pointers standing for None
type SourceFields struct {
	ChatName              *string
	ChatType              string
	UserID                *string
	UserName              *string
	ThreadID              *string
	ChatTopic             *string
	UserIDAlt             *string
	ChatIDAlt             *string
	IsBot                 bool
	ScopeID               *string
	GuildID               *string
	ParentChatID          *string
	MessageID             *string
	RoleAuthorized        bool
	AutoThreadCreated     bool
	AutoThreadInitialName *string
}

// Owner profile of an adapter as an optional value
func ownerProfileOf(adapter PlatformAdapter) *string {
	if p := adapter.Base().OwnerProfile(); p != "" {
		return &p
	}
	return nil
}

// Cancellation of the running task, the asyncio CancelledError of hermes
func taskCancelled(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	return nil
}

// Sleeps the given seconds unless the task is cancelled first
func sleepSeconds(ctx context.Context, seconds float64) error {
	timer := time.NewTimer(secondsDuration(seconds))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Ledgers the final reply before the send, nil for commands, ephemeral replies, a disabled ledger or a failure
func (b *BasePlatformAdapter) recordDeliveryObligation(ctx context.Context, event *MessageEvent, sessionKey, textContent string, deliveryAdapter PlatformAdapter, isEphemeral bool) *string {
	prefix := b.Traits.TypedCommandPrefix
	if prefix == "" {
		prefix = "!"
	}
	stripped := pystr.Lstrip(event.Text)
	if isEphemeral || strings.HasPrefix(stripped, "/") || strings.HasPrefix(stripped, prefix) {
		return nil
	}
	id, err := func() (*string, error) {
		if !LedgerEnabled(ctx, nil) {
			return nil, nil
		}
		source := event.Source
		ref := ""
		if event.LedgerMessageID != nil {
			ref = *event.LedgerMessageID
		} else if event.MessageID != nil {
			ref = *event.MessageID
		}
		oid := ComputeObligationID(sessionKey, ref, textContent)
		if err := RecordObligation(ctx, DeliveryObligation{ObligationID: oid, SessionKey: sessionKey,
			Platform: string(source.Platform), ChatID: source.ChatID, ThreadID: source.ThreadID, Content: textContent,
			AdapterProfile: ownerProfileOf(deliveryAdapter)}); err != nil {
			return nil, err
		}
		if err := MarkAttempting(ctx, oid); err != nil {
			return nil, err
		}
		return &oid, nil
	}()
	if err != nil {
		slog.Debug("delivery ledger record failed", "error", err)
		return nil
	}
	return id
}

// Marks the ledger row delivered or failed and arms the matching redelivery for a refused final
func (b *BasePlatformAdapter) finalizeDeliveryObligation(ctx context.Context, obligationID string, result SendResult, event *MessageEvent, deliveryAdapter PlatformAdapter) {
	err := func() error {
		if result.Success {
			return MarkDelivered(ctx, obligationID)
		}
		errText := ""
		if pyrepr.Truthy(result.Error) {
			errText = pyrepr.Str(result.Error)
		}
		if err := MarkFailed(ctx, obligationID, errText); err != nil {
			return err
		}
		profile := ownerProfileOf(deliveryAdapter)
		if IsReconnectOnly(errText) {
			live := b.finalDeliveryAdapter(ctx, event.Source)
			if redeliver, ok := b.GatewayRunner.(FailedObligationRedeliverer); ok && live != deliveryAdapter {
				redeliver.RedeliverFailedObligationsForPlatform(ctx, event.Source.Platform, profile)
			}
		} else if ClassifyDeadError(errText) == "" {
			if schedule, ok := b.GatewayRunner.(FloodRedeliveryScheduler); ok {
				schedule.ScheduleFloodRedelivery(ctx, event.Source.Platform, profile)
			}
		}
		return nil
	}()
	if err != nil {
		slog.Debug("delivery ledger update failed", "error", err)
	}
}

// Delivers MEDIA tag files and bare local files by type, every failure reported and every send recorded
func (b *BasePlatformAdapter) deliverMediaAttachments(ctx context.Context, event *MessageEvent, mediaFiles []MediaRef, localFiles []string, forceDocument bool, humanDelay float64, metadata map[string]any, recordDelivery func(SendResult)) error {
	self := b.Self()
	asImage := func(path string) bool {
		return imageExts[pystr.Lower(pypath.PathSuffix(path))] && !forceDocument
	}
	var images []ImageRef
	for _, m := range mediaFiles {
		if !m.IsVoice && asImage(m.Path) {
			images = append(images, ImageRef{URL: "file://" + pyurl.Quote(m.Path, "/")})
		}
	}
	for _, p := range localFiles {
		if asImage(p) {
			images = append(images, ImageRef{URL: "file://" + pyurl.Quote(p, "/")})
		}
	}
	if len(images) > 0 {
		b.sendImageBatch(ctx, event, images, metadata, humanDelay, recordDelivery)
		if err := taskCancelled(ctx); err != nil {
			return err
		}
	}
	chatID := event.Source.ChatID
	sendOne := func(path string, isVoice, mediaTag bool) (SendResult, error) {
		ext := pystr.Lower(pypath.PathSuffix(path))
		var result SendResult
		var err error
		switch {
		case mediaTag && ShouldSendMediaAsAudio(b.Platform, ext, isVoice):
			result, err = self.SendVoice(ctx, chatID, path, "", "", metadata, map[string]any{"is_voice": isVoice})
		case videoExts[ext]:
			if mediaTag {
				slog.Info(fmt.Sprintf("[%s] Sending video attachment (%s) to %s", self.Name(), ext, chatID))
			}
			result, err = self.SendVideo(ctx, chatID, path, "", "", metadata, nil)
		default:
			result, err = self.SendDocument(ctx, chatID, path, "", "", "", metadata, nil)
		}
		if err != nil {
			return result, err
		}
		if !result.Success {
			kind := "local file"
			if mediaTag {
				kind = "media"
			}
			slog.Warn(fmt.Sprintf("[%s] Failed to send %s (%s): %s", self.Name(), kind, ext, pyrepr.Str(result.Error)))
			raw, _ := result.RawResponse.(map[string]any)
			if raw == nil || !pyrepr.Truthy(raw["failure_notice_sent"]) {
				b.notifyMediaDeliveryFailure(ctx, chatID, path, isVoice, metadata)
			}
		}
		return result, nil
	}
	type queued struct {
		path              string
		isVoice, mediaTag bool
	}
	var queue []queued
	for _, m := range mediaFiles {
		if m.IsVoice || !asImage(m.Path) {
			queue = append(queue, queued{m.Path, m.IsVoice, true})
		}
	}
	if len(queue) > 0 {
		slog.Info(fmt.Sprintf("[%s] Delivering %d non-image MEDIA attachment(s)", self.Name(), len(queue)))
	}
	for _, p := range localFiles {
		if !asImage(p) {
			queue = append(queue, queued{p, false, false})
		}
	}
	for _, item := range queue {
		if humanDelay > 0 {
			if err := sleepSeconds(ctx, humanDelay); err != nil {
				return err
			}
		}
		result, err := sendOne(item.path, item.isVoice, item.mediaTag)
		if cancelled := taskCancelled(ctx); cancelled != nil {
			return cancelled
		}
		if err != nil {
			recordDelivery(SendResult{Success: false, Error: err.Error()})
			if item.mediaTag {
				slog.Warn(fmt.Sprintf("[%s] Error sending media: %v", self.Name(), err))
			} else {
				slog.Error(fmt.Sprintf("[%s] Error sending local file %s: %v", self.Name(), item.path, err))
			}
			continue
		}
		recordDelivery(result)
	}
	return nil
}

// Batch sends images, a failure logged and recorded so the other attachments still go
func (b *BasePlatformAdapter) sendImageBatch(ctx context.Context, event *MessageEvent, images []ImageRef, metadata map[string]any, humanDelay float64, recordDelivery func(SendResult)) {
	result, err := b.Self().SendMultipleImages(ctx, event.Source.ChatID, images, metadata, humanDelay)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Error batching images: %v", b.Self().Name(), err))
		recordDelivery(SendResult{Success: false, Error: err.Error()})
		return
	}
	recordDelivery(result)
}

// Ledger bracket of a final text on the current transport, returning the result with the adapter that sent it
func (b *BasePlatformAdapter) SendFinalLedgered(ctx context.Context, event *MessageEvent, sessionKey, textContent string, metadata map[string]any, replyTo *string, isEphemeral bool) (SendResult, PlatformAdapter, error) {
	deliveryAdapter := b.finalDeliveryAdapter(ctx, event.Source)
	slog.Info(fmt.Sprintf("[%s] Sending response (%d chars) to %s", deliveryAdapter.Name(), pystr.Len(textContent), event.Source.ChatID))
	var obligationID *string
	if recorder, overridden := b.Self().(deliveryObligationRecorder); overridden {
		obligationID = recorder.RecordDeliveryObligation(ctx, event, sessionKey, textContent, deliveryAdapter, isEphemeral)
	} else {
		obligationID = b.recordDeliveryObligation(ctx, event, sessionKey, textContent, deliveryAdapter, isEphemeral)
	}
	if obligationID != nil {
		if err := b.releaseTurnMarker(ctx, event); err != nil {
			return SendResult{}, deliveryAdapter, err
		}
	}
	reply := ""
	if replyTo != nil {
		reply = *replyTo
	}
	result, err := deliveryAdapter.Base().SendWithRetry(ctx, event.Source.ChatID, textContent, reply, metadata, 2, 2.0)
	if err != nil {
		return result, deliveryAdapter, err
	}
	observability.StopReplyClock(ctx, deliveryAdapter, event.Source.ChatID, result)
	if obligationID != nil {
		b.finalizeDeliveryObligation(ctx, *obligationID, result, event, deliveryAdapter)
	}
	return result, deliveryAdapter, nil
}

// Clears the crash marker handed to this delivery lifecycle and wakes the waiting drain
func (b *BasePlatformAdapter) releaseTurnMarker(ctx context.Context, event *MessageEvent) error {
	if event.TurnMarkerHandoff() && pyrepr.Truthy(event.GatewayActiveTurnToken()) && b.GatewayRunner != nil {
		clearer, ok := b.GatewayRunner.(DurableActiveTurnClearer)
		if !ok {
			return fmt.Errorf("AttributeError: '%T' object has no attribute '_clear_durable_active_turn'", b.GatewayRunner)
		}
		if err := clearer.ClearDurableActiveTurn(ctx, event); err != nil {
			return err
		}
	}
	if released := event.GatewayMarkerReleased(); released != nil {
		released.Set()
	}
	return nil
}

// Normal lane final through the ledger bracket plus the ephemeral delete on the adapter owning the message id
func (b *BasePlatformAdapter) sendFinalText(ctx context.Context, event *MessageEvent, sessionKey, textContent string, metadata map[string]any, isEphemeral bool, ttl int, recordDelivery func(SendResult)) error {
	result, deliveryAdapter, err := b.SendFinalLedgered(ctx, event, sessionKey, textContent, metadata, replyAnchorForEvent(event), isEphemeral)
	if err != nil {
		return err
	}
	recordDelivery(result)
	if ttl > 0 && result.Success && pyrepr.Truthy(result.MessageID) {
		deliveryAdapter.Base().ScheduleEphemeralDelete(ctx, event.Source.ChatID, pyrepr.Str(result.MessageID), ttl)
	}
	return nil
}

// Tells the user a turn failed unless the diagnostic wake is muted, returning the thread metadata used
func (b *BasePlatformAdapter) notifyTurnError(ctx context.Context, event *MessageEvent, turnErr error) map[string]any {
	self := b.Self()
	var threadMetadata map[string]any
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		threadMetadata = threadMetadataForEvent(event)
		detail := pyrepr.ExceptionMessage(turnErr)
		if detail != "" {
			detail = pystr.Slice(detail, 0, 300)
		} else {
			detail = i18n.T(ctx, "gateway.notify.turn_error_no_details", "", nil)
		}
		scoped, release := b.mediaDeliveryScope(ctx, event.Source)
		var content *string
		if !config.DiagnosticWakeMuted(scoped, event, nil) {
			fallback := i18n.T(scoped, "gateway.notify.turn_error", "", nil)
			detailed := i18n.T(scoped, "gateway.notify.turn_error_detailed", "", map[string]any{
				"error_type": pyrepr.ExceptionName(turnErr), "error_detail": detail})
			content = self.WarningText(scoped, detailed, &fallback, event.Source.Platform, event.Source.ChatID, threadMetadata)
		}
		release()
		if content == nil {
			return nil
		}
		_, err = self.Send(ctx, event.Source.ChatID, *content, "", threadMetadata)
		return err
	}()
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to send error notification to user: %v", self.Name(), err))
	}
	return threadMetadata
}

// Sends extracted images, MEDIA files and local files, logging a non empty response that delivered nothing
func (b *BasePlatformAdapter) deliverAttachments(ctx context.Context, event *MessageEvent, extracted *extractedResponse, metadata map[string]any, anythingSent bool, recordDelivery func(SendResult)) error {
	humanDelay := b.getHumanDelay()
	if len(extracted.images) > 0 {
		slog.Info(fmt.Sprintf("[%s] Extracted %d image(s) to send as attachments", b.Self().Name(), len(extracted.images)))
		b.sendImageBatch(ctx, event, extracted.images, metadata, humanDelay, recordDelivery)
		if err := taskCancelled(ctx); err != nil {
			return err
		}
	}
	if err := b.deliverMediaAttachments(ctx, event, extracted.mediaFiles, extracted.localFiles,
		extracted.forceDocumentAttachments, humanDelay, metadata, recordDelivery); err != nil {
		return err
	}
	if !(anythingSent || len(extracted.images) > 0 || len(extracted.localFiles) > 0 || len(extracted.mediaFiles) > 0) &&
		pystr.Strip(extracted.preExtract) != "" {
		slog.Error(fmt.Sprintf("[%s] response_delivery_dropped: non-empty response "+
			"(%d chars) produced no delivered message or attachment "+
			"for %s (empty after extract, recovery yielded nothing).", b.Self().Name(),
			pystr.Len(extracted.preExtract), event.Source.ChatID))
	}
	return nil
}

// Spawns the typing refresh task, nil when the platform disables typing or the turn is a scheduled heartbeat
func (b *BasePlatformAdapter) startTypingRefresh(ctx context.Context, event *MessageEvent, interruptEvent *InterruptEvent, metadata map[string]any) *adapterTask {
	if (b.Config != nil && !b.Config.TypingIndicator) || pyrepr.Truthy(event.HeartbeatSessionID()) {
		return nil
	}
	self := b.Self()
	chatID := event.Source.ChatID
	return spawnAdapterTask(ctx, func(tctx context.Context) error {
		self.KeepTyping(tctx, chatID, 2.0, metadata, interruptEvent)
		return nil
	})
}

// Splits a handler response into deliverable text and attachments, recovering a response extraction emptied
func (b *BasePlatformAdapter) extractResponseContent(ctx context.Context, response string, event *MessageEvent, sessionKey string, isEphemeral bool) (*extractedResponse, error) {
	name := b.Self().Name()
	forceDocument := strings.Contains(response, "[[as_document]]")
	preExtract := response
	scoped, release := b.mediaDeliveryScope(ctx, event.Source)
	mediaFiles, images, textContent, localFiles, response, err := func() ([]MediaRef, []ImageRef, string, []string, string, error) {
		defer release()
		mediaFiles, rest, err := ExtractMedia(scoped, response)
		if err != nil {
			return nil, nil, "", nil, response, err
		}
		if mediaFiles, err = FilterMediaDeliveryPathsFor(scoped, mediaFiles, sessionKey, nil); err != nil {
			return nil, nil, "", nil, rest, err
		}
		images, text := ExtractImages(rest)
		if text, err = stripMediaDirectives(scoped, text); err != nil {
			return nil, nil, "", nil, rest, err
		}
		text = pystr.Strip(text)
		if len(images) > 0 {
			slog.Info(fmt.Sprintf("[%s] extract_images found %d image(s) in response (%d chars)", name, len(images), pystr.Len(rest)))
		}
		var localFiles []string
		if !isEphemeral {
			localFiles, text = ExtractLocalFiles(scoped, text)
			if localFiles, err = FilterLocalDeliveryPaths(scoped, localFiles, sessionKey); err != nil {
				return nil, nil, "", nil, rest, err
			}
		}
		return mediaFiles, images, text, localFiles, rest, nil
	}()
	if err != nil {
		return nil, err
	}
	if len(localFiles) > 0 {
		history, err := b.boundedHistoryMediaPathsForSession(ctx, sessionKey)
		if err != nil {
			return nil, err
		}
		if len(history) > 0 {
			var suppressed, kept []string
			for _, p := range localFiles {
				if _, seen := history[p]; seen {
					suppressed = append(suppressed, p)
				} else {
					kept = append(kept, p)
				}
			}
			if len(suppressed) > 0 {
				slog.Info(fmt.Sprintf("[%s] Suppressing %d bare local file path(s) already delivered in "+
					"this session: %s", name, len(suppressed), pyrepr.Repr(suppressed)))
				localFiles = kept
			}
		}
	}
	if len(localFiles) > 0 {
		slog.Info(fmt.Sprintf("[%s] extract_local_files found %d file(s) in response", name, len(localFiles)))
	}
	if textContent == "" && len(images) == 0 && len(localFiles) == 0 && len(mediaFiles) == 0 {
		recovered, err := stripMediaDirectives(ctx, response)
		if err != nil {
			return nil, err
		}
		if recovered = pystr.Strip(recovered); recovered != "" {
			slog.Warn(fmt.Sprintf("[%s] response_delivery_recovered: extract pipeline "+
				"reduced a non-empty response (%d chars) to empty with "+
				"no attachment; delivering recovered original to %s", name, pystr.Len(preExtract), event.Source.ChatID))
			textContent = recovered
		}
	}
	return &extractedResponse{textContent: textContent, images: images, mediaFiles: mediaFiles, localFiles: localFiles,
		forceDocumentAttachments: forceDocument, preExtract: preExtract}, nil
}

// Runs the one shot post delivery callback of the generation stamped on the interrupt event during the turn
func (b *BasePlatformAdapter) firePostDeliveryCallback(ctx context.Context, sessionKey string, interruptEvent *InterruptEvent) {
	var generation *int
	if g, ok := interruptEvent.RunGeneration().(int); ok {
		generation = &g
	}
	RunPostDeliveryCallback(ctx, b.PopPostDeliveryCallback(sessionKey, generation), postDeliveryCallbackTimeoutSeconds)
}

// Reconciles session ownership at task end, handing a late pending message to the owner or a fresh drain
func (b *BasePlatformAdapter) finishSessionTask(ctx context.Context, sessionKey string, interruptEvent *InterruptEvent) {
	current := currentAdapterTask(ctx)
	b.mu.Lock()
	late, hasLate := b.pendingMessages[sessionKey]
	delete(b.pendingMessages, sessionKey)
	if hasLate && late != nil {
		existing := b.sessionTasks[sessionKey]
		if existing != nil && existing != current {
			b.pendingMessages[sessionKey] = late
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()
		slog.Debug(fmt.Sprintf("[%s] Late-arrival pending message during cleanup — spawning drain task", b.Self().Name()))
		b.spawnDrainTask(ctx, late, sessionKey, 0)
		return
	}
	owns := current != nil && b.sessionTasks[sessionKey] == current
	b.mu.Unlock()
	if owns {
		b.cleanupFinishedSessionTask(sessionKey, interruptEvent)
	}
}

// Processes one inbound turn end to end, delivering the reply and handing any queued follow up to a drain task
func (b *BasePlatformAdapter) ProcessMessageBackground(ctx context.Context, event *MessageEvent, sessionKey string) (retErr error) {
	self := b.Self()
	deliveryAttempted, deliverySucceeded := false, false
	recordDelivery := func(result SendResult) {
		deliveryAttempted = true
		deliverySucceeded = deliverySucceeded || result.Success
	}
	b.mu.Lock()
	interruptEvent := b.activeSessions[sessionKey]
	if interruptEvent == nil {
		interruptEvent = NewInterruptEvent()
	}
	lazyAttr(&b.activeSessions)[sessionKey] = interruptEvent
	b.mu.Unlock()
	chatID := event.Source.ChatID
	threadMetadata := threadMetadataForEvent(event)
	typingMetadata := typingMetadataForTurn(self, chatID, replyAnchorForEvent(event), threadMetadata)
	typingTask := b.startTypingRefresh(ctx, event, interruptEvent, typingMetadata)
	fctx := context.WithoutCancel(ctx)
	defer func() {
		if err := b.releaseTurnMarker(fctx, event); err != nil && retErr == nil {
			retErr = err
		}
		event.setTurnMarkerHandoff(false)
		b.stopTypingRefresh(fctx, chatID, typingTask, typingMetadata, 0.5, 2)
		b.firePostDeliveryCallback(fctx, sessionKey, interruptEvent)
		b.stopTypingRefresh(fctx, chatID, nil, typingMetadata, 0.5, 1)
		b.flushTextDebounceNow(fctx, sessionKey)
		b.finishSessionTask(fctx, sessionKey, interruptEvent)
	}()
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		b.runProcessingHook(ctx, "on_processing_start", func(c context.Context) error { return self.OnProcessingStart(c, event) })
		event.setTurnMarkerHandoff(b.GatewayRunner != nil)
		b.mu.Lock()
		handler := b.messageHandler
		b.mu.Unlock()
		if handler == nil {
			return errors.New("TypeError: 'NoneType' object is not callable")
		}
		response, err := handler(ctx, event)
		if err != nil {
			return err
		}
		if err := taskCancelled(ctx); err != nil {
			return err
		}
		scoped, release := b.mediaDeliveryScope(ctx, event.Source)
		if config.DiagnosticWakeMuted(scoped, event, nil) {
			response = nil
		}
		release()
		_, isEphemeral := response.(*EphemeralReply)
		response, ttl := b.unwrapEphemeral(ctx, response)
		text := handlerResponseText(response)
		b.mu.Lock()
		_, pendingNow := b.pendingMessages[sessionKey]
		b.mu.Unlock()
		if text != "" && interruptEvent.IsSet() && pendingNow {
			slog.Info(fmt.Sprintf("[%s] Suppressing stale response for interrupted session %s", self.Name(), sessionKey))
			text = ""
		}
		if text == "" {
			slog.Debug(fmt.Sprintf("[%s] Handler returned empty/None response for %s", self.Name(), chatID))
		} else {
			extracted, err := b.extractResponseContent(ctx, text, event, sessionKey, isEphemeral)
			if err != nil {
				return err
			}
			textContent := extracted.textContent
			finalMetadata := markNotifyMetadata(threadMetadata)
			var ttsPaths []string
			var ttsRequested *string
			if b.wantsAutoTTS(event, sessionKey, interruptEvent, textContent, extracted.mediaFiles) {
				ttsPaths, ttsRequested = b.synthesizeAutoTTS(ctx, textContent)
			}
			captionDelivered := false
			for i, path := range ttsPaths {
				delivered, err := b.playTTSFile(ctx, event, textContent, path, i == 0, finalMetadata, recordDelivery)
				_ = os.Remove(path)
				if err != nil {
					return err
				}
				captionDelivered = captionDelivered || delivered
			}
			if len(ttsPaths) == 0 && ttsRequested != nil {
				_ = os.Remove(*ttsRequested)
			}
			if err := taskCancelled(ctx); err != nil {
				return err
			}
			if textContent != "" || len(extracted.images) > 0 || len(extracted.mediaFiles) > 0 ||
				len(extracted.localFiles) > 0 || len(ttsPaths) > 0 || captionDelivered {
				self.Base().PauseTypingForChat(chatID)
			}
			if textContent != "" && !captionDelivered {
				if err := b.sendFinalText(ctx, event, sessionKey, textContent, finalMetadata, isEphemeral, ttl, recordDelivery); err != nil {
					return err
				}
				if err := taskCancelled(ctx); err != nil {
					return err
				}
			}
			if err := b.deliverAttachments(ctx, event, extracted, finalMetadata, deliveryAttempted || captionDelivered, recordDelivery); err != nil {
				return err
			}
		}
		if err := b.releaseTurnMarker(ctx, event); err != nil {
			return err
		}
		processingOK := text == ""
		if deliveryAttempted {
			processingOK = deliverySucceeded
		}
		if key, ok := StreamingTTSTurnKey(sessionKey, interruptEvent.RunGeneration(), event); ok {
			b.mu.Lock()
			delete(b.streamingTTSCompletedTurns, key)
			b.mu.Unlock()
		} else {
			b.mu.Lock()
			delete(b.streamingTTSCompletedTurns, "")
			b.mu.Unlock()
		}
		outcome := ProcessingFailure
		if processingOK {
			outcome = ProcessingSuccess
		}
		b.runProcessingHook(ctx, "on_processing_complete", func(c context.Context) error { return self.OnProcessingComplete(c, event, outcome) })
		b.flushTextDebounceNow(ctx, sessionKey)
		b.mu.Lock()
		pendingEvent, queued := b.pendingMessages[sessionKey]
		b.mu.Unlock()
		if queued {
			delay := b.requeueBackoffDelay(sessionKey, pendingEvent, event)
			if delay == 0 {
				b.mu.Lock()
				delete(b.pendingMessages, sessionKey)
				b.mu.Unlock()
			}
			slog.Debug(fmt.Sprintf("[%s] Processing queued follow-up message", self.Name()))
			b.clearSessionGuard(sessionKey)
			b.stopTypingRefresh(ctx, chatID, typingTask, typingMetadata, 0.5, 2)
			b.spawnDrainTask(ctx, pendingEvent, sessionKey, delay)
		}
		return nil
	}()
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) && taskCancelled(ctx) != nil {
		current := currentAdapterTask(ctx)
		b.mu.Lock()
		_, expected := b.expectedCancelledTasks[current]
		b.mu.Unlock()
		outcome := ProcessingFailure
		if expected && current != nil {
			outcome = ProcessingCancelled
		}
		b.runProcessingHook(fctx, "on_processing_complete", func(c context.Context) error { return self.OnProcessingComplete(c, event, outcome) })
		return err
	}
	b.runProcessingHook(fctx, "on_processing_complete", func(c context.Context) error {
		return self.OnProcessingComplete(c, event, ProcessingFailure)
	})
	slog.Error(fmt.Sprintf("[%s] Error handling message: %v", self.Name(), err))
	b.notifyTurnError(fctx, event, err)
	return nil
}

// Delay before redispatching a queued follow up, backing off only the event this task just dispatched coming back
func (b *BasePlatformAdapter) requeueBackoffDelay(sessionKey string, pending, dispatched *MessageEvent) float64 {
	idSet := pending.MessageID != nil && *pending.MessageID != ""
	sameID := (pending.MessageID == nil && dispatched.MessageID == nil) ||
		(pending.MessageID != nil && dispatched.MessageID != nil && *pending.MessageID == *dispatched.MessageID)
	same := sameID && (idSet || pending.Timestamp.Equal(dispatched.Timestamp))
	b.mu.Lock()
	if !same {
		delete(b.requeueCounts, sessionKey)
		b.mu.Unlock()
		return 0
	}
	attempts := b.requeueCounts[sessionKey]
	lazyAttr(&b.requeueCounts)[sessionKey] = attempts + 1
	b.mu.Unlock()
	if attempts == 0 {
		return 0
	}
	delay := provider.JitteredBackoff(attempts, provider.BackoffOptions{
		BaseDelay: secondsDuration(requeueBackoffInitialSeconds), MaxDelay: secondsDuration(requeueBackoffMaxSeconds)}).Seconds()
	msg := fmt.Sprintf("[%s] Handler re-queued a pending event for %s again (session busy elsewhere); "+
		"backing off %.2fs", b.Self().Name(), sessionKey, delay)
	if attempts == 1 {
		slog.Info(msg)
	} else {
		slog.Debug(msg)
	}
	return delay
}

// Hands the session to a fresh drain task for a queued follow up, the guard kept live and ownership moved
func (b *BasePlatformAdapter) spawnDrainTask(ctx context.Context, pending *MessageEvent, sessionKey string, delay float64) {
	b.clearSessionGuard(sessionKey)
	b.mu.Lock()
	guard := b.activeSessions[sessionKey]
	b.mu.Unlock()
	ready := make(chan struct{})
	task := spawnAdapterTask(ctx, func(tctx context.Context) error {
		<-ready
		return b.drainAfter(tctx, pending, sessionKey, delay, guard)
	})
	b.trackSessionTask(sessionKey, task)
	close(ready)
}

// Waits out the back off, then processes the follow up still queued, releasing the guard when it was consumed
func (b *BasePlatformAdapter) drainAfter(ctx context.Context, pending *MessageEvent, sessionKey string, delay float64, guard *InterruptEvent) error {
	if delay > 0 {
		if err := sleepSeconds(ctx, delay); err != nil {
			return err
		}
		b.flushTextDebounceNow(ctx, sessionKey)
		b.mu.Lock()
		next, ok := b.pendingMessages[sessionKey]
		delete(b.pendingMessages, sessionKey)
		b.mu.Unlock()
		if !ok || next == nil {
			b.cleanupFinishedSessionTask(sessionKey, guard)
			return nil
		}
		pending = next
	}
	return b.Self().ProcessMessageBackground(ctx, pending, sessionKey)
}

// Clears without deleting the interrupt event of the session so the guard stays live for inbound
func (b *BasePlatformAdapter) clearSessionGuard(sessionKey string) {
	b.mu.Lock()
	active := b.activeSessions[sessionKey]
	b.mu.Unlock()
	if active != nil {
		active.Clear()
	}
}

// Releases a finished owner's guard, dropping its task entry only when the guard was really released
func (b *BasePlatformAdapter) cleanupFinishedSessionTask(sessionKey string, interruptEvent *InterruptEvent) {
	b.releaseSessionGuard(sessionKey, interruptEvent)
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.activeSessions[sessionKey]; !ok {
		delete(b.sessionTasks, sessionKey)
		delete(b.requeueCounts, sessionKey)
	}
}

// Cancels in flight background tasks within five seconds per round, flushes pending input to disk and clears all state
func (b *BasePlatformAdapter) CancelBackgroundTasks(ctx context.Context) {
	for round := 0; round < 5; round++ {
		b.mu.Lock()
		var tasks []*adapterTask
		for task := range b.backgroundTasks {
			if !task.finished() {
				tasks = append(tasks, task)
				lazyAttr(&b.expectedCancelledTasks)[task] = struct{}{}
			}
		}
		b.mu.Unlock()
		if len(tasks) == 0 {
			break
		}
		for _, task := range tasks {
			task.cancel()
		}
		deadline := time.NewTimer(5 * time.Second)
		timedOut := false
		for _, task := range tasks {
			select {
			case <-task.done:
				continue
			case <-deadline.C:
				timedOut = true
			}
			break
		}
		deadline.Stop()
		if timedOut {
			stragglers := 0
			for _, task := range tasks {
				if !task.finished() {
					stragglers++
				}
			}
			slog.Warn(fmt.Sprintf("[%s] %d background task(s) did not exit within 5s; "+
				"releasing tracking and letting them unwind in the background", b.Self().Name(), stragglers))
			break
		}
	}
	b.mu.Lock()
	pending := maps.Clone(b.pendingMessages)
	b.mu.Unlock()
	func() {
		defer func() { _ = recover() }()
		_, _ = FlushPendingToFile(ctx, pending, "adapter_shutdown")
	}()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, state := range b.textDebounce {
		state.CancelTimer(nil)
	}
	clear(b.backgroundTasks)
	clear(b.expectedCancelledTasks)
	clear(b.sessionTasks)
	clear(b.pendingMessages)
	clear(b.activeSessions)
	clear(b.requeueCounts)
	clear(b.textDebounce)
}

// Session source of an inbound chat stamped with the routed profile and this adapter as its transport
func (b *BasePlatformAdapter) BuildSource(ctx context.Context, chatID string, f SourceFields) *SessionSource {
	opt := func(v *string) *string {
		if v == nil || *v == "" {
			return nil
		}
		s := *v
		return &s
	}
	chatType := f.ChatType
	if chatType == "" {
		chatType = "dm"
	}
	var topic *string
	if f.ChatTopic != nil {
		if s := pystr.Strip(*f.ChatTopic); s != "" {
			topic = &s
		}
	}
	fields := SessionSource{Platform: b.Platform, ChatID: chatID, ChatName: f.ChatName, ChatType: chatType,
		UserID: opt(f.UserID), UserName: f.UserName, ThreadID: opt(f.ThreadID), ChatTopic: topic, UserIDAlt: f.UserIDAlt,
		ChatIDAlt: f.ChatIDAlt, IsBot: f.IsBot, ScopeID: opt(f.ScopeID), GuildID: opt(f.GuildID),
		ParentChatID: opt(f.ParentChatID), MessageID: opt(f.MessageID)}
	ownerProfile := ownerProfileOf(b.Self())
	profile, rejected := ownerProfile, false
	var rejectedProfile *string
	if b.GatewayRunner != nil {
		probe := fields
		probe.Normalize()
		var err error
		var routed *string
		if resolver, ok := b.GatewayRunner.(SourceProfileResolver); ok {
			routed, err = resolver.ProfileNameForSource(ctx, &probe, ownerProfile)
		} else {
			err = fmt.Errorf("AttributeError: '%T' object has no attribute '_profile_name_for_source'", b.GatewayRunner)
		}
		var refusal *ProfileRouteRejected
		switch {
		case errors.As(err, &refusal):
			rejected = true
			if p := refusal.Profile(); p != nil {
				s := pyrepr.Str(p)
				rejectedProfile = &s
			}
		case err != nil:
			slog.Warn(fmt.Sprintf("Profile resolution failed for %s/%s, defaulting to active profile", b.Platform, chatID), "error", err)
		case routed != nil && *routed != "":
			profile = routed
		}
	}
	source := fields
	source.Profile = profile
	source.RoleAuthorized = f.RoleAuthorized
	source.AutoThreadCreated = f.AutoThreadCreated
	source.AutoThreadInitialName = f.AutoThreadInitialName
	source.Normalize()
	source.transportAdapterRef = b.Self()
	source.ProfileRouteRejected = rejected
	source.rejectedRouteProfile = rejectedProfile
	return &source
}

// Per source toolset override replacing the platform toolsets, false for the default
func (b *BasePlatformAdapter) ToolsetsForSource(source *SessionSource) ([]string, bool) {
	return nil, false
}

// Content formatted for this platform, unchanged by default
func (b *BasePlatformAdapter) FormatMessage(content string) string {
	return content
}
