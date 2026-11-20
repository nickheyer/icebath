package keys

import "strconv"

// Keybinding contexts. The focused window determines which contexts are active.
// Global bindings are checked last.
const (
	Global     = "global"
	Panel      = "panel"
	Session    = "session"
	Complete   = "complete"
	Search     = "search"
	Confirm    = "confirm"
	Edit       = "edit"
	Capture    = "capture"
	Palette    = "palette"
	Tabs       = "tabs"
	List       = "list"
	Filter     = "filter"
	Detail     = "detail"
	Transcript = "transcript"
	Approval   = "approval"
	Terminal   = "terminal"
)

// ContextDocs describes each context in the keybind editor.
var ContextDocs = map[string]string{
	Global:     "everywhere",
	Panel:      "all panels except the session",
	Session:    "session prompt and transcript",
	Complete:   "completion popup",
	Search:     "search bars",
	Confirm:    "yes/no prompts",
	Edit:       "table cells while editing",
	Capture:    "keybind editor while recording a key",
	Palette:    "command palette and pickers",
	Tabs:       "tabbed panels, such as config",
	List:       "tables",
	Filter:     "tables with filters",
	Detail:     "open detail panes",
	Transcript: "agent transcripts",
	Approval:   "approval dialog",
	Terminal:   "terminal windows, every other key going to the shell",
}

func act(id, ctx, label, doc string, keys ...string) Action {
	return Action{ID: id, Context: ctx, Label: label, Doc: doc, Default: keys}
}

func quiet(a Action) Action { a.Quiet = true; return a }

func overlay(a Action) Action { a.Overlay = true; return a }

// registry lists the available actions in display order.
var registry = func() []Action {
	out := []Action{
		overlay(act("palette", Global, "palette", "open the command palette and close any overlay", "alt+space")),
		act("open", Global, "open panel", "choose a panel to open in this workspace", "alt+a"),
		overlay(act("quit", Global, "quit", "quit icebath", "ctrl+q")),
		overlay(act("interrupt", Global, "cancel", "cancel the agent; quit if idle or pressed twice", "ctrl+c")),
		act("session.new", Global, "new session", "open a fresh session in this workspace", "alt+n"),
		act("terminal.new", Global, "new terminal", "open a terminal running the shell in this workspace", "alt+enter"),
		act("terminal.tab", Global, "terminal tab", "open a terminal as a new tab of the focused window", "alt+shift+enter"),
		act("tabs.join", Global, "tab in", "put the focused window into the window beside it as a tab", "alt+c"),
		act("tabs.split", Global, "tab out", "take the shown tab out into a window of its own", "alt+shift+c"),
		act("tabs.cycle", Global, "next tab", "show the next tab of the focused window", "alt+>"),
		act("tabs.cycle.back", Global, "previous tab", "show the previous tab of the focused window", "alt+<"),
		act("workspace.new", Global, "new workspace", "open a workspace holding a fresh session", "alt+shift+n"),
		act("workspace.close", Global, "close workspace", "close the workspace, storing every session in it", "alt+shift+w"),
		act("workspace.next", Global, "next workspace", "switch to the next workspace", "alt+]"),
		act("workspace.prev", Global, "previous workspace", "switch to the previous workspace", "alt+["),
	}
	for i := 1; i <= 9; i++ {
		n := strconv.Itoa(i)
		out = append(out, act("workspace."+n, Global, "workspace "+n, "switch to workspace "+n, "alt+"+n))
	}
	out = append(out,
		act("window.focus.left", Global, "focus left", "focus the window to the left", "alt+h", "alt+left"),
		act("window.focus.right", Global, "focus right", "focus the window to the right", "alt+l", "alt+right"),
		act("window.focus.up", Global, "focus up", "focus the window above", "alt+k", "alt+up"),
		act("window.focus.down", Global, "focus down", "focus the window below", "alt+j", "alt+down"),
		act("window.cycle", Global, "focus next", "focus the next window", "alt+."),
		act("window.cycle.back", Global, "focus previous", "focus the previous window", "alt+,"),
		act("window.move.left", Global, "move left", "swap the window with the one to its left", "alt+shift+h", "alt+shift+left"),
		act("window.move.right", Global, "move right", "swap the window with the one to its right", "alt+shift+l", "alt+shift+right"),
		act("window.move.up", Global, "move up", "swap the window with the one above", "alt+shift+k", "alt+shift+up"),
		act("window.move.down", Global, "move down", "swap the window with the one below", "alt+shift+j", "alt+shift+down"),
		act("window.master", Global, "swap with master", "swap the focused window with the master window", "alt+m"),
		act("window.zoom", Global, "fullscreen", "toggle fullscreen for the focused window", "alt+f"),
		act("window.float", Global, "float", "switch the focused window between floating and tiled", "alt+v"),
		act("window.close", Global, "close", "close the focused window", "alt+w"),
		act("window.grow", Global, "grow", "expand the master area", "alt+="),
		act("window.shrink", Global, "shrink", "shrink the master area", "alt+-"),
		act("layout.reset", Global, "reset layout", "reset window sizes to their defaults", "alt+r"),
		act("layout.toggle", Global, "toggle layout", "switch between dwindle and master layouts", "alt+d", "alt+shift+space"),
		act("layout.dwindle", Global, "dwindle", "arrange windows in a spiral of splits"),
		act("layout.master", Global, "master", "place the main windows beside a stack of the remaining windows"),
		act("layout.main.more", Global, "more main windows", "increase the number of windows in the main area", "alt++"),
		act("layout.main.fewer", Global, "fewer main windows", "decrease the number of windows in the main area", "alt+_"),
		act("layout.split", Global, "flip split", "change the split direction for the focused window", "alt+s"),
		act("session.compact", Global, "compact", "summarize the transcript"),
		act("run.cancel", Global, "cancel run", "stop the run immediately"),
		act("model.switch", Global, "model", "choose a model from the current endpoint"),
		act("open.agents", Global, "agents", "open the agents panel"),
		act("open.sessions", Global, "sessions", "browse saved sessions"),
		act("open.plugins", Global, "plugins", "open the plugins panel"),
		act("open.skills", Global, "skills", "open the skills panel"),

		act("panel.close", Panel, "close", "close the panel", "q"),

		act("composer.send", Session, "send", "send the prompt, or steer the working agent", "enter"),
		quiet(act("session.stop", Session, "escape", "cancel the edit of a queued input; twice on an empty prompt, stop the run", "esc")),
		quiet(act("composer.up", Session, "up", "move up a line; on the first line, show the previous prompt", "up")),
		quiet(act("composer.down", Session, "down", "move down a line; on the last line, show the next prompt", "down")),
		act("composer.attachments.clear", Session, "clear attachments", "remove all attachments", "ctrl+x"),
		act("composer.undo", Session, "undo", "undo the last edit of the prompt", "ctrl+_", "super+z"),
		act("composer.redo", Session, "redo", "redo the edit the last undo took back", "ctrl+y", "ctrl+shift+z", "shift+super+z", "super+y"),
		quiet(act("composer.left", Session, "left", "move the cursor left", "left")),
		quiet(act("composer.right", Session, "right", "move the cursor right", "right")),
		quiet(act("composer.word.left", Session, "word left", "move the cursor one word to the left", "ctrl+left", "super+left")),
		quiet(act("composer.word.right", Session, "word right", "move the cursor one word to the right", "ctrl+right", "super+right")),
		quiet(act("composer.line.start", Session, "line start", "move the cursor to the start of the line", "home", "ctrl+a")),
		quiet(act("composer.line.end", Session, "line end", "move the cursor to the end of the line", "end", "ctrl+e", "super+e")),
		quiet(act("composer.delete.back", Session, "delete back", "delete the character before the cursor", "backspace")),
		quiet(act("composer.delete.forward", Session, "delete forward", "delete the character under the cursor", "delete")),
		quiet(act("composer.delete.word.back", Session, "delete word back", "delete the word before the cursor", "ctrl+w", "ctrl+backspace", "super+w")),
		quiet(act("composer.delete.word.forward", Session, "delete word forward", "delete the word after the cursor", "ctrl+delete")),
		quiet(act("composer.delete.line.before", Session, "delete to start", "delete from the cursor to the start of the line", "ctrl+u", "super+u", "super+backspace")),
		quiet(act("composer.delete.line.after", Session, "delete to end", "delete from the cursor to the end of the line", "ctrl+k", "super+k", "super+delete")),
		quiet(act("composer.select.all", Session, "select all", "select the entire prompt", "ctrl+shift+a", "super+a")),
		quiet(act("composer.select.left", Session, "select left", "extend the selection one character to the left", "shift+left")),
		quiet(act("composer.select.right", Session, "select right", "extend the selection one character to the right", "shift+right")),
		quiet(act("composer.select.word.left", Session, "select word left", "extend the selection one word to the left", "ctrl+shift+left")),
		quiet(act("composer.select.word.right", Session, "select word right", "extend the selection one word to the right", "ctrl+shift+right")),
		quiet(act("composer.select.up", Session, "select up", "extend the selection one line up", "shift+up")),
		quiet(act("composer.select.down", Session, "select down", "extend the selection one line down", "shift+down")),
		quiet(act("composer.copy", Session, "copy", "copy the selection to the clipboard", "ctrl+shift+c", "ctrl+super+c", "super+c")),
		quiet(act("composer.paste", Session, "paste", "paste clipboard contents at the cursor", "ctrl+v", "super+v")),
		act("session.scroll.up", Session, "page up", "scroll the transcript up a page", "pgup"),
		act("session.scroll.down", Session, "page down", "scroll the transcript down a page", "pgdown"),
		act("session.scroll.top", Session, "top", "scroll the transcript to the start", "ctrl+home"),
		act("session.scroll.end", Session, "follow", "scroll to the end of the transcript and follow new output", "alt+g", "ctrl+end"),
		act("session.expand", Session, "expand all", "expand all tool results", "alt+e"),
		act("session.collapse", Session, "collapse all", "collapse all tool results", "alt+shift+e"),
		act("session.thinking", Session, "auto-expand thinking", "open thinking blocks as they arrive, or leave them folded until opened", "alt+t"),

		quiet(act("complete.next", Complete, "next", "select the next suggestion", "down", "tab", "ctrl+n")),
		quiet(act("complete.prev", Complete, "previous", "select the previous suggestion", "up", "ctrl+p", "shift+tab")),
		act("complete.accept", Complete, "accept", "insert the selected suggestion", "enter"),
		act("complete.close", Complete, "close", "close the completion popup", "esc"),

		act("search.accept", Search, "done", "close the search bar and keep the search active", "enter"),
		act("search.close", Search, "clear", "clear the search and close the search bar", "esc"),
		quiet(act("search.next", Search, "next", "move to the next match or row", "down", "ctrl+n")),
		quiet(act("search.prev", Search, "previous", "move to the previous match or row", "up", "ctrl+p")),
		quiet(act("search.delete.back", Search, "delete", "delete the character before the cursor", "backspace")),
		quiet(act("search.clear", Search, "clear text", "clear the search text", "ctrl+u")),
		quiet(act("search.delete.word", Search, "delete word", "delete the word before the cursor", "ctrl+w")),

		act("confirm.yes", Confirm, "yes", "answer yes", "y"),
		act("confirm.no", Confirm, "no", "answer no", "n", "esc"),

		act("edit.apply", Edit, "apply", "save the cell value", "enter"),
		act("edit.cancel", Edit, "cancel", "discard changes to the cell", "esc"),
		quiet(act("edit.delete.back", Edit, "delete", "delete the character before the cursor", "backspace")),
		quiet(act("edit.clear", Edit, "clear", "clear the cell", "ctrl+u")),
		quiet(act("edit.end", Edit, "end", "keep the current text and move the cursor to the end", "right", "end")),

		act("capture.cancel", Capture, "cancel", "cancel key recording", "esc"),

		quiet(act("palette.up", Palette, "up", "move to the row above", "up", "shift+tab")),
		quiet(act("palette.down", Palette, "down", "move to the row below", "down", "tab", "ctrl+n")),
		quiet(act("palette.page.up", Palette, "page up", "move eight rows up", "pgup")),
		quiet(act("palette.page.down", Palette, "page down", "move eight rows down", "pgdown")),
		act("palette.pick", Palette, "run", "run the selected item or submit the entered text", "enter"),
		act("palette.close", Palette, "close", "close the palette", "esc"),
		quiet(act("palette.delete.back", Palette, "delete", "delete the character before the cursor", "backspace")),
		quiet(act("palette.delete.forward", Palette, "delete forward", "delete the character under the cursor", "delete")),
		quiet(act("palette.left", Palette, "left", "move the cursor left", "left")),
		quiet(act("palette.right", Palette, "right", "move the cursor right", "right")),
		quiet(act("palette.home", Palette, "start", "move the cursor to the start", "home", "ctrl+a")),
		quiet(act("palette.end", Palette, "end", "move the cursor to the end", "end", "ctrl+e")),
		quiet(act("palette.clear", Palette, "clear", "clear the search text", "ctrl+u")),
		quiet(act("palette.delete.word", Palette, "delete word", "delete the word before the cursor", "ctrl+w")),

		act("tabs.next", Tabs, "next tab", "switch to the next tab", "tab"),
		act("tabs.prev", Tabs, "previous tab", "switch to the previous tab", "shift+tab"),

		quiet(act("list.up", List, "up", "move to the row above", "k", "up")),
		quiet(act("list.down", List, "down", "move to the row below", "j", "down")),
		quiet(act("list.top", List, "first", "move to the first row", "g", "home")),
		quiet(act("list.bottom", List, "last", "move to the last row", "G", "end")),
		quiet(act("list.page.up", List, "page up", "move up one page", "pgup")),
		quiet(act("list.page.down", List, "page down", "move down one page", "pgdown")),

		act("filter.toggle", Filter, "filter", "open the filter bar or clear the active filter", "/"),

		act("detail.scroll.down", Detail, "detail down", "scroll the detail pane down a page", "pgdown"),
		act("detail.scroll.up", Detail, "detail up", "scroll the detail pane up a page", "pgup"),

		quiet(act("transcript.down", Transcript, "down", "scroll down a line", "j", "down")),
		quiet(act("transcript.up", Transcript, "up", "scroll up a line", "k", "up")),
		act("transcript.page.down", Transcript, "page down", "scroll down a page", "pgdown", "ctrl+d", "space"),
		act("transcript.page.up", Transcript, "page up", "scroll up a page", "pgup", "ctrl+u"),
		quiet(act("transcript.top", Transcript, "top", "scroll to the start", "g", "home")),
		quiet(act("transcript.bottom", Transcript, "end", "scroll to the end", "G", "end")),

		act("approval.allow", Approval, "allow", "approve the request", "y"),
		act("approval.deny", Approval, "deny", "deny the request", "n"),
		act("approval.always", Approval, "always", "approve the request and save an allow rule", "Y", "a"),
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
