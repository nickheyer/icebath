package gateway

import (
	"context"
	"fmt"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
	entry, ok := receipt.ValueOr("entry", nil).(*jsonutil.Object)
	if !ok {
		return "", fmt.Errorf("the local receipt of session %s has no entry mapping", ref.SessionID)
	}
	sessionID, ok := entry.Get("session_id")
	if !ok {
		return "", &keyError{"session_id"}
	}
	return anyutil.String(sessionID), nil
}
