package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Raised when a persisted goal, loop or heartbeat row cannot be parsed
type ControlStoreError struct{ Err error }

func (e *ControlStoreError) Error() string { return "storage_unavailable" }

func (e *ControlStoreError) Unwrap() error { return e.Err }

// Goal, loop and heartbeat cards of a segment with their revision and newest persisted timestamp
func ControlSnapshot(ctx context.Context, getMeta func(ctx context.Context, key string) (string, bool, error), target string) (*pyjson.Object, error) {
	read := func(key string) (string, error) {
		raw, ok, err := getMeta(ctx, key)
		if err != nil || !ok {
			return "", err
		}
		return raw, nil
	}
	var goalState *goals.GoalState
	var loopState *SessionLoopState
	var heartbeatState *HeartbeatState
	raw, err := read(goals.GoalMetaKey(target))
	if err != nil {
		return nil, err
	}
	if raw != "" {
		if goalState, err = goals.GoalStateFromJSON(raw); err != nil {
			return nil, &ControlStoreError{err}
		}
	}
	if raw, err = read(loopMetaKey(target)); err != nil {
		return nil, err
	}
	if raw != "" {
		if loopState, err = SessionLoopStateFromJSON(raw); err != nil {
			return nil, &ControlStoreError{err}
		}
	}
	if raw, err = read(heartbeatMetaPrefix + target); err != nil {
		return nil, err
	}
	if raw != "" {
		if heartbeatState, err = HeartbeatStateFromJSON(raw); err != nil {
			return nil, &ControlStoreError{err}
		}
	}
	goal := safeGoalSnapshot(goalState)
	deferred := false
	if goal != nil {
		status, _ := goal.Get("status")
		barrier, _ := goal.Get("wait_barrier")
		deferred = pyStr(status) == "active" && barrier == nil
	}
	loop := safeLoopSnapshot(loopState, deferred && loopState != nil && pyStr(loopState.Status) == "active")
	heartbeat := safeHeartbeatSnapshot(heartbeatState)
	revision, err := controlSnapshotRevision(goal, loop, heartbeat)
	if err != nil {
		return nil, err
	}
	var candidates []float64
	if goalState != nil {
		candidates = append(candidates, goalState.CreatedAt, goalState.LastTurnAt)
	}
	if loopState != nil {
		candidates = append(candidates, loopState.CreatedAt, loopState.LastFiredAt)
	}
	if heartbeatState != nil {
		candidates = append(candidates, heartbeatState.CreatedAt, heartbeatState.LastFiredAt)
	}
	var updatedAt any = 0
	best, found := 0.0, false
	for _, value := range candidates {
		if value != 0 && (!found || value > best) {
			best, found = value, true
		}
	}
	if found {
		updatedAt = best
	}
	return pyjson.ObjectOf("goal", nilObject(goal), "loop", nilObject(loop), "heartbeat", nilObject(heartbeat),
		"revision", revision, "updated_at", updatedAt), nil
}

func nilObject(o *pyjson.Object) any {
	if o == nil {
		return nil
	}
	return o
}
