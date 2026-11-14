package terminal

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/env"
	"github.com/nickheyer/icebath/internal/tool/process"
)

const silentBackgroundHint = "background=true without notify_on_complete=true means this process runs SILENTLY — you " +
	"will not be told when it exits. If this is a bounded task (test suite, build, CI poller, " +
	"deploy, anything with a defined end), you almost certainly wanted notify_on_complete=true " +
	"so the system pings you on exit. Re-launch with notify_on_complete=true, or call " +
	"process(action='poll') / process(action='wait') yourself to learn the outcome. Only " +
	"ignore this hint for genuine long-lived processes that never exit (servers, watchers, " +
	"daemons)."

const homebrewCIPollerHint = "This looks like a homebrewed CI poller built from `gh pr view --json statusCheckRollup` " +
	"and/or `gh pr checks | jq`. That shape has burned us repeatedly in icebath dev work " +
	"(PRs #31329, #31448, #31695, #31709, #31745, #32264, #33131) — stdout buffering kills " +
	"output capture, jq null-key edge cases silently exit the loop, conclusion-vs-status field " +
	"confusion exits early with bogus all-green verdicts, TTY-only summary banners never " +
	"appear when piped. Use the canonical snippets in the green-ci-policy skill instead: the " +
	"exit-code-driven `gh pr checks $PR >/dev/null` (rc 0 = green, 8 = pending, else fail) for " +
	"exit-on-first-fail behavior, or the column-2 awk-on-tabs poller (`awk -F\"\\t\" " +
	"\"$2==\\\"pending\\\"\"`) for sharded matrices. Load " +
	"skill_view(name='github/icebath-dev', file_path='references/green-ci-policy.md') for " +
	"the verbatim snippets. If you must roll a custom loop with rich structured output, write " +
	"each tick to a known file (`tee -a $TMPDIR/ci.log`) and rely on `process(action='log')` to " +
	"read THAT file — do not rely on background-process stdout capture for line-buffered shell " +
	"loops."

const autoBackgroundNote = "Ran in the background (the trailing & was converted to background=true; icebath tracks the " +
	"process). Do NOT re-run it. Use process_manage (poll/log/wait/kill, session_id above) to check it."

const asyncUnsupportedNote = "notify_on_complete / watch_patterns are not available in this session — it cannot receive " +
	"an async completion after the turn ends (a one-shot runner such as `icebath -z`, a /bg " +
	"background task, a cron job, a Kanban worker, or a stateless HTTP endpoint). The process is running in the " +
	"background; retrieve its result with process(action='poll') or process(action='wait')."

const subagentNotifyNote = "You are a subagent: this process's completion notice will NOT reach your parent, and the process is killed when " +
	"you finish. Before you finish, either wait for it (process_manage wait), kill it, or hand it to your parent with " +
	"process_manage(action='handoff', session_id=..., data='<purpose>') so the parent receives its completion. For CI " +
	"watchers prefer returning the fact (PR number, SHA) and letting the parent watch."

const yieldedNote = "The user sent a message while this command was running, so it was moved to the " +
	"background WITHOUT being killed and is still running. You will be notified when it " +
	"exits (notify_on_complete). Read the user's message and respond to it now; use " +
	"process(action='poll'|'wait'|'log', session_id=...) to check on this command."

const flagConflictNote = "watch_patterns ignored because notify_on_complete=True; " +
	"these two flags produce duplicate notifications when combined"

const watcherInterval = 5

func looksLikeHomebrewCIPoller(command string) bool {
	hasGH := strings.Contains(command, "gh pr view") || strings.Contains(command, "gh pr checks")
	hasJQ := strings.Contains(command, " jq ") || strings.Contains(command, "| jq") || strings.Contains(command, "$(jq")
	return strings.Contains(command, "statusCheckRollup") || (hasGH && hasJQ)
}

func sessionPlatform(o tool.Origin) string {
	switch o.Platform {
	case "", "cli", "tui", "desktop", "acp", "cron", "subagent":
		return ""
	}
	return o.Platform
}

func gatewayRoute(c *tool.Call) process.Route {
	platform := sessionPlatform(c.Origin)
	if platform == "" {
		return process.Route{}
	}
	return process.Route{Platform: platform, ChatID: c.Origin.ChatID, UserID: c.Origin.UserID, UserName: c.Origin.UserName,
		ThreadID: c.Origin.ThreadID, MessageID: c.Origin.MessageID}
}

func (t *Terminal) asyncDelivery(c *tool.Call) bool {
	return c.Origin.KanbanTask == "" && !t.o.Role.Worker() && c.Origin.AsyncDelivery
}

func (t *Terminal) registerCompletionWatcher(ctx context.Context, c *tool.Call, s *process.Session, route process.Route, sessionKey string) {
	w := process.Watcher{SessionID: s.ID(), CheckInterval: watcherInterval, SessionKey: sessionKey, Route: route, Notify: true,
		ParentSessionID: c.SessionID}
	if t.o.Watchers.ArmProcessWatcher(ctx, w) {
		return
	}
	t.o.Processes.AddWatcher(w)
}

type backgroundSpawn struct {
	command          string
	env              tool.Environment
	kind             string
	eff              string
	taskID           string
	sessionKey       string
	workdir          string
	cwd              string
	mountedHost      string
	pty              bool
	notify           bool
	watch            []string
	approvalNote     string
	ptyNote          string
	completionChars  int
	heartbeat        int
	persist          bool
	autoBackgrounded bool
}

func (t *Terminal) spawnBackground(ctx context.Context, c *tool.Call, b backgroundSpawn) object {
	effectiveCwd := t.resolveCommandCwd(ctx, b.workdir, b.cwd, b.sessionKey, b.kind, b.mountedHost, b.env)
	owner := b.taskID
	if owner == "" {
		owner = b.eff
	}
	notify, watch := b.notify, b.watch
	requested := notify || len(watch) > 0
	unsupported := false
	var route process.Route
	if requested {
		if t.asyncDelivery(c) {
			route = gatewayRoute(c)
		} else {
			unsupported = true
			notify, watch = false, nil
		}
	}
	conflict := ""
	if notify && len(watch) > 0 {
		watch, conflict = nil, flagConflictNote
	}
	opts := process.SpawnOptions{Command: b.command, Cwd: effectiveCwd, TaskID: b.eff, OwnerTaskID: owner, SessionKey: b.sessionKey,
		ParentSessionID: c.SessionID, PersistOnRelease: b.persist, Notify: notify, WatchPatterns: watch, Route: route}
	if notify {
		opts.OutputChars = b.completionChars
		if route.Platform != "" {
			opts.WatcherInterval = watcherInterval
		}
	}
	var s *process.Session
	var err error
	if b.kind == "local" {
		opts.PTY, opts.RestoreLoginPath = b.pty, true
		s, err = t.o.Processes.SpawnLocal(ctx, opts)
	} else {
		s, err = t.o.Processes.SpawnEnv(ctx, b.env, opts)
	}
	if err != nil {
		return object{{"output", ""}, {"exit_code", -1}, {"error", t.redactError(ctx, fmt.Sprintf("Failed to start background process: %v", err))}}
	}
	o := object{{"output", "Background process started"}, {"session_id", s.ID()}, {"pid", s.PID()}, {"exit_code", 0}, {"error", nil}}
	if b.persist {
		o = append(o, field{"persist_on_release", true})
	}
	if b.approvalNote != "" {
		o = append(o, field{"approval", b.approvalNote})
	}
	if b.ptyNote != "" {
		o = append(o, field{"pty_note", b.ptyNote})
	}
	hint := ""
	if b.autoBackgrounded {
		o = append(o, field{"auto_backgrounded", autoBackgroundNote})
	} else if !b.notify && len(b.watch) == 0 {
		hint = silentBackgroundHint
	}
	if b.command != "" && looksLikeHomebrewCIPoller(b.command) {
		if hint != "" {
			hint += "\n\n" + homebrewCIPollerHint
		} else {
			hint = homebrewCIPollerHint
		}
	}
	if hint != "" {
		o = append(o, field{"hint", hint})
	}
	if unsupported {
		o = o.set("notify_on_complete", false)
		o = append(o, field{"notify_unsupported", asyncUnsupportedNote})
		slog.Info(fmt.Sprintf("background proc %s: async delivery unsupported on this session; notify_on_complete/watch_patterns disabled", s.ID()))
	}
	if conflict != "" {
		slog.Warn(fmt.Sprintf("background proc %s: %s", s.ID(), conflict))
		o = append(o, field{"watch_patterns_ignored", conflict})
	}
	if notify {
		o = o.set("notify_on_complete", true)
		if route.Platform != "" {
			t.registerCompletionWatcher(ctx, c, s, route, b.sessionKey)
		}
		if c.Origin.DelegatedChild {
			o = o.set("notify_on_complete", false)
			o = append(o, field{"subagent_note", subagentNotifyNote})
		} else if b.heartbeat != 0 {
			o = append(o, field{"heartbeat_seconds", t.o.Processes.ArmHeartbeat(s, b.heartbeat)})
		}
	} else if b.heartbeat != 0 {
		o = append(o, field{"heartbeat_ignored", "heartbeat needs notify=true delivery, which this session cannot receive"})
	}
	if len(watch) > 0 {
		o = append(o, field{"watch_patterns", watch})
	}
	return o
}

func (t *Terminal) yieldHandler(ctx context.Context, c *tool.Call, command, cwd, eff, sessionKey string) func(tool.Process, string) (tool.ExecResult, bool) {
	return func(p tool.Process, partial string) (tool.ExecResult, bool) {
		owner := c.TaskID
		if owner == "" {
			owner = eff
		}
		async := t.asyncDelivery(c)
		opts := process.SpawnOptions{Command: command, Cwd: cwd, TaskID: eff, OwnerTaskID: owner, SessionKey: sessionKey,
			ParentSessionID: c.SessionID, Notify: async}
		var route process.Route
		if async {
			route = gatewayRoute(c)
			opts.Route = route
			if route.Platform != "" {
				opts.WatcherInterval = watcherInterval
			}
		}
		s, err := t.o.Processes.Adopt(ctx, p, opts, partial)
		if err != nil {
			slog.Warn(fmt.Sprintf("foreground command could not be yielded to background: %v", err))
			return tool.ExecResult{}, false
		}
		if async && route.Platform != "" {
			t.registerCompletionWatcher(ctx, c, s, route, sessionKey)
		}
		slog.Info(fmt.Sprintf("foreground command yielded to background as %s (pid %d)", s.ID(), s.PID()))
		if r, ok := p.(env.Releaser); ok {
			r.Release()
		}
		return tool.ExecResult{Output: partial, YieldedSession: s.ID(), YieldedPID: s.PID()}, true
	}
}

func (t *Terminal) withPromotedNote(o object, requested float64) object {
	for _, f := range o {
		if f.key == "error" && truthy(f.value) {
			return o
		}
	}
	notify := false
	for _, f := range o {
		if f.key == "notify_on_complete" {
			notify = truthy(f.value)
		}
	}
	text := fmt.Sprintf("Requested foreground timeout %ss exceeds the %ds cap, so this command was started as a ", formatNumber(requested), t.foregroundMax)
	if notify {
		text += "tracked background process with notify_on_complete=true instead of being refused. Do NOT re-run it. " +
			"Its completion (exit code + output tail) arrives as a notification; poll with " +
			"process(action=\"poll\", session_id=...) if you need it sooner."
	} else {
		text += "tracked background process instead of being refused. Do NOT re-run it. This session cannot receive " +
			"completion notifications, so poll it with process(action=\"poll\", session_id=...) until it exits."
	}
	return append(o, field{"promoted_from_foreground", text})
}
