package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/tool"
)

const (
	patchFailurePathsCap  = 64
	readHistoryCap        = 500
	dedupCap              = 1000
	readTimestampsCap     = 1000
	fullWriteBaselinesCap = 1000
	notFoundCap           = 500
	notFoundTTL           = 60 * time.Second
	readCoverageRangesCap = 256
	defaultTaskID         = "default"
)

type ReadKey struct {
	Path   string
	Offset int
	Limit  int
}

type notFoundKey struct {
	op, resolved string
}

type notFoundEntry struct {
	at   time.Time
	text string
}

type lineRange struct {
	start, end int
}

type coverage struct {
	version  Version
	ranges   []lineRange
	redacted bool
}

type TaskReads struct {
	lastKey      string
	consecutive  int
	history      *ordered[ReadKey, struct{}]
	dedup        *ordered[ReadKey, Version]
	dedupHits    *ordered[ReadKey, int]
	served       *ordered[ReadKey, struct{}]
	readTimes    *ordered[string, time.Time]
	readCoverage *ordered[string, *coverage]
	baselines    *ordered[string, Version]
	blindPatches *ordered[string, string]
	notFound     *ordered[notFoundKey, notFoundEntry]
}

func newTaskReads() *TaskReads {
	return &TaskReads{
		history:      newOrdered[ReadKey, struct{}](),
		dedup:        newOrdered[ReadKey, Version](),
		dedupHits:    newOrdered[ReadKey, int](),
		served:       newOrdered[ReadKey, struct{}](),
		readTimes:    newOrdered[string, time.Time](),
		readCoverage: newOrdered[string, *coverage](),
		baselines:    newOrdered[string, Version](),
		blindPatches: newOrdered[string, string](),
		notFound:     newOrdered[notFoundKey, notFoundEntry](),
	}
}

func (t *TaskReads) capAll() {
	t.history.evict(readHistoryCap)
	t.dedup.evict(dedupCap)
	t.dedupHits.evict(dedupCap)
	t.served.evict(dedupCap)
	t.readTimes.evict(readTimestampsCap)
	t.readCoverage.evict(readTimestampsCap)
	t.baselines.evict(fullWriteBaselinesCap)
	t.blindPatches.evict(fullWriteBaselinesCap)
	t.notFound.evict(notFoundCap)
}

func (t *TaskReads) Bump(key string) int {
	if t.lastKey == key && t.consecutive > 0 {
		t.consecutive++
	} else {
		t.lastKey, t.consecutive = key, 1
	}
	return t.consecutive
}

func (t *TaskReads) NoteHistory(k ReadKey) {
	t.history.set(k, struct{}{})
}

func (t *TaskReads) Dedup(k ReadKey) (Version, bool) {
	return t.dedup.get(k)
}

func (t *TaskReads) SetDedup(k ReadKey, v Version) {
	t.dedup.set(k, v)
}

func (t *TaskReads) DropDedup(k ReadKey) {
	t.dedup.delete(k)
}

func (t *TaskReads) DedupHit(k ReadKey) int {
	hits, _ := t.dedupHits.get(k)
	hits++
	t.dedupHits.set(k, hits)
	return hits
}

func (t *TaskReads) ClearDedupHit(k ReadKey) {
	t.dedupHits.delete(k)
}

func (t *TaskReads) Served(k ReadKey) bool {
	_, ok := t.served.get(k)
	return ok
}

func (t *TaskReads) MarkServed(k ReadKey) {
	t.served.set(k, struct{}{})
}

func (t *TaskReads) UnmarkServed(k ReadKey) {
	t.served.delete(k)
}

func (t *TaskReads) SetReadTime(resolved string, modified time.Time) {
	t.readTimes.set(resolved, modified)
}

func (t *TaskReads) Baseline(resolved string) (Version, bool) {
	return t.baselines.get(resolved)
}

func (t *TaskReads) SetBaseline(resolved string, v Version) {
	t.baselines.set(resolved, v)
}

func (t *TaskReads) DropBlindPatch(resolved string) {
	t.blindPatches.delete(resolved)
}

func (t *TaskReads) NoteCoverage(resolved string, version Version, start, end, totalLines int, redacted bool) (bool, bool) {
	entry, ok := t.readCoverage.get(resolved)
	if !ok || entry.version != version || len(entry.ranges) > readCoverageRangesCap {
		entry = &coverage{version: version}
		t.readCoverage.set(resolved, entry)
	}
	entry.redacted = entry.redacted || redacted
	all := append(append([]lineRange(nil), entry.ranges...), lineRange{start, end})
	sort.Slice(all, func(i, j int) bool {
		if all[i].start != all[j].start {
			return all[i].start < all[j].start
		}
		return all[i].end < all[j].end
	})
	var merged []lineRange
	for _, r := range all {
		if n := len(merged); n > 0 && r.start <= merged[n-1].end+1 {
			merged[n-1].end = max(merged[n-1].end, r.end)
			continue
		}
		merged = append(merged, r)
	}
	entry.ranges = merged
	complete := totalLines > 0 && merged[0].start <= 1 && merged[0].end >= totalLines
	return complete, entry.redacted
}

type EditMarker interface {
	ProjectRoot(ctx context.Context, dir string) bool
	MarkWorkspaceEdited(ctx context.Context, session, cwd string, paths []string) error
}

type Tracker struct {
	mu       sync.Mutex
	tasks    map[string]*TaskReads
	failMu   sync.Mutex
	failures map[string]*ordered[string, int]
}

func NewTracker() *Tracker {
	return &Tracker{tasks: make(map[string]*TaskReads), failures: make(map[string]*ordered[string, int])}
}

func (t *Tracker) task(taskID string) *TaskReads {
	data, ok := t.tasks[taskID]
	if !ok {
		data = newTaskReads()
		t.tasks[taskID] = data
	}
	return data
}

func (t *Tracker) Update(taskID string, f func(*TaskReads)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	data := t.task(taskID)
	f(data)
	data.capAll()
}

func (t *Tracker) Forget(taskID string) {
	t.mu.Lock()
	delete(t.tasks, taskID)
	t.mu.Unlock()
	t.failMu.Lock()
	delete(t.failures, taskID)
	t.failMu.Unlock()
}

func (t *Tracker) RecordPatchFailure(taskID, resolved string) int {
	t.failMu.Lock()
	defer t.failMu.Unlock()
	failures, ok := t.failures[taskID]
	if !ok {
		failures = newOrdered[string, int]()
		t.failures[taskID] = failures
	}
	count, known := failures.get(resolved)
	if !known {
		failures.evict(patchFailurePathsCap - 1)
	}
	count++
	failures.set(resolved, count)
	return count
}

func (t *Tracker) ResetPatchFailures(taskID string, resolved []string) {
	if len(resolved) == 0 {
		return
	}
	t.failMu.Lock()
	defer t.failMu.Unlock()
	failures, ok := t.failures[taskID]
	if !ok {
		return
	}
	for _, p := range resolved {
		failures.delete(p)
	}
}

func (t *Tracker) CheckNotFound(op, resolved, taskID string) (string, bool) {
	key := notFoundKey{op, resolved}
	t.mu.Lock()
	data, ok := t.tasks[taskID]
	var entry notFoundEntry
	found := false
	if ok {
		entry, found = data.notFound.get(key)
	}
	if !found {
		t.mu.Unlock()
		return "", false
	}
	if time.Since(entry.at) > notFoundTTL {
		data.notFound.delete(key)
		t.mu.Unlock()
		return "", false
	}
	t.mu.Unlock()
	if _, err := os.Stat(resolved); err == nil {
		t.mu.Lock()
		if data, ok := t.tasks[taskID]; ok {
			data.notFound.delete(key)
		}
		t.mu.Unlock()
		return "", false
	}
	return entry.text, true
}

func (t *Tracker) RecordNotFound(op, resolved, taskID, text string) {
	t.Update(taskID, func(data *TaskReads) {
		data.notFound.set(notFoundKey{op, resolved}, notFoundEntry{at: time.Now(), text: text})
	})
}

func (t *Tracker) ResetDedup(taskID string) {
	type candidate struct {
		data      *TaskReads
		baselines *ordered[string, Version]
	}
	t.mu.Lock()
	var targets []*TaskReads
	if taskID != "" {
		if data, ok := t.tasks[taskID]; ok {
			targets = append(targets, data)
		}
	} else {
		for _, data := range t.tasks {
			targets = append(targets, data)
		}
	}
	candidates := make([]candidate, 0, len(targets))
	for _, data := range targets {
		data.dedupHits.clear()
		data.served.clear()
		candidates = append(candidates, candidate{data, data.baselines.clone()})
	}
	t.mu.Unlock()
	for _, c := range candidates {
		var changed []string
		for _, p := range c.baselines.order {
			recorded := c.baselines.items[p]
			recorded.Digest = [32]byte{}
			if current, ok := fileMetadata(p); !ok || current != recorded {
				changed = append(changed, p)
			}
		}
		if len(changed) == 0 {
			continue
		}
		t.mu.Lock()
		for _, p := range changed {
			if current, ok := c.data.baselines.get(p); ok && current == c.baselines.items[p] {
				c.data.baselines.delete(p)
			}
		}
		t.mu.Unlock()
	}
}

func (t *Tracker) notifyOtherTool(taskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	data, ok := t.tasks[taskID]
	if !ok {
		return
	}
	data.lastKey, data.consecutive = "", 0
	data.dedupHits.clear()
	data.notFound.clear()
}

func (t *Tracker) Called(c *tool.Call) {
	if c == nil || c.Name == "read_file" || c.Name == "search_files" {
		return
	}
	taskID := c.TaskID
	if taskID == "" {
		taskID = defaultTaskID
	}
	t.notifyOtherTool(taskID)
}

func (t *Tracker) InvalidateDedup(resolved, taskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	data, ok := t.tasks[taskID]
	if !ok {
		return
	}
	for _, k := range data.dedup.keys() {
		if k.Path == resolved {
			data.dedup.delete(k)
		}
	}
	data.notFound.delete(notFoundKey{"read", resolved})
	data.notFound.delete(notFoundKey{"search", resolved})
}

func (t *Tracker) UpdateReadTimestamp(resolved, taskID string) {
	t.InvalidateDedup(resolved, taskID)
	modified, ok := modTime(resolved)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if data, ok := t.tasks[taskID]; ok {
		data.readTimes.set(resolved, modified)
		data.capAll()
	}
}

func metadataOf(resolved string, fi os.FileInfo) (Version, bool) {
	dev, ino, changeNs, err := fileStamp(resolved, fi)
	if err != nil {
		return Version{}, false
	}
	return Version{Device: dev, Inode: ino, Size: fi.Size(), ModTimeNs: fi.ModTime().UnixNano(), ChangeTimeNs: changeNs}, true
}

func fileMetadata(resolved string) (Version, bool) {
	fi, err := os.Stat(resolved)
	if err != nil {
		return Version{}, false
	}
	return metadataOf(resolved, fi)
}

func FileVersion(resolved string) (Version, bool) {
	fi, err := os.Stat(resolved)
	if err != nil || !fi.Mode().IsRegular() {
		return Version{}, false
	}
	f, err := openRegular(resolved)
	if err != nil {
		return Version{}, false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return Version{}, false
	}
	before, ok := metadataOf(resolved, opened)
	if !ok {
		return Version{}, false
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		return Version{}, false
	}
	after, ok := fileMetadata(resolved)
	if !ok || after != before {
		return Version{}, false
	}
	copy(before.Digest[:], digest.Sum(nil))
	return before, true
}

func (v Version) SHA256() string {
	return hex.EncodeToString(v.Digest[:])
}

func (t *Tracker) MarkFullWriteBaseline(resolved, taskID, expectedSHA256 string) {
	version, ok := FileVersion(resolved)
	if !ok || (expectedSHA256 != "" && version.SHA256() != expectedSHA256) {
		return
	}
	t.Update(taskID, func(data *TaskReads) {
		data.baselines.set(resolved, version)
		data.blindPatches.delete(resolved)
	})
}

func (t *Tracker) HasFullWriteBaseline(resolved, taskID string) bool {
	t.mu.Lock()
	var baseline Version
	found := false
	if data, ok := t.tasks[taskID]; ok {
		baseline, found = data.baselines.get(resolved)
	}
	t.mu.Unlock()
	if !found {
		return false
	}
	current, ok := FileVersion(resolved)
	return ok && current == baseline
}

func (t *Tracker) KnownFullContentSHA256(resolvedPaths []string, taskID string) map[string]string {
	t.mu.Lock()
	baselines := newOrdered[string, Version]()
	if data, ok := t.tasks[taskID]; ok {
		baselines = data.baselines.clone()
	}
	t.mu.Unlock()
	known := make(map[string]string)
	for _, resolved := range resolvedPaths {
		if resolved == "" {
			continue
		}
		if _, done := known[resolved]; done {
			continue
		}
		baseline, ok := baselines.get(resolved)
		if !ok {
			continue
		}
		if current, ok := FileVersion(resolved); ok && current == baseline {
			known[resolved] = baseline.SHA256()
		}
	}
	return known
}

func (t *Tracker) CarryFullWriteBaselines(taskID string, known map[string]string, writes []PatchWrite, pathToResolved map[string]string) {
	lookup := make(map[string]string)
	for raw, resolved := range pathToResolved {
		if resolved != "" {
			lookup[raw] = resolved
		}
	}
	for _, resolved := range pathToResolved {
		if resolved != "" {
			lookup[resolved] = resolved
		}
	}
	type vouch struct {
		sha     string
		vouched bool
	}
	state := make(map[string]vouch, len(known))
	for resolved, sha := range known {
		state[resolved] = vouch{sha: sha, vouched: true}
	}
	written := newOrdered[string, string]()
	for _, w := range writes {
		resolved, ok := lookup[w.Path]
		if !ok {
			continue
		}
		prior := state[resolved]
		vouched := (w.ReadKnown && w.ReadSHA256 == "") || (w.ReadKnown && prior.vouched && w.ReadSHA256 == prior.sha)
		state[resolved] = vouch{sha: w.WrittenSHA256, vouched: vouched && w.WrittenSHA256 != ""}
		written.set(resolved, w.WrittenSHA256)
	}
	for _, resolved := range written.order {
		writtenSHA := written.items[resolved]
		if writtenSHA == "" {
			continue
		}
		if state[resolved].vouched {
			t.MarkFullWriteBaseline(resolved, taskID, writtenSHA)
			continue
		}
		t.Update(taskID, func(data *TaskReads) {
			data.blindPatches.set(resolved, writtenSHA)
		})
	}
}

func (t *Tracker) IsOwnBlindPatch(resolved, taskID string) bool {
	t.mu.Lock()
	writtenSHA, found := "", false
	if data, ok := t.tasks[taskID]; ok {
		writtenSHA, found = data.blindPatches.get(resolved)
	}
	t.mu.Unlock()
	if !found {
		return false
	}
	version, ok := FileVersion(resolved)
	return ok && version.SHA256() == writtenSHA
}

func (t *Tracker) ReadModTimeDrifted(resolved, taskID string) bool {
	t.mu.Lock()
	var readAt time.Time
	found := false
	if data, ok := t.tasks[taskID]; ok {
		readAt, found = data.readTimes.get(resolved)
	}
	t.mu.Unlock()
	if !found {
		return false
	}
	current, ok := modTime(resolved)
	return ok && !current.Equal(readAt)
}

func (t *Tracker) StalenessWarning(displayPath, resolved, taskID string) string {
	if !t.ReadModTimeDrifted(resolved, taskID) {
		return ""
	}
	return "Warning: " + displayPath + " was modified since you last read it " +
		"(external edit or concurrent agent). The content you read may be " +
		"stale. Consider re-reading the file to verify before writing."
}

func MarkVerificationStale(ctx context.Context, marker EditMarker, taskID string, resolvedPaths []string, sessionID, workspaceRoot string) error {
	if marker == nil {
		return &tool.UnfilledError{Name: "file.EditMarker"}
	}
	var paths []string
	for _, p := range resolvedPaths {
		if p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	cwd := ""
	for _, p := range paths {
		if parent := filepath.Dir(p); marker.ProjectRoot(ctx, parent) {
			cwd = parent
			break
		}
	}
	if cwd == "" {
		cwd = workspaceRoot
	}
	if cwd == "" {
		cwd = filepath.Dir(paths[0])
	}
	if sessionID == "" {
		sessionID = taskID
	}
	return marker.MarkWorkspaceEdited(ctx, sessionID, cwd, paths)
}
