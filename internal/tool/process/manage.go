package process

import (
	"context"
	"io"
	"log/slog"
	"math"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/redact"
)

const (
	SourceProcessKill         = "process.kill"
	SourceKillAll             = "kill_all"
	completionWatcherInterval = 5
	readerFinalDrainWait      = time.Second
	minPrefixChars            = 4
	defaultWaitSeconds        = 180
	previewChars              = 1000
	listChars                 = 200
	defaultOneshotWait        = 600.0
	pendingPollInterval       = time.Second
	envKillTimeout            = 5 * time.Second
)

var lifecycleKillSources = map[string]bool{SourceKillAll: true, "gateway_turn_timeout": true, "agent_close": true}

type Caller struct {
	SessionID  string
	SessionKey string
	TaskID     string
	Yield      *tool.Yield
}

func CallerOf(c *tool.Call) Caller {
	if c == nil {
		return Caller{}
	}
	return Caller{SessionID: c.SessionID, SessionKey: c.SessionKey, TaskID: c.TaskID, Yield: c.Yield}
}

type KillOptions struct {
	Source  string
	Consume bool
}

type KillAllOptions struct {
	OwnerTaskID *string
	Exclude     []string
	Source      string
	Consume     bool
}

type ListOptions struct {
	TaskID          string
	SessionKey      string
	IncludeRetained bool
}

type PendingResult struct {
	Waited    []string
	Completed []string
	TimedOut  []string
}

func pidValue(pid int) *int {
	if pid == 0 {
		return nil
	}
	return intPtr(pid)
}

func (r *Registry) IsConsumed(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.consumed[id]
}

func (r *Registry) markConsumed(id string) {
	r.mu.Lock()
	r.consumed[id] = true
	r.mu.Unlock()
}

func (r *Registry) live(id string) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.running.get(id); ok {
		return s, true
	}
	return r.finished.get(id)
}

func (r *Registry) Get(ctx context.Context, caller Caller, id string) *Session {
	if id == "" {
		return nil
	}
	s, ok := r.live(id)
	if !ok {
		for _, rs := range r.loadCompletedResults(ctx, caller.SessionID, id) {
			if rs.id == id {
				s, ok = rs, true
				break
			}
		}
	}
	if !ok {
		s = r.resolvePrefix(ctx, caller.SessionID, id)
	}
	return r.refreshDetached(ctx, s)
}

func (r *Registry) resolvePrefix(ctx context.Context, owner, id string) *Session {
	query := strings.TrimSpace(id)
	if query == "" {
		return nil
	}
	if !strings.HasPrefix(query, "proc_") {
		query = "proc_" + query
	}
	if utf8.RuneCountInString(query)-len("proc_") < minPrefixChars {
		return nil
	}
	matches := map[string]*Session{}
	for _, s := range r.loadCompletedResults(ctx, owner, query) {
		matches[s.id] = s
	}
	r.mu.Lock()
	for _, s := range append(r.running.list(), r.finished.list()...) {
		if strings.HasPrefix(s.id, query) {
			matches[s.id] = s
		}
	}
	r.mu.Unlock()
	if len(matches) != 1 {
		return nil
	}
	for _, s := range matches {
		return s
	}
	return nil
}

func (s *Session) readerFinalizing() bool {
	return s.finishRequested() && !s.isDone() && s.readerAlive()
}

func (r *Registry) reconcile(ctx context.Context, s *Session) {
	if s == nil || s.process == nil || s.isExited() {
		return
	}
	code, exited := s.process.Exited()
	if !exited {
		return
	}
	if runtime.GOOS != "windows" && s.selectable.Load() && s.readerAlive() {
		s.requestFinish()
		timer := time.NewTimer(readerFinalDrainWait)
		select {
		case <-s.done:
			timer.Stop()
			return
		case <-timer.C:
		}
		s.mu.Lock()
		s.markExitedLocked(intPtr(code), reasonExited, "")
		s.mu.Unlock()
		slog.Info("Reconciled session: direct child exited; "+
			"reader will publish the owned completion after its final drain.", "process", s.id, "code", code)
		return
	}
	s.mu.Lock()
	s.markExitedLocked(intPtr(code), reasonExited, "")
	s.mu.Unlock()
	slog.Info("Reconciled session: direct child exited but reader "+
		"was still blocked (orphaned pipe). Flipped to exited.", "process", s.id, "code", code)
	r.moveToFinished(ctx, s, false)
}

func (r *Registry) IsWaiting(ctx context.Context, id string) bool {
	if id == "" {
		return false
	}
	s, ok := r.live(id)
	if !ok {
		return false
	}
	if s = r.refreshDetached(ctx, s); s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.exited && !(len(s.watchPatterns) > 0 && !s.watchDisabled && s.watchHits > 0)
}

func (r *Registry) oneshotWait(ctx context.Context) time.Duration {
	c, err := r.cfg.LoadReadonly(ctx)
	if err != nil {
		return time.Duration(defaultOneshotWait * float64(time.Second))
	}
	seconds := c.Terminal.OneshotCompletionWaitSeconds
	if math.IsNaN(seconds) || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func idList(sessions []*Session) string {
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.id)
	}
	return strings.Join(ids, ", ")
}

func (r *Registry) WaitForPending(ctx context.Context, timeout *time.Duration) PendingResult {
	limit := time.Duration(0)
	if timeout != nil {
		limit = *timeout
	} else {
		limit = r.oneshotWait(ctx)
	}
	var result PendingResult
	var pending []*Session
	r.mu.Lock()
	for _, s := range append(r.running.list(), r.finished.list()...) {
		s.mu.Lock()
		notify := s.notify
		s.mu.Unlock()
		if notify && !s.isDone() {
			pending = append(pending, s)
		}
	}
	r.mu.Unlock()
	if len(pending) == 0 || limit <= 0 {
		return result
	}
	for _, s := range pending {
		result.Waited = append(result.Waited, s.id)
	}
	slog.Info("One-shot exit lingering for notify_on_complete background process(es)",
		"bound", limit, "count", len(pending), "processes", idList(pending))
	deadline := time.Now().Add(max(limit, 0))
	interrupted := false
	for _, s := range pending {
		for !s.isDone() {
			if interrupted || ctx.Err() != nil {
				interrupted = true
				break
			}
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			r.reconcile(ctx, s)
			if r.refreshDetached(ctx, s) == nil || s.isDone() {
				break
			}
			timer := time.NewTimer(min(remaining, pendingPollInterval))
			select {
			case <-s.done:
			case <-timer.C:
			case <-ctx.Done():
			}
			timer.Stop()
		}
		if s.isDone() {
			result.Completed = append(result.Completed, s.id)
		} else {
			result.TimedOut = append(result.TimedOut, s.id)
		}
	}
	if len(result.TimedOut) > 0 {
		slog.Warn("One-shot exit linger timed out with background process(es) still running; "+
			"they may be killed when this process exits.",
			"after", limit, "count", len(result.TimedOut), "processes", strings.Join(result.TimedOut, ", "))
	}
	return result
}

func statusText(exited bool) string {
	if exited {
		return "exited"
	}
	return "running"
}

func statusHead(s *Session, exited bool) *Reply {
	return newReply("session_id", s.id, "command", s.command, "status", statusText(exited))
}

func exitFieldsLocked(s *Session) *Reply {
	return newReply("exit_code", s.exitCode, "completion_reason", s.completionReason,
		"termination_source", s.terminationSource)
}

func completionFieldsLocked(s *Session) *Reply {
	output, cut := s.completionOutputLocked()
	out := newReply("output", output)
	if cut > 0 {
		out.set("output_cut", cut)
	}
	return out
}

func exitSnapshotLocked(s *Session, status string) *Reply {
	out := newReply("status", status, "command", s.command)
	out.merge(exitFieldsLocked(s))
	out.merge(completionFieldsLocked(s))
	return out
}

func uptime(s *Session) int { return int(now() - s.startedAt) }

func (r *Registry) Poll(ctx context.Context, caller Caller, id string) *Reply {
	s := r.Get(ctx, caller, id)
	if s == nil || uncollectedGone(s) {
		return notFound(id)
	}
	r.reconcile(ctx, s)
	s.mu.Lock()
	preview := s.outputTailLocked(previewChars)
	exited, finalizing := s.exited, s.readerFinalizing()
	pid := s.pid
	var fields *Reply
	if exited {
		fields = exitFieldsLocked(s)
	}
	detached := s.detached && s.outputLog == ""
	s.mu.Unlock()
	out := statusHead(s, exited)
	out.set("pid", pidValue(pid))
	out.set("uptime_seconds", uptime(s))
	out.set("output_preview", preview)
	if exited {
		out.merge(fields)
		if !finalizing {
			r.mu.Lock()
			r.pollObserved[id] = true
			r.mu.Unlock()
		}
	}
	if detached {
		out.set("detached", true)
		out.set("note", "Process recovered after restart -- output history unavailable")
	}
	return out
}

func sliceBound(i, n int) int {
	if i < 0 {
		i += n
		if i < 0 {
			return 0
		}
	}
	return min(i, n)
}

func (r *Registry) ReadLog(ctx context.Context, caller Caller, id string, offset *int, limit int) *Reply {
	s := r.Get(ctx, caller, id)
	if s == nil {
		return notFound(id)
	}
	s.mu.Lock()
	full := tool.StripANSI(s.out.String())
	exited, finalizing := s.exited, s.readerFinalizing()
	s.mu.Unlock()
	lines := outputLines(full)
	total := len(lines)
	var selected []string
	observed := false
	if offset == nil && limit > 0 {
		selected = lines[max(0, total-limit):]
		observed = len(selected) > 0 || total == 0
	} else {
		off := 0
		if offset != nil {
			off = *offset
		}
		start, stop := sliceBound(off, total), sliceBound(off+limit, total)
		if start < stop {
			selected = lines[start:stop]
		}
		observed = total == 0 || len(selected) > 0 && stop == total
	}
	out := statusHead(s, exited)
	out.set("output", strings.Join(selected, "\n"))
	out.set("total_lines", total)
	out.set("showing", strconv.Itoa(len(selected))+" lines")
	if exited && observed && !finalizing {
		r.markConsumed(id)
	}
	return out
}

func spellSeconds(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return formatSeconds(v)
}

func (r *Registry) maxWaitSeconds(ctx context.Context) (int, error) {
	raw, err := r.terminal.TerminalEnv(ctx, "TERMINAL_TIMEOUT")
	if err != nil {
		return 0, err
	}
	if raw == "" {
		return defaultWaitSeconds, nil
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return defaultWaitSeconds, nil
	}
	return v, nil
}

func (r *Registry) Wait(ctx context.Context, caller Caller, id string, timeout *float64) (*Reply, error) {
	maxTimeout, err := r.maxWaitSeconds(ctx)
	if err != nil {
		return nil, err
	}
	if timeout != nil && *timeout <= 0 {
		return errorReply("timeout must be positive (got " + spellSeconds(*timeout) + ")"), nil
	}
	timeoutNote := ""
	effective := float64(maxTimeout)
	if timeout != nil {
		effective = *timeout
		if *timeout > float64(maxTimeout) {
			effective = float64(maxTimeout)
			timeoutNote = "Requested wait of " + spellSeconds(*timeout) + "s was clamped to configured limit of " +
				strconv.Itoa(maxTimeout) + "s"
		}
	}
	s := r.Get(ctx, caller, id)
	if s == nil {
		return notFound(id), nil
	}
	deadline := time.Now().Add(time.Duration(effective * float64(time.Second)))
	for time.Now().Before(deadline) {
		if s = r.refreshDetached(ctx, s); s == nil {
			return notFound(id), nil
		}
		r.reconcile(ctx, s)
		var out *Reply
		s.mu.Lock()
		settled := s.exited && !s.readerFinalizing()
		s.mu.Unlock()
		if settled {
			r.markConsumed(id)
		}
		s.mu.Lock()
		switch {
		case settled:
			out = exitSnapshotLocked(s, "exited")
		case ctx.Err() != nil:
			out = newReply("status", "interrupted", "command", s.command, "output", s.outputTailLocked(previewChars),
				"note", "User sent a new message -- wait interrupted")
		case caller.Yield.Consume():
			out = newReply("status", "interrupted", "command", s.command, "output", s.outputTailLocked(previewChars),
				"process_running", true,
				"note", "User sent a new message -- wait released; the process is still "+
					"running and you will be notified on exit. Respond to the user now.")
		}
		s.mu.Unlock()
		if out != nil {
			if timeoutNote != "" {
				out.set("timeout_note", timeoutNote)
			}
			return out, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		timer := time.NewTimer(min(time.Second, remaining))
		select {
		case <-s.done:
		case <-timer.C:
		case <-ctx.Done():
		case <-caller.Yield.Requested():
		}
		timer.Stop()
	}
	s.mu.Lock()
	out := newReply("status", "timeout", "command", s.command, "output", s.outputTailLocked(previewChars),
		"process_running", true)
	notify, started := s.notify, s.startedAt
	s.mu.Unlock()
	note := "Wait window of " + spellSeconds(effective) + "s elapsed — the process is still running. This is not an error."
	if started != 0 {
		note += " Uptime: " + strconv.Itoa(uptime(s)) + "s."
	}
	if notify {
		note += " notify_on_complete is set: you will be notified on exit — do more work instead of waiting again."
	} else {
		note += " Poll again later or use terminal(background=true, " +
			"notify_on_complete=true) next time for automatic notification."
	}
	if timeoutNote != "" {
		note = timeoutNote + ". " + note
	}
	out.set("timeout_note", note)
	return out, nil
}

func (r *Registry) stopScope(ctx context.Context, s *Session) bool {
	s.mu.Lock()
	unit := s.systemdUnit
	s.mu.Unlock()
	if unit == "" {
		return true
	}
	stopped := stopUnit(ctx, unit)
	s.mu.Lock()
	s.scopeStopPending = !stopped
	s.mu.Unlock()
	return stopped
}

func (r *Registry) Kill(ctx context.Context, caller Caller, id string, o KillOptions) *Reply {
	s := r.Get(ctx, caller, id)
	if s == nil {
		return notFound(id)
	}
	if s.isExited() {
		stopped := r.stopScope(ctx, s)
		s.mu.Lock()
		out := exitSnapshotLocked(s, "already_exited")
		unit := s.systemdUnit
		s.mu.Unlock()
		finalizing := s.readerFinalizing()
		if !stopped {
			out.set("scope_stop_failed", unit)
		}
		if o.Consume && !finalizing {
			r.markConsumed(id)
		}
		return out
	}
	early, err := r.signalKill(ctx, s, id, o.Consume)
	if err != nil {
		return errorReply(err.Error())
	}
	if early != nil {
		return early
	}
	stopped := r.stopScope(ctx, s)
	var survivors []int
	if !s.isExited() {
		survivors = r.postKillSurvivors(s)
	}
	if len(survivors) > 0 {
		alive := make([]string, 0, len(survivors))
		for _, p := range survivors {
			alive = append(alive, strconv.Itoa(p))
		}
		list := strings.Join(alive, ", ")
		slog.Warn("Kill incomplete: process(es) still alive; session kept running",
			"process", s.id, "count", len(survivors), "alive", list)
		return newReply("status", "error",
			"error", "Kill incomplete: "+strconv.Itoa(len(survivors))+" process(es) still alive ("+list+"); session running",
			"session_id", s.id, "survivors", survivors, "process_running", true)
	}
	if !stopped {
		s.mu.Lock()
		rootExited, unit := s.exited, s.systemdUnit
		s.mu.Unlock()
		slog.Warn("Kill incomplete: systemd scope did not stop", "process", s.id, "unit", unit)
		return newReply("status", "error", "error", "Kill incomplete: systemd scope "+unit+" did not stop",
			"session_id", s.id, "scope_stop_failed", unit, "process_running", !rootExited)
	}
	r.mu.Lock()
	s.mu.Lock()
	output := completionFieldsLocked(s)
	if o.Consume {
		r.consumed[id] = true
	}
	s.exited = true
	s.exitCode = intPtr(-15)
	s.completionReason = reasonKilled
	s.terminationSource = o.Source
	s.mu.Unlock()
	r.mu.Unlock()
	if !r.moveToFinished(ctx, s, false) {
		r.saveCompletedResult(ctx, s)
	}
	r.writeCheckpoint(ctx, nil)
	s.mu.Lock()
	out := newReply("status", "killed", "session_id", s.id, "completion_reason", s.completionReason,
		"termination_source", s.terminationSource)
	s.mu.Unlock()
	out.merge(output)
	return out
}

func (r *Registry) signalKill(ctx context.Context, s *Session, id string, consume bool) (*Reply, error) {
	s.mu.Lock()
	pid, expected, detached := s.pid, s.hostStart, s.detached
	s.mu.Unlock()
	switch {
	case s.pty != nil:
		if _, err := s.pty.terminate(true); err != nil && pid != 0 {
			if err := signalTerminate(pid); err != nil {
				return nil, err
			}
		}
	case s.process != nil:
		Terminate(ctx, s.process.PID(), expected, r.termGrace(ctx))
	case s.env != nil && pid != 0:
		if _, err := s.env.Execute(ctx, "kill "+strconv.Itoa(pid)+" 2>/dev/null",
			tool.ExecOptions{Timeout: envKillTimeout, RewriteCompoundBackground: true}); err != nil {
			return nil, err
		}
	case detached && s.pidScope == scopeHost && pid != 0:
		if detachedHostFate(pid, expected) != fateRunning {
			stopped := r.stopScope(ctx, s)
			s.mu.Lock()
			output := completionFieldsLocked(s)
			s.mu.Unlock()
			if consume {
				r.markConsumed(id)
			}
			r.closeReusedDetached(ctx, s)
			s.mu.Lock()
			out := newReply("status", "already_exited", "exit_code", s.exitCode)
			unit := s.systemdUnit
			s.mu.Unlock()
			out.merge(output)
			if !stopped {
				out.set("scope_stop_failed", unit)
			}
			return out, nil
		}
		Terminate(ctx, pid, nil, r.termGrace(ctx))
	default:
		return errorReply("Recovered process cannot be killed after restart because " +
			"its original runtime handle is no longer available"), nil
	}
	return nil, nil
}

func (r *Registry) stdinOp(ctx context.Context, caller Caller, id string, viaPTY func(ptyJob) error,
	viaPipe func(io.WriteCloser) error, ok *Reply) *Reply {
	s := r.Get(ctx, caller, id)
	if s == nil {
		return notFound(id)
	}
	if s.isExited() {
		return newReply("status", "already_exited", "error", alreadyExitedError(s))
	}
	var err error
	switch {
	case s.pty != nil:
		err = viaPTY(s.pty)
	case s.process == nil || s.process.Stdin() == nil:
		return errorReply(stdinUnavailableError(s))
	default:
		err = viaPipe(s.process.Stdin())
	}
	if err != nil {
		return errorReply(err.Error())
	}
	return ok
}

func (r *Registry) WriteStdin(ctx context.Context, caller Caller, id, data string) *Reply {
	return r.stdinOp(ctx, caller, id,
		func(j ptyJob) error {
			_, err := j.write([]byte(data))
			return err
		},
		func(w io.WriteCloser) error {
			_, err := io.WriteString(w, data)
			return err
		},
		newReply("status", "ok", "bytes_written", utf8.RuneCountInString(data)))
}

func (r *Registry) SubmitStdin(ctx context.Context, caller Caller, id, data string) *Reply {
	enter := "\n"
	if s := r.Get(ctx, caller, id); s != nil && s.pty != nil {
		enter = "\r"
		if runtime.GOOS == "windows" {
			enter = "\r\n"
		}
	}
	return r.WriteStdin(ctx, caller, id, data+enter)
}

func (r *Registry) CloseStdin(ctx context.Context, caller Caller, id string) *Reply {
	msg := "stdin closed"
	if s := r.Get(ctx, caller, id); s != nil && s.pty != nil {
		msg = "EOF sent"
	}
	return r.stdinOp(ctx, caller, id,
		func(j ptyJob) error { return j.sendEOF() },
		func(w io.WriteCloser) error { return w.Close() },
		newReply("status", "ok", "message", msg))
}

func (r *Registry) RequestCloseTerminal(ctx context.Context, caller Caller, id string) *Reply {
	sink := r.closeSink.Load()
	if sink == nil {
		return errorReply("close_terminal is only available in the icebath desktop app.")
	}
	if err := (*sink)(r.Get(ctx, caller, id), id); err != nil {
		return errorReply(err.Error())
	}
	return newReply("status", "ok", "closed", id,
		"note", "Closed the read-only terminal tab. The process was not killed; "+
			"its output remains available and the user can reopen the tab "+
			"from the status stack.")
}

func (r *Registry) CountRunning() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running.len()
}

func (r *Registry) List(ctx context.Context, caller Caller, o ListOptions) []*Reply {
	table := newSessionTable()
	if o.IncludeRetained {
		for _, s := range r.loadCompletedResults(ctx, caller.SessionID, "") {
			table.put(s)
		}
	}
	r.mu.Lock()
	for _, s := range r.finished.list() {
		table.put(s)
	}
	for _, s := range r.running.list() {
		table.put(s)
	}
	r.mu.Unlock()
	var sessions []*Session
	for _, s := range table.list() {
		if s = r.refreshDetached(ctx, s); s != nil && !uncollectedGone(s) {
			sessions = append(sessions, s)
		}
	}
	out := []*Reply{}
	for _, s := range sessions {
		s.mu.Lock()
		owner, key := s.ownerTaskID, s.sessionKey
		s.mu.Unlock()
		if (o.TaskID != "" || o.SessionKey != "") &&
			!(o.TaskID != "" && owner == o.TaskID || o.SessionKey != "" && key == o.SessionKey) {
			continue
		}
		r.reconcile(ctx, s)
		out = append(out, listEntry(s, o))
	}
	return out
}

func listEntry(s *Session, o ListOptions) *Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec := int64(s.startedAt)
	started := time.Unix(sec, int64((s.startedAt-float64(sec))*1e9)).Local().Format("2006-01-02T15:04:05")
	e := newReply(
		"session_id", s.id,
		"command", runeHead(s.command, listChars),
		"cwd", cwdValue(s.cwd),
		"pid", pidValue(s.pid),
		"owner_task_id", s.ownerTaskID,
		"started_at", started,
		"uptime_seconds", uptime(s),
		"status", statusText(s.exited),
		"output_preview", runeTail(s.out.String(), listChars),
	)
	if o.TaskID != "" && o.SessionKey != "" && s.ownerTaskID != o.TaskID && s.sessionKey == o.SessionKey {
		e.set("session_scoped", true)
	}
	if s.wslChain {
		e.set("wsl_chain", true)
		e.set("wsl_note", wslChainNote)
	}
	if len(s.watchPatterns) > 0 && !s.watchDisabled {
		e.set("watch_patterns", append([]string(nil), s.watchPatterns...))
		e.set("watch_hit", s.watchHits > 0)
	}
	if s.notify {
		e.set("notify_on_complete", true)
	}
	if s.persistOnRelease {
		e.set("persist_on_release", true)
	}
	if s.exited {
		e.set("exit_code", s.exitCode)
		e.set("exited_at", s.exitedAt)
		e.set("completion_reason", s.completionReason)
	}
	if s.detached {
		e.set("detached", true)
	}
	return e
}

func (r *Registry) HasActiveProcesses(ctx context.Context, taskID string) bool {
	return r.anyRunning(ctx, func(s *Session) bool { return s.ownerTaskID == taskID })
}

func (r *Registry) RunningOwnedBy(owner string) []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Session
	for _, s := range r.running.list() {
		s.mu.Lock()
		hit := s.ownerTaskID == owner && !s.exited
		s.mu.Unlock()
		if hit {
			out = append(out, s)
		}
	}
	return out
}

func (r *Registry) UnreadCompletionsOwnedBy(owner string) []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Session
	for _, s := range r.finished.list() {
		s.mu.Lock()
		hit := s.ownerTaskID == owner && s.notify && s.exitCode != nil
		s.mu.Unlock()
		if hit && !r.consumed[s.id] && !r.pollObserved[s.id] {
			out = append(out, s)
		}
	}
	return out
}

func (r *Registry) TransferOwnership(ctx context.Context, id, from, to, toTaskID, toSessionKey, note string) *Session {
	s := r.Get(ctx, Caller{}, id)
	r.mu.Lock()
	defer r.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited || s.ownerTaskID != from {
		return nil
	}
	s.ownerTaskID = to
	s.taskID = toTaskID
	s.sessionKey = toSessionKey
	s.handoffNote = note
	return s
}

func (r *Registry) HasActiveForSession(ctx context.Context, sessionKey string, maxActiveAge *time.Duration) bool {
	t := now()
	return r.anyRunning(ctx, func(s *Session) bool {
		return s.sessionKey == sessionKey && (maxActiveAge == nil || t-s.startedAt < maxActiveAge.Seconds())
	})
}

func (r *Registry) HasAnyActive(ctx context.Context) bool {
	return r.anyRunning(ctx, func(*Session) bool { return true })
}

func (r *Registry) SnapshotRunningIDs(owner string) map[string]bool {
	ids := map[string]bool{}
	for _, s := range r.RunningOwnedBy(owner) {
		ids[s.id] = true
	}
	return ids
}

func (r *Registry) KillStartedSince(ctx context.Context, taskID string, baseline map[string]bool, source string) int {
	exclude := make([]string, 0, len(baseline))
	for id := range baseline {
		exclude = append(exclude, id)
	}
	return r.KillAll(ctx, KillAllOptions{OwnerTaskID: &taskID, Exclude: exclude, Source: source, Consume: true})
}

func (r *Registry) KillAll(ctx context.Context, o KillAllOptions) int {
	source := o.Source
	if source == "" {
		source = SourceKillAll
	}
	lifecycle := lifecycleKillSources[source]
	excluded := map[string]bool{}
	for _, id := range o.Exclude {
		excluded[id] = true
	}
	var targets []string
	r.mu.Lock()
	for _, s := range append(r.running.list(), r.finished.list()...) {
		s.mu.Lock()
		hit := (o.OwnerTaskID == nil || s.ownerTaskID == *o.OwnerTaskID) && !excluded[s.id] &&
			(!s.exited || s.scopeStopPending) && !(lifecycle && s.persistOnRelease)
		s.mu.Unlock()
		if hit {
			targets = append(targets, s.id)
		}
	}
	r.mu.Unlock()
	stopped := 0
	for _, id := range targets {
		res := r.Kill(ctx, Caller{}, id, KillOptions{Source: source, Consume: o.Consume})
		if status := res.Status(); status == "killed" || status == "already_exited" {
			if _, failed := res.Get("scope_stop_failed"); !failed {
				stopped++
			}
		}
	}
	return stopped
}

func (r *Registry) TransformOutput(ctx context.Context, output, command string, exitCode *int, taskID string) string {
	if replaced, ok := r.transform.TransformTerminalOutput(ctx, command, output, exitCode, taskID, ""); ok {
		return replaced
	}
	return output
}

func (r *Registry) redactReply(ctx context.Context, caller Caller, res *Reply) *Reply {
	command := res.str("command")
	taskID := res.str("task_id")
	if taskID == "" {
		if s := r.Get(ctx, caller, res.str("session_id")); s != nil {
			s.mu.Lock()
			taskID = s.taskID
			s.mu.Unlock()
		}
	}
	for _, key := range []string{"output", "output_preview"} {
		if value := res.str(key); value != "" {
			value = r.TransformOutput(ctx, value, command, res.exitCode(), taskID)
			res.set(key, r.redactor.Terminal(ctx, value, command, false))
		}
	}
	if command != "" {
		res.set("command", r.redactor.Text(ctx, command, redact.Mode{CodeFile: true}))
	}
	return res
}
