package acputil

import (
	"context"
	"sync"
)

type responseScopeKey struct{}

// Callbacks waiting for the reply of one request to be written
type responseScope struct {
	mu   sync.Mutex
	sent bool
	fns  []func()
}

// Runs fn on its own goroutine once the reply of the request ctx belongs to is written, at once when ctx belongs to no request
func AfterResponse(ctx context.Context, fn func()) {
	if scope, ok := ctx.Value(responseScopeKey{}).(*responseScope); ok {
		scope.mu.Lock()
		if !scope.sent {
			scope.fns = append(scope.fns, fn)
			scope.mu.Unlock()
			return
		}
		scope.mu.Unlock()
	}
	go fn()
}

// Request scope under ctx and the flush that runs what AfterResponse scheduled in it; a scope never flushed holds its callbacks back
func WithResponseScope(ctx context.Context) (context.Context, func()) {
	scope := &responseScope{}
	return context.WithValue(ctx, responseScopeKey{}, scope), scope.flush
}

func (s *responseScope) flush() {
	s.mu.Lock()
	fns := s.fns
	s.sent, s.fns = true, nil
	s.mu.Unlock()
	for _, fn := range fns {
		go fn()
	}
}
