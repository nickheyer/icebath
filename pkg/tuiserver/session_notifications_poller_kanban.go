//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Binds the kanban wakeups of the session notification poller
func init() { kanbanNotificationPoll = notifPollKanban }

// Kanban event kinds a TUI subscription claims
var kanbanNotifyKinds = []string{"completed", "blocked", "gave_up", "crashed", "timed_out", "status", "archived", "unblocked"}

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
func collectKanbanNotifications(ctx context.Context, session *tuihost.Session) []any {
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
func notifPollKanban(ctx context.Context, sid string, session *tuihost.Session) {
	notifPollKanbanScoped(sessionProfileRuntimeScope(ctx, session, true), sid, session)
}

// Emits new kanban texts, buffers them and runs the buffered batch as a turn when idle
func notifPollKanbanScoped(ctx context.Context, sid string, session *tuihost.Session) {
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
			tuihost.Emit(ctx, "status.update", sid, map[string]any{"kind": "process", "text": text})
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
