package tools

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
)

// Nudge for a background process nobody will hear of, almost always a bounded job missing its flag
const silentHint = "background=true without notify=true means this process runs SILENTLY — you will not be told when it exits. If this is a " +
	"bounded task (test suite, build, CI poller, deploy, anything with a defined end), you almost certainly wanted notify=true so the system " +
	"pings you on exit. Re-launch with notify=true, or call process_manage(action='poll') / process_manage(action='wait') yourself to learn " +
	"the outcome. Only ignore this hint for genuine long-lived processes that never exit (servers, watchers, daemons)."

// Hint for a homebrewed CI poller shape known to fail silently
const ciPollerHint = "This looks like a homebrewed CI poller built from `gh pr view --json statusCheckRollup` and/or `gh pr checks | jq`. That " +
	"shape fails silently again and again in real CI loops — stdout buffering kills output capture, jq null-key edge cases silently exit the " +
	"loop, conclusion-vs-status field confusion exits early with bogus all-green verdicts, TTY-only summary banners never appear when piped. " +
	"Use one of the two canonical shapes instead: the exit-code-driven `gh pr checks $PR >/dev/null` (rc 0 = green, 8 = pending, else fail) " +
	"for exit-on-first-fail behavior, or the column-2 awk-on-tabs poller (`awk -F\"\\t\" \"$2==\\\"pending\\\"\"`) for sharded matrices. Or " +
	"run `gh pr checks $PR --watch --fail-fast` to let gh itself wait and exit non-zero on the first failing check, its exit codes listed by " +
	"`gh pr checks --help`. If you must roll a custom loop with rich structured output, write each tick to a known file " +
	"(`tee -a $TMPDIR/ci.log`) and rely on `process_manage(action='log')` to read THAT file — do not rely on background-process stdout " +
	"capture for line-buffered shell loops."

// Note for a foreground call whose only backgrounding was a trailing &
const autoNote = "Ran in the background (the trailing & was converted to background=true; Icebath tracks the process). Do NOT re-run it. Use " +
	"process_manage (poll/log/wait/kill, session_id above) to check it."

// Note when the session can't be told of anything once the turn ends
const unsupportedNote = "notify_on_complete / watch_patterns are not available in this session — it cannot receive an async completion after " +
	"the turn ends, or background notices are off in its settings. The process is running in the background; retrieve its result with " +
	"process_manage(action='poll') or process_manage(action='wait')."

// Starts the command as a tracked background process and says how it will be heard of
func (r *registrant) background(ctx context.Context, call *tools.Call, lease *terminal.Lease, req request, command string, notify bool,
	watch []string, note string, ptyOff, auto bool) string {
	cfg := call.Env.Config
	send := deliver(call.Env)
	out := result{Output: "Background process started", ExitCode: ptr(0), Persist: req.persist, Approval: note}
	asked := notify || len(watch) > 0
	if asked && (send == nil || !r.host.Jobs.Allowed(cfg)) {
		out.Notify, out.NotifyUnsupported = ptr(false), unsupportedNote
		slog.Info("background proc async delivery unsupported on this session, notify flags disabled")
		notify, watch = false, nil
	}
	if notify && len(watch) > 0 {
		out.WatchIgnored = "watch_patterns ignored because notify_on_complete=True; these two flags produce duplicate notifications when combined"
		slog.Warn("background proc flag conflict", "note", out.WatchIgnored)
		watch = nil
	}
	job, err := r.host.Jobs.Spawn(ctx, terminal.Launch{
		Config:   cfg,
		Session:  call.SessionID,
		Command:  command,
		Cwd:      lease.Folder(req.workdir),
		Env:      lease.Env,
		Pty:      req.pty && !ptyOff,
		Persist:  req.persist,
		Deliver:  send,
		Notify:   notify,
		Patterns: watch,
		Vars:     tools.SessionVars(call),
		Task:     cmp.Or(hooks.TurnOf(ctx).Task, call.SessionID),
		Call:     call.ToolCallID,
	})
	if err != nil {
		return failure(terminal.RedactError(fmt.Sprintf("Failed to start background process: %s", err)), -1, "")
	}
	out.SessionID, out.Pid = job.ID, job.Pid
	if ptyOff {
		out.PtyNote = ptyOffNote
	}
	switch {
	case auto:
		out.AutoBackgrounded = autoNote
	case !asked:
		out.Hint = silentHint
	}
	if ciPoller(command) {
		if out.Hint != "" {
			out.Hint += "\n\n" + ciPollerHint
		} else {
			out.Hint = ciPollerHint
		}
	}
	switch {
	case notify:
		out.Notify = ptr(true)
		if req.heartbeat != 0 {
			out.HeartbeatSeconds = r.host.Jobs.Heartbeat(job, req.heartbeat)
		}
	case req.heartbeat != 0:
		out.HeartbeatIgnored = "heartbeat needs notify=true delivery, which this session cannot receive"
	}
	if len(watch) > 0 {
		out.WatchPatterns = watch
	}
	return encode(out)
}
