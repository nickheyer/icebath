package app

import (
	"reflect"
	"strings"
	"testing"
)

// Queued prompts as plain strings in FIFO order
func pendingStrings(t *testing.T, c *CLI) []string {
	t.Helper()
	out := []string{}
	for _, item := range c.pendingInput.snapshot() {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("queued item %#v is not a string", item)
		}
		out = append(out, s)
	}
	return out
}

// Asserts the queued prompts equal want
func assertPending(t *testing.T, c *CLI, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got := pendingStrings(t, c); !reflect.DeepEqual(got, want) {
		t.Fatalf("pending input = %q, want %q", got, want)
	}
}

func TestBusyInputModeQueueCommandCanEditRemoveMoveAndClearPendingItems(t *testing.T) {
	recordPrints(t)
	c := configuredCLI(t, nil, CLIOptions{})
	c.processCommand("/queue first prompt")
	c.processCommand("/queue second prompt")
	c.processCommand("/queue edit 2 replacement prompt")
	assertPending(t, c, "first prompt", "replacement prompt")

	c.processCommand("/queue move 2 1")
	assertPending(t, c, "replacement prompt", "first prompt")

	c.processCommand("/queue rm 2")
	assertPending(t, c, "replacement prompt")

	c.processCommand("/queue clear")
	assertPending(t, c)
	if c.pendingInput.Len() != 0 {
		t.Fatalf("queue length after clear = %d, want 0", c.pendingInput.Len())
	}
	if !c.pendingInput.Empty() {
		t.Fatal("queue not empty after clear")
	}
}

func TestBusyInputModeQueueMalformedIndexPrintsUsageInsteadOfRaising(t *testing.T) {
	cases := []struct{ id, command, usageKey string }{
		{"rm-bare", "/queue rm", "cli.queue.usage_remove"},
		{"rm-trailing-words", "/queue rm 1 extra", "cli.queue.usage_remove"},
		{"rm-circled-digit", "/queue rm ②", "cli.queue.usage_remove"},
		{"rm-over-int-limit", "/queue rm " + strings.Repeat("9", 5000), "cli.queue.usage_remove"},
		{"edit-superscript", "/queue edit ² new text", "cli.queue.usage_edit"},
		{"move-src-circled", "/queue move ⑦ 1", "cli.queue.usage_move"},
		{"move-dst-circled", "/queue move 1 ⑦", "cli.queue.usage_move"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			rec := recordPrints(t)
			c := configuredCLI(t, nil, CLIOptions{})
			c.processCommand("/queue first prompt")
			c.processCommand(tc.command)
			assertPending(t, c, "first prompt")
			printed := rec.plain()
			if len(printed) == 0 {
				t.Fatal("nothing printed")
			}
			if got, want := strings.TrimSpace(printed[len(printed)-1]), c.t(tc.usageKey, nil); got != want {
				t.Fatalf("last printed line = %q, want %q", got, want)
			}
		})
	}
}

func TestBusyInputModeQueueEditAcceptsTabAfterIndex(t *testing.T) {
	recordPrints(t)
	c := configuredCLI(t, nil, CLIOptions{})
	c.processCommand("/queue first prompt")
	c.processCommand("/queue edit 1\treplacement")
	assertPending(t, c, "replacement")
}
