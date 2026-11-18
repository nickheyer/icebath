package media

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

const speechInterruptTTLSeconds = 120.0

var (
	speechInterruptedMu sync.Mutex
	speechInterruptedAt *float64
)

// MarkSpeechInterrupted latches that the user cut a spoken reply off before it finished
func MarkSpeechInterrupted() {
	now := pythread.Monotonic()
	speechInterruptedMu.Lock()
	speechInterruptedAt = &now
	speechInterruptedMu.Unlock()
}

// TakeSpeechInterrupted pops the latch, true when a barge in happened within the last two minutes
func TakeSpeechInterrupted() bool {
	speechInterruptedMu.Lock()
	at := speechInterruptedAt
	speechInterruptedAt = nil
	speechInterruptedMu.Unlock()
	return at != nil && pythread.Monotonic()-*at < speechInterruptTTLSeconds
}
