package tuiserver

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Kanban event kinds a TUI subscription claims
var kanbanNotifyKinds = []string{"completed", "blocked", "gave_up", "crashed", "timed_out", "status", "archived", "unblocked"}

// Loop and heartbeat wakeup poll cadence, the kanban cadence
const loopPollSeconds = kanbanPollSeconds

// Routes a slash command loop wakeup through the slash pipeline, completing the tick unless it resolves to a prompt
func notifSlashLoopTick(ctx context.Context, rid, sid string, session *tuigateway.Session, mgr *agent.LoopManager, wakeup string) {
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
func maybeFireTUIHeartbeatTick(ctx context.Context, sid string, session *tuigateway.Session) {
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
		tuigateway.Emit(ctx, "status.update", sid, map[string]any{"kind": "heartbeat",
			"text": fmt.Sprintf("♥ heartbeat #%d firing…", mgr.State().FireCount)})
		started = runPromptSubmit(ctx, fmt.Sprintf("__heartbeat__%d", time.Now().UnixMilli()), sid, session, prompt, promptSubmitOptions{})
	}()
	if !started {
		notifReleaseTurn(session)
		suppress(func() { mgr.AbandonFire() })
	}
}

// Fires a due loop wakeup for an idle session, claiming it first so a racing user prompt wins
func maybeFireTUILoopTick(ctx context.Context, sid string, session *tuigateway.Session) {
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
	tuigateway.Emit(ctx, "message.start", sid, nil)
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

// Leading line of a value clipped to limit, prefixed with a newline, empty for a blank value
func kbFirstLine(value any, limit int) string {
	lines := pystr.Splitlines(strings.TrimSpace(pyrepr.Str(value)), false)
	if len(lines) == 0 {
		return ""
	}
	return "\n" + pystr.Slice(lines[0], 0, limit)
}

// Payload slot of a kanban event, nil when the payload is not a dict
func kbPayload(ev *kanban.Event, key string) any {
	switch p := ev.Payload.(type) {
	case map[string]any:
		return p[key]
	}
	return nil
}

// Suffix and glyph of each audible kanban event kind
var kanbanEventFormatters = map[string]struct {
	glyph string
	fmt   func(task *kanban.Task, ev *kanban.Event, title string) string
}{
	"completed": {"✔", func(task *kanban.Task, ev *kanban.Event, title string) string {
		handoff := ""
		if summary := kbPayload(ev, "summary"); truthy(summary) {
			handoff = kbFirstLine(summary, 200)
		} else if task != nil && task.Result != nil && *task.Result != "" {
			handoff = kbFirstLine(*task.Result, 160)
		}
		return " done — " + title + handoff
	}},
	"blocked": {"⏸", func(_ *kanban.Task, ev *kanban.Event, _ string) string {
		if reason := kbPayload(ev, "reason"); truthy(reason) {
			return " blocked: " + pystr.Slice(pyrepr.Str(reason), 0, 160)
		}
		return " blocked"
	}},
	"gave_up": {"✖", func(_ *kanban.Task, ev *kanban.Event, _ string) string {
		out := " gave up after repeated spawn failures"
		if failure := kbPayload(ev, "error"); truthy(failure) {
			out += "\n" + pystr.Slice(pyrepr.Str(failure), 0, 200)
		}
		return out
	}},
	"crashed": {"✖", func(*kanban.Task, *kanban.Event, string) string {
		return " worker crashed (pid gone); dispatcher will retry"
	}},
	"timed_out": {"⏱", func(_ *kanban.Task, ev *kanban.Event, _ string) string {
		limit := 0
		if raw := kbPayload(ev, "limit_seconds"); truthy(raw) {
			if value, err := pyFloat(raw); err == nil {
				limit = int(value)
			}
		}
		return fmt.Sprintf(" timed out (max_runtime=%ds); will retry", limit)
	}},
	"status": {"🔄", func(_ *kanban.Task, ev *kanban.Event, _ string) string {
		return " → " + strOr(kbPayload(ev, "status"))
	}},
}

// One line notification text of a kanban event, empty for silent kinds
func formatKanbanEventText(sub map[string]any, task *kanban.Task, ev *kanban.Event, boardSlug string) string {
	entry, audible := kanbanEventFormatters[ev.Kind]
	if !audible {
		return ""
	}
	taskID := strOr(sub["task_id"])
	title := taskID
	if task != nil && task.Title != "" {
		title = task.Title
	}
	title = pystr.Slice(title, 0, 120)
	prefix := entry.glyph + " "
	if boardSlug != "" {
		prefix += "[" + boardSlug + "] "
	}
	if task != nil && task.Assignee != nil && *task.Assignee != "" {
		prefix += "@" + *task.Assignee + " "
	}
	return prefix + "Kanban " + taskID + entry.fmt(task, ev, title)
}

// Whether a kanban event is infrastructure attention rather than an explicit owner decision
func kanbanEventIsDiagnostic(ev *kanban.Event) bool {
	switch ev.Kind {
	case "crashed", "timed_out", "gave_up":
		return true
	case "blocked", "block_loop_detected":
		return !pyrepr.Equal(kbPayload(ev, "kind"), "needs_input")
	case "status":
		status := kbPayload(ev, "status")
		return pyrepr.Equal(status, "blocked") || pyrepr.Equal(status, "triage")
	}
	return false
}

// Slug and resolved db identity of a board, several slugs sharing one pinned db
func kbBoardKey(ctx context.Context, meta map[string]any) (string, string) {
	slug := strOr(meta["slug"])
	if slug == "" {
		slug = kanban.DefaultBoard
	}
	resolved := func() (resolved string) {
		defer func() {
			if recover() != nil {
				resolved = "slug:" + slug
			}
		}()
		if dbPath := strOr(meta["db_path"]); dbPath != "" {
			return resolvePath(pypath.Expanduser(dbPath))
		}
		path, err := kanban.KanbanDBPath(ctx, &slug)
		if err != nil {
			return "slug:" + slug
		}
		return resolvePath(path)
	}()
	return slug, resolved
}

// Path.resolve, symlinks followed where they exist
func resolvePath(p string) string {
	abs := absPath(p)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// Claims and formats the unseen events of this session's subscriptions on one board
func kbPollBoard(ctx context.Context, slug, sessionKey string) []any {
	platform := "tui"
	if count, err := kanban.CountNotifySubs(ctx, "", &slug, kanban.NotifyOwnerScope{}, &platform, &sessionKey, nil); err == nil && count == 0 {
		return nil
	}
	conn, err := kanban.Connect(ctx, "", &slug)
	if err != nil {
		return nil
	}
	defer conn.Close()
	subs, err := kanban.ListNotifySubs(ctx, conn, nil, kanban.NotifyOwnerScope{})
	if err != nil {
		return nil
	}
	var texts []any
	for _, row := range subs {
		sub := objectMap(row)
		if strings.ToLower(strOr(sub["platform"])) != "tui" || sub["chat_id"] != sessionKey {
			continue
		}
		taskID, subPlatform, chatID := strOr(sub["task_id"]), strOr(sub["platform"]), strOr(sub["chat_id"])
		threadID := strOr(sub["thread_id"])
		_, _, events, err := kanban.ClaimUnseenEventsForSub(ctx, conn, taskID, subPlatform, chatID, &threadID, kanbanNotifyKinds)
		if err != nil {
			panic(err)
		}
		if len(events) == 0 {
			continue
		}
		task, err := kanban.GetTask(ctx, conn, taskID)
		if err != nil {
			panic(err)
		}
		for _, ev := range events {
			if text := formatKanbanEventText(sub, task, ev, slug); text != "" {
				if kanbanEventIsDiagnostic(ev) {
					texts = append(texts, config.DiagnosticText(text))
				} else {
					texts = append(texts, text)
				}
			}
		}
		if task != nil && task.Status == "archived" {
			suppress(func() { _, _ = kanban.RemoveNotifySub(ctx, conn, taskID, subPlatform, chatID, &threadID) })
		}
	}
	return texts
}

// Plain text of a buffered kanban notice and whether it is a diagnostic one
func splitDiagnostic(text any) (string, bool) {
	if diagnostic, ok := text.(config.DiagnosticText); ok {
		return string(diagnostic), true
	}
	return strOr(text), false
}

// Claims unseen terminal kanban events of this session's TUI subscriptions on every board
func collectKanbanNotifications(ctx context.Context, session *tuigateway.Session) []any {
	sessionKey := strOr(session.Get("session_key"))
	if sessionKey == "" || truthy(session.Get("_finalized")) {
		return nil
	}
	boards, err := kanban.ListBoards(false)
	if err != nil {
		def := kanban.DefaultBoard
		meta, err := kanban.ReadBoardMetadata(&def)
		if err != nil {
			return nil
		}
		boards = []*pyjson.Object{meta}
	}
	unique, order := map[string]string{}, []string{}
	for _, meta := range boards {
		slug, resolved := kbBoardKey(ctx, objectMap(meta))
		if _, seen := unique[resolved]; !seen {
			unique[resolved] = slug
			order = append(order, resolved)
		}
	}
	var texts []any
	for _, resolved := range order {
		texts = append(texts, kbPollBoard(ctx, unique[resolved], sessionKey)...)
	}
	return texts
}

// One kanban poll under the session profile scope
func notifPollKanban(ctx context.Context, sid string, session *tuigateway.Session) {
	notifPollKanbanScoped(sessionProfileRuntimeScope(ctx, session, true), sid, session)
}

// Emits new kanban texts, buffers them and runs the buffered batch as a turn when idle
func notifPollKanbanScoped(ctx context.Context, sid string, session *tuigateway.Session) {
	var texts []any
	func() {
		defer func() {
			if r := recover(); r != nil {
				notifLogFailure("kanban notification poll failed", r)
				texts = nil
			}
		}()
		texts = collectKanbanNotifications(ctx, session)
	}()
	for _, tagged := range texts {
		text, diagnostic := splitDiagnostic(tagged)
		_, _ = config.RenderNotification(ctx, func() error {
			tuigateway.Emit(ctx, "status.update", sid, map[string]any{"kind": "process", "text": text})
			return nil
		}, "tui", diagnostic, nil)
	}
	if len(texts) > 0 {
		pending, _ := session.SetDefault("_kanban_pending", []any{}).([]any)
		session.Set("_kanban_pending", append(pending, texts...))
	}
	if pending, _ := session.Get("_kanban_pending").([]any); len(pending) == 0 || !notifClaimTurn(session) {
		return
	}
	unlock := lockHistory(session)
	pending, _ := session.Get("_kanban_pending").([]any)
	split := !config.WarningNotificationsEnabled(ctx, "tui", nil)
	_, firstDiagnostic := splitDiagnostic(pending[0])
	diagnostic := split && firstDiagnostic
	var batch []string
	var rest []any
	for _, tagged := range pending {
		text, isDiagnostic := splitDiagnostic(tagged)
		if !split || isDiagnostic == diagnostic {
			batch = append(batch, text)
		} else {
			rest = append(rest, tagged)
		}
	}
	session.Set("_kanban_pending", rest)
	unlock()
	o := promptSubmitOptions{}
	if diagnostic {
		o.displayMetadata = map[string]any{"notification_category": "diagnostic"}
	}
	suppress(func() {
		notifSubmit(ctx, fmt.Sprintf("__notif__%d", time.Now().UnixMilli()), sid, session, strings.Join(batch, "\n"),
			"kanban notification dispatch failed", o)
	})
}

// Poller of one session under its profile scope
func notificationPollerLoop(ctx context.Context, stop *pythread.Event, sid string, session *tuigateway.Session) {
	notificationPollerScopedLoop(sessionProfileRuntimeScope(ctx, session, true), stop, sid, session)
}

// Drains the completion queue for this session, claims parked events and polls loop, heartbeat and kanban wakeups
func notificationPollerScopedLoop(ctx context.Context, stop *pythread.Event, sid string, session *tuigateway.Session) {
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
				fire func(context.Context, string, *tuigateway.Session)
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
		if now-lastKanbanPoll >= kanbanPollSeconds {
			lastKanbanPoll = now
			notifPollKanban(ctx, sid, session)
		}
		parked := tuigateway.ClaimParked(func(evt map[string]any) bool { return sessionOwnsNotificationEvent(ctx, sid, session, evt) })
		func() {
			var ready []map[string]any
			defer func() {
				if r := recover(); r != nil {
					notifLogFailure("notification dispatch failed", r)
				}
				tools.ReleaseEvents(ready)
				tuigateway.UnholdParked(parked)
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

// Process output and tab close sinks plus the desktop tool emitter, wired once per process
var desktopSinks struct {
	mu    sync.Mutex
	wired bool
}

// Live session id whose key owns a background process, empty when none
func processOwnerSID(s *tools.ProcessSession) string {
	if s == nil || s.SessionKey == "" {
		return ""
	}
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	for _, live := range tuigateway.LiveSessions() {
		if strOr(live.Session.Get("session_key")) == s.SessionKey {
			return live.SID
		}
	}
	return ""
}

// Wires process registry and desktop tool sinks to renderer events once
func wireDesktopSinks(ctx context.Context) {
	registry := tools.DefaultProcessRegistry()
	if registry.OnOutput == nil {
		registry.OnOutput = func(s *tools.ProcessSession, chunk string) {
			tuigateway.Emit(ctx, "agent.terminal.output", processOwnerSID(s), map[string]any{"process_id": s.ID, "chunk": chunk})
		}
	}
	if registry.OnClose == nil {
		registry.OnClose = func(s *tools.ProcessSession, pid string) error {
			tuigateway.Emit(ctx, "terminal.close", processOwnerSID(s), map[string]any{"process_id": pid})
			return nil
		}
	}
	desktopSinks.mu.Lock()
	defer desktopSinks.mu.Unlock()
	if !desktopSinks.wired {
		suppress(func() {
			tools.SetDesktopEmitter(func(_ *tools.Env, sid, event string, payload map[string]any) error {
				tuigateway.Emit(ctx, event, sid, payload)
				return nil
			})
			desktopSinks.wired = true
		})
	}
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
func startNotificationPoller(ctx context.Context, sid string, session *tuigateway.Session) *pythread.Event {
	wireDesktopSinks(ctx)
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
