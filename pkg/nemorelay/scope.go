package nemorelay

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

const CompactionEventName = "compaction"

// Receives agent scope lifecycle so resource metrics can target open agents
type AgentScopeObserver interface {
	RegisterAgentScope(scope ScopeHandle, stack *ScopeStack)
	UnregisterAgentScope(id uuid.UUID)
}

var (
	agentScopeObserverMu sync.RWMutex
	agentScopeObserver   AgentScopeObserver
)

// Installs the observer notified when agent scopes open and close
func SetAgentScopeObserver(observer AgentScopeObserver) {
	agentScopeObserverMu.Lock()
	agentScopeObserver = observer
	agentScopeObserverMu.Unlock()
}

func currentAgentScopeObserver() AgentScopeObserver {
	agentScopeObserverMu.RLock()
	defer agentScopeObserverMu.RUnlock()
	return agentScopeObserver
}

type PushScopeParams struct {
	Name       string
	ScopeType  types.ScopeType
	Parent     *ScopeHandle
	Attributes types.ScopeAttributes
	Data       any
	Metadata   any
	Input      any
	Timestamp  *time.Time
}

type PopScopeParams struct {
	HandleUUID uuid.UUID
	Output     any
	Metadata   any
	Timestamp  *time.Time
}

type EmitMarkEventParams struct {
	Name            string
	Parent          *ScopeHandle
	Data            any
	DataSchema      *types.DataSchema
	Metadata        any
	Severity        *types.LogSeverity
	Category        *types.EventCategory
	CategoryProfile *types.CategoryProfile
	Timestamp       *time.Time
}

type EmitMetricEventParams struct {
	Name         string
	Measurements []types.MetricMeasurement
	Parent       *ScopeHandle
	Metadata     any
	Timestamp    *time.Time
}

// Adds the log severity key to mark metadata, which must then be an object
func metadataWithLogSeverity(metadata any, severity *types.LogSeverity) (any, error) {
	if severity == nil {
		return metadata, nil
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	object, ok := metadata.(map[string]any)
	if !ok {
		return nil, invalidArgumentError("mark metadata must be a JSON object when severity is provided")
	}
	object = maps.Clone(object)
	object[types.LogSeverityMetadataKey] = severity.String()
	return object, nil
}

// Returns the innermost open scope of the caller's stack
func GetHandle(ctx context.Context) (*ScopeHandle, error) {
	top := CurrentScopeStack(ctx).Top()
	return &top, nil
}

// Opens a scope on the caller's stack and publishes its start event
func PushScope(ctx context.Context, params PushScopeParams) (*ScopeHandle, error) {
	parentUUID := resolveParentUUID(ctx, params.Parent)
	stack := CurrentScopeStack(ctx)
	subscribers := snapshotEventSubscribers(stack.collectScopeLocalSubscribers())
	handle := ScopeHandle{
		UUID:       newUUID(),
		StartedAt:  timestampOrNow(params.Timestamp),
		ScopeType:  params.ScopeType,
		Name:       params.Name,
		Data:       types.CloneJSON(params.Data),
		Metadata:   types.CloneJSON(params.Metadata),
		Attributes: params.Attributes,
		ParentUUID: parentUUID,
	}
	event := buildScopeStartEvent(handle, types.CloneJSON(params.Input))
	stack.Push(handle)
	if handle.ScopeType == types.ScopeTypeAgent {
		if observer := currentAgentScopeObserver(); observer != nil {
			observer.RegisterAgentScope(handle.clone(), stack)
		}
	}
	dispatchSanitizedEvent(ctx, event, snapshotEventSanitizers(event, stack), subscribers, stack)
	result := handle.clone()
	return &result, nil
}

// Closes the top scope and publishes its end event
func PopScope(ctx context.Context, params PopScopeParams) error {
	_, err := popScope(ctx, params, false)
	return err
}

// Closes the top scope and returns a receipt completed once subscribers saw its end event
func PopScopeWithSubscriberDelivery(ctx context.Context, params PopScopeParams) (*SubscriberDelivery, error) {
	delivery, err := popScope(ctx, params, true)
	if err != nil {
		return nil, err
	}
	if delivery == nil {
		return nil, internalError("tracked scope pop did not create a subscriber delivery receipt")
	}
	return delivery, nil
}

func popScope(ctx context.Context, params PopScopeParams, track bool) (*SubscriberDelivery, error) {
	stack := CurrentScopeStack(ctx)
	top := stack.Top()
	if top.UUID != params.HandleUUID {
		if _, found := stack.Find(params.HandleUUID); found {
			return nil, invalidArgumentError("scope handle is not at the top of the stack")
		}
		return nil, notFoundError("scope handle not found")
	}
	subscribers := snapshotEventSubscribers(stack.collectScopeLocalSubscribers())
	event := buildScopeEndEvent(top, types.CloneJSON(params.Output), types.CloneJSON(params.Metadata), params.Timestamp)
	sanitizers := snapshotEventSanitizers(event, stack)
	publicationStack := stack.Snapshot()
	if _, err := stack.Remove(params.HandleUUID); err != nil {
		return nil, err
	}
	if top.ScopeType == types.ScopeTypeAgent {
		if observer := currentAgentScopeObserver(); observer != nil {
			observer.UnregisterAgentScope(top.UUID)
		}
	}
	if track {
		return dispatchSanitizedEventWithDelivery(ctx, event, sanitizers, subscribers, publicationStack)
	}
	dispatchSanitizedEvent(ctx, event, sanitizers, subscribers, publicationStack)
	return nil, nil
}

func isPartialCompletionMark(name string, metadata any) bool {
	switch name {
	case "agent_end_without_start", "turn_end_without_start", "subagent_end_without_start", "tool_end_without_start":
	default:
		return false
	}
	object, ok := metadata.(map[string]any)
	if !ok {
		return false
	}
	observed, ok := object["start_observed"].(bool)
	return ok && !observed
}

// Publishes a mark event under the resolved parent scope
func Event(ctx context.Context, params EmitMarkEventParams) error {
	metadata, err := metadataWithLogSeverity(types.CloneJSON(params.Metadata), params.Severity)
	if err != nil {
		return err
	}
	stack := CurrentScopeStack(ctx)
	var parentUUID *uuid.UUID
	if !(isPartialCompletionMark(params.Name, metadata) && params.Parent == nil && stack.Top().UUID == stack.RootUUID()) {
		parentUUID = resolveParentUUID(ctx, params.Parent)
	}
	subscribers := snapshotEventSubscribers(stack.collectScopeLocalSubscribers())
	if params.Name == CompactionEventName {
		stack.markAgentFresh(parentUUID)
	}
	var profile *types.CategoryProfile
	if params.CategoryProfile != nil {
		profile = params.CategoryProfile.Clone()
	}
	event := buildMarkEvent(params.Name, parentUUID, timestampOrNow(params.Timestamp), types.CloneJSON(params.Data), params.DataSchema, metadata, params.Category, profile)
	sanitizers := snapshotEventSanitizers(event, stack)
	transform, err := completionMarkTransform(event, stack)
	if err != nil {
		return err
	}
	if transform != nil {
		dispatchTransformedEvent(ctx, event, transform, sanitizers, subscribers, stack)
		return nil
	}
	dispatchSanitizedEvent(ctx, event, sanitizers, subscribers, stack)
	return nil
}

func metricEnvelopeData(measurements []types.MetricMeasurement) (any, error) {
	envelope := types.MetricEnvelope{Measurements: measurements}
	if err := envelope.Validate(); err != nil {
		return nil, invalidArgumentError(err.Error())
	}
	data, err := types.ToJSONValue(envelope)
	if err != nil {
		return nil, invalidArgumentError(fmt.Sprintf("metric envelope could not be serialized: %v", err))
	}
	return data, nil
}

func metricDataSchema() *types.DataSchema {
	return &types.DataSchema{Name: types.MetricDataSchemaName, Version: types.MetricDataSchemaVersion}
}

// Publishes validated metric measurements as a mark event
func Metric(ctx context.Context, params EmitMetricEventParams) error {
	data, err := metricEnvelopeData(params.Measurements)
	if err != nil {
		return err
	}
	return Event(ctx, EmitMarkEventParams{
		Name:       params.Name,
		Parent:     params.Parent,
		Data:       data,
		DataSchema: metricDataSchema(),
		Metadata:   params.Metadata,
		Timestamp:  params.Timestamp,
	})
}

// Publishes measurements under a scope of another stack, reporting false when that scope closed
func MetricOnScopeStack(ctx context.Context, name string, measurements []types.MetricMeasurement, parent ScopeHandle, timestamp time.Time, stack *ScopeStack) (bool, error) {
	target, ok := stack.SnapshotThroughScope(parent.UUID)
	if !ok {
		return false, nil
	}
	data, err := metricEnvelopeData(measurements)
	if err != nil {
		return false, err
	}
	subscribers := snapshotEventSubscribers(target.collectScopeLocalSubscribers())
	parentUUID := parent.UUID
	event := buildMarkEvent(name, &parentUUID, timestamp.UTC(), data, metricDataSchema(), nil, nil, nil)
	dispatchSanitizedEvent(ctx, event, snapshotEventSanitizers(event, target), subscribers, target)
	return true, nil
}
