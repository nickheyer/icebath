package yuanbao

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	yuanbaoToolset = "icebath-yuanbao"
	mentionHint    = "To @mention a user, you MUST use the format: space + @ + nickname + space (e.g. \" @Alice \")."

	defaultStickerSearchLimit = 10
	maxStickerSearchLimit     = 50
)

// Labels of the member user types, unknown for any other type
var userTypeLabels = []string{"unknown", "user", "yuanbao_ai", "bot"}

// Tool failure whose payload is the error envelope the model reads
type ybError struct {
	msg   any
	extra *pyjson.Object
}

func (e *ybError) Error() string { return pyrepr.Str(e.msg) }

// Error envelope of the failure with its extra fields after the message
func (e *ybError) payload() *pyjson.Object {
	out := pyjson.ObjectOf("success", false, "error", e.msg)
	if e.extra != nil {
		out.Update(e.extra)
	}
	return out
}

func ybFail(msg string) error { return &ybError{msg: msg} }

// Tool handler returning the result of fn, the envelope of a tool failure or of any other logged error
func ybTool(label string, fn func(ctx context.Context, call *tools.Call) (*pyjson.Object, error)) tools.Handler {
	return func(ctx context.Context, call *tools.Call) string {
		result, err := fn(ctx, call)
		var failure *ybError
		switch {
		case err == nil:
			return tools.ToolResult(result)
		case errors.As(err, &failure):
			return tools.ToolResult(failure.payload())
		}
		slog.Error(fmt.Sprintf("[yuanbao_tools] %s error", label), "error", err)
		return tools.ToolResult((&ybError{msg: errText(err)}).payload())
	}
}

// Running adapter of the turn, a tool failure when none is connected
func toolAdapter(ctx context.Context) (*YuanbaoAdapter, error) {
	if adapter := GetActive(ctx); adapter != nil {
		return adapter, nil
	}
	return nil, ybFail("Yuanbao adapter is not connected")
}

// Argument of a tool call, the default when the model left it out
func toolArg(call *tools.Call, key string, def any) any { return dictGetOr(call.Args, key, def) }

// Nickname of a member under either key, the default when it carries neither
func memberNick(member map[string]any, def any) any {
	return dictGetOr(member, "nickname", dictGetOr(member, "nick_name", def))
}

// Members of a group through the running adapter, a tool failure when the query fails
func groupMembers(ctx context.Context, adapter *YuanbaoAdapter, groupCode any) ([]any, error) {
	code, err := encodeText(groupCode)
	if err != nil {
		return nil, err
	}
	raw, err := adapter.GetGroupMemberList(ctx, code, 0, defaultMemberListLimit)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, ybFail("get_group_member_list returned None")
	}
	return iterOf(raw.GetOr("members", []any{}))
}

// User id and nickname of the one group member whose nickname contains the name, candidates listed when several match
func resolveDMRecipient(ctx context.Context, adapter *YuanbaoAdapter, groupCode, name any) (any, any, error) {
	if !pyrepr.Truthy(groupCode) {
		return nil, nil, ybFail("group_code is required when user_id is not provided")
	}
	if !pyrepr.Truthy(name) {
		return nil, nil, ybFail("name is required when user_id is not provided")
	}
	nameText, err := strMethod(name, "strip")
	if err != nil {
		return nil, nil, err
	}
	filter := pystr.Lower(pystr.Strip(nameText))
	members, err := groupMembers(ctx, adapter, groupCode)
	if err != nil {
		return nil, nil, err
	}
	var matched []map[string]any
	for _, entry := range members {
		member, err := dictOf(entry)
		if err != nil {
			return nil, nil, err
		}
		nick, err := strMethod(pyrepr.Or(pyrepr.Or(member["nickname"], member["nick_name"]), ""), "lower")
		if err != nil {
			return nil, nil, err
		}
		if strings.Contains(pystr.Lower(nick), filter) {
			matched = append(matched, member)
		}
	}
	switch len(matched) {
	case 0:
		return nil, nil, ybFail(fmt.Sprintf("No member matching \"%s\" found in group %s.", nameText, pyrepr.Str(groupCode)))
	case 1:
		return dictGetOr(matched[0], "user_id", ""), memberNick(matched[0], name), nil
	}
	candidates := make([]any, len(matched))
	for i, member := range matched {
		candidates[i] = pyjson.ObjectOf("user_id", dictGetOr(member, "user_id", ""), "nickname", memberNick(member, ""))
	}
	return nil, nil, &ybError{msg: fmt.Sprintf("Multiple members match \"%s\". Please specify which one.", nameText),
		extra: pyjson.ObjectOf("candidates", candidates)}
}

// Name, owner and member count of a group
func getGroupInfo(ctx context.Context, call *tools.Call) (*pyjson.Object, error) {
	groupCode := toolArg(call, "group_code", "")
	if !pyrepr.Truthy(groupCode) {
		return nil, ybFail("group_code is required")
	}
	adapter, err := toolAdapter(ctx)
	if err != nil {
		return nil, err
	}
	code, err := encodeText(groupCode)
	if err != nil {
		return nil, err
	}
	info, err := adapter.QueryGroupInfo(ctx, code)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, ybFail("query_group_info returned None")
	}
	return pyjson.ObjectOf("success", true, "group_code", groupCode, "group_name", info.GetOr("group_name", ""),
		"member_count", info.GetOr("member_count", int64(0)),
		"owner", pyjson.ObjectOf("user_id", info.GetOr("owner_id", ""), "nickname", info.GetOr("owner_nickname", "")),
		"note", "The group is called \"派 (Pai)\" in the app."), nil
}

// Label of a member user type, unknown outside the known types
func userTypeLabel(userType any) (string, error) {
	if err := unhashable(userType); err != nil {
		return "", err
	}
	for value, label := range userTypeLabels {
		if equalsInt(userType, int64(value)) {
			return label, nil
		}
	}
	return "unknown", nil
}

// Members of a group, all of them, only the bots, or those whose nickname contains a name
func queryGroupMembers(ctx context.Context, call *tools.Call) (*pyjson.Object, error) {
	groupCode, name := toolArg(call, "group_code", ""), toolArg(call, "name", "")
	action := toolArg(call, "action", "list_all")
	if !pyrepr.Truthy(groupCode) {
		return nil, ybFail("group_code is required")
	}
	adapter, err := toolAdapter(ctx)
	if err != nil {
		return nil, err
	}
	members, err := groupMembers(ctx, adapter, groupCode)
	if err != nil {
		return nil, err
	}
	allMembers := make([]*pyjson.Object, 0, len(members))
	for _, entry := range members {
		member, err := dictOf(entry)
		if err != nil {
			return nil, err
		}
		role, err := userTypeLabel(dictGetOr(member, "user_type", dictGetOr(member, "role", int64(0))))
		if err != nil {
			return nil, err
		}
		allMembers = append(allMembers, pyjson.ObjectOf("user_id", dictGetOr(member, "user_id", ""), "nickname", memberNick(member, ""), "role", role))
	}
	if len(allMembers) == 0 {
		return nil, ybFail("No members found in this group.")
	}
	listing := func(ok bool, msg string, listed []*pyjson.Object) *pyjson.Object {
		rows := make([]any, len(listed))
		for i, member := range listed {
			rows[i] = member
		}
		out := pyjson.ObjectOf("success", ok, "msg", msg, "members", rows)
		if pyrepr.Truthy(toolArg(call, "mention", false)) {
			out.Set("mention_hint", mentionHint)
		}
		return out
	}
	if pyrepr.Equal(action, "list_bots") {
		var bots []*pyjson.Object
		for _, member := range allMembers {
			if role := member.GetOr("role", ""); role == "yuanbao_ai" || role == "bot" {
				bots = append(bots, member)
			}
		}
		if len(bots) == 0 {
			return nil, ybFail("No bots found in this group.")
		}
		return listing(true, fmt.Sprintf("Found %d bot(s).", len(bots)), bots), nil
	}
	if pyrepr.Equal(action, "find") && pyrepr.Truthy(name) {
		nameText, err := strMethod(name, "strip")
		if err != nil {
			return nil, err
		}
		filter := pystr.Lower(pystr.Strip(nameText))
		var matched []*pyjson.Object
		for _, member := range allMembers {
			nick, err := strMethod(member.GetOr("nickname", ""), "lower")
			if err != nil {
				return nil, err
			}
			if strings.Contains(pystr.Lower(nick), filter) {
				matched = append(matched, member)
			}
		}
		if len(matched) > 0 {
			return listing(true, fmt.Sprintf("Found %d member(s) matching \"%s\".", len(matched), nameText), matched), nil
		}
		return listing(false, fmt.Sprintf("No match for \"%s\". All members listed below.", nameText), allMembers), nil
	}
	return listing(true, fmt.Sprintf("Found %d member(s).", len(allMembers)), allMembers), nil
}

// Result count of a sticker search between one and fifty, ten for a blank or unreadable limit
func stickerSearchLimit(limit any) (int, error) {
	if !pyrepr.Truthy(limit) {
		return defaultStickerSearchLimit, nil
	}
	n, err := pyint.Of(limit)
	var conversion *pyint.Error
	switch {
	case errors.As(err, &conversion) && (conversion.Kind == "TypeError" || conversion.Kind == "ValueError"):
		return defaultStickerSearchLimit, nil
	case errors.As(err, &conversion):
		return 0, &pyErr{conversion.Kind, conversion.Msg}
	case err != nil:
		return 0, err
	case n.Cmp(big.NewInt(1)) < 0:
		return 1, nil
	case n.Cmp(big.NewInt(maxStickerSearchLimit)) > 0:
		return maxStickerSearchLimit, nil
	}
	return int(n.Int64()), nil
}

// Best matches of the builtin sticker table for a keyword, the first stickers for an empty one
func searchSticker(ctx context.Context, call *tools.Call) (*pyjson.Object, error) {
	query := pyrepr.Or(toolArg(call, "query", ""), "")
	limit, err := stickerSearchLimit(toolArg(call, "limit", int64(defaultStickerSearchLimit)))
	if err != nil {
		return nil, err
	}
	matches := SearchStickers(strOrEmpty(query), limit)
	results := make([]any, len(matches))
	for i, sticker := range matches {
		row := pyjson.NewObject()
		for _, key := range []string{"sticker_id", "name", "description", "package_id"} {
			row.Set(key, sticker.GetOr(key, ""))
		}
		results[i] = row
	}
	return pyjson.ObjectOf("success", true, "query", query, "count", int64(len(matches)), "results", results), nil
}

// Stripped text of an optional string argument, the AttributeError a value that is not text raises
func strippedArg(call *tools.Call, key string) (string, error) {
	text, err := strMethod(pyrepr.Or(toolArg(call, key, ""), ""), "strip")
	return pystr.Strip(text), err
}

// Sends a builtin sticker by name or id, a random one for an empty name, to the session chat by default
func sendSticker(ctx context.Context, call *tools.Call) (*pyjson.Object, error) {
	target, err := strippedArg(call, "chat_id")
	if err != nil {
		return nil, err
	}
	if target == "" {
		target = tools.GetSessionEnv(call.Env, "ICEBATH_SESSION_CHAT_ID", "")
	}
	if target == "" {
		return nil, ybFail("chat_id is required (no active yuanbao session detected)")
	}
	adapter, err := toolAdapter(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := strippedArg(call, "sticker")
	if err != nil {
		return nil, err
	}
	var sticker *pyjson.Object
	switch {
	case raw == "":
		sticker = GetRandomSticker("")
	case pystr.StrIsDigit(raw):
		sticker = GetStickerByID(raw)
	}
	if sticker == nil && raw != "" {
		sticker = GetStickerByName(raw)
	}
	if sticker == nil {
		return nil, ybFail("Sticker not found: " + pyrepr.StrRepr(raw) + ". Use search_sticker first to discover available stickers.")
	}
	replyTo, err := protoText(toolArg(call, "reply_to", ""))
	if err != nil {
		return nil, err
	}
	stickerName := pyrepr.Str(sticker.GetOr("name", ""))
	result, err := adapter.SendSticker(ctx, target, &stickerName, nil, replyTo, nil)
	if err != nil {
		return nil, err
	}
	if !result.Success {
		return nil, &ybError{msg: result.Error}
	}
	return pyjson.ObjectOf("success", true, "chat_id", target,
		"sticker", pyjson.ObjectOf("sticker_id", sticker.GetOr("sticker_id", ""), "name", sticker.GetOr("name", "")),
		"message_id", result.MessageID,
		"note", "Sticker delivered to the chat. If you have additional text to say, reply now; otherwise end your turn without generating text."), nil
}

// Media files of a DM call, each a path and voice flag mapping or pair
func dmMediaFiles(raw any) ([]gateway.MediaRef, error) {
	if !pyrepr.Truthy(raw) {
		return nil, nil
	}
	items, err := iterOf(raw)
	if err != nil {
		return nil, err
	}
	var media []gateway.MediaRef
	for _, item := range items {
		if fields, isDict := asDict(item); isDict {
			media = append(media, gateway.MediaRef{Path: pyrepr.Str(dictGetOr(fields, "path", "")), IsVoice: pyrepr.Truthy(fields["is_voice"])})
		} else if pair, isList := item.([]any); isList && len(pair) >= 2 {
			media = append(media, gateway.MediaRef{Path: pyrepr.Str(pair[0]), IsVoice: pyrepr.Truthy(pair[1])})
		}
	}
	return media, nil
}

// Sends a DM with optional media to a group member found by name, or to a known user id
func sendDM(ctx context.Context, call *tools.Call) (*pyjson.Object, error) {
	groupCode := toolArg(call, "group_code", "")
	if !pyrepr.Truthy(groupCode) {
		if chatID := tools.GetSessionEnv(call.Env, "ICEBATH_SESSION_CHAT_ID", ""); strings.HasPrefix(chatID, "group:") {
			groupCode = chatID[len("group:"):]
		}
	}
	mediaFiles, err := dmMediaFiles(call.Args["media_files"])
	if err != nil {
		return nil, err
	}
	rawMessage, isText := toolArg(call, "message", "").(string)
	if !isText {
		return nil, &pyErr{"TypeError", "argument of type '" + pyrepr.TypeName(call.Args["message"]) + "' is not iterable"}
	}
	embedded, message, err := gateway.ExtractMedia(ctx, rawMessage)
	if err != nil {
		return nil, err
	}
	if mediaFiles, err = gateway.FilterMediaDeliveryPaths(ctx, append(mediaFiles, embedded...)); err != nil {
		return nil, err
	}
	if message == "" && len(mediaFiles) == 0 {
		return nil, ybFail("message or media_files is required")
	}
	adapter, err := toolAdapter(ctx)
	if err != nil {
		return nil, err
	}
	name := toolArg(call, "name", "")
	userID, err := strippedArg(call, "user_id")
	if err != nil {
		return nil, err
	}
	nameText, err := strMethod(name, "strip")
	if err != nil {
		return nil, err
	}
	var resolvedUserID, resolvedNickname any = userID, pystr.Strip(nameText)
	if userID == "" {
		if resolvedUserID, resolvedNickname, err = resolveDMRecipient(ctx, adapter, groupCode, name); err != nil {
			return nil, err
		}
	}
	if !pyrepr.Truthy(resolvedUserID) {
		return nil, ybFail("Could not resolve user_id")
	}
	recipient := pyrepr.Str(resolvedUserID)
	chatID := "direct:" + recipient
	var last *gateway.SendResult
	var failures []string
	record := func(result gateway.SendResult, fallback string) {
		last = &result
		if !result.Success {
			failures = append(failures, pyrepr.Str(pyrepr.Or(result.Error, fallback)))
		}
	}
	if pystr.Strip(message) != "" {
		result, err := adapter.SendDM(ctx, recipient, message, groupCode)
		if err != nil {
			return nil, err
		}
		record(result, "text send failed")
	}
	extra := map[string]any{"group_code": groupCode}
	for _, media := range mediaFiles {
		var result gateway.SendResult
		if senderImageExts[pystr.Lower(pypath.PathSuffix(media.Path))] {
			result, err = adapter.SendImageFile(ctx, chatID, media.Path, "", "", nil, extra)
		} else {
			result, err = adapter.SendDocument(ctx, chatID, media.Path, "", "", "", nil, extra)
		}
		if err != nil {
			return nil, err
		}
		record(result, "media send failed")
	}
	if last == nil {
		return nil, ybFail("No deliverable text or media remained")
	}
	if len(failures) > 0 && !last.Success {
		return nil, ybFail(strings.Join(failures, "; "))
	}
	note := "DM sent to \"" + pyrepr.Str(resolvedNickname) + "\" successfully."
	if len(failures) > 0 {
		note += " (partial failure: " + strings.Join(failures, "; ") + ")"
	}
	return pyjson.ObjectOf("success", true, "user_id", resolvedUserID, "nickname", resolvedNickname, "message_id", last.MessageID,
		"note", note), nil
}

// Whether the turn runs in a Yuanbao gateway session or an adapter is connected for its home
func checkYuanbao(env *tools.Env) bool {
	return tools.GetSessionEnv(env, "ICEBATH_SESSION_PLATFORM", "") == "yuanbao" || activeFor(tools.HomeOverride(env)) != nil
}

// String property of a tool schema
func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// Function schema of a tool with its properties and required names
func toolSchema(name, description string, properties map[string]any, required ...any) map[string]any {
	if required == nil {
		required = []any{}
	}
	return map[string]any{"name": name, "description": description,
		"parameters": map[string]any{"type": "object", "properties": properties, "required": required}}
}

const groupCodeDescription = "The unique group identifier (group_code)."

// Schema, handler and emoji of every Yuanbao tool
var yuanbaoToolDefs = []tools.Def{
	{Name: "yb_query_group_info", Emoji: "👥", Handler: ybTool("get_group_info", getGroupInfo),
		Schema: toolSchema("yb_query_group_info", "Query a group's name, owner and member count (a group is '派/Pai' in the app).",
			map[string]any{"group_code": stringProp(groupCodeDescription)}, "group_code")},
	{Name: "yb_query_group_members", Emoji: "📋", Handler: ybTool("query_group_members", queryGroupMembers),
		Schema: toolSchema("yb_query_group_members",
			"Query members of a group (called '派/Pai' in the app). "+
				"Use this tool when you need to @mention someone, find a user by name, "+
				"list bots (including Yuanbao AI), or list all members. "+
				"IMPORTANT: You MUST call this tool before @mentioning any user, "+
				"because you need the exact nickname to construct the @mention format.",
			map[string]any{
				"group_code": stringProp(groupCodeDescription),
				"action": map[string]any{"type": "string", "enum": []any{"find", "list_bots", "list_all"},
					"description": "find — search a user by name (use when you need to @mention or look up someone); " +
						"list_bots — list bots and Yuanbao AI assistants; " +
						"list_all — list all members."},
				"name": stringProp("User name to search (partial match, case-insensitive). " +
					"Required for 'find'. Use the name the user mentioned in the conversation."),
				"mention": map[string]any{"type": "boolean",
					"description": "Set to true when you need to @mention/at someone in your reply. " +
						"The response will include the exact @mention format to use."},
			}, "group_code", "action")},
	{Name: "yb_send_dm", Emoji: "✉️", Handler: ybTool("send_dm", sendDM),
		Schema: toolSchema("yb_send_dm",
			"Send a private message (DM / 私信) to a group member by name, with optional media. "+
				"It looks the user up in the group member list (or pass user_id if known). "+
				"Use it when someone asks to privately message a user. "+
				"Supports text, images, and file attachments.",
			map[string]any{
				"group_code": stringProp("The group where the target user belongs. " +
					"Extract from chat_id: 'group:328306697' → '328306697'. " +
					"Required when user_id is not provided."),
				"name": stringProp("Target user's display name (partial match, case-insensitive). " +
					"Required when user_id is not provided."),
				"message": stringProp("The message text to send as a DM. Can be empty if only sending media."),
				"user_id": stringProp("Target user's account ID. If provided, skips the member lookup. " +
					"Usually obtained from a previous yb_query_group_members call."),
				"media_files": map[string]any{"type": "array",
					"description": "Optional list of media files to send along with the DM. " +
						"Images (.jpg/.png/.gif/.webp/.bmp) are sent as image messages; " +
						"other files are sent as document attachments.",
					"items": map[string]any{"type": "object",
						"properties": map[string]any{
							"path":     stringProp("Absolute local file path of the media to send."),
							"is_voice": map[string]any{"type": "boolean", "description": "Whether this file is a voice message (default false)."},
						},
						"required": []any{"path"}}},
			})},
	{Name: "yb_search_sticker", Emoji: "🔍", Handler: ybTool("search_sticker", searchSticker),
		Schema: toolSchema("yb_search_sticker",
			"Search the built-in Yuanbao sticker (TIM face / 表情包) catalogue by keyword. "+
				"Returns the top matching candidates with sticker_id, name, and description. "+
				"Use this BEFORE yb_send_sticker to discover the right sticker_id. "+
				"Sticker = 贴纸 = TIM face — NOT a message reaction. "+
				"Prefer sending a sticker over bare Unicode emoji when reacting/expressing emotion.",
			map[string]any{
				"query": stringProp("Search keyword (Chinese or English, e.g. '666', '比心', 'cool', '吃瓜'). " +
					"Empty string returns the first N stickers."),
				"limit": map[string]any{"type": "integer", "description": "Max number of candidates to return (default 10, max 50)."},
			})},
	{Name: "yb_send_sticker", Emoji: "🎨", Handler: ybTool("send_sticker", sendSticker),
		Schema: toolSchema("yb_send_sticker",
			"Send a built-in sticker (TIMFaceElem / 贴纸表情) to the current Yuanbao chat. "+
				"Call yb_search_sticker first if you don't know the sticker_id/name. "+
				"Sticker = 贴纸 = TIM face — NOT a message reaction. "+
				"CRITICAL: Whenever the user asks you to send a sticker / 贴纸 / 表情包, you MUST "+
				"use this tool. DO NOT draw a PNG via execute_code / Pillow / matplotlib and "+
				"then call send_image_file — that produces a fake 'sticker' image instead of a "+
				"real TIM face and is the WRONG path. If no suitable sticker_id is known, call "+
				"yb_search_sticker first. When the recent thread shows users sending stickers, "+
				"prefer matching that tone by replying with a sticker instead of (or in "+
				"addition to) text.",
			map[string]any{
				"sticker": stringProp("Sticker name (e.g. '六六六', '比心', 'ok') or numeric sticker_id " +
					"(e.g. '278'). Empty string sends a random built-in sticker."),
				"chat_id": stringProp("Target chat. Defaults to the current session. " +
					"Format: 'direct:{account_id}', 'group:{group_code}', or bare account_id."),
				"reply_to": stringProp("Optional ref_msg_id to quote-reply (group chat only)."),
			})},
}

// Registers the Yuanbao tools, available in a Yuanbao session or while an adapter is connected
func init() {
	for _, def := range yuanbaoToolDefs {
		def.Toolset, def.Check = yuanbaoToolset, checkYuanbao
		tools.Register(def)
	}
}
