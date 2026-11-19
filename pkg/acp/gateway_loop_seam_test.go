package acp

import (
	"context"
	"errors"
)

// Whether the task ended by cancellation
func (t *loopTask) cancelled() bool { return errors.Is(t.err, context.Canceled) }
