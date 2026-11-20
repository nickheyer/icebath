package tools

// Lock file beside the resolved checkpoint base whose inode survives clearing the base
func CheckpointStoreLockPath(base string) string { return checkpointStoreLockPath(base) }

// Takes the whole checkpoint store lock without waiting and returns its release, a busy store errors
func CheckpointStoreLock(base string) (func(), error) { return checkpointStoreLock(base) }
