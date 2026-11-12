package acp

import (
	"context"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Rough token estimate of a request made of messages, a system prompt and tool schemas
var estimateRequestTokensRough = func(ctx context.Context, messages []map[string]any, systemPrompt string, toolDefs []map[string]any) int {
	rows := make([]*state.Message, len(messages))
	for i, m := range messages {
		rows[i] = state.MessageFromDict(m)
	}
	return provider.EstimateRequestTokens(ctx, rows, systemPrompt, toolDefs, true)
}

// Model facing tool definitions of a toolset selection
var getToolDefinitions = func(ctx context.Context, env *tools.Env, enabled, disabled []string) []map[string]any {
	return tools.Default.ToolDefinitions(ctx, env, tools.DefinitionOptions{Enabled: enabled, Disabled: disabled, Quiet: true})
}

// Manual compression of a history on the session agent
var compressNow = func(ctx context.Context, a any, history []map[string]any, request agent.CompressRequest, o agent.CompressNowOptions) (agent.CompressResult, error) {
	real, ok := a.(*agent.AIAgent)
	if !ok {
		return agent.CompressResult{}, fmt.Errorf("Context compression not available for this agent.")
	}
	return agent.CompressNow(ctx, real, history, request, o)
}

// Version provenance of this install
var getVersionInfo = updaterecord.GetVersionInfo

// Request token estimate over history, the agent's cached system prompt and its tool schemas
func estimateTokens(ctx context.Context, history []map[string]any, a any) int {
	toolDefs := agentToolDefs(a)
	if len(toolDefs) == 0 {
		toolDefs = nil
	}
	return estimateRequestTokensRough(ctx, history, agentCachedSystemPrompt(a), toolDefs)
}

// Appends a prompt to the session queue and returns the queue length
func queuePrompt(st *SessionState, text string) int {
	st.RuntimeLock.Lock()
	defer st.RuntimeLock.Unlock()
	st.QueuedPrompts = append(st.QueuedPrompts, text)
	return len(st.QueuedPrompts)
}

// Commands that mutate shared turn state and so run only while the session is idle
var midTurnBlockedCommands = map[string]bool{"reset": true, "compress": true, "model": true}

// Help text, advertised description and input hint of one slash command
type slashCommandSpec struct {
	Name        string
	Help        string
	Description string
	Hint        string
}

// Slash commands the in process agent serves, in advertised order
var slashCommands = []slashCommandSpec{
	{"help", "Show available commands", "List available commands", ""},
	{"model", "Show or change current model", "Show current model and provider, or switch models", "model name to switch to"},
	{"tools", "List available tools", "List available tools with descriptions", ""},
	{"context", "Show conversation context info", "Show conversation message counts by role", ""},
	{"reset", "Clear conversation history", "Clear conversation history", ""},
	{"compress", "Compress conversation context", "Compress conversation context", ""},
	{"steer", "Inject guidance into the currently running agent turn", "Inject guidance into the currently running agent turn", "guidance for the active turn"},
	{"queue", "Queue a prompt to run after the current turn finishes", "Queue a prompt to run after the current turn finishes", "prompt to run next"},
	{"version", "Show Icebath version", "Show Icebath version", ""},
}

// Handler of one slash command returning its reply text
type slashHandler func(ctx context.Context, args string, st *SessionState) (string, error)

// Advertised form of the slash command table
func availableCommands() []pyacp.AvailableCommand {
	out := make([]pyacp.AvailableCommand, 0, len(slashCommands))
	for _, c := range slashCommands {
		cmd := pyacp.AvailableCommand{Name: c.Name, Description: c.Description}
		if c.Hint != "" {
			cmd.Input = &pyacp.UnstructuredCommandInput{Hint: c.Hint}
		}
		out = append(out, cmd)
	}
	return out
}

// Advertises the supported slash commands to the connected client
func (a *HermesACPAgent) sendAvailableCommandsUpdate(ctx context.Context, sessionID string) {
	if a.client() == nil {
		return
	}
	update := &pyacp.AvailableCommandsUpdate{AvailableCommands: availableCommands()}
	a.send(ctx, sessionID, update, "Failed to advertise ACP slash commands for session %s", logging.LevelWarning)
}

// Advertises the slash commands right after the current response is queued
func (a *HermesACPAgent) scheduleAvailableCommandsUpdate(ctx context.Context, sessionID string) {
	a.scheduleSoon(ctx, func(ctx context.Context) { a.sendAvailableCommandsUpdate(ctx, sessionID) })
}

// Head and stripped remainder of a text split once on whitespace
func splitCommand(text string) (head, rest string) {
	parts := pystr.SplitWhitespace(text, 1)
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) > 1 {
		rest = pystr.Strip(parts[1])
	}
	return parts[0], rest
}

// Reply of a known slash command, false for an unknown one so it falls through to the model
func (a *HermesACPAgent) handleSlashCommand(ctx context.Context, text string, st *SessionState) (reply string, known bool) {
	head, args := splitCommand(text)
	cmd := strings.ToLower(strings.TrimLeft(head, "/"))
	handler, ok := a.commandHandlers[cmd]
	if !ok {
		return "", false
	}
	observability.RecordSlashCommand(ctx, cmd, "acp")
	mutating := midTurnBlockedCommands[cmd]
	if mutating {
		st.RuntimeLock.Lock()
		if st.IsRunning || st.CommandOp {
			st.RuntimeLock.Unlock()
			return fmt.Sprintf("⏳ Session is busy; /%s only works while the session is idle. Wait for the current response or cancel first.", cmd), true
		}
		st.CommandOp = true
		st.RuntimeLock.Unlock()
		defer func() {
			st.RuntimeLock.Lock()
			st.CommandOp = false
			st.RuntimeLock.Unlock()
		}()
	}
	defer func() {
		if r := recover(); r != nil {
			serverLog.Error(fmt.Sprintf("Slash command /%s error: %v", cmd, r))
			reply, known = fmt.Sprintf("Error executing /%s: %v", cmd, r), true
		}
	}()
	if env := agentEnv(st.Agent); env != nil && env.Values != nil {
		previous := tools.SetLogicalSessionCwd(env, st.Cwd)
		defer tools.SetLogicalSessionCwd(env, previous)
	}
	out, err := handler(ctx, args, st)
	if err != nil {
		serverLog.Error(fmt.Sprintf("Slash command /%s error: %s", cmd, err))
		return fmt.Sprintf("Error executing /%s: %s", cmd, err), true
	}
	return out, true
}

func (a *HermesACPAgent) cmdHelp(ctx context.Context, args string, st *SessionState) (string, error) {
	lines := []string{"Available commands:", ""}
	for _, c := range slashCommands {
		lines = append(lines, fmt.Sprintf("  /%-10s  %s", c.Name, c.Help))
	}
	lines = append(lines, "", "Unrecognized /commands are sent to the model as normal messages.")
	return strings.Join(lines, "\n"), nil
}

func (a *HermesACPAgent) cmdModel(ctx context.Context, args string, st *SessionState) (string, error) {
	if args == "" {
		model := st.Model
		if model == "" {
			model = "unknown"
			if m, ok := st.Agent.(interface{ Model() string }); ok {
				model = m.Model()
			}
		}
		providerName := agentProvider(st.Agent)
		if providerName == "" {
			providerName = "auto"
		}
		return fmt.Sprintf("Current model: %s\nProvider: %s", model, providerName), nil
	}
	currentProvider, targetProvider, newModel, err := a.switchModel(ctx, st, args, false)
	if err != nil {
		return "", err
	}
	label := "openrouter"
	for _, candidate := range []string{agentProvider(st.Agent), targetProvider, currentProvider} {
		if candidate != "" {
			label = candidate
			break
		}
	}
	serverLog.Info(fmt.Sprintf("Session %s: model switched to %s", st.SessionID, newModel))
	reply := fmt.Sprintf("Model switched to: %s\nProvider: %s", newModel, label)
	if notice := provider.LargeModelNoticeFor(ctx, newModel, targetProvider, false, "en"); notice != "" {
		reply += "\n⚠ " + notice
	}
	return reply, nil
}

func (a *HermesACPAgent) cmdTools(ctx context.Context, args string, st *SessionState) (reply string, err error) {
	defer func() {
		if r := recover(); r != nil {
			reply, err = fmt.Sprintf("Could not list tools: %v", r), nil
		}
	}()
	enabled := agentEnabledToolsets(st.Agent)
	toolsets := expandACPEnabledToolsets(enabled, enabled == nil, nil)
	disabled := agentDisabledToolsets(st.Agent)
	view := toolViewEnv(ctx, st.Agent, toolsets, disabled)
	defs := getToolDefinitions(ctx, view, toolsets, disabled)
	surface := append([]map[string]any{}, defs...)
	names := map[string]bool{}
	for _, t := range surface {
		if fn, ok := pyjson.Dict(t["function"]); ok {
			names[pyStr(fn["name"])] = true
		}
	}
	surface, _, _ = memory.InjectMemoryProviderTools(ctx, view, agentMemoryManager(st.Agent), surface, names)
	if len(surface) == 0 {
		return "No tools available.", nil
	}
	lines := []string{fmt.Sprintf("Available tools (%d):", len(surface))}
	for _, t := range surface {
		fn, _ := pyjson.Dict(t["function"])
		name, desc := "?", ""
		if v, ok := fn["name"]; ok {
			name = pyStr(v)
		}
		if v, ok := fn["description"]; ok {
			desc = pyStr(v)
		}
		if r := []rune(desc); len(r) > 80 {
			desc = string(r[:77]) + "..."
		}
		lines = append(lines, fmt.Sprintf("  %s: %s", name, desc))
	}
	return strings.Join(lines, "\n"), nil
}

// Integer rendered with thousands separators
func grouped(n int) string {
	s := strconv.Itoa(n)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return sign + s
}

func (a *HermesACPAgent) cmdContext(ctx context.Context, args string, st *SessionState) (string, error) {
	nMessages := len(st.History)
	roles := map[string]int{}
	for _, msg := range st.History {
		role := "unknown"
		if v, ok := msg["role"]; ok {
			role = pyStr(v)
		}
		roles[role]++
	}
	model := st.Model
	if model == "" {
		model = agentModel(st.Agent)
	}
	providerName := agentProvider(st.Agent)
	if providerName == "" {
		providerName = "auto"
	}
	contextLength, thresholdTokens, _ := agentCompressorFigures(st.Agent)
	approxTokens := 0
	func() {
		defer func() {
			if r := recover(); r != nil {
				serverLog.Debug(fmt.Sprintf("Could not estimate ACP context usage\n%v", r))
				approxTokens = 0
			}
		}()
		approxTokens = estimateTokens(ctx, st.History, st.Agent)
	}()
	if thresholdTokens <= 0 && contextLength > 0 {
		thresholdTokens = int(float64(contextLength) * 0.80)
	}
	first := "Conversation is empty (no messages yet)."
	if nMessages > 0 {
		first = fmt.Sprintf("Conversation: %d messages", nMessages)
	}
	lines := []string{first, fmt.Sprintf("  user: %d, assistant: %d, tool: %d, system: %d", roles["user"], roles["assistant"], roles["tool"], roles["system"])}
	if model != "" {
		lines = append(lines, "Model: "+model)
	}
	lines = append(lines, "Provider: "+providerName)
	if approxTokens > 0 && contextLength > 0 {
		usagePct := float64(approxTokens) / float64(contextLength) * 100
		lines = append(lines, fmt.Sprintf("Context usage: ~%s / %s tokens (%.1f%%)", grouped(approxTokens), grouped(contextLength), usagePct))
	} else if approxTokens > 0 {
		lines = append(lines, fmt.Sprintf("Context usage: ~%s tokens", grouped(approxTokens)))
	}
	if thresholdTokens > 0 && approxTokens > 0 {
		thresholdPct := 0.0
		if contextLength > 0 {
			thresholdPct = float64(thresholdTokens) / float64(contextLength) * 100
		}
		pctNote := ""
		if thresholdPct != 0 {
			pctNote = fmt.Sprintf(", %.0f%%", thresholdPct)
		}
		if approxTokens >= thresholdTokens {
			lines = append(lines, fmt.Sprintf("Compression: due now (threshold ~%s%s). Run /compress.", grouped(thresholdTokens), pctNote))
		} else {
			remaining := max(thresholdTokens-approxTokens, 0)
			lines = append(lines, fmt.Sprintf("Compression: ~%s tokens until threshold (~%s%s).", grouped(remaining), grouped(thresholdTokens), pctNote))
		}
	} else if thresholdTokens > 0 {
		lines = append(lines, fmt.Sprintf("Compression threshold: ~%s tokens", grouped(thresholdTokens)))
	}
	if agentCompressionEnabled(st.Agent) {
		lines = append(lines, "Tip: run /compress to compress manually before the threshold.")
	} else {
		lines = append(lines, "Auto-compaction is disabled (compression.enabled: false); /compress still compresses manually.")
	}
	return strings.Join(lines, "\n"), nil
}

func (a *HermesACPAgent) cmdReset(ctx context.Context, args string, st *SessionState) (reply string, err error) {
	st.History = st.History[:0]
	defer a.SessionManager.SaveSession(ctx, st.SessionID)
	defer func() {
		if r := recover(); r != nil {
			serverLog.Warning(fmt.Sprintf("ACP session state reset failed for %s\n%v", st.SessionID, r))
			reply, err = "Conversation history cleared. Agent session state reset failed; see logs.", nil
		}
	}()
	if resetter, ok := st.Agent.(interface{ ResetSessionState() }); ok {
		resetter.ResetSessionState()
	}
	return "Conversation history cleared.", nil
}

func (a *HermesACPAgent) cmdCompress(ctx context.Context, args string, st *SessionState) (string, error) {
	if len(st.History) == 0 {
		return "Nothing to compress — conversation is empty.", nil
	}
	detacher, ok := st.Agent.(interface{ WithSessionStoreDetached(fn func()) })
	if !ok {
		return "Context compression not available for this agent.", nil
	}
	request := agent.ParseCompressArgs(args)
	if request.Aggressive {
		return agent.AggressiveUnsupported, nil
	}
	system := agentCachedSystemPrompt(st.Agent)
	var result agent.CompressResult
	var failure error
	func() {
		defer func() {
			if r := recover(); r != nil {
				failure = fmt.Errorf("%v", r)
			}
		}()
		detacher.WithSessionStoreDetached(func() {
			result, failure = compressNow(ctx, st.Agent, st.History, request, agent.CompressNowOptions{SystemMessage: &system, TaskID: st.SessionID})
		})
	}()
	if failure != nil {
		return "Compression failed: " + failure.Error(), nil
	}
	if result.Status != "compressed" {
		return strings.Join(agent.RenderCompressResult(result, ""), "\n"), nil
	}
	st.History = result.AfterMessages
	a.SessionManager.SaveSession(ctx, st.SessionID)
	if real, ok := st.Agent.(*agent.AIAgent); ok {
		agent.FinalizeContextEngineCompressionNotification(real, true)
	}
	return fmt.Sprintf("Context compressed: %d -> %d messages\n~%s -> ~%s tokens", len(result.BeforeMessages), len(st.History),
		grouped(result.BeforeTokens), grouped(result.AfterTokens)), nil
}

func (a *HermesACPAgent) cmdSteer(ctx context.Context, args string, st *SessionState) (reply string, err error) {
	steerText := pystr.Strip(args)
	if steerText == "" {
		return "Usage: /steer <guidance>", nil
	}
	if steerer, ok := st.Agent.(interface{ Steer(text string) bool }); ok && st.IsRunning {
		steered, failure := func() (ok bool, failure any) {
			defer func() { failure = recover() }()
			return steerer.Steer(steerText), nil
		}()
		if failure != nil {
			serverLog.Warning(fmt.Sprintf("ACP steer failed for session %s: %v", st.SessionID, failure))
			return fmt.Sprintf("⚠️ Steer failed: %v", failure), nil
		}
		if steered {
			preview := steerText
			if r := []rune(steerText); len(r) > 80 {
				preview = string(r[:80]) + "..."
			}
			return "⏩ Steer queued for the active turn: " + preview, nil
		}
	}
	return fmt.Sprintf("No active turn — queued for the next turn. (%d queued)", queuePrompt(st, steerText)), nil
}

func (a *HermesACPAgent) cmdQueue(ctx context.Context, args string, st *SessionState) (string, error) {
	queued := pystr.Strip(args)
	if queued == "" {
		return "Usage: /queue <prompt>", nil
	}
	return fmt.Sprintf("Queued for the next turn. (%d queued)", queuePrompt(st, queued)), nil
}

func (a *HermesACPAgent) cmdVersion(ctx context.Context, args string, st *SessionState) (string, error) {
	info, err := getVersionInfo()
	if err != nil {
		return "", err
	}
	return "Icebath Agent v" + info.DerivedVersion, nil
}

// Env of the session agent, nil when the agent carries none
func agentEnv(a any) *tools.Env {
	if holder, ok := a.(interface{ Env() *tools.Env }); ok {
		return holder.Env()
	}
	return nil
}

// Memory provider owner of the session agent, nil when the agent has none
func agentMemoryManager(a any) *memory.MemoryManager {
	holder, ok := a.(interface {
		MemoryManager() tools.MemoryToolManager
	})
	if !ok {
		return nil
	}
	manager, _ := holder.MemoryManager().(*memory.MemoryManager)
	return manager
}

// Copy of the agent env with its own value store, the toolset selection of a tool view bound on it
func toolViewEnv(ctx context.Context, a any, enabled, disabled []string) *tools.Env {
	view := &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
	if env := agentEnv(a); env != nil {
		copied := *env
		view = &copied
	}
	values := &sync.Map{}
	if view.Values != nil {
		view.Values.Range(func(k, v any) bool {
			values.Store(k, v)
			return true
		})
	}
	view.Values = values
	view.Enabled, view.Disabled = enabled, disabled
	return view
}
