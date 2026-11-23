package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Breakdown engine of the context gauge behind the live context view
var computeSessionContextBreakdown = agent.ComputeSessionContextBreakdown

// Per skill and per toolset cost engine behind the expanded context view
var computeContextDetails = agent.ComputeContextDetails

// Commands a local session answers in process
var contextLocalInProcess = map[string]bool{"context": true}

const noAgentUsage = "◈ No active agent -- send a message first."

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
func slashHistorySnapshot(session *tuihost.Session) []any {
	unlock := lockHistory(session)
	defer unlock()
	return append([]any(nil), historyList(session.Get("history"))...)
}

// Dispatches review against the live agent on the async delegation rail under the session identity
func formatLiveReviewOutput(ctx context.Context, sid string, session *tuihost.Session, arg string) string {
	if session == nil {
		return "Nothing to review yet — send a message first."
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
func formatLiveRefineOutput(ctx context.Context, sid string, session *tuihost.Session, arg string) string {
	if session == nil {
		return "Refine unavailable (no session)."
	}
	if truthy(session.Get("running")) {
		return "Agent is running — wait for the turn to finish, then /refine."
	}
	focus := pystr.Strip(arg)
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
			pkgLog().Debug("failed to load persisted /refine transcript", "error", failureText(failure))
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

// Provider account limit lines read under the session profile scope, no agent needed
func usageAccountBlocks(ctx context.Context, session *tuihost.Session) []string {
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
	return lines
}

// Session token usage block followed by the account limits, the no agent reply when both are empty
func formatLiveUsageOutput(ctx context.Context, sid string, session *tuihost.Session, arg string) string {
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
func formatLiveUsageTokens(ctx context.Context, session *tuihost.Session) string {
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
func liveSessionMessages(ctx context.Context, session *tuihost.Session) (rows []map[string]any, ok bool) {
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
func formatLiveHistoryOutput(ctx context.Context, sid string, session *tuihost.Session, arg string) string {
	history := slashHistorySnapshot(session)
	if dbRows, ok := liveSessionMessages(ctx, session); ok {
		history = slashAnyList(dbRows)
	}
	messages, err := tuihost.HistoryToMessages(ctx, history, strOr(session.Get("profile_home")), true)
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
		label := map[string]string{"user": "You", "assistant": "Icebath"}[role]
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

// Output of the session status method for this session
func formatLiveStatusOutput(ctx context.Context, sid string, session *tuihost.Session, arg string) string {
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
	format         func(ctx context.Context, sid string, session *tuihost.Session, arg string) string
	fixed          string
}

// Pointer to a no session reply
func replyText(s string) *string { return &s }

// Live answered slash commands by name
var liveSlashOutput map[string]liveSlashEntry

func init() {
	liveSlashOutput = map[string]liveSlashEntry{
		"compress": {replyText("no active session for /compress"), liveCompressOutput, ""},
		"usage":    {replyText(noAgentUsage), formatLiveUsageOutput, ""},
		"review":   {nil, formatLiveReviewOutput, ""},
		"history":  {replyText("No conversation history yet."), formatLiveHistoryOutput, ""},
		"refine":   {nil, formatLiveRefineOutput, ""},
		"status":   {nil, formatLiveStatusOutput, ""},
		"clear":    {nil, nil, "Screen clear is terminal-only; the TUI chat is left unchanged."},
		"models":   {nil, nil, "Use /model to view or switch the current model."},
		"rename":   {nil, nil, "Use /title <name> to rename this session."},
		"effort":   {nil, nil, "Use /reasoning <effort> to change reasoning effort."},
	}
}
