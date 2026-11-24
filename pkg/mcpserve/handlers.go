package mcpserve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// MCP server name clients see in the initialize reply
const ServerName = "icebath"

const bridgeInstructions = "Icebath Agent messaging bridge. Use these tools to interact with " +
	"conversations across Telegram, Discord, Slack, WhatsApp, Signal, " +
	"Matrix, and other connected platforms."

const toolsInstructions = " Also exposes Icebath's own capabilities (web search/extract, browser automation, " +
	"vision analysis, image generation, skills and text-to-speech) as directly callable tools."

// Largest event cursor a client may pass
const maxCursor = int64(1_000_000_000_000_000_000)

// Reports whether a requested platform filter is unset or names actual ignoring case
func platformMatches(wanted any, actual any) bool {
	w, _ := wanted.(string)
	return w == "" || pyLower(actual) == strings.ToLower(w)
}

// Optional str argument as a string, empty for None
func optStr(v any) string {
	s, _ := v.(string)
	return s
}

// Value of a or b when a is falsy, the Python a or b
func pyOr(a any, b func() any) any {
	if pyrepr.Truthy(a) {
		return a
	}
	return b()
}

// Messages of a conversation or the JSON error text naming why they cannot be read
func conversationMessages(ctx context.Context, sessionKey string) ([]*pyjson.Object, string) {
	entry := pyGet(loadSessionsIndex(ctx), sessionKey, nil)
	if !pyrepr.Truthy(entry) {
		return nil, dumps(pyjson.ObjectOf("error", "Conversation not found: "+sessionKey), false)
	}
	sessionID := pyGet(entry, "session_id", "")
	if !pyrepr.Truthy(sessionID) {
		return nil, dumps(pyjson.ObjectOf("error", "No session ID for this conversation"), false)
	}
	messages, errText := loadSessionMessages(ctx, pyrepr.Str(sessionID))
	if errText != "" {
		return nil, dumps(pyjson.ObjectOf("error", errText), false)
	}
	return messages, ""
}

// The bridge tool handlers, each reading its validated keyword arguments
type toolHandlers struct {
	ctx    context.Context
	bridge *EventBridge
}

func (h *toolHandlers) conversationsList(kw *pyjson.Object) string {
	platform, search := kw.Vals["platform"], optStr(kw.Vals["search"])
	limit := coerceInt(kw.Vals["limit"], 50, 1, 200)
	index := loadSessionsIndex(h.ctx)
	conversations := []*pyjson.Object{}
	for _, key := range index.Keys {
		entry := index.Vals[key]
		origin := pyGet(entry, "origin", pyjson.NewObject())
		entryPlatform := pyOr(pyGet(entry, "platform", nil), func() any { return pyGet(origin, "platform", "") })
		if !platformMatches(platform, entryPlatform) {
			continue
		}
		displayName := pyGet(entry, "display_name", "")
		chatName := pyGet(origin, "chat_name", "")
		if search != "" {
			hit := false
			for _, s := range []any{displayName, chatName, key} {
				if strings.Contains(pyLower(s), strings.ToLower(search)) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		conversations = append(conversations, pyjson.ObjectOf(
			"session_key", key, "session_id", pyGet(entry, "session_id", ""), "platform", entryPlatform,
			"chat_type", pyGet(entry, "chat_type", pyGet(origin, "chat_type", "")),
			"display_name", displayName, "chat_name", chatName,
			"user_name", pyGet(origin, "user_name", ""), "updated_at", pyGet(entry, "updated_at", "")))
	}
	sortByKey(conversations, func(c *pyjson.Object) any { return pyGet(c, "updated_at", "") }, true)
	if int64(len(conversations)) > limit {
		conversations = conversations[:limit]
	}
	return dumps(pyjson.ObjectOf("count", len(conversations), "conversations", conversations), true)
}

func (h *toolHandlers) conversationGet(kw *pyjson.Object) string {
	sessionKey := optStr(kw.Vals["session_key"])
	entry := pyGet(loadSessionsIndex(h.ctx), sessionKey, nil)
	if !pyrepr.Truthy(entry) {
		return dumps(pyjson.ObjectOf("error", "Conversation not found: "+sessionKey), false)
	}
	origin := pyGet(entry, "origin", pyjson.NewObject())
	return dumps(pyjson.ObjectOf(
		"session_key", sessionKey, "session_id", pyGet(entry, "session_id", ""),
		"platform", pyOr(pyGet(entry, "platform", nil), func() any { return pyGet(origin, "platform", "") }),
		"chat_type", pyGet(entry, "chat_type", pyGet(origin, "chat_type", "")),
		"display_name", pyGet(entry, "display_name", ""),
		"user_name", pyGet(origin, "user_name", ""), "chat_name", pyGet(origin, "chat_name", ""),
		"chat_id", pyGet(origin, "chat_id", ""), "thread_id", pyGet(origin, "thread_id", nil),
		"updated_at", pyGet(entry, "updated_at", ""), "created_at", pyGet(entry, "created_at", ""),
		"input_tokens", pyGet(entry, "input_tokens", int64(0)), "output_tokens", pyGet(entry, "output_tokens", int64(0)),
		"total_tokens", pyGet(entry, "total_tokens", int64(0))), true)
}

func (h *toolHandlers) messagesRead(kw *pyjson.Object) string {
	sessionKey := optStr(kw.Vals["session_key"])
	limit := coerceInt(kw.Vals["limit"], 50, 1, 200)
	all, errJSON := conversationMessages(h.ctx, sessionKey)
	if errJSON != "" {
		return errJSON
	}
	filtered := []any{}
	for _, msg := range all {
		role := pyGet(msg, "role", "")
		pyHashable(role)
		content := ""
		if isStr(role, "user") || isStr(role, "assistant") {
			content = extractMessageContent(msg)
		}
		if content != "" {
			filtered = append(filtered, pyjson.ObjectOf("id", pyrepr.Str(pyGet(msg, "id", "")), "role", role,
				"content", textutil.First(content, 2000), "timestamp", pyGet(msg, "timestamp", "")))
		}
	}
	messages := filtered[max(int64(len(filtered))-limit, 0):]
	return dumps(pyjson.ObjectOf("session_key", sessionKey, "count", len(messages),
		"total_in_session", len(filtered), "messages", messages), true)
}

func (h *toolHandlers) attachmentsFetch(kw *pyjson.Object) string {
	sessionKey, messageID := optStr(kw.Vals["session_key"]), optStr(kw.Vals["message_id"])
	all, errJSON := conversationMessages(h.ctx, sessionKey)
	if errJSON != "" {
		return errJSON
	}
	var target any
	for _, m := range all {
		if pyrepr.Str(pyGet(m, "id", "")) == messageID {
			target = m
			break
		}
	}
	if !pyrepr.Truthy(target) {
		return dumps(pyjson.ObjectOf("error", "Message not found: "+messageID), false)
	}
	attachments := extractAttachments(target)
	return dumps(pyjson.ObjectOf("message_id", messageID, "count", len(attachments), "attachments", attachments), true)
}

func (h *toolHandlers) eventsPoll(kw *pyjson.Object) string {
	afterCursor := coerceInt(kw.Vals["after_cursor"], 0, 0, maxCursor)
	limit := coerceInt(kw.Vals["limit"], 20, 1, 200)
	return dumps(h.bridge.PollEvents(afterCursor, optStr(kw.Vals["session_key"]), limit), true)
}

func (h *toolHandlers) eventsWait(kw *pyjson.Object) string {
	afterCursor := coerceInt(kw.Vals["after_cursor"], 0, 0, maxCursor)
	timeoutMs := coerceInt(kw.Vals["timeout_ms"], 30000, 0, 300000)
	if event := h.bridge.WaitForEvent(afterCursor, optStr(kw.Vals["session_key"]), timeoutMs); pyrepr.Truthy(event) {
		return dumps(pyjson.ObjectOf("event", event), true)
	}
	return dumps(pyjson.ObjectOf("event", nil, "reason", "timeout"), true)
}

func (h *toolHandlers) messagesSend(kw *pyjson.Object) (result string) {
	target, message := optStr(kw.Vals["target"]), optStr(kw.Vals["message"])
	if target == "" || message == "" {
		return dumps(pyjson.ObjectOf("error", "Both target and message are required"), false)
	}
	defer func() {
		if r := recover(); r != nil {
			result = dumps(pyjson.ObjectOf("error", "Send failed: "+raisedText(r)), false)
		}
	}()
	text, err := messagetools.SendMessageTool(h.ctx, map[string]any{"action": "send", "target": target, "message": message})
	if err != nil {
		return dumps(pyjson.ObjectOf("error", "Send failed: "+err.Error()), false)
	}
	return text
}

func (h *toolHandlers) channelsList(kw *pyjson.Object) string {
	platform := kw.Vals["platform"]
	directory := loadChannelDirectory(h.ctx)
	if !pyrepr.Truthy(directory) {
		targets, seen := []any{}, map[string]bool{}
		index := loadSessionsIndex(h.ctx)
		for _, key := range index.Keys {
			entry := index.Vals[key]
			origin := pyGet(entry, "origin", pyjson.NewObject())
			p := pyOr(pyGet(entry, "platform", nil), func() any { return pyGet(origin, "platform", "") })
			chatID := pyGet(origin, "chat_id", "")
			targetStr := pyrepr.Str(p) + ":" + pyrepr.Str(chatID)
			if !pyrepr.Truthy(p) || !pyrepr.Truthy(chatID) || !platformMatches(platform, p) || seen[targetStr] {
				continue
			}
			seen[targetStr] = true
			targets = append(targets, pyjson.ObjectOf("target", targetStr, "platform", p,
				"name", pyOr(pyGet(entry, "display_name", nil), func() any { return pyGet(origin, "chat_name", "") }),
				"chat_type", pyGet(entry, "chat_type", pyGet(origin, "chat_type", ""))))
		}
		return dumps(pyjson.ObjectOf("count", len(targets), "channels", targets), true)
	}
	channels := []any{}
	keys, vals := pyItems(pyGet(directory, "platforms", pyjson.NewObject()))
	for _, plat := range keys {
		entries, isList := vals[plat].([]any)
		if !platformMatches(platform, plat) || !isList {
			continue
		}
		for _, ch := range entries {
			if _, ok := asDict(ch); !ok {
				continue
			}
			chatID := pyGet(ch, "id", pyGet(ch, "chat_id", ""))
			target := plat
			if pyrepr.Truthy(chatID) {
				target = plat + ":" + pyrepr.Str(chatID)
			}
			channels = append(channels, pyjson.ObjectOf("target", target, "platform", plat,
				"name", pyGet(ch, "name", pyGet(ch, "display_name", "")), "chat_type", pyGet(ch, "type", "")))
		}
	}
	return dumps(pyjson.ObjectOf("count", len(channels), "channels", channels), true)
}

func (h *toolHandlers) permissionsListOpen(*pyjson.Object) string {
	approvals := h.bridge.ListPendingApprovals()
	return dumps(pyjson.ObjectOf("count", len(approvals), "approvals", approvals), true)
}

func (h *toolHandlers) permissionsRespond(kw *pyjson.Object) string {
	id, decision := optStr(kw.Vals["id"]), optStr(kw.Vals["decision"])
	if decision != "allow-once" && decision != "allow-always" && decision != "deny" {
		return dumps(pyjson.ObjectOf("error", fmt.Sprintf("Invalid decision: %s. Must be allow-once, allow-always, or deny", decision)), false)
	}
	return dumps(h.bridge.RespondToApproval(id, decision), true)
}

// Keyword parameter of a bridge handler signature
func reqStr(name string) provider.MCPToolParam {
	return provider.MCPToolParam{Name: name, Type: "str", Required: true}
}
func optionalStr(name string) provider.MCPToolParam {
	return provider.MCPToolParam{Name: name, Type: "str"}
}
func intDefault(name string, def int64) provider.MCPToolParam {
	return provider.MCPToolParam{Name: name, Type: "int", Default: def, HasDefault: true}
}

// One bridge tool with its signature, wire description and handler
type bridgeTool struct {
	name        string
	description string
	params      []provider.MCPToolParam
	handler     func(h *toolHandlers, kw *pyjson.Object) string
}

// Bridge tools in registration order, which is the tools/list order clients see
var bridgeTools = []bridgeTool{
	{"conversations_list", "List active messaging conversations across connected platforms.\n\n" +
		"Returns conversations with their session keys (needed for messages_read),\n" +
		"platform, chat type, display name, and last activity time.\n\n" +
		"Args:\n" +
		"    platform: Filter by platform name (telegram, discord, slack, etc.)\n" +
		"    limit: Maximum number of conversations to return (default 50)\n" +
		"    search: Optional text to filter conversations by name\n",
		[]provider.MCPToolParam{optionalStr("platform"), intDefault("limit", 50), optionalStr("search")},
		(*toolHandlers).conversationsList},
	{"conversation_get", "Get detailed info about one conversation by its session key.\n\n" +
		"Args:\n" +
		"    session_key: The session key from conversations_list\n",
		[]provider.MCPToolParam{reqStr("session_key")},
		(*toolHandlers).conversationGet},
	{"messages_read", "Read recent messages from a conversation.\n\n" +
		"Returns the message history in chronological order with role, content,\n" +
		"and timestamp for each message.\n\n" +
		"Args:\n" +
		"    session_key: The session key from conversations_list\n" +
		"    limit: Maximum number of messages to return (default 50, most recent)\n",
		[]provider.MCPToolParam{reqStr("session_key"), intDefault("limit", 50)},
		(*toolHandlers).messagesRead},
	{"attachments_fetch", "List non-text attachments for a message in a conversation.\n\n" +
		"Extracts images, media files, and other non-text content blocks\n" +
		"from the specified message.\n\n" +
		"Args:\n" +
		"    session_key: The session key from conversations_list\n" +
		"    message_id: The message ID from messages_read\n",
		[]provider.MCPToolParam{reqStr("session_key"), reqStr("message_id")},
		(*toolHandlers).attachmentsFetch},
	{"events_poll", "Poll for new conversation events since a cursor position.\n\n" +
		"Returns events that have occurred since the given cursor. Use the\n" +
		"returned next_cursor value for subsequent polls.\n\n" +
		"Event types: message, approval_requested, approval_resolved\n\n" +
		"Args:\n" +
		"    after_cursor: Return events after this cursor (0 for all)\n" +
		"    session_key: Optional filter to one conversation\n" +
		"    limit: Maximum events to return (default 20)\n",
		[]provider.MCPToolParam{intDefault("after_cursor", 0), optionalStr("session_key"), intDefault("limit", 20)},
		(*toolHandlers).eventsPoll},
	{"events_wait", "Wait for the next conversation event (long-poll).\n\n" +
		"Blocks until a matching event arrives or the timeout expires.\n" +
		"Use this for near-real-time event delivery without polling.\n\n" +
		"Args:\n" +
		"    after_cursor: Wait for events after this cursor\n" +
		"    session_key: Optional filter to one conversation\n" +
		"    timeout_ms: Maximum wait time in milliseconds (default 30000)\n",
		[]provider.MCPToolParam{intDefault("after_cursor", 0), optionalStr("session_key"), intDefault("timeout_ms", 30000)},
		(*toolHandlers).eventsWait},
	{"messages_send", "Send a message to a platform conversation.\n\n" +
		"The target format is \"platform:chat_id\" — same format used by the\n" +
		"channels_list tool. You can also use human-friendly channel names\n" +
		"that will be resolved automatically.\n\n" +
		"Examples:\n" +
		"    target=\"telegram:6308981865\"\n" +
		"    target=\"discord:#general\"\n" +
		"    target=\"slack:#engineering\"\n\n" +
		"Args:\n" +
		"    target: Platform target in \"platform:identifier\" format\n" +
		"    message: The message text to send\n",
		[]provider.MCPToolParam{reqStr("target"), reqStr("message")},
		(*toolHandlers).messagesSend},
	{"channels_list", "List available messaging channels and targets across platforms.\n\n" +
		"Returns channels that you can send messages to. The target strings\n" +
		"returned here can be used directly with the messages_send tool.\n\n" +
		"Args:\n" +
		"    platform: Filter by platform name (telegram, discord, slack, etc.)\n",
		[]provider.MCPToolParam{optionalStr("platform")},
		(*toolHandlers).channelsList},
	{"permissions_list_open", "List pending approval requests observed during this bridge session.\n\n" +
		"Returns exec and plugin approval requests that the bridge has seen\n" +
		"since it started. Approvals are live-session only — older approvals\n" +
		"from before the bridge connected are not included.\n",
		nil,
		(*toolHandlers).permissionsListOpen},
	{"permissions_respond", "Respond to a pending approval request.\n\n" +
		"Args:\n" +
		"    id: The approval ID from permissions_list_open\n" +
		"    decision: One of \"allow-once\", \"allow-always\", or \"deny\"\n",
		[]provider.MCPToolParam{reqStr("id"), reqStr("decision")},
		(*toolHandlers).permissionsRespond},
}

// MCP server with every bridge tool and, with includeTools, the curated icebath tools after them
func CreateServer(ctx context.Context, bridge *EventBridge, includeTools bool, in io.Reader, out io.Writer) (*provider.MCPToolServer, error) {
	instructions := bridgeInstructions
	if includeTools {
		instructions += toolsInstructions
	}
	server := provider.NewMCPToolServer(ServerName, instructions, in, out, true)
	if bridge == nil {
		bridge = NewEventBridge(ctx)
	}
	handlers := &toolHandlers{ctx: ctx, bridge: bridge}
	reserved := map[string]bool{}
	for _, t := range bridgeTools {
		handler := t.handler
		server.AddTool(t.name, t.description, t.params, nil, func(kw *pyjson.Object) string { return handler(handlers, kw) })
		reserved[t.name] = true
	}
	if includeTools {
		n, err := registerIcebathTools(ctx, server, reserved)
		if err != nil {
			return nil, err
		}
		pkgLog().Info(fmt.Sprintf("exposed %d Icebath tools over MCP", n))
	}
	return server, nil
}

// Raised by a run a keyboard interrupt ended
var errInterrupted = errors.New("KeyboardInterrupt")

// Runs the server until stdin closes or a SIGINT arrives
func runInterruptible(server *provider.MCPToolServer) error {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- errors.New(raisedText(r))
			}
		}()
		done <- server.Run()
	}()
	select {
	case err := <-done:
		return err
	case <-sigs:
		return errInterrupted
	}
}

// Serves icebath over stdio MCP until the client disconnects and returns the exit status of icebath mcp-serve
func RunServer(ctx context.Context, verbose, includeTools, acceptHooks bool) (code int) {
	configureLogging(verbose)
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", raisedText(r))
			code = 1
		}
	}()
	if includeTools {
		if err := prepareToolProcess(ctx, acceptHooks); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", err.Error())
			return 1
		}
	}
	bridge := NewEventBridge(ctx)
	bridge.Start()
	defer bridge.Stop()
	server, err := CreateServer(ctx, bridge, includeTools, os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err.Error())
		return 1
	}
	if err := runInterruptible(server); err != nil && !errors.Is(err, errInterrupted) {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err.Error())
		return 1
	}
	return 0
}
