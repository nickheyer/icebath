package gateway

import "context"

// True for a real inbound platform message on an adapter that brackets processing with its own start hook
func followupProcessingHooksApply(adapter PlatformAdapter, event *MessageEvent) bool {
	if adapter == nil || event == nil {
		return false
	}
	if !((event.MessageID != nil && *event.MessageID != "") || pyTruthy(event.RawMessage)) {
		return false
	}
	return adapterOverrides(adapter, "OnProcessingStart")
}

// Cancelled for a cancel the adapter itself routed, failure for any other, the background processing classification
func FollowupCancelOutcome(ctx context.Context, adapter PlatformAdapter) ProcessingOutcome {
	if adapter == nil {
		return ProcessingFailure
	}
	b := adapter.Base()
	b.mu.Lock()
	expected := b.expectedCancelledTasks
	b.mu.Unlock()
	if expected == nil {
		return ProcessingFailure
	}
	current := currentAdapterTask(ctx)
	if current == nil {
		return ProcessingFailure
	}
	b.mu.Lock()
	_, routed := b.expectedCancelledTasks[current]
	b.mu.Unlock()
	if routed {
		return ProcessingCancelled
	}
	return ProcessingFailure
}

// Fires one lifecycle hook for a runner drained follow up, a no op unless the follow up hooks apply
func RunFollowupProcessingHook(ctx context.Context, adapter PlatformAdapter, event *MessageEvent, hookName string, outcome ProcessingOutcome) {
	if !followupProcessingHooksApply(adapter, event) {
		return
	}
	b := adapter.Base()
	switch hookName {
	case "on_processing_start":
		b.runProcessingHook(ctx, hookName, func(c context.Context) error { return adapter.OnProcessingStart(c, event) })
	case "on_processing_complete":
		b.runProcessingHook(ctx, hookName, func(c context.Context) error { return adapter.OnProcessingComplete(c, event, outcome) })
	}
}
