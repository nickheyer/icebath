// The hooks screen: every shell hook of the profile with whether it may run, and every address events are posted to; allowing a hook or
// stopping it, adding and removing hooks, and running one on a sample event or checking it
package app

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/profile"
	hooksv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1/hooksv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the hooks screen
const hooksContext = "tui.hooks"

var actions = []keys.Action{
	{ID: "tui.hooks.toggle", Context: hooksContext, Label: "toggle", Doc: "turn the highlighted row on or off", Default: []string{"t"}},
	{ID: "tui.hooks.test", Context: hooksContext, Label: "test", Doc: "run the hook once on a sample event and show its answer", Default: []string{"r"}},
	{ID: "tui.hooks.check", Context: hooksContext, Label: "check", Doc: "whether the hook can run, is allowed, is unchanged and answers well", Default: []string{"c"}},
}

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := hooksv1connect.NewHooksServiceClient(conn)
	return []tui.Screen{{
		ID:      "hooks",
		Title:   "Hooks",
		Short:   "Shell scripts that run on agent events, their consent and tests",
		Slash:   []string{"hooks"},
		Usage:   "[@profile]",
		Actions: actions,
		Where:   map[string]string{hooksContext: "hooks screen"},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, _ := profile.Split(argument)
			return (&screen{kit: kit, service: service, profile: name}).open()
		},
	}}
}

// One row of the screen, a shell hook or an address events are posted to
type row struct {
	hook    *hooksv1.Hook
	webhook *hooksv1.Webhook
}

type screen struct {
	kit     *tui.Kit
	service hooksv1connect.HooksServiceClient
	// Profile the screen was opened on, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	rows    []row
	// Events a new hook may be set on
	events []string
	loaded bool
	err    error
}

func (s *screen) open() tea.Cmd {
	title := "Hooks"
	if s.profile != "" {
		title += " of " + s.profile
	}
	s.list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   s.kit.Env.Theme.Glyphs.Terminal,
		Key:     "tui:hooks",
		Context: hooksContext,
		Shared:  []string{cards.HubContext},
		Primary: "hub.use",
		Columns: []render.Column{{Title: "Event", Max: 28}, {Title: "Command", Flex: 1}, {Title: "Status", Max: 26}},
		Yield:   []int{1, 2},
		Rows:    s.cells,
		Detail:  s.detail,
		Empty:   func() string { return tui.Waiting(s.loaded, s.err, "No hooks yet. alt+n adds one.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			var out []wm.Hint
			if hook, ok := s.hookAt(i); ok {
				out = append(out, wm.Hint{Action: "hub.use", Label: "open"}, wm.Hint{Action: "tui.hooks.toggle", Label: strings.ToLower(toggleLabel(hook))},
					wm.Hint{Action: "hub.remove", Label: "remove"})
			}
			return append(out, wm.Hint{Action: "hub.add", Label: "add"})
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.refresh":      func(*cards.List, int) tea.Cmd { return s.load() },
			"hub.use":          func(_ *cards.List, i int) tea.Cmd { return s.actions(i) },
			"hub.add":          func(*cards.List, int) tea.Cmd { return s.add() },
			"hub.remove":       func(l *cards.List, i int) tea.Cmd { return s.remove(l, i) },
			"tui.hooks.toggle": func(l *cards.List, i int) tea.Cmd { return s.toggle(l, i) },
			"tui.hooks.test": func(_ *cards.List, i int) tea.Cmd {
				if hook, ok := s.hookAt(i); ok {
					return s.test(hook)
				}
				return nil
			},
			"tui.hooks.check": func(_ *cards.List, i int) tea.Cmd {
				if hook, ok := s.hookAt(i); ok {
					return s.check(hook)
				}
				return nil
			},
		},
		OnFocus: func(*cards.List) tea.Cmd { return s.load() },
	})
	s.hub = s.kit.Hub("hooks", s.list)
	return tea.Batch(s.kit.WM.ShowOverlay(s.hub), s.load())
}

// Shows the hooks the service answered, shell hooks first
func (s *screen) show(listing *hooksv1.ListResponse) {
	s.events = listing.GetEvents()
	s.rows = s.rows[:0]
	for _, hook := range listing.GetHooks() {
		s.rows = append(s.rows, row{hook: hook})
	}
	for _, webhook := range listing.GetWebhooks() {
		s.rows = append(s.rows, row{webhook: webhook})
	}
	s.list.Refresh()
}

// Reads the hooks again
func (s *screen) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		listing, err := s.service.List(s.kit.Env.Ctx, &hooksv1.ListRequest{Profile: s.profile})
		return func() tea.Cmd {
			s.err, s.loaded = err, true
			if err == nil {
				s.show(listing)
			} else {
				s.list.Refresh()
			}
			return nil
		}
	})
}

// Sends one change and shows the hooks it answers, back on the list, the note saying what was done
func (s *screen) change(request *hooksv1.ChangeRequest, done string) tea.Cmd {
	request.Profile = s.profile
	back := s.hub.PopTo(s.list)
	s.hub.Busy("Saving…")
	return tea.Batch(back, tui.Call(s.hub, func() (*hooksv1.ChangeResponse, error) {
		return s.service.Change(s.kit.Env.Ctx, request)
	}, func(response *hooksv1.ChangeResponse) tea.Cmd {
		s.show(response.GetHooks())
		return s.hub.Done(done)
	}))
}

// Shell hook at row i, false for a webhook row and past the rows
func (s *screen) hookAt(i int) (*hooksv1.Hook, bool) {
	if i < 0 || i >= len(s.rows) || s.rows[i].hook == nil {
		return nil, false
	}
	return s.rows[i].hook, true
}

// Whether a hook may run, in a word or two
func state(hook *hooksv1.Hook) string {
	switch {
	case !hook.GetAllowed():
		return "not allowed"
	case hook.GetChanged():
		return "changed since allowed"
	}
	return "allowed"
}

// Word for the action that lets a hook run or stops it
func toggleLabel(hook *hooksv1.Hook) string {
	if hook.GetAllowed() {
		return "Stop allowing"
	}
	return "Allow"
}

// Cells of every row: the event or events, the command or address, and whether it may run
func (s *screen) cells() [][]string {
	out := make([][]string, len(s.rows))
	for i, r := range s.rows {
		if r.hook != nil {
			out[i] = []string{r.hook.GetEvent(), r.hook.GetCommand(), state(r.hook)}
			continue
		}
		out[i] = []string{strings.Join(r.webhook.GetEvents(), ", "), r.webhook.GetLabel(), "webhook"}
	}
	return out
}

// Lines of a row's detail pane
func (s *screen) detail(i int) []string {
	if i < 0 || i >= len(s.rows) {
		return nil
	}
	if target := s.rows[i].webhook; target != nil {
		signed := "not signed"
		if target.GetSigned() {
			signed = "signed"
		}
		out := []string{target.GetLabel() + " · " + signed, target.GetUrl(), "events: " + strings.Join(target.GetEvents(), ", ")}
		if target.GetMatcher() != "" {
			out = append(out, "tools: "+target.GetMatcher())
		}
		return append(out, "may take "+strconv.Itoa(int(target.GetTimeout()))+" s")
	}
	hook := s.rows[i].hook
	out := []string{hook.GetEvent() + " · " + hook.GetCommand(), state(hook)}
	if hook.GetApprovedAt() != "" {
		out = append(out, "allowed "+hook.GetApprovedAt())
	}
	if hook.GetChanged() {
		out = append(out, "The script changed since it was allowed (then "+hook.GetChangedWas()+", now "+hook.GetChangedNow()+").")
	}
	if hook.GetMatcher() != "" {
		out = append(out, "tools: "+hook.GetMatcher())
	}
	out = append(out, "may take "+strconv.Itoa(int(hook.GetTimeout()))+" s")
	if hook.GetFailClosed() {
		out = append(out, "A failure of the script blocks the action.")
	}
	return out
}

// What can be done with the hook of a row, picked from a list
func (s *screen) actions(i int) tea.Cmd {
	hook, ok := s.hookAt(i)
	if !ok {
		return nil
	}
	toggle := tui.Choice{ID: "allow", Title: "Allow", Detail: "let it run; a hook runs with your full rights"}
	if hook.GetAllowed() {
		toggle = tui.Choice{ID: "revoke", Title: "Stop allowing", Detail: "it stays set up but no longer runs"}
	}
	choices := []tui.Choice{toggle,
		{ID: "test", Title: "Test", Detail: "run it once on a sample event and show its answer"},
		{ID: "check", Title: "Check", Detail: "whether it can run, is allowed, is unchanged and answers well"},
		{ID: "remove", Title: "Remove", Detail: "take it out of the settings"},
	}
	return s.kit.Pick(s.hub, hook.GetCommand(), "run", choices, "", func(choice tui.Choice) tea.Cmd {
		switch choice.ID {
		case "allow":
			s.list.Confirm("Allow "+hook.GetCommand()+" to run on "+hook.GetEvent()+"? It runs with your full rights.", func() tea.Cmd { return s.allow(hook) })
			return nil
		case "revoke":
			return s.revoke(hook)
		case "test":
			return s.test(hook)
		case "check":
			return s.check(hook)
		}
		s.list.Confirm("Remove the hook "+hook.GetCommand()+"?", func() tea.Cmd { return s.drop(hook) })
		return nil
	})
}

// Form of a new hook: the event it runs on, its command, the tools it is for and the seconds it may take
func (s *screen) add() tea.Cmd {
	event := cards.Field{ID: "event", Label: "Event", Kind: cards.FieldChoice, Required: true}
	for _, name := range s.events {
		event.Choices = append(event.Choices, cards.Choice{Value: name, Label: name})
	}
	if len(event.Choices) > 0 {
		event.Value = event.Choices[0].Value
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{Title: "New hook", Submit: "Create", Fields: []cards.Field{event,
		{ID: "command", Label: "Command", Help: "Path of a script; it gets the event as JSON on its input.", Required: true},
		{ID: "tools", Label: "Tools", Help: "Pattern of tool names, only for the events before and after a tool call.", More: true},
		{ID: "seconds", Label: "Seconds it may take", Kind: cards.FieldNumber, More: true},
	}, Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
		seconds, _ := strconv.Atoi(strings.TrimSpace(values["seconds"]))
		command := strings.TrimSpace(values["command"])
		return s.change(&hooksv1.ChangeRequest{Verb: "add", Event: values["event"], Command: command, Matcher: strings.TrimSpace(values["tools"]),
			Timeout: int32(seconds)}, "Added "+command+".")
	}})
	return s.hub.Push(form)
}

// Lets a hook run
func (s *screen) allow(hook *hooksv1.Hook) tea.Cmd {
	return s.change(&hooksv1.ChangeRequest{Verb: "allow", Event: hook.GetEvent(), Command: hook.GetCommand()}, hook.GetCommand()+" may run now.")
}

// Stops a hook from running, its settings staying
func (s *screen) revoke(hook *hooksv1.Hook) tea.Cmd {
	return s.change(&hooksv1.ChangeRequest{Verb: "revoke", Event: hook.GetEvent(), Command: hook.GetCommand()}, hook.GetCommand()+" no longer runs.")
}

// Takes a hook out of the settings
func (s *screen) drop(hook *hooksv1.Hook) tea.Cmd {
	return s.change(&hooksv1.ChangeRequest{Verb: "remove", Event: hook.GetEvent(), Command: hook.GetCommand()}, "Removed "+hook.GetCommand()+".")
}

// Lets the hook of a row run after a confirmation, or stops it at once
func (s *screen) toggle(l *cards.List, i int) tea.Cmd {
	hook, ok := s.hookAt(i)
	switch {
	case !ok:
		return nil
	case hook.GetAllowed():
		return s.revoke(hook)
	}
	l.Confirm("Allow "+hook.GetCommand()+" to run on "+hook.GetEvent()+"? It runs with your full rights.", func() tea.Cmd { return s.allow(hook) })
	return nil
}

// Takes the hook of a row out of the settings after a confirmation
func (s *screen) remove(l *cards.List, i int) tea.Cmd {
	hook, ok := s.hookAt(i)
	if !ok {
		return nil
	}
	l.Confirm("Remove the hook "+hook.GetCommand()+"?", func() tea.Cmd { return s.drop(hook) })
	return nil
}

// Seconds with two decimals
func seconds(value float64) string {
	return strconv.FormatFloat(value, 'f', 2, 64)
}

// Lines telling how one run of a hook's command ended
func (s *screen) runLines(run *hooksv1.Run) []string {
	var out []string
	switch {
	case run.GetError() != "":
		out = append(out, render.Bad(run.GetError(), s.kit.Env.Theme))
	case run.GetTimedOut():
		out = append(out, "It did not finish within "+seconds(run.GetSeconds())+" s.")
	case run.GetExited():
		out = append(out, "It ended with code "+strconv.Itoa(int(run.GetExitCode()))+" after "+seconds(run.GetSeconds())+" s.")
	}
	if text := strings.TrimSpace(run.GetStdout()); text != "" {
		out = append(out, "output: "+text)
	}
	if text := strings.TrimSpace(run.GetStderr()); text != "" {
		out = append(out, "errors: "+text)
	}
	if run.GetDirective() == "" {
		return append(out, "It answered nothing.")
	}
	return append(out, "answer: "+run.GetDirective())
}

// Page of the hub showing the lines, each wrapped to the width
func (s *screen) page(title, key string, lines []string) tea.Cmd {
	return s.hub.Push(cards.NewPage(s.kit.Env, cards.PageSpec{Title: title, Key: key, Lines: func(w int) []string {
		var out []string
		for _, line := range lines {
			out = append(out, strings.Split(render.Wrap(line, max(w, 8)), "\n")...)
		}
		return out
	}}))
}

// Runs a hook once on a sample event and shows how it ended
func (s *screen) test(hook *hooksv1.Hook) tea.Cmd {
	s.hub.Busy("Checking…")
	return tui.Call(s.hub, func() (*hooksv1.TestResponse, error) {
		return s.service.Test(s.kit.Env.Ctx, &hooksv1.TestRequest{Profile: s.profile, Event: hook.GetEvent(), Command: hook.GetCommand()})
	}, func(response *hooksv1.TestResponse) tea.Cmd {
		s.hub.Quiet()
		var lines []string
		for _, tested := range response.GetRuns() {
			lines = append(lines, s.runLines(tested.GetRun())...)
		}
		return s.page(hook.GetCommand(), "tui:hooks:test", lines)
	})
}

// Checks a hook and shows what was found
func (s *screen) check(hook *hooksv1.Hook) tea.Cmd {
	s.hub.Busy("Checking…")
	return tui.Call(s.hub, func() (*hooksv1.CheckResponse, error) {
		return s.service.Check(s.kit.Env.Ctx, &hooksv1.CheckRequest{Profile: s.profile, Event: hook.GetEvent(), Command: hook.GetCommand()})
	}, func(response *hooksv1.CheckResponse) tea.Cmd {
		s.hub.Quiet()
		t := s.kit.Env.Theme
		var lines []string
		for _, c := range response.GetChecks() {
			if c.GetRunnable() {
				lines = append(lines, render.Good("The script exists and can run.", t))
			} else {
				lines = append(lines, render.Bad("The script is missing or cannot run.", t))
			}
			if c.GetAllowed() {
				lines = append(lines, render.Good("allowed "+c.GetApprovedAt(), t))
			} else {
				lines = append(lines, render.Bad("Not allowed, so it does not run.", t))
			}
			switch {
			case c.GetChanged():
				lines = append(lines, render.Bad("The script changed since it was allowed (then "+c.GetChangedWas()+", now "+c.GetChangedNow()+").", t))
			case c.GetCompared():
				lines = append(lines, render.Good("Unchanged since it was allowed.", t))
			}
			if run := c.GetRun(); run != nil {
				switch c.GetOutput() {
				case "empty":
					lines = append(lines, render.Good("It ran and answered nothing; it only watches.", t))
				case "json":
					lines = append(lines, render.Good("It ran and its answer is well formed.", t))
				case "invalid":
					lines = append(lines, render.Bad("Its answer is not well formed: "+strings.TrimSpace(run.GetStdout()), t))
				default:
					lines = append(lines, s.runLines(run)[0])
				}
			}
			if c.GetProblems() > 0 {
				lines = append(lines, "", render.Bad(strconv.Itoa(int(c.GetProblems()))+" problems found.", t))
			} else {
				lines = append(lines, "", render.Good("It looks healthy.", t))
			}
		}
		return s.page(hook.GetCommand(), "tui:hooks:check", lines)
	})
}
