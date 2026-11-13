package photon

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Native tapback kinds and the emoji they stand for
var (
	emojiToTapback = map[string]string{"❤️": "love", "👍": "like", "👎": "dislike", "😂": "laugh", "‼️": "emphasize", "❓": "question"}
	tapbackToEmoji = map[string]string{"love": "❤️", "like": "👍", "dislike": "👎", "laugh": "😂", "emphasize": "‼️", "question": "❓"}
	vcardMimeTypes = map[string]bool{"text/vcard": true, "text/x-vcard": true, "text/directory": true, "application/vcard": true,
		"application/x-vcard": true}
	childIDRE = regexp.MustCompile(`^p:(\d+)/`)
)

const objectReplacement = "￼"

// Space of a message, its chat guid, dm or group, and the line phone it arrived on
type spaceRef struct {
	ID    string
	Type  string
	Phone string
}

// Attachment metadata whose bytes stay on the server until they are downloaded
type attachmentInfo struct {
	GUID     string
	Name     string
	MimeType string
	Size     int64
}

// Message content in the shapes the SDK surfaces, Type naming the variant
type content struct {
	Type        string
	Text        string
	URL         string
	Attachment  *attachmentInfo
	Items       []*smsg
	Inner       *content
	Target      *smsg
	Emoji       string
	OptionTitle string
	Selected    bool
	PollTitle   string
}

// One spectrum message, inbound from the stream or outbound from a send
type smsg struct {
	ID        string
	Direction string
	SenderID  string
	Space     spaceRef
	Timestamp time.Time
	Content   *content
	ParentID  string
	PartIndex *int
	line      *line
}

func chatTypeFromGUID(guid string) string {
	if strings.Contains(guid, ";+;") {
		return "group"
	}
	return "dm"
}

func formatChildID(part int, parent string) string { return fmt.Sprintf("p:%d/%s", part, parent) }

// Parent guid and part index of a p:N/guid child id
func parseChildID(id string) (string, int, bool) {
	m := childIDRE.FindStringSubmatch(id)
	if m == nil {
		return "", 0, false
	}
	part, err := strconv.Atoi(m[1])
	if err != nil {
		return "", 0, false
	}
	return id[len(m[0]):], part, true
}

// Apple mime of an attachment, a CAF voice memo reported as octet-stream corrected to audio/x-caf
func normalizedMime(a imAttachment) string {
	caf := strings.EqualFold(a.UTI, "com.apple.coreaudio-format") || strings.HasSuffix(strings.ToLower(a.FileName), ".caf")
	if strings.EqualFold(a.MimeType, "application/octet-stream") && caf {
		return "audio/x-caf"
	}
	return a.MimeType
}

func appleAudioMime(a imAttachment) string {
	if mime := normalizedMime(a); strings.HasPrefix(strings.ToLower(mime), "audio/") {
		return mime
	}
	return ""
}

func isVCardAttachment(mime, name string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	return (mime != "" && vcardMimeTypes[base]) || strings.HasSuffix(strings.ToLower(name), ".vcf")
}

// Whether text is a vCard the contact parser accepts
func parsesAsVCard(text string) bool {
	upper := strings.ToUpper(strings.TrimSpace(text))
	return strings.HasPrefix(upper, "BEGIN:VCARD") && strings.Contains(upper, "END:VCARD") && strings.Contains(upper, "VERSION:")
}

// One ordered bubble of a message, text or an attachment
type orderedPart struct {
	text       string
	attachment *imAttachment
}

// Bubble order of text and attachments, U+FFFC placeholders splitting the text around each attachment
func orderedParts(text string, attachments []imAttachment) []orderedPart {
	var parts []orderedPart
	addText := func(s string) {
		if t := strings.TrimSpace(s); t != "" {
			parts = append(parts, orderedPart{text: t})
		}
	}
	addAttachments := func() {
		for i := range attachments {
			parts = append(parts, orderedPart{attachment: &attachments[i]})
		}
	}
	if text == "" {
		addAttachments()
		return parts
	}
	if !strings.Contains(text, objectReplacement) {
		addAttachments()
		addText(text)
		return parts
	}
	segments := strings.Split(text, objectReplacement)
	for i := range attachments {
		if i < len(segments) {
			addText(segments[i])
		}
		parts = append(parts, orderedPart{attachment: &attachments[i]})
	}
	if len(segments) > len(attachments) {
		addText(strings.Join(segments[len(attachments):], ""))
	}
	return parts
}

// Message frame of a native message with its direction, sender and space
func (l *line) messageBase(m *imMessage, chatHint string, ts time.Time) smsg {
	chat := chatHint
	if chat == "" && len(m.ChatGUIDs) > 0 {
		chat = m.ChatGUIDs[0]
	}
	direction := "inbound"
	if m.IsFromMe {
		direction = "outbound"
	}
	sender := ""
	if m.Sender != nil {
		sender = m.Sender.Address
	}
	return smsg{Direction: direction, SenderID: sender, Space: spaceRef{ID: chat, Type: chatTypeFromGUID(chat), Phone: l.currentPhone()},
		Timestamp: ts, line: l}
}

// Spectrum message rebuilt from a fetched native message
func (l *line) rebuild(ctx context.Context, m *imMessage, chatHint string, visited map[string]bool) *smsg {
	ts := m.DateCreated
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return l.contentMessage(ctx, l.messageBase(m, chatHint, ts), m, visited)
}

func (l *line) attachmentContent(ctx context.Context, info imAttachment, voice bool) *content {
	if isVCardAttachment(info.MimeType, info.FileName) {
		data, _, err := l.download(ctx, info.GUID, 0)
		if err == nil && !parsesAsVCard(string(data)) {
			err = errors.New("attachment is not a parseable vCard")
		}
		if err == nil {
			return &content{Type: "contact"}
		}
		l.sess.tele.log(logWarn, "spectrum.imessage.inbound", "failed to parse vCard attachment; falling back to attachment content",
			withError(map[string]any{"spectrum.imessage.attachment.guid": info.GUID}, err))
	}
	if audio := appleAudioMime(info); voice && audio != "" {
		return &content{Type: "voice", Attachment: &attachmentInfo{GUID: info.GUID, Name: info.FileName, MimeType: audio, Size: info.TotalBytes}}
	}
	return &content{Type: "attachment", Attachment: &attachmentInfo{GUID: info.GUID, Name: info.FileName, MimeType: normalizedMime(info),
		Size: info.TotalBytes}}
}

// App card content when the balloon carries a parseable url and a visible text slot
func appCardContent(m *imMessage) *content {
	app := m.Content.MiniApp
	if app == nil || app.URL == "" {
		return nil
	}
	if u, err := url.Parse(app.URL); err != nil || u.Scheme == "" {
		return nil
	}
	var layout imMiniAppLayout
	if app.Layout != nil {
		layout = *app.Layout
	}
	caption := layout.Caption
	if caption == "" {
		caption = layout.Summary
	}
	if caption == "" {
		caption = app.AppName
	}
	if caption == "" && layout.Subcaption == "" && layout.TrailingCaption == "" && layout.TrailingSubcaption == "" {
		return nil
	}
	return &content{Type: "app", URL: app.URL}
}

// Message content without its reply wrapper, multi part bubbles becoming a group of child messages
func (l *line) unwrapped(ctx context.Context, base smsg, m *imMessage) *smsg {
	msg := base
	msg.ID = m.GUID
	attachments := m.Content.Attachments
	if len(attachments) == 0 {
		if card := appCardContent(m); card != nil {
			msg.Content = card
		} else if m.Content.Text != "" {
			msg.Content = &content{Type: "text", Text: m.Content.Text}
		} else {
			msg.Content = &content{Type: "custom"}
		}
		return &msg
	}
	voiceGUID := ""
	if m.IsAudioMessage {
		for _, a := range attachments {
			if appleAudioMime(a) != "" {
				voiceGUID = a.GUID
				break
			}
		}
	}
	parts := orderedParts(m.Content.Text, attachments)
	build := func(part orderedPart, id string, index int, parent string) *smsg {
		item := base
		item.ID, item.ParentID = id, parent
		idx := index
		item.PartIndex = &idx
		if part.attachment == nil {
			item.Content = &content{Type: "text", Text: part.text}
		} else {
			item.Content = l.attachmentContent(ctx, *part.attachment, part.attachment.GUID == voiceGUID)
		}
		return &item
	}
	switch len(parts) {
	case 0:
		msg.Content = &content{Type: "custom"}
		return &msg
	case 1:
		return build(parts[0], m.GUID, 0, "")
	}
	group := &content{Type: "group"}
	for i, part := range parts {
		group.Items = append(group.Items, build(part, formatChildID(i, m.GUID), i, m.GUID))
	}
	msg.Content = group
	return &msg
}

// Content message wrapped as a reply when the native message threads onto another
func (l *line) contentMessage(ctx context.Context, base smsg, m *imMessage, visited map[string]bool) *smsg {
	msg := l.unwrapped(ctx, base, m)
	target := m.ReplyTargetGUID
	if target == "" {
		target = m.ThreadOriginatorGUID
	}
	if target == "" {
		return msg
	}
	resolved := l.replyTarget(ctx, base, target, m.GUID, visited)
	msg.Content = &content{Type: "reply", Inner: msg.Content, Target: resolved}
	return msg
}

// Reply target from the cache or the server, a stub when it loops or cannot be fetched
func (l *line) replyTarget(ctx context.Context, base smsg, targetGUID, currentGUID string, visited map[string]bool) *smsg {
	stub := &smsg{ID: targetGUID, Content: &content{Type: "custom"}, Space: base.Space, line: l}
	if targetGUID == currentGUID || visited[targetGUID] {
		return stub
	}
	if cached, ok := l.cache.get(targetGUID); ok {
		return cached
	}
	next := map[string]bool{currentGUID: true}
	for k := range visited {
		next[k] = true
	}
	fetched, err := l.getMessage(ctx, targetGUID)
	if err != nil {
		if re := asRPCError(err); re == nil || !re.notFound() {
			l.sess.tele.log(logWarn, "spectrum.imessage.inbound", "failed to resolve iMessage reply target; falling back to stub target",
				withError(map[string]any{"spectrum.imessage.message.guid": currentGUID, "spectrum.imessage.reply.target_guid": targetGUID}, err))
		}
		return stub
	}
	rebuilt := l.rebuild(ctx, fetched, base.Space.ID, next)
	l.cacheMessage(rebuilt)
	return rebuilt
}

// Caches a message and every child item of its group under their ids
func (l *line) cacheMessage(m *smsg) {
	l.cache.put(m.ID, m)
	group := m.Content
	if group != nil && group.Type == "reply" {
		group = group.Inner
	}
	if group != nil && group.Type == "group" {
		for _, item := range group.Items {
			l.cache.put(item.ID, item)
		}
	}
}

// Event target from the cache or one fetch and rebuild, nil when it cannot be resolved
func (l *line) resolveTarget(ctx context.Context, chat, guid string) *smsg {
	if cached, ok := l.cache.get(guid); ok {
		return cached
	}
	fetched, err := l.getMessage(ctx, guid)
	if err != nil {
		return nil
	}
	rebuilt := l.rebuild(ctx, fetched, chat, nil)
	l.cacheMessage(rebuilt)
	return rebuilt
}

// Message for an id, child ids resolving to the item of their rebuilt parent
func (l *line) lookupMessage(ctx context.Context, chat, id string) (*smsg, error) {
	if cached, ok := l.cache.get(id); ok {
		return cached, nil
	}
	guid, part, child := parseChildID(id)
	if !child {
		guid = id
	}
	fetched, err := l.getMessage(ctx, guid)
	if err != nil {
		if re := asRPCError(err); re != nil && re.notFound() {
			return nil, nil
		}
		return nil, err
	}
	rebuilt := l.rebuild(ctx, fetched, chat, nil)
	l.cacheMessage(rebuilt)
	if !child {
		return rebuilt, nil
	}
	group := rebuilt.Content
	if group != nil && group.Type == "reply" {
		group = group.Inner
	}
	if group == nil || group.Type != "group" || part >= len(group.Items) {
		return nil, nil
	}
	return group.Items[part], nil
}

// Text of a reaction or reply target, plain text, a link url or the first text child of a group
func targetText(target *smsg) string {
	if target == nil || target.Content == nil {
		return ""
	}
	c := target.Content
	text := ""
	switch c.Type {
	case "text", "markdown":
		text = c.Text
	case "richlink":
		text = c.URL
	case "group":
		for _, item := range c.Items {
			if item.Content == nil {
				continue
			}
			if item.Content.Type == "text" && item.Content.Text != "" {
				text = item.Content.Text
				break
			}
			if item.Content.Type == "richlink" && item.Content.URL != "" {
				text = item.Content.URL
				break
			}
		}
	}
	if r := []rune(text); len(r) > reactionTargetTextCap {
		text = string(r[:reactionTargetTextCap])
	}
	return text
}

// Direction a target surfaces with, inbound when the record carries none
func targetDirection(target *smsg) string {
	if target == nil || target.Direction == "" {
		return "inbound"
	}
	return target.Direction
}

// Stream item of one durable event, its cursor, dedup id and the messages it surfaces
type streamItem struct {
	cursor string
	id     string
	values []*smsg
}

// Whether an event came from this line itself
func (l *line) fromCurrentAccount(isFromMe bool, actor *imAddress) bool {
	phone := l.currentPhone()
	return isFromMe || (phone != sharedPhone && actor != nil && actor.Address != "" && actor.Address == phone)
}

// Messages a message stream event surfaces, flattening multi part bubbles into their items
func (l *line) messageItem(ctx context.Context, e *messageEvent, seq uint64) streamItem {
	cursor := strconv.FormatUint(seq, 10)
	switch e.Kind {
	case "message.received":
		if e.Message == nil {
			return streamItem{cursor: cursor, id: fmt.Sprintf("unmappable:%s", cursor)}
		}
		if e.Message.IsFromMe {
			return streamItem{cursor: cursor, id: e.Message.GUID}
		}
		ts := e.OccurredAt
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		msg := l.contentMessage(ctx, l.messageBase(e.Message, e.ChatGUID, ts), e.Message, nil)
		l.cacheMessage(msg)
		if len(e.Message.ChatGUIDs) > 0 {
			l.contacts.onInbound(e.Message.ChatGUIDs[0])
		}
		return streamItem{cursor: cursor, id: e.Message.GUID, values: []*smsg{msg}}
	case "message.reactionAdded":
		id := fmt.Sprintf("%s:reaction:%d", e.MessageGUID, seq)
		if l.fromCurrentAccount(e.IsFromMe, e.Actor) {
			return streamItem{cursor: cursor, id: id}
		}
		return streamItem{cursor: cursor, id: id, values: l.reactionMessages(ctx, e, seq)}
	case "message.read":
		return streamItem{cursor: cursor, id: fmt.Sprintf("%s:read:%d", e.MessageGUID, seq)}
	}
	guid := e.MessageGUID
	if guid == "" {
		guid = "unknown"
	}
	return streamItem{cursor: cursor, id: fmt.Sprintf("%s:%s:%d", e.Kind, guid, seq)}
}

// Inbound reaction message whose target is resolved, nothing when the tapback has no emoji or actor
func (l *line) reactionMessages(ctx context.Context, e *messageEvent, seq uint64) []*smsg {
	emoji := e.Reaction.Emoji
	if e.Reaction.Kind != "emoji" {
		emoji = tapbackToEmoji[e.Reaction.Kind]
	}
	if emoji == "" || e.Actor == nil || e.Actor.Address == "" {
		return nil
	}
	target := l.resolveTarget(ctx, e.ChatGUID, e.MessageGUID)
	if target == nil {
		return nil
	}
	if target.Content != nil && target.Content.Type == "group" {
		part := 0
		if e.PartIndex != nil {
			part = int(*e.PartIndex)
		}
		if part >= 0 && part < len(target.Content.Items) {
			target = target.Content.Items[part]
		}
	}
	suffix := ""
	if e.PartIndex != nil {
		suffix = fmt.Sprintf(":%d", *e.PartIndex)
	}
	return []*smsg{{ID: fmt.Sprintf("%s:reaction:%d%s", e.MessageGUID, seq, suffix), Direction: "inbound", SenderID: e.Actor.Address,
		Space: spaceRef{ID: e.ChatGUID, Type: chatTypeFromGUID(e.ChatGUID), Phone: l.currentPhone()}, Timestamp: e.OccurredAt,
		Content: &content{Type: "reaction", Emoji: emoji, Target: target}, line: l}}
}

// Poll vote messages, the poll metadata cached from creation events or fetched on demand
func (l *line) pollItem(ctx context.Context, e *pollEvent, seq uint64) streamItem {
	cursor := strconv.FormatUint(seq, 10)
	item := streamItem{cursor: cursor, id: fmt.Sprintf("%s:poll:%d", e.PollGUID, seq)}
	polls := l.sess.polls
	if e.Kind == "created" || e.Kind == "optionAdded" {
		polls.put(e.PollGUID, newCachedPoll(e.Title, e.Options))
	}
	if l.fromCurrentAccount(e.IsFromMe, e.Actor) || (e.Kind != "voted" && e.Kind != "unvoted") {
		return item
	}
	if e.Actor == nil || e.Actor.Address == "" || e.OptionID == "" {
		return item
	}
	fetch := func() *cachedPoll {
		info, err := l.getPoll(ctx, e.PollGUID)
		if err != nil {
			l.sess.tele.log(logError, "spectrum.imessage.poll", "failed to resolve poll",
				withError(map[string]any{"spectrum.imessage.poll.guid": e.PollGUID}, err))
			return nil
		}
		p := newCachedPoll(info.Title, info.Options)
		polls.put(info.MessageGUID, p)
		return p
	}
	poll, ok := polls.get(e.PollGUID)
	if !ok {
		if poll = fetch(); poll == nil {
			return item
		}
	}
	if _, known := poll.byID[e.OptionID]; !known {
		if refreshed := fetch(); refreshed != nil {
			poll = refreshed
		}
	}
	title, known := poll.byID[e.OptionID]
	if !known {
		return item
	}
	action := "deselected"
	if e.Kind == "voted" {
		action = "selected"
	}
	item.values = []*smsg{{ID: fmt.Sprintf("%s:%s:%s:%s:%d", e.PollGUID, e.Actor.Address, e.OptionID, action, e.OccurredAt.UnixMilli()),
		Direction: "inbound", SenderID: e.Actor.Address, Timestamp: e.OccurredAt, line: l,
		Space:   spaceRef{ID: e.ChatGUID, Type: chatTypeFromGUID(e.ChatGUID), Phone: l.currentPhone()},
		Content: &content{Type: "poll_option", OptionTitle: title, Selected: e.Kind == "voted", PollTitle: poll.title}}}
	return item
}

// Group membership, rename and icon changes as inbound messages, the leaver acting for a leave
func (l *line) groupItem(ctx context.Context, e *groupEvent, seq uint64) streamItem {
	item := streamItem{cursor: strconv.FormatUint(seq, 10), id: fmt.Sprintf("%s:group:%d", e.ChatGUID, seq)}
	actor := e.Actor
	if e.Kind == "participantLeft" {
		actor = e.Participant
	}
	if l.fromCurrentAccount(e.IsFromMe, actor) {
		return item
	}
	var c *content
	switch e.Kind {
	case "participantAdded", "participantRemoved":
		if e.Participant != nil && e.Participant.Address != "" {
			c = &content{Type: map[string]string{"participantAdded": "addMember", "participantRemoved": "removeMember"}[e.Kind]}
		}
	case "participantLeft":
		c = &content{Type: "leaveSpace"}
	case "displayNameChanged":
		if e.DisplayName != "" {
			c = &content{Type: "rename"}
		}
	case "iconChanged":
		data, _, err := l.getIcon(ctx, e.ChatGUID)
		if err != nil {
			l.sess.tele.log(logError, "spectrum.imessage.group", "failed to fetch changed group icon",
				withError(map[string]any{"spectrum.imessage.group.chat": e.ChatGUID}, err))
		} else if data != nil {
			c = &content{Type: "avatar"}
		}
	case "iconRemoved":
		c = &content{Type: "avatar"}
	}
	if c == nil {
		return item
	}
	sender := ""
	if actor != nil {
		sender = actor.Address
	}
	item.values = []*smsg{{ID: item.id, Direction: "inbound", SenderID: sender, Timestamp: e.OccurredAt, Content: c, line: l,
		Space: spaceRef{ID: e.ChatGUID, Type: chatTypeFromGUID(e.ChatGUID), Phone: l.currentPhone()}}}
	return item
}
