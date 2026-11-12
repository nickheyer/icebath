package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Env lookup over a fixed map
func envMap(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

// Sets the macOS flag for one case
func onMac(t *testing.T, mac bool) {
	prev := isMac
	isMac = mac
	t.Cleanup(func() { isMac = prev })
}

func TestCtrlDeleteDecodeContract(t *testing.T) {
	t.Run("decodes ESC d as meta+d so the composer binding is reached", func(t *testing.T) {
		ev := keyEvent(tea.KeyPressMsg{Code: 'd', Mod: tea.ModAlt})
		if !ev.Key.Meta || ev.Key.Ctrl || ev.Input != "d" {
			t.Fatalf("got %+v", ev)
		}
	})
}

func TestDeleteWordForward(t *testing.T) {
	cases := []struct {
		name, value string
		cursor      int
		want        textInsertResult
	}{
		{"deletes the word to the right of the cursor", "foo hello world", 4, textInsertResult{4, "foo world"}},
		{"deletes from mid-word to the next word boundary", "foo hello world", 6, textInsertResult{6, "foo heworld"}},
		{"is a no-op when the cursor is already at the end", "foo bar", 7, textInsertResult{7, "foo bar"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deleteWordForward(c.value, c.cursor); got != c.want {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}

func TestIsLineKillModifier(t *testing.T) {
	cases := []struct {
		name string
		key  inkKey
		want bool
	}{
		{"accepts the super bit (Cmd via kitty CSI-u / modifyOtherKeys)", inkKey{Super: true}, true},
		{"accepts super even when the terminal also sets a benign ctrl bit", inkKey{Ctrl: true, Super: true}, true},
		{"rejects meta so Option+Backspace stays delete-word on macOS", inkKey{Meta: true}, false},
		{"rejects ctrl so Ctrl+Backspace stays delete-word on Linux/Windows", inkKey{Ctrl: true}, false},
		{"rejects an unmodified keypress", inkKey{}, false},
		{"treats a missing super field as absent rather than truthy", inkKey{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isLineKillModifier(c.key); got != c.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestKillToLineStart(t *testing.T) {
	t.Run("clears the whole value in single-line input", func(t *testing.T) {
		if got := killToLineStart("hello world", 11); got != (textInsertResult{0, ""}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("keeps text after the cursor", func(t *testing.T) {
		if got := killToLineStart("hello world", 6); got != (textInsertResult{0, "world"}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("only kills the current line, leaving earlier lines intact", func(t *testing.T) {
		if got := killToLineStart("one\ntwo", 7); got != (textInsertResult{4, "one\n"}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("repeated presses walk up a multiline draft to empty", func(t *testing.T) {
		state := textInsertResult{11, "one\ntwo\nsix"}
		var seen []string
		for i := 0; i < 6 && state.Value != ""; i++ {
			state = killToLineStart(state.Value, state.Cursor)
			seen = append(seen, state.Value)
		}
		if strings.Join(seen, "|") != strings.Join([]string{"one\ntwo\n", "one\ntwo", "one\n", "one", ""}, "|") {
			t.Fatalf("seen %q", seen)
		}
		if state != (textInsertResult{0, ""}) {
			t.Fatalf("state %+v", state)
		}
	})
	t.Run("is a no-op at the very start of the buffer", func(t *testing.T) {
		if got := killToLineStart("abc", 0); got != (textInsertResult{0, "abc"}) {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestKillToLineEnd(t *testing.T) {
	cases := []struct {
		name, value string
		cursor      int
		want        textInsertResult
	}{
		{"kills to end of a single-line value", "hello world", 6, textInsertResult{6, "hello "}},
		{"stops at the newline, leaving later lines intact", "one\ntwo", 0, textInsertResult{0, "\ntwo"}},
		{"consumes the newline when already at a line end, joining the next line", "one\ntwo", 3, textInsertResult{3, "onetwo"}},
		{"is a no-op at the very end of the buffer", "abc", 3, textInsertResult{3, "abc"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := killToLineEnd(c.value, c.cursor); got != c.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestLineNav(t *testing.T) {
	cases := []struct {
		name, s string
		p, dir  int
		want    int
		ok      bool
	}{
		{"returns null for single-line input (up)", "hello world", 6, -1, 0, false},
		{"returns null when cursor already on first line of a multiline block", "one\ntwo\nthree", 2, -1, 0, false},
		{"returns null when cursor on last line of a multiline block", "one\ntwo\nthree", 10, 1, 0, false},
		{"moves cursor up one line preserving column", "hello\nworld", 9, -1, 3, true},
		{"moves cursor down one line preserving column", "hello\nworld", 2, 1, 8, true},
		{"clamps to end of shorter destination line on up", "abc\nlong long text", 14, -1, 3, true},
		{"clamps to end of shorter destination line on down", "long long text\nabc", 10, 1, 18, true},
		{"handles empty lines correctly", "a\n\nb", 3, -1, 2, true},
		{"handles leading newline without crashing", "\nfoo", 2, -1, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := lineNav(c.s, c.p, c.dir)
			if ok != c.ok || (ok && got != c.want) {
				t.Fatalf("got %d %v want %d %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestShouldPreserveCtrlJNewline(t *testing.T) {
	t.Run("preserves Ctrl+J as newline in Ghostty even when tmux masks TERM/TERM_PROGRAM", func(t *testing.T) {
		if !shouldPreserveCtrlJNewline(envMap(map[string]string{"GHOSTTY_RESOURCES_DIR": "/usr/share/ghostty", "TERM": "tmux-256color", "TERM_PROGRAM": "tmux"})) {
			t.Fatal("want true")
		}
	})
	t.Run("keeps bare local POSIX LF-compatible prompts submitting on Ctrl+J", func(t *testing.T) {
		if shouldPreserveCtrlJNewline(envMap(map[string]string{"TERM": "xterm-256color"})) {
			t.Fatal("want false")
		}
	})
}

func TestShouldPassThroughToGlobalHandler(t *testing.T) {
	t.Run("passes through the configured voice shortcut while composer is focused", func(t *testing.T) {
		for _, c := range []struct {
			in  string
			k   inkKey
			raw string
		}{{"o", inkKey{Ctrl: true}, "ctrl+o"}, {"r", inkKey{Meta: true}, "alt+r"}, {" ", inkKey{Ctrl: true}, "ctrl+space"}, {"", inkKey{Ctrl: true, Return: true}, "ctrl+enter"}} {
			if !shouldPassThroughToGlobalHandler(c.in, c.k, parseVoiceRecordKey(c.raw)) {
				t.Fatalf("%s not passed", c.raw)
			}
		}
	})
	t.Run("keeps the legacy default pass-through when no custom key is provided", func(t *testing.T) {
		if !shouldPassThroughToGlobalHandler("b", inkKey{Ctrl: true}, defaultVoiceRecordKey) {
			t.Fatal("want true")
		}
	})
	t.Run("does not swallow ordinary typing keys", func(t *testing.T) {
		if shouldPassThroughToGlobalHandler("h", inkKey{}, parseVoiceRecordKey("ctrl+o")) || shouldPassThroughToGlobalHandler("o", inkKey{}, parseVoiceRecordKey("ctrl+o")) {
			t.Fatal("want false")
		}
	})
	t.Run("always passes through non-voice global control keys", func(t *testing.T) {
		for _, c := range []struct {
			in string
			k  inkKey
		}{{"c", inkKey{Ctrl: true}}, {"x", inkKey{Ctrl: true}}, {"o", inkKey{Ctrl: true}}, {"", inkKey{Escape: true}}, {"", inkKey{Tab: true}}, {"", inkKey{PageUp: true}}, {"", inkKey{PageDown: true}}} {
			if !shouldPassThroughToGlobalHandler(c.in, c.k, defaultVoiceRecordKey) {
				t.Fatalf("%+v not passed", c)
			}
		}
	})
}

func TestDecideRightClickAction(t *testing.T) {
	check := func(t *testing.T, value string, rng *textRange, wantText string, wantCopy bool) {
		text, isCopy := decideRightClickAction(value, rng)
		if text != wantText || isCopy != wantCopy {
			t.Fatalf("got %q %v", text, isCopy)
		}
	}
	t.Run("returns paste when there is no selection", func(t *testing.T) { check(t, "hello world", nil, "", false) })
	t.Run("returns paste for a collapsed (empty) range", func(t *testing.T) { check(t, "hello world", &textRange{5, 5}, "", false) })
	t.Run("copies the slice when range covers non-empty text", func(t *testing.T) { check(t, "hello world", &textRange{0, 5}, "hello", true) })
	t.Run("falls back to paste when slice is empty (out-of-range indices)", func(t *testing.T) { check(t, "", &textRange{0, 5}, "", false) })
	t.Run("handles unicode (emoji, CJK) in the slice", func(t *testing.T) {
		value := "hi \u4f60\u597d \U0001f389"
		check(t, value, &textRange{3, 3 + len("\u4f60\u597d")}, "\u4f60\u597d", true)
	})
}

func TestApplyPrintableInsert(t *testing.T) {
	t.Run("applies non-bracketed multi-character bursts immediately", func(t *testing.T) {
		burst := applyPrintableInsert("abc", 3, "xxxxx", nil)
		state := textInsertResult{3, "abc"}
		for _, ch := range "xxxxx" {
			state = *applyPrintableInsert(state.Value, state.Cursor, string(ch), nil)
		}
		if *burst != (textInsertResult{8, "abcxxxxx"}) || *burst != state {
			t.Fatalf("burst %+v repeated %+v", *burst, state)
		}
	})
	t.Run("replaces the selected range for burst input", func(t *testing.T) {
		if got := applyPrintableInsert("abZZef", 4, "cd", &textRange{Start: 2, End: 4}); *got != (textInsertResult{4, "abcdef"}) {
			t.Fatalf("got %+v", *got)
		}
	})
	t.Run("rejects control or escape-bearing input", func(t *testing.T) {
		if applyPrintableInsert("abc", 3, "\x1b[200~pasted", nil) != nil || applyPrintableInsert("abc", 3, "\t", nil) != nil {
			t.Fatal("want nil")
		}
	})
}

func TestShouldRouteMultiCharInputAsPaste(t *testing.T) {
	t.Run("keeps newline-bearing chunks on the paste path", func(t *testing.T) {
		if !shouldRouteMultiCharInputAsPaste("hello\nworld") || !shouldRouteMultiCharInputAsPaste(strings.ReplaceAll("hello\r\nworld", "\r\n", "\n")) {
			t.Fatal("want true")
		}
	})
	t.Run("treats repeated printable key bursts as immediate input", func(t *testing.T) {
		if shouldRouteMultiCharInputAsPaste("xxxxx") {
			t.Fatal("want false")
		}
	})
}

func TestValueForReturnSubmit(t *testing.T) {
	draft := "\u4e3a\u4ec0\u4e48\u6253\u5b57\u4e0a\u5c4f\uff0c"
	commit := "\u4f1a\u4e22\u5931\u5185\u5bb9"
	t.Run("includes printable input that arrives in the same keypress as return", func(t *testing.T) {
		if got := valueForReturnSubmit(draft, len(draft), commit, nil); got != (textInsertResult{len(draft + commit), draft + commit}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("keeps IME commit text when it arrives in the same burst as return", func(t *testing.T) {
		if got := valueForReturnSubmit(draft, len(draft), commit+"\r", nil); got != (textInsertResult{len(draft + commit), draft + commit}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("leaves the draft unchanged when return carries no printable input", func(t *testing.T) {
		for _, in := range []string{"", "\r", "\n"} {
			if got := valueForReturnSubmit("hello", 5, in, nil); got != (textInsertResult{5, "hello"}) {
				t.Fatalf("%q got %+v", in, got)
			}
		}
	})
}

// Clipboard write double recording each text and answering ok
func fakeClipboardWrite(t *testing.T, ok bool) *[]string {
	var writes []string
	prev := clipboardWrite
	clipboardWrite = func(_ context.Context, text string) bool {
		writes = append(writes, text)
		return ok
	}
	t.Cleanup(func() { clipboardWrite = prev })
	return &writes
}

// Runs a command and feeds its message back to the input
func runCmd(t *testing.T, ti *textInput, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if msg := cmd(); msg != nil {
		ti.update(msg)
	}
}

func TestCutSelectionTransactionalCut(t *testing.T) {
	selected := func() *textInput {
		ti := newTextInput(context.Background())
		ti.setValue("hello world")
		ti.sel = &textRange{Start: 0, End: 5}
		return ti
	}
	t.Run("removes the selection only after the clipboard write succeeds", func(t *testing.T) {
		writes := fakeClipboardWrite(t, true)
		ti := selected()
		cmd := ti.cutCmd()
		if ti.value != "hello world" {
			t.Fatalf("removed before write: %q", ti.value)
		}
		runCmd(t, ti, cmd)
		if strings.Join(*writes, "|") != "hello" || ti.value != " world" {
			t.Fatalf("writes %q value %q", *writes, ti.value)
		}
	})
	t.Run("keeps the text intact when the clipboard write fails (headless/SSH)", func(t *testing.T) {
		fakeClipboardWrite(t, false)
		ti := selected()
		runCmd(t, ti, ti.cutCmd())
		if ti.value != "hello world" {
			t.Fatalf("value %q", ti.value)
		}
	})
	t.Run("awaits the write before removing (no fire-and-forget removal)", func(t *testing.T) {
		writes := fakeClipboardWrite(t, true)
		ti := selected()
		cmd := ti.cutCmd()
		if len(*writes) != 0 || ti.value != "hello world" {
			t.Fatal("cut ran before its command")
		}
		runCmd(t, ti, cmd)
		if ti.value != " world" {
			t.Fatalf("value %q", ti.value)
		}
	})
}

func TestTextInputRedoChordUnderExtendedKeyTerminals(t *testing.T) {
	t.Run(`Cmd+Shift+Z delivered as uppercase "Z" (kitty CSI-u) redoes instead of typing Z`, func(t *testing.T) {
		onMac(t, true)
		ti := newTextInput(context.Background())
		for _, ch := range "ab" {
			ti.handleKey(keyEvent(tea.KeyPressMsg{Code: ch, Text: string(ch)}))
		}
		ti.handleKey(keyEvent(tea.KeyPressMsg{Code: 'z', Mod: tea.ModSuper}))
		if ti.value != "a" {
			t.Fatalf("undo left %q", ti.value)
		}
		ti.handleKey(inputEvent{Input: "Z", Key: inkKey{Super: true, Shift: true}})
		if ti.value != "ab" {
			t.Fatalf("redo left %q", ti.value)
		}
	})
}

// Types each rune of text as a keypress
func typeText(ti *textInput, text string) {
	for _, ch := range text {
		ti.handleKey(keyEvent(tea.KeyPressMsg{Code: ch, Text: string(ch)}))
	}
}

func TestTextInputSubmitClearing(t *testing.T) {
	newCleared := func(clear bool) (*textInput, *[]string) {
		var submitted []string
		ti := newTextInput(context.Background())
		ti.onSubmit = func(v string) {
			submitted = append(submitted, v)
			if clear {
				ti.setValue("")
			}
		}
		return ti, &submitted
	}
	enter := keyEvent(tea.KeyPressMsg{Code: tea.KeyEnter})
	t.Run("accepts the parent clear after a Korean IME commit immediately followed by Enter", func(t *testing.T) {
		ti, submitted := newCleared(true)
		ti.handleKey(inputEvent{Input: "\uc548\ub155"})
		ti.handleKey(enter)
		if strings.Join(*submitted, "|") != "\uc548\ub155" || ti.value != "" {
			t.Fatalf("submitted %q value %q", *submitted, ti.value)
		}
	})
	t.Run("clears the composer when text and a CSI-u Enter arrive in one read", func(t *testing.T) {
		ti, submitted := newCleared(true)
		typeText(ti, "hi")
		ti.handleKey(enter)
		if strings.Join(*submitted, "|") != "hi" || ti.value != "" {
			t.Fatalf("submitted %q value %q", *submitted, ti.value)
		}
	})
	t.Run("starts the cleared draft with the key typed after Enter", func(t *testing.T) {
		ti, _ := newCleared(true)
		typeText(ti, "hi")
		ti.handleKey(enter)
		typeText(ti, "x")
		if ti.value != "x" || ti.cur != 1 {
			t.Fatalf("value %q cur %d", ti.value, ti.cur)
		}
	})
	t.Run("keeps the draft and the key typed after Enter when the parent does not clear", func(t *testing.T) {
		ti, _ := newCleared(false)
		typeText(ti, "hi")
		ti.handleKey(enter)
		typeText(ti, "x")
		if ti.value != "hix" {
			t.Fatalf("value %q", ti.value)
		}
	})
	t.Run("submits each line and keeps the tail when one read holds two CSI-u Enters", func(t *testing.T) {
		ti, submitted := newCleared(true)
		typeText(ti, "one")
		ti.handleKey(enter)
		typeText(ti, "two")
		ti.handleKey(enter)
		typeText(ti, "tail")
		if strings.Join(*submitted, "|") != "one|two" || ti.value != "tail" {
			t.Fatalf("submitted %q value %q", *submitted, ti.value)
		}
	})
}
