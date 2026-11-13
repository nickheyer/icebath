package nemorelay

import (
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

// Open scope on a scope stack
type ScopeHandle struct {
	UUID       uuid.UUID
	StartedAt  time.Time
	ScopeType  types.ScopeType
	Name       string
	Data       any
	Metadata   any
	Attributes types.ScopeAttributes
	ParentUUID *uuid.UUID
}

func newScopeHandle(name string, scopeType types.ScopeType) ScopeHandle {
	return ScopeHandle{UUID: newUUID(), StartedAt: nowUTC(), ScopeType: scopeType, Name: name}
}

func (h ScopeHandle) clone() ScopeHandle {
	h.ParentUUID = cloneUUIDPtr(h.ParentUUID)
	return h
}

// In-flight tool call
type ToolHandle struct {
	UUID       uuid.UUID
	StartedAt  time.Time
	Name       string
	Data       any
	Metadata   any
	Attributes types.ToolAttributes
	ParentUUID *uuid.UUID
	ToolCallID *string
}

// In-flight LLM call with its optimization recorder and the stack it was created on
type LLMHandle struct {
	UUID                 uuid.UUID
	StartedAt            time.Time
	Name                 string
	Data                 any
	Metadata             any
	Attributes           types.LLMAttributes
	ParentUUID           *uuid.UUID
	ModelName            *string
	OptimizationRecorder *LLMOptimizationRecorder
	capturedScopeStack   *ScopeStack
}

// Returns the scope stack the handle was created on
func (h *LLMHandle) CapturedScopeStack() *ScopeStack {
	return h.capturedScopeStack
}

func (h *LLMHandle) clone() *LLMHandle {
	cloned := *h
	cloned.ParentUUID = cloneUUIDPtr(h.ParentUUID)
	cloned.ModelName = cloneStringPtr(h.ModelName)
	return &cloned
}

func newUUID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}

func nowUTC() time.Time {
	return time.Now().UTC()
}

// Picks an end timestamp strictly after the start even when clocks tie
func endTimestampAfter(startedAt time.Time) time.Time {
	now := nowUTC()
	floor := startedAt.Add(time.Microsecond)
	if now.After(floor) {
		return now
	}
	return floor
}

func timestampOrNow(timestamp *time.Time) time.Time {
	if timestamp != nil {
		return timestamp.UTC()
	}
	return nowUTC()
}

func newBaseEvent(name string, parentUUID *uuid.UUID, id uuid.UUID, timestamp time.Time, data, metadata any) types.BaseEvent {
	base := types.NewBaseEvent(name)
	base.ParentUUID = cloneUUIDPtr(parentUUID)
	base.UUID = id
	base.Timestamp = timestamp
	base.Data = data
	base.Metadata = metadata
	return base
}

func scopeEvent(base types.BaseEvent, category types.ScopeCategory, attributes []string, eventCategory types.EventCategory, profile *types.CategoryProfile) *types.Event {
	return &types.Event{Scope: types.NewScopeEvent(base, category, attributes, eventCategory, profile)}
}

func markEvent(base types.BaseEvent, category *types.EventCategory, profile *types.CategoryProfile) *types.Event {
	return &types.Event{Mark: types.NewMarkEvent(base, category, profile)}
}

func buildScopeStartEvent(handle ScopeHandle, data any) *types.Event {
	return scopeEvent(
		newBaseEvent(handle.Name, handle.ParentUUID, handle.UUID, handle.StartedAt, data, types.CloneJSON(handle.Metadata)),
		types.ScopeCategoryStart,
		types.ScopeAttributesToStrings(handle.Attributes),
		types.EventCategoryFromScopeType(handle.ScopeType),
		nil,
	)
}

func buildScopeEndEvent(handle ScopeHandle, data, metadata any, timestamp *time.Time) *types.Event {
	end := endTimestampAfter(handle.StartedAt)
	if timestamp != nil {
		end = timestamp.UTC()
	}
	return scopeEvent(
		newBaseEvent(handle.Name, handle.ParentUUID, handle.UUID, end, data, types.MergeJSON(types.CloneJSON(handle.Metadata), metadata)),
		types.ScopeCategoryEnd,
		types.ScopeAttributesToStrings(handle.Attributes),
		types.EventCategoryFromScopeType(handle.ScopeType),
		nil,
	)
}

func toolProfile(handle *ToolHandle) *types.CategoryProfile {
	return &types.CategoryProfile{ToolCallID: cloneStringPtr(handle.ToolCallID)}
}

func buildToolStartEvent(handle *ToolHandle, data any) *types.Event {
	return scopeEvent(
		newBaseEvent(handle.Name, handle.ParentUUID, handle.UUID, handle.StartedAt, data, types.CloneJSON(handle.Metadata)),
		types.ScopeCategoryStart,
		types.ToolAttributesToStrings(handle.Attributes),
		types.EventCategoryTool(),
		toolProfile(handle),
	)
}

func buildToolEndEvent(handle *ToolHandle, data, metadata any, timestamp *time.Time) *types.Event {
	end := endTimestampAfter(handle.StartedAt)
	if timestamp != nil {
		end = timestamp.UTC()
	}
	return scopeEvent(
		newBaseEvent(handle.Name, handle.ParentUUID, handle.UUID, end, data, types.MergeJSON(types.CloneJSON(handle.Metadata), metadata)),
		types.ScopeCategoryEnd,
		types.ToolAttributesToStrings(handle.Attributes),
		types.EventCategoryTool(),
		toolProfile(handle),
	)
}

func buildLLMStartEvent(handle *LLMHandle, data any, annotatedRequest *types.AnnotatedLLMRequest) *types.Event {
	return scopeEvent(
		newBaseEvent(handle.Name, handle.ParentUUID, handle.UUID, handle.StartedAt, data, types.CloneJSON(handle.Metadata)),
		types.ScopeCategoryStart,
		types.LLMAttributesToStrings(handle.Attributes),
		types.EventCategoryLLM(),
		&types.CategoryProfile{ModelName: cloneStringPtr(handle.ModelName), AnnotatedRequest: annotatedRequest},
	)
}

func buildLLMEndEvent(handle *LLMHandle, data, metadata any, annotatedResponse *types.AnnotatedLLMResponse, timeToFirstChunk *float64, timestamp *time.Time) *types.Event {
	end := endTimestampAfter(handle.StartedAt)
	if timestamp != nil {
		end = timestamp.UTC()
	}
	return scopeEvent(
		newBaseEvent(handle.Name, handle.ParentUUID, handle.UUID, end, data, types.MergeJSON(types.CloneJSON(handle.Metadata), metadata)),
		types.ScopeCategoryEnd,
		types.LLMAttributesToStrings(handle.Attributes),
		types.EventCategoryLLM(),
		&types.CategoryProfile{ModelName: cloneStringPtr(handle.ModelName), AnnotatedResponse: annotatedResponse, TimeToFirstChunk: timeToFirstChunk},
	)
}

// Builds a mark event parented under a scope
func buildMarkEvent(name string, parentUUID *uuid.UUID, timestamp time.Time, data any, schema *types.DataSchema, metadata any, category *types.EventCategory, profile *types.CategoryProfile) *types.Event {
	base := newBaseEvent(name, parentUUID, newUUID(), timestamp, data, metadata)
	base.DataSchema = schema
	return markEvent(base, category, profile)
}
