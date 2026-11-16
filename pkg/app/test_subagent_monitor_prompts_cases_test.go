package app

import (
	"testing"
	"time"
)

// Opens the named blocking prompt on the CLI
func openBlockingPrompt(c *CLI, name string) {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	switch name {
	case "_approval_state":
		c.approvalState = &approvalModalState{}
	case "_clarify_state":
		c.clarifyState = &clarifyModalState{}
	case "_secret_state":
		c.secretState = &secretModalState{}
	case "_sudo_state":
		c.sudoState = &sudoModalState{}
	case "_slash_confirm_state":
		c.slashConfirmState = &slashConfirmModalState{}
	}
}

// Runs fn on its own goroutine and fails when it has not returned within three seconds
func returnsPromptly(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s never returned", what)
	}
}
