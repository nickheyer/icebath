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
	Approval = "approval"
	Terminal = "terminal"
	Leader   = "leader"
	Text     = "text"
)

// ContextDocs describes each context in the keybind editor.
var ContextDocs = map[string]string{
	Global:   "everywhere",
	Panel:    "all panels except the session",
	Session:  "session prompt and transcript",
	Complete: "completion popup",
	Search:   "search bars",
	Confirm:  "yes/no prompts",
	Edit:     "table cells while editing",
	Capture:  "keybind editor while recording a key",
	Palette:  "command palette and pickers",
	Tabs:     "tabbed panels, such as config",
	List:     "tables",
	Filter:   "tables with filters",
	Detail:   "open detail panes",
	View:     "reading a transcript, a terminal's scrollback or a long text",
	Approval: "approval dialog",
	Terminal: "terminal windows, every other key going to the shell",
	Leader:   "the key pressed after the leader key",
	Text:     "every text field and the prompt",
}

func act(id, ctx, label, doc string, keys ...string) Action {
	return Action{ID: id, Context: ctx, Label: label, Doc: doc, Default: keys}
}

func quiet(a Action) Action { a.Quiet = true; return a }

func overlay(a Action) Action { a.Overlay = true; return a }

// Key after the leader, firing the action or command of the target id, listed on the strip line of its group
func lead(group, target, label, doc string, keys ...string) Action {
	a := act("leader."+target, Leader, label, doc, keys...)
	a.Group = group
	return a
}

// registry lists the available actions in display order.
var registry = func() []Action {
	out := []Action{
		overlay(act("leader", Global, "more keys", "show the keys for windows, spaces and the prompt", "ctrl+x")),
		lead("session", "session.beside", "new session", "open a new session beside this one", "n"),
		lead("session", "open.sessions", "sessions", "switch the session shown here", "s", "b"),
		lead("session", "model.switch", "model", "choose the model of this session", "m"),
		lead("session", "tui.agents", "agents", "show the agents this session started", "a"),
		lead("session", "terminal.new", "terminal", "open a terminal in this space", "t"),
		lead("session", "terminal.tab", "terminal tab", "open a terminal as a tab of this window", "T"),
		lead("window", "window.attention", "needs you", "go to the window that waits on you", "g"),
		lead("window", "view.start", "read", "read, search and copy this window's text", "v"),
		lead("window", "view.find", "find", "search this window's text", "/"),
		lead("window", "window.cycle", "next window", "focus the next window", "o", "."),
		lead("window", "window.cycle.back", "previous window", "focus the previous window", ","),
		lead("window", "window.focus.left", "focus left", "focus the window to the left", "h", "left"),
		lead("window", "window.focus.down", "focus down", "focus the window below", "j", "down"),
		lead("window", "window.focus.up", "focus up", "focus the window above", "k", "up"),
		lead("window", "window.focus.right", "focus right", "focus the window to the right", "l", "right"),
		lead("window", "window.move.left", "move left", "swap this window with the one to its left", "H", "shift+left"),
		lead("window", "window.move.down", "move down", "swap this window with the one below", "J", "shift+down"),
		lead("window", "window.move.up", "move up", "swap this window with the one above", "K", "shift+up"),
		lead("window", "window.move.right", "move right", "swap this window with the one to its right", "L", "shift+right"),
		lead("window", "window.close", "close window", "close this window; its session keeps running", "q"),
		lead("layout", "window.zoom", "fullscreen", "fill the space with this window, or go back", "z"),
		lead("layout", "window.float", "float", "float this window, or tile it again", "f"),
		lead("layout", "window.master", "to main", "swap this window with the main one", "enter"),
		lead("layout", "window.grow", "grow", "give the main windows more room", "=", "+"),
		lead("layout", "window.shrink", "shrink", "give the main windows less room", "-"),
		lead("layout", "layout.toggle", "layout", "switch between the spiral and main column layouts", "w"),
		lead("layout", "layout.reset", "reset sizes", "put the window sizes back", "r"),
		lead("layout", "layout.main.more", "more main", "put one more window in the main column", ">"),
		lead("layout", "layout.main.fewer", "fewer main", "put one window less in the main column", "<"),
		lead("layout", "layout.split", "flip split", "turn the split of this window the other way", "S"),
		lead("layout", "tabs.cycle", "next tab", "show the next tab of this window", "tab"),
		lead("layout", "tabs.cycle.back", "previous tab", "show the previous tab of this window", "shift+tab"),
		lead("layout", "tabs.join", "tab in", "put this window into the one beside it as a tab", "I"),
		lead("layout", "tabs.split", "tab out", "take the shown tab out into a window of its own", "O"),
		lead("space", "workspace.new", "new space", "open a new space with a new session", "c"),
		lead("space", "workspace.next", "next space", "show the next space", "]"),
		lead("space", "workspace.prev", "previous space", "show the previous space", "["),
		lead("space", "workspace.close", "close space", "close this space; its sessions keep running", "Q"),
		lead("prompt", "tui.editor", "editor", "edit the prompt in $EDITOR", "e"),
		lead("prompt", "tui.paste.expand", "expand paste", "show or fold pasted text", "p"),
		lead("prompt", "tui.paste.image", "paste image", "attach the image on the clipboard", "i"),
		lead("prompt", "tui.queue.delete", "delete queued", "delete the queued prompt being edited", "d"),
		lead("prompt", "tui.voice", "record", "record your voice into the prompt", "R"),
		lead("", "palette", "palette", "open the command palette", "space", ":"),
		lead("", "tui.help", "help", "list every key and command", "?"),
		lead("", "literal", "pass on", "send the leader key itself to the window, for programs that use it"),
		quiet(lead("", "cancel", "cancel", "do nothing", "esc", "ctrl+g")),
		overlay(act("palette", Global, "palette", "open the command palette", "alt+space")),
		overlay(act("quit", Global, "quit", "quit icebath", "ctrl+q")),
		overlay(act("interrupt", Global, "cancel", "copy a selection, put a draft aside, stop the reply, or quit on a second press", "ctrl+c")),
		act("session.new", Global, "new session", "start a new session in this window"),
		act("terminal.new", Global, "new terminal", "open a terminal in this space"),
		act("terminal.tab", Global, "terminal tab", "open a terminal as a new tab of the focused window"),
		act("tabs.join", Global, "tab in", "put the focused window into the window beside it as a tab"),
		act("tabs.split", Global, "tab out", "take the shown tab out into a window of its own"),
		act("tabs.cycle", Global, "next tab", "show the next tab of the focused window"),
		act("tabs.cycle.back", Global, "previous tab", "show the previous tab of the focused window"),
		act("workspace.new", Global, "new space", "open a new space with a new session"),
		act("workspace.close", Global, "close space", "close this space; its sessions keep running"),
		act("workspace.next", Global, "next space", "show the next space", "alt+]"),
		act("workspace.prev", Global, "previous space", "show the previous space", "alt+["),
	}
	for i := 1; i <= 9; i++ {
		n := strconv.Itoa(i)
		out = append(out, act("workspace."+n, Global, "space "+n, "show space "+n, "alt+"+n))
		out = append(out, quiet(lead("space", "workspace."+n, "space "+n, "show space "+n, n)))
	}
	out = append(out,
		act("window.focus.left", Global, "focus left", "focus the window to the left", "alt+h", "alt+left"),
		act("window.focus.right", Global, "focus right", "focus the window to the right", "alt+l", "alt+right"),
		act("window.focus.up", Global, "focus up", "focus the window above", "alt+k", "alt+up"),
		act("window.focus.down", Global, "focus down", "focus the window below", "alt+j", "alt+down"),
		act("window.cycle", Global, "focus next", "focus the next window"),
		act("window.cycle.back", Global, "focus previous", "focus the previous window"),
		act("window.attention", Global, "needs you", "focus the window that waits on you"),
		act("window.move.left", Global, "move left", "swap the window with the one to its left", "alt+shift+h", "alt+shift+left"),
		act("window.move.right", Global, "move right", "swap the window with the one to its right", "alt+shift+l", "alt+shift+right"),
		act("window.move.up", Global, "move up", "swap the window with the one above", "alt+shift+k", "alt+shift+up"),
		act("window.move.down", Global, "move down", "swap the window with the one below", "alt+shift+j", "alt+shift+down"),
		act("window.master", Global, "swap with main", "swap the focused window with the main window"),
		act("window.zoom", Global, "fullscreen", "toggle fullscreen for the focused window"),
		act("window.float", Global, "float", "switch the focused window between floating and tiled"),
		act("window.close", Global, "close", "close the focused window"),
		act("window.grow", Global, "grow", "give the main windows more room"),
		act("window.shrink", Global, "shrink", "give the main windows less room"),
		act("layout.reset", Global, "reset layout", "put the window sizes back"),
		act("layout.toggle", Global, "toggle layout", "switch between the spiral and main column layouts"),
		act("layout.dwindle", Global, "spiral", "arrange windows in a spiral of splits"),
		act("layout.master", Global, "main column", "place the main windows beside a stack of the rest"),
		act("layout.main.more", Global, "more main windows", "put one more window in the main column"),
		act("layout.main.fewer", Global, "fewer main windows", "put one window less in the main column"),
		act("layout.split", Global, "flip split", "turn the split of the focused window the other way"),
		act("session.compact", Global, "compact", "summarize the transcript"),
		act("run.cancel", Global, "cancel run", "stop the run immediately"),
		act("model.switch", Global, "model", "choose a model from the current endpoint"),
		act("open.agents", Global, "agents", "open the agents panel"),
		act("open.sessions", Global, "sessions", "browse saved sessions"),
		act("open.plugins", Global, "plugins", "open the plugins panel"),
		act("open.skills", Global, "skills", "open the skills panel"),

		act("panel.close", Panel, "close", "close the panel", "q"),

		act("composer.send", Session, "send", "send the prompt, or steer the working agent", "enter"),
		quiet(act("session.stop", Session, "escape", "clear a selection or recall, or stop the reply", "esc")),
		quiet(act("composer.up", Session, "up", "move up a row; on the first row, show the previous prompt", "up")),
		quiet(act("composer.down", Session, "down", "move down a row; on the last row, show the next prompt", "down")),
		act("composer.history.prev", Session, "older prompt", "show the previous prompt", "ctrl+p"),
		act("composer.history.next", Session, "newer prompt", "show the next prompt", "ctrl+n"),
		act("composer.history.search", Session, "search history", "search earlier prompts", "ctrl+r"),
		act("composer.attachments.clear", Session, "clear attachments", "remove all attachments"),
		act("complete.start", Session, "complete", "complete the path before the cursor", "tab"),
		act("session.scroll.up", Session, "page up", "scroll the transcript up a page", "pgup"),
		act("session.scroll.down", Session, "page down", "scroll the transcript down a page", "pgdown"),
		act("session.scroll.top", Session, "top", "scroll the transcript to the start", "ctrl+home"),
		act("session.scroll.end", Session, "follow", "scroll to the end of the transcript and follow new output", "ctrl+end"),
		act("session.expand", Session, "expand all", "expand all tool results", "alt+e"),
		act("session.collapse", Session, "collapse all", "collapse all tool results", "alt+shift+e"),
		act("session.thinking", Session, "auto-expand thinking", "expand thinking as it arrives, or keep it folded", "alt+t"),

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
		quiet(act("text.newline", Text, "newline", "break the line", "shift+enter", "ctrl+j", "alt+enter", "ctrl+enter", "super+enter")),
		act("text.undo", Text, "undo", "undo the last edit", "ctrl+z", "super+z", "ctrl+_"),
		act("text.redo", Text, "redo", "redo the edit the last undo took back", "ctrl+y", "ctrl+shift+z", "shift+super+z", "super+y"),
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
		act("complete.accept", Complete, "accept", "insert the selected suggestion", "tab"),
		act("complete.close", Complete, "close", "close the completion popup", "esc"),

		act("search.accept", Search, "done", "close the search bar and keep the search active", "enter"),
		act("search.close", Search, "clear", "clear the search and close the search bar", "esc"),
		quiet(act("search.next", Search, "next", "move to the next match or row", "down", "ctrl+n")),
		quiet(act("search.prev", Search, "previous", "move to the previous match or row", "up", "ctrl+p")),

		act("confirm.yes", Confirm, "yes", "answer yes", "y"),
		act("confirm.no", Confirm, "no", "answer no", "n", "esc"),

		act("edit.apply", Edit, "apply", "save the cell value", "enter"),
		act("edit.cancel", Edit, "cancel", "discard changes to the cell", "esc"),

		act("capture.cancel", Capture, "cancel", "cancel key recording", "esc"),

		quiet(act("palette.up", Palette, "up", "move to the row above", "up", "shift+tab", "ctrl+p")),
		quiet(act("palette.down", Palette, "down", "move to the row below", "down", "tab", "ctrl+n")),
		quiet(act("palette.page.up", Palette, "page up", "move eight rows up", "pgup")),
		quiet(act("palette.page.down", Palette, "page down", "move eight rows down", "pgdown")),
		act("palette.pick", Palette, "run", "run the selected item or submit the entered text", "enter"),
		act("palette.close", Palette, "close", "close the palette", "esc"),

		act("tabs.next", Tabs, "next tab", "switch to the next tab", "tab"),
		act("tabs.prev", Tabs, "previous tab", "switch to the previous tab", "shift+tab"),

		quiet(act("list.up", List, "up", "move to the row above", "up", "k")),
		quiet(act("list.down", List, "down", "move to the row below", "down", "j")),
		quiet(act("list.top", List, "first", "move to the first row", "home", "g")),
		quiet(act("list.bottom", List, "last", "move to the last row", "end", "G")),
		quiet(act("list.page.up", List, "page up", "move up one page", "pgup")),
		quiet(act("list.page.down", List, "page down", "move down one page", "pgdown")),

		act("filter.toggle", Filter, "filter", "open the filter bar or clear the active filter", "/"),

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
		act("view.copy", View, "copy", "copy the selection, else the message under the cursor", "y"),
		quiet(act("view.open", View, "open", "open or fold the part under the cursor", "enter")),
		act("view.leave", View, "leave", "leave reading for typing", "esc", "q", "i"),

		act("approval.allow", Approval, "allow", "approve the request", "y"),
		act("approval.deny", Approval, "deny", "deny the request", "n"),
		act("approval.always", Approval, "always", "approve the request and save an allow rule"),
		quiet(act("approval.down", Approval, "down", "scroll down a line", "j", "down")),
		quiet(act("approval.up", Approval, "up", "scroll up a line", "k", "up")),
		quiet(act("approval.page.down", Approval, "page down", "scroll down a page", "pgdown")),
		quiet(act("approval.page.up", Approval, "page up", "scroll up a page", "pgup")),
		quiet(act("approval.top", Approval, "top", "scroll to the top", "g", "home")),

		act("terminal.copy", Terminal, "copy", "copy the selection, or the whole screen when nothing is selected", "ctrl+shift+c"),
		act("terminal.paste", Terminal, "paste", "paste the clipboard into the shell", "ctrl+shift+v"),
		act("terminal.search", Terminal, "search", "search the screen and the scrollback", "ctrl+shift+f"),
		act("terminal.scroll.up", Terminal, "page up", "scroll back a page", "shift+pgup"),
		act("terminal.scroll.down", Terminal, "page down", "scroll forward a page", "shift+pgdown"),
		quiet(act("terminal.scroll.top", Terminal, "top", "scroll to the oldest row kept", "shift+home")),
		quiet(act("terminal.scroll.bottom", Terminal, "bottom", "scroll back to the live screen", "shift+end")),
		act("terminal.prompt.prev", Terminal, "previous prompt", "scroll to the previous shell prompt, for shells that mark them", "ctrl+shift+z"),
		act("terminal.prompt.next", Terminal, "next prompt", "scroll to the next shell prompt, for shells that mark them", "ctrl+shift+x"),
		quiet(act("terminal.select.all", Terminal, "select all", "select the screen and the scrollback", "ctrl+shift+a")),
		act("terminal.lock", Terminal, "lock keys", "send every key to the shell, the window keys included, until pressed again", "ctrl+shift+l"),
		act("terminal.clear", Terminal, "clear", "forget the scrollback", "ctrl+shift+k"),
		act("terminal.restart", Terminal, "restart", "end the shell and start it again in the same directory", "ctrl+shift+r"),
	)
	return out
}()
