package tools

// Swaps the checkpoints command status, prune and input functions as hermes tests patch them, returning the restore
func SwapCheckpointsCommandFns(status func(*Env, string) (CheckpointStatus, error),
	prune func(*Env, CheckpointPruneOptions) (CheckpointPruneResult, error), input func(string) (string, error)) func() {
	savedStatus, savedPrune, savedInput := checkpointsStoreStatusFn, checkpointsPruneFn, checkpointsInputFn
	checkpointsStoreStatusFn, checkpointsPruneFn, checkpointsInputFn = status, prune, input
	return func() { checkpointsStoreStatusFn, checkpointsPruneFn, checkpointsInputFn = savedStatus, savedPrune, savedInput }
}
