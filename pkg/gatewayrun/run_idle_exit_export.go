package gatewayrun

import "github.com/nickheyer/icebath/pkg/gateway"

// Marks dashboard or API traffic so an idle exit gateway stays up
func (r *GatewayRunner) NoteClientActivity() { noteClientActivity(r) }

// Counts an attached native client until the returned release runs
func (r *GatewayRunner) AttachClient() (release func()) { return attachedClient(r) }

// Callback loop the runner serves on, nil before the gateway start gave it one
func (r *GatewayRunner) ListenerLoop() *gateway.CallbackLoop {
	loop, _ := r.gatewayLoop.(*gateway.CallbackLoop)
	return loop
}
