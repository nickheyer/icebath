package tui

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tuigateway"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/cards"
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
	// Gap between refreshes of the agent, process, live session and control snapshots
	refreshEvery = 1500 * time.Millisecond
	// Exit status that asks the launcher to run the update
	exitForUpdate = 42
)

// Session detach finished or timed out, the program quits with the status
type exitMsg struct{ code int }

// Periodic refresh of the session rosters
type refreshTickMsg struct{ gen int }

// Text the composer changed, so the streaming batch follows the typing
type typingIdleMsg struct{ gen int }

// Busy input mode and destination captured when the user committed an input
type busyControl struct {
	destination submissionDestination
	mode        string
	generation  *int64
}

// Terminal UI of one icebath session over the shared gateway
type app struct {
	ctx     context.Context
	host    Host
	wm      *wm.Model
	keys    *keys.Map
	gw      *gatewayClient
	store   *tuiStore
	turn    *turnController
	events  *eventHandler
	life    *sessionLifecycle
	prompts *promptSubmitter
	queue   *composerQueue
	config  *configSync
	inputs  *composerHistory
	card    *sessionCard
	git     *gitBranchTracker
	battery *batteryPoller
	resize  *resizeCoalescer
	arena   arenaView
	post    func(tea.Msg)
	// Terminal graphics the pet and the arena draw through, probed on their first use
	graphics *gfx.Terminal
	// Pictures the transcript draws inline
	images         *inlineImages
	graphicsProbed bool

	history       []msg
	lastUserMsg   string
	stickyPrompt  string
	sessionStart  float64
	droppedTurns  int
	argCache      map[string]argCacheEntry
	turnStartedAt time.Time
	catalog       *slashCatalog
	tokens        []composerToken
	localCommands []slashCommand
	// Terminal windows opened so far, numbering the next, and those whose program may still run
	terminalCount  int
	terminals      []*cards.Terminal
	draftRevision  int
	editorRevision int
	editorTarget   submissionDestination
	queueEdit      *int
	// When escape and enter on an empty prompt were last pressed, a second press inside its window acting on the draft, the turn or the queue
	lastEscape     time.Time
	lastEmptyEnter time.Time
	// Session keys as they were bound before the configured voice key took a chord from them, bound so again when it changes
	voiceDisplaced []keys.Action
	// Collapsed paste expanded into the draft for editing, nil when none is, and the draft as it read when the span was last placed on it
	pasteSpan  *pasteSpan
	pasteDraft string
	// Prompt put back for editing whose send rewinds the session, the session an escape is armed for and how many prompts it saw
	rewindPending    *pendingRewind
	rewindPrimed     string
	rewindPrimeCount int
	rewindGeneration int
	expandWith       func(string) string

	bellComplete    bool
	bellPrompt      bool
	voiceEnabled    bool
	voiceTTS        bool
	voiceRecording  bool
	voiceProcessing bool
	voiceKey        voiceRecordKey

	recoverSid       string
	recoveryAttempts []float64
	gaveUp           bool
	seenSid          string
	seenOffline      bool
	seenAttach       string
	seenCwd          string
	seenBusy         bool
	seenTheme        theme
	toolkit          uitheme.Theme
	themeLook        string
	refreshGen       int
	typingGen        int
	slashFlight      int
	lastInterrupt    time.Time
	exitCode         int
	quitting         bool
	exitReason       string
	lastWords        func()
	boot             tea.Cmd
	dialogs          dialogState
	widgets          *widgetHost
	pet              petCard
	stash            *promptStash
	clipboardRead    *clipboardRead
	// Sessions this view left running, watched for the end of their turn or a question waiting
	background *backgroundWatch
	// Id this UI takes the bot screen under
	screenViewer string
	// Hints about the host terminal still to show, held until the first session stands since a session start clears the activity feed
	terminalHints      []terminalHint
	clipboardSeq       int
	spawnSections      map[string]bool
	agentDockCollapsed bool
	dockClockArmed     bool
	observeUnsupported bool
	completion         composerCompletion
	// Reloads the session switcher while it is open, nil when it is closed
	switcherReload func() tea.Cmd
	// Reads the live sessions of the open session switcher again on the roster tick, nil when it is closed
	switcherPoll func() tea.Cmd
	// Status facts the store does not hold, the live sessions the roster poll counted and when the last turn ended
	liveSessions  int
	lastTurnEnded time.Time
	// Whether the one second repaint of the status clocks runs, and whether a loop countdown was showing at the last update
	statusClockArmed bool
	seenLoopTicking  bool
	// Reaction the affection heart last lit for, when it goes out and the tone it drew
	heartSeen  int
	heartUntil time.Time
	heartTone  int
	// Whether the missing clipboard tool was named already this run, the advice shown once
	clipboardWarned bool
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
		{ID: "tui.cut", Context: tuiContext, Label: "cut", Doc: "cut the selected prompt text to the clipboard", Default: []string{"ctrl+x"}},
		{ID: "tui.queue.delete", Context: tuiContext, Label: "delete queued", Doc: "delete the queued input being edited", Default: []string{"ctrl+x"}},
		{ID: "tui.sessions", Context: tuiContext, Label: "sessions", Doc: "switch between live and stored sessions", Default: []string{"ctrl+x"}},
		{ID: "tui.agents", Context: tuiContext, Label: "agents", Doc: "open the spawn tree of the agents of the session", Default: []string{"ctrl+t"}},
		{ID: "tui.editor", Context: tuiContext, Label: "editor", Doc: "edit the prompt in the external editor", Default: []string{"ctrl+g", "alt+g"}},
		{ID: "tui.redraw", Context: tuiContext, Label: "redraw", Doc: "repaint the screen", Default: []string{"ctrl+l"}},
		{ID: "tui.suspend", Context: tuiContext, Label: "suspend", Doc: "suspend to the shell, fg brings it back"},
		{ID: "tui.exit", Context: tuiContext, Label: "exit", Doc: "quit on an empty prompt", Default: []string{"ctrl+d"}},
		{ID: "tui.queue.send", Context: tuiContext, Label: "send queued", Doc: "send the first queued input now, an unconfirmed one again", Default: []string{"alt+k", "super+k"}},
		{ID: "tui.queue.drop", Context: tuiContext, Label: "drop queued", Doc: "drop the newest queued input", Default: []string{"ctrl+k"}},
		{ID: "tui.yolo", Context: tuiContext, Label: "yolo", Doc: "turn yolo on or off without spending a turn", Default: []string{"shift+tab"}},
		{ID: "tui.paste.image", Context: tuiContext, Label: "paste image", Doc: "attach the clipboard image", Default: []string{"alt+v"}},
		{ID: "tui.paste.expand", Context: tuiContext, Label: "pasted text", Doc: "open the pasted text chip under the cursor to read and edit it or fold it back, else show or hide the pasted text under the sent prompts", Default: []string{"ctrl+p"}},
		{ID: "tui.voice", Context: tuiContext, Label: "voice", Doc: "start or stop voice recording", Default: defaultVoiceRecordKey.chords()},
		{ID: "tui.scroll.line.up", Context: tuiContext, Label: "line up", Doc: "scroll the transcript up a line", Default: []string{"shift+up"}, Quiet: true},
		{ID: "tui.scroll.line.down", Context: tuiContext, Label: "line down", Doc: "scroll the transcript down a line", Default: []string{"shift+down"}, Quiet: true},
		{ID: "tui.newline", Context: tuiContext, Label: "newline", Doc: "break the line in the prompt", Default: []string{"shift+enter", "ctrl+j", "alt+enter", "ctrl+enter", "super+enter"}, Quiet: true},
	} {
		if keyLinked(a.ID) {
			km.Register(a)
		}
	}
	registerListKeys(km)
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

// App over a fresh store and the launcher's host, its gateway client not started yet
func newApp(ctx context.Context, host Host) *app {
	store := newTUIStore()
	turn := newTurnController(store)
	gw := newGatewayClient(host.Attach)
	setRPCErrorLogSink(gw.recordLog)
	a := &app{ctx: ctx, host: host, keys: tuiKeymap(), gw: gw, store: store, turn: turn, git: newGitBranchTracker(), battery: &batteryPoller{},
		resize: newResizeCoalescer(32 * time.Millisecond), inputs: newComposerHistory(newInputHistory(ctx))}
	keysLoaded := a.loadKeybinds()
	a.background = newBackgroundWatch()
	a.events, a.boot = newEventHandler(store, turn, gw, a)
	a.life = newSessionLifecycle(store, turn, gw, a)
	a.prompts = &promptSubmitter{store: store, turn: turn, gw: gw, deps: a}
	a.queue = newComposerQueue(store, gw, a.uncaught)
	a.config = newConfigSync(store, turn, gw, a)
	a.graphics = gfx.New()
	a.images = newInlineImages(a)
	store.screenCleared = a.images.redraw
	var probe tea.Cmd
	if a.images.graphics && !a.graphicsProbed {
		a.graphicsProbed = true
		probe = a.graphics.Probe()
	}
	a.arena = arenaViewFor(a)
	a.syncTheme()
	a.widgets = newWidgetHost(a)
	a.localCommands = append(append(append(append(append(append(a.coreCommands(), a.transcriptCommands()...), a.opsCommands()...), a.sessionCommands()...), a.controlCommands()...), a.wmCommands()...), a.recoveryCommands()...)
	a.localCommands = append(a.localCommands, a.nativeCommands()...)
	a.localCommands = append(a.localCommands, a.agentsCommands()...)
	a.localCommands = append(a.localCommands, a.mcpCommands()...)
	a.localCommands = append(a.localCommands, a.roomsCommands()...)
	a.localCommands = append(a.localCommands, a.terminalCommands()...)
	a.localCommands = append(a.localCommands, a.keyCommands()...)
	a.localCommands = append(a.localCommands, a.screenCommands()...)
	a.localCommands = append(a.localCommands, a.languageCommands()...)
	for _, commands := range tierCommands {
		a.localCommands = append(a.localCommands, commands(a)...)
	}
	a.localCommands = slices.DeleteFunc(a.localCommands, func(c slashCommand) bool { return !localCommandLinked(c.name) })
	a.pet = petCardFor(a)
	a.card = newSessionCard(a)
	a.wm = wm.New(wm.WithTheme(a.toolkit), wm.WithKeys(a.keys), wm.WithWindowTitle("icebath"), wm.WithMotion(a.motion()))
	_, open := a.wm.AddWorkspace("session", a.card)
	a.boot = tea.Batch(a.boot, open, keysLoaded, probe)
	a.wm.SetBar(a.barStatus)
	a.wm.SetCommands(a.commands())
	a.wm.SetEmptyHints("palette")
	return a
}

// Animation scale of the window manager, still when the environment asks for reduced motion
func bootMotion() float64 {
	if trueRe.MatchString(strings.ToLower(trimJS(os.Getenv("ICEBATH_TUI_REDUCED_MOTION")))) {
		return 0
	}
	return 1
}

// Motion scale of the window manager, none under the plain theme
func (a *app) motion() float64 {
	if a.toolkit.Plain {
		return 0
	}
	return bootMotion()
}

// Runs the terminal UI, prints the session summary when it ends and returns the exit status
func Run(ctx context.Context, host Host) int {
	if stop := startCPUProfile(os.Getenv("ICEBATH_CPUPROFILE")); stop != nil {
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
	a.post = newMailbox(ctx, p.Send).post
	if meter != nil {
		meter.post = a.post
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

func (a *app) Init() tea.Cmd {
	return tea.Batch(a.wm.Init(), a.boot, a.scheduleRefresh(), a.widgets.warm(), a.pet.start(), terminalHintsCmd)
}

func (a *app) View() tea.View {
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

func (a *app) Update(m tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m := m.(type) {
	case uv.KittyGraphicsEvent, uv.CellSizeEvent, uv.PixelSizeEvent, uv.PrimaryDeviceAttributesEvent, uv.UnknownCsiEvent:
		a.terminalGraphics(m)
	case tea.FocusMsg, tea.BlurMsg:
		a.arena.focus(m)
		_, cmd = a.wm.Update(m)
	case tea.WindowSizeMsg:
		a.terminalGraphics(m)
		_, cmd = a.wm.Update(m)
		if a.graphicsProbed {
			cmd = tea.Batch(cmd, a.graphics.Measure())
		}
		cmd = tea.Batch(cmd, a.images.redraw())
	case tea.ResumeMsg:
		_, cmd = a.wm.Update(m)
		cmd = tea.Batch(cmd, a.images.redraw())
		if _, reflow := a.resize.schedule(); reflow != nil {
			cmd = tea.Batch(cmd, reflow)
		}
	case resizeReflowMsg:
		if a.resize.fire(m) {
			a.card.view.invalidate()
			cmd = a.reportResize()
		}
	case serverRequestMsg:
		if !a.store.handleServerRequest(m.Request, a.events) {
			m.Request.unhandled()
		}
	case gatewayExitMsg:
		a.onGatewayExit(m.Code)
	case interruptReplyMsg:
		a.turn.settleInterrupt(m)
	case resumeScrollMsg:
		cmd = a.life.onResumeScroll(m)
	case configPollMsg:
		cmd = a.config.onPoll(m)
	case loopTickMsg:
		cmd = a.store.onLoopTick(m)
	case batteryPollMsg:
		cmd = a.battery.onTick(m, a.store, a.gw)
	case refreshTickMsg:
		if m.gen == a.refreshGen {
			cmd = tea.Batch(a.refreshRosters(), a.scheduleRefresh(), a.pollSwitcher())
		}
	case typingIdleMsg:
		if m.gen == a.typingGen {
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
	case sectionCommandMsg:
		cmd = a.onSectionCommand(m)
	case exitMsg:
		a.exitCode = m.code
		a.closeTerminals()
		return a, tea.Quit
	default:
		var petCmd tea.Cmd
		if ev, isEvent := m.(gatewayEventMsg); isEvent {
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
		} else if handled, c := a.pet.update(m); handled {
			cmd = c
		} else if handled, c := a.updateExtras(m); handled {
			cmd = c
		} else {
			_, cmd = a.wm.Update(m)
		}
		cmd = tea.Batch(petCmd, cmd)
	}
	after := a.afterUpdate()
	if feedsArena(m) {
		a.arena.sync()
	}
	return a, tea.Batch(cmd, after)
}

// Reports whether a message may change what the arena shows, window manager and arena frames never doing so
func feedsArena(m tea.Msg) bool {
	switch m.(type) {
	case anim.FrameMsg, wm.Addressed, petTickMsg:
		return false
	}
	return true
}

// Follows what an update changed, the session in view, the theme, the queue, the open prompts and the turn clock
func (a *app) afterUpdate() tea.Cmd {
	var cmds []tea.Cmd
	cmds = append(cmds, a.turn.takeCmds()...)
	cmds = append(cmds, a.syncCompletion())
	a.followPaste()
	ui := &a.store.ui
	offline := ui.GatewayConnected != nil && !*ui.GatewayConnected
	reconnected := a.seenOffline && !offline
	a.seenOffline = offline
	if reconnected && ui.Sid != "" {
		cmds = append(cmds, a.events.loadSlashCatalog(ui.Sid))
	}
	if ui.Sid != a.seenSid {
		a.seenSid = ui.Sid
		a.lastTurnEnded = time.Time{}
		if !ui.Busy {
			a.turnStartedAt = time.Time{}
		}
		a.card.composer.Bind(ui.Sid)
		cmds = append(cmds, a.config.syncSession(ui.Sid), a.refreshRosters())
		if ui.Sid != "" {
			if !offline && !reconnected {
				cmds = append(cmds, a.events.loadSlashCatalog(ui.Sid))
			}
			for _, hint := range a.terminalHints {
				a.turn.pushActivity(hint.message, hint.tone, "")
			}
			a.terminalHints = nil
		}
		if err := a.queue.reconcileReceipts(); err != nil {
			a.sys(errorLine(err.Error()))
		}
	}
	if attach := a.attachKey(); attach != a.seenAttach {
		a.seenAttach = attach
		if ui.Sid == "" {
			ui.SessionTitle = ""
			a.liveSessions = 0
		}
		cmds = append(cmds, a.readControl())
	}
	if ticking := a.store.loopTicking(); ticking != a.seenLoopTicking {
		a.seenLoopTicking = ticking
		cmds = append(cmds, a.store.restartLoopTicker())
	}
	if cwd := a.store.sessionCwd(); cwd != a.seenCwd {
		a.seenCwd = cwd
		cmds = append(cmds, a.git.track(cwd))
	}
	if ui.Busy != a.seenBusy {
		a.seenBusy = ui.Busy
		if !ui.Busy && !a.turnStartedAt.IsZero() {
			a.lastTurnEnded = a.store.now()
		}
		a.turnStartedAt = time.Time{}
		if ui.Busy {
			a.turnStartedAt = a.store.now()
		}
		a.card.composer.SetBusy(ui.Busy)
	}
	if !themesEqual(ui.Theme, a.seenTheme) {
		a.syncTheme()
		a.wm.SetTheme(a.toolkit)
		a.wm.SetMotion(a.motion())
		a.card.composer.SetTheme(a.toolkit)
		a.arena.retheme(a.toolkit)
		a.card.view.invalidate()
	}
	cmds = append(cmds, a.battery.sync(a.store, a.gw), a.syncDialogs(), a.drainQueue(), a.armDockClock(), a.armStatusClock(), a.followGoodVibes(), a.pet.follow())
	a.wm.Repaint()
	return tea.Batch(cmds...)
}

// Rebuilds the toolkit palette from the session theme
func (a *app) syncTheme() {
	t := a.store.ui.Theme
	a.seenTheme = t
	light := false
	if lum, ok := relativeLuminance(t.Color.Text); ok {
		light = lum < 0.35
	}
	a.toolkit = toolkitTheme(t, light)
	a.themeLook = hashText(fmt.Sprint(t.Color, t.Brand))
	a.toolkit.Name = "session-" + a.themeLook
}

func (a *app) uiTheme() uitheme.Theme { return a.toolkit }

// Sends the queue head once the session is idle and attached
func (a *app) drainQueue() tea.Cmd {
	ui := a.store.ui
	if ui.Busy || ui.Sid == "" || a.queueEdit != nil || a.quitting || a.dialogs.waiting() {
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

// Tick of the roster refresh
func (a *app) scheduleRefresh() tea.Cmd {
	a.refreshGen++
	gen := a.refreshGen
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return refreshTickMsg{gen: gen} })
}

// Reads the agents, processes, live sessions and goal and loop controls of the session in view
func (a *app) refreshRosters() tea.Cmd {
	sid := a.store.ui.Sid
	if sid == "" || (a.store.ui.GatewayConnected != nil && !*a.store.ui.GatewayConnected) {
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
		requestSessions(a.gw, true, sid, func(sessions []listedSession, err error) tea.Cmd {
			if err != nil {
				return nil
			}
			title := ""
			for _, s := range sessions {
				if s.Current || s.ID == a.store.ui.Sid {
					title = trimJS(s.Title)
					break
				}
			}
			a.store.ui.SessionTitle = title
			if a.store.ui.Sid != "" {
				a.liveSessions = len(sessions)
			}
			return a.noteLiveSessions(sessions, sid)
		}),
	)
}

// Writes the notices of the sessions this view left running into the transcript in view. The live list of the shared gateway says only running or
// not, so while a shown session is off screen its active list is read too, which says waiting when a question holds the turn
func (a *app) noteLiveSessions(rows []listedSession, focused string) tea.Cmd {
	if a.store.ui.Sid != focused {
		return nil
	}
	a.background.focus(focused)
	announce := func(rows []listedSession) tea.Cmd {
		for _, notice := range a.background.update(rows, focused) {
			a.sys(notice)
		}
		return nil
	}
	if !a.gw.canonical() || !a.background.watching(rows, focused) {
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

// Tells a legacy gateway the terminal size, the canonical wire needs none
func (a *app) reportResize() tea.Cmd {
	if a.gw.canonical() || a.store.ui.Sid == "" {
		return nil
	}
	w, _ := a.wm.Size()
	return call(a.gw, tuigateway.TerminalResize, tuigateway.TerminalResizeParams{SessionID: a.store.ui.Sid, Cols: ptr(int64(w))}, func(*tuigateway.TerminalResizeResult, error) tea.Cmd { return nil })
}

// Gateway transport ended, the turn reset and the session marked for recovery
func (a *app) onGatewayExit(code int) {
	a.turn.reset()
	ui := &a.store.ui
	stored := ui.StoredSid
	if stored == "" {
		stored = ui.Sid
	}
	off := false
	if a.gw.attached() {
		if stored != "" {
			a.recoverSid = stored
		}
		wasConnected := ui.GatewayConnected == nil || *ui.GatewayConnected
		ui.Busy, ui.Compacting, ui.GatewayConnected, ui.Status = false, false, &off, tr(txt.SessionStatusReconnecting)
		if ui.Sid != "" && wasConnected {
			a.turn.pushActivity(tr(txt.UserMessagesBackendConnectionLostActivity), "warn", "")
			a.sys(tr(txt.UserMessagesBackendConnectionLost))
		}
		return
	}
	plan := planGatewayRecovery(stored, a.recoverSid, a.recoveryAttempts, a.store.nowMillis())
	a.recoveryAttempts = plan.Attempts
	ui.Busy, ui.Compacting, ui.GatewayConnected, ui.Status = false, false, &off, tr(txt.LibTextGatewayExited)
	a.recoverSid = plan.Sid
	if plan.Recover && plan.Sid != "" {
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
	a.card.composer.SetValue(update(a.card.composer.Value()))
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

func (a *app) setVoiceEnabled(on bool)            { a.voiceEnabled, a.voiceTTS = on, on && a.voiceTTS }
func (a *app) setVoiceTTS(on bool)                { a.voiceTTS = on }
func (a *app) setVoiceRecording(on bool)          { a.voiceRecording = on; speech.recording.Store(on) }
func (a *app) setVoiceProcessing(on bool)         { a.voiceProcessing = on }
func (a *app) setVoiceRecordKey(k voiceRecordKey) { a.voiceKey = k; a.bindVoiceKey() }
func (a *app) setBellOnComplete(on bool)          { a.bellComplete = on }
func (a *app) setBellOnPrompt(on bool)            { a.bellPrompt = on }
func (a *app) inlineImageRetry()                  { a.images.retry() }

// Drops the rows drawn in the language in force before, a pack arriving after the first rows are on screen
func (a *app) languageChanged() { a.card.view.invalidate() }

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
func (a *app) expand(text string) string {
	if a.expandWith != nil {
		return a.expandWith(text)
	}
	return expandTokens(a.tokens)(text)
}

func (a *app) clearComposerTokens() {
	a.tokens = nil
	a.draftRevision++
}

func (a *app) cols() int {
	w, _ := a.wm.Size()
	return max(w, 20)
}

// Failure nothing handled, shown in the transcript with the turn ended
func (a *app) uncaught(err error) {
	if err == nil {
		return
	}
	a.sys(errorLine(rpcErrorMessage(err)))
	a.turn.idle(false)
	a.store.ui.Busy, a.store.ui.Status = false, tr(txt.StatusReady)
}

// Follows typing, the streamed reply batching faster while the user edits the prompt
func (a *app) typed(cmd tea.Cmd) tea.Cmd {
	if !a.store.ui.Busy {
		return cmd
	}
	a.turn.boostStreamingForTyping()
	a.typingGen++
	gen := a.typingGen
	return tea.Batch(cmd, tea.Tick(time.Second, func(time.Time) tea.Msg { return typingIdleMsg{gen: gen} }))
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
	a.expandWith = func(s string) string { return s }
	defer func() { a.expandWith = nil }()
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
	a.store.ui.Busy = true
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
		a.store.ui.Busy, a.store.ui.Status = false, tr(txt.StatusReady)
		if err != nil {
			a.sys(errorLine(describeShellExecError(err)))
			return nil
		}
		out := trimJS(strings.Join(slices.DeleteFunc([]string{r.Stdout, r.Stderr}, func(s string) bool { return s == "" }), "\n"))
		if out != "" {
			a.sys(out)
		}
		if r.Code != 0 || out == "" {
			a.sys(tr(txt.CommonExitCode, r.Code))
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

// Admits an input typed while a turn runs, queued, steered into the turn or redirecting it by the busy mode
func (a *app) handleBusyInput(item *queueItem, fallbackToFront bool, control busyControl) tea.Cmd {
	destination := control.destination
	enqueueText := func() tea.Cmd {
		var err error
		if fallbackToFront {
			err = a.queue.prependQ(item, &destination)
		} else {
			_, err = a.queue.enqueue(item.Text, item.Display, &destination, item.Attachments)
		}
		if err != nil {
			a.sys(tr(txt.CanonicalQueueNotSavedDraftKept, err.Error()))
		}
		return nil
	}
	if a.gw.canonical() && control.mode != "queue" {
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
	if control.mode == "queue" {
		if a.gw.canonical() {
			opts := submitOpts{Destination: &destination, BehindTurn: true, Attachments: item.Attachments}
			if item.Settle != nil {
				opts.QueueItem = item
			}
			return a.sendExpanded(item.Text, true, item.Display, opts)
		}
		return enqueueText()
	}
	opts := submitOpts{Destination: &destination, Attachments: item.Attachments}
	if item.Settle != nil {
		queued := false
		item.Queued = &queued
		opts.QueueItem = item
	}
	return a.sendExpanded(item.Text, true, item.Display, opts)
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
		a.card.composer.SetValue(full)
		return nil
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
	a.store.stagePastes(full, text, tokens)
	ui := a.store.ui
	if ui.Sid == "" || (ui.GatewayConnected != nil && !*ui.GatewayConnected) {
		if _, err := a.queue.enqueue(text, full, &destination, attachments); err != nil {
			return notSaved(err)
		}
		clearInput()
		return nil
	}
	if a.queueEdit != nil {
		index := *a.queueEdit
		a.queueEdit = nil
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
				if control.mode == "queue" && !a.gw.canonical() {
					if err := a.queue.prependQ(item, &control.destination); err != nil {
						a.sys(tr(txt.CanonicalQueueNotSavedDraftKept, err.Error()))
					}
					return nil
				}
				return a.handleBusyInput(item, true, control)
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
		return a.handleBusyInput(&item, false, control)
	}
	if shouldInterpolateSubmission(full) {
		item, err := a.queue.stage(text, full, &destination, attachments)
		if err != nil {
			return notSaved(err)
		}
		clearInput()
		a.store.ui.Busy = true
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

// Interrupts the running turn of the session in view
func (a *app) interruptTurn() tea.Cmd { return a.stopTurn(false) }

// Interrupts the running turn, the session staying busy when a queued input goes out as it settles
func (a *app) stopTurn(keepBusy bool) tea.Cmd {
	deps := interruptDeps{AppendMessage: a.appendMessage, Gateway: a.gw, Sid: a.store.ui.Sid, Sys: a.sys}
	cmd, err := a.turn.interruptTurn(deps, keepBusy)
	if err != nil {
		a.uncaught(err)
		return nil
	}
	return cmd
}

// Interrupt key, a prompt selection is copied, then a draft clears even while a turn runs, then the running turn stops, then an armed rewind is dropped and a second press within the window quits
func (a *app) interruptKey() tea.Cmd {
	now := a.store.now()
	_, selected := a.card.composer.Selection()
	switch {
	case a.wm.Overlay() != nil:
		return a.interruptOverlay()
	case selected:
		return a.card.composer.Action("composer.copy")
	case a.card.composer.Value() != "":
		a.cancelPendingRewind(true)
		a.lastInterrupt = time.Time{}
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
	return wm.Notify(tr(txt.SessionNoticesPressAgainToExit), wm.Info)
}

// Escape, a second press within the window discards the draft or stops the turn, any other steps towards the earlier prompts or leaves a queued input being edited
func (a *app) escape() tea.Cmd {
	now := a.store.now()
	double := !a.lastEscape.IsZero() && now.Sub(a.lastEscape) <= doubleEscapeWindow
	a.lastEscape = now
	if double {
		a.lastEscape = time.Time{}
		if cmd, acted := a.doubleEscape(); acted {
			a.rewindPrimed = ""
			return cmd
		}
	}
	if cmd, taken := a.backtrackEscape(); taken {
		return cmd
	}
	if a.queueEdit != nil {
		return a.clearIn()
	}
	return nil
}

// Quits with the status after detaching the session, a quit of the person's own named so in the crash log
func (a *app) die(code int) tea.Cmd {
	if code == 0 {
		return a.dieFor(code, "app.die")
	}
	return a.dieFor(code, fmt.Sprintf("app.dieWithCode:%d", code))
}

// Quits after detaching the session, never waiting longer than the close limit, the reason of the first quit standing
func (a *app) dieFor(code int, reason string) tea.Cmd {
	if a.quitting {
		return nil
	}
	a.quitting = true
	a.exitReason = reason
	a.store.ui.Status = tr(txt.SessionStatusClosing)
	done := func() tea.Msg { return exitMsg{code: code} }
	return tea.Batch(
		a.images.release(),
		tea.Sequence(a.life.closeForExit(), done),
		tea.Tick(exitCloseLimit, func(time.Time) tea.Msg { return exitMsg{code: code} }),
	)
}

// Commands the window manager runs for global keys and lists in its palette
func (a *app) commands() []wm.Command {
	idle := func() bool { return !a.dialogs.waiting() }
	cmds := []wm.Command{
		{ID: "interrupt", Run: a.interruptKey},
		{ID: "quit", Title: "quit", Run: func() tea.Cmd { return a.die(0) }},
		{ID: "session.new", Title: "new session", When: idle, Run: func() tea.Cmd { return a.slash("/new", nil, false) }},
		{ID: "model.switch", Title: "model", When: idle, Run: a.openModelPicker},
		{ID: "open.sessions", Title: "sessions", When: idle, Run: a.openSessionSwitcher},
		{ID: "open.agents", Title: "agents", When: idle, Run: func() tea.Cmd { return a.openSpawnTree(0, nil) }},
		{ID: "open.skills", Title: "skills", When: idle, Run: a.openSkillsHub},
		{ID: "open.plugins", Title: "plugins", When: idle, Run: a.openPluginsHub},
		{ID: "open.rooms", Title: "rooms", When: idle, Run: a.openRooms},
		{ID: "terminal.new", Title: "terminal", Run: func() tea.Cmd { return a.openTerminal("", false) }},
		{ID: "terminal.tab", Title: "terminal as a tab", Run: func() tea.Cmd { return a.openTerminal("", true) }},
		{ID: "session.compact", Title: "compact", When: idle, Run: func() tea.Cmd { return a.slash("/compress", nil, false) }},
		{ID: "run.cancel", Title: "interrupt the turn", When: func() bool { return a.store.ui.Busy }, Run: a.interruptTurn},
		{ID: "tui.exit", When: func() bool { return idle() && a.card.composer.Value() == "" && a.card.composer.Empty() }, Run: func() tea.Cmd { return a.die(0) }},
		{ID: "tui.queue.drop", When: a.hasQueued, Run: a.dropQueued},
		{ID: "tui.queue.send", When: a.queueHeadWaits, Run: a.sendQueueHead},
		{ID: "tui.queue.delete", When: func() bool { return a.queueEdit != nil }, Run: a.deleteQueueEdit},
		{ID: "tui.cut", When: a.selecting, Run: a.cutSelection},
	}
	return append(cmds, a.slashPaletteCommands()...)
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
	rankModel = iota
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
	lane := wm.Lane{Name: a.laneName(), Signal: wm.Idle}
	switch {
	case ui.GatewayConnected != nil && !*ui.GatewayConnected:
		lane.Signal, lane.Detail = wm.Failed, displayStatus(trimJS(ui.Status))
	case a.dialogs.waiting():
		lane.Signal, lane.Detail = wm.Blocked, tr(txt.StatusWaitingOnYou)
	case ui.Busy:
		lane.Signal, lane.Detail = wm.Working, displayStatus(trimJS(ui.Status))
	}
	status := wm.Status{Lanes: map[*wm.Workspace]wm.Lane{}}
	if ws := a.wm.Active(); ws != nil {
		status.Lanes[ws] = lane
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
	if a.liveSessions > 0 && ui.StatusBar != "off" {
		facts = append(facts, statusFact{text: tr(txt.StatusSessionCount, a.liveSessions), style: wm.Cool, action: "open.sessions", rank: rankSessions})
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
	if latency := usage.AvgLatencyS; latency != nil && cols >= latencyCols && ui.statusField("latency") {
		facts = append(facts, statusFact{text: "◷ " + jsToFixed(*latency, 1) + "s", style: wm.Dim, rank: rankLatency})
	}
	if rate := usage.AvgTPS; rate != nil && cols >= tpsCols && ui.statusField("tps") {
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
		idle := fmtDuration(float64(a.store.now().Sub(a.lastTurnEnded).Milliseconds()))
		facts = append(facts, statusFact{text: "✓ " + idle, style: wm.Dim, rank: rankIdle, turn: true})
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

// Repaints the status clocks every second while one counts, the tick stopping with them
func (a *app) armStatusClock() tea.Cmd {
	if a.statusClockArmed || !a.statusClockLive() {
		return nil
	}
	a.statusClockArmed = true
	return tea.Tick(time.Second, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd {
			a.statusClockArmed = false
			return a.armStatusClock()
		}}
	})
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
