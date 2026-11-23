package tui

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

var resumeScrollDelays = []time.Duration{0, 80 * time.Millisecond, 240 * time.Millisecond}

// Gateway surface of session creation, resume and attachment, the gateway client in production
type sessionGateway interface {
	turnGateway
	holdAttach(sessionID string) *attachHold
	settleAttach(hold *attachHold, snapshot *tuigateway.CanonicalSnapshot)
	hydrateSharedPrompts(snapshot *tuigateway.CanonicalSnapshot, open []tuigateway.OpenRequestEntry)
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
	cols() int
	scroll() scrollHandle
	onFreshSessionStarted(sid string)
	// A session was left mid turn and keeps working out of view
	leftRunning(sid string)
	// No model is set up yet: the get started screen opens
	offerGetStarted() tea.Cmd
	// The view shows another session than the previous one, or the same one attached anew
	sessionBound(previous string)
	// Whether another card of the window already shows the session, that card taking the focus when so
	shownElsewhere(id string) bool
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

	// Subscriptions of the whole window, shared with every other session card
	subs *subscriptions
	// Session whose logins the first session of a new card starts with, the session of the card it was opened from
	loginsFrom string
	// Why the last create or attach failed, nil after one that went through
	failed error
	// Attach of what this card shows, the newest flight winning, the flights still out and the subscriptions of answers that came too late
	attachmentFlight   int
	pendingAttachments map[int]bool
	staleAttachments   map[string][2]string
	resumeScrollGen    int
}

// Lifecycle of one session card over the subscriptions of its window
func newSessionLifecycle(store *tuiStore, turn *turnController, gw sessionGateway, view sessionView, subs *subscriptions) *sessionLifecycle {
	return &sessionLifecycle{store: store, turn: turn, gw: gw, view: view, subs: subs, pendingAttachments: map[int]bool{}, staleAttachments: map[string][2]string{}}
}

// Gateway request that prints its own failure and hands nil to then
func (l *sessionLifecycle) rpc(method string, params map[string]any, then func(map[string]any) tea.Cmd) tea.Cmd {
	return gatewayCall(l.gw, method, params, func(v any, err error) tea.Cmd {
		result, isObject := v.(map[string]any)
		l.failed = err
		switch {
		case err != nil:
			l.view.sys(errorLine(rpcErrorMessage(err)))
		case !isObject:
			l.view.sys(errorLine(misfitText(tr(txt.SessionCommonInvalidResponse, method))))
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
		sys(warningLine(tr(txt.SessionNoticesRedactionOff)))
	}
}

// Status line of a live session, running or ready
func statusFromLiveSession(running bool) string {
	if running {
		return "running…"
	}
	return "ready"
}

// Notices the gateway computed with the session inventory: toolsets it cannot place, tools switched off, and the project skills of the folder
func reportInventoryNotices(info *tuigateway.SessionInfo, sys func(string)) {
	held := infoOf(info)
	if len(held.UnknownToolsets) > 0 {
		sys(warningLine(tr(txt.SessionNoticesUnknownToolsets, strings.Join(held.UnknownToolsets, ", "))))
	}
	for _, notice := range held.ToolAvailabilityWarnings {
		if notice = trimJS(notice); notice != "" {
			sys(warningLine(notice))
		}
	}
	if untrusted := held.ProjectSkillsUntrusted; untrusted != nil && untrusted.Count > 0 {
		sys(warningLine(tr(txt.SessionNoticesProjectSkillsUntrusted, untrusted.Count, untrusted.Root)))
	} else if loaded := val(held.ProjectSkillsLoaded); loaded > 0 {
		sys(tr(txt.SessionNoticesProjectSkillsLoaded, loaded))
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
		return tr(txt.SessionNoticesUpdateAvailable, command)
	case behind == 1:
		return tr(txt.SessionNoticesUpdateBehindOne, behind, command)
	}
	return tr(txt.SessionNoticesUpdateBehindOther, behind, command)
}

// Credential, config, redaction and inventory warnings of a new session, then its opening message
func reportNewSessionNotices(info *tuigateway.SessionInfo, message string, sys func(string)) {
	if warning := val(infoOf(info).CredentialWarning); warning != "" {
		sys(warningLine(describeCredentialWarning(warning)))
	}
	if warning := val(infoOf(info).ConfigWarning); warning != "" {
		sys(warningLine(warning))
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

// Tells the app a different session replaced the previous one, true when it did
func signalFreshSessionBoundary(previousSid, nextSid string, onFresh func(string)) bool {
	if previousSid == "" || nextSid == "" || previousSid == nextSid {
		return false
	}
	onFresh(nextSid)
	return true
}

// Detaches a subscription after any detach of the same session finished
func (l *sessionLifecycle) detachCanonical(sid, subscriptionID, endReason string, onlyIfHeld bool, then func(*tuigateway.SessionDetachResult) tea.Cmd) tea.Cmd {
	done := func(result *tuigateway.SessionDetachResult) tea.Cmd {
		if then == nil {
			return nil
		}
		return then(result)
	}
	if sid == "" || subscriptionID == "" {
		return resolved(func() tea.Cmd { return done(nil) })
	}
	flight := &detachFlight{}
	previous := l.subs.flights[sid]
	l.subs.flights[sid] = flight
	finish := func(result *tuigateway.SessionDetachResult) tea.Cmd {
		cmds := []tea.Cmd{done(result)}
		if l.subs.flights[sid] == flight {
			delete(l.subs.flights, sid)
		}
		for _, w := range flight.waiters {
			cmds = append(cmds, w())
		}
		return tea.Batch(cmds...)
	}
	run := func() tea.Cmd {
		if onlyIfHeld && l.subs.of(sid) != subscriptionID {
			return finish(nil)
		}
		params := tuigateway.SessionDetachParams{SessionID: sid, SubscriptionID: subscriptionID}
		if endReason != "" {
			params.EndReason = &endReason
		}
		return ask(l.rpc, l.view.sys, tuigateway.CanonicalSessionDetach, params, func(result *tuigateway.SessionDetachResult) tea.Cmd {
			if result != nil && result.Detached {
				l.subs.release(sid, subscriptionID)
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

// Detaches every subscription this card holds as the card closes or the TUI shuts down, the sessions running on in the background service
func (l *sessionLifecycle) closeForExit() tea.Cmd {
	cmds := []tea.Cmd{}
	for sid, sub := range l.subs.heldBy(l) {
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
		if l.subs.of(s[0]) != s[1] {
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
	if sub == "" {
		return nil
	}
	l.subs.hold(sid, sub, l)
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
	l.store.replayEpoch, l.store.lastSeq = "", 0
	l.view.setHistoryItems(func([]msg) []msg { return []msg{} })
	l.view.setLastUserMsg("")
	l.view.setStickyPrompt("")
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
		return l.store.clearScreen()
	}
	return nil
}

// Starts a session, closing the current one unless kept, then names it and adopts its subscription
func (l *sessionLifecycle) startNewSession(message, title string, keepCurrent bool, then func(sid string) tea.Cmd) tea.Cmd {
	return l.startSession(message, title, keepCurrent, true, then)
}

// Starts a session as startNewSession does. With intro set the transcript opens on the session banner and the notices of the new session;
// without it, as /clear wants, the transcript stays blank: no banner, no notices, no setup panel
func (l *sessionLifecycle) startSession(message, title string, keepCurrent, intro bool, then func(sid string) tea.Cmd) tea.Cmd {
	l.attachmentFlight++
	flight := l.attachmentFlight
	previousSid := l.store.ui.Sid
	previousSubscription := l.subs.of(previousSid)
	leftBusy := keepCurrent && l.store.ui.Busy
	l.pendingAttachments[flight] = true
	settle := func(sid string) tea.Cmd {
		cmds := []tea.Cmd{l.finishAttachment(flight)}
		if then != nil {
			cmds = append(cmds, then(sid))
		}
		return tea.Batch(cmds...)
	}
	created := func(a *attachment) tea.Cmd {
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
		if leftBusy {
			l.view.leftRunning(previousSid)
		}
		l.resetSession()
		l.view.setSessionStartedAt(l.store.nowMillis())
		l.store.ui.setConnected(true)
		l.store.ui.Info, l.store.ui.Sid, l.store.ui.Status, l.store.ui.StoredSid, l.store.ui.Usage = a.Info, a.SessionID, "ready", a.StoredSID, usageFrom(a.Info)
		if a.Snapshot != nil {
			l.store.replayEpoch, l.store.lastSeq = a.Snapshot.ReplayEpoch, a.Snapshot.LastSequence
		}
		l.view.sessionBound(previousSid)
		if intro {
			l.view.setHistoryItems(func([]msg) []msg { return []msg{introMsg(a.Info)} })
			reportNewSessionNotices(a.Info, message, l.view.sys)
		} else {
			l.view.setHistoryItems(func([]msg) []msg { return []msg{} })
		}
		cmds := []tea.Cmd{}
		if requestedTitle != "" {
			cmds = append(cmds, l.nameNewSession(a.SessionID, requestedTitle))
		}
		signalFreshSessionBoundary(previousSid, a.SessionID, l.view.onFreshSessionStarted)
		cmds = append(cmds, l.adoptAttachment(a.SessionID, a.SubscriptionID, previousSid, previousSubscription, !keepCurrent), settle(a.SessionID))
		return tea.Batch(cmds...)
	}
	create := func() tea.Cmd {
		if flight != l.attachmentFlight {
			return settle("")
		}
		params, err := localCreationOptions(l.gw.creationEnv())
		if err != nil {
			l.view.sys(errorLine(err.Error()))
			return created(nil)
		}
		params.RequestID = ptr(uuid.NewString())
		if cwd := strings.TrimFunc(os.Getenv("ICEBATH_TUI_CWD"), isJSSpace); cwd != "" {
			params.Cwd = &cwd
		}
		params.LoginsFrom = l.inheritLogins(previousSid)
		return ask(l.rpc, l.view.sys, tuigateway.CanonicalSessionCreate, params, func(r *tuigateway.CanonicalSnapshot) tea.Cmd {
			if r == nil {
				return created(nil)
			}
			a := snapshotAttachment(r)
			return created(&a)
		})
	}
	afterSetup := func(setup *tuigateway.SetupStatusResult) tea.Cmd {
		if flight != l.attachmentFlight || setup == nil {
			return settle("")
		}
		if setup.ProviderConfigured == nil || *setup.ProviderConfigured {
			return create()
		}
		return l.launchRouteServes(func(serves bool) tea.Cmd {
			if flight != l.attachmentFlight {
				return settle("")
			}
			if !serves {
				if intro {
					l.view.panel(setupRequiredTitle(), buildSetupRequiredSections())
				}
				l.store.ui.SetupRequired = true
				l.store.ui.Status = tr(txt.SessionStatusSetupRequired)
				return tea.Batch(settle(""), l.view.offerGetStarted())
			}
			return create()
		})
	}
	return l.setupStatus(afterSetup)
}

// Session a new session takes its logins from: the one this card showed before, else the session of the card this one was opened from,
// taken once
func (l *sessionLifecycle) inheritLogins(previousSid string) *string {
	from := firstText(previousSid, l.loginsFrom)
	l.loginsFrom = ""
	if from == "" {
		return nil
	}
	return &from
}

// Attaches the session the card shows again after the connection came back. The background service replays the events since the last one
// applied over the transcript on screen while it still holds them; when it does not, or the card shows another session, the transcript
// is read anew
func (l *sessionLifecycle) reattach(id string, then func(err error) tea.Cmd) tea.Cmd {
	sid := l.store.ui.Sid
	cursor, known := l.store.replayCursor()
	if !known || sid == "" || (sid != id && l.store.ui.StoredSid != id) {
		return l.resumeByID(id, then)
	}
	l.attachmentFlight++
	flight := l.attachmentFlight
	l.pendingAttachments[flight] = true
	hold := l.gw.holdAttach(sid)
	finish := func(err error, cmds ...tea.Cmd) tea.Cmd {
		cmds = append(cmds, l.finishAttachment(flight))
		if then != nil {
			cmds = append(cmds, then(err))
		}
		return tea.Batch(cmds...)
	}
	params := tuigateway.CanonicalResumeParams{SessionID: sid, OmitMessages: true, DisplayProjection: true, ReplayFrom: &cursor}
	return call(l.gw, tuigateway.CanonicalSessionResume, params, func(r *tuigateway.CanonicalSnapshot, err error) tea.Cmd {
		if err != nil {
			l.gw.settleAttach(hold, nil)
			if flight == l.attachmentFlight {
				// The turn that was frozen on screen has no session to go on in
				l.turn.reset()
				l.store.ui.Status = "ready"
			}
			return finish(err)
		}
		a := snapshotAttachment(r)
		if flight != l.attachmentFlight {
			l.gw.settleAttach(hold, nil)
			return finish(nil, l.discardStaleAttachment(a.SessionID, a.SubscriptionID))
		}
		if !val(r.Replayed) {
			cmd := l.applyAttachment(a, hold)
			if a.Connection != nil {
				l.store.applyConnectionRequest(*a.Connection)
			} else {
				l.store.clearConnectionOperation()
			}
			return finish(nil, cmd, l.adoptAttachment(a.SessionID, a.SubscriptionID, "", "", false))
		}
		// The transcript on screen stands; the events that were missed come next, those already applied are skipped
		l.store.ui.setConnected(true)
		l.store.ui.Info = a.Info
		l.store.ui.Status = "ready"
		if l.store.ui.Busy {
			l.store.ui.Status = "running…"
		}
		l.gw.hydrateSharedPrompts(r, a.OpenRequests)
		fence := *r
		fence.LastSequence = cursor.Sequence
		l.gw.settleAttach(hold, &fence)
		l.view.sessionBound(sid)
		return finish(nil, l.adoptAttachment(a.SessionID, a.SubscriptionID, "", "", false))
	})
}

// Whether the launch's own provider can carry a session on a profile the setup status calls unconfigured, counted only with a model from the launch or the profile
func (l *sessionLifecycle) launchRouteServes(then func(bool) tea.Cmd) tea.Cmd {
	env := l.gw.creationEnv()
	launchProvider, launchModel := env("ICEBATH_TUI_PROVIDER"), env("ICEBATH_MODEL")
	if launchProvider == "" {
		return then(false)
	}
	params := tuigateway.SetupRuntimeCheckParams{Provider: &launchProvider}
	return ask(l.rpc, l.view.sys, tuigateway.SetupRuntimeCheck, params, func(check *tuigateway.SetupRuntimeCheckResult) tea.Cmd {
		return then(check != nil && check.OK && (launchModel != "" || val(check.Model) != ""))
	})
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
			suffix = tr(txt.SessionLifecycleTitleQueuedSuffix)
		}
		l.store.ui.SessionTitle = next
		l.view.sys(tr(txt.SessionLifecycleSessionTitleSet, next) + suffix)
		return nil
	}
	return l.store.mutateCanonical(l.gw, sid, "rename", map[string]any{"title": requestedTitle}, func() bool { return false }, func(m *mutationOutcome, err error) tea.Cmd {
		if err != nil {
			if l.store.ui.Sid == sid {
				l.view.sys(warningLine(tr(txt.SessionLifecycleFailedToSetTitle, err.Error())))
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

// Wipes the transcript at once, the /clear line itself included, then starts a fresh session in place of the current one that adds nothing to it
func (l *sessionLifecycle) clearSession(then func(sid string) tea.Cmd) tea.Cmd {
	l.view.setHistoryItems(func([]msg) []msg { return []msg{} })
	l.view.setStickyPrompt("")
	return l.startSession("", "", false, false, then)
}

// Starts a fresh session beside the current live one, closing the switcher
func (l *sessionLifecycle) newLiveSession(message, title string, then func(sid string) tea.Cmd) tea.Cmd {
	if message == "" {
		message = tr(txt.SessionLifecycleNewLiveSessionStarted)
	}
	return l.startNewSession(message, title, true, then)
}

// Shows an attached session's transcript, state, prompts and connection card
func (l *sessionLifecycle) applyAttachment(a attachment, hold *attachHold) tea.Cmd {
	previous := l.store.ui.Sid
	l.resetSession()
	l.view.setSessionStartedAt(l.store.nowMillis())
	transcript := toTranscriptMessages(a.Messages, l.store.ui)
	l.view.setHistoryItems(func([]msg) []msg { return append([]msg{introMsg(a.Info)}, transcript...) })
	reportRedactionOff(a.Info, l.view.sys)
	reportInventoryNotices(a.Info, l.view.sys)
	l.store.ui.setConnected(true)
	l.store.ui.Info, l.store.ui.Sid, l.store.ui.Status, l.store.ui.StoredSid, l.store.ui.Usage = a.Info, a.SessionID, statusFromLiveSession(a.Running), a.StoredSID, usageFrom(a.Info)
	if a.Snapshot != nil {
		l.store.replayEpoch, l.store.lastSeq = a.Snapshot.ReplayEpoch, a.Snapshot.LastSequence
	}
	l.store.setBusy(a.Running)
	l.view.sessionBound(previous)
	l.gw.hydrateSharedPrompts(a.Snapshot, a.OpenRequests)
	l.gw.settleAttach(hold, a.Snapshot)
	return l.scheduleResumeScrollToBottom()
}

// Request that waits for the session's detach in flight, dropped when a newer flight started meanwhile
func (l *sessionLifecycle) requestAfterDetach(id string, flight int, then func() tea.Cmd, dropped func() tea.Cmd) tea.Cmd {
	if l.subs.flights[id] == nil {
		return then()
	}
	return l.subs.afterDetach(id, func() tea.Cmd {
		if flight != l.attachmentFlight {
			return dropped()
		}
		return then()
	})
}

// Attachment of a session by id: the background service resumes it and answers with its snapshot
func (l *sessionLifecycle) attachByID(id string, then func(*attachment, error) tea.Cmd) tea.Cmd {
	params := tuigateway.CanonicalResumeParams{SessionID: id, DisplayProjection: true}
	return call(l.gw, tuigateway.CanonicalSessionResume, params, func(r *tuigateway.CanonicalSnapshot, err error) tea.Cmd {
		if err != nil {
			return then(nil, err)
		}
		a := snapshotAttachment(r)
		return then(&a, nil)
	})
}

// Switches the view to another live session the gateway already runs, the card that already shows it taking the focus instead
func (l *sessionLifecycle) activateLiveSession(id string) tea.Cmd {
	if l.view.shownElsewhere(id) {
		return nil
	}
	l.attachmentFlight++
	flight := l.attachmentFlight
	l.pendingAttachments[flight] = true
	previousSid := l.store.ui.Sid
	previousSubscription := l.subs.of(previousSid)
	l.store.ui.Status = tr(txt.SessionStatusSwitchingSession)
	l.store.clearConnectionOperation()
	hold := l.gw.holdAttach(id)
	done := func(cmd tea.Cmd) tea.Cmd {
		l.gw.settleAttach(hold, nil)
		return tea.Batch(cmd, l.finishAttachment(flight))
	}
	return l.requestAfterDetach(id, flight, func() tea.Cmd {
		return l.attachByID(id, func(a *attachment, err error) tea.Cmd {
			if err != nil {
				if flight == l.attachmentFlight {
					l.view.sys(errorLine(err.Error()))
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

// Resumes a stored or live session by id, moving its journaled inputs to the resumed session, the card that already shows it taking the focus
// instead
func (l *sessionLifecycle) resumeByID(id string, then func(err error) tea.Cmd) tea.Cmd {
	if l.view.shownElsewhere(id) {
		return resolved(func() tea.Cmd {
			if then != nil {
				return then(nil)
			}
			return nil
		})
	}
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
	previousSubscription := l.subs.of(previousSid)
	destination := current
	if current.Sid != id && current.StoredSid != id {
		destination.Sid, destination.StoredSid = id, id
	}
	l.store.ui.Status = tr(txt.SessionStatusResuming)
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
			l.store.ui.SetupRequired = true
			l.view.panel(setupRequiredTitle(), buildSetupRequiredSections())
			l.store.ui.Status = tr(txt.SessionStatusSetupRequired)
			return tea.Batch(finish(), l.view.offerGetStarted())
		}
		hold := l.gw.holdAttach(id)
		return l.requestAfterDetach(id, flight, func() tea.Cmd {
			return l.attachByID(id, func(a *attachment, err error) tea.Cmd {
				defer l.gw.settleAttach(hold, nil)
				l.failed = err
				if err != nil {
					if flight == l.attachmentFlight {
						l.view.sys(errorLine(err.Error()))
						l.store.ui.Status = "ready"
					}
					return finish()
				}
				if flight != l.attachmentFlight {
					return finish(l.discardStaleAttachment(a.SessionID, a.SubscriptionID))
				}
				if err := pendingJournal.migrate(destination, a.SessionID, a.StoredSID); err != nil {
					if flight == l.attachmentFlight {
						l.view.sys(errorLine(err.Error()))
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
				return finish(cmd, l.adoptAttachment(a.SessionID, a.SubscriptionID, previousSid, previousSubscription, true))
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
		what = tr(txt.SessionLifecycleSwitchSessions)
	}
	l.view.sys(tr(txt.SessionLifecycleInterruptBeforeSwitch, what))
	return true
}
