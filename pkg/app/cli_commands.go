package app

// Prints each line through cprint
func cp(lines ...string) {
	for _, line := range lines {
		cprint(line)
	}
}

// Text wrapped in the dim escape
func dimText(text string) string { return dimANSI + text + resetANSI }

// Two space indented dim line, the standard slash command hint shape
func dimLine(text string) string { return "  " + dimText(text) }

// Counts one tool line focus view hides this turn against the mode saved before focus turned progress off
func (c *CLI) noteFocusHiddenLine(functionName string) {
	if !c.focusViewEnabled {
		return
	}
	if !wouldDisplayToolLine(c.focusSavedToolProgress, functionName, c.focusLastCountedTool) {
		return
	}
	c.focusLastCountedTool = functionName
	c.focusHiddenLines++
}

// Prints the dim count of tool lines focus view hid this turn and resets the counter
func (c *CLI) emitFocusRecoveryLine() {
	count := c.focusHiddenLines
	c.focusHiddenLines = 0
	c.focusLastCountedTool = ""
	if !c.focusViewEnabled {
		return
	}
	if line := formatHiddenLine(count); line != "" {
		cp(dimLine(line))
	}
}
