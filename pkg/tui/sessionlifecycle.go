package tui

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var resumeScrollDelays = []time.Duration{0, 80 * time.Millisecond, 240 * time.Millisecond}

// Gateway surface of session creation, resume and attachment, the gateway client in production
type sessionGateway interface {
	turnGateway
	holdAttach(sessionID string) *attachHold
	settleAttach(hold *attachHold, snapshot *tuigateway.CanonicalSnapshot)
	hydrateSharedPrompts(snapshot *tuigateway.CanonicalSnapshot)
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
		result, isObject := v.(map[string]any)
		switch {
		case err != nil:
			l.view.sys("error: " + rpcErrorMessage(err))
		case !isObject:
			l.view.sys("error: " + misfitText(tr("session.common.invalidResponse", method)))
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
func usageFrom(info *tuigateway.SessionInfo) *tuigateway.Usage {
	return mergeUsageStable(zeroUsage(), infoOf(info).Usage)
}

// Warning that the gateway serves the session with secret redaction off, and how to turn it back on
func reportRedactionOff(info *tuigateway.SessionInfo, sys func(string)) {
	if redact := infoOf(info).RedactSecrets; redact != nil && !*redact {
		sys("warning: " + tr("session.notices.redactionOff"))
	}
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

// Notices the gateway computed with the session inventory: toolsets it cannot place, tools switched off, and the project skills of the folder
func reportInventoryNotices(info *tuigateway.SessionInfo, sys func(string)) {
	held := infoOf(info)
	if len(held.UnknownToolsets) > 0 {
		sys("warning: " + tr("session.notices.unknownToolsets", strings.Join(held.UnknownToolsets, ", ")))
	}
	for _, notice := range held.ToolAvailabilityWarnings {
		if notice = trimJS(notice); notice != "" {
			sys("warning: " + notice)
		}
	}
	if untrusted := held.ProjectSkillsUntrusted; untrusted != nil && untrusted.Count > 0 {
		sys("warning: " + tr("session.notices.projectSkillsUntrusted", untrusted.Count, untrusted.Root))
	} else if loaded := val(held.ProjectSkillsLoaded); loaded > 0 {
		sys(tr("session.notices.projectSkillsLoaded", loaded))
	}
}

// Update line of the session banner, empty while the install is current or the check has no answer
func updateNotice(info *tuigateway.SessionInfo) string {
	behind, known := argNumber(infoOf(info).UpdateBehind)
	if !known || behind == 0 {
		return ""
	}
	command := infoText(infoOf(info).UpdateCommand)
	if command == "" {
		command = "icebath update"
	}
	switch {
	case behind < 0:
		return tr("session.notices.updateAvailable", command)
	case behind == 1:
		return tr("session.notices.updateBehindOne", behind, command)
	}
	return tr("session.notices.updateBehindOther", behind, command)
}

// Credential, config, redaction and inventory warnings of a new session, then its opening message
func reportNewSessionNotices(info *tuigateway.SessionInfo, message string, sys func(string)) {
	if warning := val(infoOf(info).CredentialWarning); warning != "" {
		sys("warning: " + describeCredentialWarning(warning))
	}
	if warning := val(infoOf(info).ConfigWarning); warning != "" {
		sys("warning: " + warning)
	}
	reportRedactionOff(info, sys)
	reportInventoryNotices(info, sys)
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
func liveSessionInflightMessages(inflight *tuigateway.InflightTurn) []msg {
	if inflight == nil {
		return []msg{}
	}
	user := strings.TrimFunc(inflight.User, isJSSpace)
	if user == "" {
		return []msg{}
	}
	return toTranscriptMessages([]tuigateway.TranscriptMessage{{Role: "user", Text: &user, DisplayKind: inflight.DisplayKind, DisplayMetadata: inflight.DisplayMetadata}})
}

// Streams the partial reply of a live session's turn in flight
func (l *sessionLifecycle) hydrateLiveSessionInflight(inflight *tuigateway.InflightTurn) {
	if inflight == nil || inflight.Assistant == "" && !inflight.Streaming {
		return
	}
	l.turn.hydrateStreamingText(inflight.Assistant)
}

// Tells the app a different session replaced the previous one, true when it did
func signalFreshSessionBoundary(previousSid, nextSid string, onFresh func(string)) bool {
	if previousSid == "" || nextSid == "" || previousSid == nextSid {
		return false
	}
	onFresh(nextSid)
	return true
}

// Closes a session on the legacy wire, printing its closing messages unless deferred
func (l *sessionLifecycle) closeSession(targetSid string, deferMessages bool, then func(*tuigateway.SessionCloseResult) tea.Cmd) tea.Cmd {
	finish := func(closed *tuigateway.SessionCloseResult) tea.Cmd {
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
	return ask(l.rpc, l.view.sys, tuigateway.SessionClose, tuigateway.SessionCloseParams{SessionID: targetSid}, finish)
}

// Prints the messages a session.close answered with
func (l *sessionLifecycle) printClosed(closed *tuigateway.SessionCloseResult) {
	if closed == nil {
		return
	}
	for _, line := range closed.Messages {
		l.view.sys(line)
	}
}

// Detaches a canonical subscription after any detach of the same session finished
func (l *sessionLifecycle) detachCanonical(sid, subscriptionID, endReason string, onlyIfHeld bool, then func(*tuigateway.SessionDetachResult) tea.Cmd) tea.Cmd {
	done := func(result *tuigateway.SessionDetachResult) tea.Cmd {
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
	finish := func(result *tuigateway.SessionDetachResult) tea.Cmd {
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
		params := tuigateway.SessionDetachParams{SessionID: sid, SubscriptionID: subscriptionID}
		if endReason != "" {
			params.EndReason = &endReason
		}
		return ask(l.rpc, l.view.sys, tuigateway.CanonicalSessionDetach, params, func(result *tuigateway.SessionDetachResult) tea.Cmd {
			if result != nil && result.Detached && l.subscriptions[sid] == subscriptionID {
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
func (l *sessionLifecycle) discardStaleAttachment(sid, sub string) tea.Cmd {
	if sub != "" {
		key, _ := json.Marshal([]string{sid, sub})
		l.staleAttachments[string(key)] = [2]string{sid, sub}
	}
	return l.disposeStaleAttachments()
}

// Holds the subscription of an attached session and detaches the one it replaced
func (l *sessionLifecycle) adoptAttachment(sid, sub, previousSid, previousSubscription string, closePrevious bool) tea.Cmd {
	if !l.gw.canonical() || sub == "" {
		return nil
	}
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
func (l *sessionLifecycle) resetVisibleHistory(info *tuigateway.SessionInfo) {
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
	created := func(closed *tuigateway.SessionCloseResult, a *attachment) tea.Cmd {
		if flight != l.attachmentFlight {
			if a == nil {
				return settle("")
			}
			return tea.Batch(l.discardStaleAttachment(a.SessionID, a.SubscriptionID), settle(""))
		}
		if a == nil {
			l.store.ui.Status = "ready"
			return settle("")
		}
		requestedTitle := strings.TrimFunc(title, isJSSpace)
		l.resetSession()
		l.view.setSessionStartedAt(l.store.nowMillis())
		writeActiveSessionFile(a.StoredSID)
		status := tr("session.status.startingAgent")
		if l.gw.canonical() || val(a.Info.Version) != "" {
			status = "ready"
		}
		connected := true
		l.store.ui.GatewayConnected = &connected
		l.store.ui.Info, l.store.ui.Sid, l.store.ui.Status, l.store.ui.StoredSid, l.store.ui.Usage = a.Info, a.SessionID, status, a.StoredSID, usageFrom(a.Info)
		l.view.setHistoryItems(func([]msg) []msg { return []msg{introMsg(a.Info)} })
		reportNewSessionNotices(a.Info, message, l.view.sys)
		l.printClosed(closed)
		cmds := []tea.Cmd{}
		if requestedTitle != "" {
			cmds = append(cmds, l.nameNewSession(a.SessionID, requestedTitle))
		}
		signalFreshSessionBoundary(previousSid, a.SessionID, l.view.onFreshSessionStarted)
		cmds = append(cmds, l.adoptAttachment(a.SessionID, a.SubscriptionID, previousSid, previousSubscription, !keepCurrent), settle(a.SessionID))
		return tea.Batch(cmds...)
	}
	create := func(closed *tuigateway.SessionCloseResult) tea.Cmd {
		if flight != l.attachmentFlight {
			return settle("")
		}
		cwd := strings.TrimFunc(os.Getenv("ICEBATH_TUI_CWD"), isJSSpace)
		if l.gw.canonical() {
			params, err := localCreationOptions(l.gw.creationEnv())
			if err != nil {
				l.view.sys("error: " + err.Error())
				return created(closed, nil)
			}
			params.RequestID = ptr(uuid.NewString())
			if cwd != "" {
				params.Cwd = &cwd
			}
			return ask(l.rpc, l.view.sys, tuigateway.CanonicalSessionCreate, params, func(r *tuigateway.CanonicalSnapshot) tea.Cmd {
				if r == nil {
					return created(closed, nil)
				}
				a := snapshotAttachment(r)
				return created(closed, &a)
			})
		}
		params := tuigateway.SessionCreateParams{Cols: ptr(int64(l.view.cols()))}
		if cwd != "" {
			params.Cwd = &cwd
		}
		return ask(l.rpc, l.view.sys, tuigateway.SessionCreate, params, func(r *tuigateway.SessionCreateResult) tea.Cmd {
			if r == nil {
				return created(closed, nil)
			}
			info := r.Info
			stored := firstText(r.StoredSessionID, val(info.StoredSessionID), r.SessionID)
			info.StoredSessionID = &stored
			return created(closed, &attachment{SessionID: r.SessionID, StoredSID: stored, Info: &info})
		})
	}
	afterSetup := func(setup *tuigateway.SetupStatusResult) tea.Cmd {
		if flight != l.attachmentFlight || setup == nil {
			return settle("")
		}
		if setup.ProviderConfigured != nil && !*setup.ProviderConfigured {
			l.view.panel(setupRequiredTitle(), buildSetupRequiredSections())
			l.store.ui.Status = tr("session.status.setupRequired")
			return settle("")
		}
		if keepCurrent {
			return create(nil)
		}
		return l.closeSession(previousSid, true, create)
	}
	return l.setupStatus(afterSetup)
}

// Names a freshly created session, reporting the title or the failure while it stays current
func (l *sessionLifecycle) nameNewSession(sid, requestedTitle string) tea.Cmd {
	apply := func(title string, pending bool) tea.Cmd {
		if l.store.ui.Sid != sid {
			return nil
		}
		next := strings.TrimFunc(firstText(title, requestedTitle), isJSSpace)
		suffix := ""
		if pending {
			suffix = tr("session.lifecycle.titleQueuedSuffix")
		}
		l.store.ui.SessionTitle = next
		l.view.sys(tr("session.lifecycle.sessionTitleSet", next) + suffix)
		return nil
	}
	if !l.gw.canonical() {
		return ask(l.rpc, l.view.sys, tuigateway.SessionTitle, tuigateway.SessionTitleParams{SessionID: sid, Title: &requestedTitle}, func(r *tuigateway.SessionTitleResult) tea.Cmd {
			if r == nil {
				return nil
			}
			return apply(r.Title, val(r.Pending))
		})
	}
	return l.store.mutateCanonical(l.gw, sid, "rename", map[string]any{"title": requestedTitle}, func() bool { return false }, func(m *mutationOutcome, err error) tea.Cmd {
		if err != nil {
			if l.store.ui.Sid == sid {
				l.view.sys("warning: " + tr("session.lifecycle.failedToSetTitle", err.Error()))
			}
			return nil
		}
		if m == nil {
			return nil
		}
		return apply(val(m.Result.Title), false)
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
func (l *sessionLifecycle) applyAttachment(a attachment, hold *attachHold) tea.Cmd {
	l.resetSession()
	startedAt := l.store.nowMillis()
	if a.StartedAt != 0 {
		startedAt = a.StartedAt * 1000
	}
	l.view.setSessionStartedAt(startedAt)
	transcript := append(toTranscriptMessages(a.Messages), liveSessionInflightMessages(a.Inflight)...)
	l.view.setHistoryItems(func([]msg) []msg { return append([]msg{introMsg(a.Info)}, transcript...) })
	reportRedactionOff(a.Info, l.view.sys)
	reportInventoryNotices(a.Info, l.view.sys)
	writeActiveSessionFile(a.StoredSID)
	connected := true
	l.store.ui.Busy, l.store.ui.GatewayConnected = a.Running, &connected
	l.store.ui.Info, l.store.ui.Sid, l.store.ui.Status, l.store.ui.StoredSid, l.store.ui.Usage = a.Info, a.SessionID, statusFromLiveSession(a.Status, a.Running), a.StoredSID, usageFrom(a.Info)
	l.gw.hydrateSharedPrompts(a.Snapshot)
	l.hydrateLiveSessionInflight(a.Inflight)
	l.gw.settleAttach(hold, a.Snapshot)
	return l.scheduleResumeScrollToBottom()
}

// Request that waits for the session's detach in flight, dropped when a newer flight started meanwhile
func (l *sessionLifecycle) requestAfterDetach(id string, flight int, then func() tea.Cmd, dropped func() tea.Cmd) tea.Cmd {
	if l.detachFlights[id] == nil {
		return then()
	}
	return l.afterDetach(id, func() tea.Cmd {
		if flight != l.attachmentFlight {
			return dropped()
		}
		return then()
	})
}

// Attachment of a session by id, the authority's resume when canonical and the method table's activation or resume otherwise
func (l *sessionLifecycle) attachByID(id string, live bool, then func(*attachment, error) tea.Cmd) tea.Cmd {
	if l.gw.canonical() {
		params := tuigateway.CanonicalResumeParams{SessionID: id, DisplayProjection: true}
		return call(l.gw, tuigateway.CanonicalSessionResume, params, func(r *tuigateway.CanonicalSnapshot, err error) tea.Cmd {
			if err != nil {
				return then(nil, err)
			}
			a := snapshotAttachment(r)
			return then(&a, nil)
		})
	}
	if live {
		return call(l.gw, tuigateway.SessionActivate, tuigateway.SessionActivateParams{SessionID: id}, func(r *tuigateway.SessionActivateResult, err error) tea.Cmd {
			if err != nil {
				return then(nil, err)
			}
			a := resumedAttachment(r, firstText(val(r.SessionKey), val(r.StoredSessionID), val(r.Info.StoredSessionID), r.SessionID))
			return then(&a, nil)
		})
	}
	params := tuigateway.SessionResumeParams{SessionID: id, Cols: ptr(int64(l.view.cols()))}
	return call(l.gw, tuigateway.SessionResume, params, func(r *tuigateway.SessionResumeResult, err error) tea.Cmd {
		if err != nil {
			return then(nil, err)
		}
		a := resumedAttachment(r, firstText(val(r.SessionKey), val(r.Info.StoredSessionID), val(r.StoredSessionID), val(r.Resumed), id))
		return then(&a, nil)
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
	done := func(cmd tea.Cmd) tea.Cmd {
		l.gw.settleAttach(hold, nil)
		return tea.Batch(cmd, l.finishAttachment(flight))
	}
	return l.requestAfterDetach(id, flight, func() tea.Cmd {
		return l.attachByID(id, true, func(a *attachment, err error) tea.Cmd {
			if err != nil {
				if flight == l.attachmentFlight {
					l.view.sys("error: " + err.Error())
					l.store.ui.Status = "ready"
				}
				return done(nil)
			}
			if flight != l.attachmentFlight {
				return done(l.discardStaleAttachment(a.SessionID, a.SubscriptionID))
			}
			cmd := l.applyAttachment(*a, hold)
			if a.Connection != nil {
				l.store.applyConnectionRequest(*a.Connection)
			}
			return done(tea.Batch(cmd, l.adoptAttachment(a.SessionID, a.SubscriptionID, previousSid, previousSubscription, false)))
		})
	}, func() tea.Cmd { return done(nil) })
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
	afterSetup := func(setup *tuigateway.SetupStatusResult) tea.Cmd {
		if flight != l.attachmentFlight || setup == nil {
			return finish()
		}
		if setup.ProviderConfigured != nil && !*setup.ProviderConfigured {
			l.view.panel(setupRequiredTitle(), buildSetupRequiredSections())
			l.store.ui.Status = tr("session.status.setupRequired")
			return finish()
		}
		hold := l.gw.holdAttach(id)
		return l.requestAfterDetach(id, flight, func() tea.Cmd {
			return l.attachByID(id, false, func(a *attachment, err error) tea.Cmd {
				defer l.gw.settleAttach(hold, nil)
				if err != nil {
					if flight == l.attachmentFlight {
						l.view.sys("error: " + err.Error())
						l.store.ui.Status = "ready"
					}
					return finish()
				}
				if flight != l.attachmentFlight {
					return finish(l.discardStaleAttachment(a.SessionID, a.SubscriptionID))
				}
				if err := pendingJournal.migrate(destination, a.SessionID, a.StoredSID); err != nil {
					if flight == l.attachmentFlight {
						l.view.sys("error: " + err.Error())
						l.store.ui.Status = "ready"
					}
					return finish()
				}
				cmd := l.applyAttachment(*a, hold)
				if a.Connection != nil {
					l.store.applyConnectionRequest(*a.Connection)
				} else {
					l.store.clearConnectionOperation()
				}
				cmds := []tea.Cmd{cmd}
				if previousSid != "" && previousSid != a.SessionID {
					cmds = append(cmds, l.closeSession(previousSid, false, nil))
				}
				return finish(append(cmds, l.adoptAttachment(a.SessionID, a.SubscriptionID, previousSid, previousSubscription, true))...)
			})
		}, func() tea.Cmd { return finish() })
	}
	return l.setupStatus(afterSetup)
}

// Setup readiness of the profile, nil after a printed failure
func (l *sessionLifecycle) setupStatus(then func(*tuigateway.SetupStatusResult) tea.Cmd) tea.Cmd {
	return ask(l.rpc, l.view.sys, tuigateway.SetupStatus, tuigateway.ProfileParams{}, then)
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
