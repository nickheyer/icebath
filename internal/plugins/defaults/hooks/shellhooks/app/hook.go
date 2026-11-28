package app

import (
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	shellhooks "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/hooks"
	hooksv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Row of a hook's page: something done to it, or one of its fields with its value
type line struct {
	act   *act
	field string
}

// Opens the page of a hook over the page of the area: its state as a sentence and what its event is, then allowing it or not, a test, its
// command, the tools it runs for on an event about a tool, its time limit, whether it blocks on an event that can block, and removing it.
// A new command puts the page of the hook under that command in its place
func (p *page) hookPage(event, command string) tea.Cmd {
	return p.hub.Push(p.hookView(event, command))
}

// The page of a hook, to show over the page of the area
func (p *page) hookView(event, command string) *cards.List {
	t := p.kit.Env.Theme
	at := &command
	var lines []line
	var view *cards.List
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   command,
		Glyph:   t.Glyphs.Terminal,
		Key:     Screen + ":hook:" + event + ":" + command,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			if hook := p.hook(event, *at); hook != nil {
				return p.hookState(hook)
			}
			return nil
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			lines = p.hookLines(event, at)
			cells := make([][]string, len(lines))
			for i, l := range lines {
				cells[i] = p.lineCells(event, *at, l)
			}
			return cells
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(lines) {
				return nil
			}
			return []string{p.lineAbout(lines[i])}
		},
		Empty: func() string { return "There is no hook " + *at + " on " + shellconfig.PointOf(event).Title + " now." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(lines) {
				return nil
			}
			return p.lineHints(event, *at, lines[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if a := lines[i].act; a != nil {
					return a.run(l)
				}
				return p.changeField(l, event, at, lines[i].field)
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if field := lines[i].field; field != "" {
					return p.resetField(l, event, *at, field)
				}
				return nil
			},
		},
		OnFocus: func(*cards.List) tea.Cmd { return p.load() },
	})
	view.ToggleDetail()
	p.views.Add(view)
	return view
}

// Puts the page of a hook under the command it was given in the place of the page it was changed on, whose title and rows are of the
// command it had; a page that is no longer on top closes instead
func (p *page) moved(old *cards.List, event, command string) tea.Cmd {
	if p.hub.Top() != wm.Card(old) {
		return p.hub.Close(old)
	}
	return p.hub.Swap(p.hookView(event, command))
}

// The state of a hook as a sentence, then what its event is; for one whose script changed since it was allowed, in the warm colour what to do
func (p *page) hookState(hook *hooksv1.Hook) []string {
	t := p.kit.Env.Theme
	var lines []string
	switch {
	case hook.GetChanged():
		lines = append(lines, "You allowed it on "+when(hook.GetApprovedAt())+", and its script changed on "+when(hook.GetChangedNow())+".",
			render.Warm("It still runs. Check the change, then allow it again.", t))
	case hook.GetUnasked():
		lines = append(lines, "Allowed, because Run without asking lets every hook run.")
	case hook.GetAllowed() && hook.GetApprovedAt() != "":
		lines = append(lines, "You allowed it on "+when(hook.GetApprovedAt())+".")
	case hook.GetAllowed():
		lines = append(lines, "Allowed.")
	default:
		lines = append(lines, "Not allowed, so it doesn't run.")
	}
	return append(lines, render.Dim(strs.Full(shellconfig.PointOf(hook.GetEvent()).About), t))
}

// Time the allowlist keeps as a person reads it, in this computer's time zone; the text as it is when it can't be read
func when(stamp string) string {
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return stamp
	}
	return at.Local().Format("2 January 2006 at 15:04")
}

// Rows of a hook's page as it stands now: tools only for an event about a tool, blocking only for an event that can block
func (p *page) hookLines(event string, at *string) []line {
	hook := p.hook(event, *at)
	if hook == nil {
		return nil
	}
	allow := &act{title: "Allow", about: "Lets it run with your full rights each time its event happens.", verb: "allow",
		run: func(l *cards.List) tea.Cmd { return p.toggle(l, p.hook(event, *at)) }}
	if hook.GetState() == "Allowed" && !hook.GetUnasked() {
		allow = &act{title: "Stop allowing", about: "It stays here and no longer runs.", verb: "stop allowing",
			run: func(l *cards.List) tea.Cmd { return p.toggle(l, p.hook(event, *at)) }}
	}
	about := "Runs it once on a sample of its event and shows what it prints."
	if hook.GetTool() {
		about = "Runs it once on a sample call of a tool you pick and shows what it prints."
	}
	out := []line{{act: allow}, {act: &act{title: "Test", about: about, verb: "test", opens: true,
		run: func(*cards.List) tea.Cmd { return p.test(p.hook(event, *at)) }}}, {field: shellconfig.Command}}
	if hook.GetTool() {
		out = append(out, line{field: shellconfig.Matcher})
	}
	out = append(out, line{field: shellconfig.Timeout})
	if hook.GetBlocking() {
		out = append(out, line{field: shellconfig.Block})
	}
	return append(out, line{act: &act{title: "Remove", about: "Takes it out of icebath. Its script stays on this computer.", verb: "remove",
		run: func(l *cards.List) tea.Cmd { return p.removeHook(l, p.hook(event, *at)) }}})
}

// Names of the fields of a hook as its page shows them
var fieldTitles = map[string]string{shellconfig.Command: "Command", shellconfig.Matcher: "Tools", shellconfig.Timeout: "Time limit",
	shellconfig.Block: "Block when it fails"}

// Cells of a row of a hook's page: an act by its name; a field by its name, the mark of a value changed from its default and its value
func (p *page) lineCells(event, command string, l line) []string {
	t := p.kit.Env.Theme
	if l.act != nil {
		if l.act.opens {
			return []string{l.act.title, render.Dim(t.Glyphs.Forward, t), ""}
		}
		return []string{l.act.title, "", ""}
	}
	hook := p.hook(event, command)
	mark := ""
	if changedField(hook, l.field) {
		mark = render.Cool("●", t)
		if t.Plain {
			mark = render.Cool("*", t)
		}
	}
	return []string{fieldTitles[l.field], mark, p.fieldValue(hook, l.field)}
}

// Whether a field of the hook holds a value other than its default
func changedField(hook *hooksv1.Hook, field string) bool {
	switch field {
	case shellconfig.Matcher:
		return hook.GetMatcher() != ""
	case shellconfig.Timeout:
		return hook.GetTimeout() != shellhooks.DefaultTimeout
	case shellconfig.Block:
		return hook.GetFailClosed()
	}
	return false
}

// Value of a field of the hook in words
func (p *page) fieldValue(hook *hooksv1.Hook, field string) string {
	switch field {
	case shellconfig.Command:
		return hook.GetCommand()
	case shellconfig.Matcher:
		if hook.GetMatcher() == "" {
			return "Every tool"
		}
		return p.tools.words(hook.GetMatcher())
	case shellconfig.Timeout:
		return strs.Span(time.Duration(hook.GetTimeout()) * time.Second)
	}
	if hook.GetFailClosed() {
		return "On"
	}
	return "Off"
}

// What a row of a hook's page does or holds, under the rows
func (p *page) lineAbout(l line) string {
	if l.act != nil {
		return l.act.about
	}
	switch l.field {
	case shellconfig.Command:
		return "The command or script that runs. It reads what happened as JSON on its input."
	case shellconfig.Matcher:
		return "The tools it runs for. It runs for every tool while none is picked."
	case shellconfig.Timeout:
		return "How long it may run, from 1 second to " + strs.Span(shellhooks.MaxTimeout*time.Second) + ". Default: " +
			strs.Span(shellhooks.DefaultTimeout*time.Second) + "."
	}
	return "Block the tool call when the command fails or runs out of time."
}

// Buttons of a row of a hook's page: its act, or what enter does on a field and reset on one changed from its default
func (p *page) lineHints(event, command string, l line) []wm.Hint {
	if l.act != nil {
		return []wm.Hint{{Action: "hub.use", Label: l.act.verb}}
	}
	hook := p.hook(event, command)
	verb := "change"
	if l.field == shellconfig.Block {
		verb = "turn on"
		if hook.GetFailClosed() {
			verb = "turn off"
		}
	}
	hints := []wm.Hint{{Action: "hub.use", Label: verb}}
	if changedField(hook, l.field) {
		hints = append(hints, wm.Hint{Action: "hub.remove", Label: "reset"})
	}
	return hints
}

// Changes a field of the hook the way its kind takes: the command and the time limit typed, the tools picked, blocking flipped at once
func (p *page) changeField(view *cards.List, event string, at *string, field string) tea.Cmd {
	hook := p.hook(event, *at)
	if hook == nil {
		return nil
	}
	edit := func(request *hooksv1.ChangeRequest, from wm.Card) tea.Cmd {
		request.Verb, request.Event, request.Command, request.Fields = "edit", event, *at, []string{field}
		return p.change(request, func(*hooksv1.ChangeResponse) tea.Cmd {
			var cmds []tea.Cmd
			switch field {
			case shellconfig.Command:
				// The hook was read again under the command it had, which no hook runs now, so its page is made again under the new one
				*at = strings.TrimSpace(request.GetTo())
				cmds = append(cmds, p.moved(view, event, *at))
			case shellconfig.Matcher:
				// The tools by the names a person reads, as the row says them
				said := *at + " now runs for every tool."
				if pattern := strings.TrimSpace(request.GetMatcher()); pattern != "" {
					said = *at + " now runs for " + p.tools.words(pattern) + "."
				}
				cmds = append(cmds, p.hub.Done(said))
			}
			if from != nil {
				cmds = append(cmds, p.hub.Finish(from))
			}
			return tea.Batch(cmds...)
		})
	}
	switch field {
	case shellconfig.Command:
		input := cards.Field{Label: "Command", Value: hook.GetCommand(), Select: true, Path: cards.PathFile,
			Help: "The command or script that runs, such as ~/hooks/check.sh. It reads what happened as JSON on its input."}
		return p.kit.Ask(p.hub, "Command", input, "Save", func(text string) tea.Cmd {
			if text == hook.GetCommand() {
				return nil
			}
			return edit(&hooksv1.ChangeRequest{To: text}, nil)
		})
	case shellconfig.Matcher:
		return p.tools.open(p.kit, p.hub, tui.Ask{Title: "Tools", Intro: []string{"The tools " + hook.GetCommand() + " runs for."},
			Values: map[string]string{"": strconv.Quote(hook.GetMatcher())}, First: []tui.Choice{{Title: "Every tool"}}, Profile: p.profile,
			Done: func(values map[string]string) tea.Cmd {
				var pattern string
				yaml.Unmarshal([]byte(values[""]), &pattern)
				return edit(&hooksv1.ChangeRequest{Matcher: pattern}, nil)
			}})
	case shellconfig.Timeout:
		value := ""
		if hook.GetTimeout() != shellhooks.DefaultTimeout {
			value = strconv.Itoa(int(hook.GetTimeout()))
		}
		input := cards.Field{ID: "seconds", Label: "Time limit", Kind: cards.FieldNumber, Value: value, Select: true, Check: seconds,
			Help: "How long it may run, in seconds from 1 to " + strconv.Itoa(shellhooks.MaxTimeout) + ".\nLeave empty for " +
				strs.Span(shellhooks.DefaultTimeout*time.Second) + "."}
		var form *cards.Form
		form = cards.NewForm(p.kit.Env, cards.FormSpec{Title: "Time limit", Fields: []cards.Field{input}, Submit: "Save",
			Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
				n, _ := strconv.Atoi(strings.TrimSpace(values["seconds"]))
				return edit(&hooksv1.ChangeRequest{Timeout: int32(n)}, form)
			}})
		return p.hub.Push(form)
	}
	return edit(&hooksv1.ChangeRequest{FailClosed: !hook.GetFailClosed()}, nil)
}

// What is wrong with a time limit typed for a hook, nothing for an empty one or whole seconds within the bounds
func seconds(typed string) string {
	typed = strings.TrimSpace(typed)
	if typed == "" {
		return ""
	}
	if n, err := strconv.Atoi(typed); err != nil || n < 1 || n > shellhooks.MaxTimeout {
		return "Must be whole seconds from 1 to " + strconv.Itoa(shellhooks.MaxTimeout) + "."
	}
	return ""
}

// Puts a field of the hook back to its default after the question
func (p *page) resetField(l *cards.List, event, command, field string) tea.Cmd {
	hook := p.hook(event, command)
	if hook == nil || !changedField(hook, field) {
		return nil
	}
	defaults := map[string]string{shellconfig.Matcher: "Every tool", shellconfig.Timeout: strs.Span(shellhooks.DefaultTimeout * time.Second),
		shellconfig.Block: "Off"}
	then := map[string]string{shellconfig.Matcher: "It then runs for every tool.", shellconfig.Timeout: "It may then run for " +
		strs.Span(shellhooks.DefaultTimeout*time.Second) + ".", shellconfig.Block: "A failure of it then no longer blocks the tool call."}
	l.Confirm(cards.Question{Text: "Reset " + fieldTitles[field] + " to " + defaults[field] + "? " + then[field], Verb: "Reset"}, func() tea.Cmd {
		return p.change(&hooksv1.ChangeRequest{Verb: "edit", Event: event, Command: command, Fields: []string{field}}, nil)
	})
	return nil
}
