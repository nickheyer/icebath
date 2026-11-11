package gateway

import (
	"context"
	"fmt"
	"time"
)

// Wires the transport handlers, dials, adopts the negotiated descriptor and watches for revocation
func (a *RelayAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.transport == nil {
		return false, &PyError{"RuntimeError", "RelayAdapter has no transport configured"}
	}
	a.transport.SetInboundHandler(a.onInbound)
	if setter, ok := a.transport.(interface{ SetInterruptInboundHandler(RelayInterruptHandler) }); ok {
		setter.SetInterruptInboundHandler(a.OnInterrupt)
	}
	if setter, ok := a.transport.(interface{ SetPassthroughHandler(RelayPassthroughHandler) }); ok {
		setter.SetPassthroughHandler(a.onPassthrough)
	}
	connected, err := a.transport.Connect(ctx)
	if err != nil || !connected {
		return false, err
	}
	descriptor, err := a.transport.Handshake(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return false, err
		}
		relayAdapterLog().Warn(fmt.Sprintf("relay handshake failed: %s", relayExcStr(err)))
		return false, nil
	}
	a.applyDescriptor(descriptor)
	if _, ok := a.transport.(interface{ AuthRevoked() bool }); ok {
		a.startRevocationMonitor(ctx)
	}
	return true, nil
}

// Starts the revocation watch unless one is already running
func (a *RelayAdapter) startRevocationMonitor(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.revocationMonitor != nil && !a.revocationMonitor.finished() {
		return
	}
	a.revocationMonitor = startRelayTask(ctx, func(mctx context.Context) { a.watchForRevocation(mctx, 1.0) })
}

// Polls the transport until revocation latches, then marks the adapter fatally disabled and notifies
func (a *RelayAdapter) watchForRevocation(ctx context.Context, pollIntervalS float64) {
	revoker, ok := a.transport.(interface{ AuthRevoked() bool })
	if !ok {
		return
	}
	for !revoker.AuthRevoked() {
		timer := time.NewTimer(secondsDuration(pollIntervalS))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	relayAdapterLog().Warn("relay credential revoked (opt-out) — marking the relay adapter disabled")
	a.SetFatalError(ctx, "relay_disabled", "Relay disabled (opted out — recreate the instance to re-enable)", false)
	if err := a.NotifyFatalError(ctx); err != nil {
		relayAdapterLog().Debug("relay revocation fatal-error notify failed", "exc_info", err)
	}
}
