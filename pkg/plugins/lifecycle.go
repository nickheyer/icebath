package plugins

import (
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Identity comparison of registry snapshots, element wise for slices of values
func sameRegistration(left, right any) bool {
	l, lok := left.([]any)
	r, rok := right.([]any)
	if lok && rok {
		if len(l) != len(r) {
			return false
		}
		for i := range l {
			if !sameRegistration(l[i], r[i]) {
				return false
			}
		}
		return true
	}
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	lv, rv := reflect.ValueOf(left), reflect.ValueOf(right)
	if lv.Type() != rv.Type() {
		return false
	}
	switch lv.Kind() {
	case reflect.Func, reflect.Map, reflect.Slice, reflect.Chan, reflect.Pointer, reflect.UnsafePointer:
		return lv.Pointer() == rv.Pointer()
	}
	if lv.Type().Comparable() {
		return left == right
	}
	return false
}

// One ownership generation in a replaceable registry slot
type ReplacementLease struct {
	coordinator *ReplacementCoordinator
	slot        any
	current     any
	previous    any
	restore     func(replacement any) error
	finalize    func() error
	predecessor *ReplacementLease
	active      bool
}

// Releases the lease, restoring the nearest live predecessor when it is the latest generation
func (l *ReplacementLease) Dispose() error { return l.coordinator.dispose(l) }

// Links and removes registration generations in any unload order
type ReplacementCoordinator struct {
	mu      sync.Mutex
	ownerMu sync.Mutex
	owner   uint64
	depth   int
	active  map[any][]*ReplacementLease
}

// Coordinator with no active generations and its re-entrant lock free
func newReplacementCoordinator() *ReplacementCoordinator {
	return &ReplacementCoordinator{active: map[any][]*ReplacementLease{}}
}

var replacementCoordinator = newReplacementCoordinator()

// Runs fn while serialized against every acquire and dispose, re-entrant on the owning goroutine
func (c *ReplacementCoordinator) Transaction(fn func()) {
	c.lock()
	defer c.unlock()
	fn()
}

// Takes the coordinator lock unless the calling goroutine already owns it
func (c *ReplacementCoordinator) lock() {
	id := goroutineID()
	c.ownerMu.Lock()
	if c.owner == id && c.depth > 0 {
		c.depth++
		c.ownerMu.Unlock()
		return
	}
	c.ownerMu.Unlock()
	c.mu.Lock()
	c.ownerMu.Lock()
	c.owner, c.depth = id, 1
	c.ownerMu.Unlock()
}

// Releases one level of the re-entrant coordinator lock
func (c *ReplacementCoordinator) unlock() {
	c.ownerMu.Lock()
	c.depth--
	release := c.depth == 0
	if release {
		c.owner = 0
	}
	c.ownerMu.Unlock()
	if release {
		c.mu.Unlock()
	}
}

// Numeric id of the calling goroutine parsed from its stack header
func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(strings.TrimPrefix(string(buf[:n]), "goroutine "))
	if len(fields) == 0 {
		return 0
	}
	id, _ := strconv.ParseUint(fields[0], 10, 64)
	return id
}

// Attaches a new live generation to the active predecessor whose value equals previous
func (c *ReplacementCoordinator) Acquire(slot, current, previous any, restore func(any) error, finalize func() error) *ReplacementLease {
	c.lock()
	defer c.unlock()
	leases := c.active[slot]
	var predecessor *ReplacementLease
	for i := len(leases) - 1; i >= 0; i-- {
		if leases[i].active && sameRegistration(leases[i].current, previous) {
			predecessor = leases[i]
			break
		}
	}
	lease := &ReplacementLease{coordinator: c, slot: slot, current: current, previous: previous,
		restore: restore, finalize: finalize, predecessor: predecessor, active: true}
	c.active[slot] = append(leases, lease)
	return lease
}

// Removes a lease, restoring then finalizing then pruning the slot even when an earlier step fails
func (c *ReplacementCoordinator) dispose(lease *ReplacementLease) (err error) {
	c.lock()
	defer c.unlock()
	if !lease.active {
		return nil
	}
	leases := c.active[lease.slot]
	var latest *ReplacementLease
	for i := len(leases) - 1; i >= 0; i-- {
		if leases[i].active {
			latest = leases[i]
			break
		}
	}
	lease.active = false
	defer func() {
		var live []*ReplacementLease
		for _, item := range leases {
			if item.active {
				live = append(live, item)
			}
		}
		if len(live) > 0 {
			c.active[lease.slot] = live
		} else if len(leases) > 0 {
			delete(c.active, lease.slot)
		}
	}()
	defer func() {
		if lease.finalize != nil {
			if ferr := callGuarded(lease.finalize); ferr != nil {
				err = ferr
			}
		}
	}()
	if latest == lease {
		replacement, predecessor := lease.previous, lease.predecessor
		for predecessor != nil && !predecessor.active {
			replacement, predecessor = predecessor.previous, predecessor.predecessor
		}
		if predecessor != nil {
			replacement = predecessor.current
		}
		return callGuarded(func() error { return lease.restore(replacement) })
	}
	return nil
}
