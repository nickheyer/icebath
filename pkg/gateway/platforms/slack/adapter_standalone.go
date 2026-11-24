package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	slackapi "github.com/slack-go/slack"
)

const slackDMCacheMax = 5000

var (
	slackDMCacheMu sync.Mutex
	slackDMCache   = orderedutil.NewMap[string, string]()
)

// Bounds the standalone DM cache oldest insertion first
func trimSlackDMCache() {
	for slackDMCache.Len() > slackDMCacheMax {
		slackDMCache.Delete(slackDMCache.Keys()[0])
	}
}

var wrongWorkspaceTokenErrors = map[string]bool{
	"invalid_auth": true, "not_authed": true, "token_revoked": true, "account_inactive": true,
	"not_in_channel": true, "channel_not_found": true,
}

// Comma separated tokens plus the saved slack_tokens.json OAuth tokens, deduplicated in file order
func loadSlackBotTokens(ctx context.Context, rawToken string, quiet bool) []string {
	var tokens []string
	for _, t := range strings.Split(rawToken, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tokens = append(tokens, t)
		}
	}
	tokensFile := filepath.Join(home.Dir(ctx), "slack_tokens.json")
	if _, err := os.Stat(tokensFile); err != nil {
		return tokens
	}
	if !quiet {
		config.WarnIfCredentialFileBroadlyReadable(tokensFile, "[Slack]", pkgLog())
	}
	raw, err := os.ReadFile(tokensFile)
	var saved any
	if err == nil {
		var text string
		if text, err = textutil.DecodeUTF8(textutil.TrimBOM(raw)); err == nil {
			saved, err = jsonutil.Decode(text)
		}
	}
	if err == nil {
		obj, ok := saved.(*jsonutil.Object)
		if !ok {
			err = fmt.Errorf("'%s' object has no attribute 'items'", anyutil.Kind(jsonutil.Plain(saved)))
		} else {
			for _, teamID := range obj.Keys() {
				entry, _ := obj.Get(teamID)
				entryMap, isMap := jsonutil.Plain(entry).(map[string]any)
				tok := ""
				if isMap {
					tok = strOrEmpty(entryMap["token"])
				}
				if tok == "" || containsString(tokens, tok) {
					continue
				}
				tokens = append(tokens, tok)
				if !quiet {
					label := teamID
					if isMap {
						label = anyutil.String(getOr(entryMap, "team_name", teamID))
					}
					pkgLog().Info(fmt.Sprintf("[Slack] Loaded saved token for workspace %s", label))
				}
			}
		}
	}
	if err != nil && !quiet {
		pkgLog().Warn(fmt.Sprintf("[Slack] Failed to read %s: %v", tokensFile, err))
	}
	return tokens
}

// Whether the list holds the value
func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// HTTP client of the standalone senders honouring the configured proxy
func standaloneHTTPClient(ctx context.Context, timeout time.Duration) *http.Client {
	proxyURL, _ := gateway.ResolveProxyURL(ctx, "", nil, "")
	client := slackHTTPClient(proxyURL)
	client.Timeout = timeout
	return client
}

// Text of a failed standalone request for a log line or a send result, the kind of error and the proxy's host when the resolved proxy URL holds a
// secret the raw text could repeat
func standaloneErrorText(ctx context.Context, err error) string {
	proxyURL, _ := gateway.ResolveProxyURL(ctx, "", nil, "")
	return gateway.ProxySafeErrorText(err, proxyURL)
}

// POSTs a JSON payload to a Web API method with a bearer token and returns the decoded body
func slackJSONPost(ctx context.Context, client *http.Client, token, method string, payload map[string]any) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackapi.APIURL+method, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var decoded any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	return asMap(decoded), nil
}

// DM conversation id of a user id through conversations.open, cached per token and user
func resolveSlackUserDM(ctx context.Context, token, userID string) string {
	cacheKey := token + ":" + userID
	slackDMCacheMu.Lock()
	cached, ok := slackDMCache.Get(cacheKey)
	slackDMCacheMu.Unlock()
	if ok {
		return cached
	}
	data, err := slackJSONPost(ctx, standaloneHTTPClient(ctx, 15*time.Second), token, "conversations.open", map[string]any{"users": userID})
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[Slack] conversations.open exception for %s: %s", userID, standaloneErrorText(ctx, err)))
		return ""
	}
	if anyutil.IsTrue(data["ok"]) {
		if channelID := strOrEmpty(asMap(getOr(data, "channel", map[string]any{}))["id"]); channelID != "" {
			slackDMCacheMu.Lock()
			slackDMCache.Set(cacheKey, channelID)
			trimSlackDMCache()
			slackDMCacheMu.Unlock()
			return channelID
		}
	}
	pkgLog().Warn(fmt.Sprintf("[Slack] conversations.open failed for %s: %s", userID, anyutil.String(getOr(data, "error", "unknown"))))
	return ""
}

// chat.postMessage arguments of the standalone senders
func standalonePostKwargs(chatID string, text any, unfurlKwargs map[string]bool, threadID string) map[string]any {
	kwargs := map[string]any{"channel": chatID, "text": text, "mrkdwn": true}
	for k, v := range unfurlKwargs {
		kwargs[k] = v
	}
	if threadID != "" {
		kwargs["thread_ts"] = threadID
	}
	return kwargs
}

// chat.postMessage through the client returning the response payload, an API failure included
func standalonePostText(ctx context.Context, client *webClient, chatID string, text any, unfurlKwargs map[string]bool, threadID string) (map[string]any, error) {
	result, err := client.call(ctx, "chat.postMessage", standalonePostKwargs(chatID, text, unfurlKwargs, threadID))
	if err != nil {
		if apiErr, ok := err.(*slackAPIError); ok && apiErr.StatusCode == http.StatusOK {
			return apiErr.Response, nil
		}
		return nil, err
	}
	return result.Data, nil
}

// Uploads one local file and reports the message ts its shares carry
func standaloneUploadFile(ctx context.Context, client *webClient, chatID, mediaPath, initialComment, threadID string) (map[string]any, error) {
	result, err := client.filesUploadV2(ctx, chatID, []fileUpload{{File: mediaPath, Filename: filepath.Base(mediaPath)}}, initialComment, threadID)
	var payload map[string]any
	if err != nil {
		apiErr, ok := err.(*slackAPIError)
		if !ok || apiErr.StatusCode != http.StatusOK {
			return nil, err
		}
		payload = apiErr.Response
	} else {
		payload = result.Data
	}
	if payload["ok"] == false {
		return gateway.SendError("Slack API error: " + anyutil.String(getOr(payload, "error", "unknown"))), nil
	}
	var messageID any
	if len(payload) > 0 {
		fileObj := asMap(firstTruthyValue(payload["file"], map[string]any{}))
		shares := asMap(firstTruthyValue(fileObj["shares"], map[string]any{}))
	sharesLoop:
		for _, bucketKey := range sortedKeys(shares) {
			bucket, ok := shares[bucketKey].(map[string]any)
			if !ok {
				continue
			}
			for _, entriesKey := range sortedKeys(bucket) {
				if entries, ok := bucket[entriesKey].([]any); ok && len(entries) > 0 {
					if ts := asMap(entries[0])["ts"]; anyutil.IsTrue(ts) {
						messageID = ts
					}
					break
				}
			}
			if anyutil.IsTrue(messageID) {
				break sharesLoop
			}
		}
		if !anyutil.IsTrue(messageID) {
			messageID = firstTruthyValue(fileObj["timestamp"], payload["ts"])
		}
	}
	return map[string]any{"success": true, "message_id": messageID, "raw": payload}, nil
}

// Media branch of the standalone sender, one upload per file with the caption on the first success
func standaloneSendMedia(ctx context.Context, token, chatID string, mediaFiles []gateway.MediaRef, threadID, formatted, formattedCaption string, unfurlKwargs map[string]bool) map[string]any {
	var warnings []string
	proxyURL, _ := gateway.ResolveProxyURL(ctx, "", nil, "")
	client := newWebClient(token, proxyURL)
	var lastMessageID any
	captionAsUploadComment := formattedCaption != "" && len(unfurlKwargs) == 0
	textToSend := ""
	if !captionAsUploadComment {
		textToSend = formattedCaption
		if textToSend == "" {
			textToSend = formatted
		}
	}
	if strings.TrimSpace(textToSend) != "" {
		payload, err := standalonePostText(ctx, client, chatID, textToSend, unfurlKwargs, threadID)
		if err != nil {
			return gateway.SendError("Slack send failed: " + gateway.ProxySafeErrorText(err, proxyURL))
		}
		if !anyutil.IsTrue(getOr(payload, "ok", true)) {
			return gateway.SendError("Slack API error: " + anyutil.String(getOr(payload, "error", "unknown")))
		}
		lastMessageID = payload["ts"]
	}
	captionPending := captionAsUploadComment
	uploadedAny := false
	for _, media := range mediaFiles {
		if _, err := os.Stat(media.Path); err != nil {
			warning := "Media file not found, skipping: " + media.Path
			pkgLog().Warn("[Slack] " + warning)
			warnings = append(warnings, warning)
			if captionPending {
				fb, err := standalonePostText(ctx, client, chatID, formattedCaption, unfurlKwargs, threadID)
				if err != nil && gateway.DescribeProxyError(err, proxyURL) != "" {
					pkgLog().Warn("[Slack] Caption-fallback send failed for missing media")
				} else if err != nil {
					pkgLog().Warn("[Slack] Caption-fallback send failed for missing media", "error", err)
				} else if anyutil.IsTrue(getOr(fb, "ok", true)) {
					lastMessageID = firstTruthyValue(fb["ts"], lastMessageID)
					captionPending = false
				}
			}
			continue
		}
		comment := ""
		if captionPending {
			comment = formattedCaption
		}
		uploadResult, err := standaloneUploadFile(ctx, client, chatID, media.Path, comment, threadID)
		if err != nil {
			warning := fmt.Sprintf("Failed to send media %s: %s", media.Path, gateway.ProxySafeErrorText(err, proxyURL))
			pkgLog().Error("[Slack] " + warning)
			warnings = append(warnings, warning)
			continue
		}
		if anyutil.IsTrue(uploadResult["error"]) {
			warnings = append(warnings, fmt.Sprintf("Failed to send media %s: %s", media.Path, anyutil.String(uploadResult["error"])))
			continue
		}
		uploadedAny = true
		captionPending = false
		lastMessageID = firstTruthyValue(uploadResult["message_id"], lastMessageID)
	}
	var result map[string]any
	if lastMessageID == nil && !uploadedAny && strings.TrimSpace(textToSend) == "" {
		result = map[string]any{"error": "No deliverable text or media remained after processing"}
	} else {
		result = map[string]any{"success": true, "platform": "slack", "chat_id": chatID, "message_id": lastMessageID}
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	return result
}

// Slack mrkdwn of text without a live adapter
func standaloneFormatMrkdwn(text string) string {
	if text == "" {
		return text
	}
	return formatSlackMrkdwn(text)
}

// Out of process delivery for cron and tool processes, text through chat.postMessage and media by upload
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID := strOrEmpty(kwargs["chat_id"])
	message := strOrEmpty(kwargs["message"])
	threadID := strOrEmpty(kwargs["thread_id"])
	caption := strOrEmpty(kwargs["caption"])
	mediaFiles, _ := kwargs["media_files"].([]gateway.MediaRef)
	var rawToken any
	if pconfig != nil {
		rawToken = pconfig.Token
	}
	if !anyutil.IsTrue(rawToken) {
		rawToken, _, _ = provider.GetSecret(ctx, "SLACK_BOT_TOKEN")
	}
	tokens := loadSlackBotTokens(ctx, strOrEmpty(rawToken), true)
	if len(tokens) == 0 {
		return gateway.SendError("Slack send failed: SLACK_BOT_TOKEN not configured"), nil
	}
	token := tokens[0]
	if strings.HasPrefix(chatID, "U") || strings.HasPrefix(chatID, "W") {
		resolved := ""
		for _, tok := range tokens {
			if resolved = resolveSlackUserDM(ctx, tok, chatID); resolved != "" {
				token = tok
				break
			}
		}
		if resolved == "" {
			return map[string]any{"error": "Slack user ID resolution failed for " + chatID + " (conversations.open — check the bot's im:write scope)"}, nil
		}
		chatID = resolved
	}
	formatted := standaloneFormatMrkdwn(message)
	formattedCaption := standaloneFormatMrkdwn(caption)
	var extra map[string]any
	if pconfig != nil {
		extra = pconfig.Extra
	}
	unfurlKwargs := slackUnfurlKwargs(extra)
	if len(mediaFiles) > 0 {
		return standaloneSendMedia(ctx, token, chatID, mediaFiles, threadID, formatted, formattedCaption, unfurlKwargs), nil
	}
	if strings.TrimSpace(formatted) == "" {
		pkgLog().Debug("[Slack] _standalone_send: skipping empty/whitespace message")
		return map[string]any{"success": true, "platform": "slack", "skipped": "empty_text"}, nil
	}
	client := standaloneHTTPClient(ctx, 30*time.Second)
	payload := standalonePostKwargs(chatID, formatted, unfurlKwargs, threadID)
	lastError := "unknown"
	for _, tok := range tokens {
		data, err := slackJSONPost(ctx, client, tok, "chat.postMessage", payload)
		if err != nil {
			return gateway.SendError("Slack send failed: " + standaloneErrorText(ctx, err)), nil
		}
		if anyutil.IsTrue(data["ok"]) {
			return map[string]any{"success": true, "platform": "slack", "chat_id": chatID, "message_id": data["ts"]}, nil
		}
		lastError = anyutil.String(getOr(data, "error", "unknown"))
		if !wrongWorkspaceTokenErrors[lastError] {
			break
		}
	}
	return gateway.SendError("Slack API error: " + lastError), nil
}
