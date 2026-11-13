package qqbot

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Extensions a file attachment is treated as a voice note by when its type is unspecific
var voiceExtensions = []string{".silk", ".amr", ".mp3", ".wav", ".ogg", ".m4a", ".aac", ".speex", ".flac"}

// Leading at mention of a group message
var atMentionRE = regexp.MustCompile(`^@\S+\s*`)

// Chat, QQ surface and sender fields one inbound message lands under
type ingestTarget struct {
	chatID   string
	kind     string
	userID   string
	userName string
	chatType string
	verbose  bool
}

// Deduplicates a message dispatch and routes it to the handler of its surface
func (a *QQAdapter) onMessage(ctx context.Context, eventType string, d map[string]any) error {
	if d == nil {
		return nil
	}
	msgID := jstr(d["id"])
	if msgID == "" || a.dedup.IsDuplicate(msgID) {
		slog.Debug(fmt.Sprintf("[%s] Duplicate or missing message id: %s", a.logTag(), msgID))
		return nil
	}
	author := jmap(d["author"])
	content := strings.TrimSpace(jstr(d["content"]))
	timestamp := jstr(d["timestamp"])
	switch inboundKinds[eventType] {
	case "c2c":
		return a.handleC2C(ctx, d, msgID, content, author, timestamp)
	case "group":
		return a.handleGroup(ctx, d, msgID, content, author, timestamp)
	case "guild":
		return a.handleGuild(ctx, d, msgID, content, author, timestamp)
	case "dm":
		return a.handleDM(ctx, d, msgID, content, author, timestamp)
	}
	return nil
}

// Private chat with a user, gated by the DM policy
func (a *QQAdapter) handleC2C(ctx context.Context, d map[string]any, msgID, content string, author map[string]any, timestamp string) error {
	userOpenID := jstr(author["user_openid"])
	if userOpenID == "" {
		return nil
	}
	allowed, err := a.IsDMIntakeAllowed(ctx, userOpenID)
	if err != nil || !allowed {
		return err
	}
	attachments := jlist(d["attachments"])
	count := "None"
	if len(attachments) > 0 {
		count = fmt.Sprintf("%d items", len(attachments))
	}
	slog.Info(fmt.Sprintf("[%s] C2C message: id=%s content=%q attachments=%s", a.logTag(), msgID, cutRunes(content, 50), count))
	for i, raw := range attachments {
		if att := jmap(raw); att != nil {
			slog.Info(fmt.Sprintf("[%s] attachment[%d]: content_type=%s url=%s filename=%s", a.logTag(), i, jstr(att["content_type"]),
				cutRunes(jstr(att["url"]), 80), jstr(att["filename"])))
		}
	}
	return a.ingest(ctx, d, msgID, content, attachments, timestamp,
		ingestTarget{chatID: userOpenID, kind: "c2c", userID: userOpenID, chatType: "dm", verbose: true})
}

// Group message that at mentions the bot, gated by the group policy
func (a *QQAdapter) handleGroup(ctx context.Context, d map[string]any, msgID, content string, author map[string]any, timestamp string) error {
	groupOpenID := jstr(d["group_openid"])
	member := jstr(author["member_openid"])
	if groupOpenID == "" {
		return nil
	}
	allowed, err := a.IsGroupAllowed(ctx, groupOpenID, member)
	if err != nil || !allowed {
		return err
	}
	return a.ingest(ctx, d, msgID, atMentionRE.ReplaceAllString(strings.TrimSpace(content), ""), jlist(d["attachments"]), timestamp,
		ingestTarget{chatID: groupOpenID, kind: "group", userID: member, chatType: "group"})
}

// Guild channel message, gated by the group policy against the guild
func (a *QQAdapter) handleGuild(ctx context.Context, d map[string]any, msgID, content string, author map[string]any, timestamp string) error {
	channelID := jstr(d["channel_id"])
	if channelID == "" {
		return nil
	}
	guildID := jstr(d["guild_id"])
	authorID := jstr(author["id"])
	scope := guildID
	if scope == "" {
		scope = channelID
	}
	allowed, err := a.IsGroupAllowed(ctx, scope, authorID)
	if err != nil {
		return err
	}
	if !allowed {
		slog.Debug(fmt.Sprintf("[%s] Guild message blocked by ACL: channel=%s user=%s", a.logTag(), channelID, authorID))
		return nil
	}
	nick := jstr(jmap(d["member"])["nick"])
	if nick == "" {
		nick = jstr(author["username"])
	}
	return a.ingest(ctx, d, msgID, content, jlist(d["attachments"]), timestamp,
		ingestTarget{chatID: channelID, kind: "guild", userID: authorID, userName: nick, chatType: "group"})
}

// Guild direct message, gated by the DM policy
func (a *QQAdapter) handleDM(ctx context.Context, d map[string]any, msgID, content string, author map[string]any, timestamp string) error {
	guildID := jstr(d["guild_id"])
	if guildID == "" {
		return nil
	}
	authorID := jstr(author["id"])
	allowed, err := a.IsDMIntakeAllowed(ctx, authorID)
	if err != nil {
		return err
	}
	if !allowed {
		slog.Debug(fmt.Sprintf("[%s] Guild DM blocked by ACL: guild=%s user=%s", a.logTag(), guildID, authorID))
		return nil
	}
	return a.ingest(ctx, d, msgID, content, jlist(d["attachments"]), timestamp,
		ingestTarget{chatID: guildID, kind: "dm", userID: authorID, chatType: "dm"})
}

// Block after text with a blank line between, the block alone when the text is blank
func appendBlock(text, block string) string {
	if strings.TrimSpace(text) == "" {
		return block
	}
	return strings.TrimSpace(text + "\n\n" + block)
}

// Quote block before the text with a blank line between
func mergeQuote(text, quote string) string {
	if quote == "" {
		return text
	}
	if strings.TrimSpace(text) == "" {
		return quote
	}
	return strings.TrimSpace(quote + "\n\n" + text)
}

// Folds transcripts, file notes and quoted context into the text and hands the event to the gateway
func (a *QQAdapter) ingest(ctx context.Context, d map[string]any, msgID, content string, attachments []any, timestamp string, target ingestTarget) error {
	att := a.processAttachments(ctx, attachments)
	text := content
	if len(att.transcripts) > 0 {
		text = appendBlock(text, strings.Join(att.transcripts, "\n"))
	}
	if att.info != "" {
		text = appendBlock(text, att.info)
	}
	images, types := att.images, att.types
	if target.verbose {
		slog.Info(fmt.Sprintf("[%s] After processing: images=%d, voice=%d", a.logTag(), len(images), len(att.transcripts)))
	}
	quote := a.processQuotedContext(ctx, d)
	text = mergeQuote(text, quote.block)
	images = append(images, quote.images...)
	types = append(types, quote.types...)
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return nil
	}
	a.setChatType(target.chatID, target.kind)
	fields := gateway.SourceFields{ChatType: target.chatType, UserID: &target.userID}
	if target.userName != "" {
		fields.UserName = &target.userName
	}
	event := gateway.NewMessageEvent(text)
	event.Source = a.BuildSource(ctx, target.chatID, fields)
	event.MessageType = detectMessageType(images, types)
	event.RawMessage = d
	event.MessageID = &msgID
	event.MediaURLs, event.MediaTypes = images, types
	event.Timestamp = parseTimestamp(timestamp)
	return a.HandleMessage(ctx, event)
}

// Quoted message text, transcripts, file notes and images of a reply, empty when nothing is quoted
type quotedContext struct {
	block  string
	images []string
	types  []string
}

// Context of the message a user replied to, read from the msg_elements of a type 103 message
func (a *QQAdapter) processQuotedContext(ctx context.Context, d map[string]any) quotedContext {
	if mt, _ := jint(d["message_type"]); mt != 103 {
		return quotedContext{}
	}
	elements := jlist(d["msg_elements"])
	if len(elements) == 0 {
		return quotedContext{}
	}
	var parts []string
	var attachments []any
	for _, raw := range elements {
		el := jmap(raw)
		if el == nil {
			continue
		}
		if t := strings.TrimSpace(jstr(el["content"])); t != "" {
			parts = append(parts, t)
		}
		for _, att := range jlist(el["attachments"]) {
			if jmap(att) != nil {
				attachments = append(attachments, att)
			}
		}
	}
	att := a.processAttachments(ctx, attachments)
	var lines []string
	if len(parts) > 0 {
		lines = append(lines, strings.Join(parts, " "))
	}
	lines = append(lines, att.transcripts...)
	if att.info != "" {
		lines = append(lines, att.info)
	}
	if len(lines) == 0 && len(att.images) == 0 {
		return quotedContext{}
	}
	block := "[Quoted message]: (image)"
	if len(lines) > 0 {
		block = "[Quoted message]:\n" + strings.Join(lines, "\n")
	}
	return quotedContext{block: block, images: att.images, types: att.types}
}

// Message type from the first media type, text when there is no media
func detectMessageType(urls, types []string) gateway.MessageType {
	if len(urls) == 0 {
		return gateway.MessageTypeText
	}
	if len(types) == 0 {
		return gateway.MessageTypePhoto
	}
	first := strings.ToLower(types[0])
	switch {
	case strings.Contains(first, "audio") || strings.Contains(first, "voice") || strings.Contains(first, "silk"):
		return gateway.MessageTypeVoice
	case strings.Contains(first, "video"):
		return gateway.MessageTypeVideo
	case strings.Contains(first, "image") || strings.Contains(first, "photo"):
		return gateway.MessageTypePhoto
	}
	slog.Debug(fmt.Sprintf("Unknown media content_type '%s', defaulting to TEXT", first))
	return gateway.MessageTypeText
}

// Cached images with their types, voice transcripts and notes for the other attachments
type attachmentResult struct {
	images      []string
	types       []string
	transcripts []string
	info        string
}

// Transcribes voice notes, caches images as media and other files as documents
func (a *QQAdapter) processAttachments(ctx context.Context, attachments []any) attachmentResult {
	var res attachmentResult
	var others []string
	for _, raw := range attachments {
		att := jmap(raw)
		if att == nil {
			continue
		}
		ct := strings.ToLower(strings.TrimSpace(jstr(att["content_type"])))
		rawURL := strings.TrimSpace(jstr(att["url"]))
		filename := jstr(att["filename"])
		if rawURL == "" {
			continue
		}
		if strings.HasPrefix(rawURL, "//") {
			rawURL = "https:" + rawURL
		}
		slog.Debug(fmt.Sprintf("[%s] Processing attachment: content_type=%s, url=%s, filename=%s", a.logTag(), ct, cutRunes(rawURL, 80), filename))
		if isVoiceContentType(ct, filename) {
			transcript := a.transcribeVoice(ctx, rawURL, filename, strings.TrimSpace(jstr(att["asr_refer_text"])),
				strings.TrimSpace(jstr(att["voice_wav_url"])))
			if transcript != "" {
				res.transcripts = append(res.transcripts, "[Voice] "+transcript)
				slog.Debug(fmt.Sprintf("[%s] Voice transcript: %s", a.logTag(), transcript))
			} else {
				slog.Warn(fmt.Sprintf("[%s] Voice STT failed for %s", a.logTag(), cutRunes(rawURL, 60)))
				res.transcripts = append(res.transcripts, "[Voice] [语音识别失败]")
			}
			continue
		}
		isImage := strings.HasPrefix(ct, "image/")
		cached, err := a.downloadAndCache(ctx, rawURL, ct, filename)
		if err != nil {
			kind := "attachment"
			if isImage {
				kind = "image"
			}
			slog.Debug(fmt.Sprintf("[%s] Failed to cache %s: %v", a.logTag(), kind, err))
			continue
		}
		if cached == "" {
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
			visible := tools.ToAgentVisibleCachePath(&tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}, cached)
			others = append(others, fmt.Sprintf("[%s: %s (%s)]", label, name, visible))
		case isRegularFile(cached):
			res.images = append(res.images, cached)
			if ct == "" {
				ct = "image/jpeg"
			}
			res.types = append(res.types, ct)
		default:
			slog.Warn(fmt.Sprintf("[%s] Cached image path does not exist: %s", a.logTag(), cached))
		}
	}
	res.info = strings.Join(others, "\n")
	return res
}

// Whether a path names an existing regular file
func isRegularFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Whether an attachment is a voice note, explicit file uploads never counting
func isVoiceContentType(ct, filename string) bool {
	if ct == "voice" || strings.HasPrefix(ct, "audio/") {
		return true
	}
	if ct == "file" {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(filename))
	for _, ext := range voiceExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// Image extensions of the common image MIME types
var imageExts = map[string]string{
	"image/jpeg": ".jpg", "image/jpg": ".jpg", "image/pjpeg": ".jpg", "image/png": ".png", "image/gif": ".gif",
	"image/webp": ".webp", "image/bmp": ".bmp", "image/tiff": ".tiff", "image/svg+xml": ".svg", "image/x-icon": ".ico",
	"image/vnd.microsoft.icon": ".ico", "image/heic": ".heic", "image/heif": ".heif", "image/avif": ".avif",
}

// Extension of an image MIME type, jpg when unknown
func imageExt(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = ct
	}
	if ext, ok := imageExts[mt]; ok {
		return ext
	}
	return ".jpg"
}

// Downloads an attachment from the QQ CDN into the image or document cache
func (a *QQAdapter) downloadAndCache(ctx context.Context, rawURL, ct, filename string) (string, error) {
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), rawURL) {
		return "", fmt.Errorf("blocked unsafe URL: %s", cutRunes(rawURL, 80))
	}
	rest, err := a.client()
	if err != nil {
		return "", nil
	}
	data, err := rest.download(ctx, rawURL, 30*time.Second)
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] Download failed for %s: %v", a.logTag(), cutRunes(rawURL, 80), err))
		return "", nil
	}
	if strings.HasPrefix(ct, "image/") {
		return gateway.CacheImageFromBytesAsync(ctx, data, imageExt(ct))
	}
	name := filename
	if name == "" {
		if u, err := url.Parse(rawURL); err == nil {
			if base := path.Base(u.Path); base != "." && base != "/" {
				name = base
			}
		}
	}
	if name == "" {
		name = "qq_attachment"
	}
	return gateway.CacheDocumentFromBytesAsync(ctx, data, name)
}

// Time of an ISO 8601 or millisecond timestamp, now when it is neither
func parseTimestamp(raw string) time.Time {
	if raw != "" {
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999Z07:00",
			"2006-01-02 15:04:05.999999999", "2006-01-02"} {
			if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
				return t
			}
		}
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return time.UnixMilli(ms).UTC()
		}
	}
	return time.Now().UTC()
}
