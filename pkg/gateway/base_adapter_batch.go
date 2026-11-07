package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"
)

type adapterTaskKey struct{}

// Task running under the context, nil outside an adapter task
func currentAdapterTask(ctx context.Context) *adapterTask {
	t, _ := ctx.Value(adapterTaskKey{}).(*adapterTask)
	return t
}

// Task running under the context for the platform adapters, nil outside an adapter task
func CurrentAdapterTask(ctx context.Context) *AdapterTask { return currentAdapterTask(ctx) }

// Starts fn as an adapter task detached from the caller's cancellation, like asyncio create_task
func spawnAdapterTask(ctx context.Context, fn func(ctx context.Context) error) *adapterTask {
	tctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t := &adapterTask{cancel: cancel, done: make(chan struct{})}
	tctx = context.WithValue(tctx, adapterTaskKey{}, t)
	go func() {
		defer close(t.done)
		defer cancel()
		t.err = fn(tctx)
	}()
	return t
}

// Starts fn as an adapter task for the platform adapters, detached from the caller's cancellation
func SpawnAdapterTask(ctx context.Context, fn func(ctx context.Context) error) *AdapterTask {
	return spawnAdapterTask(ctx, fn)
}

// Buffers a text event into the pending batch of its key and restarts the flush timer
func (b *BasePlatformAdapter) EnqueueTextEvent(ctx context.Context, event *MessageEvent) error {
	if b.dropUnresolved(ctx, event) {
		return nil
	}
	self := b.Self()
	key, err := self.TextBatchKey(ctx, event)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	batches := lazyAttr(&b.pendingTextBatches)
	existing, ok := batches[key]
	if !ok {
		existing = event
		batches[key] = event
	} else {
		if event.Text != "" {
			existing.Text = appendText(existing.Text, event.Text)
		}
		if len(event.MediaURLs) > 0 {
			existing.ExtendMedia(event)
		}
		existing.AbsorbReplyExpected(event)
	}
	existing.lastChunkLen = utf8.RuneCountInString(event.Text)
	tasks := lazyAttr(&b.pendingTextBatchTasks)
	if prior := tasks[key]; prior != nil && !prior.finished() {
		prior.cancel()
	}
	tasks[key] = spawnAdapterTask(ctx, func(tctx context.Context) error { return self.FlushTextBatch(tctx, key) })
	return nil
}

// Quiet period before a pending batch dispatches, near split chunks waiting longer
func (b *BasePlatformAdapter) TextBatchDelayFor(pending *MessageEvent) float64 {
	lastLen := 0
	if pending != nil {
		lastLen = pending.lastChunkLen
	}
	delay, split := b.TextBatchDelays()
	if lastLen >= splitThreshold {
		return split
	}
	return delay
}

// Removes and returns the pending batch of a key
func (b *BasePlatformAdapter) PopTextBatch(key string) *MessageEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	event := b.pendingTextBatches[key]
	delete(b.pendingTextBatches, key)
	return event
}

// Hands a flushed batch to the message pipeline
func (b *BasePlatformAdapter) DispatchTextBatch(ctx context.Context, event *MessageEvent) error {
	return b.Self().HandleMessage(ctx, event)
}

// Dispatches the pending batch of a key at once without the quiet period
func (b *BasePlatformAdapter) FlushTextBatchNow(ctx context.Context, key string) error {
	self := b.Self()
	if event := self.PopTextBatch(key); event != nil {
		return self.DispatchTextBatch(ctx, event)
	}
	return nil
}

// Waits the quiet period then dispatches the batch unless a newer flush owns the key, the dispatch shielded from cancel
func (b *BasePlatformAdapter) FlushTextBatch(ctx context.Context, key string) error {
	self := b.Self()
	current := currentAdapterTask(ctx)
	defer func() {
		b.mu.Lock()
		if current != nil && b.pendingTextBatchTasks[key] == current {
			delete(b.pendingTextBatchTasks, key)
		}
		b.mu.Unlock()
	}()
	b.mu.Lock()
	pending := b.pendingTextBatches[key]
	b.mu.Unlock()
	timer := time.NewTimer(secondsDuration(self.TextBatchDelayFor(pending)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil
	}
	b.mu.Lock()
	owner := b.pendingTextBatchTasks[key]
	b.mu.Unlock()
	if owner != nil && owner != current {
		return nil
	}
	event := self.PopTextBatch(key)
	if event == nil {
		return nil
	}
	slog.Info(fmt.Sprintf("[%s] Flushing text batch %s (%d chars)", self.Name(), key, utf8.RuneCountInString(event.Text)))
	done := make(chan error, 1)
	go func() { done <- self.DispatchTextBatch(context.WithoutCancel(ctx), event) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return nil
	}
}

// Pending text batch of a key
func (b *BasePlatformAdapter) PendingTextBatch(key string) *MessageEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pendingTextBatches[key]
}

// Number of flush tasks still owning a key
func (b *BasePlatformAdapter) PendingTextBatchTaskCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pendingTextBatchTasks)
}

// Waits until every pending flush task has finished or the context ends
func (b *BasePlatformAdapter) WaitTextBatches(ctx context.Context) error {
	for {
		b.mu.Lock()
		var pending *adapterTask
		for _, t := range b.pendingTextBatchTasks {
			pending = t
			break
		}
		b.mu.Unlock()
		if pending == nil {
			return nil
		}
		select {
		case <-pending.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
