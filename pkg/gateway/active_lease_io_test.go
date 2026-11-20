package gateway

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestAQueuedReleaseReturnsWhileTheRegistryIsLockedAndLandsAfter(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(home.EnvHome, tmp)
	lease, refusal, err := activesessions.TryAcquire(context.Background(), "s1", "gateway:local", map[string]any{}, nil, tmp, false)
	if err != nil || refusal != nil || lease == nil {
		t.Fatalf("lease %v refusal %v err %v", lease, refusal, err)
	}
	if lease.LockPath == "" || lease.StatePath == "" {
		t.Fatal("lease paths not pinned")
	}
	unlock, err := activesessions.FileLock(lease.LockPath, activesessions.NoLockTimeout)
	if err != nil {
		t.Fatal(err)
	}
	returned := make(chan struct{})
	go func() { ReleaseSoon(lease); close(returned) }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		unlock()
		t.Fatal("the release waited on the registry lock in the caller")
	}
	if lease.Released() {
		unlock()
		t.Fatal("released inline under a held registry lock")
	}
	unlock()
	deadline := time.Now().Add(5 * time.Second)
	for !lease.Released() {
		if time.Now().After(deadline) {
			t.Fatal("the queued release never landed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, err := os.ReadFile(lease.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := data.(*pyjson.Object).GetOr("entries", nil).([]any); len(entries) != 0 {
		t.Fatalf("entries %v", entries)
	}
}
