package app

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/state"
)

// Asks once at TTY startup whether to enable session auto prune, a failure logged at debug like the startup guard
func offerAutoPrune(c *CLI, print func(string)) {
	con := console()
	if _, err := state.AutoPruneOfferInteractive(c.ctx, con.Out, print, con.Input); err != nil {
		cliLogger.Debug(fmt.Sprintf("auto-prune offer failed: %v", err))
	}
}
