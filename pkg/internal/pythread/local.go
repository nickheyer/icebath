package pythread

import (
	"bytes"
	"runtime"
	"strconv"
	"sync"
)

// Identifier of the calling goroutine, the Go form of threading.get_ident
func GoroutineID() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	buf = bytes.TrimPrefix(buf, []byte("goroutine "))
	if i := bytes.IndexByte(buf, ' '); i > 0 {
		buf = buf[:i]
	}
	id, _ := strconv.ParseUint(string(buf), 10, 64)
	return id
}

// Per goroutine value slot, the threading.local form keyed by the calling goroutine
type Local struct {
	mu      sync.Mutex
	vals    map[uint64]any
	pruneAt int
}

// Slot count below which a Local never scans for finished goroutines
const localPruneFloor = 256

// Value the calling goroutine stored, false when it stored none
func (l *Local) Get() (any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.vals[GoroutineID()]
	return v, ok
}

// Stores the value for the calling goroutine only
func (l *Local) Set(v any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.vals == nil {
		l.vals = map[uint64]any{}
	}
	l.vals[GoroutineID()] = v
	if len(l.vals) >= max(l.pruneAt, localPruneFloor) {
		live := liveGoroutineIDs()
		for id := range l.vals {
			if !live[id] {
				delete(l.vals, id)
			}
		}
		l.pruneAt = 2 * len(l.vals)
	}
}

// Identifiers of every goroutine alive now, so slots of finished goroutines die with them as thread locals do
func liveGoroutineIDs() map[uint64]bool {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	live := map[uint64]bool{}
	for _, line := range bytes.Split(buf, []byte("\n")) {
		rest, ok := bytes.CutPrefix(line, []byte("goroutine "))
		if !ok {
			continue
		}
		if i := bytes.IndexByte(rest, ' '); i > 0 {
			if id, err := strconv.ParseUint(string(rest[:i]), 10, 64); err == nil {
				live[id] = true
			}
		}
	}
	return live
}

// Forgets the calling goroutine's value so a finished goroutine leaves nothing behind
func (l *Local) Delete() {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.vals, GoroutineID())
}
