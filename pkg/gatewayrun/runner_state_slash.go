package gatewayrun

// Lazily set attributes of the kanban watcher mixin
type runnerSlashState struct {
	runnerKanbanState
	telegramCapabilityHintTS map[string]float64
}
