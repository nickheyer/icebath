package gateway

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Refuses with a runtime store error when generation is no longer the session's running claim
func (a *SessionAuthority) CheckApprovalGeneration(ctx context.Context, sessionID string, generation int64) error {
	return a.checkApprovalGeneration(ctx, sessionID, generation)
}

// Runs the admission observers' callback for key fenced to the running claim, false when it moved or none accepted
func PublishAPIObserverCallback(ctx context.Context, a *SessionAuthority, sessionID string, generation int64, key string, args []any, kwargs map[string]any) (bool, error) {
	live, ok := a.live(sessionID)
	if !ok {
		return false, &pyKeyError{sessionID}
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	if err := a.checkApprovalGeneration(ctx, sessionID, generation); err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return false, nil
		}
		return false, err
	}
	return notifyObservers(a, sessionID, key, args, kwargs)
}

// Pending control prompts of the session for the generation, the controls snapshot viewers replay
func (l *LiveSession) SnapshotControls(sessionID string, generation int64) []map[string]any {
	return l.pendingControls().snapshot(sessionID, &generation)
}

// Dedup set of media paths prior turns already delivered
func CollectHistoryMediaPaths(ctx context.Context, history []any) (map[string]struct{}, error) {
	return collectHistoryMediaPaths(ctx, history)
}

// Integer column of a session row, nil for NULL or a non numeric value
func RowInt(row *pyjson.Object, key string) *int64 { return rowInt(row, key) }

// Live session the authority holds for the id and whether it holds one
func (a *SessionAuthority) Live(sessionID string) (*LiveSession, bool) { return a.live(sessionID) }

// Prefill messages a local session policy freezes for the session
func SessionPrefill(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy, sessionID string) ([]any, error) {
	return sessionPrefill(ctx, a, policy, sessionID)
}

// Provider routing a local session policy freezes for its agents
func SessionRouting(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy) (map[string]any, error) {
	return sessionRouting(ctx, a, policy)
}

// Whether the session row is ended in the profile store that owns the session key
func (s *SessionStore) IsSessionEndedInDB(ctx context.Context, sessionID, sessionKey string) bool {
	return s.isSessionEndedInDB(ctx, sessionID, sessionKey)
}

// Whether the adapter renders native approval buttons, a duck type counting when it sends approvals itself
func RendersExecApprovalButtons(adapter any) bool { return rendersExecApprovalButtons(adapter) }
