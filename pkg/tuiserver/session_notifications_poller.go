package tuiserver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Loop and heartbeat wakeup poll cadence, the kanban cadence
const loopPollSeconds = kanbanPollSeconds

// Kanban wakeups of a session, bound by the extras tier that carries kanban and nil in a build without it
var kanbanNotificationPoll func(ctx context.Context, sid string, session *tuihost.Session)

// Routes a slash command loop wakeup through the slash pipeline, completing the tick unless it resolves to a prompt
func notifSlashLoopTick(ctx context.Context, rid, sid string, session *tuihost.Session, mgr *agent.LoopManager, wakeup string) {
	notifReleaseTurn(session)
	sent := func() (sent bool) {
		defer func() {
			if recover() != nil {
				sent = false
			}
		}()
		name, arg := splitFirstWord(strings.TrimLeftFunc(wakeup, unicode.IsSpace)[1:])
		resp := lookupMethod("command.dispatch")(ctx, rid, map[string]any{"name": name, "arg": arg, "session_id": sid})
		payload, _ := resp["result"].(map[string]any)
		if out := strings.TrimSpace(strOr(payload["output"])); out != "" {
			notifLoopStatus(ctx, sid, out)
		}
		if payload["type"] == "send" && truthy(payload["message"]) {
			if !notifClaimTurn(session) {
				mgr.AbandonTick(ctx)
				publishSessionControlSnapshot(ctx, sid, session, false, false)
				return true
			}
			notifSubmit(ctx, rid, sid, session, strOr(payload["message"]), "loop wakeup send failed", promptSubmitOptions{})
			return true
		}
		return false
	}()
	if sent {
		return
	}
	decision := mgr.CompleteTick(ctx, "")
	if decision.Message != "" {
		notifLoopStatus(ctx, sid, decision.Message)
	}
	publishSessionControlSnapshot(ctx, sid, session, false, false)
}

// Fires a due heartbeat prompt for an idle session this process owns, rewinding the fire when no turn starts
func maybeFireTUIHeartbeatTick(ctx context.Context, sid string, session *tuihost.Session) {
	sidKey := strOr(session.Get("session_key"))
	if sidKey == "" {
		return
	}
	mgr := agent.NewHeartbeatManager(ctx, sidKey)
	if !mgr.IsActive() || !mgr.State().IsDue(nowSeconds()) || notifGatewayOwnsHeartbeat(ctx, session, sidKey) {
		return
	}
	if !notifClaimTurn(session) {
		return
	}
	prompt, due := mgr.DuePrompt(nil)
	if !due || prompt == "" {
		notifReleaseTurn(session)
		return
	}
	started := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				notifLogFailure("heartbeat dispatch failed", r)
			}
		}()
		tuihost.Emit(ctx, "status.update", sid, map[string]any{"kind": "heartbeat",
			"text": fmt.Sprintf("♥ heartbeat #%d firing…", mgr.State().FireCount)})
		started = runPromptSubmit(ctx, fmt.Sprintf("__heartbeat__%d", time.Now().UnixMilli()), sid, session, prompt, promptSubmitOptions{})
	}()
	if !started {
		notifReleaseTurn(session)
		suppress(func() { mgr.AbandonFire() })
	}
}

// Fires a due loop wakeup for an idle session, claiming it first so a racing user prompt wins
func maybeFireTUILoopTick(ctx context.Context, sid string, session *tuihost.Session) {
	sidKey := strOr(session.Get("session_key"))
	if sidKey == "" {
		return
	}
	mgr := agent.NewLoopManager(ctx, sidKey)
	if !mgr.IsDue() || agent.GoalBlocksLoopTick(ctx, sidKey) || loopRouteIsGatewayChat(mgr.State()) {
		return
	}
	if !notifClaimTurn(session) {
		return
	}
	wakeup, fired := mgr.FireTick(ctx)
	if !fired || wakeup == "" {
		notifReleaseTurn(session)
		return
	}
	rid := fmt.Sprintf("__loop__%d", time.Now().UnixMilli())
	publishSessionControlSnapshot(ctx, sid, session, false, false)
	defer func() {
		if r := recover(); r != nil {
			notifLogFailure("loop wakeup dispatch failed", r)
			notifReleaseTurn(session)
			suppress(func() { mgr.AbandonTick(ctx) })
			publishSessionControlSnapshot(ctx, sid, session, false, false)
		}
	}()
	fireCount := "?"
	if st := mgr.State(); st != nil {
		fireCount = fmt.Sprint(st.TicksFired)
	}
	notifLoopStatus(ctx, sid, fmt.Sprintf("↻ /loop wakeup #%s firing…", fireCount))
	if strings.HasPrefix(strings.TrimLeftFunc(wakeup, unicode.IsSpace), "/") {
		notifSlashLoopTick(ctx, rid, sid, session, mgr, wakeup)
		return
	}
	tuihost.Emit(ctx, "message.start", sid, nil)
	runPromptSubmit(ctx, rid, sid, session, wakeup, promptSubmitOptions{})
}

// str.split(None, 1) of a command line, the first word and the remainder with its leading whitespace dropped
func splitFirstWord(s string) (string, string) {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	end := strings.IndexFunc(s, unicode.IsSpace)
	if end < 0 {
		return s, ""
	}
	return s[:end], strings.TrimLeftFunc(s[end:], unicode.IsSpace)
}

// Poller of one session under its profile scope
func notificationPollerLoop(ctx context.Context, stop *pythread.Event, sid string, session *tuihost.Session) {
	notificationPollerScopedLoop(sessionProfileRuntimeScope(ctx, session, true), stop, sid, session)
}

// Drains the completion queue for this session, claims parked events and polls loop, heartbeat and kanban wakeups
func notificationPollerScopedLoop(ctx context.Context, stop *pythread.Event, sid string, session *tuihost.Session) {
	registry := tools.DefaultProcessRegistry()
	registry.RestoreCompletions()
	queue := registry.CompletionQueue
	emitted, _ := session.SetDefault("_notification_emitted", map[string]bool{}).(map[string]bool)
	handle := func(events []map[string]any, deferred *[]map[string]any) {
		notifHandleReady(ctx, sid, session, events, emitted, registry, tools.FormatProcessNotification, deferred, false)
	}
	lastKanbanPoll, lastLoopPoll := 0.0, 0.0
	env := tools.ToolEnvFrom(ctx)
	for !stop.IsSet() && !truthy(session.Get("_finalized")) {
		now := pythread.Monotonic()
		tools.MaybeSweepOrphanedCompletions(env, queue, nil)
		if now-lastLoopPoll >= loopPollSeconds {
			lastLoopPoll = now
			for _, tick := range []struct {
				what string
				fire func(context.Context, string, *tuihost.Session)
			}{{"loop wakeup", maybeFireTUILoopTick}, {"heartbeat", maybeFireTUIHeartbeatTick}} {
				func() {
					defer func() {
						if r := recover(); r != nil {
							notifLogFailure(tick.what+" poll failed", r)
						}
					}()
					tick.fire(ctx, sid, session)
				}()
			}
		}
		if now-lastKanbanPoll >= kanbanPollSeconds && kanbanNotificationPoll != nil {
			lastKanbanPoll = now
			kanbanNotificationPoll(ctx, sid, session)
		}
		parked := tuihost.ClaimParked(func(evt map[string]any) bool { return sessionOwnsNotificationEvent(ctx, sid, session, evt) })
		func() {
			var ready []map[string]any
			defer func() {
				if r := recover(); r != nil {
					notifLogFailure("notification dispatch failed", r)
				}
				tools.ReleaseEvents(ready)
				tuihost.UnholdParked(parked)
			}()
			wait := 500 * time.Millisecond
			if len(parked) > 0 {
				wait = 0
			}
			ready = tools.TakeReady(queue, wait)
			if len(parked) == 0 && len(ready) == 0 {
				return
			}
			handle(append(append([]map[string]any{}, parked...), ready...), nil)
		}()
	}
	var deferred []map[string]any
	ready := tools.TakeReady(queue, 0)
	defer func() {
		for _, evt := range deferred {
			queue.Put(evt)
		}
		tools.ReleaseEvents(ready)
	}()
	handle(ready, &deferred)
}

// Every poller started in this process with its stop, pruned of finished ones on each spawn
var notificationPollers struct {
	mu      sync.Mutex
	pollers []notificationPoller
}

// One started poller, its stop event and its finish signal
type notificationPoller struct {
	stop *pythread.Event
	done chan struct{}
}

// Starts the background notification poller of a session and returns its stop event
func startNotificationPoller(ctx context.Context, sid string, session *tuihost.Session) *pythread.Event {
	stop := &pythread.Event{}
	done := make(chan struct{})
	notificationPollers.mu.Lock()
	live := notificationPollers.pollers[:0]
	for _, p := range notificationPollers.pollers {
		select {
		case <-p.done:
		default:
			live = append(live, p)
		}
	}
	notificationPollers.pollers = append(live, notificationPoller{stop, done})
	notificationPollers.mu.Unlock()
	go func() {
		defer threadExceptHook("tui-notif-poller-" + sid)
		defer close(done)
		notificationPollerLoop(context.WithoutCancel(ctx), stop, sid, session)
	}()
	return stop
}
