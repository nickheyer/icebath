package gateway

import (
	"context"
	"sync"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

type surfaceTurnKey struct{}

type cliTurnKey struct{}

// Committed CLI turn fields bound to one admission, rebindable for the rest of its execution
type cliTurnBinding struct {
	mu          sync.Mutex
	admissionID string
	committed   *jsonutil.Object
}

// Binds the executing admission's committed surface for the returned context
func surfaceTurnScope(ctx context.Context, committed any) context.Context {
	return context.WithValue(ctx, surfaceTurnKey{}, committed)
}

// True when the executing admission is a spoken voice turn on the voice chat route
func surfaceVoiceTurn(ctx context.Context) bool {
	committed, ok := ctx.Value(surfaceTurnKey{}).(*jsonutil.Object)
	if !ok || committed == nil {
		return false
	}
	flag, _ := committed.Get("voice_turn")
	return anyutil.IsTrue(flag)
}

// Binds an admission's committed CLI turn fields for the returned context, nothing when they are empty
func cliTurnScope(ctx context.Context, admissionID string, committed any) context.Context {
	var bound *cliTurnBinding
	if obj, ok := committed.(*jsonutil.Object); ok && obj != nil && obj.Len() > 0 {
		bound = &cliTurnBinding{admissionID: admissionID, committed: obj}
	}
	return context.WithValue(ctx, cliTurnKey{}, bound)
}

// Committed CLI turn fields of the admission whose event id is the message id, empty for any other turn
func cliTurn(ctx context.Context, messageID any) *jsonutil.Object {
	bound, _ := ctx.Value(cliTurnKey{}).(*cliTurnBinding)
	if bound == nil || messageID == nil || anyutil.String(messageID) != bound.admissionID {
		return jsonutil.NewObject()
	}
	bound.mu.Lock()
	defer bound.mu.Unlock()
	return bound.committed
}

// Rebinds this admission's api text with its references expanded for the rest of the execution
func rebindAPIText(ctx context.Context, messageID any, text string) {
	bound, _ := ctx.Value(cliTurnKey{}).(*cliTurnBinding)
	if bound == nil || messageID == nil || anyutil.String(messageID) != bound.admissionID {
		return
	}
	bound.mu.Lock()
	defer bound.mu.Unlock()
	rebound := jsonutil.NewObject()
	for _, key := range bound.committed.Keys() {
		value, _ := bound.committed.Get(key)
		rebound.Set(key, value)
	}
	rebound.Set("api_text", text)
	bound.committed = rebound
}
