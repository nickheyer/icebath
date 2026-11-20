package kanban

// Records every spawned worker that has exited since the last call and returns their pids
func ReapWorkerZombies() []int { return reapWorkerZombies() }
