package gateway

// Committed unknown verdict that stops the FIFO without forging a failed settlement
type ManagedExecutionUnknown struct{ Message string }

func (e *ManagedExecutionUnknown) Error() string { return e.Message }
