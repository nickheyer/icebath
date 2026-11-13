package nemorelay

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

type (
	guardrail[F any]            = registryRecord[F]
	interceptRecord[F any]      = registryRecord[requestIntercept[F]]
	executionIntercept[F any]   = registryRecord[F]
	eventMetadataInjectorRecord = registryRecord[EventMetadataInjectorFn]
)

// Every middleware registry the runtime consults, shared by the global state and each scope
type middlewareRegistries struct {
	eventMetadataInjectors            sortedRegistry[EventMetadataInjectorFn]
	markSanitizeGuardrails            sortedRegistry[EventSanitizeFn]
	scopeSanitizeStartGuardrails      sortedRegistry[EventSanitizeFn]
	scopeSanitizeEndGuardrails        sortedRegistry[EventSanitizeFn]
	toolSanitizeRequestGuardrails     sortedRegistry[ToolSanitizeFn]
	toolSanitizeResponseGuardrails    sortedRegistry[ToolSanitizeFn]
	toolConditionalExecutionGuardrail sortedRegistry[ToolConditionalFn]
	toolRequestIntercepts             sortedRegistry[requestIntercept[ToolInterceptFn]]
	toolExecutionIntercepts           sortedRegistry[ToolExecutionFn]
	llmSanitizeRequestGuardrails      sortedRegistry[LLMSanitizeRequestFn]
	llmSanitizeResponseGuardrails     sortedRegistry[LLMSanitizeResponseFn]
	llmConditionalExecutionGuardrails sortedRegistry[LLMConditionalFn]
	llmRequestIntercepts              sortedRegistry[requestIntercept[LLMRequestInterceptFn]]
	llmExecutionIntercepts            sortedRegistry[LLMExecutionFn]
	llmStreamExecutionIntercepts      sortedRegistry[LLMStreamExecutionFn]
	eventSubscribers                  map[string]EventSubscriberFn
}

// Registrations owned by one open scope and removed when it closes
type scopeLocalRegistries struct {
	middlewareRegistries
}

func newScopeLocalRegistries() *scopeLocalRegistries {
	return &scopeLocalRegistries{middlewareRegistries{eventSubscribers: map[string]EventSubscriberFn{}}}
}

func (r *scopeLocalRegistries) clone() *scopeLocalRegistries {
	cloned := *r
	cloned.eventSubscribers = maps.Clone(r.eventSubscribers)
	if cloned.eventSubscribers == nil {
		cloned.eventSubscribers = map[string]EventSubscriberFn{}
	}
	return &cloned
}

// Process wide runtime registrations guarded by one lock
type runtimeState struct {
	mu sync.RWMutex
	middlewareRegistries
	observabilityFullPayloadsEnabled bool
}

var globalState = &runtimeState{middlewareRegistries: middlewareRegistries{eventSubscribers: map[string]EventSubscriberFn{}}}

// Reads a consistent copy of part of the global state
func readGlobal[T any](read func(*runtimeState) T) T {
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	return read(globalState)
}

// Mutates the global state under its write lock
func writeGlobal[T any](write func(*runtimeState) T) T {
	globalState.mu.Lock()
	defer globalState.mu.Unlock()
	return write(globalState)
}

// Turns full observability payloads on or off for LLM start events
func SetObservabilityFullPayloadsEnabled(enabled bool) {
	writeGlobal(func(state *runtimeState) struct{} {
		state.observabilityFullPayloadsEnabled = enabled
		return struct{}{}
	})
}

func observabilityFullPayloadsEnabled() bool {
	return readGlobal(func(state *runtimeState) bool { return state.observabilityFullPayloadsEnabled })
}

// Returns global subscribers enabled by conditional middleware guardrails followed by scope-local ones
func collectEventSubscribers(global map[string]EventSubscriberFn, scopeLocal []EventSubscriberFn) []EventSubscriberFn {
	subscribers := make([]EventSubscriberFn, 0, len(global)+len(scopeLocal))
	for _, name := range slices.Sorted(maps.Keys(global)) {
		if runtimeRegistrationIsEnabled(types.RuntimeRegistrationKindSubscriber, name) {
			subscribers = append(subscribers, global[name])
		}
	}
	return append(subscribers, scopeLocal...)
}

func globalSubscribersSnapshot() map[string]EventSubscriberFn {
	return readGlobal(func(state *runtimeState) map[string]EventSubscriberFn { return maps.Clone(state.eventSubscribers) })
}

// Snapshots the subscribers an emission delivers to
func snapshotEventSubscribers(scopeLocal []EventSubscriberFn) []EventSubscriberFn {
	return collectEventSubscribers(globalSubscribersSnapshot(), scopeLocal)
}

// Merges enabled global guardrails with scope-local ones ordered by priority
func mergeGuardrailEntries[F any](global sortedRegistry[F], scopeLocal []sortedRegistry[F], kind types.RuntimeRegistrationKind) []registryRecord[F] {
	all := make([]registryRecord[F], 0)
	for _, entry := range global.sortedValues() {
		if runtimeRegistrationIsEnabled(kind, entry.name) {
			all = append(all, entry)
		}
	}
	for _, registry := range scopeLocal {
		all = append(all, registry.sortedValues()...)
	}
	slices.SortStableFunc(all, func(left, right registryRecord[F]) int { return left.priority - right.priority })
	return all
}

// Merges enabled global injectors with scope-local ones ordered by priority then name
func mergeEventMetadataInjectorEntries(global sortedRegistry[EventMetadataInjectorFn], scopeLocal []sortedRegistry[EventMetadataInjectorFn]) []eventMetadataInjectorRecord {
	all := make([]eventMetadataInjectorRecord, 0)
	for _, entry := range global.sortedValues() {
		if runtimeRegistrationIsEnabled(types.RuntimeRegistrationKindEventMetadataInjector, entry.name) {
			all = append(all, entry)
		}
	}
	for _, registry := range scopeLocal {
		all = append(all, registry.sortedValues()...)
	}
	slices.SortStableFunc(all, func(left, right eventMetadataInjectorRecord) int {
		if left.priority != right.priority {
			return left.priority - right.priority
		}
		switch {
		case left.name < right.name:
			return -1
		case left.name > right.name:
			return 1
		}
		return 0
	})
	return all
}

// Snapshots the guardrails of one kind visible from a scope stack
func snapshotGuardrails[F any](stack *ScopeStack, field func(*middlewareRegistries) *sortedRegistry[F], kind types.RuntimeRegistrationKind) []registryRecord[F] {
	scopeLocal := snapshotScopeLocal(stack, func(local *scopeLocalRegistries) *sortedRegistry[F] { return field(&local.middlewareRegistries) })
	global := readGlobal(func(state *runtimeState) sortedRegistry[F] { return *field(&state.middlewareRegistries) })
	return mergeGuardrailEntries(global, scopeLocal, kind)
}

// Snapshots the metadata injectors visible from a scope stack
func snapshotEventMetadataInjectors(stack *ScopeStack) []eventMetadataInjectorRecord {
	scopeLocal := snapshotScopeLocal(stack, func(local *scopeLocalRegistries) *sortedRegistry[EventMetadataInjectorFn] {
		return &local.eventMetadataInjectors
	})
	global := readGlobal(func(state *runtimeState) sortedRegistry[EventMetadataInjectorFn] { return state.eventMetadataInjectors })
	return mergeEventMetadataInjectorEntries(global, scopeLocal)
}

// Snapshots the event sanitizers that apply to an event emitted from a scope stack
func snapshotEventSanitizers(event *types.Event, stack *ScopeStack) []registryRecord[EventSanitizeFn] {
	switch {
	case event.Mark != nil:
		return snapshotGuardrails(stack, func(r *middlewareRegistries) *sortedRegistry[EventSanitizeFn] { return &r.markSanitizeGuardrails }, types.RuntimeRegistrationKindMarkSanitizeGuardrail)
	case event.Scope != nil && event.Scope.ScopeCategory == types.ScopeCategoryStart:
		return snapshotGuardrails(stack, func(r *middlewareRegistries) *sortedRegistry[EventSanitizeFn] { return &r.scopeSanitizeStartGuardrails }, types.RuntimeRegistrationKindScopeSanitizeStartGuardrail)
	default:
		return snapshotGuardrails(stack, func(r *middlewareRegistries) *sortedRegistry[EventSanitizeFn] { return &r.scopeSanitizeEndGuardrails }, types.RuntimeRegistrationKindScopeSanitizeEndGuardrail)
	}
}

// Queues an event with the sanitizers of its scope stack
func queueSanitizedEvent(ctx context.Context, event *types.Event, subscribers []EventSubscriberFn, stack *ScopeStack) bool {
	return dispatchSanitizedEvent(ctx, event, snapshotEventSanitizers(event, stack), subscribers, stack)
}

// Collects registry callables of one execution intercept kind ordered by priority
func mergeExecutionIntercepts[F any](global sortedRegistry[F], scopeLocal []sortedRegistry[F], kind types.RuntimeRegistrationKind) []F {
	entries := mergeGuardrailEntries(global, scopeLocal, kind)
	callables := make([]F, len(entries))
	for index, entry := range entries {
		callables[index] = entry.payload
	}
	return callables
}

// Snapshots the execution intercepts of one kind visible from a scope stack
func snapshotExecutionIntercepts[F any](stack *ScopeStack, field func(*middlewareRegistries) *sortedRegistry[F], kind types.RuntimeRegistrationKind) []F {
	scopeLocal := snapshotScopeLocal(stack, func(local *scopeLocalRegistries) *sortedRegistry[F] { return field(&local.middlewareRegistries) })
	global := readGlobal(func(state *runtimeState) sortedRegistry[F] { return *field(&state.middlewareRegistries) })
	return mergeExecutionIntercepts(global, scopeLocal, kind)
}
