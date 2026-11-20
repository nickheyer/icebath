package pythread

import (
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RLock is threading.RLock, reacquirable by the goroutine that holds it and released after as many unlocks
type RLock struct {
	mu    sync.Mutex
	cond  *sync.Cond
	owner uint64
	count int
}

// Calling goroutine id read from its stack header
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

// Blocks until the lock is free or already held by this goroutine, then bumps the hold count
func (l *RLock) Lock() {
	me := goroutineID()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cond == nil {
		l.cond = sync.NewCond(&l.mu)
	}
	for l.count > 0 && l.owner != me {
		l.cond.Wait()
	}
	l.owner = me
	l.count++
}

// Drops one hold and frees the lock for other goroutines at zero, panicking for a non owner like RuntimeError
func (l *RLock) Unlock() {
	me := goroutineID()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count == 0 || l.owner != me {
		panic("cannot release un-acquired lock")
	}
	l.count--
	if l.count == 0 {
		l.owner = 0
		if l.cond != nil {
			l.cond.Broadcast()
		}
	}
}

// Acquires like Lock but gives up once timeout passes, the acquire(timeout=...) form
func (l *RLock) TryLockFor(timeout time.Duration) bool {
	me := goroutineID()
	deadline := time.Now().Add(timeout)
	for {
		l.mu.Lock()
		if l.count == 0 || l.owner == me {
			l.owner = me
			l.count++
			l.mu.Unlock()
			return true
		}
		l.mu.Unlock()
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}
