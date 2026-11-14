package yuanbao

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Parsed data of one TIMCustomElem with the msg_content dict it came from
type customElem struct{ custom, content any }

// Parsed data and content of every TIMCustomElem whose data parses as JSON
func iterCustomElems(msgBody []any) []customElem {
	var out []customElem
	for _, elem := range msgBody {
		d, ok := asDict(elem)
		if !ok || !pyrepr.Equal(d["msg_type"], "TIMCustomElem") {
			continue
		}
		content := pyrepr.Or(dictGetOr(d, "msg_content", pyjson.NewObject()), pyjson.NewObject())
		var dataStr any = ""
		if c, isDict := asDict(content); isDict {
			dataStr = dictGetOr(c, "data", "")
		}
		if !pyrepr.Truthy(dataStr) {
			continue
		}
		if custom, err := jsonLoads(dataStr); err == nil {
			out = append(out, customElem{custom, content})
		}
	}
	return out
}

// First non empty of file_name, fileName and filename, stripped
func fileName(content map[string]any) string {
	for _, key := range []string{"file_name", "fileName", "filename"} {
		if name := stripOrEmpty(content[key]); name != "" {
			return name
		}
	}
	return ""
}

// Media reference entry, the name kept only for a file that carries one
func mediaRef(kind, url, name string) InboundMediaRef {
	return InboundMediaRef{Kind: kind, URL: url, Name: name}
}

// Pointer to the text of a value, nil for None
func optText(v any) *string {
	if v == nil {
		return nil
	}
	return strPtr(pyrepr.Str(v))
}

// ExtractContentMiddleware extracts the raw text, media refs and forwarded records of the message body
type ExtractContentMiddleware struct{ middlewareBase }

func newExtractContentMiddleware() InboundMiddleware {
	return &ExtractContentMiddleware{middlewareBase{"ExtractContentMiddleware", "extract-content"}}
}

const (
	cardContentMaxLength = 1000
	unsupportedElemText  = "[unsupported message type]"
)

// Bracket placeholder text of a share card, elem_type 1010
func formatSharedLink(custom map[string]any) string {
	title, link := dictGetOr(custom, "title", ""), dictGetOr(custom, "link", "")
	first := "[share_card: " + pyrepr.Str(title) + "]"
	if pyrepr.Truthy(link) {
		first = "[share_card: " + pyrepr.Str(title) + " | " + pyrepr.Str(link) + "]"
	}
	lines := []string{first}
	preview := ""
	for _, key := range []string{"card_content", "wechat_des"} {
		if s, ok := custom[key].(string); ok && s != "" {
			preview = s
			break
		}
	}
	if preview != "" {
		if pystr.Len(preview) > cardContentMaxLength {
			preview = pystr.Slice(preview, 0, cardContentMaxLength) + "...(truncated)"
		}
		lines = append(lines, "Preview: "+preview)
	}
	if pyrepr.Truthy(link) {
		lines = append(lines, "[visit link for full content]")
	}
	return strings.Join(lines, "\n")
}

// Bracket placeholder text of a link understanding card, elem_type 1007, false when it names no link
func formatLinkUnderstanding(custom map[string]any) (string, bool) {
	content := custom["content"]
	if !pyrepr.Truthy(content) {
		return "", false
	}
	var link any
	if parsed, err := jsonLoads(content); err == nil {
		if d, isDict := asDict(parsed); isDict {
			link = d["link"]
		}
	}
	if s, ok := link.(string); ok && s != "" {
		return "[link: " + s + " | visit link for full content]", true
	}
	return "", false
}

// resourceId of the query string of a Yuanbao resource URL, empty when it has none
func parseResourceID(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	parsed, err := pyurl.Parse(rawURL)
	if err != nil {
		return ""
	}
	query := pyurl.ParseQS(parsed.Query, false)
	ids := query["resourceId"]
	if len(ids) == 0 {
		ids = query["resourceid"]
	}
	if len(ids) == 0 {
		return ""
	}
	return pystr.Strip(ids[0])
}

// URL of the medium image at index one, falling back to index zero, else empty
func pickImageURL(content map[string]any) string {
	arr, _ := content["image_info_array"].([]any)
	var info map[string]any
	if len(arr) > 1 {
		info, _ = asDict(arr[1])
	}
	if info == nil && len(arr) > 0 {
		info, _ = asDict(arr[0])
	}
	return stripOrEmpty(info["url"])
}

var mediaElemKinds = map[string]string{"TIMImageElem": "image", "TIMSoundElem": "voice", "TIMVideoFileElem": "video"}

// Plain text of the message body, media as ybres anchors, faces as emoji names and custom elems by their type, space joined
func extractText(msgBody []any) (string, error) {
	parts := []any{}
	for _, elem := range msgBody {
		d, err := dictOf(elem)
		if err != nil {
			return "", err
		}
		elemType := dictGetOr(d, "msg_type", "")
		name, _ := elemType.(string)
		kind, isMedia := mediaElemKinds[name]
		var content map[string]any
		if isMedia || name == textElemType || name == "TIMFileElem" || name == "TIMCustomElem" || name == "TIMFaceElem" {
			if content, err = dictOf(dictGetOr(d, "msg_content", pyjson.NewObject())); err != nil {
				return "", err
			}
		}
		switch {
		case name == textElemType:
			if text := dictGetOr(content, "text", ""); pyrepr.Truthy(text) {
				parts = append(parts, text)
			}
		case isMedia:
			url := stripOrEmpty(content["url"])
			if kind == "image" {
				url = pickImageURL(content)
			}
			if rid := parseResourceID(url); rid != "" {
				parts = append(parts, "["+kind+"|ybres:"+rid+"]")
			} else {
				parts = append(parts, "["+kind+"]")
			}
		case name == "TIMFileElem":
			filename := dictGetOr(content, "file_name", dictGetOr(content, "fileName", dictGetOr(content, "filename", "")))
			rid := parseResourceID(stripOrEmpty(content["url"]))
			switch {
			case rid != "" && pyrepr.Truthy(filename):
				parts = append(parts, "[file:"+pyrepr.Str(filename)+"|ybres:"+rid+"]")
			case rid != "":
				parts = append(parts, "[file|ybres:"+rid+"]")
			case pyrepr.Truthy(filename):
				parts = append(parts, "[file: "+pyrepr.Str(filename)+"]")
			default:
				parts = append(parts, "[file]")
			}
		case name == "TIMCustomElem":
			parts = append(parts, customElemText(dictGetOr(content, "data", "")))
		case name == "TIMFaceElem":
			faceName := ""
			if data := dictGetOr(content, "data", ""); pyrepr.Truthy(data) {
				if parsed, err := jsonLoads(data); err == nil {
					if face, isDict := asDict(parsed); isDict {
						if s, isStr := pyrepr.Or(face["name"], "").(string); isStr {
							faceName = pystr.Strip(s)
						}
					}
				}
			}
			if faceName != "" {
				parts = append(parts, "[emoji: "+faceName+"]")
			} else {
				parts = append(parts, "[emoji]")
			}
		case pyrepr.Truthy(elemType):
			parts = append(parts, "["+pyrepr.Str(elemType)+"]")
		}
	}
	return joinParts(" ", parts)
}

// Text of a TIMCustomElem by its elem_type, data that is not JSON passed through and anything else unsupported
func customElemText(dataVal any) any {
	if !pyrepr.Truthy(dataVal) {
		return unsupportedElemText
	}
	parsed, err := jsonLoads(dataVal)
	if err != nil {
		return dataVal
	}
	custom, isDict := asDict(parsed)
	if !isDict {
		return unsupportedElemText
	}
	ctype := custom["elem_type"]
	switch {
	case equalsInt(ctype, 1002):
		return dictGetOr(custom, "text", "[mention]")
	case equalsInt(ctype, 1009):
		return dictGetOr(custom, "text", "[chat record]")
	case equalsInt(ctype, 1010):
		return formatSharedLink(custom)
	case equalsInt(ctype, 1007):
		if text, ok := formatLinkUnderstanding(custom); ok {
			return text
		}
	}
	return unsupportedElemText
}

// Stripped text with a leading full width slash turned into the ASCII one so commands match
func rewriteSlashCommand(text string) string {
	text = pystr.Strip(text)
	if strings.HasPrefix(text, "／") {
		return "/" + pystr.Slice(text, 1, pystr.Len(text))
	}
	return text
}

// Image and file references of the message body with their URLs
func extractInboundMediaRefs(msgBody []any) []InboundMediaRef {
	refs := []InboundMediaRef{}
	for _, elem := range msgBody {
		d, ok := asDict(elem)
		if !ok {
			continue
		}
		content, isDict := asDict(pyrepr.Or(dictGetOr(d, "msg_content", pyjson.NewObject()), pyjson.NewObject()))
		if !isDict {
			continue
		}
		switch msgType := dictGetOr(d, "msg_type", ""); {
		case pyrepr.Equal(msgType, "TIMImageElem"):
			if imageURL := pickImageURL(content); imageURL != "" {
				refs = append(refs, mediaRef("image", imageURL, ""))
			}
		case pyrepr.Equal(msgType, "TIMFileElem"):
			if fileURL := stripOrEmpty(content["url"]); fileURL != "" {
				refs = append(refs, mediaRef("file", fileURL, fileName(content)))
			}
		}
	}
	return refs
}

// Forwarded chat records of an elem_type 1009 custom elem, the first base64 protobuf ext_map entry of sub type one
func extractForwardedRecords(msgBody []any) *pyjson.Object {
	for _, ce := range iterCustomElems(msgBody) {
		custom, isDict := asDict(ce.custom)
		if !isDict || !equalsInt(custom["elem_type"], 1009) {
			continue
		}
		content, _ := asDict(ce.content)
		extMap, extIsDict := asDict(content["ext_map"])
		if !extIsDict || len(extMap) == 0 {
			return nil
		}
		for _, key := range dictKeys(content["ext_map"]) {
			value, isStr := extMap[key].(string)
			if !strings.HasPrefix(key, "wexin_forward_msg_") || !isStr || value == "" {
				continue
			}
			raw, err := pybase64.B64Decode(value)
			if err != nil {
				continue
			}
			if data := DecodeForwardMsgData(raw); data != nil && equalsInt(data.GetOr("sub_type", nil), 1) {
				return data
			}
		}
	}
	return nil
}

func (m *ExtractContentMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	text, err := extractText(ic.MsgBody)
	if err != nil {
		return err
	}
	ic.RawText = rewriteSlashCommand(text)
	ic.MediaRefs = extractInboundMediaRefs(ic.MsgBody)
	ic.ForwardedRecords = extractForwardedRecords(ic.MsgBody)
	return next()
}

// PlaceholderFilterMiddleware skips a message that is only a media placeholder with no media
type PlaceholderFilterMiddleware struct{ middlewareBase }

func newPlaceholderFilterMiddleware() InboundMiddleware {
	return &PlaceholderFilterMiddleware{middlewareBase{"PlaceholderFilterMiddleware", "placeholder-filter"}}
}

var skippablePlaceholders = map[string]bool{"[image]": true, "[图片]": true, "[file]": true, "[文件]": true,
	"[video]": true, "[视频]": true, "[voice]": true, "[语音]": true}

// Whether the text is a bare placeholder and no media came with it
func isSkippablePlaceholder(text string, mediaCount int) bool {
	return mediaCount <= 0 && skippablePlaceholders[pystr.Strip(text)]
}

func (m *PlaceholderFilterMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	if isSkippablePlaceholder(ic.RawText, len(ic.MediaRefs)) {
		slog.Debug(fmt.Sprintf("[%s] Skipping placeholder message: %s", ic.Adapter.Name(), pyrepr.StrRepr(ic.RawText)))
		return nil
	}
	return next()
}

// OwnerCommandMiddleware lets the bot owner run allowlisted slash commands in a group without a mention and rejects everyone else
type OwnerCommandMiddleware struct{ middlewareBase }

func newOwnerCommandMiddleware() InboundMiddleware {
	return &OwnerCommandMiddleware{middlewareBase{"OwnerCommandMiddleware", "owner-command"}}
}

var ownerCommandAllowlist = map[string]bool{"/new": true, "/reset": true, "/retry": true, "/undo": true, "/stop": true,
	"/approve": true, "/deny": true, "/bg": true, "/btw": true, "/queue": true, "/q": true}

// Allowlisted command, its full line and whether the sender is the bot owner, an empty command when none matched
func detectOwnerCommand(push *pyjson.Object, msgBody []any, chatType string, fromAccount any) (cmd, cmdLine string, isOwner bool, err error) {
	if chatType != "group" || len(ownerCommandAllowlist) == 0 {
		return "", "", false, nil
	}
	var textElems []map[string]any
	for _, elem := range msgBody {
		d, err := dictOf(elem)
		if err != nil {
			return "", "", false, err
		}
		if pyrepr.Equal(d["msg_type"], textElemType) {
			textElems = append(textElems, d)
		}
	}
	if len(textElems) != 1 {
		return "", "", false, nil
	}
	content, err := dictOf(pyrepr.Or(textElems[0]["msg_content"], pyjson.NewObject()))
	if err != nil {
		return "", "", false, err
	}
	text, err := strMethod(dictGetOr(content, "text", ""), "strip")
	if err != nil {
		return "", "", false, err
	}
	line := rewriteSlashCommand(text)
	if !strings.HasPrefix(line, "/") {
		return "", "", false, nil
	}
	name := pystr.Lower(pystr.SplitWhitespace(line, 1)[0])
	if !ownerCommandAllowlist[name] {
		return "", "", false, nil
	}
	var owner any
	if push != nil {
		owner = push.GetOr("bot_owner_id", nil)
	}
	ownerID := stripOrEmpty(owner)
	return name, line, ownerID != "" && pyrepr.Equal(ownerID, fromAccount), nil
}

func (m *OwnerCommandMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	adapter := ic.Adapter
	matched, cmdLine, isOwner, err := detectOwnerCommand(ic.Push, ic.MsgBody, ic.ChatType, ic.FromAccount)
	if err != nil {
		return err
	}
	if matched != "" && !isOwner {
		slog.Info(fmt.Sprintf("[%s] Reject non-owner slash command: chat=%s from=%s cmd=%s", adapter.Name(), ic.ChatID,
			pyrepr.Str(ic.FromAccount), matched))
		chatID := ic.ChatID
		denial := i18n.T(ctx, "platform.yuanbao.owner_command_denied", "", map[string]any{"command": matched})
		adapter.trackTask(gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
			_, err := adapter.Send(tctx, chatID, denial, "", nil)
			return err
		}))
		return nil
	}
	if matched != "" && isOwner && cmdLine != "" {
		slog.Info(fmt.Sprintf("[%s] Bot owner slash command: chat=%s from=%s cmd=%s", adapter.Name(), ic.ChatID,
			pyrepr.Str(ic.FromAccount), matched))
		ic.OwnerCommand = &matched
		ic.RawText = cmdLine
	}
	return next()
}

// BuildSourceMiddleware builds the session source of the chat
type BuildSourceMiddleware struct{ middlewareBase }

func newBuildSourceMiddleware() InboundMiddleware {
	return &BuildSourceMiddleware{middlewareBase{"BuildSourceMiddleware", "build-source"}}
}

func (m *BuildSourceMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	fields := gateway.SourceFields{ChatName: optText(ic.ChatName), ChatType: ic.ChatType,
		UserName: optText(pyrepr.Or(ic.SenderNickname, ic.FromAccount))}
	if pyrepr.Truthy(ic.FromAccount) {
		fields.UserID = optText(ic.FromAccount)
	}
	if ic.ChatType == "group" {
		fields.ThreadID = strPtr("main")
	}
	ic.Source = ic.Adapter.BuildSource(ctx, ic.ChatID, fields)
	return next()
}

// GroupAtGuardMiddleware records group messages that do not mention the bot and lets only mentions and owner commands through
type GroupAtGuardMiddleware struct{ middlewareBase }

func newGroupAtGuardMiddleware() InboundMiddleware {
	return &GroupAtGuardMiddleware{middlewareBase{"GroupAtGuardMiddleware", "group-at-guard"}}
}

// Visits each custom elem that mentions the bot, elem_type 1002 with the bot as user_id, until visit returns false
func iterBotMentions(msgBody []any, botID any, visit func(custom map[string]any) bool) error {
	if !pyrepr.Truthy(botID) {
		return nil
	}
	for _, ce := range iterCustomElems(msgBody) {
		custom, err := dictOf(ce.custom)
		if err != nil {
			return err
		}
		if equalsInt(custom["elem_type"], 1002) && pyrepr.Equal(custom["user_id"], botID) && !visit(custom) {
			return nil
		}
	}
	return nil
}

// Whether the message body mentions the bot
func isAtBot(msgBody []any, botID any) (bool, error) {
	found := false
	err := iterBotMentions(msgBody, botID, func(map[string]any) bool {
		found = true
		return false
	})
	return found, err
}

// Display text the group uses to mention the bot, empty when no mention carries one
func extractBotMentionText(msgBody []any, botID any) (string, error) {
	text := ""
	err := iterBotMentions(msgBody, botID, func(custom map[string]any) bool {
		text = stripOrEmpty(custom["text"])
		return text == ""
	})
	return text, err
}

// Per turn group chat prompt naming the bot and which message to answer
func buildGroupChannelPrompt(msgBody []any, botID any) (string, error) {
	botMention, err := extractBotMentionText(msgBody, botID)
	if err != nil {
		return "", err
	}
	if botMention == "" {
		botMention = "unknown"
	}
	return "You are handling a Yuanbao group chat message.\n" +
		"- Your identity: user_id=" + pyrepr.Str(pyrepr.Or(botID, "unknown")) + ", @-mention name in this group=" + botMention + "\n" +
		"- Lines in history prefixed with `[nickname|user_id]` are observed group context " +
		"and are not necessarily addressed to you.\n" +
		"- Treat only the current new message as a request explicitly directed at you, " +
		"and answer it directly.", nil
}

// Appends a group message to the transcript as an observed user row without running the agent, a failure only warned
func observeGroupMessage(ctx context.Context, adapter *YuanbaoAdapter, source *gateway.SessionSource, senderDisplay any, text string,
	ic *InboundContext, msgID any, forwardedRecords *pyjson.Object) {
	store := sessionStoreOf(adapter)
	if store == nil {
		return
	}
	err := func() error {
		if source == nil {
			return attrError(nil, "user_id")
		}
		sessionEntry, err := store.GetOrCreateSession(ctx, source, false, true)
		if err != nil {
			return err
		}
		bodyText := text
		if forwardedRecords != nil && forwardedRecords.Len() > 0 {
			summary, err := buildForwardText(forwardedRecords, ic, false)
			if err != nil {
				return err
			}
			if summary != "" {
				bodyText = summary
				if text != "" {
					bodyText = text + "\n" + summary
				}
			}
		}
		userID := "unknown"
		if source.UserID != nil && *source.UserID != "" {
			userID = *source.UserID
		}
		entry := map[string]any{"role": "user", "content": "[" + pyrepr.Str(senderDisplay) + "|" + userID + "]\n" + bodyText,
			"timestamp": pydatetime.ISOFormatUTC(time.Now()), "observed": true}
		if pyrepr.Truthy(msgID) {
			entry["message_id"] = msgID
		}
		return store.AppendToTranscript(ctx, sessionEntry.SessionID, entry, false)
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Failed to observe group message: %s", adapter.Name(), errText(err)))
	}
}

func (m *GroupAtGuardMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	adapter := ic.Adapter
	if ic.ChatType == "group" && ic.OwnerCommand == nil {
		atBot, err := isAtBot(ic.MsgBody, adapter.botIDValue())
		if err != nil {
			return err
		}
		if !atBot {
			var msgID any
			if pyrepr.Truthy(ic.MsgID) {
				msgID = ic.MsgID
			}
			observeGroupMessage(ctx, adapter, ic.Source, pyrepr.Or(ic.SenderNickname, ic.FromAccount), ic.RawText, ic, msgID, ic.ForwardedRecords)
			slog.Info(fmt.Sprintf("[%s] Group message observed (no @bot): chat=%s from=%s", adapter.Name(), ic.ChatID, pyrepr.Str(ic.FromAccount)))
			return nil
		}
	}
	return next()
}

// GroupAttributionMiddleware prefixes a group turn with its sender, sets the channel prompt and clears the source user name
type GroupAttributionMiddleware struct{ middlewareBase }

func newGroupAttributionMiddleware() InboundMiddleware {
	return &GroupAttributionMiddleware{middlewareBase{"GroupAttributionMiddleware", "group-attribution"}}
}

func (m *GroupAttributionMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	if ic.ChatType == "group" && ic.OwnerCommand == nil {
		prompt, err := buildGroupChannelPrompt(ic.MsgBody, ic.Adapter.botIDValue())
		if err != nil {
			return err
		}
		ic.ChannelPrompt = &prompt
		ic.RawText = "[" + pyrepr.Str(pyrepr.Or(pyrepr.Or(ic.SenderNickname, ic.FromAccount), "unknown")) + "|" +
			pyrepr.Str(pyrepr.Or(ic.FromAccount, "unknown")) + "]\n" + ic.RawText
		if ic.Source != nil {
			ic.Source = gateway.ReplaceSource(ic.Source, func(s *gateway.SessionSource) { s.UserName = nil })
		}
	}
	return next()
}

// YuanbaoMessageType is a message subtype local to Yuanbao that dispatch coerces back to a gateway type
type YuanbaoMessageType string

const YuanbaoMessageTypeChatRecord YuanbaoMessageType = "chat_record"

var elemMessageTypes = map[string]gateway.MessageType{"TIMImageElem": gateway.MessageTypePhoto, "TIMSoundElem": gateway.MessageTypeVoice,
	"TIMVideoFileElem": gateway.MessageTypeVideo, "TIMFileElem": gateway.MessageTypeDocument}

// ClassifyMessageTypeMiddleware picks the message type from the text and the body elements
type ClassifyMessageTypeMiddleware struct{ middlewareBase }

func newClassifyMessageTypeMiddleware() InboundMiddleware {
	return &ClassifyMessageTypeMiddleware{middlewareBase{"ClassifyMessageTypeMiddleware", "classify-msg-type"}}
}

// Command for a slash text, else the type of the first media elem, a chat record for elem_type 1009, else text
func classifyMessage(text string, msgBody []any) (any, error) {
	if strings.HasPrefix(text, "/") {
		return gateway.MessageTypeCommand, nil
	}
	for _, elem := range msgBody {
		d, err := dictOf(elem)
		if err != nil {
			return nil, err
		}
		etype := dictGetOr(d, "msg_type", "")
		if err := unhashable(etype); err != nil {
			return nil, err
		}
		name, _ := etype.(string)
		if mapped, ok := elemMessageTypes[name]; ok {
			return mapped, nil
		}
		if name != "TIMCustomElem" {
			continue
		}
		content, err := dictOf(pyrepr.Or(d["msg_content"], pyjson.NewObject()))
		if err != nil {
			return nil, err
		}
		if parsed, err := jsonLoads(dictGetOr(content, "data", "")); err == nil {
			if custom, isDict := asDict(parsed); isDict && equalsInt(custom["elem_type"], 1009) {
				return YuanbaoMessageTypeChatRecord, nil
			}
		}
	}
	return gateway.MessageTypeText, nil
}

func (m *ClassifyMessageTypeMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	msgType, err := classifyMessage(ic.RawText, ic.MsgBody)
	if err != nil {
		return err
	}
	ic.MsgType = msgType
	return next()
}

// QuoteContextMiddleware reads the quoted message id, its text and its media anchors
type QuoteContextMiddleware struct{ middlewareBase }

func newQuoteContextMiddleware() InboundMiddleware {
	return &QuoteContextMiddleware{middlewareBase{"QuoteContextMiddleware", "quote-context"}}
}

// Quoted message id and its sender prefixed description from cloud_custom_data, nil for whichever is missing
func extractQuoteContext(cloudCustomData any) (*string, *string) {
	var parsed any
	if pyrepr.Truthy(cloudCustomData) {
		if loaded, err := jsonLoads(cloudCustomData); err == nil {
			parsed = loaded
		}
	}
	var quote map[string]any
	if d, isDict := asDict(parsed); isDict {
		quote, _ = asDict(d["quote"])
	}
	if quote == nil {
		return nil, nil
	}
	var quoteID, quoteText *string
	if id := stripOrEmpty(quote["id"]); id != "" {
		quoteID = &id
	}
	desc := stripOrEmpty(quote["desc"])
	sender := stripOrEmpty(pyrepr.Or(quote["sender_nickname"], quote["sender_id"]))
	if desc != "" {
		text := desc
		if sender != "" {
			text = sender + ": " + desc
		}
		quoteText = &text
	}
	return quoteID, quoteText
}

// Resolvable ybres anchors of the quoted transcript row, empty without a quote, a store, a source or anchors
func extractMediaRefsFromTranscript(ctx context.Context, ic *InboundContext) ([]ybresRef, error) {
	if ic.ReplyToMessageID == nil {
		return nil, nil
	}
	adapter := ic.Adapter
	var mediaRefs []ybresRef
	err := func() error {
		store := sessionStoreOf(adapter)
		if store == nil || ic.Source == nil {
			return nil
		}
		entry, err := store.GetOrCreateSession(ctx, ic.Source, false, true)
		if err != nil {
			return err
		}
		history, err := store.LoadTranscript(ctx, entry.SessionID)
		if err != nil {
			return err
		}
		for i := len(history) - 1; i >= 0; i-- {
			msg, err := dictOf(history[i])
			if err != nil {
				return err
			}
			mid := dictGetOr(msg, "message_id", "")
			if !pyrepr.Truthy(mid) || !pyrepr.Equal(mid, *ic.ReplyToMessageID) {
				continue
			}
			if content, isStr := dictGetOr(msg, "content", "").(string); isStr && strings.Contains(content, "|ybres:") {
				mediaRefs = append(mediaRefs, iterYbresRefs(findAll(ybResRefRE, content))...)
			}
			break
		}
		return nil
	}()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var unreadable *gateway.TranscriptReadError
		if errors.As(err, &unreadable) {
			slog.Warn(fmt.Sprintf("[%s] quote transcript lookup: transcript unreadable: %s", adapter.Name(), errText(err)))
		} else {
			slog.Warn(fmt.Sprintf("[%s] quote transcript lookup failed: %s", adapter.Name(), errText(err)))
		}
	}
	return mediaRefs, nil
}

func (m *QuoteContextMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	ic.ReplyToMessageID, ic.ReplyToText = extractQuoteContext(ic.CloudCustomData)
	refs, err := extractMediaRefsFromTranscript(ctx, ic)
	if err != nil {
		return err
	}
	ic.QuoteMediaRefs = refs
	return next()
}
