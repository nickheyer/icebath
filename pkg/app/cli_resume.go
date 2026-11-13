package app

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Sources the in chat session listing never shows
var internalListingSources = []string{"kanban", "tool", "oneshot"}

// Text after the slash command word, stripped and lowered when asked
func commandArg(cmd string, lower bool) string {
	parts := pystr.SplitWhitespace(pystr.Strip(cmd), 1)
	arg := ""
	if len(parts) > 1 {
		arg = pystr.Strip(parts[1])
	}
	if lower {
		return pystr.Lower(arg)
	}
	return arg
}

// Plural aware catalog text of a command handler, the one form when count is one
func (c *CLI) commandsTN(key string, count int, kwargs map[string]any) string {
	args := map[string]any{"count": count}
	for k, v := range kwargs {
		args[k] = v
	}
	suffix := "_other"
	if count == 1 {
		suffix = "_one"
	}
	return c.commandsT(key+suffix, args)
}

// Gateway catalog text the CLI shares verbatim with the messaging gateway
func (c *CLI) gatewayT(key string, kwargs map[string]any) string { return c.t("gateway."+key, kwargs) }

// Each line of a catalog value prefixed with two spaces, blank lines kept blank
func catalogLines(text string) []string {
	var out []string
	for _, line := range pystr.Splitlines(text, false) {
		if line == "" {
			out = append(out, "")
			continue
		}
		out = append(out, "  "+line)
	}
	return out
}

// Indented notice that the session database cannot be opened
func (c *CLI) dbUnavailableLine() string {
	return "  " + state.FormatSessionDBUnavailable(c.ctx, state.SessionDBUnavailablePrefix, true)
}

// Resume recap panel rendered at the current terminal width for resize replay
func (c *CLI) renderResumeHistoryPanelLines(panel pyrich.Panel) []string {
	var buf strings.Builder
	columns, _ := pyshutil.GetTerminalSize(80, 24)
	console := pyrich.NewConsole(io.Writer(&buf))
	console.Color, console.Terminal, console.ColorSystem, console.NoHighlight, console.Width = true, true, "truecolor", true, columns
	func() {
		defer suspendOutputHistory()()
		console.PrintPanel(panel)
	}()
	return pystr.Splitlines(strings.TrimRight(buf.String(), "\n"), false)
}

// Recent CLI sessions for the in chat browse and resume affordances, empty without a store
func (c *CLI) listRecentSessions(limit int) (rows []map[string]any) {
	if c.sessionDB == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			rows = nil
		}
	}()
	objs, err := state.QuerySessionListing(c.ctx, c.sessionDB, state.SessionListingQuery{Source: "cli", CurrentSessionID: c.sessionID,
		IncludeUnnamed: true, Limit: limit, ExcludeSources: internalListingSources})
	if err != nil {
		return nil
	}
	for _, o := range objs {
		if m, ok := pyjson.Plain(o).(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	return rows
}

// Prints the recent sessions table inline, false when no listing was available
func (c *CLI) showRecentSessions(reason string, limit int) bool {
	sessions := c.listRecentSessions(limit)
	if len(sessions) == 0 {
		return false
	}
	cliVisiblePrint("")
	if reason == "history" {
		cliVisiblePrint(c.t("cli.session.recent_intro", nil))
	} else {
		cliVisiblePrint("  " + c.t("cli.session.recent_header", nil))
	}
	cliVisiblePrint("")
	cliVisiblePrint(fmt.Sprintf("  %s %s %s %s %s", padRight("#", 3), padRight(c.t("cli.session.column_title", nil), 32),
		padRight(c.t("cli.session.column_preview", nil), 40), padRight(c.t("cli.session.column_last_active", nil), 13), c.t("cli.session.column_id", nil)))
	cliVisiblePrint(fmt.Sprintf("  %s %s %s %s %s", strings.Repeat("─", 3), strings.Repeat("─", 32), strings.Repeat("─", 40),
		strings.Repeat("─", 13), strings.Repeat("─", 24)))
	for idx, session := range sessions {
		title := cliStr(session["title"], "")
		if title == "" {
			title = "—"
		}
		preview := pystr.Slice(cliStr(session["preview"], ""), 0, 38)
		id := cliStr(session["id"], "")
		lastActive := pydatetime.RelativeTime(session["last_active"], id, time.Now())
		cliVisiblePrint(fmt.Sprintf("  %s %s %s %s %s", padRight(fmt.Sprint(idx+1), 3), padRight(title, 32), padRight(preview, 40),
			padRight(lastActive, 13), id))
	}
	cliVisiblePrint("")
	cliVisiblePrint("  " + c.t("cli.session.resume_usage", nil))
	cliVisiblePrint("  " + c.t("cli.session.resume_example", nil))
	cliVisiblePrint("")
	return true
}

// Session title or id resolved to an id on its live compression tip, empty when neither matches
func (c *CLI) resolveSessionByNameOrID(nameOrID string) string {
	reader := c.sessionDB.Reader(c.ctx)
	if reader == nil {
		return ""
	}
	db := reader.DB()
	resolved := ""
	if session, err := db.GetSession(c.ctx, nameOrID); err == nil && session != nil {
		resolved = cliStr(session.GetOr("id", ""), "")
	} else if id, found, err := db.ResolveSessionByTitle(c.ctx, nameOrID); err == nil && found {
		resolved = id
	}
	if resolved == "" && strings.HasPrefix(nameOrID, "local-") {
		if id, err := db.ResolveSessionID(nameOrID); err == nil {
			resolved = id
		}
	}
	if resolved != "" {
		if tip, err := db.GetCompressionTip(c.ctx, resolved); err == nil && tip != "" {
			resolved = tip
		}
	}
	return resolved
}

// Session id and row a resume target names, false after printing why it did not resolve
func (c *CLI) resolveResumeTarget(target string) (string, map[string]any, bool) {
	var targetID string
	if pystr.StrIsDigit(target) {
		sessions := c.listRecentSessions(10)
		n, err := pyint.Of(target)
		if err != nil {
			panic(err)
		}
		if n.Sign() < 1 || !n.IsInt64() || n.Int64() > int64(len(sessions)) {
			cp(catalogLines(c.gatewayT("resume.out_of_range", map[string]any{"index": n}))...)
			return "", nil, false
		}
		index := int(n.Int64())
		targetID = cliStr(sessions[index-1]["id"], "")
	} else {
		targetID = c.resolveSessionByNameOrID(target)
		if targetID == "" {
			targetID = target
		}
	}
	sessionMeta := c.sessionMeta(targetID)
	if len(sessionMeta) == 0 {
		cp("  "+c.commandsT("resume.not_found", map[string]any{"target": target}), "  "+c.commandsT("resume.not_found_hint", nil))
		return "", nil, false
	}
	resolvedID, err := c.sessionDB.ResolveResumeSessionID(c.ctx, targetID)
	if err != nil {
		resolvedID = targetID
	}
	if resolvedID != "" && resolvedID != targetID {
		cp("  " + c.commandsT("resume.compressed_redirect", map[string]any{"session_id": targetID, "resolved_id": resolvedID}))
		targetID = resolvedID
		if meta := c.sessionMeta(targetID); len(meta) > 0 {
			sessionMeta = meta
		}
	}
	return targetID, sessionMeta, true
}

// Ends the current gateway session with reason, the switch going ahead when the end fails
func (c *CLI) endCurrentSession(reason string) {
	if g := agentGateway(c.agentRef()); g != nil {
		g.flushMessagesToSessionDB()
	}
	if err := c.sessionDB.EndSession(c.ctx, c.sessionID, reason); err != nil {
		cliLogger.Debug(fmt.Sprintf("end_session %s failed: %v", c.sessionID, err))
	}
}

// Points a built agent at the switched session with fresh per session figures and todo mirror
func (c *CLI) syncAgentToSession(sessionID string) {
	if c.agentRef() == nil {
		return
	}
	c.agentRef().SetSessionID(sessionID)
	c.agentRef().ResetSessionState()
	c.agentRef().setTodoStore(tools.NewTodoStore())
}

// Switches to a previous session by number, title or id mid conversation
func (c *CLI) handleResumeCommand(cmdOriginal string) {
	if c.agentRunning {
		cp("  " + c.commandsT("shared.agent_busy", map[string]any{"command": "/resume"}))
		return
	}
	target := commandArg(cmdOriginal, false)
	if r := []rune(target); len(r) >= 2 {
		switch string(r[0]) + string(r[len(r)-1]) {
		case "<>", "[]", `""`, "''":
			target = pystr.Strip(string(r[1 : len(r)-1]))
		}
	}
	if target == "" {
		cp("  " + c.commandsT("resume.usage", nil))
		if c.showRecentSessions("resume", 10) {
			c.pendingResumeSessions = c.listRecentSessions(10)
			return
		}
		cp("  " + c.commandsT("resume.tip_find_sessions", nil))
		return
	}
	c.pendingResumeSessions = nil
	if c.sessionDB == nil {
		cp(c.dbUnavailableLine())
		return
	}
	targetID, sessionMeta, ok := c.resolveResumeTarget(target)
	if !ok {
		return
	}
	if refusal := kanban.KanbanResumeRefusal(c.ctx, c.sessionDB, targetID); refusal != "" {
		cp("  " + refusal)
		return
	}
	readID := targetID
	targetID, refusal := gatewayOpen(c.ctx, c.sessionDB, targetID)
	if refusal != "" {
		cp(fmt.Sprintf("  %s %s (%s)", c.gatewayT("resume.switch_failed", nil), targetID, refusal))
		return
	}
	if targetID == c.sessionID {
		cp("  " + c.commandsT("resume.already_on", nil))
		return
	}
	c.endCurrentSession("resumed_other")
	c.sessionID, c.resumed, c.pendingTitle = targetID, true, ""
	syncProcessSessionID(c.ctx, c.env, targetID)
	modelHistory, displayHistory, err := c.sessionDB.GetResumeConversations(c.ctx, readID)
	if err != nil {
		panic(err)
	}
	c.conversationHistory = withoutSessionMeta(modelHistory)
	c.resumeDisplayHistory = withoutSessionMeta(displayHistory)
	c.sessionDB.ReopenSession(c.ctx, targetID)
	c.syncAgentToSession(targetID)
	titlePart := ""
	if title := cliStr(sessionMeta["title"], ""); title != "" {
		titlePart = ` "` + title + `"`
	}
	msgCount := 0
	for _, m := range c.resumeDisplayHistory {
		if agent.IsUserOriginatedTurn(m) {
			msgCount++
		}
	}
	if len(c.conversationHistory) > 0 {
		cp("  " + c.commandsTN("resume.resumed", msgCount, map[string]any{"session_id": targetID, "title": titlePart, "total": len(c.conversationHistory)}))
		c.displayResumedHistory()
	} else {
		cp("  " + c.commandsT("resume.resumed_empty", map[string]any{"session_id": targetID, "title": titlePart}))
	}
	c.restoreSessionCwd(sessionMeta, false)
	c.restoreSessionYolo(sessionMeta, false)
	c.restoreSessionModel(sessionMeta, false)
}

// Prints the recent sessions table, or delegates an explicit target to resume
func (c *CLI) handleSessionsCommand(cmdOriginal string) {
	arg := commandArg(cmdOriginal, false)
	switch {
	case arg != "" && !map[string]bool{"list": true, "ls": true, "browse": true}[pystr.Lower(arg)]:
		c.handleResumeCommand("/resume " + arg)
	case c.sessionDB == nil:
		cp(c.dbUnavailableLine())
	case !c.showRecentSessions("sessions", 10):
		cp("  " + c.commandsT("sessions.none_yet", nil))
	}
}
