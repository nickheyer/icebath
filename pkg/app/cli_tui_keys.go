package app

import (
	"os"
	"runtime"
)

// Hard interrupt of a running agent, swapped by tests
var requestHardInterrupt = func(a cliAgent) { a.HardInterrupt() }

// Current connection card state
func (c *CLI) connectionModal() *connectionModalState {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	st, _ := c.connectionState.(*connectionModalState)
	return st
}

// Marks the run as ending, detaching from a running turn when asked
func (c *CLI) setShouldExit(detach bool) {
	c.uiMu.Lock()
	c.shouldExit = true
	if detach {
		c.detachOnExit = c.agentRunning
	}
	c.uiMu.Unlock()
}

// Platform and process hooks the suspend handler goes through
var (
	tuiGOOS       = runtime.GOOS
	suspendSelf   = suspendProcessGroup
	suspendNotice = func(msg string) { _, _ = os.Stdout.WriteString(msg) }
)

// submittedInput is a typed message with its attached images
type submittedInput struct {
	Text   string
	Images []string
}
