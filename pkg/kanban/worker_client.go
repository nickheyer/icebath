package kanban

// AdmissionAnswerMarginSeconds covers the owner's session setup and receipt around its admission write
const AdmissionAnswerMarginSeconds = 60.0

// Seconds a worker client waits for the owner's kanban.run answer, the longest board lock wait plus the margin
func AdmissionAnswerBudget() float64 {
	lockWait := float64(busyMaxRetries+1) * float64(resolveBusyTimeoutMS()) / 1000
	return lockWait + AdmissionAnswerMarginSeconds
}
