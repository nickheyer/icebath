package tools

import (
	"os"
	"sort"
	"sync"
	"time"
)

// Read stamp of one agent on one path with the mtime seen and whether the view was windowed
type readStamp struct {
	mtime   float64
	readTs  float64
	partial bool
	order   uint64
}

type lastWriter struct {
	taskID string
	ts     float64
	order  uint64
}

const maxPathsPerAgent = 4096
const maxGlobalWriters = 4096

// Reports whether the user switched the read before write guard off
func fileStateGuardDisabled() bool {
	return os.Getenv("ICEBATH_DISABLE_FILE_STATE_GUARD") == "1"
}

// Modification time in seconds or false when the path cannot be stated
func mtimeOf(resolved string) (float64, bool) {
	fi, err := os.Stat(resolved)
	if err != nil {
		return 0, false
	}
	return float64(fi.ModTime().UnixNano()) / 1e9, true
}

func fmtClock(ts float64) string {
	return time.Unix(int64(ts), 0).Format("15:04:05")
}

type pathLock struct {
	mu    sync.Mutex
	users int
}

// Process wide coordinator for cross agent file edits
type fileStateRegistry struct {
	meta      sync.Mutex
	state     sync.Mutex
	reads     map[string]map[string]*readStamp
	writers   map[string]*lastWriter
	pathLocks map[string]*pathLock
	counter   uint64
}

var fileState = &fileStateRegistry{
	reads:     map[string]map[string]*readStamp{},
	writers:   map[string]*lastWriter{},
	pathLocks: map[string]*pathLock{},
}

// Acquires the per path lock and returns its release function
func (r *fileStateRegistry) lockPath(resolved string) func() {
	r.meta.Lock()
	pl := r.pathLocks[resolved]
	if pl == nil {
		pl = &pathLock{}
		r.pathLocks[resolved] = pl
	}
	pl.users++
	r.meta.Unlock()
	pl.mu.Lock()
	return func() {
		pl.mu.Unlock()
		r.meta.Lock()
		pl.users--
		if pl.users <= 0 {
			delete(r.pathLocks, resolved)
		}
		r.meta.Unlock()
	}
}

// Evicts the oldest stamps once an agent tracks more than the cap, caller holds state
func (r *fileStateRegistry) stamp(taskID, resolved string, mtime, now float64, partial bool) {
	agent := r.reads[taskID]
	if agent == nil {
		agent = map[string]*readStamp{}
		r.reads[taskID] = agent
	}
	r.counter++
	agent[resolved] = &readStamp{mtime: mtime, readTs: now, partial: partial, order: r.counter}
	for len(agent) > maxPathsPerAgent {
		oldest, oldestOrder := "", ^uint64(0)
		for p, s := range agent {
			if s.order < oldestOrder {
				oldest, oldestOrder = p, s.order
			}
		}
		delete(agent, oldest)
	}
}

// Records a read of the current content, partial for a windowed view
func (r *fileStateRegistry) recordRead(taskID, resolved string, partial bool) {
	if fileStateGuardDisabled() {
		return
	}
	mtime, ok := mtimeOf(resolved)
	if !ok {
		return
	}
	r.state.Lock()
	defer r.state.Unlock()
	r.stamp(taskID, resolved, mtime, nowSeconds(), partial)
}

// Records a successful write as the global last writer and an implicit full read
func (r *fileStateRegistry) noteWrite(taskID, resolved string) {
	if fileStateGuardDisabled() {
		return
	}
	mtime, ok := mtimeOf(resolved)
	if !ok {
		return
	}
	now := nowSeconds()
	r.state.Lock()
	defer r.state.Unlock()
	r.counter++
	r.writers[resolved] = &lastWriter{taskID: taskID, ts: now, order: r.counter}
	for len(r.writers) > maxGlobalWriters {
		oldest, oldestOrder := "", ^uint64(0)
		for p, w := range r.writers {
			if w.order < oldestOrder {
				oldest, oldestOrder = p, w.order
			}
		}
		delete(r.writers, oldest)
	}
	r.stamp(taskID, resolved, mtime, now, false)
}

// Warning when a write by this agent would clobber sibling or external changes, else empty
func (r *fileStateRegistry) checkStale(taskID, resolved string) string {
	if fileStateGuardDisabled() {
		return ""
	}
	r.state.Lock()
	var stamp *readStamp
	if agent := r.reads[taskID]; agent != nil {
		if s := agent[resolved]; s != nil {
			copyStamp := *s
			stamp = &copyStamp
		}
	}
	var writer *lastWriter
	if w := r.writers[resolved]; w != nil {
		copyWriter := *w
		writer = &copyWriter
	}
	r.state.Unlock()
	if stamp == nil && writer == nil {
		return ""
	}
	current, ok := mtimeOf(resolved)
	if !ok {
		return ""
	}
	if writer != nil && writer.taskID != taskID {
		if stamp == nil {
			return resolved + " was modified by sibling subagent '" + writer.taskID +
				"' but this agent never read it. Read the file before writing to avoid overwriting the sibling's changes."
		}
		if writer.ts > stamp.readTs {
			return resolved + " was modified by sibling subagent '" + writer.taskID + "' at " + fmtClock(writer.ts) +
				" — after this agent's last read at " + fmtClock(stamp.readTs) + ". Re-read the file before writing."
		}
	}
	if stamp != nil {
		if current != stamp.mtime {
			return resolved + " was modified since you last read it on disk (external edit or unrecorded writer). Re-read the file before writing."
		}
		if stamp.partial {
			return resolved + " was last read with offset/limit pagination (partial view). Read the remaining pages, or use patch, before overwriting it."
		}
		return ""
	}
	return resolved + " was not read by this agent. Read the file first so you can write an informed edit."
}

// Paths written after a timestamp by agents other than the excluded one, grouped by writer
func (r *fileStateRegistry) writesSince(excludeTaskID string, sinceTs float64, paths []string) map[string][]string {
	out := map[string][]string{}
	if fileStateGuardDisabled() {
		return out
	}
	want := setOf(paths...)
	r.state.Lock()
	defer r.state.Unlock()
	for p, w := range r.writers {
		if w.taskID != excludeTaskID && w.ts >= sinceTs && want[p] {
			out[w.taskID] = append(out[w.taskID], p)
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// Resolved paths an agent has read
func (r *fileStateRegistry) knownReads(taskID string) []string {
	if fileStateGuardDisabled() {
		return nil
	}
	r.state.Lock()
	defer r.state.Unlock()
	out := make([]string, 0, len(r.reads[taskID]))
	for p := range r.reads[taskID] {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Releases read stamps and writer claims owned by a finished task
func (r *fileStateRegistry) forgetTask(taskID string) {
	r.state.Lock()
	defer r.state.Unlock()
	delete(r.reads, taskID)
	for p, w := range r.writers {
		if w.taskID == taskID {
			delete(r.writers, p)
		}
	}
}

// Resets every stamp, writer and lock
func (r *fileStateRegistry) clear() {
	r.state.Lock()
	r.reads = map[string]map[string]*readStamp{}
	r.writers = map[string]*lastWriter{}
	r.state.Unlock()
	r.meta.Lock()
	r.pathLocks = map[string]*pathLock{}
	r.meta.Unlock()
}

func nowSeconds() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// WritesSince reports paths other agents wrote after a timestamp, keyed by writer task id
func WritesSince(excludeTaskID string, sinceTs float64, paths []string) map[string][]string {
	return fileState.writesSince(excludeTaskID, sinceTs, paths)
}

// KnownReads lists the resolved paths a task has read through the file tools
func KnownReads(taskID string) []string {
	return fileState.knownReads(taskID)
}
