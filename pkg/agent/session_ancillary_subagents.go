package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Hops an ancestor walk takes before it gives up, the registry default
const maxAncestorHops = 8

// Route the runner evicted this agent from on a provider fallback, true without a route, nil when never evicted
func (a *AIAgent) EvictedOnFallback() any { return a.evictedOnFallback }

// Records the provider fallback eviction of this cached agent from its route
func (a *AIAgent) MarkEvictedOnFallback(sessionKey string) {
	if sessionKey != "" {
		a.evictedOnFallback = sessionKey
		return
	}
	a.evictedOnFallback = true
}

type fallbackEvicted interface{ EvictedOnFallback() any }

type sessionIdentified interface{ SessionID() string }

type sessionStoreHolder interface{ SessionStore() SessionStore }

type gatewayKeyed interface{ GatewaySessionKey() string }

type steerable interface{ Steer(text string) bool }

type dbPathHolder interface{ Path() string }

type compressionChildReader interface {
	GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error)
	IsCompressionChildRow(ctx context.Context, child *pyjson.Object) (bool, error)
}

type compressionLineageReader interface {
	GetCompressionLineage(ctx context.Context, sessionID string) ([]string, error)
}

// Session id attribute of an agent value, nil for none
func agentSessionID(agent any) any {
	if held, ok := agent.(sessionIdentified); ok && !isNilAgent(agent) {
		if id := held.SessionID(); id != "" {
			return id
		}
	}
	return nil
}

// Session database attribute of an agent value, nil for none
func agentSessionDB(agent any) any {
	if held, ok := agent.(sessionStoreHolder); ok && !isNilAgent(agent) {
		if store := held.SessionStore(); store != nil && !nilValue(store) {
			return store
		}
	}
	return nil
}

// Gateway session key attribute of an agent value, nil for none
func agentGatewayKey(agent any) any {
	if held, ok := agent.(gatewayKeyed); ok && !isNilAgent(agent) {
		if key := held.GatewaySessionKey(); key != "" {
			return key
		}
	}
	return nil
}

// Whether an ancestor of the agent was evicted on fallback from this conversation's route and database
func hasFallbackEvictedAncestor(agent any, targetSids map[string]bool, expectedDB any, route any) bool {
	if len(targetSids) == 0 {
		return false
	}
	current := agent
	for range maxAncestorHops {
		var parent any
		if p, ok := current.(delegateParented); ok && !isNilAgent(current) {
			parent = p.delegateParentAgent()
		}
		current = parent
		if isNilAgent(current) {
			return false
		}
		var marker any
		if held, ok := current.(fallbackEvicted); ok {
			marker = held.EvictedOnFallback()
		}
		text, isStr := marker.(string)
		if !(marker == true || (isStr && text != "")) {
			continue
		}
		if id, ok := agentSessionID(current).(string); !ok || !targetSids[id] {
			continue
		}
		db := agentSessionDB(current)
		if (expectedDB != nil || db != nil) && !pyrepr.Is(db, expectedDB) {
			continue
		}
		if route != nil && !pyjson.Equal(marker, route) {
			continue
		}
		if key := agentGatewayKey(current); route == nil && isStr && key != nil && key != text {
			continue
		}
		return true
	}
	return false
}

// Copies of the live subagent records under a parent agent or this conversation's lineage
func SubagentRecords(ctx context.Context, parent any, sessionID string, sessionDB any, route any, includeFallbackEvicted bool) ([]map[string]any, error) {
	var targetSid string
	if includeFallbackEvicted {
		targetSid = sessionID
		if targetSid == "" {
			targetSid, _ = agentSessionID(parent).(string)
		}
	}
	if isNilAgent(parent) && targetSid == "" {
		return []map[string]any{}, nil
	}
	expectedDB := sessionDB
	if expectedDB == nil {
		expectedDB = agentSessionDB(parent)
	}
	effectiveRoute := route
	if effectiveRoute == nil {
		effectiveRoute = agentGatewayKey(parent)
	}
	targetSids, err := compressionLineageOf(ctx, expectedDB, targetSid)
	if err != nil {
		return nil, err
	}
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	out := []map[string]any{}
	for _, record := range activeSubagents.values() {
		child := record["agent"]
		if (!isNilAgent(parent) && isDescendantOf(child, parent, maxAncestorHops)) ||
			hasFallbackEvictedAncestor(child, targetSids, expectedDB, effectiveRoute) || ownedByLineage(record, targetSids, expectedDB) {
			copied := make(map[string]any, len(record))
			for key, value := range record {
				copied[key] = value
			}
			out = append(out, copied)
		}
	}
	return out, nil
}

// Whether a record's durable owner is in this lineage and its child writes to the same session database file
func ownedByLineage(record map[string]any, targetSids map[string]bool, expectedDB any) bool {
	owner, isStr := record["owner_agent_session_id"].(string)
	if len(targetSids) == 0 || !isStr || !targetSids[owner] {
		return false
	}
	expected, ok := expectedDB.(dbPathHolder)
	if !ok || nilValue(expectedDB) {
		return false
	}
	child, ok := agentSessionDB(record["agent"]).(dbPathHolder)
	if !ok {
		return false
	}
	return expected.Path() == child.Path()
}

// The session plus the sessions it continues by compression, walked backward from it
func compressionLineageOf(ctx context.Context, db any, sessionID string) (map[string]bool, error) {
	ids := map[string]bool{}
	if sessionID == "" {
		return ids, nil
	}
	ids[sessionID] = true
	if reader, ok := db.(compressionChildReader); ok && !nilValue(db) {
		row, err := reader.GetSession(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		for row != nil {
			child, err := reader.IsCompressionChildRow(ctx, row)
			if err != nil {
				return nil, err
			}
			if !child {
				break
			}
			parentValue, ok := row.Get("parent_session_id")
			if !ok {
				return nil, fmt.Errorf("KeyError: 'parent_session_id'")
			}
			parentID := pyrepr.Str(parentValue)
			if ids[parentID] || len(ids) >= 200 {
				break
			}
			ids[parentID] = true
			if row, err = reader.GetSession(ctx, parentID); err != nil {
				return nil, err
			}
		}
		return ids, nil
	}
	if reader, ok := db.(compressionLineageReader); ok && !nilValue(db) {
		found, err := reader.GetCompressionLineage(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		for _, id := range found {
			ids[id] = true
		}
	}
	return ids, nil
}

// Queues a steer on a live child only while its registry record holds that child and accepts steering
func SteerOwnedSubagent(subagentID string, child any, text string) bool {
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	record := activeSubagents.get(subagentID)
	if isNilAgent(child) || record == nil || !pyrepr.Is(record["agent"], child) || !pyTruthy(record["accepting_steer"]) {
		return false
	}
	target, ok := child.(steerable)
	return ok && target.Steer(text)
}

// Live transcript file of a child agent, empty when it has none
func SubagentTranscriptPath(agent any) string {
	if holder, ok := agent.(liveTranscriptHolder); ok && !isNilAgent(agent) {
		if path, isStr := holder.liveTranscript().(string); isStr {
			return path
		}
	}
	return ""
}

// Nil interface or nil pointer, the None of an attribute slot
func nilValue(v any) bool {
	if v == nil {
		return true
	}
	value := reflect.ValueOf(v)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

// Control snapshot of a segment, a persisted row that does not parse refused as storage_unavailable
func ControlSnapshotOrRefusal(ctx context.Context, getMeta func(ctx context.Context, key string) (string, bool, error), target string) (*pyjson.Object, error) {
	snapshot, err := ControlSnapshot(ctx, getMeta, target)
	var parse *ControlStoreError
	if errors.As(err, &parse) {
		refusal := state.NewRuntimeStoreError("storage_unavailable", nil, nil)
		refusal.Cause = parse.Err
		return nil, refusal
	}
	return snapshot, err
}

// Whether a one shot template name is one the prompt template table holds
func OneshotTemplateKnown(name string) bool {
	_, ok := promptTemplates[name]
	return ok
}
