package nemorelay

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

type ScopeStackEntry = types.ScopeStackEntry
type ScopeStackSnapshot = types.ScopeStackSnapshot

// Ordered stack of open scopes with their scope-local registries and propagation identity
type ScopeStack struct {
	mu                    sync.RWMutex
	stack                 []ScopeHandle
	scopeRegistries       map[uuid.UUID]*scopeLocalRegistries
	freshAgents           map[uuid.UUID]struct{}
	propagatedParentUUID  *uuid.UUID
	propagatedRootUUID    *uuid.UUID
	propagatedTraceparent *string
	propagatedTracestate  *string
	rootlessPropagation   bool
}

// Creates a stack holding only a fresh implicit root agent scope
func NewScopeStack() *ScopeStack {
	root := newScopeHandle("root", types.ScopeTypeAgent)
	return &ScopeStack{
		stack:           []ScopeHandle{root},
		scopeRegistries: map[uuid.UUID]*scopeLocalRegistries{},
		freshAgents:     map[uuid.UUID]struct{}{root.UUID: {}},
	}
}

// Serializable parentage handed to another process or task to continue a trace
type PropagationContext struct {
	Version     uint16     `json:"version"`
	RootUUID    *uuid.UUID `json:"root_uuid,omitempty"`
	ParentUUID  uuid.UUID  `json:"parent_uuid"`
	Traceparent *string    `json:"traceparent,omitempty"`
	Tracestate  *string    `json:"tracestate,omitempty"`
}

const PropagationContextVersion uint16 = 1

// Serializes the validated and normalized context
func (c PropagationContext) ToJSON() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(c.Normalized())
	if err != nil {
		return "", internalError(err.Error())
	}
	return string(encoded), nil
}

// Returns the context traceparent or the Relay-derived one for its root and parent
func (c PropagationContext) ToTraceparent() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	normalized := c.Normalized()
	if normalized.Traceparent != nil {
		return *normalized.Traceparent, nil
	}
	if normalized.RootUUID == nil {
		return "", invalidArgumentError("rootless propagation context cannot be converted to traceparent")
	}
	return FormatTraceparent(*normalized.RootUUID, c.ParentUUID), nil
}

// Checks the version and that every identifier is usable
func (c PropagationContext) Validate() error {
	if c.Version != PropagationContextVersion {
		return invalidArgumentError(fmt.Sprintf("unsupported propagation context version %d; expected %d", c.Version, PropagationContextVersion))
	}
	if !isUsableRelayIdentifier(c.ParentUUID) {
		return invalidArgumentError("propagation context parent_uuid is not a usable Relay identifier")
	}
	if c.RootUUID != nil && !isUsableRelayIdentifier(*c.RootUUID) {
		return invalidArgumentError("propagation context root_uuid is not a usable Relay identifier")
	}
	return nil
}

// Returns the context with canonical W3C headers and invalid ones removed
func (c PropagationContext) Normalized() PropagationContext {
	c.Traceparent, c.Tracestate, _ = normalizeW3CHeaders(c.Traceparent, c.Tracestate)
	c.RootUUID = cloneUUIDPtr(c.RootUUID)
	return c
}

// Builds a stack continuing the trace and parentage of a propagation context
func NewScopeStackFromPropagation(propagation PropagationContext) (*ScopeStack, error) {
	if err := propagation.Validate(); err != nil {
		return nil, err
	}
	normalized := propagation.Normalized()
	var root ScopeHandle
	var parent *ScopeHandle
	if normalized.RootUUID != nil {
		root = newScopeHandle("propagated-root", types.ScopeTypeAgent)
		root.UUID = *normalized.RootUUID
		if *normalized.RootUUID != normalized.ParentUUID {
			handle := newScopeHandle("propagated-parent", types.ScopeTypeUnknown)
			handle.UUID = normalized.ParentUUID
			handle.ParentUUID = cloneUUIDPtr(normalized.RootUUID)
			parent = &handle
		}
	} else {
		root = newScopeHandle("propagated-root", types.ScopeTypeAgent)
		root.UUID = normalized.ParentUUID
	}
	stack := []ScopeHandle{root}
	if parent != nil {
		stack = append(stack, *parent)
	}
	parentUUID := normalized.ParentUUID
	return &ScopeStack{
		stack:                 stack,
		scopeRegistries:       map[uuid.UUID]*scopeLocalRegistries{},
		freshAgents:           map[uuid.UUID]struct{}{root.UUID: {}},
		propagatedParentUUID:  &parentUUID,
		propagatedRootUUID:    cloneUUIDPtr(normalized.RootUUID),
		propagatedTraceparent: normalized.Traceparent,
		propagatedTracestate:  normalized.Tracestate,
		rootlessPropagation:   normalized.RootUUID == nil,
	}, nil
}

// Creates a new stack continuing the caller's current trace position
func ForkScopeStack(ctx context.Context) (*ScopeStack, error) {
	propagation, err := CapturePropagationContext(ctx)
	if err != nil {
		return nil, err
	}
	return NewScopeStackFromPropagation(propagation)
}

// Pushes a scope and marks new agent scopes fresh
func (s *ScopeStack) Push(handle ScopeHandle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushLocked(handle)
}

func (s *ScopeStack) pushLocked(handle ScopeHandle) {
	if handle.ScopeType == types.ScopeTypeAgent {
		s.freshAgents[handle.UUID] = struct{}{}
	}
	s.stack = append(s.stack, handle.clone())
}

// Returns a copy of the innermost scope
func (s *ScopeStack) Top() ScopeHandle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stack[len(s.stack)-1].clone()
}

func (s *ScopeStack) topLocked() ScopeHandle {
	return s.stack[len(s.stack)-1]
}

// Returns the uuid of the implicit root scope
func (s *ScopeStack) RootUUID() uuid.UUID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stack[0].UUID
}

// Returns copies of every scope from root to top
func (s *ScopeStack) Scopes() []ScopeHandle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	scopes := make([]ScopeHandle, len(s.stack))
	for index, handle := range s.stack {
		scopes[index] = handle.clone()
	}
	return scopes
}

// Returns a copy of the scope with the given uuid
func (s *ScopeStack) Find(id uuid.UUID) (ScopeHandle, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if handle, ok := s.findLocked(id); ok {
		return handle.clone(), true
	}
	return ScopeHandle{}, false
}

func (s *ScopeStack) findLocked(id uuid.UUID) (ScopeHandle, bool) {
	for _, handle := range s.stack {
		if handle.UUID == id {
			return handle, true
		}
	}
	return ScopeHandle{}, false
}

// Reports whether the uuid is the imported propagation parent
func (s *ScopeStack) IsPropagatedParent(id uuid.UUID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.propagatedParentUUID != nil && *s.propagatedParentUUID == id
}

// Removes the top scope, refusing the root and scopes that are not on top
func (s *ScopeStack) Remove(id uuid.UUID) (ScopeHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeLocked(id)
}

func (s *ScopeStack) removeLocked(id uuid.UUID) (ScopeHandle, error) {
	top := s.stack[len(s.stack)-1]
	if top.UUID == id {
		if len(s.stack) == 1 {
			return ScopeHandle{}, invalidArgumentError("root scope cannot be removed")
		}
		delete(s.scopeRegistries, id)
		delete(s.freshAgents, id)
		s.stack = s.stack[:len(s.stack)-1]
		return top, nil
	}
	if _, found := s.findLocked(id); found {
		return ScopeHandle{}, invalidArgumentError("scope handle is not at the top of the stack")
	}
	return ScopeHandle{}, notFoundError("scope handle not found")
}

// Copies the whole stack including registries and propagation identity
func (s *ScopeStack) Snapshot() *ScopeStack {
	s.mu.RLock()
	defer s.mu.RUnlock()
	registries := make(map[uuid.UUID]*scopeLocalRegistries, len(s.scopeRegistries))
	for id, local := range s.scopeRegistries {
		registries[id] = local.clone()
	}
	snapshot := s.propagationCopyLocked()
	snapshot.stack = cloneHandles(s.stack)
	snapshot.scopeRegistries = registries
	snapshot.freshAgents = maps.Clone(s.freshAgents)
	return snapshot
}

// Copies only the implicit root with its registries and propagation identity
func (s *ScopeStack) RootOnlySnapshot() *ScopeStack {
	s.mu.RLock()
	defer s.mu.RUnlock()
	root := s.stack[0]
	snapshot := s.propagationCopyLocked()
	snapshot.stack = []ScopeHandle{root.clone()}
	snapshot.scopeRegistries = map[uuid.UUID]*scopeLocalRegistries{}
	if local, ok := s.scopeRegistries[root.UUID]; ok {
		snapshot.scopeRegistries[root.UUID] = local.clone()
	}
	snapshot.freshAgents = map[uuid.UUID]struct{}{}
	if _, fresh := s.freshAgents[root.UUID]; fresh {
		snapshot.freshAgents[root.UUID] = struct{}{}
	}
	return snapshot
}

// Copies the stack from root through the given scope when it is open
func (s *ScopeStack) SnapshotThroughScope(id uuid.UUID) (*ScopeStack, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	end := slices.IndexFunc(s.stack, func(handle ScopeHandle) bool { return handle.UUID == id })
	if end < 0 {
		return nil, false
	}
	visible := map[uuid.UUID]struct{}{}
	for _, handle := range s.stack[:end+1] {
		visible[handle.UUID] = struct{}{}
	}
	snapshot := s.propagationCopyLocked()
	snapshot.stack = cloneHandles(s.stack[:end+1])
	snapshot.scopeRegistries = map[uuid.UUID]*scopeLocalRegistries{}
	for scopeID, local := range s.scopeRegistries {
		if _, ok := visible[scopeID]; ok {
			snapshot.scopeRegistries[scopeID] = local.clone()
		}
	}
	snapshot.freshAgents = map[uuid.UUID]struct{}{}
	for scopeID := range s.freshAgents {
		if _, ok := visible[scopeID]; ok {
			snapshot.freshAgents[scopeID] = struct{}{}
		}
	}
	return snapshot, true
}

func (s *ScopeStack) propagationCopyLocked() *ScopeStack {
	return &ScopeStack{
		propagatedParentUUID:  cloneUUIDPtr(s.propagatedParentUUID),
		propagatedRootUUID:    cloneUUIDPtr(s.propagatedRootUUID),
		propagatedTraceparent: cloneStringPtr(s.propagatedTraceparent),
		propagatedTracestate:  cloneStringPtr(s.propagatedTracestate),
		rootlessPropagation:   s.rootlessPropagation,
	}
}

// Lists each scope uuid and type from root to top
func ScopeStackSnapshotOf(stack *ScopeStack) ScopeStackSnapshot {
	stack.mu.RLock()
	defer stack.mu.RUnlock()
	scopes := make([]ScopeStackEntry, 0, len(stack.stack))
	for _, handle := range stack.stack {
		scopes = append(scopes, ScopeStackEntry{ScopeID: handle.UUID, ScopeType: handle.ScopeType})
	}
	return ScopeStackSnapshot{Scopes: scopes}
}

// Returns the trace root uuid stamped on events emitted from this stack
func (s *ScopeStack) eventPropagationRootUUID() *uuid.UUID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.propagatedRootUUID != nil {
		return cloneUUIDPtr(s.propagatedRootUUID)
	}
	for _, handle := range s.stack[1:] {
		if handle.ScopeType == types.ScopeTypeAgent && isUsableRelayIdentifier(handle.UUID) {
			id := handle.UUID
			return &id
		}
	}
	if !s.rootlessPropagation {
		id := s.stack[0].UUID
		return &id
	}
	return nil
}

func (s *ScopeStack) eventPropagationParentUUID() *uuid.UUID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneUUIDPtr(s.propagatedParentUUID)
}

// Returns the W3C headers stamped on an event with the given parent
func (s *ScopeStack) eventW3CHeaders(parentUUID *uuid.UUID) (*string, *string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.eventW3CHeadersLocked(parentUUID)
}

func (s *ScopeStack) eventW3CHeadersLocked(parentUUID *uuid.UUID) (*string, *string) {
	if parentUUID == nil {
		return nil, nil
	}
	if s.propagatedParentUUID != nil && *s.propagatedParentUUID == *parentUUID {
		if s.propagatedTraceparent != nil {
			return cloneStringPtr(s.propagatedTraceparent), cloneStringPtr(s.propagatedTracestate)
		}
		if s.propagatedRootUUID == nil {
			return nil, nil
		}
	}
	return s.w3cHeadersForSpanLocked(*parentUUID, *parentUUID)
}

// Finds the agent scope that owns work parented at the given uuid
func (s *ScopeStack) owningAgentUUIDLocked(parentUUID *uuid.UUID) uuid.UUID {
	searchEnd := len(s.stack)
	if parentUUID != nil {
		if index := slices.IndexFunc(s.stack, func(handle ScopeHandle) bool { return handle.UUID == *parentUUID }); index >= 0 {
			searchEnd = index + 1
		}
	}
	for index := searchEnd - 1; index >= 0; index-- {
		if s.stack[index].ScopeType == types.ScopeTypeAgent {
			return s.stack[index].UUID
		}
	}
	panic("scope stack should always contain an owning agent")
}

// Consumes the owning agent freshness flag and reports whether it was set
func (s *ScopeStack) takeAgentFreshness(parentUUID *uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := s.owningAgentUUIDLocked(parentUUID)
	_, fresh := s.freshAgents[owner]
	delete(s.freshAgents, owner)
	return fresh
}

func (s *ScopeStack) markAgentFresh(parentUUID *uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.freshAgents[s.owningAgentUUIDLocked(parentUUID)] = struct{}{}
}

// Runs a mutation against the registries of an open scope
func (s *ScopeStack) withLocalRegistries(id uuid.UUID, mutate func(*scopeLocalRegistries) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.findLocked(id); !found {
		return notFoundError(fmt.Sprintf("scope %s not found", id))
	}
	local, ok := s.scopeRegistries[id]
	if !ok {
		local = newScopeLocalRegistries()
		s.scopeRegistries[id] = local
	}
	return mutate(local)
}

// Same as withLocalRegistries but fails instead of waiting for a held lock
func (s *ScopeStack) tryWithLocalRegistries(id uuid.UUID, mutate func(*scopeLocalRegistries) error) error {
	if !s.mu.TryLock() {
		return internalError("scope stack lock unavailable: try_lock failed because the operation would block")
	}
	defer s.mu.Unlock()
	if _, found := s.findLocked(id); !found {
		return notFoundError(fmt.Sprintf("scope %s not found", id))
	}
	local, ok := s.scopeRegistries[id]
	if !ok {
		local = newScopeLocalRegistries()
		s.scopeRegistries[id] = local
	}
	return mutate(local)
}

// Copies one registry from every open scope in stack order
func snapshotScopeLocal[T any](s *ScopeStack, field func(*scopeLocalRegistries) *sortedRegistry[T]) []sortedRegistry[T] {
	s.mu.RLock()
	defer s.mu.RUnlock()
	registries := make([]sortedRegistry[T], 0)
	for _, handle := range s.stack {
		if local, ok := s.scopeRegistries[handle.UUID]; ok {
			registries = append(registries, *field(local))
		}
	}
	return registries
}

// Collects scope-local subscribers from every open scope in stack order
func (s *ScopeStack) collectScopeLocalSubscribers() []EventSubscriberFn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	subscribers := make([]EventSubscriberFn, 0)
	for _, handle := range s.stack {
		local, ok := s.scopeRegistries[handle.UUID]
		if !ok {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(local.eventSubscribers)) {
			subscribers = append(subscribers, local.eventSubscribers[name])
		}
	}
	return subscribers
}

// Returns the span context imported for the propagated parent uuid
func (s *ScopeStack) propagatedSpanContextLocked(id uuid.UUID) (spanContext, bool) {
	if s.propagatedParentUUID == nil || *s.propagatedParentUUID != id {
		return spanContext{}, false
	}
	if context, ok := w3cSpanContext(s.propagatedTraceparent, s.propagatedTracestate); ok {
		return context, true
	}
	if s.propagatedRootUUID == nil {
		return spanContext{}, false
	}
	if !isUsableRelayIdentifier(*s.propagatedRootUUID) || !isUsableRelayIdentifier(id) {
		return spanContext{}, false
	}
	return spanContext{traceID: RelayTraceID(*s.propagatedRootUUID), spanID: RelaySpanID(id), flags: traceFlagSample, remote: true}, true
}

func (s *ScopeStack) localSpanContextLocked(id uuid.UUID) (spanContext, bool) {
	return s.localSpanContextInnerLocked(id, len(s.stack), map[uuid.UUID]struct{}{})
}

// Derives a scope span context from its causal parent chain below the given index
func (s *ScopeStack) localSpanContextInnerLocked(id uuid.UUID, beforeIndex int, visiting map[uuid.UUID]struct{}) (spanContext, bool) {
	if !isUsableRelayIdentifier(id) {
		return spanContext{}, false
	}
	if _, seen := visiting[id]; seen {
		return spanContext{}, false
	}
	visiting[id] = struct{}{}
	if context, ok := s.propagatedSpanContextLocked(id); ok {
		delete(visiting, id)
		return context, true
	}
	index := -1
	limit := min(beforeIndex, len(s.stack))
	for candidate := limit - 1; candidate >= 1; candidate-- {
		if s.stack[candidate].UUID == id {
			index = candidate
			break
		}
	}
	if index < 0 {
		return spanContext{}, false
	}
	scope := s.stack[index]
	stackParentContext := func() (spanContext, bool) {
		if index-1 < 0 {
			return spanContext{}, false
		}
		return s.parentSpanContextInnerLocked(s.stack[index-1].UUID, index, visiting)
	}
	var parent spanContext
	var hasParent bool
	if scope.ParentUUID != nil {
		parentUUID := *scope.ParentUUID
		parent, hasParent = s.parentSpanContextInnerLocked(parentUUID, index, visiting)
		if !hasParent {
			if _, found := s.findLocked(parentUUID); isUsableRelayIdentifier(parentUUID) && !found {
				parent, hasParent = stackParentContext()
			}
		}
	} else {
		parent, hasParent = stackParentContext()
	}
	var context spanContext
	if hasParent {
		context = spanContext{traceID: parent.traceID, spanID: RelaySpanID(id), flags: parent.flags, traceState: parent.traceState}
	} else {
		context = spanContext{traceID: RelayTraceID(id), spanID: RelaySpanID(id), flags: traceFlagSample}
	}
	delete(visiting, id)
	if !context.valid() {
		return spanContext{}, false
	}
	return context, true
}

func (s *ScopeStack) parentSpanContextInnerLocked(id uuid.UUID, beforeIndex int, visiting map[uuid.UUID]struct{}) (spanContext, bool) {
	if context, ok := s.propagatedSpanContextLocked(id); ok {
		return context, true
	}
	return s.localSpanContextInnerLocked(id, beforeIndex, visiting)
}

// Builds the W3C headers for a span parented by the causal parent scope
func (s *ScopeStack) w3cHeadersForSpanLocked(spanUUID, causalParentUUID uuid.UUID) (*string, *string) {
	if spanUUID == causalParentUUID && s.propagatedParentUUID != nil && *s.propagatedParentUUID == causalParentUUID && s.propagatedTraceparent != nil {
		return cloneStringPtr(s.propagatedTraceparent), cloneStringPtr(s.propagatedTracestate)
	}
	parent, ok := s.localSpanContextLocked(causalParentUUID)
	if !ok {
		if _, found := s.findLocked(causalParentUUID); isUsableRelayIdentifier(causalParentUUID) && !found {
			parent, ok = s.localSpanContextLocked(s.topLocked().UUID)
		}
	}
	if !ok {
		return nil, nil
	}
	return w3cHeadersForParent(parent, spanUUID)
}

func (s *ScopeStack) w3cHeadersForSpan(spanUUID, causalParentUUID uuid.UUID) (*string, *string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.w3cHeadersForSpanLocked(spanUUID, causalParentUUID)
}

// Chooses the headers for a captured context from the active event or the stack
func capturedW3CHeaders(ctx context.Context, stack *ScopeStack, activeUUID *uuid.UUID, parentUUID uuid.UUID) (*string, *string) {
	if active := activeEventTraceContext(ctx); active != nil {
		traceparent := active.traceparent
		return &traceparent, cloneStringPtr(active.tracestate)
	}
	if activeUUID != nil {
		return stack.w3cHeadersForSpan(parentUUID, stack.Top().UUID)
	}
	return stack.eventW3CHeaders(&parentUUID)
}

func capturedImportedW3CHeaders(stack *ScopeStack, parentUUID uuid.UUID) (*string, *string) {
	stack.mu.RLock()
	defer stack.mu.RUnlock()
	if stack.propagatedParentUUID != nil && *stack.propagatedParentUUID == parentUUID {
		return cloneStringPtr(stack.propagatedTraceparent), cloneStringPtr(stack.propagatedTracestate)
	}
	parent, ok := w3cSpanContext(stack.propagatedTraceparent, stack.propagatedTracestate)
	if !ok {
		return nil, nil
	}
	return w3cHeadersForParent(parent, parentUUID)
}

// Captures the current trace position including its root for another task or process
func CapturePropagationContext(ctx context.Context) (PropagationContext, error) {
	activeUUID := activeEventUUID(ctx)
	stack := CurrentScopeStack(ctx)
	parentUUID := stack.Top().UUID
	if activeUUID != nil {
		parentUUID = *activeUUID
	}
	rootUUID := stack.eventPropagationRootUUID()
	traceparent, tracestate := capturedW3CHeaders(ctx, stack, activeUUID, parentUUID)
	if traceparent == nil && activeUUID != nil {
		formatted := FormatTraceparent(*activeUUID, *activeUUID)
		traceparent = &formatted
	}
	propagation := PropagationContext{
		Version:     PropagationContextVersion,
		RootUUID:    rootUUID,
		ParentUUID:  parentUUID,
		Traceparent: traceparent,
		Tracestate:  tracestate,
	}
	if err := propagation.Validate(); err != nil {
		return PropagationContext{}, err
	}
	return propagation, nil
}

// Captures the current trace position without a trace root
func CaptureRootlessPropagationContext(ctx context.Context) (PropagationContext, error) {
	stack := CurrentScopeStack(ctx)
	parentUUID := stack.Top().UUID
	if activeUUID := activeEventUUID(ctx); activeUUID != nil {
		parentUUID = *activeUUID
	}
	traceparent, tracestate := capturedImportedW3CHeaders(stack, parentUUID)
	propagation := PropagationContext{
		Version:     PropagationContextVersion,
		ParentUUID:  parentUUID,
		Traceparent: traceparent,
		Tracestate:  tracestate,
	}
	if err := propagation.Validate(); err != nil {
		return PropagationContext{}, err
	}
	return propagation, nil
}

// Captures the current trace position under an explicit root, or rootless when none is given
func CapturePropagationContextWithRoot(ctx context.Context, rootUUID *uuid.UUID) (PropagationContext, error) {
	if rootUUID == nil {
		return CaptureRootlessPropagationContext(ctx)
	}
	propagation, err := CaptureRootlessPropagationContext(ctx)
	if err != nil {
		return PropagationContext{}, err
	}
	propagation.RootUUID = cloneUUIDPtr(rootUUID)
	if err := propagation.Validate(); err != nil {
		return PropagationContext{}, err
	}
	return propagation, nil
}

func captureW3CTraceContext(ctx context.Context) (*W3CTraceContext, error) {
	propagation, err := CapturePropagationContext(ctx)
	if err != nil {
		return nil, err
	}
	unavailable := invalidArgumentError("no emitted Relay scope is available for W3C trace context capture")
	if propagation.Traceparent == nil {
		return nil, unavailable
	}
	context, err := NewW3CTraceContext(*propagation.Traceparent, propagation.Tracestate)
	if err != nil {
		return nil, unavailable
	}
	return context, nil
}

// Returns the W3C traceparent of the current trace position
func CaptureTraceparent(ctx context.Context) (string, error) {
	context, err := captureW3CTraceContext(ctx)
	if err != nil {
		return "", err
	}
	return context.traceparent, nil
}

func captureTraceContext(ctx context.Context) (string, *string, error) {
	context, err := captureW3CTraceContext(ctx)
	if err != nil {
		return "", nil, err
	}
	return context.traceparent, cloneStringPtr(context.tracestate), nil
}

// Returns the trace context a managed tool or LLM span runs under
func traceContextForManagedSpan(ctx context.Context, spanUUID uuid.UUID, causalParentUUID *uuid.UUID) (*W3CTraceContext, error) {
	stack := CurrentScopeStack(ctx)
	var traceparent, tracestate *string
	active := activeEventUUID(ctx)
	if uuidPtrEqual(causalParentUUID, active) {
		resolved := false
		if activeContext := activeEventTraceContext(ctx); activeContext != nil {
			if parent, ok := activeContext.spanContext(); ok {
				traceparent, tracestate = w3cHeadersForParent(parent, spanUUID)
				resolved = true
			}
		}
		if !resolved {
			traceparent, tracestate = stack.w3cHeadersForSpan(spanUUID, stack.Top().UUID)
		}
	} else if causalParentUUID != nil {
		traceparent, tracestate = stack.w3cHeadersForSpan(spanUUID, *causalParentUUID)
	}
	if traceparent == nil {
		formatted := FormatTraceparent(spanUUID, spanUUID)
		traceparent = &formatted
	}
	return NewW3CTraceContext(*traceparent, tracestate)
}

func cloneHandles(handles []ScopeHandle) []ScopeHandle {
	cloned := make([]ScopeHandle, len(handles))
	for index, handle := range handles {
		cloned[index] = handle.clone()
	}
	return cloned
}

func cloneUUIDPtr(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func uuidPtrEqual(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
