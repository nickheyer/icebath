package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

var _ gateway.LiveTurnRunner = (*GatewayRunner)(nil)

// Whether a slash confirm prompt awaits its answer under the session key
func (r *GatewayRunner) HasPendingSlashConfirm(route string) bool {
	_, pending := SlashConfirmGetPending(route)
	return pending
}

// Whether an /update prompt awaits its reply under the session key, read without building the session state
func (r *GatewayRunner) UpdatePromptPending(route string) bool {
	pending, _ := r.updatePromptPending().Get(route)
	return pending
}

// Reply of a pending update prompt, clarify or slash confirm to a producer delivery, nil when none answers
func (r *GatewayRunner) HMPendingReplyIntercepts(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, sessionKey string) (*string, error) {
	return r.hmPendingReplyIntercepts(ctx, event, source, sessionKey)
}

// Routes a bare approval word of a producer delivery to the running turn's blocking approval, true when it was one
func (r *GatewayRunner) RoutePlaintextApprovalWhileBusy(ctx context.Context, event *gateway.MessageEvent, sessionKey string) bool {
	return r.routePlaintextApprovalWhileBusy(ctx, event, sessionKey)
}

// Busy input mode of a producer delivery against the running turn, true when its text landed inside that turn
func (r *GatewayRunner) HMBusyPreempt(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, sessionKey string) (bool, error) {
	return r.hmBusyPreempt(ctx, event, source, sessionKey)
}
