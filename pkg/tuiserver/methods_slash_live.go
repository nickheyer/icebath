package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Breakdown engine of the context gauge behind the live context view
var computeSessionContextBreakdown = agent.ComputeSessionContextBreakdown

// Per skill and per toolset cost engine behind the expanded context view
var computeContextDetails = agent.ComputeContextDetails

// Read commands answered from the live session only when its agent lives on a compute host
var isolatedSessionReadCommands = map[string]bool{"context": true, "tools": true, "help": true}

// Commands a local session answers in process instead of through the slash worker
var contextLocalInProcess = map[string]bool{"context": true}

const noAgentUsage = "(._.) No active agent -- send a message first."

// HelpCategory is one help category of the command registry with its command and description rows
type HelpCategory struct {
	Category string
	Rows     [][2]string
}

// CommandsByCategory lists the CLI command registry by category, assigned in cmd/icebath/wiring.go
var CommandsByCategory func() []HelpCategory

// Python int of a usage counter, zero for a falsy value
func slashInt(v any) int64 {
	if !truthy(v) {
		return 0
	}
	n, err := pyint.Int64(v)
	panicIf(err)
	return n
}

// Integer with comma thousands separators like the Python comma format spec
func slashGroupInt(n int64) string {
	text := fmt.Sprintf("%d", n)
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	var b strings.Builder
	for i, r := range text {
		if i > 0 && (len(text)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String()
}

// Mapping rows of a history list, other entries dropped
func slashRows(list []any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Copy of the live history taken under the history lock
func slashHistorySnapshot(session *tuigateway.Session) []any {
	unlock := lockHistory(session)
	defer unlock()
	return append([]any(nil), historyList(session.Get("history"))...)
}

// Error text of a compute host control failure, empty for a reply timeout like str of queue Empty
func controlFailureText(err error) string {
	if errors.Is(err, ErrHostReplyTimeout) {
		return ""
	}
	return err.Error()
}

// Dispatches review against the live agent on the async delegation rail under the session identity
func formatLiveReviewOutput(ctx context.Context, sid string, session *tuigateway.Session, arg string) string {
	if session == nil {
		return "Nothing to review yet — send a message first."
	}
	if sessionUsesComputeHost(ctx, session, nil) {
		return "/review runs on the local agent only for now — this session's agent lives on a remote compute host."
	}
	a := sessionAgent(session)
	if a == nil {
		return "Nothing to review yet — send a message first."
	}
	if truthy(session.Get("running")) {
		return "session busy — wait for the current turn to finish, then /review"
	}
	snapshot := slashRows(slashHistorySnapshot(session))
	if len(snapshot) == 0 {
		snapshot = agent.SessionMessageDicts(a.SessionMessages())
	}
	sessionCtx, env := setSessionContext(ctx, strOr(session.Get("session_key")), nil, sid)
	sessionCtx = withRuntimeSessionRecord(sessionCtx, session)
	defer clearSessionContext(env)
	var result string
	failure := catchPanic(func() {
		scope := sessionProfileRuntimeScope(sessionCtx, session, true)
		dispatched, err := tools.StartReview(scope, tools.ToolEnvFrom(scope), a, snapshot, arg)
		if err != nil {
			result = err.Error()
			return
		}
		result = tools.FormatDispatchNote(dispatched, arg)
	})
	if failure != nil {
		return "/review failed to start: " + failureText(failure)
	}
	return result
}

// Agent surface the refine dispatch drives, the tool names it offers and the explicit background review
type refineReviewAgent interface {
	ValidToolNames() map[string]bool
	SpawnExplicitBackgroundReview(ctx context.Context, snapshot []map[string]any, reviewMemory, reviewSkills bool, focus *string)
}

// Dispatches refine against the live agent, spawning the background memory and skills review directly
func formatLiveRefineOutput(ctx context.Context, sid string, session *tuigateway.Session, arg string) string {
	if session == nil {
		return "Refine unavailable (no session)."
	}
	if truthy(session.Get("running")) {
		return "Agent is running — wait for the turn to finish, then /refine."
	}
	focus := pystr.Strip(arg)
	if sessionUsesComputeHost(ctx, session, nil) {
		command := "/refine"
		if focus != "" {
			command += " " + arg
		}
		ack, err := sendComputeHostControl(ctx, sid, "slash.refine", command, nil, true, 30.0, nil)
		if err != nil {
			return "compute-host slash.refine failed: " + controlFailureText(err)
		}
		frame := objectMap(ack)
		if frame["type"] == "control.error" || frame["type"] == "error" {
			if msg := strOr(frame["message"]); truthy(frame["message"]) {
				return msg
			}
			return "compute-host slash.refine failed"
		}
		applyComputeHostMetadataMirror(ctx, session, frame)
		return strOr(frame["output"])
	}
	a, _ := session.Get("agent").(refineReviewAgent)
	if live, isAgent := a.(*agent.AIAgent); a == nil || (isAgent && live == nil) {
		return "Nothing to refine yet — send a message first."
	}
	var snapshot []map[string]any
	if ref := strOr(session.Get("session_key")); ref != "" {
		if failure := catchPanic(func() {
			db, release := sessionDB(ctx, session)
			defer release()
			if db != nil {
				rows, err := db.GetMessagesAsConversation(ctx, ref, state.ConversationOptions{IncludeAncestors: true})
				panicIf(err)
				snapshot = rows
			}
		}); failure != nil {
			logger().Debug("failed to load persisted /refine transcript", "error", failureText(failure))
		}
	}
	if len(snapshot) == 0 {
		snapshot = slashRows(slashHistorySnapshot(session))
	}
	if len(snapshot) == 0 {
		return "Nothing to refine yet — the conversation is empty."
	}
	reviewSkills := a.ValidToolNames()["skill_manage"]
	var focusArg *string
	if focus != "" {
		focusArg = &focus
	}
	if failure := catchPanic(func() {
		a.SpawnExplicitBackgroundReview(ctx, append([]map[string]any(nil), snapshot...), true, reviewSkills, focusArg)
	}); failure != nil {
		return "/refine failed to start: " + failureText(failure)
	}
	tail := ""
	if focus != "" {
		tail = " (focus: " + focus + ")"
	}
	return "⚗ Reviewing this conversation in the background" + tail + " — any memory/skill updates will be reported when done."
}

// Provider limits then Nous credits lines read under the session profile scope, no agent needed
func usageAccountBlocks(ctx context.Context, session *tuigateway.Session) []string {
	a := sessionAgent(session)
	pick, _ := session.Get("model_override").(map[string]any)
	var lines []string
	scope := sessionProfileRuntimeScope(ctx, session, true)
	suppress(func() {
		providerName := ""
		if a != nil {
			providerName = a.Provider()
		}
		if providerName == "" {
			providerName = strOr(pick["provider"])
		}
		if providerName == "" {
			_, providerName = configModelTarget(scope)
		}
		if providerName == "" {
			return
		}
		opts := provider.AccountUsageFetchOptions{}
		if a != nil {
			opts.BaseURL, opts.APIKey = a.BaseURL(), a.APIKey()
		}
		lines = append(lines, provider.RenderAccountUsageLines(provider.FetchAccountUsage(scope, providerName, opts), false)...)
	})
	suppress(func() {
		var credits *provider.CreditsState
		if a != nil {
			credits = a.CreditsCapture().Credits()
		}
		if credits != nil && credits.IsUsageWindow() {
			lines = append(lines, provider.NousCreditsLines(scope, false, 10*time.Second, credits)...)
			return
		}
		lines = append(lines, provider.NousCreditsLines(scope, false, 10*time.Second, nil)...)
	})
	return lines
}

// Session token usage block followed by the account limits, the no agent reply when both are empty
func formatLiveUsageOutput(ctx context.Context, sid string, session *tuigateway.Session, arg string) string {
	extra := usageAccountBlocks(ctx, session)
	var parts []string
	if tokens := formatLiveUsageTokens(ctx, session); tokens != "" {
		parts = append(parts, tokens)
	}
	if joined := strings.Join(extra, "\n"); joined != "" {
		parts = append(parts, joined)
	}
	if out := strings.Join(parts, "\n\n"); out != "" {
		return out
	}
	return noAgentUsage
}

// Session token usage table of the live usage snapshot, empty without an agent or usage
func formatLiveUsageTokens(ctx context.Context, session *tuigateway.Session) string {
	a := sessionAgent(session)
	usage := sessionUsageSnapshot(ctx, session)
	if a == nil && len(usage) == 0 {
		return ""
	}
	var messageCount int64
	if raw, present := session.Lookup("_metadata_message_count"); present && raw != nil {
		messageCount = slashInt(raw)
	} else {
		messageCount = int64(len(slashHistorySnapshot(session)))
	}
	n := func(key string) string { return slashGroupInt(slashInt(usage[key])) }
	rows := [][2]string{{"Input tokens:", n("input")}, {"Output tokens:", n("output")}}
	if slashInt(usage["reasoning"]) != 0 {
		rows = append(rows, [2]string{"Reasoning tokens:", n("reasoning")})
	}
	rows = append(rows, [2]string{"Prompt tokens:", n("prompt")}, [2]string{"Completion tokens:", n("completion")},
		[2]string{"Total tokens:", n("total")}, [2]string{"API calls:", n("calls")})
	if truthy(usage["context_max"]) {
		pct := slashInt(usage["context_percent"])
		mark := ""
		if truthy(usage["context_estimated"]) {
			mark = "~"
		}
		rows = append(rows, [2]string{"Current context:", fmt.Sprintf("%s%s / %s (%s%d%%)", mark, n("context_used"), n("context_max"), mark, pct)})
	}
	rows = append(rows, [2]string{"Messages:", slashGroupInt(messageCount)}, [2]string{"Compressions:", n("compressions")})
	model := strOr(usage["model"])
	if !truthy(usage["model"]) {
		model = strOr(metadataMirror(session)["model"])
	}
	if model == "" && a != nil {
		model = a.Model()
	}
	if model == "" {
		model = "(unknown)"
	}
	lines := []string{"Session Token Usage", strings.Repeat("─", 40), "Model: " + model}
	for _, row := range rows {
		lines = append(lines, fmt.Sprintf("%-30s%s", row[0], row[1]))
	}
	return strings.Join(lines, "\n")
}

// Session scoped transcript with row ids, ok false without a db or key or when the read fails
func liveSessionMessages(ctx context.Context, session *tuigateway.Session) (rows []map[string]any, ok bool) {
	db, release := sessionDB(ctx, session)
	defer release()
	key := strOr(session.Get("session_key"))
	if db == nil || key == "" {
		return nil, false
	}
	if failure := catchPanic(func() {
		read, err := db.GetMessagesAsConversation(ctx, key, state.ConversationOptions{IncludeAncestors: true, IncludeRowIDs: true})
		panicIf(err)
		rows, ok = read, true
	}); failure != nil {
		return nil, false
	}
	return rows, ok
}

// Rows as the generic list the history converters take
func slashAnyList(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

// Numbered transcript of the session, the persisted rows preferred over the live history
func formatLiveHistoryOutput(ctx context.Context, sid string, session *tuigateway.Session, arg string) string {
	history := slashHistorySnapshot(session)
	if dbRows, ok := liveSessionMessages(ctx, session); ok {
		history = slashAnyList(dbRows)
	}
	messages, err := tuigateway.HistoryToMessages(ctx, history, strOr(session.Get("profile_home")), true)
	panicIf(err)
	if len(messages) == 0 {
		return "No conversation history yet."
	}
	lines := []string{"Conversation History", strings.Repeat("─", 40)}
	for idx, item := range messages {
		message, _ := item.(map[string]any)
		role := strOr(message["role"])
		if !truthy(message["role"]) {
			role = "unknown"
		}
		label := map[string]string{"user": "You", "assistant": "Hermes"}[role]
		if label == "" {
			label = pystr.Title(role)
		}
		raw := message["text"]
		if !truthy(raw) {
			raw = message["context"]
		}
		text := ""
		if truthy(raw) {
			text = pystr.Strip(strOr(raw))
		}
		if runes := []rune(text); len(runes) > 400 {
			text = string(runes[:400]) + "..."
		}
		if text == "" {
			text = "(no text)"
		}
		lines = append(lines, fmt.Sprintf("[%s #%d] %s", label, idx+1, text))
	}
	return strings.Join(lines, "\n")
}

// Context view of the session, the live agent breakdown when it renders, else the persisted state view
func formatLiveContextOutput(ctx context.Context, sid string, session *tuigateway.Session, arg string) string {
	profileHome := strOr(session.Get("profile_home"))
	var messages []any
	if failure := catchPanic(func() {
		dbRows, _ := liveSessionMessages(ctx, session)
		converted, err := tuigateway.HistoryToMessages(ctx, slashAnyList(dbRows), profileHome, true)
		panicIf(err)
		messages = converted
	}); failure != nil {
		messages = nil
	}
	if len(messages) == 0 {
		converted, err := tuigateway.HistoryToMessages(ctx, slashHistorySnapshot(session), profileHome, true)
		panicIf(err)
		messages = converted
	}
	var breakdownLines []string
	if a := sessionAgent(session); a != nil {
		liveMessages := slashRows(slashHistorySnapshot(session))
		if failure := catchPanic(func() {
			cwd := sessionCwd(ctx, session)
			sessionCtx, env := setSessionContext(ctx, strOr(session.Get("session_key")), &cwd, "")
			defer clearSessionContext(env)
			payload, err := computeSessionContextBreakdown(sessionCtx, a, liveMessages)
			panicIf(err)
			var details *agent.ContextDetails
			switch pystr.Lower(pystr.Strip(arg)) {
			case "all", "full", "details":
				suppress(func() {
					d, err := computeContextDetails(sessionCtx, a)
					panicIf(err)
					details = &d
				})
			}
			breakdownLines = agent.RenderContextBreakdownLines(sessionCtx, payload, details, false)
		}); failure != nil {
			breakdownLines = nil
		}
	}
	usage := sessionUsageSnapshot(ctx, session)
	mirror := metadataMirror(session)
	var lines []string
	if len(messages) > 0 {
		lines = append(lines, fmt.Sprintf("Conversation: %d messages", len(messages)))
	} else {
		lines = append(lines, "Conversation is empty (no messages yet).")
	}
	roles := map[string]int{}
	for _, item := range messages {
		msg, _ := item.(map[string]any)
		role := "unknown"
		if truthy(msg["role"]) {
			role = strOr(msg["role"])
		}
		roles[role]++
	}
	counts := make([]string, 0, 4)
	for _, r := range []string{"user", "assistant", "tool", "system"} {
		counts = append(counts, fmt.Sprintf("%s: %d", r, roles[r]))
	}
	lines = append(lines, "  "+strings.Join(counts, ", "))
	model := mirror["model"]
	if !truthy(model) {
		model = usage["model"]
	}
	if truthy(model) {
		lines = append(lines, "Model: "+strOr(model))
	}
	providerName := "auto"
	if truthy(mirror["provider"]) {
		providerName = strOr(mirror["provider"])
	}
	lines = append(lines, "Provider: "+providerName)
	if len(breakdownLines) > 0 {
		lines = append(append(lines, ""), breakdownLines...)
		return strings.Join(lines, "\n")
	}
	contextUsed := slashInt(usage["context_used"])
	mark := ""
	if truthy(usage["context_estimated"]) {
		mark = "~"
	}
	contextMax := slashInt(usage["context_max"])
	if contextUsed != 0 && contextMax != 0 {
		lines = append(lines, fmt.Sprintf("Context usage: %s%s / %s tokens (%s%.1f%%)", mark, slashGroupInt(contextUsed), slashGroupInt(contextMax), mark,
			float64(contextUsed)/float64(contextMax)*100))
	} else if contextUsed != 0 {
		lines = append(lines, fmt.Sprintf("Context usage: %s%s tokens", mark, slashGroupInt(contextUsed)))
	}
	if truthy(usage["compressions"]) {
		lines = append(lines, "Compressions: "+slashGroupInt(slashInt(usage["compressions"])))
	}
	if a := sessionAgent(session); a != nil {
		cwd := sessionCwd(ctx, session)
		sessionCtx, env := setSessionContext(ctx, strOr(session.Get("session_key")), &cwd, "")
		fileLines := func() []string {
			defer clearSessionContext(env)
			sources, err := agent.ContextFileSourcesForAgent(sessionCtx, a)
			panicIf(err)
			return agent.RenderContextFileLines(sources)
		}()
		if len(fileLines) > 0 {
			lines = append(append(lines, ""), fileLines...)
		}
	}
	return strings.Join(lines, "\n")
}

// Sorted tool names of the session info tool groups
func formatLiveToolsOutput(ctx context.Context, sid string, session *tuigateway.Session, arg string) string {
	info := sessionInfo(ctx, sessionAgent(session), session)
	groups, _ := info["tools"].(map[string]any)
	if len(groups) == 0 {
		return "No tools available."
	}
	seen := map[string]bool{}
	for _, g := range groups {
		list, isList := g.([]any)
		if !isList {
			if names, isStrings := g.([]string); isStrings {
				for _, name := range names {
					seen[name] = true
				}
			}
			continue
		}
		for _, name := range list {
			seen[strOr(name)] = true
		}
	}
	if len(seen) == 0 {
		return "No tools available."
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]string, len(names))
	for i, name := range names {
		rows[i] = "  " + name
	}
	return fmt.Sprintf("Available tools (%d):\n%s", len(names), strings.Join(rows, "\n"))
}

// Command registry listing by category, the failure text when the registry cannot be read
func formatLiveHelpOutput(ctx context.Context, sid string, session *tuigateway.Session, arg string) (out string) {
	if failure := catchPanic(func() {
		lines := []string{"Available commands:", ""}
		for _, group := range CommandsByCategory() {
			lines = append(lines, group.Category+":")
			for _, row := range group.Rows {
				lines = append(lines, fmt.Sprintf("  %-15s %s", row[0], row[1]))
			}
		}
		out = strings.Join(lines, "\n")
	}); failure != nil {
		return "help unavailable: " + failureText(failure)
	}
	return out
}

// Current model and provider of the live agent
func formatLiveModelOutput(session *tuigateway.Session) string {
	var model, providerName string
	if session != nil {
		if a := sessionAgent(session); a != nil {
			model, providerName = a.Model(), a.Provider()
		}
	}
	if model == "" {
		return "Current model: (unknown)"
	}
	if providerName != "" {
		return "Current model: " + model + " (" + providerName + ")"
	}
	return "Current model: " + model
}

// Output of the session status method for this session
func formatLiveStatusOutput(ctx context.Context, sid string, session *tuigateway.Session, arg string) string {
	handler := lookupMethod("session.status")
	if handler == nil {
		panic(errors.New("'session.status'"))
	}
	response := handler(ctx, "status", map[string]any{"session_id": sid})
	if errPayload, ok := response["error"].(map[string]any); ok && len(errPayload) > 0 {
		if truthy(errPayload["message"]) {
			return strOr(errPayload["message"])
		}
		return "status unavailable"
	}
	result, _ := response["result"].(map[string]any)
	if !truthy(result["output"]) {
		return ""
	}
	return strOr(result["output"])
}

// Reply when no session exists and the formatter or fixed reply of a live answered slash command
type liveSlashEntry struct {
	noSessionReply *string
	format         func(ctx context.Context, sid string, session *tuigateway.Session, arg string) string
	fixed          string
}

// Pointer to a no session reply
func replyText(s string) *string { return &s }

// Live answered slash commands by name
var liveSlashOutput map[string]liveSlashEntry

func init() {
	liveSlashOutput = map[string]liveSlashEntry{
		"compress": {replyText("no active session for /compress"), func(ctx context.Context, sid string, session *tuigateway.Session, arg string) string {
			return mirrorSlashSideEffects(ctx, sid, session, pystr.Strip("/compress "+arg))
		}, ""},
		"usage":   {replyText(noAgentUsage), formatLiveUsageOutput, ""},
		"review":  {nil, formatLiveReviewOutput, ""},
		"history": {replyText("No conversation history yet."), formatLiveHistoryOutput, ""},
		"refine":  {nil, formatLiveRefineOutput, ""},
		"status":  {nil, formatLiveStatusOutput, ""},
		"context": {replyText("Conversation is empty (no messages yet)."), formatLiveContextOutput, ""},
		"tools":   {replyText("No tools available."), formatLiveToolsOutput, ""},
		"help":    {nil, formatLiveHelpOutput, ""},
		"clear":   {nil, nil, "Screen clear is terminal-only; desktop/TUI chat left unchanged."},
		"models":  {nil, nil, "Use /model to view or switch the current model; desktop users can also open the model picker."},
		"rename":  {nil, nil, "Use /title <name> to rename this session."},
		"effort":  {nil, nil, "Use /reasoning <effort> to change reasoning effort."},
	}
}

// Answers a slash command from the live session instead of the slash worker, ok false when it is not one of these
func liveSlashCommandOutput(ctx context.Context, sid string, session *tuigateway.Session, name, arg string) (string, bool) {
	name = pystr.Lower(strings.TrimLeft(name, "/"))
	if name == "model" && pystr.Strip(arg) == "" {
		return formatLiveModelOutput(session), true
	}
	if contextLocalInProcess[name] && session != nil && !sessionUsesComputeHost(ctx, session, nil) {
		return formatLiveContextOutput(ctx, sid, session, arg), true
	}
	if isolatedSessionReadCommands[name] && !(session != nil && sessionUsesComputeHost(ctx, session, nil)) {
		return "", false
	}
	entry, ok := liveSlashOutput[name]
	if !ok {
		return "", false
	}
	if session == nil && entry.noSessionReply != nil {
		return *entry.noSessionReply, true
	}
	if entry.format != nil {
		return entry.format(ctx, sid, session, arg), true
	}
	return entry.fixed, true
}
