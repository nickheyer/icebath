package gateway

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

type ephemeralDeleteCapableAdapter struct {
	*BasePlatformAdapter
	mu      sync.Mutex
	deleted [][2]string
	done    chan struct{}
}

func newEphemeralDeleteCapableAdapter() *ephemeralDeleteCapableAdapter {
	a := &ephemeralDeleteCapableAdapter{done: make(chan struct{}, 8)}
	a.BasePlatformAdapter = NewBasePlatformAdapter(NewPlatformConfig(), PlatformTelegram, a)
	return a
}

func (a *ephemeralDeleteCapableAdapter) Connect(context.Context, bool) (bool, error) {
	return true, nil
}
func (a *ephemeralDeleteCapableAdapter) Disconnect(context.Context) error { return nil }
func (a *ephemeralDeleteCapableAdapter) Send(context.Context, string, string, string, map[string]any) (SendResult, error) {
	return SendResult{Success: true, MessageID: "m-2"}, nil
}
func (a *ephemeralDeleteCapableAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (a *ephemeralDeleteCapableAdapter) DeleteMessage(_ context.Context, chatID, messageID string) (bool, error) {
	a.mu.Lock()
	a.deleted = append(a.deleted, [2]string{chatID, messageID})
	a.mu.Unlock()
	a.done <- struct{}{}
	return true, nil
}

func TestScheduleEphemeralDeleteCallsDeleteAfterTTL(t *testing.T) {
	adapter := newEphemeralDeleteCapableAdapter()
	var mu sync.Mutex
	var sleeps []float64
	prev := ephemeralDeleteWait
	ephemeralDeleteWait = func(ctx context.Context, seconds float64) error {
		mu.Lock()
		sleeps = append(sleeps, seconds)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { ephemeralDeleteWait = prev })
	adapter.ScheduleEphemeralDelete(context.Background(), "42", "m-2", 5)
	select {
	case <-adapter.done:
	case <-time.After(5 * time.Second):
		t.Fatal("ephemeral delete never ran")
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	gotSleeps := slices.Clone(sleeps)
	mu.Unlock()
	if !slices.Contains(gotSleeps, 5) {
		t.Fatalf("sleeps %v", gotSleeps)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if !slices.Equal(adapter.deleted, [][2]string{{"42", "m-2"}}) {
		t.Fatalf("deleted %v", adapter.deleted)
	}
}
