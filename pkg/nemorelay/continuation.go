package nemorelay

import (
	"context"
	"sync"
)

// Runtime bindings captured from one call so middleware continuations resume under them
type MiddlewareContinuationContext struct {
	scopeStack           *ScopeStack
	activeEvent          *anchoredActiveEvent
	activeTraceContext   *W3CTraceContext
	publicationBuffer    *PublicationBuffer
	optimizationRecorder *LLMOptimizationRecorder
}

// Captures the stack, active event, nested publication buffer and optimization recorder
func CaptureMiddlewareContinuationContext(ctx context.Context) *MiddlewareContinuationContext {
	return &MiddlewareContinuationContext{
		scopeStack:           CurrentScopeStack(ctx),
		activeEvent:          captureAnchoredActiveEvent(ctx),
		activeTraceContext:   activeEventTraceContext(ctx),
		publicationBuffer:    CaptureNestedPublicationBuffer(ctx),
		optimizationRecorder: CurrentLLMOptimizationRecorder(ctx),
	}
}

// Returns a copy bound to a snapshot of its own stack
func (c *MiddlewareContinuationContext) Isolated() (*MiddlewareContinuationContext, error) {
	return c.IsolatedWithScopeStack(c.scopeStack)
}

// Returns a copy bound to a snapshot of the given stack with the active event rebound to it
func (c *MiddlewareContinuationContext) IsolatedWithScopeStack(stack *ScopeStack) (*MiddlewareContinuationContext, error) {
	snapshot := stack.Snapshot()
	isolated := *c
	isolated.scopeStack = snapshot
	if c.activeEvent != nil {
		rebound := rebindActiveEventToStack(*c.activeEvent, snapshot)
		isolated.activeEvent = &rebound
	}
	return &isolated, nil
}

// Returns the captured scope stack
func (c *MiddlewareContinuationContext) ScopeStack() *ScopeStack {
	return c.scopeStack
}

// Isolates against the stack the continuation caller sees, else the captured one
func (c *MiddlewareContinuationContext) isolatedForCurrentInvocation(ctx context.Context) (*MiddlewareContinuationContext, error) {
	visible := contextScopeStack(ctx)
	if visible == nil {
		if binding := currentDeliveryBinding(); binding != nil {
			visible = binding.stack
		} else {
			visible = c.scopeStack
		}
	}
	return c.IsolatedWithScopeStack(visible)
}

// Returns a context carrying every captured binding
func (c *MiddlewareContinuationContext) Apply(ctx context.Context) context.Context {
	ctx = WithScopeStack(ctx, c.scopeStack)
	if c.publicationBuffer != nil {
		ctx = WithNestedPublicationBuffer(ctx, c.publicationBuffer)
	}
	if c.activeEvent != nil {
		ctx = withAnchoredActiveEvent(ctx, *c.activeEvent, c.activeTraceContext)
	}
	if c.optimizationRecorder != nil {
		ctx = withLLMOptimizationRecorder(ctx, c.optimizationRecorder)
	}
	return ctx
}

// Returns a context that also exposes the active event to synchronous callback semantics
func (c *MiddlewareContinuationContext) ApplySync(ctx context.Context) context.Context {
	ctx = c.Apply(ctx)
	if c.activeEvent != nil {
		rebound := rebindActiveEventToStack(*c.activeEvent, c.scopeStack)
		ctx = withThreadActiveEvent(ctx, &rebound, c.activeTraceContext)
	} else {
		ctx = withThreadActiveEvent(ctx, nil, nil)
	}
	return ctx
}

func inactiveContinuationError() error {
	return invalidArgumentError("execution continuation is no longer active")
}

// Continuation capture that is revoked when the owning intercept returns
type middlewareContinuationLease struct {
	context *MiddlewareContinuationContext
	once    sync.Once
	revoked chan struct{}
}

func captureMiddlewareContinuationLease(ctx context.Context) *middlewareContinuationLease {
	return &middlewareContinuationLease{context: CaptureMiddlewareContinuationContext(ctx), revoked: make(chan struct{})}
}

func (l *middlewareContinuationLease) revoke() {
	l.once.Do(func() { close(l.revoked) })
}

func (l *middlewareContinuationLease) isRevoked() bool {
	select {
	case <-l.revoked:
		return true
	default:
		return false
	}
}

// Starts one continuation call on an isolated snapshot of the caller's stack
func (l *middlewareContinuationLease) begin(ctx context.Context) (*MiddlewareContinuationContext, error) {
	if l.isRevoked() {
		return nil, inactiveContinuationError()
	}
	return l.context.isolatedForCurrentInvocation(ctx)
}

type continuationResult[T any] struct {
	value    T
	err      error
	panicked any
}

// Runs the continuation callback and abandons it with an error once the lease is revoked
func invokeContinuation[T any](ctx context.Context, lease *middlewareContinuationLease, invocation *MiddlewareContinuationContext, callback func(context.Context) (T, error)) (T, error) {
	var zero T
	if lease.isRevoked() {
		return zero, inactiveContinuationError()
	}
	callCtx, cancel := context.WithCancel(invocation.Apply(ctx))
	defer cancel()
	done := make(chan continuationResult[T], 1)
	go func() {
		var result continuationResult[T]
		defer func() {
			if recovered := recover(); recovered != nil {
				result.panicked = recovered
			}
			done <- result
		}()
		result.value, result.err = callback(callCtx)
	}()
	select {
	case <-lease.revoked:
		select {
		case result := <-done:
			if result.panicked != nil {
				panic(result.panicked)
			}
		default:
		}
		return zero, inactiveContinuationError()
	case result := <-done:
		if result.panicked != nil {
			panic(result.panicked)
		}
		return result.value, result.err
	}
}
