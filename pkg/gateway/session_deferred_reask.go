package gateway

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// True when a decoded deferred pick holds the awaiting_confirm marker, by Python in membership
func pickAwaitsConfirm(decoded any) bool {
	switch v := decoded.(type) {
	case *pyjson.Object:
		_, found := v.Get("awaiting_confirm")
		return found
	case []any:
		for _, item := range v {
			if pyrepr.Equal(item, "awaiting_confirm") {
				return true
			}
		}
	case string:
		return strings.Contains(v, "awaiting_confirm")
	}
	return false
}

// Asks a newly attached viewer again about a guarded deferred pick, rechecked at the next boundary
func reaskOnAttach(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession) error {
	stored, found, err := a.DB.GetMeta(ctx, state.DeferredPickPrefix+ref.SessionID)
	if err != nil || !found {
		return err
	}
	decoded, err := pyjson.Loads(stored)
	if err != nil {
		return err
	}
	if !pickAwaitsConfirm(decoded) {
		return nil
	}
	a.mu.Lock()
	live.ReaskPick = true
	a.mu.Unlock()
	return scheduleSession(ctx, a, ref)
}
