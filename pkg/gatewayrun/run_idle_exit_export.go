package gatewayrun

// Counts an attached native client until the returned release runs
func (r *GatewayRunner) AttachClient() (release func()) { return attachedClient(r) }
