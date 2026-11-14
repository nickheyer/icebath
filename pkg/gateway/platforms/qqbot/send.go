package qqbot

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// File extension sets of the shared standalone senders
var (
	senderImageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}
	senderVideoExts = map[string]bool{".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true, ".3gp": true}
	senderAudioExts = map[string]bool{".ogg": true, ".opus": true, ".mp3": true, ".m2a": true, ".wav": true, ".m4a": true, ".flac": true}
	senderVoiceExts = map[string]bool{".ogg": true, ".opus": true}
)

// Transport of out of process sends, a clone of the default transport when nil
var standaloneTransport http.RoundTripper

// Success payload of a standalone send with its extra fields and the warnings only when there are any
func sendSuccess(chatID string, warnings []string, fields ...any) map[string]any {
	out := map[string]any{"success": true, "platform": "qqbot", "chat_id": chatID}
	for i := 0; i+1 < len(fields); i += 2 {
		out[fields[i].(string)] = fields[i+1]
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return out
}

// QQ file type of a local path and voice flag
func qqbotMediaFileType(mediaPath string, isVoice bool) int {
	_, ext := pypath.Splitext(mediaPath)
	ext = pystr.Lower(ext)
	switch {
	case isVoice || senderVoiceExts[ext]:
		return MediaTypeVoice
	case senderImageExts[ext]:
		return MediaTypeImage
	case senderVideoExts[ext]:
		return MediaTypeVideo
	case senderAudioExts[ext]:
		return MediaTypeVoice
	}
	return MediaTypeFile
}

// One JSON call against the QQ API with the standalone headers, a RuntimeError for an HTTP error status
func qqbotAPIJSON(ctx context.Context, client *http.Client, headers map[string]string, method, path string, body *pyjson.Object, timeout float64) (map[string]any, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = jsonBody(body); err != nil {
			return nil, err
		}
	}
	resp, err := httpRequest(ctx, client, method, APIBase+path, headers, payload, timeout)
	if err != nil {
		return nil, err
	}
	var data any = map[string]any{}
	if len(resp.Content) > 0 {
		if decoded, derr := resp.JSON(); derr == nil {
			data = decoded
		}
	}
	dict, isDict := asDict(data)
	if resp.StatusCode >= 400 {
		var detail any
		if isDict {
			detail = getOr(dict, "message", data)
		} else if pyrepr.Truthy(data) {
			return nil, notDictErr(data)
		}
		if !pyrepr.Truthy(detail) {
			detail = pystr.Slice(resp.Text(), 0, 200)
		}
		return nil, runtimeError(fmt.Sprintf("QQ Bot API error [%d] %s: %s", resp.StatusCode, path, pyrepr.Str(detail)))
	}
	if !isDict {
		return map[string]any{}, nil
	}
	return dict, nil
}

// Uploads a local file in chunks through the standalone client and returns the complete response
func qqbotUploadLocalFile(ctx context.Context, client *http.Client, headers map[string]string, chatType, chatID, mediaPath string, fileType int) (any, error) {
	localPath, err := resolveLocalMedia(mediaPath, false)
	if err != nil {
		return nil, err
	}
	apiRequest := func(ctx context.Context, method, path string, body *pyjson.Object, timeout float64) (any, error) {
		if timeout == 0 {
			timeout = FileUploadTimeout
		}
		return qqbotAPIJSON(ctx, client, headers, method, path, body, timeout)
	}
	return NewChunkedUploader(apiRequest, httpPutVia(client), "send_message.qqbot").Upload(ctx, chatType, chatID, localPath, fileType, pypath.Name(localPath))
}

// Asks QQ to fetch a URL into its media store and returns the upload response
func qqbotUploadURL(ctx context.Context, client *http.Client, headers map[string]string, chatType, chatID, url string, fileType int, fileName string) (map[string]any, error) {
	body := obj("file_type", int64(fileType), "srv_send_msg", false, "url", url)
	if fileType == MediaTypeFile && fileName != "" {
		body.Set("file_name", fileName)
	}
	return qqbotAPIJSON(ctx, client, headers, http.MethodPost, restPath(chatType, chatID, "files"), body, FileUploadTimeout)
}

// Posts a rich media message that references uploaded file info
func qqbotSendMediaMessage(ctx context.Context, client *http.Client, headers map[string]string, chatType, chatID string, fileInfo any, caption string) (map[string]any, error) {
	body := obj("msg_type", int64(MsgTypeMedia), "media", obj("file_info", fileInfo), "msg_seq", int64(wallClock()*1000)%1_000_000_000)
	if stripped := pystr.Strip(caption); stripped != "" {
		body.Set("content", pystr.Slice(stripped, 0, MaxMessageLength))
	}
	return qqbotAPIJSON(ctx, client, headers, http.MethodPost, messagesPath(chatType, chatID), body, 30.0)
}

// Sends text through the channel, C2C and group endpoints in turn, the first accepted one winning
func qqbotSendTextMessage(ctx context.Context, client *http.Client, headers map[string]string, chatID, message string, timeout float64) (map[string]any, error) {
	payload, err := jsonBody(obj("content", pystr.Slice(message, 0, 4000), "msg_type", int64(0)))
	if err != nil {
		return nil, err
	}
	var statuses []string
	for _, endpoint := range []struct{ kind, url string }{
		{"channel", "https://api.sgroup.qq.com/channels/" + chatID + "/messages"},
		{"c2c", "https://api.sgroup.qq.com/v2/users/" + chatID + "/messages"},
		{"group", "https://api.sgroup.qq.com/v2/groups/" + chatID + "/messages"},
	} {
		resp, err := httpRequest(ctx, client, http.MethodPost, endpoint.url, headers, payload, timeout)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == 200 || resp.StatusCode == 201 {
			decoded, err := resp.JSON()
			if err != nil {
				return nil, err
			}
			dict, ok := asDict(decoded)
			if !ok {
				return nil, notDictErr(decoded)
			}
			return sendSuccess(chatID, nil, "message_id", dict["id"]), nil
		}
		statuses = append(statuses, fmt.Sprintf("%s=%d", endpoint.kind, resp.StatusCode))
	}
	return gateway.SendError("QQBot send failed: " + strings.Join(statuses, " ")), nil
}

// Uploads one attachment and sends it, trying the C2C and group endpoints in turn
func qqbotDeliverOneMedia(ctx context.Context, client *http.Client, headers map[string]string, chatID, mediaPath string, isVoice bool, caption string) (map[string]any, error) {
	fileType := qqbotMediaFileType(mediaPath, isVoice)
	sourceIsURL := isURL(mediaPath)
	var failures []string
	for _, chatType := range []string{"c2c", "group"} {
		result, err := func() (map[string]any, error) {
			var upload any
			var err error
			if sourceIsURL {
				parsed, perr := pyurl.Parse(mediaPath)
				if perr != nil {
					return nil, perr
				}
				name := pypath.Name(parsed.Path)
				if name == "" {
					name = "media"
				}
				upload, err = qqbotUploadURL(ctx, client, headers, chatType, chatID, mediaPath, fileType, name)
			} else {
				upload, err = qqbotUploadLocalFile(ctx, client, headers, chatType, chatID, mediaPath, fileType)
			}
			if err != nil {
				return nil, err
			}
			fileInfo := extractFileInfo(upload)
			if fileInfo == nil {
				failures = append(failures, fmt.Sprintf("%s: upload returned no file_info: %s", chatType, pyrepr.Str(upload)))
				return nil, nil
			}
			sendData, err := qqbotSendMediaMessage(ctx, client, headers, chatType, chatID, fileInfo, caption)
			if err != nil {
				return nil, err
			}
			return sendSuccess(chatID, nil, "message_id", sendData["id"], "chat_type", chatType), nil
		}()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failures = append(failures, chatType+": "+errText(err))
			continue
		}
		if result != nil {
			return result, nil
		}
	}
	return gateway.SendError("QQBot media send failed (guild/channel native media is unsupported; tried c2c and group): " + strings.Join(failures, "; ")), nil
}

// Payload with the collected warnings merged in when there are any
func withWarnings(payload map[string]any, warnings []string) map[string]any {
	if len(warnings) == 0 {
		return payload
	}
	out := map[string]any{}
	for k, v := range payload {
		out[k] = v
	}
	out["warnings"] = warnings
	return out
}

// Sends text and media through the QQ Bot REST API without a websocket, text first and each attachment as its own bubble
func sendQQBot(ctx context.Context, pconfig *gateway.PlatformConfig, chatID, message string, mediaFiles []gateway.MediaRef, caption string) (map[string]any, error) {
	extra := map[string]any{}
	var token any
	if pconfig != nil {
		token = pconfig.Token
		if pconfig.Extra != nil {
			extra = pconfig.Extra
		}
	}
	appID := extra["app_id"]
	if !pyrepr.Truthy(appID) {
		secret, err := gateway.GetScopedSecret(ctx, "QQ_APP_ID", "", false)
		if err != nil {
			return nil, err
		}
		appID = secret
	}
	secret := pyrepr.Or(token, extra["client_secret"])
	if !pyrepr.Truthy(secret) {
		scoped, err := gateway.GetScopedSecret(ctx, "QQ_CLIENT_SECRET", "", false)
		if err != nil {
			return nil, err
		}
		secret = scoped
	}
	if !pyrepr.Truthy(appID) || !pyrepr.Truthy(secret) {
		return gateway.SendError("QQBot: QQ_APP_ID / QQ_CLIENT_SECRET not configured."), nil
	}
	clientTimeout := 15.0
	if len(mediaFiles) > 0 {
		clientTimeout = FileUploadTimeout
	}
	result, err := sendQQBotWith(ctx, pyrepr.Str(appID), pyrepr.Str(secret), chatID, message, mediaFiles, caption, clientTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Debug("QQBot standalone send failed", "error", err)
		return gateway.SendError("QQBot send failed: " + errText(err)), nil
	}
	return result, nil
}

// Token fetch and delivery of one standalone send, the error the caller wraps
func sendQQBotWith(ctx context.Context, appID, secret, chatID, message string, mediaFiles []gateway.MediaRef, caption string, clientTimeout float64) (map[string]any, error) {
	transport := standaloneTransport
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	client := &http.Client{Transport: transport, CheckRedirect: pyhttpx.RedirectPolicy(false)}
	defer client.CloseIdleConnections()
	tokenBody, err := jsonBody(obj("appId", appID, "clientSecret", secret))
	if err != nil {
		return nil, err
	}
	tokenResp, err := httpRequest(ctx, client, http.MethodPost, "https://bots.qq.com/app/getAppAccessToken",
		map[string]string{"Content-Type": "application/json"}, tokenBody, clientTimeout)
	if err != nil {
		return nil, err
	}
	if tokenResp.StatusCode != 200 {
		return gateway.SendError(fmt.Sprintf("QQBot token request failed: %d", tokenResp.StatusCode)), nil
	}
	decoded, err := tokenResp.JSON()
	if err != nil {
		return nil, err
	}
	tokenData, ok := asDict(decoded)
	if !ok {
		return nil, notDictErr(decoded)
	}
	accessToken := tokenData["access_token"]
	if !pyrepr.Truthy(accessToken) {
		return gateway.SendError("QQBot: no access_token in response"), nil
	}
	headers := map[string]string{"Authorization": "QQBot " + pyrepr.Str(accessToken), "Content-Type": "application/json"}
	if len(mediaFiles) == 0 {
		return qqbotSendTextMessage(ctx, client, headers, chatID, message, clientTimeout)
	}
	var lastResult map[string]any
	text := pystr.Strip(message)
	var warnings []string
	if text != "" && pystr.Strip(caption) == "" {
		textResult, err := qqbotSendTextMessage(ctx, client, headers, chatID, text, clientTimeout)
		if err != nil {
			return nil, err
		}
		if pyrepr.Truthy(textResult["error"]) {
			return textResult, nil
		}
		lastResult = textResult
	}
	for index, media := range mediaFiles {
		if media.Path == "" {
			continue
		}
		mediaCaption := ""
		if index == 0 {
			mediaCaption = caption
		}
		if _, statErr := os.Stat(media.Path); statErr != nil && !isURL(media.Path) {
			warnings = append(warnings, "QQBot media file not found, skipping: "+media.Path)
			slog.Warn(warnings[len(warnings)-1])
			continue
		}
		result, err := qqbotDeliverOneMedia(ctx, client, headers, chatID, media.Path, media.IsVoice, mediaCaption)
		if err != nil {
			return nil, err
		}
		if failure := result["error"]; pyrepr.Truthy(failure) {
			warnings = append(warnings, pyrepr.Str(failure))
			slog.Warn(fmt.Sprintf("QQBot media delivery failed, omitting attachment: %s", pyrepr.Str(failure)))
			continue
		}
		lastResult = result
	}
	if lastResult == nil {
		fallbackText := text
		if fallbackText == "" {
			fallbackText = pystr.Strip(caption)
		}
		if fallbackText == "" {
			return withWarnings(map[string]any{"error": "QQBot: no deliverable media attachments"}, warnings), nil
		}
		textResult, err := qqbotSendTextMessage(ctx, client, headers, chatID, fallbackText, clientTimeout)
		if err != nil {
			return nil, err
		}
		if pyrepr.Truthy(textResult["error"]) {
			return withWarnings(textResult, warnings), nil
		}
		lastResult = textResult
	}
	if len(warnings) > 0 {
		existing, _ := lastResult["warnings"].([]string)
		lastResult["warnings"] = append(append([]string{}, existing...), warnings...)
	}
	return lastResult, nil
}
