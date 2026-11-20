package wecom

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestBucketNormalReservedAndWindowReset(t *testing.T) {
	now := 100.0
	old := monotonic
	monotonic = func() float64 { return now }
	t.Cleanup(func() { monotonic = old })
	q := NewChatSendQueue("wecom")
	for i := 0; i < bucketNormalTokens; i++ {
		if w := q.bucketTryConsume(" c ", false); w != 0 {
			t.Fatal(i, w)
		}
	}
	now += 10
	if w := q.bucketTryConsume("c", false); w != 50 {
		t.Fatal(w)
	}
	for i := 0; i < bucketReservedTokens; i++ {
		if w := q.bucketTryConsume("c", true); w != 0 {
			t.Fatal(i, w)
		}
	}
	if w := q.bucketTryConsume("c", true); w != 50 {
		t.Fatal(w)
	}
	now += 51
	if w := q.bucketTryConsume("c", false); w != 0 {
		t.Fatal(w)
	}
	if bucketNormalTokens+bucketReservedTokens != bucketMaxTokens {
		t.Fatal("bucket sizes")
	}
}

func TestEnqueueChatSendFIFOAndShutdown(t *testing.T) {
	q := NewChatSendQueue("wecom")
	var mu sync.Mutex
	var order []int
	release := make(chan struct{})
	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	go func() {
		defer wg.Done()
		_, err := q.EnqueueChatSend(callerCtx, "c", func(ctx context.Context) (any, error) {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			mu.Lock()
			order = append(order, 0)
			mu.Unlock()
			return "first", nil
		}, false)
		if !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	<-started
	results := make(chan error, 3)
	for i := 1; i <= 3; i++ {
		go func() {
			_, err := q.EnqueueChatSend(context.Background(), "c", func(ctx context.Context) (any, error) {
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
				return nil, errors.New("boom")
			}, false)
			results <- err
		}()
	}
	for {
		l := q.chatQueues["c"]
		l.mu.Lock()
		n := len(l.jobs)
		l.mu.Unlock()
		if n == 3 {
			break
		}
	}
	q.Shutdown()
	close(release)
	cancelCaller()
	wg.Wait()
	for i := 0; i < 3; i++ {
		if err := <-results; err == nil || err.Error() != "WeCom adapter shutting down" {
			t.Fatal(err)
		}
	}
	if len(order) != 1 {
		t.Fatal(order)
	}
	q2 := NewChatSendQueue("wecom")
	defer q2.Shutdown()
	if v, err := q2.EnqueueChatSend(context.Background(), "d", func(ctx context.Context) (any, error) { return 7, nil }, true); v != 7 || err != nil {
		t.Fatal(v, err)
	}
}
