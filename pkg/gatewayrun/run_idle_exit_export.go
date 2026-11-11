package gatewayrun

// Marks dashboard or API traffic so an idle exit gateway stays up
func (r *GatewayRunner) NoteClientActivity() { noteClientActivity(r) }

// Counts an attached native client until the returned release runs
func (r *GatewayRunner) AttachClient() (release func()) { return attachedClient(r) }
