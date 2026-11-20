package agent

// Stamps the wall clock time of the last activity, the attribute write that ages or restores the activity clock
func (a *AIAgent) SetLastActivityTS(ts float64) {
	lock := a.livenessActivityLock()
	lock.Lock()
	a.lastActivityTS = ts
	lock.Unlock()
}
