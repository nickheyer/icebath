package tui

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"
)

var resumeScrollDelays = []time.Duration{0, 80 * time.Millisecond, 240 * time.Millisecond}

// Gateway surface of session creation, resume and attachment, the gateway client in production
type sessionGateway interface {
	turnGateway
	holdAttach(sessionID string) *attachHold
	settleAttach(hold *attachHold, snapshot map[string]any)
	hydrateSharedPrompts(result map[string]any)
	creationEnv() func(string) string
}

// Launch environment the session.create options are read from
func (c *gatewayClient) creationEnv() func(string) string { return c.env }

var _ sessionGateway = (*gatewayClient)(nil)

// Transcript scroll box a resume pins to the bottom
type scrollHandle interface {
	lastManualScrollAt() float64
	isSticky() bool
	scrollToBottom()
}

// Transcript, composer and viewport surface the session lifecycle drives, the main model in production
type sessionView interface {
	sys(text string)
	panel(title string, sections []panelSection)
	setHistoryItems(update func([]msg) []msg)
	setLastUserMsg(text string)
	setSessionStartedAt(ms float64)
	setStickyPrompt(text string)
	setVoiceRecording(on bool)
	setVoiceProcessing(on bool)
	clearComposerTokens()
	cols() int
	scroll() scrollHandle
	onFreshSessionStarted(sid string)
	uncaught(err error)
}

// Scroll to bottom step of a resume, stale once a newer resume or reset cancelled it
type resumeScrollMsg struct {
	gen, index int
	startedAt  float64
}

// Detach in flight for a session and the steps waiting for it to finish
type detachFlight struct{ waiters []func() tea.Cmd }

// Session create, resume, activation, detach and reset over the shared store
type sessionLifecycle struct {
	store *tuiStore
	turn  *turnController
	gw    sessionGateway
	view  sessionView

	attachmentFlight   int
	pendingAttachments map[int]bool
	subscriptions      map[string]string
	detachFlights      map[string]*detachFlight
	staleAttachments   map[string][2]string
	resumeScrollGen    int
}

// Lifecycle of one app
func newSessionLifecycle(store *tuiStore, turn *turnController, gw sessionGateway, view sessionView) *sessionLifecycle {
	return &sessionLifecycle{store: store, turn: turn, gw: gw, view: view, pendingAttachments: map[int]bool{}, subscriptions: map[string]string{}, detachFlights: map[string]*detachFlight{}, staleAttachments: map[string][2]string{}}
}

// Gateway request that prints its own failure and hands nil to then
func (l *sessionLifecycle) rpc(method string, params map[string]any, then func(map[string]any) tea.Cmd) tea.Cmd {
	return gatewayCall(l.gw, method, params, func(v any, err error) tea.Cmd {
		var result map[string]any
		switch {
		case err != nil:
			l.view.sys("error: " + rpcErrorMessage(err))
		case asRPCResult(v) == nil:
			l.view.sys("error: " + tr("session.common.invalidResponse", method))
		default:
			result = asRPCResult(v)
		}
		if then == nil {
			return nil
		}
		return then(result)
	})
}

// Continuation run on a later update, the way an already resolved promise continues
func resolved(then func() tea.Cmd) tea.Cmd {
	return func() tea.Msg { return asyncDoneMsg{run: then} }
}

// Usage of a session's info over the zero counters
func usageFrom(info map[string]any) map[string]any {
	usage, _ := info["usage"].(map[string]any)
	if !truthyJS(info["usage"]) {
		return zeroUsage()
	}
	return spreadUsage(zeroUsage(), usage)
}

// Status line of a live session's state
func statusFromLiveSession(status string, running bool) string {
	switch {
	case status == "waiting":
		return tr("session.status.waitingForInput")
	case status == "starting":
		return tr("session.status.startingAgent")
	case running || status == "working":
		return "running…"
	}
	return "ready"
}

// Credential and config warnings of a new session, then its opening message
func reportNewSessionNotices(info map[string]any, message string, sys func(string)) {
	if truthyJS(info["credential_warning"]) {
		sys("warning: " + describeCredentialWarning(jsValueString(info["credential_warning"])))
	}
	if truthyJS(info["config_warning"]) {
		sys("warning: " + jsValueString(info["config_warning"]))
	}
	if message != "" {
		sys(message)
	}
}

// Writes the active session id where ICEBATH_TUI_ACTIVE_SESSION_FILE points, failures ignored
func writeActiveSessionFile(sessionID string) {
	file := os.Getenv("ICEBATH_TUI_ACTIVE_SESSION_FILE")
	if file == "" || sessionID == "" {
		return
	}
	raw, err := json.Marshal(map[string]any{"session_id": sessionID})
	if err == nil {
		_ = os.WriteFile(file, raw, 0o600)
	}
}

// User bubble of a live session's turn in flight
func liveSessionInflightMessages(inflight map[string]any) []msg {
	user := strings.TrimFunc(stringOr(inflight["user"], ""), isJSSpace)
	if user == "" {
		return []msg{}
	}
	row := map[string]any{"role": "user", "text": user}
	if truthyJS(inflight["display_kind"]) {
		row["display_kind"] = inflight["display_kind"]
	}
	if truthyJS(inflight["display_metadata"]) {
		row["display_metadata"] = inflight["display_metadata"]
	}
	return toTranscriptMessages([]any{row})
}

// Streams the partial reply of a live session's turn in flight
func (l *sessionLifecycle) hydrateLiveSessionInflight(inflight map[string]any) {
	assistant := stringOr(inflight["assistant"], "")
	if assistant == "" && !truthyJS(inflight["streaming"]) {
		return
	}
	l.turn.hydrateStreamingText(assistant)
}

// Tells the app a different session replaced the previous one, true when it did
func signalFreshSessionBoundary(previousSid, nextSid string, onFresh func(string)) bool {
	if previousSid == "" || nextSid == "" || previousSid == nextSid {
		return false
	}
	onFresh(nextSid)
	return true
}

// History without its last exchange, the reply rows and then the user row
func trimLastExchange(items []msg) []msg {
	q := append([]msg(nil), items...)
	for len(q) > 0 && (q[len(q)-1].Role == "assistant" || q[len(q)-1].Role == "tool") {
		q = q[:len(q)-1]
	}
	if len(q) > 0 && q[len(q)-1].Role == "user" {
		q = q[:len(q)-1]
	}
	return q
}

// Closes a session on the legacy wire, printing its closing messages unless deferred
func (l *sessionLifecycle) closeSession(targetSid string, deferMessages bool, then func(map[string]any) tea.Cmd) tea.Cmd {
	finish := func(closed map[string]any) tea.Cmd {
		if !deferMessages {
			l.printClosed(closed)
		}
		if then == nil {
			return nil
		}
		return then(closed)
	}
	if targetSid == "" || l.gw.canonical() {
		return resolved(func() tea.Cmd { return finish(nil) })
	}
	return l.rpc("session.close", map[string]any{"session_id": targetSid}, finish)
}

// Prints the messages a session.close answered with
func (l *sessionLifecycle) printClosed(closed map[string]any) {
	messages, _ := closed["messages"].([]any)
	for _, m := range messages {
		l.view.sys(jsValueString(m))
	}
}

// Detaches a canonical subscription after any detach of the same session finished
func (l *sessionLifecycle) detachCanonical(sid, subscriptionID, endReason string, onlyIfHeld bool, then func(map[string]any) tea.Cmd) tea.Cmd {
	done := func(result map[string]any) tea.Cmd {
		if then == nil {
			return nil
		}
		return then(result)
	}
	if !l.gw.canonical() || sid == "" || subscriptionID == "" {
		return resolved(func() tea.Cmd { return done(nil) })
	}
	flight := &detachFlight{}
	previous := l.detachFlights[sid]
	l.detachFlights[sid] = flight
	finish := func(result map[string]any) tea.Cmd {
		cmds := []tea.Cmd{done(result)}
		if l.detachFlights[sid] == flight {
			delete(l.detachFlights, sid)
		}
		for _, w := range flight.waiters {
			cmds = append(cmds, w())
		}
		return tea.Batch(cmds...)
	}
	run := func() tea.Cmd {
		if onlyIfHeld && l.subscriptions[sid] != subscriptionID {
			return finish(nil)
		}
		params := map[string]any{"session_id": sid, "subscription_id": subscriptionID}
		if endReason != "" {
			params["end_reason"] = endReason
		}
		return l.rpc("session.detach", params, func(result map[string]any) tea.Cmd {
			if truthyJS(result["detached"]) && l.subscriptions[sid] == subscriptionID {
				delete(l.subscriptions, sid)
			}
			return finish(result)
		})
	}
	if previous != nil {
		previous.waiters = append(previous.waiters, run)
		return nil
	}
	return run()
}

// Runs fn once the session's detach in flight finished, at once when none is
func (l *sessionLifecycle) afterDetach(sid string, fn func() tea.Cmd) tea.Cmd {
	if flight := l.detachFlights[sid]; flight != nil {
		flight.waiters = append(flight.waiters, fn)
		return nil
	}
	return fn()
}

// Detaches every held subscription as the TUI shuts down
func (l *sessionLifecycle) closeForExit() tea.Cmd {
	cmds := []tea.Cmd{}
	for sid, sub := range l.subscriptions {
		cmds = append(cmds, l.detachCanonical(sid, sub, "tui_shutdown", false, nil))
	}
	return tea.Batch(cmds...)
}

// Closes the live view of a session, a legacy close or a canonical detach it holds
func (l *sessionLifecycle) closeLiveView(targetSid string, then func(closed bool) tea.Cmd) tea.Cmd {
	if !l.gw.canonical() {
		return l.rpc("session.close", map[string]any{"session_id": targetSid}, func(closed map[string]any) tea.Cmd {
			l.printClosed(closed)
			return then(truthyJS(closed["closed"]))
		})
	}
	return l.afterDetach(targetSid, func() tea.Cmd {
		sub := l.subscriptions[targetSid]
		if sub == "" {
			return then(false)
		}
		return l.detachCanonical(targetSid, sub, "tui_close", true, func(result map[string]any) tea.Cmd {
			return then(truthyJS(result["detached"]))
		})
	})
}

// Detaches attachments a superseded flight adopted once no attachment is in flight
func (l *sessionLifecycle) disposeStaleAttachments() tea.Cmd {
	if len(l.pendingAttachments) > 0 {
		return nil
	}
	stale := l.staleAttachments
	l.staleAttachments = map[string][2]string{}
	cmds := []tea.Cmd{}
	for _, s := range stale {
		if l.subscriptions[s[0]] != s[1] {
			cmds = append(cmds, l.detachCanonical(s[0], s[1], "", false, nil))
		}
	}
	return tea.Batch(cmds...)
}

func (l *sessionLifecycle) finishAttachment(flight int) tea.Cmd {
	delete(l.pendingAttachments, flight)
	return l.disposeStaleAttachments()
}

// Remembers the subscription of an answer that arrived for a superseded flight
func (l *sessionLifecycle) discardStaleAttachment(result map[string]any) tea.Cmd {
	if sub := paramStr(result["subscription_id"]); truthyJS(result["subscription_id"]) {
		sid := jsValueString(result["session_id"])
		key, _ := json.Marshal([]string{sid, sub})
		l.staleAttachments[string(key)] = [2]string{sid, sub}
	}
	return l.disposeStaleAttachments()
}

// Holds the subscription of an attached session and detaches the one it replaced
func (l *sessionLifecycle) adoptAttachment(result map[string]any, previousSid, previousSubscription string, closePrevious bool) tea.Cmd {
	sub := paramStr(result["subscription_id"])
	if !l.gw.canonical() || sub == "" {
		return nil
	}
	sid := jsValueString(result["session_id"])
	l.subscriptions[sid] = sub
	if previousSid == "" || previousSubscription == "" || (previousSid == sid && previousSubscription == sub) {
		return nil
	}
	reason := ""
	if closePrevious && previousSid != sid {
		reason = "tui_close"
	}
	return l.detachCanonical(previousSid, previousSubscription, reason, false, nil)
}

// Clears the session, its turn, voice state, transcript and composer tokens
func (l *sessionLifecycle) resetSession() {
	l.resumeScrollGen++
	l.turn.fullReset()
	l.view.setVoiceRecording(false)
	l.view.setVoiceProcessing(false)
	l.store.ui.BgTasks, l.store.ui.Info, l.store.ui.Sid, l.store.ui.StoredSid, l.store.ui.Usage = map[string]bool{}, nil, "", "", zeroUsage()
	l.view.setHistoryItems(func([]msg) []msg { return []msg{} })
	l.view.setLastUserMsg("")
	l.view.setStickyPrompt("")
	l.view.clearComposerTokens()
}

// Clears the visible transcript and turn, showing the session intro when info is given
func (l *sessionLifecycle) resetVisibleHistory(info map[string]any) {
	l.turn.idle(false)
	l.turn.clearReasoning()
	l.turn.turnTools = nil
	clear(l.turn.persistedToolLabels)
	l.view.setHistoryItems(func([]msg) []msg {
		if info != nil {
			return []msg{introMsg(info)}
		}
		return []msg{}
	})
	l.view.setStickyPrompt("")
	l.view.setLastUserMsg("")
	l.view.clearComposerTokens()
	l.store.turn.Activity = []activityItem{}
	l.store.ui.Info, l.store.ui.Usage = info, usageFrom(info)
}

// Pins the transcript to the bottom after a resume unless the user scrolled meanwhile
func (l *sessionLifecycle) scheduleResumeScrollToBottom() tea.Cmd {
	l.resumeScrollGen++
	gen, startedAt := l.resumeScrollGen, l.store.nowMillis()
	cmds := make([]tea.Cmd, len(resumeScrollDelays))
	for i, d := range resumeScrollDelays {
		index := i
		cmds[i] = tea.Tick(d, func(time.Time) tea.Msg { return resumeScrollMsg{gen: gen, index: index, startedAt: startedAt} })
	}
	return tea.Batch(cmds...)
}

// One scroll step of a resume, the first also repainting the whole screen
func (l *sessionLifecycle) onResumeScroll(m resumeScrollMsg) tea.Cmd {
	scroll := l.view.scroll()
	if m.gen != l.resumeScrollGen || scroll == nil {
		return nil
	}
	if scroll.lastManualScrollAt() > m.startedAt || (m.index != 0 && !scroll.isSticky()) {
		return nil
	}
	scroll.scrollToBottom()
	if m.index == 0 {
		return tea.ClearScreen
	}
	return nil
}

// Session info of an attach answer with its stored id
func infoWithStoredSid(r map[string]any, storedSid string) map[string]any {
	raw, isObj := r["info"].(map[string]any)
	if !isObj || !truthyJS(r["info"]) {
		return nil
	}
	info := make(map[string]any, len(raw)+1)
	for k, v := range raw {
		info[k] = v
	}
	info["stored_session_id"] = storedSid
	return info
}

// First truthy value as text
func firstTruthy(values ...any) string {
	for _, v := range values {
		if truthyJS(v) {
			return jsValueString(v)
		}
	}
	return ""
}

// Starts a session, closing the current one unless kept, then names it and adopts its subscription
func (l *sessionLifecycle) startNewSession(message, title string, keepCurrent bool, then func(sid string) tea.Cmd) tea.Cmd {
	l.attachmentFlight++
	flight := l.attachmentFlight
	previousSid := l.store.ui.Sid
	previousSubscription := l.subscriptions[previousSid]
	l.pendingAttachments[flight] = true
	settle := func(sid string) tea.Cmd {
		cmds := []tea.Cmd{l.finishAttachment(flight)}
		if then != nil {
			cmds = append(cmds, then(sid))
		}
		return tea.Batch(cmds...)
	}
	create := func(closed map[string]any) tea.Cmd {
		if flight != l.attachmentFlight {
			return settle("")
		}
		params := map[string]any{"cols": float64(l.view.cols())}
		if l.gw.canonical() {
			params = map[string]any{"request_id": uuid.NewString()}
			for k, v := range localCreationOptions(l.gw.creationEnv()) {
				params[k] = v
			}
		}
		if cwd := strings.TrimFunc(os.Getenv("ICEBATH_TUI_CWD"), isJSSpace); cwd != "" {
			params["cwd"] = cwd
		}
		return l.rpc("session.create", params, func(r map[string]any) tea.Cmd {
			if flight != l.attachmentFlight {
				return tea.Batch(l.discardStaleAttachment(r), settle(""))
			}
			if r == nil {
				l.store.ui.Status = "ready"
				return settle("")
			}
			info, _ := r["info"].(map[string]any)
			sid := jsValueString(r["session_id"])
			storedSid := firstTruthy(r["stored_session_id"], info["stored_session_id"], r["session_id"])
			info = infoWithStoredSid(r, storedSid)
			requestedTitle := strings.TrimFunc(title, isJSSpace)
			l.resetSession()
			l.view.setSessionStartedAt(l.store.nowMillis())
			writeActiveSessionFile(storedSid)
			status := tr("session.status.startingAgent")
			if l.gw.canonical() || truthyJS(info["version"]) {
				status = "ready"
			}
			connected := true
			l.store.ui.GatewayConnected = &connected
			l.store.ui.Info, l.store.ui.Sid, l.store.ui.Status, l.store.ui.StoredSid, l.store.ui.Usage = info, sid, status, storedSid, usageFrom(info)
			if info != nil {
				l.view.setHistoryItems(func([]msg) []msg { return []msg{introMsg(info)} })
			}
			reportNewSessionNotices(info, message, l.view.sys)
			l.printClosed(closed)
			cmds := []tea.Cmd{}
			if requestedTitle != "" {
				cmds = append(cmds, l.nameNewSession(sid, requestedTitle))
			}
			signalFreshSessionBoundary(previousSid, sid, l.view.onFreshSessionStarted)
			cmds = append(cmds, l.adoptAttachment(r, previousSid, previousSubscription, !keepCurrent), settle(sid))
			return tea.Batch(cmds...)
		})
	}
	afterSetup := func(setup map[string]any) tea.Cmd {
		if flight != l.attachmentFlight {
			return settle("")
		}
		if setup["provider_configured"] == false {
			l.view.panel(setupRequiredTitle(), buildSetupRequiredSections())
			l.store.ui.Status = tr("session.status.setupRequired")
			return settle("")
		}
		if keepCurrent {
			return create(nil)
		}
		return l.closeSession(previousSid, true, create)
	}
	if l.gw.canonical() {
		return resolved(func() tea.Cmd { return afterSetup(nil) })
	}
	return l.rpc("setup.status", map[string]any{}, afterSetup)
}

// Names a freshly created session, reporting the title or the failure while it stays current
func (l *sessionLifecycle) nameNewSession(sid, requestedTitle string) tea.Cmd {
	apply := func(result map[string]any) tea.Cmd {
		if result == nil || l.store.ui.Sid != sid {
			return nil
		}
		next := strings.TrimFunc(stringOr(result["title"], requestedTitle), isJSSpace)
		suffix := ""
		if truthyJS(result["pending"]) {
			suffix = tr("session.lifecycle.titleQueuedSuffix")
		}
		l.store.ui.SessionTitle = next
		l.view.sys(tr("session.lifecycle.sessionTitleSet", next) + suffix)
		return nil
	}
	if !l.gw.canonical() {
		return l.rpc("session.title", map[string]any{"session_id": sid, "title": requestedTitle}, apply)
	}
	return l.store.mutateCanonical(l.gw, sid, "rename", map[string]any{"title": requestedTitle}, func() bool { return false }, func(m *mutationOutcome, err error) tea.Cmd {
		if err != nil {
			if l.store.ui.Sid == sid {
				l.view.sys("warning: " + tr("session.lifecycle.failedToSetTitle", err.Error()))
			}
			return nil
		}
		if m == nil || m.Result == nil {
			return nil
		}
		return apply(map[string]any{"title": strings.TrimFunc(stringOr(m.Result["title"], requestedTitle), isJSSpace)})
	})
}

// Starts a fresh session in place of the current one
func (l *sessionLifecycle) newSession(message, title string, then func(sid string) tea.Cmd) tea.Cmd {
	return l.startNewSession(message, title, false, then)
}

// Starts a fresh session beside the current live one, closing the switcher
func (l *sessionLifecycle) newLiveSession(message, title string, then func(sid string) tea.Cmd) tea.Cmd {
	if message == "" {
		message = tr("session.lifecycle.newLiveSessionStarted")
	}
	l.store.overlay.Sessions = false
	return l.startNewSession(message, title, true, then)
}

// Shows an attached session's transcript, state, prompts and connection card
func (l *sessionLifecycle) applyAttachment(r map[string]any, storedSid string, hold *attachHold) tea.Cmd {
	info := infoWithStoredSid(r, storedSid)
	running := truthyJS(r["running"]) || r["status"] == "working" || r["status"] == "waiting"
	l.resetSession()
	startedAt := l.store.nowMillis()
	if n, isNum := r["started_at"].(float64); isNum && n != 0 {
		startedAt = n * 1000
	}
	l.view.setSessionStartedAt(startedAt)
	inflight, _ := r["inflight"].(map[string]any)
	transcript := append(toTranscriptMessages(r["messages"]), liveSessionInflightMessages(inflight)...)
	l.view.setHistoryItems(func([]msg) []msg {
		if info != nil {
			return append([]msg{introMsg(info)}, transcript...)
		}
		return transcript
	})
	writeActiveSessionFile(storedSid)
	connected := true
	l.store.ui.Busy, l.store.ui.GatewayConnected = running, &connected
	l.store.ui.Info, l.store.ui.Sid, l.store.ui.Status, l.store.ui.StoredSid, l.store.ui.Usage = info, jsValueString(r["session_id"]), statusFromLiveSession(paramStr(r["status"]), running), storedSid, usageFrom(info)
	l.gw.hydrateSharedPrompts(r)
	l.hydrateLiveSessionInflight(inflight)
	l.gw.settleAttach(hold, r)
	return l.scheduleResumeScrollToBottom()
}

// Request that waits for the session's detach in flight, dropped when a newer flight started meanwhile
func (l *sessionLifecycle) requestAfterDetach(id string, flight int, method string, params map[string]any, then func(any, error) tea.Cmd) tea.Cmd {
	if l.detachFlights[id] == nil {
		return gatewayCall(l.gw, method, params, then)
	}
	return l.afterDetach(id, func() tea.Cmd {
		if flight != l.attachmentFlight {
			return resolved(func() tea.Cmd { return then(nil, nil) })
		}
		return gatewayCall(l.gw, method, params, then)
	})
}

// Switches the view to another live session the gateway already runs
func (l *sessionLifecycle) activateLiveSession(id string) tea.Cmd {
	l.attachmentFlight++
	flight := l.attachmentFlight
	l.pendingAttachments[flight] = true
	previousSid := l.store.ui.Sid
	previousSubscription := l.subscriptions[previousSid]
	l.store.overlay.Sessions = false
	l.store.ui.Status = tr("session.status.switchingSession")
	l.store.clearConnectionOperation()
	hold := l.gw.holdAttach(id)
	return l.requestAfterDetach(id, flight, "session.activate", map[string]any{"session_id": id}, func(raw any, err error) tea.Cmd {
		done := func(cmd tea.Cmd) tea.Cmd {
			l.gw.settleAttach(hold, nil)
			return tea.Batch(cmd, l.finishAttachment(flight))
		}
		if err != nil {
			if flight == l.attachmentFlight {
				l.view.sys("error: " + err.Error())
				l.store.ui.Status = "ready"
			}
			return done(nil)
		}
		r := asRPCResult(raw)
		if flight != l.attachmentFlight {
			return done(l.discardStaleAttachment(r))
		}
		if r == nil {
			l.view.sys("error: " + tr("session.common.invalidResponse", "session.activate"))
			l.store.ui.Status = "ready"
			return done(nil)
		}
		info, _ := r["info"].(map[string]any)
		storedSid := firstTruthy(r["session_key"], r["stored_session_id"], info["stored_session_id"], r["session_id"])
		cmd := l.applyAttachment(r, storedSid, hold)
		if pending, isObj := r["pending_connection"].(map[string]any); isObj && truthyJS(r["pending_connection"]) {
			l.store.applyConnectionRequest(pending)
		}
		return done(tea.Batch(cmd, l.adoptAttachment(r, previousSid, previousSubscription, false)))
	})
}

// Resumes a stored or live session by id, moving its journaled inputs to the resumed session
func (l *sessionLifecycle) resumeByID(id string, then func(err error) tea.Cmd) tea.Cmd {
	l.attachmentFlight++
	flight := l.attachmentFlight
	l.pendingAttachments[flight] = true
	current, err := l.store.captureDestination()
	if err != nil {
		delete(l.pendingAttachments, flight)
		return resolved(func() tea.Cmd {
			if then != nil {
				return then(err)
			}
			l.view.uncaught(err)
			return nil
		})
	}
	previousSid := current.Sid
	previousSubscription := l.subscriptions[previousSid]
	destination := current
	if current.Sid != id && current.StoredSid != id {
		destination.Sid, destination.StoredSid = id, id
	}
	l.store.overlay.Sessions = false
	l.store.ui.Status = tr("session.status.resuming")
	finish := func(cmds ...tea.Cmd) tea.Cmd {
		cmds = append(cmds, l.finishAttachment(flight))
		if then != nil {
			cmds = append(cmds, then(nil))
		}
		return tea.Batch(cmds...)
	}
	afterSetup := func(setup map[string]any) tea.Cmd {
		if flight != l.attachmentFlight {
			return finish()
		}
		if setup["provider_configured"] == false {
			l.view.panel(setupRequiredTitle(), buildSetupRequiredSections())
			l.store.ui.Status = tr("session.status.setupRequired")
			return finish()
		}
		hold := l.gw.holdAttach(id)
		return l.requestAfterDetach(id, flight, "session.resume", map[string]any{"cols": float64(l.view.cols()), "session_id": id}, func(raw any, err error) tea.Cmd {
			defer l.gw.settleAttach(hold, nil)
			if err != nil {
				if flight == l.attachmentFlight {
					l.view.sys("error: " + err.Error())
					l.store.ui.Status = "ready"
				}
				return finish()
			}
			r := asRPCResult(raw)
			if flight != l.attachmentFlight {
				return finish(l.discardStaleAttachment(r))
			}
			if r == nil {
				l.view.sys("error: " + tr("session.common.invalidResponse", "session.resume"))
				l.store.ui.Status = "ready"
				return finish()
			}
			info, _ := r["info"].(map[string]any)
			storedSid := firstTruthy(r["session_key"], info["stored_session_id"], r["stored_session_id"], r["resumed"], id)
			resumedSid, successorStoredSid := jsValueString(r["session_id"]), ""
			if infoWithStoredSid(r, storedSid) != nil {
				successorStoredSid = storedSid
			}
			if err := pendingJournal.migrate(destination, resumedSid, successorStoredSid); err != nil {
				if flight == l.attachmentFlight {
					l.view.sys("error: " + err.Error())
					l.store.ui.Status = "ready"
				}
				return finish()
			}
			cmd := l.applyAttachment(r, storedSid, hold)
			if pending, isObj := r["pending_connection"].(map[string]any); isObj && truthyJS(r["pending_connection"]) {
				l.store.applyConnectionRequest(pending)
			} else {
				l.store.clearConnectionOperation()
			}
			cmds := []tea.Cmd{cmd}
			if previousSid != "" && previousSid != resumedSid {
				cmds = append(cmds, l.closeSession(previousSid, false, nil))
			}
			return finish(append(cmds, l.adoptAttachment(r, previousSid, previousSubscription, true))...)
		})
	}
	if l.gw.canonical() {
		return resolved(func() tea.Cmd { return afterSetup(nil) })
	}
	return l.rpc("setup.status", map[string]any{}, afterSetup)
}

// Refuses a session switch while a turn runs, true when it refused
func (l *sessionLifecycle) guardBusySessionSwitch(what string) bool {
	if !l.store.ui.Busy {
		return false
	}
	if what == "" {
		what = tr("session.lifecycle.switchSessions")
	}
	l.view.sys(tr("session.lifecycle.interruptBeforeSwitch", what))
	return true
}
