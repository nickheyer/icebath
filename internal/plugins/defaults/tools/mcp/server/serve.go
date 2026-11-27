package server

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/approvals"
	"github.com/nickheyer/icebath/internal/config"
	mcptools "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/tools"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/tools"
	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const bridgeInstructions = "Icebath Agent messaging bridge. Use these tools to interact with conversations across Telegram, Discord, Slack, " +
	"WhatsApp, Signal, Matrix, and other connected platforms."

const toolsInstructions = " Also exposes Icebath's own capabilities (web search/extract, browser automation, vision analysis, image generation, " +
	"skills and text-to-speech) as directly callable tools."

// Largest event cursor a client may pass, and how many events the bridge keeps
const (
	maxCursor   = int64(1_000_000_000_000_000_000)
	queueLength = 1000
)

// Arguments an MCP client may not set because they write outside icebath's cache
var droppedArgs = map[string][]string{"text_to_speech": {"output_path"}}

// Argument that must hold web URLs, and whether data URLs pass too
var urlArgs = map[string]struct {
	arg       string
	allowData bool
}{"browser_navigate": {"url", false}, "web_extract": {"urls", false}, "vision_analyze": {"image_url", true}}

var schemeRE = regexp.MustCompile(`^[\s\v\x{85}\p{Z}]*([a-zA-Z][a-zA-Z0-9+.-]*):(?:\P{Nd}|$)`)

// Whether a value is an http or https URL or a bare host, a data URL only when allowed
func webURL(value any, allowData bool) bool {
	text, _ := value.(string)
	m := schemeRE.FindStringSubmatch(text)
	if m == nil {
		rest := strings.TrimLeftFunc(text, unicode.IsSpace)
		return strings.TrimSpace(text) != "" && !strings.HasPrefix(rest, "/") && !strings.HasPrefix(rest, "~") && !strings.HasPrefix(rest, "\\") &&
			!strings.HasPrefix(rest, ".")
	}
	scheme := strings.ToLower(m[1])
	return scheme == "http" || scheme == "https" || allowData && scheme == "data"
}

// Why the arguments may not reach the tool from an MCP client, empty when they may
func refusal(tool string, args map[string]any) string {
	rule, ok := urlArgs[tool]
	if !ok || args[rule.arg] == nil {
		return ""
	}
	values, isList := args[rule.arg].([]any)
	if !isList {
		values = []any{args[rule.arg]}
	}
	for _, v := range values {
		if !webURL(v, rule.allowData) {
			kinds := "http(s) URLs"
			if rule.allowData {
				kinds = "http(s) or data: URLs"
			}
			quoted, _ := json.Marshal(v)
			return fmt.Sprintf("%s over MCP accepts only %s, not local files: %s", tool, kinds, quoted)
		}
	}
	return ""
}

// Tool environment of a call from an MCP client: nobody to approve or answer, so what asks is refused
func serveEnv(cfg *config.Config, directory string) *tools.Env {
	return &tools.Env{
		Config:   cfg,
		Cwd:      directory,
		Platform: "mcp",
		Approve: func(context.Context, string, string) error {
			return errors.New("no one can approve this over MCP")
		},
		Gate: func(context.Context, tools.Gate) error {
			return &tools.Refusal{Message: "BLOCKED: this needs approval and no one can approve it over MCP.", Outcome: "blocked"}
		},
		Command: func(context.Context, string, bool) tools.Verdict {
			return tools.Verdict{Refusal: &tools.Refusal{Message: "BLOCKED: commands need approval and no one can approve them over MCP.", Outcome: "blocked"}}
		},
		Clarify: func(context.Context, []tools.Question) (tools.Reply, error) {
			return tools.Reply{Outcome: tools.Undelivered, Notice: "No one answers questions over MCP."}, nil
		},
		Notify: func(tools.Notice) error { return nil },
		Yield:  func() bool { return false },
	}
}

func (s *service) CallTool(ctx context.Context, request *mcpv1.CallToolRequest) (*mcpv1.CallToolResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	name := request.GetName()
	if !slices.Contains(mcptools.Exposed, name) {
		return nil, connect.NewError(connect.CodePermissionDenied, name+" is not one of the tools icebath serves over MCP.")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(cmp.Or(request.GetArguments(), "{}")), &args); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "The arguments are not a JSON object: "+err.Error())
	}
	for _, arg := range droppedArgs[name] {
		delete(args, arg)
	}
	if reason := refusal(name, args); reason != "" {
		return &mcpv1.CallToolResponse{Result: tools.JSON(map[string]any{"error": reason, "tool": name})}, nil
	}
	data, _ := json.Marshal(args)
	result := s.tools.Dispatch(ctx, &tools.Call{Name: name, RawArgs: string(data), Env: serveEnv(cfg, request.GetDirectory())})
	return &mcpv1.CallToolResponse{Result: result, Failed: tools.Failed(result)}, nil
}

// Schema of a bridge tool from its parameters: required strings, optional strings and integers with defaults
func schema(params ...param) string {
	properties := map[string]any{}
	required := []string{}
	for _, p := range params {
		property := map[string]any{"type": p.kind}
		if p.def != nil {
			property["default"] = p.def
		}
		properties[p.name] = property
		if p.required {
			required = append(required, p.name)
		}
	}
	out := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		out["required"] = required
	}
	data, _ := json.Marshal(out)
	return string(data)
}

type param struct {
	name, kind string
	required   bool
	def        any
}

func str(name string) param              { return param{name: name, kind: "string", required: true} }
func optional(name string) param         { return param{name: name, kind: "string"} }
func integer(name string, def int) param { return param{name: name, kind: "integer", def: def} }

// Bridge tools in the order clients list them
var bridgeTools = []*mcpv1.ServeTool{
	{Name: "conversations_list", Description: "List active messaging conversations across connected platforms.\n\nReturns conversations with their session " +
		"keys (needed for messages_read),\nplatform, chat type, display name, and last activity time.\n\nArgs:\n    platform: Filter by platform name " +
		"(telegram, discord, slack, etc.)\n    limit: Maximum number of conversations to return (default 50)\n    search: Optional text to filter " +
		"conversations by name\n", Schema: schema(optional("platform"), integer("limit", 50), optional("search"))},
	{Name: "conversation_get", Description: "Get detailed info about one conversation by its session key.\n\nArgs:\n    session_key: The session key " +
		"from conversations_list\n", Schema: schema(str("session_key"))},
	{Name: "messages_read", Description: "Read recent messages from a conversation.\n\nReturns the message history in chronological order with role, " +
		"content,\nand timestamp for each message.\n\nArgs:\n    session_key: The session key from conversations_list\n    limit: Maximum number of " +
		"messages to return (default 50, most recent)\n", Schema: schema(str("session_key"), integer("limit", 50))},
	{Name: "attachments_fetch", Description: "List non-text attachments for a message in a conversation.\n\nExtracts images, media files, and other " +
		"non-text content blocks\nfrom the specified message.\n\nArgs:\n    session_key: The session key from conversations_list\n    message_id: The " +
		"message ID from messages_read\n", Schema: schema(str("session_key"), str("message_id"))},
	{Name: "events_poll", Description: "Poll for new conversation events since a cursor position.\n\nReturns events that have occurred since the given " +
		"cursor. Use the\nreturned next_cursor value for subsequent polls.\n\nEvent types: message, approval_requested, approval_resolved\n\nArgs:\n    " +
		"after_cursor: Return events after this cursor (0 for all)\n    session_key: Optional filter to one conversation\n    limit: Maximum events to " +
		"return (default 20)\n", Schema: schema(integer("after_cursor", 0), optional("session_key"), integer("limit", 20))},
	{Name: "events_wait", Description: "Wait for the next conversation event (long-poll).\n\nBlocks until a matching event arrives or the timeout " +
		"expires.\nUse this for near-real-time event delivery without polling.\n\nArgs:\n    after_cursor: Wait for events after this cursor\n    " +
		"session_key: Optional filter to one conversation\n    timeout_ms: Maximum wait time in milliseconds (default 30000)\n",
		Schema: schema(integer("after_cursor", 0), optional("session_key"), integer("timeout_ms", 30000))},
	{Name: "messages_send", Description: "Send a message to a platform conversation.\n\nThe target format is \"platform:chat_id\" — same format used by " +
		"the\nchannels_list tool. You can also use human-friendly channel names\nthat will be resolved automatically.\n\nExamples:\n    " +
		"target=\"telegram:6308981865\"\n    target=\"discord:#general\"\n    target=\"slack:#engineering\"\n\nArgs:\n    target: Platform target in " +
		"\"platform:identifier\" format\n    message: The message text to send\n", Schema: schema(str("target"), str("message"))},
	{Name: "channels_list", Description: "List available messaging channels and targets across platforms.\n\nReturns channels that you can send messages " +
		"to. The target strings\nreturned here can be used directly with the messages_send tool.\n\nArgs:\n    platform: Filter by platform name " +
		"(telegram, discord, slack, etc.)\n", Schema: schema(optional("platform"))},
	{Name: "permissions_list_open", Description: "List pending approval requests observed during this bridge session.\n\nReturns exec and plugin " +
		"approval requests that the bridge has seen\nsince it started. Approvals are live-session only — older approvals\nfrom before the bridge " +
		"connected are not included.\n", Schema: schema()},
	{Name: "permissions_respond", Description: "Respond to a pending approval request.\n\nArgs:\n    id: The approval ID from permissions_list_open\n    " +
		"decision: One of \"allow-once\", \"allow-always\", or \"deny\"\n", Schema: schema(str("id"), str("decision"))},
}

func (s *service) ServeTools(ctx context.Context, request *mcpv1.ServeToolsRequest) (*mcpv1.ServeToolsResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if request.GetTools() && request.GetAcceptHooks() {
		if err := s.hooks.Accept(ctx, cfg); err != nil {
			return nil, err
		}
	}
	response := &mcpv1.ServeToolsResponse{Instructions: bridgeInstructions}
	reserved := map[string]bool{}
	for _, t := range bridgeTools {
		response.Tools = append(response.Tools, &mcpv1.ServeTool{Name: t.GetName(), Description: t.GetDescription(), Schema: t.GetSchema()})
		reserved[t.GetName()] = true
	}
	if !request.GetTools() {
		return response, nil
	}
	response.Instructions += toolsInstructions
	definitions := map[string]map[string]any{}
	for _, definition := range s.tools.Definitions(serveEnv(cfg, "")) {
		if function, ok := definition["function"].(map[string]any); ok {
			name, _ := function["name"].(string)
			definitions[name] = function
		}
	}
	for _, name := range mcptools.Exposed {
		function, ok := definitions[name]
		if !ok || reserved[name] {
			continue
		}
		parameters, _ := function["parameters"].(map[string]any)
		parameters = maps.Clone(parameters)
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if properties, ok := parameters["properties"].(map[string]any); ok {
			properties = maps.Clone(properties)
			for _, arg := range droppedArgs[name] {
				delete(properties, arg)
			}
			parameters["properties"] = properties
		}
		if required := requiredOf(parameters["required"]); required != nil {
			parameters["required"] = slices.DeleteFunc(required, func(arg string) bool { return slices.Contains(droppedArgs[name], arg) })
		}
		description, _ := function["description"].(string)
		data, _ := json.Marshal(parameters)
		response.Tools = append(response.Tools, &mcpv1.ServeTool{Name: name, Description: cmp.Or(description, "Icebath "+name+" tool"), Schema: string(data),
			Agent: true})
	}
	return response, nil
}

func requiredOf(v any) []string {
	switch list := v.(type) {
	case []string:
		return slices.Clone(list)
	case []any:
		var out []string
		for _, item := range list {
			if name, ok := item.(string); ok {
				out = append(out, name)
			}
		}
		return out
	}
	return nil
}

// Event of the bridge: a message of a conversation
type event struct {
	cursor int64
	key    string
	// message, approval_requested or approval_resolved
	kind string
	data map[string]any
}

func (e event) dict() map[string]any {
	out := map[string]any{"cursor": e.cursor, "type": e.kind, "session_key": e.key}
	maps.Copy(out, e.data)
	return out
}

// Approval prompt the bridge saw a session put to the user since it started
type seenApproval struct {
	approvals.Prompt
	session string
	key     string
}

// Answers permissions_respond takes, and the answer each gives
var decisions = map[string]string{"allow-once": approvals.Once, "allow-always": approvals.Always, "deny": approvals.Deny}

// Messaging bridge of a profile: its conversations read from the session store, the messages that arrive queued as events
type bridge struct {
	store   sessions.Store
	profile string
	started time.Time
	mu      sync.Mutex
	queue   []event
	cursor  int64
	// Newest message seen of each session, by id
	seen map[string]int64
	// Closed and replaced as each event is queued
	arrived chan struct{}
	stop    func()
	// Prompts of the profile's sessions waiting on the user, by id, and what answers them
	approvals *approvals.Approvals
	pending   map[string]seenApproval
}

// Bridge of the profile, started watching the store on first use
func (s *service) bridge(cfg *config.Config) (*bridge, error) {
	store, err := s.sessions.Store(cfg.GetString(sessionsconfig.Store))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.bridges[cfg.Profile()]; b != nil {
		return b, nil
	}
	b := &bridge{store: store, profile: cfg.Profile(), started: time.Now(), seen: map[string]int64{}, arrived: make(chan struct{}), approvals: s.approvals,
		pending: map[string]seenApproval{}}
	changes, stop := store.Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	unobserve := s.approvals.Observe(func(session string, prompts []approvals.Prompt) { b.prompted(ctx, session, prompts) })
	b.stop = func() {
		cancel()
		stop()
		unobserve()
	}
	go b.watch(ctx, changes)
	s.bridges[cfg.Profile()] = b
	return b, nil
}

// Conversations of the profile by key: sessions a messaging platform started, the newest of each key
func (b *bridge) conversations(ctx context.Context) (map[string]*sessionsv1.Session, []string, error) {
	list, _, err := b.store.List(ctx, &sessionsv1.Filter{Profile: b.profile, IncludeArchived: true, IncludeHidden: true})
	if err != nil {
		return nil, nil, err
	}
	out := map[string]*sessionsv1.Session{}
	var keys []string
	for _, session := range list {
		peer := session.GetPeer()
		if peer == nil {
			continue
		}
		key := cmp.Or(peer.GetSessionKey(), session.GetId())
		if have, ok := out[key]; ok && have.GetUpdated().AsTime().After(session.GetUpdated().AsTime()) {
			continue
		}
		if _, ok := out[key]; !ok {
			keys = append(keys, key)
		}
		out[key] = session
	}
	return out, keys, nil
}

// Queues the messages that arrive in the conversations as the store changes
func (b *bridge) watch(ctx context.Context, changes <-chan []string) {
	for {
		select {
		case <-ctx.Done():
			return
		case ids, ok := <-changes:
			if !ok {
				return
			}
			b.poll(ctx, ids)
		}
	}
}

func (b *bridge) poll(ctx context.Context, ids []string) {
	conversations, _, err := b.conversations(ctx)
	if err != nil {
		return
	}
	for key, session := range conversations {
		if len(ids) > 0 && !slices.Contains(ids, session.GetId()) {
			continue
		}
		messages, err := b.store.Messages(ctx, session.GetId(), sessions.Display)
		if err != nil {
			continue
		}
		b.mu.Lock()
		last, known := b.seen[session.GetId()]
		for _, message := range messages {
			newer := message.GetId() > last
			if !known {
				newer = message.GetTime().AsTime().After(b.started)
			}
			if !newer {
				continue
			}
			if message.GetId() > b.seen[session.GetId()] {
				b.seen[session.GetId()] = message.GetId()
			}
			role, text := roleOf(message), content(message)
			if role == "" || text == "" {
				continue
			}
			b.enqueue("message", key, map[string]any{"role": role, "content": strs.Head(text, 500), "timestamp": stamp(message),
				"message_id": strconv.FormatInt(message.GetId(), 10)})
		}
		if !known {
			for _, message := range messages {
				b.seen[session.GetId()] = max(b.seen[session.GetId()], message.GetId())
			}
		}
		b.mu.Unlock()
	}
}

// Queues an event, waking the clients waiting for one, b.mu held
func (b *bridge) enqueue(kind, key string, data map[string]any) {
	b.cursor++
	b.queue = append(b.queue, event{cursor: b.cursor, key: key, kind: kind, data: data})
	if len(b.queue) > queueLength {
		b.queue = b.queue[len(b.queue)-queueLength:]
	}
	close(b.arrived)
	b.arrived = make(chan struct{})
}

// Takes in the prompts a session of the profile waits on now: a new one queued as approval_requested, one gone as approval_resolved
func (b *bridge) prompted(ctx context.Context, id string, prompts []approvals.Prompt) {
	session, err := b.store.Get(ctx, id)
	if err != nil || session.GetProfile() != b.profile {
		return
	}
	key := cmp.Or(session.GetPeer().GetSessionKey(), id)
	b.mu.Lock()
	defer b.mu.Unlock()
	waiting := map[string]bool{}
	for _, p := range prompts {
		waiting[p.ID] = true
		if _, known := b.pending[p.ID]; known {
			continue
		}
		b.pending[p.ID] = seenApproval{Prompt: p, session: id, key: key}
		b.enqueue("approval_requested", key, map[string]any{"approval_id": p.ID, "session_id": id, "command": p.Command, "description": p.Description,
			"created_at": p.Created.UTC().Format(time.RFC3339Nano)})
	}
	for approvalID, seen := range b.pending {
		if seen.session == id && !waiting[approvalID] {
			delete(b.pending, approvalID)
			b.enqueue("approval_resolved", key, map[string]any{"approval_id": approvalID})
		}
	}
}

// Prompts waiting now that the bridge saw asked, oldest first
func (b *bridge) open() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := slices.Collect(maps.Values(b.pending))
	slices.SortFunc(seen, func(x, y seenApproval) int { return x.Created.Compare(y.Created) })
	out := []map[string]any{}
	for _, a := range seen {
		out = append(out, map[string]any{"id": a.ID, "session_id": a.session, "session_key": a.key, "command": a.Command, "description": a.Description,
			"created_at": a.Created.UTC().Format(time.RFC3339Nano), "expires_at": a.Deadline.UTC().Format(time.RFC3339Nano)})
	}
	return out
}

// Answers a prompt the bridge saw asked, the resolution queued with the decision
func (b *bridge) respond(id, decision string) map[string]any {
	b.mu.Lock()
	seen, ok := b.pending[id]
	b.mu.Unlock()
	if !ok || b.approvals.Resolve(seen.session, approvals.Answer{ID: id, Choice: decisions[decision]}) == 0 {
		return map[string]any{"error": "Approval not found: " + id}
	}
	b.mu.Lock()
	delete(b.pending, id)
	b.enqueue("approval_resolved", seen.key, map[string]any{"approval_id": id, "decision": decision})
	b.mu.Unlock()
	return map[string]any{"resolved": true, "approval_id": id, "decision": decision}
}

// Events after the cursor, of the conversation when one is named, at most limit of them
func (b *bridge) matching(after int64, key string, limit int) ([]map[string]any, chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []map[string]any{}
	for _, e := range b.queue {
		if e.cursor > after && (key == "" || e.key == key) && len(out) < limit {
			out = append(out, e.dict())
		}
	}
	return out, b.arrived
}

func roleOf(message *sessionsv1.Message) string {
	switch message.GetRole() {
	case sessionsv1.Role_ROLE_USER:
		return "user"
	case sessionsv1.Role_ROLE_ASSISTANT:
		return "assistant"
	}
	return ""
}

// Text of a message the person or the agent wrote, empty for one hidden from them
func content(message *sessionsv1.Message) string {
	if message.GetRemoved() || message.GetModelOnly() || message.GetDisplay() == "hidden" {
		return ""
	}
	return strings.TrimSpace(message.GetText())
}

func stamp(message *sessionsv1.Message) string {
	if message.GetTime() == nil {
		return ""
	}
	return message.GetTime().AsTime().UTC().Format(time.RFC3339Nano)
}

func iso(session *sessionsv1.Session, created bool) string {
	at := session.GetUpdated()
	if created {
		at = session.GetCreated()
	}
	if at == nil {
		return ""
	}
	return at.AsTime().UTC().Format(time.RFC3339Nano)
}

// Field of where a session came from, as its origin JSON holds it
func origin(session *sessionsv1.Session, key string) any {
	var fields map[string]any
	json.Unmarshal([]byte(session.GetPeer().GetOrigin()), &fields)
	return fields[key]
}

func text(v any) string {
	s, _ := v.(string)
	return s
}

// Integer argument within bounds, the default when it is missing or no number
func bounded(args map[string]any, key string, def, least, most int64) int64 {
	n := def
	switch v := args[key].(type) {
	case float64:
		n = int64(v)
	case string:
		if parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			n = parsed
		}
	}
	return min(max(n, least), most)
}

func platformMatches(wanted, actual string) bool {
	return wanted == "" || strings.EqualFold(wanted, actual)
}

func dump(v any) string {
	data, _ := json.MarshalIndent(v, "", "  ")
	return string(data)
}

func failure(message string) string {
	data, _ := json.Marshal(map[string]any{"error": message})
	return string(data)
}

func (s *service) Bridge(ctx context.Context, request *mcpv1.BridgeRequest) (*mcpv1.BridgeResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(cmp.Or(request.GetArguments(), "{}")), &args); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "The arguments are not a JSON object: "+err.Error())
	}
	b, err := s.bridge(cfg)
	if err != nil {
		return nil, err
	}
	result, err := b.run(ctx, request.GetName(), args)
	if err != nil {
		return nil, err
	}
	return &mcpv1.BridgeResponse{Result: result}, nil
}

func (b *bridge) run(ctx context.Context, name string, args map[string]any) (string, error) {
	switch name {
	case "events_poll":
		events, _ := b.matching(bounded(args, "after_cursor", 0, 0, maxCursor), text(args["session_key"]), int(bounded(args, "limit", 20, 1, 200)))
		var next any = bounded(args, "after_cursor", 0, 0, maxCursor)
		if len(events) > 0 {
			next = events[len(events)-1]["cursor"]
		}
		return dump(map[string]any{"events": events, "next_cursor": next}), nil
	case "events_wait":
		after, key := bounded(args, "after_cursor", 0, 0, maxCursor), text(args["session_key"])
		timer := time.NewTimer(time.Duration(bounded(args, "timeout_ms", 30000, 0, 300000)) * time.Millisecond)
		defer timer.Stop()
		for {
			found, arrived := b.matching(after, key, 1)
			if len(found) > 0 {
				return dump(map[string]any{"event": found[0]}), nil
			}
			select {
			case <-arrived:
			case <-timer.C:
				return dump(map[string]any{"event": nil, "reason": "timeout"}), nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	case "messages_send":
		target, message := text(args["target"]), text(args["message"])
		if target == "" || message == "" {
			return failure("Both target and message are required"), nil
		}
		return failure("Send failed: no messaging gateway runs in this icebath to deliver to " + target), nil
	case "permissions_list_open":
		open := b.open()
		return dump(map[string]any{"count": len(open), "approvals": open}), nil
	case "permissions_respond":
		decision := text(args["decision"])
		if _, ok := decisions[decision]; !ok {
			return failure(fmt.Sprintf("Invalid decision: %s. Must be allow-once, allow-always, or deny", decision)), nil
		}
		return dump(b.respond(text(args["id"]), decision)), nil
	}
	conversations, keys, err := b.conversations(ctx)
	if err != nil {
		return "", err
	}
	switch name {
	case "conversations_list":
		platform, search := text(args["platform"]), strings.ToLower(text(args["search"]))
		var out []map[string]any
		for _, key := range keys {
			session := conversations[key]
			peer := session.GetPeer()
			if !platformMatches(platform, session.GetSource()) {
				continue
			}
			display, chat := peer.GetDisplayName(), text(origin(session, "chat_name"))
			if search != "" && !slices.ContainsFunc([]string{display, chat, key}, func(s string) bool { return strings.Contains(strings.ToLower(s), search) }) {
				continue
			}
			out = append(out, map[string]any{"session_key": key, "session_id": session.GetId(), "platform": session.GetSource(),
				"chat_type": peer.GetChatType(), "display_name": display, "chat_name": chat, "user_name": text(origin(session, "user_name")),
				"updated_at": iso(session, false)})
		}
		slices.SortStableFunc(out, func(a, b map[string]any) int { return strings.Compare(text(b["updated_at"]), text(a["updated_at"])) })
		if limit := int(bounded(args, "limit", 50, 1, 200)); len(out) > limit {
			out = out[:limit]
		}
		if out == nil {
			out = []map[string]any{}
		}
		return dump(map[string]any{"count": len(out), "conversations": out}), nil
	case "channels_list":
		platform := text(args["platform"])
		out := []map[string]any{}
		seen := map[string]bool{}
		for _, key := range keys {
			session := conversations[key]
			peer := session.GetPeer()
			target := session.GetSource() + ":" + peer.GetChatId()
			if session.GetSource() == "" || peer.GetChatId() == "" || !platformMatches(platform, session.GetSource()) || seen[target] {
				continue
			}
			seen[target] = true
			out = append(out, map[string]any{"target": target, "platform": session.GetSource(),
				"name": cmp.Or(peer.GetDisplayName(), text(origin(session, "chat_name"))), "chat_type": peer.GetChatType()})
		}
		return dump(map[string]any{"count": len(out), "channels": out}), nil
	}
	key := text(args["session_key"])
	session, ok := conversations[key]
	if !ok {
		return failure("Conversation not found: " + key), nil
	}
	if name == "conversation_get" {
		peer, usage := session.GetPeer(), session.GetUsage()
		return dump(map[string]any{"session_key": key, "session_id": session.GetId(), "platform": session.GetSource(), "chat_type": peer.GetChatType(),
			"display_name": peer.GetDisplayName(), "user_name": text(origin(session, "user_name")), "chat_name": text(origin(session, "chat_name")),
			"chat_id": peer.GetChatId(), "thread_id": strs.Nullable(peer.GetThreadId()), "updated_at": iso(session, false), "created_at": iso(session, true),
			"input_tokens": usage.GetInput(), "output_tokens": usage.GetOutput(), "total_tokens": usage.GetInput() + usage.GetOutput()}), nil
	}
	messages, err := b.store.Messages(ctx, session.GetId(), sessions.Display)
	if err != nil {
		return failure(err.Error()), nil
	}
	switch name {
	case "messages_read":
		var shown []map[string]any
		for _, message := range messages {
			if role, body := roleOf(message), content(message); role != "" && body != "" {
				shown = append(shown, map[string]any{"id": strconv.FormatInt(message.GetId(), 10), "role": role, "content": strs.Head(body, 2000),
					"timestamp": stamp(message)})
			}
		}
		total := len(shown)
		shown = shown[max(total-int(bounded(args, "limit", 50, 1, 200)), 0):]
		if shown == nil {
			shown = []map[string]any{}
		}
		return dump(map[string]any{"session_key": key, "count": len(shown), "total_in_session": total, "messages": shown}), nil
	case "attachments_fetch":
		id := text(args["message_id"])
		for _, message := range messages {
			if strconv.FormatInt(message.GetId(), 10) != id {
				continue
			}
			attachments := []map[string]any{}
			for _, media := range message.GetMedia() {
				kind := "media"
				switch {
				case strings.HasPrefix(media.GetMimeType(), "image/"):
					kind = "image"
				case strings.HasPrefix(media.GetMimeType(), "video/"):
					kind = "video"
				}
				attachments = append(attachments, map[string]any{"type": kind, "mime_type": media.GetMimeType(), "size": len(media.GetData()),
					"url": "data:" + media.GetMimeType() + ";base64," + base64.StdEncoding.EncodeToString(media.GetData())})
			}
			return dump(map[string]any{"message_id": id, "count": len(attachments), "attachments": attachments}), nil
		}
		return failure("Message not found: " + id), nil
	}
	return "", connect.NewError(connect.CodeNotFound, "No bridge tool is named "+name+".")
}
