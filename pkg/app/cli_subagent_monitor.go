package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
)

// Terminal cells of a string, a negative width rune counting as zero
func cellWidth(s string) int {
	w := 0
	for _, r := range s {
		w += max(0, pyunicode.WcWidth(r))
	}
	return w
}

// Whitespace collapsed printable text cut to width cells with an ellipsis when it was cut
func dockClip(value any, width int) string {
	text := strings.Join(pystr.SplitWhitespace(pyrepr.Str(pyrepr.Or(value, "")), -1), " ")
	var printable strings.Builder
	for _, r := range text {
		if pystr.IsPrintable(r) {
			printable.WriteRune(r)
		}
	}
	text = printable.String()
	if cellWidth(text) <= width {
		return text
	}
	result := ""
	for _, r := range text {
		if cellWidth(result+string(r)) > max(0, width-1) {
			break
		}
		result += string(r)
	}
	if width != 0 {
		return result + "…"
	}
	return result
}

// Python floor division of two ints
func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// Live work dock state of the classic CLI, subagents, processes, goal, loop and queued prompts
type subagentMonitor struct {
	cli        *CLI
	mu         sync.Mutex
	entries    []map[string]any
	processes  []map[string]any
	goal       string
	loop       string
	loopBadge  string
	loopPrompt string
	queued     []string
	selectedID any
	signature  *string
	lastPoll   float64
	app        *monitorApp
	opening    bool
	collapsed  bool
	detail     bool
	local      *localSubagents
}

// Monitor over the CLI with nothing listed yet
func newSubagentMonitor(c *CLI) *subagentMonitor { return &subagentMonitor{cli: c} }

// Whether any dock block has something to show
func (m *subagentMonitor) hasRows() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasRowsLocked()
}

func (m *subagentMonitor) hasRowsLocked() bool {
	return len(m.entries) > 0 || len(m.processes) > 0 || m.goal != "" || m.loop != "" || len(m.queued) > 0
}

// Agents first then processes, the order the dock and the monitor paint
func (m *subagentMonitor) rosterLocked() []map[string]any {
	out := make([]map[string]any, 0, len(m.entries)+len(m.processes))
	out = append(out, m.entries...)
	return append(out, m.processes...)
}

// Row identity, its key or else its subagent id
func dockRowKey(row map[string]any) any {
	if k, ok := row["key"]; ok {
		return k
	}
	return row["subagent_id"]
}

// Roster row whose key is the selection, nil when it left
func (m *subagentMonitor) selectedLocked() map[string]any {
	for _, r := range m.rosterLocked() {
		if dockRowKey(r) == m.selectedID {
			return r
		}
	}
	return nil
}

// Selected row and whether it is a process
func (m *subagentMonitor) selected() (map[string]any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.selectedLocked()
	return row, row != nil && row["kind"] == "process"
}

// Subagent backend of this process, its own registry since the CLI runs its agent in process
func (m *subagentMonitor) subagents() *localSubagents {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.local == nil {
		m.local = &localSubagents{cli: m.cli, paths: map[string]any{}}
	}
	return m.local
}

// Rereads every dock block, true when anything changed since the last read
func (m *subagentMonitor) refreshAt(now float64) bool {
	c := m.cli
	c.uiMu.Lock()
	busy := c.agentRunning
	c.uiMu.Unlock()
	entries := m.subagents().subagentRows(busy)
	for _, row := range entries {
		started := now
		if f, err := pyint.Float(row["started_at"]); err == nil && pyrepr.Truthy(row["started_at"]) {
			started = f
		}
		row["elapsed"] = max(0, int(now-started))
		row["last_tool"] = pyrepr.Or(row["last_tool"], "")
		row["key"] = row["subagent_id"]
		delete(row, "running_seconds")
	}
	processes := processRows(c.ctx, c.env, now)
	sessionDockSyncAutomation(c)
	goal := sessionDockGoalLine(c)
	loopBadge, loop, loopPrompt := sessionDockLoopRow(c, now)
	queued := sessionDockQueuedPrompts(c)
	raw, err := json.Marshal([]any{entries, processes, goal, loop, queued})
	signature := string(raw)
	if err != nil {
		signature = fmt.Sprint([]any{entries, processes, goal, loop, queued})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := m.signature == nil || *m.signature != signature
	m.signature = &signature
	m.entries, m.processes, m.goal, m.loop = entries, processes, goal, loop
	m.loopBadge, m.loopPrompt, m.queued = loopBadge, loopPrompt, queued
	if m.selectedLocked() == nil {
		roster := m.rosterLocked()
		m.selectedID = nil
		if len(roster) > 0 {
			m.selectedID = dockRowKey(roster[0])
		}
	}
	return changed
}

// Rereads every dock block at the current time
func (m *subagentMonitor) refresh() { m.refreshAt(nowSeconds()) }

// Repaints the open monitor application on its loop
func (m *subagentMonitor) invalidate() {
	m.mu.Lock()
	app := m.app
	m.mu.Unlock()
	if app != nil {
		app.invalidate()
	}
}

// Lets the next spinner tick poll instead of blocking the caller
func (m *subagentMonitor) pollSoon() {
	m.mu.Lock()
	m.lastPoll = 0
	m.mu.Unlock()
}

// Origin the monitor's monotonic clock counts from
var monitorStart = time.Now()

// Monotonic seconds offset far past zero so a monitor that never polled polls at once
func monotonicSeconds() float64 { return time.Since(monitorStart).Seconds() + 1e6 }

// Spinner thread tick, a refresh at most once a second repainting what changed
func (m *subagentMonitor) tick() {
	now := monotonicSeconds()
	m.mu.Lock()
	if now-m.lastPoll < 1 {
		m.mu.Unlock()
		return
	}
	m.lastPoll = now
	m.mu.Unlock()
	changed := false
	failed := func() (failed bool) {
		defer func() {
			if r := recover(); r != nil {
				cliLogger.Debug(fmt.Sprintf("subagent monitor tick failed: %v", r))
				failed = true
			}
		}()
		changed = m.refreshWithTail()
		return false
	}()
	if failed || !changed {
		return
	}
	m.mu.Lock()
	app := m.app
	m.mu.Unlock()
	if app != nil {
		m.invalidate()
	} else {
		_ = m.cli.invalidate()
	}
}

// Refresh then the tail poll, true when either changed
func (m *subagentMonitor) refreshWithTail() bool {
	changed := m.refreshAt(nowSeconds())
	return m.pollTail() || changed
}

// Rereads the tail the open detail view paints, true when its text changed
func (m *subagentMonitor) pollTail() bool {
	m.mu.Lock()
	row := m.selectedLocked()
	detail := m.detail
	m.mu.Unlock()
	if !detail || row == nil || row["kind"] == "process" {
		return false
	}
	backend := m.subagents()
	id := pyrepr.Str(row["subagent_id"])
	before := backend.subagentTail(id, false)
	return backend.subagentTail(id, true) != before
}

// Moves the selection by delta through the roster, wrapping around
func (m *subagentMonitor) selectBy(delta int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	roster := m.rosterLocked()
	if len(roster) == 0 {
		return
	}
	index := 0
	for i, r := range roster {
		if dockRowKey(r) == m.selectedID {
			index = i
			break
		}
	}
	n := len(roster)
	m.selectedID = dockRowKey(roster[((index+delta)%n+n)%n])
}

// Steers or stops the target, killing a process for stop and refusing to steer one
func (m *subagentMonitor) control(action string, message any, target any) (map[string]any, error) {
	m.mu.Lock()
	if !pyrepr.Truthy(target) {
		target = m.selectedID
	}
	isProcess := false
	for _, r := range m.processes {
		if r["key"] == target {
			isProcess = true
		}
	}
	m.mu.Unlock()
	if isProcess {
		if action != "stop" {
			return map[string]any{"error": m.cli.t("cli.subagents.cannot_steer_process", nil)}, nil
		}
		return killDockProcess(m.cli.ctx, m.cli.env, pyrepr.Str(target)), nil
	}
	return m.subagents().subagentControl(action, pyrepr.Str(target), message)
}

// Count fragment, live agents and processes, the collapsed heading adding goal, loop and queue
func (m *subagentMonitor) countsLocked(session bool) string {
	t := m.cli.t
	var parts []string
	if len(m.entries) > 0 {
		parts = append(parts, t("cli.subagents.count_live", map[string]any{"count": len(m.entries)}))
	}
	if len(m.processes) > 0 {
		running := 0
		for _, r := range m.processes {
			if r["status"] == "running" {
				running++
			}
		}
		switch {
		case running == 1:
			parts = append(parts, t("cli.subagents.count_procs_one", map[string]any{"count": running}))
		case running > 0:
			parts = append(parts, t("cli.subagents.count_procs_other", map[string]any{"count": running}))
		default:
			parts = append(parts, t("cli.subagents.count_done", map[string]any{"count": len(m.processes)}))
		}
	}
	if session && m.goal != "" {
		key := "cli.subagents.goal_active"
		if strings.HasPrefix(m.goal, "⏳") {
			key = "cli.subagents.goal_parked"
		} else if strings.HasPrefix(m.goal, "⏸") {
			key = "cli.subagents.goal_paused"
		}
		parts = append(parts, t(key, nil))
	}
	if session && m.loopBadge != "" {
		parts = append(parts, m.loopBadge)
	}
	if session && len(m.queued) > 0 {
		parts = append(parts, t("cli.subagents.count_queued", map[string]any{"count": len(m.queued)}))
	}
	return strings.Join(parts, " · ")
}

// Live work, subagents or processes title of what the dock holds
func (m *subagentMonitor) titleLocked() string {
	if len(m.entries) > 0 && len(m.processes) > 0 {
		return m.cli.t("cli.subagents.title_live_work", nil)
	}
	if len(m.entries) > 0 {
		return m.cli.t("cli.subagents.title_subagents", nil)
	}
	return m.cli.t("cli.subagents.title_processes", nil)
}

// Last tool while one runs, else the raw status id, else starting
func (m *subagentMonitor) agentActivity(row map[string]any) string {
	if pyrepr.Truthy(row["last_tool"]) {
		return m.cli.t("cli.subagents.last_tool", map[string]any{"tool": row["last_tool"]})
	}
	if pyrepr.Truthy(row["status"]) {
		return pyrepr.Str(row["status"])
	}
	return m.cli.t("cli.subagents.starting", nil)
}

// Activity of the collapsed heading, the first agent, process, goal, loop prompt or queued prompt
func (m *subagentMonitor) collapsedActivityLocked() string {
	if len(m.entries) > 0 {
		return m.agentActivity(m.entries[0])
	}
	if len(m.processes) > 0 {
		return processActivity(m.cli.ctx, m.processes[0])
	}
	if m.goal != "" {
		return m.goal
	}
	if m.loopPrompt != "" {
		return m.loopPrompt
	}
	return m.cli.t("cli.subagents.next_queued", map[string]any{"text": m.queued[0]})
}

// Dock text for the terminal size, one heading line collapsed or every block expanded
func (m *subagentMonitor) dockText(columns, rows int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.cli.t
	if !m.hasRowsLocked() {
		return ""
	}
	if m.collapsed {
		count := m.countsLocked(true)
		var headings []string
		if len(m.entries) > 0 || len(m.processes) > 0 {
			headings = []string{t("cli.subagents.heading_full", map[string]any{"title": m.titleLocked(), "count": count}),
				t("cli.subagents.heading_medium", map[string]any{"count": count}),
				t("cli.subagents.heading_compact", map[string]any{"count": count}), count}
		} else {
			headings = []string{t("cli.subagents.heading_session_restore", map[string]any{"count": count}),
				t("cli.subagents.heading_session_compact", map[string]any{"count": count}), count}
		}
		width := max(0, columns-1)
		heading := count
		for _, text := range headings {
			if cellWidth(text) <= width {
				heading = text
				break
			}
		}
		activity := m.collapsedActivityLocked()
		room := width - cellWidth(heading+" · ")
		if cellWidth(activity) <= room || (room >= 12 && !(len(m.entries) > 0 || len(m.processes) > 0)) {
			heading += " · " + dockClip(activity, room)
		}
		return dockClip(" "+heading, max(0, columns))
	}
	columns = max(0, columns-2)
	var lines []string
	for _, row := range []string{m.goal, m.loop} {
		if row != "" {
			lines = append(lines, dockClip(" "+row, columns))
		}
	}
	budget := max(1, min(4, floorDiv(rows-10, 3)))
	agentBudget := budget
	if len(m.processes) > 0 {
		agentBudget = max(1, budget-max(1, budget/2))
	}
	agentCount := min(len(m.entries), agentBudget)
	if len(m.entries) > 0 {
		hidden := len(m.entries) - agentCount
		lines = append(lines, dockClip(" "+t("cli.subagents.subagents_heading", map[string]any{"count": len(m.entries)}), columns))
		for _, row := range m.entries[:agentCount] {
			activity := fmt.Sprintf("%vs · ", row["elapsed"]) + m.agentActivity(row)
			goalWidth := max(3, columns-cellWidth(activity)-5)
			lines = append(lines, dockClip(" ● "+dockClip(row["goal"], goalWidth)+" · "+activity, columns))
		}
		if hidden > 0 {
			lines = append(lines, dockClip(" "+t("cli.subagents.more_subagents", map[string]any{"count": hidden}), columns))
		}
	}
	if len(m.processes) > 0 {
		procCount := min(len(m.processes), max(1, budget-agentCount))
		running := 0
		for _, r := range m.processes {
			if r["status"] == "running" {
				running++
			}
		}
		done := len(m.processes) - running
		var summaryParts []string
		if running > 0 {
			summaryParts = append(summaryParts, t("cli.subagents.count_running", map[string]any{"count": running}))
		}
		if done > 0 {
			summaryParts = append(summaryParts, t("cli.subagents.count_done", map[string]any{"count": done}))
		}
		controls := ""
		if len(m.entries) == 0 {
			controls = t("cli.subagents.controls_expand_collapse", nil)
		}
		lines = append(lines, dockClip(" "+t("cli.subagents.processes_heading", map[string]any{"summary": strings.Join(summaryParts, " · "),
			"controls": controls}), columns))
		for _, row := range m.processes[:procCount] {
			activity := processActivity(m.cli.ctx, row)
			commandWidth := max(3, columns-cellWidth(activity)-5)
			lines = append(lines, dockClip(" "+processGlyph(row)+" "+dockClip(row["command"], commandWidth)+" · "+activity, columns))
		}
		if len(m.processes) > procCount {
			lines = append(lines, dockClip(" "+t("cli.subagents.more_processes", map[string]any{"count": len(m.processes) - procCount}), columns))
		}
	}
	if len(m.queued) > 0 {
		shown := 1
		if rows >= 24 {
			shown = sessionDockQueueRows
		}
		shown = min(len(m.queued), shown)
		controls := ""
		if len(m.entries) == 0 && len(m.processes) == 0 {
			controls = t("cli.subagents.controls_collapse", nil)
		}
		lines = append(lines, dockClip(" "+t("cli.subagents.queue_heading", map[string]any{"count": len(m.queued), "controls": controls}), columns))
		for index, text := range m.queued[:shown] {
			lines = append(lines, dockClip(fmt.Sprintf("  %d. %s", index+1, text), columns))
		}
		if len(m.queued) > shown {
			lines = append(lines, dockClip("  "+t("cli.subagents.more_queued", map[string]any{"count": len(m.queued) - shown}), columns))
		}
	}
	for i, line := range lines {
		lines[i] = " " + line
	}
	return strings.Join(lines, "\n")
}

// Printable last 32KiB of a transcript file, the unavailable notice without a path or on a read error
func readTail(c *CLI, path any) string {
	p, _ := path.(string)
	if !pyrepr.Truthy(path) || p == "" {
		return c.t("cli.subagents.transcript_unavailable", nil)
	}
	f, err := os.Open(p)
	if err != nil {
		return c.t("cli.subagents.transcript_unavailable", nil)
	}
	defer f.Close()
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return c.t("cli.subagents.transcript_unavailable", nil)
	}
	if _, err := f.Seek(max(0, end-32768), io.SeekStart); err != nil {
		return c.t("cli.subagents.transcript_unavailable", nil)
	}
	raw := make([]byte, 32768)
	n, err := io.ReadFull(f, raw)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return c.t("cli.subagents.transcript_unavailable", nil)
	}
	var b strings.Builder
	for _, r := range pystr.DecodeUTF8Replace(raw[:n]) {
		if pystr.IsPrintable(r) || r == '\n' || r == '\t' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Transcript tail of a subagent row from the monitor backend
func subagentTail(m *subagentMonitor, row map[string]any) string {
	return m.subagents().subagentTail(pyrepr.Str(row["subagent_id"]), false)
}

// This process's subagent registry behind the dock's subagent surface
type localSubagents struct {
	cli   *CLI
	mu    sync.Mutex
	paths map[string]any
}

// Live child rows of the CLI agent's spawn tree with start time and latest tool
func (l *localSubagents) subagentRows(bool) []map[string]any {
	parent := l.cli.agent
	var entries []map[string]any
	if parent != nil {
		entries = agent.SubagentListRows(parent)
	}
	live := map[string]map[string]any{}
	if len(entries) > 0 {
		for _, r := range agent.ListActiveSubagents() {
			live[pyrepr.Str(r["subagent_id"])] = r
		}
	}
	paths := map[string]any{}
	for _, row := range entries {
		record := live[pyrepr.Str(row["subagent_id"])]
		row["started_at"], row["last_tool"] = record["started_at"], pyrepr.Or(record["last_tool"], "")
		paths[pyrepr.Str(row["subagent_id"])] = row["live_transcript"]
	}
	l.mu.Lock()
	l.paths = paths
	l.mu.Unlock()
	return entries
}

// Transcript tail of a child from its live transcript path
func (l *localSubagents) subagentTail(subagentID string, _ bool) string {
	l.mu.Lock()
	path := l.paths[subagentID]
	l.mu.Unlock()
	return readTail(l.cli, path)
}

// Runs a steer or stop control action against the CLI agent's spawn tree
func (l *localSubagents) subagentControl(action, subagentID string, message any) (map[string]any, error) {
	return agent.SubagentControl(action, subagentID, message, l.cli.agent)
}

// Whether a prompt owns the composer, every composer shortcut staying off while one is open
func modalPromptActive(c *CLI) bool {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.clarifyState != nil || c.approvalState != nil || c.slashConfirmState != nil || c.sudoState != nil ||
		c.secretState != nil || c.connectionState != nil || c.modelPickerState != nil || c.commandPaletteState != nil
}

// Opens the full height monitor over the terminal unless it is already opening
func openMonitor(c *CLI) {
	c.uiMu.Lock()
	m, app := c.subagentMonitor, c.app
	c.uiMu.Unlock()
	if m == nil || app == nil {
		return
	}
	m.mu.Lock()
	if m.opening {
		m.mu.Unlock()
		return
	}
	m.opening = true
	m.mu.Unlock()
	app.RunInTerminal(func() {
		defer func() {
			if r := recover(); r != nil {
				cliLogger.Error(fmt.Sprintf("Task exception was never retrieved: %v", r))
			}
			m.mu.Lock()
			m.app = nil
			m.detail, m.opening = false, false
			m.mu.Unlock()
			_ = c.invalidate()
		}()
		m.pollSoon()
		monitorApp := buildMonitorApplication(m, console().In, console().Out)
		m.mu.Lock()
		m.app = monitorApp
		m.mu.Unlock()
		monitorApp.run()
	})
}

// Whether the monitor application is being opened
func (m *subagentMonitor) isOpening() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opening
}

// Collapses the dock to its heading or restores it
func toggleDock(c *CLI) {
	c.uiMu.Lock()
	m := c.subagentMonitor
	c.uiMu.Unlock()
	if m == nil {
		return
	}
	m.mu.Lock()
	m.collapsed = !m.collapsed
	m.mu.Unlock()
	_ = c.invalidate()
}

// Builds the monitor and the dock window shown while it holds rows and no prompt is open
func installDock(c *CLI) {
	m := newSubagentMonitor(c)
	c.uiMu.Lock()
	c.subagentMonitor = m
	c.uiMu.Unlock()
	m.refresh()
	text := func() []styledFragment {
		columns, rows := 80, 24
		if c.app != nil {
			if w, h, ok := c.app.Size(); ok {
				columns, rows = w, h
			}
		}
		lines := pystr.Splitlines(m.dockText(columns, rows), false)
		out := make([]styledFragment, len(lines))
		for i, line := range lines {
			style := ""
			if i == 0 {
				style = "class:subagent-dock.heading"
			}
			if i < len(lines)-1 {
				line += "\n"
			}
			out[i] = styledFragment{Style: style, Text: line}
		}
		return out
	}
	c.tui.subagentDockWidget = &ptWindow{name: "subagent-dock", fragments: text, style: "class:subagent-dock",
		filter: func() bool { return m.hasRows() && !modalPromptActive(c) }}
}
