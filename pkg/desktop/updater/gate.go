package updater

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

const (
	updateWaitTimeout  = 20 * time.Minute
	updateWaitPoll     = time.Second
	updateHandoffDwell = 2500 * time.Millisecond
	gateReasonMarker   = "marker"
	gateReasonInFlight = "update-in-flight"
	gateReasonHandoff  = "handoff"
	clearanceClear     = "clear"
	clearanceFinished  = "finished"
	clearanceTimeout   = "timeout"
	clearanceCancelled = "cancelled"
)

// The three signals that an update owns the local runtime
type updateGateDeps struct {
	hasLiveMarker    func() bool
	isUpdateInFlight func() bool
	isHandoffActive  func() bool
}

// Why the gate is closed now, empty when it is open
func updateGateReason(deps updateGateDeps) string {
	if deps.hasLiveMarker() {
		return gateReasonMarker
	}
	if deps.isUpdateInFlight() {
		return gateReasonInFlight
	}
	if deps.isHandoffActive() {
		return gateReasonHandoff
	}
	return ""
}

type clearanceOptions struct {
	ctx         context.Context
	isCancelled func() bool
	timeout     time.Duration
	poll        time.Duration
	onWaitTick  func(reason string, waited time.Duration)
	now         func() time.Time
}

// Parks until no update signal remains, a live marker never aging out, reporting how the wait ended
func waitForUpdateClearance(deps updateGateDeps, opts clearanceOptions) (string, error) {
	now := opts.now
	if now == nil {
		now = time.Now
	}
	cancelled := func() bool {
		return (opts.ctx != nil && opts.ctx.Err() != nil) || (opts.isCancelled != nil && opts.isCancelled())
	}
	if cancelled() {
		return clearanceCancelled, nil
	}
	reason := updateGateReason(deps)
	if reason == "" {
		return clearanceClear, nil
	}
	startedAt := now()
	deadline := startedAt.Add(opts.timeout)
	for reason != "" && (reason == gateReasonMarker || now().Before(deadline)) {
		if cancelled() {
			return clearanceCancelled, nil
		}
		if opts.onWaitTick != nil {
			tickReason, waited := reason, now().Sub(startedAt)
			if _, err := backend.RunBackendStartStep(opts.ctx, func() (struct{}, error) {
				opts.onWaitTick(tickReason, waited)
				return struct{}{}, nil
			}); err != nil {
				if cancelled() {
					return clearanceCancelled, nil
				}
				return "", err
			}
		}
		if cancelled() {
			return clearanceCancelled, nil
		}
		if err := sleepCtx(opts.ctx, opts.poll); err != nil {
			if cancelled() {
				return clearanceCancelled, nil
			}
			return "", err
		}
		if cancelled() {
			return clearanceCancelled, nil
		}
		reason = updateGateReason(deps)
	}
	if reason != "" {
		return clearanceTimeout, nil
	}
	return clearanceFinished, nil
}

// Sleeps for d unless ctx ends first, answering its cause
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	if ctx == nil {
		<-timer.C
		return nil
	}
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
