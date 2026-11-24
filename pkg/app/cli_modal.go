package app

import (
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Monotonic seconds the reasoning block timer counts against
var monotonic = timeutil.Monotonic

// Rings the terminal bell and sends the OS notification of a finished turn when display.bell_on_complete allows
func (c *CLI) ringBell() {
	c.uiMu.Lock()
	flag := c.bellOnComplete
	c.uiMu.Unlock()
	if !flag {
		return
	}
	notify, err := notificationSequence(c.t("cli.modal.bell_turn_complete", nil), c.sessionID)
	if err != nil {
		return
	}
	writeTTY("\a" + notify)
}
