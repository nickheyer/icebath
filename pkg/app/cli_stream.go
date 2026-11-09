package app

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

// Prints Rich markup through the command safe console, prompt safe once the TUI is live
func (c *CLI) consolePrint(markup string) {
	if c.app != nil {
		chatConsolePrint(markup)
		return
	}
	fmt.Fprintln(c.console.Out, pyrich.Render(markup, c.console.ShouldUseColor()))
}
