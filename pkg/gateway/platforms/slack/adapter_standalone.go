package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	slackapi "github.com/slack-go/slack"
)

const slackDMCacheMax = 5000

var (
	slackDMCacheMu sync.Mutex
	slackDMCache   = pydict.New[string, string]()
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
		if t = pystr.Strip(t); t != "" {
			tokens = append(tokens, t)
		}
	}
	tokensFile := filepath.Join(home.Dir(ctx), "slack_tokens.json")
	if _, err := os.Stat(tokensFile); err != nil {
		return tokens
	}
	if !quiet {
		config.WarnIfCredentialFileBroadlyReadable(tokensFile, "[Slack]", slog.Default())
	}
	raw, err := os.ReadFile(tokensFile)
	var saved any
	if err == nil {
		var text string
		if text, err = pystr.DecodeUTF8Sig(raw); err == nil {
			saved, err = pyjson.Loads(text)
		}
	}
	if err == nil {
		obj, ok := saved.(*pyjson.Object)
		if !ok {
			err = fmt.Errorf("'%s' object has no attribute 'items'", pyrepr.TypeName(pyjson.Plain(saved)))
		} else {
			for _, teamID := range obj.Keys {
				entry, _ := obj.Get(teamID)
				entryMap, isMap := pyjson.Plain(entry).(map[string]any)
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
						label = pyrepr.Str(getOr(entryMap, "team_name", teamID))
					}
					slog.Info(fmt.Sprintf("[Slack] Loaded saved token for workspace %s", label))
				}
			}
		}
	}
	if err != nil && !quiet {
		slog.Warn(fmt.Sprintf("[Slack] Failed to read %s: %v", tokensFile, err))
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
		slog.Warn(fmt.Sprintf("[Slack] conversations.open exception for %s: %v", userID, err))
		return ""
	}
	if pyrepr.Truthy(data["ok"]) {
		if channelID := strOrEmpty(asMap(getOr(data, "channel", map[string]any{}))["id"]); channelID != "" {
			slackDMCacheMu.Lock()
			slackDMCache.Set(cacheKey, channelID)
			trimSlackDMCache()
			slackDMCacheMu.Unlock()
			return channelID
		}
	}
	slog.Warn(fmt.Sprintf("[Slack] conversations.open failed for %s: %s", userID, pyrepr.Str(getOr(data, "error", "unknown"))))
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
		return gateway.SendError("Slack API error: " + pyrepr.Str(getOr(payload, "error", "unknown"))), nil
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
					if ts := asMap(entries[0])["ts"]; pyrepr.Truthy(ts) {
						messageID = ts
					}
					break
				}
			}
			if pyrepr.Truthy(messageID) {
				break sharesLoop
			}
		}
		if !pyrepr.Truthy(messageID) {
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
	if pystr.Strip(textToSend) != "" {
		payload, err := standalonePostText(ctx, client, chatID, textToSend, unfurlKwargs, threadID)
		if err != nil {
			return gateway.SendError(fmt.Sprintf("Slack send failed: %v", err))
		}
		if !pyrepr.Truthy(getOr(payload, "ok", true)) {
			return gateway.SendError("Slack API error: " + pyrepr.Str(getOr(payload, "error", "unknown")))
		}
		lastMessageID = payload["ts"]
	}
	captionPending := captionAsUploadComment
	uploadedAny := false
	for _, media := range mediaFiles {
		if _, err := os.Stat(media.Path); err != nil {
			warning := "Media file not found, skipping: " + media.Path
			slog.Warn("[Slack] " + warning)
			warnings = append(warnings, warning)
			if captionPending {
				fb, err := standalonePostText(ctx, client, chatID, formattedCaption, unfurlKwargs, threadID)
				if err != nil {
					slog.Warn("[Slack] Caption-fallback send failed for missing media", "error", err)
				} else if pyrepr.Truthy(getOr(fb, "ok", true)) {
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
			warning := fmt.Sprintf("Failed to send media %s: %v", media.Path, err)
			slog.Error("[Slack] " + warning)
			warnings = append(warnings, warning)
			continue
		}
		if pyrepr.Truthy(uploadResult["error"]) {
			warnings = append(warnings, fmt.Sprintf("Failed to send media %s: %s", media.Path, pyrepr.Str(uploadResult["error"])))
			continue
		}
		uploadedAny = true
		captionPending = false
		lastMessageID = firstTruthyValue(uploadResult["message_id"], lastMessageID)
	}
	var result map[string]any
	if lastMessageID == nil && !uploadedAny && pystr.Strip(textToSend) == "" {
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
	if !pyrepr.Truthy(rawToken) {
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
	if pystr.Strip(formatted) == "" {
		slog.Debug("[Slack] _standalone_send: skipping empty/whitespace message")
		return map[string]any{"success": true, "platform": "slack", "skipped": "empty_text"}, nil
	}
	client := standaloneHTTPClient(ctx, 30*time.Second)
	payload := standalonePostKwargs(chatID, formatted, unfurlKwargs, threadID)
	lastError := "unknown"
	for _, tok := range tokens {
		data, err := slackJSONPost(ctx, client, tok, "chat.postMessage", payload)
		if err != nil {
			return gateway.SendError(fmt.Sprintf("Slack send failed: %v", err)), nil
		}
		if pyrepr.Truthy(data["ok"]) {
			return map[string]any{"success": true, "platform": "slack", "chat_id": chatID, "message_id": data["ts"]}, nil
		}
		lastError = pyrepr.Str(getOr(data, "error", "unknown"))
		if !wrongWorkspaceTokenErrors[lastError] {
			break
		}
	}
	return gateway.SendError("Slack API error: " + lastError), nil
}
