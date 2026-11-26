package keys

import "strconv"

// Keybinding contexts. The focused window determines which contexts are active.
// Global bindings are checked last.
const (
	Global   = "global"
	Panel    = "panel"
	Session  = "session"
	Complete = "complete"
	Search   = "search"
	Confirm  = "confirm"
	Edit     = "edit"
	Capture  = "capture"
	Palette  = "palette"
	Tabs     = "tabs"
	List     = "list"
	Filter   = "filter"
	Detail   = "detail"
	View     = "view"
	Terminal = "terminal"
	Text     = "text"
)

// ContextDocs describes each context in the keybind editor.
var ContextDocs = map[string]string{
	Global:   "everywhere",
	Panel:    "panels",
	Session:  "chats",
	Complete: "suggestions",
	Search:   "search fields",
	Confirm:  "confirmations",
	Edit:     "cell editing",
	Capture:  "key recording",
	Palette:  "command palette",
	Tabs:     "tabs",
	List:     "lists",
	Filter:   "filterable lists",
	Detail:   "detail panes",
	View:     "read mode",
	Terminal: "terminals",
	Text:     "text fields",
}

func act(id, ctx, label, doc string, keys ...string) Action {
	return Action{ID: id, Context: ctx, Label: label, Doc: doc, Default: keys}
}

func quiet(a Action) Action { a.Quiet = true; return a }

func overlay(a Action) Action { a.Overlay = true; return a }

// registry lists the available actions in display order.
var registry = func() []Action {
	out := []Action{
		overlay(act("palette", Global, "commands", "search and run any command", "ctrl+x")),
		overlay(act("quit", Global, "quit", "quit icebath", "ctrl+q")),
		overlay(act("interrupt", Global, "interrupt", "copy the selection or stop the reply; press twice to quit", "ctrl+c")),
		act("tabs.join", Global, "merge into tab", "merge the focused window into the next window as a tab"),
		act("tabs.split", Global, "split tab out", "move the current tab into its own window"),
		act("tabs.cycle", Global, "next tab", "show the next tab of the focused window"),
		act("tabs.cycle.back", Global, "previous tab", "show the previous tab of the focused window"),
		act("workspace.new", Global, "new space", "open a new space"),
		act("workspace.close", Global, "close space", "close this space; its sessions keep running"),
		act("workspace.next", Global, "next space", "show the next space", "alt+]"),
		act("workspace.prev", Global, "previous space", "show the previous space", "alt+["),
	}
	for i := 1; i <= 9; i++ {
		n := strconv.Itoa(i)
		out = append(out, act("workspace."+n, Global, "space "+n, "show space "+n, "alt+"+n))
	}
	out = append(out,
		act("window.focus.left", Global, "focus left", "focus the window to the left", "alt+h", "alt+left"),
		act("window.focus.right", Global, "focus right", "focus the window to the right", "alt+l", "alt+right"),
		act("window.focus.up", Global, "focus up", "focus the window above", "alt+k", "alt+up"),
		act("window.focus.down", Global, "focus down", "focus the window below", "alt+j", "alt+down"),
		act("window.cycle", Global, "focus next", "focus the next window", "alt+o"),
		act("window.cycle.back", Global, "focus previous", "focus the previous window", "alt+shift+o"),
		act("window.attention", Global, "go to waiting window", "focus the window that's waiting for you", "alt+g"),
		act("window.move.left", Global, "move left", "swap with the window to the left", "alt+shift+h", "alt+shift+left"),
		act("window.move.right", Global, "move right", "swap with the window to the right", "alt+shift+l", "alt+shift+right"),
		act("window.move.up", Global, "move up", "swap with the window above", "alt+shift+k", "alt+shift+up"),
		act("window.move.down", Global, "move down", "swap with the window below", "alt+shift+j", "alt+shift+down"),
		act("window.master", Global, "make main", "swap the focused window with the main window"),
		act("window.zoom", Global, "fullscreen", "toggle fullscreen for the focused window", "alt+z"),
		act("window.float", Global, "float", "toggle floating for the focused window"),
		act("window.close", Global, "close", "close the focused window", "alt+w"),
		act("window.grow", Global, "grow", "widen the main column", "alt+="),
		act("window.shrink", Global, "shrink", "narrow the main column", "alt+-"),
		act("layout.reset", Global, "reset layout", "reset window sizes"),
		act("layout.toggle", Global, "toggle layout", "switch between spiral and main column layouts"),
		act("layout.dwindle", Global, "spiral layout", "arrange windows in a spiral of splits"),
		act("layout.master", Global, "main column layout", "main windows on one side, the rest stacked beside them"),
		act("layout.main.more", Global, "more main windows", "add a window to the main column"),
		act("layout.main.fewer", Global, "fewer main windows", "remove a window from the main column"),
		act("layout.split", Global, "flip split", "flip the split direction of the focused window"),

		act("panel.close", Panel, "close", "close the panel", "q"),

		quiet(act("composer.send", Session, "send", "send the message", "enter")),
		quiet(act("session.stop", Session, "escape", "clear the selection, leave history, or stop the reply", "esc")),
		quiet(act("composer.up", Session, "up", "move up a line; on the first line, show the previous message", "up")),
		quiet(act("composer.down", Session, "down", "move down a line; on the last line, show the next message", "down")),
		quiet(act("composer.history.prev", Session, "previous message", "show the previous sent message", "ctrl+p")),
		quiet(act("composer.history.next", Session, "next message", "show the next sent message", "ctrl+n")),
		act("composer.history.search", Session, "search history", "search sent messages", "ctrl+r"),
		quiet(act("complete.start", Session, "complete", "complete the path at the cursor", "tab")),
		quiet(act("session.scroll.up", Session, "page up", "scroll up a page", "pgup")),
		quiet(act("session.scroll.down", Session, "page down", "scroll down a page", "pgdown")),
		act("session.scroll.top", Session, "scroll to top", "scroll to the start of the chat", "ctrl+home"),
		act("session.scroll.end", Session, "scroll to bottom", "scroll to the end and follow new output", "ctrl+end"),
		act("session.expand", Session, "expand tool output", "expand every tool call and result", "alt+e"),
		act("session.collapse", Session, "collapse tool output", "collapse every tool call and result", "alt+shift+e"),
		act("session.thinking", Session, "toggle reasoning", "expand or collapse model reasoning", "alt+t"),

		quiet(act("text.left", Text, "left", "move the cursor left", "left", "ctrl+b")),
		quiet(act("text.right", Text, "right", "move the cursor right", "right", "ctrl+f")),
		quiet(act("text.word.left", Text, "word left", "move the cursor one word to the left", "ctrl+left", "alt+b", "super+left")),
		quiet(act("text.word.right", Text, "word right", "move the cursor one word to the right", "ctrl+right", "alt+f", "super+right")),
		quiet(act("text.line.start", Text, "line start", "move the cursor to the start of the line", "home", "ctrl+a")),
		quiet(act("text.line.end", Text, "line end", "move the cursor to the end of the line", "end", "ctrl+e", "super+e")),
		quiet(act("text.delete.back", Text, "delete back", "delete the character before the cursor", "backspace", "ctrl+h")),
		quiet(act("text.delete.forward", Text, "delete forward", "delete the character under the cursor", "delete", "ctrl+d")),
		quiet(act("text.delete.word.back", Text, "delete word back", "delete the word before the cursor", "ctrl+w", "ctrl+backspace", "alt+backspace", "super+w")),
		quiet(act("text.delete.word.forward", Text, "delete word forward", "delete the word after the cursor", "ctrl+delete", "alt+d")),
		quiet(act("text.delete.line.before", Text, "delete to start", "delete from the cursor to the start of the line", "ctrl+u", "super+u", "super+backspace")),
		quiet(act("text.delete.line.after", Text, "delete to end", "delete from the cursor to the end of the line", "ctrl+k", "super+k", "super+delete")),
		quiet(act("text.newline", Text, "newline", "insert a line break", "shift+enter", "ctrl+j", "alt+enter", "ctrl+enter", "super+enter")),
		quiet(act("text.undo", Text, "undo", "undo the last edit", "ctrl+z", "super+z", "ctrl+_")),
		quiet(act("text.redo", Text, "redo", "redo the last undone edit", "ctrl+y", "ctrl+shift+z", "shift+super+z", "super+y")),
		quiet(act("text.select.all", Text, "select all", "select all the text", "ctrl+shift+a", "super+a")),
		quiet(act("text.select.left", Text, "select left", "extend the selection one character to the left", "shift+left")),
		quiet(act("text.select.right", Text, "select right", "extend the selection one character to the right", "shift+right")),
		quiet(act("text.select.word.left", Text, "select word left", "extend the selection one word to the left", "ctrl+shift+left")),
		quiet(act("text.select.word.right", Text, "select word right", "extend the selection one word to the right", "ctrl+shift+right")),
		quiet(act("text.select.up", Text, "select up", "extend the selection one line up")),
		quiet(act("text.select.down", Text, "select down", "extend the selection one line down")),
		quiet(act("text.copy", Text, "copy", "copy the selection to the clipboard", "ctrl+shift+c", "ctrl+super+c", "super+c")),
		quiet(act("text.paste", Text, "paste", "paste the clipboard at the cursor", "ctrl+v", "super+v")),

		quiet(act("complete.next", Complete, "next", "select the next suggestion", "down", "ctrl+n")),
		quiet(act("complete.prev", Complete, "previous", "select the previous suggestion", "up", "ctrl+p")),
		quiet(act("complete.accept", Complete, "accept", "insert the selected suggestion", "tab")),
		quiet(act("complete.close", Complete, "close", "close the suggestions", "esc")),

		act("search.accept", Search, "done", "close the search bar and keep the filter", "enter"),
		act("search.close", Search, "clear", "clear the search and close the search bar", "esc"),
		quiet(act("search.next", Search, "next", "move to the next match or row", "down", "ctrl+n")),
		quiet(act("search.prev", Search, "previous", "move to the previous match or row", "up", "ctrl+p")),

		act("confirm.yes", Confirm, "yes", "confirm", "y"),
		act("confirm.no", Confirm, "no", "cancel", "n", "esc"),

		act("edit.apply", Edit, "apply", "save the cell", "enter"),
		act("edit.cancel", Edit, "cancel", "discard changes to the cell", "esc"),

		act("capture.cancel", Capture, "cancel", "stop recording a key", "esc"),

		quiet(act("palette.up", Palette, "up", "move to the row above", "up", "shift+tab", "ctrl+p")),
		quiet(act("palette.down", Palette, "down", "move to the row below", "down", "tab", "ctrl+n")),
		quiet(act("palette.page.up", Palette, "page up", "move eight rows up", "pgup")),
		quiet(act("palette.page.down", Palette, "page down", "move eight rows down", "pgdown")),
		act("palette.pick", Palette, "run", "run the selected command", "enter"),
		act("palette.close", Palette, "close", "close the palette", "esc"),

		act("tabs.next", Tabs, "next tab", "switch to the next tab", "tab"),
		act("tabs.prev", Tabs, "previous tab", "switch to the previous tab", "shift+tab"),

		quiet(act("list.up", List, "up", "move to the row above", "up", "k")),
		quiet(act("list.down", List, "down", "move to the row below", "down", "j")),
		quiet(act("list.top", List, "first", "move to the first row", "home", "g")),
		quiet(act("list.bottom", List, "last", "move to the last row", "end", "G")),
		quiet(act("list.page.up", List, "page up", "move up one page", "pgup")),
		quiet(act("list.page.down", List, "page down", "move down one page", "pgdown")),

		act("filter.toggle", Filter, "filter", "open the filter bar, or clear the filter", "/"),

		act("detail.scroll.down", Detail, "detail down", "scroll the detail pane down a page", "pgdown"),
		act("detail.scroll.up", Detail, "detail up", "scroll the detail pane up a page", "pgup"),

		quiet(act("view.down", View, "down", "move down a line", "j", "down")),
		quiet(act("view.up", View, "up", "move up a line", "k", "up")),
		act("view.page.down", View, "page down", "move down a page", "pgdown", "space", "ctrl+f"),
		act("view.page.up", View, "page up", "move up a page", "pgup", "b", "ctrl+b"),
		quiet(act("view.half.down", View, "half page down", "move down half a page", "ctrl+d", "d")),
		quiet(act("view.half.up", View, "half page up", "move up half a page", "ctrl+u", "u")),
		quiet(act("view.top", View, "top", "go to the start", "g", "home")),
		quiet(act("view.bottom", View, "end", "go to the end", "G", "end")),
		act("view.prompt.prev", View, "previous prompt", "go to the prompt before", "["),
		act("view.prompt.next", View, "next prompt", "go to the prompt after", "]"),
		act("view.search", View, "search", "search the text", "/"),
		quiet(act("view.search.back", View, "search back", "search the text upward", "?")),
		quiet(act("view.match.next", View, "next match", "go to the next match", "n")),
		quiet(act("view.match.prev", View, "previous match", "go to the match before", "N")),
		act("view.select", View, "select", "start or end a selection at the cursor", "v"),
		act("view.copy", View, "copy", "copy the selection, or the message at the cursor", "y"),
		quiet(act("view.open", View, "open", "expand or collapse the item at the cursor", "enter")),
		act("view.leave", View, "exit read mode", "go back to typing", "esc", "q", "i"),

		act("terminal.copy", Terminal, "copy", "copy the selection, or the whole screen if nothing is selected", "ctrl+shift+c"),
		act("terminal.paste", Terminal, "paste", "paste into the shell", "ctrl+shift+v"),
		act("terminal.search", Terminal, "search", "search the screen and scrollback", "ctrl+shift+f"),
		act("view.start", Terminal, "read mode", "move through the screen and scrollback to select and copy", "ctrl+shift+s"),
		act("terminal.scroll.up", Terminal, "page up", "scroll up a page", "shift+pgup"),
		act("terminal.scroll.down", Terminal, "page down", "scroll down a page", "shift+pgdown"),
		quiet(act("terminal.scroll.top", Terminal, "top", "scroll to the oldest line", "shift+home")),
		quiet(act("terminal.scroll.bottom", Terminal, "bottom", "scroll back to the live screen", "shift+end")),
		act("terminal.prompt.prev", Terminal, "previous prompt", "scroll to the previous shell prompt (needs shell integration)", "ctrl+shift+z"),
		act("terminal.prompt.next", Terminal, "next prompt", "scroll to the next shell prompt (needs shell integration)", "ctrl+shift+x"),
		quiet(act("terminal.select.all", Terminal, "select all", "select the screen and scrollback", "ctrl+shift+a")),
		act("terminal.lock", Terminal, "lock keys", "send every key to the shell, including window keys, until pressed again", "ctrl+shift+l"),
		act("terminal.clear", Terminal, "clear scrollback", "clear the scrollback", "ctrl+shift+k"),
		act("terminal.restart", Terminal, "restart", "restart the shell in the same directory", "ctrl+shift+r"),
	)
	return out
}()
