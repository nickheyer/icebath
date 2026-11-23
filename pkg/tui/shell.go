package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tuigateway"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/gfx"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	uitheme "github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/tty"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Part of the terminal UI every session card shares: window manager, connection, keymap, terminal, display preferences and the open sessions
type shell struct {
	ctx  context.Context
	host Host
	wm   *wm.Model
	keys *keys.Map
	gw   *gatewayClient
	// Mailbox of the program, for messages from outside the update loop
	post func(tea.Msg)
	// Display preferences and connection state every session card shows, and the theme negotiated with the terminal
	ui     *sharedUI
	themes *themeSync
	// Subscriptions this window holds on the sessions of the background service
	subs *subscriptions
	// Prompt history every composer reads and writes
	inputs *composerHistory
	resize *resizeCoalescer
	// Battery reading of the status bar, polled while the preference is on
	battery *batteryPoller
	// Terminal graphics the pet and the arena draw through, probed on their first use
	graphics       *gfx.Terminal
	graphicsProbed bool
	// Terminal windows opened so far, numbering the next, and those whose program may still run
	terminalCount int
	terminals     []*cards.Terminal
	// Configured voice chord another key holds and that key, named once in a notice
	voiceKeyWarned string
	voiceKey       voiceRecordKey
	toolkit        uitheme.Theme
	themeLook      string
	lastInterrupt  time.Time
	exitCode       int
	quitting       bool
	exitReason     string
	lastWords      func()
	// Sessions this view left running, watched for the end of their turn or a question waiting
	background *backgroundWatch
	// Id this UI takes the bot screen under
	screenViewer string
	// Hints about the host terminal still to show, held until the first session stands since a session start clears the activity feed
	terminalHints []terminalHint
	// Whether the missing clipboard tool was named already this run, the advice shown once
	clipboardWarned bool
	// Sessions open in session cards, in the order they opened, and the one that last held the focus
	sessions []*app
	current  *app
	// Workspaces and session cards opened so far, numbering the next of each
	spaces, made int
	// Sessions that took a message in the update under way, settled when it ends, and whether something every card shows changed with it:
	// a clock second, the live sessions, the battery, the theme
	touched []*app
	stale   bool
	// When a locally started background service last died, the restarts counted against the budget
	recoveryAttempts []float64
	// Whether the one second repaint runs that keeps the status clocks and agent docks of the session cards counting
	clockOn bool
	// Session the active-session file names
	activeFile string
	// Questions of sessions shown in no card, by request id the session that asked; they stay open on the background service until answered
	waiting map[string]string
	// Sessions the background service runs, as the last poll listed them, the chain of poll ticks in force and whether a turn that just
	// started asks for a poll at once
	live      []listedSession
	liveChain int
	liveKick  bool
	// Session that holds the one microphone, the last to start recording
	voiceOwner *app
	// Widgets of the window, the ambient ones drawn in the session card in view
	widgets *widgetHost
	// Pet of the window, drawn in the session card in view and following that session
	pet petCard
}

// Whether the session's card is the one in view, which alone shows the ambient widgets and the pet
func (sh *shell) inView(s *app) bool { return sh.current == s }

// Hands the microphone to a session, the session that was recording with it told to stop
func (sh *shell) claimVoice(s *app) tea.Cmd {
	prev := sh.voiceOwner
	sh.voiceOwner = s
	if prev == nil || prev == s || !prev.voiceRecording || !slices.Contains(sh.sessions, prev) {
		return nil
	}
	prev.voiceRecording, prev.voiceProcessing = false, false
	prev.store.voicePartial = ""
	params := tuigateway.VoiceRecordParams{Action: "stop", SessionID: prev.store.ui.Sid}
	return sh.tag(prev, call(sh.gw, tuigateway.VoiceRecord, params, func(_ *tuigateway.VoiceRecordResult, err error) tea.Cmd {
		if err != nil {
			prev.sys(errorLine(rpcErrorMessage(err)))
		}
		return nil
	}))
}

// Whether the event is the microphone's: what it hears, what it made of it and whether it listens
func voiceEvent(ev gatewayEventMsg) bool {
	return ev.Type == "voice.partial" || ev.Type == "voice.transcript" || ev.Type == "voice.status"
}

// Tick of the poll of the sessions the background service runs, of the chain of ticks that was current when it was scheduled
type liveTickMsg struct{ chain int }

// Next poll of the live sessions, rare while no session of the window runs a turn
func (sh *shell) scheduleLive() tea.Cmd {
	every := idleRefreshEvery
	if slices.ContainsFunc(sh.sessions, func(s *app) bool { return s.store.ui.Busy }) {
		every = refreshEvery
	}
	chain := sh.liveChain
	return tea.Tick(every, func(time.Time) tea.Msg { return liveTickMsg{chain: chain} })
}

// A turn started in some session card: the live sessions are polled at the short interval from now
func (sh *shell) watchLive() {
	sh.liveChain++
	sh.liveKick = true
}

// Reads the sessions the background service runs, once for the whole window: every session card takes its title from the list, and the
// session in view hears of the turns that ended and the questions asked in sessions it left running
func (sh *shell) pollLive() tea.Cmd {
	cur := sh.active()
	if cur == nil || sh.ui.offline() || !slices.ContainsFunc(sh.sessions, func(s *app) bool { return s.store.ui.Sid != "" }) {
		return nil
	}
	return requestSessions(sh.gw, true, func(rows []listedSession, err error) tea.Cmd {
		if err != nil {
			return nil
		}
		sh.live, sh.stale = rows, true
		for _, row := range rows {
			// A title the list does not carry leaves the one the card has
			if s := sh.bySid(row.ID); s != nil && trimJS(row.Title) != "" {
				s.store.ui.SessionTitle = trimJS(row.Title)
			}
		}
		if cur = sh.active(); cur == nil {
			return nil
		}
		return sh.run(cur, func() tea.Cmd { return cur.noteLiveSessions(rows, cur.store.ui.Sid) })
	})
}

// How many sessions the background service runs, as the last poll counted them
func (sh *shell) liveCount() int { return len(sh.live) }

// Sessions the background service runs that no card of this window shows
func (sh *shell) liveElsewhere() int {
	n := 0
	for _, row := range sh.live {
		if row.ID != "" && sh.bySid(row.ID) == nil {
			n++
		}
	}
	return n
}

// One second passed while a status clock or an agent dock counts
type clockTickMsg struct{}

// Runs a step of a session: the cards it opens belong to the session, its commands come back to it and the session settles when the update ends
func (sh *shell) run(s *app, step func() tea.Cmd) tea.Cmd {
	sh.touch(s)
	return sh.tag(s, sh.wm.As(s, step))
}

// Notes that a session took a message in the update under way
func (sh *shell) touch(s *app) {
	if s != nil && !slices.Contains(sh.touched, s) && slices.Contains(sh.sessions, s) {
		sh.touched = append(sh.touched, s)
	}
}

// Rebuilds the toolkit palette from the theme and hands it to the window manager and every session card
func (sh *shell) themeChanged() {
	t := sh.ui.Theme
	light := false
	if lum, ok := relativeLuminance(t.Color.Text); ok {
		light = lum < 0.35
	}
	sh.toolkit = toolkitTheme(t, light)
	sh.themeLook = hashText(fmt.Sprint(t.Color, t.Brand))
	sh.toolkit.Name = "session-" + sh.themeLook
	if sh.wm == nil {
		return
	}
	sh.wm.SetTheme(sh.toolkit)
	sh.wm.SetMotion(sh.motion())
	sh.stale = true
	for _, s := range sh.sessions {
		s.card.composer.SetTheme(sh.toolkit)
		s.arena.retheme(sh.toolkit)
		s.card.view.invalidate()
	}
}

// Motion scale of the window manager, none under the plain theme
func (sh *shell) motion() float64 {
	if sh.toolkit.Plain {
		return 0
	}
	return bootMotion()
}

// Draws the pictures of every session card again after the screen was cleared
func (sh *shell) redrawImages() tea.Cmd {
	cmds := make([]tea.Cmd, len(sh.sessions))
	for i, s := range sh.sessions {
		cmds[i] = sh.tag(s, s.images.redraw())
	}
	return tea.Batch(cmds...)
}

// Hints about the host terminal still to show, handed out once
func (sh *shell) takeTerminalHints() []terminalHint {
	hints := sh.terminalHints
	sh.terminalHints = nil
	return hints
}

// The session in view changed or shows another session: the active-session file names it
func (sh *shell) focusChanged() {
	s := sh.active()
	if s == nil {
		return
	}
	sh.pet.host(s)
	sid := s.store.ui.StoredSid
	if sid == "" || sid == sh.activeFile {
		return
	}
	sh.activeFile = sid
	writeActiveSessionFile(sid)
}

// The connection to the background service ended: the subscriptions died with it, every session card resets its turn and waits for the
// reconnect, and a locally started service is restarted only while the budget of restarts lasts
func (sh *shell) gatewayExited(code int) {
	wasConnected := !sh.ui.offline()
	sh.ui.setConnected(false)
	sh.subs.lost()
	attached := sh.gw.attached()
	restart := true
	if !attached {
		sid := ""
		for _, s := range sh.sessions {
			if sid = s.recoveryTarget(); sid != "" {
				break
			}
		}
		plan := planGatewayRecovery(sid, "", sh.recoveryAttempts, float64(time.Now().UnixMilli()))
		sh.recoveryAttempts, restart = plan.Attempts, plan.Recover
	}
	for _, s := range sh.sessions {
		sh.touch(s)
		s.onGatewayExit(code, attached, wasConnected, restart)
	}
}

// A session shown in no card asked a question: it stays open on the background service, and the window says which session waits and how to
// reach it
func (sh *shell) waitsUnshown(sid string, r *serverRequest) tea.Cmd {
	if sh.waiting == nil {
		sh.waiting = map[string]string{}
	}
	_, known := sh.waiting[requestKey(r.ID)]
	sh.waiting[requestKey(r.ID)] = sid
	if known || r.Replayed {
		return nil
	}
	return sh.wm.Toast(tr(txt.SessionLifecycleQuestionElsewhere, sessionLabel(sid), sh.keys.Display("leader")+" "+sh.keys.Display("leader.window.attention")), wm.Warn, wm.DefaultToastTTL)
}

// Event of a session shown in no card: a question the background service withdrew stops waiting, anything else is that session's own
func (sh *shell) unshown(ev gatewayEventMsg) tea.Cmd {
	if ev.Type != tuigateway.RequestCancelEvent.Name() {
		return nil
	}
	if cancel, err := tuigateway.RequestCancelEvent.Payload(ev.Payload); err == nil {
		delete(sh.waiting, cancel.ID)
	}
	return nil
}

// Session shown in no card that waits on an answer, empty when none does
func (sh *shell) waitingUnshown() string {
	for id, sid := range sh.waiting {
		if sh.bySid(sid) != nil {
			delete(sh.waiting, id)
			continue
		}
		return sid
	}
	return ""
}

// Goes to what waits on the person: the next session card with a question open, else a session shown in no card that asked one, opened
// beside the cards in view
func (sh *shell) attention() tea.Cmd {
	cur := sh.active()
	at := slices.Index(sh.sessions, cur)
	for i := 1; i <= len(sh.sessions); i++ {
		s := sh.sessions[(at+i)%len(sh.sessions)]
		if s != cur && s.dialogs.waiting() {
			sh.current = s
			return sh.wm.Focus(s.card)
		}
	}
	if sid := sh.waitingUnshown(); sid != "" {
		return sh.openSession(sid, false)
	}
	// No session waits: a card of the window may, a terminal whose program rang or a sheet open in another card
	return sh.wm.FocusAttention()
}

// Repaints once a second while a status clock or an agent dock of any session card counts, the tick stopping with them
func (sh *shell) armClock() tea.Cmd {
	if sh.clockOn || !slices.ContainsFunc(sh.sessions, (*app).clockLive) {
		return nil
	}
	sh.clockOn = true
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return clockTickMsg{} })
}

// Session whose card holds the focus, else the one that held it last, else the first one open
func (sh *shell) active() *app {
	if f, ok := sh.wm.Focused().(*sessionCard); ok && slices.Contains(sh.sessions, f.app) {
		sh.current = f.app
	}
	if sh.current == nil && len(sh.sessions) > 0 {
		sh.current = sh.sessions[0]
	}
	return sh.current
}

// Session bound to the session id, nil when no card shows it
func (sh *shell) bySid(sid string) *app {
	if sid == "" {
		return nil
	}
	for _, s := range sh.sessions {
		if s.store.ui.Sid == sid || s.store.ui.StoredSid == sid {
			return s
		}
	}
	return nil
}

// Id of the next workspace the shell opens
func (sh *shell) nextWorkspaceID() string {
	sh.spaces++
	return "space-" + strconv.Itoa(sh.spaces)
}

// How much a lane signal asks for attention, a workspace chip showing its most urgent session
func signalWeight(s wm.Signal) int {
	switch s {
	case wm.Blocked:
		return 4
	case wm.Failed:
		return 3
	case wm.Working:
		return 2
	case wm.Fresh:
		return 1
	}
	return 0
}

// Bar with a chip per workspace, the session in view or else the most urgent one, and the dials of the session in view
func (sh *shell) barStatus() wm.Status {
	cur := sh.active()
	if cur == nil {
		return wm.Status{Lanes: map[*wm.Workspace]wm.Lane{}}
	}
	status := cur.barStatus()
	for _, s := range sh.sessions {
		ws := sh.wm.WorkspaceOf(s.card)
		if ws == nil || (s != cur && ws == sh.wm.WorkspaceOf(cur.card)) {
			continue
		}
		lane := s.lane()
		if prev, has := status.Lanes[ws]; has && signalWeight(prev.Signal) >= signalWeight(lane.Signal) {
			continue
		}
		status.Lanes[ws] = lane
	}
	if text, signal, waiting := sh.waitingElsewhere(cur); waiting && status.Notice.Text == "" {
		status.Notice = wm.Notice{Text: text, Signal: signal}
	}
	if sid := sh.waitingUnshown(); sid != "" && status.Notice.Text == "" {
		status.Notice = wm.Notice{Text: tr(txt.SessionLifecycleQuestionElsewhere, sessionLabel(sid), sh.keys.Display("leader")+" "+sh.keys.Display("leader.window.attention")), Signal: wm.Blocked}
	}
	return status
}

// Question waiting in a session other than the one in view, named for the bar notice
func (sh *shell) waitingElsewhere(cur *app) (string, wm.Signal, bool) {
	for _, s := range sh.sessions {
		if s == cur {
			continue
		}
		if text, signal, waiting := s.dialogs.notice(); waiting {
			return s.card.Title() + ": " + text, signal, true
		}
	}
	return "", wm.Idle, false
}

// Commands of the palette and the global keys: the space commands, then those of the session in view tagged with it
func (sh *shell) commands() []wm.Command {
	out := sh.spaceCommands()
	s := sh.active()
	if s == nil {
		return out
	}
	for _, c := range s.commands() {
		if run := c.Run; run != nil {
			c.Run = func() tea.Cmd { return sh.run(s, run) }
		}
		out = append(out, c)
	}
	return out
}

// Commands that open and close session cards and workspaces
func (sh *shell) spaceCommands() []wm.Command {
	return []wm.Command{
		{ID: "workspace.new", Title: "new space with a new session", Run: sh.openSpace},
		{ID: "session.beside", Title: "new session beside this one", Run: sh.openBeside},
		{ID: "workspace.close", Title: "close this space", When: sh.spaceClosable, Run: sh.closeSpace},
		{ID: "window.attention", Title: "go to what waits on you", Run: sh.attention},
	}
}

// Puts the card of a spawned session beside the cards in view or in a new workspace, focused
func (sh *shell) place(s *app, space bool) tea.Cmd {
	sh.current = s
	return sh.wm.As(s, func() tea.Cmd {
		if ws := sh.wm.Active(); ws != nil && !space {
			return sh.wm.Open(ws, s.card)
		}
		_, open := sh.wm.AddWorkspace(sh.nextWorkspaceID(), s.card)
		return tea.Batch(open, sh.wm.Focus(s.card))
	})
}

// Opens a workspace holding a new session card and shows it
func (sh *shell) openSpace() tea.Cmd { return sh.openNew(true) }

// Opens a new session card tiled beside the cards of the workspace in view
func (sh *shell) openBeside() tea.Cmd { return sh.openNew(false) }

// Opens a card with a fresh session, beside the cards in view or in a new workspace; the session starts with the logins of the session
// it was opened from
func (sh *shell) openNew(space bool) tea.Cmd {
	from := sh.active()
	s := sh.newSession()
	if from != nil {
		s.life.loginsFrom = from.store.ui.Sid
	}
	started := func(string) tea.Cmd { return sh.unopened(s, from) }
	return tea.Batch(sh.place(s, space), sh.run(s, func() tea.Cmd { return tea.Batch(s.start(), s.life.newSession("", "", started)) }))
}

// Closes a card whose session the background service refused to start or attach, the card it was opened from saying why; a card that got
// its session, or waits for a model to be set up, stays
func (sh *shell) unopened(s, from *app) tea.Cmd {
	if s.life.failed == nil || s.store.ui.Sid != "" || !slices.Contains(sh.sessions, s) || len(sh.sessions) < 2 {
		return nil
	}
	why := errorLine(rpcErrorMessage(s.life.failed))
	var cmds []tea.Cmd
	if ws := sh.wm.WorkspaceOf(s.card); ws != nil && len(sh.wm.Workspaces()) > 1 && !slices.ContainsFunc(sh.sessions, func(o *app) bool { return o != s && sh.wm.WorkspaceOf(o.card) == ws }) {
		cmds = append(cmds, sh.wm.RemoveWorkspace(sh.wm.IndexOf(ws)))
	} else {
		cmds = append(cmds, sh.wm.Close(s.card))
	}
	if from == nil || !slices.Contains(sh.sessions, from) {
		from = sh.sessions[0]
	}
	from.sys(why)
	sh.touch(from)
	sh.current = from
	return tea.Batch(append(cmds, sh.wm.Focus(from.card))...)
}

// Whether the workspace in view may close: another one stays and a session card stays open outside it
func (sh *shell) spaceClosable() bool {
	ws := sh.wm.Active()
	return len(sh.wm.Workspaces()) > 1 && slices.ContainsFunc(sh.sessions, func(s *app) bool { return sh.wm.WorkspaceOf(s.card) != ws })
}

// Closes the workspace in view with its cards, the sessions in it running on in the background service
func (sh *shell) closeSpace() tea.Cmd {
	if !sh.spaceClosable() {
		return nil
	}
	return sh.wm.RemoveWorkspace(sh.wm.IndexOf(sh.wm.Active()))
}

// Forgets the session of a closed card: its dialog and pictures go and its attachment is let go, its turn running on in the background service
func (sh *shell) dropSession(s *app) tea.Cmd {
	i := slices.Index(sh.sessions, s)
	if i < 0 {
		return nil
	}
	sh.sessions = slices.Delete(sh.sessions, i, i+1)
	if sh.current == s {
		sh.current = nil
	}
	if sh.voiceOwner == s {
		sh.voiceOwner = nil
		speech.recording.Store(false)
	}
	// The questions it asked stay open on the background service, their sheets going with the card
	var cmds []tea.Cmd
	for _, d := range s.dialogs.open {
		d.settled = true
		cmds = append(cmds, sh.wm.DismissCard(d))
	}
	s.dialogs = dialogState{}
	sh.focusChanged()
	return tea.Batch(append(cmds, s.images.release(), sh.tag(s, s.life.closeForExit()))...)
}

// Starts the window manager and the sessions open at launch
func (sh *shell) Init() tea.Cmd {
	cmds := []tea.Cmd{sh.wm.Init(), terminalHintsCmd, sh.scheduleLive(), sh.widgets.warm(), sh.pet.start()}
	for _, s := range sh.sessions {
		cmds = append(cmds, sh.run(s, s.start))
	}
	return tea.Batch(append(cmds, sh.settle())...)
}

// Frame of the window manager with the colors, mouse mode and tab title of the session in view
func (sh *shell) View() tea.View {
	if s := sh.active(); s != nil {
		return s.view()
	}
	return sh.wm.View()
}

// Event types every session takes, the connection's own and those about the whole profile
func broadcastEvent(ev gatewayEventMsg) bool {
	return ev.SessionID == "" && (strings.HasPrefix(ev.Type, "gateway.") || ev.Type == "skin.changed" || ev.Type == tuigateway.SessionsChangedEvent.Name())
}

// Session showing the session id, else the session in view
func (sh *shell) owner(sid string) *app {
	if s := sh.bySid(sid); s != nil {
		return s
	}
	return sh.active()
}

// Routes a message to the window manager or to the session it belongs to, then the sessions it reached settle
func (sh *shell) Update(m tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	deliver := func(s *app, msg tea.Msg) {
		if s != nil && slices.Contains(sh.sessions, s) {
			cmds = append(cmds, sh.run(s, func() tea.Cmd { return s.update(msg) }))
		}
	}
	everyone := func(msg tea.Msg) {
		for _, s := range slices.Clone(sh.sessions) {
			deliver(s, msg)
		}
	}
	window := func() {
		_, cmd := sh.wm.Update(m)
		cmds = append(cmds, cmd)
	}
	switch v := m.(type) {
	case sessionMsg:
		deliver(v.s, v.m)
	case exitMsg:
		sh.exitCode = v.code
		sh.saveLayout()
		if s := sh.active(); s != nil {
			s.closeTerminals()
		}
		return sh, tea.Quit
	case clockTickMsg:
		sh.clockOn, sh.stale = false, true
	case liveTickMsg:
		if v.chain == sh.liveChain {
			cmds = append(cmds, sh.pollLive(), sh.scheduleLive())
		}
	case petPollMsg, petTickMsg, petFlashMsg:
		// The one pet keeps its own clocks, whichever session card hosts it
		_, cmd := sh.pet.update(v)
		cmds = append(cmds, cmd)
		sh.touch(sh.current)
	case batteryPollMsg:
		sh.stale = true
		cmds = append(cmds, sh.battery.onTick(v, sh.ui, sh.gw))
	case uv.KittyGraphicsEvent, uv.CellSizeEvent, uv.PixelSizeEvent, uv.PrimaryDeviceAttributesEvent, uv.UnknownCsiEvent:
		if s := sh.active(); s != nil {
			s.terminalGraphics(v)
		}
	case tea.FocusMsg, tea.BlurMsg:
		for _, s := range sh.sessions {
			s.arena.focus(v)
		}
		window()
	case tea.WindowSizeMsg:
		if s := sh.active(); s != nil {
			s.terminalGraphics(v)
		}
		window()
		if sh.graphicsProbed {
			cmds = append(cmds, sh.graphics.Measure())
		}
		cmds = append(cmds, sh.redrawImages())
	case tea.ResumeMsg:
		window()
		cmds = append(cmds, sh.redrawImages())
		if _, reflow := sh.resize.schedule(); reflow != nil {
			cmds = append(cmds, reflow)
		}
	case resizeReflowMsg:
		if sh.resize.fire(v) {
			for _, s := range sh.sessions {
				s.card.view.invalidate()
			}
			sh.stale = true
		}
	case gatewayEventMsg:
		switch {
		case broadcastEvent(v):
			everyone(v)
		case v.SessionID == "" && voiceEvent(v) && slices.Contains(sh.sessions, sh.voiceOwner):
			// The microphone speaks to the session that holds it, not to whichever one is in view
			deliver(sh.voiceOwner, v)
		case v.SessionID == "" || sh.bySid(v.SessionID) != nil:
			deliver(sh.owner(v.SessionID), v)
		default:
			cmds = append(cmds, sh.unshown(v))
		}
	case gatewayExitMsg:
		sh.gatewayExited(v.Code)
	case tea.BackgroundColorMsg, tea.ForegroundColorMsg:
		// The terminal's colors set the one theme every session card shares
		deliver(sh.active(), v)
	case serverRequestMsg:
		sid, _ := v.Request.Params["session_id"].(string)
		if s := sh.bySid(sid); s != nil || sid == "" || !sessionPromptMethods[v.Request.Method] {
			deliver(sh.owner(sid), v)
		} else {
			cmds = append(cmds, sh.waitsUnshown(sid, v.Request))
		}
	case cards.SubmitMsg:
		deliver(sh.owner(v.Session), v)
	case tea.ClipboardMsg, tty.SuspendRefusedMsg, tty.TerminateMsg, tty.GoneMsg:
		deliver(sh.active(), v)
	case sessionScoped:
		// One that no session tagged belongs to the session in view
		deliver(sh.active(), v)
	default:
		window()
	}
	cmds = append(cmds, sh.settle())
	if feedsArena(m) {
		for _, s := range sh.sessions {
			s.arena.sync()
		}
	}
	return sh, tea.Batch(cmds...)
}

// Settles the sessions the update reached, then follows what the window shows: the battery reading and the one second clock
func (sh *shell) settle() tea.Cmd {
	var cmds []tea.Cmd
	// Settling may reach further sessions, so the list is read as it grows
	for i := 0; i < len(sh.touched); i++ {
		s := sh.touched[i]
		if slices.Contains(sh.sessions, s) {
			cmds = append(cmds, sh.tag(s, sh.wm.As(s, s.settle)))
		}
	}
	// Cards are drawn again only when a session took a message or the window's own state moved; a bare animation frame redraws nothing
	if len(sh.touched) > 0 || sh.stale {
		sh.wm.Repaint()
		// A card that started to move by itself, a turn that began, needs the frame clock a resting window stopped
		if slices.ContainsFunc(sh.touched, func(s *app) bool { return s.card.Awake() }) {
			cmds = append(cmds, sh.wm.Wake())
		}
	}
	sh.touched, sh.stale = sh.touched[:0], false
	if sh.liveKick {
		sh.liveKick = false
		cmds = append(cmds, sh.pollLive(), sh.scheduleLive())
	}
	cmds = append(cmds, sh.battery.sync(sh.ui, sh.gw), sh.armClock())
	return tea.Batch(cmds...)
}

// File of the profile home that keeps the windows between runs
func layoutPath(ctx context.Context) string { return filepath.Join(home.Dir(ctx), "tui-layout.json") }

// Saved state of a session card, the session it shows
type sessionCardState struct {
	Sid string `json:"sid,omitempty"`
}

// Keeps the workspaces, their cards and the session each card shows for the next run, a failure noted in the lifecycle log
func (sh *shell) saveLayout() {
	data, err := json.MarshalIndent(sh.wm.Layout(), "", "  ")
	if err == nil {
		path := layoutPath(sh.ctx)
		if err = os.WriteFile(path+".tmp", data, 0o600); err == nil {
			err = os.Rename(path+".tmp", path)
		}
	}
	if err != nil {
		recordParentLifecycle("layout not saved: " + err.Error())
	}
}

// Reopens the last run's workspaces and session cards, each card resuming its session; cards with no session to resume are left out
func (sh *shell) restoreLayout(first *app) tea.Cmd {
	raw, err := os.ReadFile(layoutPath(sh.ctx))
	if err != nil {
		return nil
	}
	var l wm.Layout
	if json.Unmarshal(raw, &l) != nil || len(l.Workspaces) == 0 {
		return nil
	}
	sh.wm.Restore(l)
	firstTaken := false
	var cmds []tea.Cmd
	for i, saved := range l.Workspaces {
		cards := map[string]wm.Card{}
		// Rekeyed asks again for every place a key appears, each saved card taking one session
		renamed := map[string]string{}
		rename := func(key string) string {
			if to, done := renamed[key]; done {
				return to
			}
			var st sessionCardState
			if !strings.HasPrefix(key, "session:") || json.Unmarshal(saved.Cards[key], &st) != nil || st.Sid == "" {
				renamed[key] = ""
				return ""
			}
			card := first.card
			if firstTaken || i > 0 {
				card = sh.newSession().card
			}
			firstTaken = true
			cards[card.Key()], renamed[key] = card, card.Key()
			return card.Key()
		}
		ws := sh.wm.Active()
		if i > 0 {
			var open tea.Cmd
			ws, open = sh.wm.AddWorkspace(sh.nextWorkspaceID())
			cmds = append(cmds, open)
		}
		cmds = append(cmds, sh.wm.RestoreWorkspace(ws, saved.Rekeyed(ws.ID, rename), func(key string) wm.Card { return cards[key] }))
	}
	if l.Active > 0 && l.Active < len(sh.wm.Workspaces()) {
		cmds = append(cmds, sh.wm.Switch(l.Active))
	}
	return tea.Batch(cmds...)
}

// Whether this is the first open session, which alone starts what the window runs once, like the wake word listener
func (a *app) primary() bool { return slices.Index(a.sessions, a) == 0 }

// Opens a stored or live session in a new card, beside the cards in view or in a new space, or focuses the card that already holds it
func (sh *shell) openSession(sid string, space bool) tea.Cmd {
	if s := sh.bySid(sid); s != nil {
		sh.current = s
		return sh.wm.Focus(s.card)
	}
	from := sh.active()
	s := sh.newSession()
	opened := func(error) tea.Cmd { return sh.unopened(s, from) }
	return tea.Batch(sh.place(s, space), sh.run(s, func() tea.Cmd { return tea.Batch(s.start(), s.resumeByID(sid, opened)) }))
}
