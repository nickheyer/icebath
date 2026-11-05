package state

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"weak"
)

const (
	readPoolMax         = 8
	readPoolProcessMax  = 24
	handlesPerPathWarn  = 4
	fdHeadroomReserve   = 64
	fdUsageCacheSeconds = 0.25
)

// Python threading.BoundedSemaphore taken without blocking, refusing a release past its bound
type boundedSemaphore struct {
	mu    sync.Mutex
	value int
	bound int
}

func newBoundedSemaphore(n int) *boundedSemaphore { return &boundedSemaphore{value: n, bound: n} }

// Takes one permit when any is free and reports whether it did
func (s *boundedSemaphore) tryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value == 0 {
		return false
	}
	s.value--
	return true
}

// Returns one permit, the ValueError BoundedSemaphore raises when every permit is already free
func (s *boundedSemaphore) release() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value >= s.bound {
		return &pyConvError{"ValueError", "Semaphore released too many times"}
	}
	s.value++
	return nil
}

var (
	processReadPermits = newBoundedSemaphore(readPoolProcessMax)

	readBudgetsMu            sync.Mutex
	readBudgets              = map[string]weak.Pointer[pathReadBudget]{}
	readBudgetOrder          []string
	readOpenDeniedFDHeadroom int

	fdUsageMu    sync.Mutex
	fdUsageCache fdUsage

	readPoolClockStart = time.Now()
	monotonicSeconds   = func() float64 { return time.Since(readPoolClockStart).Seconds() }

	fdSoftLimit = fdSoftLimitImpl
	openFDCount = openFDCountImpl
)

// Cached descriptor count with its monotonic stamp, known false for an unmeasurable count
type fdUsage struct {
	stamp float64
	count int
	known bool
}

// One readable descriptor link of a process with the fd path it came from
type fdTarget struct {
	target string
	fdPath string
}

// Names in a directory in listing order, the os.listdir view
func listdirNames(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// Readlink target and fd path of each descriptor of pid, unreadable links skipped, error for an unlistable fd dir
func procFDTargets(pid int) ([]fdTarget, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	names, err := listdirNames(fdDir)
	if err != nil {
		return nil, err
	}
	out := make([]fdTarget, 0, len(names))
	for _, fd := range names {
		fdPath := fdDir + "/" + fd
		target, err := os.Readlink(fdPath)
		if err != nil {
			continue
		}
		out = append(out, fdTarget{target: target, fdPath: fdPath})
	}
	return out, nil
}

// Open descriptors of this process, unknown without an fd dir and -1 when the probe itself hit EMFILE or ENFILE
func openFDCountImpl() (int, bool) {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		names, err := listdirNames(dir)
		if err == nil {
			return len(names), true
		}
		if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
			return -1, true
		}
	}
	return 0, false
}

// Soft RLIMIT_NOFILE of the process, false without a resource module, on error or for an infinite limit
func fdSoftLimitImpl() (int64, bool) {
	if rlimitNofile == nil {
		return 0, false
	}
	soft, err := rlimitNofile()
	if err != nil || soft == rlimInfinity || soft == -1 {
		return 0, false
	}
	return soft, true
}

// Whether the process can spare a descriptor, open when unmeasurable and closed only on a measured shortfall
func fdHeadroomOK() bool {
	soft, ok := fdSoftLimit()
	if !ok {
		return true
	}
	now := monotonicSeconds()
	fdUsageMu.Lock()
	cached := fdUsageCache
	fresh := cached.known && now-cached.stamp < fdUsageCacheSeconds
	fdUsageMu.Unlock()
	if !fresh {
		count, known := openFDCount()
		cached = fdUsage{stamp: now, count: count, known: known}
		fdUsageMu.Lock()
		fdUsageCache = cached
		fdUsageMu.Unlock()
	}
	if !cached.known {
		return true
	}
	return cached.count >= 0 && soft-int64(cached.count) > fdHeadroomReserve
}

// Closes one idle read connection on any path since the process ceiling is shared across files
func reclaimIdleReadConnAnywhere() bool {
	readBudgetsMu.Lock()
	budgets := make([]*pathReadBudget, 0, len(readBudgetOrder))
	for _, key := range readBudgetOrder {
		if b := readBudgets[key].Value(); b != nil {
			budgets = append(budgets, b)
		}
	}
	readBudgetsMu.Unlock()
	for _, b := range budgets {
		if b.reclaimIdle(nil) {
			return true
		}
	}
	return false
}

// Session handle a path budget tracks, the SessionDB attributes the read pool consults
type readPoolMember interface {
	readOnlyHandle() bool
	hasWriterConn() bool
	creationSite() string
	dbPath() string
	evictOneIdleReadConn() bool
}

// Weakly held budget member, value nil once the handle was collected
type weakMember struct {
	key   any
	value func() readPoolMember
}

// Weak reference to a handle so a SessionDB dropped without close never pins its peers' budget
func weakReadPoolMember[T any, P interface {
	*T
	readPoolMember
}](db P) weakMember {
	wp := weak.Make((*T)(db))
	return weakMember{key: wp, value: func() readPoolMember {
		if p := wp.Value(); p != nil {
			return P(p)
		}
		return nil
	}}
}

// Read connection permits for one database file shared process wide with weakly held member handles
type pathReadBudget struct {
	permits                *boundedSemaphore
	mu                     sync.Mutex
	members                []weakMember
	duplicateHandlesWarned bool
}

func newPathReadBudget() *pathReadBudget {
	return &pathReadBudget{permits: newBoundedSemaphore(readPoolMax)}
}

// Live members in registration order, dropping collected ones, caller holds the budget lock
func (b *pathReadBudget) liveMembers() []readPoolMember {
	live := make([]readPoolMember, 0, len(b.members))
	kept := b.members[:0]
	for _, m := range b.members {
		if v := m.value(); v != nil {
			live = append(live, v)
			kept = append(kept, m)
		}
	}
	clear(b.members[len(kept):])
	b.members = kept
	return live
}

// Adds a handle and warns once when writable handles on the file pass the duplicate threshold
func registerReadPoolMember[T any, P interface {
	*T
	readPoolMember
}](b *pathReadBudget, db P) {
	b.register(db, weakReadPoolMember(db))
}

func (b *pathReadBudget) register(db readPoolMember, entry weakMember) {
	b.mu.Lock()
	present := false
	for _, m := range b.members {
		if m.key == entry.key {
			present = true
			break
		}
	}
	if !present {
		b.members = append(b.members, entry)
	}
	members := b.liveMembers()
	handles := 0
	for _, m := range members {
		if !m.readOnlyHandle() && m.hasWriterConn() {
			handles++
		}
	}
	warn := handles > handlesPerPathWarn && !b.duplicateHandlesWarned
	var creationSites string
	if warn {
		b.duplicateHandlesWarned = true
		var sites []string
		for _, m := range members {
			if !m.readOnlyHandle() {
				sites = append(sites, m.creationSite())
			}
		}
		sort.Strings(sites)
		creationSites = strings.Join(sites, ", ")
	}
	b.mu.Unlock()
	if warn {
		slog.Warn(fmt.Sprintf("%d live SessionDB handles on %s in this process; each holds "+
			"its own writer connection (read connections are capped at %d "+
			"for the file). A long-lived process should share one handle per path. "+
			"Created at: %s", handles, db.dbPath(), readPoolMax, creationSites))
	}
}

// Removes a closed writer from the duplicate handle diagnostics immediately
func unregisterReadPoolMember[T any, P interface {
	*T
	readPoolMember
}](b *pathReadBudget, db P) {
	key := weakReadPoolMember(db).key
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, m := range b.members {
		if m.key == key {
			b.members = append(b.members[:i], b.members[i+1:]...)
			return
		}
	}
}

// Takes a permit for a new read connection through the headroom, process and file gates, broadest first
func (b *pathReadBudget) acquire(requester readPoolMember) (bool, error) {
	if !fdHeadroomOK() {
		readBudgetsMu.Lock()
		readOpenDeniedFDHeadroom++
		readBudgetsMu.Unlock()
		return false, nil
	}
	if !b.acquireProcessPermit() {
		return false, nil
	}
	if b.acquirePathPermit(requester) {
		return true, nil
	}
	return false, processReadPermits.release()
}

// Returns one connection's file and process permits, pairing a successful acquire
func (b *pathReadBudget) release() error {
	if err := b.permits.release(); err != nil {
		return err
	}
	return processReadPermits.release()
}

// Process permit taken directly or after reclaiming an idle connection anywhere, a lost race is not retried
func (b *pathReadBudget) acquireProcessPermit() bool {
	return processReadPermits.tryAcquire() || (reclaimIdleReadConnAnywhere() && processReadPermits.tryAcquire())
}

// File permit taken directly or after reclaiming an idle connection from a peer of the requester
func (b *pathReadBudget) acquirePathPermit(requester readPoolMember) bool {
	return b.permits.tryAcquire() || (b.reclaimIdle(requester) && b.permits.tryAcquire())
}

// Closes one idle pooled connection held by a member other than exclude, true when one went
func (b *pathReadBudget) reclaimIdle(exclude readPoolMember) bool {
	b.mu.Lock()
	var members []readPoolMember
	for _, m := range b.liveMembers() {
		if exclude == nil || m != exclude {
			members = append(members, m)
		}
	}
	b.mu.Unlock()
	for _, m := range members {
		if m.evictOneIdleReadConn() {
			return true
		}
	}
	return false
}

// Canonical database path so two spellings share one budget
func readBudgetKey(dbPath string) string {
	return realpathLax(dbPath)
}

// Budget of a database file, created on first use and dropped once no handle holds it
func readBudgetFor(dbPath string) *pathReadBudget {
	key := readBudgetKey(dbPath)
	readBudgetsMu.Lock()
	defer readBudgetsMu.Unlock()
	if wp, ok := readBudgets[key]; ok {
		if b := wp.Value(); b != nil {
			return b
		}
		dropReadBudgetKey(key)
	}
	b := newPathReadBudget()
	wp := weak.Make(b)
	readBudgets[key] = wp
	readBudgetOrder = append(readBudgetOrder, key)
	runtime.AddCleanup(b, func(key string) {
		readBudgetsMu.Lock()
		defer readBudgetsMu.Unlock()
		if cur, ok := readBudgets[key]; ok && cur == wp {
			dropReadBudgetKey(key)
		}
	}, key)
	return b
}

// Deletes a budget key with its insertion order slot, caller holds the registry lock
func dropReadBudgetKey(key string) {
	delete(readBudgets, key)
	for i, k := range readBudgetOrder {
		if k == key {
			readBudgetOrder = append(readBudgetOrder[:i], readBudgetOrder[i+1:]...)
			return
		}
	}
}
