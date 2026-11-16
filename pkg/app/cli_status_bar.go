package app

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/provider"
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

// Drops missing, negative and absurd provider timings
func finiteTiming(v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || *v < 0 || *v > 1e6 {
		return nil
	}
	return v
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
	a := c.agentRef()
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

// Width of the live prompt, the terminal columns when no application reports one
func (c *CLI) getTUITerminalWidth() int {
	if app := c.app; app != nil {
		if columns, _, ok := app.Size(); ok {
			return columns
		}
	}
	return terminalColumns()
}

// Full viewport width for printed scrollback box rules, floored at thirty two columns
func scrollbackBoxWidth() int { return max(32, terminalColumns()) }

// Reads whether a turn or a slash command runs
func (c *CLI) busyState() (agentRunning, commandRunning bool) {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.agentRunning, c.commandRunning
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
