package gateway

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
