package gatewayrun

import "github.com/nickheyer/icebath/pkg/gateway"

// The session's overflow FIFO, nil when no session state exists yet
func (r *GatewayRunner) overflowQueue(sessionKey string) *[]*gateway.MessageEvent {
	state := r.peekSessionState(sessionKey)
	if state == nil {
		return nil
	}
	return &state.conversation.queuedEvents
}
