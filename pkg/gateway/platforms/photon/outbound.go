package photon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// POSTs a JSON body to a sidecar path, the decoded answer when it reports ok
func (a *PhotonAdapter) sidecarCall(ctx context.Context, path string, body map[string]any) (map[string]any, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, sidecarCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, a.sidecarURL(path), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(sidecarTokenHeader, a.sidecarToken)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, sidecarErrorFromResponse(path, resp.StatusCode, string(raw), nil)
	}
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	if data == nil {
		data = map[string]any{}
	}
	if !pyrepr.Truthy(data["ok"]) {
		return nil, sidecarErrorFromResponse(path, resp.StatusCode, string(raw), data)
	}
	return data, nil
}

// Whether a failure text reads as a transient upstream overload, never when marked not retryable or auth_or_config
func photonRetryable(errText string) bool {
	lowered := strings.ToLower(errText)
	if lowered == "" || strings.Contains(lowered, "retryable=false") || strings.Contains(lowered, "auth_or_config") {
		return false
	}
	for _, p := range photonRetryablePatterns {
		if strings.Contains(lowered, p) {
			return true
		}
	}
	return false
}

// Posts a send like body and wraps the outcome, recording the id and the text a later reply quotes
func (a *PhotonAdapter) postSend(ctx context.Context, path string, body map[string]any, structured bool, sentText string) gateway.SendResult {
	data, err := a.sidecarCall(ctx, path, body)
	if err != nil {
		var se *sidecarError
		if errors.As(err, &se) && structured {
			return gateway.SendResult{Success: false, Error: se.Error(), Retryable: se.retryable || photonRetryable(se.Error()),
				RawResponse: map[string]any{"error_class": se.errorClass, "retryable": se.retryable}}
		}
		return gateway.SendResult{Success: false, Error: err.Error(), Retryable: photonRetryable(err.Error())}
	}
	messageID := pyrepr.Str(orEmpty(data["messageId"]))
	if messageID != "" {
		a.mu.Lock()
		a.sentMessageIDs.put(messageID, time.Now())
		a.mu.Unlock()
	}
	if sentText != "" {
		gateway.RichSentRecord(ctx, normalizeChatKey(pyrepr.Str(orEmpty(body["spaceId"]))), messageID, sentText)
	}
	var id any
	if messageID != "" {
		id = messageID
	}
	return gateway.SendResult{Success: true, MessageID: id}
}

// Sends text, a URL only message as a rich link first, markdown when enabled and safe
func (a *PhotonAdapter) sidecarSend(ctx context.Context, spaceID, text string, richlink, markdown bool) gateway.SendResult {
	sentText := text
	if richlink {
		if richURL := richlinkCandidate(ctx, text); richURL != "" {
			res := a.postSend(ctx, "/send-richlink", map[string]any{"spaceId": spaceID, "url": richURL}, false, sentText)
			if res.Success {
				return res
			}
			pkgLog().Warn(fmt.Sprintf("[photon] rich-link send failed, falling back to plain text: %v", res.Error))
			markdown = false
		}
	}
	sendMarkdown := markdown && markdownEnabled(ctx)
	text = sidecarPayloadText(text, sendMarkdown)
	if n := utf8.RuneCountInString(text); n > maxMessageLength {
		pkgLog().Warn(fmt.Sprintf("[photon] truncating outbound from %d to %d chars", n, maxMessageLength))
		text = textutil.First(text, maxMessageLength)
	}
	body := map[string]any{"spaceId": spaceID, "text": text}
	if sendMarkdown {
		body["format"] = "markdown"
	}
	return a.postSend(ctx, "/send", body, true, sentText)
}

// Native poll, at least two non blank options under a title
func (a *PhotonAdapter) sidecarSendPoll(ctx context.Context, spaceID, title string, options []string) gateway.SendResult {
	var opts []any
	for _, o := range options {
		if o = strings.TrimSpace(o); o != "" {
			opts = append(opts, o)
		}
	}
	if strings.TrimSpace(title) == "" {
		return gateway.SendResult{Success: false, Error: "poll title is required"}
	}
	if len(opts) < 2 {
		return gateway.SendResult{Success: false, Error: "poll needs at least two options"}
	}
	shown := textutil.First(strings.TrimSpace(title), maxMessageLength)
	return a.postSend(ctx, "/send-poll", map[string]any{"spaceId": spaceID, "title": shown, "options": opts}, false, shown)
}

// Local file sent through /send-attachment, voice kind as a voice note, refused when the path is unsafe
func (a *PhotonAdapter) sidecarSendAttachment(ctx context.Context, spaceID, path, name, mimeType, caption, kind string) (gateway.SendResult, error) {
	safe, err := gateway.ValidateMediaDeliveryPath(ctx, path, "")
	if err != nil {
		return gateway.SendResult{}, err
	}
	if safe == "" {
		return gateway.SendResult{Success: false, Error: "unsafe or missing attachment path: " + path}, nil
	}
	if mimeType == "" {
		mimeType = guessMime(safe)
	}
	label := caption
	if label == "" {
		shownName := name
		if shownName == "" {
			shownName = filepath.Base(safe)
		}
		label = attachmentLabel(kind, shownName)
	}
	return a.postSend(ctx, "/send-attachment", attachmentBody(spaceID, safe, kind, name, mimeType, caption), true, label), nil
}

// Markdown verbatim for the sidecar builder, stripped to plain text when PHOTON_MARKDOWN is off
func (a *PhotonAdapter) FormatMessage(content string) string {
	if markdownEnabled(context.Background()) {
		return content
	}
	return gateway.StripMarkdown(content, true)
}

// Sends one formatted message
func (a *PhotonAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.sidecarSend(ctx, chatID, a.FormatMessage(content), true, true), nil
}

// Multiple choice clarify as a native poll whose vote comes back as typed text, the base text prompt otherwise
func (a *PhotonAdapter) SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (gateway.SendResult, error) {
	if len(choices) == 0 {
		return a.BasePlatformAdapter.SendClarify(ctx, chatID, question, choices, clarifyID, sessionKey, metadata)
	}
	tools.MarkClarifyAwaitingText(clarifyID)
	res := a.sidecarSendPoll(ctx, chatID, question, choices)
	if !res.Success {
		pkgLog().Warn(fmt.Sprintf("[photon] poll clarify failed (%v); falling back to text list", res.Error))
		return a.BasePlatformAdapter.SendClarify(ctx, chatID, question, choices, clarifyID, sessionKey, metadata)
	}
	return res, nil
}

// Caches an image URL and sends it as an attachment, the URL as text when it cannot be fetched
func (a *PhotonAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	local, err := gateway.CacheImageFromURL(ctx, imageURL, ".jpg", 2)
	if err != nil {
		return a.BasePlatformAdapter.SendImage(ctx, chatID, imageURL, caption, replyTo, nil)
	}
	return a.sidecarSendAttachment(ctx, chatID, local, "", "", caption, "attachment")
}

// Sends a local image as an attachment
func (a *PhotonAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sidecarSendAttachment(ctx, chatID, imagePath, "", "", caption, "attachment")
}

// Sends local audio as a voice note
func (a *PhotonAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sidecarSendAttachment(ctx, chatID, audioPath, "", "", caption, "voice")
}

// Sends a local video as an attachment
func (a *PhotonAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sidecarSendAttachment(ctx, chatID, videoPath, "", "", caption, "attachment")
}

// Sends a local document as an attachment under its given name
func (a *PhotonAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sidecarSendAttachment(ctx, chatID, filePath, fileName, "", caption, "attachment")
}

// Soft failing sidecar call, true on success
func (a *PhotonAdapter) sidecarTry(ctx context.Context, path string, body map[string]any, what string) bool {
	if _, err := a.sidecarCall(ctx, path, body); err != nil {
		pkgLog().Debug(fmt.Sprintf("[photon] %s failed: %v", what, err))
		return false
	}
	return true
}

// Starts the typing indicator at most once per cooldown per chat
func (a *PhotonAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.mu.Lock()
	if time.Since(a.typingLastSent[chatID]) < typingCooldown {
		a.mu.Unlock()
		return nil
	}
	a.typingLastSent[chatID] = time.Now()
	a.mu.Unlock()
	a.sidecarTry(ctx, "/typing", map[string]any{"spaceId": chatID, "state": "start"}, "send_typing")
	return nil
}

// Stops the typing indicator and resets the cooldown
func (a *PhotonAdapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.mu.Lock()
	delete(a.typingLastSent, chatID)
	a.mu.Unlock()
	a.sidecarTry(ctx, "/typing", map[string]any{"spaceId": chatID, "state": "stop"}, "stop_typing")
	return nil
}

// Whether the lifecycle reactions are on, opt in through PHOTON_REACTIONS
func (a *PhotonAdapter) ReactionsEnabled(ctx context.Context) bool {
	v, _ := gateway.GetScopedSecret(ctx, "PHOTON_REACTIONS", "false", false)
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// Reaction target, the given message or the chat's latest inbound one
func (a *PhotonAdapter) reactionTarget(chatID, messageID string) string {
	if messageID != "" {
		return messageID
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastInbound.vals[normalizeChatKey(chatID)]
}

// Tapback onto a message, the chat's latest inbound one when no id is given
func (a *PhotonAdapter) AddReaction(ctx context.Context, chatID, messageID, emoji string) error {
	target := a.reactionTarget(chatID, messageID)
	if target == "" {
		return errors.New("no message to react to — pass message_id (no inbound message seen in this chat since the gateway started)")
	}
	if !a.sidecarTry(ctx, "/react", map[string]any{"spaceId": chatID, "messageId": target, "emoji": emoji}, "add_reaction") {
		return errors.New("reaction failed (see gateway debug log)")
	}
	return nil
}

// Retracts this adapter's tapback from a message, the chat's latest inbound one when no id is given
func (a *PhotonAdapter) RemoveReaction(ctx context.Context, chatID, messageID string) error {
	target := a.reactionTarget(chatID, messageID)
	if target == "" {
		return errors.New("no message to unreact — pass message_id")
	}
	if !a.sidecarTry(ctx, "/unreact", map[string]any{"spaceId": chatID, "messageId": target}, "remove_reaction") {
		return errors.New("unreact failed (see gateway debug log)")
	}
	return nil
}

// Tapbacks eyes on the triggering message while the agent works, when lifecycle reactions are on
func (a *PhotonAdapter) OnProcessingStart(ctx context.Context, event *gateway.MessageEvent) error {
	if !a.ReactionsEnabled(ctx) || event == nil || event.Source == nil || event.MessageID == nil || *event.MessageID == "" {
		return nil
	}
	a.sidecarTry(ctx, "/react", map[string]any{"spaceId": event.Source.ChatID, "messageId": *event.MessageID, "emoji": "\U0001f440"}, "add_reaction")
	return nil
}

// Photon space ids are opaque, so a chat reads as a DM
func (a *PhotonAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": chatID, "type": "dm", "id": chatID}, nil
}

// Whether a failed send is an auth, config or target refusal that retrying would only double send
func (a *PhotonAdapter) SendRetryIsFinal(result gateway.SendResult) bool {
	raw, ok := result.RawResponse.(map[string]any)
	if !ok || raw["retryable"] != false {
		return false
	}
	class := raw["error_class"]
	return class == "auth_or_config" || class == "target_not_allowed"
}

// Plain text resend without the rich link path so a rich link outage never strands a sendable URL
func (a *PhotonAdapter) SendPlainFallback(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.sidecarSend(ctx, chatID, textutil.First(a.FormatMessage(content), maxMessageLength), false, false), nil
}

// Send error of a failed standalone call with the sidecar's class and retryability
func standaloneError(status int, raw string) map[string]any {
	var data map[string]any
	_ = json.Unmarshal([]byte(raw), &data)
	errorClass := "sidecar_error"
	if pyrepr.Truthy(data["error_class"]) {
		errorClass = pyrepr.Str(data["error_class"])
	}
	retryable := pyrepr.Truthy(data["retryable"])
	var text string
	switch {
	case errorClass == "target_not_allowed":
		text, retryable = targetNotAllowedMessage, false
	case status != http.StatusOK:
		text = fmt.Sprintf("sidecar returned %d: %s", status, textutil.First(raw, 200))
	default:
		text = pyrepr.Str(orEmpty(data["error"]))
		if text == "" {
			text = "sidecar reported failure"
		}
	}
	out := gateway.SendError(text)
	out["error_class"], out["retryable"] = errorClass, retryable
	return out
}

// Token and port of the live sidecar from its runtime record, or why none is usable
func standaloneTokenFromRecord(ctx context.Context, port int) (string, int, string) {
	record := readRuntimeRecord(ctx)
	staleHint := ""
	if record != nil && pyrepr.Truthy(record["token"]) {
		if sidecarPIDAlive(record["pid"]) {
			return pyrepr.Str(record["token"]), gateway.CoercePort(record["port"], port), ""
		}
		staleHint = fmt.Sprintf(" A stale sidecar runtime record was found (pid %v is not running) — the gateway appears to be down.", record["pid"])
	}
	return "", port, "Photon standalone send requires a running sidecar. Start the Icebath gateway (which spawns the sidecar and " +
		"records its address in <icebath-home>/" + runtimeRecordName + "), or set PHOTON_SIDECAR_TOKEN in this process's environment." + staleHint
}

// Delivers text then each attachment through a live sidecar for cron when the gateway is not in this process
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	var portRaw any
	if pconfig != nil {
		portRaw = pconfig.Extra["sidecar_port"]
	}
	if !pyrepr.Truthy(portRaw) {
		portRaw, _ = gateway.GetScopedSecret(ctx, "PHOTON_SIDECAR_PORT", "", false)
	}
	port := gateway.CoercePort(portRaw, defaultSidecarPort)
	token, _ := gateway.GetScopedSecret(ctx, "PHOTON_SIDECAR_TOKEN", "", false)
	if token == "" {
		var reason string
		if token, port, reason = standaloneTokenFromRecord(ctx, port); token == "" {
			return gateway.SendError(reason), nil
		}
	}
	client := &http.Client{Transport: &http.Transport{DialContext: netpref.DialContext((&net.Dialer{}).DialContext), Proxy: nil}, Timeout: sidecarCallTimeout}
	defer client.CloseIdleConnections()
	base := fmt.Sprintf("http://%s:%d", defaultSidecarBind, port)
	post := func(path string, body map[string]any) (int, string, map[string]any, error) {
		payload, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(payload))
		if err != nil {
			return 0, "", nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(sidecarTokenHeader, token)
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return 0, "", nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, string(raw), nil, nil
		}
		var data map[string]any
		_ = json.Unmarshal(raw, &data)
		if !pyrepr.Truthy(data["ok"]) {
			return resp.StatusCode, string(raw), nil, nil
		}
		return resp.StatusCode, string(raw), data, nil
	}
	failed := func(err error) (map[string]any, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.SendError("Photon standalone send failed: " + err.Error()), nil
	}
	var lastMessageID any
	if message != "" {
		var data map[string]any
		richURL := richlinkCandidate(ctx, message)
		if richURL != "" {
			_, _, d, err := post("/send-richlink", map[string]any{"spaceId": chatID, "url": richURL})
			if err != nil {
				return failed(err)
			}
			data = d
		}
		if data == nil {
			sendMarkdown := markdownEnabled(ctx) && richURL == ""
			body := map[string]any{"spaceId": chatID, "text": textutil.First(sidecarPayloadText(message, sendMarkdown), maxMessageLength)}
			if sendMarkdown {
				body["format"] = "markdown"
			}
			status, raw, d, err := post("/send", body)
			if err != nil {
				return failed(err)
			}
			if d == nil {
				return standaloneError(status, raw), nil
			}
			data = d
		}
		lastMessageID = data["messageId"]
		gateway.RichSentRecord(ctx, normalizeChatKey(chatID), pyrepr.Str(orEmpty(lastMessageID)), message)
	}
	for _, media := range standaloneMedia(kwargs["media_files"]) {
		safe, err := gateway.ValidateMediaDeliveryPath(ctx, media.path, "")
		if err != nil || safe == "" {
			pkgLog().Warn("[photon] standalone send skipping unsafe path")
			continue
		}
		kind := "attachment"
		if media.voice {
			kind = "voice"
		}
		status, raw, data, err := post("/send-attachment", attachmentBody(chatID, safe, kind, "", guessMime(safe), ""))
		if err != nil {
			return failed(err)
		}
		if data == nil {
			return standaloneError(status, raw), nil
		}
		if pyrepr.Truthy(data["messageId"]) {
			lastMessageID = data["messageId"]
		}
		gateway.RichSentRecord(ctx, normalizeChatKey(chatID), pyrepr.Str(orEmpty(data["messageId"])), attachmentLabel(kind, filepath.Base(safe)))
	}
	return map[string]any{"success": true, "message_id": lastMessageID}, nil
}

// One attachment of a standalone send
type standaloneFile struct {
	path  string
	voice bool
}

// Attachments of the media_files kwarg in either of its shapes
func standaloneMedia(raw any) []standaloneFile {
	var out []standaloneFile
	switch v := raw.(type) {
	case []gateway.MediaRef:
		for _, m := range v {
			out = append(out, standaloneFile{m.Path, m.IsVoice})
		}
	case []any:
		for _, item := range v {
			if pair, ok := item.([]any); ok && len(pair) == 2 {
				out = append(out, standaloneFile{pyrepr.Str(pair[0]), pyrepr.Truthy(pair[1])})
			}
		}
	}
	return out
}
