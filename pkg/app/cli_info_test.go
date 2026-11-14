package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSteerInlineDetector(t *testing.T) {
	t.Run("test_detects_steer_when_agent_running", func(t *testing.T) {
		recordPrints(t)
		c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{})
		c.agentRunning = true
		for _, text := range []string{"/steer focus on error handling", "/queue list", "/q follow up later"} {
			if !c.shouldHandleSteerCommandInline(text, false) {
				t.Fatalf("%q must dispatch inline while the agent runs", text)
			}
		}
		if !c.shouldHandleBackgroundCommandInline("/bg look this up", false) || !c.shouldHandleStopCommandInline("/stop", false) {
			t.Fatal("background and stop commands dispatch inline while the agent runs")
		}
		c.agentRunning = false
		if c.shouldHandleSteerCommandInline("/steer later", false) || c.shouldHandleSteerCommandInline("/Users/x/file.md", false) {
			t.Fatal("an idle CLI and a file path take the normal route")
		}
	})
	t.Run("test_ignores_steer_with_attached_images", func(t *testing.T) {
		recordPrints(t)
		c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{})
		c.agentRunning = true
		if c.shouldHandleSteerCommandInline("/steer text", true) {
			t.Fatal("a steer with images takes the normal path")
		}
	})
}

func TestInterruptQueueDrain(t *testing.T) {
	t.Run("test_preserves_order_when_draining_multiple_messages", func(t *testing.T) {
		c := bareCLI()
		c.interruptQueue, c.pendingInput = &inputQueue{}, &inputQueue{}
		for _, msg := range []string{"first", "second", "third"} {
			c.interruptQueue.Put(msg)
		}
		c.drainInterruptQueueToPendingInput()
		if !c.interruptQueue.Empty() || !reflect.DeepEqual(drain(c.pendingInput), []any{"first", "second", "third"}) {
			t.Fatal("the drain must move every message in order")
		}
	})
	t.Run("test_skips_falsy_messages", func(t *testing.T) {
		c := bareCLI()
		c.interruptQueue, c.pendingInput = &inputQueue{}, &inputQueue{}
		c.interruptQueue.Put("")
		c.interruptQueue.Put(nil)
		c.interruptQueue.Put("real")
		c.drainInterruptQueueToPendingInput()
		if got := drain(c.pendingInput); !c.interruptQueue.Empty() || !reflect.DeepEqual(got, []any{"real"}) {
			t.Fatalf("drained %v", got)
		}
	})
	t.Run("test_swallows_exceptions_so_main_loop_never_breaks", func(t *testing.T) {
		c := bareCLI()
		c.interruptQueue = &inputQueue{}
		c.interruptQueue.Put("anything")
		c.drainInterruptQueueToPendingInput()
	})
}

func TestRestoreSessionCwdMovesIntoTheRecordedDirectory(t *testing.T) {
	t.Setenv("COLUMNS", "500")
	rec := recordPrints(t)
	c := bareCLI()
	c.console = console()
	start := t.TempDir()
	recorded := t.TempDir()
	t.Chdir(start)
	t.Setenv("TERMINAL_CWD", start)
	c.restoreSessionCwd(map[string]any{"cwd": recorded}, false)
	now, _ := os.Getwd()
	want, _ := filepath.EvalSymlinks(recorded)
	got, _ := filepath.EvalSymlinks(now)
	if got != want || os.Getenv("TERMINAL_CWD") != recorded {
		t.Fatalf("cwd %q terminal cwd %q", now, os.Getenv("TERMINAL_CWD"))
	}
	rec.reset()
	c.restoreSessionCwd(map[string]any{"cwd": recorded}, false)
	c.restoreSessionCwd(map[string]any{}, false)
	if len(rec.raw()) != 0 {
		t.Fatalf("an unchanged or unrecorded cwd printed %q", rec.raw())
	}
	gone := filepath.Join(recorded, "gone")
	c.restoreSessionCwd(map[string]any{"cwd": gone}, false)
	if out := strings.Join(rec.plain(), "\n"); !strings.Contains(out, gone) {
		t.Fatalf("a missing directory must be named: %q", out)
	}
	if after, _ := os.Getwd(); after != now {
		t.Fatal("a missing directory must leave the process where it is")
	}
}

func TestConsumePendingResumeSelectionIsOneShot(t *testing.T) {
	rec := recordPrints(t)
	c := bareCLI()
	if c.consumePendingResumeSelection("3") {
		t.Fatal("no resume prompt is armed")
	}
	c.pendingResumeSessions = []map[string]any{{"id": "a"}, {"id": "b"}}
	if c.consumePendingResumeSelection("hello") || c.pendingResumeSessions != nil {
		t.Fatal("other input disarms the prompt and passes through")
	}
	c.pendingResumeSessions = []map[string]any{{"id": "a"}, {"id": "b"}}
	if !c.consumePendingResumeSelection("7") || len(rec.raw()) != 2 || c.pendingResumeSessions != nil {
		t.Fatalf("an out of range number must be consumed with its two hint lines: %q", rec.raw())
	}
}

func TestDiscardSessionIfEmptyKeepsALiveTranscript(t *testing.T) {
	recordPrints(t)
	c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{})
	if c.discardSessionIfEmpty("") {
		t.Fatal("no session id means nothing to discard")
	}
	c.conversationHistory = []map[string]any{{"role": "user", "content": "hi"}}
	if c.discardSessionIfEmpty(c.sessionID) {
		t.Fatal("a session with an in memory transcript must never be pruned")
	}
}
