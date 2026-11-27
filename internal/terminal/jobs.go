package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

const (
	// Rolling output kept per job
	maxOutput = 200_000
	// Output tail a completion notice carries by default
	completionChars = 2000
	// Finished jobs stay listed this long
	finishedTTL = 30 * time.Minute
	// Jobs tracked before the oldest finished one is dropped
	maxJobs = 64
	// Output read after a stop was asked, past which the reader gives up on a descendant that keeps writing
	finalDrainMax = 1 << 20
	// Wait for the reader's last drain once the direct child exited
	finalDrainWait = time.Second
)

// Fixed head of the line a background job prints just before its command
const startPrefix = "__icebath_start_"

// Purpose a background job's process is recorded under in the spawn ledger and stamped with
const backgroundPurpose = "terminal-background"

// Most start-up output held while the start line is awaited
const startupHeadMax = 256 * 1024

// Where a job's completion and other notices go: text the model reads, a short title the user sees; an error when they can't be taken
type Deliver func(text, title string) error

// A command running in the background, what process_manage reads, waits on, writes to and kills
type Job struct {
	ID      string
	Command string
	// Session that started it, whose agent its notices reach, and the profile it ran for
	Session string
	Config  *config.Config
	Cwd     string
	Pid     int
	// Its process on this machine, its pseudo terminal, or the environment of the sandbox it runs in
	proc *HostProc
	pty  *ptyProc
	env  Environment
	// Start time of its process, to tell it from a later process given the same pid
	start     int64
	StartedAt time.Time
	// Process was recovered after icebath restarted, its handles gone
	Detached bool
	// Pid is one of this machine, not of a sandbox
	hostPid bool
	// Survives the end of its session
	Persist bool
	// Task of the agent that started it and the tool call that did, empty for one recovered after icebath restarted
	Task string
	Call string
	// Why pty=true was not honoured
	ptyFallback string
	// Started through a wsl launcher, whose pid is the launcher's
	wsl bool
	// Transient systemd scope it runs in, and whether stopping it failed so a kill must try again
	unit         string
	scopePending bool

	mu       sync.Mutex
	exited   bool
	exitedAt time.Time
	code     *int
	// How it ended: exited, killed, lost or failed_start, and what killed it
	reason string
	source string
	output string
	total  int
	// Note of the subagent that handed it over
	handoff string

	notify          bool
	completionChars int
	patterns        []string
	heartbeat       int
	beatLast        time.Time
	beatTotal       int
	beatSeq         int
	watchHits       int
	watchSuppressed int
	watchOff        bool
	watchCooldown   time.Time
	watchStrike     bool
	watchStrikes    int
	deliver         Deliver

	completed       *event
	finishClaimed   bool
	readerDone      chan struct{}
	finishRequested *event
	selectable      bool
	marker          string
	runSeq, finSeq  int64
}

// Event set once that others wait on
type event struct {
	once sync.Once
	ch   chan struct{}
}

func newEvent() *event { return &event{ch: make(chan struct{})} }

func (s *event) set() { s.once.Do(func() { close(s.ch) }) }

func (s *event) isSet() bool {
	select {
	case <-s.ch:
		return true
	default:
		return false
	}
}

func (s *event) wait(d time.Duration) bool {
	if d <= 0 {
		return s.isSet()
	}
	select {
	case <-s.ch:
		return true
	case <-time.After(d):
		return false
	}
}

func newJob(id, command string) *Job {
	return &Job{ID: id, Command: command, reason: "exited", completed: newEvent(), finishRequested: newEvent()}
}

func (j *Job) appendLocked(text string) {
	j.output += text
	j.total += utf8.RuneCountInString(text)
	if utf8.RuneCountInString(j.output) > maxOutput {
		j.output = strs.Tail(j.output, maxOutput)
	}
}

func (j *Job) add(text string) {
	j.mu.Lock()
	j.appendLocked(text)
	j.mu.Unlock()
}

// Appends unless the job ended, decided under the lock a kill holds
func (j *Job) addIfRunning(text string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.exited {
		return false
	}
	j.appendLocked(text)
	return true
}

// Records an exit without overwriting a kill that raced the reader
func (j *Job) markExited(code *int, reason, source string) {
	j.exited = true
	if j.reason != "killed" {
		j.code, j.reason = code, reason
		if source != "" {
			j.source = source
		}
	}
}

func (j *Job) Exited() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.exited
}

func (j *Job) readerAlive() bool {
	if j.readerDone == nil {
		return false
	}
	select {
	case <-j.readerDone:
		return false
	default:
		return true
	}
}

// A reconcile marked it exited while the reader that owns its output still drains
func (j *Job) finalizing() bool {
	return j.finishRequested.isSet() && !j.completed.isSet() && j.readerAlive()
}

// Last n characters of the output without ANSI sequences, read under the job's lock
func (j *Job) tailLocked(n int) string {
	return term.StripANSI(strs.Tail(j.output, n))
}

// Output a completion carries and the characters cut ahead of it, read under the job's lock
func (j *Job) completionLocked() (string, int) {
	limit := j.completionChars
	if limit == 0 {
		limit = completionChars
	}
	return j.tailLocked(limit), max(utf8.RuneCountInString(j.output)-limit, 0)
}

// Background processes: started by the terminal tool, read by process_manage, kept while they run and for a while after, their
// completions delivered to the agent of the session that started them
type Jobs struct {
	t        *Terminal
	mu       sync.Mutex
	running  map[string]*Job
	finished map[string]*Job
	seq      int64
	// Completions the agent already read through wait or log
	marks    sync.Mutex
	consumed map[string]bool
	// Jobs started while icebath serves others outlive it and are recovered when it starts again
	outlive bool
	// Icebath is stopping, so the jobs it kills tell nobody
	closing atomic.Bool
	beating bool
	watch   globalWatch
	notes   notices
	policy  waitPolicy
	// Transient systemd scopes the background commands of a server systemd runs go in
	scopes *scopes
	// Checkpoint entries of jobs whose process is gone but whose scope could not be stopped, by profile, kept to try again at the next start
	unresolved map[string][]checkpointEntry
}

func newJobs(t *Terminal) *Jobs {
	return &Jobs{
		t:          t,
		running:    map[string]*Job{},
		finished:   map[string]*Job{},
		consumed:   map[string]bool{},
		notes:      notices{pending: map[string]*batch{}, deliverers: map[string]Deliver{}},
		policy:     waitPolicy{state: map[string]waitEntry{}},
		scopes:     newScopes(),
		unresolved: map[string][]checkpointEntry{},
	}
}

func (r *Jobs) consume(id string) {
	r.marks.Lock()
	r.consumed[id] = true
	r.marks.Unlock()
}

// Whether the agent already read the job's completion through wait or log
func (r *Jobs) Consumed(id string) bool {
	r.marks.Lock()
	defer r.marks.Unlock()
	return r.consumed[id]
}

// Records the job as running, first insertion order kept, the registry's lock held
func (r *Jobs) insertRunning(j *Job) {
	if j.runSeq == 0 {
		r.seq++
		j.runSeq = r.seq
	}
	r.running[j.ID] = j
}

func (r *Jobs) insertFinished(j *Job) {
	if j.finSeq == 0 {
		r.seq++
		j.finSeq = r.seq
	}
	r.finished[j.ID] = j
}

// What a background job is started with
type Launch struct {
	// Settings of the profile it runs for
	Config  *config.Config
	Session string
	Command string
	// Folder it runs in
	Cwd string
	// Environment it runs in, its own processes on this machine when the environment is a Spawner
	Env Environment
	// Runs on a pseudo terminal it can be driven through
	Pty bool
	// Survives the end of its session
	Persist bool
	// Where its notices go
	Deliver Deliver
	// Tells the session's agent when it ends, with this many characters of its output, the default for zero
	Notify bool
	Chars  int
	// Tells the session's agent of the lines of its output holding one of these
	Patterns []string
	// Variables of this command alone, as Exec.Env sets them
	Vars map[string]string
	// Task of the agent that started it and the tool call that did, as hooks are told
	Task string
	Call string
}

func (r *Jobs) newJob(s Launch, command string) *Job {
	j := newJob("proc_"+strs.Random(6), command)
	j.Session, j.Config, j.Cwd, j.Persist = s.Session, s.Config, s.Cwd, s.Persist
	j.Task, j.Call = s.Task, s.Call
	j.wsl = wslLauncher(command)
	j.StartedAt = time.Now()
	j.deliver = s.Deliver
	j.notify, j.completionChars, j.patterns = s.Notify, s.Chars, append([]string(nil), s.Patterns...)
	r.notes.remember(s.Session, s.Deliver)
	return j
}

// Whether a job's process group dies with icebath
func (r *Jobs) owns(persist bool) bool {
	return groups && !persist && !r.outlive
}

// Script printing the job's start line, then running the command in a subshell so its own exit prints nothing, the outer shell silenced
// before it exits; the line goes out in two pieces so traced start-up output never prints it whole
func backgroundScript(id, command string) string {
	return "set +m; printf '%s%s\\n' " + Quote(startPrefix) + " " + Quote(id+"__") + "; ( :\n" + command + "\n)\n__icebath_rc=$?; exec >/dev/null 2>&1; (exit $__icebath_rc)"
}

// Starts a background job: a process of this machine when the environment is a Spawner, pipe or pseudo terminal, else a nohup inside
// the sandbox whose log is polled
func (r *Jobs) Spawn(ctx context.Context, s Launch) (*Job, error) {
	spawner, local := s.Env.(Spawner)
	if !local {
		return r.spawnInEnv(ctx, s)
	}
	j := r.newJob(s, s.Command)
	j.hostPid = true
	j.marker = startPrefix + j.ID + "__"
	script := backgroundScript(j.ID, RewriteCompoundBackground(s.Command))
	command, err := spawner.Command(ctx, script, s.Cwd)
	if err != nil {
		return nil, err
	}
	j.Cwd = command.Dir
	command.Env = append(slices.DeleteFunc(command.Env, func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		_, own := s.Vars[name]
		return name == SpawnEnv || own
	}), SpawnEnv+"="+SpawnTag(backgroundPurpose))
	for _, name := range slices.Sorted(maps.Keys(s.Vars)) {
		if value := s.Vars[name]; value != "" {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	suffix := j.ID
	if s.Pty {
		if !ptyAvailable() {
			j.ptyFallback = "the PTY library is not available on this host"
			slog.Warn("PTY support unavailable, falling back to pipe mode")
		} else if err := r.startPty(j, r.scope(s.Config, j, command, j.ID, "PTY")); err != nil {
			j.ptyFallback = fmt.Sprintf("the PTY spawn failed (%v)", err)
			slog.Warn(fmt.Sprintf("PTY spawn failed (%v), falling back to pipe mode", err))
			if j.unit != "" {
				if !stopUnit(j.unit) {
					return nil, fmt.Errorf("PTY scope could not be reaped; refusing pipe fallback to avoid duplicate command execution: %w", err)
				}
				j.unit, suffix = "", j.ID+"-pipe-fallback"
			}
		} else {
			return j, nil
		}
	}
	cmd := r.scope(s.Config, j, command, suffix, "Local").exec()
	NewSession(cmd)
	p, err := Start(cmd, nil)
	if err != nil {
		return nil, err
	}
	if r.owns(s.Persist) {
		p.Own(r.t.Supervisor)
		RegisterChild(s.Config, p.Pid(), backgroundPurpose)
	}
	j.proc, j.Pid = p, p.Pid()
	j.start, _ = StartTime(j.Pid)
	r.track(j, func() { r.readPipe(j) })
	return j, nil
}

func (c Command) exec() *exec.Cmd {
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Dir, cmd.Env = c.Dir, c.Env
	return cmd
}

// Starts the job on a pseudo terminal of 30 rows by 120 columns, pagers off so it never waits on one
func (r *Jobs) startPty(j *Job, command Command) error {
	env := slices.Clone(command.Env)
	for _, pager := range []string{"GIT_PAGER", "PAGER"} {
		if !slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, pager+"=") }) {
			env = append(env, pager+"=cat")
		}
	}
	p, err := startPty(command.Argv, command.Dir, env, 30, 120)
	if err != nil {
		return err
	}
	if r.owns(j.Persist) {
		r.t.Supervisor.Own(p.Pid())
		RegisterChild(j.Config, p.Pid(), backgroundPurpose)
	}
	j.pty, j.Pid = p, p.Pid()
	j.start, _ = StartTime(j.Pid)
	r.track(j, func() { r.readPty(j) })
	return nil
}

// Takes a still running foreground command into the background as a job, its completion notified
func (r *Jobs) Adopt(s Launch, p *HostProc, soFar string) *Job {
	j := r.newJob(s, s.Command)
	j.proc, j.Pid, j.hostPid = p, p.Pid(), true
	j.start, _ = StartTime(j.Pid)
	owned := p.owned
	p.owned = 0
	switch {
	case owned != 0 && r.owns(false):
		p.owned = owned
	case owned != 0:
		r.t.Supervisor.Release(owned)
	}
	if soFar != "" {
		j.add(soFar)
	}
	r.track(j, func() { r.readPipe(j) })
	return j
}

// Starts the job inside a sandbox with nohup, its output, pid and exit status left in files the poller reads
func (r *Jobs) spawnInEnv(ctx context.Context, s Launch) (*Job, error) {
	j := r.newJob(s, s.Command)
	j.env = s.Env
	j.marker = startPrefix + j.ID + "__"
	temp := strings.TrimRight(s.Env.TempDir(), "/")
	if !strings.HasPrefix(temp, "/") {
		temp = "/tmp"
	}
	log, pid, exit := fmt.Sprintf("%s/icebath_bg_%s.log", temp, j.ID), fmt.Sprintf("%s/icebath_bg_%s.pid", temp, j.ID), fmt.Sprintf("%s/icebath_bg_%s.exit", temp, j.ID)
	launch := fmt.Sprintf("mkdir -p %s && ( nohup bash -lc %s > %s 2>&1; rc=$?; printf '%%s\\n' \"$rc\" > %s ) & echo $! > %s && cat %s",
		Quote(temp), Quote(backgroundScript(j.ID, s.Command)), Quote(log), Quote(exit), Quote(pid), Quote(pid))
	result, err := s.Env.Execute(ctx, launch, Exec{Cwd: s.Cwd, Raw: true, Env: s.Vars})
	if err != nil {
		return nil, err
	}
	output := strings.TrimSpace(result.Output)
	for _, line := range strings.Split(output, "\n") {
		if n, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && n > 0 {
			j.Pid = n
			break
		}
	}
	if j.Pid == 0 {
		if output == "" {
			output = "(no output)"
		}
		return nil, fmt.Errorf("launch wrapper exited %d without a PID: %s", result.Code, output)
	}
	r.track(j, func() { r.poll(j, log, pid, exit) })
	return j, nil
}

// Registers the job, then starts its reader, so the reader can't publish a completion of a job not yet listed
func (r *Jobs) track(j *Job, reader func()) {
	j.readerDone = make(chan struct{})
	r.mu.Lock()
	r.pruneLocked()
	r.insertRunning(j)
	r.mu.Unlock()
	go func() {
		defer close(j.readerDone)
		reader()
	}()
	r.checkpoint(j.Config)
}

// Output of a job held back until its start line, which drops whatever the user's start-up files printed ahead of the command
type startup struct {
	marker  string
	head    string
	passing bool
	lineEnd bool
}

func newStartup(marker string) *startup {
	return &startup{marker: marker, passing: marker == ""}
}

// What to keep of a fresh chunk: nothing until the start line, everything after that line from then on
func (f *startup) feed(text string) string {
	if f.passing {
		return f.dropLineEnd(text)
	}
	f.head += text
	i := strings.Index(f.head, f.marker)
	if i < 0 {
		if len(f.head) > startupHeadMax {
			return f.flush()
		}
		return ""
	}
	rest := f.head[i+len(f.marker):]
	f.passing, f.head, f.lineEnd = true, "", true
	return f.dropLineEnd(rest)
}

func (f *startup) dropLineEnd(text string) string {
	for f.lineEnd && text != "" {
		switch text[0] {
		case '\r':
			text = text[1:]
		case '\n':
			text, f.lineEnd = text[1:], false
		default:
			f.lineEnd = false
		}
	}
	return text
}

// Start-up output still held when the start line never came, so a shell that died in its start-up files still shows why
func (f *startup) flush() string {
	head := f.head
	f.passing, f.head = true, ""
	return head
}

// Buffers a fresh chunk, then checks the watch patterns
func (r *Jobs) ingest(j *Job, text string, unlessExited bool) {
	if !unlessExited {
		j.add(text)
	} else if !j.addIfRunning(text) {
		return
	}
	r.matchPatterns(j, text)
}

// Reads a pipe job's output until it closes, or shortly after its direct child exited while a descendant holds the pipe open
func (r *Jobs) readPipe(j *Job) {
	start := newStartup(j.marker)
	dec := &decoder{}
	add := func(chunk string) {
		if chunk = start.feed(chunk); chunk != "" {
			r.ingest(j, chunk, false)
		}
	}
	stdout := j.proc.Output()
	j.selectable = true
	idle, drained := 0, 0
	buf := make([]byte, 4096)
	for {
		timeout := 200 * time.Millisecond
		if j.finishRequested.isSet() {
			timeout = 0
		}
		n, ready, err := readChunk(stdout, buf, timeout)
		if !ready && err == nil {
			if j.finishRequested.isSet() {
				break
			}
			if _, done := j.proc.Poll(); done {
				idle++
			}
			if idle >= 3 {
				break
			}
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Debug(fmt.Sprintf("Process stdout reader ended: %v", err))
			}
			break
		}
		chunk := dec.decode(buf[:n])
		add(chunk)
		if j.finishRequested.isSet() {
			if drained += utf8.RuneCountInString(chunk); drained >= finalDrainMax {
				break
			}
		}
		idle = 0
	}
	r.finishReader(j, dec, start, add, func() error { _, err := j.proc.Wait(0); return err }, func() *int {
		code, done := j.proc.Poll()
		if !done {
			return nil
		}
		return &code
	})
}

// Reads a pseudo terminal job's output, answering the terminal queries a program sends it
func (r *Jobs) readPty(j *Job) {
	dec := &decoder{}
	responder := newQueryResponder(30, 120)
	start := newStartup(j.marker)
	add := func(text string) {
		if text = start.feed(text); text != "" {
			r.ingest(j, text, true)
		}
	}
	buf := make([]byte, 4096)
	for j.pty.Alive() {
		n, err := j.pty.Read(buf)
		if err != nil {
			break
		}
		if n == 0 {
			continue
		}
		chunk, replies := responder.process(buf[:n])
		if len(replies) > 0 {
			if err := j.pty.Write(replies); err != nil {
				slog.Debug("PTY query response write failed", "error", err)
			}
		}
		add(dec.decode(chunk))
	}
	add(dec.decode(responder.flush()))
	r.finishReader(j, dec, start, add, func() error { j.pty.Wait(); return nil }, j.pty.ExitStatus)
}

// Flushes what the decoder and the start-up filter hold, reaps the child and records its exit
func (r *Jobs) finishReader(j *Job, dec *decoder, start *startup, add func(string), wait func() error, code func() *int) {
	add(dec.finish())
	add(start.flush())
	if err := wait(); err != nil {
		if code() == nil {
			slog.Warn(fmt.Sprintf("Wait for %s failed; leaving process tracked: %v", j.ID, err))
			return
		}
		slog.Warn(fmt.Sprintf("Wait for %s failed; recording known exit status: %v", j.ID, err))
	}
	r.finish(j, code(), "exited", "", true)
}

// Polls a sandbox job's log for new bytes and its pid for its end
func (r *Jobs) poll(j *Job, log, pid, exit string) {
	ctx := context.Background()
	start := newStartup(j.marker)
	offset := 0
	add := func(delta string) bool {
		if delta == "" {
			return true
		}
		if !j.addIfRunning(delta) {
			return false
		}
		r.matchPatterns(j, delta)
		return true
	}
	end := func(code *int, reason, source string) {
		add(start.flush())
		r.finish(j, code, reason, source, true)
	}
	lost := func() {
		code := -1
		end(&code, "lost", "backend_lost")
	}
	for !j.Exited() {
		time.Sleep(2 * time.Second)
		result, err := j.env.Execute(ctx, logDelta(Quote(log), offset), Exec{Timeout: 10 * time.Second, Raw: true})
		if err != nil {
			lost()
			return
		}
		header, delta, _ := strings.Cut(result.Output, "\n")
		size, used := -1, -1
		if parts := strings.Fields(header); len(parts) == 2 {
			a, errA := strconv.Atoi(parts[0])
			b, errB := strconv.Atoi(parts[1])
			if errA == nil && errB == nil {
				size, used = a, b
			}
		}
		if size < 0 {
			delta = ""
		} else {
			if used < offset {
				j.mu.Lock()
				gone := j.exited
				if !gone {
					j.output = ""
				}
				j.mu.Unlock()
				if gone {
					return
				}
			}
			offset = size
		}
		if !add(start.feed(delta)) {
			return
		}
		check, err := j.env.Execute(ctx, fmt.Sprintf("kill -0 \"$(cat %s 2>/dev/null)\" 2>/dev/null; echo $?", Quote(pid)), Exec{Timeout: 5 * time.Second, Raw: true})
		if err != nil {
			lost()
			return
		}
		lines := strings.Split(strings.TrimSpace(check.Output), "\n")
		if strings.TrimSpace(check.Output) == "" || strings.TrimSpace(lines[len(lines)-1]) == "0" {
			continue
		}
		status, err := j.env.Execute(ctx, "cat "+Quote(exit)+" 2>/dev/null", Exec{Timeout: 5 * time.Second, Raw: true})
		if err != nil {
			lost()
			return
		}
		code := -1
		if text := strings.TrimSpace(status.Output); text != "" {
			last := strings.Split(text, "\n")
			if n, err := strconv.Atoi(strings.TrimSpace(last[len(last)-1])); err == nil {
				code = n
			}
		}
		end(&code, "exited", "")
		return
	}
}

// Shell command printing a log's size and the offset used, then only the bytes written since offset, cut at a UTF-8 boundary
func logDelta(log string, offset int) string {
	return fmt.Sprintf("O=%d; ", offset) +
		fmt.Sprintf("S=$({ wc -c < %s; } 2>/dev/null | tr -dc '0-9'); ", log) +
		"S=${S:-0}; " +
		`if [ "$S" -lt "$O" ]; then O=0; fi; ` +
		`N=0; P=$S; while [ "$P" -gt "$O" ] && [ "$N" -lt 3 ]; do ` +
		fmt.Sprintf("B=$(tail -c +$P %s 2>/dev/null | head -c 1 | od -An -to1 | tr -dc '0-9'); ", log) +
		`case "$B" in 2[0-7][0-7]) P=$((P-1)); N=$((N+1));; *) break;; esac; done; ` +
		`if [ "$N" -gt 0 ] || [ "$P" -eq "$S" ]; then ` +
		fmt.Sprintf("B=$(tail -c +$P %s 2>/dev/null | head -c 1 | od -An -to1 | tr -dc '0-9'); ", log) +
		`case "$B" in 3[0-3][0-7]) L=2;; 3[4-5][0-7]) L=3;; 3[6-7][0-7]) L=4;; *) L=1;; esac; ` +
		`if [ "$L" -gt $((N+1)) ]; then S=$((P-1)); fi; fi; ` +
		`echo "$S $O"; ` +
		fmt.Sprintf(`if [ "$S" -gt "$O" ]; then tail -c +$((O+1)) %s 2>/dev/null | head -c $((S-O)); fi`, log)
}

// Records a reader's exit under the job's lock and finishes the job
func (r *Jobs) finish(j *Job, code *int, reason, source string, fromReader bool) {
	j.mu.Lock()
	j.markExited(code, reason, source)
	j.mu.Unlock()
	r.moveToFinished(j, fromReader)
}

// Moves a job from running to finished; only the first move publishes its completion
func (r *Jobs) moveToFinished(j *Job, fromReader bool) bool {
	r.mu.Lock()
	_, wasRunning := r.running[j.ID]
	if wasRunning {
		j.finishClaimed = true
		j.mu.Lock()
		j.exitedAt = time.Now()
		j.mu.Unlock()
		r.saveReceipt(j)
		delete(r.running, j.ID)
	}
	claimed := j.finishClaimed
	r.insertFinished(j)
	r.mu.Unlock()
	if j.proc != nil {
		j.proc.Release()
	} else if j.pty != nil && r.owns(j.Persist) {
		r.t.Supervisor.Release(j.Pid)
	}
	defer func() {
		if wasRunning || !claimed {
			j.completed.set()
		}
	}()
	r.releaseHandles(j, fromReader)
	r.checkpoint(j.Config)
	r.policy.forget(j.ID)
	if wasRunning {
		r.completion(j)
	}
	return wasRunning
}

// Closes a finished job's pipe and pseudo terminal, more than once harmless
func (r *Jobs) releaseHandles(j *Job, fromReader bool) {
	if j.proc != nil && j.proc.Output() != nil {
		j.proc.Output().Close()
	}
	if j.pty != nil && (fromReader || !j.readerAlive()) {
		j.pty.Close()
	}
}

// Drops finished jobs past their time, then the oldest finished one once too many are tracked, the registry's lock held
func (r *Jobs) pruneLocked() {
	var expired []string
	prunable := map[string]*Job{}
	for id, j := range r.finished {
		if !j.scopePending && (!j.finishClaimed || j.completed.isSet()) {
			prunable[id] = j
			if time.Since(j.StartedAt) > finishedTTL {
				expired = append(expired, id)
			}
		}
	}
	if len(r.running)+len(r.finished)-len(expired) >= maxJobs {
		oldest := ""
		for id, j := range prunable {
			if !slices.Contains(expired, id) && (oldest == "" || j.StartedAt.Before(r.finished[oldest].StartedAt) || j.StartedAt.Equal(r.finished[oldest].StartedAt) && id < oldest) {
				oldest = id
			}
		}
		if oldest != "" {
			expired = append(expired, oldest)
		}
	}
	for _, id := range expired {
		r.releaseHandles(r.finished[id], false)
		delete(r.finished, id)
	}
	r.marks.Lock()
	for id := range r.consumed {
		if r.running[id] == nil && r.finished[id] == nil {
			delete(r.consumed, id)
		}
	}
	r.marks.Unlock()
}

// Whether the command goes through a wsl launcher, whose pid is the short lived launcher's
func wslLauncher(command string) bool {
	if strings.TrimSpace(command) == "" {
		return false
	}
	var candidates []string
	if words, err := Split(command); err == nil && len(words) > 0 {
		candidates = append(candidates, words[0])
	}
	if words := strings.Fields(command); len(words) > 0 {
		candidates = append(candidates, words[0])
	}
	for _, first := range candidates {
		base := strings.ToLower(strings.Trim(filepath.Base(strings.ReplaceAll(first, `\`, "/")), `'"`))
		if base == "wsl" || base == "wsl.exe" {
			return true
		}
	}
	return false
}

// Every job of the sessions, running ones only, for the idle cleanup
func (r *Jobs) Running(sessions ...string) bool {
	r.mu.Lock()
	jobs := make([]*Job, 0, len(r.running))
	for _, j := range r.running {
		jobs = append(jobs, j)
	}
	r.mu.Unlock()
	for _, j := range jobs {
		r.refresh(j)
		if !j.Exited() && slices.Contains(sessions, j.Session) {
			return true
		}
	}
	return false
}

// Hands a running job of the session over to another session, such as a subagent's process taken over by the agent that delegated to it:
// that session owns it from now on and hears of its end in its place, the note saying why it came. The job, nil when the session owns no
// running job of the id
func (r *Jobs) Transfer(cfg *config.Config, from, id, to string, deliver Deliver, note string) *Job {
	j := r.Get(cfg, from, id)
	if j == nil || j.Session != from || j.Exited() {
		return nil
	}
	r.mu.Lock()
	j.Session = to
	r.mu.Unlock()
	j.mu.Lock()
	j.deliver, j.handoff = deliver, note
	j.mu.Unlock()
	r.notes.remember(to, deliver)
	r.checkpoint(j.Config)
	return j
}

// Job of the id or of a unique prefix of it, those finished and kept as receipts of the session included
func (r *Jobs) Get(cfg *config.Config, session, id string) *Job {
	if id == "" {
		return nil
	}
	r.mu.Lock()
	j := r.running[id]
	if j == nil {
		j = r.finished[id]
	}
	r.mu.Unlock()
	if j == nil && cfg != nil {
		j = r.receipts(cfg, session, id)[id]
	}
	if j == nil {
		j = r.prefix(cfg, session, id)
	}
	return r.refresh(j)
}

// Job whose id starts with the query, a bare tail read as proc_<tail>, never a guess between several
func (r *Jobs) prefix(cfg *config.Config, session, query string) *Job {
	query = strings.TrimSpace(query)
	if !strings.HasPrefix(query, "proc_") {
		query = "proc_" + query
	}
	if len(query)-len("proc_") < 4 {
		return nil
	}
	matches := map[string]*Job{}
	if cfg != nil {
		matches = r.receipts(cfg, session, query)
	}
	r.mu.Lock()
	for _, table := range []map[string]*Job{r.running, r.finished} {
		for id, j := range table {
			if strings.HasPrefix(id, query) {
				matches[id] = j
			}
		}
	}
	r.mu.Unlock()
	if len(matches) != 1 {
		return nil
	}
	for _, j := range matches {
		return j
	}
	return nil
}

// Checks a job recovered after a restart against its pid: still running, its pid recycled, or gone; nil once it was dropped, a job with
// a systemd scope kept among the finished so a kill can still stop what is left in it
func (r *Jobs) refresh(j *Job) *Job {
	if j == nil || j.Exited() || !j.Detached || !j.hostPid {
		return j
	}
	switch fate(j.Pid, j.start) {
	case "running":
		return j
	case "gone":
		j.mu.Lock()
		j.exited = true
		j.mu.Unlock()
		r.mu.Lock()
		delete(r.running, j.ID)
		delete(r.finished, j.ID)
		if j.unit != "" {
			r.insertFinished(j)
		}
		r.mu.Unlock()
		r.checkpoint(j.Config)
		j.completed.set()
		if j.unit != "" {
			return j
		}
		return nil
	}
	r.closeReused(j)
	return j
}

// Closes a recovered job whose pid now belongs to another process, never signalling that one
func (r *Jobs) closeReused(j *Job) {
	j.mu.Lock()
	if j.exited {
		j.mu.Unlock()
		return
	}
	j.exited, j.exitedAt = true, time.Now()
	j.mu.Unlock()
	r.mu.Lock()
	delete(r.running, j.ID)
	r.insertFinished(j)
	r.mu.Unlock()
	r.checkpoint(j.Config)
	j.completed.set()
}

// How a pid recorded with a start time fares: running, reused by another process, or gone
func fate(pid int, start int64) string {
	if ours(pid, start) {
		return "running"
	}
	if pid == 0 || !alive(pid) {
		return "gone"
	}
	if start == 0 {
		return "running"
	}
	current, ok := StartTime(pid)
	if !ok || current == start {
		return "running"
	}
	return "reused"
}

// Flips a local job whose direct child exited while a descendant still holds its pipe, letting the reader finish its drain first
func (r *Jobs) reconcile(j *Job) {
	if j == nil || j.Exited() || j.proc == nil {
		return
	}
	code, done := j.proc.Poll()
	if !done {
		return
	}
	if j.selectable && j.readerAlive() {
		j.finishRequested.set()
		if j.completed.wait(finalDrainWait) {
			return
		}
		j.mu.Lock()
		j.markExited(&code, "exited", "")
		j.mu.Unlock()
		slog.Info(fmt.Sprintf("Reconciled session %s: direct child exited with code %d; reader will publish the owned completion after its final drain.", j.ID, code))
		return
	}
	j.mu.Lock()
	j.markExited(&code, "exited", "")
	j.mu.Unlock()
	slog.Info(fmt.Sprintf("Reconciled session %s: direct child exited with code %d but reader was still blocked (orphaned pipe). Flipped to exited.", j.ID, code))
	r.moveToFinished(j, false)
}
