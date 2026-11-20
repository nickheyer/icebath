package tools

// Replaces the ref deleter a checkpoint rekey goes through and returns the func restoring the real one
func SetCheckpointDeleteRef(fn func(env *Env, store, ref string) bool) (restore func()) {
	old := checkpointDeleteRef
	checkpointDeleteRef = fn
	return func() { checkpointDeleteRef = old }
}

// Shadow store holding every project's checkpoints under a checkpoints base dir
func CheckpointStorePath(base string) string { return checkpointStorePath(base) }

// Hash keying a working directory's checkpoint ref and ledger
func CheckpointProjectHash(workingDir string) string { return checkpointProjectHash(workingDir) }

// Ledger of one project's checkpoints in a store, empty when absent
func LoadCheckpointLedger(store, dirHash string) map[string]any {
	return loadCheckpointLedger(store, dirHash)
}

// Refs in a store that belong to the working directory's project
func ListCheckpointProjectRefs(env *Env, store, workingDir string) []string {
	return listCheckpointProjectRefs(env, store, workingDir)
}

// Ref name a project hash is stored under
func CheckpointRefName(dirHash string) string { return checkpointRefName(dirHash) }
