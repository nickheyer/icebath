package media

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

func TestTakeSpeechInterruptedPopsTheLatchWithinItsTTL(t *testing.T) {
	if TakeSpeechInterrupted() {
		t.Fatal("no barge in was marked")
	}
	MarkSpeechInterrupted()
	if !TakeSpeechInterrupted() {
		t.Fatal("a fresh barge in must be taken")
	}
	if TakeSpeechInterrupted() {
		t.Fatal("the latch must pop on the first take")
	}
	stale := pythread.Monotonic() - speechInterruptTTLSeconds - 1
	speechInterruptedMu.Lock()
	speechInterruptedAt = &stale
	speechInterruptedMu.Unlock()
	if TakeSpeechInterrupted() {
		t.Fatal("a barge in older than the TTL must not annotate the next message")
	}
}
