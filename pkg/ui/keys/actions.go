package keys

import "strconv"

// Contexts a bind may belong to, the focused card naming the ones in force and global always last
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
	Files      = "files"
	Findings   = "findings"
	Jobs       = "jobs"
	Transcript = "transcript"
	Approval   = "approval"
	Mission    = "mission"
	Form       = "form"
	Profiles   = "config.profiles"
	Providers  = "config.providers"
	Models     = "config.models"
	App        = "config.app"
	Rules      = "config.rules"
	Hooks      = "config.hooks"
	MCP        = "config.mcp"
	Tools      = "config.tools"
	Log        = "config.log"
	Keybinds   = "config.keys"
)

// What each context covers, shown in the editor
var ContextDocs = map[string]string{
	Global:     "everywhere, after the focused card had its say",
	Panel:      "any card that is not the prompt",
	Session:    "the session card, prompt and transcript",
	Complete:   "the prompt while its completion popup is open",
	Search:     "a search bar while it takes typing",
	Confirm:    "a yes or no question",
	Edit:       "a table cell being typed into",
	Capture:    "the keybind editor while it waits for a key",
	Palette:    "the command palette and every picker",
	Tabs:       "a tabbed card such as the configuration",
	List:       "any table",
	Filter:     "a table that narrows its rows by text",
	Detail:     "the detail pane under a table while it is open",
	Sessions:   "the stored sessions",
	Agents:     "the agent roster",
	Files:      "the files touched",
	Findings:   "the findings",
	Jobs:       "the background jobs",
	Transcript: "an agent transcript on its own",
	Approval:   "the approval dialog",
	Mission:    "a mission card",
	Form:       "a dialog of fields such as a new profile, rule, hook or server",
	Profiles:   "the profiles tab",
	Providers:  "the providers tab",
	Models:     "the models tab",
	App:        "the app tab",
	Rules:      "the rules tab",
	Hooks:      "the hooks tab",
	MCP:        "the mcp tab",
	Tools:      "the tools tab",
	Log:        "the log tab",
	Keybinds:   "the keys tab",
}

func act(id, ctx, label, doc string, keys ...string) Action {
	return Action{ID: id, Context: ctx, Label: label, Doc: doc, Default: keys}
}

func quiet(a Action) Action { a.Quiet = true; return a }

func overlay(a Action) Action { a.Overlay = true; return a }

// Every action icebath answers, in the order the file lists them
var registry = func() []Action {
	out := []Action{
		overlay(act("palette", Global, "commands", "opens the command palette, closes any overlay", "alt+space")),
		act("open", Global, "open panel", "picks a panel to open in this workspace", "alt+a"),
		act("keys.edit", Global, "keybinds", "opens the keybind editor"),
		overlay(act("quit", Global, "quit", "leaves icebath", "ctrl+q")),
		overlay(act("interrupt", Global, "cancel", "cancels the run, quits when nothing runs or when pressed again", "ctrl+c")),
		act("workspace.new", Global, "new session", "opens a fresh session in a new workspace", "alt+n"),
		act("workspace.close", Global, "close workspace", "closes the workspace on the stage", "alt+shift+w"),
		act("workspace.next", Global, "next workspace", "shows the next workspace", "alt+]"),
		act("workspace.prev", Global, "previous workspace", "shows the previous workspace", "alt+["),
	}
	for i := 1; i <= 9; i++ {
		n := strconv.Itoa(i)
		out = append(out, act("workspace."+n, Global, "workspace "+n, "shows workspace "+n, "alt+"+n))
	}
	out = append(out,
		act("window.focus.left", Global, "focus left", "focuses the card to the left", "alt+h", "alt+left"),
		act("window.focus.right", Global, "focus right", "focuses the card to the right", "alt+l", "alt+right"),
		act("window.focus.up", Global, "focus up", "focuses the card above", "alt+k", "alt+up"),
		act("window.focus.down", Global, "focus down", "focuses the card below", "alt+j", "alt+down"),
		act("window.cycle", Global, "focus next", "focuses the next card", "alt+."),
		act("window.cycle.back", Global, "focus previous", "focuses the previous card", "alt+,"),
		act("window.move.left", Global, "move left", "trades the card with the one to its left", "alt+shift+h", "alt+shift+left"),
		act("window.move.right", Global, "move right", "trades the card with the one to its right", "alt+shift+l", "alt+shift+right"),
		act("window.move.up", Global, "move up", "trades the card with the one above", "alt+shift+k", "alt+shift+up"),
		act("window.move.down", Global, "move down", "trades the card with the one below", "alt+shift+j", "alt+shift+down"),
		act("window.master", Global, "swap master", "trades the card with the master", "alt+m"),
		act("window.zoom", Global, "fullscreen", "fills the stage with the card or gives it back", "alt+f"),
		act("window.float", Global, "float", "floats the card or tiles it again", "alt+v"),
		act("window.close", Global, "close", "closes the focused card", "alt+w"),
		act("window.grow", Global, "grow", "grows the master area", "alt+="),
		act("window.shrink", Global, "shrink", "shrinks the master area", "alt+-"),
		act("layout.reset", Global, "reset layout", "returns every size to its default", "alt+r"),
		act("layout.toggle", Global, "toggle layout", "switches between the dwindle and master arrangements", "alt+d", "alt+shift+space"),
		act("layout.dwindle", Global, "dwindle", "arranges the cards as a spiral of splits"),
		act("layout.master", Global, "master", "arranges the cards as a master beside a stack"),
		act("layout.main.more", Global, "more main", "gives the main area one more card", "alt++"),
		act("layout.main.fewer", Global, "fewer main", "takes one card out of the main area", "alt+_"),
		act("layout.split", Global, "flip split", "flips the split holding the focused card", "alt+s"),
		act("session.open", Global, "open stored", "picks a stored session to resume"),
		act("session.fork", Global, "fork", "forks the session into a new workspace"),
		act("session.fork.at", Global, "fork at turn", "picks the turn a fork starts from"),
		act("session.rename", Global, "rename", "names the session"),
		act("session.compact", Global, "compact", "summarizes the transcript now"),
		act("session.export", Global, "export", "writes the transcript as markdown"),
		act("run.cancel", Global, "cancel run", "stops the run at once"),
		act("run.interrupt", Global, "interrupt run", "stops the run after the current tool"),
		act("mission.start", Global, "start mission", "starts a mission from an objective"),
		act("mission.open", Global, "open mission", "picks a mission card to open"),
		act("answer.allow", Global, "allow", "allows the approval at the head of the queue"),
		act("answer.deny", Global, "deny", "denies the approval at the head of the queue"),
		act("answer.always", Global, "always allow", "allows the approval and remembers the rule"),
		act("answer.never", Global, "never allow", "denies the approval and remembers the rule"),
		act("model.switch", Global, "model", "picks a model from the endpoint"),
		act("profile.switch", Global, "profile", "picks an endpoint profile"),
		act("mode.set", Global, "permission mode", "picks the permission mode"),
		act("theme.set", Global, "theme", "picks the palette"),
		act("motion.set", Global, "motion", "picks how much the cards animate"),
		act("open.agents", Global, "agents", "opens the agent roster"),
		act("open.metrics", Global, "metrics", "opens the metrics"),
		act("open.files", Global, "files", "opens the files touched"),
		act("open.jobs", Global, "jobs", "opens the background jobs"),
		act("open.sessions", Global, "sessions", "opens the stored sessions"),
		act("open.config", Global, "config", "opens the configuration"),
		act("open.findings", Global, "findings", "opens the findings"),
		act("agent.open", Global, "agent transcript", "picks a child agent transcript to open"),

		act("panel.close", Panel, "close", "closes the card", "q"),

		act("composer.send", Session, "send", "sends the prompt, steering a run in progress", "enter"),
		act("composer.complete", Session, "complete", "completes the path or command under the cursor", "tab"),
		quiet(act("composer.up", Session, "up", "moves up a line, or back through history on the first line", "up")),
		quiet(act("composer.down", Session, "down", "moves down a line, or forward through history on the last line", "down")),
		act("composer.attachments.clear", Session, "clear attachments", "drops every attached file", "ctrl+x"),
		quiet(act("composer.left", Session, "left", "moves the cursor left", "left")),
		quiet(act("composer.right", Session, "right", "moves the cursor right", "right")),
		quiet(act("composer.word.left", Session, "word left", "moves the cursor a word left", "ctrl+left")),
		quiet(act("composer.word.right", Session, "word right", "moves the cursor a word right", "ctrl+right")),
		quiet(act("composer.line.start", Session, "line start", "moves the cursor to the start of the line", "home", "ctrl+a")),
		quiet(act("composer.line.end", Session, "line end", "moves the cursor to the end of the line", "end", "ctrl+e")),
		quiet(act("composer.delete.back", Session, "delete back", "deletes the character before the cursor", "backspace")),
		quiet(act("composer.delete.forward", Session, "delete forward", "deletes the character under the cursor", "delete")),
		quiet(act("composer.delete.word.back", Session, "delete word back", "deletes the word before the cursor", "ctrl+w", "ctrl+backspace")),
		quiet(act("composer.delete.word.forward", Session, "delete word forward", "deletes the word after the cursor", "ctrl+delete")),
		quiet(act("composer.delete.line.before", Session, "delete to start", "deletes from the line start to the cursor", "ctrl+u")),
		quiet(act("composer.delete.line.after", Session, "delete to end", "deletes from the cursor to the line end", "ctrl+k")),
		quiet(act("composer.select.all", Session, "select all", "selects the whole prompt", "ctrl+shift+a")),
		quiet(act("composer.select.left", Session, "select left", "extends the selection a character left", "shift+left")),
		quiet(act("composer.select.right", Session, "select right", "extends the selection a character right", "shift+right")),
		quiet(act("composer.select.word.left", Session, "select word left", "extends the selection a word left", "ctrl+shift+left")),
		quiet(act("composer.select.word.right", Session, "select word right", "extends the selection a word right", "ctrl+shift+right")),
		quiet(act("composer.select.up", Session, "select up", "extends the selection a line up", "shift+up")),
		quiet(act("composer.select.down", Session, "select down", "extends the selection a line down", "shift+down")),
		quiet(act("composer.copy", Session, "copy", "copies the selection to the clipboard", "ctrl+shift+c")),
		quiet(act("composer.paste", Session, "paste", "pastes the clipboard at the cursor", "ctrl+v")),
		act("session.search", Session, "search", "searches the transcript", "alt+/"),
		act("session.scroll.up", Session, "page up", "scrolls the transcript up a page", "pgup"),
		act("session.scroll.down", Session, "page down", "scrolls the transcript down a page", "pgdown"),
		act("session.scroll.top", Session, "top", "scrolls the transcript to the start", "ctrl+home"),
		act("session.scroll.end", Session, "follow", "scrolls the transcript to the end and follows new lines", "alt+g", "ctrl+end"),
		act("session.expand", Session, "expand all", "expands every tool result", "alt+e"),
		act("session.collapse", Session, "fold all", "folds every tool result", "alt+shift+e"),
		act("session.thinking", Session, "thinking", "shows or hides thinking", "alt+t"),
		act("mode.cycle", Session, "mode", "moves to the next permission mode", "shift+tab"),

		quiet(act("complete.next", Complete, "next", "moves to the next candidate", "down", "tab", "ctrl+n")),
		quiet(act("complete.prev", Complete, "previous", "moves to the previous candidate", "up", "ctrl+p")),
		act("complete.accept", Complete, "accept", "takes the candidate", "enter"),
		act("complete.close", Complete, "close", "closes the popup", "esc"),

		act("search.accept", Search, "done", "keeps the search and leaves the bar", "enter"),
		act("search.close", Search, "clear", "clears the search and leaves the bar", "esc"),
		quiet(act("search.next", Search, "next", "moves to the next match or row", "down", "ctrl+n")),
		quiet(act("search.prev", Search, "previous", "moves to the previous match or row", "up", "ctrl+p")),
		quiet(act("search.delete.back", Search, "delete", "deletes the character before the cursor", "backspace")),
		quiet(act("search.clear", Search, "clear text", "empties the search", "ctrl+u")),
		quiet(act("search.delete.word", Search, "delete word", "deletes the word before the cursor", "ctrl+w")),

		act("confirm.yes", Confirm, "yes", "answers yes", "y"),
		act("confirm.no", Confirm, "no", "answers no", "n", "esc"),

		act("edit.apply", Edit, "apply", "applies the text", "enter"),
		act("edit.cancel", Edit, "cancel", "drops the edit", "esc"),
		quiet(act("edit.delete.back", Edit, "delete", "deletes the character before the cursor", "backspace")),
		quiet(act("edit.clear", Edit, "clear", "empties the cell", "ctrl+u")),
		quiet(act("edit.end", Edit, "end", "keeps the text and moves the cursor to its end", "right", "end")),

		act("capture.cancel", Capture, "cancel", "stops waiting for a key", "esc"),

		quiet(act("palette.up", Palette, "up", "moves to the row above", "up", "shift+tab")),
		quiet(act("palette.down", Palette, "down", "moves to the row below", "down", "tab", "ctrl+n")),
		quiet(act("palette.page.up", Palette, "page up", "moves eight rows up", "pgup")),
		quiet(act("palette.page.down", Palette, "page down", "moves eight rows down", "pgdown")),
		act("palette.pick", Palette, "run", "runs the row or hands the text over", "enter"),
		act("palette.close", Palette, "close", "closes the palette", "esc"),
		quiet(act("palette.delete.back", Palette, "delete", "deletes the character before the cursor", "backspace")),
		quiet(act("palette.delete.forward", Palette, "delete forward", "deletes the character under the cursor", "delete")),
		quiet(act("palette.left", Palette, "left", "moves the cursor left", "left")),
		quiet(act("palette.right", Palette, "right", "moves the cursor right", "right")),
		quiet(act("palette.home", Palette, "start", "moves the cursor to the start", "home", "ctrl+a")),
		quiet(act("palette.end", Palette, "end", "moves the cursor to the end", "end", "ctrl+e")),
		quiet(act("palette.clear", Palette, "clear", "empties the query", "ctrl+u")),
		quiet(act("palette.delete.word", Palette, "delete word", "deletes the word before the cursor", "ctrl+w")),

		act("tabs.next", Tabs, "next tab", "shows the next tab", "tab"),
		act("tabs.prev", Tabs, "previous tab", "shows the previous tab", "shift+tab"),

		quiet(act("list.up", List, "up", "moves to the row above", "k", "up")),
		quiet(act("list.down", List, "down", "moves to the row below", "j", "down")),
		quiet(act("list.top", List, "first", "moves to the first row", "g", "home")),
		quiet(act("list.bottom", List, "last", "moves to the last row", "G", "end")),
		quiet(act("list.page.up", List, "page up", "moves a page of rows up", "pgup")),
		quiet(act("list.page.down", List, "page down", "moves a page of rows down", "pgdown")),

		act("filter.toggle", Filter, "filter", "opens a filter bar under the rows, or closes it and shows every row", "/"),

		act("detail.scroll.down", Detail, "detail down", "scrolls the detail pane down a page", "pgdown"),
		act("detail.scroll.up", Detail, "detail up", "scrolls the detail pane up a page", "pgup"),

		act("sessions.resume", Sessions, "resume", "opens the selected sessions in workspaces", "enter"),
		act("sessions.fork", Sessions, "fork", "forks the selected sessions into new workspaces", "f"),
		act("sessions.delete", Sessions, "delete", "deletes the selected sessions after asking", "d"),
		act("sessions.refresh", Sessions, "refresh", "reads the stored sessions again", "r"),
		quiet(act("sessions.select.up", Sessions, "select up", "extends the selection to the row above", "shift+up", "K")),
		quiet(act("sessions.select.down", Sessions, "select down", "extends the selection to the row below", "shift+down", "J")),

		act("agents.open", Agents, "transcript", "opens the transcript of the run", "enter"),
		act("agents.stop", Agents, "stop", "stops the run", "x"),

		act("files.diff", Files, "diff", "shows or hides the diff of the file", "enter"),
		act("files.whole", Files, "since start", "diffs against the file as the session found it", "a"),
		act("files.scroll.up", Files, "diff up", "scrolls the diff up a page", "pgup"),
		act("files.scroll.down", Files, "diff down", "scrolls the diff down a page", "pgdown"),

		act("findings.detail", Findings, "detail", "shows or hides the body and evidence", "enter"),
		act("findings.resolve", Findings, "resolve", "marks the fact resolved or open again", "x"),
		act("findings.tree", Findings, "tree", "switches between the table and the tree", "t"),
		act("findings.kind", Findings, "kind", "cycles the kind filter", "c"),
		act("findings.tag", Findings, "tag", "cycles the tag filter", "T"),
		act("findings.status", Findings, "status", "cycles the status filter", "s"),
		act("findings.export.md", Findings, "export md", "writes every fact as markdown", "e"),
		act("findings.export.json", Findings, "export json", "writes every fact as json", "E"),

		act("jobs.stop", Jobs, "stop", "stops the job", "x"),
		act("jobs.follow", Jobs, "follow", "follows new output or holds the tail still", "f"),
		act("jobs.scroll.up", Jobs, "tail up", "scrolls the tail up a page", "pgup"),
		act("jobs.scroll.down", Jobs, "tail down", "scrolls the tail down a page", "pgdown"),

		quiet(act("transcript.down", Transcript, "down", "scrolls down a line", "j", "down")),
		quiet(act("transcript.up", Transcript, "up", "scrolls up a line", "k", "up")),
		act("transcript.page.down", Transcript, "page down", "scrolls down a page", "pgdown", "ctrl+d", "space"),
		act("transcript.page.up", Transcript, "page up", "scrolls up a page", "pgup", "ctrl+u"),
		quiet(act("transcript.top", Transcript, "top", "scrolls to the start", "g", "home")),
		quiet(act("transcript.bottom", Transcript, "end", "scrolls to the end", "G", "end")),
		act("transcript.search", Transcript, "search", "searches the transcript", "/"),
		act("transcript.next", Transcript, "next match", "moves to the next match", "n"),
		act("transcript.prev", Transcript, "previous match", "moves to the previous match", "N"),
		act("transcript.turn.prev", Transcript, "previous turn", "jumps to the previous turn", "["),
		act("transcript.turn.next", Transcript, "next turn", "jumps to the next turn", "]"),
		quiet(act("transcript.block.next", Transcript, "next block", "moves the cursor to the next block", ".")),
		quiet(act("transcript.block.prev", Transcript, "previous block", "moves the cursor to the previous block", ",")),
		act("transcript.toggle", Transcript, "expand", "expands or folds the block under the cursor", "enter", "o"),
		act("transcript.expand", Transcript, "expand all", "expands every tool result", "e"),
		act("transcript.collapse", Transcript, "fold all", "folds every tool result", "E"),
		act("transcript.thinking", Transcript, "thinking", "shows or hides thinking", "T"),
		act("transcript.follow", Transcript, "follow", "follows new lines or holds still", "f"),

		act("approval.allow", Approval, "allow", "allows the request", "y"),
		act("approval.deny", Approval, "deny", "denies the request", "n"),
		act("approval.always", Approval, "always", "allows the request and remembers the rule", "a"),
		act("approval.never", Approval, "never", "denies the request and remembers the rule", "v"),
		quiet(act("approval.down", Approval, "down", "scrolls down a line", "j", "down")),
		quiet(act("approval.up", Approval, "up", "scrolls up a line", "k", "up")),
		quiet(act("approval.page.down", Approval, "page down", "scrolls down a page", "pgdown")),
		quiet(act("approval.page.up", Approval, "page up", "scrolls up a page", "pgup")),
		quiet(act("approval.top", Approval, "top", "scrolls to the top", "g", "home")),

		act("mission.done", Mission, "done", "finishes the mission as done", "d"),
		act("mission.failed", Mission, "failed", "finishes the mission as failed", "x"),
		act("mission.resume", Mission, "resume", "resumes an ended mission", "r"),
		act("mission.forget", Mission, "forget", "forgets an ended mission", "D"),

		act("form.next", Form, "next field", "moves to the next field", "tab", "down"),
		act("form.prev", Form, "previous field", "moves to the previous field", "shift+tab", "up"),
		quiet(act("form.left", Form, "left", "picks the choice to the left, lowers a number or moves the cursor left", "left")),
		quiet(act("form.right", Form, "right", "picks the choice to the right, raises a number or moves the cursor right", "right")),
		act("form.submit", Form, "save", "saves the dialog", "enter"),
		act("form.cancel", Form, "cancel", "closes the dialog without saving", "esc"),
		quiet(act("form.delete.back", Form, "delete", "deletes the character before the cursor", "backspace")),
		quiet(act("form.delete.word", Form, "delete word", "deletes the word before the cursor", "ctrl+w")),
		quiet(act("form.clear", Form, "clear", "empties the field", "ctrl+u")),
		quiet(act("form.unset", Form, "unset", "leaves an optional field unset", "delete")),
		quiet(act("form.home", Form, "start", "moves the cursor to the start of the field", "home", "ctrl+a")),
		quiet(act("form.end", Form, "end", "moves the cursor to the end of the field", "end", "ctrl+e")),

		act("config.profile.use", Profiles, "use", "switches to the profile", "enter"),
		act("config.profile.add", Profiles, "new", "adds a profile from a provider", "a"),
		act("config.profile.edit", Profiles, "edit", "edits the profile", "e"),
		act("config.profile.delete", Profiles, "delete", "deletes the profile after asking", "d"),
		act("config.profile.toggle", Profiles, "on/off", "turns the profile on or off, an off profile left out of pickers", "space"),
		act("config.profile.up", Profiles, "move up", "moves the profile up the list", "K", "shift+up"),
		act("config.profile.down", Profiles, "move down", "moves the profile down the list", "J", "shift+down"),
		act("config.profile.detail", Profiles, "detail", "shows or hides the profile details", "o"),
		act("config.profile.recheck", Profiles, "recheck", "checks every credential again", "r"),
		act("config.provider.new", Providers, "new profile", "adds a profile starting from the provider", "enter", "a"),
		act("config.provider.use", Providers, "use as is", "switches to the provider with its own credential source", "u"),
		act("config.provider.detail", Providers, "detail", "shows or hides the provider details", "o"),
		act("config.model.use", Models, "use", "switches to the model", "enter"),
		act("config.model.detail", Models, "detail", "shows or hides the model details", "o"),
		act("config.model.refresh", Models, "refresh", "fetches the model list from the endpoint again", "r"),
		act("config.app.left", App, "less", "picks the choice to the left or lowers the number", "left", "h"),
		act("config.app.right", App, "more", "picks the choice to the right or raises the number", "right", "l"),
		act("config.app.edit", App, "type", "types a number in", "enter"),
		act("config.app.unset", App, "unset", "unsets the setting so its default applies", "delete", "backspace"),
		act("config.rule.add", Rules, "new", "adds a rule", "a"),
		act("config.rule.edit", Rules, "edit", "edits the rule", "enter", "e"),
		act("config.rule.delete", Rules, "delete", "deletes the rule after asking", "d"),
		act("config.hook.add", Hooks, "new", "adds a hook", "a"),
		act("config.hook.edit", Hooks, "edit", "edits the hook", "enter", "e"),
		act("config.hook.delete", Hooks, "delete", "deletes the hook after asking", "d"),
		act("config.mcp.detail", MCP, "detail", "shows or hides the server details", "enter", "o"),
		act("config.mcp.add", MCP, "new", "adds a server", "a"),
		act("config.mcp.edit", MCP, "edit", "edits the server", "e"),
		act("config.mcp.delete", MCP, "delete", "deletes the server after asking", "d"),
		act("config.mcp.toggle", MCP, "on/off", "turns the server on or off", "space"),
		act("config.mcp.reconnect", MCP, "reconnect", "reconnects the server", "R"),
		act("config.tool.toggle", Tools, "on/off", "turns the tool on or off for the model", "space"),
		act("config.tool.detail", Tools, "detail", "shows or hides the tool schema", "enter", "o"),
		act("config.log.detail", Log, "detail", "shows or hides the whole line", "enter", "o"),
		act("config.log.clear", Log, "clear", "drops every line", "c"),
		act("config.key.rebind", Keybinds, "rebind", "waits for a key and binds the action to it alone", "enter"),
		act("config.key.add", Keybinds, "add key", "waits for a key and adds it to the action", "a"),
		act("config.key.type", Keybinds, "type keys", "types the keys of the action by name", "t"),
		act("config.key.unbind", Keybinds, "unbind", "leaves the action without keys", "d"),
		act("config.key.reset", Keybinds, "reset", "returns the action to its default keys", "r"),
		act("config.key.detail", Keybinds, "detail", "shows or hides the action details", "o"),
	)
	return out
}()
