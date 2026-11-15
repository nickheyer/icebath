//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package agentimport

import (
	"io"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// Text pasted at a console without termios, read to end of input
func readTerminalPaste(c *cliui.Console) (string, error) {
	c.PrintInfo("Press Ctrl-D once it has all pasted (Ctrl-Z then Enter on Windows).")
	raw, err := io.ReadAll(c.In)
	return string(raw), err
}

// A console without termios has no pending input queue to flush
func discardPendingInput(*cliui.Console) {}
