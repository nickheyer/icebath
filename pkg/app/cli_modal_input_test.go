package app

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
)

var paletteEntries = []commandPaletteEntry{
	{"/model", "Model", "Switch model"},
	{"/compress", "Context", "Compress the conversation"},
	{"/goal", "Background", "Set an autonomous goal"},
	{"/status", "Context", "Show session status"},
}

// Commands of the palette rows visible for a query over the entries
func paletteCommands(entries []commandPaletteEntry, query string) []string {
	c := &CLI{ctx: context.Background(), commandPaletteState: &commandPaletteModalState{Entries: entries, Filter: query}}
	var out []string
	for _, row := range c.commandPaletteVisibleEntries() {
		out = append(out, row.Command)
	}
	return out
}

func TestCommandPaletteFilter(t *testing.T) {
	t.Run("test_empty_query_returns_all", func(t *testing.T) {
		c := &CLI{ctx: context.Background(), commandPaletteState: &commandPaletteModalState{Entries: paletteEntries}}
		if got := c.commandPaletteVisibleEntries(); !reflect.DeepEqual(got, paletteEntries) {
			t.Fatalf("rows %v", got)
		}
	})
	t.Run("test_filter_by_command_name", func(t *testing.T) {
		if got := paletteCommands(paletteEntries, "model"); !reflect.DeepEqual(got, []string{"/model"}) {
			t.Fatalf("rows %v", got)
		}
	})
	t.Run("test_filter_by_description", func(t *testing.T) {
		if got := paletteCommands(paletteEntries, "autonomous"); !reflect.DeepEqual(got, []string{"/goal"}) {
			t.Fatalf("rows %v", got)
		}
	})
	t.Run("test_subsequence_match", func(t *testing.T) {
		if got := paletteCommands(paletteEntries, "cmprs"); indexOf(got, "/compress") < 0 {
			t.Fatalf("rows %v", got)
		}
	})
	t.Run("test_no_match", func(t *testing.T) {
		if got := paletteCommands(paletteEntries, "zzznope"); len(got) != 0 {
			t.Fatalf("rows %v", got)
		}
	})
	t.Run("test_command_name_matches_rank_above_description", func(t *testing.T) {
		entries := []commandPaletteEntry{{"/goal", "Background", "check the status of things"}, {"/status", "Context", "Show session status"}}
		if got := paletteCommands(entries, "status"); len(got) == 0 || got[0] != "/status" {
			t.Fatalf("rows %v", got)
		}
	})
	t.Run("test_query_does_not_overmatch_via_description", func(t *testing.T) {
		entries := []commandPaletteEntry{{"/new", "Session", "Start a totally separate resettable session"}}
		if got := paletteCommands(entries, "steer"); len(got) != 0 {
			t.Fatalf("rows %v", got)
		}
	})
}

func TestCommandPaletteSelectionInserts(t *testing.T) {
	t.Run("test_selection_prefills_exact_command", func(t *testing.T) {
		c, app := newModalTestCLI(t, nil)
		c.commandPaletteState = &commandPaletteModalState{Entries: []commandPaletteEntry{{"/model", "Model", "Switch model"}}}
		c.handleCommandPaletteSelection()
		if app.BufferText() != "/model " || c.commandPaletteState != nil {
			t.Fatalf("buffer %q palette %v", app.BufferText(), c.commandPaletteState)
		}
	})
}

// Routes console input reads to the given text for the test
func withConsoleInput(t *testing.T, input string) *bytes.Buffer {
	t.Helper()
	out := &bytes.Buffer{}
	restore := SetStreams(&cliui.Console{In: strings.NewReader(input), Out: out}, out)
	t.Cleanup(restore)
	return out
}

// inlineTerminalApp is an application on its loop whose terminal handoff runs the function at once
type inlineTerminalApp struct{ *fakeTUIApp }

func (a inlineTerminalApp) RunInTerminal(fn func()) { fn() }

func TestPromptTextInputThreadSafety(t *testing.T) {
	t.Run("test_background_thread_cancels_instead_of_hanging", func(t *testing.T) {
		c, app := newModalTestCLI(t, nil)
		withConsoleInput(t, "must not be read\n")
		done := make(chan [2]any, 1)
		go func() {
			value, ok := c.promptTextInput("Choice [1/2/3]: ")
			done <- [2]any{value, ok}
		}()
		select {
		case got := <-done:
			if got[0] != "" || got[1] != false || len(app.inTerminal) != 0 {
				t.Fatalf("result %v terminal handoffs %d", got, len(app.inTerminal))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the prompt hung off the loop")
		}
	})
	t.Run("test_no_app_uses_direct_input", func(t *testing.T) {
		c, _ := newModalTestCLI(t, nil)
		c.app = nil
		withConsoleInput(t, "cancel\n")
		if value, ok := c.promptTextInput("Choice: "); !ok || value != "cancel" {
			t.Fatalf("result %q %v", value, ok)
		}
	})
	t.Run("test_run_in_terminal_exception_falls_back", func(t *testing.T) {
		c, app := newModalTestCLI(t, nil)
		app.onLoop = true
		c.app = inlineTerminalApp{app}
		withConsoleInput(t, "3\n")
		if value, ok := c.promptTextInput("Choice: "); !ok || value != "3" {
			t.Fatalf("result %q %v", value, ok)
		}
	})
	t.Run("test_eof_returns_none", func(t *testing.T) {
		c, _ := newModalTestCLI(t, nil)
		c.app = nil
		withConsoleInput(t, "")
		if value, ok := c.promptTextInput("Choice: "); ok || value != "" {
			t.Fatalf("result %q %v", value, ok)
		}
	})
}

// inlineLoopApp is a running application whose loop runs a scheduled callback at once
type inlineLoopApp struct{ *fakeTUIApp }

func (a inlineLoopApp) CallSoonThreadsafe(fn func()) bool { fn(); return true }

// deadLoopApp is a running application whose loop refuses every callback
type deadLoopApp struct{ *fakeTUIApp }

func (a deadLoopApp) CallSoonThreadsafe(func()) bool { return false }

var sampleConfirmChoices = []slashConfirmChoice{
	{"once", "Approve Once", "proceed this time only"},
	{"always", "Always Approve", "proceed and silence this prompt permanently"},
	{"cancel", "Cancel", "keep current conversation"},
}

// Answers the slash confirm modal with the response once it opens
func answerModalWhenOpen(c *CLI, response string, stop <-chan struct{}) {
	for i := 0; i < 100; i++ {
		select {
		case <-stop:
			return
		default:
		}
		c.uiMu.Lock()
		state := c.slashConfirmState
		c.uiMu.Unlock()
		if state != nil {
			state.response <- response
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Runs call off the loop the way the process loop does, the modal answered once open, failing when it hangs
func runOffLoop(t *testing.T, c *CLI, response string, call func() (string, bool)) (string, bool) {
	t.Helper()
	type result struct {
		value string
		ok    bool
	}
	done, stop := make(chan result, 1), make(chan struct{})
	go answerModalWhenOpen(c, response, stop)
	go func() {
		value, ok := call()
		done <- result{value, ok}
	}()
	select {
	case r := <-done:
		close(stop)
		return r.value, r.ok
	case <-time.After(5 * time.Second):
		close(stop)
		t.Fatal("the confirm hung off the loop")
		return "", false
	}
}

// A CLI whose application runs a loop, console input reads failing the test when reached
func modalLoopCLI(t *testing.T) (*CLI, *fakeTUIApp) {
	t.Helper()
	c, app := newModalTestCLI(t, map[string]any{"approvals": map[string]any{"destructive_slash_confirm": true}})
	app.running, app.loop = true, true
	c.app = inlineLoopApp{app}
	withConsoleInput(t, "")
	return c, app
}

func TestModal(t *testing.T) {
	t.Run("test_daemon_thread_uses_modal_via_app_loop", func(t *testing.T) {
		c, app := modalLoopCLI(t)
		app.SetBuffer("draft", 5)
		value, ok := runOffLoop(t, c, "once", func() (string, bool) {
			return c.promptTextInputModal("⚠️  /reset", "This starts a fresh session.", sampleConfirmChoices, 5)
		})
		if !ok || value != "once" || c.slashConfirmState != nil || app.BufferText() != "draft" || app.bufferResets() != 1 {
			t.Fatalf("value %q ok %v state %v buffer %q resets %d", value, ok, c.slashConfirmState, app.BufferText(), app.bufferResets())
		}
	})
	t.Run("test_main_thread_with_app_uses_modal", func(t *testing.T) {
		c, app := modalLoopCLI(t)
		app.onLoop = true
		stop := make(chan struct{})
		defer close(stop)
		go answerModalWhenOpen(c, "once", stop)
		if value, ok := c.promptTextInputModal("⚠️  /new", "This starts a fresh session.", sampleConfirmChoices, 5); !ok || value != "once" {
			t.Fatalf("value %q ok %v", value, ok)
		}
	})
}

func TestConfirmDestructiveSlash(t *testing.T) {
	t.Run("test_confirm_destructive_slash_uses_modal", func(t *testing.T) {
		for response, expected := range map[string]string{"once": "once", "cancel": ""} {
			c, _ := modalLoopCLI(t)
			value, _ := runOffLoop(t, c, response, func() (string, bool) {
				return c.confirmDestructiveSlash("new", "This starts a fresh session.\nThe current conversation history will be discarded.", ""), true
			})
			if value != expected {
				t.Fatalf("response %q returned %q", response, value)
			}
		}
	})
}

func TestNativeWindowsNoRawInputDeadlock(t *testing.T) {
	onWindows := func(t *testing.T) {
		prev := modalGOOS
		modalGOOS = "windows"
		t.Cleanup(func() { modalGOOS = prev })
	}
	t.Run("test_win32_daemon_thread_never_blocks_on_real_input", func(t *testing.T) {
		onWindows(t)
		c, _ := modalLoopCLI(t)
		value, _ := runOffLoop(t, c, "cancel", func() (string, bool) {
			return c.promptTextInputModal("/new", "destroys conversation state", sampleConfirmChoices, 3)
		})
		if value != "" && value != "cancel" {
			t.Fatalf("value %q", value)
		}
	})
	t.Run("test_win32_scheduling_failure_cleanly_cancels_no_input", func(t *testing.T) {
		onWindows(t)
		c, app := modalLoopCLI(t)
		c.app = deadLoopApp{app}
		out := withConsoleInput(t, "1\n")
		value, ok := runOffLoop(t, c, "once", func() (string, bool) {
			return c.promptTextInputModal("/new", "destroys conversation state", sampleConfirmChoices, 3)
		})
		if ok || value != "" || out.Len() != 0 {
			t.Fatalf("value %q ok %v prompt output %q", value, ok, out.String())
		}
	})
}

// A CLI with no application whose console answers the confirm prompt with the typed line
func typedConfirmCLI(t *testing.T, typed string) *CLI {
	t.Helper()
	c, _ := newModalTestCLI(t, map[string]any{"approvals": map[string]any{"destructive_slash_confirm": true}})
	c.app = nil
	withConsoleInput(t, typed+"\n")
	return c
}

func TestGateOnChoiceOnceReturnsOnce(t *testing.T) {
	if got := typedConfirmCLI(t, "1").confirmDestructiveSlash("clear", "detail", ""); got != "once" {
		t.Fatalf("got %q", got)
	}
}

func TestGateOnChoiceAlwaysPersistsAndReturnsAlways(t *testing.T) {
	c := typedConfirmCLI(t, "2")
	if got := c.confirmDestructiveSlash("clear", "detail", ""); got != "always" {
		t.Fatalf("got %q", got)
	}
	if approvalGateOn(c.ctx, "destructive_slash_confirm") {
		t.Fatal("the opt out was not persisted")
	}
}

func TestSplitDestructiveSkipRecognizedTokens(t *testing.T) {
	for _, text := range []string{"/reset now", "/clear --yes", "/undo -y"} {
		if rest, skip := splitDestructiveSkip(text); rest != "" || !skip {
			t.Fatalf("%q split into %q %v", text, rest, skip)
		}
	}
}

func TestSplitDestructiveSkipHandlesEmptyAndNone(t *testing.T) {
	for _, text := range []string{"", "   "} {
		if rest, skip := splitDestructiveSkip(text); rest != "" || skip {
			t.Fatalf("%q split into %q %v", text, rest, skip)
		}
	}
}

func TestConfirmDestructiveSlashNowSkipsModal(t *testing.T) {
	c := typedConfirmCLI(t, "3")
	if got := c.confirmDestructiveSlash("new", "detail", "/reset now"); got != "once" {
		t.Fatalf("got %q", got)
	}
}

func TestConfirmDestructiveSlashNoSkipTokenStillPrompts(t *testing.T) {
	if got := typedConfirmCLI(t, "3").confirmDestructiveSlash("new", "detail", "/new My Session"); got != "" {
		t.Fatalf("got %q", got)
	}
}
