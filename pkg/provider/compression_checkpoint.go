package provider

// CompressionCheckpointUnavailable is raised when required durable pre compress checkpointing is unavailable
type CompressionCheckpointUnavailable struct{ Msg string }

func (e *CompressionCheckpointUnavailable) Error() string { return e.Msg }

// Refusal carrying the missing prerequisite marker and the reason the checkpoint is unavailable
func CheckpointBlocked(reason string) *CompressionCheckpointUnavailable {
	return &CompressionCheckpointUnavailable{"BLOCKED_MISSING_PREREQUISITE: required pre-compress checkpoint unavailable: " + reason}
}
