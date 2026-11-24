package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tuigateway"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/clip"
	"github.com/nickheyer/icebath/pkg/ui/gfx"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	uitheme "github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/tty"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	// Longest the exit waits for the session detach before it quits anyway
	exitCloseLimit = 1500 * time.Millisecond
	// Window in which a second interrupt key on an idle empty prompt quits
	exitConfirmWindow = 2 * time.Second
	// Gap between refreshes of the agent, process and live session snapshots while a turn runs or the dock shows something, and the
	// longer gap while everything is idle
	refreshEvery     = 1500 * time.Millisecond
	idleRefreshEvery = 10 * time.Second
)

// Session detach finished or timed out, the program quits with the status
type exitMsg struct{ code int }

// Periodic refresh of the session rosters, of the chain of ticks that was current when it was scheduled
type refreshTickMsg struct{ chain int }

// A second after a key was typed while a reply streams, the streaming batch relaxing unless another key followed
type typingIdleMsg struct{ typedAt time.Time }

// Busy input mode and destination captured when the user committed an input
type busyControl struct {
	destination submissionDestination
	mode        string
	generation  *int64
}

// One session card and everything its session needs, over the shell every session shares
type app struct {
	*shell
	// Number of the session card, its key among the cards
	num int
	// What was typed into each queued input while stepping through them, by queue index
	queueSlots map[int]string
	store      *tuiStore
	turn       *turnController
	events     *eventHandler
	life       *sessionLifecycle
	prompts    *promptSubmitter
	queue      *composerQueue
	config     *configSync
	card       *sessionCard
	git        *gitBranchTracker
	arena      arenaView
	// Pictures the transcript draws inline
	images *inlineImages

	history        []msg
	lastUserMsg    string
	stickyPrompt   string
	sessionStart   float64
	droppedTurns   int
	argCache       map[string]argCacheEntry
	turnStartedAt  time.Time
	catalog        *slashCatalog
	tokens         []composerToken
	localCommands  []slashCommand
	editorRevision int
	editorTarget   submissionDestination
	// When escape was last pressed and when an escape last closed something
	lastEscape, lastEscapeClosed time.Time
	// Collapsed paste expanded into the draft for editing, nil when none is, and the draft as it read when the span was last placed on it
	pasteSpan  *pasteSpan
	pasteDraft string
	// Prompt put back for editing whose send rewinds the session, the session an escape is armed for and how many prompts it saw
	rewindPending    *pendingRewind
	rewindPrimed     string
	rewindPrimeCount int
	// Rewind on its way to the owner, armed again when it fails unless it was cancelled meanwhile
	rewindFlight *pendingRewind

	bellComplete    bool
	bellPrompt      bool
	voiceEnabled    bool
	voiceTTS        bool
	voiceRecording  bool
	voiceProcessing bool

	recoverSid string
	gaveUp     bool
	// When the prompt last took a key while a reply streamed
	lastTypedAt time.Time
	// Chain of roster refresh ticks in force, a tick of an older chain doing nothing
	refreshChain int
	// Commands the session's state changes ask for, handed to the program once the update that caused them ends
	effects            []tea.Cmd
	boot               tea.Cmd
	dialogs            dialogState
	widgets            *widgetHost
	pet                petCard
	stash              *promptStash
	clipboardRead      *clipboardRead
	clipboardSeq       int
	spawnSections      map[string]bool
	agentDockCollapsed bool
	completion         composerCompletion
	// Reloads the session switcher while it is open, nil when it is closed
	switcherReload func() tea.Cmd
	// Reads the live sessions of the open session switcher again on the roster tick, nil when it is closed
	switcherPoll func() tea.Cmd
	// When the last turn ended, counted from in the status bar
	lastTurnEnded time.Time
}

var (
	_ eventApp    = (*app)(nil)
	_ sessionView = (*app)(nil)
	_ submitDeps  = (*app)(nil)
	_ configApp   = (*app)(nil)
)

// Keymap of the terminal UI, the toolkit defaults with the session keys users of the product know
func tuiKeymap() *keys.Map {
	if keys.ContextDocs[tuiContext] == "" {
		_ = keys.AddContext(tuiContext, "terminal UI session")
	}
	km := keys.Default()
	for _, a := range []keys.Action{
		{ID: "tui.model", Context: tuiContext, Label: "model", Doc: "choose the model of the session", Default: []string{"ctrl+o"}},
		{ID: "tui.cut", Context: tuiContext, Label: "cut", Doc: "cut the selected prompt text to the clipboard"},
		{ID: "tui.queue.delete", Context: tuiContext, Label: "delete queued", Doc: "delete the queued input being edited"},
		{ID: "tui.sessions", Context: tuiContext, Label: "sessions", Doc: "switch between live and stored sessions"},
		{ID: "tui.agents", Context: tuiContext, Label: "agents", Doc: "show the agents this session started", Default: []string{"ctrl+t"}},
		{ID: "tui.editor", Context: tuiContext, Label: "editor", Doc: "edit the prompt in the external editor", Default: []string{"ctrl+g"}},
		{ID: "tui.redraw", Context: tuiContext, Label: "redraw", Doc: "repaint the screen", Default: []string{"ctrl+l"}},
		{ID: "tui.help", Context: keys.Global, Label: "help", Doc: "list every key and command", Default: []string{"f1"}},
		{ID: "tui.suspend", Context: tuiContext, Label: "suspend", Doc: "suspend to the shell, fg brings it back"},
		{ID: "tui.exit", Context: tuiContext, Label: "exit", Doc: "quit on an empty prompt", Default: []string{"ctrl+d"}},
		{ID: "tui.queue.send", Context: tuiContext, Label: "send queued", Doc: "send the first queued prompt now", Default: []string{"super+k"}},
		{ID: "tui.queue.drop", Context: tuiContext, Label: "drop queued", Doc: "drop the newest queued input"},
		{ID: "tui.yolo", Context: tuiContext, Label: "yolo", Doc: "run commands without asking, or ask again (yolo)", Default: []string{"shift+tab"}},
		{ID: "tui.paste.image", Context: tuiContext, Label: "paste image", Doc: "attach the clipboard image"},
		{ID: "tui.paste.expand", Context: tuiContext, Label: "pasted text", Doc: "show or fold pasted text"},
		{ID: "tui.voice", Context: tuiContext, Label: "voice", Doc: "start or stop voice recording", Default: defaultVoiceRecordKey.chords()},
		{ID: "tui.scroll.line.up", Context: tuiContext, Label: "line up", Doc: "scroll the transcript up a line", Default: []string{"shift+up"}, Quiet: true},
		{ID: "tui.scroll.line.down", Context: tuiContext, Label: "line down", Doc: "scroll the transcript down a line", Default: []string{"shift+down"}, Quiet: true},
	} {
		if keyLinked(a.ID) {
			km.Register(a)
		}
	}
	registerListKeys(km)
	registerHubKeys(km)
	registerSettingsKeys(km)
	registerRoomKeys(km)
	registerSpawnTreeKeys(km)
	registerJourneyKeys(km)
	registerStashKeys(km)
	registerRewindKeys(km)
	registerLanguageKeys(km)
	registerPromptKeys(km)
	for _, register := range tierKeys {
		register(km)
	}
	return km
}

// First session of a new shell over the launcher's host, the gateway client not started yet
func newApp(ctx context.Context, host Host) *app {
	gw := newGatewayClient(host.Attach)
	setRPCErrorLogSink(gw.recordLog)
	sh := &shell{ctx: ctx, host: host, keys: tuiKeymap(), gw: gw, resize: newResizeCoalescer(32 * time.Millisecond), ui: newSharedUI(),
		themes: &themeSync{configPinned: loadBootTheme().seededPin}, subs: newSubscriptions(), battery: &batteryPoller{},
		inputs: newComposerHistory(newInputHistory(ctx)), background: newBackgroundWatch(), graphics: gfx.New()}
	sh.ui.themeChanged, sh.widgets = sh.themeChanged, newWidgetHost(sh)
	sh.themeChanged()
	a := sh.newSession()
	keysLoaded := a.loadKeybinds()
	var probe tea.Cmd
	if a.images.graphics && !sh.graphicsProbed {
		sh.graphicsProbed = true
		probe = sh.graphics.Probe()
	}
	sh.wm = wm.New(wm.WithTheme(sh.toolkit), wm.WithKeys(sh.keys), wm.WithWindowTitle("icebath"), wm.WithMotion(sh.motion()))
	// Every command of a card comes back to the session that opened the card
	sh.wm.SetRoute(func(owner any, cmd tea.Cmd) tea.Cmd {
		s, _ := owner.(*app)
		sh.touch(s)
		return sh.tag(s, cmd)
	})
	var open tea.Cmd
	a.boot = tea.Batch(a.boot, sh.wm.As(a, func() tea.Cmd {
		_, open = sh.wm.AddWorkspace(sh.nextWorkspaceID(), a.card)
		return tea.Batch(open, sh.restoreLayout(a))
	}), keysLoaded, probe)
	sh.wm.SetBar(sh.barStatus)
	sh.wm.SetCommandSource(sh.commands)
	sh.wm.SetEmptyHints("palette")
	return a
}

// Session of a new session card over its own store, sharing the shell's window manager, connection, keys and display preferences
func (sh *shell) newSession() *app {
	store := newTUIStore()
	store.ui.sharedUI, store.themes, store.screenCleared = sh.ui, sh.themes, sh.redrawImages
	turn := newTurnController(store)
	sh.made++
	a := &app{shell: sh, num: sh.made, store: store, turn: turn, git: newGitBranchTracker()}
	store.busyChanged = a.busyChanged
	a.events, a.boot = newEventHandler(store, turn, sh.gw, a)
	// Only the first card resumes the session the launch named
	if len(sh.sessions) > 0 {
		a.events.startupResumeID = ""
	}
	a.life = newSessionLifecycle(store, turn, sh.gw, a, sh.subs)
	a.prompts = &promptSubmitter{store: store, turn: turn, gw: sh.gw, deps: a}
	// Queue journal failures are reported without ending the turn
	a.queue = newComposerQueue(store, sh.gw, func(err error) {
		if err != nil {
			a.sys(errorLine(rpcErrorMessage(err)))
		}
	})
	a.config = newConfigSync(store, turn, sh.gw, a)
	a.images = newInlineImages(a)
	a.arena = arenaViewFor(a)
	a.widgets = sh.widgets
	a.localCommands = append(append(append(append(append(append(a.coreCommands(), a.transcriptCommands()...), a.opsCommands()...), a.sessionCommands()...), a.controlCommands()...), a.wmCommands()...), a.recoveryCommands()...)
	a.localCommands = append(a.localCommands, a.nativeCommands()...)
	a.localCommands = append(a.localCommands, a.agentsCommands()...)
	a.localCommands = append(a.localCommands, a.roomsCommands()...)
	a.localCommands = append(a.localCommands, a.terminalCommands()...)
	a.localCommands = append(a.localCommands, a.keyCommands()...)
	a.localCommands = append(a.localCommands, a.screenCommands()...)
	a.localCommands = append(a.localCommands, a.languageCommands()...)
	for _, commands := range tierCommands {
		a.localCommands = append(a.localCommands, commands(a)...)
	}
	a.localCommands = slices.DeleteFunc(a.localCommands, func(c slashCommand) bool { return !localCommandLinked(c.name) })
	// One pet for the window, in the card of the session in view
	if sh.pet == nil {
		sh.pet = petCardFor(a)
	}
	a.pet = sh.pet
	a.card = newSessionCard(a)
	sh.sessions = append(sh.sessions, a)
	if sh.current == nil {
		sh.current = a
	}
	return a
}

// Animation scale of the window manager, still when the settings ask for reduced motion
func bootMotion() float64 {
	if prefs.ReducedMotion {
		return 0
	}
	return 1
}

// Runs the terminal UI, prints the session summary when it ends and returns the exit status
func Run(ctx context.Context, host Host) int {
	if host.Prefs != nil {
		prefs = *host.Prefs
	}
	clip.ForceSequence(prefs.ClipboardSequence)
	noteLaunchFolder()
	if stop := startCPUProfile(prefs.CPUProfile); stop != nil {
		defer stop()
	}
	a := newApp(ctx, host)
	meter := newFrameMeter()
	defer meter.close()
	p := tty.NewProgram(ctx, meter.wrap(a))
	p.OnFrame(meter.listener())
	panicked := false
	p.OnPanic(func(value any) {
		panicked = true
		recordPanic(value)
	})
	a.shell.post = newMailbox(ctx, p.Send).post
	if meter != nil {
		meter.post = a.shell.post
	}
	a.gw.attach(p.Send)
	a.gw.start()
	defer crashReportsToLog()()
	stopWatch := a.startMemoryWatch()
	_, err := p.Run()
	stopWatch()
	gfx.Sweep()
	a.gw.kill(a.exitReasonOf(err, panicked))
	a.settleNewWorktrees()
	return a.exitStatus(err, p.Terminated())
}

// Work a session starts with once its card stands: its boot, roster refresh, widgets and pet
func (a *app) start() tea.Cmd {
	return tea.Batch(a.boot, a.scheduleRefresh())
}

// Frame of the window manager with this session's colors, mouse mode and tab title
func (a *app) view() tea.View {
	v := a.wm.View()
	if bg := a.store.themes.terminalBg; isPaintableHex(bg) {
		v.BackgroundColor = toneColor(bg)
	}
	if fg := a.store.themes.terminalFg; isPaintableHex(fg) {
		v.ForegroundColor = toneColor(fg)
	}
	switch a.store.ui.MouseTracking {
	case "off":
		v.MouseMode = tea.MouseModeNone
	case "wheel", "buttons":
		v.MouseMode = tea.MouseModeCellMotion
	}
	v.WindowTitle = composeTabTitle(a.tabMarker(), a.store.ui.SessionTitle, infoText(infoOf(a.store.ui.Info).Model), a.store.sessionCwd(), 40)
	return v
}

// Mark of the terminal tab, a turn running, a prompt waiting or idle
func (a *app) tabMarker() string {
	switch {
	case a.dialogs.waiting():
		return "⚠"
	case a.store.ui.Busy:
		return "⏳"
	}
	return "◇"
}

// Takes a message that belongs to this session
func (a *app) update(m tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch m := m.(type) {
	case serverRequestMsg:
		if !a.store.handleServerRequest(m.Request, a.events) {
			m.Request.unhandled()
		}
		cmd = a.events.takeCmds()
	case interruptReplyMsg:
		a.turn.settleInterrupt(m)
	case resumeScrollMsg:
		cmd = a.life.onResumeScroll(m)
	case configPollMsg:
		cmd = a.config.onPoll(m)
	case loopTickMsg:
		cmd = a.store.onLoopTick(m)
	case refreshTickMsg:
		if m.chain == a.refreshChain {
			cmd = tea.Batch(a.refreshRosters(), a.scheduleRefresh(), a.pollSwitcher())
		}
	case typingIdleMsg:
		if m.typedAt.Equal(a.lastTypedAt) {
			a.turn.relaxStreaming()
		}
	case cards.SubmitMsg:
		if room := a.roomCardOf(m.Session); room != nil {
			cmd = room.send(m.Text)
		} else {
			cmd = a.dispatchSubmission(m.Text)
		}
	case editorDoneMsg:
		cmd = a.onEditorDone(m)
	default:
		var petCmd tea.Cmd
		if ev, isEvent := m.(gatewayEventMsg); isEvent {
			a.store.sawFrame(ev)
			a.arena.observe(ev)
			petCmd = a.pet.observe(ev)
			if ev.Type == tuigateway.SessionsChangedEvent.Name() && a.switcherReload != nil {
				petCmd = tea.Batch(petCmd, a.switcherReload())
			}
		}
		if handled, c := a.events.update(m); handled {
			cmd = c
		} else if handled, c := a.git.update(m); handled {
			cmd = c
		} else if handled, c := a.updateExtras(m); handled {
			cmd = c
		} else {
			_, cmd = a.wm.Update(m)
		}
		cmd = tea.Batch(petCmd, cmd)
	}
	return cmd
}

// Reports whether a message may change what the arena shows, window manager and arena frames never doing so
func feedsArena(m tea.Msg) bool {
	switch m.(type) {
	case anim.FrameMsg, wm.Addressed, petTickMsg:
		return false
	}
	return true
}

// Hands a command a state change asks for to the program once the update that caused it ends
func (a *app) effect(cmd tea.Cmd) {
	if cmd != nil {
		a.effects = append(a.effects, cmd)
	}
}

// What the session does once it took a message: the commands its state changes asked for, the completions of the draft, the prompt it must show
// and the next queued input
func (a *app) settle() tea.Cmd {
	cmds := a.turn.takeCmds()
	cmds = append(cmds, a.syncCompletion())
	a.followPaste()
	cmds = append(cmds, a.git.track(a.store.sessionCwd()), a.store.syncLoopTicker(), a.syncDialogs(), a.drainQueue())
	if a.shell.inView(a) {
		cmds = append(cmds, a.pet.follow())
	}
	cmds = append(cmds, a.effects...)
	a.effects = nil
	return tea.Batch(cmds...)
}

// The card now shows another session, or none: the queued input held for editing is let go, the turn clock starts over and the session's
// config, rosters, commands and controls are read
func (a *app) sessionBound(previous string) {
	ui := &a.store.ui
	if previous != "" && previous != ui.Sid {
		a.queue.dropEdits()
		a.queueSlots = nil
	}
	// A running turn of the session now shown counts from when it came into view
	a.lastTurnEnded, a.turnStartedAt = time.Time{}, time.Time{}
	if ui.Busy {
		a.turnStartedAt = a.store.now()
	}
	a.card.composer.SetBusy(ui.Busy)
	a.effect(a.card.composer.Bind(ui.Sid))
	a.effect(a.config.syncSession(ui.Sid))
	if ui.Sid == "" {
		ui.SessionTitle = ""
	} else {
		a.effect(a.refreshRosters())
		if !ui.offline() {
			a.effect(a.events.loadSlashCatalog(ui.Sid))
			a.effect(a.readControl())
		}
		for _, hint := range a.takeTerminalHints() {
			a.turn.pushActivity(hint.message, hint.tone, "")
		}
		a.shell.focusChanged()
	}
	if err := a.queue.reconcileReceipts(); err != nil {
		a.sys(errorLine(err.Error()))
	}
}

// A turn of the session started or ended: the turn clock follows and the prompt shows it
func (a *app) busyChanged(busy bool) {
	now := a.store.now()
	if !busy && !a.turnStartedAt.IsZero() {
		a.lastTurnEnded = now
	}
	a.turnStartedAt = time.Time{}
	if busy {
		a.turnStartedAt = now
		// A turn that starts is watched at the short interval from now, the slow chain of the idle session ending here
		a.refreshChain++
		a.effect(tea.Batch(a.refreshRosters(), a.scheduleRefresh()))
		a.shell.watchLive()
	}
	if a.card != nil {
		a.card.composer.SetBusy(busy)
	}
}

// The background service came back: the session's commands and controls are read again
func (a *app) reconnected() {
	if sid := a.store.ui.Sid; sid != "" {
		a.effect(a.events.loadSlashCatalog(sid))
		a.effect(a.readControl())
	}
}

func (a *app) uiTheme() uitheme.Theme { return a.toolkit }

// Sends the queue head once the session is idle and attached
func (a *app) drainQueue() tea.Cmd {
	ui := a.store.ui
	if ui.Busy || ui.Sid == "" || a.queue.queueEditIndex() != nil || a.quitting || a.dialogs.waiting() {
		return nil
	}
	item, err := a.queue.dequeue(false)
	if err != nil {
		a.sys(errorLine(err.Error()))
		return nil
	}
	if item == nil {
		return nil
	}
	return a.sendQueued(item)
}

// Tick of the roster refresh, one chain per session started with its card; an idle session with nothing in its dock is asked rarely
func (a *app) scheduleRefresh() tea.Cmd {
	every := idleRefreshEvery
	if a.store.ui.Busy || a.dockLive(a.store.nowMillis()) {
		every = refreshEvery
	}
	chain := a.refreshChain
	return tea.Tick(every, func(time.Time) tea.Msg { return refreshTickMsg{chain: chain} })
}

// Reads the agents and processes of the session
func (a *app) refreshRosters() tea.Cmd {
	sid := a.store.ui.Sid
	if sid == "" || a.store.ui.offline() {
		return nil
	}
	params := tuigateway.SessionIDParams{SessionID: sid}
	return tea.Batch(
		call(a.gw, tuigateway.SubagentList, params, func(r *tuigateway.SubagentListResult, err error) tea.Cmd {
			if err == nil && a.store.ui.Sid == sid {
				a.store.applyAgentSnapshot(sid, *r)
			}
			return nil
		}),
		call(a.gw, tuigateway.ProcessList, params, func(r *tuigateway.ProcessListResult, err error) tea.Cmd {
			if err == nil && a.store.ui.Sid == sid {
				a.store.applyProcessSnapshot(sid, r.Processes)
			}
			return nil
		}),
	)
}

// Writes the notices of the sessions this view left running into the transcript in view. The live list of the shared gateway says only running or
// not, so while a shown session is off screen its active list is read too, which says waiting when a question holds the turn
func (a *app) noteLiveSessions(rows []listedSession, focused string) tea.Cmd {
	if a.store.ui.Sid != focused || a.shell.active() != a {
		return nil
	}
	// Sessions open in another card are not background work
	rows = slices.DeleteFunc(slices.Clone(rows), func(r listedSession) bool { return r.ID != focused && a.shell.bySid(r.ID) != nil })
	a.background.focus(focused)
	announce := func(rows []listedSession) tea.Cmd {
		for _, notice := range a.background.update(rows, focused) {
			a.sys(notice)
		}
		return nil
	}
	if !a.background.watching(rows, focused) {
		return announce(rows)
	}
	params := tuigateway.SessionActiveListParams{CurrentSessionID: &focused}
	return call(a.gw, tuigateway.SessionActiveList, params, func(r *tuigateway.SessionActiveListResult, err error) tea.Cmd {
		if a.store.ui.Sid != focused {
			return nil
		}
		if err != nil || r == nil {
			return announce(rows)
		}
		live := make([]listedSession, len(r.Sessions))
		for i, s := range r.Sessions {
			live[i] = listedSession{ID: s.ID, SessionID: s.SessionKey, Title: s.Title, Status: string(s.Status), Current: s.Current}
		}
		return announce(live)
	})
}

// Session in view and whether its gateway is up, the goal and loop controls read once per change of either
func (a *app) attachKey() string {
	ui := a.store.ui
	if ui.Sid == "" || (ui.GatewayConnected != nil && !*ui.GatewayConnected) {
		return ""
	}
	return ui.Sid
}

// Reads the goal and loop controls of the session in view once, their updates arriving as events afterwards
func (a *app) readControl() tea.Cmd {
	sid := a.attachKey()
	if sid == "" {
		return nil
	}
	return call(a.gw, tuigateway.SessionControlRead, tuigateway.SessionControlReadParams{SessionID: sid}, func(r *tuigateway.SessionControlReadResult, err error) tea.Cmd {
		if err == nil && a.store.ui.Sid == sid {
			a.store.applyControlSnapshot(sid, r.Control)
		}
		return nil
	})
}

// Session the card resumes once the background service is back: the one it shows, else the one it was already waiting to recover
func (a *app) recoveryTarget() string {
	return firstText(a.store.ui.StoredSid, a.store.ui.Sid, a.recoverSid)
}

// The connection to the background service ended: the turn is reset and the session marked for recovery. An attached service is reconnected
// to; one this window started is restarted while restart says the budget lasts
func (a *app) onGatewayExit(code int, attached, wasConnected, restart bool) {
	ui := &a.store.ui
	a.recoverSid = a.recoveryTarget()
	ui.Compacting = false
	if attached {
		// The turn stays as it stands on screen: the reconnect replays what was missed, or reads the session anew
		ui.Status = tr(txt.SessionStatusReconnecting)
		if ui.Sid != "" && wasConnected {
			a.turn.pushActivity(tr(txt.UserMessagesBackendConnectionLostActivity), "warn", "")
			a.sys(tr(txt.UserMessagesBackendConnectionLost))
		}
		return
	}
	a.turn.reset()
	ui.Status = tr(txt.LibTextGatewayExited)
	if restart && a.recoverSid != "" {
		a.turn.pushActivity(tr(txt.UserMessagesBackendRestartingActivity), "warn", "")
		a.sys(tr(txt.UserMessagesBackendRestarting))
		return
	}
	ui.Status = tr(txt.SessionStatusStopped)
	if !a.gaveUp {
		a.gaveUp = true
		a.turn.pushActivity(tr(txt.UserMessagesBackendGaveUpActivity), "error", "")
		last, _ := lastStderrLine(a.gw.logTail(20))
		a.sys(errorLine(backendGaveUp(&code, last)))
	}
}

func (a *app) sys(text string) { a.appendMessage(msg{Role: "system", Text: text}) }

// Appends a transcript row, counting the user turns the history cap drops
func (a *app) appendMessage(m msg) {
	full := appendTranscriptMessage(a.history, m, func() float64 { return a.store.nowMillis() / 1000 })
	a.history = capTranscriptHistory(full)
	if len(a.history) < len(full) {
		a.droppedTurns += userRows(full) - userRows(a.history)
	}
	a.store.transcriptGen++
}

func (a *app) panel(title string, sections []panelSection) {
	a.appendMessage(msg{Kind: "panel", Role: "system", PanelData: &panelData{Title: title, Sections: sections}})
}

func (a *app) setHistoryItems(update func([]msg) []msg) {
	a.history = update(a.history)
	a.store.transcriptGen++
}

func (a *app) setInput(update func(string) string) {
	a.effect(a.card.composer.SetValue(update(a.card.composer.Value())))
}

func (a *app) enqueue(text, display string, destination submissionDestination) {
	if _, err := a.queue.enqueue(text, display, &destination, nil); err != nil {
		a.sys(tr(txt.CanonicalQueueNotSavedDraftKept, err.Error()))
	}
}

func (a *app) submit(text string) tea.Cmd { return a.dispatchSubmission(text) }

func (a *app) newSession(then func() tea.Cmd) tea.Cmd {
	return a.life.newSession("", "", func(string) tea.Cmd {
		if then == nil {
			return nil
		}
		return then()
	})
}

// Attaches the session of the card again after the connection came back
func (a *app) reattach(id string, then func(err error) tea.Cmd) tea.Cmd {
	return a.life.reattach(id, then)
}

func (a *app) resumeByID(id string, then func(err error) tea.Cmd) tea.Cmd {
	if then == nil {
		then = func(err error) tea.Cmd {
			if err != nil {
				a.uncaught(err)
			}
			return nil
		}
	}
	return a.life.resumeByID(id, then)
}

func (a *app) setCatalog(c *slashCatalog) {
	a.catalog = c
}

func (a *app) setVoiceEnabled(on bool) { a.voiceEnabled, a.voiceTTS = on, on && a.voiceTTS }
func (a *app) setVoiceTTS(on bool)     { a.voiceTTS = on }

// Marks the session recording or not. The one microphone belongs to the session that records: taking it stops the session that held it, and
// only the holder's state says whether the microphone is live
func (a *app) setVoiceRecording(on bool) {
	a.voiceRecording = on
	if on {
		a.effect(a.shell.claimVoice(a))
	}
	if a.shell.voiceOwner == a {
		speech.recording.Store(on)
	}
}

func (a *app) setVoiceProcessing(on bool)         { a.voiceProcessing = on }
func (a *app) setVoiceRecordKey(k voiceRecordKey) { a.voiceKey = k; a.bindVoiceKey() }
func (a *app) setBellOnComplete(on bool)          { a.bellComplete = on }
func (a *app) setBellOnPrompt(on bool)            { a.bellPrompt = on }
func (a *app) inlineImageRetry()                  { a.images.retry() }

// Drops the rows drawn in the language in force before, a pack arriving after the first rows are on screen
func (a *app) languageChanged() { a.card.view.invalidate() }

// Drops the full config the event handler read, after the session or its config changed
func (a *app) configChanged() { a.events.invalidateConfig() }

func (a *app) recoverSidRef() *string         { return &a.recoverSid }
func (a *app) bellOnComplete() bool           { return a.bellComplete }
func (a *app) bellOnPrompt() bool             { return a.bellPrompt }
func (a *app) setLastUserMsg(text string)     { a.lastUserMsg = text }
func (a *app) setSessionStartedAt(ms float64) { a.sessionStart, a.droppedTurns = ms, 0 }
func (a *app) setStickyPrompt(text string)    { a.stickyPrompt = text }
func (a *app) scroll() scrollHandle           { return a.card.view }
func (a *app) onFreshSessionStarted(string)   { a.gaveUp = false }
func (a *app) onGatewayReady()                { a.gaveUp = false }
func (a *app) leftRunning(sid string)         { a.background.handoff(sid) }

// Expansion of the chips the draft holds right now
func (a *app) expander() func(string) string { return expandTokens(a.tokens) }

func (a *app) clearComposerTokens() {
	a.tokens = nil
}

func (a *app) cols() int {
	w, _ := a.wm.Size()
	return max(w, 20)
}

// Reports a failure that ended the session's turn before it started, leaving the session ready
func (a *app) uncaught(err error) {
	if err == nil {
		return
	}
	a.sys(errorLine(rpcErrorMessage(err)))
	a.turn.idle(false)
	a.store.setBusy(false)
	a.store.ui.Status = tr(txt.StatusReady)
}

// Follows typing, the streamed reply batching faster while the user edits the prompt
func (a *app) typed(cmd tea.Cmd) tea.Cmd {
	if !a.store.ui.Busy {
		return cmd
	}
	a.turn.boostStreamingForTyping()
	typedAt := a.store.now()
	a.lastTypedAt = typedAt
	return tea.Batch(cmd, tea.Tick(time.Second, func(time.Time) tea.Msg { return typingIdleMsg{typedAt: typedAt} }))
}

// Sends a prompt to the session, staged in the durable queue journal until the owner answers
func (a *app) send(text string, show bool, display string, opts submitOpts) tea.Cmd {
	if opts.Destination == nil {
		destination, err := a.store.captureDestination()
		if err != nil {
			a.uncaught(err)
			return nil
		}
		opts.Destination = &destination
	}
	if opts.QueueItem == nil && opts.Destination.Sid != "" {
		item, err := a.queue.stage(text, display, opts.Destination, opts.Attachments)
		if err != nil {
			a.sys(tr(txt.CanonicalQueueNotSavedDraftKept, err.Error()))
			a.store.ui.Status = tr(txt.CanonicalQueueStatusInputNotSaved)
			return nil
		}
		opts.QueueItem = item
	}
	return a.prompts.submitPrompt(text, show, display, opts)
}

// Sends a prompt whose tokens were already expanded
func (a *app) sendExpanded(text string, show bool, display string, opts submitOpts) tea.Cmd {
	opts.Expand = func(s string) string { return s }
	return a.send(text, show, display, opts)
}

// Delivers a queued input the way it was typed, a command, a shell line or a prompt with the output of the shell commands its person typed between braces
func (a *app) sendQueued(item *queueItem) tea.Cmd {
	destination := item.Destination
	if destination == nil {
		captured, err := a.store.captureDestination()
		if err != nil {
			a.uncaught(err)
			return nil
		}
		destination = &captured
	}
	opts := submitOpts{Destination: destination, QueueItem: item, Attachments: item.Attachments}
	if item.PreparedText != nil {
		opts.SkipDetectDrop = true
		return a.sendExpanded(*item.PreparedText, true, item.Display, opts)
	}
	switch {
	case looksLikeSlashCommand(item.Text):
		item.settle(true)
		return a.slash(item.Text, &slashSubmission{attachments: item.Attachments, expand: func(s string) string { return s }}, false)
	case strings.HasPrefix(item.Text, "!"):
		return a.shellExec(trimJS(item.Text[1:]), *destination, item)
	}
	typed := typedInterpolations(item.Display, item.Text)
	if len(typed) == 0 {
		return a.sendExpanded(item.Text, true, item.Display, opts)
	}
	a.store.setBusy(true)
	return a.interpolate(typed, *destination, item, func(outputs []string) tea.Cmd {
		shown, text := spliceInterpolations(item.Display, item.Text, typed, outputs)
		return a.sendExpanded(text, true, shown, opts)
	})
}

// Runs a shell line in the session and prints what it wrote and its exit status
func (a *app) shellExec(command string, destination submissionDestination, item *queueItem) tea.Cmd {
	focused := func() bool { return a.store.isCurrentDestination(destination) }
	a.appendMessage(msg{Role: "user", Text: "!" + command})
	a.store.markSubmitting()
	return call(a.gw, tuigateway.CanonicalShellExec, tuigateway.CanonicalShellExecParams{Command: command, SessionID: destination.Sid}, func(r *tuigateway.ShellExecResult, err error) tea.Cmd {
		if item != nil {
			item.settle(err == nil)
		}
		if !focused() {
			return nil
		}
		a.store.setBusy(false)
		a.store.ui.Status = tr(txt.StatusReady)
		if err != nil {
			a.sys(errorLine(describeShellExecError(err)))
			return nil
		}
		for _, m := range shellOutputRows(*r) {
			a.appendMessage(m)
		}
		return nil
	})
}

// Busy mode, destination and running generation at the moment an input was committed
func (a *app) captureBusyControl() (busyControl, error) {
	destination, err := a.store.captureDestination()
	if err != nil {
		return busyControl{}, err
	}
	control := busyControl{destination: destination, mode: a.store.ui.BusyInputMode}
	if stamp := infoOf(a.store.ui.Info).ExecutionGeneration; stamp != nil {
		generation := *stamp
		control.generation = &generation
	}
	return control, nil
}

// Admits an input typed while a turn runs: behind the turn in queue mode, else steered into the turn or redirecting it by the busy mode
func (a *app) handleBusyInput(item *queueItem, control busyControl) tea.Cmd {
	destination := control.destination
	if control.mode == "queue" {
		opts := submitOpts{Destination: &destination, BehindTurn: true, Attachments: item.Attachments}
		if item.Settle != nil {
			opts.QueueItem = item
		}
		return a.sendExpanded(item.Text, true, item.Display, opts)
	}
	staged := item
	if staged.Settle == nil {
		var err error
		if staged, err = a.queue.stage(item.Text, item.Display, &destination, item.Attachments); err != nil {
			a.sys(tr(txt.CanonicalQueueNotSavedDraftKept, err.Error()))
			return nil
		}
	}
	if staged.ControlMethod == "" {
		staged.ControlMethod = "session.redirect"
		if control.mode == "steer" {
			staged.ControlMethod = "session.steer"
		}
	}
	if staged.ExecutionGeneration == nil {
		staged.ExecutionGeneration = control.generation
	}
	if staged.Attachments == nil {
		staged.Attachments = item.Attachments
	}
	return a.sendExpanded(item.Text, true, item.Display, submitOpts{Destination: &destination, BehindTurn: true, QueueItem: staged, Attachments: item.Attachments})
}

// Routes a sent line, a slash command, a shell line or a prompt by the state of the session
func (a *app) dispatchSubmission(full string) tea.Cmd {
	if trimJS(full) == "" {
		return nil
	}
	if cmd, rewound := a.sendPendingRewind(full); rewound {
		return cmd
	}
	destination, err := a.store.captureDestination()
	if err != nil {
		a.uncaught(err)
		return nil
	}
	tokens := a.tokens
	text, attachments := prepareSubmission(full, tokens)
	clearInput := func() { a.clearComposerTokens() }
	notSaved := func(err error) tea.Cmd {
		a.sys(tr(txt.CanonicalQueueNotSavedDraftKept, err.Error()))
		a.store.ui.Status = tr(txt.CanonicalQueueStatusInputNotSaved)
		return a.card.composer.SetValue(full)
	}
	if looksLikeSlashCommand(full) {
		command, shown := prepareSlashSubmission(full, tokens)
		a.appendMessage(msg{Kind: "slash", Role: "system", Text: shown})
		name, _ := parseSlashCommand(full)
		if name == "queue" || name == "q" {
			if plan := planQueueSlash(shown, command); plan.kind == "enqueue" {
				if _, err := a.queue.enqueue(plan.text, plan.display, &destination, attachments); err != nil {
					return notSaved(err)
				}
				clearInput()
				a.sys(tr(txt.SlashCmdCoreQueueQueued, previewOf(plan.display)))
				return nil
			}
		}
		clearInput()
		return a.slash(command, &slashSubmission{attachments: attachments, expand: expandTokens(tokens)}, true)
	}
	if strings.HasPrefix(full, "!") {
		clearInput()
		return a.shellExec(trimJS(full[1:]), destination, nil)
	}
	ui := a.store.ui
	if ui.Sid == "" && ui.SetupRequired {
		kept := a.card.composer.SetValue(full)
		a.sys(tr(txt.SessionInputSetupFirst))
		return tea.Batch(kept, a.openSetup("model"))
	}
	a.store.stagePastes(full, text, tokens)
	if ui.Sid == "" || (ui.GatewayConnected != nil && !*ui.GatewayConnected) {
		if _, err := a.queue.enqueue(text, full, &destination, attachments); err != nil {
			return notSaved(err)
		}
		clearInput()
		return nil
	}
	if editing := a.queue.queueEditIndex(); editing != nil {
		index := *editing
		a.queueSlots = nil
		if err := a.queue.setQueueEdit(nil); err != nil {
			return notSaved(err)
		}
		control, cerr := a.captureBusyControl()
		if cerr != nil {
			a.uncaught(cerr)
			return nil
		}
		editedWhileBusy := ui.Busy
		edited := full
		cmd, err := a.queue.takeQ(index, &edited, func(item *queueItem) tea.Cmd {
			if item == nil {
				return nil
			}
			if editedWhileBusy {
				return a.handleBusyInput(item, control)
			}
			return a.sendQueued(item)
		})
		if err != nil {
			return notSaved(err)
		}
		clearInput()
		return cmd
	}
	if ui.Busy {
		control, cerr := a.captureBusyControl()
		if cerr != nil {
			a.uncaught(cerr)
			return nil
		}
		item := newQueueItem(text, full)
		item.Attachments = attachments
		clearInput()
		return a.handleBusyInput(&item, control)
	}
	if shouldInterpolateSubmission(full) {
		item, err := a.queue.stage(text, full, &destination, attachments)
		if err != nil {
			return notSaved(err)
		}
		clearInput()
		a.store.setBusy(true)
		typed := typedInterpolations(full, full)
		return a.interpolate(typed, destination, item, func(outputs []string) tea.Cmd {
			shown, _ := spliceInterpolations(full, full, typed, outputs)
			expanded, _ := prepareSubmission(shown, tokens)
			return a.sendExpanded(expanded, true, shown, submitOpts{Destination: &destination, QueueItem: item, Attachments: attachments})
		})
	}
	clearInput()
	return a.sendExpanded(text, true, full, submitOpts{Destination: &destination, Attachments: attachments})
}

// Interrupts the running turn of the session
func (a *app) interruptTurn() tea.Cmd {
	deps := interruptDeps{AppendMessage: a.appendMessage, Gateway: a.gw, Sid: a.store.ui.Sid, Sys: a.sys}
	cmd, err := a.turn.interruptTurn(deps, false)
	if err != nil {
		a.uncaught(err)
		return nil
	}
	return cmd
}

// Interrupt key: copies a prompt selection, else puts the draft aside, else stops the turn, else drops an armed rewind, else a second press quits
func (a *app) interruptKey() tea.Cmd {
	now := a.store.now()
	_, selected := a.card.composer.Selection()
	switch {
	case a.wm.Overlay() != nil || a.wm.Sheet(a.card) != nil:
		return a.interruptOverlay()
	case selected:
		return a.card.composer.Action("text.copy")
	case a.card.composer.Value() != "":
		a.cancelPendingRewind(true)
		a.lastInterrupt = time.Time{}
		if a.queue.queueEditIndex() == nil {
			if stashed, ok := a.stashDraft(); ok {
				return tea.Batch(stashed, a.wm.Toast(tr(txt.SessionNoticesDraftStashed, a.keys.Display("tui.stash")), wm.Info, wm.DefaultToastTTL))
			}
		}
		return a.clearIn()
	case a.store.ui.Busy && a.store.ui.Sid != "":
		a.lastInterrupt = time.Time{}
		return a.interruptTurn()
	case a.cancelPendingRewind(true):
		return nil
	case !a.lastInterrupt.IsZero() && now.Sub(a.lastInterrupt) < exitConfirmWindow:
		return a.die(0)
	}
	a.lastInterrupt = now
	// The notice lasts exactly as long as a second press still quits
	return a.wm.Toast(tr(txt.SessionNoticesPressAgainToExit), wm.Info, exitConfirmWindow)
}

// Escape removes one layer per press: a selection, a queued or recalled prompt, the running turn, then arms the rewind; it never clears the draft
func (a *app) escape() tea.Cmd {
	now := a.store.now()
	composer := a.card.composer
	if _, selected := composer.Selection(); selected {
		composer.ClearSelection()
		a.escapeClosed()
		return nil
	}
	if a.queue.queueEditIndex() != nil {
		a.setQueueEdit(nil)
		a.escapeClosed()
		return composer.SetValue("")
	}
	if _, recalling := composer.Recalling(); recalling {
		composer.BackToDraft()
		a.escapeClosed()
		return nil
	}
	ui := a.store.ui
	if ui.Busy && ui.Sid != "" && !a.dialogs.waiting() {
		// The second tap of a double tap that closed something leaves the turn alone
		if now.Sub(a.lastEscapeClosed) < escapeAfterClose {
			return nil
		}
		return a.interruptTurn()
	}
	if composer.Value() != "" {
		a.rewindPrimed = ""
		return nil
	}
	if a.rewindPrimed != "" && now.Sub(a.lastEscape) > rewindArmWindow {
		a.rewindPrimed = ""
	}
	a.lastEscape = now
	cmd, _ := a.backtrackEscape()
	return cmd
}

// Notes that an escape closed something, starting the guard that keeps the next one from stopping the turn
func (a *app) escapeClosed() { a.lastEscapeClosed = a.store.now() }

// Quits with the status after detaching the session, a quit of the person's own named so in the crash log
func (a *app) die(code int) tea.Cmd {
	if code == 0 {
		return a.dieFor(code, "app.die")
	}
	return a.dieFor(code, fmt.Sprintf("app.dieWithCode:%d", code))
}

// Quits after detaching every open session, never waiting longer than the close limit, the reason of the first quit standing
func (a *app) dieFor(code int, reason string) tea.Cmd {
	if a.quitting {
		return nil
	}
	a.quitting = true
	a.exitReason = reason
	a.store.ui.Status = tr(txt.SessionStatusClosing)
	var released, detached []tea.Cmd
	for _, s := range a.sessions {
		released = append(released, s.images.release())
		detached = append(detached, a.tag(s, s.life.closeForExit()))
	}
	done := func() tea.Msg { return exitMsg{code: code} }
	return tea.Batch(
		tea.Batch(released...),
		tea.Sequence(tea.Batch(detached...), done),
		tea.Tick(exitCloseLimit, func(time.Time) tea.Msg { return exitMsg{code: code} }),
	)
}

// Commands the window manager runs for global keys and lists in its palette
func (a *app) commands() []wm.Command {
	idle := func() bool { return !a.dialogs.waiting() }
	cmds := []wm.Command{
		{ID: "interrupt", Run: a.interruptKey},
		{ID: "session.new", Title: "new session", When: idle, Run: func() tea.Cmd { return a.slash("/new", nil, false) }},
		{ID: "model.switch", Title: "model", When: idle, Run: a.openModelPicker},
		{ID: "open.agents", Title: "agents", When: idle, Run: func() tea.Cmd { return a.openSpawnTree(0, nil) }},
		{ID: "tui.help", Title: "help", Run: a.openHelp},
		{ID: "open.rooms", Title: "rooms", When: idle, Run: a.openRooms},
		{ID: "terminal.new", Title: "terminal", Run: func() tea.Cmd { return a.openTerminal("", false) }},
		{ID: "terminal.tab", Title: "terminal as a tab", Run: func() tea.Cmd { return a.openTerminal("", true) }},
		{ID: "session.compact", Title: "compact", When: idle, Run: func() tea.Cmd { return a.slash("/compress", nil, false) }},
		{ID: "run.cancel", Title: "interrupt the turn", When: func() bool { return a.store.ui.Busy }, Run: a.interruptTurn},
		// Quitting on ctrl+d needs an empty prompt and no turn running; mid turn the key stays the editor's delete
		{ID: "tui.exit", When: func() bool {
			return idle() && !a.store.ui.Busy && a.card.composer.Value() == "" && a.card.composer.Empty()
		}, Run: func() tea.Cmd { return a.die(0) }},
		{ID: "tui.queue.drop", When: a.hasQueued, Run: a.dropQueued},
		{ID: "tui.queue.send", When: a.queueHeadWaits, Run: a.sendQueueHead},
		{ID: "tui.queue.delete", When: func() bool { return a.queue.queueEditIndex() != nil }, Run: a.deleteQueueEdit},
		{ID: "tui.cut", When: a.selecting, Run: a.cutSelection},
	}
	cmds = append(append(cmds, a.hubPaletteCommands()...), a.slashPaletteCommands()...)
	// Last in the palette, so a stray enter on the palette never quits
	return append(cmds, wm.Command{ID: "quit", Title: "quit", Run: func() tea.Cmd { return a.die(0) }})
}

// Whether any input waits in the queue of the session in view
func (a *app) hasQueued() bool {
	shown, err := a.queue.queuedDisplay()
	return err == nil && len(shown) > 0 && a.card.composer.Empty()
}

// Drops the newest queued input
func (a *app) dropQueued() tea.Cmd {
	shown, err := a.queue.queuedDisplay()
	if err != nil || len(shown) == 0 {
		return nil
	}
	cmd, err := a.queue.removeQ(len(shown) - 1)
	if err != nil {
		a.sys(errorLine(err.Error()))
		return nil
	}
	return tea.Batch(cmd, wm.Notify(tr(txt.CanonicalQueueDroppedNewest), wm.Info))
}

// Fact of the status bar, its text, color role and the command a click runs
type statusFact struct {
	text   string
	style  wm.SegmentStyle
	action string
	// Place in the order the facts give way when room runs out, the lowest staying longest
	rank int
	// Whether the fact stands beside the turn status at the left of the session status row
	turn bool
	// Whether the fact is a hint painted faint
	faint bool
}

// Order the status facts give way in
const (
	// Approvals off for the session, never given up for room
	rankYolo = iota
	rankModel
	rankContext
	rankCost
	rankDuration
	rankFolder
	rankBattery
	rankIdle
	rankCompressions
	rankCacheHit
	rankLatency
	rankTPS
	rankVoice
	rankSessions
	rankBgTasks
	rankSubagents
	rankStash
	rankResume
)

// Narrowest terminal that still shows a fact
const (
	contextFullCols  = 72
	durationCols     = 76
	compressionsCols = 80
	bgTasksCols      = 88
	subagentsCols    = 92
	cacheHitCols     = 96
	latencyCols      = 104
	tpsCols          = 110
)

// Widest the folder label runs on the bar and on the status row
const (
	barFolderCols = 36
	rowFolderCols = 28
)

// Bar of the window manager, the session lane with its signal and the dials of the session, none while the status bar is off or sits in the session window
func (a *app) barStatus() wm.Status {
	ui := a.store.ui
	status := wm.Status{Lanes: map[*wm.Workspace]wm.Lane{}}
	if ws := a.wm.WorkspaceOf(a.card); ws != nil {
		status.Lanes[ws] = a.lane()
	}
	if text, signal, waiting := a.dialogs.notice(); waiting {
		status.Notice = wm.Notice{Text: text, Signal: signal}
	}
	if ui.StatusBar == "bottom" {
		return status
	}
	cols, _ := a.wm.Size()
	facts := a.barFacts(cols, barFolderCols)
	for i := len(facts) - 1; i >= 0; i-- {
		status.Dials = append(status.Dials, wm.Segment{Text: facts[i].text, Style: facts[i].style, Action: facts[i].action})
	}
	return status
}

// Chip of the session on the bar, its name and what it is doing
func (a *app) lane() wm.Lane {
	ui := a.store.ui
	lane := wm.Lane{Name: a.laneName(), Signal: wm.Idle}
	switch {
	case ui.GatewayConnected != nil && !*ui.GatewayConnected:
		lane.Signal, lane.Detail = wm.Failed, displayStatus(trimJS(ui.Status))
	case a.dialogs.waiting():
		lane.Signal, lane.Detail = wm.Blocked, tr(txt.StatusWaitingOnYou)
	case ui.Busy:
		lane.Signal, lane.Detail = wm.Working, displayStatus(trimJS(ui.Status))
	}
	return lane
}

// Name of the session lane, the session title unless a field list leaves the title out
func (a *app) laneName() string {
	ui := a.store.ui
	if len(ui.StatusBarFields) == 0 || ui.StatusBarFields["title"] {
		return a.card.Title()
	}
	return a.card.untitled()
}

// Dials of the session most important first, the model with its effort, the context, cost, session time, folder, battery, voice, live sessions and parked drafts
func (a *app) barFacts(cols, folderCols int) []statusFact {
	ui := a.store.ui
	info := infoOf(ui.Info)
	var facts []statusFact
	if ui.Yolo {
		facts = append(facts, statusFact{text: tr(txt.SessionInputYoloOn), style: wm.Bad, action: "tui.yolo", rank: rankYolo})
	}
	if model := modelLabel(info); model != "" && ui.statusField("model") {
		facts = append(facts, statusFact{text: model, style: wm.Cool, action: "model.switch", rank: rankModel})
	}
	if ui.statusField("context", "context_detail", "context_pct") {
		if text, style, has := contextDial(ui.Usage, cols >= contextFullCols, ui.statusField("context", "context_pct")); has {
			facts = append(facts, statusFact{text: text, style: style, rank: rankContext})
		}
	}
	if cost := val(usageOf(ui.Usage).CostUsd); cost > 0 && ui.statusField("cost") {
		facts = append(facts, statusFact{text: render.Money(cost), style: wm.Plain, rank: rankCost})
	}
	if ui.Sid != "" && a.sessionStart > 0 && cols >= durationCols && ui.statusField("duration") {
		facts = append(facts, statusFact{text: fmtDuration(a.store.nowMillis() - a.sessionStart), style: wm.Dim, rank: rankDuration})
	}
	if cwd := a.store.sessionCwd(); cwd != "" && ui.statusField("cwd") {
		project := ""
		if info.Project != nil {
			project = info.Project.Name
		}
		facts = append(facts, statusFact{text: fmtProjectCwdBranch(cwd, a.git.current(), project, folderCols), style: wm.Dim, rank: rankFolder})
	}
	if b := ui.BatteryStatus; ui.Battery && b != nil && b.Available && ui.statusField("battery") {
		facts = append(facts, statusFact{text: batteryText(b), style: batteryStyle(b), rank: rankBattery})
	}
	if voice, ok := a.voiceFact(); ok {
		facts = append(facts, voice)
	}
	if more := a.shell.liveElsewhere(); more > 0 && ui.StatusBar != "off" {
		facts = append(facts, statusFact{text: tr(txt.StatusSessionCount, more), style: wm.Cool, action: "open.sessions", rank: rankSessions})
	}
	if pin := a.stashIndicator(); pin != "" {
		facts = append(facts, statusFact{text: pin, style: wm.Plain, rank: rankStash})
	}
	return facts
}

// Mark of voice input: lit while voice is on, dim while off, red while recording, warm while transcribing, with the spoken replies mark
// after it while replies are read aloud and a click toggling recording while voice is on; none in a build without voice or with the voice
// field hidden
func (a *app) voiceFact() (statusFact, bool) {
	if !keyLinked("tui.voice") || !a.store.ui.statusField("voice") {
		return statusFact{}, false
	}
	g := a.uiTheme().Glyphs
	mark := g.Voice
	if mark == "" {
		mark = "~"
	}
	style := wm.Dim
	switch {
	case a.voiceRecording:
		mark, style = g.Live, wm.Bad
	case a.voiceProcessing:
		style = wm.Warm
	case a.voiceEnabled:
		style = wm.Cool
	}
	if a.voiceTTS && g.Speak != "" {
		mark += g.Speak
	}
	f := statusFact{text: mark, style: style, rank: rankVoice}
	if a.voiceEnabled {
		f.action = "tui.voice"
	}
	return f, true
}

// Read-outs of the session most important first, its compressions, cache hit share, reply latency and token rate
func (a *app) sessionFacts(cols int) []statusFact {
	ui := a.store.ui
	usage := usageOf(ui.Usage)
	var facts []statusFact
	if n := val(usage.Compressions); n > 0 && cols >= compressionsCols && ui.statusField("compressions") {
		style := wm.Dim
		switch {
		case n >= 10:
			style = wm.Bad
		case n >= 5:
			style = wm.Hot
		}
		facts = append(facts, statusFact{text: tr(txt.StatusCompressions, n), style: style, rank: rankCompressions})
	}
	if pct := usage.CacheHitPct; pct != nil && cols >= cacheHitCols && ui.statusField("cache_hit") {
		style := wm.Dim
		switch {
		case *pct >= 70:
			style = wm.Good
		case *pct >= 40:
			style = wm.Hot
		}
		facts = append(facts, statusFact{text: fmt.Sprintf("◎ %d%%", *pct), style: style, rank: rankCacheHit})
	}
	// The session averages wait for the turn to end, so a running turn never reads as having the last one's speed
	if latency := usage.AvgLatencyS; latency != nil && !ui.Busy && cols >= latencyCols && ui.statusField("latency") {
		facts = append(facts, statusFact{text: "◷ " + jsToFixed(*latency, 1) + "s", style: wm.Dim, rank: rankLatency})
	}
	if rate := usage.AvgTPS; rate != nil && !ui.Busy && cols >= tpsCols && ui.statusField("tps") {
		facts = append(facts, statusFact{text: "↑ " + spawnNumber(jsRound(*rate)) + " t/s", style: wm.Dim, rank: rankTPS})
	}
	return facts
}

// Facts beside the turn status most important first, the time since the last reply, the background tasks, the subagents in flight and the hint that the turn resumes on its own
func (a *app) turnFacts(cols int) []statusFact {
	ui := a.store.ui
	if ui.StatusBar == "off" {
		return nil
	}
	var facts []statusFact
	if !ui.Busy && ui.Sid != "" && !a.lastTurnEnded.IsZero() && cols >= durationCols {
		ago := fmtDuration(float64(a.store.now().Sub(a.lastTurnEnded).Milliseconds()))
		text := tr(txt.StatusTurnDoneAgo, ago)
		if trimJS(ui.Status) == "interrupted" || trimJS(ui.Status) == tr(txt.StatusInterrupted) {
			text = tr(txt.StatusTurnStoppedAgo, ago)
		}
		facts = append(facts, statusFact{text: text, style: wm.Dim, rank: rankIdle, turn: true})
	}
	if n := len(ui.BgTasks); n > 0 && cols >= bgTasksCols && ui.statusField("bg_tasks") {
		facts = append(facts, statusFact{text: tr(txt.StatusBgTasks, n), style: wm.Dim, rank: rankBgTasks, turn: true})
	}
	subagents := val(usageOf(ui.Usage).ActiveSubagents)
	if subagents > 0 && cols >= subagentsCols && ui.statusField("bg_subagents") {
		facts = append(facts, statusFact{text: fmt.Sprintf("⛓ %d", subagents), style: wm.Dim, rank: rankSubagents, turn: true})
	}
	if !ui.Busy && subagents > 0 {
		hint := tr(txt.StatusResumesWhenSubagentFinishes)
		if subagents != 1 {
			hint = tr(txt.StatusResumesWhenSubagentsFinish, subagents)
		}
		facts = append(facts, statusFact{text: hint, style: wm.Dim, rank: rankResume, turn: true, faint: true})
	}
	return facts
}

// Whether a clock of the status bar is counting, the session time or the time since the last reply
func (a *app) statusClockLive() bool {
	ui := a.store.ui
	if ui.Sid == "" || ui.StatusBar == "off" {
		return false
	}
	return (a.sessionStart > 0 && ui.statusField("duration")) || (!ui.Busy && !a.lastTurnEnded.IsZero())
}

// Whether the card shows something that counts by the second: a status clock, or agents and processes in its dock
func (a *app) clockLive() bool { return a.statusClockLive() || a.dockLive(a.store.nowMillis()) }

// Whether another card of the window already shows the session, that card taking the focus when so
func (a *app) shownElsewhere(id string) bool {
	other := a.shell.bySid(id)
	if other == nil || other == a {
		return false
	}
	a.shell.current = other
	a.effect(a.wm.Focus(other.card))
	return true
}

// Status as the bar and the status row show it, the state words through the language pack and any other text as it is
func displayStatus(status string) string {
	switch status {
	case "ready":
		return tr(txt.StatusReady)
	case "running…":
		return tr(txt.StatusRunning)
	case "interrupted":
		return tr(txt.StatusInterrupted)
	case english(txt.StatusSummoning):
		return tr(txt.StatusSummoning)
	}
	return status
}

// Reasoning effort shown beside the model, nothing at the default levels and the level the route sends after an arrow when it differs
func effortLabel(effort, wire string) string {
	value, sent := strings.ToLower(trimJS(effort)), strings.ToLower(trimJS(wire))
	switch {
	case value == "" || value == "medium" || value == "normal" || value == "default":
		return ""
	case sent != "" && sent != value:
		return value + "→" + sent
	}
	return value
}

// Model of the session with its reasoning effort and the mark of the fast tier
func modelLabel(info *tuigateway.SessionInfo) string {
	model := infoText(info.Model)
	if model == "" {
		return ""
	}
	if effort := effortLabel(val(info.ReasoningEffort), val(info.ReasoningEffortWire)); effort != "" {
		model += " " + effort
	}
	if val(info.Fast) {
		model += " " + tr(txt.StatusFast)
	}
	return model
}

// Context use of the session as a dial, warmer as the window fills and led by ~ when estimated, only the used count when not full and the share when percent
func contextDial(raw *tuigateway.Usage, full, percent bool) (string, wm.SegmentStyle, bool) {
	usage := usageOf(raw)
	if usage.ContextUsed == nil || usage.ContextMax == nil || *usage.ContextMax <= 0 {
		return "", wm.Plain, false
	}
	used, limit := float64(*usage.ContextUsed), float64(*usage.ContextMax)
	pct := used / limit
	style := wm.Good
	switch {
	case pct >= 0.8:
		style = wm.Bad
	case pct >= 0.5:
		style = wm.Warm
	}
	text := tr(txt.StatusContextLabel) + " "
	if val(usage.ContextEstimated) {
		text += "~"
	}
	text += compactNumber(used)
	if !full {
		return text, style, true
	}
	text += "/" + compactNumber(limit)
	if percent {
		text += fmt.Sprintf(" %d%%", int(pct*100+0.5))
	}
	return text, style, true
}

// Battery charge with its charging mark
func batteryText(b *batteryInfo) string {
	mark := "🔋"
	if b.Plugged != nil && *b.Plugged {
		mark = "⚡"
	}
	if b.Percent == nil {
		return mark + " " + b.Category
	}
	return fmt.Sprintf("%s %d%%", mark, *b.Percent)
}

// Color role of the battery by its charge category
func batteryStyle(b *batteryInfo) wm.SegmentStyle {
	switch b.Category {
	case "good":
		return wm.Good
	case "warn":
		return wm.Hot
	case "bad", "critical":
		return wm.Bad
	}
	return wm.Dim
}

// Session theme tone of a bar color role, for the facts the session window paints itself
func factTone(style wm.SegmentStyle, t theme) string {
	switch style {
	case wm.Dim:
		return t.Color.Muted
	case wm.Cool:
		return t.Color.Accent
	case wm.Warm:
		return t.Color.Primary
	case wm.Hot:
		return t.Color.Warn
	case wm.Good:
		return t.Color.Ok
	case wm.Bad:
		return t.Color.Error
	}
	return t.Color.Text
}

// Trailer of the session window, the tokens of the session and its tool and reasoning counts while a turn runs
func (a *app) frameStatus() string {
	ui := a.store.ui
	var parts []string
	if ui.Yolo {
		parts = append(parts, tr(txt.SessionInputYoloOn))
	}
	if total := usageOf(ui.Usage).Total; total > 0 {
		parts = append(parts, tr(txt.StatusTokens, compactNumber(float64(total))))
	}
	if n := a.store.turn.ReasoningTokens; n > 0 && ui.Busy {
		parts = append(parts, tr(txt.StatusReasoningTokens, compactNumber(float64(n))))
	}
	if !a.card.view.follow {
		parts = append(parts, tr(txt.StatusScrolled))
	}
	return strings.Join(parts, " · ")
}
