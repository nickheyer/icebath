package whatsappcloud

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var errPayloadTooLarge = errors.New("payload too large")

// Inner reply of an interactive message, the button or list reply carrying id and title
func interactiveInner(raw map[string]any) (map[string]any, error) {
	inter, err := dictOr(raw["interactive"])
	if err != nil {
		return nil, err
	}
	return dictOr(pyrepr.Or(inter["button_reply"], inter["list_reply"]))
}

// Body text of an inbound message by kind, the caption for media kinds, nil for kinds without one
func bodyByKind(kind string, raw map[string]any) (any, bool, error) {
	field := ""
	switch {
	case kind == "text":
		kind, field = "text", "body"
	case kind == "button":
		kind, field = "button", "text"
	case kind == "interactive":
		inner, err := interactiveInner(raw)
		if err != nil {
			return nil, true, err
		}
		return inner["title"], true, nil
	case inboundMediaKinds[kind]:
		field = "caption"
	default:
		return nil, false, nil
	}
	block, err := dictOr(raw[kind])
	if err != nil {
		return nil, true, err
	}
	return block[field], true, nil
}

// At most maxBytes of the request body, the too large error when more arrives
func readLimitedRequestBody(ctx context.Context, request *gateway.WebRequest, maxBytes int) ([]byte, error) {
	body, err := request.ReadExactly(ctx, maxBytes+1)
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		return nil, errPayloadTooLarge
	}
	return body, nil
}

// Health snapshot of the webhook with its configuration flags and counters
func (a *WhatsAppCloudAdapter) handleHealth(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	a.mu.Lock()
	accepted, duplicates, rejected := a.acceptedCount, a.duplicateCount, a.rejectedSignatureCount
	a.mu.Unlock()
	return gateway.WebJSONResponse(pyjson.ObjectOf("status", "ok", "platform", string(a.Platform), "phone_number_id", a.phoneNumberID,
		"webhook_path", a.webhookPath, "verify_token_configured", a.verifyToken != "", "app_secret_configured", a.appSecret != "",
		"ffmpeg_present", ffmpegPath != "", "accepted", accepted, "duplicates", duplicates, "rejected_signature", rejected), http.StatusOK, nil)
}

// Meta subscription handshake echoing the challenge when the mode and verify token match
func (a *WhatsAppCloudAdapter) handleVerify(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if a.verifyToken == "" {
		return gateway.WebTextResponse(http.StatusServiceUnavailable, "verify_token not configured", "", nil), nil
	}
	q := request.Query()
	if q.Get("hub.mode") != "subscribe" {
		return gateway.WebTextResponse(http.StatusBadRequest, "bad mode", "", nil), nil
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("hub.verify_token")), []byte(a.verifyToken)) != 1 {
		return gateway.WebTextResponse(http.StatusForbidden, "verify_token mismatch", "", nil), nil
	}
	challenge := q.Get("hub.challenge")
	if challenge == "" {
		return gateway.WebTextResponse(http.StatusBadRequest, "missing challenge", "", nil), nil
	}
	return gateway.WebTextResponse(http.StatusOK, challenge, "text/plain", nil), nil
}

// Inbound webhook POST, raw bytes verified by HMAC before JSON parsing and dispatch, acked with 200
func (a *WhatsAppCloudAdapter) handleWebhook(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	raw, err := readLimitedRequestBody(ctx, request, webhookMaxBodyBytes)
	if errors.Is(err, errPayloadTooLarge) {
		return gateway.NewWebResponse(http.StatusRequestEntityTooLarge, nil), nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.NewWebResponse(http.StatusBadRequest, nil), nil
	}
	if a.appSecret == "" {
		slog.Error("[whatsapp_cloud] webhook POST refused: app_secret unset. Set WHATSAPP_CLOUD_APP_SECRET to enable inbound delivery.")
		return gateway.WebTextResponse(http.StatusServiceUnavailable, "app_secret not configured", "", nil), nil
	}
	signatureHeader := request.Header.Get("X-Hub-Signature-256")
	if !a.verifySignature(raw, signatureHeader) {
		a.mu.Lock()
		a.rejectedSignatureCount++
		a.mu.Unlock()
		slog.Warn(fmt.Sprintf("[whatsapp_cloud] rejected webhook: invalid X-Hub-Signature-256 (header=%s, body_len=%d)", pyrepr.Repr(signatureHeader), len(raw)))
		return gateway.NewWebResponse(http.StatusUnauthorized, nil), nil
	}
	payload, err := pyjson.LoadsBytes(raw)
	if err != nil {
		slog.Warn("[whatsapp_cloud] webhook body is not valid JSON")
		payload = nil
	}
	pm, ok := pyjson.Dict(payload)
	if !ok {
		return gateway.NewWebResponse(http.StatusBadRequest, nil), nil
	}
	if err := a.dispatchPayload(ctx, pm); err != nil {
		return nil, err
	}
	return gateway.NewWebResponse(http.StatusOK, nil), nil
}

// Whether the sha256 header is the HMAC of the raw body keyed by the app secret, compared in constant time
func (a *WhatsAppCloudAdapter) verifySignature(rawBody []byte, header string) bool {
	if a.appSecret == "" || header == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	expected := pystr.Strip(header[7:])
	if expected == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(a.appSecret))
	mac.Write(rawBody)
	computed := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(pystr.Lower(computed)), []byte(pystr.Lower(expected))) == 1
}

// True for a new wamid, false counting a duplicate for one already seen
func (a *WhatsAppCloudAdapter) dedupWamid(wamid string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seenWamids.Has(wamid) {
		a.duplicateCount++
		return false
	}
	if wamid != "" {
		gateway.BoundedPut(a.seenWamids, wamid, true, wamidDedupCacheSize)
	}
	return true
}

// Walks entry changes for message ingress, dispatching each message and logging status receipts
func (a *WhatsAppCloudAdapter) dispatchPayload(ctx context.Context, payload map[string]any) error {
	if s, _ := payload["object"].(string); s != "whatsapp_business_account" {
		slog.Debug("[whatsapp_cloud] ignoring non-WABA payload (object=" + pyrepr.Repr(payload["object"]) + ")")
		return nil
	}
	entries, err := pyIter(payload["entry"])
	if err != nil {
		return err
	}
	for _, rawEntry := range entries {
		entry, isDict := pyjson.Dict(rawEntry)
		if !isDict {
			continue
		}
		changes, err := pyIter(entry["changes"])
		if err != nil {
			return err
		}
		for _, rawChange := range changes {
			change, isDict := pyjson.Dict(rawChange)
			if !isDict {
				continue
			}
			if field, _ := change["field"].(string); field != "messages" {
				continue
			}
			value := change["value"]
			if !pyrepr.Truthy(value) {
				value = map[string]any{}
			}
			vm, isDict := pyjson.Dict(value)
			if !isDict {
				continue
			}
			if err := a.dispatchChange(ctx, entry, vm); err != nil {
				return err
			}
		}
	}
	return nil
}

// One messages change of a signed callback, refused when it names a foreign phone number or WABA
func (a *WhatsAppCloudAdapter) dispatchChange(ctx context.Context, entry, value map[string]any) error {
	metadata := value["metadata"]
	if !pyrepr.Truthy(metadata) {
		metadata = map[string]any{}
	}
	if !a.webhookIdentityMatches(entry["id"], metadata) {
		var phone any
		if m, ok := pyjson.Dict(metadata); ok {
			phone = m["phone_number_id"]
		}
		waba := a.wabaID
		if waba == "" {
			waba = "<any>"
		}
		msg := fmt.Sprintf("[whatsapp_cloud] ignoring signed webhook for waba=%s phone_number_id=%s (configured waba=%s phone_number_id=%s)",
			pyrepr.Repr(entry["id"]), pyrepr.Repr(phone), waba, pyrepr.Repr(a.phoneNumberID))
		if pyrepr.Truthy(value["messages"]) {
			slog.Warn(msg)
		} else {
			slog.Debug(msg)
		}
		return nil
	}
	contacts, err := pyIter(value["contacts"])
	if err != nil {
		return err
	}
	contactsByWaid := map[string]string{}
	for _, rawContact := range contacts {
		contact, ok := pyjson.Dict(rawContact)
		if !ok {
			continue
		}
		waID := strField(contact["wa_id"])
		if waID == "" {
			continue
		}
		profile, err := dictOr(contact["profile"])
		if err != nil {
			return err
		}
		contactsByWaid[waID] = strField(profile["name"])
	}
	meta, _ := pyjson.Dict(metadata)
	messages, err := pyIter(value["messages"])
	if err != nil {
		return err
	}
	for _, rawMessage := range messages {
		if m, ok := pyjson.Dict(rawMessage); ok {
			if err := a.ingestMessage(ctx, m, contactsByWaid, meta); err != nil {
				return err
			}
		}
	}
	statuses, err := pyIter(value["statuses"])
	if err != nil {
		return err
	}
	for _, rawStatus := range statuses {
		if status, ok := pyjson.Dict(rawStatus); ok {
			slog.Debug(fmt.Sprintf("[whatsapp_cloud] status %s for %s", pyrepr.Str(status["status"]), pyrepr.Str(status["id"])))
		}
	}
	return nil
}

// Dedups, builds the event and hands it to the message handler, logging rather than raising either failure
func (a *WhatsAppCloudAdapter) ingestMessage(ctx context.Context, raw map[string]any, contactsByWaid map[string]string, metadata map[string]any) error {
	wamid := strField(raw["id"])
	if !a.dedupWamid(wamid) {
		slog.Debug("[whatsapp_cloud] duplicate wamid " + wamid + ", skipping")
		return nil
	}
	event, err := a.buildMessageEventFromCloud(ctx, raw, contactsByWaid, metadata)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Error(fmt.Sprintf("[whatsapp_cloud] failed to build event for wamid %s: %s", wamid, err.Error()))
		return nil
	}
	if event == nil {
		return nil
	}
	a.mu.Lock()
	a.acceptedCount++
	a.mu.Unlock()
	if err := a.handleMessage(ctx, event); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Error(fmt.Sprintf("[whatsapp_cloud] handle_message raised for wamid %s: %s", wamid, err.Error()))
	}
	return nil
}

// Routes a button tap to its resolver, true when claimed and false to fall back to text dispatch
func (a *WhatsAppCloudAdapter) dispatchInteractiveReply(ctx context.Context, raw map[string]any) (bool, error) {
	inner, err := interactiveInner(raw)
	if err != nil {
		return false, err
	}
	buttonID := strField(inner["id"])
	senderID := strField(raw["from"])
	if buttonID == "" {
		return false, nil
	}
	allowed := false
	if senderID != "" {
		if allowed, err = a.IsDMAllowed(ctx, senderID); err != nil {
			return false, err
		}
	}
	if !allowed {
		who := senderID
		if who == "" {
			who = "<unknown>"
		}
		slog.Warn(fmt.Sprintf("[whatsapp_cloud] Rejected unauthorized interactive tap from %s (button_id=%s)", who, pyrepr.Repr(buttonID)))
		return true, nil
	}
	parts := strings.SplitN(buttonID, ":", 3)
	var handler func(ctx context.Context, to string, inner map[string]any, parts []string) (bool, error)
	switch {
	case strings.HasPrefix(buttonID, "cl:"):
		handler = a.handleClarifyTap
	case strings.HasPrefix(buttonID, "appr:"):
		handler = a.handleApprovalTap
	case strings.HasPrefix(buttonID, "sc:"):
		handler = a.handleSlashConfirmTap
	}
	if handler == nil || len(parts) != 3 {
		return false, nil
	}
	to := ""
	if pyrepr.Truthy(raw["from"]) {
		to = pyrepr.Str(raw["from"])
	}
	return handler(ctx, to, inner, parts)
}

// Sends a confirmation text, logging instead of raising a failure
func (a *WhatsAppCloudAdapter) replyBestEffort(ctx context.Context, to, text, failLog string) {
	if _, err := a.Send(ctx, to, text, "", nil); err != nil {
		slog.Error(failLog + ": " + err.Error())
	}
}

// Pops the session key of a tapped prompt, empty for a stale tap or an unknown choice that keeps it live
func (a *WhatsAppCloudAdapter) popTapState(state *pydict.Dict[string, string], key, staleLog, choice string, valid ...string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	sessionKey, _ := state.Get(key)
	state.Delete(key)
	if sessionKey == "" {
		slog.Info(fmt.Sprintf(staleLog, key))
		return ""
	}
	if len(valid) > 0 {
		for _, v := range valid {
			if v == choice {
				return sessionKey
			}
		}
		state.Set(key, sessionKey)
		return ""
	}
	return sessionKey
}

// Restores a session key under its prompt id so later taps or text still resolve it
func (a *WhatsAppCloudAdapter) keepTapState(state *pydict.Dict[string, string], key, sessionKey string) {
	a.mu.Lock()
	state.Set(key, sessionKey)
	a.mu.Unlock()
}

// Clarify tap resolving the waiting clarify, Other flipping it into text capture
func (a *WhatsAppCloudAdapter) handleClarifyTap(ctx context.Context, to string, inner map[string]any, parts []string) (bool, error) {
	clarifyID, choice := parts[1], parts[2]
	sessionKey := a.popTapState(a.clarifyState, clarifyID,
		"[whatsapp_cloud] clarify tap with no matching state (clarify_id=%s) — likely stale; falling back to text", "")
	if sessionKey == "" {
		return false, nil
	}
	if choice == "other" {
		if !tools.MarkClarifyAwaitingText(clarifyID) {
			slog.Info(fmt.Sprintf("[whatsapp_cloud] clarify 'Other' tap but entry missing (clarify_id=%s); falling back to text", clarifyID))
			return false, nil
		}
		a.keepTapState(a.clarifyState, clarifyID, sessionKey)
		a.replyBestEffort(ctx, to, t(ctx, "platform.whatsapp.clarify_type_answer"), "[whatsapp_cloud] clarify other-prompt failed")
		return true, nil
	}
	idx, ok := pyint.Parse(choice)
	if !ok {
		slog.Warn("[whatsapp_cloud] clarify tap had non-int choice: " + pyrepr.Repr(choice))
		a.keepTapState(a.clarifyState, clarifyID, sessionKey)
		return false, nil
	}
	title := new(big.Int).Add(idx, big.NewInt(1)).String()
	if pyrepr.Truthy(inner["title"]) {
		title = pyrepr.Str(inner["title"])
	}
	if !tools.ResolveGatewayClarify(clarifyID, title) {
		slog.Info(fmt.Sprintf("[whatsapp_cloud] clarify resolver reported no waiter (clarify_id=%s) — falling back to text", clarifyID))
		return false, nil
	}
	return true, nil
}

// Approval tap resolving the gateway approval, the expired notice when nothing was waiting
func (a *WhatsAppCloudAdapter) handleApprovalTap(ctx context.Context, to string, inner map[string]any, parts []string) (bool, error) {
	approvalID, choice := parts[1], parts[2]
	sessionKey := a.popTapState(a.execApprovalState, approvalID,
		"[whatsapp_cloud] approval tap with no matching state (approval_id=%s) — likely stale; falling back to text", choice, "approve", "deny")
	if sessionKey == "" {
		return false, nil
	}
	var confirmText string
	if count := tools.ResolveGatewayApproval(sessionKey, choice, false, "", ""); count != 0 {
		key := "platform.whatsapp.denied"
		if choice == "approve" {
			key = "platform.whatsapp.approved"
		}
		confirmText = t(ctx, key)
	} else {
		slog.Info(fmt.Sprintf("[whatsapp_cloud] approval resolver reported no waiter (session_key=%s) — likely already resolved", sessionKey))
		confirmText = t(ctx, "platform.whatsapp.approval_expired")
	}
	a.replyBestEffort(ctx, to, confirmText, "[whatsapp_cloud] approval confirm failed")
	return true, nil
}

// Slash confirm tap resolving the pending confirmation and replying its result text
func (a *WhatsAppCloudAdapter) handleSlashConfirmTap(ctx context.Context, to string, inner map[string]any, parts []string) (bool, error) {
	choice, confirmID := parts[1], parts[2]
	sessionKey := a.popTapState(a.slashConfirmState, confirmID,
		"[whatsapp_cloud] slash_confirm tap with no matching state (confirm_id=%s) — likely stale", choice, "once", "always", "cancel")
	if sessionKey == "" {
		return false, nil
	}
	if gateway.SlashConfirmResolve == nil {
		slog.Warn("[whatsapp_cloud] slash_confirm resolver unavailable")
		return false, nil
	}
	if resultText := gateway.SlashConfirmResolve(ctx, sessionKey, confirmID, choice); resultText != "" {
		a.replyBestEffort(ctx, to, resultText, "[whatsapp_cloud] slash_confirm reply failed")
	}
	return true, nil
}

// Downloads inbound media by id, returning its cached path, mime and the body with a document label
func (a *WhatsAppCloudAdapter) collectInboundMedia(ctx context.Context, kind string, raw map[string]any, body string) ([]string, []string, string, error) {
	inner, err := dictOr(raw[kind])
	if err != nil {
		return nil, nil, body, err
	}
	mediaID, inboundMime := strField(inner["id"]), strField(inner["mime_type"])
	if mediaID == "" {
		return []string{}, []string{}, body, nil
	}
	localPath, dlMime, err := a.downloadMediaToCache(ctx, mediaID, extForMime(inboundMime))
	if err != nil {
		return nil, nil, body, err
	}
	if localPath != "" {
		slog.Info(fmt.Sprintf("[whatsapp_cloud] cached inbound %s media: %s", kind, localPath))
	} else {
		slog.Warn(fmt.Sprintf("[whatsapp_cloud] failed to download inbound %s (id=%s) — agent will see message metadata but not the binary", kind, mediaID))
	}
	fname := ""
	if kind == "document" {
		fname = strField(inner["filename"])
	}
	if body == "" && fname != "" {
		body = "[Document: " + fname + "]"
	}
	if localPath == "" {
		return []string{}, []string{}, body, nil
	}
	mime := dlMime
	if mime == "" {
		mime = inboundMime
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	return []string{localPath}, []string{mime}, body, nil
}

// Prepends text readable documents up to 100KB to the body, flagging each injected entry
func injectDocumentText(mediaURLs []string, body string) (string, []bool) {
	inlined := make([]bool, len(mediaURLs))
	for i, doc := range mediaURLs {
		if _, suffix := pypath.Splitext(filepath.Base(doc)); !textInjectExts[pystr.Lower(suffix)] {
			continue
		}
		info, err := os.Stat(doc)
		if err != nil {
			slog.Error(fmt.Sprintf("[whatsapp_cloud] failed to read document text: %s: %s", doc, pypath.OSErrorText(err)))
			continue
		}
		if size := info.Size(); size > maxTextInjectBytes {
			slog.Info(fmt.Sprintf("[whatsapp_cloud] skipping text injection for %s (%d bytes > %d)", doc, size, maxTextInjectBytes))
			continue
		}
		data, err := os.ReadFile(doc)
		if err != nil {
			slog.Error(fmt.Sprintf("[whatsapp_cloud] failed to read document text: %s: %s", doc, pypath.OSErrorText(err)))
			continue
		}
		injection := "[Content of " + filepath.Base(doc) + "]:\n" + pystr.TranslateNewlines(pystr.DecodeUTF8SigReplace(data))
		if body != "" {
			body = injection + "\n\n" + body
		} else {
			body = injection
		}
		inlined[i] = true
	}
	return body, inlined
}

// Cloud API message converted to a gateway event, nil when contentless, a claimed tap or gated out
func (a *WhatsAppCloudAdapter) buildMessageEventFromCloud(ctx context.Context, raw map[string]any, contactsByWaid map[string]string, metadata map[string]any) (*gateway.MessageEvent, error) {
	kind := "text"
	if pyrepr.Truthy(raw["type"]) {
		kind = pystr.Lower(pyrepr.Str(raw["type"]))
	}
	if contentlessKinds[kind] {
		slog.Debug(fmt.Sprintf("[whatsapp_cloud] skipping contentless %s envelope (wamid=%s)", kind, pyrepr.Str(raw["id"])))
		return nil, nil
	}
	if kind == "interactive" {
		claimed, err := a.dispatchInteractiveReply(ctx, raw)
		if err != nil || claimed {
			return nil, err
		}
	}
	body := ""
	if extracted, known, err := bodyByKind(kind, raw); err != nil {
		return nil, err
	} else if known && pyrepr.Truthy(extracted) {
		body = pyrepr.Str(extracted)
	}
	chatID := strField(raw["from"])
	senderID := chatID
	senderName := contactsByWaid[senderID]
	if pyrepr.Truthy(raw["chat"]) {
		slog.Warn(fmt.Sprintf("[whatsapp_cloud] received group-shaped message (chat=%s, wamid=%s) — group support is not yet "+
			"implemented; dropping. Use the Baileys whatsapp adapter for group chats.", pyrepr.Str(raw["chat"]), pyrepr.Str(raw["id"])))
		return nil, nil
	}
	process, err := a.ShouldProcessMessage(ctx, map[string]any{"chatId": chatID, "senderId": senderID, "isGroup": false, "body": body})
	if err != nil || !process {
		return nil, err
	}
	mediaURLs, mediaTypes, inlined := []string{}, []string{}, []bool{}
	if inboundMediaKinds[kind] {
		if mediaURLs, mediaTypes, body, err = a.collectInboundMedia(ctx, kind, raw, body); err != nil {
			return nil, err
		}
		if kind == "document" && len(mediaURLs) > 0 {
			body, inlined = injectDocumentText(mediaURLs, body)
		}
	}
	replyContext, err := dictOr(raw["context"])
	if err != nil {
		return nil, err
	}
	replyToID := strField(replyContext["id"])
	var replyToText *string
	if replyToID != "" {
		if text, ok := gateway.RichSentLookup(ctx, chatID, replyToID); ok {
			replyToText = &text
		}
	}
	quotedFrom := strField(replyContext["from"])
	ourNumber := strField(metadata["display_phone_number"])
	replyToIsOwn := replyToID != "" && quotedFrom != "" && ourNumber != "" && quotedFrom == ourNumber
	wamid := ""
	if pyrepr.Truthy(raw["id"]) {
		wamid = pyrepr.Str(raw["id"])
	}
	if wamid != "" && chatID != "" {
		a.mu.Lock()
		gateway.BoundedPut(a.lastInboundWamidByChat, chatID, wamid, interactiveStateCacheSize)
		a.mu.Unlock()
		if body != "" {
			gateway.RichSentRecord(ctx, chatID, wamid, body)
		}
		if inboundMediaKinds[kind] && len(mediaURLs) > 0 {
			pairs := make([][2]string, len(mediaURLs))
			for i := range mediaURLs {
				pairs[i] = [2]string{mediaURLs[i], mediaTypes[i]}
			}
			gateway.RichSentRecordMedia(ctx, chatID, wamid, pairs)
		}
	}
	if replyToID != "" {
		for _, pair := range gateway.RichSentLookupMedia(ctx, chatID, replyToID) {
			if !containsString(mediaURLs, pair[0]) {
				mediaURLs = append(mediaURLs, pair[0])
				mediaTypes = append(mediaTypes, pair[1])
			}
		}
	}
	chatName := senderName
	if chatName == "" {
		chatName = chatID
	}
	fields := gateway.SourceFields{ChatName: &chatName, ChatType: "dm", UserID: &senderID}
	if senderName != "" {
		fields.UserName = &senderName
	}
	msgType, ok := messageTypeByKind[kind]
	if !ok {
		msgType = gateway.MessageTypeText
	}
	event := gateway.NewMessageEvent(body)
	event.MessageType = msgType
	event.Source = a.BuildSource(ctx, chatID, fields)
	event.RawMessage = raw
	if wamid != "" {
		event.MessageID = &wamid
	}
	if replyToID != "" {
		event.ReplyToMessageID = &replyToID
	}
	event.ReplyToText = replyToText
	event.ReplyToIsOwnMessage = replyToIsOwn
	event.MediaURLs, event.MediaTypes = mediaURLs, mediaTypes
	event.MediaTextInlined = make([]*bool, len(inlined))
	for i := range inlined {
		v := inlined[i]
		event.MediaTextInlined[i] = &v
	}
	return event, nil
}

// Whether the list holds the value
func containsString(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}
