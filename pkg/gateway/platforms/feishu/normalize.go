package feishu

import (
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	fallbackPostText        = "[Rich text message]"
	fallbackForwardText     = "[Merged forward message]"
	fallbackShareChatText   = "[Shared chat]"
	fallbackInteractiveText = "[Interactive message]"
	fallbackImageText       = "[Image]"
	fallbackAttachmentText  = "[Attachment]"
)

var (
	markdownHintRe = pyre.MustCompile(`(?m)(^\|.*\|\s*\n\|[-:|\s]+\|)|(^#{1,6}\s)|(^\s*[-*]\s)|(^\s*\d+\.\s)|(^\s*---+\s*$)|(` +
		"```" + `)|(` + "`[^`\\n]+`" + `)|(\*\*[^*\n].+?\*\*)|(~~[^~\n].+?~~)|(<u>.+?</u>)|(\*[^*\n]+\*)|(\[[^\]]+\]\([^)]+\))|(^>\s)`)
	markdownLinkRe         = pyre.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	markdownFenceOpenRe    = pyre.MustCompile("^```([^\\n`]*)\\s*$")
	markdownFenceCloseRe   = pyre.MustCompile("^```\\s*$")
	multispaceRe           = pyre.MustCompile(`[ \t]{2,}`)
	postContentInvalidRe   = pyre.MustCompile(`(?i)content format of the post type is incorrect`)
	markdownSpecialCharsRe = pyre.MustCompile(`([\\` + "`" + `*_{}\[\]()#+\-!|>~])`)
	mentionTokenRe         = pyre.MustCompile(`@_user_\d+`)
	whitespaceRe           = pyre.MustCompile(`\s+`)
	backtickRunRe          = pyre.MustCompile("`+")
	blockquoteRe           = pyre.MustCompile(`(?m)^>\s?`)
	hrLineRe               = pyre.MustCompile(`(?m)^\s*---+\s*$`)
	strikeRe               = pyre.MustCompile(`~~([^~\n]+)~~`)
	underlineRe            = pyre.MustCompile(`<u>([\s\S]*?)</u>`)
)

const mentionBoundaryChars = " \t\n\r.,;:!?、，。；：！？()[]{}<>\"'`"
const trailingTerminalPunct = " \t\n\r.!?。！？"

var preferredLocales = []string{"zh_cn", "en_us"}

var supportedCardTextKeys = []string{"title", "text", "content", "label", "value", "name", "summary", "subtitle", "description", "marker", "hint"}

var richBlockTags = map[string]bool{"plain_text": true, "lark_md": true, "markdown": true, "note": true, "div": true, "column_set": true,
	"column": true, "action": true, "button": true, "select_static": true, "date_picker": true}

var skipTextKeys = map[string]bool{"tag": true, "type": true, "msg_type": true, "message_type": true, "chat_id": true, "open_chat_id": true,
	"share_chat_id": true, "file_key": true, "image_key": true, "user_id": true, "open_id": true, "union_id": true, "url": true, "href": true,
	"link": true, "token": true, "template": true, "locale": true}

// FeishuPostMediaRef is a file, audio or video resource referenced by a message
type FeishuPostMediaRef struct {
	FileKey      string
	FileName     string
	ResourceType string
}

// FeishuMentionRef is one resolved mention of a message
type FeishuMentionRef struct {
	Name   string
	OpenID string
	IsAll  bool
	IsSelf bool
}

type feishuBotIdentity struct {
	OpenID string
	UserID string
	Name   string
}

// Reports a self match by open_id first, user_id next and name last when either side lacks the id
func (b feishuBotIdentity) matches(openID, userID, name string) bool {
	if openID != "" && b.OpenID != "" {
		return openID == b.OpenID
	}
	if userID != "" && b.UserID != "" {
		return userID == b.UserID
	}
	return b.Name != "" && name == b.Name
}

// FeishuPostParseResult is the rendered text and resource keys of a post payload
type FeishuPostParseResult struct {
	TextContent string
	ImageKeys   []string
	MediaRefs   []FeishuPostMediaRef
}

// FeishuNormalizedMessage is an inbound message reduced to text, resources, mentions and relation metadata
type FeishuNormalizedMessage struct {
	RawType              string
	TextContent          string
	PreferredMessageType string
	ImageKeys            []string
	MediaRefs            []FeishuPostMediaRef
	Mentions             []FeishuMentionRef
	RelationKind         string
	Metadata             map[string]any
}

func newNormalized(rawType, text string) FeishuNormalizedMessage {
	return FeishuNormalizedMessage{RawType: rawType, TextContent: text, PreferredMessageType: "text", RelationKind: "plain", Metadata: map[string]any{}}
}

type mentionsMap = pydict.Dict[string, FeishuMentionRef]

// Mention as the event payload or message.get carries it, the string id form with its id type or the nested id object
type rawMention struct {
	Key    string
	Name   string
	IDText *string
	IDType string
	IDs    *larkim.UserId
}

func sdkStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Mentions of a receive event in payload order
func mentionsFromEvent(items []*larkim.MentionEvent) []rawMention {
	out := make([]rawMention, 0, len(items))
	for _, m := range items {
		if m == nil {
			continue
		}
		out = append(out, rawMention{Key: sdkStr(m.Key), Name: sdkStr(m.Name), IDs: m.Id})
	}
	return out
}

// Mentions of a message.get item in payload order
func mentionsFromMessage(items []*larkim.Mention) []rawMention {
	out := make([]rawMention, 0, len(items))
	for _, m := range items {
		if m == nil {
			continue
		}
		out = append(out, rawMention{Key: sdkStr(m.Key), Name: sdkStr(m.Name), IDText: m.Id, IDType: sdkStr(m.IdType)})
	}
	return out
}

// Reports a bot or app sender type
func isBotSender(sender *larkim.EventSender) bool {
	if sender == nil {
		return false
	}
	t := sdkStr(sender.SenderType)
	return t == "bot" || t == "app"
}

// Non empty open, user and union ids of a sender
func senderIdentity(sender *larkim.EventSender) map[string]bool {
	out := map[string]bool{}
	if sender == nil || sender.SenderId == nil {
		return out
	}
	for _, v := range []*string{sender.SenderId.OpenId, sender.SenderId.UserId, sender.SenderId.UnionId} {
		if sdkStr(v) != "" {
			out[*v] = true
		}
	}
	return out
}

// Python re.sub with a function replacement
func reSub(re *regexp2.Regexp, s string, fn func(m regexp2.Match) string) string {
	out, err := re.ReplaceFunc(s, fn, -1, -1)
	if err != nil {
		return s
	}
	return out
}

// Python re.sub with group one as the whole replacement
func reSubGroup(re *regexp2.Regexp, s string) string {
	return reSub(re, s, func(m regexp2.Match) string { return m.GroupByNumber(1).String() })
}

func reSearch(re *regexp2.Regexp, s string) bool {
	ok, err := re.MatchString(s)
	return err == nil && ok
}

// Python re.match anchored at the start of s
func reMatchStart(re *regexp2.Regexp, s string) bool {
	m, err := re.FindStringMatch(s)
	return err == nil && m != nil && m.RuneIndex == 0
}

// Backslash escapes for markdown control characters
func escapeMarkdownText(text string) string {
	return reSub(markdownSpecialCharsRe, text, func(m regexp2.Match) string { return `\` + m.String() })
}

// Reports True, 1 or the string true
func toBoolean(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	case nil:
		return false
	}
	return pyrepr.Equal(value, int64(1))
}

func isStyleEnabled(style *pyjson.Object, key string) bool {
	if style == nil || style.Len() == 0 {
		return false
	}
	return toBoolean(dget(style, key))
}

// Inline code fenced by one more backtick than the longest run inside, padded when it touches a backtick
func wrapInlineCode(text string) string {
	maxRun := 0
	if m, _ := backtickRunRe.FindStringMatch(text); m != nil {
		for ; m != nil; m, _ = backtickRunRe.FindNextMatch(m) {
			maxRun = max(maxRun, m.RuneLength)
		}
	}
	fence := strings.Repeat("`", maxRun+1)
	body := text
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		body = " " + text + " "
	}
	return fence + body + fence
}

func sanitizeFenceLanguage(language string) string {
	return strings.ReplaceAll(strings.ReplaceAll(pystr.Strip(language), "\n", " "), "\r", " ")
}

var textStyleWrappers = [][3]string{{"bold", "**", "**"}, {"italic", "*", "*"}, {"underline", "<u>", "</u>"}, {"strikethrough", "~~", "~~"}}

// Markdown for a post text element from its raw text and style flags
func renderTextElement(element *pyjson.Object) string {
	text := strOr(dgetDef(element, "text", ""))
	styleDict, _ := dget(element, "style").(*pyjson.Object)
	if isStyleEnabled(styleDict, "code") {
		return wrapInlineCode(text)
	}
	rendered := text
	if rendered == "" {
		return ""
	}
	for _, w := range textStyleWrappers {
		if isStyleEnabled(styleDict, w[0]) {
			rendered = w[1] + rendered + w[2]
		}
	}
	return rendered
}

// Fenced code block of a post code element
func renderCodeBlockElement(element *pyjson.Object) string {
	language := strOr(dgetDef(element, "language", ""))
	if language == "" {
		language = strOr(dgetDef(element, "lang", ""))
	}
	language = sanitizeFenceLanguage(language)
	code := strOr(dgetDef(element, "text", ""))
	if code == "" {
		code = strOr(dgetDef(element, "content", ""))
	}
	code = strings.ReplaceAll(code, "\r\n", "\n")
	trailing := "\n"
	if strings.HasSuffix(code, "\n") {
		trailing = ""
	}
	return "```" + language + "\n" + code + trailing + "```"
}

// Plain text of markdown with blockquotes, strikethrough, underline and rules flattened too
func stripMarkdownToPlainText(text string) string {
	plain := strings.ReplaceAll(text, "\r\n", "\n")
	plain = reSub(markdownLinkRe, plain, func(m regexp2.Match) string {
		return m.GroupByNumber(1).String() + " (" + pystr.Strip(m.GroupByNumber(2).String()) + ")"
	})
	plain = reSub(blockquoteRe, plain, func(regexp2.Match) string { return "" })
	plain = reSub(hrLineRe, plain, func(regexp2.Match) string { return "---" })
	plain = reSubGroup(strikeRe, plain)
	plain = reSubGroup(underlineRe, plain)
	return gateway.StripMarkdown(plain, false)
}

// Python int of a value when at least minValue, def otherwise
func coerceInt(value any, def *int, minValue int) *int {
	n, err := pyint.Int64(value)
	if err != nil {
		return def
	}
	if n >= int64(minValue) {
		v := int(n)
		return &v
	}
	return def
}

func coerceRequiredInt(value any, def, minValue int) int {
	parsed := coerceInt(value, &def, minValue)
	if parsed == nil {
		return def
	}
	return *parsed
}

// Post content JSON of markdown under the zh_cn locale
func buildMarkdownPostPayload(content string) string {
	rows := buildMarkdownPostRows(content)
	locale := pyjson.NewObject()
	locale.Set("content", rows)
	root := pyjson.NewObject()
	root.Set("zh_cn", locale)
	out, _ := pyjson.Dumps(root, pyjson.EnsureASCII(false))
	return out
}

func mdRow(text string) []any {
	el := pyjson.NewObject()
	el.Set("tag", "md")
	el.Set("text", text)
	return []any{el}
}

// Post rows of markdown, each fenced code block in its own row
func buildMarkdownPostRows(content string) []any {
	if content == "" {
		return []any{mdRow("")}
	}
	if !strings.Contains(content, "```") {
		return []any{mdRow(content)}
	}
	var rows []any
	var current []string
	inCode := false
	flush := func() {
		segment := strings.Join(current, "\n")
		if pystr.Strip(segment) != "" {
			rows = append(rows, mdRow(segment))
		}
		current = nil
	}
	for _, raw := range pystr.Splitlines(content, false) {
		fenceRe := markdownFenceOpenRe
		if inCode {
			fenceRe = markdownFenceCloseRe
		}
		isFence := reMatchStart(fenceRe, pystr.Strip(raw))
		if isFence && !inCode {
			flush()
		}
		current = append(current, raw)
		if isFence {
			inCode = !inCode
			if !inCode {
				flush()
			}
		}
	}
	flush()
	if len(rows) == 0 {
		return []any{mdRow(content)}
	}
	return rows
}

// Rendered text, image keys and media refs of a post payload
func parseFeishuPostPayload(payload any, mentions *mentionsMap) FeishuPostParseResult {
	resolved := resolvePostPayload(payload)
	if resolved == nil {
		return FeishuPostParseResult{TextContent: fallbackPostText}
	}
	var imageKeys []string
	var mediaRefs []FeishuPostMediaRef
	var parts []string
	if title := normalizeFeishuText(pystr.Strip(pyrepr.Str(dgetDef(resolved, "title", ""))), nil); title != "" {
		parts = append(parts, title)
	}
	rowsVal := dgetDef(resolved, "content", []any{})
	rows, _ := rowsVal.([]any)
	for _, row := range rows {
		items, ok := row.([]any)
		if !ok {
			continue
		}
		var b strings.Builder
		for _, item := range items {
			b.WriteString(renderPostElement(item, &imageKeys, &mediaRefs, mentions))
		}
		if rowText := normalizeFeishuText(b.String(), nil); rowText != "" {
			parts = append(parts, rowText)
		}
	}
	files, _ := dgetDef(resolved, "files", []any{}).([]any)
	for _, entry := range files {
		e, ok := entry.(*pyjson.Object)
		if !ok || toBoolean(dget(e, "is_folder")) || pystr.Strip(pyrepr.Str(dgetDef(e, "file_key", ""))) == "" {
			continue
		}
		withTag := pyjson.NewObject()
		for _, k := range e.Keys {
			v, _ := e.Get(k)
			withTag.Set(k, v)
		}
		withTag.Set("tag", "file")
		if marker := renderPostElement(withTag, &imageKeys, &mediaRefs, mentions); marker != "" {
			parts = append(parts, marker)
		}
	}
	text := pystr.Strip(strings.Join(parts, "\n"))
	if text == "" {
		text = fallbackPostText
	}
	return FeishuPostParseResult{TextContent: text, ImageKeys: imageKeys, MediaRefs: mediaRefs}
}

// Post body of a payload given directly, under post, or under a locale key
func resolvePostPayload(payload any) *pyjson.Object {
	if direct := toPostPayload(payload); direct != nil {
		return direct
	}
	d, ok := payload.(*pyjson.Object)
	if !ok {
		return nil
	}
	if r := resolveLocalePayload(dget(d, "post")); r != nil {
		return r
	}
	return resolveLocalePayload(d)
}

// Post body of the preferred locales first, then of any locale carrying a content list
func resolveLocalePayload(payload any) *pyjson.Object {
	if direct := toPostPayload(payload); direct != nil {
		return direct
	}
	d, ok := payload.(*pyjson.Object)
	if !ok {
		return nil
	}
	for _, key := range preferredLocales {
		if c := toPostPayload(dget(d, key)); c != nil {
			return c
		}
	}
	for _, k := range d.Keys {
		v, _ := d.Get(k)
		if c := toPostPayload(v); c != nil {
			return c
		}
	}
	return nil
}

// Title, content and files of a candidate holding a content list, nil for anything else
func toPostPayload(candidate any) *pyjson.Object {
	d, ok := candidate.(*pyjson.Object)
	if !ok {
		return nil
	}
	content, ok := dget(d, "content").([]any)
	if !ok {
		return nil
	}
	files, ok := dget(d, "files").([]any)
	if !ok {
		files = []any{}
	}
	out := pyjson.NewObject()
	out.Set("title", strOr(dgetDef(d, "title", "")))
	out.Set("content", content)
	out.Set("files", files)
	return out
}

var staticPostTags = map[string]string{"br": "\n", "hr": "\n\n---\n\n", "divider": "\n\n---\n\n"}

func strippedField(el *pyjson.Object, key string) string {
	return pystr.Strip(pyrepr.Str(dgetDef(el, key, "")))
}

// Markdown of one post element, recording image keys, media refs and an at all mention it meets
func renderPostElement(element any, imageKeys *[]string, mediaRefs *[]FeishuPostMediaRef, mentions *mentionsMap) string {
	if s, ok := element.(string); ok {
		return s
	}
	el, ok := element.(*pyjson.Object)
	if !ok {
		return ""
	}
	tag := pystr.Lower(strippedField(el, "tag"))
	if s, ok := staticPostTags[tag]; ok {
		return s
	}
	switch tag {
	case "text":
		return renderTextElement(el)
	case "code_block", "pre":
		return renderCodeBlockElement(el)
	case "a":
		href := strippedField(el, "href")
		label := pystr.Strip(strOr(dgetDef(el, "text", href)))
		if label == "" {
			return ""
		}
		escaped := escapeMarkdownText(label)
		if href != "" {
			return "[" + escaped + "](" + href + ")"
		}
		return escaped
	case "at":
		marker := strippedField(el, "user_id")
		if marker == "@_all" {
			if mentions != nil && !mentions.Has("@_all") {
				mentions.Set("@_all", FeishuMentionRef{IsAll: true})
			}
			return "@all"
		}
		var ref FeishuMentionRef
		found := false
		if mentions != nil {
			ref, found = mentions.Get(marker)
		}
		display := ""
		if found {
			display = firstNonEmpty(ref.Name, ref.OpenID, "user")
		} else {
			display = firstNonEmpty(strippedField(el, "user_name"), "user")
		}
		return "@" + escapeMarkdownText(display)
	case "img", "image":
		imageKey := strippedField(el, "image_key")
		if imageKey != "" && !containsString(*imageKeys, imageKey) {
			*imageKeys = append(*imageKeys, imageKey)
		}
		alt := firstNonEmpty(strippedField(el, "text"), strippedField(el, "alt"))
		if alt != "" {
			return "[Image: " + alt + "]"
		}
		return "[Image]"
	case "media", "file", "audio", "video":
		fileKey := strippedField(el, "file_key")
		fileName := firstNonEmpty(strippedField(el, "file_name"), strippedField(el, "title"), strippedField(el, "text"))
		marker := "[Attachment]"
		if fileName != "" {
			marker = "[Attachment: " + fileName + "]"
		}
		if fileKey == "" {
			return marker
		}
		for _, r := range *mediaRefs {
			if r.FileKey == fileKey {
				return ""
			}
		}
		resourceType := "file"
		if tag == "audio" || tag == "video" {
			resourceType = tag
		}
		*mediaRefs = append(*mediaRefs, FeishuPostMediaRef{FileKey: fileKey, FileName: fileName, ResourceType: resourceType})
		return marker
	case "emotion", "emoji":
		label := firstNonEmpty(strippedField(el, "text"), strippedField(el, "emoji_type"))
		if label != "" {
			return ":" + escapeMarkdownText(label) + ":"
		}
		return "[Emoji]"
	case "code":
		code := strOr(dgetDef(el, "text", ""))
		if code == "" {
			code = strOr(dgetDef(el, "content", ""))
		}
		if code == "" {
			return ""
		}
		return wrapInlineCode(code)
	}
	var nested []any
	for _, key := range []string{"text", "title", "content", "children", "elements"} {
		nested = append(nested, dget(el, key))
	}
	return joinNestedPosts(nested, imageKeys, mediaRefs, mentions)
}

func joinNestedPosts(values []any, imageKeys *[]string, mediaRefs *[]FeishuPostMediaRef, mentions *mentionsMap) string {
	var parts []string
	for _, item := range values {
		if part := renderNestedPost(item, imageKeys, mediaRefs, mentions); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " ")
}

// Escaped text of a nested string, joined renders of a list, or the element or value render of a dict
func renderNestedPost(value any, imageKeys *[]string, mediaRefs *[]FeishuPostMediaRef, mentions *mentionsMap) string {
	switch v := value.(type) {
	case string:
		return escapeMarkdownText(v)
	case []any:
		return joinNestedPosts(v, imageKeys, mediaRefs, mentions)
	case *pyjson.Object:
		if direct := renderPostElement(v, imageKeys, mediaRefs, mentions); direct != "" {
			return direct
		}
		return joinNestedPosts(objectValues(v), imageKeys, mediaRefs, mentions)
	}
	return ""
}

// Normalized text, resources and mentions of an inbound message by its type
func normalizeFeishuMessage(messageType, rawContent string, mentions []rawMention, bot feishuBotIdentity) FeishuNormalizedMessage {
	normalizedType := pystr.Lower(pystr.Strip(messageType))
	payload := loadFeishuPayload(rawContent)
	mm := buildMentionsMap(mentions, bot)
	switch normalizedType {
	case "text":
		text := strOr(dgetDef(payload, "text", ""))
		if strings.Contains(text, "@_all") && !mm.Has("@_all") {
			mm.Set("@_all", FeishuMentionRef{IsAll: true})
		}
		out := newNormalized(normalizedType, normalizeFeishuText(text, mm))
		out.Mentions = mm.Values()
		return out
	case "post":
		parsed := parseFeishuPostPayload(payload, mm)
		out := newNormalized(normalizedType, parsed.TextContent)
		out.ImageKeys, out.MediaRefs, out.Mentions, out.RelationKind = parsed.ImageKeys, parsed.MediaRefs, mm.Values(), "post"
		return out
	}
	mentionRefs := mm.Values()
	switch normalizedType {
	case "image":
		imageKey := pystr.Strip(strOr(dgetDef(payload, "image_key", "")))
		alt := firstNonEmpty(strOr(dgetDef(payload, "text", "")), strOr(dgetDef(payload, "alt", "")), fallbackImageText)
		altText := normalizeFeishuText(alt, mm)
		text := altText
		if altText == fallbackImageText {
			text = ""
		}
		out := newNormalized(normalizedType, text)
		out.PreferredMessageType, out.RelationKind, out.Mentions = "photo", "image", mentionRefs
		if imageKey != "" {
			out.ImageKeys = []string{imageKey}
		}
		return out
	case "file", "audio", "media":
		ref := buildMediaRefFromPayload(payload, normalizedType)
		out := newNormalized(normalizedType, "")
		out.PreferredMessageType = "document"
		if normalizedType == "audio" {
			out.PreferredMessageType = "audio"
		}
		if ref.FileKey != "" {
			out.MediaRefs = []FeishuPostMediaRef{ref}
		}
		out.RelationKind, out.Mentions = normalizedType, mentionRefs
		out.Metadata = map[string]any{"placeholder_text": attachmentLabel(ref.FileName)}
		return out
	case "merge_forward":
		return normalizeMergeForwardMessage(payload)
	case "share_chat":
		return normalizeShareChatMessage(payload)
	case "interactive", "card":
		return normalizeInteractiveMessage(normalizedType, payload)
	}
	return newNormalized(normalizedType, "")
}

// Decoded JSON object of message content, the raw text under text when it is not JSON, a non object under content
func loadFeishuPayload(rawContent string) *pyjson.Object {
	if rawContent == "" {
		return pyjson.NewObject()
	}
	parsed, err := pyjson.Loads(rawContent)
	if err != nil {
		out := pyjson.NewObject()
		out.Set("text", rawContent)
		return out
	}
	if d, ok := parsed.(*pyjson.Object); ok {
		return d
	}
	out := pyjson.NewObject()
	out.Set("content", parsed)
	return out
}

// Title plus the first eight forwarded entries of a merged forward
func normalizeMergeForwardMessage(payload *pyjson.Object) FeishuNormalizedMessage {
	title := firstTextField(payload, []string{"title", "summary", "preview"}, []string{"title", "summary", "preview", "description"})
	entries := collectForwardEntries(payload)
	var lines []string
	if title != "" {
		lines = append(lines, title)
	}
	lines = append(lines, entries[:min(8, len(entries))]...)
	text := pystr.Strip(strings.Join(lines, "\n"))
	if text == "" {
		text = fallbackForwardText
	}
	out := newNormalized("merge_forward", text)
	out.RelationKind = "merge_forward"
	out.Metadata = map[string]any{"entry_count": len(entries), "title": title}
	return out
}

// Shared chat name and id lines of a share_chat message
func normalizeShareChatMessage(payload *pyjson.Object) FeishuNormalizedMessage {
	chatName := firstTextField(payload, []string{"chat_name", "name", "title"}, []string{"chat_name", "name", "title"})
	shareID := firstTextField(payload, []string{"chat_id", "open_chat_id", "share_chat_id"}, nil)
	lines := []string{fallbackShareChatText}
	if chatName != "" {
		lines[0] = "Shared chat: " + chatName
	}
	if shareID != "" {
		lines = append(lines, "Chat ID: "+shareID)
	}
	out := newNormalized("share_chat", strings.Join(lines, "\n"))
	out.RelationKind = "share_chat"
	out.Metadata = map[string]any{"chat_id": shareID, "chat_name": chatName}
	return out
}

// Title, text lines and action labels of an interactive card, capped at twelve lines
func normalizeInteractiveMessage(messageType string, payload *pyjson.Object) FeishuNormalizedMessage {
	var cardPayload any = payload
	if card, ok := dget(payload, "card").(*pyjson.Object); ok {
		cardPayload = card
	}
	title := firstNonEmptyText(findHeaderTitle(cardPayload), dget(payload, "title"), findFirstText(cardPayload, []string{"title", "summary", "subtitle"}))
	actions := collectActionLabels(cardPayload)
	var lines []string
	if title != "" {
		lines = append(lines, title)
	}
	for _, line := range collectCardLines(cardPayload) {
		if line != title {
			lines = append(lines, line)
		}
	}
	if len(actions) > 0 {
		lines = append(lines, "Actions: "+strings.Join(actions, ", "))
	}
	text := pystr.Strip(strings.Join(lines[:min(12, len(lines))], "\n"))
	if text == "" {
		text = fallbackInteractiveText
	}
	out := newNormalized(messageType, text)
	out.RelationKind = "interactive"
	if actions == nil {
		actions = []string{}
	}
	out.Metadata = map[string]any{"title": title, "actions": actions}
	return out
}

// Bulleted sender and body lines of every forwarded entry, deduplicated
func collectForwardEntries(payload *pyjson.Object) []string {
	var candidates []any
	for _, key := range []string{"messages", "items", "message_list", "records", "content"} {
		if v, ok := dget(payload, key).([]any); ok {
			candidates = append(candidates, v...)
		}
	}
	var entries []string
	for _, item := range candidates {
		d, ok := item.(*pyjson.Object)
		if !ok {
			if text := normalizeFeishuText(strOr(item), nil); text != "" {
				entries = append(entries, "- "+text)
			}
			continue
		}
		sender := firstTextField(d, []string{"sender_name", "user_name", "sender", "name"}, nil)
		typeVal := dgetDef(d, "message_type", "")
		if !pyrepr.Truthy(typeVal) {
			typeVal = dgetDef(d, "msg_type", "")
		}
		nestedType := pystr.Lower(pystr.Strip(pyrepr.Str(typeVal)))
		var body string
		if nestedType == "post" {
			content := dget(d, "content")
			if !pyrepr.Truthy(content) {
				content = d
			}
			body = parseFeishuPostPayload(content, nil).TextContent
		} else {
			body = firstTextField(d, []string{"text", "summary", "preview", "content"}, []string{"text", "content", "summary", "preview", "title"})
		}
		body = normalizeFeishuText(body, nil)
		if sender != "" && body != "" {
			entries = append(entries, "- "+sender+": "+body)
		} else if body != "" {
			entries = append(entries, "- "+body)
		}
	}
	return uniqueLines(entries)
}

func collectCardLines(payload any) []string {
	var normalized []string
	for _, line := range collectTextSegments(payload, false) {
		if n := normalizeFeishuText(line, nil); n != "" {
			normalized = append(normalized, n)
		}
	}
	return uniqueLines(normalized)
}

var actionTags = map[string]bool{"button": true, "select_static": true, "overflow": true, "date_picker": true, "picker": true}

// Labels of every button, select, overflow and picker in a card, deduplicated
func collectActionLabels(payload any) []string {
	var labels []string
	walkNodes(payload, func(item *pyjson.Object) bool {
		if !actionTags[nodeTag(item)] {
			return true
		}
		if label := firstTextField(item, []string{"text", "name", "value"}, []string{"text", "content", "name", "value"}); label != "" {
			labels = append(labels, label)
		}
		return true
	})
	return uniqueLines(labels)
}

func nodeTag(d *pyjson.Object) string {
	v := dgetDef(d, "tag", "")
	if !pyrepr.Truthy(v) {
		v = dgetDef(d, "type", "")
	}
	return pystr.Lower(pystr.Strip(pyrepr.Str(v)))
}

// Text of every string inside rich blocks of a card tree, skipping id and routing keys
func collectTextSegments(value any, inRichBlock bool) []string {
	switch v := value.(type) {
	case string:
		if inRichBlock {
			return []string{normalizeFeishuText(v, nil)}
		}
		return nil
	case []any:
		var out []string
		for _, item := range v {
			out = append(out, collectTextSegments(item, inRichBlock)...)
		}
		return out
	case *pyjson.Object:
		next := inRichBlock || richBlockTags[nodeTag(v)]
		var segments []string
		if next {
			for _, key := range supportedCardTextKeys {
				if s, ok := dget(v, key).(string); ok {
					if n := normalizeFeishuText(s, nil); n != "" {
						segments = append(segments, n)
					}
				}
			}
		}
		for _, k := range v.Keys {
			if skipTextKeys[k] {
				continue
			}
			item, _ := v.Get(k)
			segments = append(segments, collectTextSegments(item, next)...)
		}
		return segments
	}
	return nil
}

// File key, name and audio, video or file resource type of a media payload
func buildMediaRefFromPayload(payload *pyjson.Object, resourceType string) FeishuPostMediaRef {
	fileKey := pystr.Strip(strOr(dgetDef(payload, "file_key", "")))
	fileName := firstTextField(payload, []string{"file_name", "title", "text"}, nil)
	effective := "file"
	if resourceType == "audio" || resourceType == "video" {
		effective = resourceType
	}
	return FeishuPostMediaRef{FileKey: fileKey, FileName: fileName, ResourceType: effective}
}

func attachmentLabel(fileName string) string {
	if n := normalizeFeishuText(fileName, nil); n != "" {
		return "[Attachment: " + n + "]"
	}
	return fallbackAttachmentText
}

// Card header title from its content, text or name, or the header title itself
func findHeaderTitle(payload any) string {
	d, ok := payload.(*pyjson.Object)
	if !ok {
		return ""
	}
	header, ok := dget(d, "header").(*pyjson.Object)
	if !ok {
		return ""
	}
	title := dget(header, "title")
	if t, ok := title.(*pyjson.Object); ok {
		return firstNonEmptyText(dget(t, "content"), dget(t, "text"), dget(t, "name"))
	}
	return normalizeFeishuText(strOr(title), nil)
}

// First non empty normalized string under any of keys in a depth first walk
func findFirstText(payload any, keys []string) string {
	found := ""
	walkNodes(payload, func(node *pyjson.Object) bool {
		for _, key := range keys {
			if s, ok := dget(node, key).(string); ok {
				if n := normalizeFeishuText(s, nil); n != "" {
					found = n
					return false
				}
			}
		}
		return true
	})
	return found
}

// Visits every dict of a tree depth first, stopping when visit returns false
func walkNodes(value any, visit func(*pyjson.Object) bool) bool {
	switch v := value.(type) {
	case *pyjson.Object:
		if !visit(v) {
			return false
		}
		for _, k := range v.Keys {
			item, _ := v.Get(k)
			if !walkNodes(item, visit) {
				return false
			}
		}
	case []any:
		for _, item := range v {
			if !walkNodes(item, visit) {
				return false
			}
		}
	}
	return true
}

// First scalar value that normalizes to non empty text
func firstNonEmptyText(values ...any) string {
	for _, value := range values {
		switch value.(type) {
		case nil, *pyjson.Object, []any, map[string]any:
			continue
		}
		if n := normalizeFeishuText(pyrepr.Str(value), nil); n != "" {
			return n
		}
	}
	return ""
}

// First non empty text among payload keys, then a deep search over the deep keys
func firstTextField(payload *pyjson.Object, keys []string, deep []string) string {
	values := make([]any, 0, len(keys)+1)
	for _, key := range keys {
		values = append(values, dget(payload, key))
	}
	if len(deep) > 0 {
		values = append(values, findFirstText(payload, deep))
	}
	return firstNonEmptyText(values...)
}

// Text with mention tokens resolved, whitespace collapsed and blank lines dropped
func normalizeFeishuText(text string, mentions *mentionsMap) string {
	cleaned := reSub(mentionTokenRe, text, func(m regexp2.Match) string {
		if mentions == nil {
			return " "
		}
		ref, ok := mentions.Get(m.String())
		if !ok {
			return " "
		}
		return "@" + firstNonEmpty(ref.Name, ref.OpenID, "user")
	})
	cleaned = strings.ReplaceAll(cleaned, "@_all", "@all")
	cleaned = strings.ReplaceAll(strings.ReplaceAll(cleaned, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(cleaned, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if l := pystr.Strip(reSub(whitespaceRe, line, func(regexp2.Match) string { return " " })); l != "" {
			kept = append(kept, l)
		}
	}
	cleaned = reSub(multispaceRe, strings.Join(kept, "\n"), func(regexp2.Match) string { return " " })
	return pystr.Strip(cleaned)
}

func uniqueLines(lines []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range lines {
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}

// Open id and user id of a mention in either payload form
func extractMentionIDs(m rawMention) (string, string) {
	if m.IDText != nil {
		switch pystr.Lower(m.IDType) {
		case "open_id":
			return *m.IDText, ""
		case "user_id":
			return "", *m.IDText
		}
		return "", ""
	}
	if m.IDs == nil {
		return "", ""
	}
	return sdkStr(m.IDs.OpenId), sdkStr(m.IDs.UserId)
}

// Token keyed mention refs with the bot's own mentions flagged
func buildMentionsMap(mentions []rawMention, bot feishuBotIdentity) *mentionsMap {
	result := pydict.New[string, FeishuMentionRef]()
	for _, m := range mentions {
		if m.Key == "" {
			continue
		}
		if m.Key == "@_all" {
			result.Set(m.Key, FeishuMentionRef{IsAll: true})
			continue
		}
		openID, userID := extractMentionIDs(m)
		name := pystr.Strip(m.Name)
		result.Set(m.Key, FeishuMentionRef{Name: name, OpenID: openID, IsSelf: bot.matches(openID, userID, name)})
	}
	return result
}

// Mentioned line listing every non self mention once, empty when there is none
func buildMentionHint(mentions []FeishuMentionRef) string {
	type sig struct {
		all          bool
		openID, name string
	}
	seen := map[sig]bool{}
	var parts []string
	for _, ref := range mentions {
		if ref.IsSelf {
			continue
		}
		s := sig{ref.IsAll, ref.OpenID, ref.Name}
		if seen[s] {
			continue
		}
		seen[s] = true
		switch {
		case ref.IsAll:
			parts = append(parts, "@all")
		case ref.OpenID != "":
			parts = append(parts, firstNonEmpty(ref.Name, "unknown")+" (open_id="+ref.OpenID+")")
		default:
			parts = append(parts, firstNonEmpty(ref.Name, "unknown"))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "[Mentioned: " + strings.Join(parts, ", ") + "]"
}

// Text without leading self mentions and without trailing ones that end the message
func stripEdgeSelfMentions(text string, mentions []FeishuMentionRef) string {
	if text == "" {
		return text
	}
	var selfNames []string
	for _, ref := range mentions {
		if ref.IsSelf {
			selfNames = append(selfNames, "@"+firstNonEmpty(ref.Name, ref.OpenID, "user"))
		}
	}
	if len(selfNames) == 0 {
		return text
	}
	remaining := pystr.Lstrip(text)
	for {
		stripped := false
		for _, nm := range selfNames {
			if !strings.HasPrefix(remaining, nm) {
				continue
			}
			after := remaining[len(nm):]
			if after != "" {
				r, _ := utf8.DecodeRuneInString(after)
				if !strings.ContainsRune(mentionBoundaryChars, r) {
					continue
				}
			}
			remaining = pystr.Lstrip(after)
			stripped = true
			break
		}
		if !stripped {
			break
		}
	}
	for {
		i := len(remaining)
		for i > 0 {
			r, size := utf8.DecodeLastRuneInString(remaining[:i])
			if !strings.ContainsRune(trailingTerminalPunct, r) {
				break
			}
			i -= size
		}
		body, tail := remaining[:i], remaining[i:]
		stripped := false
		for _, nm := range selfNames {
			if strings.HasSuffix(body, nm) {
				remaining = pystr.Rstrip(body[:len(body)-len(nm)]) + tail
				stripped = true
				break
			}
		}
		if !stripped {
			return remaining
		}
	}
}

func dget(d *pyjson.Object, key string) any {
	if d == nil {
		return nil
	}
	v, _ := d.Get(key)
	return v
}

func dgetDef(d *pyjson.Object, key string, def any) any {
	if d == nil {
		return def
	}
	if v, ok := d.Get(key); ok {
		return v
	}
	return def
}

// Python str of value or the empty string when value is falsy
func strOr(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pyrepr.Str(value)
}

func objectValues(d *pyjson.Object) []any {
	out := make([]any, 0, d.Len())
	for _, k := range d.Keys {
		v, _ := d.Get(k)
		out = append(out, v)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
