package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"
)

// Read pool member double whose idle pooled connections return their permits when evicted
type fakeReadMember struct {
	readOnly bool
	writer   bool
	site     string
	path     string
	budget   *pathReadBudget
	idle     atomic.Int32
	evicted  atomic.Int32
}

func (m *fakeReadMember) readOnlyHandle() bool { return m.readOnly }
func (m *fakeReadMember) hasWriterConn() bool  { return m.writer }
func (m *fakeReadMember) creationSite() string { return m.site }
func (m *fakeReadMember) dbPath() string       { return m.path }
func (m *fakeReadMember) evictOneIdleReadConn() bool {
	for {
		n := m.idle.Load()
		if n == 0 {
			return false
		}
		if m.idle.CompareAndSwap(n, n-1) {
			m.evicted.Add(1)
			if err := m.budget.release(); err != nil {
				panic(err)
			}
			return true
		}
	}
}

// Fresh process permits, budgets and descriptor probes with an unmeasurable limit, restored after the test
func resetReadPool(t *testing.T) {
	t.Helper()
	savedSoft, savedCount, savedClock := fdSoftLimit, openFDCount, monotonicSeconds
	reset := func() {
		readBudgetsMu.Lock()
		processReadPermits = newBoundedSemaphore(readPoolProcessMax)
		readBudgets = map[string]weak.Pointer[pathReadBudget]{}
		readBudgetOrder = nil
		readOpenDeniedFDHeadroom = 0
		readBudgetsMu.Unlock()
		fdUsageMu.Lock()
		fdUsageCache = fdUsage{}
		fdUsageMu.Unlock()
	}
	reset()
	fdSoftLimit = func() (int64, bool) { return 0, false }
	t.Cleanup(func() {
		fdSoftLimit, openFDCount, monotonicSeconds = savedSoft, savedCount, savedClock
		reset()
	})
}

func newFakeMember(b *pathReadBudget, path, site string, readOnly bool) *fakeReadMember {
	m := &fakeReadMember{readOnly: readOnly, writer: !readOnly, site: site, path: path, budget: b}
	registerReadPoolMember(b, m)
	return m
}

// Takes permits until the budget refuses, returning how many it granted
func drainBudget(t *testing.T, b *pathReadBudget, requester readPoolMember, limit int) int {
	t.Helper()
	got := 0
	for range limit {
		ok, err := b.acquire(requester)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got++
	}
	return got
}

func TestFDHeadroomGuardFailsOpenWhereItCannotMeasure(t *testing.T) {
	resetReadPool(t)
	if !fdHeadroomOK() {
		t.Fatal("no RLIMIT_NOFILE must read as unmeasurable, not tight")
	}
	fdSoftLimit = func() (int64, bool) { return 256, true }
	openFDCount = func() (int, bool) { return -1, true }
	fdUsageCache = fdUsage{}
	if fdHeadroomOK() {
		t.Fatal("a probe that could not get a descriptor of its own is evidence of no headroom")
	}
}

func TestFDSoftLimitFailsOpenForResourceWithoutGetrlimit(t *testing.T) {
	resetReadPool(t)
	fdSoftLimit = fdSoftLimitImpl
	saved := rlimitNofile
	t.Cleanup(func() { rlimitNofile = saved })
	rlimitNofile = func() (int64, error) {
		return 0, errors.New("AttributeError: module 'resource' has no attribute 'getrlimit'")
	}
	if _, ok := fdSoftLimit(); ok {
		t.Fatal("a resource module without getrlimit must read as unmeasurable")
	}
	if !fdHeadroomOK() {
		t.Fatal("headroom must fail open when the limit is unmeasurable")
	}
	rlimitNofile = nil
	if _, ok := fdSoftLimit(); ok || !fdHeadroomOK() {
		t.Fatal("a platform without the resource module must read as unmeasurable")
	}
	for _, infinite := range []int64{rlimInfinity, -1} {
		rlimitNofile = func() (int64, error) { return infinite, nil }
		if _, ok := fdSoftLimit(); ok {
			t.Fatalf("soft limit %d is infinite", infinite)
		}
	}
	rlimitNofile = func() (int64, error) { return 1024, nil }
	if soft, ok := fdSoftLimit(); !ok || soft != 1024 {
		t.Fatalf("soft limit %d %v", soft, ok)
	}
}

func TestRealSoftLimitAndDescriptorCountAreMeasured(t *testing.T) {
	soft, softOK := fdSoftLimitImpl()
	count, countOK := openFDCountImpl()
	if runtime.GOOS == "windows" {
		if softOK || countOK {
			t.Fatalf("windows has neither a soft limit nor an fd dir, got %d %v %d %v", soft, softOK, count, countOK)
		}
		return
	}
	if !countOK || count < 3 {
		t.Fatalf("descriptor count %d %v", count, countOK)
	}
	if softOK && soft <= 0 {
		t.Fatalf("soft limit %d", soft)
	}
}

func TestNoReadConnectionIsOpenedWithoutDescriptorHeadroom(t *testing.T) {
	resetReadPool(t)
	b := readBudgetFor(filepath.Join(t.TempDir(), "state.db"))
	m := newFakeMember(b, "state.db", "site", false)
	fdSoftLimit = func() (int64, bool) { return 256, true }
	openFDCount = func() (int, bool) { return 250, true }
	fdUsageCache = fdUsage{}
	if ok, err := b.acquire(m); ok || err != nil {
		t.Fatalf("acquire within the reserve %v %v", ok, err)
	}
	if readOpenDeniedFDHeadroom != 1 {
		t.Fatalf("denied counter %d", readOpenDeniedFDHeadroom)
	}
	if !processReadPermits.tryAcquire() || processReadPermits.release() != nil {
		t.Fatal("a headroom refusal must not consume a process permit")
	}
	openFDCount = func() (int, bool) { return 10, true }
	fdUsageCache = fdUsage{}
	if ok, err := b.acquire(m); !ok || err != nil {
		t.Fatalf("headroom recovered but acquire %v %v", ok, err)
	}
	if err := b.release(); err != nil {
		t.Fatal(err)
	}
}

func TestDescriptorCountIsCachedBriefly(t *testing.T) {
	resetReadPool(t)
	now := 100.0
	monotonicSeconds = func() float64 { return now }
	fdSoftLimit = func() (int64, bool) { return 256, true }
	probes := 0
	openFDCount = func() (int, bool) { probes++; return 10, true }
	fdHeadroomOK()
	now += 0.1
	fdHeadroomOK()
	if probes != 1 {
		t.Fatalf("probe ran %d times inside the cache window", probes)
	}
	now += fdUsageCacheSeconds
	fdHeadroomOK()
	if probes != 2 {
		t.Fatalf("probe ran %d times after the cache window", probes)
	}
	openFDCount = func() (int, bool) { probes++; return 0, false }
	fdUsageCache = fdUsage{}
	fdHeadroomOK()
	fdHeadroomOK()
	if probes != 4 {
		t.Fatalf("an unmeasurable count is never cached, probes %d", probes)
	}
}

func TestPathBudgetCapsReadConnectionsAndReleasesBothPermits(t *testing.T) {
	resetReadPool(t)
	b := readBudgetFor(filepath.Join(t.TempDir(), "state.db"))
	m := newFakeMember(b, "state.db", "site", false)
	if got := drainBudget(t, b, m, readPoolMax+3); got != readPoolMax {
		t.Fatalf("granted %d read permits on one file", got)
	}
	for range readPoolMax {
		if err := b.release(); err != nil {
			t.Fatal(err)
		}
	}
	err := b.release()
	var conv *pyConvError
	if !errors.As(err, &conv) || conv.Class != "ValueError" || conv.Msg != "Semaphore released too many times" {
		t.Fatalf("over release %v", err)
	}
	if got := drainBudget(t, b, m, readPoolMax); got != readPoolMax {
		t.Fatalf("permits were stranded, granted %d after release", got)
	}
}

func TestIdlePermitsAreReclaimedFromAPeerInstance(t *testing.T) {
	resetReadPool(t)
	path := filepath.Join(t.TempDir(), "state.db")
	b := readBudgetFor(path)
	first := newFakeMember(b, path, "first", false)
	second := newFakeMember(readBudgetFor(path), path, "second", false)
	if got := drainBudget(t, b, first, readPoolMax); got != readPoolMax {
		t.Fatalf("granted %d", got)
	}
	first.idle.Store(readPoolMax)
	if ok, err := b.acquire(first); ok || err != nil {
		t.Fatalf("the requester's own idle connections are never reclaimed for it, got %v %v", ok, err)
	}
	if ok, err := b.acquire(second); !ok || err != nil {
		t.Fatalf("a peer's idle connection must be reclaimed, got %v %v", ok, err)
	}
	if first.evicted.Load() != 1 || second.evicted.Load() != 0 {
		t.Fatalf("evictions first %d second %d", first.evicted.Load(), second.evicted.Load())
	}
}

func TestProcessCeilingSpansDatabaseFilesAndReclaimsIdleAnywhere(t *testing.T) {
	resetReadPool(t)
	dir := t.TempDir()
	var members []*fakeReadMember
	var budgets []*pathReadBudget
	total := 0
	for i := range readPoolProcessMax/readPoolMax + 1 {
		path := filepath.Join(dir, fmt.Sprintf("p%d", i), "state.db")
		b := readBudgetFor(path)
		m := newFakeMember(b, path, "site", false)
		budgets, members = append(budgets, b), append(members, m)
		total += drainBudget(t, b, m, readPoolMax)
	}
	if total != readPoolProcessMax {
		t.Fatalf("granted %d read permits across files", total)
	}
	busy := len(budgets) - 1
	if ok, err := budgets[busy].acquire(members[busy]); ok || err != nil {
		t.Fatalf("process ceiling breached %v %v", ok, err)
	}
	members[0].idle.Store(readPoolMax)
	if ok, err := budgets[busy].acquire(members[busy]); !ok || err != nil {
		t.Fatalf("an idle connection on a quiet file must be reclaimed for a busy one, got %v %v", ok, err)
	}
	if members[0].evicted.Load() != 1 {
		t.Fatalf("evicted %d", members[0].evicted.Load())
	}
}

func TestPathRefusalReturnsTheProcessPermit(t *testing.T) {
	resetReadPool(t)
	b := readBudgetFor(filepath.Join(t.TempDir(), "state.db"))
	m := newFakeMember(b, "state.db", "site", false)
	drainBudget(t, b, m, readPoolMax)
	if ok, err := b.acquire(m); ok || err != nil {
		t.Fatalf("acquire past the file ceiling %v %v", ok, err)
	}
	free := 0
	for processReadPermits.tryAcquire() {
		free++
	}
	if free != readPoolProcessMax-readPoolMax {
		t.Fatalf("a refused file permit stranded a process permit, %d free", free)
	}
}

func TestDuplicateWritableHandlesOnOnePathAreReportedOnce(t *testing.T) {
	resetReadPool(t)
	logs := captureSlog(t)
	path := filepath.Join(t.TempDir(), "state.db")
	b := readBudgetFor(path)
	var keep []*fakeReadMember
	for i := range 3 {
		keep = append(keep, newFakeMember(b, path, fmt.Sprintf("ro%d", i), true))
	}
	for i := handlesPerPathWarn; i > 0; i-- {
		keep = append(keep, newFakeMember(b, path, fmt.Sprintf("site%d", i), false))
	}
	if strings.Contains(logs.String(), "live SessionDB handles") {
		t.Fatalf("read only handles counted toward the warning: %s", logs.String())
	}
	keep = append(keep, newFakeMember(b, path, "site0", false))
	want := fmt.Sprintf("5 live SessionDB handles on %s in this process; each holds its own writer connection "+
		"(read connections are capped at 8 for the file). A long-lived process should share one handle per path. "+
		"Created at: site0, site1, site2, site3, site4", path)
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("warning missing\nwant %s\ngot %s", want, logs.String())
	}
	keep = append(keep, newFakeMember(b, path, "site5", false))
	if n := strings.Count(logs.String(), "live SessionDB handles"); n != 1 {
		t.Fatalf("warned %d times", n)
	}
	runtime.KeepAlive(keep)
}

func TestClosedAndDroppedHandlesLeaveTheDuplicateCount(t *testing.T) {
	resetReadPool(t)
	logs := captureSlog(t)
	path := filepath.Join(t.TempDir(), "state.db")
	b := readBudgetFor(path)
	var closed []*fakeReadMember
	for i := range handlesPerPathWarn - 1 {
		closed = append(closed, newFakeMember(b, path, fmt.Sprintf("closed%d", i), false))
	}
	for _, m := range closed {
		m.writer = false
		unregisterReadPoolMember(b, m)
	}
	func() {
		for i := range handlesPerPathWarn {
			newFakeMember(b, path, fmt.Sprintf("dropped%d", i), false)
		}
	}()
	waitFor(t, func() bool {
		runtime.GC()
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.liveMembers()) == 0
	})
	survivor := newFakeMember(b, path, "survivor", false)
	if strings.Contains(logs.String(), "live SessionDB handles") {
		t.Fatalf("closed or collected handles counted: %s", logs.String())
	}
	runtime.KeepAlive(survivor)
	runtime.KeepAlive(closed)
}

func TestReadBudgetIsSharedAcrossSpellingsAndDroppedWhenUnheld(t *testing.T) {
	resetReadPool(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	b := readBudgetFor(path)
	if readBudgetFor("state.db") != b || readBudgetFor(dir+string(filepath.Separator)+"sub"+string(filepath.Separator)+".."+string(filepath.Separator)+"state.db") != b {
		t.Fatal("two spellings of one file got different budgets")
	}
	runtime.KeepAlive(b)
	b = nil
	waitFor(t, func() bool {
		runtime.GC()
		readBudgetsMu.Lock()
		defer readBudgetsMu.Unlock()
		return len(readBudgets) == 0 && len(readBudgetOrder) == 0
	})
}

func TestReclaimIdleAnywhereStopsAtTheFirstEviction(t *testing.T) {
	resetReadPool(t)
	dir := t.TempDir()
	var members []*fakeReadMember
	for i := range 3 {
		path := filepath.Join(dir, fmt.Sprintf("r%d", i), "state.db")
		b := readBudgetFor(path)
		m := newFakeMember(b, path, "site", false)
		drainBudget(t, b, m, 2)
		m.idle.Store(2)
		members = append(members, m)
	}
	if !reclaimIdleReadConnAnywhere() {
		t.Fatal("no idle connection was reclaimed")
	}
	if members[0].evicted.Load() != 1 || members[1].evicted.Load()+members[2].evicted.Load() != 0 {
		t.Fatal("reclaim did not stop at the first budget in insertion order")
	}
	for _, m := range members {
		m.idle.Store(0)
	}
	if reclaimIdleReadConnAnywhere() {
		t.Fatal("reclaimed with nothing idle")
	}
}

func TestConcurrentAcquireNeverExceedsTheCeilings(t *testing.T) {
	resetReadPool(t)
	path := filepath.Join(t.TempDir(), "state.db")
	b := readBudgetFor(path)
	m := newFakeMember(b, path, "site", false)
	var live, peak atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				ok, err := b.acquire(m)
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					continue
				}
				n := live.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				time.Sleep(time.Microsecond)
				live.Add(-1)
				if err := b.release(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if peak.Load() > readPoolMax {
		t.Fatalf("peak %d live read connections on one file", peak.Load())
	}
}

func TestProcFDTargetsListsOwnDescriptors(t *testing.T) {
	if runtime.GOOS != "linux" {
		if _, err := procFDTargets(os.Getpid()); err == nil {
			t.Fatal("a kernel without procfs must surface the listing error")
		}
		return
	}
	path := filepath.Join(t.TempDir(), "held")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	targets, err := procFDTargets(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), f.Fd())
	found := false
	for _, tg := range targets {
		if tg.target == path && tg.fdPath == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("own descriptor %s -> %s missing from %v", want, path, targets)
	}
	if _, err := procFDTargets(-1); err == nil {
		t.Fatal("a missing fd dir must surface its error")
	}
}

// Polls a condition for up to five seconds
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
