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

// tuiCloser pairs a foreground state probe with the function closing it
type tuiCloser struct {
	open  func() bool
	close func()
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
	tuiGOOS         = runtime.GOOS
	suspendSelf     = suspendProcessGroup
	suspendNotice   = func(msg string) { _, _ = os.Stdout.WriteString(msg) }
	wallClockFormat = "15:04:05"
)

// submittedInput is a typed message with its attached images
type submittedInput struct {
	Text   string
	Images []string
}

// Python spelling of a boolean
func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// Seconds a paste handler may take before it is logged as slow
const slowPasteMillis = 500.0

// Enter arriving this soon after a buffer change is part of a burst of input
const rapidInputEnterWindow = 0.05

// Quiet time that ends a burst of input
const burstEndQuiet = 0.25
