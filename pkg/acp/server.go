package acp

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent turns running at once across sessions, the size of the Python executor
const maxConcurrentTurns = 4

var turnSlots = make(chan struct{}, maxConcurrentTurns)

// Sessions returned per session/list page, clients paginate through the cursor
const listSessionsPageSize = 50

// Connects editor provided MCP servers and returns every MCP tool name
var registerMCPServers = func(ctx context.Context, env *tools.Env, configs map[string]map[string]any) ([]string, error) {
	servers := make(map[string]*mcp.Rendered, len(configs))
	for name, cfg := range configs {
		servers[name] = &mcp.Rendered{Config: cfg}
	}
	return mcp.Connect(ctx, env, servers)
}

// Shared model switch resolution of the CLI, gateway and TUI
var switchModelFn = provider.SwitchModel

// Whether background MCP discovery is still running
var mcpDiscoveryInFlight = mcp.DiscoveryInFlight

// Joins background MCP discovery, true once none is alive
var joinMCPDiscovery = func(ctx context.Context, timeout time.Duration) bool { return mcp.JoinDiscovery(ctx, &timeout) }

// Rebuilds a live agent tool snapshot from the registry and returns the added tool names
var refreshAgentMCPTools = func(ctx context.Context, a any) map[string]bool {
	snapshot, ok := a.(mcp.AgentToolSnapshot)
	if !ok {
		return nil
	}
	return mcp.RefreshAgentMCPTools(ctx, snapshot, mcp.RefreshOptions{Quiet: true})
}

// Client side calls the agent makes over its connection
type Client interface {
	SessionUpdater
	RequestPermission(ctx context.Context, options []pyacp.PermissionOption, sessionID string, toolCall *pyacp.ToolCallUpdate, meta *pyjson.Object) (*pyacp.RequestPermissionResponse, error)
}

// Session operations the in process agent drives
type Sessions interface {
	CreateSession(ctx context.Context, cwd string) (*SessionState, error)
	GetSession(ctx context.Context, sessionID string) *SessionState
	ForkSession(ctx context.Context, sessionID, cwd string) (*SessionState, error)
	ListSessions(ctx context.Context, cwd string) ([]*pyjson.Object, error)
	UpdateCwd(ctx context.Context, sessionID, cwd string) *SessionState
	SaveSession(ctx context.Context, sessionID string)
	MakeAgent(ctx context.Context, o MakeAgentOptions) (any, error)
	GetDB(ctx context.Context) *state.SessionDB
	memorySession(sessionID string) *SessionState
}

// Python dict like value of a history message, keys in order
func historyDict(v any) (*pyjson.Object, bool) {
	switch x := v.(type) {
	case *pyjson.Object:
		return x, x != nil
	case map[string]any:
		o := pyjson.NewObject()
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			o.Set(k, historyValue(x[k]))
		}
		return o, true
	case interface{ PyItems() iter.Seq2[any, any] }:
		o := pyjson.NewObject()
		for k, val := range x.PyItems() {
			o.Set(pyStr(k), historyValue(val))
		}
		return o, true
	}
	return nil, false
}

// Python list like value of a history message
func historyList(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case []map[string]any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// History value with every nested dict as an ordered object and every list as a plain list
func historyValue(v any) any {
	if d, ok := historyDict(v); ok {
		return d
	}
	if l, ok := historyList(v); ok {
		out := make([]any, len(l))
		for i, item := range l {
			out[i] = historyValue(item)
		}
		return out
	}
	return v
}

// Persisted content or reasoning as one stripped string, empty when there is nothing to emit
func flattenHistoryText(value any) string {
	if s, ok := value.(string); ok {
		return pystr.Strip(s)
	}
	items, ok := historyList(value)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range items {
		if d, ok := historyDict(item); ok {
			if text, ok := d.GetOr("text", nil).(string); ok {
				parts = append(parts, text)
			} else if content, ok := d.GetOr("content", nil).(string); ok && d.GetOr("type", nil) == "text" {
				parts = append(parts, content)
			}
		} else if s, ok := item.(string); ok {
			parts = append(parts, s)
		}
	}
	var kept []string
	for _, part := range parts {
		if stripped := pystr.Strip(part); stripped != "" {
			kept = append(kept, stripped)
		}
	}
	return pystr.Strip(strings.Join(kept, "\n"))
}

// First non empty of reasoning_content and reasoning
func historyReasoningText(message map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if text := flattenHistoryText(message[key]); text != "" {
			return text
		}
	}
	return ""
}

// Meta of a replayed compaction summary, nil for ordinary content
func historySummaryMeta(message map[string]any, text string) *pyjson.Object {
	kind := state.ClassifySummaryContent(text)
	if kind == "" && truthy(message[state.CompressedSummaryMetadataKey]) {
		kind = "standalone"
	}
	key := map[string]string{"standalone": "compactionSummary", "merged": "containsCompactionSummary"}[kind]
	if key == "" {
		return nil
	}
	inner := pyjson.NewObject()
	inner.Set(key, true)
	meta := pyjson.NewObject()
	meta.Set("hermes", inner)
	return meta
}

// Function name and arguments of an OpenAI style tool call
func historyToolCallNameArgs(toolCall *pyjson.Object) (string, *pyjson.Object) {
	function, ok := historyDict(toolCall.GetOr("function", nil))
	if !ok {
		function = pyjson.NewObject()
	}
	name := "unknown_tool"
	for _, candidate := range []any{function.GetOr("name", nil), toolCall.GetOr("name", nil)} {
		if truthy(candidate) {
			name = pyStr(candidate)
			break
		}
	}
	var raw any = pyjson.NewObject()
	for _, candidate := range []any{function.GetOr("arguments", nil), toolCall.GetOr("arguments", nil), toolCall.GetOr("args", nil)} {
		if truthy(candidate) {
			raw = candidate
			break
		}
	}
	return name, CoerceToolArgs(raw)
}

// User or assistant chunk of a history message, nil when it has no text
func historyMessageChunk(role string, message map[string]any) pyacp.SessionUpdate {
	text := flattenHistoryText(message["content"])
	if text == "" {
		return nil
	}
	meta := historySummaryMeta(message, text)
	if role == "user" {
		return &pyacp.UserMessageChunk{Content: pyacp.TextBlock(text), Meta: meta}
	}
	return &pyacp.AgentMessageChunk{Content: pyacp.TextBlock(text), Meta: meta}
}

type replayedCall struct {
	name string
	args *pyjson.Object
}

// Session updates that reconstruct a persisted transcript in order, text, thoughts and tool call pairs
func historyReplayUpdates(history []map[string]any) []pyacp.SessionUpdate {
	var updates []pyacp.SessionUpdate
	active := map[string]replayedCall{}
	for _, message := range history {
		role := ""
		if truthy(message["role"]) {
			role = pyStr(message["role"])
		}
		switch role {
		case "user":
			if chunk := historyMessageChunk(role, message); chunk != nil {
				updates = append(updates, chunk)
			}
		case "assistant":
			if thought := historyReasoningText(message); thought != "" {
				updates = append(updates, pyacp.UpdateAgentThoughtText(thought))
			}
			if chunk := historyMessageChunk(role, message); chunk != nil {
				updates = append(updates, chunk)
			}
			toolCalls, _ := historyList(message["tool_calls"])
			for _, raw := range toolCalls {
				toolCall, ok := historyDict(raw)
				if !ok {
					continue
				}
				id := ""
				for _, key := range []string{"id", "call_id", "tool_call_id"} {
					if v := toolCall.GetOr(key, nil); truthy(v) {
						id = pystr.Strip(pyStr(v))
						break
					}
				}
				if id == "" {
					continue
				}
				name, args := historyToolCallNameArgs(toolCall)
				active[id] = replayedCall{name, args}
				updates = append(updates, BuildToolStart(id, name, args, nil))
			}
		case "tool":
			id, name := "", ""
			if truthy(message["tool_call_id"]) {
				id = pystr.Strip(pyStr(message["tool_call_id"]))
			}
			if truthy(message["tool_name"]) {
				name = pystr.Strip(pyStr(message["tool_name"]))
			}
			var args *pyjson.Object
			if call, ok := active[id]; ok {
				delete(active, id)
				name, args = call.name, call.args
			}
			if id == "" || name == "" {
				continue
			}
			var resultText *string
			if s, ok := message["content"].(string); ok {
				resultText = &s
			}
			updates = append(updates, BuildToolComplete(id, name, resultText, args, nil, false))
			if name == "todo" && resultText != nil {
				if plan := buildPlanUpdateFromTodoResult(*resultText); plan != nil {
					updates = append(updates, plan)
				}
			}
		}
	}
	return updates
}

// Config of an editor provided MCP server as the registry reads it
func mcpServerConfig(server pyacp.McpServer) (string, map[string]any) {
	pairs := func(names, values []string) map[string]any {
		out := map[string]any{}
		for i := range names {
			out[names[i]] = values[i]
		}
		return out
	}
	switch s := server.(type) {
	case *pyacp.McpServerStdio:
		names, values := make([]string, len(s.Env)), make([]string, len(s.Env))
		for i, e := range s.Env {
			names[i], values[i] = e.Name, e.Value
		}
		args := make([]any, len(s.Args))
		for i, arg := range s.Args {
			args[i] = arg
		}
		return s.Name, map[string]any{"command": s.Command, "args": args, "env": pairs(names, values)}
	case *pyacp.HttpMcpServer:
		names, values := make([]string, len(s.Headers)), make([]string, len(s.Headers))
		for i, h := range s.Headers {
			names[i], values[i] = h.Name, h.Value
		}
		return s.Name, map[string]any{"url": s.URL, "headers": pairs(names, values)}
	case *pyacp.SseMcpServer:
		names, values := make([]string, len(s.Headers)), make([]string, len(s.Headers))
		for i, h := range s.Headers {
			names[i], values[i] = h.Name, h.Value
		}
		return s.Name, map[string]any{"url": s.URL, "headers": pairs(names, values)}
	}
	return "", nil
}

// Sets or removes a process environment variable
func restoreEnv(key string, value string, present bool) {
	if present {
		os.Setenv(key, value)
	} else {
		os.Unsetenv(key)
	}
}

// Teardowns of one turn's bindings, run last in first out
type exitStack struct{ teardowns []func() }

func (s *exitStack) callback(fn func()) { s.teardowns = append(s.teardowns, fn) }

func (s *exitStack) close() {
	for i := len(s.teardowns) - 1; i >= 0; i-- {
		s.teardowns[i]()
	}
}

// Runs setup and registers the teardown it returns, a failure of either half only logged
func bindGuarded(stack *exitStack, label string, setup func() (func(), error)) {
	teardown, err := func() (teardown func(), err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return setup()
	}()
	if err != nil {
		serverLog.Debug(fmt.Sprintf("Could not set ACP %s\n%s", label, err))
		return
	}
	stack.callback(func() {
		defer func() {
			if r := recover(); r != nil {
				serverLog.Debug(fmt.Sprintf("Could not restore ACP %s\n%v", label, r))
			}
		}()
		teardown()
	})
}

func attachInterruptedPrompt(interruptedPrompt, guidance string) string {
	return interruptedPrompt + "\n\nUser correction/guidance after interrupt: " + guidance
}

// Whether the session is idle and the cancelled prompt it held, consumed only when idle
func takeInterruptedPrompt(st *SessionState) (bool, string) {
	st.RuntimeLock.Lock()
	defer st.RuntimeLock.Unlock()
	if st.IsRunning {
		return false, ""
	}
	text := st.InterruptedPromptText
	st.InterruptedPromptText = ""
	return true, text
}

// Refusal of the requested model by the shared switch resolution
type ModelRejected struct{ Message string }

func (e *ModelRejected) Error() string { return e.Message }

// Streaming callbacks of one turn, all nil when no client is connected
type turnCallbacks struct {
	toolProgressCb        func(event, name, preview string, args map[string]any, extra map[string]any)
	reasoningCb           func(text any)
	stepCb                func(apiCallCount int, prevTools []map[string]any)
	streamDeltaCb         func(text any)
	approvalCb            ApprovalCallback
	editApprovalRequester tools.EditApprovalRequester
	mu                    sync.Mutex
	streamed              bool
	toolCalls             *ToolCalls
}

func (c *turnCallbacks) markStreamed(text any) {
	if truthy(text) {
		c.mu.Lock()
		c.streamed = true
		c.mu.Unlock()
	}
}

func (c *turnCallbacks) wasStreamed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streamed
}

// Arguments of the executor body of one turn
type turnArgs struct {
	state                 *SessionState
	sessionID             string
	userText              string
	userContent           any
	conn                  Client
	loop                  Loop
	approvalCb            ApprovalCallback
	editApprovalRequester tools.EditApprovalRequester
}

// ACP agent wrapping an in process AIAgent per session
type HermesACPAgent struct {
	SessionManager  Sessions
	connMu          sync.Mutex
	conn            Client
	loop            Loop
	background      sync.WaitGroup
	commandHandlers map[string]slashHandler
	turnRunner      func(ctx context.Context, t turnArgs) (map[string]any, error)
	prompter        func(ctx context.Context, req *pyacp.PromptRequest) (*pyacp.PromptResponse, error)
}

// In process agent over the given session manager
func NewHermesACPAgent(sessionManager Sessions) *HermesACPAgent {
	a := &HermesACPAgent{SessionManager: sessionManager}
	a.commandHandlers = map[string]slashHandler{
		"help": a.cmdHelp, "model": a.cmdModel, "tools": a.cmdTools, "context": a.cmdContext, "reset": a.cmdReset,
		"compress": a.cmdCompress, "steer": a.cmdSteer, "queue": a.cmdQueue, "version": a.cmdVersion,
	}
	a.turnRunner = a.runAgentTurn
	a.prompter = a.Prompt
	return a
}

// Stores the client connection that receives session updates
func (a *HermesACPAgent) OnConnect(conn *pyacp.AgentSideConnection) {
	a.connMu.Lock()
	a.conn = conn
	a.loop = NewConnLoop(context.Background())
	a.connMu.Unlock()
	serverLog.Info("ACP client connected")
}

// Connected client, nil before one connects
func (a *HermesACPAgent) client() Client {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return a.conn
}

// Loop worker threads schedule client calls on
func (a *HermesACPAgent) runningLoop() Loop {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if a.loop == nil {
		a.loop = NewConnLoop(context.Background())
	}
	return a.loop
}

// Sends a session update, logging instead of failing, false when it was not delivered
func (a *HermesACPAgent) send(ctx context.Context, sessionID string, update pyacp.SessionUpdate, failMsg string, level int) bool {
	if err := a.client().SessionUpdate(ctx, sessionID, update, nil); err != nil {
		serverLog.Log(level, fmt.Sprintf(failMsg, sessionID)+"\n"+err.Error())
		return false
	}
	return true
}

// Runs a notification coroutine right after the current response is queued
func (a *HermesACPAgent) scheduleSoon(ctx context.Context, coro func(ctx context.Context)) {
	if a.client() == nil {
		return
	}
	a.background.Add(1)
	detached := context.WithoutCancel(ctx)
	pyacp.AfterResponse(ctx, func() {
		defer a.background.Done()
		defer func() {
			if r := recover(); r != nil {
				serverLog.Error(fmt.Sprintf("Task exception was never retrieved\n%v", r))
			}
		}()
		coro(detached)
	})
}

// Edit approval policy of the session as ACP modes
func (a *HermesACPAgent) sessionModes(st *SessionState) *pyacp.SessionModeState {
	current := st.Mode
	if _, ok := Modes[current]; !ok {
		current = DefaultMode
	}
	modes := make([]pyacp.SessionMode, 0, len(ModeOrder))
	for _, id := range ModeOrder {
		spec := Modes[id]
		modes = append(modes, pyacp.SessionMode{ID: id, Name: spec.Name, Description: pyacp.Ptr(spec.Description)})
	}
	return &pyacp.SessionModeState{CurrentModeID: current, AvailableModes: modes}
}

// Edit approval policy and cwd the session's mode stands for
func (a *HermesACPAgent) editApprovalPolicyForState(st *SessionState) (string, string, error) {
	mode := st.Mode
	if mode == "" {
		mode = DefaultMode
	}
	policy, ok := ModeToEditApprovalPolicy[mode]
	if !ok {
		policy = DefaultEditApprovalPolicy
	}
	return policy, st.Cwd, nil
}

// Model picker of the session from the shared inventory, a single current model row when nothing is listable
func (a *HermesACPAgent) buildModelState(ctx context.Context, st *SessionState) *pyacp.SessionModelState {
	model := st.Model
	if model == "" {
		model = agentModel(st.Agent)
	}
	model = pystr.Strip(model)
	providerName := agentProvider(st.Agent)
	if providerName == "" {
		providerName = DetectProvider(ctx)
	}
	if providerName == "" {
		providerName = "openrouter"
	}
	picker, err := func() (picker *pyacp.SessionModelState, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return BuildModelState(ctx, model, providerName, agentBaseURL(st.Agent))
	}()
	if err != nil {
		serverLog.Debug("Could not build ACP model state\n" + err.Error())
	} else if picker != nil {
		return picker
	}
	if model == "" {
		return nil
	}
	choice := EncodeModelChoice(providerName, model)
	return &pyacp.SessionModelState{AvailableModels: []pyacp.ModelInfo{{ModelID: choice, Name: model}}, CurrentModelID: choice}
}

// Rebuilds the session agent on a new model and returns the old provider, the new provider and the model
func (a *HermesACPAgent) switchModel(ctx context.Context, st *SessionState, rawModel string, keepEndpoint bool) (string, string, string, error) {
	currentProvider, currentModel := agentProvider(st.Agent), st.Model
	explicitProvider, modelInput := provider.ParseModelInput(ctx, rawModel, "", nil)
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err != nil {
		return "", "", "", err
	}
	seed := currentProvider
	if seed == "" {
		seed = "openrouter"
	}
	userProviders, _ := cfg["providers"].(map[string]any)
	if userProviders == nil {
		userProviders = map[string]any{}
	}
	compatible := provider.CompatibleCustomProviders(cfg)
	custom := make([]any, len(compatible))
	for i, entry := range compatible {
		custom[i] = entry
	}
	result, err := switchModelFn(ctx, provider.SwitchModelOptions{
		RawInput: modelInput, ExplicitProvider: explicitProvider, CurrentProvider: seed,
		CurrentBaseURL: agentBaseURL(st.Agent), CurrentAPIKey: agentAPIKey(st.Agent),
		UserProviders: userProviders, CustomProviders: custom,
	})
	if err != nil {
		return "", "", "", err
	}
	if !result.Success {
		message := result.ErrorMessage
		if message == "" {
			message = "Cannot switch to " + rawModel
		}
		return "", "", "", &ModelRejected{Message: message}
	}
	targetProvider, newModel := result.TargetProvider, result.NewModel
	opts := MakeAgentOptions{SessionID: st.SessionID, Cwd: st.Cwd, Model: newModel, RequestedProvider: targetProvider,
		EnabledToolsets: agentEnabledToolsets(st.Agent), DisabledToolsets: agentDisabledToolsets(st.Agent)}
	if keepEndpoint && !(currentProvider != "" && targetProvider != currentProvider) {
		opts.BaseURL, opts.APIMode = agentBaseURL(st.Agent), agentAPIMode(st.Agent)
	}
	rebuilt, err := a.SessionManager.MakeAgent(ctx, opts)
	if err != nil {
		return "", "", "", err
	}
	st.Agent, st.Model = rebuilt, newModel
	a.SessionManager.SaveSession(ctx, st.SessionID)
	var from any
	if currentProvider != "" {
		from = currentProvider
	}
	observability.RecordModelSwitch(ctx, from, targetProvider, "acp", currentModel, st.SessionID)
	return currentProvider, targetProvider, newModel, nil
}

// Usage update of the context indicator, nil when the agent reports no context window
func (a *HermesACPAgent) buildUsageUpdate(ctx context.Context, st *SessionState) *pyacp.UsageUpdate {
	size, _, lastPromptTokens := agentCompressorFigures(st.Agent)
	if size <= 0 {
		return nil
	}
	used := 0
	func() {
		defer func() {
			if r := recover(); r != nil {
				serverLog.Debug(fmt.Sprintf("Could not estimate ACP native context usage\n%v", r))
				used = lastPromptTokens
			}
		}()
		used = estimateTokens(ctx, st.History, st.Agent)
	}()
	return &pyacp.UsageUpdate{Size: max(size, 0), Used: max(used, 0)}
}

func (a *HermesACPAgent) sendUsageUpdate(ctx context.Context, st *SessionState) {
	if a.client() == nil {
		return
	}
	if update := a.buildUsageUpdate(ctx, st); update != nil {
		a.send(ctx, st.SessionID, update, "Failed to send ACP usage update for session %s", logging.LevelWarning)
	}
}

// Session provenance meta of an ACP session, nil when it cannot be built
func (a *HermesACPAgent) provenanceMeta(ctx context.Context, acpSessionID, currentID, previousID string) (meta *pyjson.Object) {
	defer func() {
		if r := recover(); r != nil {
			serverLog.Debug(fmt.Sprintf("Could not build ACP session provenance for %s\n%v", acpSessionID, r))
			meta = nil
		}
	}()
	db := a.SessionManager.GetDB(ctx)
	if db == nil {
		return SessionProvenanceMeta(ctx, nil, acpSessionID, currentID, previousID)
	}
	return SessionProvenanceMeta(ctx, db, acpSessionID, currentID, previousID)
}

// Sends the session title with provenance, the previous head named when compression rotated it
func (a *HermesACPAgent) sendSessionInfoUpdate(ctx context.Context, sessionID, currentID, previousID string) {
	if a.client() == nil {
		return
	}
	db := a.SessionManager.GetDB(ctx)
	if db == nil {
		serverLog.Debug(fmt.Sprintf("Could not read ACP session info for %s", sessionID))
		return
	}
	row, err := db.GetSession(ctx, sessionID)
	if err != nil {
		serverLog.Debug(fmt.Sprintf("Could not read ACP session info for %s\n%s", sessionID, err))
		return
	}
	if row == nil || row.Len() == 0 {
		return
	}
	update := &pyacp.SessionInfoUpdate{UpdatedAt: pyacp.Ptr(pydatetime.ISOFormatUTC(time.Now()))}
	if title, ok := row.GetOr("title", nil).(string); ok && pystr.Strip(title) != "" {
		update.Title = &title
	}
	if currentID == "" {
		currentID = sessionID
	}
	update.Meta = a.provenanceMeta(ctx, sessionID, currentID, previousID)
	a.send(ctx, sessionID, update, "Could not send ACP session info update for %s", logging.LevelDebug)
}

// Registers editor provided MCP servers and refreshes the agent tool surface
func (a *HermesACPAgent) registerSessionMCPServers(ctx context.Context, st *SessionState, servers []pyacp.McpServer) {
	if len(servers) == 0 {
		return
	}
	configs := map[string]map[string]any{}
	var names []string
	for _, server := range servers {
		name, cfg := mcpServerConfig(server)
		configs[name] = cfg
		names = append(names, name)
	}
	if err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		pinned := toolViewEnv(ctx, st.Agent, agentEnabledToolsets(st.Agent), agentDisabledToolsets(st.Agent))
		tools.SetLogicalSessionCwd(pinned, st.Cwd)
		_, err = registerMCPServers(ctx, pinned, configs)
		return err
	}(); err != nil {
		serverLog.Warning(fmt.Sprintf("Session %s: failed to register ACP MCP servers\n%s", st.SessionID, err))
		return
	}
	if err := a.refreshToolSurface(ctx, st, names); err != nil {
		serverLog.Warning(fmt.Sprintf("Session %s: failed to refresh tool surface after ACP MCP registration\n%s", st.SessionID, err))
	}
}

// Rebuilds the agent tool snapshot with the editor servers added to its toolsets
func (a *HermesACPAgent) refreshToolSurface(ctx context.Context, st *SessionState, serverNames []string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	ag := st.Agent
	surface, ok := ag.(interface {
		SetToolsets(enabled, disabled []string)
		SetToolSnapshot(defs []map[string]any, names map[string]bool)
	})
	if !ok {
		return errors.New("agent has no tool surface")
	}
	current := agentEnabledToolsets(ag)
	enabled := expandACPEnabledToolsets(current, current == nil, serverNames)
	disabled := agentDisabledToolsets(ag)
	surface.SetToolsets(enabled, disabled)
	if backend, ok := ag.(interface{ SetDelegationBackend(backend string) }); ok {
		backend.SetDelegationBackend(tools.SavedDelegationBackend(ctx))
	}
	view := toolViewEnv(ctx, ag, enabled, disabled)
	defs := getToolDefinitions(ctx, view, enabled, disabled)
	names := map[string]bool{}
	for _, def := range defs {
		fn, _ := def["function"].(map[string]any)
		names[pyStr(fn["name"])] = true
	}
	defs, names, _ = memory.InjectMemoryProviderTools(ctx, view, agentMemoryManager(ag), defs, names)
	surface.SetToolSnapshot(defs, names)
	if target, ok := ag.(systemPromptInvalidator); ok {
		if err := target.InvalidateSystemPrompt(ctx); err != nil {
			return err
		}
	}
	serverLog.Info(fmt.Sprintf("Session %s: refreshed tool surface after ACP MCP registration (%d tools)", st.SessionID, len(defs)))
	return nil
}

// Refreshes the tool snapshot once background MCP discovery lands, before the first turn only
func (a *HermesACPAgent) scheduleMCPLateRefresh(ctx context.Context, st *SessionState) {
	inFlight := func() (inFlight bool) {
		defer func() {
			if recover() != nil {
				inFlight = false
			}
		}()
		return mcpDiscoveryInFlight(ctx)
	}()
	if !inFlight {
		return
	}
	ag, sessionID := st.Agent, st.SessionID
	detached := context.WithoutCancel(ctx)
	a.background.Add(1)
	go func() {
		defer a.background.Done()
		defer func() {
			if r := recover(); r != nil {
				serverLog.Debug(fmt.Sprintf("Session %s: late MCP refresh failed\n%v", sessionID, r))
			}
		}()
		if !joinMCPDiscovery(detached, 30*time.Second) {
			return
		}
		current := a.SessionManager.memorySession(sessionID)
		if current == nil || current.Agent != ag {
			return
		}
		added := func() map[string]bool {
			current.RuntimeLock.Lock()
			defer current.RuntimeLock.Unlock()
			if current.IsRunning {
				return nil
			}
			if counts, ok := ag.(interface {
				UserTurnCount() int
				APICallCount() int
			}); ok && (counts.UserTurnCount() > 0 || counts.APICallCount() > 0) {
				return nil
			}
			return refreshAgentMCPTools(detached, ag)
		}()
		if len(added) > 0 {
			names := make([]string, 0, len(added))
			for name := range added {
				names = append(names, name)
			}
			sort.Strings(names)
			serverLog.Info(fmt.Sprintf("Session %s: late MCP refresh added %d tools: %s", sessionID, len(added), strings.Join(names, ", ")))
		}
	}()
}

// Answers initialize with the protocol version, agent info, capabilities and auth methods
func (a *HermesACPAgent) Initialize(ctx context.Context, req *pyacp.InitializeRequest) (*pyacp.InitializeResponse, error) {
	authMethods := BuildAuthMethods(ctx)
	clientName := "unknown"
	if req != nil && req.ClientInfo != nil {
		clientName = req.ClientInfo.Name
	}
	version := pyacp.ProtocolVersion
	if req != nil {
		version = req.ProtocolVersion
	}
	serverLog.Info(fmt.Sprintf("Initialize from %s (protocol v%d)", clientName, version))
	info, err := getVersionInfo()
	if err != nil {
		return nil, err
	}
	return &pyacp.InitializeResponse{
		ProtocolVersion: pyacp.ProtocolVersion,
		AgentInfo:       &pyacp.Implementation{Name: "icebath-agent", Version: info.BaseVersion},
		AgentCapabilities: &pyacp.AgentCapabilities{
			LoadSession:        pyacp.Ptr(true),
			PromptCapabilities: &pyacp.PromptCapabilities{Image: pyacp.Ptr(true)},
			SessionCapabilities: &pyacp.SessionCapabilities{
				Fork: &pyacp.SessionForkCapabilities{}, List: &pyacp.SessionListCapabilities{}, Resume: &pyacp.SessionResumeCapabilities{},
			},
		},
		AuthMethods: authMethods,
	}, nil
}

// Acknowledges only an auth method initialize advertised, nil otherwise
func authenticate(ctx context.Context, methodID string) *pyacp.AuthenticateResponse {
	normalized := strings.ToLower(pystr.Strip(methodID))
	providerName := DetectProvider(ctx)
	if normalized == TerminalSetupAuthMethodID {
		if providerName != "" {
			return &pyacp.AuthenticateResponse{}
		}
		return nil
	}
	if providerName == "" || normalized != providerName {
		return nil
	}
	return &pyacp.AuthenticateResponse{}
}

// Answers authenticate for the detected provider or the terminal setup method
func (a *HermesACPAgent) Authenticate(ctx context.Context, req *pyacp.AuthenticateRequest) (*pyacp.AuthenticateResponse, error) {
	return authenticate(ctx, req.MethodID), nil
}

// Replays the history as chunks and tool call pairs so the editor shows the transcript
func (a *HermesACPAgent) replaySessionHistory(ctx context.Context, st *SessionState) {
	if a.client() == nil || len(st.History) == 0 {
		return
	}
	for _, update := range historyReplayUpdates(st.History) {
		if !a.send(ctx, st.SessionID, update, "Failed to replay ACP history for session %s", logging.LevelWarning) {
			return
		}
	}
}

// Models, modes and provenance meta of a session response, after the history replay load and resume owe
func (a *HermesACPAgent) sessionResponseFields(ctx context.Context, st *SessionState, replayVerb string) (*pyacp.SessionModelState, *pyacp.SessionModeState, *pyjson.Object) {
	if replayVerb != "" {
		func() {
			defer func() {
				if r := recover(); r != nil {
					serverLog.Warning(fmt.Sprintf("ACP history replay raised during session/%s for %s — %s will still succeed, partial transcript may be missing\n%v",
						replayVerb, st.SessionID, replayVerb, r))
				}
			}()
			a.replaySessionHistory(ctx, st)
		}()
	}
	a.scheduleAvailableCommandsUpdate(ctx, st.SessionID)
	a.scheduleSoon(ctx, func(ctx context.Context) { a.sendUsageUpdate(ctx, st) })
	return a.buildModelState(ctx, st), a.sessionModes(st), a.provenanceMeta(ctx, st.SessionID, agentSessionID(st.Agent, st.SessionID), "")
}

func (a *HermesACPAgent) attachSessionMCP(ctx context.Context, st *SessionState, servers []pyacp.McpServer, logLine string) {
	a.registerSessionMCPServers(ctx, st, servers)
	a.scheduleMCPLateRefresh(ctx, st)
	serverLog.Info(logLine)
}

// Creates a session with a fresh agent
func (a *HermesACPAgent) NewSession(ctx context.Context, req *pyacp.NewSessionRequest) (*pyacp.NewSessionResponse, error) {
	st, err := a.SessionManager.CreateSession(ctx, req.Cwd)
	if err != nil {
		return nil, err
	}
	a.attachSessionMCP(ctx, st, req.MCPServers, fmt.Sprintf("New session %s (cwd=%s)", st.SessionID, req.Cwd))
	models, modes, meta := a.sessionResponseFields(ctx, st, "")
	return &pyacp.NewSessionResponse{SessionID: st.SessionID, Models: models, Modes: modes, Meta: meta}, nil
}

// Loads a session into the given cwd and replays its history, nil when it is unknown
func (a *HermesACPAgent) LoadSession(ctx context.Context, req *pyacp.LoadSessionRequest) (*pyacp.LoadSessionResponse, error) {
	st := a.SessionManager.UpdateCwd(ctx, req.SessionID, req.Cwd)
	if st == nil {
		serverLog.Warning(fmt.Sprintf("load_session: session %s not found", req.SessionID))
		return nil, nil
	}
	a.attachSessionMCP(ctx, st, req.MCPServers, "Loaded session "+req.SessionID)
	models, modes, meta := a.sessionResponseFields(ctx, st, "load")
	return &pyacp.LoadSessionResponse{Models: models, Modes: modes, Meta: meta}, nil
}

// Resumes a session, creating a new one when it is unknown
func (a *HermesACPAgent) ResumeSession(ctx context.Context, req *pyacp.ResumeSessionRequest) (*pyacp.ResumeSessionResponse, error) {
	st := a.SessionManager.UpdateCwd(ctx, req.SessionID, req.Cwd)
	if st == nil {
		serverLog.Warning(fmt.Sprintf("resume_session: session %s not found, creating new", req.SessionID))
		created, err := a.SessionManager.CreateSession(ctx, req.Cwd)
		if err != nil {
			return nil, err
		}
		st = created
	}
	a.attachSessionMCP(ctx, st, req.MCPServers, "Resumed session "+st.SessionID)
	models, modes, meta := a.sessionResponseFields(ctx, st, "resume")
	return &pyacp.ResumeSessionResponse{Models: models, Modes: modes, Meta: meta}, nil
}

// Cancels the running turn of a session, keeping its prompt for a correction that follows
func (a *HermesACPAgent) Cancel(ctx context.Context, req *pyacp.CancelNotification) error {
	st := a.SessionManager.GetSession(ctx, req.SessionID)
	if st == nil || st.CancelEvent == nil {
		return nil
	}
	func() {
		st.RuntimeLock.Lock()
		defer st.RuntimeLock.Unlock()
		if st.IsRunning && st.CurrentPromptText != "" {
			st.InterruptedPromptText = st.CurrentPromptText
		}
		st.CancelEvent.Set()
		defer func() {
			if r := recover(); r != nil {
				serverLog.Debug(fmt.Sprintf("Failed to interrupt ACP session %s\n%v", req.SessionID, r))
			}
		}()
		if st.Agent != nil {
			agent.RequestHardInterruptOf(st.Agent, nil, nil)
		}
	}()
	serverLog.Info("Cancelled session " + req.SessionID)
	return nil
}

// Forks a session into a new one carrying its history, an empty id when the original is unknown
func (a *HermesACPAgent) ForkSession(ctx context.Context, req *pyacp.ForkSessionRequest) (*pyacp.ForkSessionResponse, error) {
	st, err := a.SessionManager.ForkSession(ctx, req.SessionID, req.Cwd)
	if err != nil {
		return nil, err
	}
	if st == nil {
		serverLog.Info(fmt.Sprintf("Forked session %s -> %s", req.SessionID, ""))
		return &pyacp.ForkSessionResponse{SessionID: ""}, nil
	}
	a.registerSessionMCPServers(ctx, st, req.MCPServers)
	serverLog.Info(fmt.Sprintf("Forked session %s -> %s", req.SessionID, st.SessionID))
	a.scheduleAvailableCommandsUpdate(ctx, st.SessionID)
	return &pyacp.ForkSessionResponse{SessionID: st.SessionID, Models: a.buildModelState(ctx, st), Modes: a.sessionModes(st)}, nil
}

// Lists sessions after the cursor, one fixed size page at a time
func (a *HermesACPAgent) ListSessions(ctx context.Context, req *pyacp.ListSessionsRequest) (*pyacp.ListSessionsResponse, error) {
	cwd, cursor := "", ""
	if req.Cwd != nil {
		cwd = *req.Cwd
	}
	if req.Cursor != nil {
		cursor = *req.Cursor
	}
	infos, err := a.SessionManager.ListSessions(ctx, cwd)
	if err != nil {
		return nil, err
	}
	if cursor != "" {
		rest := []*pyjson.Object{}
		for i, s := range infos {
			if s.GetOr("session_id", nil) == cursor {
				rest = infos[i+1:]
				break
			}
		}
		infos = rest
	}
	hasMore := len(infos) > listSessionsPageSize
	if hasMore {
		infos = infos[:listSessionsPageSize]
	}
	sessions := make([]pyacp.SessionInfo, 0, len(infos))
	for _, s := range infos {
		info := pyacp.SessionInfo{SessionID: pyStr(s.GetOr("session_id", nil)), Cwd: pyStr(s.GetOr("cwd", nil))}
		if title := s.GetOr("title", nil); title != nil {
			info.Title = pyacp.Ptr(pyStr(title))
		}
		if updated := s.GetOr("updated_at", nil); updated != nil {
			info.UpdatedAt = pyacp.Ptr(pyStr(updated))
		}
		sessions = append(sessions, info)
	}
	resp := &pyacp.ListSessionsResponse{Sessions: sessions}
	if hasMore && len(sessions) > 0 {
		resp.NextCursor = pyacp.Ptr(sessions[len(sessions)-1].SessionID)
	}
	return resp, nil
}

// Prompt text and content after an interrupt, the cancelled request reattached to a steer or a plain follow up
func (a *HermesACPAgent) rewritePromptForInterrupt(st *SessionState, userText string, userContent any, textOnly bool) (string, any) {
	if _, isText := userContent.(string); !textOnly || !isText {
		return userText, userContent
	}
	if strings.HasPrefix(userText, "/steer") {
		_, steerText := splitCommand(userText)
		if steerText == "" {
			return userText, userContent
		}
		idle, interrupted := takeInterruptedPrompt(st)
		if interrupted != "" {
			attached := attachInterruptedPrompt(interrupted, steerText)
			return attached, attached
		}
		if idle {
			return steerText, steerText
		}
		return userText, userContent
	}
	if !strings.HasPrefix(userText, "/") {
		if _, interrupted := takeInterruptedPrompt(st); interrupted != "" {
			attached := attachInterruptedPrompt(interrupted, userText)
			return attached, attached
		}
	}
	return userText, userContent
}

// Marks the session running, or redirects or queues the prompt behind a live turn and returns the client message
func (a *HermesACPAgent) claimTurnOrQueue(st *SessionState, sessionID, userText string, userContent any, textOnly bool) (string, bool) {
	st.RuntimeLock.Lock()
	defer st.RuntimeLock.Unlock()
	queued := userText
	if queued == "" {
		queued = "[Image attachment]"
	}
	if !st.IsRunning && !st.CommandOp {
		st.IsRunning = true
		st.CurrentPromptText = queued
		return "", false
	}
	if text, isText := userContent.(string); st.IsRunning && textOnly && isText {
		redirector, ok := st.Agent.(interface {
			Redirect(text string) bool
			SupportsActiveTurnRedirect() bool
		})
		if ok && redirector.SupportsActiveTurnRedirect() {
			redirected := func() (ok bool) {
				defer func() {
					if r := recover(); r != nil {
						serverLog.Debug(fmt.Sprintf("ACP active-turn redirect failed for %s\n%v", sessionID, r))
						ok = false
					}
				}()
				return redirector.Redirect(text)
			}()
			if redirected {
				return "Redirected the active turn with your correction.", true
			}
		}
	}
	st.QueuedPrompts = append(st.QueuedPrompts, queued)
	return fmt.Sprintf("Queued for the next turn. (%d queued)", len(st.QueuedPrompts)), true
}

// Executor body of one turn with the session context, approval routing and title notice bound around the agent run
func (a *HermesACPAgent) runAgentTurn(ctx context.Context, t turnArgs) (map[string]any, error) {
	st, ag := t.state, t.state.Agent
	stack := &exitStack{}
	defer stack.close()
	env := agentEnv(ag)
	bindGuarded(stack, "session context", func() (func(), error) {
		if env == nil {
			return nil, errors.New("agent carries no tool environment")
		}
		cron := ""
		tools.SetSessionVars(env, tools.SessionVars{SessionKey: t.sessionID, SessionID: t.sessionID, Cwd: st.Cwd, CronSession: &cron})
		return func() { tools.ClearSessionVars(env) }, nil
	})
	if t.approvalCb != nil {
		bindGuarded(stack, "approval callback", func() (func(), error) {
			if env == nil {
				return nil, errors.New("agent carries no tool environment")
			}
			previous := env.Ask
			env.Ask = ApprovalAsker{Callback: t.approvalCb}
			return func() { env.Ask = previous }, nil
		})
	}
	if t.editApprovalRequester != nil {
		ctx = SetEditApprovalRequester(ctx, t.editApprovalRequester)
	}
	if env != nil {
		tools.SetInteractiveContext(env, true)
		stack.callback(func() { tools.ResetInteractiveContext(env) })
	}
	previousID, hadID := os.LookupEnv("ICEBATH_SESSION_ID")
	stack.callback(func() { restoreEnv("ICEBATH_SESSION_ID", previousID, hadID) })
	os.Setenv("ICEBATH_SESSION_ID", t.sessionID)
	if titled, ok := ag.(interface {
		SetOnSessionTitle(cb func(title, source string))
	}); ok {
		detached := context.WithoutCancel(ctx)
		titled.SetOnSessionTitle(func(title, source string) {
			if t.conn != nil {
				safeScheduleThreadsafe(func(context.Context) (any, error) {
					a.sendSessionInfoUpdate(detached, t.sessionID, "", "")
					return nil, nil
				}, t.loop, serverLog, "")
			}
		})
	}
	persisted := t.userText
	if persisted == "" {
		persisted = "[Image attachment]"
	}
	result, err := func() (result map[string]any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		runner, ok := ag.(interface {
			RunConversationContent(ctx context.Context, userMessage any, o agent.RunConversationOptions) (map[string]any, error)
		})
		if !ok {
			return nil, fmt.Errorf("'%T' object has no attribute 'run_conversation'", ag)
		}
		return runner.RunConversationContent(ctx, t.userContent, agent.RunConversationOptions{
			ConversationHistory: st.History, TaskID: t.sessionID, PersistUserMessage: persisted})
	}()
	if err != nil {
		serverLog.Error(fmt.Sprintf("Agent error in session %s\n%s", t.sessionID, err))
		return map[string]any{"final_response": "Error: " + err.Error(), "messages": st.History}, nil
	}
	return result, nil
}

// Runs the agent on the user's prompt and streams its events back to the editor
func (a *HermesACPAgent) Prompt(ctx context.Context, req *pyacp.PromptRequest) (*pyacp.PromptResponse, error) {
	sessionID := req.SessionID
	st := a.SessionManager.GetSession(ctx, sessionID)
	if st == nil {
		serverLog.Error(fmt.Sprintf("prompt: session %s not found", sessionID))
		return &pyacp.PromptResponse{StopReason: "refusal"}, nil
	}
	userText := pystr.Strip(ExtractText(req.Prompt))
	userContent, err := ContentBlocksToOpenAIUserContent(req.Prompt)
	if err != nil {
		return nil, err
	}
	textOnly := true
	for _, block := range req.Prompt {
		if _, ok := block.(*pyacp.TextContentBlock); !ok {
			textOnly = false
		}
	}
	if parts, isList := userContent.([]map[string]any); userText == "" && !(isList && len(parts) > 0) {
		return &pyacp.PromptResponse{StopReason: "end_turn"}, nil
	}
	userText, userContent = a.rewritePromptForInterrupt(st, userText, userContent, textOnly)
	if _, isText := userContent.(string); textOnly && isText && strings.HasPrefix(userText, "/") {
		if reply, known := a.handleSlashCommand(ctx, userText, st); known {
			if conn := a.client(); conn != nil {
				if err := conn.SessionUpdate(ctx, sessionID, pyacp.UpdateAgentMessageText(reply), nil); err != nil {
					return nil, err
				}
				a.sendUsageUpdate(ctx, st)
			}
			if err := a.drainQueuedPrompts(ctx, st, sessionID, a.client()); err != nil {
				return nil, err
			}
			return &pyacp.PromptResponse{StopReason: "end_turn"}, nil
		}
	}
	if absorbed, ok := a.claimTurnOrQueue(st, sessionID, userText, userContent, textOnly); ok {
		if conn := a.client(); conn != nil {
			if err := conn.SessionUpdate(ctx, sessionID, pyacp.UpdateAgentMessageText(absorbed), nil); err != nil {
				return nil, err
			}
		}
		return &pyacp.PromptResponse{StopReason: "end_turn"}, nil
	}
	preview := userText
	if r := []rune(userText); len(r) > 100 {
		preview = string(r[:100])
	}
	serverLog.Info(fmt.Sprintf("Prompt on session %s: %s", sessionID, preview))
	conn, loop := a.client(), a.runningLoop()
	if st.CancelEvent != nil {
		st.CancelEvent.Clear()
	}
	cbs := a.wireTurnCallbacks(st, sessionID, conn, loop)
	preTurnID := agentSessionID(st.Agent, "")
	turnCtx := context.WithoutCancel(ctx)
	type outcome struct {
		result map[string]any
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		turnSlots <- struct{}{}
		defer func() { <-turnSlots }()
		var out outcome
		defer func() { done <- out }()
		defer a.flushTurnToolCalls(cbs, sessionID, conn, loop)
		defer func() {
			if r := recover(); r != nil {
				out = outcome{nil, fmt.Errorf("%v", r)}
			}
		}()
		result, err := a.turnRunner(turnCtx, turnArgs{state: st, sessionID: sessionID, userText: userText, userContent: userContent,
			conn: conn, loop: loop, approvalCb: cbs.approvalCb, editApprovalRequester: cbs.editApprovalRequester})
		out = outcome{result, err}
	}()
	out := <-done
	if out.err != nil {
		serverLog.Error(fmt.Sprintf("Executor error for session %s\n%s", sessionID, out.err))
		st.RuntimeLock.Lock()
		st.IsRunning = false
		st.CurrentPromptText = ""
		st.RuntimeLock.Unlock()
		return &pyacp.PromptResponse{StopReason: "end_turn"}, nil
	}
	return a.finishTurn(ctx, st, sessionID, conn, out.result, preTurnID, cbs.wasStreamed())
}

// Closes any tool call the turn left open so no bubble spins after the turn ends
func (a *HermesACPAgent) flushTurnToolCalls(cbs *turnCallbacks, sessionID string, conn Client, loop Loop) {
	if conn == nil || cbs.toolCalls == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			serverLog.Debug(fmt.Sprintf("Could not flush open ACP tool calls for %s\n%v", sessionID, r))
		}
	}()
	FlushOpenToolCalls(conn, sessionID, loop, cbs.toolCalls)
}

// Permission request of a connection in the form the approval bridges call
func requestPermissionVia(conn Client) RequestPermissionFunc {
	return func(ctx context.Context, sessionID string, toolCall *pyacp.ToolCallProgress, options []pyacp.PermissionOption) (*pyacp.RequestPermissionResponse, error) {
		update := &pyacp.ToolCallUpdate{Meta: toolCall.Meta, Content: toolCall.Content, Kind: toolCall.Kind, Locations: toolCall.Locations,
			RawInput: toolCall.RawInput, RawOutput: toolCall.RawOutput, Status: toolCall.Status, Title: toolCall.Title, ToolCallID: toolCall.ToolCallID}
		return conn.RequestPermission(ctx, options, sessionID, update, nil)
	}
}

// Installs the streaming callbacks of one turn on the session agent
func (a *HermesACPAgent) wireTurnCallbacks(st *SessionState, sessionID string, conn Client, loop Loop) *turnCallbacks {
	cbs := &turnCallbacks{}
	if conn != nil {
		calls := NewToolCalls()
		cbs.toolCalls = calls
		turn := &TurnState{}
		policy := func() (string, string, error) { return a.editApprovalPolicyForState(st) }
		cbs.toolProgressCb = MakeToolProgressCb(conn, sessionID, loop, calls, policy, turn)
		if st.MessageIDs == nil {
			st.MessageIDs = &AssistantMessageIDAllocator{}
		}
		st.MessageIDs.Close()
		cbs.reasoningCb = MakeThinkingCb(conn, sessionID, loop, st.MessageIDs)
		cbs.stepCb = MakeStepCb(conn, sessionID, loop, calls, turn)
		messageCb := MakeMessageCb(conn, sessionID, loop, st.MessageIDs)
		cbs.streamDeltaCb = func(text any) {
			cbs.markStreamed(text)
			messageCb(text)
		}
		send := func(update pyacp.SessionUpdate) { sendUpdate(conn, sessionID, loop, update) }
		request := requestPermissionVia(conn)
		cbs.approvalCb = MakeApprovalCallback(request, loop, sessionID, nil, send)
		func() {
			defer func() {
				if r := recover(); r != nil {
					serverLog.Debug(fmt.Sprintf("Could not create ACP edit approval requester\n%v", r))
				}
			}()
			cbs.editApprovalRequester = MakeACPEditApprovalRequester(request, loop, sessionID, nil, policy, send)
		}()
	}
	ag := st.Agent
	if target, ok := ag.(interface{ SetToolProgressCallback(cb any) }); ok {
		if cbs.toolProgressCb != nil {
			target.SetToolProgressCallback(cbs.toolProgressCb)
		} else {
			target.SetToolProgressCallback(nil)
		}
	}
	if target, ok := ag.(interface{ SetThinkingCallback(cb any) }); ok {
		target.SetThinkingCallback(nil)
	}
	if target, ok := ag.(interface{ SetReasoningCallback(cb any) }); ok {
		if cbs.reasoningCb != nil {
			target.SetReasoningCallback(cbs.reasoningCb)
		} else {
			target.SetReasoningCallback(nil)
		}
	}
	if target, ok := ag.(interface{ SetStepCallback(cb any) }); ok {
		if cbs.stepCb != nil {
			target.SetStepCallback(cbs.stepCb)
		} else {
			target.SetStepCallback(nil)
		}
	}
	if target, ok := ag.(interface{ SetStreamDeltaCallback(cb any) }); ok {
		if cbs.streamDeltaCb != nil {
			target.SetStreamDeltaCallback(cbs.streamDeltaCb)
		} else {
			target.SetStreamDeltaCallback(nil)
		}
	}
	return cbs
}

// Integer of a turn result counter, def when the key is absent or not a number
func resultInt(result map[string]any, key string, def int) int {
	if f, ok := pyFloat(result[key]); ok {
		if _, isText := result[key].(string); !isText {
			return int(f)
		}
	}
	return def
}

// Persists the turn, emits provenance and the final text, drains queued prompts and reports usage
func (a *HermesACPAgent) finishTurn(ctx context.Context, st *SessionState, sessionID string, conn Client, result map[string]any, preTurnID string, streamed bool) (resp *pyacp.PromptResponse, err error) {
	cancelled := false
	err = func() (err error) {
		defer func() {
			st.RuntimeLock.Lock()
			st.IsRunning = false
			st.CurrentPromptText = ""
			st.RuntimeLock.Unlock()
			if drainErr := a.drainQueuedPrompts(ctx, st, sessionID, conn); drainErr != nil && err == nil {
				err = drainErr
			}
		}()
		if raw, ok := result["messages"]; ok {
			if messages, isList := raw.([]map[string]any); isList {
				if messages == nil {
					messages = []map[string]any{}
				}
				st.History = messages
				a.SessionManager.SaveSession(ctx, sessionID)
			}
		}
		postTurnID := agentSessionID(st.Agent, "")
		if conn != nil && postTurnID != "" && preTurnID != "" && postTurnID != preTurnID {
			func() {
				defer func() {
					if r := recover(); r != nil {
						serverLog.Debug(fmt.Sprintf("Could not emit ACP provenance update after rotation for %s\n%v", sessionID, r))
					}
				}()
				a.sendSessionInfoUpdate(ctx, sessionID, postTurnID, preTurnID)
			}()
		}
		finalResponse := ""
		if truthy(result["final_response"]) {
			finalResponse = pyStr(result["final_response"])
		}
		cancelled = st.CancelEvent != nil && st.CancelEvent.IsSet()
		interrupted := truthy(result["interrupted"]) || cancelled
		suppress := interrupted && strings.HasPrefix(finalResponse, agent.InterruptWaitingForModelPrefix)
		transformed := truthy(result["response_transformed"])
		if finalResponse != "" && conn != nil && !suppress && (!streamed || transformed) {
			update := pyacp.UpdateAgentMessageText(finalResponse)
			if st.MessageIDs != nil {
				id := st.MessageIDs.Current()
				if streamed && transformed {
					if last := st.MessageIDs.Last(); last != "" {
						id = last
					}
				}
				update.MessageID = &id
				st.MessageIDs.Close()
			}
			if err := conn.SessionUpdate(ctx, sessionID, update, nil); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	var usage *pyacp.Usage
	if result["prompt_tokens"] != nil || result["completion_tokens"] != nil || result["total_tokens"] != nil {
		usage = &pyacp.Usage{InputTokens: resultInt(result, "prompt_tokens", 0), OutputTokens: resultInt(result, "completion_tokens", 0),
			TotalTokens: resultInt(result, "total_tokens", 0)}
		if result["reasoning_tokens"] != nil {
			usage.ThoughtTokens = pyacp.Ptr(resultInt(result, "reasoning_tokens", 0))
		}
		if result["cache_read_tokens"] != nil {
			usage.CachedReadTokens = pyacp.Ptr(resultInt(result, "cache_read_tokens", 0))
		}
	}
	a.sendUsageUpdate(ctx, st)
	stop := "end_turn"
	if cancelled {
		stop = "cancelled"
	}
	return &pyacp.PromptResponse{StopReason: stop, Usage: usage}, nil
}

// Runs queued prompts while the session is idle
func (a *HermesACPAgent) drainQueuedPrompts(ctx context.Context, st *SessionState, sessionID string, conn Client) error {
	for {
		st.RuntimeLock.Lock()
		if st.IsRunning || st.CommandOp || len(st.QueuedPrompts) == 0 {
			st.RuntimeLock.Unlock()
			return nil
		}
		next := st.QueuedPrompts[0]
		st.QueuedPrompts = st.QueuedPrompts[1:]
		st.RuntimeLock.Unlock()
		if conn != nil {
			if err := conn.SessionUpdate(ctx, sessionID, pyacp.UpdateUserMessageText(next), nil); err != nil {
				return err
			}
		}
		if _, err := a.prompter(ctx, &pyacp.PromptRequest{Prompt: []pyacp.ContentBlock{pyacp.TextBlock(next)}, SessionID: sessionID}); err != nil {
			return err
		}
	}
}

// Switches the model of an idle session through the shared switch resolution
func (a *HermesACPAgent) SetSessionModel(ctx context.Context, req *pyacp.SetSessionModelRequest) (*pyacp.SetSessionModelResponse, error) {
	sessionID := req.SessionID
	st := a.SessionManager.GetSession(ctx, sessionID)
	if st == nil {
		serverLog.Warning(fmt.Sprintf("Session %s: model switch requested for missing session", sessionID))
		return nil, nil
	}
	st.RuntimeLock.Lock()
	if st.IsRunning || st.CommandOp {
		st.RuntimeLock.Unlock()
		return nil, pyacp.NewRequestError(-32603, "Session is busy; switch models while the session is idle", nil)
	}
	st.CommandOp = true
	st.RuntimeLock.Unlock()
	_, requestedProvider, resolvedModel, err := func() (old, requested, resolved string, err error) {
		defer func() {
			st.RuntimeLock.Lock()
			st.CommandOp = false
			st.RuntimeLock.Unlock()
			a.scheduleSoon(ctx, func(ctx context.Context) {
				if err := a.drainQueuedPrompts(ctx, st, sessionID, a.client()); err != nil {
					serverLog.Error(fmt.Sprintf("Task exception was never retrieved\n%s", err))
				}
			})
		}()
		return a.switchModel(ctx, st, req.ModelID, true)
	}()
	if err != nil {
		var rejected *ModelRejected
		if errors.As(err, &rejected) {
			data := pyjson.NewObject()
			data.Set("details", rejected.Message)
			return nil, pyacp.InvalidParams(data)
		}
		return nil, err
	}
	serverLog.Info(fmt.Sprintf("Session %s: model switched to %s via provider %s", sessionID, resolvedModel, requestedProvider))
	return &pyacp.SetSessionModelResponse{}, nil
}

// Persists the editor requested mode, an unknown mode becoming the default
func (a *HermesACPAgent) SetSessionMode(ctx context.Context, req *pyacp.SetSessionModeRequest) (*pyacp.SetSessionModeResponse, error) {
	st := a.SessionManager.GetSession(ctx, req.SessionID)
	if st == nil {
		serverLog.Warning(fmt.Sprintf("Session %s: mode switch requested for missing session", req.SessionID))
		return nil, nil
	}
	mode := pystr.Strip(req.ModeID)
	if _, ok := Modes[mode]; !ok {
		mode = DefaultMode
	}
	st.Mode = mode
	a.SessionManager.SaveSession(ctx, req.SessionID)
	serverLog.Info(fmt.Sprintf("Session %s: mode switched to %s", req.SessionID, mode))
	return &pyacp.SetSessionModeResponse{}, nil
}

// Accepts a config option update, the edit approval policy becoming the session mode
func (a *HermesACPAgent) SetConfigOption(ctx context.Context, req *pyacp.SetConfigOptionParams) (*pyacp.SetSessionConfigOptionResponse, error) {
	st := a.SessionManager.GetSession(ctx, req.SessionID)
	if st == nil {
		serverLog.Warning(fmt.Sprintf("Session %s: config update requested for missing session", req.SessionID))
		return nil, nil
	}
	if req.ConfigID == EditApprovalPolicyConfigID {
		mode, ok := EditApprovalPolicyToMode[pyStr(req.Value)]
		if !ok {
			mode = DefaultMode
		}
		st.Mode = mode
	} else {
		if st.ConfigOptions == nil {
			st.ConfigOptions = map[string]any{}
		}
		st.ConfigOptions[req.ConfigID] = req.Value
	}
	a.SessionManager.SaveSession(ctx, req.SessionID)
	serverLog.Info(fmt.Sprintf("Session %s: config option %s updated", req.SessionID, req.ConfigID))
	return &pyacp.SetSessionConfigOptionResponse{ConfigOptions: []pyacp.SessionConfigOption{}}, nil
}
