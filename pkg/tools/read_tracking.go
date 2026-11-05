package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	readHistoryCap        = 500
	dedupCap              = 1000
	readTimestampsCap     = 1000
	fullWriteBaselinesCap = 1000
	notFoundCap           = 500
	notFoundTTL           = 60 * time.Second
	patchFailurePathsCap  = 64
	readCoverageRangesCap = 256
)

// Insertion ordered map whose oldest entries are evicted first
type ordered[K comparable, V any] struct {
	m   map[K]*orderedEntry[V]
	seq uint64
}

type orderedEntry[V any] struct {
	v   V
	seq uint64
}

func newOrdered[K comparable, V any]() *ordered[K, V] {
	return &ordered[K, V]{m: map[K]*orderedEntry[V]{}}
}

func (o *ordered[K, V]) get(k K) (V, bool) {
	if e, ok := o.m[k]; ok {
		return e.v, true
	}
	var zero V
	return zero, false
}

func (o *ordered[K, V]) has(k K) bool { _, ok := o.m[k]; return ok }

// Stores a value keeping the original insertion position of an existing key
func (o *ordered[K, V]) set(k K, v V) {
	if e, ok := o.m[k]; ok {
		e.v = v
		return
	}
	o.seq++
	o.m[k] = &orderedEntry[V]{v: v, seq: o.seq}
}

func (o *ordered[K, V]) del(k K) { delete(o.m, k) }

func (o *ordered[K, V]) size() int { return len(o.m) }

func (o *ordered[K, V]) clear() { o.m = map[K]*orderedEntry[V]{} }

// Evicts the oldest entries until the map is within the cap
func (o *ordered[K, V]) cap(limit int) {
	for len(o.m) > limit {
		var oldest K
		oldestSeq := ^uint64(0)
		for k, e := range o.m {
			if e.seq < oldestSeq {
				oldest, oldestSeq = k, e.seq
			}
		}
		delete(o.m, oldest)
	}
}

func (o *ordered[K, V]) keys() []K {
	out := make([]K, 0, len(o.m))
	for k := range o.m {
		out = append(out, k)
	}
	return out
}

// Stat identity used to tell whether a file changed between two looks
type fileMeta struct {
	size    int64
	mtimeNs int64
	mode    os.FileMode
}

// Byte snapshot of a regular file, stat identity plus content digest
type fileVersion struct {
	meta   fileMeta
	digest [32]byte
}

func (v fileVersion) shaHex() string { return hex.EncodeToString(v.digest[:]) }

type dedupKey struct {
	path   string
	offset int
	limit  int
}

type notFoundKey struct {
	op   string
	path string
}

type notFoundEntry struct {
	at   time.Time
	json string
}

type coverageEntry struct {
	version  fileVersion
	ranges   [][2]int
	redacted bool
}

// Per task bookkeeping behind read_file, search_files, write_file and patch
type taskTracker struct {
	lastKey              string
	consecutive          int
	readHistory          *ordered[dedupKey, struct{}]
	dedup                *ordered[dedupKey, fileMeta]
	dedupGenerationReads *ordered[dedupKey, struct{}]
	dedupHits            *ordered[dedupKey, int]
	readTimestamps       *ordered[string, float64]
	readCoverage         *ordered[string, *coverageEntry]
	fullWriteBaselines   *ordered[string, fileVersion]
	blindPatches         *ordered[string, string]
	notFound             *ordered[notFoundKey, notFoundEntry]
}

var readTrackerLock sync.Mutex
var readTracker = map[string]*taskTracker{}

var patchFailureLock sync.Mutex
var patchFailureTracker = map[string]*ordered[string, int]{}

// Get or create the tracker entry for a task, lock must be held
func taskData(taskID string) *taskTracker {
	td := readTracker[taskID]
	if td == nil {
		td = &taskTracker{
			readHistory:          newOrdered[dedupKey, struct{}](),
			dedup:                newOrdered[dedupKey, fileMeta](),
			dedupGenerationReads: newOrdered[dedupKey, struct{}](),
			dedupHits:            newOrdered[dedupKey, int](),
			readTimestamps:       newOrdered[string, float64](),
			readCoverage:         newOrdered[string, *coverageEntry](),
			fullWriteBaselines:   newOrdered[string, fileVersion](),
			blindPatches:         newOrdered[string, string](),
			notFound:             newOrdered[notFoundKey, notFoundEntry](),
		}
		readTracker[taskID] = td
	}
	return td
}

// Increments and returns the consecutive failure count for a path
func recordPatchFailure(taskID, resolved string) int {
	patchFailureLock.Lock()
	defer patchFailureLock.Unlock()
	failures := patchFailureTracker[taskID]
	if failures == nil {
		failures = newOrdered[string, int]()
		patchFailureTracker[taskID] = failures
	}
	if !failures.has(resolved) {
		failures.cap(patchFailurePathsCap - 1)
	}
	n, _ := failures.get(resolved)
	failures.set(resolved, n+1)
	return n + 1
}

// Clears consecutive failure counts for the given paths
func resetPatchFailures(taskID string, resolved []string) {
	if len(resolved) == 0 {
		return
	}
	patchFailureLock.Lock()
	defer patchFailureLock.Unlock()
	if failures := patchFailureTracker[taskID]; failures != nil {
		for _, p := range resolved {
			failures.del(p)
		}
	}
}

// Enforces the size caps on every per task container, lock must be held
func capReadTrackerData(td *taskTracker) {
	td.readHistory.cap(readHistoryCap)
	td.dedup.cap(dedupCap)
	td.dedupHits.cap(dedupCap)
	td.dedupGenerationReads.cap(dedupCap)
	td.readTimestamps.cap(readTimestampsCap)
	td.readCoverage.cap(readTimestampsCap)
	td.fullWriteBaselines.cap(fullWriteBaselinesCap)
	td.blindPatches.cap(fullWriteBaselinesCap)
	td.notFound.cap(notFoundCap)
}

// Drops the negative cache entry for an op and path, lock must be held
func popNotFound(op, resolved, taskID string) {
	if td := readTracker[taskID]; td != nil {
		td.notFound.del(notFoundKey{op, resolved})
	}
}

// Cached not found JSON for an op and path while still fresh and still absent
func checkNotFoundCache(op, resolved, taskID string) (string, bool) {
	readTrackerLock.Lock()
	td := readTracker[taskID]
	var entry notFoundEntry
	ok := false
	if td != nil {
		entry, ok = td.notFound.get(notFoundKey{op, resolved})
	}
	if ok && time.Since(entry.at) > notFoundTTL {
		popNotFound(op, resolved, taskID)
		ok = false
	}
	readTrackerLock.Unlock()
	if !ok {
		return "", false
	}
	if _, err := os.Stat(resolved); err == nil {
		readTrackerLock.Lock()
		popNotFound(op, resolved, taskID)
		readTrackerLock.Unlock()
		return "", false
	}
	return entry.json, true
}

// Caches a not found error so the next call for the path skips I/O
func recordNotFound(op, resolved, taskID, errorJSON string) {
	readTrackerLock.Lock()
	defer readTrackerLock.Unlock()
	td := taskData(taskID)
	td.notFound.set(notFoundKey{op, resolved}, notFoundEntry{at: time.Now(), json: errorJSON})
	capReadTrackerData(td)
}

// Updates the consecutive counter for a key and returns the new count, lock must be held
func bumpConsecutive(td *taskTracker, key string) int {
	if td.lastKey == key {
		td.consecutive++
	} else {
		td.lastKey = key
		td.consecutive = 1
	}
	return td.consecutive
}

// ResetFileDedup advances the read dedup generation after context compression for one task or all
func ResetFileDedup(taskID string) {
	readTrackerLock.Lock()
	var targets []*taskTracker
	if taskID != "" {
		if td := readTracker[taskID]; td != nil {
			targets = append(targets, td)
		}
	} else {
		for _, td := range readTracker {
			targets = append(targets, td)
		}
	}
	type candidate struct {
		td        *taskTracker
		baselines map[string]fileVersion
	}
	var candidates []candidate
	for _, td := range targets {
		td.dedupHits.clear()
		td.dedupGenerationReads.clear()
		snap := map[string]fileVersion{}
		for _, p := range td.fullWriteBaselines.keys() {
			v, _ := td.fullWriteBaselines.get(p)
			snap[p] = v
		}
		candidates = append(candidates, candidate{td, snap})
	}
	readTrackerLock.Unlock()
	for _, c := range candidates {
		var changed []string
		for p, v := range c.baselines {
			meta, ok := fileMetadata(p)
			if !ok || meta != v.meta {
				changed = append(changed, p)
			}
		}
		if len(changed) == 0 {
			continue
		}
		readTrackerLock.Lock()
		for _, p := range changed {
			if cur, ok := c.td.fullWriteBaselines.get(p); ok && cur == c.baselines[p] {
				c.td.fullWriteBaselines.del(p)
			}
		}
		readTrackerLock.Unlock()
	}
}

// NotifyOtherToolCall resets the consecutive read counter, stub hits and not found cache of a task
func NotifyOtherToolCall(taskID string) {
	if taskID == "" {
		taskID = "default"
	}
	readTrackerLock.Lock()
	defer readTrackerLock.Unlock()
	if td := readTracker[taskID]; td != nil {
		td.lastKey = ""
		td.consecutive = 0
		td.dedupHits.clear()
		td.notFound.clear()
	}
}

// ClearFileOpsCache drops every per task file tool state, all tasks when the id is empty
func ClearFileOpsCache(taskID string) {
	forgetFileOps(taskID)
	readTrackerLock.Lock()
	if taskID != "" {
		delete(readTracker, taskID)
	} else {
		readTracker = map[string]*taskTracker{}
	}
	readTrackerLock.Unlock()
	patchFailureLock.Lock()
	if taskID != "" {
		delete(patchFailureTracker, taskID)
	} else {
		patchFailureTracker = map[string]*ordered[string, int]{}
	}
	patchFailureLock.Unlock()
	if taskID != "" {
		fileState.forgetTask(taskID)
	} else {
		fileState.clear()
	}
}

// Evicts every dedup and not found entry for a written path so the next read is fresh
func invalidateDedupForPath(resolved, taskID string) {
	readTrackerLock.Lock()
	defer readTrackerLock.Unlock()
	td := readTracker[taskID]
	if td == nil {
		return
	}
	for _, k := range td.dedup.keys() {
		if k.path == resolved {
			td.dedup.del(k)
		}
	}
	popNotFound("read", resolved, taskID)
	popNotFound("search", resolved, taskID)
}

// Refreshes the stored mtime after a write so same task edits never read as stale
func updateReadTimestamp(env *Env, path, taskID string) error {
	resolved, err := resolvePathForTaskID(env, taskID, path)
	if err != nil {
		if isPathResolutionFailure(err) {
			return nil
		}
		return err
	}
	invalidateDedupForPath(resolved, taskID)
	mtime, ok := mtimeOf(resolved)
	if !ok {
		return nil
	}
	readTrackerLock.Lock()
	defer readTrackerLock.Unlock()
	if td := readTracker[taskID]; td != nil {
		td.readTimestamps.set(resolved, mtime)
		capReadTrackerData(td)
	}
	return nil
}

func metaOf(fi os.FileInfo) fileMeta {
	return fileMeta{size: fi.Size(), mtimeNs: fi.ModTime().UnixNano(), mode: fi.Mode()}
}

// Stat identity of a path, false when it cannot be stated
func fileMetadata(resolved string) (fileMeta, bool) {
	fi, err := os.Stat(resolved)
	if err != nil {
		return fileMeta{}, false
	}
	return metaOf(fi), true
}

// Opens a regular file without blocking on a FIFO, nil when missing or not regular
func openRegular(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		if err == nil {
			err = &os.PathError{Op: "open", Path: path, Err: syscall.EINVAL}
		}
		return nil, nil, err
	}
	return f, fi, nil
}

// Content snapshot of a regular file whose stat identity held across the hash
func readFileVersion(resolved string) (fileVersion, bool) {
	f, before, err := openRegular(resolved)
	if err != nil {
		return fileVersion{}, false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fileVersion{}, false
	}
	after, err := os.Stat(resolved)
	if err != nil || metaOf(after) != metaOf(before) {
		return fileVersion{}, false
	}
	var v fileVersion
	v.meta = metaOf(before)
	copy(v.digest[:], h.Sum(nil))
	return v, true
}

// Records that a task saw the whole current content of a path so write_file may replace it
func markFullWriteBaseline(resolved, taskID, expectedSha string) {
	v, ok := readFileVersion(resolved)
	if !ok || (expectedSha != "" && v.shaHex() != expectedSha) {
		return
	}
	readTrackerLock.Lock()
	defer readTrackerLock.Unlock()
	td := taskData(taskID)
	td.fullWriteBaselines.set(resolved, v)
	td.blindPatches.del(resolved)
	capReadTrackerData(td)
}

// Reports whether the task holds a baseline still matching the bytes on disk
func hasFullWriteBaseline(resolved, taskID string) bool {
	readTrackerLock.Lock()
	var baseline fileVersion
	ok := false
	if td := readTracker[taskID]; td != nil {
		baseline, ok = td.fullWriteBaselines.get(resolved)
	}
	readTrackerLock.Unlock()
	if !ok {
		return false
	}
	v, ok := readFileVersion(resolved)
	return ok && v == baseline
}

// Sha256 hex per resolved path whose recorded baseline still matches disk
func knownFullContentSha256(resolvedPaths []string, taskID string) map[string]string {
	readTrackerLock.Lock()
	baselines := map[string]fileVersion{}
	if td := readTracker[taskID]; td != nil {
		for _, p := range td.fullWriteBaselines.keys() {
			v, _ := td.fullWriteBaselines.get(p)
			baselines[p] = v
		}
	}
	readTrackerLock.Unlock()
	known := map[string]string{}
	seen := map[string]bool{}
	for _, r := range resolvedPaths {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		baseline, ok := baselines[r]
		if !ok {
			continue
		}
		if v, ok := readFileVersion(r); ok && v == baseline {
			known[r] = baseline.shaHex()
		}
	}
	return known
}

// One file write of a patch with the sha of the bytes read and the bytes written
type patchWrite struct {
	path         string
	readSha      string
	readKnown    bool
	writtenSha   string
	writtenKnown bool
}

// Keeps the whole file baseline across a patch the task could vouch for, else notes a blind patch
func carryFullWriteBaselines(taskID string, known map[string]string, writes []patchWrite, pathToResolved map[string]string) {
	lookup := map[string]string{}
	for raw, r := range pathToResolved {
		if r != "" {
			lookup[raw] = r
			lookup[r] = r
		}
	}
	knownCopy := map[string]*string{}
	for k, v := range known {
		s := v
		knownCopy[k] = &s
	}
	written := map[string]*string{}
	var order []string
	for _, w := range writes {
		resolved, ok := lookup[w.path]
		if !ok {
			continue
		}
		vouched := false
		if w.readKnown {
			if w.readSha == "" {
				vouched = true
			} else if k := knownCopy[resolved]; k != nil && *k == w.readSha {
				vouched = true
			}
		}
		if vouched && w.writtenKnown {
			s := w.writtenSha
			knownCopy[resolved] = &s
		} else {
			knownCopy[resolved] = nil
		}
		if _, seen := written[resolved]; !seen {
			order = append(order, resolved)
		}
		if w.writtenKnown {
			s := w.writtenSha
			written[resolved] = &s
		} else {
			written[resolved] = nil
		}
	}
	for _, resolved := range order {
		writtenSha := written[resolved]
		if writtenSha == nil {
			continue
		}
		if knownCopy[resolved] != nil {
			markFullWriteBaseline(resolved, taskID, *writtenSha)
			continue
		}
		readTrackerLock.Lock()
		td := taskData(taskID)
		td.blindPatches.set(resolved, *writtenSha)
		capReadTrackerData(td)
		readTrackerLock.Unlock()
	}
}

// Reports whether the bytes on disk are exactly what this task patched without a full baseline
func isOwnBlindPatch(resolved, taskID string) bool {
	readTrackerLock.Lock()
	var sha string
	ok := false
	if td := readTracker[taskID]; td != nil {
		sha, ok = td.blindPatches.get(resolved)
	}
	readTrackerLock.Unlock()
	if !ok {
		return false
	}
	v, ok := readFileVersion(resolved)
	return ok && v.shaHex() == sha
}

// Merges a page into the coverage of a version and reports completeness and redaction, lock held
func noteReadCoverage(td *taskTracker, resolved string, version fileVersion, start, end int, totalLines int, redacted bool) (bool, bool) {
	entry, ok := td.readCoverage.get(resolved)
	if !ok || entry.version != version || len(entry.ranges) > readCoverageRangesCap {
		entry = &coverageEntry{version: version}
		td.readCoverage.set(resolved, entry)
	}
	entry.redacted = entry.redacted || redacted
	all := append([][2]int{}, entry.ranges...)
	all = append(all, [2]int{start, end})
	sort.Slice(all, func(i, j int) bool {
		if all[i][0] != all[j][0] {
			return all[i][0] < all[j][0]
		}
		return all[i][1] < all[j][1]
	})
	var merged [][2]int
	for _, r := range all {
		if len(merged) > 0 && r[0] <= merged[len(merged)-1][1]+1 {
			if r[1] > merged[len(merged)-1][1] {
				merged[len(merged)-1][1] = r[1]
			}
		} else {
			merged = append(merged, r)
		}
	}
	entry.ranges = merged
	complete := totalLines > 0 && merged[0][0] <= 1 && merged[0][1] >= totalLines
	return complete, entry.redacted
}

// Reports whether the mtime changed since this task last read the file
func readMtimeDrifted(env *Env, path, taskID string) (bool, error) {
	resolved, err := resolvePathForTaskID(env, taskID, path)
	if err != nil {
		if isPathResolutionFailure(err) {
			return false, nil
		}
		return false, err
	}
	readTrackerLock.Lock()
	var readMtime float64
	ok := false
	if td := readTracker[taskID]; td != nil {
		readMtime, ok = td.readTimestamps.get(resolved)
	}
	readTrackerLock.Unlock()
	if !ok {
		return false, nil
	}
	current, exists := mtimeOf(resolved)
	return exists && current != readMtime, nil
}

// Warning when the file changed since the last read, else empty
func checkFileStaleness(env *Env, path, taskID string) (string, error) {
	drifted, err := readMtimeDrifted(env, path, taskID)
	if err != nil {
		return "", err
	}
	if drifted {
		return "Warning: " + path + " was modified since you last read it (external edit or concurrent agent). " +
			"The content you read may be stale. Consider re-reading the file to verify before writing.", nil
	}
	return "", nil
}

// Tells the host that successful edits made earlier verification evidence stale
func markVerificationStale(env *Env, taskID, sessionID string, resolvedPaths []string) {
	var paths []string
	for _, p := range resolvedPaths {
		if p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return
	}
	var parents []string
	for _, p := range paths {
		parents = append(parents, filepath.Dir(p))
	}
	cwd := ""
	for _, c := range parents {
		if projectFactsFor(c) != nil {
			cwd = c
			break
		}
	}
	if cwd == "" {
		root, err := authoritativeWorkspaceRootFor(env, taskID)
		if err != nil {
			envLog(env).Debug("verification stale marker failed", "error", err)
			return
		}
		cwd = root
	}
	if cwd == "" {
		cwd = parents[0]
	}
	if sessionID == "" {
		sessionID = taskID
	}
	env.Emit(Event{Kind: "workspace_edited", TaskID: taskID, Data: map[string]any{
		"session_id": sessionID, "cwd": cwd, "paths": paths,
	}})
}
