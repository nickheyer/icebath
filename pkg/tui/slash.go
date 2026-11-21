package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/tuigateway"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Attachments and token expansion a typed command carries into the prompt it sends
type slashSubmission struct {
	attachments []imageAttachment
	expand      func(string) string
}

// One run of a slash command, fenced to the session and command flight it started in
type slashRun struct {
	a           *app
	name        string
	arg         string
	cmd         string
	sid         string
	flight      int
	destination submissionDestination
	typed       bool
	observed    bool
	submission  *slashSubmission
}

// Command the terminal UI answers itself
type slashCommand struct {
	name    string
	aliases []string
	help    string
	// Help line worded at the moment it is shown, for a line built from parts or kept under a catalog key of its own
	helpFn func() string
	usage  string
	// Values the argument completes to
	suggest func(prefix string) []string
	// Values a later argument completes to after the finished words before it
	suggestAfter func(done []string) []string
	run          func(r *slashRun) tea.Cmd
}

// Help line of the command in the active language, the registered line when the packs hold none
func (c slashCommand) helpText() string {
	if c.helpFn != nil {
		return c.helpFn()
	}
	return trOr("slashHelp."+c.name, c.help)
}

// Help line that reads a catalog key each time it is shown, so it follows a language switch
func helpOf(key txt.Key) func() string { return func() string { return tr(key) } }

// Whether a newer command or another session replaced this run
func (r *slashRun) stale() bool {
	return r.flight != r.a.slashFlight || !r.a.store.isCurrentDestination(r.destination)
}

// Gateway request of a command, its answer dropped once the run went stale and its failure printed
func (r *slashRun) rpc(method string, params map[string]any, then func(map[string]any) tea.Cmd) tea.Cmd {
	return gatewayCall(r.a.gw, method, params, func(v any, err error) tea.Cmd {
		if r.stale() {
			return nil
		}
		if err != nil {
			r.fail(err)
			return nil
		}
		result, isObject := v.(map[string]any)
		if !isObject {
			r.a.sys(errorLine(misfitText(tr(txt.SessionCommonInvalidResponse, method))))
			return nil
		}
		if then == nil {
			return nil
		}
		return then(result)
	})
}

// Prints the failure of a command request in the user's words
func (r *slashRun) fail(err error) {
	if isUnsupportedCommand(err) {
		r.a.sys(r.a.slashRefusal(r.name, err))
		return
	}
	r.a.sys(errorLine(refusalDetail(err)))
}

// Tells a canonical gateway once that a typed command ran, a gateway refusing the verb never asked again
func (r *slashRun) ran() tea.Cmd {
	if !r.typed || r.observed || r.sid == "" || !r.a.gw.canonical() || r.a.observeUnsupported {
		return nil
	}
	r.observed = true
	return call(r.a.gw, tuigateway.CommandObserve, tuigateway.CommandObserveParams{Arg: r.arg, Name: r.name, SessionID: r.sid}, func(_ *tuigateway.CommandObserveResult, err error) tea.Cmd {
		var refusal *rpcError
		if err != nil && ((errors.As(err, &refusal) && refusal.HasCode && refusal.Code == jsonRPCMethodNotFound) || strings.Contains(rpcErrorMessage(err), "unknown_method")) {
			r.a.observeUnsupported = true
		}
		return nil
	})
}

func (r *slashRun) sys(text string) { r.a.sys(text) }

// Local command of a name or alias
func (a *app) findSlashCommand(name string) *slashCommand {
	name = strings.ToLower(name)
	for i := range a.localCommands {
		c := &a.localCommands[i]
		if c.name == name || slices.Contains(c.aliases, name) {
			return c
		}
	}
	return nil
}

// Long output goes to the pager and short output into the transcript
func (a *app) showOutput(text, name string) tea.Cmd {
	lines := 0
	for _, line := range strings.Split(text, "\n") {
		if line != "" {
			lines++
		}
	}
	if utf8.RuneCountInString(text) > 180 || lines > 2 {
		title := name
		if title != "" {
			title = strings.ToUpper(title[:1]) + title[1:]
		}
		return a.page(text, title)
	}
	a.sys(text)
	return nil
}

// Runs a slash command, a local one here, a widget, a catalog alias, or the gateway's own command
func (a *app) slash(cmd string, submission *slashSubmission, typed bool) tea.Cmd {
	a.slashFlight++
	destination, err := a.store.captureDestination()
	if err != nil {
		a.uncaught(err)
		return nil
	}
	name, arg := parseSlashCommand(cmd)
	run := &slashRun{a: a, name: name, arg: arg, cmd: cmd, sid: a.store.ui.Sid, flight: a.slashFlight, destination: destination, typed: typed, submission: submission}
	argTail := ""
	if arg != "" {
		argTail = " " + arg
	}
	if line, absent := linktier.Refusal("/"+strings.ToLower(name), linktier.SlashTiers[strings.ToLower(name)]); absent {
		a.sys(line)
		return nil
	}
	if found := a.findSlashCommand(name); found != nil {
		return found.run(run)
	}
	if a.widgets.has(name) {
		return a.widgets.launch(name, arg)
	}
	if a.catalog != nil && len(a.catalog.Canon) > 0 {
		needle := strings.ToLower("/" + name)
		exact := ""
		for alias, canon := range a.catalog.Canon {
			if strings.ToLower(alias) == needle {
				exact = canon
			}
		}
		if exact != "" {
			if strings.ToLower(exact) != needle {
				return a.slash(exact+argTail, submission, typed)
			}
		} else {
			best := 3.0
			matches := map[string]bool{}
			for alias, canon := range a.catalog.Canon {
				score := scoreSlashMenuItem(slashScoreItem{ID: strings.TrimPrefix(alias, "/")}, needle[1:])
				switch {
				case score >= 3 || score > best:
				case score < best:
					best, matches = score, map[string]bool{canon: true}
				default:
					matches[canon] = true
				}
			}
			names := make([]string, 0, len(matches))
			for canon := range matches {
				names = append(names, canon)
			}
			sort.Strings(names)
			if len(names) == 1 && strings.ToLower(names[0]) != needle {
				return a.slash(names[0]+argTail, submission, typed)
			}
			if len(names) > 1 {
				shown := strings.Join(names[:min(len(names), 6)], ", ")
				if len(names) > 6 {
					shown += ", …"
				}
				a.sys(tr(txt.SlashCmdCoreAmbiguousCommand, shown))
				return nil
			}
		}
	}
	exec := call(a.gw, tuigateway.SlashExec, tuigateway.SlashExecParams{Command: strings.TrimPrefix(cmd, "/"), SessionID: run.sid}, func(r *tuigateway.SlashExecResult, err error) tea.Cmd {
		if run.stale() {
			return nil
		}
		if err == nil {
			if directive, isDirective := r.Directive(); isDirective {
				return a.handleDispatch(run, directive, true)
			}
			body := r.Output
			if body == "" {
				body = tr(txt.SlashCmdCoreNoOutputFor, name)
			}
			return a.showOutput(body, name)
		}
		if isUnsupportedCommand(err) {
			a.sys(a.slashRefusal(name, err))
			return nil
		}
		if !shouldFallbackToDispatch(err) {
			a.sys(errorLine(describeSlashExecError(name, err)))
			return nil
		}
		return call(a.gw, tuigateway.CommandDispatch, tuigateway.CommandDispatchParams{Arg: arg, Name: name, SessionID: run.sid}, func(d *tuigateway.CommandDispatchResult, err error) tea.Cmd {
			if run.stale() {
				return nil
			}
			if err != nil {
				run.fail(err)
				return nil
			}
			return a.handleDispatch(run, *d, false)
		})
	})
	return exec
}

// Carries out what the gateway decided a command is, output, an alias, a skill or message to send, or a prefill
func (a *app) handleDispatch(run *slashRun, d tuigateway.CommandDispatchResult, fromExec bool) tea.Cmd {
	if d.Type == "" || d.Type == "alias" && d.Target == "" || d.Type == "skill" && d.Name == "" {
		a.sys(errorLine(misfitText(tr(txt.SessionCommonInvalidResponse, tuigateway.CommandDispatch.Name()))))
		return nil
	}
	sendDispatch := func(display, message string) tea.Cmd {
		shown := trimJS(display)
		if run.submission != nil {
			a.expandWith = run.submission.expand
			defer func() { a.expandWith = nil }()
			return a.send(message, true, shown, submitOpts{Attachments: run.submission.attachments})
		}
		return a.sendExpanded(message, true, shown, submitOpts{})
	}
	switch d.Type {
	case "exec", "plugin":
		if out := d.Output; fromExec && out != "" {
			return a.showOutput(out, run.name)
		} else if out != "" {
			a.sys(out)
		} else {
			a.sys(tr(txt.SlashCmdCoreNoOutput))
		}
	case "alias":
		argTail := ""
		if run.arg != "" {
			argTail = " " + run.arg
		}
		return a.slash("/"+d.Target+argTail, run.submission, false)
	case "skill":
		if trimJS(d.Message) == "" {
			a.sys(tr(txt.SlashCmdCoreSkillNoMessage, run.name))
			return nil
		}
		return sendDispatch(d.Display, d.Message)
	case "send":
		if notice := trimJS(d.Notice); notice != "" {
			a.sys(d.Notice)
		}
		if trimJS(d.Message) == "" {
			a.sys(tr(txt.SlashCmdCoreEmptyMessage, run.name))
			return nil
		}
		return sendDispatch(d.Display, d.Message)
	case "prefill":
		if notice := trimJS(d.Notice); notice != "" {
			a.sys(d.Notice)
		}
		if message := d.Message; message != "" {
			return a.card.composer.SetValue(message)
		}
	}
	return nil
}

// Local commands as palette entries, so every one is reachable without typing its name
func (a *app) slashPaletteCommands() []wm.Command {
	var out []wm.Command
	for _, c := range a.localCommands {
		if c.usage != "" && !strings.HasPrefix(c.usage, "[") {
			continue
		}
		name := c.name
		out = append(out, wm.Command{ID: "slash." + name, Title: "/" + name + "  " + c.help, Text: func() string { return "/" + name + "  " + c.helpText() }, When: func() bool { return !a.dialogs.waiting() },
			Run: func() tea.Cmd { return a.slash("/"+name, nil, false) }})
	}
	return out
}

// Actions whose key help row is one catalog text, the chords of a group joined in one row
var hotkeyGroups = []struct {
	ids []string
	key txt.Key
}{
	{[]string{"interrupt"}, txt.HintsCtrlC},
	{[]string{"composer.paste"}, txt.HintsPaste},
	{[]string{"complete.accept"}, txt.HintsApplyCompletion},
	{[]string{"composer.up", "composer.down"}, txt.HintsArrows},
	{[]string{"composer.undo", "composer.redo"}, txt.HintsUndoRedo},
	{[]string{"tui.suspend"}, txt.HintsSuspend},
	{[]string{"composer.word.left", "composer.word.right"}, txt.HintsJumpWord},
	{[]string{"composer.line.start", "composer.line.end"}, txt.HintsLineStartEnd},
	{[]string{"composer.copy"}, txt.HintsCopySelection},
	{[]string{"tui.paste.expand"}, txt.HintsPasteExpand},
}

// Actions the key help opens with, the keys people reach for first and the ones the quick help card shows
var hotkeyLead = []string{"interrupt", "tui.exit", "tui.editor", "tui.redraw", "composer.paste", "session.stop", "complete.accept", "composer.up"}

// Chords each key of the session window answers, spelled for a legend: a chord a key of an earlier context takes in every state is left out of
// the later keys it hides, while a key that acts only in some state, such as dropping a queued input, hides nothing
func (a *app) sessionChords() map[string][]string {
	conditional := map[string]bool{}
	for _, c := range a.commands() {
		conditional[c.ID] = c.When != nil
	}
	out, taken := map[string][]string{}, map[string]bool{}
	for _, action := range a.keys.Actions() {
		if action.Context == keys.Complete {
			for _, chord := range a.keys.Sendable(action.ID) {
				out[action.ID] = append(out[action.ID], keys.Format(chord))
			}
		}
	}
	for _, action := range a.keys.InContexts([]string{tuiContext, keys.Session}) {
		for _, chord := range a.keys.Sendable(action.ID) {
			if taken[chord] {
				continue
			}
			out[action.ID] = append(out[action.ID], keys.Format(chord))
			taken[chord] = !conditional[action.ID]
		}
	}
	return out
}

// Hotkeys of the session as the keymap binds them now, the most used first
func (a *app) hotkeyRows() [][2]string {
	chords := a.sessionChords()
	spelled := func(id string) string { return strings.Join(chords[id], " ") }
	group := func(id string) int {
		return slices.IndexFunc(hotkeyGroups, func(g struct {
			ids []string
			key txt.Key
		}) bool {
			return slices.Contains(g.ids, id)
		})
	}
	var rows [][2]string
	listed := map[string]bool{}
	add := func(id string) {
		if listed[id] {
			return
		}
		listed[id] = true
		if g := group(id); g >= 0 {
			var parts []string
			for _, member := range hotkeyGroups[g].ids {
				listed[member] = true
				if chord := spelled(member); chord != "" {
					parts = append(parts, chord)
				}
			}
			if len(parts) > 0 {
				rows = append(rows, [2]string{strings.Join(parts, " / "), tr(hotkeyGroups[g].key)})
			}
			return
		}
		if action, known := a.keys.Action(id); known && spelled(id) != "" && action.Doc != "" {
			rows = append(rows, [2]string{spelled(id), action.DocText()})
		}
	}
	for _, id := range hotkeyLead {
		if id != "session.stop" {
			add(id)
		} else if chord := spelled(id); chord != "" {
			rows = append(rows, [2]string{chord + " " + chord, tr(txt.HintsDiscardDraft)}, [2]string{chord + " " + chord, tr(txt.HintsRewindPicker)})
		}
	}
	commands := map[string]bool{"palette": true, "quit": true}
	for _, action := range a.keys.InContexts([]string{tuiContext, keys.Session}) {
		switch {
		case action.Context == keys.Global && !commands[action.ID]:
		case action.Context == tuiContext, !action.Quiet, group(action.ID) >= 0, action.ID == "session.stop", strings.HasPrefix(action.ID, "composer.delete"):
			add(action.ID)
		}
	}
	return append(rows, [2]string{`\ + enter`, tr(txt.HintsContinuation)}, [2]string{"!<cmd>", tr(txt.HintsShell)}, [2]string{"{!<cmd>}", tr(txt.HintsInterpolate)})
}

// Whether destructive commands skip their confirmation
func noConfirmDestructive() bool {
	return trueRe.MatchString(strings.ToLower(trimJS(os.Getenv("ICEBATH_TUI_NO_CONFIRM"))))
}

// Reads on, off or toggle against the current state, false for anything else
func flagFromArg(arg string, current bool) (bool, bool) {
	switch strings.ToLower(trimJS(arg)) {
	case "", "toggle":
		return !current, true
	case "on":
		return true, true
	case "off":
		return false, true
	}
	return false, false
}

// Saves a display preference, session scoped for the focus view on the canonical wire
func (r *slashRun) saveDisplayPref(key, value string) tea.Cmd {
	params := tuigateway.ConfigSetParams{Key: key, Value: tuigateway.TextValue(value)}
	if key == "focus" && r.a.gw.canonical() && r.sid != "" {
		params.SessionID = r.sid
	}
	return fire(r.a.gw, tuigateway.ConfigSet, params)
}

// Asks before a destructive step unless confirmations are off
func (a *app) confirmDestructive(title, detail, yes, no string, commit func() tea.Cmd) tea.Cmd {
	if noConfirmDestructive() || !a.store.ui.DestructiveSlashConfirm {
		return commit()
	}
	a.store.overlay.Confirm = &confirmReq{CancelLabel: no, ConfirmLabel: yes, Danger: true, Detail: detail, OnConfirm: commit, Title: title}
	return nil
}

var mouseModeAliases = map[string]string{"all": "all", "any": "all", "button": "buttons", "buttons": "buttons", "click": "buttons", "full": "all", "off": "off", "on": "all", "scroll": "wheel", "wheel": "wheel"}

// Core commands, help, exit, the session reset and the display switches
func (a *app) coreCommands() []slashCommand {
	return []slashCommand{
		{name: "help", help: "list commands + hotkeys", run: func(r *slashRun) tea.Cmd {
			var sections []panelSection
			if a.catalog != nil {
				for _, cat := range a.catalog.Categories {
					title := cat.Name
					sections = append(sections, panelSection{Rows: cat.Pairs, Title: &title})
				}
				if a.catalog.SkillCount > 0 {
					text := tr(txt.SlashCmdCoreHelpSkillCommandsAvailable, a.catalog.SkillCount)
					sections = append(sections, panelSection{Text: &text})
				}
			}
			var local [][2]string
			for _, c := range a.localCommands {
				label := "/" + c.name
				if c.usage != "" {
					label += " " + c.usage
				}
				local = append(local, [2]string{label, c.helpText()})
			}
			for _, w := range a.widgets.list() {
				local = append(local, [2]string{"/" + w.ID, w.Help})
			}
			tui, hot := tr(txt.SlashCmdCoreHelpTuiSection), tr(txt.HelpHotkeys)
			sections = append(sections, panelSection{Rows: local, Title: &tui}, panelSection{Rows: a.hotkeyRows(), Title: &hot})
			header := a.store.ui.Theme.Brand.HelpHeader
			if header == "" {
				header = "Commands"
			}
			a.panel(header, sections)
			return r.ran()
		}},
		{name: "quit", aliases: []string{"exit"}, help: "exit icebath", run: func(r *slashRun) tea.Cmd {
			return a.die(0)
		}},
		{name: "update", help: "update icebath to the latest version, leaving the terminal UI", run: func(r *slashRun) tea.Cmd {
			r.sys(tr(txt.SlashCmdCoreUpdateExiting))
			return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return dieMsg{code: exitForUpdate} })
		}},
		{name: "mouse", aliases: []string{"scroll"}, help: "set mouse tracking", usage: "[on|off|toggle|wheel|buttons|all]",
			suggest: func(string) []string { return []string{"on", "off", "toggle", "wheel", "buttons", "all"} },
			run: func(r *slashRun) tea.Cmd {
				current := a.store.ui.MouseTracking
				mode := strings.ToLower(trimJS(r.arg))
				next, known := mouseModeAliases[mode]
				if mode == "" || mode == "toggle" {
					next, known = "off", true
					if current == "off" {
						next = "all"
					}
				}
				if !known {
					r.sys(tr(txt.SlashCmdCoreMouseUsage))
					return nil
				}
				a.store.ui.MouseTracking = next
				r.sys(tr(txt.SlashCmdCoreMouseTracking, next))
				return r.saveDisplayPref("mouse", next)
			}},
		{name: "clear", aliases: []string{"new"}, help: "start a new session", usage: "[title]", run: func(r *slashRun) tea.Cmd {
			isNew := strings.HasPrefix(r.cmd, "/new")
			if isNew && a.store.ui.Busy {
				return tea.Batch(r.ran(), a.life.newLiveSession(tr(txt.SlashCmdCoreClearPreviousKeptRunning), trimJS(r.arg), nil))
			}
			if a.life.guardBusySessionSwitch(tr(txt.SlashCmdCoreClearSwitchSessions)) {
				return nil
			}
			a.store.ui.Status = tr(txt.SlashCmdCoreClearForgingSession)
			if !isNew {
				return tea.Batch(r.ran(), a.life.clearSession(nil))
			}
			return tea.Batch(r.ran(), a.life.newSession(tr(txt.SlashCmdCoreClearNewSessionStarted), trimJS(r.arg), nil))
		}},
		{name: "redraw", help: "force a full UI repaint", run: func(r *slashRun) tea.Cmd {
			a.card.view.invalidate()
			r.sys(tr(txt.SlashCmdCoreRedrawDone))
			return a.store.clearScreen()
		}},
		{name: "status", help: "show live session info", run: func(r *slashRun) tea.Cmd {
			if r.sid == "" {
				r.sys(tr(txt.SlashCmdCoreStatusNoActiveSession))
				return nil
			}
			show := func(out string) tea.Cmd {
				if out == "" {
					out = tr(txt.SlashCmdCoreStatusEmpty)
				}
				return a.page(out, tr(txt.SlashCmdCoreStatusPageTitle))
			}
			if a.gw.canonical() {
				return ask(r.rpc, r.sys, tuigateway.SlashExec, tuigateway.SlashExecParams{Command: "status", SessionID: r.sid}, func(res *tuigateway.SlashExecResult) tea.Cmd {
					if res == nil {
						return nil
					}
					return show(res.Output)
				})
			}
			return ask(r.rpc, a.sys, tuigateway.SessionStatus, tuigateway.SessionStatusParams{SessionID: r.sid}, func(res *tuigateway.SessionStatusResult) tea.Cmd {
				if res == nil {
					return nil
				}
				return show(res.Output)
			})
		}},
		{name: "density", help: "toggle compact display", usage: "[on|off|toggle]", run: func(r *slashRun) tea.Cmd {
			next, ok := flagFromArg(r.arg, a.store.ui.Compact)
			if !ok {
				r.sys(tr(txt.SlashCmdCoreDensityUsage))
				return nil
			}
			a.store.ui.Compact = next
			r.sys(tr(txt.SlashCmdCoreDensityState, onOff(next)))
			return r.saveDisplayPref("density", onOff(next))
		}},
		{name: "timestamps", aliases: []string{"ts"}, help: "toggle [HH:MM] timestamps on transcript messages", usage: "[on|off|status]", run: func(r *slashRun) tea.Cmd {
			mode := strings.ToLower(trimJS(r.arg))
			current := a.store.ui.Timestamps
			if mode == "status" {
				r.sys("timestamps " + onOff(current))
				return nil
			}
			next, ok := flagFromArg(mode, current)
			if !ok {
				r.sys("usage: /timestamps [on|off|status]")
				return nil
			}
			a.store.ui.Timestamps = next
			r.sys("timestamps " + onOff(next))
			return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "timestamps", Value: tuigateway.TextValue(onOff(next))}, done)
		}},
		{name: "config", help: "show the presentation settings the gateway serves for this profile", run: func(r *slashRun) tea.Cmd {
			if !a.gw.canonical() {
				return ask(r.rpc, r.sys, tuigateway.SlashExec, tuigateway.SlashExecParams{Command: strings.TrimPrefix(r.cmd, "/"), SessionID: r.sid}, func(res *tuigateway.SlashExecResult) tea.Cmd {
					if res == nil {
						return nil
					}
					out := res.Output
					if out == "" {
						out = "{}"
					}
					return a.page(out, "Config")
				})
			}
			return ask(r.rpc, r.sys, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "full", SessionID: r.sid}, func(res *tuigateway.ConfigGetResult) tea.Cmd {
				var cfg map[string]any
				if res != nil && res.Config != nil {
					cfg, _ = tuigateway.Plain(*res.Config).(map[string]any)
				}
				lines := configLines(cfg, "")
				if len(lines) == 0 {
					lines = []string{"{}"}
				}
				return a.page(strings.Join(lines, "\n"), "Config")
			})
		}},
	}
}

// Word for a switch state
func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// Config as indented key and value lines, keys in order
func configLines(value map[string]any, indent string) []string {
	names := make([]string, 0, len(value))
	for k := range value {
		names = append(names, k)
	}
	sort.Strings(names)
	var lines []string
	for _, k := range names {
		if nested, isMap := value[k].(map[string]any); isMap {
			lines = append(lines, indent+k+":")
			lines = append(lines, configLines(nested, indent+"  ")...)
			continue
		}
		lines = append(lines, indent+k+": "+configValueText(value[k]))
	}
	return lines
}

// Text of one config value, text as it is and every other value as its JSON
func configValueText(v any) string {
	if text, isText := v.(string); isText {
		return text
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(encoded)
}

// Asks the app to quit with a status after a command printed its last line
type dieMsg struct{ code int }

// Refusal of a command the gateway does not run, a name no catalog knows told apart with the closest known names
func (a *app) slashRefusal(name string, err error) string {
	if a.catalog != nil {
		if desc, chat := a.catalog.Chat[strings.ToLower(name)]; chat {
			return tr(txt.SlashChatCommand, name, desc)
		}
	}
	known := a.knownCommandNames()
	if slices.Contains(known, strings.ToLower(name)) {
		return describeSlashExecError(name, err)
	}
	if near := closestCommands(strings.ToLower(name), known); len(near) > 0 {
		return tr(txt.SlashUnknownCommand, name, "/"+strings.Join(near, ", /"))
	}
	return tr(txt.SlashUnknownCommandNoMatch, name)
}

// Names of every command the app runs itself or the gateway catalog offers, aliases included, without the slash
func (a *app) knownCommandNames() []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "/"))
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, c := range a.localCommands {
		add(c.name)
	}
	if a.catalog != nil {
		for _, pair := range a.catalog.Pairs {
			if fields := strings.Fields(pair[0]); len(fields) > 0 {
				add(fields[0])
			}
		}
		for alias, canon := range a.catalog.Canon {
			add(alias)
			add(canon)
		}
	}
	return out
}

// Up to three known names nearest a mistyped one, by edit distance with a prefix match counted close
func closestCommands(name string, known []string) []string {
	type scored struct {
		name string
		dist int
	}
	limit := max(2, len([]rune(name))/3)
	var hits []scored
	for _, k := range known {
		d := editDistance(name, k)
		if strings.HasPrefix(k, name) {
			d = min(d, 1)
		}
		if d <= limit {
			hits = append(hits, scored{k, d})
		}
	}
	slices.SortStableFunc(hits, func(x, y scored) int {
		if x.dist != y.dist {
			return x.dist - y.dist
		}
		return strings.Compare(x.name, y.name)
	})
	out := []string{}
	for _, h := range hits[:min(len(hits), 3)] {
		out = append(out, h.name)
	}
	return out
}

// Edits that turn one name into the other, an adjacent swap counted as one
func editDistance(a, b string) int {
	x, y := []rune(a), []rune(b)
	prev2 := make([]int, len(y)+1)
	prev := make([]int, len(y)+1)
	cur := make([]int, len(y)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(x); i++ {
		cur[0] = i
		for j := 1; j <= len(y); j++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && x[i-1] == y[j-2] && x[i-2] == y[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(y)]
}
