package qqbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

var permanentSendErrors = []string{"invalid", "forbidden", "not found"}

// Whether the text contains any of the needles
func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// One authenticated REST call, a RuntimeError for an HTTP error status or a timeout
func (a *QQAdapter) apiRequest(ctx context.Context, method, path string, body *pyjson.Object, timeout float64) (any, error) {
	client, err := a.requireHTTPClient()
	if err != nil {
		return nil, err
	}
	headers, err := a.authHeaders(ctx)
	if err != nil {
		return nil, err
	}
	var payload []byte
	if body != nil {
		if payload, err = jsonBody(body); err != nil {
			return nil, err
		}
	}
	resp, err := httpRequest(ctx, client, method, APIBase+path, headers, payload, timeout)
	if err != nil {
		var pe *pyErr
		if errors.As(err, &pe) && pe.kind == "ReadTimeout" {
			return nil, runtimeError(fmt.Sprintf("QQ Bot API timeout [%s]: %s", path, pe.msg))
		}
		return nil, err
	}
	data, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		dict, ok := asDict(data)
		if !ok {
			return nil, notDictErr(data)
		}
		return nil, runtimeError(fmt.Sprintf("QQ Bot API error [%d] %s: %s", resp.StatusCode, path, pyrepr.Str(getOr(dict, "message", data))))
	}
	return data, nil
}

// JSON REST headers carrying a fresh bot token
func (a *QQAdapter) authHeaders(ctx context.Context) (map[string]string, error) {
	token, err := a.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "QQBot " + token, "Content-Type": "application/json", "User-Agent": BuildUserAgent()}, nil
}

// Registers media by URL or base64 data with up to three attempts, permanent failures raised at once
func (a *QQAdapter) uploadMedia(ctx context.Context, targetType, targetID string, fileType int, url, fileData string, srvSendMsg bool, fileName string) (any, error) {
	body := obj("file_type", int64(fileType), "srv_send_msg", srvSendMsg)
	if url != "" {
		body.Set("url", url)
	} else if fileData != "" {
		body.Set("file_data", fileData)
	}
	if fileType == MediaTypeFile && fileName != "" {
		body.Set("file_name", fileName)
	}
	path := restPath(targetType, targetID, "files")
	for attempt := 0; ; attempt++ {
		data, err := a.apiRequest(ctx, http.MethodPost, path, body, FileUploadTimeout)
		if err == nil {
			return data, nil
		}
		if !isRuntimeError(err) || attempt == 2 || containsAny(errText(err), "400", "401", "Invalid", "timeout", "Timeout") {
			return nil, err
		}
		if serr := sleep(ctx, 1.5*float64(attempt+1)); serr != nil {
			return nil, serr
		}
	}
}

// Polls the transport for a reconnection the listener is making, false when it does not come back in time
func (a *QQAdapter) waitForReconnection(ctx context.Context) (bool, error) {
	slog.Info(fmt.Sprintf("[%s] Not connected — waiting for reconnection (up to %.0fs)", a.logTag(), reconnectWaitSeconds))
	for waited := 0.0; waited < reconnectWaitSeconds; {
		if err := sleep(ctx, reconnectPollInterval); err != nil {
			return false, err
		}
		waited += reconnectPollInterval
		if a.IsConnected() {
			slog.Info(fmt.Sprintf("[%s] Reconnected after %.1fs", a.logTag(), waited))
			return true, nil
		}
	}
	slog.Warn(fmt.Sprintf("[%s] Still not connected after %.0fs", a.logTag(), reconnectWaitSeconds))
	return false, nil
}

// Retryable failure of a send attempted while the transport is down
func notConnected() gateway.SendResult {
	return gateway.SendResult{Success: false, Error: "Not connected", Retryable: true}
}

// Whether the transport is connected now or after waiting for the listener to reconnect
func (a *QQAdapter) ensureConnected(ctx context.Context) (bool, error) {
	if a.IsConnected() {
		return true, nil
	}
	return a.waitForReconnection(ctx)
}

// Sends text or markdown split at the length cap, only the first chunk replying to the given message
func (a *QQAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	connected, err := a.ensureConnected(ctx)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if !connected {
		return notConnected(), nil
	}
	if pystr.Strip(content) == "" {
		return gateway.SendResult{Success: true}, nil
	}
	lastResult := gateway.SendResult{Success: false, Error: "No chunks"}
	for _, chunk := range gateway.TruncateMessage(a.FormatMessage(content), MaxMessageLength, nil) {
		if lastResult, err = a.sendChunk(ctx, chatID, chunk, replyTo); err != nil || !lastResult.Success {
			return lastResult, err
		}
		replyTo = ""
	}
	return lastResult, nil
}

// Sender of one text body to a chat
type textSender func(ctx context.Context, targetID, content, replyTo string, keyboard *InlineKeyboard) (gateway.SendResult, error)

// Sends one chunk with up to three attempts, permanent failures not retried
func (a *QQAdapter) sendChunk(ctx context.Context, chatID, content, replyTo string) (gateway.SendResult, error) {
	sender := a.textSenderFor(a.guessChatType(chatID), false)
	if sender == nil {
		return gateway.SendResult{Success: false, Error: "Unknown chat type for " + chatID}, nil
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		result, err := sender(ctx, chatID, content, replyTo, nil)
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		lastErr = err
		if containsAny(pystr.Lower(errText(err)), append(append([]string{}, permanentSendErrors...), "bad request")...) {
			break
		}
		if attempt < 2 {
			delay := 1.0 * math.Pow(2, float64(attempt))
			slog.Warn(fmt.Sprintf("[%s] send retry %d/3 after %.1fs: %s", a.logTag(), attempt+1, delay, errText(err)))
			if serr := sleep(ctx, delay); serr != nil {
				return gateway.SendResult{}, serr
			}
		}
	}
	errorMsg := errTextOrName(lastErr)
	slog.Error(fmt.Sprintf("[%s] Send failed: %s", a.logTag(), errorMsg))
	return gateway.SendResult{Success: false, Error: errorMsg, Retryable: !containsAny(pystr.Lower(errorMsg), permanentSendErrors...)}, nil
}

// REST path of an endpoint under a c2c user or a group
func restPath(chatType, targetID, endpoint string) string {
	kind := "groups"
	if chatType == "c2c" {
		kind = "users"
	}
	return "/v2/" + kind + "/" + targetID + "/" + endpoint
}

func messagesPath(chatType, targetID string) string { return restPath(chatType, targetID, "messages") }

// Posts a message body and wraps the response as a successful send result
func (a *QQAdapter) postMessage(ctx context.Context, path string, body *pyjson.Object) (gateway.SendResult, error) {
	data, err := a.apiRequest(ctx, http.MethodPost, path, body, DefaultAPITimeout)
	if err != nil {
		return gateway.SendResult{}, err
	}
	dict, ok := asDict(data)
	if !ok {
		return gateway.SendResult{}, notDictErr(data)
	}
	return gateway.SendResult{Success: true, MessageID: pyrepr.Str(getOr(dict, "id", uuidHex()[:12])), RawResponse: data}, nil
}

// Sends text, optionally with an inline keyboard, to a c2c user or a group
func (a *QQAdapter) sendTextTo(ctx context.Context, chatType, targetID, content, replyTo string, keyboard *InlineKeyboard) (gateway.SendResult, error) {
	body := a.buildTextBody(content, replyTo)
	if replyTo != "" {
		body.Set("msg_id", replyTo)
	}
	if keyboard != nil {
		body.Set("keyboard", keyboard.ToDict())
	}
	return a.postMessage(ctx, messagesPath(chatType, targetID), body)
}

func (a *QQAdapter) sendC2CText(ctx context.Context, openID, content, replyTo string, keyboard *InlineKeyboard) (gateway.SendResult, error) {
	return a.sendTextTo(ctx, "c2c", openID, content, replyTo, keyboard)
}

func (a *QQAdapter) sendGroupText(ctx context.Context, groupOpenID, content, replyTo string, keyboard *InlineKeyboard) (gateway.SendResult, error) {
	return a.sendTextTo(ctx, "group", groupOpenID, content, replyTo, keyboard)
}

// Text sender of a chat type, nil for an unknown type and for guild when a keyboard is wanted
func (a *QQAdapter) textSenderFor(chatType string, keyboardOK bool) textSender {
	switch {
	case keyboardOK && chatType == "guild":
		return nil
	case chatType == "c2c":
		return a.sendC2CText
	case chatType == "group":
		return a.sendGroupText
	case chatType == "guild":
		return func(ctx context.Context, channelID, content, replyTo string, _ *InlineKeyboard) (gateway.SendResult, error) {
			return a.sendGuildText(ctx, channelID, content, replyTo)
		}
	}
	return nil
}

// Sends plain content to a guild channel
func (a *QQAdapter) sendGuildText(ctx context.Context, channelID, content, replyTo string) (gateway.SendResult, error) {
	body := obj("content", pystr.Slice(content, 0, MaxMessageLength))
	if replyTo != "" {
		body.Set("msg_id", replyTo)
	}
	return a.postMessage(ctx, "/channels/"+channelID+"/messages", body)
}

// Sends one unsplit text message with an inline keyboard, guild chats being refused
func (a *QQAdapter) SendWithKeyboard(ctx context.Context, chatID, content string, keyboard InlineKeyboard, replyTo string) (gateway.SendResult, error) {
	connected, err := a.ensureConnected(ctx)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if !connected {
		return notConnected(), nil
	}
	chatType := a.guessChatType(chatID)
	sender := a.textSenderFor(chatType, true)
	if sender == nil {
		return gateway.SendResult{Success: false, Error: "Inline keyboards not supported for chat_type " + pyrepr.Repr(chatType), Retryable: false}, nil
	}
	truncated := pystr.Slice(a.FormatMessage(content), 0, MaxMessageLength)
	result, err := sender(ctx, chatID, truncated, replyTo, &keyboard)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		slog.Error(fmt.Sprintf("[%s] send_with_keyboard failed: %s", a.logTag(), errText(err)))
		return gateway.SendResult{Success: false, Error: errTextOrName(err)}, nil
	}
	return result, nil
}

// Sends an approval request with its allow once, allow always and deny buttons
func (a *QQAdapter) SendApprovalRequest(ctx context.Context, chatID string, req ApprovalRequest, replyTo string) (gateway.SendResult, error) {
	return a.SendWithKeyboard(ctx, chatID, BuildApprovalText(req), BuildApprovalKeyboard(req.SessionKey, req.AllowPermanent), replyTo)
}

// Keyboard card approval sent while the agent blocks, replying to the last inbound message of the chat
func (a *QQAdapter) SendExecApprovalPrompt(ctx context.Context, prompt *gateway.ExecApprovalPrompt) (gateway.SendResult, error) {
	description := prompt.Description
	if prompt.SmartDenied {
		description += " Owner override applies to this one operation only."
	}
	req := NewApprovalRequest(prompt.SessionKey, "Execute this command?")
	req.Description, req.CommandPreview, req.TimeoutSec, req.AllowPermanent = description, prompt.Command, approvalTimeoutSeconds, false
	for _, choice := range prompt.Choices() {
		if choice == "always" {
			req.AllowPermanent = true
		}
	}
	a.mu.Lock()
	replyTo := a.lastMsgID[prompt.ChatID]
	a.mu.Unlock()
	return a.SendApprovalRequest(ctx, prompt.ChatID, req, replyTo)
}

// Yes or no update confirmation whose button clicks the interaction callback writes to the response file
func (a *QQAdapter) SendUpdatePrompt(ctx context.Context, chatID, prompt, def, sessionKey string, metadata map[string]any) (gateway.SendResult, error) {
	defaultHint := ""
	if def != "" {
		defaultHint = " (default: " + def + ")"
	}
	a.mu.Lock()
	replyTo := a.lastMsgID[chatID]
	a.mu.Unlock()
	return a.SendWithKeyboard(ctx, chatID, "☤ **Update Needs Your Input**\n\n"+prompt+defaultHint, BuildUpdatePromptKeyboard(), replyTo)
}

// Text message body, markdown when supported and plain content with a message reference otherwise
func (a *QQAdapter) buildTextBody(content, replyTo string) *pyjson.Object {
	msgSeq := nextMsgSeq()
	text := pystr.Slice(content, 0, MaxMessageLength)
	if a.markdownSupport {
		return obj("markdown", obj("content", text), "msg_type", int64(MsgTypeMarkdown), "msg_seq", msgSeq)
	}
	body := obj("content", text, "msg_type", int64(MsgTypeText), "msg_seq", msgSeq)
	if replyTo != "" {
		body.Set("message_reference", obj("message_id", replyTo))
	}
	return body
}

// Sends an image natively, a URL source that fails going out as text with its caption
func (a *QQAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	result, err := a.sendMedia(ctx, chatID, imageURL, MediaTypeImage, caption, replyTo, "")
	if err != nil || result.Success || !isURL(imageURL) {
		return result, err
	}
	slog.Warn(fmt.Sprintf("[%s] Image send failed, falling back to text: %s", a.logTag(), pyrepr.Str(result.Error)))
	fallback := imageURL
	if caption != "" {
		fallback = caption + "\n" + imageURL
	}
	return a.Self().Send(ctx, chatID, fallback, replyTo, nil)
}

// Sends a local image natively
func (a *QQAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMedia(ctx, chatID, imagePath, MediaTypeImage, caption, replyTo, "")
}

// Sends audio as a native voice message
func (a *QQAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMedia(ctx, chatID, audioPath, MediaTypeVoice, caption, replyTo, "")
}

// Sends a video natively
func (a *QQAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMedia(ctx, chatID, videoPath, MediaTypeVideo, caption, replyTo, "")
}

// Sends a file natively under an optional file name
func (a *QQAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMedia(ctx, chatID, filePath, MediaTypeFile, caption, replyTo, fileName)
}

// Uploads media, by URL in one call or as a chunked local upload, and sends it as a native message
func (a *QQAdapter) sendMedia(ctx context.Context, chatID, mediaSource string, fileType int, caption, replyTo, fileName string) (gateway.SendResult, error) {
	connected, err := a.ensureConnected(ctx)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if !connected {
		return notConnected(), nil
	}
	chatType := a.guessChatType(chatID)
	if chatType == "guild" {
		return gateway.SendResult{Success: false, Error: "Guild media send not supported via this path"}, nil
	}
	result, err := a.uploadAndSendMedia(ctx, chatType, chatID, mediaSource, fileType, caption, replyTo, fileName)
	if err == nil {
		return result, nil
	}
	if ctx.Err() != nil {
		return gateway.SendResult{}, ctx.Err()
	}
	var dailyLimit *UploadDailyLimitExceededError
	var tooLarge *UploadFileTooLargeError
	switch {
	case errors.As(err, &dailyLimit):
		slog.Warn(fmt.Sprintf("[%s] Daily upload limit exceeded for %s (%s)", a.logTag(), dailyLimit.FileName, dailyLimit.FileSizeHuman()))
		return gateway.SendResult{Success: false, Retryable: false,
			Error: fmt.Sprintf("QQ daily upload limit exceeded for %s (%s). Retry tomorrow.", pyrepr.Repr(dailyLimit.FileName), dailyLimit.FileSizeHuman())}, nil
	case errors.As(err, &tooLarge):
		slog.Warn(fmt.Sprintf("[%s] File too large: %s (%s, platform limit %s)", a.logTag(), tooLarge.FileName, tooLarge.FileSizeHuman(), tooLarge.LimitHuman()))
		return gateway.SendResult{Success: false, Retryable: false,
			Error: fmt.Sprintf("%s (%s) exceeds the QQ per-file upload limit (%s).", pyrepr.Repr(tooLarge.FileName), tooLarge.FileSizeHuman(), tooLarge.LimitHuman())}, nil
	}
	slog.Error(fmt.Sprintf("[%s] Media send failed: %s", a.logTag(), errText(err)))
	return gateway.SendResult{Success: false, Error: errTextOrName(err)}, nil
}

// File info of an upload response at the top level or under its data object
func extractFileInfo(upload any) any {
	dict, ok := asDict(upload)
	if !ok {
		return nil
	}
	if info := dict["file_info"]; pyrepr.Truthy(info) {
		return info
	}
	if data, isDict := asDict(dict["data"]); isDict {
		if info := data["file_info"]; pyrepr.Truthy(info) {
			return info
		}
	}
	return nil
}

// Uploads the media source and posts the rich media message that references it
func (a *QQAdapter) uploadAndSendMedia(ctx context.Context, chatType, chatID, mediaSource string, fileType int, caption, replyTo, fileName string) (gateway.SendResult, error) {
	var upload any
	var err error
	if isURL(mediaSource) {
		resolvedName := fileName
		if resolvedName == "" {
			parsed, perr := pyurl.Parse(mediaSource)
			if perr != nil {
				return gateway.SendResult{}, perr
			}
			if resolvedName = pypath.Name(parsed.Path); resolvedName == "" {
				resolvedName = "media"
			}
		}
		if fileType != MediaTypeFile {
			resolvedName = ""
		}
		upload, err = a.uploadMedia(ctx, chatType, chatID, fileType, mediaSource, "", false, resolvedName)
	} else {
		upload, err = a.uploadLocalFile(ctx, chatType, chatID, mediaSource, fileType, fileName)
	}
	if err != nil {
		return gateway.SendResult{}, err
	}
	if _, ok := asDict(upload); !ok {
		return gateway.SendResult{}, notDictErr(upload)
	}
	fileInfo := extractFileInfo(upload)
	if fileInfo == nil {
		return gateway.SendResult{Success: false, Error: "Upload returned no file_info: " + pyrepr.Str(upload)}, nil
	}
	body := obj("msg_type", int64(MsgTypeMedia), "media", obj("file_info", fileInfo), "msg_seq", nextMsgSeq())
	if caption != "" {
		body.Set("content", pystr.Slice(caption, 0, MaxMessageLength))
	}
	if replyTo != "" {
		body.Set("msg_id", replyTo)
	}
	return a.postMessage(ctx, messagesPath(chatType, chatID), body)
}

// Absolute path of a local media source, a ValueError for a placeholder and FileNotFoundError for a missing file
func resolveLocalMedia(mediaSource string, placeholderCheck bool) (string, error) {
	localPath := pypath.Expanduser(mediaSource)
	if !pypath.IsAbs(localPath) {
		abs, err := filepath.Abs(localPath)
		if err != nil {
			return "", err
		}
		localPath = abs
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			localPath = resolved
		}
	}
	if info, err := os.Stat(localPath); err != nil || !info.Mode().IsRegular() {
		if placeholderCheck && (strings.HasPrefix(mediaSource, "<") || pystr.Len(mediaSource) < 3) {
			return "", &pyErr{"ValueError", "Invalid media source (looks like a placeholder): " + pyrepr.Repr(mediaSource)}
		}
		return "", &pyErr{"FileNotFoundError", "Media file not found: " + localPath}
	}
	return localPath, nil
}

// PUT of part bytes to a presigned URL through a client
func httpPutVia(client *http.Client) HTTPPutFunc {
	return func(ctx context.Context, url string, data []byte, headers map[string]string) (PutResponse, error) {
		resp, err := httpRequest(ctx, client, http.MethodPut, url, headers, data, DefaultAPITimeout)
		if err != nil {
			return PutResponse{}, err
		}
		return PutResponse{StatusCode: resp.StatusCode, Text: resp.Text()}, nil
	}
}

// Uploads a local file in chunks and returns the complete response that carries its file info
func (a *QQAdapter) uploadLocalFile(ctx context.Context, chatType, chatID, mediaSource string, fileType int, fileName string) (any, error) {
	client, err := a.requireHTTPClient()
	if err != nil {
		return nil, err
	}
	localPath, err := resolveLocalMedia(mediaSource, true)
	if err != nil {
		return nil, err
	}
	if fileName == "" {
		fileName = pypath.Name(localPath)
	}
	return NewChunkedUploader(a.apiRequest, httpPutVia(client), a.logTag()).Upload(ctx, chatType, chatID, localPath, fileType, fileName)
}

// Input notify of a direct chat, sent at most once per debounce window and only with an inbound message id to reply to
func (a *QQAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	now := wallClock()
	a.mu.Lock()
	msgID, sentAt := a.lastMsgID[chatID], a.typingSentAt[chatID]
	a.mu.Unlock()
	if !a.IsConnected() || a.guessChatType(chatID) != "c2c" || msgID == "" || now-sentAt < typingDebounceSeconds {
		return nil
	}
	body := obj("msg_type", int64(MsgTypeInputNotify), "msg_id", msgID,
		"input_notify", obj("input_type", int64(1), "input_second", int64(typingInputSeconds)), "msg_seq", nextMsgSeq())
	if _, err := a.apiRequest(ctx, http.MethodPost, "/v2/users/"+chatID+"/messages", body, DefaultAPITimeout); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Debug(fmt.Sprintf("[%s] send_typing failed: %s", a.logTag(), errText(err)))
		return nil
	}
	a.mu.Lock()
	a.typingSentAt[chatID] = now
	a.mu.Unlock()
	return nil
}

// Markdown passed through when the bot supports it and stripped otherwise
func (a *QQAdapter) FormatMessage(content string) string {
	if a.markdownSupport {
		return content
	}
	return gateway.StripMarkdown(content, false)
}

// Name and type of a chat, groups and guild channels counting as group
func (a *QQAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	chatType := "dm"
	if kind := a.guessChatType(chatID); kind == "group" || kind == "guild" {
		chatType = "group"
	}
	return map[string]any{"name": chatID, "type": chatType}, nil
}

// Whether a media source is an http or https URL
func isURL(source string) bool {
	parsed, err := pyurl.Parse(source)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

// QQ chat kind recorded from inbound traffic, c2c when the chat was never seen
func (a *QQAdapter) guessChatType(chatID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if kind, ok := a.chatTypeMap[chatID]; ok {
		return kind
	}
	return "c2c"
}
