package app

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
)

// State of the prompt after a vi key sequence, compared against prompt_toolkit 3.0.53
type viOutcome struct {
	text   string
	cursor int
	mode   ptInputMode
	clip   ptClipboardData
	regs   map[string]ptClipboardData
	multi  []int
}

// Key press of a case token, angle brackets naming a special key
func viPress(tok string) ptKeyPress {
	named := map[string]ptKeyPress{"<escape>": {Key: "escape", Data: "\x1b"}, "<c-v>": {Key: "c-v", Data: "\x16"},
		"<c-k>": {Key: "c-k", Data: "\x0b"}, "<c-o>": {Key: "c-o", Data: "\x0f"}, "<backspace>": {Key: "c-h", Data: "\x7f"},
		"<enter>": {Key: "c-m", Data: "\r"}}
	if k, ok := named[tok]; ok {
		return k
	}
	return ptKeyPress{Key: tok, Data: tok}
}

// Vi application over text with the cursor at cursor, after the keys and a final flush
func viRun(text string, cursor int, keys []string, readOnly bool) (*ptApplication, viOutcome) {
	app := newPTApplication(nil, io.Discard)
	ptLoadViBindings(app)
	app.timeoutLen = 0
	if readOnly {
		app.buffer.readOnly = func() bool { return true }
	}
	_ = app.buffer.setDocument(text, cursor, true)
	app.viState.reset()
	for _, tok := range keys {
		app.feedKeys(viPress(tok))
	}
	app.processor.flushKeys()
	b := app.buffer
	multi := append([]int{}, b.multipleCursors...)
	return app, viOutcome{text: b.text(), cursor: b.cursor, mode: app.viState.inputMode, clip: app.clipboard.getData(),
		regs: app.viState.namedRegisters, multi: multi}
}

func TestPTViKeySequencesMatchPromptToolkit(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		cursor int
		keys   []string
		want   viOutcome
	}{
		{name: "insert_then_escape", text: "", cursor: 0, keys: []string{"h", "e", "l", "l", "o", "<escape>"},
			want: viOutcome{text: "hello", cursor: 4, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "word_motions", text: "hello world foo", cursor: 15, keys: []string{"<escape>", "0", "w", "w", "b", "e"},
			want: viOutcome{text: "hello world foo", cursor: 10, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "dollar_and_caret", text: "  indented line", cursor: 0, keys: []string{"<escape>", "$", "^"},
			want: viOutcome{text: "  indented line", cursor: 2, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "replace_mode", text: "abcdef", cursor: 0, keys: []string{"<escape>", "R", "X", "Y", "<escape>"},
			want: viOutcome{text: "XYcdef", cursor: 1, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "replace_single", text: "abcdef", cursor: 0, keys: []string{"<escape>", "r", "Z"},
			want: viOutcome{text: "Zbcdef", cursor: 0, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "operator_dw", text: "one two three", cursor: 0, keys: []string{"<escape>", "0", "d", "w"},
			want: viOutcome{text: "two three", cursor: 0, mode: ptViNavigation, clip: ptClipboardData{text: "one ", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "operator_pending_mode", text: "one two three", cursor: 0, keys: []string{"<escape>", "0", "d"},
			want: viOutcome{text: "one two three", cursor: 0, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "change_inner_word", text: "foo bar baz", cursor: 5, keys: []string{"<escape>", "c", "i", "w", "X", "X", "<escape>"},
			want: viOutcome{text: "foo XX baz", cursor: 5, mode: ptViNavigation, clip: ptClipboardData{text: "bar", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "delete_inner_parens", text: "call(a, b) x", cursor: 6, keys: []string{"<escape>", "d", "i", "("},
			want: viOutcome{text: "call() x", cursor: 5, mode: ptViNavigation, clip: ptClipboardData{text: "a, b", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "delete_a_quote", text: "say \"hi there\" now", cursor: 7, keys: []string{"<escape>", "d", "a", "\""},
			want: viOutcome{text: "say  now", cursor: 4, mode: ptViNavigation, clip: ptClipboardData{text: "\"hi there\"", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "register_yank_paste", text: "line1\nline2", cursor: 0, keys: []string{"<escape>", "g", "g", "V", "\"", "a", "y", "j", "\"", "a", "p"},
			want: viOutcome{text: "line1\nline2\nline1", cursor: 12, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{"a": {text: "line1", typ: ptSelectLines}}, multi: []int{}}},
		{name: "register_delete", text: "alpha beta", cursor: 0, keys: []string{"<escape>", "0", "v", "e", "\"", "b", "d", "\"", "b", "P"},
			want: viOutcome{text: "alpha beta", cursor: 4, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{"b": {text: "alpha", typ: ptSelectCharacters}}, multi: []int{}}},
		{name: "undo_after_x", text: "abc", cursor: 0, keys: []string{"<escape>", "0", "x", "x", "u"},
			want: viOutcome{text: "bc", cursor: 0, mode: ptViNavigation, clip: ptClipboardData{text: "b", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "visual_delete", text: "hello world", cursor: 0, keys: []string{"<escape>", "0", "v", "e", "d"},
			want: viOutcome{text: " world", cursor: 0, mode: ptViNavigation, clip: ptClipboardData{text: "hello", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "visual_line_yank", text: "aa\nbb\ncc", cursor: 0, keys: []string{"<escape>", "g", "g", "V", "j", "y", "G", "p"},
			want: viOutcome{text: "aa\nbb\ncc\naa\nbb", cursor: 9, mode: ptViNavigation, clip: ptClipboardData{text: "aa\nbb", typ: ptSelectLines}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "count_dd", text: "a\nb\nc\nd", cursor: 0, keys: []string{"<escape>", "g", "g", "2", "d", "d"},
			want: viOutcome{text: "c\nd", cursor: 0, mode: ptViNavigation, clip: ptClipboardData{text: "a\nb", typ: ptSelectLines}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "macro_record_play", text: "abcdef", cursor: 0, keys: []string{"<escape>", "0", "q", "a", "x", "q", "@", "a"},
			want: viOutcome{text: "cdef", cursor: 0, mode: ptViNavigation, clip: ptClipboardData{text: "b", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{"a": {text: "x", typ: ptSelectCharacters}}, multi: []int{}}},
		{name: "digraph_insert", text: "", cursor: 0, keys: []string{"<c-k>", "e", ":"},
			want: viOutcome{text: "ë", cursor: 1, mode: ptViInsert, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "tilde_swap", text: "abC", cursor: 0, keys: []string{"<escape>", "0", "~", "~", "~"},
			want: viOutcome{text: "ABc", cursor: 2, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "indent_lines", text: "x\ny", cursor: 0, keys: []string{"<escape>", "g", "g", ">", ">"},
			want: viOutcome{text: "    x\ny", cursor: 4, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "join_lines", text: "a\n   b", cursor: 0, keys: []string{"<escape>", "g", "g", "J"},
			want: viOutcome{text: "a b", cursor: 1, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "paste_after_char", text: "ab", cursor: 0, keys: []string{"<escape>", "0", "x", "p"},
			want: viOutcome{text: "ba", cursor: 1, mode: ptViNavigation, clip: ptClipboardData{text: "a", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "block_insert", text: "ab\ncd", cursor: 0, keys: []string{"<escape>", "g", "g", "<c-v>", "j", "I", "#", "<escape>"},
			want: viOutcome{text: "#ab\n#cd", cursor: 1, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{1, 5}}},
		{name: "temporary_navigation", text: "abc", cursor: 3, keys: []string{"<c-o>", "0", "Z"},
			want: viOutcome{text: "Zabc", cursor: 1, mode: ptViInsert, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "find_and_repeat", text: "a,b,c,d", cursor: 0, keys: []string{"<escape>", "0", "f", ",", ";", ";"},
			want: viOutcome{text: "a,b,c,d", cursor: 5, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "till_and_reverse", text: "a,b,c,d", cursor: 0, keys: []string{"<escape>", "$", "T", ",", ","},
			want: viOutcome{text: "a,b,c,d", cursor: 6, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "percent_bracket", text: "f(a[b]c)", cursor: 1, keys: []string{"<escape>", "0", "l", "%"},
			want: viOutcome{text: "f(a[b]c)", cursor: 7, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "change_to_end", text: "keep this", cursor: 5, keys: []string{"<escape>", "C", "!", "<escape>"},
			want: viOutcome{text: "keep!", cursor: 4, mode: ptViNavigation, clip: ptClipboardData{text: " this", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "open_line_below", text: "  a", cursor: 0, keys: []string{"<escape>", "o", "b", "<escape>"},
			want: viOutcome{text: "  a\n  b", cursor: 6, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "gu_text_object", text: "HELLO World", cursor: 0, keys: []string{"<escape>", "0", "g", "u", "w"},
			want: viOutcome{text: "hello World", cursor: 6, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "yank_word_paste", text: "ab cd", cursor: 0, keys: []string{"<escape>", "0", "y", "w", "$", "p"},
			want: viOutcome{text: "ab cdab ", cursor: 7, mode: ptViNavigation, clip: ptClipboardData{text: "ab ", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "dd_last_line", text: "one\ntwo", cursor: 7, keys: []string{"<escape>", "d", "d"},
			want: viOutcome{text: "one", cursor: 2, mode: ptViNavigation, clip: ptClipboardData{text: "two", typ: ptSelectLines}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "read_only_x", text: "abc", cursor: 0, keys: []string{"x"},
			want: viOutcome{text: "abc", cursor: 0, mode: ptViInsert, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "navigation_end_fix", text: "abc", cursor: 3, keys: []string{"<escape>", "A", "<escape>"},
			want: viOutcome{text: "abc", cursor: 2, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{}}},
		{name: "insert_backspace_multiple", text: "ab\ncd", cursor: 0, keys: []string{"<escape>", "g", "g", "l", "<c-v>", "j", "A", "<backspace>", "<escape>"},
			want: viOutcome{text: "a\nc", cursor: 0, mode: ptViNavigation, clip: ptClipboardData{text: "", typ: ptSelectCharacters}, regs: map[string]ptClipboardData{}, multi: []int{1, 3}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := viRun(tc.text, tc.cursor, tc.keys, tc.name == "read_only_x")
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("keys %q\n got %#v\nwant %#v", tc.keys, got, tc.want)
			}
		})
	}
}

func TestVimModeLabelLabelTracksInputMode(t *testing.T) {
	caseHome(t)
	c := &CLI{ctx: context.Background()}
	if got := c.vimModeLabel(); got != "" {
		t.Fatalf("vim off %q", got)
	}
	c.vimMode = true
	if got := c.vimModeLabel(); got != "" {
		t.Fatalf("no app %q", got)
	}
	app := newPTApplication(nil, io.Discard)
	c.app, c.tui.app = app, app
	for _, tc := range []struct {
		mode ptInputMode
		want string
	}{{ptViInsert, "INSERT"}, {ptViNavigation, "NORMAL"}, {ptViReplace, "REPLACE"}} {
		app.viState.inputMode = tc.mode
		if got := c.vimModeLabel(); got != tc.want {
			t.Fatalf("mode %s label %q want %q", tc.mode, got, tc.want)
		}
	}
}

// Styles of the input cells of a row, the prompt left out
func viRowStyles(content ptInputContent, row int) []string {
	var out []string
	for _, cell := range content.lines[row] {
		out = append(out, cell.style)
	}
	return out
}

func TestPTViSelectionAndMultipleCursorsRenderLikeTheProcessors(t *testing.T) {
	app, _ := viRun("ab\n\ncd", 0, []string{"<escape>", "g", "g", "V", "j"}, false)
	content := app.inputContent()
	for row, want := range map[int]int{0: 3, 1: 1} {
		selected := 0
		for _, style := range viRowStyles(content, row) {
			if strings.Contains(style, "class:selected") {
				selected++
			}
		}
		if selected != want {
			t.Fatalf("row %d selected cells %d want %d: %v", row, selected, want, viRowStyles(content, row))
		}
	}
	if styles := viRowStyles(content, 2); strings.Contains(strings.Join(styles, " "), "class:selected") {
		t.Fatalf("row 2 outside the selection: %v", styles)
	}
	app, _ = viRun("ab\ncd", 0, []string{"<escape>", "g", "g", "<c-v>", "j", "I"}, false)
	content = app.inputContent()
	for _, row := range []int{0, 1} {
		if styles := viRowStyles(content, row); !strings.Contains(styles[0], "class:multiple-cursors") {
			t.Fatalf("row %d first cell %q", row, styles[0])
		}
	}
}
