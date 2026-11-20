package tools

import (
	"context"
	"sync"
	"testing"
)

func TestRegisterWorkerCleanupOnPanic(t *testing.T) {
	e := NewExecutor(nil, nil)
	var wg sync.WaitGroup
	ids := make(chan WorkerID, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { recover() }()
			ctx, id, release := e.registerWorker(context.Background())
			defer release()
			ids <- id
			if WorkerOf(ctx) != id {
				t.Errorf("worker ctx id mismatch")
			}
			SetInterrupt(id, true, "simulated CancelledError")
			panic("simulated CancelledError")
		}()
	}
	wg.Wait()
	close(ids)
	e.workersMu.Lock()
	left := len(e.workers)
	e.workersMu.Unlock()
	if left != 0 {
		t.Fatalf("workers not cleaned up %d", left)
	}
	for id := range ids {
		if IsWorkerInterrupted(id) {
			t.Fatalf("interrupt bit left set on %v", id)
		}
	}
}

func TestInterruptWorkersFansOut(t *testing.T) {
	e := NewExecutor(nil, nil)
	_, a, releaseA := e.registerWorker(context.Background())
	_, b, releaseB := e.registerWorker(context.Background())
	e.InterruptWorkers("stop")
	if !IsWorkerInterrupted(a) || !IsWorkerInterrupted(b) {
		t.Fatal("tracked workers not interrupted")
	}
	releaseA()
	releaseB()
	if IsWorkerInterrupted(a) || IsWorkerInterrupted(b) {
		t.Fatal("released workers keep their interrupt bit")
	}
}
