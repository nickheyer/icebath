package app

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Feeds named keys to an idle application as if typed, plain characters typed as themselves
func pressKeysOn(app *ptApplication, keys ...string) {
	for _, key := range keys {
		press := ptKeyPress{Key: ptParseKey(key), Data: key}
		if ptNamedKeys[press.Key] || len([]rune(key)) > 1 {
			press.Data = ""
		}
		app.feedKeys(press)
	}
}

// Types each character of the text
func typeOn(app *ptApplication, text string) {
	for _, r := range text {
		app.feedKeys(ptKeyPress{Key: string(r), Data: string(r)})
	}
}

func TestEmacsKeysEditTheBufferLikeReadline(t *testing.T) {
	app := newPTApplication(nil, io.Discard)
	buf := app.buffer
	typeOn(app, "hello brave world")
	pressKeysOn(app, "c-a")
	if buf.cursor != 0 {
		t.Fatalf("Ctrl A left the cursor at %d", buf.cursor)
	}
	pressKeysOn(app, "escape", "f")
	if buf.cursor != 5 {
		t.Fatalf("Alt F moved to %d", buf.cursor)
	}
	pressKeysOn(app, "c-k")
	if buf.text() != "hello" || app.clipboard.text() != " brave world" {
		t.Fatalf("kill line left %q and killed %q", buf.text(), app.clipboard.text())
	}
	pressKeysOn(app, "c-y")
	if buf.text() != "hello brave world" {
		t.Fatalf("yank gave %q", buf.text())
	}
	pressKeysOn(app, "c-w")
	if buf.text() != "hello brave " {
		t.Fatalf("word rubout gave %q", buf.text())
	}
	pressKeysOn(app, "c-_")
	if buf.text() != "hello brave world" {
		t.Fatalf("undo gave %q", buf.text())
	}
	pressKeysOn(app, "c-e", "backspace", "left", "delete")
	if buf.text() != "hello brave wor" {
		t.Fatalf("backspace and delete gave %q", buf.text())
	}
	pressKeysOn(app, "c-u")
	if buf.text() != "" {
		t.Fatalf("line discard left %q", buf.text())
	}
	typeOn(app, "ab")
	pressKeysOn(app, "c-t")
	if buf.text() != "ba" {
		t.Fatalf("transpose gave %q", buf.text())
	}
	pressKeysOn(app, "escape", "3")
	typeOn(app, "x")
	if buf.text() != "baxxx" {
		t.Fatalf("repeat count gave %q", buf.text())
	}
}

func TestKeyProcessorPrefersEagerLaterAndFilteredBindings(t *testing.T) {
	app := newPTApplication(nil, io.Discard)
	var fired []string
	record := func(name string) ptHandler { return func(*ptEvent) { fired = append(fired, name) } }
	modal := false
	app.keyBindings.add(record("double escape"), ptBind{filter: func() bool { return !modal }}, "escape", "escape")
	app.keyBindings.add(record("modal escape"), ptBind{filter: func() bool { return modal }, eager: true}, "escape")
	app.keyBindings.add(record("first x"), ptBind{}, "x")
	app.keyBindings.add(record("second x"), ptBind{}, "x")
	pressKeysOn(app, "x")
	pressKeysOn(app, "escape")
	if len(app.processor.keyBuffer) != 1 {
		t.Fatalf("a lone escape was not held for the longer binding, fired %v", fired)
	}
	pressKeysOn(app, "escape")
	modal = true
	pressKeysOn(app, "escape")
	if want := []string{"second x", "double escape", "modal escape"}; !reflect.DeepEqual(fired, want) {
		t.Fatalf("fired %v, want %v", fired, want)
	}
	modal = false
	pressKeysOn(app, "escape")
	app.processor.flushKeys()
	typeOn(app, "q")
	if app.buffer.text() != "q" || len(app.processor.keyBuffer) != 0 {
		t.Fatalf("after the sequence timeout the buffer is %q with %d keys pending", app.buffer.text(), len(app.processor.keyBuffer))
	}
}

func TestFileHistoryKeepsThePromptToolkitFormatAndBrowsesNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	seed := "\n# 2026-01-01 10:00:00.000000\n+first entry\n\n# 2026-01-01 10:01:00.000000\n+second\n+with two lines\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	history := newPTFileHistory(path)
	if got := history.strings(); !reflect.DeepEqual(got, []string{"first entry", "second\nwith two lines"}) {
		t.Fatalf("loaded %q", got)
	}
	app := newPTApplication(nil, io.Discard)
	app.setBuffer(newPTBuffer(history))
	buf := app.buffer
	typeOn(app, "draft")
	pressKeysOn(app, "up")
	if buf.text() != "second\nwith two lines" {
		t.Fatalf("first Up recalled %q", buf.text())
	}
	pressKeysOn(app, "up")
	if buf.cursor != len("second") {
		t.Fatalf("Up inside a recalled entry moved the cursor to %d", buf.cursor)
	}
	pressKeysOn(app, "up")
	if buf.text() != "first entry" {
		t.Fatalf("Up on the first line recalled %q", buf.text())
	}
	pressKeysOn(app, "down", "down", "down")
	if buf.text() != "draft" {
		t.Fatalf("Down returned to %q", buf.text())
	}
	_ = buf.setText("third entry")
	buf.reset(true)
	buf.reset(true)
	data, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(data), "\n+third entry\n") || strings.Count(string(data), "+third entry") != 1 || strings.Count(string(data), "\n# ") != 3 {
		t.Fatalf("history file %q", string(data))
	}
	if got := newPTFileHistory(path).strings(); len(got) != 3 || got[2] != "third entry" {
		t.Fatalf("reloaded %q", got)
	}
}

func TestCompletionMenuOpensWhileTypingAndTabAppliesTheSelection(t *testing.T) {
	app := newPTApplication(nil, io.Discard)
	buf := app.buffer
	buf.completeWhileTyping = true
	buf.completer = func(before string) []ptCompletion {
		var out []ptCompletion
		for _, name := range []string{"/help", "/history", "/model"} {
			if strings.HasPrefix(name, before) && name != before {
				out = append(out, ptCompletion{Text: name, Start: -len([]rune(before)), Meta: "command"})
			}
		}
		return out
	}
	typeOn(app, "/h")
	if buf.completeState == nil || len(buf.completeState.completions) != 2 || buf.completeState.index != nil {
		t.Fatalf("menu state %+v", buf.completeState)
	}
	pressKeysOn(app, "down")
	if buf.text() != "/help" || buf.completeState == nil {
		t.Fatalf("Down selected %q", buf.text())
	}
	pressKeysOn(app, "down")
	if buf.text() != "/history" {
		t.Fatalf("second Down selected %q", buf.text())
	}
	pressKeysOn(app, "down")
	if buf.text() != "/h" || buf.completeState == nil {
		t.Fatalf("wrapping past the last entry gave %q", buf.text())
	}
	app.layout = []*ptWindow{{name: "input", kind: ptWindowBuffer}, {name: "menu", kind: ptWindowMenu, maxHeight: 12}}
	app.inputHeight = oneRow
	lines, _, _ := app.renderChrome(40, 20)
	if len(lines) != 3 || !strings.Contains(lines[1], "/help") || !strings.Contains(lines[1], "command") || !strings.Contains(lines[2], "/history") {
		t.Fatalf("menu rows %q", lines)
	}
	c := &CLI{}
	c.tuiHandleTab(&ptEvent{app: app, keySequence: []ptKeyPress{{Key: "c-i"}}})
	if buf.text() != "/help" || buf.cursor != 5 {
		t.Fatalf("Tab applied %q", buf.text())
	}
}

func TestInputWindowWrapsMasksAndPlacesTheCursor(t *testing.T) {
	app := newPTApplication(nil, io.Discard)
	app.prompt = func() []styledFragment { return []styledFragment{{"class:prompt", "> "}} }
	app.layout = []*ptWindow{{name: "input", kind: ptWindowBuffer}}
	app.inputHeight = func() int {
		return estimateTUIInputHeight(app.buffer.document().lines(), "> ", 10, 8)
	}
	hint := "type here"
	app.emptyHint = func() string { return hint }
	lines, cx, cy := app.renderChrome(10, 20)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "> type he") || cx != 2 || cy != 0 {
		t.Fatalf("empty input rows %q cursor %d,%d", lines, cx, cy)
	}
	typeOn(app, "abcdefghijkl")
	lines, cx, cy = app.renderChrome(10, 20)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "> abcdefgh") || !strings.HasPrefix(lines[1], "ijkl") || cx != 4 || cy != 1 {
		t.Fatalf("wrapped rows %q cursor %d,%d", lines, cx, cy)
	}
	app.password = func() bool { return true }
	lines, _, _ = app.renderChrome(10, 20)
	if !strings.HasPrefix(lines[0], "> ********") || strings.Contains(strings.Join(lines, ""), "abc") {
		t.Fatalf("masked rows %q", lines)
	}
}

func TestStyleRulesResolveClassesInOrderOverTheDefaults(t *testing.T) {
	d := &styleDict{}
	d.set("completion-menu", "bg:#1a1a2e #FFF8DC")
	d.set("completion-menu.completion.current", "bg:#333355 #FFD700")
	d.set("hint", "#888 italic")
	user, err := ptStyleFromDict(d)
	if err != nil {
		t.Fatal(err)
	}
	merged := newPTMergedStyle(user)
	current := merged.attrsFor("class:completion-menu class:completion-menu.completion.current")
	if *current.color != "FFD700" || *current.bgcolor != "333355" || current.reverse == nil || !*current.reverse {
		t.Fatalf("current completion attrs %+v", current)
	}
	hint := merged.attrsFor("class:hint")
	if *hint.color != "888888" || hint.italic == nil || !*hint.italic {
		t.Fatalf("hint attrs %+v", hint)
	}
	if merged.hasStyle("") || !merged.hasStyle("class:hint") {
		t.Fatal("style presence misreported")
	}
	if _, err := ptStyleFromDict(&styleDict{keys: []string{"bad"}, values: map[string]string{"bad": "#12"}}); err == nil {
		t.Fatal("a malformed color was accepted")
	}
}

func TestDivideHeightsGrowsToPreferredThenLeavesFlexibleRows(t *testing.T) {
	sizes, ok := ptDivideHeights([]ptDim{{0, ptUnbounded, 6}, {1, 1, 1}, {2, 2, 2}}, 20, false)
	if !ok || !reflect.DeepEqual(sizes, []int{17, 1, 2}) {
		t.Fatalf("sizes %v ok %v", sizes, ok)
	}
	sizes, ok = ptDivideHeights([]ptDim{{0, ptUnbounded, 6}, {1, 1, 1}, {2, 2, 2}}, 20, true)
	if !ok || !reflect.DeepEqual(sizes, []int{6, 1, 2}) {
		t.Fatalf("sizes of a finished application %v ok %v", sizes, ok)
	}
	if _, ok := ptDivideHeights([]ptDim{{3, 3, 3}, {2, 2, 2}}, 4, false); ok {
		t.Fatal("overflowing minimums were divided")
	}
}
