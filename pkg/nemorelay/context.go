package nemorelay

import (
	"bytes"
	"context"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"weak"

	"github.com/google/uuid"
)

type contextKey int

const (
	scopeStackKey contextKey = iota
	activeEventKey
	threadActiveEventKey
	publicationBufferKey
	publicationLineageKey
	optimizationRecorderKey
)

var (
	defaultStackOnce sync.Once
	defaultStack     *ScopeStack
)

// Returns the process wide stack used when no context or dispatcher stack is bound
func processDefaultScopeStack() *ScopeStack {
	defaultStackOnce.Do(func() { defaultStack = NewScopeStack() })
	return defaultStack
}

// Binds a scope stack to the context for every Relay call made with it
func WithScopeStack(ctx context.Context, stack *ScopeStack) context.Context {
	return context.WithValue(ctx, scopeStackKey, stack)
}

// Returns the context stack, the dispatcher delivery stack, or the process default stack
func CurrentScopeStack(ctx context.Context) *ScopeStack {
	if stack := contextScopeStack(ctx); stack != nil {
		return stack
	}
	if binding := currentDeliveryBinding(); binding != nil {
		return binding.stack
	}
	return processDefaultScopeStack()
}

// Returns only an explicitly bound stack, never the process default
func contextScopeStack(ctx context.Context) *ScopeStack {
	if ctx == nil {
		return nil
	}
	stack, _ := ctx.Value(scopeStackKey).(*ScopeStack)
	return stack
}

// Reports whether a stack was explicitly bound for the caller
func ScopeStackActive(ctx context.Context) bool {
	return contextScopeStack(ctx) != nil || currentDeliveryBinding() != nil
}

// Returns the explicitly bound stack or an error when none is bound
func PropagateScopeToThread(ctx context.Context) (*ScopeStack, error) {
	if !ScopeStackActive(ctx) {
		return nil, internalError("no active scope stack in current context; call create_scope_stack() and set_thread_scope_stack() first")
	}
	return CurrentScopeStack(ctx), nil
}

// Returns a snapshot of the explicitly bound stack when one exists
func CurrentScopeStackSnapshot(ctx context.Context) (*ScopeStackSnapshot, error) {
	stack := contextScopeStack(ctx)
	if stack == nil {
		if binding := currentDeliveryBinding(); binding != nil {
			stack = binding.stack
		}
	}
	if stack == nil {
		return nil, nil
	}
	snapshot := ScopeStackSnapshotOf(stack)
	return &snapshot, nil
}

// Event anchored to the stack and scope that were current when it became active
type anchoredActiveEvent struct {
	eventUUID       uuid.UUID
	scopeStack      weak.Pointer[ScopeStack]
	anchorScopeUUID uuid.UUID
}

// Task scoped active event and its trace context
type activeEventBinding struct {
	event        anchoredActiveEvent
	traceContext *W3CTraceContext
}

// Caller scoped active event that is revalidated against the current stack top
type threadActiveEventBinding struct {
	mu           sync.Mutex
	event        *anchoredActiveEvent
	traceContext *W3CTraceContext
}

func activeEventFromContext(ctx context.Context) *activeEventBinding {
	if ctx == nil {
		return nil
	}
	binding, _ := ctx.Value(activeEventKey).(*activeEventBinding)
	return binding
}

func threadActiveEventFromContext(ctx context.Context) *threadActiveEventBinding {
	if ctx != nil {
		if binding, ok := ctx.Value(threadActiveEventKey).(*threadActiveEventBinding); ok {
			return binding
		}
	}
	if delivery := currentDeliveryBinding(); delivery != nil {
		return delivery.activeEvent
	}
	return nil
}

func scopeStackIdentityAndAnchor(ctx context.Context) (weak.Pointer[ScopeStack], uuid.UUID) {
	stack := CurrentScopeStack(ctx)
	return weak.Make(stack), stack.Top().UUID
}

// Marks an event as active for work run with the returned context
func WithActiveEventUUID(ctx context.Context, id uuid.UUID) context.Context {
	return withActiveEventTraceContext(ctx, id, nil)
}

func withActiveEventTraceContext(ctx context.Context, id uuid.UUID, traceContext *W3CTraceContext) context.Context {
	stack, anchor := scopeStackIdentityAndAnchor(ctx)
	return withAnchoredActiveEvent(ctx, anchoredActiveEvent{eventUUID: id, scopeStack: stack, anchorScopeUUID: anchor}, traceContext)
}

func withAnchoredActiveEvent(ctx context.Context, event anchoredActiveEvent, traceContext *W3CTraceContext) context.Context {
	return context.WithValue(ctx, activeEventKey, &activeEventBinding{event: event, traceContext: traceContext})
}

// Sets the caller scoped active event the way a synchronous callback thread sees it
func withThreadActiveEvent(ctx context.Context, event *anchoredActiveEvent, traceContext *W3CTraceContext) context.Context {
	return context.WithValue(ctx, threadActiveEventKey, &threadActiveEventBinding{event: event, traceContext: traceContext})
}

func captureAnchoredActiveEvent(ctx context.Context) *anchoredActiveEvent {
	if binding := activeEventFromContext(ctx); binding != nil {
		event := binding.event
		return &event
	}
	return threadActiveEvent(ctx)
}

// Rebinds an active event to another stack, keeping its anchor when still present
func rebindActiveEventToStack(event anchoredActiveEvent, stack *ScopeStack) anchoredActiveEvent {
	anchor := event.anchorScopeUUID
	if _, found := stack.Find(anchor); !found {
		anchor = stack.Top().UUID
	}
	return anchoredActiveEvent{eventUUID: event.eventUUID, scopeStack: weak.Make(stack), anchorScopeUUID: anchor}
}

func activeEventUUID(ctx context.Context) *uuid.UUID {
	if binding := activeEventFromContext(ctx); binding != nil {
		id := binding.event.eventUUID
		return &id
	}
	return threadActiveEventUUID(ctx)
}

func activeEventTraceContext(ctx context.Context) *W3CTraceContext {
	if binding := activeEventFromContext(ctx); binding != nil {
		return binding.traceContext
	}
	if threadActiveEvent(ctx) == nil {
		return nil
	}
	binding := threadActiveEventFromContext(ctx)
	binding.mu.Lock()
	defer binding.mu.Unlock()
	return binding.traceContext
}

// Returns the caller scoped active event while it still belongs to the current stack top
func threadActiveEvent(ctx context.Context) *anchoredActiveEvent {
	binding := threadActiveEventFromContext(ctx)
	if binding == nil {
		return nil
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	if binding.event == nil {
		return nil
	}
	event := *binding.event
	stack := CurrentScopeStack(ctx)
	eventStack := event.scopeStack.Value()
	if eventStack == nil || eventStack != stack {
		return nil
	}
	top := stack.Top().UUID
	if event.anchorScopeUUID != top {
		if _, found := stack.Find(event.anchorScopeUUID); found {
			return nil
		}
		event.anchorScopeUUID = top
		rebound := event
		binding.event = &rebound
	}
	return &event
}

func threadActiveEventUUID(ctx context.Context) *uuid.UUID {
	if event := threadActiveEvent(ctx); event != nil {
		id := event.eventUUID
		return &id
	}
	return nil
}

// Copies the active event into the caller scope rebound to the given stack
func SyncThreadActiveEventForStack(ctx context.Context, stack *ScopeStack) context.Context {
	event := captureAnchoredActiveEvent(ctx)
	if event == nil {
		return withThreadActiveEvent(ctx, nil, nil)
	}
	rebound := rebindActiveEventToStack(*event, stack)
	return withThreadActiveEvent(ctx, &rebound, activeEventTraceContext(ctx))
}

// Binds a stack the way a callback thread binds it, dropping an active event of another stack
func SetThreadScopeStack(ctx context.Context, stack *ScopeStack) context.Context {
	if CurrentScopeStack(ctx) != stack {
		ctx = withThreadActiveEvent(ctx, nil, nil)
	}
	return WithScopeStack(ctx, stack)
}

// Dispatcher goroutine state that stands in for the delivery thread bindings
type deliveryBinding struct {
	stack       *ScopeStack
	lineage     *publicationLineage
	activeEvent *threadActiveEventBinding
}

var (
	dispatcherGoroutine atomic.Uint64
	activeDelivery      atomic.Pointer[deliveryBinding]
)

// Returns the delivery binding when called from the dispatcher goroutine during delivery
func currentDeliveryBinding() *deliveryBinding {
	binding := activeDelivery.Load()
	if binding == nil {
		return nil
	}
	if goroutineID() != dispatcherGoroutine.Load() {
		return nil
	}
	return binding
}

// Reports whether the caller runs on the dispatcher goroutine
func onDispatcherGoroutine() bool {
	id := dispatcherGoroutine.Load()
	return id != 0 && goroutineID() == id
}

// Parses the running goroutine id from its stack header
func goroutineID() uint64 {
	var buffer [64]byte
	header := buffer[:runtime.Stack(buffer[:], false)]
	header = bytes.TrimPrefix(header, []byte("goroutine "))
	if end := bytes.IndexByte(header, ' '); end >= 0 {
		header = header[:end]
	}
	id, _ := strconv.ParseUint(string(header), 10, 64)
	return id
}
