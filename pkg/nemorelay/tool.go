package nemorelay

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/nemorelay/logging"
	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

const toolResultSchema = "nemo.relay.tool_result@1"

type ToolCallParams struct {
	Name       string
	Args       any
	Parent     *ScopeHandle
	Attributes types.ToolAttributes
	Data       any
	Metadata   any
	ToolCallID *string
	Timestamp  *time.Time
}

type ToolCallExecuteParams struct {
	Name       string
	Args       any
	Func       ToolExecuteFn
	Parent     *ScopeHandle
	Attributes types.ToolAttributes
	Data       any
	Metadata   any
	ToolCallID *string
}

type ToolCallEndParams struct {
	Handle    *ToolHandle
	Result    *types.ToolExecutionResult
	Data      any
	Metadata  any
	Timestamp *time.Time
}

// Reports whether a projected tool result envelope marks an error
func projectedToolResultIsError(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if schema, ok := object["schema"].(string); !ok || schema != toolResultSchema {
		return false
	}
	content, present := object["content"]
	if !present {
		return false
	}
	switch content.(type) {
	case nil, string, []any:
	default:
		return false
	}
	isError, ok := object["is_error"].(bool)
	if !ok {
		return false
	}
	if _, present := object["result"]; !present {
		return false
	}
	return isError
}

func toolResultMetadata(metadata any, result any) any {
	if projectedToolResultIsError(result) {
		return metadataWithOTelStatus(metadata, "ERROR", nil)
	}
	return metadata
}

// Sets the tool result annotation on the event category profile when present
func attachToolResultAnnotation(event *types.Event, annotation any) {
	if annotation == nil {
		return
	}
	if profile := event.CategoryProfile(); profile != nil {
		profile.ToolResultAnnotation = annotation
	}
}

func resolveSkillLoads(name string, args any, metadata any) []skillLoad {
	if object, ok := metadata.(map[string]any); ok {
		if handled, ok := object[skillLoadHandledMetadataKey].(bool); ok && handled {
			return nil
		}
	}
	if loads, ok := precomputedSkillLoads(metadata); ok {
		return loads
	}
	return detectSkillLoads(name, args)
}

func toolSanitizeRequestEntries(stack *ScopeStack) []registryRecord[ToolSanitizeFn] {
	return snapshotGuardrails(stack, fieldToolSanitizeRequest, types.RuntimeRegistrationKindToolSanitizeRequestGuardrail)
}

func toolSanitizeResponseEntries(stack *ScopeStack) []registryRecord[ToolSanitizeFn] {
	return snapshotGuardrails(stack, fieldToolSanitizeResponse, types.RuntimeRegistrationKindToolSanitizeResponseGuardrail)
}

// Builds the deferred transform that sanitizes partial tool completion marks
func completionMarkTransform(event *types.Event, stack *ScopeStack) (eventTransformFn, error) {
	if event.Name() != "tool_end_without_start" {
		return nil, nil
	}
	data := event.Data()
	if data == nil {
		return nil, invalidArgumentError("tool completion marks require data.tool_name")
	}
	object, _ := data.(map[string]any)
	toolName, ok := object["tool_name"].(string)
	if !ok || strings.TrimSpace(toolName) == "" {
		return nil, invalidArgumentError("tool completion marks require a nonblank string data.tool_name")
	}
	args := object["arguments"]
	result := object["result"]
	requests := toolSanitizeRequestEntries(stack)
	responses := toolSanitizeResponseEntries(stack)
	return func(ctx context.Context, event *types.Event) *types.Event {
		sanitizedArgs, argsKept := toolSanitizeChain(ctx, toolName, args, requests, false)
		var sanitizedResult any
		if argsKept {
			sanitizedResult, _ = toolSanitizeChain(ctx, toolName, result, responses, true)
		}
		fields := event.SanitizeFields()
		if fieldsData, ok := fields.Data.(map[string]any); ok {
			updated := make(map[string]any, len(fieldsData)+2)
			for key, value := range fieldsData {
				updated[key] = value
			}
			updated["arguments"] = sanitizedArgs
			updated["result"] = sanitizedResult
			fields.Data = updated
		}
		event.ApplySanitizeFields(fields)
		return event
	}, nil
}

// Opens a tool span, queuing its sanitized start event and any skill load marks
func toolCallWithSubscribers(ctx context.Context, params ToolCallParams) (*ToolHandle, []EventSubscriberFn) {
	parentUUID := resolveParentUUID(ctx, params.Parent)
	stack := CurrentScopeStack(ctx)
	entries := toolSanitizeRequestEntries(stack)
	subscribers := snapshotEventSubscribers(stack.collectScopeLocalSubscribers())
	args := types.CloneJSON(params.Args)
	metadata := types.CloneJSON(params.Metadata)
	loads := resolveSkillLoads(params.Name, args, metadata)
	handle := &ToolHandle{
		UUID:       newUUID(),
		StartedAt:  timestampOrNow(params.Timestamp),
		Name:       params.Name,
		Data:       types.CloneJSON(params.Data),
		Metadata:   metadata,
		Attributes: params.Attributes,
		ParentUUID: parentUUID,
		ToolCallID: cloneStringPtr(params.ToolCallID),
	}
	event := buildToolStartEvent(handle, nil)
	marks := make([]*types.Event, 0, len(loads))
	for _, load := range loads {
		handleUUID := handle.UUID
		marks = append(marks, buildMarkEvent("skill.load", &handleUUID, handle.StartedAt,
			map[string]any{"skill_name": load.name},
			nil,
			map[string]any{"skill_load_source": string(load.source), "tool_name": handle.Name},
			nil, nil))
	}
	toolName := handle.Name
	dispatchTransformedEvent(ctx, event, func(ctx context.Context, event *types.Event) *types.Event {
		sanitized, _ := toolSanitizeChain(ctx, toolName, args, entries, false)
		fields := event.SanitizeFields()
		fields.Data = sanitized
		event.ApplySanitizeFields(fields)
		return event
	}, snapshotEventSanitizers(event, stack), subscribers, stack)
	for _, mark := range marks {
		dispatchSanitizedEvent(ctx, mark, snapshotEventSanitizers(mark, stack), subscribers, stack)
	}
	return handle, subscribers
}

// Opens a manually managed tool span
func ToolCall(ctx context.Context, params ToolCallParams) (*ToolHandle, error) {
	handle, _ := toolCallWithSubscribers(ctx, params)
	return handle, nil
}

// Closes a manually managed tool span with its sanitized result
func ToolCallEnd(ctx context.Context, params ToolCallEndParams) error {
	return toolCallEndWithPendingMarks(ctx, params, nil, nil)
}

func toolCallEndWithPendingMarks(ctx context.Context, params ToolCallEndParams, pendingMarks []types.PendingMarkSpec, lifecycleSubscribers []EventSubscriberFn) error {
	stack := CurrentScopeStack(ctx)
	entries := toolSanitizeResponseEntries(stack)
	subscribers := lifecycleSubscribers
	if subscribers == nil {
		subscribers = snapshotEventSubscribers(stack.collectScopeLocalSubscribers())
	}
	result := types.ToolExecutionResult{}
	if params.Result != nil {
		result = types.ToolExecutionResult{Result: types.CloneJSON(params.Result.Result), Annotation: types.CloneJSON(params.Result.Annotation)}
	}
	metadata := toolResultMetadata(types.CloneJSON(params.Metadata), result.Result)
	event := buildToolEndEvent(params.Handle, nil, metadata, params.Timestamp)
	marks := make([]*types.Event, 0, len(pendingMarks))
	for index, mark := range pendingMarks {
		timestamp := event.Timestamp().Add(time.Duration(index+1) * time.Microsecond)
		markMetadata, err := metadataWithLogSeverity(types.CloneJSON(mark.Metadata), mark.Severity)
		if err != nil {
			logging.Warn("nemo_relay.observability", "Tool pending mark was dropped because its severity metadata is invalid: "+err.Error(), map[string]any{
				"event":              "tool_pending_mark_dropped",
				"tool_name":          params.Handle.Name,
				"tool_uuid":          params.Handle.UUID.String(),
				"pending_mark_index": index,
				"pending_mark_name":  mark.Name,
			})
			continue
		}
		handleUUID := params.Handle.UUID
		var profile *types.CategoryProfile
		if mark.CategoryProfile != nil {
			profile = mark.CategoryProfile.Clone()
		}
		marks = append(marks, buildMarkEvent(mark.Name, &handleUUID, timestamp, types.CloneJSON(mark.Data), mark.DataSchema, markMetadata, mark.Category, profile))
	}
	toolName := params.Handle.Name
	fallback := types.CloneJSON(params.Data)
	dispatchTransformedEvent(ctx, event, func(ctx context.Context, event *types.Event) *types.Event {
		sanitized, kept := toolSanitizeChain(ctx, toolName, result.Result, entries, true)
		fields := event.SanitizeFields()
		switch {
		case !kept:
			fields.Data = nil
		case sanitized == nil:
			fields.Data = fallback
		default:
			fields.Data = sanitized
		}
		event.ApplySanitizeFields(fields)
		attachToolResultAnnotation(event, result.Annotation)
		return event
	}, snapshotEventSanitizers(event, stack), subscribers, stack)
	for _, mark := range marks {
		dispatchSanitizedEvent(ctx, mark, snapshotEventSanitizers(mark, stack), subscribers, stack)
	}
	return nil
}

func emitToolEndWithoutOutput(ctx context.Context, handle *ToolHandle, metadata any, subscribers []EventSubscriberFn, stack *ScopeStack) {
	event := buildToolEndEvent(handle, types.CloneJSON(handle.Data), metadata, nil)
	queueSanitizedEvent(ctx, event, subscribers, stack)
}

// Emits a cancelled tool end unless the managed execution completed normally
type managedToolCompletion struct {
	ctx         context.Context
	handle      *ToolHandle
	metadata    any
	subscribers []EventSubscriberFn
	stack       *ScopeStack
	pending     *pendingPublication
}

func newManagedToolCompletion(ctx context.Context, handle *ToolHandle, metadata any, subscribers []EventSubscriberFn, stack *ScopeStack) *managedToolCompletion {
	completion := &managedToolCompletion{ctx: ctx, handle: handle, metadata: metadata, subscribers: subscribers, stack: stack}
	if len(subscribers) > 0 {
		completion.pending = registerPendingPublication()
	}
	return completion
}

func (c *managedToolCompletion) disarm() {
	c.handle = nil
	c.pending.complete()
}

// Publishes the cancelled end when execution unwound before reaching a terminal event
func (c *managedToolCompletion) cancelIfArmed() {
	if c.handle == nil {
		return
	}
	handle := c.handle
	c.handle = nil
	message := "tool execution cancelled"
	emitToolEndWithoutOutput(c.ctx, handle, metadataWithOTelStatus(c.metadata, "ERROR", &message), c.subscribers, c.stack)
	c.pending.complete()
}

func toolOutcomeFromResult(result *types.ToolExecutionResult) *types.ToolExecutionInterceptOutcome {
	if result == nil {
		return &types.ToolExecutionInterceptOutcome{}
	}
	return &types.ToolExecutionInterceptOutcome{Result: result.Result, Annotation: result.Annotation}
}

func toolResultFromOutcome(outcome *types.ToolExecutionInterceptOutcome) *types.ToolExecutionResult {
	return &types.ToolExecutionResult{Result: outcome.Result, Annotation: outcome.Annotation}
}

type toolOutcomeNextFn func(ctx context.Context, args any) (*types.ToolExecutionInterceptOutcome, error)

type sequencedMarks struct {
	sequence int
	marks    []types.PendingMarkSpec
}

// Wraps the tool function in every execution intercept, outermost first by priority
func buildToolExecutionChain(stack *ScopeStack, execution ToolExecutionContext, defaultFn ToolExecutionNextFn) toolOutcomeNextFn {
	callables := snapshotExecutionIntercepts(stack, fieldToolExecutionIntercepts, types.RuntimeRegistrationKindToolExecutionIntercept)
	next := toolOutcomeNextFn(func(ctx context.Context, args any) (*types.ToolExecutionInterceptOutcome, error) {
		result, err := defaultFn(ctx, args)
		if err != nil {
			return nil, err
		}
		return toolOutcomeFromResult(result), nil
	})
	for _, callable := range slices.Backward(callables) {
		currentNext := next
		next = func(ctx context.Context, args any) (*types.ToolExecutionInterceptOutcome, error) {
			lease := captureMiddlewareContinuationLease(ctx)
			var mu sync.Mutex
			sequence := 0
			downstream := make([]sequencedMarks, 0)
			rawNext := ToolExecutionNextFn(func(callCtx context.Context, args any) (*types.ToolExecutionResult, error) {
				mu.Lock()
				current := sequence
				sequence++
				mu.Unlock()
				invocation, err := lease.begin(callCtx)
				if err != nil {
					return nil, err
				}
				outcome, err := invokeContinuation(callCtx, lease, invocation, func(runCtx context.Context) (*types.ToolExecutionInterceptOutcome, error) {
					return currentNext(runCtx, args)
				})
				if err != nil {
					return nil, err
				}
				mu.Lock()
				downstream = append(downstream, sequencedMarks{sequence: current, marks: outcome.PendingMarks})
				mu.Unlock()
				return toolResultFromOutcome(outcome), nil
			})
			executionContext := execution
			executionContext.Args = args
			outcome, err := func() (*types.ToolExecutionInterceptOutcome, error) {
				defer lease.revoke()
				return callable(ctx, executionContext, rawNext)
			}()
			if err != nil {
				return nil, err
			}
			if outcome == nil {
				outcome = &types.ToolExecutionInterceptOutcome{}
			}
			mu.Lock()
			batches := slices.Clone(downstream)
			downstream = downstream[:0]
			mu.Unlock()
			slices.SortStableFunc(batches, func(left, right sequencedMarks) int { return left.sequence - right.sequence })
			marks := make([]types.PendingMarkSpec, 0)
			for _, batch := range batches {
				marks = append(marks, batch.marks...)
			}
			result := *outcome
			result.PendingMarks = append(marks, outcome.PendingMarks...)
			return &result, nil
		}
	}
	return next
}

// Runs a tool through guardrails, intercepts, execution intercepts and lifecycle events
func ToolCallExecute(ctx context.Context, params ToolCallExecuteParams) (*types.ToolExecutionResult, error) {
	args := types.CloneJSON(params.Args)
	metadata := types.CloneJSON(params.Metadata)
	data := types.CloneJSON(params.Data)
	stack := CurrentScopeStack(ctx)
	conditional := snapshotGuardrails(stack, fieldToolConditionalExecution, types.RuntimeRegistrationKindToolConditionalExecutionGuardrail)
	guardrailSubscribers := snapshotEventSubscribers(stack.collectScopeLocalSubscribers())
	reason, err := toolConditionalExecutionChain(ctx, params.Name, args, conditional, guardrailSubscribers, resolveParentUUID(ctx, params.Parent), types.CloneJSON(metadata))
	if err != nil {
		return nil, err
	}
	if reason != nil {
		_ = Event(ctx, EmitMarkEventParams{
			Name:     params.Name,
			Parent:   params.Parent,
			Data:     map[string]any{"rejected": true, "rejection_reason": *reason},
			Metadata: metadata,
		})
		return nil, guardrailRejectedError(*reason)
	}
	intercepts := snapshotGuardrails(stack, fieldToolRequestIntercepts, types.RuntimeRegistrationKindToolRequestIntercept)
	interceptedArgs, err := toolRequestInterceptChain(ctx, params.Name, args, intercepts)
	if err != nil {
		return nil, err
	}
	handle, lifecycleSubscribers := toolCallWithSubscribers(ctx, ToolCallParams{
		Name:       params.Name,
		Args:       interceptedArgs,
		Parent:     params.Parent,
		Attributes: params.Attributes,
		Data:       data,
		Metadata:   metadata,
		ToolCallID: params.ToolCallID,
	})
	lifecycleStack := CurrentScopeStack(ctx)
	completion := newManagedToolCompletion(ctx, handle, types.CloneJSON(metadata), lifecycleSubscribers, lifecycleStack)
	defer completion.cancelIfArmed()
	traceContext, err := traceContextForManagedSpan(ctx, handle.UUID, handle.ParentUUID)
	if err != nil {
		emitToolEndWithoutOutput(ctx, handle, metadataWithOTelError(metadata, err), lifecycleSubscribers, lifecycleStack)
		completion.disarm()
		return nil, err
	}
	executionCtx := withActiveEventTraceContext(ctx, handle.UUID, traceContext)
	execution := ToolExecutionContext{ToolName: params.Name, ToolCallID: cloneStringPtr(handle.ToolCallID)}
	chain := buildToolExecutionChain(CurrentScopeStack(executionCtx), execution, params.Func)
	outcome, err := chain(executionCtx, interceptedArgs)
	if err != nil {
		emitToolEndWithoutOutput(ctx, handle, metadataWithOTelError(metadata, err), lifecycleSubscribers, lifecycleStack)
		completion.disarm()
		return nil, err
	}
	pendingMarks := outcome.PendingMarks
	result := toolResultFromOutcome(outcome)
	if err := toolCallEndWithPendingMarks(ctx, ToolCallEndParams{
		Handle:   handle,
		Result:   result,
		Data:     data,
		Metadata: metadataWithOTelStatus(metadata, "OK", nil),
	}, pendingMarks, lifecycleSubscribers); err != nil {
		return nil, err
	}
	completion.disarm()
	return result, nil
}

// Runs only the tool request intercepts and returns the rewritten arguments
func ToolRequestIntercepts(ctx context.Context, name string, args any) (any, error) {
	entries := snapshotGuardrails(CurrentScopeStack(ctx), fieldToolRequestIntercepts, types.RuntimeRegistrationKindToolRequestIntercept)
	return toolRequestInterceptChain(ctx, name, types.CloneJSON(args), entries)
}

// Runs only the tool conditional guardrails and fails when one rejects
func ToolConditionalExecution(ctx context.Context, name string, args any) error {
	return ToolConditionalExecutionWithEventContext(ctx, name, args, nil, nil)
}

// Runs the tool conditional guardrails with an explicit guardrail scope parent and metadata
func ToolConditionalExecutionWithEventContext(ctx context.Context, name string, args any, parentUUID *uuid.UUID, metadata any) error {
	stack := CurrentScopeStack(ctx)
	entries := snapshotGuardrails(stack, fieldToolConditionalExecution, types.RuntimeRegistrationKindToolConditionalExecutionGuardrail)
	subscribers := snapshotEventSubscribers(stack.collectScopeLocalSubscribers())
	resolved := parentUUID
	if resolved == nil {
		resolved = resolveParentUUID(ctx, nil)
	}
	reason, err := toolConditionalExecutionChain(ctx, name, types.CloneJSON(args), entries, subscribers, resolved, types.CloneJSON(metadata))
	if err != nil {
		return err
	}
	if reason != nil {
		return guardrailRejectedError(*reason)
	}
	return nil
}
