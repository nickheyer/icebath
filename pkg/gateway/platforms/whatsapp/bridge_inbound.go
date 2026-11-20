package whatsapp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Set sub message of a proto message by its JSON field name, nil when unset
func pfield(m protoreflect.Message, name string) protoreflect.Message {
	if m == nil || !m.IsValid() {
		return nil
	}
	fd := m.Descriptor().Fields().ByJSONName(name)
	if fd == nil || fd.Kind() != protoreflect.MessageKind || fd.IsList() || !m.Has(fd) {
		return nil
	}
	return m.Get(fd).Message()
}

// Scalar field value of a proto message by JSON name, false when unset
func pscalar(m protoreflect.Message, name string) (protoreflect.Value, bool) {
	if m == nil || !m.IsValid() {
		return protoreflect.Value{}, false
	}
	fd := m.Descriptor().Fields().ByJSONName(name)
	if fd == nil || fd.IsList() || !m.Has(fd) {
		return protoreflect.Value{}, false
	}
	return m.Get(fd), true
}

// String field value, empty when unset
func pstr(m protoreflect.Message, name string) string {
	if v, ok := pscalar(m, name); ok {
		if s, isStr := v.Interface().(string); isStr {
			return s
		}
	}
	return ""
}

// Bool field value, false when unset
func pbool(m protoreflect.Message, name string) bool {
	if v, ok := pscalar(m, name); ok {
		b, _ := v.Interface().(bool)
		return b
	}
	return false
}

// Number field value, nil when unset
func pnum(m protoreflect.Message, name string) *float64 {
	v, ok := pscalar(m, name)
	if !ok {
		return nil
	}
	var f float64
	switch x := v.Interface().(type) {
	case float64:
		f = x
	case float32:
		f = float64(x)
	case int32:
		f = float64(x)
	case int64:
		f = float64(x)
	case uint32:
		f = float64(x)
	case uint64:
		f = float64(x)
	default:
		return nil
	}
	return &f
}

// Repeated field of a proto message by JSON name
func plist(m protoreflect.Message, name string) protoreflect.List {
	if m == nil || !m.IsValid() {
		return nil
	}
	fd := m.Descriptor().Fields().ByJSONName(name)
	if fd == nil || !fd.IsList() || !m.Has(fd) {
		return nil
	}
	return m.Get(fd).List()
}

// Message content with ephemeral, view once and document caption envelopes peeled, at most eight deep
func unwrapMessageEnvelopes(content protoreflect.Message) protoreflect.Message {
	cur := content
	for i := 0; i < 8 && cur != nil; i++ {
		var next protoreflect.Message
		for _, env := range []string{"ephemeralMessage", "viewOnceMessage", "viewOnceMessageV2", "documentWithCaptionMessage"} {
			if wrapper := pfield(cur, env); wrapper != nil {
				next = pfield(wrapper, "message")
				if next == nil {
					next = (&waE2E.Message{}).ProtoReflect()
				}
				break
			}
		}
		if next == nil {
			break
		}
		cur = next
	}
	return cur
}

// The payload message of an inbound message, template, buttons and list shapes resolved for unenveloped messages
func getMessageContent(msg *waE2E.Message) protoreflect.Message {
	if msg == nil {
		msg = &waE2E.Message{}
	}
	raw := msg.ProtoReflect()
	content := unwrapMessageEnvelopes(raw)
	if content != raw {
		return content
	}
	if tpl := pfield(content, "templateMessage"); tpl != nil {
		if hydrated := pfield(tpl, "hydratedTemplate"); hydrated != nil {
			return hydrated
		}
	}
	if buttons := pfield(content, "buttonsMessage"); buttons != nil {
		return buttons
	}
	if list := pfield(content, "listMessage"); list != nil {
		return list
	}
	return content
}

// Context info of the first set field that carries one, in field order
func getContextInfo(content protoreflect.Message) protoreflect.Message {
	if content == nil {
		return nil
	}
	fields := content.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() || !content.Has(fd) {
			continue
		}
		if ci := pfield(content.Get(fd).Message(), "contextInfo"); ci != nil {
			return ci
		}
	}
	return nil
}

// Option names of a poll creation message
func pollOptions(poll protoreflect.Message) []string {
	out := []string{}
	opts := plist(poll, "options")
	for i := 0; opts != nil && i < opts.Len(); i++ {
		o := opts.Get(i).Message()
		name := pstr(o, "optionName")
		if name == "" {
			name = pstr(o, "name")
		}
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Location text of a location proto message
func locationText(loc protoreflect.Message, isLive bool) string {
	lat, lng := pnum(loc, "degreesLatitude"), pnum(loc, "degreesLongitude")
	return formatLocationText(pstr(loc, "name"), pstr(loc, "address"), lat, lng, isLive)
}

// Poll text of a poll creation proto message
func pollText(poll protoreflect.Message) string {
	q := pstr(poll, "name")
	if q == "" {
		q = pstr(poll, "title")
	}
	return formatPollText(q, pollOptions(poll))
}

// Readable text of a quoted message, empty when it has none
func textFromQuotedMessage(quoted protoreflect.Message) string {
	content := unwrapMessageEnvelopes(quoted)
	if content == nil {
		return ""
	}
	if s := pstr(content, "conversation"); s != "" {
		return s
	}
	for _, f := range [][2]string{{"extendedTextMessage", "text"}, {"imageMessage", "caption"}, {"videoMessage", "caption"}, {"documentMessage", "caption"}} {
		if s := pstr(pfield(content, f[0]), f[1]); s != "" {
			return s
		}
	}
	if name := pstr(pfield(content, "documentMessage"), "fileName"); name != "" {
		return "[Document: " + name + "]"
	}
	if loc := pfield(content, "locationMessage"); loc != nil {
		return locationText(loc, false)
	}
	if c := pfield(content, "contactMessage"); c != nil {
		return formatContactText(pstr(c, "displayName"), pstr(c, "vcard"))
	}
	if poll := pfield(content, "pollCreationMessage"); poll != nil {
		return pollText(poll)
	}
	return ""
}

// JSON object of a proto message with its camelCase field names
func protoObject(m protoreflect.Message) any {
	raw, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(m.Interface())
	if err != nil {
		return pyjson.NewObject()
	}
	v, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return pyjson.NewObject()
	}
	return v
}

// Media bytes of an inbound message, re-requested from the phone when the CDN copy expired
func (b *bridgeHost) download(ctx context.Context, client *whatsmeow.Client, info types.MessageInfo, media whatsmeow.DownloadableMessage) ([]byte, error) {
	data, err := client.Download(ctx, media)
	if err == nil || !(errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) || errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410)) {
		return data, err
	}
	ch := make(chan *events.MediaRetry, 1)
	b.mu.Lock()
	b.mediaRetries[info.ID] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.mediaRetries, info.ID)
		b.mu.Unlock()
	}()
	if err := client.SendMediaRetryReceipt(ctx, &info, media.GetMediaKey()); err != nil {
		return nil, err
	}
	var evt *events.MediaRetry
	select {
	case evt = <-ch:
	case <-time.After(bridgeMediaRetryLimit):
		return nil, errors.New("media reupload request timed out")
	case <-b.done:
		return nil, errors.New("bridge stopped")
	}
	notif, err := whatsmeow.DecryptMediaRetryNotification(evt, media.GetMediaKey())
	if err != nil {
		return nil, err
	}
	if notif.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS {
		return nil, fmt.Errorf("media reupload failed: %s", notif.GetResult().String())
	}
	return client.DownloadMediaWithPath(ctx, notif.GetDirectPath(), media.GetFileEncSHA256(), media.GetFileSHA256(),
		media.GetMediaKey(), whatsmeow.GetMediaType(media), "", false)
}

// Bridge event fields of one inbound message the extraction produced
type bridgeEvent struct {
	obj             *pyjson.Object
	body            string
	hasMedia        bool
	mediaType       string
	mediaURLs       []any
	quotedMediaURLs []any
}

// Normalised bridge event of an inbound message with its media downloaded into the caches
func (b *bridgeHost) extractBridgeEvent(ctx context.Context, client *whatsmeow.Client, evt *events.Message, chatID, senderID, senderNumber string, botIDs []any, isGroup bool) bridgeEvent {
	content := getMessageContent(evt.Message)
	ci := getContextInfo(content)
	mentioned := []any{}
	seen := map[string]bool{}
	mj := plist(ci, "mentionedJid")
	for i := 0; mj != nil && i < mj.Len(); i++ {
		if id := bridgeNormalizeID(mj.Get(i).String()); id != "" && !seen[id] {
			seen[id] = true
			mentioned = append(mentioned, id)
		}
	}
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	quotedMessageID := pstr(ci, "stanzaId")
	quotedParticipant := bridgeNormalizeID(pstr(ci, "participant"))
	quotedRemoteJid := bridgeNormalizeID(pstr(ci, "remoteJid"))
	quotedMsg := pfield(ci, "quotedMessage")
	hasQuoted := quotedMsg != nil
	quotedText := textFromQuotedMessage(quotedMsg)
	quotedMediaURLs := []any{}
	quotedMediaType := ""
	if quotedMessageID != "" {
		lookupChat := quotedRemoteJid
		if lookupChat == "" {
			lookupChat = chatID
		}
		if original, ok := b.quotedMedia.get(lookupChat+":"+quotedMessageID, false); ok {
			urls, _ := original.GetOr("mediaUrls", nil).([]any)
			kind, _ := original.GetOr("mediaType", "").(string)
			if hasMedia, _ := original.GetOr("hasMedia", false).(bool); hasMedia && len(urls) > 0 {
				quotedMediaURLs = urls
				quotedMediaType = kind
				if quotedText == "" {
					quotedText = map[string]string{"image": "sent an image", "video": "sent a video", "gif": "sent a GIF",
						"audio": "sent an audio message", "ptt": "sent a voice message", "document": "sent a document",
						"sticker": "sent a sticker"}[kind]
					if quotedText == "" {
						quotedText = "sent media"
					}
				}
			} else if body, _ := original.GetOr("body", "").(string); body != "" && quotedText == "" {
				quotedText = body
			}
		}
	}
	body, mediaType, mime, fileName, nativeType := "", "", "", "", ""
	hasMedia := false
	mediaURLs := []any{}
	native := pyjson.NewObject()
	failures := []string{}
	saveMedia := func(item protoreflect.Message, dir, prefix, fallbackExt, name, kind string) {
		media, ok := item.Interface().(whatsmeow.DownloadableMessage)
		if !ok {
			return
		}
		buf, err := b.download(ctx, client, evt.Info, media)
		if err == nil {
			var saved string
			if saved, err = writeMediaFile(buf, dir, prefix, mediaExtForMime(pstr(item, "mimetype"), fallbackExt), name); err == nil && saved != "" {
				mediaURLs = append(mediaURLs, saved)
				return
			}
		}
		if kind == "" {
			kind = "media"
		}
		failures = append(failures, kind)
		b.console.log("[bridge] failed to download inbound %s: %s", kind, err.Error())
	}
	or := func(s, def string) string {
		if s == "" {
			return def
		}
		return s
	}
	if s := pstr(content, "conversation"); s != "" {
		body, nativeType = s, "conversation"
	} else if s := pstr(pfield(content, "extendedTextMessage"), "text"); s != "" {
		body, nativeType = s, "extendedTextMessage"
	} else if item := pfield(content, "imageMessage"); item != nil {
		body, hasMedia, mediaType, nativeType = pstr(item, "caption"), true, "image", "imageMessage"
		mime = or(pstr(item, "mimetype"), "image/jpeg")
		saveMedia(item, b.cfg.imageCacheDir, "img", ".jpg", "", "image")
	} else if item := pfield(content, "videoMessage"); item != nil {
		body, hasMedia, nativeType = pstr(item, "caption"), true, "videoMessage"
		mediaType = "video"
		if pbool(item, "gifPlayback") {
			mediaType = "gif"
		}
		mime = or(pstr(item, "mimetype"), "video/mp4")
		native.Set("video", jsonObj("gifPlayback", pbool(item, "gifPlayback")))
		saveMedia(item, b.cfg.documentCacheDir, "vid", ".mp4", "", mediaType)
	} else if item := pfield(content, "audioMessage"); item != nil {
		hasMedia, nativeType = true, "audioMessage"
		mediaType = "audio"
		if pbool(item, "ptt") {
			mediaType = "ptt"
		}
		mime = or(pstr(item, "mimetype"), "audio/ogg")
		native.Set("audio", jsonObj("ptt", mediaType == "ptt"))
		saveMedia(item, b.cfg.audioCacheDir, "aud", ".ogg", "", "audio")
	} else if item := pfield(content, "documentMessage"); item != nil {
		body, hasMedia, mediaType, nativeType = pstr(item, "caption"), true, "document", "documentMessage"
		mime = or(pstr(item, "mimetype"), "application/octet-stream")
		fileName = or(pstr(item, "fileName"), "document")
		saveMedia(item, b.cfg.documentCacheDir, "doc", ".bin", fileName, "document")
	} else if item := pfield(content, "stickerMessage"); item != nil {
		hasMedia, mediaType, nativeType = true, "sticker", "stickerMessage"
		mime = or(pstr(item, "mimetype"), "image/webp")
		body = "[Sticker]"
		native.Set("sticker", jsonObj("animated", pbool(item, "isAnimated"), "mimetype", mime))
		saveMedia(item, b.cfg.imageCacheDir, "sticker", ".webp", "", "sticker")
	} else if item, isLive := pfield(content, "liveLocationMessage"), true; item != nil || pfield(content, "locationMessage") != nil {
		if item == nil {
			item, isLive = pfield(content, "locationMessage"), false
		}
		mediaType, nativeType = "location", "locationMessage"
		if isLive {
			mediaType, nativeType = "live_location", "liveLocationMessage"
		}
		body = locationText(item, isLive)
		numOrNil := func(p *float64) any {
			if p == nil {
				return nil
			}
			return *p
		}
		native.Set("location", jsonObj("name", pstr(item, "name"), "address", pstr(item, "address"),
			"latitude", numOrNil(pnum(item, "degreesLatitude")), "longitude", numOrNil(pnum(item, "degreesLongitude")), "isLive", isLive))
	} else if item := pfield(content, "contactMessage"); item != nil {
		mediaType, nativeType = "contact", "contactMessage"
		body = formatContactText(pstr(item, "displayName"), pstr(item, "vcard"))
		native.Set("contact", jsonObj("displayName", pstr(item, "displayName"), "vcard", pstr(item, "vcard")))
	} else if item := pfield(content, "contactsArrayMessage"); item != nil {
		mediaType, nativeType = "contacts", "contactsArrayMessage"
		contacts := plist(item, "contacts")
		names, meta := []string{}, []any{}
		for i := 0; contacts != nil && i < contacts.Len(); i++ {
			c := contacts.Get(i).Message()
			names = append(names, pstr(c, "displayName"))
			meta = append(meta, jsonObj("displayName", pstr(c, "displayName"), "vcard", pstr(c, "vcard")))
		}
		count := 0
		if contacts != nil {
			count = contacts.Len()
		}
		body = formatContactsText(names, count)
		native.Set("contacts", meta)
	} else if item := pfield(content, "reactionMessage"); item != nil {
		mediaType, nativeType = "reaction", "reactionMessage"
		key := pfield(item, "key")
		body = formatReactionText(pstr(item, "text"), pstr(key, "id"))
		native.Set("reaction", jsonObj("text", pstr(item, "text"), "messageId", pstr(key, "id"),
			"remoteJid", bridgeNormalizeID(pstr(key, "remoteJid")), "participant", bridgeNormalizeID(pstr(key, "participant"))))
	} else if poll, which := firstPoll(content); poll != nil {
		mediaType, nativeType = "poll", which
		body = pollText(poll)
		q := pstr(poll, "name")
		if q == "" {
			q = pstr(poll, "title")
		}
		count := 1
		if n := pnum(poll, "selectableOptionsCount"); n != nil && *n != 0 {
			count = int(*n)
		}
		native.Set("poll", jsonObj("question", q, "options", stringsAny(pollOptions(poll)), "selectableCount", count))
	} else if item := pfield(content, "pollUpdateMessage"); item != nil {
		mediaType, nativeType = "poll_update", "pollUpdateMessage"
		body = formatPollUpdateText(pstr(pfield(item, "pollCreationMessageKey"), "id"))
		native.Set("pollUpdate", protoObject(item))
	}
	body = appendMediaFailureNote(body, failures)
	if hasMedia && body == "" {
		body = "[" + mediaType + " received]"
	}
	senderName := evt.Info.PushName
	if senderName == "" {
		senderName = senderNumber
	}
	chatName := senderName
	if isGroup {
		chatName, _, _ = strings.Cut(chatID, "@")
	}
	participant := senderID
	if isGroup {
		participant = evt.Info.Sender.ToNonAD().String()
	}
	obj := jsonObj("messageId", evt.Info.ID, "chatId", chatID, "senderId", senderID, "senderName", senderName,
		"chatName", chatName, "isGroup", isGroup, "body", body, "hasMedia", hasMedia, "mediaType", mediaType, "mime", mime,
		"fileName", fileName, "nativeType", nativeType, "nativeMetadata", native, "mediaUrls", mediaURLs,
		"mentionedIds", mentioned, "quotedMessageId", nullable(quotedMessageID), "quotedParticipant", nullable(quotedParticipant),
		"quotedRemoteJid", nullable(quotedRemoteJid), "quotedText", quotedText, "quotedMediaUrls", quotedMediaURLs,
		"quotedMediaType", quotedMediaType, "hasQuotedMessage", hasQuoted, "botIds", botIDs,
		"readReceiptKey", jsonObj("remoteJid", evt.Info.Chat.String(), "id", evt.Info.ID, "participant", participant, "fromMe", evt.Info.IsFromMe),
		"timestamp", evt.Info.Timestamp.Unix())
	return bridgeEvent{obj: obj, body: body, hasMedia: hasMedia, mediaType: mediaType, mediaURLs: mediaURLs, quotedMediaURLs: quotedMediaURLs}
}

// First set poll creation field and its native type name
func firstPoll(content protoreflect.Message) (protoreflect.Message, string) {
	for _, name := range []string{"pollCreationMessage", "pollCreationMessageV2", "pollCreationMessageV3"} {
		if p := pfield(content, name); p != nil {
			return p, name
		}
	}
	return nil, ""
}

// Strings as a JSON array
func stringsAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// Processes inbound messages one at a time in arrival order
func (b *bridgeHost) inboundWorker() {
	for {
		select {
		case evt := <-b.inbound:
			b.handleUpsert(evt)
		case <-b.done:
			return
		}
	}
}

// Mode, ownership and intake gates of one inbound message, then the poll or message queueing
func (b *bridgeHost) handleUpsert(evt *events.Message) {
	client, _ := b.socket()
	if client == nil || evt.Message == nil {
		return
	}
	botIDs := []any{}
	seenBot := map[string]bool{}
	ownID, ownLID := "", ""
	if client.Store.ID != nil {
		ownID = client.Store.ID.String()
	}
	if !client.Store.LID.IsEmpty() {
		ownLID = client.Store.LID.String()
	}
	for _, id := range []string{bridgeNormalizeID(ownID), bridgeNormalizeID(ownLID)} {
		if id != "" && !seenBot[id] {
			seenBot[id] = true
			botIDs = append(botIDs, id)
		}
	}
	info := evt.Info
	chatID := info.Chat.String()
	isGroup := strings.HasSuffix(chatID, "@g.us")
	senderID := chatID
	if isGroup {
		senderID = info.Sender.ToNonAD().String()
	}
	senderAltID := ""
	if !info.SenderAlt.IsEmpty() {
		senderAltID = bridgeNormalizeID(info.SenderAlt.ToNonAD().String())
	}
	resolvedSenderID := senderID
	if strings.HasSuffix(senderAltID, "@s.whatsapp.net") {
		resolvedSenderID = senderAltID
	}
	senderNumber, _, _ := strings.Cut(resolvedSenderID, "@")
	msgKeys := messageKeys(evt.Message)
	b.emitDebugEvent("stage", "upsert", "type", "notify", "fromMe", info.IsFromMe, "chatId", redactWhatsAppID(chatID),
		"senderId", redactWhatsAppID(senderID), "messageKeys", msgKeys)
	fromOwner := false
	if info.IsFromMe {
		if isGroup || strings.Contains(chatID, "status") {
			reason := "from_me_status"
			if isGroup {
				reason = "from_me_group"
			}
			b.emitDebugEvent("stage", "ignored", "reason", reason, "chatId", redactWhatsAppID(chatID))
			return
		}
		if b.cfg.mode == "bot" {
			decision := classifyOwnerMessageGate(true, b.cfg.forwardOwner, b.recentlySent,
				func(id string) bool { return matchesAllowedUser(id, b.cfg.allowedUsers, b.cfg.session) }, info.ID, chatID)
			switch decision {
			case "drop_echo", "drop_disabled":
				return
			case "drop_allowlist":
				b.console.jsonLine(jsonObj("event", "ignored", "reason", "allowlist_mismatch_owner_chat", "chatId", chatID, "senderId", senderID))
				return
			}
			fromOwner = true
		} else {
			myNumber := identifierUser(ownID)
			myLid := identifierUser(ownLID)
			chatNumber, _, _ := strings.Cut(chatID, "@")
			isSelfChat := (myNumber != "" && chatNumber == myNumber) || (myLid != "" && chatNumber == myLid)
			b.emitDebugEvent("stage", "self_chat_check", "matched", isSelfChat, "chatId", redactWhatsAppID(chatID),
				"accountId", redactWhatsAppID(ownID), "accountLid", redactWhatsAppID(ownLID))
			if !isSelfChat {
				b.emitDebugEvent("stage", "ignored", "reason", "self_chat_mismatch", "chatId", redactWhatsAppID(chatID), "senderId", redactWhatsAppID(senderID))
				return
			}
		}
	}
	if !info.IsFromMe {
		if b.cfg.mode == "self-chat" {
			b.console.jsonLine(jsonObj("event", "ignored", "reason", "self_chat_mode_rejects_non_self", "chatId", chatID, "senderId", senderID))
			return
		}
		var intakeAllowed bool
		if isGroup {
			intakeAllowed = matchesInboundWhatsAppGroup(chatID, b.cfg.groupPolicy, b.cfg.groupAllowedUsers, b.cfg.session)
		} else {
			intakeAllowed = b.cfg.dmPolicy == "pairing" || matchesAllowedSender(senderID, senderAltID, b.cfg.allowedUsers, b.cfg.session)
		}
		if !intakeAllowed {
			reason := "allowlist_mismatch"
			if isGroup {
				reason = "group_policy_rejected"
			}
			b.console.jsonLine(jsonObj("event", "ignored", "reason", reason, "chatId", chatID, "senderId", senderID, "senderAltId", senderAltID))
			return
		}
	}
	content := getMessageContent(evt.Message)
	if update := pfield(content, "pollUpdateMessage"); update != nil {
		b.handlePollUpsert(client, evt, update, chatID, senderID)
		return
	}
	event := b.extractBridgeEvent(b.ctx, client, evt, chatID, resolvedSenderID, senderNumber, botIDs, isGroup)
	event.obj.Set("fromOwner", fromOwner)
	if info.IsFromMe && ((b.cfg.replyPrefix != "" && strings.HasPrefix(event.body, b.cfg.replyPrefix)) || b.recentlySent.has(info.ID)) {
		b.emitDebugEvent("stage", "ignored", "reason", "agent_echo", "chatId", redactWhatsAppID(chatID), "messageId", info.ID)
		return
	}
	if event.body == "" && !event.hasMedia && len(event.quotedMediaURLs) == 0 {
		b.emitDebugEvent("stage", "ignored", "reason", "empty", "chatId", redactWhatsAppID(chatID), "messageKeys", msgKeys)
		return
	}
	b.messageStore.put(info.ID, &storedMessage{info: info, message: evt.Message})
	b.quotedMedia.put(chatID+":"+info.ID, jsonObj("body", event.body, "hasMedia", event.hasMedia, "mediaType", event.mediaType, "mediaUrls", event.mediaURLs))
	n := b.pushMessage(event.obj)
	b.emitDebugEvent("stage", "queued", "chatId", redactWhatsAppID(chatID), "senderId", redactWhatsAppID(senderID),
		"fromOwner", fromOwner, "bodyLength", jsLen(event.body), "hasMedia", event.hasMedia, "mediaType", event.mediaType, "queueLength", n)
}

// User part of an id with its device dropped
func identifierUser(id string) string {
	if i := strings.Index(id, ":"); i >= 0 {
		if j := strings.LastIndex(id, "@"); j > i {
			id = id[:i] + id[j:]
		}
	}
	user, _, _ := strings.Cut(id, "@")
	return user
}

// JSON names of the set top level fields of a message
func messageKeys(msg *waE2E.Message) []any {
	out := []any{}
	m := msg.ProtoReflect()
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		if fd := fields.Get(i); m.Has(fd) {
			out = append(out, fd.JSONName())
		}
	}
	return out
}

// Decrypts a vote on a poll the bridge sent, aggregates it and queues it as a poll update event
func (b *bridgeHost) handlePollUpsert(client *whatsmeow.Client, evt *events.Message, update protoreflect.Message, chatID, senderID string) {
	pollID, pollRemote, pollParticipant := evt.Info.ID, chatID, senderID
	if key := pfield(update, "pollCreationMessageKey"); key != nil {
		pollID, pollRemote, pollParticipant = pstr(key, "id"), pstr(key, "remoteJid"), pstr(key, "participant")
	}
	pollCreation, found := b.messageStore.get(pollID, true)
	aggregation := []any{}
	updateKeys := []any{}
	hasVote := false
	var selected []string
	if found {
		vote, err := client.DecryptPollVote(b.ctx, evt)
		if err != nil {
			b.console.log("[bridge] failed to aggregate poll upsert: %s", err.Error())
		} else {
			updateKeys = stringsAny([]string{"pollUpdateMessageKey", "vote", "senderTimestampMs"})
			hasVote = true
			voter := senderID
			if evt.Info.IsFromMe && client.Store.ID != nil {
				voter = client.Store.ID.ToNonAD().String()
			}
			aggregation, selected = aggregateVote(pollCreation.message, vote.GetSelectedOptions(), voter)
		}
	}
	if !hasVote {
		m := update
		fields := m.Descriptor().Fields()
		for i := 0; i < fields.Len(); i++ {
			if fd := fields.Get(i); m.Has(fd) {
				updateKeys = append(updateKeys, fd.JSONName())
			}
		}
		hasVote = pfield(update, "vote") != nil
	}
	if selected == nil {
		selected = []string{}
	}
	summary := []any{}
	for _, a := range aggregation {
		o := a.(*pyjson.Object)
		voters, _ := o.GetOr("voters", nil).([]any)
		summary = append(summary, jsonObj("name", o.GetOr("name", ""), "voterCount", len(voters)))
	}
	b.console.jsonLine(jsonObj("event", "poll_update_decode", "sourcePath", "messages.upsert", "pollId", pollID,
		"pollCreationFound", found, "updateKeys", updateKeys, "hasVote", hasVote, "selectedOptionsLength", len(selected), "aggregation", summary))
	if pollRemote == "" {
		pollRemote = chatID
	}
	if pollParticipant == "" {
		pollParticipant = senderID
	}
	b.enqueuePollUpdateEvent(pollID, pollRemote, pollParticipant, selected, aggregation)
}

// Options of the poll with the voter recorded on each option the vote selected, plus the selected names
func aggregateVote(poll *waE2E.Message, selectedHashes [][]byte, voter string) ([]any, []string) {
	creation, _ := firstPoll(poll.ProtoReflect())
	type option struct {
		name   string
		voters []any
	}
	var order []string
	byHash := map[string]*option{}
	for _, name := range pollOptions(creation) {
		sum := sha256.Sum256([]byte(name))
		h := fmt.Sprintf("%x", sum[:])
		if _, dup := byHash[h]; !dup {
			order = append(order, h)
			byHash[h] = &option{name: name, voters: []any{}}
		}
	}
	for _, sel := range selectedHashes {
		h := fmt.Sprintf("%x", sel)
		opt, ok := byHash[h]
		if !ok {
			opt = &option{name: "Unknown", voters: []any{}}
			byHash[h] = opt
			order = append(order, h)
		}
		opt.voters = append(opt.voters, voter)
	}
	out := []any{}
	selected := []string{}
	for _, h := range order {
		o := byHash[h]
		out = append(out, jsonObj("name", o.name, "voters", o.voters))
		if len(o.voters) > 0 && o.name != "" && o.name != "Unknown" {
			selected = append(selected, o.name)
		}
	}
	return out, selected
}

// Queues a deduplicated vote event for a poll the bridge itself created
func (b *bridgeHost) enqueuePollUpdateEvent(pollID, remoteJid, participant string, selected []string, aggregation []any) {
	chatID := bridgeNormalizeID(remoteJid)
	senderID := bridgeNormalizeID(participant)
	if senderID == "" {
		senderID = chatID
	}
	if pollID == "" || !b.recentlySent.has(pollID) {
		if b.cfg.debug {
			b.console.jsonLine(jsonObj("event", "ignored", "reason", "foreign_poll_update", "pollId", pollID))
		}
		return
	}
	chosen := strings.Join(selected, ", ")
	if len(selected) == 0 {
		chosen = formatPollUpdateText(pollID)
	}
	dedupe := "poll:" + pollID + ":" + senderID + ":" + strings.Join(selected, "|")
	if b.processedPolls.has(dedupe) {
		return
	}
	b.processedPolls.remember(dedupe)
	senderName, _, _ := strings.Cut(senderID, "@")
	chatName, _, _ := strings.Cut(chatID, "@")
	b.pushMessage(jsonObj("messageId", fmt.Sprintf("%s:update:%d", pollID, time.Now().UnixMilli()), "chatId", chatID,
		"senderId", senderID, "senderName", senderName, "chatName", chatName, "isGroup", strings.HasSuffix(chatID, "@g.us"),
		"body", chosen, "hasMedia", false, "mediaType", "poll_update", "mime", "", "fileName", "", "nativeType", "pollUpdateMessage",
		"nativeMetadata", jsonObj("pollUpdate", jsonObj("pollId", pollID, "selectedOptions", stringsAny(selected), "aggregation", aggregation)),
		"mediaUrls", []any{}, "mentionedIds", []any{}, "quotedMessageId", pollID, "quotedParticipant", "", "quotedRemoteJid", chatID,
		"quotedText", "", "hasQuotedMessage", true, "botIds", []any{}, "timestamp", time.Now().Unix()))
}

// Delivery and read receipts of the bridge's own sends as delivery status events
func (b *bridgeHost) onReceipt(evt *events.Receipt) {
	status := ""
	switch evt.Type {
	case types.ReceiptTypeDelivered:
		status = "delivered"
	case types.ReceiptTypeRead:
		status = "read"
	case types.ReceiptTypePlayed:
		status = "played"
	case types.ReceiptTypeServerError:
		status = "error"
	default:
		return
	}
	chatID := bridgeNormalizeID(evt.Chat.String())
	for _, id := range evt.MessageIDs {
		if !b.recentlySent.has(id) {
			continue
		}
		at := time.Now().Unix()
		event := jsonObj("event", "delivery_status", "messageId", id, "chatId", chatID, "status", status)
		if evt.IsGroup && status != "error" {
			if !evt.Timestamp.IsZero() {
				at = evt.Timestamp.Unix()
			}
			event.Set("timestamp", at)
			if recipient := bridgeNormalizeID(evt.Sender.ToNonAD().String()); recipient != "" {
				event.Set("recipientId", recipient)
			}
		} else {
			event.Set("timestamp", at)
		}
		b.pushDelivery(event)
	}
}

// Server acknowledgement of one of the bridge's sends as a delivery status event
func (b *bridgeHost) onServerAck(chatID, id string, at time.Time) {
	if id == "" || !b.recentlySent.has(id) {
		return
	}
	ts := at.Unix()
	if at.IsZero() {
		ts = time.Now().Unix()
	}
	b.pushDelivery(jsonObj("event", "delivery_status", "messageId", id, "chatId", bridgeNormalizeID(chatID), "status", "server_ack", "timestamp", ts))
}
