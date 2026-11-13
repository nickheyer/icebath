package feishu

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var errPayloadTooLarge = errors.New("payload too large")

// At most maxBytes of the raw request body, failing when the stream holds more
func readLimitedFeishuWebhookBody(ctx context.Context, request *gateway.WebRequest, maxBytes int) ([]byte, error) {
	body, err := request.ReadExactly(ctx, maxBytes+1)
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		return nil, errPayloadTooLarge
	}
	return body, nil
}

// Counts consecutive error responses per remote address, warning at every threshold multiple
func (a *FeishuAdapter) recordWebhookAnomaly(remoteIP, status string) {
	now := nowFn()
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.webhookAnomalyCounts[remoteIP]
	if !ok {
		entry = anomalyEntry{firstSeen: now}
	}
	count, firstSeen := entry.count, entry.firstSeen
	if count > 0 && now-firstSeen >= feishuWebhookAnomalyTTLSeconds {
		count, firstSeen = 0, now
	}
	count++
	if count%feishuWebhookAnomalyThreshold == 0 {
		slog.Warn(fmt.Sprintf("[Feishu] Webhook anomaly: %d consecutive error responses (%s) from %s over the last %.0fs", count, status, remoteIP, now-firstSeen))
	}
	a.webhookAnomalyCounts[remoteIP] = anomalyEntry{count: count, lastStatus: status, firstSeen: firstSeen}
}

func (a *FeishuAdapter) clearWebhookAnomaly(remoteIP string) {
	a.mu.Lock()
	delete(a.webhookAnomalyCounts, remoteIP)
	a.mu.Unlock()
}

// Records an anomaly and builds the matching text or JSON error response
func (a *FeishuAdapter) webhookReject(remoteIP, anomaly string, status int, text, jsonMsg string) (*gateway.WebResponse, error) {
	a.recordWebhookAnomaly(remoteIP, anomaly)
	if jsonMsg != "" {
		body := pyjson.NewObject()
		body.Set("code", int64(status))
		body.Set("msg", jsonMsg)
		return gateway.WebJSONResponse(body, status, nil)
	}
	return gateway.WebTextResponse(status, text, "", nil), nil
}

// Rate limits, validates and dispatches one webhook event, answering the URL verification challenge
func (a *FeishuAdapter) handleWebhookRequest(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	remoteIP := request.Remote
	if remoteIP == "" {
		remoteIP = "unknown"
	}
	if !a.checkWebhookRateLimit(a.settings.AppID + ":" + a.settings.WebhookPath + ":" + remoteIP) {
		slog.Warn("[Feishu] Webhook rate limit exceeded for " + remoteIP)
		return a.webhookReject(remoteIP, "429", 429, "Too Many Requests", "")
	}
	contentType := pystr.Lower(pystr.Strip(strings.Split(request.Header.Get("Content-Type"), ";")[0]))
	if contentType != "" && contentType != "application/json" {
		slog.Warn(fmt.Sprintf("[Feishu] Webhook rejected: unexpected Content-Type %s from %s", pyrepr.Repr(contentType), remoteIP))
		return a.webhookReject(remoteIP, "415", 415, "Unsupported Media Type", "")
	}
	if request.ContentLength != nil && *request.ContentLength > feishuWebhookMaxBodyBytes {
		slog.Warn(fmt.Sprintf("[Feishu] Webhook body too large (%d bytes) from %s", *request.ContentLength, remoteIP))
		return a.webhookReject(remoteIP, "413", 413, "Request body too large", "")
	}
	readCtx, cancel := context.WithTimeout(ctx, feishuWebhookBodyTimeoutSeconds*time.Second)
	body, err := readLimitedFeishuWebhookBody(readCtx, request, feishuWebhookMaxBodyBytes)
	timedOut := errors.Is(readCtx.Err(), context.DeadlineExceeded)
	cancel()
	switch {
	case errors.Is(err, errPayloadTooLarge):
		slog.Warn("[Feishu] Webhook body exceeds limit from " + remoteIP)
		return a.webhookReject(remoteIP, "413", 413, "Request body too large", "")
	case err != nil && timedOut:
		slog.Warn(fmt.Sprintf("[Feishu] Webhook body read timed out after %ds from %s", feishuWebhookBodyTimeoutSeconds, remoteIP))
		return a.webhookReject(remoteIP, "408", 408, "Request Timeout", "")
	case err != nil:
		return a.webhookReject(remoteIP, "400", 400, "", "failed to read body")
	}
	text, err := pystr.DecodeUTF8(body)
	if err != nil {
		return a.webhookReject(remoteIP, "400", 400, "", "invalid json")
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return a.webhookReject(remoteIP, "400", 400, "", "invalid json")
	}
	payload, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))
	}
	header := dget(payload, "header")
	if !pyrepr.Truthy(header) {
		header = pyjson.NewObject()
	}
	headerObj, ok := header.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(header))
	}
	if token := a.settings.VerificationToken; token != "" {
		incoming := dget(headerObj, "token")
		if !pyrepr.Truthy(incoming) {
			incoming = dget(payload, "token")
		}
		incomingToken := strOr(incoming)
		if incomingToken == "" || !hmac.Equal([]byte(incomingToken), []byte(token)) {
			slog.Warn("[Feishu] Webhook rejected: invalid verification token from " + remoteIP)
			return a.webhookReject(remoteIP, "401-token", 401, "Invalid verification token", "")
		}
	}
	if pyrepr.Equal(dget(payload, "type"), "url_verification") {
		out := pyjson.NewObject()
		out.Set("challenge", dgetDef(payload, "challenge", ""))
		return gateway.WebJSONResponse(out, 200, nil)
	}
	if a.settings.EncryptKey != "" && !a.isWebhookSignatureValid(request, body) {
		slog.Warn("[Feishu] Webhook rejected: invalid signature from " + remoteIP)
		return a.webhookReject(remoteIP, "401-sig", 401, "Invalid signature", "")
	}
	if pyrepr.Truthy(dget(payload, "encrypt")) {
		slog.Error("[Feishu] Encrypted webhook payloads are not supported by Icebath webhook mode")
		return a.webhookReject(remoteIP, "400-encrypted", 400, "", "encrypted webhook payloads are not supported")
	}
	a.clearWebhookAnomaly(remoteIP)
	eventType := strOr(dget(headerObj, "event_type"))
	a.dispatchWebhookEvent(eventType, body, payload)
	ok200 := pyjson.NewObject()
	ok200.Set("code", int64(0))
	ok200.Set("msg", "ok")
	return gateway.WebJSONResponse(ok200, 200, nil)
}

// Routes a verified webhook event to the handler the websocket dispatcher uses for its type
func (a *FeishuAdapter) dispatchWebhookEvent(eventType string, body []byte, payload *pyjson.Object) {
	decode := func(v any) {
		if err := json.Unmarshal(body, v); err != nil {
			slog.Debug("[Feishu] Webhook event payload did not decode fully: " + err.Error())
		}
	}
	switch eventType {
	case "im.message.reaction.created_v1":
		e := &larkim.P2MessageReactionCreatedV1{}
		decode(e)
		a.onReactionEvent(eventType, reactionFromCreated(e))
	case "im.message.reaction.deleted_v1":
		e := &larkim.P2MessageReactionDeletedV1{}
		decode(e)
		a.onReactionEvent(eventType, reactionFromDeleted(e))
	case "im.message.receive_v1":
		e := &larkim.P2MessageReceiveV1{}
		decode(e)
		a.onMessageEvent(e)
	case "im.message.message_read_v1":
		a.onMessageReadEvent()
	case "im.chat.member.bot.added_v1":
		e := &larkim.P2ChatMemberBotAddedV1{}
		decode(e)
		a.onBotMembershipChange(botAddedChatID(e), "added to")
	case "im.chat.member.bot.deleted_v1":
		e := &larkim.P2ChatMemberBotDeletedV1{}
		decode(e)
		a.onBotMembershipChange(botDeletedChatID(e), "removed from")
	case "card.action.trigger":
		a.onCardActionTrigger(parseCardAction(payload))
	case "drive.notice.comment_add_v1":
		a.onDriveCommentEvent(string(body))
	case "vc.bot.meeting_invited_v1":
		a.onMeetingInvitedEvent(string(body))
	default:
		slog.Debug("[Feishu] Ignoring webhook event type: " + firstNonEmpty(eventType, "unknown"))
	}
}

// Timing safe check of the lark signature over timestamp, nonce, encrypt key and body
func (a *FeishuAdapter) isWebhookSignatureValid(request *gateway.WebRequest, body []byte) bool {
	timestamp := request.Header.Get("x-lark-request-timestamp")
	nonce := request.Header.Get("x-lark-request-nonce")
	signature := request.Header.Get("x-lark-signature")
	if timestamp == "" || nonce == "" || signature == "" {
		return false
	}
	sum := sha256.Sum256([]byte(timestamp + nonce + a.settings.EncryptKey + pystr.DecodeUTF8Replace(body)))
	return hmac.Equal([]byte(hex.EncodeToString(sum[:])), []byte(signature))
}

// Sliding window limiter per app, path and address, denying untracked keys when the table is full
func (a *FeishuAdapter) checkWebhookRateLimit(rateKey string) bool {
	now := nowFn()
	a.mu.Lock()
	defer a.mu.Unlock()
	if entry, ok := a.webhookRateCounts[rateKey]; ok && now-entry.windowStart < feishuWebhookRateWindowSeconds {
		if entry.count >= feishuWebhookRateLimitMax {
			return false
		}
		a.webhookRateCounts[rateKey] = rateEntry{count: entry.count + 1, windowStart: entry.windowStart}
		return true
	}
	if len(a.webhookRateCounts) >= feishuWebhookRateMaxKeys {
		for k, e := range a.webhookRateCounts {
			if now-e.windowStart >= feishuWebhookRateWindowSeconds {
				delete(a.webhookRateCounts, k)
			}
		}
		if _, tracked := a.webhookRateCounts[rateKey]; !tracked && len(a.webhookRateCounts) >= feishuWebhookRateMaxKeys {
			slog.Warn(fmt.Sprintf("[Feishu] Webhook rate-limit table at capacity (%d keys) — denying untracked key", feishuWebhookRateMaxKeys))
			return false
		}
	}
	a.webhookRateCounts[rateKey] = rateEntry{count: 1, windowStart: now}
	return true
}
