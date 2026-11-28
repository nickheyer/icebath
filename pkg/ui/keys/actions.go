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
		overlay(act("palette", Global, "commands", "Search and run any command", "ctrl+x")),
		overlay(act("quit", Global, "quit", "Quit icebath", "ctrl+q")),
		overlay(act("interrupt", Global, "interrupt", "Copy the selection, or stop the reply. Press twice to quit", "ctrl+c")),
		act("tabs.join", Global, "merge into tab", "Merge the focused window into the next window as a tab"),
		act("tabs.split", Global, "split tab out", "Move the current tab into its own window"),
		act("tabs.cycle", Global, "next tab", "Show the next tab of the focused window"),
		act("tabs.cycle.back", Global, "previous tab", "Show the previous tab of the focused window"),
		act("workspace.new", Global, "new space", "Open a new space"),
		act("workspace.close", Global, "close space", "Close this space and keep its sessions running"),
		act("workspace.next", Global, "next space", "Show the next space", "alt+]"),
		act("workspace.prev", Global, "previous space", "Show the previous space", "alt+["),
	}
	for i := 1; i <= 9; i++ {
		n := strconv.Itoa(i)
		out = append(out, act("workspace."+n, Global, "space "+n, "Show space "+n, "alt+"+n))
	}
	out = append(out,
		act("window.focus.left", Global, "focus left", "Focus the window to the left", "alt+h", "alt+left"),
		act("window.focus.right", Global, "focus right", "Focus the window to the right", "alt+l", "alt+right"),
		act("window.focus.up", Global, "focus up", "Focus the window above", "alt+k", "alt+up"),
		act("window.focus.down", Global, "focus down", "Focus the window below", "alt+j", "alt+down"),
		act("window.cycle", Global, "focus next", "Focus the next window", "alt+o"),
		act("window.cycle.back", Global, "focus previous", "Focus the previous window", "alt+shift+o"),
		act("window.attention", Global, "go to waiting window", "Focus the window that's waiting for you", "alt+g"),
		act("window.move.left", Global, "move left", "Swap with the window to the left", "alt+shift+h", "alt+shift+left"),
		act("window.move.right", Global, "move right", "Swap with the window to the right", "alt+shift+l", "alt+shift+right"),
		act("window.move.up", Global, "move up", "Swap with the window above", "alt+shift+k", "alt+shift+up"),
		act("window.move.down", Global, "move down", "Swap with the window below", "alt+shift+j", "alt+shift+down"),
		act("window.master", Global, "make main", "Swap the focused window with the main window"),
		act("window.zoom", Global, "fullscreen", "Toggle fullscreen for the focused window", "alt+z"),
		act("window.float", Global, "float", "Toggle floating for the focused window"),
		act("window.close", Global, "close", "Close the focused window", "alt+w"),
		act("window.grow", Global, "grow", "Widen the main column", "alt+="),
		act("window.shrink", Global, "shrink", "Narrow the main column", "alt+-"),
		act("layout.reset", Global, "reset layout", "Reset window sizes"),
		act("layout.toggle", Global, "toggle layout", "Switch between spiral and main column layouts"),
		act("layout.dwindle", Global, "spiral layout", "Arrange windows in a spiral of splits"),
		act("layout.master", Global, "main column layout", "Main windows on one side, the rest stacked beside them"),
		act("layout.main.more", Global, "more main windows", "Add a window to the main column"),
		act("layout.main.fewer", Global, "fewer main windows", "Remove a window from the main column"),
		act("layout.split", Global, "flip split", "Flip the split direction of the focused window"),

		act("panel.close", Panel, "close", "Close the panel", "q"),

		quiet(act("composer.send", Session, "send", "Send the message", "enter")),
		quiet(act("session.stop", Session, "escape", "Clear the selection, leave history, or stop the reply", "esc")),
		quiet(act("composer.up", Session, "up", "Move up a line, or from the first line show the message before", "up")),
		quiet(act("composer.down", Session, "down", "Move down a line, or from the last line show the message after", "down")),
		quiet(act("composer.history.prev", Session, "previous message", "Show the previous sent message", "ctrl+p")),
		quiet(act("composer.history.next", Session, "next message", "Show the next sent message", "ctrl+n")),
		act("composer.history.search", Session, "search history", "Search sent messages", "ctrl+r"),
		quiet(act("complete.start", Session, "complete", "Complete the path at the cursor", "tab")),
		quiet(act("session.scroll.up", Session, "page up", "Scroll up a page", "pgup")),
		quiet(act("session.scroll.down", Session, "page down", "Scroll down a page", "pgdown")),
		act("session.scroll.top", Session, "scroll to top", "Scroll to the start of the chat", "ctrl+home"),
		act("session.scroll.end", Session, "scroll to bottom", "Scroll to the end and follow new output", "ctrl+end"),
		act("session.expand", Session, "expand tool output", "Expand every tool call and result", "alt+e"),
		act("session.collapse", Session, "collapse tool output", "Collapse every tool call and result", "alt+shift+e"),
		act("session.thinking", Session, "toggle reasoning", "Expand or collapse model reasoning", "alt+t"),

		quiet(act("text.left", Text, "left", "Move the cursor left", "left", "ctrl+b")),
		quiet(act("text.right", Text, "right", "Move the cursor right", "right", "ctrl+f")),
		quiet(act("text.word.left", Text, "word left", "Move the cursor one word to the left", "ctrl+left", "alt+b", "super+left")),
		quiet(act("text.word.right", Text, "word right", "Move the cursor one word to the right", "ctrl+right", "alt+f", "super+right")),
		quiet(act("text.line.start", Text, "line start", "Move the cursor to the start of the line", "home", "ctrl+a")),
		quiet(act("text.line.end", Text, "line end", "Move the cursor to the end of the line", "end", "ctrl+e", "super+e")),
		quiet(act("text.delete.back", Text, "delete back", "Delete the character before the cursor", "backspace", "ctrl+h")),
		quiet(act("text.delete.forward", Text, "delete forward", "Delete the character under the cursor", "delete", "ctrl+d")),
		quiet(act("text.delete.word.back", Text, "delete word back", "Delete the word before the cursor", "ctrl+w", "ctrl+backspace", "alt+backspace", "super+w")),
		quiet(act("text.delete.word.forward", Text, "delete word forward", "Delete the word after the cursor", "ctrl+delete", "alt+d")),
		quiet(act("text.delete.line.before", Text, "delete to start", "Delete from the cursor to the start of the line", "ctrl+u", "super+u", "super+backspace")),
		quiet(act("text.delete.line.after", Text, "delete to end", "Delete from the cursor to the end of the line", "ctrl+k", "super+k", "super+delete")),
		quiet(act("text.newline", Text, "newline", "Insert a line break", "shift+enter", "ctrl+j", "alt+enter", "ctrl+enter", "super+enter")),
		quiet(act("text.undo", Text, "undo", "Undo the last edit", "ctrl+z", "super+z", "ctrl+_")),
		quiet(act("text.redo", Text, "redo", "Redo the last undone edit", "ctrl+y", "ctrl+shift+z", "shift+super+z", "super+y")),
		quiet(act("text.select.all", Text, "select all", "Select all the text", "ctrl+shift+a", "super+a")),
		quiet(act("text.select.left", Text, "select left", "Extend the selection one character to the left", "shift+left")),
		quiet(act("text.select.right", Text, "select right", "Extend the selection one character to the right", "shift+right")),
		quiet(act("text.select.word.left", Text, "select word left", "Extend the selection one word to the left", "ctrl+shift+left")),
		quiet(act("text.select.word.right", Text, "select word right", "Extend the selection one word to the right", "ctrl+shift+right")),
		quiet(act("text.select.up", Text, "select up", "Extend the selection one line up")),
		quiet(act("text.select.down", Text, "select down", "Extend the selection one line down")),
		quiet(act("text.copy", Text, "copy", "Copy the selection to the clipboard", "ctrl+shift+c", "ctrl+super+c", "super+c")),
		quiet(act("text.paste", Text, "paste", "Paste the clipboard at the cursor", "ctrl+v", "super+v")),

		quiet(act("complete.next", Complete, "next", "Select the next suggestion", "down", "ctrl+n")),
		quiet(act("complete.prev", Complete, "previous", "Select the previous suggestion", "up", "ctrl+p")),
		quiet(act("complete.accept", Complete, "accept", "Insert the selected suggestion", "tab")),
		quiet(act("complete.close", Complete, "close", "Close the suggestions", "esc")),

		act("search.accept", Search, "done", "Close the search bar and keep the filter", "enter"),
		act("search.close", Search, "clear", "Clear the search and close the search bar", "esc"),
		quiet(act("search.next", Search, "next", "Move to the next match or row", "down", "ctrl+n")),
		quiet(act("search.prev", Search, "previous", "Move to the previous match or row", "up", "ctrl+p")),

		act("confirm.yes", Confirm, "yes", "Confirm", "y"),
		act("confirm.no", Confirm, "cancel", "Cancel", "esc", "n"),

		act("edit.apply", Edit, "apply", "Save the cell", "enter"),
		act("edit.cancel", Edit, "cancel", "Discard changes to the cell", "esc"),

		act("capture.cancel", Capture, "cancel", "Stop recording a key", "esc"),

		quiet(act("palette.up", Palette, "up", "Move to the row above", "up", "shift+tab", "ctrl+p")),
		quiet(act("palette.down", Palette, "down", "Move to the row below", "down", "tab", "ctrl+n")),
		quiet(act("palette.page.up", Palette, "page up", "Move eight rows up", "pgup")),
		quiet(act("palette.page.down", Palette, "page down", "Move eight rows down", "pgdown")),
		act("palette.pick", Palette, "run", "Run the selected command", "enter"),
		act("palette.close", Palette, "close", "Close the palette", "esc"),

		act("tabs.next", Tabs, "next tab", "Switch to the next tab", "tab"),
		act("tabs.prev", Tabs, "previous tab", "Switch to the previous tab", "shift+tab"),

		quiet(act("list.up", List, "up", "Move to the row above", "up", "k")),
		quiet(act("list.down", List, "down", "Move to the row below", "down", "j")),
		quiet(act("list.top", List, "first", "Move to the first row", "home", "g")),
		quiet(act("list.bottom", List, "last", "Move to the last row", "end", "G")),
		quiet(act("list.page.up", List, "page up", "Move up one page", "pgup")),
		quiet(act("list.page.down", List, "page down", "Move down one page", "pgdown")),

		act("filter.toggle", Filter, "filter", "Open the filter bar, or clear the filter", "/"),

		act("detail.scroll.down", Detail, "detail down", "Scroll the detail pane down a page", "pgdown"),
		act("detail.scroll.up", Detail, "detail up", "Scroll the detail pane up a page", "pgup"),

		quiet(act("view.down", View, "down", "Move down a line", "j", "down")),
		quiet(act("view.up", View, "up", "Move up a line", "k", "up")),
		act("view.page.down", View, "page down", "Move down a page", "pgdown", "space", "ctrl+f"),
		act("view.page.up", View, "page up", "Move up a page", "pgup", "b", "ctrl+b"),
		quiet(act("view.half.down", View, "half page down", "Move down half a page", "ctrl+d", "d")),
		quiet(act("view.half.up", View, "half page up", "Move up half a page", "ctrl+u", "u")),
		quiet(act("view.top", View, "top", "Go to the start", "g", "home")),
		quiet(act("view.bottom", View, "end", "Go to the end", "G", "end")),
		act("view.prompt.prev", View, "previous prompt", "Go to the prompt before", "["),
		act("view.prompt.next", View, "next prompt", "Go to the prompt after", "]"),
		act("view.search", View, "search", "Search the text", "/"),
		quiet(act("view.search.back", View, "search back", "Search the text upward", "?")),
		quiet(act("view.match.next", View, "next match", "Go to the next match", "n")),
		quiet(act("view.match.prev", View, "previous match", "Go to the match before", "N")),
		act("view.select", View, "select", "Start or end a selection at the cursor", "v"),
		act("view.copy", View, "copy", "Copy the selection, or the message at the cursor", "y"),
		quiet(act("view.open", View, "open", "Expand or collapse the item at the cursor", "enter")),
		act("view.leave", View, "exit read mode", "Go back to typing", "esc", "q", "i"),

		act("terminal.copy", Terminal, "copy", "Copy the selection, or the whole screen if nothing is selected", "ctrl+shift+c"),
		act("terminal.paste", Terminal, "paste", "Paste into the shell", "ctrl+shift+v"),
		act("terminal.search", Terminal, "search", "Search the screen and scrollback", "ctrl+shift+f"),
		act("view.start", Terminal, "read mode", "Move through the screen and scrollback to select and copy", "ctrl+shift+s"),
		act("terminal.scroll.up", Terminal, "page up", "Scroll up a page", "shift+pgup"),
		act("terminal.scroll.down", Terminal, "page down", "Scroll down a page", "shift+pgdown"),
		quiet(act("terminal.scroll.top", Terminal, "top", "Scroll to the oldest line", "shift+home")),
		quiet(act("terminal.scroll.bottom", Terminal, "bottom", "Scroll back to the live screen", "shift+end")),
		act("terminal.prompt.prev", Terminal, "previous prompt", "Scroll to the previous shell prompt (needs shell integration)", "ctrl+shift+z"),
		act("terminal.prompt.next", Terminal, "next prompt", "Scroll to the next shell prompt (needs shell integration)", "ctrl+shift+x"),
		quiet(act("terminal.select.all", Terminal, "select all", "Select the screen and scrollback", "ctrl+shift+a")),
		act("terminal.lock", Terminal, "lock keys", "Send every key to the shell, including window keys, until pressed again", "ctrl+shift+l"),
		act("terminal.clear", Terminal, "clear scrollback", "Clear the scrollback", "ctrl+shift+k"),
		act("terminal.restart", Terminal, "restart", "Restart the shell in the same folder", "ctrl+shift+r"),
	)
	return out
}()
