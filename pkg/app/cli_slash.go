package app

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Reports a slash command, whose first word carries no further slash the way a file path does
func looksLikeSlashCommand(text string) bool {
	if text == "" || !strings.HasPrefix(text, "/") {
		return false
	}
	words := pystr.SplitWhitespace(text, -1)
	return len(words) > 0 && !strings.Contains(words[0][1:], "/")
}

// Text after the slash command word with its padding removed, empty when there is none
func slashArgs(cmd string) string {
	if parts := pystr.SplitWhitespace(cmd, 1); len(parts) > 1 {
		return pystr.Strip(parts[1])
	}
	return ""
}

// Moves stray interrupt queue messages into the pending input after every turn
func (c *CLI) drainInterruptQueueToPendingInput() {
	defer func() { _ = recover() }()
	for {
		stray, ok := c.interruptQueue.GetNoWait()
		if !ok {
			return
		}
		if pyrepr.Truthy(interruptPayloadValue(stray)) {
			c.pendingInput.Put(stray)
		}
	}
}
