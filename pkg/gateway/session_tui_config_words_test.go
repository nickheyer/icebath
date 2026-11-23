package gateway

import (
	"testing"
)

func TestTranscriptDrainLockIsTheOneLockOfTheStore(t *testing.T) {
	store := &SessionStore{}
	lock := store.transcriptDrainLock()
	lock.Lock()
	if store.transcriptDrainLock().TryLock() {
		t.Fatal("a second holder took the drain lock")
	}
	lock.Unlock()
}
