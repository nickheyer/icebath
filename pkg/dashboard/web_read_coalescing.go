package dashboard

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
)

// In flight read shared by every caller with the same home and arguments
type coalescedTask[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// Synchronous read whose concurrent identical calls share one worker admitted one at a time
type coalescedRead[T any] struct {
	fn        func(ctx context.Context, args ...any) (T, error)
	mu        sync.Mutex
	pending   map[string]*coalescedTask[T]
	admission chan struct{}
}

// Wrapper of fn coalescing concurrent identical reads before admission to its single worker slot
func newCoalescedRead[T any](fn func(ctx context.Context, args ...any) (T, error)) *coalescedRead[T] {
	return &coalescedRead[T]{fn: fn, pending: map[string]*coalescedTask[T]{}, admission: make(chan struct{}, 1)}
}

// Hashable identity of the bound arguments, each value keyed with its type
func freezeArgs(args []any) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = fmt.Sprintf("%T:%#v", a, a)
	}
	return strings.Join(parts, "\x00")
}

// Result of the shared read for this home and args, the worker outliving a caller that gives up
func (c *coalescedRead[T]) call(ctx context.Context, args ...any) (T, error) {
	key := home.Dir(ctx) + "\x00" + freezeArgs(args)
	c.mu.Lock()
	task := c.pending[key]
	if task == nil {
		task = &coalescedTask[T]{done: make(chan struct{})}
		c.pending[key] = task
		wctx := context.WithoutCancel(ctx)
		go func() {
			defer close(task.done)
			defer func() {
				c.mu.Lock()
				delete(c.pending, key)
				c.mu.Unlock()
			}()
			c.admission <- struct{}{}
			defer func() { <-c.admission }()
			task.val, task.err = c.fn(wctx, args...)
		}()
	}
	c.mu.Unlock()
	select {
	case <-task.done:
		return task.val, task.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}
