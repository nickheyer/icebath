package gatewayrun

// Session runtime descriptor the drain checks read and stamp
func (r *GatewayRunner) SessionRuntimeDescriptor() map[string]any {
	return r.sessionRuntimeDescriptor.load()
}
