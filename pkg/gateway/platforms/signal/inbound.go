package signal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/mediacache"
)

// Object replacement character Signal puts where a mention sits in the body
const mentionPlaceholder = "￼"

// One signal-cli receive envelope
type envelope struct {
	Source       string          `json:"source"`
	SourceNumber string          `json:"sourceNumber"`
	SourceUUID   string          `json:"sourceUuid"`
	SourceName   string          `json:"sourceName"`
	Timestamp    int64           `json:"timestamp"`
	DataMessage  *dataMessage    `json:"dataMessage"`
	EditMessage  *editMessage    `json:"editMessage"`
	SyncMessage  json.RawMessage `json:"syncMessage"`
	StoryMessage json.RawMessage `json:"storyMessage"`
}

// Edited message carrying its replacement body
type editMessage struct {
	DataMessage *dataMessage `json:"dataMessage"`
}

// Message body of an envelope or of a sync sent transcript
type dataMessage struct {
	Timestamp         int64           `json:"timestamp"`
	Message           string          `json:"message"`
	GroupInfo         *groupInfo      `json:"groupInfo"`
	Mentions          []mention       `json:"mentions"`
	Quote             json.RawMessage `json:"quote"`
	Attachments       []attachment    `json:"attachments"`
	Destination       string          `json:"destination"`
	DestinationNumber string          `json:"destinationNumber"`
}

type groupInfo struct {
	GroupID   string `json:"groupId"`
	GroupName string `json:"groupName"`
}

// Mention range in UTF-16 code units of the body
type mention struct {
	Number string `json:"number"`
	UUID   string `json:"uuid"`
	Start  int    `json:"start"`
	Length *int   `json:"length"`
}

// Quoted message a reply points at, ID the quoted message timestamp
type quote struct {
	ID                    int64   `json:"id"`
	Author                string  `json:"author"`
	AuthorNumber          string  `json:"authorNumber"`
	AuthorUUID            string  `json:"authorUuid"`
	AuthorACI             string  `json:"authorAci"`
	AuthorServiceID       string  `json:"authorServiceId"`
	AuthorServiceIDString string  `json:"authorServiceIdString"`
	AuthorName            string  `json:"authorName"`
	AuthorProfileName     string  `json:"authorProfileName"`
	Text                  *string `json:"text"`
}

// First author identifier the quote carries
func (q *quote) author() string {
	for _, v := range []string{q.Author, q.AuthorNumber, q.AuthorUUID, q.AuthorACI, q.AuthorServiceID, q.AuthorServiceIDString} {
		if v != "" {
			return v
		}
	}
	return ""
}

type attachment struct {
	ID          string `json:"id"`
	ContentType string `json:"contentType"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
}

// Decodes into v, keeping fields decoded around a value of an unexpected type
func lenientUnmarshal(raw []byte, v any) error {
	err := json.Unmarshal(raw, v)
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		slog.Debug(fmt.Sprintf("Signal: envelope field of unexpected type: %v", err))
		return nil
	}
	return err
}

// Envelope of one SSE payload, the payload itself when it is not wrapped
func decodeEnvelope(raw []byte) (*envelope, error) {
	var wrapper struct {
		Envelope json.RawMessage `json:"envelope"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, err
	}
	body := raw
	if present(wrapper.Envelope) {
		body = wrapper.Envelope
	}
	env := &envelope{}
	if err := lenientUnmarshal(body, env); err != nil {
		return nil, err
	}
	return env, nil
}

// Whether a raw member is an object with at least one key
func nonEmptyObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return present(raw) && json.Unmarshal(raw, &m) == nil && len(m) > 0
}

// Note to Self or group sync transcript promoted to a data message, nil for other sync events and own echoes
func (a *SignalAdapter) unwrapSyncMessage(env *envelope) *envelope {
	var sync struct {
		SentMessage json.RawMessage `json:"sentMessage"`
	}
	if lenientUnmarshal(env.SyncMessage, &sync) != nil || !nonEmptyObject(sync.SentMessage) {
		return nil
	}
	sent := &dataMessage{}
	if lenientUnmarshal(sync.SentMessage, sent) != nil {
		return nil
	}
	dest := sent.DestinationNumber
	if dest == "" {
		dest = sent.Destination
	}
	if dest != a.account && (sent.GroupInfo == nil || sent.GroupInfo.GroupID == "") {
		return nil
	}
	if a.consumeSent(sent.Timestamp) {
		return nil
	}
	promoted := *env
	promoted.DataMessage = sent
	return &promoted
}

// Body with each mention placeholder range replaced by an at sign and the mentioned identifier
func renderMentions(text string, mentions []mention) string {
	if len(mentions) == 0 || !strings.Contains(text, mentionPlaceholder) {
		return text
	}
	units := utf16.Encode([]rune(text))
	ordered := append([]mention(nil), mentions...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Start > ordered[j].Start })
	for _, m := range ordered {
		length := 1
		if m.Length != nil {
			length = *m.Length
		}
		start := min(max(m.Start, 0), len(units))
		end := min(max(start+length, start), len(units))
		id := m.Number
		if id == "" {
			id = m.UUID
		}
		if id == "" {
			id = "user"
		}
		replacement := utf16.Encode([]rune("@" + id))
		units = append(units[:start:start], append(replacement, units[end:]...)...)
	}
	return string(utf16.Decode(units))
}

// Whether a value is a Signal service identifier, a PNI or u prefixed id or a UUID
func isServiceID(value string) bool {
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "PNI:") || strings.HasPrefix(value, "u:") {
		return true
	}
	_, err := uuid.Parse(value)
	return err == nil
}

// Whether a value is a plausible E.164 phone number
func looksLikeE164(value string) bool {
	digits, ok := strings.CutPrefix(value, "+")
	if !ok || len(digits) < 7 || len(digits) > 15 {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Caches a number to service id mapping seen in an envelope or the contact list
func (a *SignalAdapter) rememberRecipient(number, serviceID string) {
	if number == "" || !isServiceID(serviceID) {
		return
	}
	a.recipientMu.Lock()
	a.uuidByNumber[number] = serviceID
	a.numberByUUID[serviceID] = number
	delete(a.unresolved, number)
	a.recipientMu.Unlock()
}

// Cached service id of a number, empty when unknown
func (a *SignalAdapter) cachedUUID(number string) string {
	a.recipientMu.Lock()
	defer a.recipientMu.Unlock()
	return a.uuidByNumber[number]
}

// Whether a group passes SIGNAL_GROUP_ALLOWED_USERS, unset disabling groups and a star allowing all
func (a *SignalAdapter) groupAllowed(groupID string) bool {
	if len(a.groupAllow) == 0 {
		slog.Debug("Signal: ignoring group message (no SIGNAL_GROUP_ALLOWED_USERS)")
		return false
	}
	if !a.groupAllow["*"] && !a.groupAllow[groupID] {
		slog.Debug(fmt.Sprintf("Signal: group %.8s not in allowlist", groupID))
		return false
	}
	return true
}

// Drops a group message without the required mention and strips the bot's own mention from the text
func (a *SignalAdapter) applyGroupMentionRules(text string, mentions []mention) (bool, string) {
	acct := a.account
	botUUID := a.cachedUUID(acct)
	if a.requireMention {
		mentioned := acct != "" && strings.Contains(text, "@"+acct)
		for _, m := range mentions {
			if acct != "" && (m.Number == acct || m.UUID == acct) || botUUID != "" && m.UUID == botUUID {
				mentioned = true
			}
		}
		if !mentioned {
			slog.Debug("Signal: ignoring group message (require_mention=true, bot not mentioned)")
			return false, text
		}
	}
	if text != "" && acct != "" {
		text = strings.ReplaceAll(text, "@"+acct, "")
		if botUUID != "" {
			text = strings.ReplaceAll(text, "@"+botUUID, "")
		}
		text = strings.TrimFunc(strings.ReplaceAll(text, "  ", " "), isSpace)
	}
	return true, text
}

// Whether a quote points at a message this adapter sent
func (a *SignalAdapter) quoteIsOwn(id int64, author string) bool {
	if id != 0 && a.isSentMessage(id) {
		return true
	}
	author = strings.TrimSpace(author)
	if author == "" || a.account == "" {
		return false
	}
	if author == a.account {
		return true
	}
	a.recipientMu.Lock()
	defer a.recipientMu.Unlock()
	return author == a.uuidByNumber[a.account] || a.numberByUUID[author] == a.account
}

// Message type of the first matching media class, documents for everything else
func messageTypeFor(mediaTypes []string) gateway.MessageType {
	if len(mediaTypes) == 0 {
		return gateway.MessageTypeText
	}
	for _, class := range []struct {
		prefix string
		kind   gateway.MessageType
	}{{"audio/", gateway.MessageTypeVoice}, {"image/", gateway.MessageTypePhoto}, {"video/", gateway.MessageTypeVideo}} {
		for _, mt := range mediaTypes {
			if strings.HasPrefix(mt, class.prefix) {
				return class.kind
			}
		}
	}
	return gateway.MessageTypeDocument
}

// Turns one envelope into a gateway message after echo, story, allowlist, mention and content gates
func (a *SignalAdapter) handleEnvelope(ctx context.Context, env *envelope) error {
	noteToSelf := len(env.SyncMessage) > 0
	if noteToSelf {
		if env = a.unwrapSyncMessage(env); env == nil {
			return nil
		}
	}
	sender := env.SourceNumber
	if sender == "" {
		sender = env.SourceUUID
	}
	if sender == "" {
		sender = env.Source
	}
	a.rememberRecipient(sender, env.SourceUUID)
	if sender == "" {
		slog.Debug("Signal: ignoring envelope with no sender")
		return nil
	}
	if a.account != "" && sender == a.account && !noteToSelf {
		return nil
	}
	if a.ignoreStories && nonEmptyObject(env.StoryMessage) {
		return nil
	}
	dm := env.DataMessage
	if dm == nil && env.EditMessage != nil {
		dm = env.EditMessage.DataMessage
	}
	if dm == nil {
		return nil
	}
	group := dm.GroupInfo
	if group != nil && group.GroupID == "" && group.GroupName == "" {
		group = nil
	}
	isGroup := group != nil && group.GroupID != ""
	if isGroup && !a.groupAllowed(group.GroupID) {
		return nil
	}
	chatID := sender
	if isGroup {
		chatID = "group:" + group.GroupID
	}
	text := dm.Message
	if text != "" {
		text = renderMentions(text, dm.Mentions)
	}
	if isGroup {
		var keep bool
		if keep, text = a.applyGroupMentionRules(text, dm.Mentions); !keep {
			return nil
		}
	}
	q := &quote{}
	var rawQuote any
	if present(dm.Quote) {
		_ = lenientUnmarshal(dm.Quote, q)
		rawQuote = rawJSON(dm.Quote)
	}
	mediaURLs, mediaTypes := a.collectAttachments(ctx, dm.Attachments)
	if strings.TrimSpace(text) == "" && len(mediaURLs) == 0 {
		slog.Debug(fmt.Sprintf("Signal: skipping contentless envelope from %s (%d attachments)", gateway.RedactPhone(sender), len(mediaURLs)))
		return nil
	}
	fields := gateway.SourceFields{ChatType: "dm", UserID: &sender, UserName: &sender}
	if env.SourceName != "" {
		fields.UserName = &env.SourceName
	}
	switch {
	case group != nil:
		if group.GroupName != "" {
			fields.ChatName = &group.GroupName
		}
	case env.SourceName != "":
		fields.ChatName = &env.SourceName
	}
	if isGroup {
		fields.ChatType, fields.ChatIDAlt = "group", &group.GroupID
	}
	if env.SourceUUID != "" {
		fields.UserIDAlt = &env.SourceUUID
	}
	event := gateway.NewMessageEvent(text)
	event.Source = a.BuildSource(ctx, chatID, fields)
	event.MessageType = messageTypeFor(mediaTypes)
	event.MediaURLs, event.MediaTypes = mediaURLs, mediaTypes
	if env.Timestamp != 0 {
		event.Timestamp = time.UnixMilli(env.Timestamp).UTC()
	} else {
		event.Timestamp = time.Now().UTC()
	}
	event.RawMessage = map[string]any{"sender": sender, "timestamp_ms": env.Timestamp, "quote": rawQuote}
	author := q.author()
	if q.ID != 0 {
		id := fmt.Sprint(q.ID)
		event.ReplyToMessageID = &id
	}
	event.ReplyToText = q.Text
	if author != "" {
		event.ReplyToAuthorID = &author
	}
	if name := q.AuthorName; name != "" {
		event.ReplyToAuthorName = &name
	} else if name := q.AuthorProfileName; name != "" {
		event.ReplyToAuthorName = &name
	}
	event.ReplyToIsOwnMessage = a.quoteIsOwn(q.ID, author)
	slog.Debug(fmt.Sprintf("Signal: message from %s in %.20s: %.50s", gateway.RedactPhone(sender), chatID, text))
	return a.HandleMessage(ctx, event)
}

// Fetches and caches inbound attachments, returning cached paths and their MIME types
func (a *SignalAdapter) collectAttachments(ctx context.Context, atts []attachment) ([]string, []string) {
	var urls, types []string
	for _, att := range atts {
		if att.ID == "" {
			continue
		}
		if att.Size > maxAttachmentBytes {
			slog.Warn(fmt.Sprintf("Signal: attachment too large (%d bytes), skipping", att.Size))
			continue
		}
		path, mime, err := a.fetchAttachment(ctx, att)
		if err != nil {
			if ctx.Err() != nil {
				return urls, types
			}
			slog.Error(fmt.Sprintf("Signal: failed to fetch attachment %s: %v", att.ID, err))
			continue
		}
		urls, types = append(urls, path), append(types, mime)
	}
	return urls, types
}

// Downloads one attachment over JSON-RPC and caches it by sniffed type, returning its path and MIME type
func (a *SignalAdapter) fetchAttachment(ctx context.Context, att attachment) (string, string, error) {
	c := a.client()
	if c == nil {
		return "", "", errors.New("client not connected")
	}
	raw, err := c.call(ctx, "getAttachment", map[string]any{"account": a.account, "id": att.ID}, "", 0)
	if err != nil {
		return "", "", err
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) != nil {
		var wrapped struct {
			Data string `json:"data"`
		}
		if json.Unmarshal(raw, &wrapped) != nil || wrapped.Data == "" {
			return "", "", errors.New("attachment response missing data")
		}
		encoded = wrapped.Data
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", fmt.Errorf("attachment data is not base64: %w", err)
	}
	if len(data) > maxAttachmentBytes {
		return "", "", fmt.Errorf("attachment too large (%d bytes)", len(data))
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(att.ContentType, ";", 2)[0]))
	ext := guessExtension(data)
	if ext == ".aac" {
		if m4a, err := remuxADTSToM4A(data); err != nil {
			slog.Warn(fmt.Sprintf("Signal: AAC to M4A remux failed, caching raw AAC: %v", err))
		} else {
			data, ext, contentType = m4a, ".m4a", "audio/mp4"
		}
	}
	mime := contentType
	if mime == "" {
		mime = extToMIME(ext)
	}
	var path string
	switch {
	case isImageExt(ext):
		path, err = gateway.CacheImageFromBytesAsync(ctx, data, ext)
	case isAudioExt(ext) || strings.HasPrefix(contentType, "audio/"):
		path, err = gateway.CacheAudioFromBytesAsync(ctx, data, ext)
	case strings.HasPrefix(contentType, "video/") || contentType == "" && mediacache.SupportedVideoTypes[ext] != "":
		path, err = gateway.CacheVideoFromBytesAsync(ctx, data, ext)
	default:
		name := att.Filename
		if name == "" {
			name = "attachment" + ext
		}
		path, err = gateway.CacheDocumentFromBytesAsync(ctx, data, name)
	}
	if err != nil {
		return "", "", err
	}
	return path, mime, nil
}

// File extension sniffed from magic bytes, WEBP claimed before the audio sniffer since both use RIFF
func guessExtension(data []byte) string {
	for _, m := range []struct {
		magic []byte
		ext   string
	}{{[]byte("\x89PNG"), ".png"}, {[]byte{0xFF, 0xD8}, ".jpg"}, {[]byte("GIF8"), ".gif"}, {[]byte("%PDF"), ".pdf"}} {
		if bytes.HasPrefix(data, m.magic) {
			return m.ext
		}
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return ".webp"
	}
	if container, ok := mediacache.SniffContainer(data); ok {
		return mediacache.ContainerToExt[container]
	}
	if bytes.HasPrefix(data, []byte("PK")) {
		return ".zip"
	}
	return ".bin"
}

func isImageExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return true
	}
	return false
}

func isAudioExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".mp3", ".wav", ".ogg", ".m4a", ".aac":
		return true
	}
	return false
}

// Canonical MIME type of an extension, octet stream when unknown
func extToMIME(ext string) string {
	if mime, ok := map[string]string{".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
		".webp": "image/webp", ".ogg": "audio/ogg", ".mp3": "audio/mpeg", ".wav": "audio/wav", ".m4a": "audio/mp4",
		".aac": "audio/aac", ".mp4": "video/mp4", ".webm": "video/webm", ".flac": "audio/flac", ".pdf": "application/pdf",
		".zip": "application/zip"}[strings.ToLower(ext)]; ok {
		return mime
	}
	return "application/octet-stream"
}
