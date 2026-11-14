package qqbot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var atMentionRE = pyre.MustCompile(`^@\S+\s*`)

// Button decision mapped to the approval choice, the three button layout folding session into always
var approvalButtonToChoice = map[string]string{"allow-once": "once", "allow-always": "always", "deny": "deny"}

// Handler of one inbound message kind
type inboundHandler func(a *QQAdapter, ctx context.Context, d map[string]any, msgID, content string, author map[string]any, timestamp string) error

// Inbound dispatch events and the handler of each
var inboundHandlers = map[string]inboundHandler{
	"C2C_MESSAGE_CREATE":      (*QQAdapter).handleC2CMessage,
	"GROUP_AT_MESSAGE_CREATE": (*QQAdapter).handleGroupMessage,
	"GUILD_MESSAGE_CREATE":    (*QQAdapter).handleGuildMessage,
	"GUILD_AT_MESSAGE_CREATE": (*QQAdapter).handleGuildMessage,
	"DIRECT_MESSAGE_CREATE":   (*QQAdapter).handleDMMessage,
}

// str of a dict value with a default for an absent key
func strAt(d map[string]any, key string) string { return pyrepr.Str(getOr(d, key, "")) }

// Caches the last message id of the chat for typing and passive replies, then runs the base pipeline
func (a *QQAdapter) HandleMessage(ctx context.Context, event *gateway.MessageEvent) error {
	if event.MessageID != nil && *event.MessageID != "" && event.Source != nil && event.Source.ChatID != "" {
		a.mu.Lock()
		a.lastMsgID[event.Source.ChatID] = *event.MessageID
		a.mu.Unlock()
	}
	return a.BasePlatformAdapter.HandleMessage(ctx, event)
}

// Dedups one inbound dispatch and hands it to the handler of its event type
func (a *QQAdapter) onMessage(ctx context.Context, eventType string, raw any) error {
	d, ok := asDict(raw)
	if !ok {
		return nil
	}
	msgID := strAt(d, "id")
	if msgID == "" || a.dedup.IsDuplicate(msgID) {
		slog.Debug(fmt.Sprintf("[%s] Duplicate or missing message id: %s", a.logTag(), msgID))
		return nil
	}
	handler := inboundHandlers[eventType]
	if handler == nil {
		return nil
	}
	author, isDict := asDict(d["author"])
	if !isDict {
		author = map[string]any{}
	}
	return handler(a, ctx, d, msgID, pystr.Strip(strAt(d, "content")), author, strAt(d, "timestamp"))
}

// Installs or clears the callback run for every acknowledged button interaction
func (a *QQAdapter) SetInteractionCallback(callback func(ctx context.Context, event InteractionEvent) error) {
	a.mu.Lock()
	a.interactionCallback = callback
	a.mu.Unlock()
}

// Parses a button interaction, acknowledges it at once and hands it to the registered callback
func (a *QQAdapter) onInteraction(ctx context.Context, raw any) {
	if _, ok := asDict(raw); !ok {
		return
	}
	event, err := ParseInteractionEvent(raw)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Failed to parse INTERACTION_CREATE: %s", a.logTag(), errText(err)))
		return
	}
	if event.ID == "" {
		slog.Warn(fmt.Sprintf("[%s] INTERACTION_CREATE missing id, skipping ACK", a.logTag()))
		return
	}
	if err := a.acknowledgeInteraction(ctx, event.ID, 0); err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Warn(fmt.Sprintf("[%s] Failed to ACK interaction %s: %s", a.logTag(), event.ID, errText(err)))
	}
	slog.Info(fmt.Sprintf("[%s] Interaction: scene=%s button_data=%s operator=%s", a.logTag(), event.Scene, pyrepr.Repr(event.ButtonData), event.OperatorOpenID()))
	a.mu.Lock()
	callback := a.interactionCallback
	a.mu.Unlock()
	if callback == nil {
		slog.Debug(fmt.Sprintf("[%s] No interaction callback registered; dropping button click %s", a.logTag(), pyrepr.Repr(event.ButtonData)))
		return
	}
	if err := callback(ctx, event); err != nil && ctx.Err() == nil {
		slog.Error(fmt.Sprintf("[%s] Interaction callback raised: %s", a.logTag(), errText(err)), "error", err)
	}
}

// Acknowledges a button interaction so the client shows no error icon on the button
func (a *QQAdapter) acknowledgeInteraction(ctx context.Context, interactionID string, code int) error {
	client, err := a.requireHTTPClient()
	if err != nil {
		return err
	}
	headers, err := a.authHeaders(ctx)
	if err != nil {
		return err
	}
	body, err := jsonBody(obj("code", int64(code)))
	if err != nil {
		return err
	}
	resp, err := httpRequest(ctx, client, "PUT", APIBase+"/interactions/"+interactionID, headers, body, DefaultAPITimeout)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return runtimeError(fmt.Sprintf("Interaction ACK failed [%d]: %s", resp.StatusCode, pystr.Slice(resp.Text(), 0, 200)))
	}
	return nil
}

// Platform, chat type, chat id and optional user id of a gateway session key, false when it has another shape
func parseGatewaySessionKey(sessionKey string) (map[string]string, bool) {
	parts := strings.Split(sessionKey, ":")
	if len(parts) < 5 || parts[0] != "agent" || parts[1] == "" {
		return nil, false
	}
	parsed := map[string]string{"platform": parts[2], "chat_type": parts[3], "chat_id": parts[4]}
	if len(parts) > 5 {
		parsed["user_id"] = parts[5]
	}
	return parsed, true
}

// Whether the operator of a button click owns the session the click is aimed at
func isAuthorizedInteractionForSession(event InteractionEvent, sessionKey string) bool {
	parsed, ok := parseGatewaySessionKey(sessionKey)
	operator := pystr.Strip(event.OperatorOpenID())
	if !ok || parsed["platform"] != "qqbot" || operator == "" {
		return false
	}
	chatType, chatID := parsed["chat_type"], parsed["chat_id"]
	switch chatType {
	case "c2c", "dm":
		return chatID != "" && operator == chatID
	case "group", "guild":
		eventChat := event.GroupOpenID
		if eventChat == "" {
			eventChat = event.GuildID
		}
		if eventChat = pystr.Strip(eventChat); eventChat == "" || eventChat != chatID {
			return false
		}
		sessionUser := pystr.Strip(parsed["user_id"])
		return sessionUser != "" && operator == sessionUser
	}
	return false
}

// Session key an update prompt click is authorized against, built by the shared key builder so it carries the profile namespace
func (a *QQAdapter) updatePromptSessionKey(ctx context.Context, event InteractionEvent, chat string) (string, error) {
	return a.SourceSessionKey(ctx, a.BuildSource(ctx, chat, gateway.SourceFields{ChatType: event.Scene}))
}

// Default interaction callback, approval clicks resolving the pending approval and update prompt clicks writing the answer file
func (a *QQAdapter) defaultInteractionDispatch(ctx context.Context, event InteractionEvent) error {
	buttonData := event.ButtonData
	if buttonData == "" {
		return nil
	}
	if sessionKey, decision, ok := ParseApprovalButtonData(buttonData); ok {
		choice, known := approvalButtonToChoice[decision]
		if !known {
			slog.Warn(fmt.Sprintf("[%s] Unknown approval decision %s (session=%s)", a.logTag(), pyrepr.Repr(decision), sessionKey))
			return nil
		}
		if !isAuthorizedInteractionForSession(event, sessionKey) {
			slog.Warn(fmt.Sprintf("[%s] Rejected unauthorized approval click for session %s (operator=%s)", a.logTag(), sessionKey, event.OperatorOpenID()))
			return nil
		}
		count := tools.ResolveGatewayApproval(sessionKey, choice, false, "", "")
		slog.Info(fmt.Sprintf("[%s] Button resolved %d approval(s) for session %s (choice=%s, operator=%s)", a.logTag(), count, sessionKey, choice,
			event.OperatorOpenID()))
		return nil
	}
	if updateAnswer, ok := ParseUpdatePromptButtonData(buttonData); ok {
		chat := event.GroupOpenID
		for _, candidate := range []string{event.GuildID, event.UserOpenID} {
			if chat == "" {
				chat = candidate
			}
		}
		sessionKey, err := a.updatePromptSessionKey(ctx, event, chat)
		if err != nil {
			return err
		}
		if !isAuthorizedInteractionForSession(event, sessionKey) {
			slog.Warn(fmt.Sprintf("[%s] Rejected unauthorized update prompt click (operator=%s)", a.logTag(), event.OperatorOpenID()))
			return nil
		}
		writeUpdateResponse(ctx, updateAnswer, event.OperatorOpenID())
		return nil
	}
	slog.Debug(fmt.Sprintf("[%s] Unrecognised button_data %s from interaction %s", a.logTag(), pyrepr.Repr(buttonData), event.ID))
	return nil
}

// Writes the update prompt answer to the response file the detached updater polls, through a temp file and rename
func writeUpdateResponse(ctx context.Context, answer, operator string) {
	responsePath := filepath.Join(home.Dir(ctx), ".update_response")
	tmp := responsePath + ".tmp"
	err := os.WriteFile(tmp, []byte(answer), 0o666)
	if err == nil {
		err = os.Rename(tmp, responsePath)
	}
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to write update response: %s", errText(err)))
		return
	}
	if operator == "" {
		operator = "(unknown)"
	}
	slog.Info(fmt.Sprintf("QQ update prompt answered %s by %s", pyrepr.Repr(answer), operator))
}

// Source fields of an inbound message beyond its chat id
type ingestSource struct {
	chatID, qqChatType, userID, chatType string
	userName                             *string
	verbose                              bool
}

// Direct message from a user, gated by the DM policy
func (a *QQAdapter) handleC2CMessage(ctx context.Context, d map[string]any, msgID, content string, author map[string]any, timestamp string) error {
	userOpenID := strAt(author, "user_openid")
	if userOpenID == "" {
		return nil
	}
	if allowed, err := a.IsDMIntakeAllowed(ctx, userOpenID); err != nil || !allowed {
		return err
	}
	attachmentsRaw := d["attachments"]
	summary := "None"
	list, isList := attachmentsRaw.([]any)
	if pyrepr.Truthy(attachmentsRaw) {
		summary = fmt.Sprintf("%d items", len(list))
	}
	slog.Info(fmt.Sprintf("[%s] C2C message: id=%s content=%s attachments=%s", a.logTag(), msgID, pyrepr.Repr(pystr.Slice(content, 0, 50)), summary))
	if isList {
		for i, raw := range list {
			if att, ok := asDict(raw); ok {
				slog.Info(fmt.Sprintf("[%s] attachment[%d]: content_type=%s url=%s filename=%s", a.logTag(), i, strAt(att, "content_type"),
					pystr.Slice(strAt(att, "url"), 0, 80), strAt(att, "filename")))
			}
		}
	}
	return a.ingest(ctx, d, msgID, content, attachmentsRaw, timestamp,
		ingestSource{chatID: userOpenID, qqChatType: "c2c", userID: userOpenID, chatType: "dm", verbose: true})
}

// Group message that mentions the bot, gated by the group policy
func (a *QQAdapter) handleGroupMessage(ctx context.Context, d map[string]any, msgID, content string, author map[string]any, timestamp string) error {
	groupOpenID, member := strAt(d, "group_openid"), strAt(author, "member_openid")
	if groupOpenID == "" {
		return nil
	}
	if allowed, err := a.IsGroupAllowed(ctx, groupOpenID, member); err != nil || !allowed {
		return err
	}
	return a.ingest(ctx, d, msgID, stripAtMention(content), d["attachments"], timestamp,
		ingestSource{chatID: groupOpenID, qqChatType: "group", userID: member, chatType: "group"})
}

// Guild channel message, gated by the group policy on the guild or channel
func (a *QQAdapter) handleGuildMessage(ctx context.Context, d map[string]any, msgID, content string, author map[string]any, timestamp string) error {
	channelID := strAt(d, "channel_id")
	if channelID == "" {
		return nil
	}
	guildID, authorID := strAt(d, "guild_id"), strAt(author, "id")
	aclChat := guildID
	if aclChat == "" {
		aclChat = channelID
	}
	allowed, err := a.IsGroupAllowed(ctx, aclChat, authorID)
	if err != nil {
		return err
	}
	if !allowed {
		slog.Debug(fmt.Sprintf("[%s] Guild message blocked by ACL: channel=%s user=%s", a.logTag(), channelID, authorID))
		return nil
	}
	member, isDict := asDict(d["member"])
	if !isDict {
		member = map[string]any{}
	}
	nick := strAt(member, "nick")
	if nick == "" {
		nick = strAt(author, "username")
	}
	source := ingestSource{chatID: channelID, qqChatType: "guild", userID: authorID, chatType: "group"}
	if nick != "" {
		source.userName = &nick
	}
	return a.ingest(ctx, d, msgID, content, d["attachments"], timestamp, source)
}

// Guild direct message, gated by the DM policy
func (a *QQAdapter) handleDMMessage(ctx context.Context, d map[string]any, msgID, content string, author map[string]any, timestamp string) error {
	guildID := strAt(d, "guild_id")
	if guildID == "" {
		return nil
	}
	authorID := strAt(author, "id")
	allowed, err := a.IsDMIntakeAllowed(ctx, authorID)
	if err != nil {
		return err
	}
	if !allowed {
		slog.Debug(fmt.Sprintf("[%s] Guild DM blocked by ACL: guild=%s user=%s", a.logTag(), guildID, authorID))
		return nil
	}
	return a.ingest(ctx, d, msgID, content, d["attachments"], timestamp,
		ingestSource{chatID: guildID, qqChatType: "dm", userID: authorID, chatType: "dm"})
}

// Block appended to text after a blank line, the block alone when the text is blank
func appendBlock(text, block string) string {
	if pystr.Strip(text) == "" {
		return block
	}
	return pystr.Strip(text + "\n\n" + block)
}

// Shared inbound tail that folds transcripts, file info and quoted context into the text and dispatches the event
func (a *QQAdapter) ingest(ctx context.Context, d map[string]any, msgID, content string, attachments any, timestamp string, src ingestSource) error {
	att, err := a.processAttachments(ctx, attachments)
	if err != nil {
		return err
	}
	text := content
	if len(att.voiceTranscripts) > 0 {
		text = appendBlock(text, strings.Join(att.voiceTranscripts, "\n"))
	}
	if att.attachmentInfo != "" {
		text = appendBlock(text, att.attachmentInfo)
	}
	imageURLs, imageMediaTypes := att.imageURLs, att.imageMediaTypes
	if src.verbose {
		slog.Info(fmt.Sprintf("[%s] After processing: images=%d, voice=%d", a.logTag(), len(imageURLs), len(att.voiceTranscripts)))
	}
	quoted, err := a.processQuotedContext(ctx, d)
	if err != nil {
		return err
	}
	text = mergeQuoteInto(text, quoted.quoteBlock)
	if len(quoted.imageURLs) > 0 {
		imageURLs = append(append([]string{}, imageURLs...), quoted.imageURLs...)
		imageMediaTypes = append(append([]string{}, imageMediaTypes...), quoted.imageMediaTypes...)
	}
	if pystr.Strip(text) == "" && len(imageURLs) == 0 {
		return nil
	}
	a.mu.Lock()
	a.chatTypeMap[src.chatID] = src.qqChatType
	a.mu.Unlock()
	fields := gateway.SourceFields{ChatType: src.chatType, UserID: &src.userID, UserName: src.userName}
	event := gateway.NewMessageEvent(text)
	event.Source = a.BuildSource(ctx, src.chatID, fields)
	event.MessageType, event.RawMessage, event.MessageID = detectMessageType(imageURLs, imageMediaTypes), d, &msgID
	event.MediaURLs, event.MediaTypes, event.Timestamp = imageURLs, imageMediaTypes, parseQQTimestamp(timestamp)
	return a.Self().HandleMessage(ctx, event)
}

// Quote block and cached images of the message a user replies to
type quotedContext struct {
	quoteBlock      string
	imageURLs       []string
	imageMediaTypes []string
}

// Text, transcripts and images of a quoted message, empty when nothing is quoted
func (a *QQAdapter) processQuotedContext(ctx context.Context, d map[string]any) (quotedContext, error) {
	isQuote := false
	messageType := getOr(d, "message_type", int64(0))
	if !pyrepr.Truthy(messageType) {
		messageType = int64(0)
	}
	if n, err := pyInt64(messageType); err == nil {
		isQuote = n == 103
	}
	elements, isList := d["msg_elements"].([]any)
	if !isQuote || !isList || len(elements) == 0 {
		return quotedContext{}, nil
	}
	var quotedTextParts []string
	var allAttachments []any
	for _, raw := range elements {
		element, ok := asDict(raw)
		if !ok {
			continue
		}
		if text := pystr.Strip(strAt(element, "content")); text != "" {
			quotedTextParts = append(quotedTextParts, text)
		}
	}
	for _, raw := range elements {
		element, ok := asDict(raw)
		if !ok {
			continue
		}
		if list, isList := element["attachments"].([]any); isList {
			for _, att := range list {
				if _, isDict := asDict(att); isDict {
					allAttachments = append(allAttachments, att)
				}
			}
		}
	}
	att, err := a.processAttachments(ctx, allAttachments)
	if err != nil {
		return quotedContext{}, err
	}
	var lines []string
	if len(quotedTextParts) > 0 {
		lines = append(lines, strings.Join(quotedTextParts, " "))
	}
	lines = append(lines, att.voiceTranscripts...)
	if att.attachmentInfo != "" {
		lines = append(lines, att.attachmentInfo)
	}
	if len(lines) == 0 && len(att.imageURLs) == 0 {
		return quotedContext{}, nil
	}
	block := "[Quoted message]: (image)"
	if len(lines) > 0 {
		block = "[Quoted message]:\n" + strings.Join(lines, "\n")
	}
	return quotedContext{quoteBlock: block, imageURLs: att.imageURLs, imageMediaTypes: att.imageMediaTypes}, nil
}

// Quote block placed before the text after a blank line
func mergeQuoteInto(text, quoteBlock string) string {
	if quoteBlock == "" {
		return text
	}
	if pystr.Strip(text) == "" {
		return quoteBlock
	}
	return pystr.Strip(quoteBlock + "\n\n" + text)
}

// Message type from the first media type, text when there is no media
func detectMessageType(mediaURLs, mediaTypes []string) gateway.MessageType {
	if len(mediaURLs) == 0 {
		return gateway.MessageTypeText
	}
	if len(mediaTypes) == 0 {
		return gateway.MessageTypePhoto
	}
	firstType := pystr.Lower(mediaTypes[0])
	contains := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(firstType, part) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("audio", "voice", "silk"):
		return gateway.MessageTypeVoice
	case contains("video"):
		return gateway.MessageTypeVideo
	case contains("image", "photo"):
		return gateway.MessageTypePhoto
	}
	slog.Debug(fmt.Sprintf("Unknown media content_type '%s', defaulting to TEXT", firstType))
	return gateway.MessageTypeText
}

// Cached images, voice transcripts and a text description of the other files of one attachment list
type processedAttachments struct {
	imageURLs        []string
	imageMediaTypes  []string
	voiceTranscripts []string
	attachmentInfo   string
}

// Stripped string value, empty for anything that is not a non blank string
func optStr(value any) string {
	s, _ := value.(string)
	return pystr.Strip(s)
}

// Transcribes voice attachments, caches images and files and describes the files in text
func (a *QQAdapter) processAttachments(ctx context.Context, attachments any) (processedAttachments, error) {
	out := processedAttachments{imageURLs: []string{}, imageMediaTypes: []string{}, voiceTranscripts: []string{}}
	var otherAttachments []string
	list, _ := attachments.([]any)
	for _, raw := range list {
		att, ok := asDict(raw)
		if !ok {
			continue
		}
		ct := pystr.Lower(pystr.Strip(strAt(att, "content_type")))
		url := pystr.Strip(strAt(att, "url"))
		filename := strAt(att, "filename")
		if url == "" {
			continue
		}
		if strings.HasPrefix(url, "//") {
			url = "https:" + url
		}
		slog.Debug(fmt.Sprintf("[%s] Processing attachment: content_type=%s, url=%s, filename=%s", a.logTag(), ct, pystr.Slice(url, 0, 80), filename))
		if isVoiceContentType(ct, filename) {
			transcript, err := a.sttVoiceAttachment(ctx, url, ct, filename, optStr(att["asr_refer_text"]), optStr(att["voice_wav_url"]))
			if err != nil {
				return out, err
			}
			if transcript != "" {
				out.voiceTranscripts = append(out.voiceTranscripts, "[Voice] "+transcript)
				slog.Debug(fmt.Sprintf("[%s] Voice transcript: %s", a.logTag(), transcript))
			} else {
				slog.Warn(fmt.Sprintf("[%s] Voice STT failed for %s", a.logTag(), pystr.Slice(url, 0, 60)))
				out.voiceTranscripts = append(out.voiceTranscripts, "[Voice] [语音识别失败]")
			}
			continue
		}
		isImage := strings.HasPrefix(ct, "image/")
		cachedPath, err := a.downloadAndCache(ctx, url, ct, filename)
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			kind := "attachment"
			if isImage {
				kind = "image"
			}
			slog.Debug(fmt.Sprintf("[%s] Failed to cache %s: %s", a.logTag(), kind, errText(err)))
			continue
		}
		if cachedPath == "" {
			continue
		}
		switch {
		case !isImage:
			label := "file"
			if strings.HasPrefix(ct, "video/") {
				label = "video"
			}
			name := filename
			if name == "" {
				name = ct
			}
			otherAttachments = append(otherAttachments, fmt.Sprintf("[%s: %s (%s)]", label, name, cachedPath))
		case isRegularFile(cachedPath):
			out.imageURLs = append(out.imageURLs, cachedPath)
			mediaType := ct
			if mediaType == "" {
				mediaType = "image/jpeg"
			}
			out.imageMediaTypes = append(out.imageMediaTypes, mediaType)
		default:
			slog.Warn(fmt.Sprintf("[%s] Cached image path does not exist: %s", a.logTag(), cachedPath))
		}
	}
	out.attachmentInfo = strings.Join(otherAttachments, "\n")
	return out, nil
}

// Whether the path is an existing regular file
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Leading at mention removed from stripped group content
func stripAtMention(content string) string {
	out, err := atMentionRE.Replace(pystr.Strip(content), "", -1, -1)
	if err != nil {
		return pystr.Strip(content)
	}
	return out
}

// Time of a QQ timestamp given as ISO 8601 text or integer milliseconds, the current UTC time otherwise
func parseQQTimestamp(raw string) time.Time {
	if raw != "" {
		if t, err := pydatetime.FromISOFormat(raw); err == nil {
			return t
		}
		if ms, err := pyInt64(raw); err == nil {
			seconds := float64(ms) / 1000
			if seconds > -62135596800 && seconds < 253402300800 {
				return pydatetime.FromTimestamp(seconds).UTC()
			}
		}
	}
	return time.Now().UTC()
}
