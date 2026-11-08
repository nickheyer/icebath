package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// In memory registry of background processes with rolling output, poll, log, wait, kill and crash recovery

const (
	// MaxOutputChars is the rolling output buffer size per process
	MaxOutputChars = 200_000
	// CompletionOutputChars is the output tail a completion notification carries
	CompletionOutputChars = 2000
	// FinishedTTLSeconds keeps finished processes listed for 30 minutes
	FinishedTTLSeconds = 1800
	// MaxProcesses caps the tracked processes before LRU pruning
	MaxProcesses = 64
	// WatchMinIntervalSeconds spaces watch match notifications per session
	WatchMinIntervalSeconds = 15
	// WatchStrikeLimit is the consecutive rate limit windows that disable watching
	WatchStrikeLimit = 3
	// WatchLifetimeMaxHits caps delivered matches over a session's life
	WatchLifetimeMaxHits = 8
	// HeartbeatMinSeconds floors the heartbeat interval
	HeartbeatMinSeconds = 60
	// HeartbeatTickSeconds is the heartbeat loop period
	HeartbeatTickSeconds = 5
	// WatchGlobalMaxPerWindow caps watch matches across every session per window
	WatchGlobalMaxPerWindow = 15
	// WatchGlobalWindowSeconds is the global watch window
	WatchGlobalWindowSeconds = 10
	// WatchGlobalCooldownSeconds is the global breaker cooldown
	WatchGlobalCooldownSeconds = 30
)

const (
	finalDrainMaxChars   = 1 << 20
	readerFinalDrainWait = time.Second
)

// Event a goroutine sets once and others wait on
type eventSignal struct {
	once sync.Once
	ch   chan struct{}
}

func newEventSignal() *eventSignal { return &eventSignal{ch: make(chan struct{})} }

func (e *eventSignal) set() { e.once.Do(func() { close(e.ch) }) }

func (e *eventSignal) isSet() bool {
	select {
	case <-e.ch:
		return true
	default:
		return false
	}
}

// Waits up to d for the signal, true when set
func (e *eventSignal) wait(d time.Duration) bool {
	if d <= 0 {
		return e.isSet()
	}
	select {
	case <-e.ch:
		return true
	case <-time.After(d):
		return e.isSet()
	}
}

// EventQueue is the unified background event queue the drain loops consume
type EventQueue struct {
	mu       sync.Mutex
	items    []map[string]any
	notEmpty chan struct{}
}

func newEventQueue() *EventQueue { return &EventQueue{notEmpty: make(chan struct{})} }

// Put appends an event and wakes waiters
func (q *EventQueue) Put(evt map[string]any) {
	q.mu.Lock()
	q.items = append(q.items, evt)
	close(q.notEmpty)
	q.notEmpty = make(chan struct{})
	q.mu.Unlock()
}

// GetNowait pops the oldest event, false when empty
func (q *EventQueue) GetNowait() (map[string]any, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil, false
	}
	evt := q.items[0]
	q.items = q.items[1:]
	return evt, true
}

// Empty reports whether nothing is queued
func (q *EventQueue) Empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) == 0
}

// Len is the number of queued events
func (q *EventQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Snapshot copies the queued events in order
func (q *EventQueue) Snapshot() []map[string]any {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]map[string]any{}, q.items...)
}

// Runs fn under the queue mutex with the live item slice, replacing it with what fn returns
func (q *EventQueue) withItems(fn func(items []map[string]any) []map[string]any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = fn(q.items)
}

// Blocks up to timeout for the queue to become non empty
func (q *EventQueue) waitNotEmpty(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			q.mu.Unlock()
			return
		}
		ch := q.notEmpty
		q.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		select {
		case <-ch:
		case <-time.After(remaining):
			return
		}
	}
}

func notFoundResult(sessionID string) map[string]any {
	return map[string]any{"status": "not_found", "error": "No process with ID " + sessionID}
}

// Last n characters of s
func tailChars(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	count := utf8.RuneCountInString(s)
	if count <= n {
		return s
	}
	skip := count - n
	i := 0
	for ; skip > 0; skip-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[i:]
}

// First n characters of s
func headChars(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for ; n > 0; n-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

// Last n chars of the session output with ANSI sequences stripped
func outputTail(s *ProcessSession, n int) string {
	return StripANSI(tailChars(s.OutputBuffer, n))
}

// OutputTail is the last n characters of a session output buffer
func OutputTail(s *ProcessSession, n int) string { return outputTail(s, n) }

// Output sized by the session's completion_output_chars plus output_cut when trimmed
func completionOutput(s *ProcessSession) map[string]any {
	limit := s.CompletionOutputChars
	if limit == 0 {
		limit = CompletionOutputChars
	}
	cut := utf8.RuneCountInString(s.OutputBuffer) - limit
	out := map[string]any{"output": outputTail(s, limit)}
	if cut > 0 {
		out["output_cut"] = cut
	}
	return out
}

// ProcessSession is a tracked background process with output buffering
type ProcessSession struct {
	ID                    string
	Command               string
	TaskID                string
	OwnerTaskID           string
	SessionKey            string
	Pid                   int
	Process               *ExecProcess
	EnvRef                Environment
	Cwd                   string
	StartedAt             float64
	HostStartTime         *int64
	Exited                bool
	ExitedAt              float64
	ExitCode              *int
	CompletionReason      string
	TerminationSource     string
	OutputBuffer          string
	MaxOutputChars        int
	Detached              bool
	PidScope              string
	SystemdUnit           string
	WslChain              bool
	HandoffNote           string
	PersistOnRelease      bool
	OutputLog             string
	PtyFallback           string
	WatcherPlatform       string
	WatcherChatID         string
	WatcherUserID         string
	WatcherUserName       string
	WatcherThreadID       string
	WatcherMessageID      string
	WatcherInterval       int
	ParentSessionID       string
	NotifyOnComplete      bool
	CompletionOutputChars int
	WatchPatterns         []string
	HeartbeatSeconds      int
	TotalOutputChars      int

	heartbeatLast           float64
	heartbeatTotalAtLast    int
	heartbeatSeq            int
	watchHits               int
	watchSuppressed         int
	watchDisabled           bool
	watchCooldownUntil      float64
	watchStrikeCandidate    bool
	watchConsecutiveStrikes int
	completionEvent         *eventSignal
	finishClaimed           bool
	scopeStopPending        bool
	mu                      sync.Mutex
	readerDone              chan struct{}
	readerFinishRequested   *eventSignal
	readerSelectable        bool
	pty                     *ptyProcess
	ownedPgid               int
	env                     *Env
	runSeq                  int64
	finSeq                  int64
}

// NewProcessSession builds a session with the hermes field defaults
func NewProcessSession(id, command string) *ProcessSession {
	return &ProcessSession{
		ID: id, Command: command, CompletionReason: "exited", MaxOutputChars: MaxOutputChars, PidScope: "host",
		WatchPatterns: []string{}, completionEvent: newEventSignal(), readerFinishRequested: newEventSignal(),
	}
}

// Owner defaults to the task when the session was built without one
func (s *ProcessSession) finishInit() *ProcessSession {
	if s.OwnerTaskID == "" {
		s.OwnerTaskID = s.TaskID
	}
	return s
}

// Appends to the rolling output buffer under the session lock
func (s *ProcessSession) AppendOutput(text string) {
	s.mu.Lock()
	s.appendLocked(text)
	s.mu.Unlock()
}

// Appends unless the session has exited, decided under the lock a kill holds
func (s *ProcessSession) appendOutputIfRunning(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Exited {
		return false
	}
	s.appendLocked(text)
	return true
}

func (s *ProcessSession) appendLocked(text string) {
	s.OutputBuffer += text
	s.TotalOutputChars += utf8.RuneCountInString(text)
	if utf8.RuneCountInString(s.OutputBuffer) > s.MaxOutputChars {
		s.OutputBuffer = tailChars(s.OutputBuffer, s.MaxOutputChars)
	}
}

// Records an exit without overwriting a kill that raced the observer
func (s *ProcessSession) markExited(exitCode *int, reason, source string) {
	s.Exited = true
	if s.CompletionReason != "killed" {
		s.ExitCode = exitCode
		s.CompletionReason = reason
		if source != "" {
			s.TerminationSource = source
		}
	}
}

// Reports the recorded exit, read under the session lock its writers hold
func (s *ProcessSession) isExited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Exited
}

func (s *ProcessSession) readerAlive() bool {
	if s.readerDone == nil {
		return false
	}
	select {
	case <-s.readerDone:
		return false
	default:
		return true
	}
}

// Watcher routing fields in event key order
var watcherRouteKeys = []string{"platform", "chat_id", "user_id", "user_name", "thread_id", "message_id"}

func (s *ProcessSession) watcherField(key string) string {
	switch key {
	case "platform":
		return s.WatcherPlatform
	case "chat_id":
		return s.WatcherChatID
	case "user_id":
		return s.WatcherUserID
	case "user_name":
		return s.WatcherUserName
	case "thread_id":
		return s.WatcherThreadID
	case "message_id":
		return s.WatcherMessageID
	}
	return ""
}

func (s *ProcessSession) setWatcherField(key, value string) {
	switch key {
	case "platform":
		s.WatcherPlatform = value
	case "chat_id":
		s.WatcherChatID = value
	case "user_id":
		s.WatcherUserID = value
	case "user_name":
		s.WatcherUserName = value
	case "thread_id":
		s.WatcherThreadID = value
	case "message_id":
		s.WatcherMessageID = value
	}
}

// Session fields persisted verbatim in the crash recovery checkpoint
var checkpointFields = []string{
	"command", "pid", "pid_scope", "host_start_time", "systemd_unit", "wsl_chain", "cwd",
	"started_at", "task_id", "owner_task_id", "session_key",
	"watcher_platform", "watcher_chat_id", "watcher_user_id", "watcher_user_name", "watcher_thread_id", "watcher_message_id",
	"watcher_interval", "parent_session_id", "notify_on_complete", "completion_output_chars", "watch_patterns",
	"heartbeat_seconds", "persist_on_release", "output_log",
}

// Value of a persisted session field by its hermes name
func (s *ProcessSession) field(name string) any {
	switch name {
	case "id":
		return s.ID
	case "command":
		return s.Command
	case "pid":
		if s.Pid == 0 {
			return nil
		}
		return s.Pid
	case "pid_scope":
		return s.PidScope
	case "host_start_time":
		if s.HostStartTime == nil {
			return nil
		}
		return *s.HostStartTime
	case "systemd_unit":
		return s.SystemdUnit
	case "wsl_chain":
		return s.WslChain
	case "cwd":
		if s.Cwd == "" {
			return nil
		}
		return s.Cwd
	case "started_at":
		return s.StartedAt
	case "task_id":
		return s.TaskID
	case "owner_task_id":
		return s.OwnerTaskID
	case "session_key":
		return s.SessionKey
	case "watcher_interval":
		return s.WatcherInterval
	case "parent_session_id":
		return s.ParentSessionID
	case "notify_on_complete":
		return s.NotifyOnComplete
	case "completion_output_chars":
		return s.CompletionOutputChars
	case "watch_patterns":
		return append([]string{}, s.WatchPatterns...)
	case "heartbeat_seconds":
		return s.HeartbeatSeconds
	case "persist_on_release":
		return s.PersistOnRelease
	case "output_log":
		return s.OutputLog
	case "exit_code":
		if s.ExitCode == nil {
			return nil
		}
		return *s.ExitCode
	case "completion_reason":
		return s.CompletionReason
	case "termination_source":
		return s.TerminationSource
	}
	if strings.HasPrefix(name, "watcher_") {
		return s.watcherField(strings.TrimPrefix(name, "watcher_"))
	}
	return nil
}

// Assigns a persisted field from its JSON value, the default when absent
func (s *ProcessSession) setField(name string, value any) {
	switch name {
	case "id":
		s.ID = asStr(value)
	case "command":
		s.Command = asStr(value)
	case "pid":
		s.Pid, _ = asIntValue(value)
	case "pid_scope":
		if v := asStr(value); v != "" {
			s.PidScope = v
		}
	case "host_start_time":
		if n, ok := asIntValue(value); ok {
			v := int64(n)
			s.HostStartTime = &v
		} else {
			s.HostStartTime = nil
		}
	case "systemd_unit":
		s.SystemdUnit = asStr(value)
	case "wsl_chain":
		s.WslChain, _ = value.(bool)
	case "cwd":
		s.Cwd = asStr(value)
	case "started_at":
		s.StartedAt = asFloatValue(value)
	case "task_id":
		s.TaskID = asStr(value)
	case "owner_task_id":
		s.OwnerTaskID = asStr(value)
	case "session_key":
		s.SessionKey = asStr(value)
	case "watcher_interval":
		s.WatcherInterval, _ = asIntValue(value)
	case "parent_session_id":
		s.ParentSessionID = asStr(value)
	case "notify_on_complete":
		s.NotifyOnComplete, _ = value.(bool)
	case "completion_output_chars":
		s.CompletionOutputChars, _ = asIntValue(value)
	case "watch_patterns":
		s.WatchPatterns = []string{}
		if list, ok := value.([]any); ok {
			for _, item := range list {
				s.WatchPatterns = append(s.WatchPatterns, asStr(item))
			}
		} else if list, ok := value.([]string); ok {
			s.WatchPatterns = append(s.WatchPatterns, list...)
		}
	case "heartbeat_seconds":
		s.HeartbeatSeconds, _ = asIntValue(value)
	case "persist_on_release":
		s.PersistOnRelease, _ = value.(bool)
	case "output_log":
		s.OutputLog = asStr(value)
	case "exit_code":
		if n, ok := asIntValue(value); ok {
			s.ExitCode = &n
		} else {
			s.ExitCode = nil
		}
	case "completion_reason":
		if v := asStr(value); v != "" {
			s.CompletionReason = v
		}
	case "termination_source":
		s.TerminationSource = asStr(value)
	default:
		if strings.HasPrefix(name, "watcher_") {
			s.setWatcherField(strings.TrimPrefix(name, "watcher_"), asStr(value))
		}
	}
}

func asFloatValue(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

const wslChainNote = "Spawned via a wsl[.exe] launcher: the recorded host PID is the short-lived " +
	"launcher, not the Linux-side workers. Inspect them with `wsl -e ps` / " +
	"`wsl --list --running` from the host."

var shellNoiseSubstrings = []string{
	"no job control in this shell", "cannot set terminal process group",
	"tcsetattr: Inappropriate ioctl for device",
}

// ProcessRegistry tracks running and finished background processes across executor goroutines and the gateway
type ProcessRegistry struct {
	mu       sync.Mutex
	running  map[string]*ProcessSession
	finished map[string]*ProcessSession
	// PendingWatchers is the side channel for check_interval watchers the gateway reads after a run
	PendingWatchers []map[string]any
	// CompletionQueue holds every background event, distinguished by type
	CompletionQueue *EventQueue
	markMu          sync.Mutex
	// Completions the agent already consumed via wait or log
	completionConsumed map[string]bool
	// Sessions merely observed exited via poll
	pollObserved                    map[string]bool
	globalWatchMu                   sync.Mutex
	globalWatchWindowStart          float64
	globalWatchTrippedUntil         float64
	globalWatchWindowHits           int
	globalWatchSuppressedDuringTrip int
	// OnOutput streams live output chunks from reader goroutines to a driver installed sink
	OnOutput func(s *ProcessSession, chunk string)
	// OnClose drops a read only terminal tab without killing the process
	OnClose             func(s *ProcessSession, processID string) error
	heartbeatRunning    bool
	completionsRestored bool
	jobsOutliveHost     bool
	ownedPgidMu         sync.Mutex
	seq                 int64
}

// Records the first insertion order of a session into the running table, caller holds the lock
func (r *ProcessRegistry) insertRunning(s *ProcessSession) {
	if s.runSeq == 0 {
		r.seq++
		s.runSeq = r.seq
	}
	r.running[s.ID] = s
}

// Records the first insertion order of a session into the finished table, caller holds the lock
func (r *ProcessRegistry) insertFinished(s *ProcessSession) {
	if s.finSeq == 0 {
		r.seq++
		s.finSeq = r.seq
	}
	r.finished[s.ID] = s
}

// Sessions of a table in first insertion order
func orderedSessions(table map[string]*ProcessSession, seqOf func(*ProcessSession) int64) []*ProcessSession {
	out := make([]*ProcessSession, 0, len(table))
	for _, s := range table {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return seqOf(out[i]) < seqOf(out[j]) })
	return out
}

// NewProcessRegistry returns an empty registry
func NewProcessRegistry() *ProcessRegistry {
	return &ProcessRegistry{
		running: map[string]*ProcessSession{}, finished: map[string]*ProcessSession{},
		PendingWatchers: []map[string]any{}, CompletionQueue: newEventQueue(),
		completionConsumed: map[string]bool{}, pollObserved: map[string]bool{},
	}
}

// Process wide registry every terminal spawn tracks into
var processRegistry = NewProcessRegistry()

// DefaultProcessRegistry is the process wide background process registry
func DefaultProcessRegistry() *ProcessRegistry { return processRegistry }

func (r *ProcessRegistry) markConsumed(id string) {
	r.markMu.Lock()
	r.completionConsumed[id] = true
	r.markMu.Unlock()
}

func (r *ProcessRegistry) markPollObserved(id string) {
	r.markMu.Lock()
	r.pollObserved[id] = true
	r.markMu.Unlock()
}

// Queues an event and emits it to the session host surface
func (r *ProcessRegistry) enqueue(env *Env, evt map[string]any) {
	r.CompletionQueue.Put(evt)
	taskID, _ := evt["task_id"].(string)
	env.Emit(Event{Kind: "process_event", Tool: "process_manage", TaskID: taskID, Text: formatProcessNotification(evt), Data: evt})
}

// ArmHeartbeat enables periodic heartbeat events for the session and returns the effective interval
func (r *ProcessRegistry) ArmHeartbeat(s *ProcessSession, seconds int) int {
	seconds = max(seconds, HeartbeatMinSeconds)
	s.HeartbeatSeconds = seconds
	s.heartbeatLast = nowSeconds()
	r.ensureHeartbeatThread()
	return seconds
}

func (r *ProcessRegistry) ensureHeartbeatThread() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.heartbeatRunning {
		return
	}
	r.heartbeatRunning = true
	go r.heartbeatLoop()
}

// One goroutine for every heartbeat session
func (r *ProcessRegistry) heartbeatLoop() {
	for {
		time.Sleep(HeartbeatTickSeconds * time.Second)
		now := nowSeconds()
		due := []*ProcessSession{}
		r.mu.Lock()
		for _, s := range r.running {
			if s.HeartbeatSeconds > 0 && !s.isExited() && now-s.heartbeatLast >= float64(s.HeartbeatSeconds) {
				due = append(due, s)
			}
		}
		r.mu.Unlock()
		for _, s := range due {
			r.emitHeartbeat(s, now)
		}
	}
}

// Queues a heartbeat carrying the output since the last one, skipping a tick with nothing new
func (r *ProcessRegistry) emitHeartbeat(s *ProcessSession, now float64) {
	s.heartbeatLast = now
	s.mu.Lock()
	delta := s.TotalOutputChars - s.heartbeatTotalAtLast
	output := ""
	if delta > 0 {
		output = tailChars(s.OutputBuffer, delta)
	}
	s.heartbeatTotalAtLast = s.TotalOutputChars
	s.mu.Unlock()
	if output == "" {
		return
	}
	output, omitted := heartbeatOutputTail(output, 0)
	s.heartbeatSeq++
	elapsed := 0
	if s.StartedAt != 0 {
		elapsed = int(now - s.StartedAt)
	}
	notification := r.watchEventBase(s)
	notification["type"] = "heartbeat"
	notification["seq"] = s.heartbeatSeq
	notification["interval"] = s.HeartbeatSeconds
	notification["elapsed"] = elapsed
	notification["output"] = output
	notification["omitted"] = omitted
	notification["started_at"] = s.StartedAt
	redactProcessResult(s.env, notification)
	r.enqueue(s.env, notification)
}

// Strips shell startup warnings from the beginning of output
func cleanShellNoise(text string) string {
	lines := strings.Split(text, "\n")
	for len(lines) > 0 {
		noisy := false
		for _, noise := range shellNoiseSubstrings {
			if strings.Contains(lines[0], noise) {
				noisy = true
				break
			}
		}
		if !noisy {
			break
		}
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}

// Forwards a chunk to the live output sink, never panics
func (r *ProcessRegistry) emitOutput(s *ProcessSession, chunk string) {
	sink := r.OnOutput
	if sink == nil || chunk == "" {
		return
	}
	defer func() { recover() }()
	sink(s, chunk)
}

// Scans a freshly read chunk for watch patterns and queues rate limited notifications
func (r *ProcessRegistry) checkWatchPatterns(s *ProcessSession, newText string) {
	if len(s.WatchPatterns) == 0 || s.watchDisabled || s.isExited() {
		return
	}
	matchedPattern := ""
	matchedLines := []string{}
	for _, line := range splitLines(newText) {
		for _, p := range s.WatchPatterns {
			if strings.Contains(line, p) {
				if matchedPattern == "" {
					matchedPattern = p
				}
				matchedLines = append(matchedLines, strings.TrimRight(line, " \t\r\n\v\f"))
				break
			}
		}
	}
	if len(matchedLines) == 0 {
		return
	}
	now := nowSeconds()
	s.mu.Lock()
	if s.watchCooldownUntil != 0 && now < s.watchCooldownUntil {
		s.watchSuppressed += len(matchedLines)
		if s.watchStrikeCandidate {
			s.mu.Unlock()
			return
		}
		s.watchStrikeCandidate = true
		s.watchConsecutiveStrikes++
		if s.watchConsecutiveStrikes < WatchStrikeLimit {
			s.mu.Unlock()
			return
		}
		s.watchDisabled = true
		s.NotifyOnComplete = true
		suppressed := s.watchSuppressed
		s.mu.Unlock()
		r.emitWatchDisabled(s, suppressed, fmt.Sprintf("%d consecutive rate-limit windows triggered (min spacing %ds). ", WatchStrikeLimit, WatchMinIntervalSeconds))
		return
	}
	if s.watchCooldownUntil != 0 && !s.watchStrikeCandidate {
		s.watchConsecutiveStrikes = 0
	}
	s.watchStrikeCandidate = false
	s.watchCooldownUntil = now + WatchMinIntervalSeconds
	s.watchHits++
	suppressed := s.watchSuppressed
	s.watchSuppressed = 0
	lifetimeExhausted := s.watchHits >= WatchLifetimeMaxHits
	if lifetimeExhausted {
		s.watchDisabled = true
		s.NotifyOnComplete = true
	}
	s.mu.Unlock()
	shown := matchedLines
	if len(shown) > 20 {
		shown = shown[:20]
	}
	output := strings.Join(shown, "\n")
	if utf8.RuneCountInString(output) > 2000 {
		output = headChars(output, 2000) + "\n...(truncated)"
	}
	if r.globalWatchAdmit(s.env, now) {
		notification := r.watchEventBase(s)
		notification["type"] = "watch_match"
		notification["pattern"] = matchedPattern
		notification["output"] = output
		notification["suppressed"] = suppressed
		redactProcessResult(s.env, notification)
		r.enqueue(s.env, notification)
	}
	if lifetimeExhausted {
		r.emitWatchDisabled(s, 0, fmt.Sprintf("reached the lifetime cap of %d delivered matches. ", WatchLifetimeMaxHits))
	}
}

// Queues the one shot watch_disabled summary
func (r *ProcessRegistry) emitWatchDisabled(s *ProcessSession, suppressed int, why string) {
	evt := r.watchEventBase(s)
	evt["type"] = "watch_disabled"
	evt["suppressed"] = suppressed
	evt["message"] = fmt.Sprintf("Watch patterns disabled for process %s — %sFalling back to notify_on_complete semantics; you'll get exactly one notification when the process exits.", s.ID, why)
	r.enqueue(s.env, evt)
}

// Session identity and watcher routing fields shared by every watch event
func (r *ProcessRegistry) watchEventBase(s *ProcessSession) map[string]any {
	evt := map[string]any{
		"event_id": randomHex(16), "started_at": s.StartedAt, "parent_session_id": s.ParentSessionID,
		"session_id": s.ID, "session_key": s.SessionKey, "task_id": s.TaskID, "owner_task_id": s.OwnerTaskID,
		"command": s.Command,
	}
	for _, key := range watcherRouteKeys {
		evt[key] = s.watcherField(key)
	}
	return evt
}

// Unaddressed all sessions watch breaker event
func globalWatchEvent(type_, message string, extra map[string]any) map[string]any {
	evt := map[string]any{
		"session_id": "", "session_key": "", "command": "", "type": type_, "message": message,
		"platform": "", "chat_id": "", "user_id": "", "user_name": "", "thread_id": "",
	}
	for k, v := range extra {
		evt[k] = v
	}
	return evt
}

// Reports whether a watch_match may pass the global breaker, queuing trip and release summaries
func (r *ProcessRegistry) globalWatchAdmit(env *Env, now float64) bool {
	events := []map[string]any{}
	r.globalWatchMu.Lock()
	if r.globalWatchTrippedUntil != 0 && now >= r.globalWatchTrippedUntil {
		suppressed := r.globalWatchSuppressedDuringTrip
		r.globalWatchTrippedUntil = 0
		r.globalWatchSuppressedDuringTrip = 0
		r.globalWatchWindowStart, r.globalWatchWindowHits = now, 0
		if suppressed > 0 {
			events = append(events, globalWatchEvent("watch_overflow_released",
				fmt.Sprintf("Watch-pattern notifications resumed. %d match event(s) were suppressed during the flood.", suppressed),
				map[string]any{"suppressed": suppressed}))
		}
	}
	var admit bool
	if r.globalWatchTrippedUntil != 0 && now < r.globalWatchTrippedUntil {
		r.globalWatchSuppressedDuringTrip++
		admit = false
	} else {
		if now-r.globalWatchWindowStart >= WatchGlobalWindowSeconds {
			r.globalWatchWindowStart, r.globalWatchWindowHits = now, 0
		}
		admit = r.globalWatchWindowHits < WatchGlobalMaxPerWindow
		if admit {
			r.globalWatchWindowHits++
		} else {
			r.globalWatchTrippedUntil = now + WatchGlobalCooldownSeconds
			r.globalWatchSuppressedDuringTrip++
			events = append(events, globalWatchEvent("watch_overflow_tripped",
				fmt.Sprintf("Watch-pattern overflow: >%d notifications in %ds across all processes. Suppressing further watch_match events for %ds.",
					WatchGlobalMaxPerWindow, WatchGlobalWindowSeconds, WatchGlobalCooldownSeconds), nil))
		}
	}
	r.globalWatchMu.Unlock()
	for _, msg := range events {
		r.enqueue(env, msg)
	}
	return admit
}

// Liveness of a host visible pid
func isHostPidAlive(pid int) bool {
	return procAlive(pid)
}

// Kernel start ticks for a host pid, nil when unavailable
func safeHostStartTime(pid int) *int64 { return safeHostStartTimeSeam(pid) }

// Start time probe swapped by tests that simulate a failing probe
var safeHostStartTimeSeam = func(pid int) *int64 {
	if pid <= 0 {
		return nil
	}
	start, ok := processStartTime(pid)
	if !ok {
		return nil
	}
	return &start
}

// Reports pid alive and still the process spawned, a missing baseline degrades to liveness
func hostPidIsOurs(pid int, expectedStart *int64) bool {
	if !isHostPidAlive(pid) {
		return false
	}
	if expectedStart == nil {
		return true
	}
	current := safeHostStartTime(pid)
	return current != nil && *current == *expectedStart
}

// How a recovered host pid should be supervised, running, reused or gone
func (r *ProcessRegistry) detachedHostFate(pid int, expectedStart *int64) string {
	if hostPidIsOurs(pid, expectedStart) {
		return "running"
	}
	if pid == 0 || !isHostPidAlive(pid) {
		return "gone"
	}
	if expectedStart == nil {
		return "running"
	}
	current := safeHostStartTime(pid)
	if current == nil || *current == *expectedStart {
		return "running"
	}
	return "reused"
}

// Re attaches, closes or prunes a recovered host pid session
func (r *ProcessRegistry) refreshDetachedSession(s *ProcessSession) *ProcessSession {
	if s == nil || s.isExited() || !s.Detached || s.PidScope != "host" || s.OutputLog != "" {
		return s
	}
	switch r.detachedHostFate(s.Pid, s.HostStartTime) {
	case "running":
		return s
	case "gone":
		return r.pruneUncollectedDetached(s)
	}
	r.closeReusedDetached(s)
	return s
}

// Closes an entry whose pid was recycled onto another process without signalling it
func (r *ProcessRegistry) closeReusedDetached(s *ProcessSession) {
	s.mu.Lock()
	if s.Exited {
		s.mu.Unlock()
		return
	}
	s.Exited = true
	s.mu.Unlock()
	r.mu.Lock()
	if _, ok := r.running[s.ID]; ok {
		s.mu.Lock()
		s.ExitedAt = nowSeconds()
		s.mu.Unlock()
		delete(r.running, s.ID)
	}
	r.insertFinished(s)
	r.mu.Unlock()
	r.writeCheckpoint(s.env, nil)
	s.completionEvent.set()
}

// Drops a recovered entry whose pid is gone, keeping an owned scope reachable for kill
func (r *ProcessRegistry) pruneUncollectedDetached(s *ProcessSession) *ProcessSession {
	s.mu.Lock()
	s.Exited = true
	s.mu.Unlock()
	r.mu.Lock()
	delete(r.running, s.ID)
	if s.SystemdUnit != "" {
		r.insertFinished(s)
	} else {
		delete(r.finished, s.ID)
	}
	r.mu.Unlock()
	r.writeCheckpoint(s.env, nil)
	s.completionEvent.set()
	if s.SystemdUnit != "" {
		return s
	}
	return nil
}

// Reports a detached entry closed without a collected exit whose pid is gone
func (r *ProcessRegistry) uncollectedGone(s *ProcessSession) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	uncollected := s.Exited && s.ExitCode == nil
	s.mu.Unlock()
	return uncollected && s.Detached && s.PidScope == "host" && !isHostPidAlive(s.Pid)
}

// terminal.<key> as a non negative float, zero disables, fallback when unreadable
func configSeconds(env *Env, key string, fallback float64) float64 {
	v := cfgTree(env).Float("terminal."+key, fallback)
	return max(v, 0)
}

// Grace between SIGTERM and the escalated SIGKILL, zero disables escalation
func (r *ProcessRegistry) daemonTermGraceSeconds(env *Env) float64 {
	return configSeconds(env, "daemon_term_grace_seconds", 2.0)
}

// Session context variable bound for the current task, the process env otherwise
func sessionEnvValue(env *Env, name string) string {
	vars, _ := sessionContextState(env)
	if v, ok := vars[name]; ok {
		return v
	}
	return os.Getenv(name)
}

// Session context variable of the turn, the process env otherwise
func SessionEnvValue(env *Env, name string) string { return sessionEnvValue(env, name) }

func (r *ProcessRegistry) newSession(env *Env, command, taskID, ownerTaskID, sessionKey, cwd string) *ProcessSession {
	s := NewProcessSession("proc_"+randomHex(6), command)
	s.TaskID, s.OwnerTaskID, s.SessionKey, s.Cwd = taskID, ownerTaskID, sessionKey, cwd
	s.ParentSessionID = sessionEnvValue(env, "ICEBATH_SESSION_ID")
	s.WslChain = isWslLauncherCommand(command)
	s.StartedAt = nowSeconds()
	s.env = env
	return s.finishInit()
}

// Writable sandbox temp dir for env backed background tasks
func envTempDir(e Environment) string {
	if e != nil {
		if dir := e.TempDir(); strings.HasPrefix(dir, "/") {
			if trimmed := strings.TrimRight(dir, "/"); trimmed != "" {
				return trimmed
			}
			return "/"
		}
	}
	return os.TempDir()
}

// Login shell argv for safeCommand wrapped in a transient systemd scope under the supervised gateway
func (r *ProcessRegistry) scopeArgv(s *ProcessSession, safeCommand, unitSuffix, label string) ([]string, error) {
	shell, err := FindShell()
	if err != nil {
		return nil, err
	}
	argv := []string{shell, "-lic", "set +m; " + safeCommand}
	inSupervisedGateway := isLinuxHost && isSupervisedGatewayProcess()
	if inSupervisedGateway && systemdRunUserScopeAvailable() {
		s.SystemdUnit = "icebath-worker-" + unitSuffix + ".scope"
		return buildSystemdScopeArgv(argv, unitSuffix), nil
	}
	if inSupervisedGateway {
		toolsLogger(s.env).Debug(fmt.Sprintf("%s background executor not isolated in a systemd scope (systemd-run --user unavailable); worker shares the gateway cgroup.", label))
	}
	return argv, nil
}

// Sanitized child env from the session's client toolchain with PYTHONUNBUFFERED so poll sees progress
func (r *ProcessRegistry) spawnEnv(env *Env, envVars map[string]string) (map[string]string, error) {
	base := environMap()
	if SessionToolchainEnviron != nil {
		base = SessionToolchainEnviron(env, base)
	}
	child, err := SanitizeSubprocessEnv(env, base, envVars)
	if err != nil {
		return nil, err
	}
	child = terminalEnv(child)
	child["PYTHONUNBUFFERED"] = "1"
	return child, nil
}

// Registers the session before its reader can publish completion
func (r *ProcessRegistry) trackStarted(s *ProcessSession, reader func()) {
	s.readerDone = make(chan struct{})
	r.mu.Lock()
	r.pruneIfNeeded()
	done := s.readerDone
	go func() {
		defer close(done)
		reader()
	}()
	r.insertRunning(s)
	r.mu.Unlock()
	r.writeCheckpoint(s.env, nil)
}

// PTY spawn for interactive CLI tools
func (r *ProcessRegistry) spawnLocalPty(s *ProcessSession, safeCommand string, envVars map[string]string) (*ProcessSession, error) {
	if !ptyAvailable() {
		return nil, errPtyUnavailable
	}
	ptyArgv, err := r.scopeArgv(s, safeCommand, s.ID, "PTY")
	if err != nil {
		return nil, err
	}
	ptyEnv, err := r.spawnEnv(s.env, envVars)
	if err != nil {
		return nil, err
	}
	if s.SystemdUnit != "" {
		ptyEnv = SystemdUserBusEnv(ptyEnv)
	}
	if _, ok := ptyEnv["GIT_PAGER"]; !ok {
		ptyEnv["GIT_PAGER"] = "cat"
	}
	if _, ok := ptyEnv["PAGER"]; !ok {
		ptyEnv["PAGER"] = "cat"
	}
	pty, err := spawnPty(ptyArgv, s.Cwd, ptyEnv, 30, 120)
	if err != nil {
		return nil, err
	}
	r.ownPtyJob(s, pty)
	s.Pid = pty.Pid()
	s.HostStartTime = safeHostStartTime(s.Pid)
	s.pty = pty
	r.trackStarted(s, func() { r.ptyReaderLoop(s) })
	return s, nil
}

var errPtyUnavailable = errors.New("the PTY library is not available on this host")

// SpawnLocalOptions tunes a local background spawn
type SpawnLocalOptions struct {
	Cwd              string
	TaskID           string
	SessionKey       string
	EnvVars          map[string]string
	UsePty           bool
	OwnerTaskID      string
	PersistOnRelease bool
	RestoreLoginPath bool
}

// SpawnLocal starts a background process on the host, pipe mode or a PTY with pipe fallback
func (r *ProcessRegistry) SpawnLocal(env *Env, command string, o SpawnLocalOptions) (*ProcessSession, error) {
	parentPath := ""
	if o.RestoreLoginPath {
		spawnEnv, err := r.spawnEnv(env, o.EnvVars)
		if err != nil {
			return nil, err
		}
		parentPath = spawnEnv["PATH"]
	}
	safeCommand := appendDroppedPathEntries(RewriteCompoundBackground(command), parentPath)
	cwd := o.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	s := r.newSession(env, command, o.TaskID, o.OwnerTaskID, o.SessionKey, resolveSafeCwd(env, cwd))
	s.PersistOnRelease = o.PersistOnRelease
	ptyScopeAttempted := false
	log := workerOutputLog(env, s.ID)
	if o.UsePty && log == "" {
		spawned, err := r.spawnLocalPty(s, safeCommand, o.EnvVars)
		if err == nil {
			return spawned, nil
		}
		if errors.Is(err, errPtyUnavailable) {
			s.PtyFallback = "the PTY library is not available on this host"
			toolsLogger(env).Warn("PTY support unavailable, falling back to pipe mode")
		} else {
			s.PtyFallback = fmt.Sprintf("the PTY spawn failed (%v)", err)
			toolsLogger(env).Warn(fmt.Sprintf("PTY spawn failed (%v), falling back to pipe mode", err))
			if s.SystemdUnit != "" {
				ptyScopeAttempted = true
				if !stopSystemdUnit(s.SystemdUnit) {
					return nil, fmt.Errorf("PTY scope could not be reaped; refusing pipe fallback to avoid duplicate command execution: %w", err)
				}
				s.SystemdUnit = ""
			}
		}
	}
	unitSuffix := s.ID
	if ptyScopeAttempted {
		unitSuffix = s.ID + "-pipe-fallback"
	}
	spawnCommand := safeCommand
	if log != "" {
		spawnCommand = recordExitCommand(log, safeCommand)
	}
	spawnArgv, err := r.scopeArgv(s, spawnCommand, unitSuffix, "Local")
	if err != nil {
		return nil, err
	}
	spawnEnv, err := r.spawnEnv(env, o.EnvVars)
	if err != nil {
		return nil, err
	}
	if s.SystemdUnit != "" {
		spawnEnv = SystemdUserBusEnv(spawnEnv)
	}
	var logFile *os.File
	if log != "" {
		logFile, err = openOutputLog(log)
		if err != nil {
			return nil, err
		}
	}
	proc, err := r.spawnJob(s, spawnArgv, o.PersistOnRelease, s.Cwd, spawnEnv, logFile)
	if logFile != nil {
		logFile.Close()
	}
	if err != nil {
		return nil, err
	}
	s.Process, s.OutputLog = proc, log
	s.Pid = proc.Pid()
	s.HostStartTime = safeHostStartTime(s.Pid)
	if log != "" {
		r.trackStarted(s, func() { r.logReaderLoop(s) })
	} else {
		r.trackStarted(s, func() { r.readerLoop(s) })
	}
	return s, nil
}

// AdoptLocal takes over a still running foreground process as a tracked background session
func (r *ProcessRegistry) AdoptLocal(env *Env, proc *ExecProcess, command, cwd, taskID, sessionKey, ownerTaskID, outputSoFar string, notifyOnComplete bool) *ProcessSession {
	s := r.newSession(env, command, taskID, ownerTaskID, sessionKey, cwd)
	s.Process = proc
	s.Pid = proc.Pid()
	s.HostStartTime = safeHostStartTime(s.Pid)
	s.NotifyOnComplete = notifyOnComplete
	r.takeHandoffGroup(s, proc)
	if outputSoFar != "" {
		s.AppendOutput(outputSoFar)
	}
	r.trackStarted(s, func() { r.readerLoop(s) })
	return s
}

// SpawnViaEnv starts a background process inside a non local backend's sandbox and polls its log
func (r *ProcessRegistry) SpawnViaEnv(ctx context.Context, env *Env, e Environment, command, cwd, taskID, sessionKey string, timeout int, ownerTaskID string, persistOnRelease bool) (*ProcessSession, error) {
	s := r.newSession(env, command, taskID, ownerTaskID, sessionKey, cwd)
	s.EnvRef = e
	s.PidScope = "sandbox"
	s.PersistOnRelease = persistOnRelease
	tempDir := envTempDir(e)
	logPath := fmt.Sprintf("%s/icebath_bg_%s.log", tempDir, s.ID)
	pidPath := fmt.Sprintf("%s/icebath_bg_%s.pid", tempDir, s.ID)
	exitPath := fmt.Sprintf("%s/icebath_bg_%s.exit", tempDir, s.ID)
	bgCommand := fmt.Sprintf("mkdir -p %s && ( nohup bash -lc %s > %s 2>&1; rc=$?; printf '%%s\\n' \"$rc\" > %s ) & echo $! > %s && cat %s",
		shQuote(tempDir), shQuote(command), shQuote(logPath), shQuote(exitPath), shQuote(pidPath), shQuote(pidPath))
	result, err := e.Execute(ctx, bgCommand, ExecOptions{Cwd: cwd, Timeout: timeout, NoCompoundRewrite: true})
	if err != nil {
		return nil, err
	}
	output := strings.TrimSpace(result.Output)
	for _, ln := range strings.Split(output, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" && allDigits([]byte(ln)) {
			s.Pid, _ = strconv.Atoi(ln)
			break
		}
	}
	if s.Pid == 0 {
		shown := output
		if shown == "" {
			shown = "(no output)"
		}
		return nil, fmt.Errorf("launch wrapper exited %d without a PID: %s", result.ReturnCode, shown)
	}
	r.trackStarted(s, func() { r.envPollerLoop(s, e, logPath, pidPath, exitPath) })
	return s, nil
}

// Reads stdout of a local process, stopping shortly after the direct child exits on an orphaned pipe
func (r *ProcessRegistry) readerLoop(s *ProcessSession) {
	headNoise := true
	decoder := &utf8Decoder{}
	appendChunk := func(chunk string) {
		if headNoise {
			chunk = cleanShellNoise(chunk)
			headNoise = strings.TrimSpace(chunk) == ""
		}
		r.ingestOutput(s, chunk, false)
	}
	proc := s.Process
	if proc == nil || proc.Stdout() == nil {
		return
	}
	stdout := proc.Stdout()
	s.readerSelectable = !isWindowsHost
	idleAfterExit := 0
	drainedAfterRequest := 0
	buf := make([]byte, 4096)
	for {
		wait := 200 * time.Millisecond
		if s.readerSelectable && s.readerFinishRequested.isSet() {
			wait = 0
		}
		n, ready, err := readChunkTimeout(stdout, buf, wait)
		if !ready && err == nil {
			if !s.readerSelectable {
				continue
			}
			if s.readerFinishRequested.isSet() {
				break
			}
			if _, done := proc.Poll(); done {
				idleAfterExit++
			}
			if idleAfterExit >= 3 {
				break
			}
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				toolsLogger(s.env).Debug(fmt.Sprintf("Process stdout reader ended: %v", err))
			}
			break
		}
		chunk := decoder.decode(buf[:n])
		if chunk != "" {
			appendChunk(chunk)
		}
		if s.readerFinishRequested.isSet() {
			drainedAfterRequest += utf8.RuneCountInString(chunk)
			if drainedAfterRequest >= finalDrainMaxChars {
				break
			}
		}
		idleAfterExit = 0
	}
	r.finishReader(s, decoder, appendChunk, "Process",
		func() error { _, err := proc.Wait(0); return err },
		func() *int {
			code, done := proc.Poll()
			if !done {
				return nil
			}
			return &code
		})
}

// Reader teardown, flushes the decoder, reaps the child and records the exit
func (r *ProcessRegistry) finishReader(s *ProcessSession, decoder *utf8Decoder, appendFn func(string), label string, wait func() error, exitCode func() *int) {
	if tail := decoder.finish(); tail != "" {
		appendFn(tail)
	}
	if err := wait(); err != nil {
		if exitCode() == nil {
			toolsLogger(s.env).Warn(fmt.Sprintf("%s wait failed; leaving process tracked: %v", label, err))
			return
		}
		toolsLogger(s.env).Warn(fmt.Sprintf("%s wait failed; recording known exit status: %v", label, err))
	}
	r.finishExited(s, exitCode(), "exited", "", true)
}

// Polls a sandbox log file for non local backends
func (r *ProcessRegistry) envPollerLoop(s *ProcessSession, e Environment, logPath, pidPath, exitPath string) {
	prevOutputBytes := 0
	ctx := context.Background()
	lost := func() { r.finishExited(s, procIntPtr(-1), "lost", "backend_lost", true) }
	for !s.isExited() {
		time.Sleep(2 * time.Second)
		res, err := e.Execute(ctx, logDeltaCommand(shQuote(logPath), prevOutputBytes), ExecOptions{Timeout: 10})
		if err != nil {
			lost()
			return
		}
		header, delta, _ := strings.Cut(res.Output, "\n")
		parts := strings.Fields(header)
		newSize, usedOffset := -1, -1
		if len(parts) == 2 {
			a, errA := strconv.Atoi(parts[0])
			b, errB := strconv.Atoi(parts[1])
			if errA == nil && errB == nil {
				newSize, usedOffset = a, b
			}
		}
		if newSize < 0 {
			delta = ""
		} else {
			if usedOffset < prevOutputBytes {
				s.mu.Lock()
				if s.Exited {
					s.mu.Unlock()
					return
				}
				s.OutputBuffer = ""
				s.mu.Unlock()
			}
			prevOutputBytes = newSize
		}
		if delta != "" {
			s.mu.Lock()
			if s.Exited {
				s.mu.Unlock()
				return
			}
			s.OutputBuffer += delta
			if utf8.RuneCountInString(s.OutputBuffer) > s.MaxOutputChars {
				s.OutputBuffer = tailChars(s.OutputBuffer, s.MaxOutputChars)
			}
			s.mu.Unlock()
			r.checkWatchPatterns(s, delta)
			r.emitOutput(s, delta)
		}
		check, err := e.Execute(ctx, fmt.Sprintf("kill -0 \"$(cat %s 2>/dev/null)\" 2>/dev/null; echo $?", shQuote(pidPath)), ExecOptions{Timeout: 5})
		if err != nil {
			lost()
			return
		}
		checkOutput := strings.TrimSpace(check.Output)
		if checkOutput != "" {
			lines := strings.Split(checkOutput, "\n")
			if strings.TrimSpace(lines[len(lines)-1]) != "0" {
				exitRes, err := e.Execute(ctx, fmt.Sprintf("cat %s 2>/dev/null", shQuote(exitPath)), ExecOptions{Timeout: 5})
				if err != nil {
					lost()
					return
				}
				code := -1
				exitStr := strings.TrimSpace(exitRes.Output)
				if exitStr != "" {
					exitLines := strings.Split(exitStr, "\n")
					if n, err := strconv.Atoi(strings.TrimSpace(exitLines[len(exitLines)-1])); err == nil {
						code = n
					}
				}
				r.finishExited(s, &code, "exited", "", true)
				return
			}
		}
	}
}

func procIntPtr(n int) *int { return &n }

// Reads output from a PTY process, answering terminal queries on POSIX
func (r *ProcessRegistry) ptyReaderLoop(s *ProcessSession) {
	pty := s.pty
	decoder := &utf8Decoder{}
	var responder *PtyQueryResponder
	if !isWindowsHost {
		responder = NewPtyQueryResponder(30, 120)
	}
	ingest := func(text string) { r.ingestOutput(s, text, true) }
	buf := make([]byte, 4096)
	for pty.IsAlive() {
		n, err := pty.Read(buf)
		if err != nil {
			break
		}
		if n == 0 {
			continue
		}
		chunk := buf[:n]
		if responder != nil {
			var replies []byte
			chunk, replies = responder.Process(chunk)
			if len(replies) > 0 {
				if err := pty.Write(replies); err != nil {
					toolsLogger(s.env).Debug("PTY query response write failed", "error", err)
				}
			}
		}
		if text := decoder.decode(chunk); text != "" {
			ingest(text)
		}
	}
	if responder != nil {
		if tail := decoder.decode(responder.Flush()); tail != "" {
			ingest(tail)
		}
	}
	r.finishReader(s, decoder, ingest, "PTY", func() error { pty.Wait(); return nil }, pty.ExitStatus)
}

// Buffers a freshly read chunk, then scans watch patterns and streams it live
func (r *ProcessRegistry) ingestOutput(s *ProcessSession, text string, unlessExited bool) {
	if !unlessExited {
		s.AppendOutput(text)
	} else if !s.appendOutputIfRunning(text) {
		return
	}
	r.checkWatchPatterns(s, text)
	r.emitOutput(s, text)
}

// Marks a reader observed exit under the session lock and finishes
func (r *ProcessRegistry) finishExited(s *ProcessSession, exitCode *int, reason, source string, fromReader bool) {
	s.mu.Lock()
	s.markExited(exitCode, reason, source)
	s.mu.Unlock()
	r.moveToFinished(s, fromReader)
}

// Moves a session from running to finished, only the first move queues the completion
func (r *ProcessRegistry) moveToFinished(s *ProcessSession, fromReader bool) bool {
	r.mu.Lock()
	_, wasRunning := r.running[s.ID]
	if wasRunning {
		s.finishClaimed = true
		s.mu.Lock()
		s.ExitedAt = nowSeconds()
		s.mu.Unlock()
		saveCompletedResult(s.env, s)
		delete(r.running, s.ID)
	}
	claimed := s.finishClaimed
	r.insertFinished(s)
	r.mu.Unlock()
	r.releaseJobGroup(s)
	func() {
		defer func() {
			if wasRunning || !claimed {
				s.completionEvent.set()
			}
		}()
		r.publishFinished(s, wasRunning, fromReader)
	}()
	return wasRunning
}

func (r *ProcessRegistry) publishFinished(s *ProcessSession, wasRunning, fromReader bool) {
	r.releaseFinishedHandles(s, fromReader)
	r.writeCheckpoint(s.env, nil)
	if wasRunning && s.NotifyOnComplete {
		notification := map[string]any{
			"type": "completion", "session_id": s.ID, "session_key": s.SessionKey, "task_id": s.TaskID,
			"owner_task_id": s.OwnerTaskID, "command": s.Command,
		}
		if s.HandoffNote != "" {
			notification["handoff_note"] = s.HandoffNote
		}
		s.mu.Lock()
		for k, v := range exitFields(s) {
			notification[k] = v
		}
		for k, v := range completionOutput(s) {
			notification[k] = v
		}
		s.mu.Unlock()
		notification["started_at"] = s.StartedAt
		redactProcessResult(s.env, notification)
		r.enqueue(s.env, notification)
	}
}

// Exit code, completion reason and termination source, read under the session lock
func exitFields(s *ProcessSession) map[string]any {
	return map[string]any{
		"exit_code": s.field("exit_code"), "completion_reason": s.CompletionReason, "termination_source": s.TerminationSource,
	}
}

// Closes a finished session's pipe and PTY handles, idempotent
func (r *ProcessRegistry) releaseFinishedHandles(s *ProcessSession, fromReader bool) {
	if s.Process != nil && s.Process.Stdout() != nil {
		s.Process.Stdout().Close()
	}
	if s.pty != nil {
		if !isWindowsHost && s.readerAlive() && !fromReader {
			return
		}
		s.pty.Close()
	}
}

// A reconcile set exited while the reader that owns the output is still on its final drain
func readerFinalizing(s *ProcessSession) bool {
	return s.readerFinishRequested.isSet() && !s.completionEvent.isSet() && s.readerAlive()
}

// IsCompletionConsumed reports a completion already consumed via wait or log
func (r *ProcessRegistry) IsCompletionConsumed(sessionID string) bool {
	r.markMu.Lock()
	defer r.markMu.Unlock()
	return r.completionConsumed[sessionID]
}

func (r *ProcessRegistry) isPollObserved(sessionID string) bool {
	r.markMu.Lock()
	defer r.markMu.Unlock()
	return r.pollObserved[sessionID]
}

// IsSessionWaiting reports whether a goal loop barrier should stay parked on the session
func (r *ProcessRegistry) IsSessionWaiting(sessionID string) bool {
	var s *ProcessSession
	if sessionID != "" {
		r.mu.Lock()
		s = r.running[sessionID]
		if s == nil {
			s = r.finished[sessionID]
		}
		r.mu.Unlock()
	}
	if s == nil {
		return false
	}
	refreshed := r.refreshDetachedSession(s)
	if refreshed == nil {
		return false
	}
	s = refreshed
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.Exited && !(len(s.WatchPatterns) > 0 && !s.watchDisabled && s.watchHits > 0)
}

// PendingCompletionWait reports which notify_on_complete processes a one shot exit waited for
type PendingCompletionWait struct {
	Waited    []string
	Completed []string
	TimedOut  []string
}

// WaitForPendingCompletions lingers at one shot exit for notify_on_complete background processes
func (r *ProcessRegistry) WaitForPendingCompletions(ctx context.Context, env *Env, taskID string, timeout *float64, pollInterval float64) PendingCompletionWait {
	limit := r.oneshotCompletionWaitSeconds(env)
	if timeout != nil {
		limit = *timeout
	}
	result := PendingCompletionWait{Waited: []string{}, Completed: []string{}, TimedOut: []string{}}
	pending := []*ProcessSession{}
	r.mu.Lock()
	for _, store := range []map[string]*ProcessSession{r.running, r.finished} {
		for _, s := range store {
			if s.NotifyOnComplete && !s.completionEvent.isSet() && (taskID == "" || s.OwnerTaskID == taskID) {
				pending = append(pending, s)
			}
		}
	}
	r.mu.Unlock()
	if len(pending) == 0 || limit <= 0 {
		return result
	}
	ids := []string{}
	for _, s := range pending {
		ids = append(ids, s.ID)
	}
	result.Waited = ids
	toolsLogger(env).Info(fmt.Sprintf("One-shot exit lingering (bounded %gs) for %d notify_on_complete background process(es): %s", limit, len(pending), strings.Join(ids, ", ")))
	deadline := time.Now().Add(time.Duration(max(limit, 0) * float64(time.Second)))
	interval := time.Duration(max(pollInterval, 0.05) * float64(time.Second))
	interrupted := false
	for _, s := range pending {
		for !s.completionEvent.isSet() {
			if interrupted || IsInterrupted(ctx) {
				interrupted = true
				break
			}
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			r.reconcileLocalExit(s)
			if r.refreshDetachedSession(s) == nil {
				break
			}
			if s.completionEvent.isSet() {
				break
			}
			s.completionEvent.wait(min(remaining, interval))
		}
		if s.completionEvent.isSet() {
			result.Completed = append(result.Completed, s.ID)
		} else {
			result.TimedOut = append(result.TimedOut, s.ID)
		}
	}
	if len(result.TimedOut) > 0 {
		toolsLogger(env).Warn(fmt.Sprintf("One-shot exit linger timed out after %gs with %d background process(es) still running: %s — they may be killed when this process exits.", limit, len(result.TimedOut), strings.Join(result.TimedOut, ", ")))
	}
	return result
}

// Linger for one shot exits with pending notify_on_complete processes, zero disables
func (r *ProcessRegistry) oneshotCompletionWaitSeconds(env *Env) float64 {
	return configSeconds(env, "oneshot_completion_wait_seconds", 600.0)
}

// Skips a completion the CLI agent already has this turn
func (r *ProcessRegistry) drainShouldSkip(sessionID string, skipPollObserved bool) bool {
	return r.IsCompletionConsumed(sessionID) || (skipPollObserved && r.isPollObserved(sessionID))
}

// delegation.surface_child_process_notifications, false on any config error
func surfaceChildProcessNotifications(env *Env) bool {
	return cfgTree(env).Bool("delegation.surface_child_process_notifications", false)
}

// Routing verdict for one drained event, false means requeue
func ownsEvent(evt map[string]any, sessionKey string, owns func(map[string]any) bool, isAsyncDelegation bool) bool {
	evtSessionKey := asStr(evt["session_key"])
	origin := asStr(evt["origin_ui_session_id"])
	requiresPositiveProof := isAsyncDelegation || evtSessionKey != "" || origin != ""
	if owns != nil && requiresPositiveProof {
		verdict := false
		func() {
			defer func() {
				if recover() != nil {
					verdict = false
				}
			}()
			verdict = owns(evt)
		}()
		return verdict
	}
	if sessionKey != "" && requiresPositiveProof {
		return evtSessionKey == sessionKey
	}
	restored, _ := evt["restored"].(bool)
	return !(isAsyncDelegation && restored)
}

// RestoreCompletions rehydrates durable delegation completions once per process
func (r *ProcessRegistry) RestoreCompletions() int {
	r.mu.Lock()
	if r.completionsRestored {
		r.mu.Unlock()
		return 0
	}
	r.completionsRestored = true
	r.mu.Unlock()
	if AsyncDelegationLedger == nil {
		return 0
	}
	n, err := AsyncDelegationLedger.RestoreUndelivered(r.CompletionQueue)
	if err != nil {
		toolsLogger(nil).Warn(fmt.Sprintf("Could not restore async delegation completions: %v", err))
		return 0
	}
	return n
}

// DrainedNotification is one drained event with its formatted text
type DrainedNotification struct {
	Event map[string]any
	Text  string
}

// DrainNotifications pops every pending event the caller owns and formats it, requeueing foreign ones
func (r *ProcessRegistry) DrainNotifications(env *Env, sessionKey string, owns func(map[string]any) bool, skipPollObserved bool) []DrainedNotification {
	r.RestoreCompletions()
	results := []DrainedNotification{}
	requeue := []map[string]any{}
	var surfaceChild *bool
	for !r.CompletionQueue.Empty() {
		evt, ok := r.CompletionQueue.GetNowait()
		if !ok {
			break
		}
		isAsyncDelegation := evt["type"] == "async_delegation"
		if !ownsEvent(evt, sessionKey, owns, isAsyncDelegation) {
			requeue = append(requeue, evt)
			continue
		}
		evtSID := asStr(evt["session_id"])
		if evt["type"] == "completion" && r.drainShouldSkip(evtSID, skipPollObserved) {
			continue
		}
		evtTaskID := asStr(evt["owner_task_id"])
		if evtTaskID == "" {
			evtTaskID = asStr(evt["task_id"])
		}
		if isChildProcessNotification(evt) {
			if surfaceChild == nil {
				v := surfaceChildProcessNotifications(env)
				surfaceChild = &v
			}
			if !shouldSurfaceNotification(env, evt, surfaceChild) {
				evtType := "completion"
				if t := asStr(evt["type"]); t != "" {
					evtType = t
				}
				toolsLogger(env).Debug(fmt.Sprintf("Suppressed subagent-owned process notification (delegation.surface_child_process_notifications=false): type=%s session_id=%s task_id=%s", evtType, evtSID, evtTaskID))
				continue
			}
		}
		if text := formatProcessNotification(evt); text != "" {
			results = append(results, DrainedNotification{Event: evt, Text: text})
		}
	}
	for _, evt := range requeue {
		r.CompletionQueue.Put(evt)
	}
	return results
}

// Minimum suffix chars for prefix resolution
const minPrefixChars = 4

// Get returns the session by full id or unique prefix, never a guess
func (r *ProcessRegistry) Get(env *Env, sessionID string) *ProcessSession {
	if sessionID == "" {
		return nil
	}
	r.mu.Lock()
	s := r.running[sessionID]
	if s == nil {
		s = r.finished[sessionID]
	}
	r.mu.Unlock()
	if s == nil {
		s = loadCompletedResults(env, sessionID)[sessionID]
	}
	if s == nil {
		s = r.resolvePrefix(env, sessionID)
	}
	return r.refreshDetachedSession(s)
}

// Resolves a unique session id prefix, a bare hex tail is normalized to proc_<tail>
func (r *ProcessRegistry) resolvePrefix(env *Env, sessionID string) *ProcessSession {
	query := strings.TrimSpace(sessionID)
	if query == "" {
		return nil
	}
	if !strings.HasPrefix(query, "proc_") {
		query = "proc_" + query
	}
	if len(query)-len("proc_") < minPrefixChars {
		return nil
	}
	matches := loadCompletedResults(env, query)
	r.mu.Lock()
	for _, store := range []map[string]*ProcessSession{r.running, r.finished} {
		for sid, s := range store {
			if strings.HasPrefix(sid, query) {
				matches[sid] = s
			}
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

// Reconciles exited against the real child state when a descendant holds the pipe open
func (r *ProcessRegistry) reconcileLocalExit(s *ProcessSession) {
	if s == nil || s.isExited() {
		return
	}
	proc := s.Process
	if proc == nil {
		return
	}
	rc, done := proc.Poll()
	if !done {
		return
	}
	if !isWindowsHost && s.readerSelectable && s.readerAlive() {
		s.readerFinishRequested.set()
		if s.completionEvent.wait(readerFinalDrainWait) {
			return
		}
		s.mu.Lock()
		s.markExited(&rc, "exited", "")
		s.mu.Unlock()
		toolsLogger(s.env).Info(fmt.Sprintf("Reconciled session %s: direct child exited with code %d; reader will publish the owned completion after its final drain.", s.ID, rc))
		return
	}
	if stdout := proc.Stdout(); stdout != nil && !isWindowsHost {
		buf := make([]byte, 65536)
		var drained []byte
		for {
			n, ready, err := readChunkTimeout(stdout, buf, 0)
			if n > 0 {
				drained = append(drained, buf[:n]...)
			}
			if !ready || err != nil {
				break
			}
		}
		if len(drained) > 0 {
			s.AppendOutput(strings.ToValidUTF8(string(drained), string(utf8.RuneError)))
		}
	}
	s.mu.Lock()
	s.markExited(&rc, "exited", "")
	s.mu.Unlock()
	toolsLogger(s.env).Info(fmt.Sprintf("Reconciled session %s: direct child exited with code %d but reader was still blocked (orphaned pipe). Flipped to exited.", s.ID, rc))
	r.moveToFinished(s, false)
}

// Session id, command and running or exited status, read under the session lock
func statusHead(s *ProcessSession) map[string]any {
	status := "running"
	if s.Exited {
		status = "exited"
	}
	return map[string]any{"session_id": s.ID, "command": s.Command, "status": status}
}

// Poll checks status and returns the new output preview of a background process
func (r *ProcessRegistry) Poll(env *Env, sessionID string) map[string]any {
	s := r.Get(env, sessionID)
	if s == nil || r.uncollectedGone(s) {
		return notFoundResult(sessionID)
	}
	r.reconcileLocalExit(s)
	s.mu.Lock()
	preview := outputTail(s, 1000)
	exited, finalizing := s.Exited, readerFinalizing(s)
	result := statusHead(s)
	if exited {
		for k, v := range exitFields(s) {
			result[k] = v
		}
	}
	s.mu.Unlock()
	result["pid"] = s.field("pid")
	result["uptime_seconds"] = int(nowSeconds() - s.StartedAt)
	result["output_preview"] = preview
	if exited {
		if !finalizing {
			r.markPollObserved(sessionID)
		}
	}
	if s.Detached && s.OutputLog == "" {
		result["detached"] = true
		result["note"] = "Process recovered after restart -- output history unavailable"
	}
	return result
}

// ReadLog returns the full output paged by lines
func (r *ProcessRegistry) ReadLog(env *Env, sessionID string, offset *int, limit int) map[string]any {
	s := r.Get(env, sessionID)
	if s == nil {
		return notFoundResult(sessionID)
	}
	s.mu.Lock()
	full := StripANSI(s.OutputBuffer)
	exited, finalizing := s.Exited, readerFinalizing(s)
	result := statusHead(s)
	s.mu.Unlock()
	lines := splitLines(full)
	total := len(lines)
	var selected []string
	var observedCompletionOutput bool
	if offset == nil && limit > 0 {
		start := max(total-limit, 0)
		selected = lines[start:]
		observedCompletionOutput = len(selected) > 0 || total == 0
	} else {
		off := 0
		if offset != nil {
			off = *offset
		}
		start, stop := sliceIndices(off, off+limit, total)
		selected = lines[start:stop]
		observedCompletionOutput = total == 0 || (len(selected) > 0 && stop == total)
	}
	result["output"] = strings.Join(selected, "\n")
	result["total_lines"] = total
	result["showing"] = fmt.Sprintf("%d lines", len(selected))
	if exited && observedCompletionOutput && !finalizing {
		r.markConsumed(sessionID)
	}
	return result
}

// Python splitlines without a trailing empty element
func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// Python slice bounds of [start, stop) over a sequence of length n
func sliceIndices(start, stop, n int) (int, int) {
	norm := func(i int) int {
		if i < 0 {
			i += n
			if i < 0 {
				i = 0
			}
		}
		if i > n {
			i = n
		}
		return i
	}
	a, b := norm(start), norm(stop)
	if b < a {
		b = a
	}
	return a, b
}

// Wait blocks until the process exits, the timeout elapses, an interrupt lands or a yield releases it
func (r *ProcessRegistry) Wait(ctx context.Context, env *Env, sessionID string, timeout *int) map[string]any {
	maxTimeout := 180
	if raw := os.Getenv("TERMINAL_TIMEOUT"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			maxTimeout = n
		}
	}
	if timeout != nil && *timeout <= 0 {
		return map[string]any{"status": "error", "error": fmt.Sprintf("timeout must be positive (got %d)", *timeout)}
	}
	timeoutNote := ""
	effective := maxTimeout
	if timeout != nil && *timeout > 0 {
		effective = *timeout
		if *timeout > maxTimeout {
			effective = maxTimeout
			timeoutNote = fmt.Sprintf("Requested wait of %ds was clamped to configured limit of %ds", *timeout, maxTimeout)
		}
	}
	s := r.Get(env, sessionID)
	if s == nil {
		return notFoundResult(sessionID)
	}
	deadline := time.Now().Add(time.Duration(effective) * time.Second)
	for time.Now().Before(deadline) {
		s = r.refreshDetachedSession(s)
		if s == nil {
			return notFoundResult(sessionID)
		}
		r.reconcileLocalExit(s)
		var result map[string]any
		s.mu.Lock()
		exited := s.Exited && !readerFinalizing(s)
		if exited {
			result = exitSnapshot(s, "exited")
		}
		tail := outputTail(s, 1000)
		s.mu.Unlock()
		if exited {
			r.markConsumed(sessionID)
		} else if IsInterrupted(ctx) {
			result = map[string]any{"status": "interrupted", "command": s.Command, "output": tail,
				"note": "User sent a new message -- wait interrupted"}
		} else if ConsumeYield(WorkerOf(ctx)) {
			result = map[string]any{"status": "interrupted", "command": s.Command, "output": tail,
				"process_running": true,
				"note":            "User sent a new message -- wait released; the process is still running and you will be notified on exit. Respond to the user now."}
		}
		if result != nil {
			if timeoutNote != "" {
				result["timeout_note"] = timeoutNote
			}
			return result
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		s.completionEvent.wait(min(time.Second, remaining))
	}
	s.mu.Lock()
	tail := outputTail(s, 1000)
	s.mu.Unlock()
	result := map[string]any{"status": "timeout", "command": s.Command, "output": tail, "process_running": true}
	baseNote := fmt.Sprintf("Wait window of %ds elapsed — the process is still running. This is not an error.", effective)
	if s.StartedAt != 0 {
		baseNote += fmt.Sprintf(" Uptime: %ds.", int(nowSeconds()-s.StartedAt))
	}
	if s.NotifyOnComplete {
		baseNote += " notify_on_complete is set: you will be notified on exit — do more work instead of waiting again."
	} else {
		baseNote += " Poll again later or use terminal(background=true, notify_on_complete=true) next time for automatic notification."
	}
	if timeoutNote != "" {
		result["timeout_note"] = timeoutNote + ". " + baseNote
	} else {
		result["timeout_note"] = baseNote
	}
	return result
}

// Result for an exited session with exit metadata and the completion sized output tail
func exitSnapshot(s *ProcessSession, status string) map[string]any {
	result := map[string]any{"status": status, "command": s.Command}
	for k, v := range exitFields(s) {
		result[k] = v
	}
	for k, v := range completionOutput(s) {
		result[k] = v
	}
	return result
}

// KillProcess kills a background process and returns its output snapshot
func (r *ProcessRegistry) KillProcess(env *Env, sessionID, source string, consumeOutput bool) map[string]any {
	if source == "" {
		source = "process.kill"
	}
	s := r.Get(env, sessionID)
	if s == nil {
		return notFoundResult(sessionID)
	}
	if s.isExited() {
		scopeStopped := r.stopScope(s)
		s.mu.Lock()
		result := exitSnapshot(s, "already_exited")
		finalizing := readerFinalizing(s)
		s.mu.Unlock()
		if !scopeStopped {
			result["scope_stop_failed"] = s.SystemdUnit
		}
		if consumeOutput && !finalizing {
			r.markConsumed(sessionID)
		}
		return result
	}
	if early := r.signalKill(env, s, sessionID, consumeOutput); early != nil {
		return early
	}
	scopeStopped := r.stopScope(s)
	s.mu.Lock()
	signalRaceExited := s.Exited
	s.mu.Unlock()
	survivors := []int{}
	if !signalRaceExited {
		survivors = r.postKillSurvivors(s)
	}
	if len(survivors) > 0 {
		parts := make([]string, len(survivors))
		for i, pid := range survivors {
			parts[i] = strconv.Itoa(pid)
		}
		alive := strings.Join(parts, ", ")
		toolsLogger(env).Warn(fmt.Sprintf("Kill incomplete for %s: %d process(es) still alive (%s) — session kept running", s.ID, len(survivors), alive))
		return map[string]any{
			"status":     "error",
			"error":      fmt.Sprintf("Kill incomplete: %d process(es) still alive (%s); session running", len(survivors), alive),
			"session_id": s.ID, "survivors": survivors, "process_running": true,
		}
	}
	if !scopeStopped {
		s.mu.Lock()
		rootExited := s.Exited
		s.mu.Unlock()
		toolsLogger(env).Warn(fmt.Sprintf("Kill incomplete for %s: systemd scope %s did not stop", s.ID, s.SystemdUnit))
		return map[string]any{
			"status": "error", "error": fmt.Sprintf("Kill incomplete: systemd scope %s did not stop", s.SystemdUnit),
			"session_id": s.ID, "scope_stop_failed": s.SystemdUnit, "process_running": !rootExited,
		}
	}
	s.mu.Lock()
	output := completionOutput(s)
	if consumeOutput {
		r.markConsumed(sessionID)
	}
	s.Exited = true
	s.ExitCode = procIntPtr(-15)
	s.CompletionReason = "killed"
	s.TerminationSource = source
	result := map[string]any{"status": "killed", "session_id": s.ID, "completion_reason": s.CompletionReason, "termination_source": s.TerminationSource}
	s.mu.Unlock()
	if !r.moveToFinished(s, false) {
		saveCompletedResult(env, s)
	}
	r.writeCheckpoint(env, nil)
	for k, v := range output {
		result[k] = v
	}
	return result
}

// Stops the session's systemd scope, true when it has none or the stop worked
func (r *ProcessRegistry) stopScope(s *ProcessSession) bool {
	if s.SystemdUnit == "" {
		return true
	}
	stopped := stopSystemdUnit(s.SystemdUnit)
	r.mu.Lock()
	s.scopeStopPending = !stopped
	r.mu.Unlock()
	return stopped
}

// Delivers the kill via PTY, local tree, sandbox exec or recovered host pid, a result when it cannot proceed
func (r *ProcessRegistry) signalKill(env *Env, s *ProcessSession, sessionID string, consumeOutput bool) map[string]any {
	switch {
	case s.pty != nil:
		s.pty.Terminate(true)
	case s.Process != nil:
		r.terminateHostPid(s.env, s.Process.Pid(), s.HostStartTime)
	case s.EnvRef != nil && s.Pid != 0:
		s.EnvRef.Execute(context.Background(), fmt.Sprintf("kill %d 2>/dev/null", s.Pid), ExecOptions{Timeout: 5})
	case s.Detached && s.PidScope == "host" && s.Pid != 0:
		if r.detachedHostFate(s.Pid, s.HostStartTime) != "running" {
			scopeStopped := r.stopScope(s)
			s.mu.Lock()
			output := completionOutput(s)
			s.mu.Unlock()
			if consumeOutput {
				r.markConsumed(sessionID)
			}
			r.closeReusedDetached(s)
			s.mu.Lock()
			result := map[string]any{"status": "already_exited", "exit_code": s.field("exit_code")}
			s.mu.Unlock()
			for k, v := range output {
				result[k] = v
			}
			if !scopeStopped {
				result["scope_stop_failed"] = s.SystemdUnit
			}
			return result
		}
		r.terminateHostPid(s.env, s.Pid, nil)
	default:
		return map[string]any{
			"status": "error",
			"error":  "Recovered process cannot be killed after restart because its original runtime handle is no longer available",
		}
	}
	return nil
}

// Runs the PTY op under PTY mode, else the pipe op, returning ok on success
func (r *ProcessRegistry) stdinOp(env *Env, sessionID string, ptyOp func(*ptyProcess) error, pipeOp func(*ExecProcess) error, ok map[string]any) map[string]any {
	s := r.Get(env, sessionID)
	if s == nil {
		return notFoundResult(sessionID)
	}
	if s.isExited() {
		return map[string]any{"status": "already_exited", "error": alreadyExitedError(s)}
	}
	var err error
	switch {
	case s.pty != nil:
		err = ptyOp(s.pty)
	case s.Process == nil || s.Process.stdinDone == nil:
		return map[string]any{"status": "error", "error": stdinUnavailableError(s)}
	default:
		err = pipeOp(s.Process)
	}
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}
	}
	return ok
}

// Pipe stdin of a tracked process is handed to the child at spawn and closed after its data, so a write cannot reach it
var errPipeStdinClosed = errors.New("stdin pipe is closed")

// WriteStdin sends raw data to a running process's stdin without a newline
func (r *ProcessRegistry) WriteStdin(env *Env, sessionID, data string) map[string]any {
	return r.stdinOp(env, sessionID,
		func(p *ptyProcess) error {
			if isWindowsHost {
				return p.Write([]byte(data))
			}
			raw, err := pystr.SurrogateEscapeEncode(data)
			if err != nil {
				return err
			}
			return p.Write(raw)
		},
		func(p *ExecProcess) error { return errPipeStdinClosed },
		map[string]any{"status": "ok", "bytes_written": pystr.Len(data)})
}

// SubmitStdin sends data plus Enter, CR on a POSIX PTY, CRLF on a Windows PTY, LF on a pipe
func (r *ProcessRegistry) SubmitStdin(env *Env, sessionID, data string) map[string]any {
	s := r.Get(env, sessionID)
	enter := "\n"
	if s != nil && s.pty != nil {
		enter = "\r"
		if isWindowsHost {
			enter = "\r\n"
		}
	}
	return r.WriteStdin(env, sessionID, data+enter)
}

// RequestCloseTerminal asks the desktop GUI to close the process's read only terminal tab
func (r *ProcessRegistry) RequestCloseTerminal(env *Env, sessionID string) map[string]any {
	if r.OnClose == nil {
		return map[string]any{"status": "error", "error": "close_terminal is only available in the icebath desktop app."}
	}
	if err := r.OnClose(r.Get(env, sessionID), sessionID); err != nil {
		return map[string]any{"status": "error", "error": err.Error()}
	}
	return map[string]any{
		"status": "ok", "closed": sessionID,
		"note": "Closed the read-only terminal tab. The process was not killed; its output remains available and the user can reopen the tab from the status stack.",
	}
}

// CloseStdin sends EOF to a running process without killing it
func (r *ProcessRegistry) CloseStdin(env *Env, sessionID string) map[string]any {
	s := r.Get(env, sessionID)
	msg := "stdin closed"
	if s != nil && s.pty != nil {
		msg = "EOF sent"
	}
	return r.stdinOp(env, sessionID,
		func(p *ptyProcess) error { return p.SendEOF() },
		func(p *ExecProcess) error { return nil },
		map[string]any{"status": "ok", "message": msg})
}

// CountRunning is the running count for status bar polling
func (r *ProcessRegistry) CountRunning() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.running)
}

// ListSessions lists running and recently finished processes for a task and or session key
func (r *ProcessRegistry) ListSessions(env *Env, taskID, sessionKey string, includeRetained bool) []map[string]any {
	order := []string{}
	sessions := map[string]*ProcessSession{}
	add := func(s *ProcessSession) {
		if _, seen := sessions[s.ID]; !seen {
			order = append(order, s.ID)
		}
		sessions[s.ID] = s
	}
	if includeRetained {
		for _, s := range loadCompletedResultList(env, "") {
			add(s)
		}
	}
	r.mu.Lock()
	for _, s := range orderedSessions(r.finished, func(s *ProcessSession) int64 { return s.finSeq }) {
		add(s)
	}
	for _, s := range orderedSessions(r.running, func(s *ProcessSession) int64 { return s.runSeq }) {
		add(s)
	}
	r.mu.Unlock()
	all := []*ProcessSession{}
	for _, id := range order {
		refreshed := r.refreshDetachedSession(sessions[id])
		if refreshed != nil && !r.uncollectedGone(refreshed) {
			all = append(all, refreshed)
		}
	}
	if taskID != "" || sessionKey != "" {
		kept := all[:0]
		for _, s := range all {
			if (taskID != "" && s.OwnerTaskID == taskID) || (sessionKey != "" && s.SessionKey == sessionKey) {
				kept = append(kept, s)
			}
		}
		all = kept
	}
	result := []map[string]any{}
	for _, s := range all {
		r.reconcileLocalExit(s)
		s.mu.Lock()
		status := "running"
		if s.Exited {
			status = "exited"
		}
		entry := map[string]any{
			"session_id": s.ID, "command": headChars(s.Command, 200), "cwd": s.field("cwd"), "pid": s.field("pid"),
			"owner_task_id":  s.OwnerTaskID,
			"started_at":     time.Unix(int64(s.StartedAt), 0).Local().Format("2006-01-02T15:04:05"),
			"uptime_seconds": int(nowSeconds() - s.StartedAt), "status": status,
			"output_preview": tailChars(s.OutputBuffer, 200),
		}
		if taskID != "" && sessionKey != "" && s.OwnerTaskID != taskID && s.SessionKey == sessionKey {
			entry["session_scoped"] = true
		}
		if s.WslChain {
			entry["wsl_chain"] = true
			entry["wsl_note"] = wslChainNote
		}
		if len(s.WatchPatterns) > 0 && !s.watchDisabled {
			entry["watch_patterns"] = append([]string{}, s.WatchPatterns...)
			entry["watch_hit"] = s.watchHits > 0
		}
		if s.NotifyOnComplete {
			entry["notify_on_complete"] = true
		}
		if s.PersistOnRelease {
			entry["persist_on_release"] = true
		}
		if s.Exited {
			entry["exit_code"] = s.field("exit_code")
			entry["exited_at"] = s.ExitedAt
			entry["completion_reason"] = s.CompletionReason
		}
		s.mu.Unlock()
		if s.Detached {
			entry["detached"] = true
		}
		result = append(result, entry)
	}
	return result
}

// Reports whether any still running session satisfies the predicate after refreshing detached ones
func (r *ProcessRegistry) anyRunning(pred func(*ProcessSession) bool) bool {
	r.mu.Lock()
	sessions := make([]*ProcessSession, 0, len(r.running))
	for _, s := range r.running {
		sessions = append(sessions, s)
	}
	r.mu.Unlock()
	for _, s := range sessions {
		r.refreshDetachedSession(s)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.running {
		if !s.isExited() && pred(s) {
			return true
		}
	}
	return false
}

// AnyRunning reports whether any still running session satisfies the predicate
func (r *ProcessRegistry) AnyRunning(pred func(*ProcessSession) bool) bool { return r.anyRunning(pred) }

// HasActiveProcesses reports whether any process the task spawned is still running
func (r *ProcessRegistry) HasActiveProcesses(taskID string) bool {
	return r.anyRunning(func(s *ProcessSession) bool { return s.OwnerTaskID == taskID })
}

// RunningOwnedBy lists running processes whose raw spawning owner is ownerTaskID
func (r *ProcessRegistry) RunningOwnedBy(ownerTaskID string) []*ProcessSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []*ProcessSession{}
	for _, s := range r.running {
		if s.OwnerTaskID == ownerTaskID && !s.isExited() {
			out = append(out, s)
		}
	}
	return out
}

// UnreadCompletionsOwnedBy lists exited notify_on_complete processes of the owner whose result nobody read
func (r *ProcessRegistry) UnreadCompletionsOwnedBy(ownerTaskID string) []*ProcessSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []*ProcessSession{}
	for _, s := range r.finished {
		s.mu.Lock()
		collected := s.NotifyOnComplete && s.ExitCode != nil
		s.mu.Unlock()
		if s.OwnerTaskID == ownerTaskID && collected && !r.IsCompletionConsumed(s.ID) && !r.isPollObserved(s.ID) {
			out = append(out, s)
		}
	}
	return out
}

// TransferOwnership moves a running process from one owner to another, nil when the transfer cannot happen
func (r *ProcessRegistry) TransferOwnership(env *Env, sessionID, fromOwner, toOwner, toTaskID, toSessionKey, note string) *ProcessSession {
	s := r.Get(env, sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if s == nil || s.isExited() || s.OwnerTaskID != fromOwner {
		return nil
	}
	s.OwnerTaskID = toOwner
	s.TaskID = toTaskID
	s.SessionKey = toSessionKey
	s.HandoffNote = note
	return s
}

// HasActiveForSession reports active processes for a gateway session key younger than maxActiveAge
func (r *ProcessRegistry) HasActiveForSession(sessionKey string, maxActiveAge *float64) bool {
	now := nowSeconds()
	return r.anyRunning(func(s *ProcessSession) bool {
		return s.SessionKey == sessionKey && (maxActiveAge == nil || now-s.StartedAt < *maxActiveAge)
	})
}

// HasAnyActive reports whether any background process is running
func (r *ProcessRegistry) HasAnyActive() bool {
	return r.anyRunning(func(*ProcessSession) bool { return true })
}

// SnapshotRunningIDs is the set of running ids owned by the task, a turn boundary marker
func (r *ProcessRegistry) SnapshotRunningIDs(taskID string) map[string]bool {
	out := map[string]bool{}
	for _, s := range r.RunningOwnedBy(taskID) {
		out[s.ID] = true
	}
	return out
}

// Kill sources that are agent lifecycle cleanup rather than a deliberate stop
var lifecycleKillSources = setOf("kill_all", "gateway_turn_timeout", "agent_close")

// KillStartedSince kills the task's processes created after the baseline, consuming their output
func (r *ProcessRegistry) KillStartedSince(env *Env, taskID string, baselineIDs map[string]bool, source string) int {
	return r.KillAll(env, taskID, baselineIDs, source, true)
}

// KillAll kills every running process, or only those the task spawned, returning how many it stopped
func (r *ProcessRegistry) KillAll(env *Env, taskID string, excludeIDs map[string]bool, source string, consumeOutput bool) int {
	if source == "" {
		source = "kill_all"
	}
	lifecycle := lifecycleKillSources[source]
	targets := []*ProcessSession{}
	r.mu.Lock()
	for _, store := range []map[string]*ProcessSession{r.running, r.finished} {
		for _, s := range store {
			if (taskID == "" || s.OwnerTaskID == taskID) && !excludeIDs[s.ID] && (!s.isExited() || s.scopeStopPending) && !(lifecycle && s.PersistOnRelease) {
				targets = append(targets, s)
			}
		}
	}
	r.mu.Unlock()
	count := 0
	for _, s := range targets {
		res := r.KillProcess(env, s.ID, source, consumeOutput)
		status := asStr(res["status"])
		if _, failed := res["scope_stop_failed"]; (status == "killed" || status == "already_exited") && !failed {
			count++
		}
	}
	return count
}

// Drops expired then oldest over cap finished sessions, caller holds the registry lock
func (r *ProcessRegistry) pruneIfNeeded() {
	now := nowSeconds()
	prunable := map[string]*ProcessSession{}
	for sid, s := range r.finished {
		if !s.scopeStopPending && !retainedForDelivery(s) {
			prunable[sid] = s
		}
	}
	expired := []string{}
	expiredSet := map[string]bool{}
	for sid, s := range prunable {
		if now-s.StartedAt > FinishedTTLSeconds {
			expired = append(expired, sid)
			expiredSet[sid] = true
		}
	}
	overCap := len(r.running)+len(r.finished)-len(expired) >= MaxProcesses
	if overCap {
		oldest := ""
		for sid := range prunable {
			if expiredSet[sid] {
				continue
			}
			if oldest == "" || r.finished[sid].StartedAt < r.finished[oldest].StartedAt || (r.finished[sid].StartedAt == r.finished[oldest].StartedAt && sid < oldest) {
				oldest = sid
			}
		}
		if oldest != "" {
			expired = append(expired, oldest)
		}
	}
	for _, sid := range expired {
		r.releaseFinishedHandles(r.finished[sid], false)
		delete(r.finished, sid)
	}
	r.markMu.Lock()
	for sid := range r.completionConsumed {
		if _, ok := r.running[sid]; !ok {
			if _, ok := r.finished[sid]; !ok {
				delete(r.completionConsumed, sid)
			}
		}
	}
	for sid := range r.pollObserved {
		if _, ok := r.running[sid]; !ok {
			if _, ok := r.finished[sid]; !ok {
				delete(r.pollObserved, sid)
			}
		}
	}
	r.markMu.Unlock()
}

// Pipe job in its own session, owned by the supervisor unless the job must outlive this process
func (r *ProcessRegistry) spawnJob(s *ProcessSession, argv []string, persistOnRelease bool, cwd string, env map[string]string, logFile *os.File) (*ExecProcess, error) {
	configure := func(c *exec.Cmd) {
		c.Dir = cwd
		c.Env = envSlice(env)
		startNewSession(c)
	}
	var proc *ExecProcess
	var err error
	if logFile != nil {
		proc, err = startLogProcess(argv, logFile, configure)
	} else {
		proc, err = popenBash(argv, nil, configure)
	}
	if err != nil {
		return nil, err
	}
	if r.ownsJob(persistOnRelease) {
		ownProcessGroups([]int{proc.Pid()})
		RegisterChild(proc.Pid(), "terminal-background", "")
		s.ownedPgid = proc.Pid()
	}
	return proc, nil
}

// Spawns argv with stdout and stderr appended to the log file and no stdin
func startLogProcess(argv []string, logFile *os.File, configure func(*exec.Cmd)) (*ExecProcess, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	configure(cmd)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	hideConsoleWindow(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &ExecProcess{Cmd: cmd, waitDone: make(chan struct{})}
	go func() {
		defer close(p.waitDone)
		err := cmd.Wait()
		p.code = exitCodeOf(cmd, err)
	}()
	return p, nil
}
