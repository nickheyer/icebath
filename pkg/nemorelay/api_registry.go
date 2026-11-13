package nemorelay

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/nemorelay/logging"
	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

type conditionalMiddlewareGuardrail struct {
	kinds            []types.RuntimeRegistrationKind
	registrationName string
	callback         ConditionalMiddlewareGuardrailFn
}

var (
	conditionalGatesMu   sync.RWMutex
	conditionalGates     map[string]conditionalMiddlewareGuardrail
	conditionalGatesUsed bool
)

// Registers a gate that can disable named runtime registrations of the given kinds
func RegisterConditionalMiddlewareGuardrail(name string, kinds []types.RuntimeRegistrationKind, registrationName string, guardrail ConditionalMiddlewareGuardrailFn) error {
	if len(kinds) == 0 {
		return invalidArgumentError("conditional middleware guardrail kinds must not be empty")
	}
	conditionalGatesMu.Lock()
	defer conditionalGatesMu.Unlock()
	if conditionalGates == nil {
		conditionalGates = map[string]conditionalMiddlewareGuardrail{}
		conditionalGatesUsed = true
	}
	if _, exists := conditionalGates[name]; exists {
		return alreadyExistsError(name + " conditional middleware guardrail already exists")
	}
	unique := slices.Clone(kinds)
	slices.Sort(unique)
	conditionalGates[name] = conditionalMiddlewareGuardrail{kinds: slices.Compact(unique), registrationName: registrationName, callback: guardrail}
	return nil
}

// Removes a conditional middleware gate and reports whether it existed
func DeregisterConditionalMiddlewareGuardrail(name string) (bool, error) {
	conditionalGatesMu.Lock()
	defer conditionalGatesMu.Unlock()
	if conditionalGates == nil {
		conditionalGates = map[string]conditionalMiddlewareGuardrail{}
		conditionalGatesUsed = true
	}
	_, existed := conditionalGates[name]
	delete(conditionalGates, name)
	return existed, nil
}

// Asks every matching gate whether a registration may run, enabling it when none objects
func runtimeRegistrationIsEnabled(kind types.RuntimeRegistrationKind, effectiveName string) bool {
	conditionalGatesMu.RLock()
	if !conditionalGatesUsed {
		conditionalGatesMu.RUnlock()
		return true
	}
	type match struct {
		name  string
		gate  conditionalMiddlewareGuardrail
		kinds []types.RuntimeRegistrationKind
	}
	matching := make([]match, 0)
	for _, name := range slices.Sorted(maps.Keys(conditionalGates)) {
		gate := conditionalGates[name]
		if slices.Contains(gate.kinds, kind) && gate.registrationName == effectiveName {
			matching = append(matching, match{name: name, gate: gate, kinds: slices.Clone(gate.kinds)})
		}
	}
	conditionalGatesMu.RUnlock()
	for _, candidate := range matching {
		reason, panicked := callGate(candidate.gate.callback, candidate.kinds, effectiveName)
		fields := map[string]any{"gate": candidate.name, "registration_kind": kind.String(), "registration_name": effectiveName}
		if panicked {
			fields["event"] = "conditional_middleware_guardrail_panicked"
			logging.Error(runtimeLogTarget, "Conditional middleware guardrail panicked; enabling target", fields)
			continue
		}
		if reason != nil {
			fields["event"] = "runtime_registration_disabled"
			fields["reason"] = *reason
			logging.Debug(runtimeLogTarget, "Conditional middleware guardrail disabled runtime registration", fields)
			return false
		}
	}
	return true
}

func callGate(callback ConditionalMiddlewareGuardrailFn, kinds []types.RuntimeRegistrationKind, effectiveName string) (reason *string, panicked bool) {
	defer func() {
		if recover() != nil {
			reason, panicked = nil, true
		}
	}()
	return callback(kinds, effectiveName), false
}

// Builds the identity of a registration, decoding plugin component namespaces
func registrationIdentity(kind types.RuntimeRegistrationKind, effectiveName string) types.RuntimeRegistrationIdentity {
	pluginKind, ordinal, localName, ok := DecodePluginComponentEffectiveName(effectiveName)
	if !ok {
		return types.RuntimeRegistrationIdentity{
			Kind:          kind,
			LocalName:     effectiveName,
			EffectiveName: effectiveName,
			Owner:         types.RuntimeRegistrationOwner{Kind: types.RuntimeRegistrationOwnerKindGlobalAPI},
		}
	}
	return types.RuntimeRegistrationIdentity{
		Kind:          kind,
		LocalName:     localName,
		EffectiveName: effectiveName,
		Owner: types.RuntimeRegistrationOwner{
			Kind:             types.RuntimeRegistrationOwnerKindPlugin,
			PluginKind:       &pluginKind,
			ComponentOrdinal: &ordinal,
		},
	}
}

// Reports for each kind whether any global registration of it exists
func RuntimeRegistrationPresence(kinds []types.RuntimeRegistrationKind) ([]bool, error) {
	return readGlobal(func(state *runtimeState) []bool {
		presence := make([]bool, len(kinds))
		for index, kind := range kinds {
			presence[index] = state.kindPresent(kind)
		}
		return presence
	}), nil
}

func (s *runtimeState) kindPresent(kind types.RuntimeRegistrationKind) bool {
	switch kind {
	case types.RuntimeRegistrationKindSubscriber:
		return len(s.eventSubscribers) > 0
	case types.RuntimeRegistrationKindEventMetadataInjector:
		return !s.eventMetadataInjectors.isEmpty()
	case types.RuntimeRegistrationKindMarkSanitizeGuardrail:
		return !s.markSanitizeGuardrails.isEmpty()
	case types.RuntimeRegistrationKindScopeSanitizeStartGuardrail:
		return !s.scopeSanitizeStartGuardrails.isEmpty()
	case types.RuntimeRegistrationKindScopeSanitizeEndGuardrail:
		return !s.scopeSanitizeEndGuardrails.isEmpty()
	case types.RuntimeRegistrationKindToolSanitizeRequestGuardrail:
		return !s.toolSanitizeRequestGuardrails.isEmpty()
	case types.RuntimeRegistrationKindToolSanitizeResponseGuardrail:
		return !s.toolSanitizeResponseGuardrails.isEmpty()
	case types.RuntimeRegistrationKindToolConditionalExecutionGuardrail:
		return !s.toolConditionalExecutionGuardrail.isEmpty()
	case types.RuntimeRegistrationKindToolRequestIntercept:
		return !s.toolRequestIntercepts.isEmpty()
	case types.RuntimeRegistrationKindToolExecutionIntercept:
		return !s.toolExecutionIntercepts.isEmpty()
	case types.RuntimeRegistrationKindLLMSanitizeRequestGuardrail:
		return !s.llmSanitizeRequestGuardrails.isEmpty()
	case types.RuntimeRegistrationKindLLMSanitizeResponseGuardrail:
		return !s.llmSanitizeResponseGuardrails.isEmpty()
	case types.RuntimeRegistrationKindLLMConditionalExecutionGuardrail:
		return !s.llmConditionalExecutionGuardrails.isEmpty()
	case types.RuntimeRegistrationKindLLMRequestIntercept:
		return !s.llmRequestIntercepts.isEmpty()
	case types.RuntimeRegistrationKindLLMExecutionIntercept:
		return !s.llmExecutionIntercepts.isEmpty()
	case types.RuntimeRegistrationKindLLMStreamExecutionIntercept:
		return !s.llmStreamExecutionIntercepts.isEmpty()
	}
	return false
}

func registryNames[T any](registry sortedRegistry[T]) []string {
	values := registry.values()
	names := make([]string, len(values))
	for index, value := range values {
		names[index] = value.name
	}
	return names
}

// Lists global registrations of the selected kinds, or every kind when none is given
func ListRuntimeRegistrations(kinds []types.RuntimeRegistrationKind) ([]types.RuntimeRegistrationIdentity, error) {
	selected := func(kind types.RuntimeRegistrationKind) bool {
		return kinds == nil || slices.Contains(kinds, kind)
	}
	registrations := readGlobal(func(state *runtimeState) []types.RuntimeRegistrationIdentity {
		names := []struct {
			kind  types.RuntimeRegistrationKind
			names []string
		}{
			{types.RuntimeRegistrationKindSubscriber, slices.Collect(maps.Keys(state.eventSubscribers))},
			{types.RuntimeRegistrationKindEventMetadataInjector, registryNames(state.eventMetadataInjectors)},
			{types.RuntimeRegistrationKindMarkSanitizeGuardrail, registryNames(state.markSanitizeGuardrails)},
			{types.RuntimeRegistrationKindScopeSanitizeStartGuardrail, registryNames(state.scopeSanitizeStartGuardrails)},
			{types.RuntimeRegistrationKindScopeSanitizeEndGuardrail, registryNames(state.scopeSanitizeEndGuardrails)},
			{types.RuntimeRegistrationKindToolSanitizeRequestGuardrail, registryNames(state.toolSanitizeRequestGuardrails)},
			{types.RuntimeRegistrationKindToolSanitizeResponseGuardrail, registryNames(state.toolSanitizeResponseGuardrails)},
			{types.RuntimeRegistrationKindToolConditionalExecutionGuardrail, registryNames(state.toolConditionalExecutionGuardrail)},
			{types.RuntimeRegistrationKindToolRequestIntercept, registryNames(state.toolRequestIntercepts)},
			{types.RuntimeRegistrationKindToolExecutionIntercept, registryNames(state.toolExecutionIntercepts)},
			{types.RuntimeRegistrationKindLLMSanitizeRequestGuardrail, registryNames(state.llmSanitizeRequestGuardrails)},
			{types.RuntimeRegistrationKindLLMSanitizeResponseGuardrail, registryNames(state.llmSanitizeResponseGuardrails)},
			{types.RuntimeRegistrationKindLLMConditionalExecutionGuardrail, registryNames(state.llmConditionalExecutionGuardrails)},
			{types.RuntimeRegistrationKindLLMRequestIntercept, registryNames(state.llmRequestIntercepts)},
			{types.RuntimeRegistrationKindLLMExecutionIntercept, registryNames(state.llmExecutionIntercepts)},
			{types.RuntimeRegistrationKindLLMStreamExecutionIntercept, registryNames(state.llmStreamExecutionIntercepts)},
		}
		identities := make([]types.RuntimeRegistrationIdentity, 0)
		for _, group := range names {
			if !selected(group.kind) {
				continue
			}
			for _, name := range group.names {
				identities = append(identities, registrationIdentity(group.kind, name))
			}
		}
		return identities
	})
	slices.SortStableFunc(registrations, func(left, right types.RuntimeRegistrationIdentity) int {
		if order := cmp.Compare(left.Kind, right.Kind); order != 0 {
			return order
		}
		return cmp.Compare(left.EffectiveName, right.EffectiveName)
	})
	return registrations, nil
}

type registryField[F any] func(*middlewareRegistries) *sortedRegistry[F]

var (
	fieldEventMetadataInjectors registryField[EventMetadataInjectorFn] = func(r *middlewareRegistries) *sortedRegistry[EventMetadataInjectorFn] {
		return &r.eventMetadataInjectors
	}
	fieldMarkSanitize         registryField[EventSanitizeFn] = func(r *middlewareRegistries) *sortedRegistry[EventSanitizeFn] { return &r.markSanitizeGuardrails }
	fieldScopeSanitizeStart   registryField[EventSanitizeFn] = func(r *middlewareRegistries) *sortedRegistry[EventSanitizeFn] { return &r.scopeSanitizeStartGuardrails }
	fieldScopeSanitizeEnd     registryField[EventSanitizeFn] = func(r *middlewareRegistries) *sortedRegistry[EventSanitizeFn] { return &r.scopeSanitizeEndGuardrails }
	fieldToolSanitizeRequest  registryField[ToolSanitizeFn]  = func(r *middlewareRegistries) *sortedRegistry[ToolSanitizeFn] { return &r.toolSanitizeRequestGuardrails }
	fieldToolSanitizeResponse registryField[ToolSanitizeFn]  = func(r *middlewareRegistries) *sortedRegistry[ToolSanitizeFn] {
		return &r.toolSanitizeResponseGuardrails
	}
	fieldToolConditionalExecution registryField[ToolConditionalFn] = func(r *middlewareRegistries) *sortedRegistry[ToolConditionalFn] {
		return &r.toolConditionalExecutionGuardrail
	}
	fieldToolRequestIntercepts registryField[requestIntercept[ToolInterceptFn]] = func(r *middlewareRegistries) *sortedRegistry[requestIntercept[ToolInterceptFn]] {
		return &r.toolRequestIntercepts
	}
	fieldToolExecutionIntercepts registryField[ToolExecutionFn]      = func(r *middlewareRegistries) *sortedRegistry[ToolExecutionFn] { return &r.toolExecutionIntercepts }
	fieldLLMSanitizeRequest      registryField[LLMSanitizeRequestFn] = func(r *middlewareRegistries) *sortedRegistry[LLMSanitizeRequestFn] {
		return &r.llmSanitizeRequestGuardrails
	}
	fieldLLMSanitizeResponse registryField[LLMSanitizeResponseFn] = func(r *middlewareRegistries) *sortedRegistry[LLMSanitizeResponseFn] {
		return &r.llmSanitizeResponseGuardrails
	}
	fieldLLMConditionalExecution registryField[LLMConditionalFn] = func(r *middlewareRegistries) *sortedRegistry[LLMConditionalFn] {
		return &r.llmConditionalExecutionGuardrails
	}
	fieldLLMRequestIntercepts registryField[requestIntercept[LLMRequestInterceptFn]] = func(r *middlewareRegistries) *sortedRegistry[requestIntercept[LLMRequestInterceptFn]] {
		return &r.llmRequestIntercepts
	}
	fieldLLMExecutionIntercepts       registryField[LLMExecutionFn]       = func(r *middlewareRegistries) *sortedRegistry[LLMExecutionFn] { return &r.llmExecutionIntercepts }
	fieldLLMStreamExecutionIntercepts registryField[LLMStreamExecutionFn] = func(r *middlewareRegistries) *sortedRegistry[LLMStreamExecutionFn] {
		return &r.llmStreamExecutionIntercepts
	}
)

func registerGlobal[F any](field registryField[F], name string, priority int, payload F) error {
	return writeGlobal(func(state *runtimeState) error {
		if err := field(&state.middlewareRegistries).register(registryRecord[F]{name: name, priority: priority, payload: payload}); err != nil {
			return alreadyExistsError(err.Error())
		}
		return nil
	})
}

func deregisterGlobal[F any](field registryField[F], name string) (bool, error) {
	return writeGlobal(func(state *runtimeState) bool { return field(&state.middlewareRegistries).deregister(name) }), nil
}

func registerScope[F any](ctx context.Context, scopeUUID uuid.UUID, field registryField[F], name string, priority int, payload F) error {
	return CurrentScopeStack(ctx).withLocalRegistries(scopeUUID, func(local *scopeLocalRegistries) error {
		if err := field(&local.middlewareRegistries).register(registryRecord[F]{name: name, priority: priority, payload: payload}); err != nil {
			return alreadyExistsError(err.Error())
		}
		return nil
	})
}

func deregisterScope[F any](ctx context.Context, scopeUUID uuid.UUID, field registryField[F], name string) (bool, error) {
	removed := false
	err := CurrentScopeStack(ctx).withLocalRegistries(scopeUUID, func(local *scopeLocalRegistries) error {
		removed = field(&local.middlewareRegistries).deregister(name)
		return nil
	})
	return removed, err
}

func RegisterMarkSanitizeGuardrail(name string, priority int, guardrail EventSanitizeFn) error {
	return registerGlobal(fieldMarkSanitize, name, priority, guardrail)
}

func DeregisterMarkSanitizeGuardrail(name string) (bool, error) {
	return deregisterGlobal(fieldMarkSanitize, name)
}

func RegisterEventMetadataInjector(name string, priority int, injector EventMetadataInjectorFn) error {
	return registerGlobal(fieldEventMetadataInjectors, name, priority, injector)
}

func DeregisterEventMetadataInjector(name string) (bool, error) {
	return deregisterGlobal(fieldEventMetadataInjectors, name)
}

func RegisterScopeSanitizeStartGuardrail(name string, priority int, guardrail EventSanitizeFn) error {
	return registerGlobal(fieldScopeSanitizeStart, name, priority, guardrail)
}

func DeregisterScopeSanitizeStartGuardrail(name string) (bool, error) {
	return deregisterGlobal(fieldScopeSanitizeStart, name)
}

func RegisterScopeSanitizeEndGuardrail(name string, priority int, guardrail EventSanitizeFn) error {
	return registerGlobal(fieldScopeSanitizeEnd, name, priority, guardrail)
}

func DeregisterScopeSanitizeEndGuardrail(name string) (bool, error) {
	return deregisterGlobal(fieldScopeSanitizeEnd, name)
}

func RegisterToolSanitizeRequestGuardrail(name string, priority int, guardrail ToolSanitizeFn) error {
	return registerGlobal(fieldToolSanitizeRequest, name, priority, guardrail)
}

func DeregisterToolSanitizeRequestGuardrail(name string) (bool, error) {
	return deregisterGlobal(fieldToolSanitizeRequest, name)
}

func RegisterToolSanitizeResponseGuardrail(name string, priority int, guardrail ToolSanitizeFn) error {
	return registerGlobal(fieldToolSanitizeResponse, name, priority, guardrail)
}

func DeregisterToolSanitizeResponseGuardrail(name string) (bool, error) {
	return deregisterGlobal(fieldToolSanitizeResponse, name)
}

func RegisterToolConditionalExecutionGuardrail(name string, priority int, guardrail ToolConditionalFn) error {
	return registerGlobal(fieldToolConditionalExecution, name, priority, guardrail)
}

func DeregisterToolConditionalExecutionGuardrail(name string) (bool, error) {
	return deregisterGlobal(fieldToolConditionalExecution, name)
}

func RegisterToolRequestIntercept(name string, priority int, breakChain bool, intercept ToolInterceptFn) error {
	return registerGlobal(fieldToolRequestIntercepts, name, priority, requestIntercept[ToolInterceptFn]{breakChain: breakChain, callable: intercept})
}

func DeregisterToolRequestIntercept(name string) (bool, error) {
	return deregisterGlobal(fieldToolRequestIntercepts, name)
}

func RegisterToolExecutionIntercept(name string, priority int, intercept ToolExecutionFn) error {
	return registerGlobal(fieldToolExecutionIntercepts, name, priority, intercept)
}

func DeregisterToolExecutionIntercept(name string) (bool, error) {
	return deregisterGlobal(fieldToolExecutionIntercepts, name)
}

func RegisterLLMSanitizeRequestGuardrail(name string, priority int, guardrail LLMSanitizeRequestFn) error {
	return registerGlobal(fieldLLMSanitizeRequest, name, priority, guardrail)
}

func DeregisterLLMSanitizeRequestGuardrail(name string) (bool, error) {
	return deregisterGlobal(fieldLLMSanitizeRequest, name)
}

func RegisterLLMSanitizeResponseGuardrail(name string, priority int, guardrail LLMSanitizeResponseFn) error {
	return registerGlobal(fieldLLMSanitizeResponse, name, priority, guardrail)
}

func DeregisterLLMSanitizeResponseGuardrail(name string) (bool, error) {
	return deregisterGlobal(fieldLLMSanitizeResponse, name)
}

func RegisterLLMConditionalExecutionGuardrail(name string, priority int, guardrail LLMConditionalFn) error {
	return registerGlobal(fieldLLMConditionalExecution, name, priority, guardrail)
}

func DeregisterLLMConditionalExecutionGuardrail(name string) (bool, error) {
	return deregisterGlobal(fieldLLMConditionalExecution, name)
}

func RegisterLLMRequestIntercept(name string, priority int, breakChain bool, intercept LLMRequestInterceptFn) error {
	return registerGlobal(fieldLLMRequestIntercepts, name, priority, requestIntercept[LLMRequestInterceptFn]{breakChain: breakChain, callable: intercept})
}

func DeregisterLLMRequestIntercept(name string) (bool, error) {
	return deregisterGlobal(fieldLLMRequestIntercepts, name)
}

func RegisterLLMExecutionIntercept(name string, priority int, intercept LLMExecutionFn) error {
	return registerGlobal(fieldLLMExecutionIntercepts, name, priority, intercept)
}

func DeregisterLLMExecutionIntercept(name string) (bool, error) {
	return deregisterGlobal(fieldLLMExecutionIntercepts, name)
}

func RegisterLLMStreamExecutionIntercept(name string, priority int, intercept LLMStreamExecutionFn) error {
	return registerGlobal(fieldLLMStreamExecutionIntercepts, name, priority, intercept)
}

func DeregisterLLMStreamExecutionIntercept(name string) (bool, error) {
	return deregisterGlobal(fieldLLMStreamExecutionIntercepts, name)
}

func ScopeRegisterMarkSanitizeGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, guardrail EventSanitizeFn) error {
	return registerScope(ctx, scopeUUID, fieldMarkSanitize, name, priority, guardrail)
}

func ScopeDeregisterMarkSanitizeGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldMarkSanitize, name)
}

func ScopeRegisterEventMetadataInjector(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, injector EventMetadataInjectorFn) error {
	return registerScope(ctx, scopeUUID, fieldEventMetadataInjectors, name, priority, injector)
}

func ScopeDeregisterEventMetadataInjector(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldEventMetadataInjectors, name)
}

func ScopeRegisterScopeSanitizeStartGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, guardrail EventSanitizeFn) error {
	return registerScope(ctx, scopeUUID, fieldScopeSanitizeStart, name, priority, guardrail)
}

func ScopeDeregisterScopeSanitizeStartGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldScopeSanitizeStart, name)
}

func ScopeRegisterScopeSanitizeEndGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, guardrail EventSanitizeFn) error {
	return registerScope(ctx, scopeUUID, fieldScopeSanitizeEnd, name, priority, guardrail)
}

func ScopeDeregisterScopeSanitizeEndGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldScopeSanitizeEnd, name)
}

func ScopeRegisterToolSanitizeRequestGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, guardrail ToolSanitizeFn) error {
	return registerScope(ctx, scopeUUID, fieldToolSanitizeRequest, name, priority, guardrail)
}

func ScopeDeregisterToolSanitizeRequestGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldToolSanitizeRequest, name)
}

func ScopeRegisterToolSanitizeResponseGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, guardrail ToolSanitizeFn) error {
	return registerScope(ctx, scopeUUID, fieldToolSanitizeResponse, name, priority, guardrail)
}

func ScopeDeregisterToolSanitizeResponseGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldToolSanitizeResponse, name)
}

func ScopeRegisterToolConditionalExecutionGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, guardrail ToolConditionalFn) error {
	return registerScope(ctx, scopeUUID, fieldToolConditionalExecution, name, priority, guardrail)
}

func ScopeDeregisterToolConditionalExecutionGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldToolConditionalExecution, name)
}

func ScopeRegisterToolRequestIntercept(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, breakChain bool, intercept ToolInterceptFn) error {
	return registerScope(ctx, scopeUUID, fieldToolRequestIntercepts, name, priority, requestIntercept[ToolInterceptFn]{breakChain: breakChain, callable: intercept})
}

func ScopeDeregisterToolRequestIntercept(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldToolRequestIntercepts, name)
}

func ScopeRegisterToolExecutionIntercept(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, intercept ToolExecutionFn) error {
	return registerScope(ctx, scopeUUID, fieldToolExecutionIntercepts, name, priority, intercept)
}

func ScopeDeregisterToolExecutionIntercept(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldToolExecutionIntercepts, name)
}

func ScopeRegisterLLMSanitizeRequestGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, guardrail LLMSanitizeRequestFn) error {
	return registerScope(ctx, scopeUUID, fieldLLMSanitizeRequest, name, priority, guardrail)
}

func ScopeDeregisterLLMSanitizeRequestGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldLLMSanitizeRequest, name)
}

func ScopeRegisterLLMSanitizeResponseGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, guardrail LLMSanitizeResponseFn) error {
	return registerScope(ctx, scopeUUID, fieldLLMSanitizeResponse, name, priority, guardrail)
}

func ScopeDeregisterLLMSanitizeResponseGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldLLMSanitizeResponse, name)
}

func ScopeRegisterLLMConditionalExecutionGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, guardrail LLMConditionalFn) error {
	return registerScope(ctx, scopeUUID, fieldLLMConditionalExecution, name, priority, guardrail)
}

func ScopeDeregisterLLMConditionalExecutionGuardrail(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldLLMConditionalExecution, name)
}

func ScopeRegisterLLMRequestIntercept(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, breakChain bool, intercept LLMRequestInterceptFn) error {
	return registerScope(ctx, scopeUUID, fieldLLMRequestIntercepts, name, priority, requestIntercept[LLMRequestInterceptFn]{breakChain: breakChain, callable: intercept})
}

func ScopeDeregisterLLMRequestIntercept(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldLLMRequestIntercepts, name)
}

func ScopeRegisterLLMExecutionIntercept(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, intercept LLMExecutionFn) error {
	return registerScope(ctx, scopeUUID, fieldLLMExecutionIntercepts, name, priority, intercept)
}

func ScopeDeregisterLLMExecutionIntercept(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldLLMExecutionIntercepts, name)
}

func ScopeRegisterLLMStreamExecutionIntercept(ctx context.Context, scopeUUID uuid.UUID, name string, priority int, intercept LLMStreamExecutionFn) error {
	return registerScope(ctx, scopeUUID, fieldLLMStreamExecutionIntercepts, name, priority, intercept)
}

func ScopeDeregisterLLMStreamExecutionIntercept(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	return deregisterScope(ctx, scopeUUID, fieldLLMStreamExecutionIntercepts, name)
}
