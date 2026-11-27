package terminal

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

const (
	// Spacing of a job's watch match notices
	watchSpacing = 15 * time.Second
	// Rate limited windows in a row that turn watching off
	watchStrikeLimit = 3
	// Matches a job delivers over its life
	watchLifetime = 8
	// Floor of a heartbeat's interval in seconds
	HeartbeatMin = 60
	// How often heartbeats are looked for
	heartbeatTick = 5 * time.Second
	// Matches across every job in one window before the breaker trips, the window, and how long it stays tripped
	watchGlobalMax      = 15
	watchGlobalWindow   = 10 * time.Second
	watchGlobalCooldown = 30 * time.Second
	// Output a heartbeat carries
	heartbeatChars = 2000
	// Completions arriving this close together are delivered as one batch
	batchWindow = 500 * time.Millisecond
)

// Watch match rate of every job together, tripped by a flood
type globalWatch struct {
	mu          sync.Mutex
	windowStart time.Time
	hits        int
	trippedTill time.Time
	suppressed  int
}

// Notices waiting to reach each session's agent, and where each session's go
type notices struct {
	mu         sync.Mutex
	pending    map[string]*batch
	deliverers map[string]Deliver
}

type batch struct {
	completions []*Job
	timer       *time.Timer
}

// Keeps where the session's notices go, for jobs recovered after a restart that came with none
func (n *notices) remember(session string, deliver Deliver) {
	if deliver == nil {
		return
	}
	n.mu.Lock()
	n.deliverers[session] = deliver
	n.mu.Unlock()
}

// Where the job's notices go, nil when nobody can be told
func (r *Jobs) target(j *Job) Deliver {
	if j.deliver != nil {
		return j.deliver
	}
	r.notes.mu.Lock()
	defer r.notes.mu.Unlock()
	return r.notes.deliverers[j.Session]
}

// Output of a background process as the transform_terminal_output hooks hand it on, before it is masked: its exit code nil while it runs,
// the task that of the agent that started it, empty for a job not known
func (r *Jobs) Transform(ctx context.Context, cfg *config.Config, j *Job, output, command string, code *int, call string) string {
	if output == "" {
		return output
	}
	task := ""
	if j != nil {
		task = j.Task
	}
	return r.t.hooks.TransformTerminal(ctx, cfg, hooks.TerminalOutput{Command: command, Output: output, ReturnCode: code, Task: task, ID: call})
}

// How much of background processes the profile's user and agent are told
func mode(j *Job) string {
	return j.Config.GetString(terminalconfig.Notifications)
}

// Delivers one notice of the job at once
func (r *Jobs) send(j *Job, text, title string) {
	if r.closing.Load() || mode(j) == terminalconfig.NotifyOff {
		return
	}
	deliver := r.target(j)
	if deliver == nil {
		slog.Debug("Background process notice has nowhere to go", "session_id", j.ID, "session", j.Session)
		return
	}
	if err := deliver(text, title); err != nil {
		slog.Warn("Background process notice could not reach its session", "session_id", j.ID, "session", j.Session, "error", err)
	}
}

// Publishes the completion of a job that asked for one, batched with others of its session that end close by
func (r *Jobs) completion(j *Job) {
	j.mu.Lock()
	notify := j.notify
	j.mu.Unlock()
	if !notify || mode(j) == terminalconfig.NotifyOff {
		return
	}
	r.notes.mu.Lock()
	defer r.notes.mu.Unlock()
	b := r.notes.pending[j.Session]
	if b == nil {
		b = &batch{}
		r.notes.pending[j.Session] = b
		b.timer = time.AfterFunc(batchWindow, func() { r.flush(j.Session) })
	}
	b.completions = append(b.completions, j)
}

// Delivers the session's batch of completions, those whose output the agent already read left out
func (r *Jobs) flush(session string) {
	r.notes.mu.Lock()
	b := r.notes.pending[session]
	delete(r.notes.pending, session)
	r.notes.mu.Unlock()
	if b == nil {
		return
	}
	var live []*Job
	for _, j := range b.completions {
		if !r.Consumed(j.ID) {
			live = append(live, j)
		}
	}
	if len(live) == 0 {
		return
	}
	var texts []string
	var failed []*Job
	for _, j := range live {
		texts = append(texts, r.completionText(j))
		if j.failed() {
			failed = append(failed, j)
		}
	}
	text := texts[0]
	if len(texts) > 1 {
		text = fmt.Sprintf("[IMPORTANT: %d background processes completed. Treat these results as one batch and give one consolidated response; "+
			"preserve failures and actionable results.]\n\n", len(texts)) + strings.Join(texts, "\n\n")
	}
	first := live[0]
	title := ""
	switch mode(first) {
	case terminalconfig.NotifyConcise:
		title = CompletionTitle(live)
	case terminalconfig.NotifyResult, terminalconfig.NotifyAll:
		title = completionReport(live)
	case terminalconfig.NotifyError:
		if len(failed) > 0 {
			title = completionReport(failed)
		}
	}
	r.send(first, text, title)
}

// Whether the job ended badly
func (j *Job) failed() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.reason == "lost" || j.reason == "failed_start" || j.reason == "exited" && (j.code == nil || *j.code != 0)
}

// Title of the completions with each one's output tail, for the result and all notice modes
func completionReport(jobs []*Job) string {
	parts := []string{CompletionTitle(jobs)}
	for _, j := range jobs {
		j.mu.Lock()
		tail := strings.TrimSpace(j.tailLocked(1000))
		j.mu.Unlock()
		if tail = Redact(j.Config, tail, j.Command, false); tail == "" {
			continue
		}
		if len(jobs) > 1 {
			parts = append(parts, "`"+shortCommand(RedactCommand(j.Config, j.Command, false))+"`")
		}
		parts = append(parts, "```\n"+tail+"\n```")
	}
	return strings.Join(parts, "\n\n")
}

// Command on one line, cut at 80 characters
func shortCommand(command string) string {
	command = strings.Join(strings.Fields(command), " ")
	if utf8.RuneCountInString(command) > 80 {
		return strs.Head(command, 77) + "..."
	}
	return command
}

// Short title of one or more completions, as the user sees them
func CompletionTitle(jobs []*Job) string {
	if len(jobs) != 1 {
		return fmt.Sprintf("%d Background Processes Finished", len(jobs))
	}
	j := jobs[0]
	j.mu.Lock()
	reason, code := j.reason, j.code
	j.mu.Unlock()
	exit := "?"
	if code != nil {
		exit = strconv.Itoa(*code)
	}
	var outcome string
	switch {
	case reason == "killed":
		outcome = "Terminated"
	case reason == "lost":
		outcome = "Lost"
	case reason == "failed_start":
		outcome = "Failed to Start"
	case exit == "0":
		outcome = "Finished"
	default:
		outcome = "Failed"
	}
	detail := ""
	if reason == "exited" && exit != "0" {
		detail = " (exit " + exit + ")"
	}
	if command := shortCommand(RedactCommand(j.Config, j.Command, false)); command != "" {
		return fmt.Sprintf("Background Process %s%s: %s", outcome, detail, command)
	}
	return fmt.Sprintf("Background Process %s%s", outcome, detail)
}

// How a completion's status reads
func completionStatus(reason, source, exit string) string {
	switch reason {
	case "killed":
		return "terminated by " + cmp.Or(source, "icebath")
	case "lost":
		return "marked lost because the process backend disappeared"
	case "failed_start":
		return "failed to start"
	}
	if exit == "0" {
		return "completed normally"
	}
	return "exited"
}

// Message a completion puts into the conversation
func (r *Jobs) completionText(j *Job) string {
	j.mu.Lock()
	output, cut := j.completionLocked()
	reason, source, code, handoff := j.reason, j.source, j.code, j.handoff
	j.mu.Unlock()
	output = r.Transform(context.Background(), j.Config, j, output, j.Command, code, j.Call)
	exit := "?"
	if code != nil {
		exit = strconv.Itoa(*code)
	}
	signal := ""
	if exit == "-15" || exit == "143" {
		signal = ", SIGTERM"
	}
	attribution := ""
	if handoff != "" {
		attribution = "Handed off to you by a subagent before it finished. Purpose: " + handoff + "\n"
	}
	output = Redact(j.Config, output, j.Command, false)
	if cut > 0 {
		output = fmt.Sprintf("...(first %d characters cut — process_manage(action=\"log\", session_id=\"%s\") has the full output)\n%s", cut, j.ID, output)
	}
	return fmt.Sprintf("[IMPORTANT: Background process %s %s (exit code %s%s).\n%sCommand: %s\nOutput:\n%s]",
		j.ID, completionStatus(reason, source, exit), exit, signal, attribution, RedactCommand(j.Config, j.Command, false), output)
}

// Delivers a notice of the job with a title for the modes that show running jobs to the user
func (r *Jobs) progress(j *Job, text, title string) {
	if mode(j) == terminalconfig.NotifyError {
		title = ""
	}
	r.send(j, text, title)
}

// Arms periodic notices of the job's new output, answering the interval used
func (r *Jobs) Heartbeat(j *Job, seconds int) int {
	seconds = max(seconds, HeartbeatMin)
	j.mu.Lock()
	j.heartbeat, j.beatLast = seconds, time.Now()
	j.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.beating {
		r.beating = true
		go r.beats()
	}
	return seconds
}

// One loop for every job's heartbeat
func (r *Jobs) beats() {
	for {
		time.Sleep(heartbeatTick)
		now := time.Now()
		var due []*Job
		r.mu.Lock()
		for _, j := range r.running {
			j.mu.Lock()
			if j.heartbeat > 0 && !j.exited && now.Sub(j.beatLast) >= time.Duration(j.heartbeat)*time.Second {
				due = append(due, j)
			}
			j.mu.Unlock()
		}
		r.mu.Unlock()
		for _, j := range due {
			r.beat(j, now)
		}
	}
}

// Delivers the output since the last heartbeat, nothing when there is none
func (r *Jobs) beat(j *Job, now time.Time) {
	j.mu.Lock()
	j.beatLast = now
	output := ""
	if delta := j.total - j.beatTotal; delta > 0 {
		output = strs.Tail(j.output, delta)
	}
	j.beatTotal = j.total
	if output == "" {
		j.mu.Unlock()
		return
	}
	j.beatSeq++
	seq, interval := j.beatSeq, j.heartbeat
	j.mu.Unlock()
	omitted := 0
	if n := utf8.RuneCountInString(output); n > heartbeatChars {
		omitted, output = n-heartbeatChars, strs.Tail(output, heartbeatChars)
	}
	output = r.Transform(context.Background(), j.Config, j, term.StripANSI(output), j.Command, nil, j.Call)
	output = Redact(j.Config, output, j.Command, false)
	if omitted > 0 {
		output = fmt.Sprintf("...(%d earlier characters omitted)\n%s", omitted, output)
	}
	age := Elapsed(now.Sub(j.StartedAt))
	text := fmt.Sprintf("[Background process %s heartbeat #%d — still running after %s (next in %ds when there is new output; you will also be told "+
		"when it exits).\nCommand: %s\nOutput since last heartbeat:\n%s]", j.ID, seq, age, interval, RedactCommand(j.Config, j.Command, false), output)
	title := "Background Process Output after " + age
	if command := shortCommand(RedactCommand(j.Config, j.Command, false)); command != "" {
		title += ": " + command
	}
	r.progress(j, text, title)
}

// Elapsed time as 45s, 18m, 2m3s, 2h or 2h3m
func Elapsed(d time.Duration) string {
	s := max(int(d.Seconds()), 0)
	if s < 60 {
		return strconv.Itoa(s) + "s"
	}
	m, s := s/60, s%60
	if m < 60 {
		if s != 0 {
			return fmt.Sprintf("%dm%ds", m, s)
		}
		return fmt.Sprintf("%dm", m)
	}
	h, m := m/60, m%60
	if m != 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dh", h)
}

// Scans fresh output for the job's watch patterns and delivers rate limited notices of the lines that match
func (r *Jobs) matchPatterns(j *Job, text string) {
	j.mu.Lock()
	if len(j.patterns) == 0 || j.watchOff || j.exited {
		j.mu.Unlock()
		return
	}
	patterns := j.patterns
	j.mu.Unlock()
	pattern := ""
	var lines []string
	for _, line := range splitLines(text) {
		for _, p := range patterns {
			if strings.Contains(line, p) {
				if pattern == "" {
					pattern = p
				}
				lines = append(lines, strings.TrimRight(line, " \t\r\n\v\f"))
				break
			}
		}
	}
	if len(lines) == 0 {
		return
	}
	now := time.Now()
	j.mu.Lock()
	if !j.watchCooldown.IsZero() && now.Before(j.watchCooldown) {
		j.watchSuppressed += len(lines)
		if j.watchStrike {
			j.mu.Unlock()
			return
		}
		j.watchStrike = true
		if j.watchStrikes++; j.watchStrikes < watchStrikeLimit {
			j.mu.Unlock()
			return
		}
		j.watchOff, j.notify = true, true
		suppressed := j.watchSuppressed
		j.mu.Unlock()
		r.watchDisabled(j, suppressed, fmt.Sprintf("%d consecutive rate-limit windows triggered (min spacing %ds). ", watchStrikeLimit, int(watchSpacing.Seconds())))
		return
	}
	if !j.watchCooldown.IsZero() && !j.watchStrike {
		j.watchStrikes = 0
	}
	j.watchStrike = false
	j.watchCooldown = now.Add(watchSpacing)
	j.watchHits++
	suppressed := j.watchSuppressed
	j.watchSuppressed = 0
	exhausted := j.watchHits >= watchLifetime
	if exhausted {
		j.watchOff, j.notify = true, true
	}
	j.mu.Unlock()
	if r.admit(j, now) {
		output := r.Transform(context.Background(), j.Config, j, term.StripANSI(strings.Join(lines[:min(len(lines), 20)], "\n")), j.Command, nil, j.Call)
		output = Redact(j.Config, output, j.Command, false)
		if utf8.RuneCountInString(output) > 2000 {
			output = strs.Head(output, 2000) + "\n...(truncated)"
		}
		text := fmt.Sprintf("[IMPORTANT: Background process %s matched watch pattern \"%s\".\nCommand: %s\nMatched output:\n%s", j.ID, pattern,
			RedactCommand(j.Config, j.Command, false), output)
		if suppressed > 0 {
			text += fmt.Sprintf("\n(%d earlier matches were suppressed by rate limit)", suppressed)
		}
		title := fmt.Sprintf("Background Process Matched %q", pattern)
		if command := shortCommand(RedactCommand(j.Config, j.Command, false)); command != "" {
			title += ": " + command
		}
		r.progress(j, text+"]", title)
	}
	if exhausted {
		r.watchDisabled(j, 0, fmt.Sprintf("reached the lifetime cap of %d delivered matches. ", watchLifetime))
	}
}

func (r *Jobs) watchDisabled(j *Job, suppressed int, why string) {
	message := fmt.Sprintf("Watch patterns disabled for process %s — %sFalling back to notify_on_complete semantics; you'll get exactly one "+
		"notification when the process exits.", j.ID, why)
	slog.Debug("Watch patterns disabled", "session_id", j.ID, "suppressed", suppressed)
	r.progress(j, "[IMPORTANT: "+message+"]", "")
}

// Whether a watch match may pass the breaker of every job together, the breaker's trip and release told to the job's session
func (r *Jobs) admit(j *Job, now time.Time) bool {
	var messages []string
	r.watch.mu.Lock()
	if !r.watch.trippedTill.IsZero() && !now.Before(r.watch.trippedTill) {
		suppressed := r.watch.suppressed
		r.watch.trippedTill, r.watch.suppressed, r.watch.windowStart, r.watch.hits = time.Time{}, 0, now, 0
		if suppressed > 0 {
			messages = append(messages, fmt.Sprintf("Watch-pattern notifications resumed. %d match event(s) were suppressed during the flood.", suppressed))
		}
	}
	admit := false
	switch {
	case !r.watch.trippedTill.IsZero() && now.Before(r.watch.trippedTill):
		r.watch.suppressed++
	default:
		if now.Sub(r.watch.windowStart) >= watchGlobalWindow {
			r.watch.windowStart, r.watch.hits = now, 0
		}
		if admit = r.watch.hits < watchGlobalMax; admit {
			r.watch.hits++
		} else {
			r.watch.trippedTill = now.Add(watchGlobalCooldown)
			r.watch.suppressed++
			messages = append(messages, fmt.Sprintf("Watch-pattern overflow: >%d notifications in %ds across all processes. Suppressing further "+
				"watch_match events for %ds.", watchGlobalMax, int(watchGlobalWindow.Seconds()), int(watchGlobalCooldown.Seconds())))
		}
	}
	r.watch.mu.Unlock()
	for _, message := range messages {
		r.progress(j, "[IMPORTANT: "+message+"]", "")
	}
	return admit
}

// Wait and poll calls on a quiet running job, counted to turn its completion notice on
type waitPolicy struct {
	mu    sync.Mutex
	state map[string]waitEntry
	order []string
}

type waitEntry struct {
	total int
	count int
}

// Calls of this many in a row with no new output turn the notice on
const waitPromoteAfter = 2

func (w *waitPolicy) forget(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.state, id)
}

// Records one wait or poll that found the job running, answering how many in a row saw no new output
func (w *waitPolicy) count(j *Job) int {
	j.mu.Lock()
	total := j.total
	j.mu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	entry, known := w.state[j.ID]
	count := 0
	if total == entry.total {
		count = entry.count + 1
	}
	if !known {
		w.order = append(w.order, j.ID)
	}
	w.state[j.ID] = waitEntry{total: total, count: count}
	for len(w.state) > 512 && len(w.order) > 0 {
		delete(w.state, w.order[0])
		w.order = w.order[1:]
	}
	return count
}

// Counts a wait or poll that found the job running and quiet, answering whether its notice should now be turned on
func (r *Jobs) Quiet(j *Job) bool {
	return r.policy.count(j) >= waitPromoteAfter
}

// Forgets the wait and poll count of a job no longer running quietly
func (r *Jobs) Unquiet(id string) {
	r.policy.forget(id)
}

// Turns the job's completion notice on unless it already ended, answering whether it did
func (r *Jobs) Promote(j *Job) bool {
	r.mu.Lock()
	_, running := r.running[j.ID]
	ok := running && !j.finishClaimed
	r.mu.Unlock()
	if !ok {
		return false
	}
	j.mu.Lock()
	already := j.notify
	j.notify = true
	j.mu.Unlock()
	if !already {
		r.checkpoint(j.Config)
		slog.Info(fmt.Sprintf("process %s: repeated wait/poll with no new output; auto-enabled notify_on_complete", j.ID))
	}
	return true
}

// Whether the job's completion notice is on
func (j *Job) Notifies() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.notify
}

// Whether the profile lets background processes notify the agent at all
func (r *Jobs) Allowed(cfg *config.Config) bool {
	return cfg.GetString(terminalconfig.Notifications) != terminalconfig.NotifyOff
}

// Keeps where the session's notices go from now, for its jobs that came without one
func (r *Jobs) Remember(session string, deliver Deliver) {
	r.notes.remember(session, deliver)
}
