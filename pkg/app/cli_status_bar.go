package app

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	statusBarClass       = "class:status-bar"
	statusBarDim         = "class:status-bar-dim"
	statusBarStrong      = "class:status-bar-strong"
	petFrameInterval     = 160 * time.Millisecond
	petConfigIntervalSec = 2.5
)

var (
	agentCounters = []string{"session_input_tokens", "session_output_tokens", "session_cache_read_tokens",
		"session_cache_write_tokens", "session_prompt_tokens", "session_completion_tokens",
		"session_total_tokens", "session_api_calls"}
	readBattery          = hostinfo.ReadBattery
	loadStatusBarConfig  = func(c *CLI) (map[string]any, error) { return config.Load(home.Dir(c.ctx), nil) }
	statusBarUsage       = func(c *CLI, a cliAgent) agent.StatusBarUsage { return a.StatusBarUsage(c.ctx) }
	statusBarSnapshotFor = (*CLI).getStatusBarSnapshot
	tuiTerminalWidthFor  = (*CLI).getTUITerminalWidth
)

// threshold is one descending bound of a status bar colour ladder
type threshold struct {
	bound float64
	name  string
}

// First status bar class whose bound the value reaches in a descending ladder, the fallback class otherwise
func thresholdStyle(value float64, ladder []threshold, fallback string) string {
	for _, step := range ladder {
		if value >= step.bound {
			return "class:status-bar-" + step.name
		}
	}
	return "class:status-bar-" + fallback
}

// Drops missing, negative and absurd provider timings
func finiteTiming(v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || *v < 0 || *v > 1e6 {
		return nil
	}
	return v
}

// Colour class of the context gauge for the percent used, dim when unknown
func (c *CLI) statusBarContextStyle(percentUsed *int) string {
	if percentUsed == nil {
		return statusBarDim
	}
	if *percentUsed >= 95 {
		return "class:status-bar-critical"
	}
	if *percentUsed > 80 {
		return "class:status-bar-bad"
	}
	return thresholdStyle(float64(*percentUsed), []threshold{{50, "warn"}}, "good")
}

// Number a snapshot value holds, zero when it holds none
func snapshotNumber(v any) float64 {
	n, _ := progressNumber(v)
	return n
}

// Cache hit percent and its label, false without cache data
func (c *CLI) cacheHitRate(snapshot map[string]any, precision int) (float64, string, bool) {
	if delta, ok := snapshot["cache_hit_pct"].(float64); ok {
		return delta, fmt.Sprintf("◎ %.*f%%", precision, delta), true
	}
	cacheRead, promptTotal := snapshotNumber(snapshot["session_cache_read_tokens"]), snapshotNumber(snapshot["session_prompt_tokens"])
	if cacheRead > 0 && promptTotal > 0 {
		pct := cacheRead / promptTotal * 100
		return pct, fmt.Sprintf("◎ %.*f%%", precision, pct), true
	}
	return 0, "", false
}

// Colour class of a cache hit percent, higher being better
func (c *CLI) cacheHitRateStyle(cachePct float64) string {
	return thresholdStyle(cachePct, []threshold{{70, "good"}, {40, "warn"}}, "bad")
}

// Colour class of a battery category, dim for an unknown one
func batteryStatusStyle(category string) string {
	switch category {
	case "good", "warn", "bad", "critical":
		return "class:status-bar-" + category
	}
	return statusBarDim
}

// Short label of the current vi editing mode, empty when vim mode is off or no application runs
func (c *CLI) vimModeLabel() string {
	if !c.vimMode || c.app == nil {
		return ""
	}
	mode, ok := c.tuiViInputMode()
	if !ok {
		return ""
	}
	switch mode {
	case "insert", "insert_multiple":
		return c.t("cli.status_bar.vim_insert", nil)
	case "replace":
		return c.t("cli.status_bar.vim_replace", nil)
	}
	return c.t("cli.status_bar.vim_normal", nil)
}

// Toggles, sets or reports the battery segment of the status bar and persists display.battery
func (c *CLI) handleBatteryCommand(cmdOriginal string) {
	parts := pystr.SplitWhitespace(cmdOriginal, -1)
	arg := ""
	if len(parts) > 1 {
		arg = pystr.Lower(pystr.Strip(parts[1]))
	}
	var reading *hostinfo.BatteryStatus
	func() {
		defer func() { _ = recover() }()
		r := readBattery(false)
		reading = &r
	}()
	detail := func(noBattery string) string {
		if reading == nil {
			return ""
		}
		if reading.Available {
			return " — " + hostinfo.FormatBattery(*reading)
		}
		return " — " + noBattery
	}
	if arg == "status" || arg == "show" {
		state := c.t("cli.shared.label_off", nil)
		if c.batteryVisible {
			state = c.t("cli.shared.label_on", nil)
		}
		text := detail(c.t("cli.status_bar.battery_none", nil))
		if reading != nil && reading.Available {
			text = c.t("cli.status_bar.battery_currently", map[string]any{"reading": hostinfo.FormatBattery(*reading)})
		}
		c.consolePrint("  " + c.t("cli.status_bar.battery_state", map[string]any{"state": state, "detail": text}))
		return
	}
	var target bool
	switch arg {
	case "on", "true", "yes":
		target = true
	case "off", "false", "no":
		target = false
	case "", "toggle":
		target = !c.batteryVisible
	default:
		c.consolePrint("  " + c.t("cli.status_bar.battery_usage", nil))
		return
	}
	c.batteryVisible = target
	SaveConfigValue(c.ctx, "display.battery", target)
	if target {
		c.consolePrint("  " + c.t("cli.status_bar.battery_on", map[string]any{"detail": detail(c.t("cli.status_bar.battery_none_nothing_shown", nil))}))
		return
	}
	c.consolePrint("  " + c.t("cli.status_bar.battery_off", nil))
}

// Colour class of the compression counter
func compressionCountStyle(count float64) string {
	return thresholdStyle(count, []threshold{{10, "bad"}, {5, "warn"}}, "dim")
}

// Ten cell gauge of the context percent used
func (c *CLI) buildContextBar(percentUsed *int, width int) string {
	safePercent := 0
	if percentUsed != nil {
		safePercent = max(0, min(100, *percentUsed))
	}
	filled := int(math.RoundToEven(float64(safePercent) / 100 * float64(width)))
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", max(0, width-filled)) + "]"
}

// Python divmod of two floats
func floatDivMod(a, b float64) (float64, float64) {
	q := math.Floor(a / b)
	return q, a - q*b
}

// Elapsed time of the running or last prompt, a stopwatch glyph while live and a timer glyph once frozen
func (c *CLI) formatPromptElapsed(promptStartTime *float64, promptDuration float64, live bool) string {
	if promptStartTime == nil && promptDuration == 0 {
		return "⏲ 0s"
	}
	elapsed := max(0.0, promptDuration)
	if promptStartTime != nil {
		elapsed = max(0.0, wallClock()-*promptStartTime)
	}
	daysF, remaining := floatDivMod(elapsed, 86400)
	hoursF, remaining := floatDivMod(remaining, 3600)
	minutes, seconds := int(math.Floor(remaining/60)), int(math.Mod(remaining, 60))
	days, hours := int(daysF), int(hoursF)
	var timeStr string
	switch {
	case days > 0:
		timeStr = c.t("cli.shared.duration_dhm", map[string]any{"days": days, "hours": hours, "minutes": minutes})
	case hours > 0 && seconds != 0:
		timeStr = c.t("cli.shared.duration_hms", map[string]any{"hours": hours, "minutes": minutes, "seconds": seconds})
	case hours > 0:
		timeStr = c.t("cli.shared.duration_hm", map[string]any{"hours": hours, "minutes": minutes})
	case minutes > 0 && seconds != 0:
		timeStr = c.t("cli.shared.duration_ms", map[string]any{"minutes": minutes, "seconds": seconds})
	case minutes > 0:
		timeStr = c.t("cli.shared.duration_m", map[string]any{"minutes": minutes})
	default:
		timeStr = c.t("cli.shared.duration_s", map[string]any{"seconds": int(elapsed)})
	}
	if live {
		return "⏱ " + timeStr
	}
	return "⏲ " + timeStr
}

// Compact time since the last final response, empty while a turn is live or before the first one ends
func formatIdleSince(lastFinishedAt *float64, turnLive bool) string {
	if turnLive || lastFinishedAt == nil {
		return ""
	}
	return "✓ " + provider.FormatDurationCompact(max(0.0, wallClock()-*lastFinishedAt))
}

// Collects every value the status bar renders for one repaint
func (c *CLI) getStatusBarSnapshot() map[string]any {
	a := c.agent
	modelName := c.model
	if a != nil && a.Model() != "" {
		modelName = a.Model()
	}
	if modelName == "" {
		modelName = c.t("cli.shared.unknown", nil)
	}
	modelShort := reverseAliasForDisplay(c.ctx, modelName)
	if modelShort == modelName {
		if i := strings.LastIndex(modelName, "/"); i >= 0 {
			modelShort = modelName[i+1:]
		}
		modelShort = provider.FormatModelForDisplay(modelShort)
	}
	modelShort = strings.TrimSuffix(modelShort, ".gguf")
	if pystr.Len(modelShort) > 26 {
		modelShort = pystr.Slice(modelShort, 0, 23) + "..."
	}
	c.uiMu.Lock()
	promptStart, promptDuration, lastFinished := c.promptStartTime, c.promptDuration, c.lastTurnFinishedAt
	c.uiMu.Unlock()
	turnLive := promptStart != nil
	snapshot := map[string]any{
		"model_name": modelName, "model_short": modelShort,
		"duration":                    provider.FormatDurationCompact(max(0.0, time.Since(c.sessionStart).Seconds())),
		"session_title":               c.getStatusBarSessionTitle(),
		"prompt_elapsed":              c.formatPromptElapsed(promptStart, promptDuration, turnLive),
		"idle_since":                  formatIdleSince(lastFinished, turnLive),
		"context_tokens":              0,
		"context_length":              nil,
		"context_percent":             nil,
		"compressions":                0,
		"active_background_tasks":     0,
		"active_background_processes": 0,
		"active_background_subagents": 0,
		"battery_label":               "",
		"battery_category":            "dim",
		"focus_label":                 focusStatusbarSegment(c.focusViewEnabled),
		"git_branch":                  "",
		"goal_active":                 false,
		"goal_turns_used":             0,
		"goal_max_turns":              0,
	}
	for _, key := range agentCounters {
		snapshot[key] = 0
	}
	if fields := c.getStatusBarFieldSet(); fields != nil && fields["git_branch"] {
		snapshot["git_branch"] = currentGitBranch("")
	}
	if c.batteryVisible {
		func() {
			defer func() { _ = recover() }()
			battery := readBattery(true)
			snapshot["battery_label"] = hostinfo.FormatBattery(battery)
			snapshot["battery_category"] = hostinfo.BatteryCategory(battery)
		}()
	}
	c.uiMu.Lock()
	backgroundTasks := len(c.backgroundTasks)
	c.uiMu.Unlock()
	if backgroundTasks > 0 {
		snapshot["active_background_tasks"] = backgroundTasks
	}
	func() {
		defer func() { _ = recover() }()
		counts := gatewayBackgroundCounts()
		snapshot["active_background_processes"], snapshot["active_background_subagents"] = counts["active_background_processes"], counts["active_background_subagents"]
	}()
	if manager := c.getGoalManager(); manager != nil && manager.IsActive() {
		snapshot["goal_active"] = true
		snapshot["goal_turns_used"] = int(manager.State().TurnsUsed)
		snapshot["goal_max_turns"] = int(manager.State().MaxTurns)
	}
	if a == nil {
		return snapshot
	}
	usage := statusBarUsage(c, a)
	for _, key := range agentCounters {
		snapshot[key] = usage.Counters[key]
	}
	if usage.HasCompressor {
		snapshot["context_estimated"] = usage.ContextEstimated
		snapshot["context_tokens"] = usage.ContextTokens
		if usage.ContextLength != 0 {
			snapshot["context_length"] = usage.ContextLength
		}
		snapshot["context_pinned"] = usage.ContextPinned
		snapshot["compressions"] = usage.Compressions
		if usage.ContextPercent != nil {
			snapshot["context_percent"] = *usage.ContextPercent
		}
	}
	snapshot["cache_hit_pct"] = nil
	snapshot["cache_hit_label"] = ""
	if pct, ok := c.cacheHitSinceBaseline(modelName, usage); ok {
		snapshot["cache_hit_pct"] = pct
		snapshot["cache_hit_label"] = fmt.Sprintf("%.0f%%", pct)
	}
	var avgLatency, avgVelocity *float64
	if n := min(len(usage.LatencyHistory), len(usage.OutputHistory)); n > 0 {
		latencies, outputs := usage.LatencyHistory[len(usage.LatencyHistory)-n:], usage.OutputHistory[len(usage.OutputHistory)-n:]
		totalLatency, totalOutput := 0.0, 0.0
		for i := range latencies {
			totalLatency += latencies[i]
			totalOutput += float64(outputs[i])
		}
		mean := totalLatency / float64(n)
		avgLatency = finiteTiming(&mean)
		if totalLatency > 0 {
			velocity := totalOutput / totalLatency
			avgVelocity = finiteTiming(&velocity)
		}
	}
	snapshot["avg_latency"], snapshot["avg_latency_label"] = nil, ""
	if avgLatency != nil {
		snapshot["avg_latency"] = *avgLatency
		snapshot["avg_latency_label"] = c.t("cli.status_bar.latency_label", map[string]any{"value": fmt.Sprintf("%.1f", *avgLatency)})
	}
	snapshot["avg_velocity"], snapshot["avg_velocity_label"] = nil, ""
	if avgVelocity != nil {
		snapshot["avg_velocity"] = *avgVelocity
		snapshot["avg_velocity_label"] = c.t("cli.status_bar.velocity_label", map[string]any{"value": fmt.Sprintf("%.0f", *avgVelocity)})
	}
	return snapshot
}

// Cache hit percent since the baseline, which a model switch or a compression moves to the current counters
func (c *CLI) cacheHitSinceBaseline(modelName string, usage agent.StatusBarUsage) (float64, bool) {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	curComps, curPrompt, curRead := usage.Compressions, usage.Counters["session_prompt_tokens"], usage.Counters["session_cache_read_tokens"]
	rebase := func(tokens bool) {
		c.cacheHitBaselineModel = modelName
		c.cacheHitBaselineCompressions = curComps
		if tokens {
			c.cacheHitBaselinePrompt, c.cacheHitBaselineRead = curPrompt, curRead
		}
	}
	if c.cacheHitBaselineModel == "" {
		rebase(false)
	} else if modelName != c.cacheHitBaselineModel {
		rebase(true)
	}
	if curComps != c.cacheHitBaselineCompressions {
		rebase(true)
	}
	deltaPrompt, deltaRead := curPrompt-c.cacheHitBaselinePrompt, curRead-c.cacheHitBaselineRead
	if deltaPrompt > 0 && deltaRead > 0 {
		return max(0.0, min(100.0, float64(deltaRead)/float64(deltaPrompt)*100)), true
	}
	return 0, false
}

// Current session title, read from the session store at most every second and a half
func (c *CLI) getStatusBarSessionTitle() string {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	pending := pystr.Strip(c.pendingTitle)
	now := monotonic()
	cacheFresh := c.statusBarTitleSessionID == c.sessionID && now-c.statusBarTitleCheckedAt < 1.5
	if pending == "" && cacheFresh {
		return c.statusBarTitleCache
	}
	title := pending
	if pending == "" && c.sessionDB != nil && c.sessionID != "" {
		title = ""
		if stored, _, err := c.sessionDB.GetSessionTitle(c.ctx, c.sessionID); err == nil {
			title = pystr.Strip(stored)
		}
	}
	c.statusBarTitleSessionID, c.statusBarTitleCache, c.statusBarTitleCheckedAt = c.sessionID, title, now
	return title
}

// Terminal cells the text occupies, wide glyphs counting two and control characters none
func statusBarDisplayWidth(text string) int {
	width := 0
	for _, r := range pystr.Runes(text) {
		width += max(0, pyunicode.WcWidth(r))
	}
	return width
}

// Text cut to one terminal row of maxWidth cells with a trailing ellipsis
func trimStatusBarText(text string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if statusBarDisplayWidth(text) <= maxWidth {
		return text
	}
	ellipsisWidth := statusBarDisplayWidth("...")
	if maxWidth <= ellipsisWidth {
		return "..."[:maxWidth]
	}
	out, width := []rune{}, 0
	for _, ch := range pystr.Runes(text) {
		cw := max(0, pyunicode.WcWidth(ch))
		if width+cw+ellipsisWidth > maxWidth {
			break
		}
		out = append(out, ch)
		width += cw
	}
	return pystr.Rstrip(pystr.FromRunes(out)) + "..."
}

// Session title badge for the far right edge and the width left of it, false when it cannot fit
func statusTitleBadge(title string, width int) (string, int, bool) {
	title = pystr.Strip(title)
	if title == "" || width < 24 {
		return "", 0, false
	}
	titleWidth := max(6, min(30, width/3))
	badge := " " + trimStatusBarText(title, titleWidth-2) + " "
	suffixWidth := statusBarDisplayWidth(" ─") + statusBarDisplayWidth(badge)
	return badge, max(0, width-suffixWidth), true
}

// Pins a bounded session title badge to the far right edge of the plain status text
func rightAlignStatusTitle(text, title string, width int) string {
	badge, leftWidth, ok := statusTitleBadge(title, width)
	if !ok {
		return trimStatusBarText(text, width)
	}
	left := trimStatusBarText(pystr.Rstrip(text), leftWidth)
	return left + strings.Repeat(" ", max(0, leftWidth-statusBarDisplayWidth(left))) + " ─" + badge
}

// Pins a bounded session title badge to the far right edge of the styled status fragments
func rightAlignStatusTitleFragments(frags []styledFragment, title string, width int) []styledFragment {
	badge, leftWidth, ok := statusTitleBadge(title, width)
	if !ok {
		return frags
	}
	trimmed, used := []styledFragment{}, 0
	for _, frag := range frags {
		remaining := leftWidth - used
		if remaining <= 0 {
			break
		}
		valueWidth := statusBarDisplayWidth(frag.Text)
		if valueWidth <= remaining {
			trimmed = append(trimmed, frag)
			used += valueWidth
			continue
		}
		if clipped := trimStatusBarText(frag.Text, remaining); clipped != "" {
			trimmed = append(trimmed, styledFragment{frag.Style, clipped})
			used += statusBarDisplayWidth(clipped)
		}
		break
	}
	if used < leftWidth {
		trimmed = append(trimmed, styledFragment{statusBarDim, strings.Repeat(" ", leftWidth-used)})
	}
	return append(trimmed, styledFragment{statusBarDim, " ─"}, styledFragment{"class:status-bar-session-title", badge})
}

// Width of the live prompt, the terminal columns when no application reports one
func (c *CLI) getTUITerminalWidth() int {
	if app := c.app; app != nil {
		if columns, _, ok := app.Size(); ok {
			return columns
		}
	}
	return terminalColumns()
}

// Reports a narrow terminal where low value chrome is hidden to save rows
func (c *CLI) useMinimalTUIChrome(width *int) bool {
	if width == nil {
		return tuiTerminalWidthFor(c) < 64
	}
	return *width < 64
}

// Full viewport width for printed scrollback box rules, floored at thirty two columns
func scrollbackBoxWidth() int { return max(32, terminalColumns()) }

// Reads whether a turn or a slash command runs
func (c *CLI) busyState() (agentRunning, commandRunning bool) {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.agentRunning, c.commandRunning
}

// Spacer rows above the status bar while the agent runs
func (c *CLI) agentSpacerHeight() int { return c.agentSpacerHeightAt(nil) }

// Spacer rows above the status bar at a width, the live terminal width when nil
func (c *CLI) agentSpacerHeightAt(width *int) int {
	if running, _ := c.busyState(); !running || c.modernLayout {
		return 0
	}
	if c.useMinimalTUIChrome(width) {
		return 0
	}
	return 1
}

// Visible rows of the spinner line above the status bar
func (c *CLI) spinnerWidgetHeight() int { return c.spinnerWidgetHeightAt(nil) }

// Visible rows of the spinner line at a width, the live terminal width when nil
func (c *CLI) spinnerWidgetHeightAt(width *int) int {
	spinnerLine := c.renderSpinnerText()
	if spinnerLine == "" || c.useMinimalTUIChrome(width) || c.modernLayout {
		return 0
	}
	w := 0
	if width != nil {
		w = *width
	}
	if w == 0 {
		w = tuiTerminalWidthFor(c)
	}
	if w > 10 {
		return max(1, (statusBarDisplayWidth(spinnerLine)+w-1)/w)
	}
	return 1
}

// Live spinner text exactly as the TUI renders it, with the tool timer and the token flow
func (c *CLI) renderSpinnerText() string {
	c.uiMu.Lock()
	txt, t0 := c.spinnerText, c.toolStartTime
	c.uiMu.Unlock()
	if txt == "" {
		return ""
	}
	flow := c.spinnerTokenFlow()
	if t0 > 0 {
		elapsed := monotonic() - t0
		var elapsedStr string
		if elapsed >= 60 {
			elapsedStr = c.t("cli.shared.duration_ms_compact", map[string]any{
				"minutes": fmt.Sprintf("%02d", int(math.Floor(elapsed/60))), "seconds": fmt.Sprintf("%02d", int(math.Mod(elapsed, 60)))})
		} else {
			elapsedStr = c.t("cli.shared.duration_s", map[string]any{"seconds": fmt.Sprintf("%5.1f", elapsed)})
		}
		if flow != "" {
			return "  " + txt + "  (" + elapsedStr + " · " + flow + ")"
		}
		return "  " + txt + "  (" + elapsedStr + ")"
	}
	if flow != "" {
		return "  " + txt + "  (" + flow + ")"
	}
	return "  " + txt
}

// Output tokens the running turn produced so far, for the spinner
func (c *CLI) spinnerTokenFlow() string {
	if !c.spinnerTokenFlowEnabled {
		return ""
	}
	running, _ := c.busyState()
	a := c.agent
	if !running || a == nil {
		return ""
	}
	produced := statusBarUsage(c, a).Counters["session_output_tokens"] - c.turnTokenBaseline
	flow, err := agent.FormatTokenFlow(produced, "↓")
	if err != nil {
		return ""
	}
	return flow
}

// Reports whether the per turn summary line renders on this surface
func (c *CLI) turnSummaryIsActive() bool {
	if !c.turnSummaryEnabled || c.toolProgressMode == "off" {
		return false
	}
	if a := c.agent; a != nil && a.QuietMode() && !c.modernLayout {
		return false
	}
	return c.interactiveTurn
}

// Starts the per turn accounting of the turn about to run
func (c *CLI) turnSummaryBegin() {
	defer func() {
		if recover() != nil {
			c.turnSummaryCollector = nil
		}
	}()
	if c.turnSummaryCollector == nil {
		c.turnSummaryCollector = agent.NewTurnSummaryCollector()
	}
	c.turnSummaryCollector.Begin()
	c.turnSummaryStart = monotonic()
	c.turnTokenBaseline = 0
	if a := c.agent; a != nil {
		c.turnTokenBaseline = statusBarUsage(c, a).Counters["session_output_tokens"]
	}
}

// Feeds one completed tool call into the active turn tally
func (c *CLI) turnSummaryRecord(functionName string, result any, isError bool) {
	collector := c.turnSummaryCollector
	if collector == nil {
		return
	}
	defer func() { _ = recover() }()
	collector.RecordTool(functionName, result, isError)
}

// Prints the post turn accounting line when it is enabled on this surface
func (c *CLI) turnSummaryEmit() {
	collector := c.turnSummaryCollector
	if collector == nil || !c.turnSummaryIsActive() {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug("Turn summary render failed", "error", r)
		}
	}()
	elapsed := 0.0
	if c.turnSummaryStart != 0 {
		elapsed = max(0.0, monotonic()-c.turnSummaryStart)
	}
	line := collector.Render(elapsed)
	switch {
	case line != "" && c.modernLayout:
		cprint(turnFooter(line))
	case line != "":
		cprint("  " + dimANSI + line + resetANSI)
	}
}

// Drops the pet renderer and the queued kitty state with the pet lock held
func (c *CLI) petClearRuntime() {
	c.petEnabled = false
	c.petRenderer = nil
	c.petFramesCache = map[string][][][]pet.Cell{}
	c.petKittyCache = map[string]*petKittyEntry{}
	c.petKittyPending = ""
	c.petKittyImageID = 0
}

// Resolves the active pet from config again so pet changes apply without a restart, any problem disabling it
func (c *CLI) petResolveConfig() {
	defer func() {
		if recover() != nil {
			c.petLock.Lock()
			c.petClearRuntime()
			c.petLock.Unlock()
		}
	}()
	cfg, err := loadStatusBarConfig(c)
	if err != nil {
		panic(err)
	}
	display, _ := cfg["display"].(map[string]any)
	petCfg, _ := display["pet"].(map[string]any)
	enabled := petCfg["enabled"] != nil && provider.IsTruthyValue(petCfg["enabled"])
	slug := cliStr(petCfg["slug"], "")
	scale := pet.DefaultScale
	if raw := petCfg["scale"]; pyrepr.Truthy(raw) {
		parsed, ferr := pyint.Float(raw)
		if ferr != nil {
			panic(ferr)
		}
		if parsed != 0 {
			scale = parsed
		}
	}
	unicodeCols := 0
	if raw := petCfg["unicode_cols"]; pyrepr.Truthy(raw) {
		n, ierr := pyint.Of(raw)
		if ierr != nil {
			panic(ierr)
		}
		unicodeCols = int(n.Int64())
	}
	cols := pet.ResolveCols(scale, unicodeCols)
	configuredMode := "auto"
	if raw := petCfg["render_mode"]; pyrepr.Truthy(raw) {
		configuredMode = pystr.Lower(pyrepr.Str(raw))
	}
	rendererMode := "unicode"
	if (configuredMode == "" || configuredMode == "auto" || configuredMode == "kitty") && pet.SupportsKittyPlaceholders() {
		rendererMode = "kitty"
	}
	var active *pet.InstalledPet
	if enabled && configuredMode != "off" {
		if active, err = pet.ResolveActivePet(home.Dir(c.ctx), slug); err != nil {
			panic(err)
		}
	}
	c.petLock.Lock()
	defer c.petLock.Unlock()
	if active == nil || !active.Exists() {
		c.petClearRuntime()
		return
	}
	if c.petRenderer == nil || c.petSlug != active.Slug || c.petCols != cols || c.petScale != scale || c.petRenderer.Mode != rendererMode {
		c.petRenderer = pet.NewRenderer(active.Spritesheet, rendererMode, scale, cols)
		c.petSlug, c.petCols, c.petScale = active.Slug, cols, scale
		c.petFramesCache = map[string][][][]pet.Cell{}
		c.petKittyCache = map[string]*petKittyEntry{}
		c.petKittyPending = ""
		c.petKittyImageID = pet.KittyImageID(active.Slug)
		c.petFrameIdx = 0
	}
	c.petEnabled = true
}

// Forces a transient pet reaction for the given seconds before it rests again
func (c *CLI) petFlash(state string, secs float64) {
	c.uiMu.Lock()
	c.petEvent = state
	c.petEventUntil = monotonic() + secs
	c.uiMu.Unlock()
}

// Plays the pet jump when the agent detected user affection
func (c *CLI) onReaction(kind string) {
	if kind == "vibe" {
		c.petFlash("jump", 1.6)
	}
}

// Plays the end of turn beat, failed on error, jump on a finished plan, else wave
func (c *CLI) petReactTurnEnd() {
	c.petLock.Lock()
	enabled := c.petEnabled
	c.petLock.Unlock()
	if !enabled {
		return
	}
	c.uiMu.Lock()
	turnError := c.petTurnError
	c.uiMu.Unlock()
	if turnError {
		c.petFlash("failed", 1.6)
		return
	}
	done := false
	if a := c.agent; a != nil {
		func() {
			defer func() {
				if recover() != nil {
					done = false
				}
			}()
			if store := a.TodoStore(); store != nil {
				items := store.Read()
				statuses := make([]string, len(items))
				for i, item := range items {
					statuses[i] = item.Status
				}
				done = pet.TodosAllDone(statuses)
			}
		}()
	}
	if done {
		c.petFlash("jump", 1.6)
		return
	}
	c.petFlash("wave", 1.6)
}

// Animation state of the pet, a live transient beat winning over the shared activity order
func (c *CLI) derivePetState() string {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	if c.petEvent != "" && monotonic() < c.petEventUntil {
		return c.petEvent
	}
	c.petEvent = ""
	awaitingInput := c.approvalState != nil || c.clarifyState != nil || c.sudoState != nil || c.secretState != nil || c.slashConfirmState != nil
	return string(pet.DeriveState(pet.Signals{AwaitingInput: awaitingInput, Busy: c.agentRunning, Reasoning: c.petReasoning}))
}

// Half block grids of one state, cached, with the pet lock held
func (c *CLI) petFramesFor(state string) [][][]pet.Cell {
	if cached, ok := c.petFramesCache[state]; ok {
		return cached
	}
	renderer := c.petRenderer
	if renderer == nil {
		return nil
	}
	grids := func() (grids [][][]pet.Cell) {
		defer func() {
			if recover() != nil {
				grids = [][][]pet.Cell{}
			}
		}()
		count := renderer.FrameCount(state)
		if count == 0 {
			count = 1
		}
		for i := 0; i < count; i++ {
			grids = append(grids, renderer.Cells(state, i, c.petCols))
		}
		return grids
	}()
	c.petFramesCache[state] = grids
	return grids
}

// Kitty payload of a state with its image id, cached, nil when the pet is not drawn through kitty
func (c *CLI) petKittyPayloadFor(state string) *petKittyEntry {
	c.petLock.Lock()
	if cached := c.petKittyCache[state]; cached != nil {
		c.petLock.Unlock()
		return cached
	}
	renderer, imageID := c.petRenderer, c.petKittyImageID
	c.petLock.Unlock()
	if renderer == nil || renderer.Mode != "kitty" {
		return nil
	}
	var payload *pet.KittyPayload
	func() {
		defer func() {
			if recover() != nil {
				payload = nil
			}
		}()
		payload = renderer.KittyPayload(state, imageID)
	}()
	if payload == nil {
		return nil
	}
	entry := &petKittyEntry{payload: payload, imageID: imageID}
	c.petLock.Lock()
	if c.petRenderer == renderer && c.petKittyImageID == imageID {
		c.petKittyCache[state] = entry
	}
	c.petLock.Unlock()
	return entry
}

// Queues one virtual kitty frame of the current state for the next render
func (c *CLI) petQueueKittyFrame() {
	c.petLock.Lock()
	enabled := c.petEnabled
	c.petLock.Unlock()
	if !enabled {
		return
	}
	entry := c.petKittyPayloadFor(c.derivePetState())
	if entry == nil || len(entry.payload.Frames) == 0 {
		return
	}
	c.petLock.Lock()
	if c.petRenderer != nil && c.petRenderer.Mode == "kitty" {
		c.petKittyPending = entry.payload.Frames[c.petFrameIdx%len(entry.payload.Frames)]
	}
	c.petLock.Unlock()
}

// Writes a queued kitty frame after the prompt finished its screen diff
func (c *CLI) petFlushKittyFrame() {
	c.petLock.Lock()
	frame := c.petKittyPending
	c.petKittyPending = ""
	c.petLock.Unlock()
	if frame == "" || c.app == nil {
		return
	}
	_ = c.app.WriteRaw(frame)
}

// State of the current pet frame and whether kitty draws it, false when no pet shows
func (c *CLI) petView() (string, bool, bool) {
	c.petLock.Lock()
	shown := c.petEnabled && c.petRenderer != nil
	kitty := shown && c.petRenderer.Mode == "kitty"
	c.petLock.Unlock()
	if !shown {
		return "", false, false
	}
	return c.derivePetState(), kitty, true
}

// Styled fragments of the current pet frame, none when no pet shows
func (c *CLI) petFragments() []styledFragment {
	state, kitty, shown := c.petView()
	if !shown {
		return []styledFragment{}
	}
	frags := []styledFragment{}
	if kitty {
		entry := c.petKittyPayloadFor(state)
		if entry == nil {
			return []styledFragment{}
		}
		color := pet.KittyColorHex(entry.imageID)
		for y, row := range entry.payload.Placeholder {
			if y > 0 {
				frags = append(frags, styledFragment{"", "\n"})
			}
			frags = append(frags, styledFragment{"fg:" + color, row})
		}
		return frags
	}
	c.petLock.Lock()
	grids := c.petFramesFor(state)
	var grid [][]pet.Cell
	if len(grids) > 0 {
		grid = grids[c.petFrameIdx%len(grids)]
	}
	c.petLock.Unlock()
	if len(grids) == 0 {
		return []styledFragment{}
	}
	hex := func(p [4]uint8) string { return fmt.Sprintf("#%02x%02x%02x", p[0], p[1], p[2]) }
	for y, row := range grid {
		if y > 0 {
			frags = append(frags, styledFragment{"", "\n"})
		}
		for _, cell := range row {
			top, bottom := cell[0], cell[1]
			topOpaque, bottomOpaque := top[3] >= 32, bottom[3] >= 32
			switch {
			case !topOpaque && !bottomOpaque:
				frags = append(frags, styledFragment{"", " "})
			case topOpaque && bottomOpaque:
				frags = append(frags, styledFragment{"fg:" + hex(top) + " bg:" + hex(bottom), "▀"})
			case topOpaque:
				frags = append(frags, styledFragment{"fg:" + hex(top), "▀"})
			default:
				frags = append(frags, styledFragment{"fg:" + hex(bottom), "▄"})
			}
		}
	}
	return frags
}

// Visible rows of the pet window, zero collapsing it when no pet shows
func (c *CLI) petWidgetHeight() int {
	state, kitty, shown := c.petView()
	if !shown {
		return 0
	}
	if kitty {
		if entry := c.petKittyPayloadFor(state); entry != nil {
			return entry.payload.Rows
		}
		return 0
	}
	c.petLock.Lock()
	defer c.petLock.Unlock()
	if grids := c.petFramesFor(state); len(grids) > 0 {
		return len(grids[0])
	}
	return 0
}

// Advances the pet frame and repaints on a timer while the animation runs
func (c *CLI) petAnimLoop(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		case <-time.After(petFrameInterval):
		}
		c.uiMu.Lock()
		broken := c.terminalIOBroken
		c.uiMu.Unlock()
		if broken {
			c.petLock.Lock()
			c.petAnimRunning = false
			c.petLock.Unlock()
			return
		}
		if now := monotonic(); now-c.petCfgChecked >= petConfigIntervalSec {
			c.petCfgChecked = now
			c.petResolveConfig()
		}
		c.petLock.Lock()
		enabled := c.petEnabled
		kitty := false
		if enabled {
			c.petFrameIdx++
			kitty = c.petRenderer != nil && c.petRenderer.Mode == "kitty"
		}
		c.petLock.Unlock()
		if !enabled {
			continue
		}
		if kitty {
			c.petQueueKittyFrame()
		}
		if app := c.app; app != nil {
			if err := app.Invalidate(); err != nil && errors.Is(err, syscall.EIO) {
				c.markTerminalIOBroken("pet_anim")
				return
			}
		}
	}
}

// Starts the pet animation goroutine once
func (c *CLI) petStartAnim() {
	c.petLock.Lock()
	running := c.petAnimRunning
	c.petLock.Unlock()
	if running {
		return
	}
	c.petResolveConfig()
	if _, kitty, shown := c.petView(); shown && kitty {
		c.petQueueKittyFrame()
	}
	stop, done := make(chan struct{}), make(chan struct{})
	c.petLock.Lock()
	c.petAnimRunning, c.petAnimStop, c.petAnimDone = true, stop, done
	c.petLock.Unlock()
	go c.petAnimLoop(stop, done)
}

// Stops the pet animation goroutine, waiting for it briefly
func (c *CLI) petStopAnim() {
	c.petLock.Lock()
	stop, done := c.petAnimStop, c.petAnimDone
	c.petAnimRunning, c.petAnimStop, c.petAnimDone = false, nil, nil
	c.petLock.Unlock()
	if stop != nil {
		close(stop)
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Push to talk key label every voice hint advertises, the cached one or Ctrl+B
func (c *CLI) voiceRecordKeyLabel() string {
	if c.voiceRecordKeyDisplayCache != "" {
		return c.voiceRecordKeyDisplayCache
	}
	return "Ctrl+B"
}

// Fills the voice label cache from a raw voice.record_key so the label matches the live binding
func (c *CLI) setVoiceRecordKeyCache(raw any) {
	defer func() {
		if recover() != nil {
			c.voiceRecordKeyDisplayCache = "Ctrl+B"
		}
	}()
	c.voiceRecordKeyDisplayCache = formatVoiceRecordKeyForStatus(raw)
}

// Voice status bar fragments of the interactive TUI
func (c *CLI) getVoiceStatusFragments() []styledFragment { return c.getVoiceStatusFragmentsAt(nil) }

// Voice status bar fragments at a width, the live terminal width when nil
func (c *CLI) getVoiceStatusFragmentsAt(width *int) []styledFragment {
	if width != nil && *width == 0 {
		width = nil
	}
	compact := c.useMinimalTUIChrome(width)
	label := c.voiceRecordKeyLabel()
	c.voiceLock.Lock()
	recording, processing, tts, continuous := c.voiceRecording, c.voiceProcessing, c.voiceTTS, c.voiceContinuous
	c.voiceLock.Unlock()
	switch {
	case recording && compact:
		return []styledFragment{{"class:voice-status-recording", " " + c.t("cli.status_bar.voice_rec", nil) + " "}}
	case recording:
		return []styledFragment{{"class:voice-status-recording", " " + c.t("cli.status_bar.voice_rec_stop_hint", map[string]any{"shortcut": label}) + " "}}
	case processing && compact:
		return []styledFragment{{"class:voice-status", " " + c.t("cli.status_bar.voice_stt", nil) + " "}}
	case processing:
		return []styledFragment{{"class:voice-status", " " + c.t("cli.status_bar.voice_transcribing", nil) + " "}}
	case compact:
		return []styledFragment{{"class:voice-status", " 🎤 " + label + " "}}
	}
	ttsLabel, contLabel := "", ""
	if tts {
		ttsLabel = c.t("cli.status_bar.voice_tts_on", nil)
	}
	if continuous {
		contLabel = c.t("cli.status_bar.voice_continuous", nil)
	}
	return []styledFragment{{"class:voice-status", " " + c.t("cli.status_bar.voice_mode_hint", map[string]any{"tts": ttsLabel, "cont": contLabel, "shortcut": label}) + " "}}
}

// Goal segment with its turn count while a goal is active, empty otherwise
func (c *CLI) statusBarGoalSegment(snapshot map[string]any) string {
	if !cliTruthy(snapshot["goal_active"]) {
		return ""
	}
	used, maxTurns := int(snapshotNumber(snapshot["goal_turns_used"])), int(snapshotNumber(snapshot["goal_max_turns"]))
	if maxTurns != 0 {
		return c.t("cli.status_bar.goal_turns", map[string]any{"used": used, "max": maxTurns})
	}
	return c.t("cli.status_bar.goal", nil)
}

// Visible status bar fields from display.status_bar.fields, nil when the bar is not customized
func (c *CLI) getStatusBarFieldSet() map[string]bool {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	if c.statusBarFieldSetLoaded {
		return c.statusBarFieldSet
	}
	var result map[string]bool
	display, _ := CLIConfig(c.ctx)["display"].(map[string]any)
	statusBar, _ := display["status_bar"].(map[string]any)
	if fields, ok := statusBar["fields"].([]any); ok && len(fields) > 0 {
		result = map[string]bool{}
		for _, f := range fields {
			result[pyrepr.Str(f)] = true
		}
	}
	c.statusBarFieldSet, c.statusBarFieldSetLoaded = result, true
	return result
}

// Ordered status bar segments for one width tier, shared by the plain and the styled renderer
func (c *CLI) statusBarSegments(snapshot map[string]any, width int, fieldSet map[string]bool, yoloActive, styled bool) [][]styledFragment {
	modelShort := cliStr(snapshot["model_short"], "")
	durationLabel := cliStr(snapshot["duration"], "")
	goalSegment := c.statusBarGoalSegment(snapshot)
	focusLabel := cliStr(snapshot["focus_label"], "")
	ok := func(name string) bool { return fieldSet == nil || fieldSet[name] }
	segs := [][]styledFragment{}
	add := func(name, style, text string) {
		if ok(name) {
			segs = append(segs, []styledFragment{{style, text}})
		}
	}
	addCount := func(name, key, glyph string, style func(float64) string) {
		count := snapshotNumber(snapshot[key])
		if count == 0 {
			return
		}
		st := statusBarStrong
		if style != nil {
			st = style(count)
		}
		add(name, st, glyph+" "+pyrepr.Str(snapshot[key]))
	}
	if ok("model") {
		if styled {
			segs = append(segs, []styledFragment{{statusBarClass, " ☤ "}, {statusBarStrong, modelShort}})
		} else {
			segs = append(segs, []styledFragment{{"", "☤ " + modelShort}})
		}
	}
	narrow, wide := width < 52, width >= 76
	if narrow {
		add("duration", statusBarDim, durationLabel)
	} else {
		var percent *int
		if raw := snapshot["context_percent"]; raw != nil {
			p := int(snapshotNumber(raw))
			percent = &p
		}
		mark := ""
		if cliTruthy(snapshot["context_estimated"]) {
			mark = "~"
		}
		percentLabel := "--"
		if percent != nil {
			percentLabel = fmt.Sprintf("%s%d%%", mark, *percent)
		}
		if wide && ok("context_detail") {
			contextLabel := c.t("cli.status_bar.ctx_unknown", nil)
			if length := int(snapshotNumber(snapshot["context_length"])); length != 0 {
				pin := ""
				if cliTruthy(snapshot["context_pinned"]) {
					pin = c.t("cli.status_bar.ctx_pinned", nil)
				}
				contextLabel = mark + provider.FormatTokenCountCompact(int(snapshotNumber(snapshot["context_tokens"]))) + "/" + formatContextLength(length) + pin
			}
			segs = append(segs, []styledFragment{{statusBarDim, contextLabel}})
		}
		if ok("context_pct") {
			barStyle := c.statusBarContextStyle(percent)
			if wide && styled {
				segs = append(segs, []styledFragment{{barStyle, c.buildContextBar(percent, 10)}, {statusBarDim, " "}, {barStyle, percentLabel}})
			} else {
				segs = append(segs, []styledFragment{{barStyle, percentLabel}})
			}
		}
		precision := 0
		if wide {
			precision = 1
		}
		if pct, label, has := c.cacheHitRate(snapshot, precision); has {
			add("cache_hit", c.cacheHitRateStyle(pct), label)
		}
		if wide {
			for _, row := range [][3]string{{"latency", "avg_latency_label", "◷"}, {"tps", "avg_velocity_label", "↑"}} {
				if label := cliStr(snapshot[row[1]], ""); label != "" {
					add(row[0], statusBarDim, row[2]+" "+label)
				}
			}
		}
		addCount("compressions", "compressions", "🗜️", compressionCountStyle)
		addCount("bg_tasks", "active_background_tasks", "▶", nil)
		addCount("bg_processes", "active_background_processes", "⚙", nil)
		addCount("bg_subagents", "active_background_subagents", "⛓", nil)
	}
	if goalSegment != "" {
		add("goal", statusBarStrong, goalSegment)
	}
	if gitBranch := cliStr(snapshot["git_branch"], ""); gitBranch != "" {
		add("git_branch", statusBarDim, "⎇ "+gitBranch)
	}
	if !narrow {
		add("duration", statusBarDim, durationLabel)
	}
	if wide {
		for _, name := range []string{"prompt_elapsed", "idle_since"} {
			if label := cliStr(snapshot[name], ""); label != "" {
				add(name, statusBarDim, label)
			}
		}
	}
	if focusLabel != "" {
		add("focus", statusBarStrong, focusLabel)
	}
	if yoloActive {
		add("yolo", "class:status-bar-yolo", c.t("cli.status_bar.yolo_badge", nil))
	}
	if wide {
		if total := int(snapshotNumber(snapshot["session_total_tokens"])); total != 0 && fieldSet != nil && fieldSet["total_tokens"] {
			segs = append(segs, []styledFragment{{statusBarDim, "Σ" + provider.FormatTokenCountCompact(total)}})
		}
	}
	return segs
}

// Reports whether the approval bypass is on for this session or frozen on at process start
func (c *CLI) isSessionYoloActive() bool {
	if tools.YoloModeFrozen() {
		return true
	}
	sessionKey := c.sessionID
	if sessionKey == "" {
		sessionKey = "default"
	}
	return tools.IsSessionYoloEnabled(sessionKey)
}

// Model name a failed status bar render falls back to
func (c *CLI) statusBarFallbackModel() string {
	if c.model != "" {
		return c.model
	}
	return "Icebath"
}

// Compact one line session status text for the TUI footer
func (c *CLI) buildStatusBarText(width *int) (text string) {
	defer func() {
		if recover() != nil {
			text = "☤ " + c.statusBarFallbackModel()
		}
	}()
	snapshot := statusBarSnapshotFor(c)
	w := tuiTerminalWidthFor(c)
	if width != nil {
		w = *width
	}
	batteryLabel := cliStr(snapshot["battery_label"], "")
	fieldSet := c.getStatusBarFieldSet()
	sessionTitle := ""
	if fieldSet == nil || fieldSet["title"] {
		sessionTitle = cliStr(snapshot["session_title"], "")
	}
	parts := []string{}
	for _, seg := range c.statusBarSegments(snapshot, w, fieldSet, c.isSessionYoloActive(), false) {
		var b strings.Builder
		for _, frag := range seg {
			b.WriteString(frag.Text)
		}
		parts = append(parts, b.String())
	}
	if len(parts) == 0 {
		parts = []string{"☤ " + cliStr(snapshot["model_short"], "")}
	}
	if batteryLabel != "" {
		parts = append([]string{batteryLabel}, parts...)
	}
	switch {
	case w < 52 && batteryLabel != "":
		text = parts[0] + " │ " + strings.Join(parts[1:], " · ")
	case w < 52:
		text = strings.Join(parts, " · ")
	case w < 76:
		text = strings.Join(parts, " · ")
	default:
		text = strings.Join(parts, " │ ")
	}
	return rightAlignStatusTitle(text, sessionTitle, w)
}

// Styled status bar fragments, none while the bar is hidden or a picker owns the bottom of the screen
func (c *CLI) getStatusBarFragments() (frags []styledFragment) {
	c.uiMu.Lock()
	hidden := !c.statusBarVisible || c.modelPickerState != nil || c.commandPaletteState != nil
	c.uiMu.Unlock()
	if hidden {
		return []styledFragment{}
	}
	if c.modernLayout {
		return statusLineFragments(c)
	}
	defer func() {
		if recover() != nil {
			frags = []styledFragment{{statusBarClass, " " + c.buildStatusBarText(nil) + " "}}
		}
	}()
	snapshot := statusBarSnapshotFor(c)
	width := tuiTerminalWidthFor(c)
	fieldSet := c.getStatusBarFieldSet()
	ok := func(name string) bool { return fieldSet == nil || fieldSet[name] }
	sessionTitle := ""
	if ok("title") {
		sessionTitle = cliStr(snapshot["session_title"], "")
	}
	segs := c.statusBarSegments(snapshot, width, fieldSet, c.isSessionYoloActive(), true)
	if len(segs) == 0 {
		segs = [][]styledFragment{{{statusBarClass, " ☤ "}, {statusBarStrong, cliStr(snapshot["model_short"], "")}}}
	}
	sep := " │ "
	if width < 76 {
		sep = " · "
	}
	frags = []styledFragment{}
	for _, seg := range segs {
		if len(frags) > 0 {
			frags = append(frags, styledFragment{statusBarDim, sep})
		}
		frags = append(frags, seg...)
	}
	if indicator := c.stashIndicator(); indicator != "" && ok("stash") {
		frags = append(frags, styledFragment{statusBarDim, " · "}, styledFragment{statusBarStrong, indicator})
	}
	frags = append(frags, styledFragment{statusBarClass, " "})
	if batteryLabel := cliStr(snapshot["battery_label"], ""); batteryLabel != "" && ok("battery") {
		frags = append([]styledFragment{{statusBarClass, " "}, {batteryStatusStyle(cliStr(snapshot["battery_category"], "dim")), batteryLabel}, {statusBarDim, " │"}}, frags...)
	}
	frags = rightAlignStatusTitleFragments(frags, sessionTitle, width)
	if vimLabel := c.vimModeLabel(); vimLabel != "" {
		frags = append(frags, styledFragment{statusBarDim, " │ "}, styledFragment{statusBarStrong, vimLabel}, styledFragment{statusBarClass, " "})
	}
	totalWidth := 0
	var plain strings.Builder
	for _, frag := range frags {
		totalWidth += statusBarDisplayWidth(frag.Text)
		plain.WriteString(frag.Text)
	}
	if totalWidth > width {
		return []styledFragment{{statusBarClass, trimStatusBarText(plain.String(), width)}}
	}
	return frags
}

// Pin indicator of the stashed drafts, empty when nothing is stashed or no stash exists
func (c *CLI) stashIndicator() string {
	if c.promptStash == nil {
		return ""
	}
	return c.promptStash.Indicator()
}

// Age of a stashed draft in words
func (c *CLI) fmtStashAge(stashedAt float64) string {
	secs := int(monotonic() - stashedAt)
	if secs < 10 {
		return c.t("cli.shared.just_now", nil)
	}
	if secs < 90 {
		return c.t("cli.shared.ago_seconds", map[string]any{"seconds": secs})
	}
	mins := secs / 60
	if mins < 60 {
		return c.t("cli.shared.ago_minutes", map[string]any{"minutes": mins})
	}
	return c.t("cli.shared.ago_hours", map[string]any{"hours": mins / 60})
}

// Styled fragments of the stash panel box, every measurement in display cells
func (c *CLI) renderStashPanel(stashList []map[string]any, cursor, width int) []styledFragment {
	cw := statusBarDisplayWidth
	boxWidth := max(12, min(width-4, 80))
	n := len(stashList)
	headerKey := "cli.status_bar.stash_header_other"
	if n == 1 {
		headerKey = "cli.status_bar.stash_header_one"
	}
	headerPrefix := "╭─ " + c.t(headerKey, map[string]any{"count": n}) + " "
	headerSuffix := " Ctrl+S ─╮"
	footerPrefix := "╰"
	footerSuffix := " " + c.t("cli.status_bar.stash_footer", nil) + " ─╯"
	if cw(headerPrefix)+cw(headerSuffix) > boxWidth {
		headerPrefix = fmt.Sprintf("╭─ 📌 %d ", n)
		headerSuffix = "─╮"
	}
	if cw(footerPrefix)+cw(footerSuffix) > boxWidth {
		footerSuffix = " " + c.t("cli.status_bar.stash_footer_compact", nil) + " ─╯"
	}
	if cw(footerPrefix)+cw(footerSuffix) > boxWidth {
		footerSuffix = "─╯"
	}
	headerDashes := max(0, boxWidth-cw(headerPrefix)-cw(headerSuffix))
	footerDashes := max(0, boxWidth-cw(footerPrefix)-cw(footerSuffix))
	inner := boxWidth - 2
	frags := []styledFragment{}
	line := func(text, style string) {
		frags = append(frags, styledFragment{style, trimStatusBarText(text, boxWidth) + "\n"})
	}
	line(headerPrefix+strings.Repeat("─", headerDashes)+headerSuffix, "class:subagent-border")
	for i, item := range stashList {
		age := c.fmtStashAge(snapshotNumber(item["stashed_at"]))
		marker := " "
		if i == cursor {
			marker = "►"
		}
		prefix := fmt.Sprintf(" %s [%d] %s ", marker, i+1, pystr.Ljust(age, 10))
		if cw(prefix) > inner-2 {
			prefix = fmt.Sprintf(" %s [%d] ", marker, i+1)
		}
		avail := max(0, inner-cw(prefix)-1)
		preview := trimStatusBarText(cliStr(item["preview"], ""), avail)
		preview += strings.Repeat(" ", max(0, avail-cw(preview)))
		if i == cursor {
			frags = append(frags, styledFragment{"class:subagent-selected", trimStatusBarText("│"+prefix+preview+" │", boxWidth) + "\n"})
			continue
		}
		frags = append(frags, styledFragment{"class:subagent-border", "│"}, styledFragment{"class:subagent-sub", prefix + preview + " "},
			styledFragment{"class:subagent-border", "│\n"})
	}
	line(footerPrefix+strings.Repeat("─", footerDashes)+footerSuffix, "class:subagent-border")
	return frags
}
