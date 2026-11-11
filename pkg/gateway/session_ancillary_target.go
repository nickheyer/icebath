package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Current physical session of a conversation, the local receipt's entry for a local session
func physicalTarget(ctx context.Context, a *SessionAuthority, ref SessionRef) (string, error) {
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return "", err
	}
	if live.Source == nil || live.Source.Platform != PlatformLocal {
		return ref.SessionID, nil
	}
	receipt, err := a.DB.LocalReceipt(ctx, ref.SessionID)
	if err != nil {
		return "", err
	}
	entry, ok := receipt.GetOr("entry", nil).(*pyjson.Object)
	if !ok {
		return "", fmt.Errorf("TypeError: receipt entry is not subscriptable")
	}
	sessionID, ok := entry.Get("session_id")
	if !ok {
		return "", &pyKeyError{"session_id"}
	}
	return pyrepr.Str(sessionID), nil
}
