package nemorelay

import (
	"context"
	"maps"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

const (
	DynamoSessionIDHeaderKey       = "x-dynamo-session-id"
	DynamoParentSessionIDHeaderKey = "x-dynamo-parent-session-id"
	TraceparentHeaderKey           = "traceparent"
	TracestateHeaderKey            = "tracestate"
)

// Resolves the parent of new work from an explicit handle, the callback active event, or the stack top
func resolveParentUUID(ctx context.Context, parent *ScopeHandle) *uuid.UUID {
	if parent != nil {
		id := parent.UUID
		return &id
	}
	if active := threadActiveEventUUID(ctx); active != nil {
		return active
	}
	id := CurrentScopeStack(ctx).Top().UUID
	return &id
}

// Returns the innermost agent session id and its enclosing agent session id
func resolveAgentSessionIDs(ctx context.Context) (string, *string, bool) {
	scopes := CurrentScopeStack(ctx).Scopes()
	agents := make([]ScopeHandle, 0)
	for index := len(scopes) - 1; index >= 1; index-- {
		if scopes[index].ScopeType == types.ScopeTypeAgent {
			agents = append(agents, scopes[index])
		}
	}
	if len(agents) == 0 {
		return "", nil, false
	}
	current := agentScopeID(agents[0])
	if len(agents) == 1 {
		return current, nil, true
	}
	parent := agentScopeID(agents[1])
	return current, &parent, true
}

func agentScopeID(scope ScopeHandle) string {
	if metadata, ok := scope.Metadata.(map[string]any); ok {
		for _, key := range []string{"codex_subagent_session_id", "subagent_session_id", "subagent_id", "session_id"} {
			if value, ok := metadata[key].(string); ok {
				return value
			}
		}
	}
	return scope.Name
}

// Stamps the Dynamo session routing headers of the enclosing agent scopes
func injectDynamoSessionIDs(ctx context.Context, request *types.LLMRequest) {
	current, parent, ok := resolveAgentSessionIDs(ctx)
	if !ok {
		return
	}
	if request.Headers == nil {
		request.Headers = map[string]any{}
	}
	request.Headers[DynamoSessionIDHeaderKey] = current
	if parent != nil {
		request.Headers[DynamoParentSessionIDHeaderKey] = *parent
	} else {
		delete(request.Headers, DynamoParentSessionIDHeaderKey)
	}
}

// Replaces any trace headers of the request with the given W3C values
func injectTraceContextValue(request *types.LLMRequest, traceparent string, tracestate *string) {
	headers := make(map[string]any, len(request.Headers)+2)
	for key, value := range request.Headers {
		if asciiEqualFold(key, TraceparentHeaderKey) || asciiEqualFold(key, TracestateHeaderKey) {
			continue
		}
		headers[key] = value
	}
	headers[TraceparentHeaderKey] = traceparent
	if tracestate != nil {
		headers[TracestateHeaderKey] = *tracestate
	}
	request.Headers = headers
}

// Injects the trace context of a managed LLM span into its outgoing request
func injectTraceparent(ctx context.Context, request *types.LLMRequest, spanUUID uuid.UUID, causalParentUUID *uuid.UUID) (*W3CTraceContext, error) {
	context, err := traceContextForManagedSpan(ctx, spanUUID, causalParentUUID)
	if err != nil {
		return nil, err
	}
	injectTraceContextValue(request, context.traceparent, context.tracestate)
	return context, nil
}

// Merges an OpenTelemetry status into event metadata
func metadataWithOTelStatus(metadata any, statusCode string, statusMessage *string) any {
	status := map[string]any{"otel.status_code": statusCode}
	if statusCode == "ERROR" && statusMessage != nil {
		status["otel.status_description"] = *statusMessage
	}
	merged := types.MergeJSON(metadata, status)
	if statusCode != "ERROR" {
		if object, ok := merged.(map[string]any); ok {
			if _, present := object["otel.status_description"]; present {
				object = maps.Clone(object)
				delete(object, "otel.status_description")
				merged = object
			}
		}
	}
	return merged
}

// Merges an error status, error type and exception type into event metadata
func metadataWithOTelError(metadata any, err error) any {
	message := err.Error()
	merged := metadataWithOTelStatus(metadata, "ERROR", &message)
	object, ok := merged.(map[string]any)
	if !ok {
		return merged
	}
	object = maps.Clone(object)
	if _, present := object["error.type"]; !present {
		object["error.type"] = otelErrorType(err)
	}
	if exceptionType := errorExceptionType(err); exceptionType != nil {
		if _, present := object["exception.type"]; !present {
			object["exception.type"] = *exceptionType
		}
	}
	return object
}

// Result of running request intercepts and re-encoding through the request codec
type interceptedLLMRequest struct {
	request       types.LLMRequest
	annotated     *types.AnnotatedLLMRequest
	pendingMarks  []types.PendingMarkSpec
	contributions []types.LLMOptimizationContribution
}

// Decodes, intercepts, stamps session headers and re-encodes an LLM request
func runRequestInterceptsWithCodec(ctx context.Context, name string, request types.LLMRequest, codec types.LLMCodec, recorder *LLMOptimizationRecorder) (*interceptedLLMRequest, error) {
	var annotated *types.AnnotatedLLMRequest
	if codec != nil {
		decoded, err := codec.Decode(&request)
		if err != nil {
			return nil, err
		}
		annotated = decoded
	}
	entries := snapshotGuardrails(CurrentScopeStack(ctx), func(r *middlewareRegistries) *sortedRegistry[requestIntercept[LLMRequestInterceptFn]] {
		return &r.llmRequestIntercepts
	}, types.RuntimeRegistrationKindLLMRequestIntercept)
	var baseline *types.AnnotatedLLMRequest
	if len(entries) > 0 && annotated != nil {
		baseline = annotated.Clone()
	}
	outcome, err := llmRequestInterceptChain(ctx, name, request, annotated, entries, codec != nil, recorder)
	if err != nil {
		return nil, err
	}
	intercepted := outcome.Request
	injectDynamoSessionIDs(ctx, &intercepted)
	if codec != nil && outcome.AnnotatedRequest != nil {
		reference := baseline
		if reference == nil {
			reference = outcome.AnnotatedRequest
		}
		encoded, err := types.EncodeOwnedWithBaseline(codec, outcome.AnnotatedRequest, intercepted, reference)
		if err != nil {
			return nil, err
		}
		return &interceptedLLMRequest{request: *encoded, annotated: outcome.AnnotatedRequest, pendingMarks: outcome.PendingMarks, contributions: outcome.OptimizationContributions}, nil
	}
	return &interceptedLLMRequest{request: intercepted, annotated: outcome.AnnotatedRequest, pendingMarks: outcome.PendingMarks, contributions: outcome.OptimizationContributions}, nil
}
