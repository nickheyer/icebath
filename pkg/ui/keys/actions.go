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
	Sessions   = "sessions"
	Agents     = "agents"
	Findings   = "findings"
	Jobs       = "jobs"
	Transcript = "transcript"
	Approval   = "approval"
	Mission    = "mission"
	Form       = "form"
	Area       = "area"
	Profiles   = "config.profiles"
	Models     = "config.models"
	App        = "config.app"
	Rules      = "config.rules"
	Hooks      = "config.hooks"
	MCP        = "config.mcp"
	Tools      = "config.tools"
	Keybinds   = "config.keys"
	Extensions = "config.ext"
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
	Sessions:   "sessions panel",
	Agents:     "agents panel",
	Findings:   "findings panel",
	Jobs:       "jobs panel",
	Transcript: "agent transcripts",
	Approval:   "approval dialog",
	Mission:    "mission panels",
	Form:       "dialogs",
	Area:       "multi line fields in dialogs",
	Profiles:   "profiles tab",
	Models:     "models tab",
	App:        "app tab",
	Rules:      "rules tab",
	Hooks:      "hooks tab",
	MCP:        "MCP tab",
	Tools:      "tools tab",
	Keybinds:   "keys tab",
	Extensions: "extensions tab",
}

func act(id, ctx, label, doc string, keys ...string) Action {
	return Action{ID: id, Context: ctx, Label: label, Doc: doc, Default: keys}
}

func quiet(a Action) Action { a.Quiet = true; return a }

func overlay(a Action) Action { a.Overlay = true; return a }

// registry lists the available actions in display order.
var registry = func() []Action {
	out := []Action{
		overlay(act("palette", Global, "commands", "open the command palette and close any overlay", "alt+space")),
		act("open", Global, "open panel", "choose a panel to open in this workspace", "alt+a"),
		act("keys.edit", Global, "keybinds", "open the keybind editor"),
		overlay(act("quit", Global, "quit", "quit icebath", "ctrl+q")),
		overlay(act("interrupt", Global, "cancel", "cancel the agent; quit if idle or pressed twice", "ctrl+c")),
		act("session.new", Global, "new session", "open a fresh session in this workspace", "alt+n"),
		act("session.close", Global, "close session", "close the session, storing it"),
		act("workspace.new", Global, "new workspace", "open a workspace holding a fresh session", "alt+shift+n"),
		act("workspace.close", Global, "close workspace", "close the workspace, storing every session in it", "alt+shift+w"),
		act("template.save", Global, "save template", "save every workspace as a layout template under a name"),
		act("template.apply", Global, "apply template", "choose a layout template to add its workspaces"),
		act("template.default", Global, "default template", "choose the layout template a fresh project starts with"),
		act("template.delete", Global, "delete template", "choose a layout template to forget"),
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
		act("session.open", Global, "open session", "choose a saved session to resume in this workspace"),
		act("session.fork", Global, "fork", "fork the session beside it"),
		act("session.fork.at", Global, "fork at turn", "choose which turn to fork the session from"),
		act("session.rename", Global, "rename", "rename the session"),
		act("session.compact", Global, "compact", "summarize the transcript"),
		act("session.clear", Global, "clear", "save the current session and start a new one in this workspace"),
		act("session.export", Global, "export", "export the transcript as Markdown"),
		act("run.cancel", Global, "cancel run", "stop the run immediately"),
		act("run.interrupt", Global, "interrupt run", "stop the run after the current tool finishes"),
		act("mission.start", Global, "start mission", "start a mission from an objective"),
		act("mission.open", Global, "open mission", "choose a mission to open"),
		act("attention.next", Global, "attention", "go to the next session waiting on you, approvals first", "alt+i"),
		act("answer.allow", Global, "allow", "approve the next request in the queue"),
		act("answer.deny", Global, "deny", "deny the next request in the queue"),
		act("answer.always", Global, "always allow", "approve the request and save an allow rule"),
		act("answer.never", Global, "never allow", "deny the request and save a deny rule"),
		act("model.switch", Global, "model", "choose a model from the current endpoint"),
		act("profile.switch", Global, "profile", "choose an endpoint profile"),
		act("mode.set", Global, "permission mode", "choose the permission mode"),
		act("theme.set", Global, "theme", "choose a color theme"),
		act("motion.set", Global, "motion", "adjust how much windows animate"),
		act("open.agents", Global, "agents", "open the agents panel"),
		act("open.metrics", Global, "metrics", "open the metrics panel"),
		act("open.jobs", Global, "jobs", "open the background jobs panel"),
		act("open.sessions", Global, "sessions", "browse saved sessions"),
		act("open.config", Global, "config", "open settings"),
		act("open.findings", Global, "findings", "open the findings panel"),
		act("agent.open", Global, "agent transcript", "choose a child agent's transcript to open"),

		act("panel.close", Panel, "close", "close the panel", "q"),

		act("composer.send", Session, "send", "send the prompt, or steer the working agent", "enter"),
		act("session.stop", Session, "stop", "stop the run", "esc"),
		act("composer.complete", Session, "complete", "complete the path or command under the cursor", "tab"),
		quiet(act("composer.up", Session, "up", "move up a line; on the first line, show the previous prompt", "up")),
		quiet(act("composer.down", Session, "down", "move down a line; on the last line, show the next prompt", "down")),
		act("composer.attachments.clear", Session, "clear attachments", "remove all attachments", "ctrl+x"),
		quiet(act("composer.left", Session, "left", "move the cursor left", "left")),
		quiet(act("composer.right", Session, "right", "move the cursor right", "right")),
		quiet(act("composer.word.left", Session, "word left", "move the cursor one word to the left", "ctrl+left")),
		quiet(act("composer.word.right", Session, "word right", "move the cursor one word to the right", "ctrl+right")),
		quiet(act("composer.line.start", Session, "line start", "move the cursor to the start of the line", "home", "ctrl+a")),
		quiet(act("composer.line.end", Session, "line end", "move the cursor to the end of the line", "end", "ctrl+e")),
		quiet(act("composer.delete.back", Session, "delete back", "delete the character before the cursor", "backspace")),
		quiet(act("composer.delete.forward", Session, "delete forward", "delete the character under the cursor", "delete")),
		quiet(act("composer.delete.word.back", Session, "delete word back", "delete the word before the cursor", "ctrl+w", "ctrl+backspace")),
		quiet(act("composer.delete.word.forward", Session, "delete word forward", "delete the word after the cursor", "ctrl+delete")),
		quiet(act("composer.delete.line.before", Session, "delete to start", "delete from the cursor to the start of the line", "ctrl+u")),
		quiet(act("composer.delete.line.after", Session, "delete to end", "delete from the cursor to the end of the line", "ctrl+k")),
		quiet(act("composer.select.all", Session, "select all", "select the entire prompt", "ctrl+shift+a")),
		quiet(act("composer.select.left", Session, "select left", "extend the selection one character to the left", "shift+left")),
		quiet(act("composer.select.right", Session, "select right", "extend the selection one character to the right", "shift+right")),
		quiet(act("composer.select.word.left", Session, "select word left", "extend the selection one word to the left", "ctrl+shift+left")),
		quiet(act("composer.select.word.right", Session, "select word right", "extend the selection one word to the right", "ctrl+shift+right")),
		quiet(act("composer.select.up", Session, "select up", "extend the selection one line up", "shift+up")),
		quiet(act("composer.select.down", Session, "select down", "extend the selection one line down", "shift+down")),
		quiet(act("composer.copy", Session, "copy", "copy the selection to the clipboard", "ctrl+shift+c")),
		quiet(act("composer.paste", Session, "paste", "paste clipboard contents at the cursor", "ctrl+v")),
		act("session.search", Session, "search", "search the transcript", "alt+/"),
		act("session.scroll.up", Session, "page up", "scroll the transcript up a page", "pgup"),
		act("session.scroll.down", Session, "page down", "scroll the transcript down a page", "pgdown"),
		act("session.scroll.top", Session, "top", "scroll the transcript to the start", "ctrl+home"),
		act("session.scroll.end", Session, "follow", "scroll to the end of the transcript and follow new output", "alt+g", "ctrl+end"),
		act("session.expand", Session, "expand all", "expand all tool results", "alt+e"),
		act("session.collapse", Session, "collapse all", "collapse all tool results", "alt+shift+e"),
		act("session.thinking", Session, "thinking", "show or hide thinking output", "alt+t"),
		act("mode.cycle", Session, "mode", "switch to the next permission mode", "shift+tab"),

		quiet(act("complete.next", Complete, "next", "select the next suggestion", "down", "tab", "ctrl+n")),
		quiet(act("complete.prev", Complete, "previous", "select the previous suggestion", "up", "ctrl+p")),
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

		act("sessions.resume", Sessions, "resume", "open the selected sessions in this workspace", "enter"),
		act("sessions.fork", Sessions, "fork", "fork the selected sessions into this workspace", "f"),
		act("sessions.delete", Sessions, "delete", "ask for confirmation, then delete the selected sessions", "d"),
		act("sessions.every", Sessions, "scope", "switch between the sessions of this project and those of every project", "p"),
		quiet(act("sessions.select.up", Sessions, "select up", "extend the selection to the row above", "shift+up", "K")),
		quiet(act("sessions.select.down", Sessions, "select down", "extend the selection to the row below", "shift+down", "J")),

		act("agents.open", Agents, "transcript", "open the agent's transcript", "enter"),
		act("agents.stop", Agents, "stop", "stop the agent", "x"),

		act("findings.detail", Findings, "detail", "show or hide the finding and its evidence", "enter"),
		act("findings.resolve", Findings, "resolve", "mark the finding as resolved or reopen it", "x"),
		act("findings.tree", Findings, "tree", "switch between table and tree views", "t"),
		act("findings.kind", Findings, "kind", "cycle through kind filters", "c"),
		act("findings.tag", Findings, "tag", "cycle through tag filters", "T"),
		act("findings.status", Findings, "status", "cycle through status filters", "s"),
		act("findings.export.md", Findings, "export md", "export all findings as Markdown", "e"),
		act("findings.export.json", Findings, "export json", "export all findings as JSON", "E"),

		act("jobs.stop", Jobs, "stop", "stop the job", "x"),
		act("jobs.follow", Jobs, "follow", "pause or resume scrolling with new output", "f"),
		act("jobs.scroll.up", Jobs, "tail up", "scroll job output up one page", "pgup"),
		act("jobs.scroll.down", Jobs, "tail down", "scroll job output down one page", "pgdown"),

		quiet(act("transcript.down", Transcript, "down", "scroll down a line", "j", "down")),
		quiet(act("transcript.up", Transcript, "up", "scroll up a line", "k", "up")),
		act("transcript.page.down", Transcript, "page down", "scroll down a page", "pgdown", "ctrl+d", "space"),
		act("transcript.page.up", Transcript, "page up", "scroll up a page", "pgup", "ctrl+u"),
		quiet(act("transcript.top", Transcript, "top", "scroll to the start", "g", "home")),
		quiet(act("transcript.bottom", Transcript, "end", "scroll to the end", "G", "end")),
		act("transcript.search", Transcript, "search", "search the transcript", "/"),
		act("transcript.next", Transcript, "next match", "move to the next match", "n"),
		act("transcript.prev", Transcript, "previous match", "move to the previous match", "N"),
		act("transcript.turn.prev", Transcript, "previous turn", "jumps to the previous turn", "["),
		act("transcript.turn.next", Transcript, "next turn", "jumps to the next turn", "]"),
		quiet(act("transcript.block.next", Transcript, "next block", "move the cursor to the next block", ".")),
		quiet(act("transcript.block.prev", Transcript, "previous block", "move the cursor to the previous block", ",")),
		act("transcript.toggle", Transcript, "expand", "expand or collapse the block under the cursor", "enter", "o"),
		act("transcript.expand", Transcript, "expand all", "expand all tool results", "e"),
		act("transcript.collapse", Transcript, "collapse all", "collapse all tool results", "E"),
		act("transcript.thinking", Transcript, "thinking", "show or hide thinking output", "T"),
		act("transcript.follow", Transcript, "follow", "pause or resume scrolling with new output", "f"),

		act("approval.allow", Approval, "allow", "approve the request", "y"),
		act("approval.deny", Approval, "deny", "deny the request", "n"),
		act("approval.always", Approval, "always", "approve the request and save an allow rule", "Y", "a"),
		act("approval.never", Approval, "never", "deny the request and save a deny rule", "N"),
		quiet(act("approval.down", Approval, "down", "scroll down a line", "j", "down")),
		quiet(act("approval.up", Approval, "up", "scroll up a line", "k", "up")),
		quiet(act("approval.page.down", Approval, "page down", "scroll down a page", "pgdown")),
		quiet(act("approval.page.up", Approval, "page up", "scroll up a page", "pgup")),
		quiet(act("approval.top", Approval, "top", "scroll to the top", "g", "home")),

		act("mission.done", Mission, "done", "mark the mission as done", "d"),
		act("mission.failed", Mission, "failed", "mark the mission as failed", "x"),
		act("mission.resume", Mission, "resume", "resume a finished mission", "r"),
		act("mission.forget", Mission, "forget", "forget a finished mission", "D"),

		act("form.next", Form, "next field", "move to the next field", "tab", "down"),
		act("form.prev", Form, "previous field", "move to the previous field", "shift+tab", "up"),
		quiet(act("form.left", Form, "left", "select the previous option, decrease the value, or move the cursor left", "left")),
		quiet(act("form.right", Form, "right", "select the next option, increase the value, or move the cursor right", "right")),
		act("form.submit", Form, "save", "save changes in the dialog", "enter"),
		act("form.cancel", Form, "cancel", "close the dialog without saving", "esc"),
		quiet(act("form.delete.back", Form, "delete", "delete the character before the cursor", "backspace")),
		quiet(act("form.delete.word", Form, "delete word", "delete the word before the cursor", "ctrl+w")),
		quiet(act("form.clear", Form, "clear", "clear the field", "ctrl+u")),
		quiet(act("form.unset", Form, "unset", "clear the value of an optional field", "delete")),
		quiet(act("form.home", Form, "start", "move the cursor to the start of the field", "home", "ctrl+a")),
		quiet(act("form.end", Form, "end", "move the cursor to the end of the field", "end", "ctrl+e")),

		act("area.newline", Area, "new line", "start a new line", "enter"),
		act("area.save", Area, "save", "save changes in the dialog", "ctrl+s"),
		quiet(act("area.up", Area, "up", "move up a line; on the first line, move to the previous field", "up")),
		quiet(act("area.down", Area, "down", "move down a line; on the last line, move to the next field", "down")),

		act("config.profile.use", Profiles, "use", "switch to the selected profile", "enter"),
		act("config.profile.add", Profiles, "new", "add a profile for a provider", "a"),
		act("config.profile.edit", Profiles, "edit", "edit the profile", "e"),
		act("config.profile.delete", Profiles, "delete", "ask for confirmation, then delete the profile", "d"),
		act("config.profile.toggle", Profiles, "on/off", "turn the profile on or off", "space"),
		act("config.profile.up", Profiles, "move up", "move the profile up the list", "K", "shift+up"),
		act("config.profile.down", Profiles, "move down", "move the profile down the list", "J", "shift+down"),
		act("config.profile.detail", Profiles, "detail", "show or hides the profile details", "o"),
		act("config.profile.recheck", Profiles, "recheck", "recheck all credentials", "r"),
		act("config.model.use", Models, "use", "switch to the selected model", "enter"),
		act("config.model.detail", Models, "detail", "show or hides the model details", "o"),
		act("config.model.refresh", Models, "refresh", "refresh the model list from the endpoint", "r"),
		act("config.app.left", App, "less", "select the previous option or decrease the value", "left", "h"),
		act("config.app.right", App, "more", "select the next option or increase the value", "right", "l"),
		act("config.app.edit", App, "type", "type a number or text, or flip a switch", "enter"),
		act("config.app.unset", App, "unset", "reset the setting to its default", "delete", "backspace"),
		act("config.rule.add", Rules, "new", "add a rule", "a"),
		act("config.rule.edit", Rules, "edit", "edit the rule", "enter", "e"),
		act("config.rule.delete", Rules, "delete", "ask for confirmation, then delete the rule", "d"),
		act("config.hook.add", Hooks, "new", "add a hook", "a"),
		act("config.hook.edit", Hooks, "edit", "edit the hook", "enter", "e"),
		act("config.hook.delete", Hooks, "delete", "ask for confirmation, then delete the hook", "d"),
		act("config.mcp.detail", MCP, "detail", "show or hides the server details", "enter", "o"),
		act("config.mcp.add", MCP, "new", "add a server", "a"),
		act("config.mcp.edit", MCP, "edit", "edit the server", "e"),
		act("config.mcp.delete", MCP, "delete", "ask for confirmation, then delete the server", "d"),
		act("config.mcp.toggle", MCP, "on/off", "turn the server on or off", "space"),
		act("config.mcp.reconnect", MCP, "reconnect", "reconnect to the server", "R"),
		act("config.tool.toggle", Tools, "on/off", "turn the tool on or off for the model", "space"),
		act("config.tool.detail", Tools, "detail", "show or hides the tool schema", "enter", "o"),
		act("config.key.rebind", Keybinds, "rebind", "record a key and replace the existing bindings with it", "enter"),
		act("config.key.add", Keybinds, "add key", "record an additional key for this action", "a"),
		act("config.key.type", Keybinds, "type keys", "enter key names for this action", "t"),
		act("config.key.unbind", Keybinds, "unbind", "remove all keybindings for this action", "d"),
		act("config.key.reset", Keybinds, "reset", "restore the default keybindings for this action", "r"),
		act("config.key.detail", Keybinds, "detail", "show or hides the action details", "o"),
		act("config.ext.detail", Extensions, "detail", "show or hides what the extension contributes", "enter", "o"),
		act("kind.set", Global, "kind", "choose the kind the session runs as"),
		act("workflow.stop", Global, "stop workflow", "choose a running workflow to stop"),
	)
	return out
}()
