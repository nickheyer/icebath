package app

import (
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Monotonic seconds the reasoning block timer counts against
var monotonic = pythread.Monotonic

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
