package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Longest a checkpoint write waits for the store lock, the Python lock blocks without a deadline
const checkpointWriteLockWait = 24 * time.Hour

// Durable state of the process registry, the crash checkpoint, completed receipts, ownership, awaiting, wait policy and notifications

// Hooks the delegation runtime and the gateway install for completion routing
var (
	// SubagentHandoffResolver finds the live subagent record of a task for process handoff, installed by the delegate slice
	SubagentHandoffResolver func(taskID string) SubagentHandoff
	// SubagentAttribution returns the goal and delegation_id of a subagent task for notification provenance
	SubagentAttribution func(taskID string) map[string]any
	// DelegationConfig returns the active delegation config section, installed by the delegate slice
	DelegationConfig func(env *Env) map[string]any
	// DelegationFallbackChain returns the configured fallback chain of a delegation config
	DelegationFallbackChain func(cfg map[string]any) []any
	// ModelNotFoundPatterns returns the failover classifier's model not found phrases, installed by provider wiring
	ModelNotFoundPatterns func() []string
	// AsyncDelegationLedger replays and marks durable delegation completions, installed by the delegate slice
	AsyncDelegationLedger AsyncDelegationCompletions
	// DiagnosticProcessEvent reports a delegation event that is diagnostic rather than a result, installed by the agent
	DiagnosticProcessEvent func(evt map[string]any) bool
	// AsyncDeliverySupported reports whether the bound session can receive async notices, installed by the gateway
	AsyncDeliverySupported func(env *Env) bool
)

// SubagentHandoff is the live subagent record a process handoff validates against
type SubagentHandoff interface {
	ParentOwner() string
	ParentSessionID() string
	HandedOff() []map[string]any
	AppendHandoff(entry map[string]any)
}

// AsyncDelegationCompletions is the durable delegation completion ledger
type AsyncDelegationCompletions interface {
	RestoreUndelivered(q *EventQueue) (int, error)
	CopyID(evt map[string]any) string
	MarkCopyHeld(id string)
	UnmarkCopyHeld(id string)
}

// Checkpoint of running processes for crash recovery

// Active profile's checkpoint file at call time, swapped by tests
var checkpointPathFn = func(env *Env) string {
	return filepath.Join(envHome(env), "processes.json")
}

// Default values of the persisted session fields
func checkpointDefault(name string) any {
	switch name {
	case "pid":
		return nil
	case "pid_scope":
		return "host"
	case "host_start_time", "cwd":
		return nil
	case "wsl_chain", "notify_on_complete", "persist_on_release":
		return false
	case "started_at":
		return 0.0
	case "watcher_interval", "completion_output_chars", "heartbeat_seconds":
		return 0
	case "watch_patterns":
		return []any{}
	}
	return ""
}

// Writes the running process metadata to the checkpoint file atomically under the file lock
func (r *ProcessRegistry) writeCheckpoint(env *Env, extraEntries []map[string]any) {
	entries := []map[string]any{}
	r.mu.Lock()
	for _, s := range orderedSessions(r.running, func(s *ProcessSession) int64 { return s.runSeq }) {
		s.mu.Lock()
		if s.Exited {
			s.mu.Unlock()
			continue
		}
		if s.HostStartTime == nil && s.PidScope == "host" && s.Pid != 0 {
			s.HostStartTime = safeHostStartTime(s.Pid)
		}
		entry := map[string]any{"session_id": s.ID}
		for _, f := range checkpointFields {
			entry[f] = s.field(f)
		}
		s.mu.Unlock()
		entry["command"] = RedactSensitiveTextWith(env, s.Command, RedactOptions{CodeFile: true})
		owner := s.OwnerTaskID
		if owner == "" {
			owner = s.TaskID
		}
		entry["owner_task_id"] = owner
		entries = append(entries, entry)
	}
	if len(extraEntries) > 0 {
		tracked := map[string]bool{}
		for _, item := range entries {
			tracked[asStr(item["session_id"])] = true
		}
		for _, item := range extraEntries {
			if !tracked[asStr(item["session_id"])] {
				entries = append(entries, item)
			}
		}
	}
	known := map[string]bool{}
	for id := range r.running {
		known[id] = true
	}
	for id := range r.finished {
		known[id] = true
	}
	r.mu.Unlock()
	path := checkpointPathFn(env)
	lockPath := path + ".lock"
	os.MkdirAll(filepath.Dir(lockPath), 0o700)
	unlock, err := lockStoreFile(lockPath, checkpointWriteLockWait)
	if err != nil {
		toolsLogger(env).Debug(fmt.Sprintf("Failed to write checkpoint file: %v", err))
		return
	}
	defer unlock()
	entries = append(entries, r.foreignHandoffs(env, path, known)...)
	if err := procAtomicJSONWrite(path, entries, 0o644, false); err != nil {
		toolsLogger(env).Debug(fmt.Sprintf("Failed to write checkpoint file: %v", err))
	}
}

// On disk worker handoffs this registry never tracked whose log still decides an outcome
func (r *ProcessRegistry) foreignHandoffs(env *Env, path string, known map[string]bool) []map[string]any {
	onDisk, ok := readCheckpointEntries(path)
	if !ok {
		return nil
	}
	out := []map[string]any{}
	for _, entry := range onDisk {
		if known[asStr(entry["session_id"])] {
			continue
		}
		log := adoptableLog(env, entry["output_log"])
		if log == "" {
			continue
		}
		if _, err := os.Stat(log); err != nil {
			continue
		}
		if _, err := os.Stat(exitPathOf(log)); err == nil {
			out = append(out, entry)
			continue
		}
		pid, _ := asIntValue(entry["pid"])
		if r.detachedHostFate(pid, entryInt64Ptr(entry, "host_start_time")) == "running" {
			out = append(out, entry)
		}
	}
	return out
}

func entryInt64Ptr(entry map[string]any, key string) *int64 {
	n, ok := asIntValue(entry[key])
	if !ok {
		return nil
	}
	v := int64(n)
	return &v
}

// Checkpoint entries from disk, false when missing or unreadable
func readCheckpointEntries(path string) ([]map[string]any, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var parsed []any
	dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(string(data), "\xEF\xBB\xBF")))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return nil, false
	}
	out := []map[string]any{}
	for _, item := range parsed {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, true
}

// RecoverFromCheckpoint probes the checkpointed pids at startup and re adopts the live ones as detached sessions
func (r *ProcessRegistry) RecoverFromCheckpoint(ctx context.Context, env *Env) int {
	r.jobsOutliveHost = true
	path := checkpointPathFn(env)
	if _, err := os.Stat(path); err != nil {
		return 0
	}
	entries, ok := readCheckpointEntries(path)
	if !ok {
		return 0
	}
	recovered := 0
	unresolvedScopeEntries := []map[string]any{}
	for _, entry := range entries {
		pid, _ := asIntValue(entry["pid"])
		pidScope := "host"
		if v, ok := entry["pid_scope"]; ok {
			pidScope = asStr(v)
		}
		if pid == 0 {
			continue
		}
		sessionID := asStr(entry["session_id"])
		r.mu.Lock()
		_, alreadyTracked := r.running[sessionID]
		r.mu.Unlock()
		if alreadyTracked {
			continue
		}
		command := asStr(entry["command"])
		if _, ok := entry["command"]; !ok {
			command = "unknown"
		}
		if pidScope != "host" {
			toolsLogger(env).Info(fmt.Sprintf("Skipping recovery for non-host process: %s (pid=%d, scope=%s)", headChars(command, 60), pid, pidScope))
			continue
		}
		fate := r.detachedHostFate(pid, entryInt64Ptr(entry, "host_start_time"))
		log := adoptableLog(env, entry["output_log"])
		if log != "" {
			fate = "running"
		}
		if fate == "reused" {
			shown := sessionID
			if shown == "" {
				shown = "?"
			}
			toolsLogger(env).Info(fmt.Sprintf("Not recovering session %s: pid %d is alive but its start time no longer matches — PID was recycled onto an unrelated process; refusing to adopt or signal it.", shown, pid))
			continue
		}
		if fate != "running" {
			if unit := asStr(entry["systemd_unit"]); unit != "" && !stopSystemdUnit(unit) {
				toolsLogger(env).Warn(fmt.Sprintf("Could not reap persisted scope %s for dead wrapper pid %d; retaining checkpoint entry for the next startup", unit, pid))
				unresolvedScopeEntries = append(unresolvedScopeEntries, entry)
			}
			continue
		}
		s := NewProcessSession(sessionID, command)
		for _, f := range checkpointFields {
			value, ok := entry[f]
			if !ok {
				value = checkpointDefault(f)
			}
			s.setField(f, value)
		}
		s.Command = command
		owner := asStr(entry["owner_task_id"])
		if owner == "" {
			owner = asStr(entry["task_id"])
		}
		s.OwnerTaskID = owner
		if _, ok := entry["started_at"]; !ok {
			s.StartedAt = nowSeconds()
		}
		s.OutputLog = log
		s.Detached = true
		s.env = env
		s.finishInit()
		if s.OutputLog != "" {
			r.trackStarted(s, func() { r.logReaderLoop(WithEnvHome(ctx, env), s) })
		} else {
			r.mu.Lock()
			r.insertRunning(s)
			r.mu.Unlock()
		}
		recovered++
		toolsLogger(env).Info(fmt.Sprintf("Recovered detached process: %s (pid=%d)", headChars(s.Command, 60), pid))
		if s.WatcherInterval > 0 {
			watcher := map[string]any{
				"session_id": s.ID, "check_interval": s.WatcherInterval, "session_key": s.SessionKey,
				"notify_on_complete": s.NotifyOnComplete, "parent_session_id": s.ParentSessionID,
			}
			for _, key := range watcherRouteKeys {
				watcher[key] = s.watcherField(key)
			}
			r.mu.Lock()
			r.PendingWatchers = append(r.PendingWatchers, watcher)
			r.mu.Unlock()
		}
	}
	r.writeCheckpoint(env, unresolvedScopeEntries)
	return recovered
}

// Tails the session's output log until its process ends, the spawner reading the exit from its handle
func (r *ProcessRegistry) logReaderLoop(ctx context.Context, s *ProcessSession) {
	decoder := &utf8Decoder{}
	proc := s.Process
	s.readerSelectable = true
	headNoise := true
	ingest := func(text string) {
		if headNoise {
			text = cleanShellNoise(text)
			headNoise = strings.TrimSpace(text) == ""
		}
		r.ingestOutput(s, text, false)
	}
	running := func() bool {
		if s.readerFinishRequested.isSet() {
			return false
		}
		if proc != nil {
			_, done := proc.Poll()
			return !done
		}
		return r.detachedHostFate(s.Pid, s.HostStartTime) == "running"
	}
	stream, err := os.Open(s.OutputLog)
	if err != nil {
		toolsLogger(s.env).Warn(fmt.Sprintf("Process output log %s unreadable: %v", s.OutputLog, err))
	} else {
		drained := 0
		buf := make([]byte, 65536)
		for {
			alive := running()
			n, rerr := stream.Read(buf)
			if n > 0 {
				if text := decoder.decode(buf[:n]); text != "" {
					ingest(text)
				}
				if !alive {
					drained += n
				}
				if drained < outputLogFinalDrainMaxBytes {
					continue
				}
			}
			if rerr != nil && rerr.Error() != "EOF" {
				toolsLogger(s.env).Warn(fmt.Sprintf("Process output log %s unreadable: %v", s.OutputLog, rerr))
				break
			}
			if !alive {
				break
			}
			time.Sleep(outputLogTailInterval)
		}
		stream.Close()
	}
	if proc != nil {
		r.finishReader(WithEnvHome(ctx, s.env), s, decoder, ingest, "Log",
			func() error { _, err := proc.Wait(0); return err },
			func() *int {
				code, done := proc.Poll()
				if !done {
					return nil
				}
				return &code
			})
		return
	}
	if tail := decoder.finish(); tail != "" {
		ingest(tail)
	}
	code, ok := recordedExitCode(s.OutputLog)
	if !ok {
		r.finishExited(WithEnvHome(ctx, s.env), s, procIntPtr(-1), "lost", "backend_lost", true)
		return
	}
	r.finishExited(WithEnvHome(ctx, s.env), s, &code, "exited", "", true)
}

// Completed process receipts, profile local and bounded

// ResultRetentionSeconds is how long completed receipts are kept
const ResultRetentionSeconds = 7 * 24 * time.Hour

// MaxRetainedResults caps the receipts kept per profile
const MaxRetainedResults = 64

var resultFields = []string{
	"id", "command", "cwd", "task_id", "owner_task_id", "session_key",
	"parent_session_id", "started_at", "exit_code", "completion_reason",
	"termination_source", "notify_on_complete",
}

func processResultsDir(env *Env) string {
	return filepath.Join(envHome(env), "logs", "process-results")
}

// Retained receipt paths newest first after pruning by completion time and count
func resultPaths(env *Env) []string {
	directory := processResultsDir(env)
	cutoff := time.Now().Add(-ResultRetentionSeconds)
	type retainedPath struct {
		modified time.Time
		path     string
	}
	retained := []retainedPath{}
	matches, _ := filepath.Glob(filepath.Join(directory, "proc_*.json"))
	for _, path := range matches {
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		if st.ModTime().Before(cutoff) {
			os.Remove(path)
		} else {
			retained = append(retained, retainedPath{st.ModTime(), path})
		}
	}
	sort.Slice(retained, func(i, j int) bool {
		if !retained[i].modified.Equal(retained[j].modified) {
			return retained[i].modified.After(retained[j].modified)
		}
		return filepath.Base(retained[i].path) > filepath.Base(retained[j].path)
	})
	for _, item := range retained[min(len(retained), MaxRetainedResults):] {
		os.Remove(item.path)
	}
	out := []string{}
	for _, item := range retained[:min(len(retained), MaxRetainedResults)] {
		out = append(out, item.path)
	}
	return out
}

// Writes the session's durable receipt with its output and command redacted
func saveCompletedResult(ctx context.Context, env *Env, s *ProcessSession) {
	s.mu.Lock()
	record := map[string]any{}
	for _, key := range resultFields {
		record[key] = s.field(key)
	}
	record["output"] = tailChars(s.OutputBuffer, MaxOutputChars)
	s.mu.Unlock()
	record["output"] = RedactTerminalCommandOutput(env, asStr(record["output"]), s.Command, true)
	record["command"] = RedactSensitiveTextWith(env, s.Command, RedactOptions{CodeFile: true, Force: true})
	directory := processResultsDir(env)
	if _, err := home.Mkdir(WithEnvHome(ctx, env), directory); err != nil {
		toolsLogger(env).Warn(fmt.Sprintf("Could not retain completed process result %s", s.ID), "error", err)
		return
	}
	if err := procAtomicJSONWrite(filepath.Join(directory, s.ID+".json"), record, 0o600, false); err != nil {
		toolsLogger(env).Warn(fmt.Sprintf("Could not retain completed process result %s", s.ID), "error", err)
		return
	}
	resultPaths(env)
}

// Reports whether the owner session is the parent or its compression tip
func ownsResult(env *Env, owner, parent string) bool {
	if parent == "" {
		return false
	}
	if owner == parent {
		return true
	}
	db := env.State
	if db == nil {
		opened, err := state.Open(home.StatePath(envHome(env)), state.Options{ReadOnly: true})
		if err != nil {
			return false
		}
		defer opened.Close()
		db = opened
	}
	tip, err := db.CompressionTip(parent)
	if err != nil {
		return false
	}
	return tip == owner
}

var resultIDRe = regexp.MustCompile(`^proc_[\w]+$`)

// Read only snapshots of retained receipts the current session owns, newest first
func loadCompletedResultList(env *Env, prefix string) []*ProcessSession {
	owner := sessionEnvValue(env, "ICEBATH_SESSION_ID")
	if owner == "" {
		return nil
	}
	results := []*ProcessSession{}
	for _, path := range resultPaths(env) {
		stem := strings.TrimSuffix(filepath.Base(path), ".json")
		if !strings.HasPrefix(stem, prefix) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			toolsLogger(env).Debug(fmt.Sprintf("Skipping unreadable process result %s", filepath.Base(path)), "error", err)
			continue
		}
		var record map[string]any
		dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(string(data), "\xEF\xBB\xBF")))
		dec.UseNumber()
		if err := dec.Decode(&record); err != nil {
			toolsLogger(env).Debug(fmt.Sprintf("Skipping unreadable process result %s", filepath.Base(path)), "error", err)
			continue
		}
		id := asStr(record["id"])
		if id != stem || !resultIDRe.MatchString(id) {
			continue
		}
		if !ownsResult(env, owner, asStr(record["parent_session_id"])) {
			continue
		}
		output, ok := record["output"].(string)
		if !ok {
			toolsLogger(env).Debug(fmt.Sprintf("Skipping unreadable process result %s", filepath.Base(path)))
			continue
		}
		s := NewProcessSession(id, asStr(record["command"]))
		for _, key := range resultFields {
			s.setField(key, record[key])
		}
		s.Exited = true
		s.OutputBuffer = output
		s.env = env
		s.finishInit()
		s.completionEvent.set()
		results = append(results, s)
	}
	return results
}

// Retained receipts keyed by id
func loadCompletedResults(env *Env, prefix string) map[string]*ProcessSession {
	out := map[string]*ProcessSession{}
	for _, s := range loadCompletedResultList(env, prefix) {
		out[s.ID] = s
	}
	return out
}

// Ownership of local jobs by the owner death supervisor

// Reports whether a local job's group should die with this process
func (r *ProcessRegistry) ownsJob(persistOnRelease bool) bool {
	worker := IsWorkerProcess != nil && IsWorkerProcess()
	return !(isWindowsHost || persistOnRelease || r.jobsOutliveHost || worker || isSupervisedGatewayProcess())
}

// The PTY child leads its own session and group, covered while the job runs
func (r *ProcessRegistry) ownPtyJob(s *ProcessSession, pty *ptyProcess) {
	if r.ownsJob(s.PersistOnRelease) {
		ownProcessGroups([]int{pty.Pid()})
		s.ownedPgid = pty.Pid()
	}
}

// A yielding foreground command hands its supervised group over with the pipe
func (r *ProcessRegistry) takeHandoffGroup(s *ProcessSession, proc *ExecProcess) {
	ownedPgid := proc.ownedPgid
	proc.ownedPgid = 0
	if ownedPgid != 0 && r.ownsJob(false) {
		s.ownedPgid = ownedPgid
	} else if ownedPgid != 0 {
		releaseProcessGroups([]int{ownedPgid})
	}
}

// The job's own process exited or was killed, stop covering its group
func (r *ProcessRegistry) releaseJobGroup(s *ProcessSession) {
	r.ownedPgidMu.Lock()
	ownedPgid := s.ownedPgid
	s.ownedPgid = 0
	r.ownedPgidMu.Unlock()
	if ownedPgid != 0 {
		releaseProcessGroups([]int{ownedPgid})
	}
}

// Awaiting, whether a background process will still report back to its spawner

var (
	inFlightMu     sync.Mutex
	inFlightOwners = map[string]int{}
)

func eventOwner(evt map[string]any) string {
	if evt == nil {
		return ""
	}
	return asStr(evt["owner_task_id"])
}

func adjustInFlight(owners []string, delta int) {
	inFlightMu.Lock()
	defer inFlightMu.Unlock()
	for _, owner := range owners {
		if owner == "" {
			continue
		}
		inFlightOwners[owner] += delta
		if inFlightOwners[owner] <= 0 {
			delete(inFlightOwners, owner)
		}
	}
}

// TakeReady dequeues everything queued, waiting up to timeout for the first event, holding each owner in flight
func TakeReady(q *EventQueue, timeout time.Duration) []map[string]any {
	q.waitNotEmpty(timeout)
	var events []map[string]any
	q.withItems(func(items []map[string]any) []map[string]any {
		events = append([]map[string]any{}, items...)
		if len(events) > 0 {
			owners := make([]string, 0, len(events))
			for _, e := range events {
				owners = append(owners, eventOwner(e))
			}
			adjustInFlight(owners, +1)
			if AsyncDelegationLedger != nil {
				for _, evt := range events {
					AsyncDelegationLedger.MarkCopyHeld(AsyncDelegationLedger.CopyID(evt))
				}
			}
		}
		return items[:0]
	})
	return events
}

// ReleaseEvents ends the in flight hold TakeReady took for the events
func ReleaseEvents(events []map[string]any) {
	if len(events) == 0 {
		return
	}
	owners := make([]string, 0, len(events))
	for _, e := range events {
		owners = append(owners, eventOwner(e))
	}
	adjustInFlight(owners, -1)
	if AsyncDelegationLedger != nil {
		for _, evt := range events {
			AsyncDelegationLedger.UnmarkCopyHeld(AsyncDelegationLedger.CopyID(evt))
		}
	}
}

// DrainedFor dequeues everything for a caller that consumes only the events owns accepts, requeueing the rest
func (r *ProcessRegistry) DrainedFor(owns func(map[string]any) bool, fn func(mine []map[string]any)) {
	r.RestoreCompletions()
	ready := TakeReady(r.CompletionQueue, 0)
	defer ReleaseEvents(ready)
	mine := []map[string]any{}
	accepted := map[*map[string]any]bool{}
	for i := range ready {
		if owns(ready[i]) {
			mine = append(mine, ready[i])
			accepted[&ready[i]] = true
		}
	}
	for i := range ready {
		if !accepted[&ready[i]] {
			r.CompletionQueue.Put(ready[i])
		}
	}
	fn(mine)
}

// A finished entry whose completion is not yet published stays until publication
func retainedForDelivery(s *ProcessSession) bool {
	return s.finishClaimed && !s.completionEvent.isSet()
}

func sessionNotifies(s *ProcessSession) bool {
	return s.NotifyOnComplete || (len(s.WatchPatterns) > 0 && !s.watchDisabled)
}

// AwaitsNotification reports whether a process spawned by one of the owners still has a notice on its way
func (r *ProcessRegistry) AwaitsNotification(ownerTaskIDs []string) bool {
	owners := map[string]bool{}
	for _, o := range ownerTaskIDs {
		if o != "" {
			owners[o] = true
		}
	}
	if len(owners) == 0 {
		return false
	}
	if r.anyRunning(func(s *ProcessSession) bool { return owners[s.OwnerTaskID] && sessionNotifies(s) }) {
		return true
	}
	r.mu.Lock()
	for _, store := range []map[string]*ProcessSession{r.running, r.finished} {
		for _, s := range store {
			if owners[s.OwnerTaskID] && s.NotifyOnComplete && s.isExited() && !s.completionEvent.isSet() {
				r.mu.Unlock()
				return true
			}
		}
	}
	r.mu.Unlock()
	found := false
	r.CompletionQueue.withItems(func(items []map[string]any) []map[string]any {
		inFlightMu.Lock()
		for o := range owners {
			if inFlightOwners[o] > 0 {
				found = true
			}
		}
		inFlightMu.Unlock()
		if !found {
			for _, evt := range items {
				if owners[eventOwner(evt)] {
					found = true
					break
				}
			}
		}
		return items
	})
	return found
}

// Wait and poll policy, promoting a silent running process to notify_on_complete

const (
	waitPolicyPromoteAfter = 2
	waitPolicyMaxTracked   = 512
)

type waitPolicyEntry struct {
	lastTotal int
	count     int
}

var (
	waitPolicyMu    sync.Mutex
	waitPolicyState = map[string]waitPolicyEntry{}
	waitPolicyOrder = []string{}
)

const waitPolicyStopNote = "The process is still running and has printed nothing new since your last check. icebath has turned on " +
	"notify_on_complete for it: when it exits, a message with its exit code and final output starts a new turn " +
	"automatically, even after you end this turn. Do NOT call process_manage wait or poll on %s again, and do " +
	"not sleep or loop to wait for it. If you have other work, do it now; otherwise end your turn with a one-line " +
	"status and report the result when the completion message arrives."

// terminal.process_wait_auto_notify, true unless the config says false
func waitPolicyEnabled(env *Env) bool {
	v, ok := cfgTree(env).Lookup("terminal.process_wait_auto_notify")
	if !ok {
		return true
	}
	b, isBool := v.(bool)
	return !(isBool && !b)
}

func waitPolicyForget(sessionID string) {
	waitPolicyMu.Lock()
	defer waitPolicyMu.Unlock()
	delete(waitPolicyState, sessionID)
}

// Records one still running wait or poll observation and returns the consecutive no new output count
func waitPolicyCountUnchanged(s *ProcessSession) int {
	total := s.TotalOutputChars
	waitPolicyMu.Lock()
	defer waitPolicyMu.Unlock()
	entry, known := waitPolicyState[s.ID]
	count := 0
	if total == entry.lastTotal {
		count = entry.count + 1
	}
	if !known {
		waitPolicyOrder = append(waitPolicyOrder, s.ID)
	}
	waitPolicyState[s.ID] = waitPolicyEntry{lastTotal: total, count: count}
	for len(waitPolicyState) > waitPolicyMaxTracked && len(waitPolicyOrder) > 0 {
		oldest := waitPolicyOrder[0]
		waitPolicyOrder = waitPolicyOrder[1:]
		delete(waitPolicyState, oldest)
	}
	return count
}

// Gate env read with per profile isolation under multiplexing, the process env otherwise
func platformGateEnv(ctx context.Context, env *Env, name string) string {
	if name == "" {
		return ""
	}
	if ProfileSecrets != nil && isMultiplexActive(env) {
		if scope := profileSecrets().CurrentSecretScope(ctx); scope != nil {
			return strings.TrimSpace(scope[name])
		}
	}
	return strings.TrimSpace(os.Getenv(name))
}

// Whether a completion notice for this session can reach the agent that is waiting
func canDeliverCompletion(ctx context.Context, env *Env) bool {
	finite, known := false, false
	if FiniteTurnRequired != nil {
		finite, known = FiniteTurnRequired(env)
	}
	if (known && finite) || (!known && sessionEnvValue(env, "ICEBATH_SINGLE_QUERY_SESSION") == "1") {
		return false
	}
	supported := true
	if AsyncDeliverySupported != nil {
		supported = AsyncDeliverySupported(env)
	}
	if !supported || isDelegatedChildProcessContext(env) {
		return false
	}
	modes := []any{platformGateEnv(ctx, env, "ICEBATH_BACKGROUND_NOTIFICATIONS")}
	if v, ok := cfgTree(env).Lookup("display.background_process_notifications"); ok {
		modes = append(modes, v)
	}
	for _, m := range modes {
		if b, ok := m.(bool); ok && !b {
			return false
		}
		if strings.ToLower(strings.TrimSpace(asStr(m))) == "off" {
			return false
		}
	}
	return true
}

// Whether the completion this process would publish is routed to the caller
func callerReceivesCompletion(env *Env, s *ProcessSession) bool {
	callerKey := sessionEnvValue(env, "ICEBATH_SESSION_KEY")
	evt := map[string]any{"type": "completion", "session_id": s.ID, "session_key": s.SessionKey,
		"task_id": s.TaskID, "owner_task_id": s.OwnerTaskID}
	if s.SessionKey != "" && callerKey == "" {
		return false
	}
	return ownsEvent(evt, callerKey, nil, false) && shouldSurfaceNotification(env, evt, nil)
}

// Sets notify_on_complete unless the exit already won, false means it did
func (r *ProcessRegistry) claimNotifyIfRunning(s *ProcessSession) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, running := r.running[s.ID]; s.finishClaimed || !running {
		return false
	}
	s.NotifyOnComplete = true
	return true
}

// Wires completion delivery for a process just claimed, stamping routing and arming a gateway watcher
func (r *ProcessRegistry) promoteNotify(env *Env, s *ProcessSession) {
	if s.WatcherPlatform == "" {
		stampGatewayRouting(env, s)
	}
	if s.WatcherPlatform != "" && s.WatcherInterval == 0 {
		registerCompletionWatcher(s, s.SessionKey)
	}
	r.writeCheckpoint(env, nil)
	toolsLogger(env).Info(fmt.Sprintf("process %s: repeated wait/poll with no new output; auto-enabled notify_on_complete", s.ID))
}

// Post processes one process_manage session action result, promoting a silent process to notify_on_complete
func applyWaitPollPolicy(ctx context.Context, env *Env, action, sessionID string, result map[string]any) map[string]any {
	if result == nil || !waitPolicyEnabled(env) {
		return result
	}
	s := processRegistry.Get(env, sessionID)
	if s == nil {
		return result
	}
	running := false
	switch action {
	case "wait":
		running = result["status"] == "timeout"
	case "poll":
		running = result["status"] == "running"
	}
	if !running {
		if (action != "wait" && action != "poll") || result["status"] != "interrupted" {
			waitPolicyForget(s.ID)
		}
		return result
	}
	if s.isExited() || waitPolicyCountUnchanged(s) < waitPolicyPromoteAfter || !canDeliverCompletion(ctx, env) {
		return result
	}
	if !callerReceivesCompletion(env, s) {
		return result
	}
	if !s.NotifyOnComplete {
		if !processRegistry.claimNotifyIfRunning(s) {
			waitPolicyForget(s.ID)
			if action == "poll" {
				return processRegistry.Poll(WithEnvHome(ctx, env), env, sessionID)
			}
			return processRegistry.Wait(ctx, env, sessionID, procIntPtr(5))
		}
		processRegistry.promoteNotify(env, s)
	}
	note := fmt.Sprintf(waitPolicyStopNote, s.ID)
	result["notify_on_complete"] = true
	if action == "wait" {
		clamp := asStr(result["timeout_note"])
		if strings.HasPrefix(clamp, "Requested wait") {
			clamp = strings.SplitN(clamp, ". Wait window", 2)[0] + ". "
		} else {
			clamp = ""
		}
		result["timeout_note"] = clamp + note
	} else {
		result["note"] = note
	}
	return result
}

// Human readable rendering of completion queue events

var doneStatuses = setOf("completed", "success")

var reasonStatus = map[string]string{
	"lost":         "marked lost because the process backend disappeared",
	"failed_start": "failed to start",
}

// ProcessNotificationBatch keeps completion identity until the owning surface starts its turn
type ProcessNotificationBatch struct {
	Notifications []DrainedNotification
}

func (b ProcessNotificationBatch) live(r *ProcessRegistry) []DrainedNotification {
	out := []DrainedNotification{}
	for _, n := range b.Notifications {
		if !r.IsCompletionConsumed(asStr(n.Event["session_id"])) {
			out = append(out, n)
		}
	}
	return out
}

// Render joins the live notifications under a batch header, empty when none remain
func (b ProcessNotificationBatch) Render(r *ProcessRegistry) string {
	messages := []string{}
	for _, n := range b.live(r) {
		messages = append(messages, n.Text)
	}
	if len(messages) == 0 {
		return ""
	}
	if len(messages) == 1 {
		return messages[0]
	}
	header := fmt.Sprintf("[IMPORTANT: %d background processes completed. Treat these results as one batch and give one consolidated response; preserve failures and actionable results.]", len(messages))
	return strings.Join(append([]string{header}, messages...), "\n\n")
}

// DisplayText is the compact UI title of the live completions
func (b ProcessNotificationBatch) DisplayText(r *ProcessRegistry) string {
	events := []map[string]any{}
	for _, n := range b.live(r) {
		events = append(events, n.Event)
	}
	return ProcessCompletionDisplayText(events)
}

// GroupProcessNotifications groups consecutive completions, watches and delegations are barriers
func GroupProcessNotifications(notifications []DrainedNotification) []ProcessNotificationBatch {
	out := []ProcessNotificationBatch{}
	batch := []DrainedNotification{}
	for _, n := range notifications {
		evtType := "completion"
		if t := asStr(n.Event["type"]); t != "" {
			evtType = t
		}
		if evtType == "completion" {
			batch = append(batch, n)
			continue
		}
		if len(batch) > 0 {
			out = append(out, ProcessNotificationBatch{Notifications: batch})
			batch = []DrainedNotification{}
		}
		out = append(out, ProcessNotificationBatch{Notifications: []DrainedNotification{n}})
	}
	if len(batch) > 0 {
		out = append(out, ProcessNotificationBatch{Notifications: batch})
	}
	return out
}

// Raw child process noise rather than a delegation or work closeout deliverable
func isChildProcessNotification(evt map[string]any) bool {
	if t := asStr(evt["type"]); t == "async_delegation" || t == "async_delegation_work_closeout" {
		return false
	}
	owner := asStr(evt["owner_task_id"])
	if owner == "" {
		owner = asStr(evt["task_id"])
	}
	return strings.HasPrefix(owner, "sa-")
}

// Shared surfacing policy for subagent owned process events
func shouldSurfaceNotification(env *Env, evt map[string]any, surfaceChild *bool) bool {
	if !isChildProcessNotification(evt) {
		return true
	}
	if surfaceChild == nil {
		return surfaceChildProcessNotifications(env)
	}
	return *surfaceChild
}

// Human friendly elapsed string such as 18m, 2h3m or 45s
func formatAge(seconds float64) string {
	s := int(max(0, seconds))
	if s < 60 {
		return fmt.Sprintf("%ds", s)
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

// Model not found phrases from the failover classifier, a minimal built in set when none is installed
func modelNotFoundPatterns() []string {
	if ModelNotFoundPatterns != nil {
		return ModelNotFoundPatterns()
	}
	return []string{"is not a valid model", "model not found", "model_not_found"}
}

func delegationConfig(env *Env) map[string]any {
	if DelegationConfig == nil {
		return map[string]any{}
	}
	cfg := DelegationConfig(env)
	if cfg == nil {
		return map[string]any{}
	}
	return cfg
}

// A result reflecting a config level model not found rejection of the configured model
func delegationModelNotFound(results []map[string]any, cfg map[string]any) bool {
	model := strings.ToLower(asStr(cfg["model"]))
	if model == "" {
		return false
	}
	patterns := modelNotFoundPatterns()
	for _, r := range results {
		parts := []string{}
		for _, key := range []string{"error", "summary"} {
			if v := r[key]; v != nil && asStr(v) != "" {
				parts = append(parts, asStr(v))
			}
		}
		text := strings.ToLower(strings.Join(parts, " "))
		if !strings.Contains(text, model) {
			continue
		}
		for _, p := range patterns {
			if strings.Contains(text, p) {
				return true
			}
		}
	}
	return false
}

// Config level model not found notice lines, nil when the notice does not apply
func delegationModelNotFoundNotice(env *Env, results []map[string]any) []string {
	cfg := delegationConfig(env)
	if !delegationModelNotFound(results, cfg) {
		return nil
	}
	model := asStr(cfg["model"])
	if model == "" {
		model = "?"
	}
	provider := asStr(cfg["provider"])
	if provider == "" {
		provider = "configured provider"
	}
	lines := []string{
		fmt.Sprintf("⚠ SUBAGENT MODEL REJECTED: the configured Subagent Model %q was rejected by provider %q (HTTP 400: not a valid model ID).", model, provider),
		"Every task in this batch failed for this reason before doing any work.",
		"Check Settings → Advanced → Subagent Model (or: icebath config get delegation.model).",
	}
	if DelegationFallbackChain != nil && len(DelegationFallbackChain(cfg)) == 0 {
		lines = append(lines, "No fallback chain is configured, so no failover was attempted.")
	}
	return lines
}

const truncatedSummaryNote = "[TRUNCATED — subagent hit its iteration cap; the summary below " +
	"may be incomplete. Verify before relying on it, or re-dispatch " +
	"the unfinished part.]"

func isTruncatedResult(entry map[string]any) bool {
	truncated, _ := entry["truncated"].(bool)
	return truncated || entry["exit_reason"] == "max_iterations"
}

func noticeLines(env *Env, results []map[string]any) []string {
	notice := delegationModelNotFoundNotice(env, results)
	if notice == nil {
		return nil
	}
	return append([]string{""}, notice...)
}

// Result entries of an event as maps, live pyjson objects and decoded maps alike, other items dropped
func resultList(v any) []map[string]any {
	out := []map[string]any{}
	switch list := v.(type) {
	case []map[string]any:
		return list
	case []*pyjson.Object:
		for _, item := range list {
			if item != nil {
				out = append(out, item.Vals)
			}
		}
	case []any:
		for _, item := range list {
			switch m := item.(type) {
			case map[string]any:
				out = append(out, m)
			case *pyjson.Object:
				if m != nil {
					out = append(out, m.Vals)
				}
			}
		}
	}
	return out
}

func anyToFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// Value of key or the placeholder when absent
func valueOr(evt map[string]any, key string, placeholder string) string {
	if v, ok := evt[key]; ok && v != nil {
		return pyStr(v)
	}
	return placeholder
}

// Shared preamble of a delegation notice, title, intro, dispatch time, goal, context, toolsets, role and model
func preamble(evt map[string]any, title, intro string, completedAt float64, withGoal bool) []string {
	lines := []string{title, intro, ""}
	if dispatchedAt, ok := anyToFloat(evt["dispatched_at"]); ok {
		ts := time.Unix(int64(dispatchedAt), 0).Local().Format("2006-01-02 15:04:05")
		lines = append(lines, fmt.Sprintf("Dispatched: %s (%s ago)", ts, formatAge(completedAt-dispatchedAt)))
	}
	if withGoal {
		lines = append(lines, "Original goal: "+asStr(evt["goal"]))
	}
	if ctx := asStr(evt["context"]); ctx != "" {
		lines = append(lines, "Context you provided: "+ctx)
	}
	if toolsets := stringList(evt["toolsets"]); len(toolsets) > 0 {
		lines = append(lines, "Toolsets: "+strings.Join(toolsets, ", "))
	}
	role := asStr(evt["role"])
	if role == "" {
		role = "leaf"
	}
	line := "Role: " + role
	if model := asStr(evt["model"]); model != "" {
		line += "   Model: " + model
	}
	return append(lines, line)
}

// One child of a still running fan out failed
func formatTaskFailureNotice(evt map[string]any, delegID string) string {
	results := resultList(evt["results"])
	r := map[string]any{}
	if len(results) > 0 {
		r = results[0]
	}
	goals := stringList(evt["goals"])
	idx, _ := asIntValue(r["task_index"])
	n, ok := asIntValue(evt["n_tasks"])
	if !ok || n == 0 {
		n = len(goals)
	}
	goal := asStr(r["goal"])
	if idx < len(goals) {
		goal = goals[idx]
	}
	errText := strings.ReplaceAll(strings.TrimSpace(asStr(r["error"])), "\n", " ")
	errText = headChars(errText, 400)
	lines := []string{
		fmt.Sprintf("[ASYNC DELEGATION TASK FAILED — %s, task %d/%d]", delegID, idx+1, n),
		"One subagent in a background fan-out you dispatched has failed while its siblings are still running. " +
			"The batch's consolidated results will still arrive when the last sibling finishes; this is an early " +
			"warning so you can re-dispatch or investigate now instead of then.",
	}
	if goal != "" {
		lines = append(lines, "Task: "+goal)
	}
	status := fmt.Sprintf("Status: %s   Duration: %ss", valueOr(r, "status", "?"), valueOr(r, "duration_seconds", "?"))
	if errText != "" {
		status += "\nError: " + errText
	}
	lines = append(lines, status)
	if lt := asStr(r["live_transcript"]); lt != "" {
		lines = append(lines, "Live transcript: "+lt)
	}
	return strings.Join(lines, "\n")
}

// Owner died recovery diagnostics
func recoveryLines(evt map[string]any) []string {
	last := asStr(evt["last_known_status"])
	if last == "" {
		return nil
	}
	lines := []string{fmt.Sprintf("Last persisted unit status: %s (before owner exit; not current liveness). Unrecorded outcomes remain unknown; inspect evidence before retrying side effects.", last)}
	tails, _ := evt["transcript_tails"].(map[string]any)
	transcripts, _ := evt["task_transcripts"].(map[string]any)
	keys := make([]string, 0, len(transcripts))
	for k := range transcripts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, index := range keys {
		lines = append(lines, fmt.Sprintf("Task index %s transcript (may be incomplete): %s", index, pyStr(transcripts[index])))
		if tail := asStr(tails[index]); tail != "" {
			lines = append(lines, fmt.Sprintf("--- last lines of task %s transcript ---", index), tail, "--- end ---")
		}
	}
	if hint := asStr(evt["git_state_hint"]); hint != "" {
		lines = append(lines, "Owner working tree at recovery: "+hint)
	}
	return lines
}

// Consolidated block for a delegate_task fan out that finished as one unit
func formatBatchDelegation(env *Env, evt map[string]any, delegID string, completedAt float64) string {
	results := resultList(evt["results"])
	goals := stringList(evt["goals"])
	n := len(goals)
	if n == 0 {
		n = len(results)
	}
	nUnit := len(results)
	if nUnit == 0 {
		nUnit = len(goals)
	}
	unit := fmt.Sprintf("%d subagent(s)", nUnit)
	if group, ok := evt["group"]; ok && group != nil {
		unit = fmt.Sprintf("group '%s' (%d subagent(s))", pyStr(group), nUnit)
	}
	lines := preamble(evt,
		fmt.Sprintf("[ASYNC DELEGATION BATCH COMPLETE — %s]", delegID),
		fmt.Sprintf("A background fan-out unit you dispatched earlier — %s — has finished; its consolidated results are "+
			"below. Any other units from the same delegate_task call report separately as they finish. You may have "+
			"moved on since dispatching — act on these or re-dispatch if things have changed. If you are still waiting "+
			"on siblings, end your turn after acting on this one.", unit),
		completedAt, false)
	total := valueOr(evt, "total_duration_seconds", valueOr(evt, "duration_seconds", "?"))
	lines[len(lines)-1] += fmt.Sprintf("   Total duration: %ss", total)
	lines = append(lines, recoveryLines(evt)...)
	if errText := asStr(evt["error"]); errText != "" && len(results) == 0 {
		lines = append(lines, "--- ERROR ---", "The batch did not complete successfully: "+errText)
		return strings.Join(lines, "\n")
	}
	lines = append(lines, noticeLines(env, results)...)
	sorted := append([]map[string]any{}, results...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, _ := asIntValue(sorted[i]["task_index"])
		b, _ := asIntValue(sorted[j]["task_index"])
		return a < b
	})
	for _, r := range sorted {
		idx, _ := asIntValue(r["task_index"])
		rTruncated := isTruncatedResult(r)
		rStatus := valueOr(r, "status", "?")
		rSummary := asStr(r["summary"])
		rError := asStr(r["error"])
		rGoal := asStr(r["goal"])
		if idx < len(goals) {
			rGoal = goals[idx]
		}
		icon := "✗"
		if rTruncated {
			icon = "⚠"
		} else if doneStatuses[rStatus] {
			icon = "✓"
		}
		header := fmt.Sprintf("--- %s TASK %d/%d", icon, idx+1, n)
		if rGoal != "" {
			header += ": " + rGoal
		}
		header += fmt.Sprintf("  (status=%s", rStatus)
		if calls := r["api_calls"]; calls != nil && pyStr(calls) != "0" && pyStr(calls) != "" && pyStr(calls) != "False" {
			header += ", api_calls=" + pyStr(calls)
		}
		if dur, ok := r["duration_seconds"]; ok && dur != nil {
			header += fmt.Sprintf(", %ss", pyStr(dur))
		}
		if rTruncated {
			header += ", TRUNCATED: hit max_iterations — work may be incomplete"
		}
		lines = append(lines, "", header+") ---")
		switch {
		case doneStatuses[rStatus] && rSummary != "":
			if rTruncated {
				lines = append(lines, truncatedSummaryNote)
			}
			lines = append(lines, rSummary)
		case rSummary != "":
			if rError != "" {
				lines = append(lines, fmt.Sprintf("(%s: %s)", rStatus, rError))
			}
			lines = append(lines, "Partial output:", rSummary)
		default:
			line := fmt.Sprintf("(no summary — status=%s", rStatus)
			if rError != "" {
				line += ": " + rError
			}
			lines = append(lines, line+")")
		}
		if lt := asStr(r["live_transcript"]); lt != "" {
			lines = append(lines, "Full live transcript (complete tool/assistant trace): "+lt)
		}
		lines = append(lines, processAccountingLines(r)...)
	}
	return strings.Join(lines, "\n")
}

// Runtime truth lines about a child's background processes
func processAccountingLines(r map[string]any) []string {
	lines := []string{}
	for _, h := range resultList(r["handed_off_processes"]) {
		lines = append(lines, fmt.Sprintf("Handed off to you: %s (%s) — %s. You own it now; its completion notice will arrive here.",
			pyStr(h["session_id"]), headChars(asStr(h["command"]), 120), asStr(h["note"])))
	}
	orphans := resultList(r["orphaned_processes"])
	if len(orphans) > 0 {
		parts := []string{}
		for _, o := range orphans {
			parts = append(parts, fmt.Sprintf("%s `%s` (%ss)", pyStr(o["session_id"]), headChars(asStr(o["command"]), 100), pyStr(o["runtime_seconds"])))
		}
		lines = append(lines, fmt.Sprintf("Child left %d background process(es) running that were TERMINATED with it (subagent process notices never reach you): %s. Re-launch in this session anything you still need.", len(orphans), strings.Join(parts, "; ")))
	}
	for _, u := range resultList(r["unread_completions"]) {
		lines = append(lines, fmt.Sprintf("Child's process %s `%s` finished (exit code %s) but the child never read its result; output tail:\n%s",
			pyStr(u["session_id"]), headChars(asStr(u["command"]), 100), pyStr(u["exit_code"]), asStr(u["output_tail"])))
	}
	return lines
}

// ProcessAccountingLines are the runtime truth lines about one result entry background processes
func ProcessAccountingLines(r map[string]any) []string { return processAccountingLines(r) }

// Self contained re injection for an async delegation completion
func formatAsyncDelegation(env *Env, evt map[string]any) string {
	delegID := valueOr(evt, "delegation_id", "unknown")
	completedAt, ok := anyToFloat(evt["completed_at"])
	if !ok || completedAt == 0 {
		completedAt = nowSeconds()
	}
	if v, ok := evt["task_failure_notice"]; ok && v != nil && v != false {
		return formatTaskFailureNotice(evt, delegID)
	}
	isBatch, _ := evt["is_batch"].(bool)
	_, hasResults := evt["results"].([]any)
	if !hasResults {
		_, hasResults = evt["results"].([]map[string]any)
	}
	if isBatch || hasResults {
		return formatBatchDelegation(env, evt, delegID, completedAt)
	}
	status := asStr(evt["status"])
	if status == "" {
		status = "completed"
	}
	summary := asStr(evt["summary"])
	errText := asStr(evt["error"])
	truncated := isTruncatedResult(evt)
	lines := preamble(evt,
		fmt.Sprintf("[ASYNC DELEGATION COMPLETE — %s]", delegID),
		"A background subagent you dispatched earlier has finished. You may "+
			"have moved on since dispatching it; the full task source is below so "+
			"you can act on the result or re-dispatch if things have changed.",
		completedAt, true)
	lines = append(lines, noticeLines(env, []map[string]any{evt})...)
	statusLine := "Status: " + status
	if calls, ok := evt["api_calls"]; ok && calls != nil {
		statusLine += "   API calls: " + pyStr(calls)
	}
	statusLine += fmt.Sprintf("   Duration: %ss", valueOr(evt, "duration_seconds", "?"))
	if truncated {
		statusLine += " [TRUNCATED: hit max_iterations — work may be incomplete]"
	}
	lines = append(lines, statusLine, "--- RESULT ---")
	if doneStatuses[status] && summary != "" {
		if truncated {
			lines = append(lines, truncatedSummaryNote)
		}
		lines = append(lines, summary)
	} else {
		if status == "interrupted" {
			line := "The subagent was interrupted before completing"
			if errText != "" {
				line += ": " + errText
			} else {
				line += "."
			}
			lines = append(lines, line)
		} else {
			line := fmt.Sprintf("The subagent did not complete successfully (status=%s).", status)
			if errText != "" {
				line += "\n" + errText
			}
			lines = append(lines, line)
			lines = append(lines, recoveryLines(evt)...)
		}
		if summary != "" {
			lines = append(lines, "Partial output:", summary)
		}
	}
	return strings.Join(lines, "\n")
}

// AsyncDelegationDisplayText is the compact UI title of a delegation completion
func AsyncDelegationDisplayText(evt map[string]any) string {
	results := resultList(evt["results"])
	if len(results) == 0 {
		results = []map[string]any{evt}
	}
	goals := stringList(evt["goals"])
	statusLabels := map[string]string{
		"failed": "Failed", "error": "Failed", "cancelled": "Cancelled", "interrupted": "Interrupted",
		"timeout": "Timed Out", "stalled": "Stalled", "unknown": "Unknown", "rejected": "Failed",
	}
	labels, titles := []string{}, []string{}
	for _, result := range results {
		status := asStr(result["status"])
		if status == "" {
			status = "completed"
			if e := result["error"]; e != nil && asStr(e) != "" {
				status = "failed"
			}
		}
		label := "Incomplete"
		if doneStatuses[status] {
			if !isTruncatedResult(result) {
				label = "Completed"
			}
		} else if l, ok := statusLabels[status]; ok {
			label = l
		}
		labels = append(labels, label)
		index, _ := asIntValue(result["task_index"])
		goal := asStr(result["goal"])
		if index >= 0 && index < len(goals) {
			goal = goals[index]
		}
		if goal == "" {
			goal = "Background task"
		}
		titles = append(titles, strings.Join(strings.Fields(goal), " "))
	}
	if len(results) == 1 {
		return fmt.Sprintf("Subagent Task %s: %s", labels[0], shortTitle(titles[0]))
	}
	outcome := "Finished with Issues"
	same := true
	for _, l := range labels[1:] {
		if l != labels[0] {
			same = false
			break
		}
	}
	if same {
		outcome = labels[0]
	}
	title := strings.Join(strings.Fields(asStr(evt["group"])), " ")
	if title == "" {
		title = strings.Join(titles, "; ")
	}
	return fmt.Sprintf("Subagent Tasks %s: %s (%d tasks)", outcome, shortTitle(title), len(results))
}

func shortTitle(title string) string {
	if utf8Len(title) > 200 {
		return headChars(title, 197) + "..."
	}
	return title
}

func utf8Len(s string) int { return pystr.Len(s) }

// ProcessCompleteDisplayKind names the UI row kind of a process completion
const ProcessCompleteDisplayKind = "process_complete"

func shortCommand(command any) string {
	cmd := strings.Join(strings.Fields(asStr(command)), " ")
	if utf8Len(cmd) > 80 {
		return headChars(cmd, 77) + "..."
	}
	return cmd
}

// ProcessCompletionDisplayText is the compact UI title of one or more process completions
func ProcessCompletionDisplayText(events []map[string]any) string {
	if len(events) != 1 {
		return fmt.Sprintf("%d Background Processes Finished", len(events))
	}
	evt := events[0]
	reason := asStr(evt["completion_reason"])
	if reason == "" {
		reason = "exited"
	}
	exitCode := valueOr(evt, "exit_code", "?")
	var outcome string
	switch {
	case reason == "killed":
		outcome = "Terminated"
	case reason == "lost":
		outcome = "Lost"
	case reason == "failed_start":
		outcome = "Failed to Start"
	case exitCode == "0":
		outcome = "Finished"
	default:
		outcome = "Failed"
	}
	cmd := shortCommand(evt["command"])
	detail := ""
	if _, special := reasonStatus[reason]; reason != "killed" && !special && exitCode != "0" {
		detail = fmt.Sprintf(" (exit %s)", exitCode)
	}
	if cmd != "" {
		return fmt.Sprintf("Background Process %s%s: %s", outcome, detail, cmd)
	}
	return fmt.Sprintf("Background Process %s%s", outcome, detail)
}

// HeartbeatDisplayKind hides heartbeat rows, a wake rather than a message
const HeartbeatDisplayKind = "hidden"

// HeartbeatOutputChars caps the output a heartbeat tick carries
const HeartbeatOutputChars = 2000

// A tick's output capped to its newest characters and the count cut before it
func heartbeatOutputTail(output string, omitted int) (string, int) {
	if n := utf8Len(output); n > HeartbeatOutputChars {
		omitted += n - HeartbeatOutputChars
		output = tailChars(output, HeartbeatOutputChars)
	}
	return output, omitted
}

// JoinHeartbeatOutputs merges two consecutive ticks' output and omitted counts as one tick's
func JoinHeartbeatOutputs(earlier, later map[string]any) map[string]any {
	earlierCut, _ := asIntValue(earlier["omitted"])
	earlierText := asStr(earlier["output"])
	laterCut, _ := asIntValue(later["omitted"])
	laterText := asStr(later["output"])
	var output string
	var omitted int
	if laterCut != 0 {
		output, omitted = heartbeatOutputTail(laterText, earlierCut+utf8Len(earlierText)+laterCut)
	} else {
		output, omitted = heartbeatOutputTail(earlierText+laterText, earlierCut)
	}
	return map[string]any{"output": output, "omitted": omitted}
}

// HeartbeatOutputText renders a tick's output with its cut marker
func HeartbeatOutputText(evt map[string]any) string {
	omitted, _ := asIntValue(evt["omitted"])
	output := asStr(evt["output"])
	if omitted != 0 {
		return fmt.Sprintf("...(%d earlier characters omitted)\n%s", omitted, output)
	}
	return output
}

// HeartbeatDisplayText is the one line CLI receipt of a heartbeat wake
func HeartbeatDisplayText(evt map[string]any) string {
	cmd := shortCommand(evt["command"])
	elapsed, _ := anyToFloat(evt["elapsed"])
	age := formatAge(elapsed)
	if cmd != "" {
		return fmt.Sprintf("Background Process Output after %s: %s", age, cmd)
	}
	return "Background Process Output after " + age
}

// TimelineNotification is queued model text with the display kind and compact title a surface paints
type TimelineNotification struct {
	Text                 string
	DisplayText          string
	DisplayKind          string
	NotificationCategory string
}

// NewTimelineNotification builds a notification with the result category by default
func NewTimelineNotification(text, displayText, displayKind, category string) TimelineNotification {
	if category == "" {
		category = "result"
	}
	return TimelineNotification{Text: text, DisplayText: displayText, DisplayKind: displayKind, NotificationCategory: category}
}

// TimelineNotificationForDelegation renders a delegation event as a timeline notification
func TimelineNotificationForDelegation(text string, event map[string]any) TimelineNotification {
	if DiagnosticProcessEvent == nil {
		panic("tools: DiagnosticProcessEvent must be installed by the agent notification presentation")
	}
	category := "result"
	if DiagnosticProcessEvent(event) {
		category = "diagnostic"
	}
	return NewTimelineNotification(text, AsyncDelegationDisplayText(event), "async_delegation_complete", category)
}

// One line provenance for a subagent owned process event, empty otherwise
func delegationAttributionLine(evt map[string]any) string {
	taskID := asStr(evt["owner_task_id"])
	if taskID == "" {
		taskID = asStr(evt["task_id"])
	}
	if !strings.HasPrefix(taskID, "sa-") {
		return ""
	}
	var info map[string]any
	if SubagentAttribution != nil {
		info = SubagentAttribution(taskID)
	}
	if len(info) == 0 {
		return fmt.Sprintf("Started by subagent %s (delegate_task).", taskID)
	}
	goal := strings.TrimSpace(asStr(info["goal"]))
	if utf8Len(goal) > 120 {
		goal = headChars(goal, 117) + "..."
	}
	line := "Started by subagent " + taskID
	if deleg := info["delegation_id"]; deleg != nil && asStr(deleg) != "" {
		line += " of delegation " + pyStr(deleg)
	}
	line += "."
	if goal != "" {
		line += fmt.Sprintf(" Task: %q", goal)
	}
	return line
}

func completionStatus(evt map[string]any) string {
	reason := asStr(evt["completion_reason"])
	if reason == "" {
		reason = "exited"
	}
	if reason == "killed" {
		source := asStr(evt["termination_source"])
		if source == "" {
			source = "icebath"
		}
		return "terminated by " + source
	}
	if text, ok := reasonStatus[reason]; ok {
		return text
	}
	if valueOr(evt, "exit_code", "?") == "0" {
		return "completed normally"
	}
	return "exited"
}

// Formats a completion queue event into the IMPORTANT message injected into the conversation
func formatProcessNotification(evt map[string]any) string {
	return formatProcessNotificationWith(nil, evt)
}

// IMPORTANT message a background process completion queue event injects into the conversation
func FormatProcessNotification(evt map[string]any) string { return formatProcessNotification(evt) }

func formatProcessNotificationWith(env *Env, evt map[string]any) string {
	evtType := asStr(evt["type"])
	if evtType == "" {
		evtType = "completion"
	}
	switch evtType {
	case "watch_disabled", "watch_overflow_tripped", "watch_overflow_released":
		return fmt.Sprintf("[IMPORTANT: %s]", asStr(evt["message"]))
	case "async_delegation":
		return formatAsyncDelegation(env, evt)
	}
	sid := valueOr(evt, "session_id", "unknown")
	cmd := valueOr(evt, "command", "unknown")
	attributionLine := delegationAttributionLine(evt)
	if note := asStr(evt["handoff_note"]); note != "" {
		attributionLine = "Handed off to you by a subagent before it finished. Purpose: " + note
	}
	attribution := ""
	if attributionLine != "" {
		attribution = attributionLine + "\n"
	}
	if evtType == "heartbeat" {
		elapsed, _ := anyToFloat(evt["elapsed"])
		return fmt.Sprintf("[Background process %s heartbeat #%s — still running after %s (next in %ss when there is new output; you will also be told when it exits).\n%sCommand: %s\nOutput since last heartbeat:\n%s]",
			sid, valueOr(evt, "seq", "?"), formatAge(elapsed), valueOr(evt, "interval", "?"), attribution, cmd, HeartbeatOutputText(evt))
	}
	if evtType == "watch_match" {
		sup, _ := asIntValue(evt["suppressed"])
		text := fmt.Sprintf("[IMPORTANT: Background process %s matched watch pattern \"%s\".\n%sCommand: %s\nMatched output:\n%s",
			sid, valueOr(evt, "pattern", "?"), attribution, cmd, asStr(evt["output"]))
		if sup != 0 {
			text += fmt.Sprintf("\n(%d earlier matches were suppressed by rate limit)", sup)
		}
		return text + "]"
	}
	exit := valueOr(evt, "exit_code", "?")
	out := asStr(evt["output"])
	signal := ""
	if exit == "-15" || exit == "143" {
		signal = ", SIGTERM"
	}
	if attributionLine != "" && utf8Len(out) > 600 {
		out = "...(output trimmed — subagent-owned process; see the delegation's live transcript for full output)\n" + tailChars(out, 600)
	} else if cut, ok := asIntValue(evt["output_cut"]); ok && cut != 0 {
		out = fmt.Sprintf("...(first %d characters cut — process(action=\"log\", session_id=\"%s\") has the full output)\n%s", cut, sid, out)
	}
	return fmt.Sprintf("[IMPORTANT: Background process %s %s (exit code %s%s).\n%sCommand: %s\nOutput:\n%s]",
		sid, completionStatus(evt), exit, signal, attribution, cmd, out)
}
