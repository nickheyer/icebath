package terminal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

// Kill signals need a moment to land before survivors can be judged
const killSettle = time.Second

// How long a job's own process gets to act on SIGTERM before the rest of its tree is signalled, when it goes first
const leaderHeadStart = 200 * time.Millisecond

// Note for a job started through a wsl launcher
const wslNote = "Spawned via a wsl[.exe] launcher: the recorded host PID is the short-lived launcher, not the Linux-side workers. Inspect " +
	"them with `wsl -e ps` / `wsl --list --running` from the host."

// What process_manage and the processes screen are told of a job, in the order the model reads it
type Report struct {
	Status            string   `json:"status,omitempty"`
	SessionID         string   `json:"session_id,omitempty"`
	Command           string   `json:"command,omitempty"`
	Error             string   `json:"error,omitempty"`
	Cwd               string   `json:"cwd,omitempty"`
	Pid               int      `json:"pid,omitempty"`
	Owner             string   `json:"owner_session,omitempty"`
	StartedAt         string   `json:"started_at,omitempty"`
	Uptime            *int     `json:"uptime_seconds,omitempty"`
	ExitCode          *int     `json:"exit_code,omitempty"`
	CompletionReason  string   `json:"completion_reason,omitempty"`
	TerminationSource string   `json:"termination_source,omitempty"`
	ExitedAt          string   `json:"exited_at,omitempty"`
	Output            *string  `json:"output,omitempty"`
	OutputPreview     *string  `json:"output_preview,omitempty"`
	OutputCut         int      `json:"output_cut,omitempty"`
	TotalLines        *int     `json:"total_lines,omitempty"`
	Showing           string   `json:"showing,omitempty"`
	BytesWritten      *int     `json:"bytes_written,omitempty"`
	Message           string   `json:"message,omitempty"`
	ProcessRunning    bool     `json:"process_running,omitempty"`
	Survivors         []int    `json:"survivors,omitempty"`
	ScopeStopFailed   string   `json:"scope_stop_failed,omitempty"`
	Notify            bool     `json:"notify_on_complete,omitempty"`
	Persist           bool     `json:"persist_on_release,omitempty"`
	WatchPatterns     []string `json:"watch_patterns,omitempty"`
	WatchHit          *bool    `json:"watch_hit,omitempty"`
	Wsl               bool     `json:"wsl_chain,omitempty"`
	WslNote           string   `json:"wsl_note,omitempty"`
	Detached          bool     `json:"detached,omitempty"`
	Note              string   `json:"note,omitempty"`
	TimeoutNote       string   `json:"timeout_note,omitempty"`
}

func ptr[T any](v T) *T { return &v }

func notFound(id string) *Report {
	return &Report{Status: "not_found", Error: "There is no background process with the ID " + id + "."}
}

// Status of the job, its exit when it ended, read under its lock
func (j *Job) headLocked() *Report {
	status := "running"
	if j.exited {
		status = "exited"
	}
	return &Report{SessionID: j.ID, Command: j.Command, Status: status}
}

func (j *Job) exitLocked(report *Report) {
	report.ExitCode = j.code
	report.CompletionReason = j.reason
	report.TerminationSource = j.source
}

// Result of an ended job with its exit and the tail a completion carries, read under its lock
func (j *Job) snapshotLocked(status string) *Report {
	report := &Report{Status: status, Command: j.Command}
	j.exitLocked(report)
	output, cut := j.completionLocked()
	report.Output, report.OutputCut = &output, cut
	return report
}

func uptime(j *Job) *int {
	return ptr(int(time.Since(j.StartedAt).Seconds()))
}

// Status of the job and its newest output
func (r *Jobs) Poll(cfg *config.Config, session, id string) *Report {
	j := r.Get(cfg, session, id)
	if j == nil || r.uncollectedGone(j) {
		return notFound(id)
	}
	r.reconcile(j)
	j.mu.Lock()
	preview := j.tailLocked(1000)
	exited := j.exited
	report := j.headLocked()
	if exited {
		j.exitLocked(report)
	}
	j.mu.Unlock()
	report.Pid, report.Uptime, report.OutputPreview = j.Pid, uptime(j), &preview
	if j.Detached {
		report.Detached = true
		report.Note = "Process recovered after restart -- output history unavailable"
	}
	return report
}

// Lines of the job's output: the last limit lines without an offset, else limit lines from it
func (r *Jobs) Log(cfg *config.Config, session, id string, offset *int, limit int) *Report {
	j := r.Get(cfg, session, id)
	if j == nil {
		return notFound(id)
	}
	j.mu.Lock()
	full := term.StripANSI(j.output)
	exited, finalizing := j.exited, j.finalizing()
	report := j.headLocked()
	j.mu.Unlock()
	lines := splitLines(full)
	total := len(lines)
	var selected []string
	observed := false
	if offset == nil && limit > 0 {
		selected = lines[max(total-limit, 0):]
		observed = len(selected) > 0 || total == 0
	} else {
		from := 0
		if offset != nil {
			from = *offset
		}
		start, stop := slice(from, from+limit, total)
		selected = lines[start:stop]
		observed = total == 0 || len(selected) > 0 && stop == total
	}
	report.Output, report.TotalLines, report.Showing = ptr(strings.Join(selected, "\n")), &total, fmt.Sprintf("%d lines", len(selected))
	if exited && observed && !finalizing {
		r.consume(j.ID)
	}
	return report
}

// Lines of the text without a trailing empty one, any line ending counted
func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// Bounds of [start, stop) over n items, negatives counted from the end
func slice(start, stop, n int) (int, int) {
	norm := func(i int) int {
		if i < 0 {
			i = max(i+n, 0)
		}
		return min(i, n)
	}
	a, b := norm(start), norm(stop)
	return a, max(a, b)
}

// Waits until the job ends, the timeout passes, the call is stopped or the user asks the tool to let go
func (r *Jobs) Wait(ctx context.Context, cfg *config.Config, session, id string, timeout *int, yield func() bool) *Report {
	limit := cfg.GetInt(terminalconfig.Timeout)
	if timeout != nil && *timeout <= 0 {
		return &Report{Status: "error", Error: fmt.Sprintf("timeout must be positive (got %d)", *timeout)}
	}
	note, effective := "", limit
	if timeout != nil {
		effective = *timeout
		if *timeout > limit {
			effective = limit
			note = fmt.Sprintf("Requested wait of %ds was clamped to configured limit of %ds", *timeout, limit)
		}
	}
	j := r.Get(cfg, session, id)
	if j == nil {
		return notFound(id)
	}
	deadline := time.Now().Add(time.Duration(effective) * time.Second)
	for time.Now().Before(deadline) {
		if j = r.refresh(j); j == nil {
			return notFound(id)
		}
		r.reconcile(j)
		var report *Report
		j.mu.Lock()
		exited := j.exited && !j.finalizing()
		if exited {
			report = j.snapshotLocked("exited")
		}
		tail := j.tailLocked(1000)
		j.mu.Unlock()
		switch {
		case exited:
			r.consume(j.ID)
		case ctx.Err() != nil:
			report = &Report{Status: "interrupted", Command: j.Command, Output: &tail, Note: "User sent a new message -- wait interrupted"}
		case yield != nil && yield():
			report = &Report{Status: "interrupted", Command: j.Command, Output: &tail, ProcessRunning: true,
				Note: "User sent a new message -- wait released; the process is still running and you will be notified on exit. Respond to the user now."}
		}
		if report != nil {
			report.TimeoutNote = note
			return report
		}
		j.completed.wait(min(time.Second, time.Until(deadline)))
	}
	j.mu.Lock()
	tail := j.tailLocked(1000)
	notify := j.notify
	j.mu.Unlock()
	base := fmt.Sprintf("Wait window of %ds elapsed — the process is still running. This is not an error. Uptime: %ds.", effective, *uptime(j))
	if notify {
		base += " notify_on_complete is set: you will be notified on exit — do more work instead of waiting again."
	} else {
		base += " Poll again later or use terminal(background=true, notify=true) next time for automatic notification."
	}
	if note != "" {
		base = note + ". " + base
	}
	return &Report{Status: "timeout", Command: j.Command, Output: &tail, ProcessRunning: true, TimeoutNote: base}
}

// Kills the job and its tree, answering its output; source names what killed it
func (r *Jobs) Kill(ctx context.Context, cfg *config.Config, session, id, source string, consume bool) *Report {
	source = cmp.Or(source, "process.kill")
	j := r.Get(cfg, session, id)
	if j == nil {
		return notFound(id)
	}
	if j.Exited() {
		stopped := r.stopScope(j)
		j.mu.Lock()
		report := j.snapshotLocked("already_exited")
		finalizing := j.finalizing()
		j.mu.Unlock()
		if !stopped {
			report.ScopeStopFailed = j.unit
		}
		if consume && !finalizing {
			r.consume(j.ID)
		}
		return report
	}
	if early := r.signal(j, id, consume); early != nil {
		return early
	}
	stopped := r.stopScope(j)
	j.mu.Lock()
	raced := j.exited
	j.mu.Unlock()
	if !raced {
		if survivors := r.survivors(j); len(survivors) > 0 {
			alive := strings.Trim(strings.Join(strings.Fields(fmt.Sprint(survivors)), ", "), "[]")
			slog.Warn(fmt.Sprintf("Kill incomplete for %s: %d process(es) still alive (%s) — session kept running", j.ID, len(survivors), alive))
			return &Report{Status: "error", Error: fmt.Sprintf("The background process %s was not fully stopped and keeps running. Process IDs still alive: %s.", j.ID, alive),
				SessionID: j.ID, Survivors: survivors, ProcessRunning: true}
		}
	}
	if !stopped {
		slog.Warn(fmt.Sprintf("Kill incomplete for %s: systemd scope %s did not stop", j.ID, j.unit))
		return &Report{Status: "error", Error: fmt.Sprintf("The background process %s was not fully stopped, because its systemd scope %s did not stop.", j.ID, j.unit), SessionID: j.ID,
			ScopeStopFailed: j.unit, ProcessRunning: !j.Exited()}
	}
	j.mu.Lock()
	output, cut := j.completionLocked()
	if consume {
		r.consume(j.ID)
	}
	j.exited, j.code, j.reason, j.source = true, ptr(-15), "killed", source
	report := &Report{Status: "killed", SessionID: j.ID, CompletionReason: j.reason, TerminationSource: j.source, Output: &output, OutputCut: cut}
	j.mu.Unlock()
	if !r.moveToFinished(j, false) {
		r.saveReceipt(j)
	}
	r.checkpoint(j.Config)
	return report
}

// Delivers the kill through the job's pseudo terminal, its process tree, its sandbox or its recovered pid; a result when it can't
func (r *Jobs) signal(j *Job, id string, consume bool) *Report {
	switch {
	case j.pty != nil:
		j.pty.Terminate(true)
	case j.proc != nil:
		r.terminate(j.Config, j.proc.Pid(), j.start, false)
	case j.env != nil && j.Pid != 0:
		j.env.Execute(context.Background(), fmt.Sprintf("kill %d 2>/dev/null", j.Pid), Exec{Timeout: 5 * time.Second, Raw: true})
	case j.Detached && j.hostPid && j.Pid != 0:
		if fate(j.Pid, j.start) != "running" {
			stopped := r.stopScope(j)
			j.mu.Lock()
			report := j.snapshotLocked("already_exited")
			j.mu.Unlock()
			if !stopped {
				report.ScopeStopFailed = j.unit
			}
			if consume {
				r.consume(id)
			}
			r.closeReused(j)
			return report
		}
		r.terminate(j.Config, j.Pid, 0, false)
	default:
		return &Report{Status: "error", Error: "This background process came back after icebath restarted, and icebath can no longer reach it to stop it."}
	}
	return nil
}

// Stops the job's systemd scope and what is left in it, true when it has none or it stopped; a failed stop is tried again by the next kill
func (r *Jobs) stopScope(j *Job) bool {
	if j.unit == "" {
		return true
	}
	stopped := stopUnit(j.unit)
	r.mu.Lock()
	j.scopePending = !stopped
	r.mu.Unlock()
	return stopped
}

// Terminates a host pid and its tree after checking its start time: TERM to all of it, or to the pid first with a head start when
// leaderFirst is set, one grace for everything, then KILL to what is left
func (r *Jobs) terminate(cfg *config.Config, pid int, start int64, leaderFirst bool) {
	if start != 0 && !ours(pid, start) {
		slog.Warn(fmt.Sprintf("Refusing to terminate host pid %d: start-time mismatch — PID was recycled onto an unrelated process.", pid))
		return
	}
	if !groups {
		SignalGroup(pid, true)
		return
	}
	if !Running(pid) {
		return
	}
	targets := append([]int{pid}, term.Descendants(pid)...)
	rest := targets
	if leaderFirst {
		terminate(pid)
		for until := time.Now().Add(leaderHeadStart); alive(pid) && time.Now().Before(until); {
			time.Sleep(time.Millisecond)
		}
		rest = targets[1:]
	}
	grouped := leadsGroup(pid) && SignalGroup(pid, false) == nil
	for _, p := range rest {
		terminate(p)
	}
	grace := time.Duration(max(cfg.GetInt(terminalconfig.TermGrace), 0)) * time.Second
	if grace <= 0 {
		return
	}
	for deadline := time.Now().Add(grace); time.Now().Before(deadline) && anyAlive(targets); {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(pid) {
		for _, p := range term.Descendants(pid) {
			if !slices.Contains(targets, p) {
				targets = append(targets, p)
			}
		}
	}
	if grouped && !GroupGone(pid) {
		SignalGroup(pid, true)
	}
	for _, p := range targets {
		if alive(p) {
			kill(p)
			slog.Info(fmt.Sprintf("Escalated to SIGKILL for pid %d (ignored SIGTERM within %.1fs grace)", p, grace.Seconds()))
		}
	}
}

func anyAlive(pids []int) bool {
	return slices.ContainsFunc(pids, alive)
}

// Host pids of the job still alive once the kill had time to land, failing closed
func (r *Jobs) survivors(j *Job) []int {
	deadline := time.Now().Add(killSettle)
	for {
		survivors := r.probeSurvivors(j)
		if len(survivors) == 0 || !time.Now().Before(deadline) {
			return survivors
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (r *Jobs) probeSurvivors(j *Job) []int {
	var survivors []int
	add := func(pid int) {
		if pid != 0 && !slices.Contains(survivors, pid) {
			survivors = append(survivors, pid)
		}
	}
	if j.proc != nil {
		if _, done := j.proc.Poll(); !done {
			add(cmp.Or(j.proc.Pid(), j.Pid))
		}
	}
	if j.pty != nil && j.pty.Alive() {
		add(j.Pid)
	}
	if j.hostPid && j.Pid != 0 && fate(j.Pid, j.start) == "running" {
		add(j.Pid)
		for _, pid := range term.Descendants(j.Pid) {
			if alive(pid) {
				add(pid)
			}
		}
	}
	return survivors
}

// Whether a recovered job closed without a collected exit has a pid that is gone
func (r *Jobs) uncollectedGone(j *Job) bool {
	j.mu.Lock()
	uncollected := j.exited && j.code == nil
	j.mu.Unlock()
	return uncollected && j.Detached && j.hostPid && !alive(j.Pid)
}

const stdinNonInteractive = "Pass input non-interactively instead (arguments, a here-doc, or `printf ... | cmd`)."

// Why giving the local pipe job a pseudo terminal would not help, empty when it would
func (j *Job) noPtyReason() string {
	switch {
	case j.ptyFallback != "":
		return fmt.Sprintf("pty=true was requested but %s, so it runs without stdin", j.ptyFallback)
	case !ptyAvailable():
		return "the PTY library is not available on this host"
	}
	return ""
}

func (j *Job) exitedError() string {
	if j.proc == nil || j.proc.Piped() {
		return "Process has already finished"
	}
	if reason := j.noPtyReason(); reason != "" {
		return fmt.Sprintf("Process has already finished. It ran without stdin (%s), so if it read stdin, it got end-of-file at once. %s", reason, stdinNonInteractive)
	}
	return "Process has already finished. It was started without stdin (terminal background=true without pty=true), so if it read stdin, " +
		"it got end-of-file at once. Relaunch it with terminal(background=true, pty=true) to send input."
}

func (j *Job) stdinError() string {
	if j.proc == nil {
		return "Process stdin not available: it runs on a non-local terminal backend (or was recovered after a restart), which has no live stdin. " + stdinNonInteractive
	}
	if reason := j.noPtyReason(); reason != "" {
		return fmt.Sprintf("This process has no stdin: %s. Relaunching with pty=true would not help. %s", reason, stdinNonInteractive)
	}
	return "This process was started without stdin (terminal background=true without pty=true). Relaunch it with terminal(background=true, pty=true) to send input."
}

// Runs an operation on the job's stdin: its pseudo terminal, or the pipe a command taken into the background was given its input on
func (r *Jobs) stdin(cfg *config.Config, session, id string, onPty func(*ptyProc) error, onPipe func(*HostProc) error, ok *Report) *Report {
	j := r.Get(cfg, session, id)
	if j == nil {
		return notFound(id)
	}
	if j.Exited() {
		return &Report{Status: "already_exited", Error: j.exitedError()}
	}
	var err error
	switch {
	case j.pty != nil:
		err = onPty(j.pty)
	case j.proc == nil || !j.proc.Piped():
		return &Report{Status: "error", Error: j.stdinError()}
	default:
		err = onPipe(j.proc)
	}
	if err != nil {
		return &Report{Status: "error", Error: err.Error()}
	}
	return ok
}

// Sends the text to the job's stdin as it is, with no newline
func (r *Jobs) Write(cfg *config.Config, session, id, data string) *Report {
	return r.stdin(cfg, session, id, func(p *ptyProc) error { return p.Write([]byte(data)) }, func(*HostProc) error { return errClosed },
		&Report{Status: "ok", BytesWritten: ptr(utf8.RuneCountInString(data))})
}

// Sends the text and Enter: a carriage return on a pseudo terminal, a newline elsewhere
func (r *Jobs) Submit(cfg *config.Config, session, id, data string) *Report {
	enter := "\n"
	if j := r.Get(cfg, session, id); j != nil && j.pty != nil {
		enter = ptyEnter
	}
	return r.Write(cfg, session, id, data+enter)
}

// Sends end of file to the job's stdin without killing it
func (r *Jobs) EOF(cfg *config.Config, session, id string) *Report {
	message := "stdin closed"
	if j := r.Get(cfg, session, id); j != nil && j.pty != nil {
		message = "EOF sent"
	}
	return r.stdin(cfg, session, id, func(p *ptyProc) error { return p.SendEOF() }, func(*HostProc) error { return nil }, &Report{Status: "ok", Message: message})
}

// Jobs of the session, running and recently finished, receipts it kept included when receipts is set; every job for no session
func (r *Jobs) List(cfg *config.Config, session string, receipts bool) []*Report {
	var order []string
	jobs := map[string]*Job{}
	add := func(j *Job) {
		if jobs[j.ID] == nil {
			order = append(order, j.ID)
		}
		jobs[j.ID] = j
	}
	if receipts && cfg != nil && session != "" {
		for _, j := range r.receiptList(cfg, session, "") {
			add(j)
		}
	}
	r.mu.Lock()
	for _, j := range ordered(r.finished, func(j *Job) int64 { return j.finSeq }) {
		add(j)
	}
	for _, j := range ordered(r.running, func(j *Job) int64 { return j.runSeq }) {
		add(j)
	}
	r.mu.Unlock()
	var out []*Report
	for _, id := range order {
		j := r.refresh(jobs[id])
		if j == nil || r.uncollectedGone(j) || session != "" && j.Session != session {
			continue
		}
		r.reconcile(j)
		j.mu.Lock()
		report := j.headLocked()
		report.Command = strs.Head(j.Command, 200)
		report.Cwd, report.Pid, report.Owner = j.Cwd, j.Pid, j.Session
		report.StartedAt = j.StartedAt.Local().Format("2006-01-02T15:04:05")
		report.Uptime = uptime(j)
		report.OutputPreview = ptr(strs.Tail(j.output, 200))
		if j.wsl {
			report.Wsl, report.WslNote = true, wslNote
		}
		if len(j.patterns) > 0 && !j.watchOff {
			report.WatchPatterns, report.WatchHit = slices.Clone(j.patterns), ptr(j.watchHits > 0)
		}
		report.Notify, report.Persist, report.Detached = j.notify, j.Persist, j.Detached
		if j.exited {
			j.exitLocked(report)
			if !j.exitedAt.IsZero() {
				report.ExitedAt = j.exitedAt.Local().Format("2006-01-02T15:04:05")
			}
		}
		j.mu.Unlock()
		out = append(out, report)
	}
	return out
}

func ordered(table map[string]*Job, seq func(*Job) int64) []*Job {
	out := make([]*Job, 0, len(table))
	for _, j := range table {
		out = append(out, j)
	}
	slices.SortFunc(out, func(a, b *Job) int { return int(seq(a) - seq(b)) })
	return out
}

// Kill sources that are a session's cleanup rather than a deliberate stop, sparing jobs kept on release
var lifecycleSources = []string{"agent_close", "session_end"}

// Kills the running jobs of the session, every session's when it is empty, answering how many it stopped
func (r *Jobs) KillAll(ctx context.Context, session, source string, consume bool) int {
	return r.killWhere(ctx, func(j *Job) bool { return session == "" || j.Session == session }, source, consume)
}

// Kills the running jobs started in the session, those started outside every session for none, as the processes screen kills the
// processes of one session; answers how many it stopped
func (r *Jobs) KillIn(ctx context.Context, session string) int {
	return r.killWhere(ctx, func(j *Job) bool { return j.Session == session }, "process.stop", false)
}

// Kills the running jobs that match, answering how many it stopped
func (r *Jobs) killWhere(ctx context.Context, match func(*Job) bool, source string, consume bool) int {
	source = cmp.Or(source, "kill_all")
	lifecycle := slices.Contains(lifecycleSources, source)
	var targets []*Job
	r.mu.Lock()
	for _, table := range []map[string]*Job{r.running, r.finished} {
		for _, j := range table {
			if match(j) && (!j.Exited() || j.scopePending) && !(lifecycle && j.Persist) {
				targets = append(targets, j)
			}
		}
	}
	r.mu.Unlock()
	count := 0
	for _, j := range targets {
		switch report := r.Kill(ctx, j.Config, j.Session, j.ID, source, consume); report.Status {
		case "killed", "already_exited":
			if report.ScopeStopFailed == "" {
				count++
			}
		}
	}
	return count
}

// Kills the session's jobs but those kept on release, as the session ends
func (r *Jobs) Release(ctx context.Context, session string) {
	if killed := r.KillAll(ctx, session, "session_end", true); killed > 0 {
		slog.Info(fmt.Sprintf("Killed %d background process(es) of session %s", killed, session))
	}
}

// Kills every job as icebath stops; jobs that outlive icebath survive only its crash, to be recovered when it starts again
func (r *Jobs) Close() {
	r.closing.Store(true)
	if killed := r.KillAll(context.Background(), "", "shutdown", false); killed > 0 {
		slog.Info(fmt.Sprintf("Shutdown: killed %d background process(es)", killed))
	}
}

// A command's stdin pipe carries only the text it was started with, closed once that was written
var errClosed = errors.New("stdin pipe is closed")

// What the processes screen shows of a job
type View struct {
	ID      string
	Command string
	Session string
	Profile string
	Status  string
	Reason  string
	Code    *int
	Cwd     string
	Pid     int
	Started time.Time
	Ended   time.Time
	// Newest part of its output
	Tail string
}

// Jobs of the session, every session's for none, running ones and those that ended still listed
func (r *Jobs) Views(session string) []View {
	r.mu.Lock()
	jobs := append(ordered(r.finished, func(j *Job) int64 { return j.finSeq }), ordered(r.running, func(j *Job) int64 { return j.runSeq })...)
	r.mu.Unlock()
	var views []View
	for _, j := range jobs {
		if j = r.refresh(j); j == nil || session != "" && j.Session != session {
			continue
		}
		views = append(views, r.view(j))
	}
	return views
}

// The job of the id, or the one job whose id starts with it, in whichever session it runs, as the processes screen and /processes find
// one
func (r *Jobs) Find(id string) (View, bool) {
	j := r.Get(nil, "", id)
	if j == nil {
		return View{}, false
	}
	return r.view(j), true
}

// What the processes screen shows of the job, its exit read first
func (r *Jobs) view(j *Job) View {
	r.reconcile(j)
	j.mu.Lock()
	defer j.mu.Unlock()
	view := View{ID: j.ID, Command: j.Command, Session: j.Session, Status: "running", Reason: j.reason, Code: j.code, Cwd: j.Cwd, Pid: j.Pid,
		Started: j.StartedAt, Ended: j.exitedAt, Tail: j.tailLocked(2000)}
	if j.exited {
		view.Status = "exited"
	}
	if j.Config != nil {
		view.Profile = j.Config.Profile()
	}
	return view
}

// Kills every job of the deleted profile, and drops it from the list
func (r *Jobs) Forget(profile string) {
	r.mu.Lock()
	unresolved := r.unresolved[profile]
	delete(r.unresolved, profile)
	r.mu.Unlock()
	for _, entry := range unresolved {
		if !stopUnit(entry.Unit) {
			slog.Warn(fmt.Sprintf("Could not reap scope %s of the deleted profile %s", entry.Unit, profile))
		}
	}
	r.mu.Lock()
	var jobs []*Job
	for _, table := range []map[string]*Job{r.running, r.finished} {
		for _, j := range table {
			if j.Config != nil && j.Config.Profile() == profile {
				jobs = append(jobs, j)
			}
		}
	}
	r.mu.Unlock()
	for _, j := range jobs {
		if !j.Exited() {
			r.Kill(context.Background(), j.Config, j.Session, j.ID, "profile_deleted", true)
		}
		r.mu.Lock()
		delete(r.running, j.ID)
		delete(r.finished, j.ID)
		r.mu.Unlock()
	}
}
