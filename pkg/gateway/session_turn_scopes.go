package gateway

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

type surfaceTurnKey struct{}

type cliTurnKey struct{}

// Committed CLI turn fields bound to one admission, rebindable for the rest of its execution
type cliTurnBinding struct {
	mu          sync.Mutex
	admissionID string
	committed   *pyjson.Object
}

// Binds the executing admission's committed surface for the returned context
func surfaceTurnScope(ctx context.Context, committed any) context.Context {
	return context.WithValue(ctx, surfaceTurnKey{}, committed)
}

// True when the executing admission is a spoken voice turn on the voice chat route
func surfaceVoiceTurn(ctx context.Context) bool {
	committed, ok := ctx.Value(surfaceTurnKey{}).(*pyjson.Object)
	if !ok || committed == nil {
		return false
	}
	flag, _ := committed.Get("voice_turn")
	return pyTruthy(flag)
}

// Binds an admission's committed CLI turn fields for the returned context, nothing when they are empty
func cliTurnScope(ctx context.Context, admissionID string, committed any) context.Context {
	var bound *cliTurnBinding
	if obj, ok := committed.(*pyjson.Object); ok && obj != nil && obj.Len() > 0 {
		bound = &cliTurnBinding{admissionID: admissionID, committed: obj}
	}
	return context.WithValue(ctx, cliTurnKey{}, bound)
}

// Committed CLI turn fields of the admission whose event id is the message id, empty for any other turn
func cliTurn(ctx context.Context, messageID any) *pyjson.Object {
	bound, _ := ctx.Value(cliTurnKey{}).(*cliTurnBinding)
	if bound == nil || messageID == nil || pyrepr.Str(messageID) != bound.admissionID {
		return pyjson.NewObject()
	}
	bound.mu.Lock()
	defer bound.mu.Unlock()
	return bound.committed
}

// Rebinds this admission's api text with its references expanded for the rest of the execution
func rebindAPIText(ctx context.Context, messageID any, text string) {
	bound, _ := ctx.Value(cliTurnKey{}).(*cliTurnBinding)
	if bound == nil || messageID == nil || pyrepr.Str(messageID) != bound.admissionID {
		return
	}
	bound.mu.Lock()
	defer bound.mu.Unlock()
	rebound := pyjson.NewObject()
	for _, key := range bound.committed.Keys {
		value, _ := bound.committed.Get(key)
		rebound.Set(key, value)
	}
	rebound.Set("api_text", text)
	bound.committed = rebound
}
