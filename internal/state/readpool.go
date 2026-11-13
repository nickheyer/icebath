package state

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// Read connections alive at once against one database file, idle pooled and checked out together
const readPoolMax = 8

// Read connections alive at once in this process across every database file
const readPoolProcessMax = 24

// Writable store handles on one file past which a duplicate handle warning fires
const handlesPerPathWarn = 4

// Descriptors kept in reserve for everything in the process that is not a database
const fdHeadroomReserve = 64

// Lifetime of one descriptor count so a read burst is not a directory listing per query
const fdUsageCacheFor = 250 * time.Millisecond

// Read permits per database file and for the whole process, plus the cached descriptor count
type readBudgets struct {
	mu        sync.Mutex
	byPath    map[string]*pathBudget
	process   chan struct{}
	fdMu      sync.Mutex
	fdStamp   time.Time
	fdCount   int
	fdMeasure bool
}

// Budgets with every permit free
func newReadBudgets() readBudgets {
	return readBudgets{byPath: map[string]*pathBudget{}, process: make(chan struct{}, readPoolProcessMax)}
}

// Read permits for one database file shared by every store of the process on it
type pathBudget struct {
	key     string
	refs    int
	permits chan struct{}
	mu      sync.Mutex
	members map[*Store]bool
	warned  bool
}

// Budget of path, created on first use and held until its last store lets go
func (e *Engine) budgetFor(path string) *pathBudget {
	key := resolvePath(path)
	r := &e.reads
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.byPath[key]
	if !ok {
		b = &pathBudget{key: key, permits: make(chan struct{}, readPoolMax), members: map[*Store]bool{}}
		r.byPath[key] = b
	}
	b.refs++
	return b
}

// Lets go of a budget, forgetting it once no store holds it
func (e *Engine) dropBudget(b *pathBudget) {
	r := &e.reads
	r.mu.Lock()
	defer r.mu.Unlock()
	if b.refs--; b.refs <= 0 && r.byPath[b.key] == b {
		delete(r.byPath, b.key)
	}
}

// Adds an opened store to the budget and warns once when too many writable handles share the file
func (b *pathBudget) register(s *Store) {
	b.mu.Lock()
	b.members[s] = true
	writers, sites := 0, []string{}
	for m := range b.members {
		if !m.readOnly {
			sites = append(sites, m.site)
			if m.hasWriter() {
				writers++
			}
		}
	}
	warn := writers > handlesPerPathWarn && !b.warned
	if warn {
		b.warned = true
	}
	b.mu.Unlock()
	if warn {
		slices.Sort(sites)
		slog.Warn(fmt.Sprintf("%d live state store handles on %s in this process; each holds its own writer connection "+
			"(read connections are capped at %d for the file). A long-lived process should share one handle per path. "+
			"Created at: %s", writers, s.path, readPoolMax, strings.Join(sites, ", ")))
	}
}

// Removes a closed store from the budget
func (b *pathBudget) unregister(s *Store) {
	b.mu.Lock()
	delete(b.members, s)
	b.mu.Unlock()
}

// Takes the permits for one new read connection or refuses, descriptor headroom first, then the process and file ceilings
func (e *Engine) acquireRead(b *pathBudget, requester *Store) bool {
	if !e.fdHeadroomOK() {
		return false
	}
	if !e.acquireProcessPermit() {
		return false
	}
	if tryTake(b.permits) || (e.reclaimIdleIn(b, requester) && tryTake(b.permits)) {
		return true
	}
	<-e.reads.process
	return false
}

// Returns the permits of one read connection
func (e *Engine) releaseRead(b *pathBudget) {
	<-b.permits
	<-e.reads.process
}

// Takes one slot of a permit channel without waiting
func tryTake(permits chan struct{}) bool {
	select {
	case permits <- struct{}{}:
		return true
	default:
		return false
	}
}

// Takes a process permit, reclaiming an idle read connection on any file when none is free
func (e *Engine) acquireProcessPermit() bool {
	return tryTake(e.reads.process) || (e.reclaimIdleAnywhere() && tryTake(e.reads.process))
}

// Closes one idle read connection held by a store of the budget other than exclude
func (e *Engine) reclaimIdleIn(b *pathBudget, exclude *Store) bool {
	b.mu.Lock()
	members := make([]*Store, 0, len(b.members))
	for m := range b.members {
		if m != exclude {
			members = append(members, m)
		}
	}
	b.mu.Unlock()
	for _, m := range members {
		if m.evictIdleRead() {
			return true
		}
	}
	return false
}

// Closes one idle read connection on any file, the process ceiling being shared across files
func (e *Engine) reclaimIdleAnywhere() bool {
	e.reads.mu.Lock()
	budgets := make([]*pathBudget, 0, len(e.reads.byPath))
	for _, b := range e.reads.byPath {
		budgets = append(budgets, b)
	}
	e.reads.mu.Unlock()
	for _, b := range budgets {
		if e.reclaimIdleIn(b, nil) {
			return true
		}
	}
	return false
}

// Reports whether the process can spare a descriptor for a new read connection, failing closed only on measured shortfall
func (e *Engine) fdHeadroomOK() bool {
	soft, ok := fdSoftLimit()
	if !ok {
		return true
	}
	r := &e.reads
	r.fdMu.Lock()
	fresh := r.fdMeasure && time.Since(r.fdStamp) < fdUsageCacheFor
	count, measured := r.fdCount, r.fdMeasure
	r.fdMu.Unlock()
	if !fresh {
		count, measured = openFDCount()
		r.fdMu.Lock()
		r.fdStamp, r.fdCount, r.fdMeasure = time.Now(), count, measured
		r.fdMu.Unlock()
	}
	if !measured {
		return true
	}
	return count >= 0 && soft-int64(count) > fdHeadroomReserve
}
