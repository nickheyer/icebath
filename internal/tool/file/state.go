package file

import (
	"os"
	"strings"
	"sync"
	"time"
)

const (
	stateGuardEnv       = "ICEBATH_DISABLE_FILE_STATE_GUARD"
	maxPathsPerAgent    = 4096
	maxGlobalWriters    = 4096
	clockLayout         = "15:04:05"
	staleReadFirst      = "Read the file before writing to avoid overwriting the sibling's changes."
	staleRereadRequired = "Re-read the file before writing."
)

type ordered[K comparable, V any] struct {
	items map[K]V
	order []K
}

func newOrdered[K comparable, V any]() *ordered[K, V] {
	return &ordered[K, V]{items: make(map[K]V)}
}

func (o *ordered[K, V]) get(k K) (V, bool) {
	v, ok := o.items[k]
	return v, ok
}

func (o *ordered[K, V]) set(k K, v V) {
	if _, present := o.items[k]; !present {
		o.order = append(o.order, k)
	}
	o.items[k] = v
}

func (o *ordered[K, V]) delete(k K) {
	if _, present := o.items[k]; !present {
		return
	}
	delete(o.items, k)
	for i, existing := range o.order {
		if existing == k {
			o.order = append(o.order[:i], o.order[i+1:]...)
			break
		}
	}
}

func (o *ordered[K, V]) evict(limit int) {
	for len(o.order) > limit && len(o.order) > 0 {
		delete(o.items, o.order[0])
		o.order = o.order[1:]
	}
}

func (o *ordered[K, V]) len() int {
	return len(o.order)
}

func (o *ordered[K, V]) clear() {
	o.items = make(map[K]V)
	o.order = nil
}

func (o *ordered[K, V]) keys() []K {
	return append([]K(nil), o.order...)
}

func (o *ordered[K, V]) clone() *ordered[K, V] {
	out := newOrdered[K, V]()
	for _, k := range o.order {
		out.set(k, o.items[k])
	}
	return out
}

func GuardDisabled() bool {
	return strings.TrimSpace(os.Getenv(stateGuardEnv)) == "1"
}

func modTime(resolved string) (time.Time, bool) {
	fi, err := os.Stat(resolved)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

type readStamp struct {
	modified time.Time
	readAt   time.Time
	partial  bool
}

type writerClaim struct {
	taskID string
	at     time.Time
}

type pathLock struct {
	mu    sync.Mutex
	users int
}

type State struct {
	meta       sync.Mutex
	pathLocks  map[string]*pathLock
	mu         sync.Mutex
	reads      map[string]*ordered[string, readStamp]
	lastWriter *ordered[string, writerClaim]
}

type TaskWrites struct {
	TaskID string
	Paths  []string
}

func NewState() *State {
	return &State{
		pathLocks:  make(map[string]*pathLock),
		reads:      make(map[string]*ordered[string, readStamp]),
		lastWriter: newOrdered[string, writerClaim](),
	}
}

func (s *State) LockPath(resolved string) func() {
	s.meta.Lock()
	lock, ok := s.pathLocks[resolved]
	if !ok {
		lock = &pathLock{}
		s.pathLocks[resolved] = lock
	}
	lock.users++
	s.meta.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		s.meta.Lock()
		lock.users--
		if lock.users == 0 {
			delete(s.pathLocks, resolved)
		}
		s.meta.Unlock()
	}
}

func (s *State) stamp(taskID, resolved string, modified, now time.Time, partial bool) {
	agentReads, ok := s.reads[taskID]
	if !ok {
		agentReads = newOrdered[string, readStamp]()
		s.reads[taskID] = agentReads
	}
	agentReads.set(resolved, readStamp{modified: modified, readAt: now, partial: partial})
	agentReads.evict(maxPathsPerAgent)
}

func (s *State) RecordRead(taskID, resolved string, partial bool) {
	if GuardDisabled() {
		return
	}
	modified, ok := modTime(resolved)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stamp(taskID, resolved, modified, time.Now(), partial)
}

func (s *State) NoteWrite(taskID, resolved string) {
	if GuardDisabled() {
		return
	}
	modified, ok := modTime(resolved)
	if !ok {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastWriter.set(resolved, writerClaim{taskID: taskID, at: now})
	s.lastWriter.evict(maxGlobalWriters)
	s.stamp(taskID, resolved, modified, now, false)
}

func (s *State) CheckStale(taskID, resolved string) string {
	if GuardDisabled() {
		return ""
	}
	s.mu.Lock()
	var stamp readStamp
	haveStamp := false
	if agentReads, ok := s.reads[taskID]; ok {
		stamp, haveStamp = agentReads.get(resolved)
	}
	writer, haveWriter := s.lastWriter.get(resolved)
	s.mu.Unlock()

	if !haveStamp && !haveWriter {
		return ""
	}
	current, ok := modTime(resolved)
	if !ok {
		return ""
	}
	if haveWriter && writer.taskID != taskID {
		if !haveStamp {
			return resolved + " was modified by sibling subagent " +
				quote(writer.taskID) + " but this agent never read it. " + staleReadFirst
		}
		if writer.at.After(stamp.readAt) {
			return resolved + " was modified by sibling subagent " +
				quote(writer.taskID) + " at " + writer.at.Local().Format(clockLayout) + " — after " +
				"this agent's last read at " + stamp.readAt.Local().Format(clockLayout) + ". " + staleRereadRequired
		}
	}
	if haveStamp {
		if !current.Equal(stamp.modified) {
			return resolved + " was modified since you last read it " +
				"on disk (external edit or unrecorded writer). " + staleRereadRequired
		}
		if stamp.partial {
			return resolved + " was last read with offset/limit pagination " +
				"(partial view). Read the remaining pages, or use patch, " +
				"before overwriting it."
		}
		return ""
	}
	return resolved + " was not read by this agent. " +
		"Read the file first so you can write an informed edit."
}

func (s *State) WritesSince(excludeTaskID string, since time.Time, paths []string) []TaskWrites {
	if GuardDisabled() {
		return nil
	}
	wanted := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		wanted[p] = struct{}{}
	}
	var out []TaskWrites
	index := make(map[string]int)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.lastWriter.order {
		claim := s.lastWriter.items[p]
		if claim.taskID == excludeTaskID || claim.at.Before(since) {
			continue
		}
		if _, ok := wanted[p]; !ok {
			continue
		}
		i, seen := index[claim.taskID]
		if !seen {
			i = len(out)
			index[claim.taskID] = i
			out = append(out, TaskWrites{TaskID: claim.taskID})
		}
		out[i].Paths = append(out[i].Paths, p)
	}
	return out
}

func (s *State) KnownReads(taskID string) []string {
	if GuardDisabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	agentReads, ok := s.reads[taskID]
	if !ok {
		return nil
	}
	return agentReads.keys()
}

func (s *State) ForgetTask(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reads, taskID)
	for _, p := range s.lastWriter.keys() {
		if claim, _ := s.lastWriter.get(p); claim.taskID == taskID {
			s.lastWriter.delete(p)
		}
	}
}
