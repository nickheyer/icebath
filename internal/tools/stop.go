package tools

// Registrant whose tools keep work of a session going in the background that /stop ends, such as the subagents delegate_task runs: ends
// the session's, how many it ended
type Stopper interface {
	Stop(session string) int
}

// Ends the background work of the session every registrant that keeps some has, how many it ended in all
func (r *Registry) Stop(session string) int {
	stopped := 0
	for _, registrant := range r.registrants {
		if stopper, ok := registrant.(Stopper); ok {
			stopped += stopper.Stop(session)
		}
	}
	return stopped
}
