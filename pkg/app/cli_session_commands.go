package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const boldRedANSI = "\033[1m\033[91m"

const boldGreenANSI = "\033[1m\033[92m"

// Indices of real user turns, scaffolding, timeline rows and compaction handoffs excluded
func userTurnIndices(history []map[string]any) []int {
	var out []int
	for i, m := range history {
		if !state.IsEphemeralScaffolding(m) && state.UserOriginatedTurnView(m) != nil {
			out = append(out, i)
		}
	}
	return out
}

// Rederives model and provider from config.yaml so a session only switch never leaks into the next session
func resetModelToConfigDefault(c *CLI, silent bool) {
	rawDefault, configProvider := any(""), ""
	switch modelConfig := c.config["model"].(type) {
	case map[string]any:
		rawDefault = modelConfig["default"]
		if !cliTruthy(rawDefault) {
			rawDefault = modelConfig["model"]
		}
		if !cliTruthy(rawDefault) {
			rawDefault = ""
		}
		configProvider = cliStr(modelConfig["provider"], "")
	default:
		if cliTruthy(modelConfig) {
			rawDefault = modelConfig
		}
	}
	configModel, _ := config.SplitModelDefault(rawDefault)
	if configModel == "" || configModel == c.model {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug("/new model reset to config default failed")
		}
	}()
	r, err := provider.SwitchModel(c.ctx, provider.SwitchModelOptions{RawInput: configModel, CurrentProvider: c.provider,
		CurrentBaseURL: c.baseURL, CurrentAPIKey: c.apiKey, IsGlobal: false, ExplicitProvider: configProvider})
	if err != nil || !r.Success {
		if err != nil {
			cliLogger.Debug("/new model reset to config default failed")
		}
		return
	}
	if c.agentRef() != nil && !c.gatewaySessionOpen {
		if _, err := c.agentRef().SwitchModel(r.NewModel, r.TargetProvider, false, false, ""); err != nil {
			cliLogger.Debug("/new model reset to config default failed")
			return
		}
	}
	c.model = r.NewModel
	c.provider = r.TargetProvider
	c.requestedProvider = r.TargetProvider
	c.explicitAPIKey = r.APIKey
	c.explicitBaseURL = r.BaseURL
	if r.APIKey != "" {
		c.apiKey = r.APIKey
	}
	if r.BaseURL != "" {
		c.baseURL = r.BaseURL
	}
	if r.APIMode != "" {
		c.apiMode = r.APIMode
	}
	if !silent {
		cprint("  " + c.t("cli.session.model_reset_default", map[string]any{"model": r.NewModel}))
	}
}

// Sanitizes and stores a /new title, the stored title or false when the session stays untitled
func applyNewSessionTitle(c *CLI, title string) (string, bool) {
	sanitized, err := state.SanitizeTitle(title)
	if err != nil {
		cprint("  " + c.t("cli.session.title_rejected", map[string]any{"error": err.Error()}))
		return "", false
	}
	if sanitized == "" {
		cprint("  " + c.t("cli.session.title_empty_untitled", nil))
		return "", false
	}
	if _, err := c.sessionDB.SetSessionTitle(c.ctx, c.sessionID, sanitized); err != nil {
		if isValueError(err) {
			cprint("  " + c.t("cli.session.title_error_untitled", map[string]any{"error": err.Error()}))
		}
		return "", false
	}
	c.pendingTitle = ""
	c.statusBarTitleCheckedAt = 0
	return sanitized, true
}

// Writes a /save export on this machine, the typed filename against the cwd or a stamped file under the home
func writeSave(c *CLI, sessionID, format, filename string, render func() (string, error)) {
	savedDir := filepath.Join(home.Dir(c.ctx), "sessions", "saved")
	if err := os.MkdirAll(savedDir, 0o755); err != nil {
		printLines(c.t("cli.session.save_dir_failed", map[string]any{"path": savedDir, "error": pypath.OSErrorText(err)}))
		return
	}
	var path string
	if filename != "" {
		path = pypath.Expanduser(filename)
		if !filepath.IsAbs(path) {
			cwd, _ := os.Getwd()
			path = filepath.Join(cwd, path)
		}
	} else {
		path = filepath.Join(savedDir, "icebath_conversation_"+pydatetime.Strftime(time.Now(), "%Y%m%d_%H%M%S")+"."+format)
	}
	content, err := render()
	if err == nil {
		err = os.WriteFile(path, []byte(content), 0o644)
	}
	if err != nil {
		printLines(c.t("cli.session.save_failed", map[string]any{"error": pypath.OSErrorText(err)}))
		return
	}
	label := map[string]string{"json": "JSON", "md": c.t("cli.session.format_markdown", nil), "html": "HTML"}[format]
	printLines(c.t("cli.session.save_ok", map[string]any{"path": path, "label": label}))
	if sessionID != "" {
		printLines("       " + c.t("cli.session.save_resume_hint", map[string]any{"session_id": sessionID}))
	}
}

// Prints the gateway style status block of the current CLI session
func (c *CLI) showSessionStatus() {
	var meta map[string]any
	if c.sessionDB != nil {
		func() {
			defer func() { _ = recover() }()
			meta = c.sessionMeta(c.sessionID)
		}()
	}
	start := c.sessionStart
	in := statusFieldsInput{ctx: c.ctx, sessionID: c.sessionID, meta: meta, model: c.model, provider: c.provider,
		createdFallback: &start, agentRunning: c.agentRunning}
	reasoning := c.reasoningConfig
	if c.agentRef() != nil {
		in.agentModel, in.agentProvider = c.agentRef().Model(), c.agentRef().Provider()
		in.agentTokens = c.agentRef().StatusBarUsage(c.ctx).Counters["session_total_tokens"]
		if rc := c.agentRef().ReasoningConfig(); len(rc) > 0 {
			reasoning = rc
		}
	}
	fields := buildStatusFields(in)
	reasoningLabel := ""
	if reasoning != nil {
		if enabled, ok := reasoning["enabled"].(bool); ok && !enabled {
			reasoningLabel = c.t("cli.shared.label_off", nil)
		} else if cliTruthy(reasoning["effort"]) {
			reasoningLabel = pyrepr.Str(reasoning["effort"])
		}
	}
	if reasoningLabel != "" {
		stateLabel := c.t("cli.shared.label_off", nil)
		if c.showReasoning {
			stateLabel = c.t("cli.shared.label_on", nil)
		}
		reasoningLabel += c.t("cli.session.reasoning_display_suffix", map[string]any{"state": stateLabel})
	}
	approvalLabel := tools.GetApprovalMode(c.env)
	if tools.IsApprovalBypassActiveForSession(c.env, "") {
		approvalLabel += c.t("cli.session.yolo_bypass_suffix", nil)
	}
	ctxLabel := ""
	func() {
		defer func() {
			if r := recover(); r != nil {
				ctxLabel = ""
			}
		}()
		snap := c.getStatusBarSnapshot()
		ctxMax, _ := pyrepr.Int(snap["context_length"])
		if ctxMax == nil || ctxMax.Sign() == 0 {
			return
		}
		left := ""
		switch pct := snap["context_percent"].(type) {
		case int:
			left = c.t("cli.session.ctx_left_prefix", map[string]any{"percent": max(0, 100-pct)})
		case float64:
			left = c.t("cli.session.ctx_left_prefix", map[string]any{"percent": max(0, 100-int(pct))})
		}
		used := int64(0)
		if n, ok := pyrepr.Int(snap["context_tokens"]); ok && n != nil {
			used = n.Int64()
		}
		ctxLabel = c.t("cli.session.ctx_tokens_used", map[string]any{"left": left, "used": pystr.GroupThousands(used),
			"max": pystr.GroupThousands(ctxMax.Int64())})
	}()
	lines := append([]string{c.t("cli.session.status_title", nil), ""}, statusLines(fields, "session_id", "path", "title", "model")...)
	for _, row := range [][2]string{{c.t("cli.session.label_reasoning", nil), reasoningLabel},
		{c.t("cli.session.label_approvals", nil), approvalLabel}, {c.t("cli.session.label_context", nil), ctxLabel}} {
		if row[1] != "" {
			lines = append(lines, c.t("cli.shared.label_value", map[string]any{"label": row[0], "value": row[1]}))
		}
	}
	lines = append(lines, statusLines(fields, "created", "last_activity", "tokens", "agent_running")...)
	c.consolePrint(pyrich.Escape(strings.Join(lines, "\n")))
}

// Prints the conversation so far, user and assistant turns with tool rows counted between them
func (c *CLI) showHistory() {
	if len(c.conversationHistory) == 0 {
		if !c.showRecentSessions("history", 10) {
			cliVisiblePrint(c.t("cli.session.history_empty", nil))
		}
		return
	}
	const previewLimit = 400
	visibleIndex, hiddenToolMessages := 0, 0
	tsSuffix := func(message map[string]any) string {
		if !c.showTimestamps || !cliTruthy(message["timestamp"]) {
			return ""
		}
		stamp := timestampOr(message["timestamp"], nil)
		if stamp == nil {
			return ""
		}
		format := c.timestampFormat
		if format == "" {
			format = "%H:%M"
		}
		return "  [" + pydatetime.Strftime(*stamp, format) + "]"
	}
	flushToolSummary := func() {
		if hiddenToolMessages == 0 {
			return
		}
		key := "cli.session.history_tools_hidden_other"
		if hiddenToolMessages == 1 {
			key = "cli.session.history_tools_hidden_one"
		}
		cliVisiblePrint("\n  " + c.t("cli.session.history_tools_header", nil))
		cliVisiblePrint("    " + c.t(key, map[string]any{"count": hiddenToolMessages}))
		hiddenToolMessages = 0
	}
	rule := "+" + strings.Repeat("-", 50) + "+"
	for _, line := range []string{"", rule, "|" + pystr.Center(c.t("cli.session.history_title", nil), 50) + "|", rule} {
		cliVisiblePrint(line)
	}
	for _, msg := range c.conversationHistory {
		role := cliStr(mapValueOr(msg, "role", "unknown"), "unknown")
		if role == "tool" {
			hiddenToolMessages++
			continue
		}
		if role != "user" && role != "assistant" {
			continue
		}
		flushToolSummary()
		visibleIndex++
		contentText := ""
		if content, ok := msg["content"]; ok && content != nil {
			contentText = pyrepr.Str(content)
		}
		preview := pystr.Slice(contentText, 0, previewLimit)
		suffix := ""
		if pystr.Len(contentText) > previewLimit {
			suffix = "..."
		}
		if role == "user" {
			cliVisiblePrint("\n  " + c.t("cli.session.history_you_header", map[string]any{"index": visibleIndex}) + tsSuffix(msg))
			cliVisiblePrint("    " + preview + suffix)
			continue
		}
		cliVisiblePrint("\n  " + c.t("cli.session.history_assistant_header", map[string]any{"agent_name": "Hermes", "index": visibleIndex}) + tsSuffix(msg))
		nCalls := 0
		if calls, ok := msg["tool_calls"].([]any); ok {
			nCalls = len(calls)
		} else if calls, ok := msg["tool_calls"].([]map[string]any); ok {
			nCalls = len(calls)
		}
		if contentText == "" {
			suffix = ""
			preview = c.t("cli.session.history_no_text", nil)
			if nCalls > 0 {
				key := "cli.session.history_tool_calls_other"
				if nCalls == 1 {
					key = "cli.session.history_tool_calls_one"
				}
				preview = c.t(key, map[string]any{"count": nCalls})
			}
		}
		cliVisiblePrint("    " + preview + suffix)
	}
	flushToolSummary()
	cliVisiblePrint("")
}

// Fires the on_session_finalize or on_session_reset boundary hook, never failing
func (c *CLI) notifySessionBoundary(eventType string) {
	defer func() { _ = recover() }()
	var sessionID any
	if c.agentRef() != nil {
		sessionID = c.agentRef().SessionID()
	}
	reason := "session_boundary"
	if eventType == "on_session_reset" {
		reason = "new_session"
	}
	kwargs := map[string]any{"session_id": sessionID, "platform": "cli", "reason": reason}
	if eventType == "on_session_finalize" {
		for _, message := range agent.SessionEndMessages(agent.FinalizeSession(c.ctx, kwargs)) {
			cliVisiblePrint(message)
		}
		return
	}
	agent.LifecycleInvokeHook(c.ctx, eventType, kwargs)
}

// Starts a fresh gateway session with cleared agent state, the old one ended first so the gateway finalizes it
func (c *CLI) newSession(silent bool, title string) {
	oldSessionID := c.sessionID
	if c.sessionDB != nil && oldSessionID != "" {
		if g := agentGateway(c.agentRef()); g != nil {
			g.flushMessagesToSessionDB()
		}
		if err := c.sessionDB.EndSession(c.ctx, oldSessionID, "new_session"); err != nil {
			cliLogger.Debug(fmt.Sprintf("end_session %s failed: %v", oldSessionID, err))
		}
		c.discardSessionIfEmpty(oldSessionID)
	}
	c.sessionStart = time.Now()
	sid, err := c.newSessionID(c.sessionStart, "", "")
	if err != nil {
		panic(err)
	}
	c.uiMu.Lock()
	c.sessionID = sid
	c.uiMu.Unlock()
	c.writeTerminalBreadcrumb()
	c.conversationHistory = []map[string]any{}
	c.pendingTitle = ""
	c.resumed = false
	c.explicitModelOverride = false
	c.pendingOneTurnModelRestore = nil
	c.serviceTier = parseServiceTierConfig(cliStr(cliMap(c.config, "agent")["service_tier"], ""))
	resetModelToConfigDefault(c, silent)
	c.resolveCLIReasoning()
	syncProcessSessionID(c.ctx, c.env, c.sessionID)
	shownTitle := title
	if c.agentRef() != nil {
		c.agentRef().SetSessionID(c.sessionID)
		c.agentRef().SetReasoningConfig(c.reasoningConfig)
		c.agentRef().ResetSessionState()
		c.agentRef().setTodoStore(tools.NewTodoStore())
		if c.sessionDB != nil && title != "" {
			shownTitle, _ = applyNewSessionTitle(c, title)
		}
	}
	if !silent {
		if shownTitle != "" {
			printLines(c.t("cli.session.new_session_titled", map[string]any{"title": shownTitle}))
		} else {
			printLines(c.t("cli.session.new_session", nil))
		}
	}
}

// Handles /save with a format, optional filename and optional redact flag
func (c *CLI) saveConversation(cmd string) {
	parts := pystr.SplitWhitespace(cmd, -1)
	if len(parts) > 0 {
		parts = parts[1:]
	}
	redact := len(parts) > 0 && (pystr.Lower(parts[len(parts)-1]) == "redact" || pystr.Lower(parts[len(parts)-1]) == "--redact")
	if redact {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		printLines(state.SaveUsage)
		return
	}
	format, err := state.NormalizeSaveFormat(parts[0])
	if err != nil {
		printLines("(._.) "+err.Error(), state.SaveUsage)
		return
	}
	filename := ""
	if len(parts) > 1 {
		filename = parts[1]
	}
	if store := c.gatewayStore(); store != nil {
		if len(c.conversationHistory) == 0 {
			printLines(c.t("cli.session.save_nothing", nil))
			return
		}
		if content, ok := c.exportOnGateway(store.Connection, format, filename, redact); ok {
			writeSave(c, c.sessionID, format, filename, func() (string, error) { return content, nil })
		}
		return
	}
	var sessionData *pyjson.Object
	if sessionData == nil || sessionData.Len() == 0 {
		if len(c.conversationHistory) == 0 {
			printLines(c.t("cli.session.save_nothing", nil))
			return
		}
		messages := make([]any, len(c.conversationHistory))
		for i, m := range c.conversationHistory {
			messages[i] = m
		}
		sessionData = pyjson.NewObject()
		sessionData.Set("id", c.sessionID)
		sessionData.Set("model", c.model)
		sessionData.Set("started_at", float64(c.sessionStart.UnixNano())/1e9)
		sessionData.Set("messages", messages)
	}
	if redact {
		sessionData = memory.RedactSessionData(sessionData)
	}
	writeSave(c, c.sessionID, format, filename, func() (string, error) { return state.RenderSessionForSave(sessionData, format) })
}

// Prints a gateway save refusal, not_found reading as nothing to save
func printSaveRefusal(c *CLI, err error) {
	if err.Error() == "not_found" {
		printLines(c.t("cli.session.save_nothing", nil))
		return
	}
	printLines(c.t("cli.session.save_failed", map[string]any{"error": err.Error()}))
}

// Owner's rendering of its stored row for save, false when refused or handed to an older gateway's save
func (c *CLI) exportOnGateway(connection *gatewaycli.GatewayConnection, format, filename string, redact bool) (string, bool) {
	raw, err := connection.Call(c.ctx, "session.export", map[string]any{"session_id": c.sessionID, "format": format, "redact": redact})
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		var refused *gateway.GatewayRPCError
		if !(errors.As(err, &refused) && ((refused.Code != nil && *refused.Code == -32601) || refused.Error() == "unknown_method")) {
			printSaveRefusal(c, err)
			return "", false
		}
		c.saveOnOlderGateway(connection, format, filename, redact)
		return "", false
	}
	reply, _ := pyjson.Plain(raw).(map[string]any)
	content, isStr := reply["content"].(string)
	if !isStr {
		printLines(c.t("cli.session.save_failed", map[string]any{"error": "the gateway returned no export"}))
		return "", false
	}
	return content, true
}

// Plain json save on an owner without session.export, written on the gateway host
func (c *CLI) saveOnOlderGateway(connection *gatewaycli.GatewayConnection, format, filename string, redact bool) {
	if redact || format != "json" || filename != "" {
		printLines("(._.) This gateway predates this CLI: only a plain /save json works until it restarts " +
			"(it saves on the gateway host).")
		return
	}
	raw, err := connection.Call(c.ctx, "session.save", map[string]any{"session_id": c.sessionID})
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		printSaveRefusal(c, err)
		return
	}
	reply, _ := pyjson.Plain(raw).(map[string]any)
	if saved := strOrEmpty(reply["file"]); saved != "" {
		printLines("(^_^)v Conversation saved on the gateway host: " + saved)
		return
	}
	printLines(c.t("cli.session.save_failed", map[string]any{"error": "the gateway returned no file"}))
}

// Installs a rewound history and mirrors it onto the agent's transcript
func (c *CLI) publishTruncatedHistory(truncated []map[string]any) {
	c.conversationHistory = truncated
	if c.agentRef() == nil {
		return
	}
	c.agentRef().setSessionMessages(c.conversationHistory)
}

// Drops the last exchange and returns the user text to send again, false when nothing can be retried
func (c *CLI) retryLast() (string, bool) {
	if len(c.conversationHistory) == 0 {
		printLines(c.t("cli.session.retry_no_messages", nil))
		return "", false
	}
	warmHistory := append([]map[string]any(nil), c.conversationHistory...)
	userIndices := userTurnIndices(warmHistory)
	if len(userIndices) == 0 {
		printLines(c.t("cli.session.retry_no_user_message", nil))
		return "", false
	}
	truncated, liveView, err := agent.HistoryBeforeUserOriginatedTurn(warmHistory, userIndices[len(userIndices)-1])
	var lastMessage string
	if err == nil {
		liveContent := liveView["content"]
		if s, ok := liveContent.(string); ok {
			liveContent = pystr.Strip(state.SanitizeContext(s))
		}
		lastMessage, err = agent.RetryableUserText(liveContent)
	}
	if err != nil {
		printLines(c.t("cli.session.retry_unsafe", map[string]any{"error": err.Error()}))
		return "", false
	}
	if c.sessionDB != nil && c.sessionID != "" {
		outcome, err := c.sessionDB.RewindUserTurn(c.ctx, c.sessionID, -1, state.RewindUserTurnOptions{WarmHistory: warmHistory,
			HasWarmHistory: true, RequireRetryable: true})
		if err != nil {
			printLines(c.t("cli.session.retry_rewind_failed", map[string]any{"error": err.Error()}))
			return "", false
		}
		truncated = outcome.Prefix
	}
	c.publishTruncatedHistory(truncated)
	preview := pystr.Slice(lastMessage, 0, 60)
	if pystr.Len(lastMessage) > 60 {
		preview += "..."
	}
	printLines(c.t("cli.session.retrying", map[string]any{"preview": preview}))
	return lastMessage, true
}

// Backs up n user turns, rewinding the store and prefilling the composer, the count undone or zero
func (c *CLI) undoLast(n int, prefill bool) int {
	if len(c.conversationHistory) == 0 {
		printLines(c.t("cli.session.undo_no_messages", nil))
		return 0
	}
	n = max(n, 1)
	warmHistory := append([]map[string]any(nil), c.conversationHistory...)
	userIndices := userTurnIndices(warmHistory)
	if len(userIndices) == 0 {
		printLines(c.t("cli.session.undo_no_user_message", nil))
		return 0
	}
	turnsUndone := min(n, len(userIndices))
	targetOrdinal := len(userIndices) - turnsUndone
	cutIdx := userIndices[targetOrdinal]
	removedCount := len(warmHistory) - cutIdx
	truncated, liveView, err := agent.HistoryBeforeUserOriginatedTurn(warmHistory, cutIdx)
	if err != nil {
		panic(err)
	}
	removedText := undoContentToText(liveView["content"])
	rewoundRows := 0
	if c.sessionDB != nil && c.sessionID != "" {
		outcome, err := c.sessionDB.RewindUserTurn(c.ctx, c.sessionID, targetOrdinal, state.RewindUserTurnOptions{WarmHistory: warmHistory, HasWarmHistory: true})
		if err != nil {
			cliLogger.Debug(fmt.Sprintf("undo: durable rewind failed: %v", err))
			printLines(c.t("cli.session.undo_failed", map[string]any{"error": err.Error()}))
			return 0
		}
		truncated = outcome.Prefix
		if outcome.LiveText != "" {
			removedText = outcome.LiveText
		}
		rewoundRows = outcome.RewoundCount
	}
	c.publishTruncatedHistory(truncated)
	key := "cli.session.undo_ok_other"
	if turnsUndone == 1 {
		key = "cli.session.undo_ok_one"
	}
	messages := rewoundRows
	if messages == 0 {
		messages = removedCount
	}
	backup := pystr.Slice(removedText, 0, 60)
	if pystr.Len(removedText) > 60 {
		backup += "..."
	}
	printLines(c.t(key, map[string]any{"count": turnsUndone, "messages": messages, "backup": backup}))
	printLines("  " + c.t("cli.session.undo_remaining", map[string]any{"count": len(c.conversationHistory)}))
	if prefill && removedText != "" {
		c.prefillInputBuffer(removedText)
	}
	return turnsUndone
}

// Flattens message content, a string or content part list, to plain text
func undoContentToText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, p := range v {
			if part, ok := p.(map[string]any); ok && part["type"] == "text" {
				if text := cliStr(part["text"], ""); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	case []map[string]any:
		var parts []string
		for _, part := range v {
			if part["type"] == "text" {
				if text := cliStr(part["text"], ""); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// Toggles the per session approval bypass, refusing when the process start bypass is frozen on
func (c *CLI) toggleYolo() {
	if tools.YoloModeFrozen() {
		stateText := boldRedANSI + c.t("cli.session.yolo_state_locked_on", nil) + resetANSI
		cprint("  " + c.t("cli.session.yolo_locked", map[string]any{"state": stateText}))
		return
	}
	sessionKey := c.sessionID
	if sessionKey == "" {
		sessionKey = "default"
	}
	var persist func(bool) error
	if c.sessionDB != nil && sessionKey != "default" {
		db := c.sessionDB
		persist = func(on bool) error {
			return db.SetSessionYolo(c.ctx, sessionKey, on)
		}
	}
	if tools.ToggleSessionYolo(sessionKey, nil, false, persist) {
		stateText := boldGreenANSI + c.t("cli.shared.label_on_upper", nil) + resetANSI
		cprint("  " + c.t("cli.session.yolo_on", map[string]any{"state": stateText}))
		return
	}
	stateText := boldRedANSI + c.t("cli.shared.label_off_upper", nil) + resetANSI
	cprint("  " + c.t("cli.session.yolo_off", map[string]any{"state": stateText}))
}

// Runs /compress, whole history or the boundary aware here form, with a preview that changes nothing
func (c *CLI) manualCompress(cmdOriginal string) {
	if len(c.conversationHistory) < agent.MinCompressMessages {
		printLines(c.t("cli.session.compress_too_short", map[string]any{"min": agent.MinCompressMessages}))
		return
	}
	if c.agentRef() == nil {
		printLines(c.t("cli.session.compress_no_agent", nil))
		return
	}
	raw := ""
	if parts := pystr.SplitWhitespace(pystr.Strip(cmdOriginal), 1); len(parts) > 1 {
		raw = parts[1]
	}
	request := agent.ParseCompressArgs(raw)
	if request.Aggressive {
		printLines("(._.) " + agent.AggressiveUnsupported)
		if !request.Preview {
			return
		}
	}
	if request.Preview {
		result, err := compressNowOnGateway(c.ctx, agentGateway(c.agentRef()), c.conversationHistory, request)
		if err != nil {
			panic(err)
		}
		for _, line := range agent.RenderCompressResult(result, "") {
			printLines("🗜️  " + line)
		}
		return
	}
	originalCount := len(c.conversationHistory)
	_ = c.busyCommand(c.t("cli.session.compress_busy", nil), false, func() {
		defer func() {
			if r := recover(); r != nil {
				printLines("  " + c.t("cli.session.compress_failed", map[string]any{"error": refusalText(c.ctx, r)}))
			}
		}()
		switch {
		case request.Partial:
			printLines(c.t("cli.session.compress_summarizing", map[string]any{"count": originalCount, "keep_last": request.KeepLast}))
		case request.FocusTopic != nil:
			printLines(c.t("cli.session.compress_focus", map[string]any{"count": originalCount, "focus": *request.FocusTopic}))
		default:
			printLines(c.t("cli.session.compress_plain", map[string]any{"count": originalCount}))
		}
		result, err := compressNowOnGateway(c.ctx, agentGateway(c.agentRef()), c.conversationHistory, request)
		if err != nil {
			panic(err)
		}
		if result.Status != "compressed" {
			for _, line := range agent.RenderCompressResult(result, "") {
				printLines("  " + line)
			}
			return
		}
		c.conversationHistory = result.AfterMessages
		if agentSID := c.agentRef().SessionID(); agentSID != "" && agentSID != c.sessionID {
			tools.TransferSessionYolo(c.sessionID, agentSID)
			c.sessionID = agentSID
			c.writeTerminalBreadcrumb()
			c.pendingTitle = ""
			if g := agentGateway(c.agentRef()); g != nil {
				g.flushMessagesToSessionDB()
			}
		}
		summary := result.Summary
		icon := "✅"
		switch {
		case summary.Aborted || summary.FallbackUsed || summary.RefusedWouldGrow:
			icon = "⚠️"
		case summary.Noop:
			icon = "🗜️"
		}
		printLines("  "+icon+" "+summary.Headline, "     "+summary.TokenLine)
		if summary.Note != nil && *summary.Note != "" {
			printLines("     " + *summary.Note)
		}
	})
}

// Parses an undo count token as Python int would, false when it is not an integer
func parsePyInt(token string) (int, bool) {
	n, err := strconv.Atoi(pystr.Strip(strings.ReplaceAll(token, "_", "")))
	if err != nil || strings.HasPrefix(token, "_") || strings.HasSuffix(token, "_") || strings.Contains(token, "__") {
		return 0, false
	}
	return n, true
}

// Notifies plugins of a command the CLI runs, the in process pre_command hook
func firePreCommand(c *CLI, canonical, aliasUsed, argsRaw string) {
	sessionKey := c.sessionID
	platform := "cli"
	plugins.FirePreCommandHook(c.ctx, "cli", canonical, aliasUsed, argsRaw, &sessionKey, &platform)
}
