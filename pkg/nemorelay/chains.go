package nemorelay

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/nemorelay/logging"
	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

// Calls a callback and converts a panic into a reported flag
func callRecovering[T any](call func() (T, error)) (value T, err error, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	value, err = call()
	return value, err, false
}

// Applies metadata injectors in order, keeping existing metadata keys
func eventMetadataInjectionChain(ctx context.Context, event *types.Event, entries []eventMetadataInjectorRecord) *types.Event {
	for _, entry := range entries {
		attributes, err, panicked := callRecovering(func() (map[string]any, error) {
			return entry.payload(ctx, event.Clone())
		})
		fields := map[string]any{"injector": entry.name, "event_name": event.Name()}
		switch {
		case panicked:
			fields["event"] = "event_metadata_injector_panicked"
			logging.Error(runtimeLogTarget, "Event metadata injector panicked; omitting its attributes", fields)
			continue
		case err != nil:
			fields["event"] = "event_metadata_injector_failed"
			logging.Error(runtimeLogTarget, "Event metadata injector failed; omitting its attributes: "+err.Error(), fields)
			continue
		}
		if invalid := validateEventMetadataAttributes(attributes); invalid != "" {
			fields["event"] = "event_metadata_injector_invalid_output"
			logging.Error(runtimeLogTarget, "Event metadata injector returned invalid attributes; omitting them: "+invalid, fields)
			continue
		}
		if len(attributes) == 0 {
			continue
		}
		metadata := event.Metadata()
		if metadata == nil {
			metadata = map[string]any{}
		}
		object, ok := metadata.(map[string]any)
		if !ok {
			fields["event"] = "event_metadata_injector_non_object_metadata"
			logging.Error(runtimeLogTarget, "Event metadata is not an object; omitting injected attributes", fields)
			continue
		}
		object = maps.Clone(object)
		for _, key := range slices.Sorted(maps.Keys(attributes)) {
			if _, exists := object[key]; !exists {
				object[key] = attributes[key]
			}
		}
		setEventMetadata(event, object)
	}
	return event
}

func setEventMetadata(event *types.Event, metadata any) {
	switch {
	case event.Scope != nil:
		event.Scope.Base.Metadata = metadata
	case event.Mark != nil:
		event.Mark.Base.Metadata = metadata
	}
}

// Applies event sanitizers in order and clears observability fields at the first failure
func eventSanitizeChain(ctx context.Context, event *types.Event, entries []registryRecord[EventSanitizeFn]) *types.Event {
	for _, entry := range entries {
		fields := cloneSanitizeFields(event.SanitizeFields())
		sanitized, err, panicked := callRecovering(func() (types.EventSanitizeFields, error) {
			return entry.payload(ctx, event.Clone(), fields)
		})
		logFields := map[string]any{"sanitizer": entry.name, "event_name": event.Name()}
		switch {
		case panicked:
			logFields["event"] = "event_sanitizer_panicked"
			logging.Error(runtimeLogTarget, "Event sanitizer panicked; clearing observability fields", logFields)
			event.ApplySanitizeFields(types.EventSanitizeFields{})
			return event
		case err != nil:
			logFields["event"] = "event_sanitizer_failed"
			logging.Error(runtimeLogTarget, "Event sanitizer failed; clearing observability fields", logFields)
			event.ApplySanitizeFields(types.EventSanitizeFields{})
			return event
		}
		event.ApplySanitizeFields(sanitized)
	}
	return event
}

func cloneSanitizeFields(fields types.EventSanitizeFields) types.EventSanitizeFields {
	fields.Data = types.CloneJSON(fields.Data)
	fields.Metadata = types.CloneJSON(fields.Metadata)
	if fields.CategoryProfile != nil {
		profile := fields.CategoryProfile.Clone()
		fields.CategoryProfile = profile
	}
	return fields
}

// Rejects injected attributes that cannot become OpenTelemetry attributes
func validateEventMetadataAttributes(attributes map[string]any) string {
	for _, key := range slices.Sorted(maps.Keys(attributes)) {
		if !isValidEventMetadataAttributeKey(key) {
			return fmt.Sprintf("attribute key `%s` must contain letter, number, underscore, or hyphen segments separated by single dots", key)
		}
		if !isOTelCompatibleAttributeValue(attributes[key]) {
			return fmt.Sprintf("attribute `%s` must be a primitive or homogeneous primitive list", key)
		}
	}
	return ""
}

// Reports whether a key is dot separated segments of letters, digits, underscores or hyphens
func isValidEventMetadataAttributeKey(key string) bool {
	for _, segment := range strings.Split(key, ".") {
		if segment == "" {
			return false
		}
		for i := 0; i < len(segment); i++ {
			if b := segment[i]; !isASCIIAlphanumeric(b) && b != '_' && b != '-' {
				return false
			}
		}
	}
	return true
}

type otelPrimitiveKind int

const (
	otelPrimitiveNone otelPrimitiveKind = iota
	otelPrimitiveBool
	otelPrimitiveNumber
	otelPrimitiveString
)

func otelNumberKind(value any) otelPrimitiveKind {
	if unsigned, ok := types.JSONUint64(value); ok {
		if unsigned > 1<<63-1 {
			return otelPrimitiveNone
		}
		return otelPrimitiveNumber
	}
	if _, ok := types.JSONFloat64(value); ok {
		return otelPrimitiveNumber
	}
	return otelPrimitiveNone
}

func otelPrimitive(value any) otelPrimitiveKind {
	switch value.(type) {
	case bool:
		return otelPrimitiveBool
	case string:
		return otelPrimitiveString
	case nil, map[string]any, []any:
		return otelPrimitiveNone
	}
	if types.IsJSONNumber(value) {
		return otelNumberKind(value)
	}
	return otelPrimitiveNone
}

func isOTelCompatibleAttributeValue(value any) bool {
	switch typed := value.(type) {
	case bool, string:
		return true
	case nil, map[string]any:
		return false
	case []any:
		if len(typed) == 0 {
			return true
		}
		kind := otelPrimitive(typed[0])
		if kind == otelPrimitiveNone {
			return false
		}
		for _, item := range typed {
			if otelPrimitive(item) != kind {
				return false
			}
		}
		return true
	}
	return types.IsJSONNumber(value) && otelNumberKind(value) != otelPrimitiveNone
}

// Runs tool sanitizers in order, dropping the payload once one fails
func toolSanitizeChain(ctx context.Context, toolName string, value any, entries []registryRecord[ToolSanitizeFn], response bool) (any, bool) {
	current, present := value, true
	kind, title := "request", "Tool request"
	if response {
		kind, title = "response", "Tool response"
	}
	for _, entry := range entries {
		if !present {
			continue
		}
		next, err, panicked := callRecovering(func() (any, error) { return entry.payload(ctx, toolName, current) })
		fields := map[string]any{"sanitizer": entry.name, "tool_name": toolName}
		switch {
		case panicked:
			fields["event"] = "tool_" + kind + "_sanitizer_panicked"
			logging.Error(runtimeLogTarget, title+" sanitizer panicked; omitting the observability payload", fields)
			present = false
		case err != nil:
			fields["event"] = "tool_" + kind + "_sanitizer_failed"
			logging.Error(runtimeLogTarget, title+" sanitizer failed; omitting the observability payload", fields)
			present = false
		default:
			current = next
		}
	}
	if !present {
		return nil, false
	}
	return current, true
}

// Runs LLM request sanitizers in order, dropping the payload once one fails or drops it
func llmSanitizeRequestChain(ctx context.Context, request types.LLMRequest, sanitize LLMSanitizeRequestContext, entries []registryRecord[LLMSanitizeRequestFn]) *types.LLMRequest {
	current := &request
	for _, entry := range entries {
		if current == nil {
			continue
		}
		input := *current
		next, err, panicked := callRecovering(func() (*types.LLMRequest, error) { return entry.payload(ctx, input, sanitize) })
		fields := map[string]any{"sanitizer": entry.name}
		switch {
		case panicked:
			fields["event"] = "llm_request_sanitizer_panicked"
			logging.Error(runtimeLogTarget, "LLM request sanitizer panicked; omitting the observability payload", fields)
			current = nil
		case err != nil:
			fields["event"] = "llm_request_sanitizer_failed"
			logging.Error(runtimeLogTarget, "LLM request sanitizer failed; omitting the observability payload", fields)
			current = nil
		default:
			current = next
		}
	}
	return current
}

// Runs LLM response sanitizers in order, dropping the payload once one fails or drops it
func llmSanitizeResponseChain(ctx context.Context, response any, sanitize LLMSanitizeResponseContext, entries []registryRecord[LLMSanitizeResponseFn]) (any, bool) {
	current, present := response, true
	for _, entry := range entries {
		if !present {
			continue
		}
		input := current
		type result struct {
			value any
			keep  bool
		}
		next, err, panicked := callRecovering(func() (result, error) {
			value, keep, err := entry.payload(ctx, input, sanitize)
			return result{value: value, keep: keep}, err
		})
		fields := map[string]any{"sanitizer": entry.name}
		switch {
		case panicked:
			fields["event"] = "llm_response_sanitizer_panicked"
			logging.Error(runtimeLogTarget, "LLM response sanitizer panicked; omitting the observability payload", fields)
			present = false
		case err != nil:
			fields["event"] = "llm_response_sanitizer_failed"
			logging.Error(runtimeLogTarget, "LLM response sanitizer failed; omitting the observability payload", fields)
			present = false
		default:
			current, present = next.value, next.keep
		}
	}
	if !present {
		return nil, false
	}
	return current, true
}

// Guardrail scope whose end event is emitted exactly once, as cancelled if never finished
type guardrailScopeCompletion struct {
	ctx         context.Context
	handle      *ScopeHandle
	subscribers []EventSubscriberFn
	stack       *ScopeStack
	pending     *pendingPublication
}

func newGuardrailScopeCompletion(ctx context.Context, handle ScopeHandle, subscribers []EventSubscriberFn, stack *ScopeStack) *guardrailScopeCompletion {
	completion := &guardrailScopeCompletion{ctx: ctx, handle: &handle, subscribers: subscribers, stack: stack}
	if len(subscribers) > 0 {
		completion.pending = registerPendingPublication()
	}
	return completion
}

func (c *guardrailScopeCompletion) finish(output any) {
	if c.handle == nil {
		return
	}
	handle := *c.handle
	c.handle = nil
	emitGuardrailScopeEnd(c.ctx, handle, output, c.subscribers, c.stack)
	c.pending.complete()
}

// Emits the cancelled end when the evaluation unwound before finishing
func (c *guardrailScopeCompletion) cancelIfUnfinished() {
	c.finish(map[string]any{"allowed": false, "cancelled": true, "error": "guardrail evaluation cancelled"})
}

func emitGuardrailScopeStart(ctx context.Context, name string, parentUUID *uuid.UUID, metadata, input any, subscribers []EventSubscriberFn, stack *ScopeStack) ScopeHandle {
	handle := newScopeHandle(name, types.ScopeTypeGuardrail)
	handle.ParentUUID = cloneUUIDPtr(parentUUID)
	handle.Metadata = metadata
	event := buildScopeStartEvent(handle, input)
	dispatchSanitizedEvent(ctx, event, snapshotEventSanitizers(event, stack), subscribers, stack)
	return handle
}

func emitGuardrailScopeEnd(ctx context.Context, handle ScopeHandle, output any, subscribers []EventSubscriberFn, stack *ScopeStack) {
	event := scopeEvent(
		newBaseEvent(handle.Name, handle.ParentUUID, handle.UUID, endTimestampAfter(handle.StartedAt), output, types.CloneJSON(handle.Metadata)),
		types.ScopeCategoryEnd,
		types.ScopeAttributesToStrings(handle.Attributes),
		types.EventCategoryFromScopeType(handle.ScopeType),
		nil,
	)
	dispatchSanitizedEvent(ctx, event, snapshotEventSanitizers(event, stack), subscribers, stack)
}

func guardrailOutput(reason *string, err error) map[string]any {
	switch {
	case err != nil:
		return map[string]any{"allowed": false, "error": err.Error()}
	case reason != nil:
		return map[string]any{"allowed": false, "rejected": true, "rejection_reason": *reason}
	}
	return map[string]any{"allowed": true, "rejected": false}
}

// Evaluates one conditional guardrail inside its own guardrail scope events
func evaluateConditionalGuardrail(ctx context.Context, entryName string, input any, parentUUID *uuid.UUID, metadata any, subscribers []EventSubscriberFn, panicMessage string, evaluate func() (*string, error)) (*string, error) {
	stack := CurrentScopeStack(ctx)
	handle := emitGuardrailScopeStart(ctx, entryName, parentUUID, types.CloneJSON(metadata), input, subscribers, stack)
	completion := newGuardrailScopeCompletion(ctx, handle, subscribers, stack)
	defer completion.cancelIfUnfinished()
	reason, err, panicked := callRecovering(evaluate)
	if panicked {
		reason, err = nil, internalError(panicMessage)
	}
	completion.finish(guardrailOutput(reason, err))
	return reason, err
}

// Runs tool conditional guardrails until one rejects or fails
func toolConditionalExecutionChain(ctx context.Context, name string, args any, entries []registryRecord[ToolConditionalFn], subscribers []EventSubscriberFn, parentUUID *uuid.UUID, metadata any) (*string, error) {
	for _, entry := range entries {
		input := map[string]any{"kind": "tool_conditional_execution", "target_name": name}
		reason, err := evaluateConditionalGuardrail(ctx, entry.name, input, parentUUID, metadata, subscribers,
			fmt.Sprintf("tool conditional guardrail '%s' panicked", entry.name),
			func() (*string, error) { return entry.payload(ctx, name, types.CloneJSON(args)) })
		if err != nil {
			return nil, err
		}
		if reason != nil {
			return reason, nil
		}
	}
	return nil, nil
}

// Runs LLM conditional guardrails until one rejects or fails
func llmConditionalExecutionChain(ctx context.Context, request types.LLMRequest, entries []registryRecord[LLMConditionalFn], subscribers []EventSubscriberFn, parentUUID *uuid.UUID, metadata any) (*string, error) {
	for _, entry := range entries {
		input := map[string]any{"kind": "llm_conditional_execution"}
		reason, err := evaluateConditionalGuardrail(ctx, entry.name, input, parentUUID, metadata, subscribers,
			fmt.Sprintf("LLM conditional guardrail '%s' panicked", entry.name),
			func() (*string, error) { return entry.payload(ctx, request.Clone()) })
		if err != nil {
			return nil, err
		}
		if reason != nil {
			return reason, nil
		}
	}
	return nil, nil
}

// Rewrites tool arguments through request intercepts honoring break_chain
func toolRequestInterceptChain(ctx context.Context, name string, args any, entries []interceptRecord[ToolInterceptFn]) (any, error) {
	value := args
	for _, entry := range entries {
		input := value
		next, err, panicked := callRecovering(func() (any, error) { return entry.payload.callable(ctx, name, input) })
		if panicked {
			return nil, internalError(fmt.Sprintf("tool request intercept '%s' panicked", entry.name))
		}
		if err != nil {
			return nil, err
		}
		value = next
		if entry.payload.breakChain {
			break
		}
	}
	return value, nil
}

// Rewrites an LLM request through request intercepts, enforcing codec ownership of content
func llmRequestInterceptChain(ctx context.Context, name string, request types.LLMRequest, annotated *types.AnnotatedLLMRequest, entries []interceptRecord[LLMRequestInterceptFn], codecActive bool, recorder *LLMOptimizationRecorder) (*types.LLMRequestInterceptOutcome, error) {
	requestValue := request
	annotatedValue := annotated
	pendingMarks := make([]types.PendingMarkSpec, 0)
	contributions := make([]types.LLMOptimizationContribution, 0)
	for _, entry := range entries {
		var inputContent any
		if codecActive {
			inputContent = types.CloneJSON(requestValue.Content)
		}
		currentRequest, currentAnnotated := requestValue, annotatedValue
		outcome, err, panicked := callRecovering(func() (*types.LLMRequestInterceptOutcome, error) {
			return entry.payload.callable(ctx, name, currentRequest, currentAnnotated)
		})
		if panicked {
			return nil, internalError(fmt.Sprintf("LLM request intercept '%s' panicked", entry.name))
		}
		if err != nil {
			return nil, err
		}
		if outcome == nil {
			outcome = &types.LLMRequestInterceptOutcome{Request: currentRequest, AnnotatedRequest: currentAnnotated}
		}
		if codecActive && !types.JSONEqual(outcome.Request.Content, inputContent) {
			return nil, invalidArgumentError(fmt.Sprintf("LLM request intercept '%s' changed request.content while a request codec is active; modify annotated_request instead", entry.name))
		}
		if codecActive && outcome.AnnotatedRequest == nil {
			return nil, invalidArgumentError(fmt.Sprintf("LLM request intercept '%s' omitted annotated_request while a request codec is active", entry.name))
		}
		requestValue = outcome.Request
		annotatedValue = outcome.AnnotatedRequest
		pendingMarks = append(pendingMarks, outcome.PendingMarks...)
		if recorder != nil {
			recorder.recordAll(outcome.OptimizationContributions)
		} else {
			contributions = append(contributions, outcome.OptimizationContributions...)
		}
		if entry.payload.breakChain {
			break
		}
	}
	return &types.LLMRequestInterceptOutcome{
		Request:                   requestValue,
		AnnotatedRequest:          annotatedValue,
		PendingMarks:              pendingMarks,
		OptimizationContributions: contributions,
	}, nil
}
