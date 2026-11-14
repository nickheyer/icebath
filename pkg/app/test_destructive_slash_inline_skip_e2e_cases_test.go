package app

import (
	"bytes"
	"strings"
	"testing"
)

// Minimal classic CLI with the destructive slash gate on, no application, and a typed cancel waiting on stdin
func destructiveSkipCLI(t *testing.T) (*CLI, *bytes.Buffer) {
	t.Helper()
	c, _ := newModalTestCLI(t, map[string]any{"approvals": map[string]any{"destructive_slash_confirm": true}})
	c.app = nil
	c.compact = false
	c.model = "stub-model"
	c.sessionID = "stub-session"
	c.pendingTitle = ""
	c.sessionDB = nil
	return c, withConsoleInput(t, "3\n")
}

// Reports whether the confirm modal asked for a choice
func destructiveModalReached(c *CLI, out *bytes.Buffer) bool {
	return strings.Contains(out.String(), c.t("cli.modal.choice_prompt", nil))
}

func TestResetNowInvokesNewSessionWithoutModal(t *testing.T) {
	c, out := destructiveSkipCLI(t)
	c.processCommand("/reset now")
	if c.sessionID == "stub-session" {
		t.Fatal("new_session was never invoked")
	}
	if destructiveModalReached(c, out) {
		t.Fatal("modal must not be invoked when inline-skip token present")
	}
	titled := c.t("cli.session.new_session_titled", map[string]any{"title": "now"})
	if !strings.Contains(out.String(), c.t("cli.session.new_session", nil)) || strings.Contains(out.String(), titled) {
		t.Fatalf("the /new branch must start an untitled session, output %q", out.String())
	}
}

func TestNewYesWithTitlePreservesTitle(t *testing.T) {
	c, out := destructiveSkipCLI(t)
	c.processCommand("/new --yes My Session")
	if c.sessionID == "stub-session" {
		t.Fatal("new_session was never invoked")
	}
	if destructiveModalReached(c, out) {
		t.Fatal("modal must not be invoked when inline-skip token present")
	}
	if !strings.Contains(out.String(), c.t("cli.session.new_session_titled", map[string]any{"title": "My Session"})) {
		t.Fatalf("title not preserved, output %q", out.String())
	}
}

func TestNewWithoutSkipTokenStillConsultsModal(t *testing.T) {
	c, out := destructiveSkipCLI(t)
	c.processCommand("/new My Session")
	if !destructiveModalReached(c, out) {
		t.Fatalf("modal must be reached when no skip token is present, output %q", out.String())
	}
	if c.sessionID != "stub-session" {
		t.Fatal("user cancelled, new_session must not run")
	}
}
