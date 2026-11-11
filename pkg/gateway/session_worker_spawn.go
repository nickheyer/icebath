package gateway

import (
	"context"
	"sync"
)

// Spawns a managed worker child off the owner, a cancelled acquisition closing whatever child the spawn produced
func acquireProcess(ctx context.Context, spawn func() (*workerProcess, error)) (*workerProcess, error) {
	var mu sync.Mutex
	cancelled := false
	var held *workerProcess
	type outcome struct {
		process *workerProcess
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		process, err := spawn()
		if err == nil {
			mu.Lock()
			late := cancelled
			if !late {
				held = process
			}
			mu.Unlock()
			if late {
				_ = newManagedWorker(process).close()
			}
		}
		done <- outcome{process, err}
	}()
	select {
	case result := <-done:
		mu.Lock()
		held = nil
		mu.Unlock()
		return result.process, result.err
	case <-ctx.Done():
		mu.Lock()
		cancelled = true
		process := held
		held = nil
		mu.Unlock()
		if process != nil {
			_ = newManagedWorker(process).close()
		}
		return nil, ctx.Err()
	}
}
