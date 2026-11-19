package tui

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/tuigateway"

	tea "charm.land/bubbletea/v2"
)

const (
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
	newSession(then func() tea.Cmd) tea.Cmd
	resumeByID(id string, then func(err error) tea.Cmd) tea.Cmd
	setCatalog(c *slashCatalog)
	setVoiceEnabled(on bool)
	setVoiceRecording(on bool)
	setVoiceProcessing(on bool)
	interruptTurn() tea.Cmd
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
	// Commands that run only in messaging chats with their descriptions
	Chat map[string]string
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
	value   tuigateway.ClientConfig
	waiters []func(tuigateway.ClientConfig) tea.Cmd
}

// Batched status of the thinking stream firing
type thinkingStatusMsg struct{ gen int }

// Gateway event dispatch over the shared store, the turn controller and the app
type eventHandler struct {
	store *tuiStore
	turn  *turnController
	gw    turnGateway
	app   eventApp

	startupResumeID string

	pendingThinkingStatus     string
	thinkingGen               int
	thinkingArmed             bool
	persistedAbandonedClarify map[string]bool
	lastDelegationFetchAt     float64
	config                    *fullConfigFlight
	agentsNudgeEnabled        bool
	agentsNudgeConfigFetched  bool
	agentsNudgedThisTurn      bool
	view                      mainView
	// Gateway build the other build line was told for, nil while the gateway runs this build
	toldBuild *string

	cmds       []tea.Cmd
	microtasks []func() tea.Cmd
}

// Handler of one app, with the command that starts syncing the theme to the terminal colors
func newEventHandler(store *tuiStore, turn *turnController, gw turnGateway, app eventApp) (*eventHandler, tea.Cmd) {
	env := func(name string) string { return strings.TrimFunc(os.Getenv(name), isJSSpace) }
	h := &eventHandler{
		store: store, turn: turn, gw: gw, app: app,
		startupResumeID:           env("ICEBATH_TUI_RESUME"),
		persistedAbandonedClarify: map[string]bool{}, agentsNudgeEnabled: true, config: &fullConfigFlight{},
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
		result, isObject := v.(map[string]any)
		switch {
		case err != nil:
			h.app.sys("error: " + rpcErrorMessage(err))
		case !isObject:
			h.app.sys("error: " + misfitText(tr("session.common.invalidResponse", method)))
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

// Calls fn with the full config, fetched once and the defaults when the fetch failed
func (h *eventHandler) withFullConfig(fn func(tuigateway.ClientConfig) tea.Cmd) {
	f := h.config
	if f.done {
		cfg := f.value
		h.later(func() tea.Cmd { return fn(cfg) })
		return
	}
	f.waiters = append(f.waiters, fn)
	if f.started {
		return
	}
	f.started = true
	h.queue(ask(h.rpc, h.app.sys, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "full"}, func(r *tuigateway.ConfigGetResult) tea.Cmd {
		cfg := tuigateway.ClientConfigOf(nil)
		if r != nil && r.Config != nil {
			cfg = *r.Config
		}
		f.done, f.value = true, cfg
		waiters := f.waiters
		f.waiters = nil
		cmds := make([]tea.Cmd, 0, len(waiters))
		for _, w := range waiters {
			cmds = append(cmds, w(cfg))
		}
		return tea.Batch(cmds...)
	}))
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

// Saved form of a subagent as spawn_tree.save stores it
func spawnTreeSubagentOf(s subagentProgress) tuigateway.SpawnTreeSubagent {
	out := tuigateway.SpawnTreeSubagent{Depth: float64(s.Depth), Goal: s.Goal, ID: s.ID, Index: float64(s.Index), Notes: s.Notes, Status: tuigateway.SubagentStatus(s.Status),
		TaskCount: float64(s.TaskCount), Thinking: s.Thinking, ToolCount: float64(s.ToolCount), Tools: s.Tools, ParentID: optionalText(s.ParentID),
		APICalls: s.APICalls, CostUSD: s.CostUSD, DurationSeconds: s.DurationSeconds, InputTokens: s.InputTokens, Iteration: s.Iteration,
		OutputTokens: s.OutputTokens, ReasoningTokens: s.ReasoningTokens, StartedAt: s.StartedAt, DelegationID: s.DelegationID, Model: s.Model,
		Summary: s.Summary, FilesRead: s.FilesRead, FilesWritten: s.FilesWritten, Toolsets: s.Toolsets}
	if s.OutputTail != nil {
		out.OutputTail = make([]tuigateway.SpawnTreeOutput, len(s.OutputTail))
		for i, e := range s.OutputTail {
			out.OutputTail[i] = tuigateway.SpawnTreeOutput{IsError: e.IsError, Preview: e.Preview, Tool: e.Tool}
		}
	}
	return out
}

// Saves a finished fan out to disk so replay survives a restart, a later save replacing its file
func (h *eventHandler) persistSpawnTree(snap spawnSnapshot) tea.Cmd {
	subagents, sessionID := snap.Subagents, snap.SessionID
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
	label := strconv.Itoa(len(subagents)) + " subagents"
	if len(top) > 0 {
		label = strings.Join(top, " · ")
	}
	if sessionID == "" {
		sessionID = "default"
	}
	params := tuigateway.SpawnTreeSaveParams{FinishedAt: ptr(snap.FinishedAt / 1000), Label: ptr(jsSlice(label, 0, 120)), SessionID: &sessionID}
	if startedAt != 0 {
		params.StartedAt = ptr(startedAt / 1000)
	}
	for _, s := range subagents {
		params.Subagents = append(params.Subagents, spawnTreeSubagentOf(s))
	}
	return ask(h.rpc, h.app.sys, tuigateway.SpawnTreeSave, params, done)
}

// State the main view of the reference redraws on, its event handler built anew at each redraw
type mainView struct {
	status       string
	info, usage  unsafe.Pointer
	busy, agents bool
	activity     int
	transcript   int
}

// Main view state as it stands
func (h *eventHandler) mainView() mainView {
	v := mainView{status: h.store.ui.Status, info: reflect.ValueOf(h.store.ui.Info).UnsafePointer(), usage: reflect.ValueOf(h.store.ui.Usage).UnsafePointer(),
		busy: h.store.ui.Busy, agents: h.store.overlay.Agents, transcript: h.store.transcriptGen}
	if n := len(h.store.turn.Activity); n > 0 {
		v.activity = h.store.turn.Activity[n-1].ID
	}
	return v
}

// Starts the per handler state over when the main view changed since the last event, the delegation fetch clock, the full config read and the agents nudge
func (h *eventHandler) renewOnRedraw() {
	v := h.mainView()
	if v == h.view {
		return
	}
	h.view = v
	h.lastDelegationFetchAt = 0
	h.config = &fullConfigFlight{}
	h.agentsNudgeEnabled, h.agentsNudgeConfigFetched, h.agentsNudgedThisTurn = true, false, false
}

// Reads display.tui_agents_nudge once, a false turning the agents nudge off
func (h *eventHandler) ensureAgentsNudgeConfig() {
	if h.agentsNudgeConfigFetched {
		return
	}
	h.agentsNudgeConfigFetched = true
	h.withFullConfig(func(cfg tuigateway.ClientConfig) tea.Cmd {
		if !cfg.Display.AgentsNudge {
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
	h.queue(ask(h.rpc, h.app.sys, tuigateway.DelegationStatus, tuigateway.ProfileParams{}, func(r *tuigateway.DelegationStatusResult) tea.Cmd {
		if r != nil {
			h.store.applyDelegationStatus(*r)
		}
		return nil
	}))
}

// Loads the slash catalog for a session, dropped when the session changed meanwhile
func (h *eventHandler) loadSlashCatalog(sid string) tea.Cmd {
	return ask(h.rpc, h.app.sys, tuigateway.CommandsCatalog, tuigateway.CommandsCatalogParams{SessionID: sid}, func(r *tuigateway.CommandsCatalogResult) tea.Cmd {
		if r == nil || len(r.Pairs) == 0 || h.store.ui.Sid != sid {
			return nil
		}
		catalog := &slashCatalog{Canon: r.Canon, Pairs: r.Pairs, SkillCount: int(r.SkillCount), Categories: []slashCategory{}, Chat: r.ChatCommands}
		for _, c := range r.Categories {
			catalog.Categories = append(catalog.Categories, slashCategory{Name: c.Name, Pairs: c.Pairs})
		}
		h.app.setCatalog(catalog)
		if r.Warning != "" {
			h.turn.pushActivity(r.Warning, "warn", "")
		}
		return nil
	})
}

// Reloads the slash catalog when the skills changed, true when the event was that
func (h *eventHandler) refreshSlashCatalogOnSkillsChange(ev gatewayEventMsg) bool {
	if ev.Type != tuigateway.SkillsChangedEvent.Name() {
		return false
	}
	h.queue(h.loadSlashCatalog(h.store.ui.Sid))
	return true
}

// Starts a fresh session with the forging status, then runs after
func (h *eventHandler) forgeSession(after func() tea.Cmd) tea.Cmd {
	h.store.ui.Status = tr("gatewayMsg.status.forgingSession")
	return h.app.newSession(after)
}

// Records whether the gateway runs another build, returning the line to tell once per gateway build or nil
func (h *eventHandler) noteGatewayBuild(ready tuigateway.GatewayReadyPayload) func() tea.Cmd {
	other := !tuigateway.SameBuild(ready.Build, ready.Contract)
	gatewayOtherBuild.Store(other)
	if !other {
		h.toldBuild = nil
		return func() tea.Cmd { return nil }
	}
	if h.toldBuild != nil && *h.toldBuild == ready.Build {
		return func() tea.Cmd { return nil }
	}
	h.toldBuild = &ready.Build
	return func() tea.Cmd {
		h.app.sys(tr("gatewayMsg.otherBuild.line"))
		return nil
	}
}

// First gateway.ready of a connection, skin, wake listener, catalog and the session to open, then the build line
func (h *eventHandler) handleReady(ready tuigateway.GatewayReadyPayload) {
	tell := h.noteGatewayBuild(ready)
	if skin := gatewaySkinOf(ready.Skin); skin != nil {
		h.queue(h.store.applySkin(skin))
	}
	h.ensureAgentsNudgeConfig()
	if !h.store.wakeUserDisabled && linktier.Linked(linktier.Extras) {
		h.queue(ask(h.rpc, h.app.sys, tuigateway.WakeStart, tuigateway.WakeStartParams{Surface: "tui"}, done[tuigateway.WakeStartResult]))
	}
	if h.store.ui.Sid == "" {
		h.queue(h.loadSlashCatalog(""))
	}
	if ref := h.app.recoverSidRef(); ref != nil && *ref != "" {
		recoverSid := *ref
		h.queue(h.app.resumeByID(recoverSid, func(err error) tea.Cmd {
			if err != nil {
				h.app.uncaught(err)
				return tell()
			}
			if h.store.ui.Sid != "" && *ref == recoverSid {
				*ref = ""
			}
			return tell()
		}))
		h.store.ui.Status = tr("gatewayMsg.status.recoveringSession")
		return
	}
	resumed := func(error) tea.Cmd { return tell() }
	if h.startupResumeID != "" {
		h.store.ui.Status = tr("session.status.resuming")
		h.queue(h.app.resumeByID(h.startupResumeID, resumed))
		return
	}
	h.withFullConfig(func(cfg tuigateway.ClientConfig) tea.Cmd {
		if !cfg.Display.AutoResumeRecent {
			return h.forgeSession(tell)
		}
		return ask(h.rpc, h.app.sys, tuigateway.SessionMostRecent, tuigateway.SessionMostRecentParams{}, func(r *tuigateway.SessionMostRecentResult) tea.Cmd {
			if r != nil && val(r.SessionID) != "" {
				h.store.ui.Status = tr("gatewayMsg.status.resumingMostRecent")
				return h.app.resumeByID(*r.SessionID, resumed)
			}
			return h.forgeSession(tell)
		})
	})
}

// Shows a gateway notice, its detail line first
func (h *eventHandler) showGatewayNotice(p tuigateway.NotificationShowPayload) {
	if p.Text == "" {
		return
	}
	if strings.TrimFunc(p.Detail, isJSSpace) != "" {
		h.app.sys(p.Detail)
	}
	n := notice{ID: p.ID, Key: p.Key, Kind: "sticky", Level: "info", Text: p.Text}
	if p.Kind == "ttl" {
		n.Kind = "ttl"
	}
	if p.Level == "error" || p.Level == "info" || p.Level == "success" || p.Level == "warn" {
		n.Level = p.Level
	}
	if p.TTLMs != nil {
		ttl := float64(*p.TTLMs)
		n.TTLms = &ttl
	}
	h.turn.showNotice(n)
}

// Final transcript of a voice capture, submitted, drafted or queued for the session it was spoken in
func (h *eventHandler) handleVoiceTranscript(p tuigateway.VoiceTranscriptPayload) {
	if p.StopPhrase || p.NoSpeechLimit {
		h.app.setVoiceEnabled(false)
		h.app.setVoiceRecording(false)
		h.app.setVoiceProcessing(false)
		if p.StopPhrase {
			h.app.sys(tr("gatewayMsg.voice.stopPhrase"))
		} else {
			h.app.sys(tr("gatewayMsg.voice.noSpeechLimit"))
		}
		return
	}
	text := strings.TrimFunc(p.Text, isJSSpace)
	if text == "" {
		return
	}
	destination, err := h.store.captureDestination()
	if err != nil {
		h.app.uncaught(err)
		return
	}
	h.withFullConfig(func(cfg tuigateway.ClientConfig) tea.Cmd {
		if !h.store.isCurrentDestination(destination) {
			h.app.enqueue(text, text, destination)
			return nil
		}
		if cfg.Voice.SubmitMode == "draft" {
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
func (h *eventHandler) handleWakeDetected(p tuigateway.WakeDetectedPayload) {
	resume := func() tea.Cmd {
		return ask(h.rpc, h.app.sys, tuigateway.WakeResume, tuigateway.WakeControlParams{}, done[tuigateway.WakeResumeResult])
	}
	fail := func(err error) tea.Cmd {
		h.app.sys(tr("gatewayMsg.wake.failed", rpcErrorMessage(err)))
		return resume()
	}
	wakeProfile := strings.TrimFunc(val(p.Profile), isJSSpace)
	ownProfile := "default"
	if name := val(infoOf(h.store.ui.Info).ProfileName); name != "" {
		ownProfile = name
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
			params := tuigateway.VoiceRecordParams{Action: "start", SessionID: sid}
			return ask(h.rpc, h.app.sys, tuigateway.VoiceRecord, params, done[tuigateway.VoiceRecordResult])
		}
		return call(h.gw, tuigateway.VoiceToggle, tuigateway.VoiceToggleParams{Action: "on", SessionID: sid}, func(_ *tuigateway.VoiceToggleResult, err error) tea.Cmd {
			if err != nil {
				return fail(err)
			}
			return record()
		})
	}
	if p.StartNewSession {
		h.queue(h.app.newSession(arm))
		return
	}
	h.later(arm)
}
