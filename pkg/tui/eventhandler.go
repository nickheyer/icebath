package tui

import (
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	startupPollInterval  = 100 * time.Millisecond
	startupPollLimit     = 40
	delegationFetchEvery = 5000
)

// App surface the gateway event handler drives, the main model in production
type eventApp interface {
	sys(text string)
	appendMessage(m msg)
	panel(title string, sections []panelSection)
	setHistoryItems(update func([]msg) []msg)
	setInput(update func(string) string)
	enqueue(text, display string, destination submissionDestination)
	submit(text string) tea.Cmd
	submitLiteral(text string, attachments []imageAttachment) tea.Cmd
	newSession(then func() tea.Cmd) tea.Cmd
	resumeByID(id string, then func(err error) tea.Cmd) tea.Cmd
	setCatalog(c *slashCatalog)
	setVoiceEnabled(on bool)
	setVoiceRecording(on bool)
	setVoiceProcessing(on bool)
	recoverSidRef() *string
	bellOnComplete() bool
	bellOnPrompt() bool
	uncaught(err error)
}

// Slash command catalog of commands.catalog
type slashCatalog struct {
	Canon      map[string]string
	Categories []slashCategory
	Pairs      [][2]string
	SkillCount int
	Sub        map[string][]string
}

// Named group of catalog pairs
type slashCategory struct {
	Name  string
	Pairs [][2]string
}

// Single fetch of the full config, shared by every reader once it answered
type fullConfigFlight struct {
	started bool
	done    bool
	value   map[string]any
	waiters []func(map[string]any) tea.Cmd
}

// Batched status of the thinking stream firing
type thinkingStatusMsg struct{ gen int }

// Startup query check of the session, waited the number of polls so far
type startupPromptMsg struct{ waited int }

// Gateway event dispatch over the shared store, the turn controller and the app
type eventHandler struct {
	store *tuiStore
	turn  *turnController
	gw    turnGateway
	app   eventApp

	startupResumeID string
	startupQuery    string
	startupImage    string

	pendingThinkingStatus     string
	thinkingGen               int
	thinkingArmed             bool
	startupPromptSubmitted    bool
	persistedAbandonedClarify map[string]bool
	openedChallenges          map[string]bool
	lastDelegationFetchAt     float64
	config                    fullConfigFlight
	agentsNudgeEnabled        bool
	agentsNudgeConfigFetched  bool
	agentsNudgedThisTurn      bool

	cmds       []tea.Cmd
	microtasks []func() tea.Cmd
}

// Handler of one app, with the command that starts syncing the theme to the terminal colors
func newEventHandler(store *tuiStore, turn *turnController, gw turnGateway, app eventApp) (*eventHandler, tea.Cmd) {
	env := func(name string) string { return strings.TrimFunc(os.Getenv(name), isJSSpace) }
	h := &eventHandler{
		store: store, turn: turn, gw: gw, app: app,
		startupResumeID: env("ICEBATH_TUI_RESUME"), startupQuery: env("ICEBATH_TUI_QUERY"), startupImage: env("ICEBATH_TUI_IMAGE"),
		persistedAbandonedClarify: map[string]bool{}, openedChallenges: map[string]bool{}, agentsNudgeEnabled: true,
	}
	return h, store.syncThemeToTerminalBackground()
}

func (h *eventHandler) queue(cmd tea.Cmd) {
	if cmd != nil {
		h.cmds = append(h.cmds, cmd)
	}
}

// Runs fn after the current dispatch, the way a resolved promise continues
func (h *eventHandler) later(fn func() tea.Cmd) { h.microtasks = append(h.microtasks, fn) }

// Handles the messages the event handler owns, false for any other message
func (h *eventHandler) update(m tea.Msg) (bool, tea.Cmd) {
	switch m := m.(type) {
	case gatewayEventMsg:
		h.handle(m)
	case asyncDoneMsg:
		h.queue(m.resume())
	case turnTimerMsg:
		h.turn.fire(m)
	case thinkingStatusMsg:
		if h.thinkingArmed && m.gen == h.thinkingGen {
			h.thinkingArmed = false
			status := h.pendingThinkingStatus
			if status == "" {
				status = h.store.statusFromBusy()
			}
			h.store.ui.Status = status
		}
	case startupPromptMsg:
		h.queue(h.runStartupPrompt(m.waited))
	case tea.BackgroundColorMsg:
		h.queue(h.store.onTerminalBackground(m.String()))
	case tea.ForegroundColorMsg:
		h.queue(h.store.onTerminalForeground(m.String()))
	case bootBackgroundRecheckMsg:
		h.queue(h.store.onBootBackgroundRecheck())
	case macAppearanceProbeMsg:
		h.queue(h.store.onMacAppearanceProbe())
	default:
		return false, nil
	}
	for len(h.microtasks) > 0 {
		next := h.microtasks[0]
		h.microtasks = h.microtasks[1:]
		h.queue(next())
	}
	cmds := append(h.cmds, h.turn.takeCmds()...)
	h.cmds = nil
	return true, tea.Batch(cmds...)
}

// Gateway request that prints its own failure and hands nil to then
func (h *eventHandler) rpc(method string, params map[string]any, then func(map[string]any) tea.Cmd) tea.Cmd {
	return gatewayCall(h.gw, method, params, func(v any, err error) tea.Cmd {
		var result map[string]any
		switch {
		case err != nil:
			h.app.sys("error: " + rpcErrorMessage(err))
		case asRPCResult(v) == nil:
			h.app.sys("error: " + tr("session.common.invalidResponse", method))
		default:
			result = asRPCResult(v)
		}
		if then == nil {
			return nil
		}
		return then(result)
	})
}

// Raw gateway request whose failure is dropped
func (h *eventHandler) quietRequest(method string, params map[string]any) tea.Cmd {
	return gatewayCall(h.gw, method, params, func(any, error) tea.Cmd { return nil })
}

// Calls fn with the full config, fetched once and nil when the fetch failed
func (h *eventHandler) withFullConfig(fn func(map[string]any) tea.Cmd) {
	if h.config.done {
		cfg := h.config.value
		h.later(func() tea.Cmd { return fn(cfg) })
		return
	}
	h.config.waiters = append(h.config.waiters, fn)
	if h.config.started {
		return
	}
	h.config.started = true
	h.queue(h.rpc("config.get", map[string]any{"key": "full"}, func(cfg map[string]any) tea.Cmd {
		h.config.done, h.config.value = true, cfg
		waiters := h.config.waiters
		h.config.waiters = nil
		cmds := make([]tea.Cmd, 0, len(waiters))
		for _, w := range waiters {
			cmds = append(cmds, w(cfg))
		}
		return tea.Batch(cmds...)
	}))
}

// Nested object of a config path, nil where a step is missing
func configPath(cfg map[string]any, keys ...string) any {
	var cur any = cfg
	for _, k := range keys {
		obj, _ := cur.(map[string]any)
		if obj == nil {
			return nil
		}
		cur = obj[k]
	}
	return cur
}

// Sets the status line, cancelling a batched thinking status
func (h *eventHandler) setStatus(status string) {
	h.pendingThinkingStatus = ""
	h.thinkingArmed = false
	h.thinkingGen++
	h.store.ui.Status = status
}

// Shows a thinking status after one stream batch, the latest one winning
func (h *eventHandler) scheduleThinkingStatus(status string) {
	h.pendingThinkingStatus = status
	if h.thinkingArmed {
		return
	}
	h.thinkingArmed = true
	h.thinkingGen++
	gen := h.thinkingGen
	h.queue(tea.Tick(streamBatch, func(time.Time) tea.Msg { return thinkingStatusMsg{gen: gen} }))
}

// Rings the terminal bell when a prompt opens and the user asked for it
func (h *eventHandler) ringPromptBell() {
	if h.app.bellOnPrompt() {
		h.queue(tea.Raw("\x07"))
	}
}

// Records an unanswered clarify as a transcript block once and closes its card
func (h *eventHandler) flushAbandonedClarify() {
	clarify := h.store.overlay.Clarify
	if clarify == nil || h.persistedAbandonedClarify[clarify.RequestID] {
		return
	}
	h.persistedAbandonedClarify[clarify.RequestID] = true
	answers := clarify.Answers
	if answers == nil {
		answers = map[string]string{}
	}
	h.app.appendMessage(msg{Role: "system", Text: formatAbandonedClarify(clarify.Questions, answers, tr("gatewayMsg.clarify.timedOut"))})
	h.store.overlay.Clarify = nil
}

// Wire form of a subagent as spawn_tree.save stores it
func subagentJSON(s subagentProgress) map[string]any {
	out := map[string]any{"depth": float64(s.Depth), "goal": s.Goal, "id": s.ID, "index": float64(s.Index), "notes": s.Notes, "status": s.Status, "taskCount": float64(s.TaskCount), "thinking": s.Thinking, "toolCount": float64(s.ToolCount), "tools": s.Tools, "parentId": nil}
	if s.ParentID != "" {
		out["parentId"] = s.ParentID
	}
	optional := map[string]*float64{"apiCalls": s.APICalls, "costUsd": s.CostUSD, "durationSeconds": s.DurationSeconds, "inputTokens": s.InputTokens, "iteration": s.Iteration, "outputTokens": s.OutputTokens, "reasoningTokens": s.ReasoningTokens, "startedAt": s.StartedAt}
	for k, v := range optional {
		if v != nil {
			out[k] = *v
		}
	}
	for k, v := range map[string]string{"delegationId": s.DelegationID, "model": s.Model, "summary": s.Summary} {
		if v != "" {
			out[k] = v
		}
	}
	for k, v := range map[string][]string{"filesRead": s.FilesRead, "filesWritten": s.FilesWritten, "toolsets": s.Toolsets} {
		if v != nil {
			out[k] = v
		}
	}
	if s.OutputTail != nil {
		tail := make([]map[string]any, len(s.OutputTail))
		for i, e := range s.OutputTail {
			tail[i] = map[string]any{"isError": e.IsError, "preview": e.Preview, "tool": e.Tool}
		}
		out["outputTail"] = tail
	}
	return out
}

// Saves a finished fan out to disk so replay survives a restart
func (h *eventHandler) persistSpawnTree(subagents []subagentProgress, sessionID string) tea.Cmd {
	startedAt := 0.0
	for _, s := range subagents {
		if s.StartedAt == nil || *s.StartedAt == 0 {
			continue
		}
		if startedAt == 0 {
			startedAt = *s.StartedAt
		} else {
			startedAt = min(startedAt, *s.StartedAt)
		}
	}
	top := []string{}
	for _, s := range topLevelSubagents(subagents) {
		if s.Goal != "" && len(top) < 2 {
			top = append(top, s.Goal)
		}
	}
	label := jsString(float64(len(subagents))) + " subagents"
	if len(top) > 0 {
		label = strings.Join(top, " · ")
	}
	if sessionID == "" {
		sessionID = "default"
	}
	var started any
	if startedAt != 0 {
		started = startedAt / 1000
	}
	wire := make([]map[string]any, len(subagents))
	for i, s := range subagents {
		wire[i] = subagentJSON(s)
	}
	return h.rpc("spawn_tree.save", map[string]any{"finished_at": h.store.nowMillis() / 1000, "label": jsSlice(label, 0, 120), "session_id": sessionID, "started_at": started, "subagents": wire}, nil)
}

// Reads display.tui_agents_nudge once, a false turning the agents nudge off
func (h *eventHandler) ensureAgentsNudgeConfig() {
	if h.agentsNudgeConfigFetched {
		return
	}
	h.agentsNudgeConfigFetched = true
	h.withFullConfig(func(cfg map[string]any) tea.Cmd {
		if configPath(cfg, "config", "display", "tui_agents_nudge") == false {
			h.agentsNudgeEnabled = false
		}
		return nil
	})
}

// Points at the agents overlay once a turn when subagents start and it is closed
func (h *eventHandler) maybeNudgeAgents() {
	h.ensureAgentsNudgeConfig()
	if !h.agentsNudgeEnabled || h.agentsNudgedThisTurn || h.store.overlay.Agents {
		return
	}
	h.agentsNudgedThisTurn = true
	h.turn.pushActivity(tr("gatewayMsg.agents.workingNudge"), "info", "")
}

// Fetches the delegation caps, at most every five seconds unless forced
func (h *eventHandler) refreshDelegationStatus(force bool) {
	now := h.store.nowMillis()
	if !force && now-h.lastDelegationFetchAt < delegationFetchEvery {
		return
	}
	h.lastDelegationFetchAt = now
	h.queue(h.rpc("delegation.status", map[string]any{}, func(r map[string]any) tea.Cmd {
		h.store.applyDelegationStatus(r)
		return nil
	}))
}

// Loads the slash catalog for a session, dropped when the session changed meanwhile
func (h *eventHandler) loadSlashCatalog(sid string) tea.Cmd {
	params := map[string]any{}
	if sid != "" {
		params["session_id"] = sid
	}
	return h.rpc("commands.catalog", params, func(r map[string]any) tea.Cmd {
		pairs, isList := r["pairs"].([]any)
		if r == nil || !truthyJS(r["pairs"]) || h.store.ui.Sid != sid {
			return nil
		}
		catalog := &slashCatalog{Canon: stringMap(r["canon"]), Pairs: catalogPairs(pairs, isList), Sub: map[string][]string{}}
		if n, isNum := r["skill_count"].(float64); isNum {
			catalog.SkillCount = int(n)
		}
		categories, _ := r["categories"].([]any)
		catalog.Categories = []slashCategory{}
		for _, raw := range categories {
			c, _ := raw.(map[string]any)
			list, listed := c["pairs"].([]any)
			catalog.Categories = append(catalog.Categories, slashCategory{Name: jsValueString(c["name"]), Pairs: catalogPairs(list, listed)})
		}
		sub, _ := r["sub"].(map[string]any)
		for k, v := range sub {
			catalog.Sub[k] = paramStrList(v)
		}
		h.app.setCatalog(catalog)
		if truthyJS(r["warning"]) {
			h.turn.pushActivity(jsValueString(r["warning"]), "warn", "")
		}
		return nil
	})
}

// Name and description pairs of a catalog list
func catalogPairs(list []any, isList bool) [][2]string {
	out := [][2]string{}
	if !isList {
		return out
	}
	for _, raw := range list {
		pair, _ := raw.([]any)
		var p [2]string
		for i := 0; i < 2 && i < len(pair); i++ {
			p[i] = jsValueString(pair[i])
		}
		out = append(out, p)
	}
	return out
}

// Reloads the slash catalog when the skills changed, true when the event was that
func (h *eventHandler) refreshSlashCatalogOnSkillsChange(ev gatewayEventMsg) bool {
	if ev.Type != "skills.changed" {
		return false
	}
	h.queue(h.loadSlashCatalog(h.store.ui.Sid))
	return true
}

// Submits the launch query and image once the first session exists
func (h *eventHandler) scheduleStartupPrompt() {
	if h.startupPromptSubmitted || (h.startupQuery == "" && h.startupImage == "") {
		return
	}
	h.startupPromptSubmitted = true
	h.queue(func() tea.Msg { return startupPromptMsg{} })
}

// Polls for the session, then attaches the startup image and submits the startup query literally
func (h *eventHandler) runStartupPrompt(waited int) tea.Cmd {
	sid := h.store.ui.Sid
	if sid == "" && waited < startupPollLimit {
		return tea.Tick(startupPollInterval, func(time.Time) tea.Msg { return startupPromptMsg{waited: waited + 1} })
	}
	if sid == "" {
		h.app.sys(tr("gatewayMsg.startup.querySkipped"))
		return nil
	}
	destination, err := h.store.captureDestination()
	if err != nil {
		h.app.uncaught(err)
		return nil
	}
	submit := func(attachments []imageAttachment) tea.Cmd {
		if !h.store.isCurrentDestination(destination) {
			h.app.sys(tr("canonical.submit.startupQuerySwitched"))
			return nil
		}
		query := h.startupQuery
		if query == "" {
			query = "What do you see in this image?"
		}
		return h.app.submitLiteral(query, attachments)
	}
	if h.startupImage == "" {
		return submit([]imageAttachment{})
	}
	if h.gw.canonical() {
		return h.store.stageImagePath(h.startupImage, h.gw, destination, func(image *stagedImage, err error) tea.Cmd {
			if err != nil {
				h.app.sys(tr("gatewayMsg.startup.imageAttachFailed", rpcErrorMessage(err)))
				return nil
			}
			return submit([]imageAttachment{image.imageAttachment})
		})
	}
	return h.rpc("image.attach", map[string]any{"path": h.startupImage, "session_id": sid}, func(map[string]any) tea.Cmd {
		return submit([]imageAttachment{})
	})
}

// Starts a fresh session and the startup prompt with the forging status
func (h *eventHandler) forgeSession() tea.Cmd {
	h.store.ui.Status = tr("gatewayMsg.status.forgingSession")
	cmd := h.app.newSession(nil)
	h.scheduleStartupPrompt()
	return cmd
}

// First gateway.ready of a connection, skin, wake listener, catalog and the session to open
func (h *eventHandler) handleReady(skin *gatewaySkin) {
	if skin != nil {
		h.queue(h.store.applySkin(skin))
	}
	if params, report := startupReport.take(); report {
		h.queue(h.quietRequest("shared_metrics.startup_latency", params))
	}
	h.ensureAgentsNudgeConfig()
	if !h.store.wakeUserDisabled {
		h.queue(h.rpc("wake.start", map[string]any{"surface": "tui"}, nil))
	}
	if h.store.ui.Sid == "" {
		h.queue(h.loadSlashCatalog(""))
	}
	if ref := h.app.recoverSidRef(); ref != nil && *ref != "" {
		recoverSid := *ref
		h.queue(h.app.resumeByID(recoverSid, func(err error) tea.Cmd {
			if err != nil {
				h.app.uncaught(err)
				return nil
			}
			if h.store.ui.Sid != "" && *ref == recoverSid {
				*ref = ""
			}
			return nil
		}))
		h.store.ui.Status = tr("gatewayMsg.status.recoveringSession")
		return
	}
	if h.startupResumeID != "" {
		h.store.ui.Status = tr("session.status.resuming")
		h.queue(h.app.resumeByID(h.startupResumeID, nil))
		h.scheduleStartupPrompt()
		return
	}
	h.withFullConfig(func(cfg map[string]any) tea.Cmd {
		if !truthyJS(configPath(cfg, "config", "display", "tui_auto_resume_recent")) {
			return h.forgeSession()
		}
		return h.rpc("session.most_recent", map[string]any{}, func(r map[string]any) tea.Cmd {
			if target := r["session_id"]; truthyJS(target) {
				h.store.ui.Status = tr("gatewayMsg.status.resumingMostRecent")
				cmd := h.app.resumeByID(jsValueString(target), nil)
				h.scheduleStartupPrompt()
				return cmd
			}
			return h.forgeSession()
		})
	})
}

// Shows a gateway notice, its detail line first
func (h *eventHandler) showGatewayNotice(p map[string]any) {
	if !truthyJS(p["text"]) {
		return
	}
	if detail, isStr := p["detail"].(string); isStr && strings.TrimFunc(detail, isJSSpace) != "" {
		h.app.sys(detail)
	}
	n := notice{ID: paramStr(p["id"]), Key: paramStr(p["key"]), Kind: "sticky", Level: "info", Text: jsValueString(p["text"])}
	if p["kind"] == "ttl" {
		n.Kind = "ttl"
	}
	if level, isStr := p["level"].(string); isStr && (level == "error" || level == "info" || level == "success" || level == "warn") {
		n.Level = level
	}
	if ttl, isNum := p["ttl_ms"].(float64); isNum {
		n.TTLms = &ttl
	}
	h.turn.showNotice(n)
}

// Puts the billing step up link where the user is and opens it
func (h *eventHandler) showBillingVerification(p map[string]any) {
	link, _ := p["verification_url"].(string)
	if link == "" {
		return
	}
	h.app.sys(tr("gatewayMsg.billing.openLinkRemoteSpending"))
	h.app.sys(link)
	if truthyJS(p["user_code"]) {
		h.app.sys(tr("gatewayMsg.billing.enterCode", jsValueString(p["user_code"])))
	}
	openExternalURL(link)
}

// Puts the free tier browser check link where the user is, one browser tab per ticket
func (h *eventHandler) showChallenge(p map[string]any) {
	link, _ := p["url"].(string)
	if !truthyJS(p["required"]) || link == "" {
		return
	}
	h.app.sys(stringOr(p["message"], ""))
	h.app.sys(link)
	if !h.openedChallenges[link] {
		h.openedChallenges[link] = true
		openExternalURL(link)
	}
}

// Voice submit mode of the config, draft or direct
func normalizeVoiceSubmitMode(v any) string {
	if s, isStr := v.(string); isStr && strings.ToLower(strings.TrimFunc(s, isJSSpace)) == "draft" {
		return "draft"
	}
	return "direct"
}

// Final transcript of a voice capture, submitted, drafted or queued for the session it was spoken in
func (h *eventHandler) handleVoiceTranscript(p map[string]any) {
	if truthyJS(p["stop_phrase"]) || truthyJS(p["no_speech_limit"]) {
		h.app.setVoiceEnabled(false)
		h.app.setVoiceRecording(false)
		h.app.setVoiceProcessing(false)
		if truthyJS(p["stop_phrase"]) {
			h.app.sys(tr("gatewayMsg.voice.stopPhrase"))
		} else {
			h.app.sys(tr("gatewayMsg.voice.noSpeechLimit"))
		}
		return
	}
	text := strings.TrimFunc(stringOr(p["text"], ""), isJSSpace)
	if text == "" {
		return
	}
	destination, err := h.store.captureDestination()
	if err != nil {
		h.app.uncaught(err)
		return
	}
	h.withFullConfig(func(cfg map[string]any) tea.Cmd {
		if !h.store.isCurrentDestination(destination) {
			h.app.enqueue(text, text, destination)
			return nil
		}
		if normalizeVoiceSubmitMode(configPath(cfg, "config", "voice", "submit_mode")) == "draft" {
			h.app.setInput(func(current string) string {
				if strings.TrimFunc(current, isJSSpace) != "" {
					return strings.TrimRightFunc(current, isJSSpace) + " " + text
				}
				return text
			})
			return nil
		}
		h.app.setInput(func(string) string { return "" })
		return func() tea.Msg {
			return asyncDoneMsg{run: func() tea.Cmd {
				if !h.store.isCurrentDestination(destination) {
					h.app.enqueue(text, text, destination)
					return nil
				}
				h.store.markNextSubmitVoice(text)
				return h.app.submit(text)
			}}
		}
	})
}

// Wake word heard, a fresh session on this profile with voice capture on, else the listener resumed
func (h *eventHandler) handleWakeDetected(p map[string]any) {
	resume := func() tea.Cmd { return h.rpc("wake.resume", map[string]any{}, nil) }
	fail := func(err error) tea.Cmd {
		h.app.sys(tr("gatewayMsg.wake.failed", rpcErrorMessage(err)))
		return resume()
	}
	wakeProfile := strings.TrimFunc(paramStr(p["profile"]), isJSSpace)
	ownProfile := "default"
	if truthyJS(h.store.ui.Info["profile_name"]) {
		ownProfile = jsValueString(h.store.ui.Info["profile_name"])
	}
	if wakeProfile != "" && wakeProfile != ownProfile {
		h.app.sys(tr("gatewayMsg.wake.otherProfile", wakeProfile))
		h.queue(resume())
		return
	}
	arm := func() tea.Cmd {
		sid := h.store.ui.Sid
		if sid == "" {
			return resume()
		}
		h.app.setVoiceEnabled(true)
		record := func() tea.Cmd {
			return h.rpc("voice.record", map[string]any{"action": "start", "session_id": sid}, nil)
		}
		if h.gw.canonical() {
			return gatewayCall(h.gw, "slash.exec", map[string]any{"command": "voice on", "session_id": sid}, func(_ any, err error) tea.Cmd {
				if err != nil {
					return fail(err)
				}
				return record()
			})
		}
		return h.rpc("voice.toggle", map[string]any{"action": "on"}, func(map[string]any) tea.Cmd { return record() })
	}
	if p["start_new_session"] != false {
		h.queue(h.app.newSession(arm))
		return
	}
	h.later(arm)
}
