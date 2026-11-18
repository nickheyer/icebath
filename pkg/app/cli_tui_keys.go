package app

// Hard interrupt of a running agent, swapped by tests
var requestHardInterrupt = func(a cliAgent) { a.HardInterrupt() }

// Current connection card state
func (c *CLI) connectionModal() *connectionModalState {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	st, _ := c.connectionState.(*connectionModalState)
	return st
}

// Marks the run as ending
func (c *CLI) setShouldExit() {
	c.uiMu.Lock()
	c.shouldExit = true
	c.uiMu.Unlock()
}

// submittedInput is a typed message with its attached images
type submittedInput struct {
	Text   string
	Images []string
}
