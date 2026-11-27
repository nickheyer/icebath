package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

// Caps of what one session keeps
const (
	dedupCap     = 1000
	stampsCap    = 1000
	baselineCap  = 1000
	missingCap   = 500
	missingTTL   = 60 * time.Second
	failuresCap  = 64
	coverageCap  = 256
	readsCap     = 4096
	writersCap   = 4096
	repeatsBlock = 4
	repeatsWarn  = 3
)

// Map whose oldest entries go first when it is over its cap
type ordered[K comparable, V any] struct {
	m   map[K]*slot[V]
	seq uint64
}

type slot[V any] struct {
	v   V
	seq uint64
}

func newOrdered[K comparable, V any]() *ordered[K, V] {
	return &ordered[K, V]{m: map[K]*slot[V]{}}
}

func (o *ordered[K, V]) get(k K) (V, bool) {
	if s, ok := o.m[k]; ok {
		return s.v, true
	}
	var zero V
	return zero, false
}

func (o *ordered[K, V]) has(k K) bool { _, ok := o.m[k]; return ok }

// Stores the value, an existing key keeping its place
func (o *ordered[K, V]) set(k K, v V) {
	if s, ok := o.m[k]; ok {
		s.v = v
		return
	}
	o.seq++
	o.m[k] = &slot[V]{v, o.seq}
}

func (o *ordered[K, V]) del(k K) { delete(o.m, k) }

func (o *ordered[K, V]) clear() { o.m = map[K]*slot[V]{} }

// Drops the oldest entries until no more than limit are left
func (o *ordered[K, V]) cap(limit int) {
	for len(o.m) > limit {
		var oldest K
		seq := ^uint64(0)
		for k, s := range o.m {
			if s.seq < seq {
				oldest, seq = k, s.seq
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

// Stat identity that tells whether a file changed between two looks
type fileMeta struct {
	size  int64
	mtime int64
	mode  os.FileMode
}

// Bytes of a regular file at one moment: its stat identity and digest
type fileVersion struct {
	meta   fileMeta
	digest [32]byte
}

func (v fileVersion) sha() string { return hex.EncodeToString(v.digest[:]) }

func metaOf(fi os.FileInfo) fileMeta {
	return fileMeta{size: fi.Size(), mtime: fi.ModTime().UnixNano(), mode: fi.Mode()}
}

func metadata(path string) (fileMeta, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileMeta{}, false
	}
	return metaOf(fi), true
}

func mtime(path string) (int64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return fi.ModTime().UnixNano(), true
}

// Opens a regular file without blocking on a FIFO, nil and no error for a missing one
func openRegular(path string) (*os.File, error) {
	flags := os.O_RDONLY
	if !windows {
		flags |= syscall.O_NONBLOCK
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, errNotRegular
	}
	return f, nil
}

// Version of a regular file whose stat identity held while it was hashed
func readVersion(path string) (fileVersion, bool) {
	f, err := openRegular(path)
	if err != nil || f == nil {
		return fileVersion{}, false
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return fileVersion{}, false
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fileVersion{}, false
	}
	after, err := os.Stat(path)
	if err != nil || metaOf(after) != metaOf(before) {
		return fileVersion{}, false
	}
	v := fileVersion{meta: metaOf(before)}
	copy(v.digest[:], h.Sum(nil))
	return v, true
}

type dedupKey struct {
	path          string
	offset, limit int
}

type missingKey struct{ op, path string }

type missingEntry struct {
	at     time.Time
	result string
}

// Line ranges of one version of a file a session has read, and whether any of them was shown redacted
type coverage struct {
	version  fileVersion
	ranges   [][2]int
	redacted bool
}

// When a session last read a path, the mtime it saw, and whether it saw only part of it
type stamp struct {
	mtime   int64
	at      time.Time
	partial bool
}

// Session that last wrote a path and when
type writer struct {
	session string
	at      time.Time
	seq     uint64
}

// What the file tools remember of one session
type track struct {
	cfg     *config.Config
	session string
	// Shape of the last read or search, and how many times in a row it came
	lastKey     string
	consecutive int
	// Reads that may be answered with the unchanged stub: the version each read saw, those served since the context was last compacted,
	// and how many stubs each got
	dedup  *ordered[dedupKey, fileMeta]
	served *ordered[dedupKey, struct{}]
	hits   *ordered[dedupKey, int]
	// Mtime each path had when last read or written
	stamps   *ordered[string, int64]
	coverage *ordered[string, *coverage]
	// Versions the session saw whole, which write_file may replace, and the sha256 a patch left a file at without one
	baselines *ordered[string, fileVersion]
	blind     *ordered[string, string]
	missing   *ordered[missingKey, missingEntry]
	failures  *ordered[string, int]
	reads     *ordered[string, stamp]
}

func newTrack(cfg *config.Config, session string) *track {
	return &track{
		cfg: cfg, session: session,
		dedup: newOrdered[dedupKey, fileMeta](), served: newOrdered[dedupKey, struct{}](), hits: newOrdered[dedupKey, int](),
		stamps: newOrdered[string, int64](), coverage: newOrdered[string, *coverage](), baselines: newOrdered[string, fileVersion](),
		blind: newOrdered[string, string](), missing: newOrdered[missingKey, missingEntry](), failures: newOrdered[string, int](),
		reads: newOrdered[string, stamp](),
	}
}

// Keeps every container within its cap
func (t *track) trim() {
	t.dedup.cap(dedupCap)
	t.hits.cap(dedupCap)
	t.served.cap(dedupCap)
	t.stamps.cap(stampsCap)
	t.coverage.cap(stampsCap)
	t.baselines.cap(baselineCap)
	t.blind.cap(baselineCap)
	t.missing.cap(missingCap)
	t.reads.cap(readsCap)
}

// Counts one more read or search of the same shape in a row
func (t *track) repeat(key string) int {
	if t.lastKey == key {
		t.consecutive++
	} else {
		t.lastKey, t.consecutive = key, 1
	}
	return t.consecutive
}

// What the file tools remember of every session, and the writers and locks of paths they share
type memory struct {
	mu       sync.Mutex
	sessions map[string]*track
	writers  map[string]*writer
	seq      uint64
	lockMu   sync.Mutex
	locks    map[string]*pathLock
}

type pathLock struct {
	mu    sync.Mutex
	users int
}

func newMemory() *memory {
	return &memory{sessions: map[string]*track{}, writers: map[string]*writer{}, locks: map[string]*pathLock{}}
}

func sessionKey(cfg *config.Config, session string) string {
	if session == "" {
		session = "default"
	}
	return cfg.Profile() + "/" + session
}

// Tracker of the session, made when there is none; the lock must be held
func (m *memory) track(cfg *config.Config, session string) *track {
	key := sessionKey(cfg, session)
	t := m.sessions[key]
	if t == nil {
		t = newTrack(cfg, session)
		m.sessions[key] = t
	}
	return t
}

// Forgets what the session read and the writes it claimed
func (m *memory) forget(key string) {
	t := m.sessions[key]
	delete(m.sessions, key)
	if t == nil {
		return
	}
	for path, w := range m.writers {
		if w.session == t.session {
			delete(m.writers, path)
		}
	}
}

// Forgets the sessions whose environment is gone, as an environment goes idle or its session ends; kept says which still have one
func (m *memory) sweep(kept func(cfg *config.Config, session string) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, t := range m.sessions {
		if !kept(t.cfg, t.session) {
			m.forget(key)
		}
	}
}

// Forgets the sessions of a deleted profile
func (m *memory) drop(profile string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, t := range m.sessions {
		if t.cfg.Profile() == profile {
			m.forget(key)
		}
	}
}

// Holds the path's lock until the release it returns runs
func (m *memory) lock(path string) func() {
	m.lockMu.Lock()
	l := m.locks[path]
	if l == nil {
		l = &pathLock{}
		m.locks[path] = l
	}
	l.users++
	m.lockMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		m.lockMu.Lock()
		if l.users--; l.users <= 0 {
			delete(m.locks, path)
		}
		m.lockMu.Unlock()
	}
}

// Holds the locks of every path, in a fixed order so two edits never wait on each other, until the release it returns runs
func (m *memory) lockAll(paths []string) func() {
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	var releases []func()
	for _, p := range sorted {
		if p != "" {
			releases = append(releases, m.lock(p))
		}
	}
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
}

// Error result the session got for an op on a path that was not there, while it is fresh and the path still missing
func (m *memory) cachedMissing(t *track, op, path string) (string, bool) {
	m.mu.Lock()
	entry, ok := t.missing.get(missingKey{op, path})
	if ok && time.Since(entry.at) > missingTTL {
		t.missing.del(missingKey{op, path})
		ok = false
	}
	m.mu.Unlock()
	if !ok {
		return "", false
	}
	if _, err := os.Stat(path); err == nil {
		m.mu.Lock()
		t.missing.del(missingKey{op, path})
		m.mu.Unlock()
		return "", false
	}
	return entry.result, true
}

// Remembers the error result of an op on a missing path, so the next call for it answers without looking
func (m *memory) rememberMissing(t *track, op, path, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t.missing.set(missingKey{op, path}, missingEntry{time.Now(), result})
	t.trim()
}

// Another tool ran: reads and searches in a row start counting again, and the stubs and missing paths are forgotten
func (m *memory) other(t *track) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t.lastKey, t.consecutive = "", 0
	t.hits.clear()
	t.missing.clear()
}

// The session's context was compacted, so the model no longer holds earlier reads: none is answered with the unchanged stub again, and
// the whole file views of files that changed since are dropped
func (m *memory) compacted(t *track) {
	m.mu.Lock()
	t.hits.clear()
	t.served.clear()
	baselines := map[string]fileVersion{}
	for _, path := range t.baselines.keys() {
		baselines[path], _ = t.baselines.get(path)
	}
	m.mu.Unlock()
	for path, version := range baselines {
		if meta, ok := metadata(path); ok && meta == version.meta {
			continue
		}
		m.mu.Lock()
		if current, ok := t.baselines.get(path); ok && current == version {
			t.baselines.del(path)
		}
		m.mu.Unlock()
	}
}

// Drops the stubs and missing entries of a path that was written, so the next read is fresh; the lock must be held
func (t *track) invalidate(path string) {
	for _, k := range t.dedup.keys() {
		if k.path == path {
			t.dedup.del(k)
		}
	}
	t.missing.del(missingKey{"read", path})
	t.missing.del(missingKey{"search", path})
}

// Notes the mtime of a path the session just wrote or failed to, so its own edits never look stale
func (m *memory) stamp(t *track, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t.invalidate(path)
	if at, ok := mtime(path); ok {
		t.stamps.set(path, at)
		t.trim()
	}
}

// Whether the path changed on disk since the session last read or wrote it
func (m *memory) drifted(t *track, path string) bool {
	m.mu.Lock()
	seen, ok := t.stamps.get(path)
	m.mu.Unlock()
	if !ok {
		return false
	}
	current, exists := mtime(path)
	return exists && current != seen
}

// Records that the session holds the whole current content of a path, when the bytes are what it expected
func (m *memory) markBaseline(t *track, path, expected string) {
	v, ok := readVersion(path)
	if !ok || expected != "" && v.sha() != expected {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t.baselines.set(path, v)
	t.blind.del(path)
	t.trim()
}

// Whether the session holds the whole content of the path as it is on disk now
func (m *memory) hasBaseline(t *track, path string) bool {
	m.mu.Lock()
	baseline, ok := t.baselines.get(path)
	m.mu.Unlock()
	if !ok {
		return false
	}
	v, ok := readVersion(path)
	return ok && v == baseline
}

// Sha256 of each path whose whole content the session holds as it is on disk now
func (m *memory) known(t *track, paths []string) map[string]string {
	m.mu.Lock()
	baselines := map[string]fileVersion{}
	for _, path := range t.baselines.keys() {
		baselines[path], _ = t.baselines.get(path)
	}
	m.mu.Unlock()
	out := map[string]string{}
	for _, path := range paths {
		baseline, ok := baselines[path]
		if path == "" || !ok {
			continue
		}
		if _, done := out[path]; done {
			continue
		}
		if v, ok := readVersion(path); ok && v == baseline {
			out[path] = baseline.sha()
		}
	}
	return out
}

// Carries the whole file views across a patch the session could vouch for, and marks a file a patch wrote without one as patched blind
func (m *memory) carry(t *track, known map[string]string, writes []patchWrite, resolved map[string]string) {
	lookup := map[string]string{}
	for raw, path := range resolved {
		if path != "" {
			lookup[raw], lookup[path] = path, path
		}
	}
	vouched := map[string]*string{}
	for path, sha := range known {
		vouched[path] = &sha
	}
	written := map[string]*string{}
	var order []string
	for _, w := range writes {
		path, ok := lookup[w.path]
		if !ok {
			continue
		}
		trusted := w.readKnown && (w.read == "" || vouched[path] != nil && *vouched[path] == w.read)
		if trusted && w.writtenKnown {
			sha := w.written
			vouched[path] = &sha
		} else {
			vouched[path] = nil
		}
		if _, seen := written[path]; !seen {
			order = append(order, path)
		}
		written[path] = nil
		if w.writtenKnown {
			sha := w.written
			written[path] = &sha
		}
	}
	for _, path := range order {
		sha := written[path]
		if sha == nil {
			continue
		}
		if vouched[path] != nil {
			m.markBaseline(t, path, *sha)
			continue
		}
		m.mu.Lock()
		t.blind.set(path, *sha)
		t.trim()
		m.mu.Unlock()
	}
}

// Whether the bytes on disk are exactly what the session's own patch left without its having seen the whole file
func (m *memory) ownBlind(t *track, path string) bool {
	m.mu.Lock()
	sha, ok := t.blind.get(path)
	m.mu.Unlock()
	if !ok {
		return false
	}
	v, ok := readVersion(path)
	return ok && v.sha() == sha
}

// Merges a page of a version into what the session read of it, answering whether it now covers the whole file and whether any page was
// shown redacted; the lock must be held
func (t *track) cover(path string, version fileVersion, start, end, total int, redacted bool) (bool, bool) {
	entry, ok := t.coverage.get(path)
	if !ok || entry.version != version || len(entry.ranges) > coverageCap {
		entry = &coverage{version: version}
		t.coverage.set(path, entry)
	}
	entry.redacted = entry.redacted || redacted
	ranges := append(slices.Clone(entry.ranges), [2]int{start, end})
	slices.SortFunc(ranges, func(a, b [2]int) int {
		if a[0] != b[0] {
			return a[0] - b[0]
		}
		return a[1] - b[1]
	})
	var merged [][2]int
	for _, r := range ranges {
		if n := len(merged); n > 0 && r[0] <= merged[n-1][1]+1 {
			merged[n-1][1] = max(merged[n-1][1], r[1])
			continue
		}
		merged = append(merged, r)
	}
	entry.ranges = merged
	return total > 0 && merged[0][0] <= 1 && merged[0][1] >= total, entry.redacted
}

// Counts one more failed patch of the path in a row
func (m *memory) failed(t *track, path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !t.failures.has(path) {
		t.failures.cap(failuresCap - 1)
	}
	n, _ := t.failures.get(path)
	t.failures.set(path, n+1)
	return n + 1
}

// Clears the failed patch counts of the paths
func (m *memory) succeeded(t *track, paths []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, path := range paths {
		t.failures.del(path)
	}
}

// Records that the session read the current content, partial for a windowed view
func (m *memory) recordRead(t *track, path string, partial bool) {
	at, ok := mtime(path)
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t.reads.set(path, stamp{at, time.Now(), partial})
	t.reads.cap(readsCap)
}

// Records the session as the last writer of the path, and the write as a whole read of it
func (m *memory) noteWrite(t *track, path string) {
	at, ok := mtime(path)
	if !ok {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.writers[path] = &writer{t.session, now, m.seq}
	for len(m.writers) > writersCap {
		oldest, seq := "", ^uint64(0)
		for p, w := range m.writers {
			if w.seq < seq {
				oldest, seq = p, w.seq
			}
		}
		delete(m.writers, oldest)
	}
	t.reads.set(path, stamp{at, now, false})
	t.reads.cap(readsCap)
}

// Paths the session read, sorted
func (m *memory) knownReads(cfg *config.Config, session string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.sessions[sessionKey(cfg, session)]
	if t == nil {
		return nil
	}
	return slices.Sorted(slices.Values(t.reads.keys()))
}

// Paths among those given, every path for nil, whose last writer since the time is a session other than the one left out, by that session,
// sorted
func (m *memory) writesSince(exclude string, since time.Time, paths []string) map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if paths == nil {
		for path := range m.writers {
			paths = append(paths, path)
		}
	}
	out := map[string][]string{}
	for _, path := range paths {
		if w := m.writers[path]; w != nil && w.session != exclude && !w.at.Before(since) && !slices.Contains(out[w.session], path) {
			out[w.session] = append(out[w.session], path)
		}
	}
	for session := range out {
		slices.Sort(out[session])
	}
	return out
}

// Why a write by the session would clobber another session's or an outside change of the path, empty when it would not
func (m *memory) stale(t *track, path string) string {
	m.mu.Lock()
	seen, read := t.reads.get(path)
	var last *writer
	if w := m.writers[path]; w != nil {
		copied := *w
		last = &copied
	}
	m.mu.Unlock()
	if !read && last == nil {
		return ""
	}
	current, ok := mtime(path)
	if !ok {
		return ""
	}
	if last != nil && last.session != t.session {
		if !read {
			return path + " was modified by sibling subagent '" + last.session + "' but this agent never read it. Read the file before writing to " +
				"avoid overwriting the sibling's changes."
		}
		if last.at.After(seen.at) {
			return path + " was modified by sibling subagent '" + last.session + "' at " + last.at.Format("15:04:05") + " — after this agent's last " +
				"read at " + seen.at.Format("15:04:05") + ". Re-read the file before writing."
		}
	}
	if read {
		if current != seen.mtime {
			return path + " was modified since you last read it on disk (external edit or unrecorded writer). Re-read the file before writing."
		}
		if seen.partial {
			return path + " was last read with offset/limit pagination (partial view). Read the remaining pages, or use patch, before overwriting it."
		}
		return ""
	}
	return path + " was not read by this agent. Read the file first so you can write an informed edit."
}
