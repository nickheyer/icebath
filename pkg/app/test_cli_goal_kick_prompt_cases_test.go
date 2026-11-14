package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/goals"
)

// Goal kick CLI over the given history whose goal manager is the local one of session kick-test
func goalKickCLI(t *testing.T, history []map[string]any) *CLI {
	t.Helper()
	recordPrints(t)
	c := bareCLI()
	c.sessionID = "kick-test"
	c.conversationHistory = history
	c.pendingInput = &inputQueue{}
	return c
}

// Runs /goal text and returns the prompt it queued
func goalKick(t *testing.T, c *CLI, text string) any {
	t.Helper()
	c.handleGoalCommand("/goal " + text)
	item, ok := c.pendingInput.GetNoWait()
	if !ok {
		t.Fatalf("/goal %.40q queued nothing", text)
	}
	return item
}

// Multi line handoff note pasted as a user message before the goal is set
func goalKickHandoff() string {
	steps := make([]string, 8)
	for i := range steps {
		steps[i] = fmt.Sprintf("  - step %d: merge r3-%d and run its targeted suite, then the full suite", i, i)
	}
	return "HANDOFF: resume round 3 integration.\n" + strings.Join(steps, "\n")
}

// Asserts the kicked prompt equals want
func assertKick(t *testing.T, got any, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("kicked %q, want %q", got, want)
	}
}

func TestGoalThatTheUserJustPastedKicksWithAPointerNotTheText(t *testing.T) {
	handoff := goalKickHandoff()
	c := goalKickCLI(t, []map[string]any{{"role": "user", "content": handoff + "\n\nGo."}, {"role": "assistant", "content": "ok"}})
	assertKick(t, goalKick(t, c, handoff), goals.GoalAlreadySeenKick)
	c = goalKickCLI(t, []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "text", "text": handoff}}}})
	assertKick(t, goalKick(t, c, handoff), goals.GoalAlreadySeenKick)
}

func TestANewGoalOrAGoalFromAnOlderTurnIsKickedVerbatim(t *testing.T) {
	handoff := goalKickHandoff()
	flat := strings.Join(strings.Fields(handoff), " ")
	assertKick(t, goalKick(t, goalKickCLI(t, []map[string]any{}), "Ship the release"), "Ship the release")
	c := goalKickCLI(t, []map[string]any{{"role": "user", "content": "Something unrelated"}})
	assertKick(t, goalKick(t, c, handoff), flat)
	c = goalKickCLI(t, []map[string]any{{"role": "user", "content": handoff}, {"role": "assistant", "content": "done"},
		{"role": "user", "content": "now something else"}})
	assertKick(t, goalKick(t, c, handoff), flat)
}

func TestAShortGoalThatSelectsOneOptionFromTheLastMessageIsKickedVerbatim(t *testing.T) {
	offer := strings.Repeat("I can either ship the API or ship the UI next; which do you want? ", 8)
	c := goalKickCLI(t, []map[string]any{{"role": "user", "content": offer}})
	assertKick(t, goalKick(t, c, "ship the API"), "ship the API")
	assertKick(t, goalKick(t, c, "ship the UI"), "ship the UI")
	longGoal := strings.Repeat("x", 500)
	c = goalKickCLI(t, []map[string]any{{"role": "user", "content": longGoal + " " + strings.Repeat("y", 2000)}})
	assertKick(t, goalKick(t, c, longGoal), longGoal)
}
