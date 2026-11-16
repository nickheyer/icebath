package app

import (
	"errors"
	"os"

	"github.com/nickheyer/icebath/pkg/memory"
)

// Chat turn a submitted input runs, the method itself in production
var tuiChat = (*CLI).chat

// errKeyboardInterrupt is the Ctrl C of a slow slash command
var errKeyboardInterrupt = errors.New("KeyboardInterrupt")

// Environment value, the fallback when unset
func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}

// Curator pass of the startup maintenance, swapped by tests
var maybeRunCurator = func(c *CLI, idleForSeconds *float64, onSummary func(string)) {
	memory.MaybeRunCurator(c.ctx, idleForSeconds, onSummary)
}
