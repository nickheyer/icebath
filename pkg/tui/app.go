package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	uitheme "github.com/nickheyer/icebath/pkg/ui/theme"
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
	generation  *float64
}

// Terminal UI of one icebath session over the shared gateway
type app struct {
	ctx     context.Context
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

	history        []msg
	lastUserMsg    string
	stickyPrompt   string
	sessionStart   float64
	turnStartedAt  time.Time
	catalog        *slashCatalog
	tokens         []composerToken
	localCommands  []slashCommand
	draftRevision  int
	editorRevision int
	queueEdit      *int
	expandWith     func(string) string

	bellComplete    bool
	bellPrompt      bool
	voiceEnabled    bool
	voiceRecording  bool
	voiceProcessing bool
	voiceKey        voiceRecordKey

	recoverSid       string
	recoveryAttempts []float64
	gaveUp           bool
	seenSid          string
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
	boot             tea.Cmd
	dialogs          dialogState
	widgets          *widgetHost
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
		{ID: "tui.sessions", Context: tuiContext, Label: "sessions", Doc: "switch between live and stored sessions", Default: []string{"ctrl+x"}},
		{ID: "tui.agents", Context: tuiContext, Label: "agents", Doc: "open the agents of the session", Default: []string{"ctrl+t"}},
		{ID: "tui.editor", Context: tuiContext, Label: "editor", Doc: "edit the prompt in the external editor", Default: []string{"ctrl+g", "alt+g"}},
		{ID: "tui.redraw", Context: tuiContext, Label: "redraw", Doc: "repaint the screen", Default: []string{"ctrl+l"}},
		{ID: "tui.exit", Context: tuiContext, Label: "exit", Doc: "quit on an empty prompt", Default: []string{"ctrl+d"}},
		{ID: "tui.queue.drop", Context: tuiContext, Label: "drop queued", Doc: "drop the newest queued input", Default: []string{"ctrl+k"}},
		{ID: "tui.paste.image", Context: tuiContext, Label: "paste image", Doc: "attach the clipboard image", Default: []string{"alt+v"}},
		{ID: "tui.voice", Context: tuiContext, Label: "voice", Doc: "start or stop voice recording", Default: []string{"ctrl+b"}},
		{ID: "tui.scroll.line.up", Context: tuiContext, Label: "line up", Doc: "scroll the transcript up a line", Default: []string{"shift+up"}, Quiet: true},
		{ID: "tui.scroll.line.down", Context: tuiContext, Label: "line down", Doc: "scroll the transcript down a line", Default: []string{"shift+down"}, Quiet: true},
		{ID: "tui.newline", Context: tuiContext, Label: "newline", Doc: "break the line in the prompt", Default: []string{"shift+enter", "ctrl+j", "alt+enter"}, Quiet: true},
	} {
		km.Register(a)
	}
	registerListKeys(km)
	return km
}

// App over a fresh store, its gateway client not started yet
func newApp(ctx context.Context) *app {
	store := newTUIStore()
	turn := newTurnController(store)
	gw := newGatewayClient()
	a := &app{ctx: ctx, keys: tuiKeymap(), gw: gw, store: store, turn: turn, git: newGitBranchTracker(), battery: &batteryPoller{},
		resize: newResizeCoalescer(32 * time.Millisecond), inputs: newComposerHistory(newInputHistory(ctx))}
	a.events, a.boot = newEventHandler(store, turn, gw, a)
	a.life = newSessionLifecycle(store, turn, gw, a)
	a.prompts = &promptSubmitter{store: store, turn: turn, gw: gw, deps: a}
	a.queue = newComposerQueue(store, gw, a.uncaught)
	a.config = newConfigSync(store, turn, gw, a)
	a.syncTheme()
	a.widgets = newWidgetHost(a)
	a.localCommands = append(append(append(append(a.coreCommands(), a.transcriptCommands()...), a.opsCommands()...), a.sessionCommands()...), a.controlCommands()...)
	a.card = newSessionCard(a)
	a.wm = wm.New(wm.WithTheme(a.toolkit), wm.WithKeys(a.keys), wm.WithWindowTitle("icebath"), wm.WithMotion(bootMotion()))
	_, open := a.wm.AddWorkspace("session", a.card)
	a.boot = tea.Batch(a.boot, open)
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

// Runs the terminal UI on the process terminal until the user quits, the exit status as the result
func Run(ctx context.Context) int {
	a := newApp(ctx)
	p := tea.NewProgram(a, tea.WithContext(ctx))
	a.gw.attach(p.Send)
	a.gw.start()
	_, err := p.Run()
	a.gw.kill("terminal UI exit")
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "icebath: "+err.Error())
		return 1
	}
	if goodbye := trimJS(a.store.ui.Theme.Brand.Goodbye); goodbye != "" && a.exitCode == 0 {
		fmt.Fprintln(os.Stdout, goodbye)
	}
	return a.exitCode
}

func (a *app) Init() tea.Cmd {
	return tea.Batch(a.wm.Init(), a.boot, a.scheduleRefresh(), a.widgets.warm())
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
	v.WindowTitle = composeTabTitle(a.tabMarker(), a.store.ui.SessionTitle, infoText(a.store.ui.Info, "model"), a.store.sessionCwd(), 40)
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
	case tea.WindowSizeMsg:
		_, cmd = a.wm.Update(m)
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
			cmd = tea.Batch(a.refreshRosters(), a.scheduleRefresh())
		}
	case typingIdleMsg:
		if m.gen == a.typingGen {
			a.turn.relaxStreaming()
		}
	case cards.SubmitMsg:
		cmd = a.dispatchSubmission(m.Text)
	case editorDoneMsg:
		cmd = a.onEditorDone(m)
	case exitMsg:
		a.exitCode = m.code
		return a, tea.Quit
	default:
		if handled, c := a.events.update(m); handled {
			cmd = c
		} else if handled, c := a.git.update(m); handled {
			cmd = c
		} else if handled, c := a.updateExtras(m); handled {
			cmd = c
		} else {
			_, cmd = a.wm.Update(m)
		}
	}
	return a, tea.Batch(cmd, a.afterUpdate())
}

// Follows what an update changed, the session in view, the theme, the queue, the open prompts and the turn clock
func (a *app) afterUpdate() tea.Cmd {
	var cmds []tea.Cmd
	cmds = append(cmds, a.turn.takeCmds()...)
	ui := &a.store.ui
	if ui.Sid != a.seenSid {
		a.seenSid = ui.Sid
		a.card.composer.Bind(ui.Sid)
		cmds = append(cmds, a.config.syncSession(ui.Sid), a.store.restartLoopTicker(), a.refreshRosters())
		if ui.Sid != "" {
			cmds = append(cmds, a.events.loadSlashCatalog(ui.Sid))
		}
		if err := a.queue.reconcileReceipts(); err != nil {
			a.sys("error: " + err.Error())
		}
	}
	if cwd := a.store.sessionCwd(); cwd != a.seenCwd {
		a.seenCwd = cwd
		cmds = append(cmds, a.git.track(cwd))
	}
	if ui.Busy != a.seenBusy {
		a.seenBusy = ui.Busy
		a.turnStartedAt = time.Time{}
		if ui.Busy {
			a.turnStartedAt = a.store.now()
		}
		a.card.composer.SetBusy(ui.Busy)
	}
	if !themesEqual(ui.Theme, a.seenTheme) {
		a.syncTheme()
		a.wm.SetTheme(a.toolkit)
		a.card.composer.SetTheme(a.toolkit)
		a.card.view.invalidate()
	}
	cmds = append(cmds, a.battery.sync(a.store, a.gw), a.syncDialogs(), a.drainQueue())
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
		a.sys("error: " + err.Error())
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
	quiet := func(method string, apply func(map[string]any)) tea.Cmd {
		return gatewayCall(a.gw, method, map[string]any{"session_id": sid}, func(v any, err error) tea.Cmd {
			if r := asRPCResult(v); err == nil && r != nil && a.store.ui.Sid == sid {
				apply(r)
			}
			return nil
		})
	}
	return tea.Batch(
		quiet("subagent.list", func(r map[string]any) { a.store.applyAgentSnapshot(sid, r) }),
		quiet("process.list", func(r map[string]any) {
			processes, _ := r["processes"].([]any)
			a.store.applyProcessSnapshot(sid, processes)
		}),
		quiet("session.control.read", func(r map[string]any) { a.store.applyControlSnapshot(sid, r) }),
	)
}

// Tells a legacy gateway the terminal size, the canonical wire needs none
func (a *app) reportResize() tea.Cmd {
	if a.gw.canonical() || a.store.ui.Sid == "" {
		return nil
	}
	w, _ := a.wm.Size()
	return gatewayCall(a.gw, "terminal.resize", map[string]any{"session_id": a.store.ui.Sid, "cols": w}, func(any, error) tea.Cmd { return nil })
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
		ui.Busy, ui.Compacting, ui.GatewayConnected, ui.Status = false, false, &off, tr("session.status.reconnecting")
		if ui.Sid != "" && wasConnected {
			a.turn.pushActivity(tr("userMessages.backend.connectionLostActivity"), "warn", "")
			a.sys(tr("userMessages.backend.connectionLost"))
		}
		return
	}
	plan := planGatewayRecovery(stored, a.recoverSid, a.recoveryAttempts, a.store.nowMillis())
	a.recoveryAttempts = plan.Attempts
	ui.Busy, ui.Compacting, ui.GatewayConnected, ui.Status = false, false, &off, tr("libText.gateway.exited")
	a.recoverSid = plan.Sid
	if plan.Recover && plan.Sid != "" {
		a.turn.pushActivity(tr("userMessages.backend.restartingActivity"), "warn", "")
		a.sys(tr("userMessages.backend.restarting"))
		return
	}
	ui.Status = tr("session.status.stopped")
	if !a.gaveUp {
		a.gaveUp = true
		a.turn.pushActivity(tr("userMessages.backend.gaveUpActivity"), "error", "")
		last, _ := lastStderrLine(a.gw.logTail(20))
		a.sys("error: " + backendGaveUp(&code, last))
	}
}

func (a *app) sys(text string) { a.appendMessage(msg{Role: "system", Text: text}) }

func (a *app) appendMessage(m msg) {
	a.history = capTranscriptHistory(appendTranscriptMessage(a.history, m, a.store.nowMillis))
}

func (a *app) panel(title string, sections []panelSection) {
	a.appendMessage(msg{Kind: "panel", Role: "system", PanelData: &panelData{Title: title, Sections: sections}})
}

func (a *app) setHistoryItems(update func([]msg) []msg) { a.history = update(a.history) }

func (a *app) setInput(update func(string) string) {
	a.card.composer.SetValue(update(a.card.composer.Value()))
}

func (a *app) enqueue(text, display string, destination submissionDestination) {
	if _, err := a.queue.enqueue(text, display, &destination, nil); err != nil {
		a.sys(tr("canonical.queue.notSavedDraftKept", err.Error()))
	}
}

func (a *app) submit(text string) tea.Cmd { return a.dispatchSubmission(text) }

func (a *app) submitLiteral(text string, attachments []imageAttachment) tea.Cmd {
	return a.send(text, true, "", submitOpts{Attachments: attachments})
}

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
	a.card.composer.SetCommands(a.composerCommands())
}

func (a *app) setVoiceEnabled(on bool)            { a.voiceEnabled = on }
func (a *app) setVoiceRecording(on bool)          { a.voiceRecording = on }
func (a *app) setVoiceProcessing(on bool)         { a.voiceProcessing = on }
func (a *app) setVoiceRecordKey(k voiceRecordKey) { a.voiceKey = k }
func (a *app) setBellOnComplete(on bool)          { a.bellComplete = on }
func (a *app) setBellOnPrompt(on bool)            { a.bellPrompt = on }
func (a *app) recoverSidRef() *string             { return &a.recoverSid }
func (a *app) bellOnComplete() bool               { return a.bellComplete }
func (a *app) bellOnPrompt() bool                 { return a.bellPrompt }
func (a *app) setLastUserMsg(text string)         { a.lastUserMsg = text }
func (a *app) setSessionStartedAt(ms float64)     { a.sessionStart = ms }
func (a *app) setStickyPrompt(text string)        { a.stickyPrompt = text }
func (a *app) scroll() scrollHandle               { return a.card.view }
func (a *app) onFreshSessionStarted(string)       { a.gaveUp = false }
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
	a.sys("error: " + rpcErrorMessage(err))
	a.turn.idle(false)
	a.store.ui.Busy, a.store.ui.Status = false, tr("status.ready")
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
			a.sys(tr("canonical.queue.notSavedDraftKept", err.Error()))
			a.store.ui.Status = tr("canonical.queue.statusInputNotSaved")
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

// Delivers a queued input the way it was typed, a command, a shell line or a prompt
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
	switch {
	case looksLikeSlashCommand(item.Text):
		item.settle(true)
		return a.slash(item.Text, &slashSubmission{attachments: item.Attachments, expand: func(s string) string { return s }}, false)
	case strings.HasPrefix(item.Text, "!"):
		return a.shellExec(trimJS(item.Text[1:]), *destination, item)
	}
	return a.sendExpanded(item.Text, true, item.Display, submitOpts{Destination: destination, QueueItem: item, Attachments: item.Attachments})
}

// Runs a shell line in the session and prints what it wrote and its exit status
func (a *app) shellExec(command string, destination submissionDestination, item *queueItem) tea.Cmd {
	focused := func() bool { return a.store.isCurrentDestination(destination) }
	a.appendMessage(msg{Role: "user", Text: "!" + command})
	a.store.markSubmitting()
	return gatewayCall(a.gw, "shell.exec", map[string]any{"command": command, "session_id": destination.Sid}, func(v any, err error) tea.Cmd {
		r := asRPCResult(v)
		if item != nil {
			item.settle(err == nil && r != nil)
		}
		if !focused() {
			return nil
		}
		a.store.ui.Busy, a.store.ui.Status = false, tr("status.ready")
		switch {
		case err != nil:
			a.sys("error: " + describeShellExecError(err))
		case r == nil:
			a.sys("error: " + tr("session.common.invalidResponse", "shell.exec"))
		default:
			var parts []string
			for _, k := range []string{"stdout", "stderr"} {
				if s, _ := r[k].(string); s != "" {
					parts = append(parts, s)
				}
			}
			out := trimJS(strings.Join(parts, "\n"))
			if out != "" {
				a.sys(out)
			}
			if code, _ := r["code"].(float64); code != 0 || out == "" {
				a.sys(fmt.Sprintf("exit %d", int(code)))
			}
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
	if generation, isNum := a.store.ui.Info["execution_generation"].(float64); isNum {
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
			a.sys(tr("canonical.queue.notSavedDraftKept", err.Error()))
		}
		return nil
	}
	if a.gw.canonical() && control.mode != "queue" {
		staged := item
		if staged.Settle == nil {
			var err error
			if staged, err = a.queue.stage(item.Text, item.Display, &destination, item.Attachments); err != nil {
				a.sys(tr("canonical.queue.notSavedDraftKept", err.Error()))
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
	destination, err := a.store.captureDestination()
	if err != nil {
		a.uncaught(err)
		return nil
	}
	tokens := a.tokens
	text, attachments := prepareSubmission(full, tokens)
	clearInput := func() { a.clearComposerTokens() }
	notSaved := func(err error) tea.Cmd {
		a.sys(tr("canonical.queue.notSavedDraftKept", err.Error()))
		a.store.ui.Status = tr("canonical.queue.statusInputNotSaved")
		a.card.composer.SetValue(full)
		return nil
	}
	if looksLikeSlashCommand(full) {
		command, shown := prepareSlashSubmission(full, tokens)
		a.appendMessage(msg{Kind: "slash", Role: "system", Text: shown})
		name, _ := parseSlashCommand(full)
		if queued := queueItemFromSlash(shown, command); queued != nil && (name == "queue" || name == "q") {
			if _, err := a.queue.enqueue(queued.Text, queued.Display, &destination, attachments); err != nil {
				return notSaved(err)
			}
			clearInput()
			a.sys(`queued: "` + truncateEnd(queued.Display, 50) + `"`)
			return a.reportSlashCommand(name)
		}
		clearInput()
		return a.slash(command, &slashSubmission{attachments: attachments, expand: expandTokens(tokens)}, true)
	}
	if strings.HasPrefix(full, "!") {
		clearInput()
		return a.shellExec(trimJS(full[1:]), destination, nil)
	}
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
						a.sys(tr("canonical.queue.notSavedDraftKept", err.Error()))
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
	clearInput()
	if shouldInterpolateSubmission(full) {
		item, err := a.queue.stage(text, full, &destination, attachments)
		if err != nil {
			return notSaved(err)
		}
		a.store.ui.Busy = true
		return a.interpolate(text, destination, item, func(result string) tea.Cmd {
			return a.sendExpanded(result, true, "", submitOpts{Destination: &destination, QueueItem: item, Attachments: attachments})
		})
	}
	return a.sendExpanded(text, true, full, submitOpts{Destination: &destination, Attachments: attachments})
}

// Interrupts the running turn of the session in view
func (a *app) interruptTurn() tea.Cmd {
	deps := interruptDeps{AppendMessage: a.appendMessage, Gateway: a.gw, Sid: a.store.ui.Sid, Sys: a.sys}
	cmd, err := a.turn.interruptTurn(deps, false)
	if err != nil {
		a.uncaught(err)
		return nil
	}
	return cmd
}

// Interrupt key, a running turn stops, then a draft clears, then a second press within the window quits
func (a *app) interruptKey() tea.Cmd {
	now := a.store.now()
	switch {
	case a.dialogs.waiting():
		return a.dialogs.decline(a)
	case a.store.ui.Busy:
		a.lastInterrupt = time.Time{}
		return a.interruptTurn()
	case !a.card.composer.Empty():
		a.clearComposerTokens()
		a.lastInterrupt = time.Time{}
		return a.card.composer.SetValue("")
	case !a.lastInterrupt.IsZero() && now.Sub(a.lastInterrupt) < exitConfirmWindow:
		return a.die(0)
	}
	a.lastInterrupt = now
	return wm.Notify("press again to exit", wm.Info)
}

// Escape, a queue edit is dropped, then a running turn is interrupted
func (a *app) escape() tea.Cmd {
	switch {
	case a.queueEdit != nil:
		a.queueEdit = nil
		if err := a.queue.setQueueEdit(nil); err != nil {
			a.sys("error: " + err.Error())
		}
		return a.card.composer.SetValue("")
	case a.store.ui.Busy:
		return a.interruptTurn()
	}
	return nil
}

// Quits after detaching the session, never waiting longer than the close limit
func (a *app) die(code int) tea.Cmd {
	if a.quitting {
		return nil
	}
	a.quitting = true
	a.store.ui.Status = "closing…"
	done := func() tea.Msg { return exitMsg{code: code} }
	return tea.Batch(
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
		{ID: "open.agents", Title: "agents", When: idle, Run: a.openAgents},
		{ID: "open.skills", Title: "skills", When: idle, Run: a.openSkillsHub},
		{ID: "open.plugins", Title: "plugins", When: idle, Run: a.openPluginsHub},
		{ID: "session.compact", Title: "compact", When: idle, Run: func() tea.Cmd { return a.slash("/compress", nil, false) }},
		{ID: "run.cancel", Title: "interrupt the turn", When: func() bool { return a.store.ui.Busy }, Run: a.interruptTurn},
		{ID: "tui.exit", When: func() bool { return a.card.composer.Empty() && !a.store.ui.Busy }, Run: func() tea.Cmd { return a.die(0) }},
		{ID: "tui.queue.drop", When: a.hasQueued, Run: a.dropQueued},
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
		a.sys("error: " + err.Error())
		return nil
	}
	return tea.Batch(cmd, wm.Notify("dropped the newest queued input", wm.Info))
}

// Bar of the window manager, the session lane with its signal and the model, context, cost and folder dials
func (a *app) barStatus() wm.Status {
	ui := a.store.ui
	lane := wm.Lane{Name: a.card.Title(), Signal: wm.Idle}
	switch {
	case ui.GatewayConnected != nil && !*ui.GatewayConnected:
		lane.Signal, lane.Detail = wm.Failed, trimJS(ui.Status)
	case a.dialogs.waiting():
		lane.Signal, lane.Detail = wm.Blocked, "waiting on you"
	case ui.Busy:
		lane.Signal, lane.Detail = wm.Working, trimJS(ui.Status)
	}
	status := wm.Status{Lanes: map[*wm.Workspace]wm.Lane{}}
	if ws := a.wm.Active(); ws != nil {
		status.Lanes[ws] = lane
	}
	if text, signal, waiting := a.dialogs.notice(); waiting {
		status.Notice = wm.Notice{Text: text, Signal: signal}
	}
	fields := ui.StatusBarFields
	show := func(name string) bool {
		return ui.StatusBar != "off" && (fields == nil || fields[name] || len(fields) == 0)
	}
	var dials []wm.Segment
	if b := ui.BatteryStatus; ui.Battery && b != nil && b.Available {
		dials = append(dials, wm.Segment{Text: batteryText(b), Style: wm.Dim})
	}
	if cwd := a.store.sessionCwd(); cwd != "" && show("cwd") {
		dials = append(dials, wm.Segment{Text: fmtCwdBranch(cwd, a.git.current(), 36), Style: wm.Dim})
	}
	if a.sessionStart > 0 && show("duration") {
		dials = append(dials, wm.Segment{Text: fmtDuration(a.store.nowMillis() - a.sessionStart), Style: wm.Dim})
	}
	if cost, isNum := ui.Usage["cost_usd"].(float64); isNum && cost > 0 && show("cost") {
		dials = append(dials, wm.Segment{Text: render.Money(cost), Style: wm.Plain})
	}
	if ui.StatusBar == "bottom" {
		status.Dials = nil
		return status
	}
	if text, style, has := contextDial(ui.Usage); has && show("context") {
		dials = append(dials, wm.Segment{Text: text, Style: style})
	}
	if model := infoText(ui.Info, "model"); model != "" && show("model") {
		dials = append(dials, wm.Segment{Text: model, Style: wm.Cool, Action: "model.switch"})
	}
	status.Dials = dials
	return status
}

// Context use of the session as a dial, warmer as the window fills
func contextDial(usage map[string]any) (string, wm.SegmentStyle, bool) {
	used, hasUsed := usage["context_used"].(float64)
	limit, hasMax := usage["context_max"].(float64)
	if !hasUsed || !hasMax || limit <= 0 {
		return "", wm.Plain, false
	}
	pct := used / limit
	style := wm.Good
	switch {
	case pct >= 0.8:
		style = wm.Bad
	case pct >= 0.5:
		style = wm.Warm
	}
	return fmt.Sprintf("ctx %s/%s %d%%", compactNumber(used), compactNumber(limit), int(pct*100+0.5)), style, true
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

// Trailer of the session window, the tokens of the session and its tool and reasoning counts while a turn runs
func (a *app) frameStatus() string {
	ui := a.store.ui
	var parts []string
	if total, isNum := ui.Usage["total"].(float64); isNum && total > 0 {
		parts = append(parts, compactNumber(total)+" tokens")
	}
	if n := a.store.turn.ReasoningTokens; n > 0 && ui.Busy {
		parts = append(parts, compactNumber(float64(n))+" reasoning")
	}
	if len(ui.BgTasks) > 0 {
		parts = append(parts, countNoun(len(ui.BgTasks), "background task"))
	}
	if !a.card.view.follow {
		parts = append(parts, "scrolled")
	}
	return strings.Join(parts, " · ")
}

// Rows of the background process dock, each process with its state and newest output
func (a *app) dockRows(width int, rc renderCtx) []string {
	var rows []string
	for _, row := range a.store.currentProcessRows(a.store.nowMillis()) {
		glyph, tone := processGlyph(row.Status, rc.t)
		elapsed := render.Elapsed(time.Duration(row.ElapsedSeconds * float64(time.Second)).Truncate(time.Second))
		head := "  " + toneStyle(tone).Render(glyph) + " " + toneStyle(rc.t.Color.Text).Render(truncateEnd(row.Command, max(width/2, 12)))
		tail := toneStyle(rc.t.Color.Muted).Render(" · " + elapsed + " · " + truncateEnd(row.Detail, max(width/3, 8)))
		rows = append(rows, render.Fit(head+tail, width, rc.ut))
	}
	return rows
}
