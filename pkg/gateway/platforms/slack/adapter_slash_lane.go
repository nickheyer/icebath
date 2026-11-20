package slack

import (
	"math/big"
	"sync"
)

var bigOne = big.NewInt(1)

// One session slot a slash command hands over once it returned from the gateway
type HandoffSlot struct {
	done chan struct{}
	once sync.Once
}

// Whether the slot was already handed over
func (s *HandoffSlot) Done() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Channel closed when the slot is handed over
func (s *HandoffSlot) Wait() <-chan struct{} { return s.done }

// Per session FIFO of slash command handoffs so a later slash never reaches the gateway first
type SlashHandoffLanes struct {
	mu    sync.Mutex
	tails map[string]*HandoffSlot
}

// Empty handoff lanes
func NewSlashHandoffLanes() *SlashHandoffLanes {
	return &SlashHandoffLanes{tails: map[string]*HandoffSlot{}}
}

// Takes the session's next slot, returning the slot ahead or nil and this slot
func (l *SlashHandoffLanes) Reserve(key string) (ahead, handedOff *HandoffSlot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ahead = l.tails[key]
	handedOff = &HandoffSlot{done: make(chan struct{})}
	l.tails[key] = handedOff
	return ahead, handedOff
}

// Opens this slot once done, after the slot ahead opened when it is still pending
func (l *SlashHandoffLanes) Release(key string, ahead, handedOff *HandoffSlot) {
	if ahead != nil && !ahead.Done() {
		go func() {
			<-ahead.done
			l.open(key, handedOff)
		}()
		return
	}
	l.open(key, handedOff)
}

// Hands the slot over and drops it as the session tail when it still is
func (l *SlashHandoffLanes) open(key string, handedOff *HandoffSlot) {
	if handedOff == nil || handedOff.Done() {
		return
	}
	handedOff.once.Do(func() { close(handedOff.done) })
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tails[key] == handedOff {
		delete(l.tails, key)
	}
}
