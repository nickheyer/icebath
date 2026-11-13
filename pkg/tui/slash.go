package tui

import (
	"os"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
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
	submission  *slashSubmission
}

// Command the terminal UI answers itself
type slashCommand struct {
	name    string
	aliases []string
	help    string
	usage   string
	// Values the argument completes to
	suggest func(prefix string) []string
	run     func(r *slashRun) tea.Cmd
}

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
		result := asRPCResult(v)
		if result == nil {
			r.a.sys("error: " + tr("session.common.invalidResponse", method))
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
		r.a.sys(describeSlashExecError(r.name, err))
		return
	}
	r.a.sys("error: " + rpcErrorMessage(err))
}

// Tells the gateway a typed command ran, so observers of the session see it
func (r *slashRun) ran() tea.Cmd {
	if !r.typed {
		return nil
	}
	return r.a.events.quietRequest("command.observe", map[string]any{"arg": r.arg, "name": r.name, "session_id": nilIfEmpty(r.sid)})
}

func (r *slashRun) sys(text string) { r.a.sys(text) }

// Counts a typed command in the shared metrics
func (a *app) reportSlashCommand(name string) tea.Cmd {
	params := map[string]any{"command": name}
	if sid := a.store.ui.Sid; sid != "" {
		params["session_id"] = sid
	}
	return a.events.quietRequest("shared_metrics.slash_command", params)
}

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
	var count tea.Cmd
	if typed {
		count = a.reportSlashCommand(name)
	}
	if found := a.findSlashCommand(name); found != nil {
		return tea.Batch(count, found.run(run))
	}
	if a.widgets.has(name) {
		return tea.Batch(count, a.widgets.launch(name, arg))
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
				a.sys("ambiguous command: " + shown)
				return count
			}
		}
	}
	exec := gatewayCall(a.gw, "slash.exec", map[string]any{"command": strings.TrimPrefix(cmd, "/"), "session_id": nilIfEmpty(run.sid)}, func(v any, err error) tea.Cmd {
		if run.stale() {
			return nil
		}
		if err == nil {
			if parseCommandDispatch(v) != nil {
				return a.handleDispatch(run, v, true)
			}
			r := asRPCResult(v)
			body, _ := r["output"].(string)
			if body == "" {
				body = "/" + name + ": no output"
			}
			if warning, _ := r["warning"].(string); warning != "" {
				body = "warning: " + warning + "\n" + body
			}
			return a.showOutput(body, name)
		}
		if isUnsupportedCommand(err) {
			a.sys(describeSlashExecError(name, err))
			return nil
		}
		if !shouldFallbackToDispatch(err) {
			a.sys("error: " + describeSlashExecError(name, err))
			return nil
		}
		return gatewayCall(a.gw, "command.dispatch", map[string]any{"arg": arg, "name": name, "session_id": nilIfEmpty(run.sid)}, func(raw any, err error) tea.Cmd {
			if run.stale() {
				return nil
			}
			if err != nil {
				run.fail(err)
				return nil
			}
			return a.handleDispatch(run, raw, false)
		})
	})
	return tea.Batch(count, exec)
}

// Carries out what the gateway decided a command is, output, an alias, a skill or message to send, or a prefill
func (a *app) handleDispatch(run *slashRun, raw any, fromExec bool) tea.Cmd {
	d := parseCommandDispatch(raw)
	if d == nil {
		a.sys("error: " + tr("session.common.invalidResponse", "command.dispatch"))
		return nil
	}
	text := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
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
		if out := text(d.Output); fromExec && out != "" {
			return a.showOutput(out, run.name)
		} else if out != "" {
			a.sys(out)
		} else {
			a.sys("(no output)")
		}
	case "alias":
		argTail := ""
		if run.arg != "" {
			argTail = " " + run.arg
		}
		return a.slash("/"+d.Target+argTail, run.submission, false)
	case "skill":
		if trimJS(text(d.Message)) == "" {
			a.sys("/" + run.name + ": skill payload missing message")
			return nil
		}
		return sendDispatch(text(d.Display), text(d.Message))
	case "send":
		if notice := trimJS(text(d.Notice)); notice != "" {
			a.sys(text(d.Notice))
		}
		if trimJS(text(d.Message)) == "" {
			a.sys("/" + run.name + ": empty message")
			return nil
		}
		return sendDispatch(text(d.Display), text(d.Message))
	case "prefill":
		if notice := trimJS(text(d.Notice)); notice != "" {
			a.sys(text(d.Notice))
		}
		if message := text(d.Message); message != "" {
			return a.card.composer.SetValue(message)
		}
	}
	return nil
}

// Commands the composer completes, the local ones, the widgets and every command of the gateway catalog
func (a *app) composerCommands() []cards.Command {
	seen := map[string]bool{}
	var out []cards.Command
	add := func(name, desc string, suggest func(string) []string, arg string) {
		name = strings.TrimPrefix(strings.ToLower(name), "/")
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		required := ""
		if strings.HasPrefix(arg, "<") {
			required = arg
		} else if arg != "" {
			desc = arg + "  " + desc
		}
		out = append(out, cards.Command{Name: name, Desc: desc, Arg: required, Suggest: suggest})
	}
	for _, c := range a.localCommands {
		add(c.name, c.help, c.suggest, c.usage)
	}
	for _, w := range a.widgets.list() {
		add(w.ID, w.Help, nil, "")
	}
	if a.catalog != nil {
		for _, pair := range a.catalog.Pairs {
			name, usage, _ := strings.Cut(strings.TrimPrefix(pair[0], "/"), " ")
			var suggest func(string) []string
			if subs := a.catalog.Sub["/"+name]; len(subs) > 0 {
				suggest = func(string) []string { return subs }
			}
			add(name, pair[1], suggest, usage)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Local commands as palette entries, so every one is reachable without typing its name
func (a *app) slashPaletteCommands() []wm.Command {
	var out []wm.Command
	for _, c := range a.localCommands {
		if c.usage != "" && !strings.HasPrefix(c.usage, "[") {
			continue
		}
		name := c.name
		out = append(out, wm.Command{ID: "slash." + name, Title: "/" + name + "  " + c.help, When: func() bool { return !a.dialogs.waiting() },
			Run: func() tea.Cmd { return a.slash("/"+name, nil, false) }})
	}
	return out
}

// Hotkeys of the session as the keymap binds them now
func (a *app) hotkeyRows() [][2]string {
	var rows [][2]string
	commands := map[string]bool{"palette": true, "quit": true, "interrupt": true}
	for _, action := range a.keys.InContexts([]string{tuiContext, keys.Session}) {
		if action.Context == keys.Global && !commands[action.ID] {
			continue
		}
		if chord := a.keys.DisplayAll(action.ID); chord != "" && action.Doc != "" && (action.Context == tuiContext || !action.Quiet || strings.HasPrefix(action.ID, "composer.delete")) {
			rows = append(rows, [2]string{chord, action.Doc})
		}
	}
	return append(rows, [2]string{`\ + enter`, "continue the prompt on a new line"}, [2]string{"!<cmd>", "run a shell command in the session"}, [2]string{"{!<cmd>}", "put the output of a shell command into the prompt"})
}

// Whether the terminal UI runs inside the dashboard chat tab, where it may not exit or update
func dashboardMode() bool {
	return trueRe.MatchString(strings.ToLower(trimJS(os.Getenv("ICEBATH_TUI_DASHBOARD"))))
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
	params := map[string]any{"key": key, "value": value}
	if key == "focus" && r.a.gw.canonical() && r.sid != "" {
		params["session_id"] = r.sid
	}
	return r.a.events.quietRequest("config.set", params)
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
					text := tr("slashCmd.core.help.skillCommandsAvailable", jsString(float64(a.catalog.SkillCount)))
					sections = append(sections, panelSection{Text: &text})
				}
			}
			var local [][2]string
			for _, c := range a.localCommands {
				label := "/" + c.name
				if c.usage != "" {
					label += " " + c.usage
				}
				local = append(local, [2]string{label, c.help})
			}
			for _, w := range a.widgets.list() {
				local = append(local, [2]string{"/" + w.ID, w.Help})
			}
			tui, hot := tr("slashCmd.core.help.tuiSection"), tr("help.hotkeys")
			sections = append(sections, panelSection{Rows: local, Title: &tui}, panelSection{Rows: a.hotkeyRows(), Title: &hot})
			header := a.store.ui.Theme.Brand.HelpHeader
			if header == "" {
				header = "Commands"
			}
			a.panel(header, sections)
			return r.ran()
		}},
		{name: "quit", aliases: []string{"exit"}, help: "exit icebath", run: func(r *slashRun) tea.Cmd {
			if dashboardMode() {
				r.sys(tr("slashCmd.core.quit.dashboardDisabled"))
				return nil
			}
			return a.die(0)
		}},
		{name: "update", help: "update icebath to the latest version, leaving the terminal UI", run: func(r *slashRun) tea.Cmd {
			if dashboardMode() {
				r.sys(tr("slashCmd.core.update.dashboardDisabled"))
				return nil
			}
			r.sys(tr("slashCmd.core.update.exiting"))
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
					r.sys(tr("slashCmd.core.mouse.usage"))
					return nil
				}
				a.store.ui.MouseTracking = next
				r.sys(tr("slashCmd.core.mouse.tracking", next))
				return r.saveDisplayPref("mouse", next)
			}},
		{name: "clear", aliases: []string{"new"}, help: "start a new session", usage: "[title]", run: func(r *slashRun) tea.Cmd {
			if a.life.guardBusySessionSwitch(tr("slashCmd.core.clear.switchSessions")) {
				return nil
			}
			isNew := strings.HasPrefix(r.cmd, "/new")
			title, message := "", ""
			if isNew {
				title, message = trimJS(r.arg), tr("slashCmd.core.clear.newSessionStarted")
			}
			commit := func() tea.Cmd {
				a.store.ui.Status = tr("slashCmd.core.clear.forgingSession")
				return tea.Batch(r.ran(), a.life.newSession(message, title, nil))
			}
			yes, heading := tr("slashCmd.core.clear.confirmClear"), tr("slashCmd.core.clear.titleClear")
			if isNew {
				yes, heading = tr("slashCmd.core.clear.confirmNew"), tr("slashCmd.core.clear.titleNew")
			}
			return a.confirmDestructive(heading, tr("slashCmd.core.clear.detail"), yes, tr("slashCmd.core.clear.cancelLabel"), commit)
		}},
		{name: "redraw", help: "force a full UI repaint", run: func(r *slashRun) tea.Cmd {
			a.card.view.invalidate()
			r.sys(tr("slashCmd.core.redraw.done"))
			return tea.ClearScreen
		}},
		{name: "status", help: "show live session info", run: func(r *slashRun) tea.Cmd {
			if r.sid == "" {
				r.sys(tr("slashCmd.core.status.noActiveSession"))
				return nil
			}
			method, params := "session.status", map[string]any{"session_id": r.sid}
			if a.gw.canonical() {
				method, params = "slash.exec", map[string]any{"command": "status", "session_id": r.sid}
			}
			return r.rpc(method, params, func(res map[string]any) tea.Cmd {
				out, _ := res["output"].(string)
				if out == "" {
					out = tr("slashCmd.core.status.empty")
				}
				return a.page(out, tr("slashCmd.core.status.pageTitle"))
			})
		}},
		{name: "density", help: "toggle compact display", usage: "[on|off|toggle]", run: func(r *slashRun) tea.Cmd {
			next, ok := flagFromArg(r.arg, a.store.ui.Compact)
			if !ok {
				r.sys(tr("slashCmd.core.density.usage"))
				return nil
			}
			a.store.ui.Compact = next
			r.sys(tr("slashCmd.core.density.state", onOff(next)))
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
			return r.rpc("config.set", map[string]any{"key": "timestamps", "value": onOff(next)}, nil)
		}},
		{name: "config", help: "show the presentation settings the gateway serves for this profile", run: func(r *slashRun) tea.Cmd {
			if !a.gw.canonical() {
				return r.rpc("slash.exec", map[string]any{"command": strings.TrimPrefix(r.cmd, "/"), "session_id": nilIfEmpty(r.sid)}, func(res map[string]any) tea.Cmd {
					out, _ := res["output"].(string)
					if out == "" {
						out = "{}"
					}
					return a.page(out, "Config")
				})
			}
			params := map[string]any{"key": "full"}
			if r.sid != "" {
				params["session_id"] = r.sid
			}
			return r.rpc("config.get", params, func(res map[string]any) tea.Cmd {
				cfg, _ := res["config"].(map[string]any)
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
		lines = append(lines, indent+k+": "+jsString(value[k]))
	}
	return lines
}

// Asks the app to quit with a status after a command printed its last line
type dieMsg struct{ code int }
