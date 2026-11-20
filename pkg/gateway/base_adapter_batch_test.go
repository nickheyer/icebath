package gateway

import (
	"context"
	"sync"
	"testing"
	"time"
)

// test_text_batch_flush_invariants.py
type batchAdapter struct {
	*BasePlatformAdapter
	mu         sync.Mutex
	dispatched []*MessageEvent
	entered    chan struct{}
	enterOnce  sync.Once
	release    chan struct{}
}

func (a *batchAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *batchAdapter) Disconnect(context.Context) error            { return nil }
func (a *batchAdapter) Send(context.Context, string, string, string, map[string]any) (SendResult, error) {
	return SendResult{}, nil
}
func (a *batchAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (a *batchAdapter) HandleMessage(_ context.Context, event *MessageEvent) error {
	a.enterOnce.Do(func() { close(a.entered) })
	<-a.release
	a.mu.Lock()
	a.dispatched = append(a.dispatched, event)
	a.mu.Unlock()
	return nil
}

func (a *batchAdapter) texts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []string{}
	for _, e := range a.dispatched {
		out = append(out, e.Text)
	}
	return out
}

func newBatchAdapter() *batchAdapter {
	a := &batchAdapter{entered: make(chan struct{}), release: make(chan struct{})}
	cfg := NewPlatformConfig()
	cfg.Enabled = true
	a.BasePlatformAdapter = NewBasePlatformAdapter(cfg, PlatformTelegram, a)
	return a
}

func batchEvent(text string) *MessageEvent {
	return &MessageEvent{Text: text, MessageType: MessageTypeText,
		Source: &SessionSource{Platform: PlatformTelegram, ChatID: "c", ChatType: "dm"}}
}

func TestCancelMidDispatchDoesNotAbortTheTurn(t *testing.T) {
	a := newBatchAdapter()
	a.pendingTextBatches["k"] = batchEvent("hello")
	a.mu.Lock()
	task := spawnAdapterTask(context.Background(), func(ctx context.Context) error { return a.FlushTextBatch(ctx, "k") })
	a.BasePlatformAdapter.mu.Lock()
	a.pendingTextBatchTasks["k"] = task
	a.BasePlatformAdapter.mu.Unlock()
	a.mu.Unlock()
	<-a.entered
	task.cancel()
	close(a.release)
	<-task.done
	if task.err != nil {
		t.Fatal(task.err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(a.texts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := a.texts(); len(got) != 1 || got[0] != "hello" {
		t.Fatal(got)
	}
	if a.PendingTextBatchTaskCount() != 0 {
		t.Fatal("flush task still owns the key")
	}
}

func TestSupersededFlushLeavesBatchForItsSuccessor(t *testing.T) {
	a := newBatchAdapter()
	close(a.release)
	event := batchEvent("hello")
	a.pendingTextBatches["k"] = event
	gate := make(chan struct{})
	stale := spawnAdapterTask(context.Background(), func(ctx context.Context) error {
		<-gate
		return a.FlushTextBatch(ctx, "k")
	})
	successor := spawnAdapterTask(context.Background(), func(ctx context.Context) error { <-ctx.Done(); return nil })
	a.BasePlatformAdapter.mu.Lock()
	a.pendingTextBatchTasks["k"] = successor
	a.BasePlatformAdapter.mu.Unlock()
	close(gate)
	<-stale.done
	successor.cancel()
	if len(a.texts()) != 0 || a.PendingTextBatch("k") != event {
		t.Fatal("stale flush consumed the batch")
	}
	a.BasePlatformAdapter.mu.Lock()
	owner := a.pendingTextBatchTasks["k"]
	a.BasePlatformAdapter.mu.Unlock()
	if owner != successor {
		t.Fatal("successor lost ownership")
	}
}

func TestEnqueueThenFlushDeliversMergedTextOnce(t *testing.T) {
	a := newBatchAdapter()
	close(a.release)
	ctx := context.Background()
	err := a.RunLoopTurn(ctx, func(ctx context.Context) error {
		if err := a.EnqueueTextEvent(ctx, batchEvent("part one")); err != nil {
			return err
		}
		return a.EnqueueTextEvent(ctx, batchEvent("part two"))
	})
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := a.WaitTextBatches(wctx); err != nil {
		t.Fatal(err)
	}
	if got := a.texts(); len(got) != 1 || got[0] != "part one\npart two" {
		t.Fatal(got)
	}
	if a.PendingTextBatch("k") != nil || len(a.pendingTextBatches) != 0 || a.PendingTextBatchTaskCount() != 0 {
		t.Fatal("batch state left behind")
	}
}

func TestTextBatchDelaysClampAndSplit(t *testing.T) {
	a := newBatchAdapter()
	a.Config.Extra = map[string]any{"text_batch_delay_seconds": "9", "text_batch_split_delay_seconds": -1.0}
	a.ConfigureTextBatchDelays()
	if d, s := a.TextBatchDelays(); d != 2.0 || s != 2.0 {
		t.Fatalf("delay %v split %v", d, s)
	}
	a.Config.Extra = map[string]any{"text_batch_delay_seconds": "nan", "text_batch_split_delay_seconds": nil}
	a.ConfigureTextBatchDelays()
	if d, s := a.TextBatchDelays(); d != 0.3 || s != 1.0 {
		t.Fatalf("delay %v split %v", d, s)
	}
	long := batchEvent("x")
	long.lastChunkLen = 4000
	if a.TextBatchDelayFor(long) != 1.0 || a.TextBatchDelayFor(batchEvent("x")) != 0.3 || a.TextBatchDelayFor(nil) != 0.3 {
		t.Fatal("split delay not chosen for near split chunks")
	}
}
