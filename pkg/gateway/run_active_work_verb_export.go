package gateway

// Live managed workers, cron cancellations and unabandoned side agents an unserve of the authority stops
func (a *SessionAuthority) UnserveStopCounts() (managed, crons, sides int) {
	a.mu.Lock()
	for _, worker := range a.managedWorkers {
		if !worker.Closed() {
			managed++
		}
	}
	crons = len(a.cronCancellations)
	a.mu.Unlock()
	return managed, crons, len(sideAgents(a))
}
