package googlechat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Text body with the thread name when replying into a thread
func threadBody(text, threadID string) map[string]any {
	body := map[string]any{"text": text}
	if threadID != "" {
		body["thread"] = map[string]any{"name": threadID}
	}
	return body
}

// Translated platform text
func tr(ctx context.Context, key string, kwargs map[string]any) string {
	return i18n.T(ctx, "platform.google_chat."+key, "", kwargs)
}

// Hands a prepared message to the gateway, intercepting /setup-files before the agent sees it
func (a *GoogleChatAdapter) dispatchMessage(ctx context.Context, msg, envelope map[string]any) error {
	event, err := a.buildMessageEvent(ctx, msg, envelope)
	if err != nil {
		pkgLog().Error("[GoogleChat] _dispatch_message failed", "error", err)
		return nil
	}
	text := strings.TrimSpace(event.Text)
	if strings.HasPrefix(text, "/setup-files") && event.Source != nil {
		thread, email := "", ""
		if event.Source.ThreadID != nil {
			thread = *event.Source.ThreadID
		}
		if event.Source.UserIDAlt != nil {
			email = *event.Source.UserIDAlt
		}
		if a.handleSetupFilesCommand(ctx, event.Source.ChatID, thread, text, email) {
			return nil
		}
	}
	if err := a.HandleMessage(ctx, event); err != nil {
		var parked *gateway.ParkedRouteRefused
		if errors.As(err, &parked) {
			return err
		}
		pkgLog().Error("[GoogleChat] _dispatch_message failed", "error", err)
	}
	return nil
}

// Message event of a Chat message with its attachments cached and the DM thread heuristic applied
func (a *GoogleChatAdapter) buildMessageEvent(ctx context.Context, msg, envelope map[string]any) (*gateway.MessageEvent, error) {
	space, _ := envelope["space"].(map[string]any)
	if len(space) == 0 {
		space, _ = msg["space"].(map[string]any)
	}
	spaceName := pyrepr.Str(orEmpty(space["name"]))
	spaceType := strings.ToUpper(pyrepr.Str(orEmpty(firstTruthy(space["type"], space["spaceType"]))))
	thread, _ := msg["thread"].(map[string]any)
	threadName := pyrepr.Str(orEmpty(thread["name"]))
	sender, _ := msg["sender"].(map[string]any)
	senderName := pyrepr.Str(orEmpty(sender["name"]))
	senderEmail := pyrepr.Str(orEmpty(sender["email"]))
	if senderEmail != "" && spaceName != "" {
		a.mu.Lock()
		a.lastSenderByChat[spaceName] = strings.ToLower(strings.TrimSpace(senderEmail))
		a.mu.Unlock()
	}
	chatType := "group"
	if spaceType == "DIRECT_MESSAGE" || spaceType == "DM" {
		chatType = "dm"
	}
	text := strings.TrimSpace(pyrepr.Str(orEmpty(firstTruthy(msg["argumentText"], msg["text"]))))
	slash, _ := msg["slashCommand"].(map[string]any)
	if len(slash) > 0 {
		if commandID := pyrepr.Str(orEmpty(slash["commandId"])); commandID != "" && !strings.HasPrefix(text, "/") {
			text = strings.TrimSpace(fmt.Sprintf("/cmd_%s %s", commandID, text))
		}
	}
	var mediaURLs, mediaTypes []string
	messageType := gateway.MessageTypeText
	attachments, _ := msg["attachment"].([]any)
	for _, raw := range attachments {
		att, _ := raw.(map[string]any)
		path, mime, err := a.downloadAttachment(ctx, att)
		if err != nil {
			pkgLog().Error("[GoogleChat] attachment download failed: "+excText(err), errAttr(err)...)
			continue
		}
		if path == "" {
			continue
		}
		if mime == "" {
			mime = "application/octet-stream"
		}
		mediaURLs, mediaTypes = append(mediaURLs, path), append(mediaTypes, mime)
		if messageType == gateway.MessageTypeText && text == "" {
			messageType = mimeForMessageType(mime)
		}
	}
	if len(slash) > 0 {
		messageType = gateway.MessageTypeCommand
	}
	prevCount := 0
	if threadName != "" && spaceName != "" {
		prevCount = a.threadCounts.incr(spaceName, threadName)
	}
	var sessionThread *string
	a.mu.Lock()
	if chatType == "dm" {
		side := prevCount > 0
		if side {
			sessionThread = &threadName
		}
		if threadName != "" && spaceName != "" && side {
			a.lastInboundThread[spaceName] = threadName
		} else if spaceName != "" {
			delete(a.lastInboundThread, spaceName)
		}
	} else {
		if threadName != "" {
			sessionThread = &threadName
		}
		if threadName != "" && spaceName != "" {
			a.lastInboundThread[spaceName] = threadName
		}
	}
	a.mu.Unlock()
	chatName := pyrepr.Str(orEmpty(firstTruthy(space["displayName"], space["name"])))
	userID := senderEmail
	if userID == "" {
		userID = senderName
	}
	userName := pyrepr.Str(orEmpty(firstTruthy(sender["displayName"], senderEmail, senderName)))
	fields := gateway.SourceFields{ChatName: &chatName, ChatType: chatType, UserID: &userID, UserName: &userName, ThreadID: sessionThread}
	if senderName != "" {
		fields.UserIDAlt = &senderName
	}
	msgName := pyrepr.Str(orEmpty(msg["name"]))
	if msgName != "" {
		fields.MessageID = &msgName
	}
	source := a.BuildSource(ctx, spaceName, fields)
	event := gateway.NewMessageEvent(text)
	event.MessageType, event.Source, event.RawMessage = messageType, source, msg
	if msgName != "" {
		event.MessageID = &msgName
	}
	event.MediaURLs, event.MediaTypes = mediaURLs, mediaTypes
	return event, nil
}

// First truthy value, nil when none is
func firstTruthy(values ...any) any {
	for _, v := range values {
		if pyrepr.Truthy(v) {
			return v
		}
	}
	return nil
}

// Downloads an inbound attachment into the cache through media download, a Google owned download URI as the last resort
func (a *GoogleChatAdapter) downloadAttachment(ctx context.Context, att map[string]any) (string, string, error) {
	mime := pyrepr.Str(orEmpty(att["contentType"]))
	name := pyrepr.Str(orEmpty(att["name"]))
	ref, _ := att["attachmentDataRef"].(map[string]any)
	resourceName := pyrepr.Str(orEmpty(ref["resourceName"]))
	downloadURI := pyrepr.Str(orEmpty(att["downloadUri"]))
	if pyrepr.Str(orEmpty(att["source"])) == "DRIVE_FILE" && resourceName == "" {
		pkgLog().Info("[GoogleChat] Skipping Drive-picker attachment (no resourceName, would need user-OAuth Drive scope)")
		return "", mime, nil
	}
	var data []byte
	if resourceName != "" {
		got, err := a.api.downloadMedia(ctx, resourceName)
		if err != nil {
			pkgLog().Warn("[GoogleChat] media.download_media failed: " + excText(err))
		} else {
			data = got
		}
	}
	if data == nil && downloadURI != "" {
		if !isGoogleOwnedHost(downloadURI) {
			pkgLog().Warn("[GoogleChat] Rejecting attachment fetch: non-Google host")
			return "", mime, nil
		}
		got, err := a.api.fetchURI(ctx, downloadURI)
		if err != nil {
			pkgLog().Warn("[GoogleChat] downloadUri fetch failed (SA tokens often lack access here; this is expected for user-uploaded content): " + excText(err, downloadURI, "https://oauth2.googleapis.com"))
			return "", mime, nil
		}
		data = got
	}
	if data == nil {
		return "", mime, nil
	}
	filename := "attachment"
	if name != "" {
		filename = name[strings.LastIndex(name, "/")+1:]
	}
	ext := ""
	if i := strings.LastIndex(filename, "."); i >= 0 {
		ext = "." + strings.ToLower(filename[i+1:])
	}
	pick := func(def string) string {
		if ext != "" {
			return ext
		}
		return def
	}
	var path string
	var err error
	switch {
	case strings.HasPrefix(mime, "image/"):
		path, err = gateway.CacheImageFromBytesAsync(ctx, data, pick(".jpg"))
	case strings.HasPrefix(mime, "audio/"):
		path, err = gateway.CacheAudioFromBytesAsync(ctx, data, pick(".ogg"))
	case strings.HasPrefix(mime, "video/"):
		path, err = gateway.CacheVideoFromBytesAsync(ctx, data, pick(".mp4"))
	default:
		path, err = gateway.CacheDocumentFromBytesAsync(ctx, data, filename)
	}
	return path, mime, err
}

// Checks the bearer of an HTTP event callback is a Google ID token from an expected service account
func (a *GoogleChatAdapter) VerifyHTTPEventRequest(ctx context.Context, authHeader string) (bool, string, error) {
	if a.httpEventsAudience == "" || a.httpEventsServiceAccount == "" {
		return false, "google_chat_http_events_not_configured", nil
	}
	token := ""
	if strings.HasPrefix(authHeader, "Bearer ") {
		token = strings.TrimSpace(authHeader[7:])
	}
	if token == "" {
		return false, "missing_google_bearer", nil
	}
	claims, err := verifyGoogleIDToken(ctx, token, a.httpEventsAudience)
	if err != nil {
		pkgLog().Warn("[GoogleChat] HTTP event bearer verification failed: " + excText(err))
		return false, "invalid_google_bearer", nil
	}
	expected := map[string]bool{}
	for _, item := range strings.Split(a.httpEventsServiceAccount, ",") {
		if item = strings.ToLower(strings.TrimSpace(item)); item != "" {
			expected[item] = true
		}
	}
	if !expected[strings.ToLower(strings.TrimSpace(pyrepr.Str(orEmpty(claims["email"]))))] {
		return false, "unexpected_google_bearer_identity", nil
	}
	return true, "", nil
}

// Dispatches one HTTP callback event, a card click answered in the reply, a message retried when its route is parked
func (a *GoogleChatAdapter) DispatchHTTPEvent(ctx context.Context, payload *pyjson.Object) (any, error) {
	envelope, ok := pyjson.Plain(payload).(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	if click, ok := parseCardClick(envelope); ok {
		return pyjson.Ordered(a.handleCardClick(ctx, click, true)), nil
	}
	msg, env, ok := a.prepareInbound(envelope, "", false)
	if !ok {
		return map[string]any{}, nil
	}
	scoped, scope, closeScope := gateway.RetryableIngress(ctx)
	err := a.dispatchMessage(scoped, msg, env)
	closeScope()
	if err != nil {
		return nil, err
	}
	if scope.Refused() {
		a.releaseUnadmitted(msg, env)
		return nil, &gateway.ParkedRouteRefused{}
	}
	return map[string]any{}, nil
}

// Undoes the dedup claim and DM thread count of a delivery that was not admitted
func (a *GoogleChatAdapter) releaseUnadmitted(msg, envelope map[string]any) {
	a.dedup.Discard(pyrepr.Str(orEmpty(msg["name"])))
	space, _ := envelope["space"].(map[string]any)
	if len(space) == 0 {
		space, _ = msg["space"].(map[string]any)
	}
	thread, _ := msg["thread"].(map[string]any)
	spaceName, threadName := pyrepr.Str(orEmpty(space["name"])), pyrepr.Str(orEmpty(thread["name"]))
	if spaceName != "" && threadName != "" {
		a.threadCounts.decr(spaceName, threadName)
	}
}

// Counts a rate limit hit for the chat
func (a *GoogleChatAdapter) noteRateLimit(chatID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rateLimitHits[chatID]++
	return a.rateLimitHits[chatID]
}

// Chat dialect of standard markdown
func (a *GoogleChatAdapter) FormatMessage(content string) string { return FormatMessage(content) }

// Text split under the size limit, preferring a newline past the midpoint
func chunkText(text string) []string {
	var chunks []string
	remaining := text
	for remaining != "" {
		if utf8.RuneCountInString(remaining) <= maxTextLength {
			chunks = append(chunks, remaining)
			break
		}
		head := textutil.First(remaining, maxTextLength)
		cut := strings.LastIndex(head, "\n")
		if cut < 0 || utf8.RuneCountInString(head[:cut]) < maxTextLength/2 {
			cut = len(head)
		}
		chunks = append(chunks, remaining[:cut])
		remaining = strings.TrimLeft(remaining[cut:], " \t\n\r\x0b\x0c")
	}
	return chunks
}

// Thread to reply under from the metadata, a thread shaped reply target or the last inbound thread unless cron
func (a *GoogleChatAdapter) resolveThreadID(replyTo string, metadata map[string]any, chatID string) string {
	for _, key := range []string{"thread_id", "thread_name", "thread_ts"} {
		if v := metadata[key]; pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
	}
	if replyTo != "" && strings.Contains(replyTo, "/threads/") && !strings.Contains(replyTo, "/messages/") {
		return replyTo
	}
	if pyrepr.Truthy(metadata["job_id"]) {
		return ""
	}
	if chatID == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastInboundThread[chatID]
}

// Counts the destination thread of an outbound message so a later reply in it reads as a side thread
func (a *GoogleChatAdapter) trackOutboundThread(chatID string, resp chatMessage) {
	thread, _ := resp["thread"].(map[string]any)
	if name := pyrepr.Str(orEmpty(thread["name"])); chatID != "" && name != "" {
		a.threadCounts.incr(chatID, name)
	}
}

// Creates a message with retry and records its thread
func (a *GoogleChatAdapter) createMessage(ctx context.Context, chatID string, body map[string]any) (gateway.SendResult, error) {
	resp, err := callWithRetry(ctx, "messages.create", func() (chatMessage, error) { return a.api.createMessage(ctx, chatID, body) })
	if err != nil {
		return gateway.SendResult{}, err
	}
	a.trackOutboundThread(chatID, resp)
	return gateway.SendResult{Success: true, MessageID: resp["name"]}, nil
}

// Updates text and cards of a message in place, the thread being immutable
func (a *GoogleChatAdapter) patchMessage(ctx context.Context, name string, body map[string]any) (gateway.SendResult, error) {
	var mask []string
	for _, k := range []string{"text", "cardsV2"} {
		if _, ok := body[k]; ok {
			mask = append(mask, k)
		}
	}
	if len(mask) == 0 {
		mask = []string{"text"}
	}
	patch := map[string]any{}
	for k, v := range body {
		if k != "thread" {
			patch[k] = v
		}
	}
	resp, err := a.api.patchMessage(ctx, name, strings.Join(mask, ","), patch)
	if err != nil {
		return gateway.SendResult{}, err
	}
	id := any(name)
	if v, ok := resp["name"]; ok {
		id = v
	}
	return gateway.SendResult{Success: true, MessageID: id}, nil
}

// Sends text in chunks, the first patching a tracked typing card and the rest created, typing paused meanwhile
func (a *GoogleChatAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	threadID := a.resolveThreadID(replyTo, metadata, chatID)
	a.PauseTypingForChat(chatID)
	defer a.ResumeTypingForChat(chatID)
	chunks := chunkText(a.FormatMessage(content))
	if len(chunks) == 0 {
		return gateway.SendResult{Success: false, Error: "empty message"}, nil
	}
	a.mu.Lock()
	typingName := a.typingMessages[chatID]
	delete(a.typingMessages, chatID)
	a.mu.Unlock()
	if typingName == typingConsumedSentinel {
		typingName = ""
	}
	var last *gateway.SendResult
	patched := false
	for idx, chunk := range chunks {
		thread := ""
		if idx > 0 || typingName == "" {
			thread = threadID
		}
		body := threadBody(chunk, thread)
		var res gateway.SendResult
		var err error
		if idx == 0 && typingName != "" {
			res, err = a.patchMessage(ctx, typingName, body)
			patched = err == nil
		} else {
			res, err = a.createMessage(ctx, chatID, body)
		}
		if err != nil {
			switch httpStatus(err) {
			case 403:
				a.SetFatalError(ctx, "chat_forbidden", "Bot lacks access (removed from space or perms revoked)", false)
				return gateway.SendResult{Success: false, Error: err.Error()}, nil
			case 404:
				if idx == 0 && typingName != "" {
					pkgLog().Info("[GoogleChat] Typing card disappeared; creating new message")
					typingName = ""
					if res, err = a.createMessage(ctx, chatID, threadBody(chunk, threadID)); err != nil {
						return gateway.SendResult{}, err
					}
					last = &res
					continue
				}
				pkgLog().Info("[GoogleChat] send target 404; skipping")
				return gateway.SendResult{Success: false, Error: "target not found"}, nil
			case 429:
				if hits := a.noteRateLimit(chatID); hits >= rateLimitWarnThreshold {
					pkgLog().Warn(fmt.Sprintf("[GoogleChat] Rate limit hit %d times on chat; throttling", hits))
				}
			}
			return gateway.SendResult{}, err
		}
		last = &res
	}
	if last == nil {
		return gateway.SendResult{Success: false, Error: "empty message"}, nil
	}
	if patched {
		a.mu.Lock()
		a.typingMessages[chatID] = typingConsumedSentinel
		a.mu.Unlock()
	}
	return *last, nil
}

// Posts a cards v2 message into the resolved thread
func (a *GoogleChatAdapter) sendCard(ctx context.Context, chatID string, card any, metadata map[string]any) gateway.SendResult {
	body := map[string]any{"cardsV2": []any{card}}
	if thread := a.resolveThreadID("", metadata, chatID); thread != "" {
		body["thread"] = map[string]any{"name": thread}
	}
	res, err := a.createMessage(ctx, chatID, body)
	if err != nil {
		retryable := isRetryableError(err)
		if status := httpStatus(err); status != 0 {
			retryable = retryableStatuses[status]
		}
		return gateway.SendResult{Success: false, Error: excText(err), Retryable: retryable}
	}
	if res.RawResponse == nil {
		res.RawResponse = map[string]any{"cardsV2": body["cardsV2"]}
	}
	return res
}

// Clarify prompt with choices as card buttons, checkboxes and a send button when several may be picked
func (a *GoogleChatAdapter) SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (gateway.SendResult, error) {
	if len(choices) == 0 {
		return a.BasePlatformAdapter.SendClarify(ctx, chatID, question, choices, clarifyID, sessionKey, metadata)
	}
	params := func(choice string) map[string]string {
		return map[string]string{clickKindParam: clickClarify, "clarify_id": clarifyID, "choice": choice}
	}
	label := func(text string) string {
		if utf8.RuneCountInString(text) > 80 {
			return textutil.First(text, 77) + "..."
		}
		return text
	}
	widgets := []any{markdownParagraph(tr(ctx, "clarify.question", map[string]any{"question": question}))}
	var buttons []any
	if entry := tools.GetPendingClarify(clarifyID); entry != nil && entry.MultiSelect {
		var items []any
		for idx, choice := range choices {
			if text := strings.TrimSpace(choice); text != "" {
				items = append(items, map[string]any{"text": label(text), "value": strconv.Itoa(idx), "selected": false})
			}
		}
		widgets = append(widgets, map[string]any{"type": "selection", "name": clarifyChoicesInput, "label": tr(ctx, "clarify.header", nil), "items": items})
		buttons = append(buttons, a.cardButton(tr(ctx, "clarify.submit_button", nil), params("")))
	} else {
		for idx, choice := range choices {
			if text := strings.TrimSpace(choice); text != "" {
				buttons = append(buttons, a.cardButton(label(text), params(strconv.Itoa(idx))))
			}
		}
	}
	buttons = append(buttons, a.cardButton(tr(ctx, "clarify.other_button", nil), params(clarifyOtherChoice)))
	card, err := promptCard("clarify-"+clarifyID, tr(ctx, "clarify.header", nil), append(widgets, buttonRow(buttons)))
	if err != nil {
		return gateway.SendResult{}, err
	}
	if res := a.sendCard(ctx, chatID, card, metadata); res.Success {
		return res, nil
	}
	return a.BasePlatformAdapter.SendClarify(ctx, chatID, question, choices, clarifyID, sessionKey, metadata)
}

// Edits a message in place, a 403 or 404 failing so the gateway sends a fresh message
func (a *GoogleChatAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	if messageID == "" {
		return gateway.SendResult{Success: false, Error: "missing message_id"}, nil
	}
	if utf8.RuneCountInString(content) > maxTextLength {
		content = textutil.First(content, maxTextLength-1) + "…"
	}
	res, err := a.patchMessage(ctx, messageID, map[string]any{"text": content})
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		if httpStatus(err) == 429 {
			a.noteRateLimit(chatID)
		}
		return gateway.SendResult{Success: false, Error: excText(err)}, nil
	}
	return res, nil
}

// Deletes a message, false on any failure with 403 and 404 kept quiet
func (a *GoogleChatAdapter) DeleteMessage(ctx context.Context, chatID, messageID string) (bool, error) {
	if messageID == "" {
		return false, nil
	}
	if err := a.api.deleteMessage(ctx, messageID); err != nil {
		if status := httpStatus(err); status != 403 && status != 404 {
			pkgLog().Debug("[GoogleChat] delete_message failed: " + excText(err))
		}
		return false, nil
	}
	return true, nil
}

// Posts the visible thinking marker that the reply later patches, one create in flight per chat
func (a *GoogleChatAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.mu.Lock()
	if _, ok := a.typingMessages[chatID]; ok {
		a.mu.Unlock()
		return nil
	}
	if inflight, ok := a.typingInflight[chatID]; ok {
		a.mu.Unlock()
		select {
		case <-inflight:
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
		return nil
	}
	done := make(chan struct{})
	a.typingInflight[chatID] = done
	a.mu.Unlock()
	thread := a.resolveThreadID("", metadata, chatID)
	text := tr(ctx, "typing.thinking", nil)
	if pyrepr.Truthy(a.Config.TypingStatusText) {
		text = pyrepr.Str(a.Config.TypingStatusText)
	}
	body := threadBody(text, thread)
	detached := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.typingInflight, chatID)
			a.mu.Unlock()
			close(done)
		}()
		res, err := a.createMessage(detached, chatID, body)
		if err != nil {
			pkgLog().Debug("[GoogleChat] send_typing background create failed", errAttr(err)...)
			return
		}
		if id := pyrepr.Str(orEmpty(res.MessageID)); res.Success && id != "" {
			a.mu.Lock()
			if _, taken := a.typingMessages[chatID]; !taken {
				a.typingMessages[chatID] = id
			} else {
				a.orphanTyping[chatID] = append(a.orphanTyping[chatID], id)
			}
			a.mu.Unlock()
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// Clears only the consumed sentinel so a live card is never deleted into a tombstone
func (a *GoogleChatAdapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.typingMessages[chatID] == typingConsumedSentinel {
		delete(a.typingMessages, chatID)
	}
	return nil
}

// Patches a quietly failing message text update
func (a *GoogleChatAdapter) patchQuietly(ctx context.Context, name, text, logMsg string) {
	if _, err := a.patchMessage(ctx, name, map[string]any{"text": text}); err != nil {
		pkgLog().Debug(logMsg, errAttr(err)...)
	}
}

// Reaps typing cards after a turn, a stranded card relabelled and orphan cards reduced to a dot
func (a *GoogleChatAdapter) OnProcessingComplete(ctx context.Context, event *gateway.MessageEvent, outcome gateway.ProcessingOutcome) error {
	if event == nil || event.Source == nil {
		return nil
	}
	chatID := event.Source.ChatID
	a.mu.Lock()
	current := a.typingMessages[chatID]
	delete(a.typingMessages, chatID)
	orphans := a.orphanTyping[chatID]
	delete(a.orphanTyping, chatID)
	a.mu.Unlock()
	if current != "" && current != typingConsumedSentinel {
		key := "typing.no_reply"
		if outcome == gateway.ProcessingCancelled {
			key = "typing.interrupted"
		}
		a.patchQuietly(ctx, current, tr(ctx, key, nil), "[GoogleChat] on_processing_complete patch fallback failed")
	}
	for _, orphan := range orphans {
		a.patchQuietly(ctx, orphan, "·", "[GoogleChat] orphan typing-card patch failed: "+orphan)
	}
	return nil
}

// Patches the tracked typing card with the text, nil result when there is no live card
func (a *GoogleChatAdapter) consumeTypingCardWithText(ctx context.Context, chatID, text string) (*gateway.SendResult, error) {
	a.mu.Lock()
	current := a.typingMessages[chatID]
	if current == "" || current == typingConsumedSentinel {
		a.mu.Unlock()
		return nil, nil
	}
	delete(a.typingMessages, chatID)
	a.mu.Unlock()
	res, err := a.patchMessage(ctx, current, map[string]any{"text": text})
	if err != nil {
		if httpStatus(err) == 404 {
			return nil, nil
		}
		return nil, err
	}
	a.mu.Lock()
	a.typingMessages[chatID] = typingConsumedSentinel
	a.mu.Unlock()
	return &res, nil
}

// Sends an inline image link, patching the typing card when one is tracked
func (a *GoogleChatAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	thread := a.resolveThreadID(replyTo, metadata, chatID)
	text := imageURL
	if caption != "" {
		text = caption + "\n" + imageURL
	}
	patched, err := a.consumeTypingCardWithText(ctx, chatID, text)
	if err == nil && patched != nil {
		return *patched, nil
	}
	if err == nil {
		var res gateway.SendResult
		if res, err = a.createMessage(ctx, chatID, threadBody(text, thread)); err == nil {
			return res, nil
		}
	}
	var h *httpError
	if errors.As(err, &h) {
		return gateway.SendResult{Success: false, Error: excText(err)}, nil
	}
	return gateway.SendResult{}, err
}

// Chat has no animation type, so the URL goes as an inline image
func (a *GoogleChatAdapter) SendAnimation(ctx context.Context, chatID, animationURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.SendImage(ctx, chatID, animationURL, caption, replyTo, metadata)
}

// Uploads an image file as a native attachment
func (a *GoogleChatAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFile(ctx, chatID, imagePath, caption, "image/*", a.resolveThreadID(replyTo, metadata, chatID), "")
}

// Uploads a document as a native attachment under its given name
func (a *GoogleChatAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFile(ctx, chatID, filePath, caption, "", a.resolveThreadID(replyTo, metadata, chatID), fileName)
}

// Uploads audio as a native attachment
func (a *GoogleChatAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFile(ctx, chatID, audioPath, caption, "audio/ogg", a.resolveThreadID(replyTo, metadata, chatID), "")
}

// Uploads video as a native attachment
func (a *GoogleChatAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFile(ctx, chatID, videoPath, caption, "video/mp4", a.resolveThreadID(replyTo, metadata, chatID), "")
}

// User authed client of an email, the cached one refreshed or one loaded from its token file
func (a *GoogleChatAdapter) loadPerUserChatAPI(ctx context.Context, email string) *restClient {
	a.mu.Lock()
	cachedAPI, cachedCreds := a.userAPIByEmail[email], a.userCredsByEmail[email]
	a.mu.Unlock()
	if cachedAPI != nil && cachedCreds != nil {
		refreshed := refreshOrNone(ctx, cachedCreds)
		a.mu.Lock()
		defer a.mu.Unlock()
		if refreshed == nil {
			delete(a.userCredsByEmail, email)
			delete(a.userAPIByEmail, email)
			return nil
		}
		a.userCredsByEmail[email] = refreshed
		return cachedAPI
	}
	creds := loadUserCredentials(ctx, email)
	if creds == nil {
		return nil
	}
	api := newRESTClient(creds.creds)
	a.mu.Lock()
	a.userCredsByEmail[email], a.userAPIByEmail[email] = creds, api
	a.mu.Unlock()
	return api
}

// User authed client for an attachment, sender token first then the shared one, with the identity to evict
func (a *GoogleChatAdapter) acquireUserChatAPI(ctx context.Context, senderEmail string) (*restClient, string) {
	if senderEmail != "" {
		if api := a.loadPerUserChatAPI(ctx, senderEmail); api != nil {
			return api, senderEmail
		}
	}
	a.mu.Lock()
	api, creds := a.userAPI, a.userCreds
	a.mu.Unlock()
	if api == nil {
		return nil, ""
	}
	refreshed := refreshOrNone(ctx, creds)
	if refreshed == nil {
		pkgLog().Warn("[GoogleChat] legacy user-OAuth refresh returned None — evicting fallback creds")
		a.invalidateUserCreds(legacyUserIdentity)
		return nil, ""
	}
	a.mu.Lock()
	a.userCreds = refreshed
	a.mu.Unlock()
	return api, legacyUserIdentity
}

// Drops the credentials of an identity after an auth failure
func (a *GoogleChatAdapter) invalidateUserCreds(identity string) {
	if identity == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if identity == legacyUserIdentity {
		a.userCreds, a.userAPI = nil, nil
		return
	}
	delete(a.userCredsByEmail, identity)
	delete(a.userAPIByEmail, identity)
}

// Native attachment uploaded and posted as the latest sender, the setup notice when no user token is usable
func (a *GoogleChatAdapter) sendFile(ctx context.Context, chatID, path, caption, mimeHint, threadID, overrideFilename string) (gateway.SendResult, error) {
	if _, err := os.Stat(path); err != nil {
		return gateway.SendResult{Success: false, Error: "file not found: " + path}, nil
	}
	filename := overrideFilename
	if filename == "" {
		filename = filepath.Base(path)
	}
	if filename == "" || filename == "." || filename == "/" {
		filename = "upload.bin"
	}
	mime := mimeHint
	if mime == "" {
		mime = "application/octet-stream"
	}
	a.mu.Lock()
	sender := a.lastSenderByChat[chatID]
	a.mu.Unlock()
	api, identity := a.acquireUserChatAPI(ctx, sender)
	if api == nil {
		return a.postAttachmentFallback(ctx, chatID, path, filename, caption, threadID), nil
	}
	cardText := caption
	if cardText == "" {
		cardText = " "
	}
	if _, err := a.consumeTypingCardWithText(ctx, chatID, cardText); err != nil {
		pkgLog().Debug("[GoogleChat] _send_file pre-patch typing-card failed", errAttr(err)...)
	}
	upload, err := api.uploadMedia(ctx, chatID, filename, path, mime)
	if err != nil {
		if status := httpStatus(err); status == 401 || status == 403 {
			pkgLog().Warn(fmt.Sprintf("[GoogleChat] media.upload auth failure for identity=%s (token revoked or scope missing) — falling back to text notice. Status=%d", identity, status))
			a.invalidateUserCreds(identity)
			return a.postAttachmentFallback(ctx, chatID, path, filename, caption, threadID), nil
		}
		return gateway.SendResult{Success: false, Error: excText(err)}, nil
	}
	ref, ok := upload["attachmentDataRef"]
	if !ok || !pyrepr.Truthy(ref) {
		return gateway.SendResult{Success: false, Error: "upload returned no attachmentDataRef"}, nil
	}
	body := map[string]any{"attachment": []any{map[string]any{"attachmentDataRef": ref}}}
	if caption != "" {
		body["text"] = caption
	}
	if threadID != "" {
		body["thread"] = map[string]any{"name": threadID}
	}
	resp, err := api.createMessage(ctx, chatID, body)
	if err != nil {
		return gateway.SendResult{Success: false, Error: excText(err)}, nil
	}
	a.trackOutboundThread(chatID, resp)
	return gateway.SendResult{Success: true, MessageID: resp["name"]}, nil
}

// Posts the /setup-files notice with the host path when native delivery is unavailable, always a failure result
func (a *GoogleChatAdapter) postAttachmentFallback(ctx context.Context, chatID, path, filename, caption, threadID string) gateway.SendResult {
	notice := strings.Join([]string{
		tr(ctx, "attachment_fallback.header", map[string]any{"filename": filename}),
		tr(ctx, "attachment_fallback.explain", nil),
		tr(ctx, "attachment_fallback.activate", nil),
		tr(ctx, "attachment_fallback.host_path", map[string]any{"path": path}),
	}, "\n")
	visible := notice
	if caption != "" {
		visible = caption + "\n" + notice
	}
	body := a.WarningText(ctx, visible, &caption, a.Platform, chatID, nil)
	if body != nil && *body != "" {
		if _, err := a.createMessage(ctx, chatID, threadBody(*body, threadID)); err != nil {
			pkgLog().Debug("[GoogleChat] attachment fallback notice send failed", errAttr(err)...)
		}
	}
	return gateway.SendResult{Success: false, Error: "google_chat: native attachment requires user OAuth — run /setup-files in chat"}
}

// Name and type of a space
func (a *GoogleChatAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	info, err := a.api.getSpace(ctx, chatID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pkgLog().Debug("[GoogleChat] get_chat_info failed: " + excText(err))
		return map[string]any{"name": chatID, "type": "group", "chat_id": chatID}, nil
	}
	spaceType := strings.ToUpper(pyrepr.Str(orEmpty(firstTruthy(info["spaceType"], info["type"]))))
	chatType := "group"
	if spaceType == "DIRECT_MESSAGE" || spaceType == "DM" {
		chatType = "dm"
	}
	name := pyrepr.Str(orEmpty(info["displayName"]))
	if name == "" {
		name = chatID
	}
	return map[string]any{"name": name, "type": chatType, "chat_id": chatID}, nil
}
