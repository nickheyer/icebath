// The hooks screen: every shell hook of the profile under the event it runs on with whether it may run, and every address events are
// posted to; allowing a hook or stopping it, adding and removing hooks, running one or every hook of an event on a sample event, and
// checking one hook or all of them
package app

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/hooks"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	shellhooks "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/hooks"
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
	{ID: "tui.hooks.toggle", Context: hooksContext, Label: "turn on/off", Doc: "Turn the highlighted hook on or off", Default: []string{"t"}},
	{ID: "tui.hooks.test", Context: hooksContext, Label: "test", Doc: "Run the hook once on a sample event and show the answer", Default: []string{"r"}},
	{ID: "tui.hooks.check", Context: hooksContext, Label: "check", Doc: "Check that the hook can run, is allowed, is unchanged and answers well", Default: []string{"c"}},
}

// Headings of the rows that are not hooks of an event
const (
	webhooksGroup = "Webhooks"
	moreGroup     = "More"
)

// One event a shell hook runs on: its name in the settings file, what the screen calls it and what a hook of it does
type event struct {
	name, title, about string
}

// Events a shell hook runs on, in the order the screen lists them, as the settings name and order them
var events = func() []event {
	out := make([]event, len(shellconfig.Points))
	for i, point := range shellconfig.Points {
		out[i] = event{point.Name, point.Title, point.About}
	}
	return out
}()

// The event of the name as the screen shows it, the name itself for one the screen does not know
func eventOf(name string) event {
	for _, e := range events {
		if e.name == name {
			return e
		}
	}
	return event{name: name, title: name}
}

// Place of the event in the order of the screen, those it does not know last
func place(name string) int {
	if i := slices.IndexFunc(events, func(e event) bool { return e.name == name }); i >= 0 {
		return i
	}
	return len(events)
}

// Whether hooks of the event run for each tool call, so they may name the tools they are for
func toolEvent(name string) bool {
	point, _ := hooks.PointOf(name)
	return point.Tool
}

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := hooksv1connect.NewHooksServiceClient(conn)
	return []tui.Screen{{
		ID:      "hooks",
		Group:   tui.GroupAbilities,
		Order:   100,
		Title:   "Hooks",
		Short:   "Run your own commands when the agent acts",
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

// One row of the screen: a shell hook, an address events are posted to, or something done to every hook
type row struct {
	hook    *hooksv1.Hook
	webhook *hooksv1.Webhook
	act     *action
}

// Something a row does to every hook
type action struct {
	title, about string
	// Word on the button that does it
	verb string
	run  func(l *cards.List) tea.Cmd
}

type screen struct {
	kit     *tui.Kit
	service hooksv1connect.HooksServiceClient
	// Profile the screen was opened on, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	listing *hooksv1.ListResponse
	rows    []row
	loaded  bool
	err     error
}

func (s *screen) open() tea.Cmd {
	title := "Hooks"
	if s.profile != "" {
		title += " · " + s.profile
	}
	s.list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   s.kit.Env.Theme.Glyphs.Terminal,
		Key:     "tui:hooks",
		Context: cards.HubContext,
		// Its own keys stay in force on every row, those the footer has no room for too
		Shared:  []string{hooksContext},
		Primary: "hub.use",
		Intro: func() []string {
			lines := []string{"Hooks run your own commands when the agent acts, such as before it runs a tool."}
			if s.listing.GetAcceptAll() {
				lines = append(lines, "Every hook runs without asking, because Run without asking is on in settings.")
			}
			return lines
		},
		Columns: []render.Column{{Flex: 1}, {}},
		Rows:    s.cells,
		Group:   func(i int) string { return s.group(s.rows[i]) },
		Detail:  s.detail,
		Empty: func() string {
			return tui.Waiting(s.loaded, s.err, "No hooks yet. "+s.kit.Env.Keys.Display("hub.add")+" adds one.")
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			add := wm.Hint{Action: "hub.add", Label: "add"}
			if i < 0 || i >= len(s.rows) {
				return []wm.Hint{add}
			}
			switch r := s.rows[i]; {
			case r.hook != nil:
				return []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "tui.hooks.toggle", Label: toggleLabel(r.hook)},
					{Action: "tui.hooks.test", Label: "test"}, {Action: "hub.remove", Label: "remove"}, add}
			case r.webhook != nil:
				return []wm.Hint{{Action: "hub.use", Label: "change"}, add}
			}
			return []wm.Hint{{Action: "hub.use", Label: s.rows[i].act.verb}, add}
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.refresh": func(*cards.List, int) tea.Cmd { return s.load() },
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				switch r := s.rows[i]; {
				case r.hook != nil:
					return s.page(r.hook)
				case r.webhook != nil:
					return s.webhooks()
				}
				return s.rows[i].act.run(l)
			},
			"hub.add":          func(*cards.List, int) tea.Cmd { return s.add() },
			"hub.remove":       s.withHook(func(l *cards.List, hook *hooksv1.Hook) tea.Cmd { return s.remove(l, hook) }),
			"tui.hooks.toggle": s.withHook(func(l *cards.List, hook *hooksv1.Hook) tea.Cmd { return s.toggle(l, hook) }),
			"tui.hooks.test":   s.withHook(func(_ *cards.List, hook *hooksv1.Hook) tea.Cmd { return s.test(hook, "") }),
			"tui.hooks.check":  s.withHook(func(_ *cards.List, hook *hooksv1.Hook) tea.Cmd { return s.check(hook) }),
		},
		OnFocus: func(*cards.List) tea.Cmd { return s.load() },
	})
	s.list.ToggleDetail()
	s.hub = s.kit.Hub("hooks", s.list)
	return tea.Batch(s.kit.WM.ShowOverlay(s.hub), s.load())
}

// Action on the hook of the row under the cursor, nothing on any other row
func (s *screen) withHook(act func(l *cards.List, hook *hooksv1.Hook) tea.Cmd) func(*cards.List, int) tea.Cmd {
	return func(l *cards.List, i int) tea.Cmd {
		if i < 0 || i >= len(s.rows) || s.rows[i].hook == nil {
			return nil
		}
		return act(l, s.rows[i].hook)
	}
}

// Shows the hooks the service answered: the shell hooks under their events in the order of the screen, the webhooks, then what can be done
// to every hook while there are any
func (s *screen) show(listing *hooksv1.ListResponse) {
	s.listing = listing
	shell := slices.Clone(listing.GetHooks())
	slices.SortStableFunc(shell, func(a, b *hooksv1.Hook) int { return place(a.GetEvent()) - place(b.GetEvent()) })
	s.rows = s.rows[:0]
	for _, hook := range shell {
		s.rows = append(s.rows, row{hook: hook})
	}
	for _, webhook := range listing.GetWebhooks() {
		s.rows = append(s.rows, row{webhook: webhook})
	}
	if len(shell) > 0 {
		for _, act := range s.actions(shell) {
			s.rows = append(s.rows, row{act: act})
		}
	}
	s.list.Refresh()
}

// What can be done to every hook: allowing those not allowed yet, checking them all, and running the hooks of an event on a sample
func (s *screen) actions(shell []*hooksv1.Hook) []*action {
	var out []*action
	if slices.ContainsFunc(shell, func(hook *hooksv1.Hook) bool { return !hook.GetAllowed() || hook.GetChanged() }) && !s.listing.GetAcceptAll() {
		out = append(out, &action{"Allow every hook", "Let every hook in your settings run", "allow", func(l *cards.List) tea.Cmd {
			l.Confirm("Allow every hook in your settings? Each one runs with your full rights.", s.acceptAll)
			return nil
		}})
	}
	return append(out,
		&action{"Check every hook", "See whether each hook can run, is allowed, is unchanged and answers well", "check", func(*cards.List) tea.Cmd {
			return s.checkAll()
		}},
		&action{"Test an event", "Run every hook of an event on a sample of it, with your own data if you like", "test", func(*cards.List) tea.Cmd {
			return s.testEvent()
		}})
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

// Heading a row is listed under: the event of a hook, the webhooks, or what is done to every hook
func (s *screen) group(r row) string {
	switch {
	case r.hook != nil:
		return eventOf(r.hook.GetEvent()).title
	case r.webhook != nil:
		return webhooksGroup
	}
	return moreGroup
}

// Whether a hook may run, in words
func (s *screen) state(hook *hooksv1.Hook) string {
	t := s.kit.Env.Theme
	switch {
	case hook.GetAllowed() && hook.GetChanged():
		return render.Warm("Changed since you allowed it", t)
	case hook.GetAllowed():
		return render.Good("Allowed", t)
	case s.listing.GetAcceptAll():
		return "Runs without asking"
	}
	return render.Warm("Not allowed", t)
}

// Word for the button that lets a hook run or stops it
func toggleLabel(hook *hooksv1.Hook) string {
	if hook.GetAllowed() && !hook.GetChanged() {
		return "stop allowing"
	}
	return "allow"
}

// Cells of every row: the command, the name of a webhook or what a row does, and the state of a hook or a webhook
func (s *screen) cells() [][]string {
	t := s.kit.Env.Theme
	out := make([][]string, len(s.rows))
	for i, r := range s.rows {
		switch {
		case r.hook != nil:
			out[i] = []string{r.hook.GetCommand(), s.state(r.hook)}
		case r.webhook != nil:
			signed := render.Dim("Not signed", t)
			if r.webhook.GetSigned() {
				signed = "Signed"
			}
			out[i] = []string{r.webhook.GetLabel(), signed}
		default:
			out[i] = []string{r.act.title, ""}
		}
	}
	return out
}

// Lines under the list about the highlighted row
func (s *screen) detail(i int) []string {
	if i < 0 || i >= len(s.rows) {
		return nil
	}
	switch r := s.rows[i]; {
	case r.webhook != nil:
		return s.webhookFacts(r.webhook)
	case r.act != nil:
		return []string{r.act.about + "."}
	}
	return s.facts(s.rows[i].hook)
}

// What a hook is and how it stands: its event, its command whole, the tools it is for, how long it may take, what its failure does,
// whether it may run and whether its script changed since
func (s *screen) facts(hook *hooksv1.Hook) []string {
	t := s.kit.Env.Theme
	e := eventOf(hook.GetEvent())
	var lines []string
	if e.about != "" {
		lines = append(lines, e.about+".")
	}
	lines = append(lines, "Command: "+hook.GetCommand(), render.Dim("Event in the settings file: "+hook.GetEvent(), t))
	if matcher := hook.GetMatcher(); matcher != "" {
		lines = append(lines, "Only for the tools "+matcher+".")
	}
	lines = append(lines, "May take up to "+seconds(float64(hook.GetTimeout()))+".")
	if hook.GetFailClosed() {
		lines = append(lines, "If the command fails, the tool call is blocked.")
	}
	switch {
	case hook.GetAllowed() && hook.GetApprovedAt() != "":
		lines = append(lines, render.Good("You allowed it on "+hook.GetApprovedAt()+".", t))
	case hook.GetAllowed():
		lines = append(lines, render.Good("You allowed it.", t))
	case s.listing.GetAcceptAll():
		lines = append(lines, "It runs without asking, because Run without asking is on in settings.")
	default:
		lines = append(lines, render.Warm("Not allowed yet, so it doesn't run.", t))
	}
	if hook.GetChanged() {
		lines = append(lines, render.Warm(changed(hook.GetChangedWas(), hook.GetChangedNow()), t))
	}
	return lines
}

// What changed in a script since it was allowed, from its times then and now
func changed(was, now string) string {
	return "The script changed after you allowed it. It was dated " + was + " then, and is dated " + now + " now. Allow it again to let it run."
}

// Where a webhook posts, on which events, for which tools, how long it may take and whether its posts are signed
func (s *screen) webhookFacts(target *hooksv1.Webhook) []string {
	t := s.kit.Env.Theme
	lines := []string{"Posts what the agent does to " + target.GetUrl() + "."}
	var on []string
	for _, name := range target.GetEvents() {
		on = append(on, eventOf(name).title)
	}
	lines = append(lines, "On: "+strings.Join(on, ", ")+".")
	if matcher := target.GetMatcher(); matcher != "" {
		lines = append(lines, "Only for the tools "+matcher+".")
	}
	lines = append(lines, "May take up to "+seconds(float64(target.GetTimeout()))+".")
	if target.GetSigned() {
		lines = append(lines, "Each post is signed with your secret.")
	} else {
		lines = append(lines, render.Dim("Posts are not signed.", t))
	}
	return append(lines, render.Dim("Webhooks are kept under Hooks › Shell hooks › Webhooks in settings.", t))
}

// Opens the webhooks setting in place of the screen
func (s *screen) webhooks() tea.Cmd {
	return tea.Batch(s.kit.WM.DismissCard(s.hub), s.kit.Open(configapp.Edit, profile.Join(s.profile, shellconfig.Outbound)))
}

// One hook over the list: what it is, then what can be done to it
func (s *screen) page(hook *hooksv1.Hook) tea.Cmd {
	type step struct {
		title, about string
		// Word on the button that runs it
		verb string
		run  func(l *cards.List) tea.Cmd
	}
	t := s.kit.Env.Theme
	current := func() *hooksv1.Hook {
		for _, have := range s.listing.GetHooks() {
			if have.GetEvent() == hook.GetEvent() && have.GetCommand() == hook.GetCommand() {
				return have
			}
		}
		return hook
	}
	steps := func() []step {
		now := current()
		toggle := step{"Allow", "Let it run. A hook runs with your full rights", "allow", func(l *cards.List) tea.Cmd { return s.toggle(l, now) }}
		if toggleLabel(now) != "allow" {
			toggle = step{"Stop allowing", "It stays in your settings but no longer runs", "stop allowing", func(l *cards.List) tea.Cmd { return s.toggle(l, now) }}
		}
		out := []step{toggle, {"Test", "Run it once on a sample event and show what it answers", "test", func(*cards.List) tea.Cmd { return s.test(now, "") }}}
		if toolEvent(now.GetEvent()) {
			out = append(out, step{"Test with a tool", "Run it on a sample call of a tool you name", "test", func(*cards.List) tea.Cmd { return s.testTool(now) }})
		}
		return append(out, step{"Check", "See whether it can run, is allowed, is unchanged and answers well", "check", func(*cards.List) tea.Cmd { return s.check(now) }},
			step{"Remove", "Take it out of your settings", "remove", func(l *cards.List) tea.Cmd { return s.remove(l, now) }})
	}
	var shown []step
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   hook.GetCommand(),
		Glyph:   t.Glyphs.Terminal,
		Key:     "tui:hooks:" + hook.GetEvent() + ":" + hook.GetCommand(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return s.facts(current()) },
		Columns: []render.Column{{}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			shown = steps()
			cells := make([][]string, len(shown))
			for i, st := range shown {
				cells[i] = []string{st.title, render.Dim(st.about, t)}
			}
			return cells
		},
		Detail: func(i int) []string {
			if list.Shows(1) || i < 0 || i >= len(shown) {
				return nil
			}
			return []string{shown[i].about + "."}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: shown[i].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd { return shown[i].run(l) },
		},
	})
	list.ToggleDetail()
	return s.hub.Push(list)
}

// Form of a new hook: the event it runs on, its command, and behind more options the tools it is for and how long it may take
func (s *screen) add() tea.Cmd {
	names := slices.Clone(s.listing.GetEvents())
	slices.SortStableFunc(names, func(a, b string) int { return place(a) - place(b) })
	chosen := ""
	if len(names) > 0 {
		chosen = names[0]
	}
	fields := func(on string, values map[string]string) []cards.Field {
		choice := cards.Field{ID: "event", Label: "Event", Kind: cards.FieldChoice, Value: on, Required: true}
		for _, name := range names {
			e := eventOf(name)
			choice.Choices = append(choice.Choices, cards.Choice{Value: name, Label: e.title, Meaning: e.about})
		}
		out := []cards.Field{choice,
			{ID: "command", Label: "Command", Value: values["command"], Required: true, Help: "The path of a script, or a command line.\nIt gets what happened as JSON on its input."}}
		if toolEvent(on) {
			out = append(out, cards.Field{ID: "tools", Label: "Tools", Value: values["tools"], More: true,
				Help: "A pattern of tool names, such as terminal or web_*.\nLeave empty for every tool."})
		}
		return append(out, cards.Field{ID: "seconds", Label: "Time limit", Kind: cards.FieldNumber, Value: values["seconds"], More: true,
			Help:  "Seconds the command may take, from 1 to " + strconv.Itoa(shellhooks.MaxTimeout) + ".\nLeave empty for " + strconv.Itoa(shellhooks.DefaultTimeout) + " seconds.",
			Check: limit})
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  "New hook",
		Intro:  []string{"A hook runs your command when the agent acts. Choose when it runs, then the command."},
		Fields: fields(chosen, nil),
		Submit: "Add",
		Pick: func(f *cards.Form, field cards.Field) tea.Cmd {
			return s.pickEvent(field, func(name string) tea.Cmd { return f.Commit(field.ID, name) })
		},
		Changed: func(f *cards.Form, field cards.Field) tea.Cmd {
			if field.ID == "event" && field.Value != chosen {
				chosen = field.Value
				f.SetFields(fields(chosen, f.Values()))
			}
			return nil
		},
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			timeout, _ := strconv.Atoi(strings.TrimSpace(values["seconds"]))
			command := strings.TrimSpace(values["command"])
			told := "Added " + command + ". It runs once you allow it."
			if s.listing.GetAcceptAll() {
				told = "Added " + command + "."
			}
			return s.change(&hooksv1.ChangeRequest{Verb: "add", Event: values["event"], Command: command, Matcher: strings.TrimSpace(values["tools"]),
				Timeout: int32(timeout)}, told)
		},
	})
	return s.hub.Push(form)
}

// What is wrong with a time limit typed for a hook, nothing for an empty one or whole seconds within the bounds
func limit(typed string) string {
	typed = strings.TrimSpace(typed)
	if typed == "" {
		return ""
	}
	n, err := strconv.Atoi(typed)
	if err != nil || n < 1 || n > shellhooks.MaxTimeout {
		return "Must be whole seconds from 1 to " + strconv.Itoa(shellhooks.MaxTimeout) + "."
	}
	return ""
}

// Opens the events to choose one from, the one in the field marked
func (s *screen) pickEvent(field cards.Field, chose func(name string) tea.Cmd) tea.Cmd {
	choices := make([]tui.Choice, len(field.Choices))
	for i, choice := range field.Choices {
		choices[i] = tui.Choice{ID: choice.Value, Title: choice.Label, Detail: choice.Meaning}
	}
	return s.kit.Pick(s.hub, "When it runs", "choose", choices, field.Value, func(choice tui.Choice) tea.Cmd { return chose(choice.ID) })
}

// Lets every hook the settings set run, after a yes
func (s *screen) acceptAll() tea.Cmd {
	s.hub.Busy("Saving…")
	return tui.Call(s.hub, func() (*hooksv1.AcceptResponse, error) {
		return s.service.Accept(s.kit.Env.Ctx, &hooksv1.AcceptRequest{Profile: s.profile})
	}, func(*hooksv1.AcceptResponse) tea.Cmd {
		return tea.Batch(s.load(), s.hub.Done("Every hook in your settings is now allowed."))
	})
}

// Lets a hook run
func (s *screen) allow(hook *hooksv1.Hook) tea.Cmd {
	return s.change(&hooksv1.ChangeRequest{Verb: "allow", Event: hook.GetEvent(), Command: hook.GetCommand()}, hook.GetCommand()+" is now allowed.")
}

// Stops a hook from running, its settings staying
func (s *screen) revoke(hook *hooksv1.Hook) tea.Cmd {
	return s.change(&hooksv1.ChangeRequest{Verb: "revoke", Event: hook.GetEvent(), Command: hook.GetCommand()},
		hook.GetCommand()+" is no longer allowed. It stays in your settings.")
}

// Takes a hook out of the settings
func (s *screen) drop(hook *hooksv1.Hook) tea.Cmd {
	return s.change(&hooksv1.ChangeRequest{Verb: "remove", Event: hook.GetEvent(), Command: hook.GetCommand()}, "Removed "+hook.GetCommand()+".")
}

// Lets a hook run after a yes, or stops an allowed one at once
func (s *screen) toggle(l *cards.List, hook *hooksv1.Hook) tea.Cmd {
	if toggleLabel(hook) != "allow" {
		return s.revoke(hook)
	}
	l.Confirm("Allow "+hook.GetCommand()+"? It runs with your full rights.", func() tea.Cmd { return s.allow(hook) })
	return nil
}

// Takes a hook out of the settings after a yes
func (s *screen) remove(l *cards.List, hook *hooksv1.Hook) tea.Cmd {
	l.Confirm("Remove "+hook.GetCommand()+"? It leaves your settings and no longer runs.", func() tea.Cmd { return s.drop(hook) })
	return nil
}

// Length of time in words from a count of seconds, to the millisecond
func seconds(value float64) string {
	return config.Span(time.Duration(math.Round(value*1000)) * time.Millisecond)
}

// Lines telling how one run of a hook's command ended
func (s *screen) runLines(run *hooksv1.Run) []string {
	t := s.kit.Env.Theme
	var out []string
	switch {
	case run.GetError() != "":
		out = append(out, render.Bad("It couldn't run: "+run.GetError(), t))
	case run.GetTimedOut():
		out = append(out, render.Bad("It didn't finish within "+seconds(run.GetSeconds())+".", t))
	case run.GetExited() && run.GetExitCode() != 0:
		out = append(out, render.Warm("It ended with code "+strconv.Itoa(int(run.GetExitCode()))+" after "+seconds(run.GetSeconds())+".", t))
	case run.GetExited():
		out = append(out, render.Good("It ended well after "+seconds(run.GetSeconds())+".", t))
	}
	if text := strings.TrimSpace(run.GetStdout()); text != "" {
		out = append(out, "Output: "+text)
	}
	if text := strings.TrimSpace(run.GetStderr()); text != "" {
		out = append(out, "Errors: "+text)
	}
	if run.GetDirective() == "" {
		return append(out, "It answered nothing.")
	}
	return append(out, "It answered: "+run.GetDirective())
}

// Page of the hub showing the lines
func (s *screen) showPage(title, key string, lines []string) tea.Cmd {
	return s.hub.Push(cards.NewPage(s.kit.Env, cards.PageSpec{Title: title, Key: key, Lines: func(int) []string { return lines }}))
}

// Runs a hook once on a sample event, the tool given named in it, and shows how it ended
func (s *screen) test(hook *hooksv1.Hook, tool string) tea.Cmd {
	s.hub.Busy("Running " + hook.GetCommand() + "…")
	return tui.Call(s.hub, func() (*hooksv1.TestResponse, error) {
		return s.service.Test(s.kit.Env.Ctx, &hooksv1.TestRequest{Profile: s.profile, Event: hook.GetEvent(), Command: hook.GetCommand(), Tool: tool})
	}, func(response *hooksv1.TestResponse) tea.Cmd {
		s.hub.Quiet()
		t := s.kit.Env.Theme
		lines := []string{render.Heading(hook.GetCommand(), t), render.Dim("Ran on a sample of "+eventOf(hook.GetEvent()).title+".", t), ""}
		if tool != "" {
			lines[1] = render.Dim("Ran on a sample call of "+tool+".", t)
		}
		for _, tested := range response.GetRuns() {
			lines = append(lines, s.runLines(tested.GetRun())...)
		}
		return s.showPage("Test", "tui:hooks:test", lines)
	})
}

// Asks the tool a sample call names, then runs the hook on it
func (s *screen) testTool(hook *hooksv1.Hook) tea.Cmd {
	field := cards.Field{Label: "Tool", Help: "The name of the tool the sample call is for, such as terminal."}
	return s.kit.Ask(s.hub, "Test with a tool", field, "Test", func(tool string) tea.Cmd { return s.test(hook, tool) })
}

// Form that runs every hook of an event on a sample of it: the event, the tool a sample call names, and fields laid over the sample
func (s *screen) testEvent() tea.Cmd {
	names := slices.Clone(s.listing.GetEvents())
	slices.SortStableFunc(names, func(a, b string) int { return place(a) - place(b) })
	chosen := ""
	for _, hook := range s.listing.GetHooks() {
		if chosen == "" || place(hook.GetEvent()) < place(chosen) {
			chosen = hook.GetEvent()
		}
	}
	fields := func(on string, values map[string]string) []cards.Field {
		choice := cards.Field{ID: "event", Label: "Event", Kind: cards.FieldChoice, Value: on, Required: true}
		for _, name := range names {
			e := eventOf(name)
			choice.Choices = append(choice.Choices, cards.Choice{Value: name, Label: e.title, Meaning: e.about})
		}
		out := []cards.Field{choice}
		if toolEvent(on) {
			out = append(out, cards.Field{ID: "tool", Label: "Tool", Value: values["tool"],
				Help: "The tool the sample call is for, such as terminal.\nOnly the hooks for that tool run. Leave empty to run every hook of the event."})
		}
		return append(out, cards.Field{ID: "payload", Label: "Your data", Value: values["payload"],
			Help:  "Fields laid over the sample, as JSON such as {\"command\": \"ls\"}.\nLeave empty to use the sample as it is.",
			Check: object})
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  "Test an event",
		Intro:  []string{"Runs every hook of an event once, on a sample of what happens then."},
		Fields: fields(chosen, nil),
		Submit: "Test",
		Pick: func(f *cards.Form, field cards.Field) tea.Cmd {
			return s.pickEvent(field, func(name string) tea.Cmd { return f.Commit(field.ID, name) })
		},
		Changed: func(f *cards.Form, field cards.Field) tea.Cmd {
			if field.ID == "event" && field.Value != chosen {
				chosen = field.Value
				f.SetFields(fields(chosen, f.Values()))
			}
			return nil
		},
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			name, tool, payload := values["event"], strings.TrimSpace(values["tool"]), strings.TrimSpace(values["payload"])
			s.hub.Busy("Running the hooks of " + eventOf(name).title + "…")
			return tui.Call(s.hub, func() (*hooksv1.TestResponse, error) {
				return s.service.Test(s.kit.Env.Ctx, &hooksv1.TestRequest{Profile: s.profile, Event: name, Tool: tool, Payload: payload})
			}, func(response *hooksv1.TestResponse) tea.Cmd {
				s.hub.Quiet()
				t := s.kit.Env.Theme
				lines := []string{render.Dim("Ran on a sample of "+eventOf(name).title+".", t)}
				if len(response.GetRuns()) == 0 {
					lines = append(lines, "", "No hook runs on this event.")
				}
				for _, tested := range response.GetRuns() {
					lines = append(append(lines, "", render.Heading(tested.GetCommand(), t)), s.runLines(tested.GetRun())...)
				}
				return tea.Batch(s.hub.Close(form), s.showPage("Test", "tui:hooks:test-event", lines))
			})
		},
	})
	return s.hub.Push(form)
}

// What is wrong with fields typed to lay over a sample, nothing for none or a JSON object
func object(typed string) string {
	if strings.TrimSpace(typed) == "" {
		return ""
	}
	var fields map[string]any
	if json.Unmarshal([]byte(typed), &fields) != nil {
		return "Must be a JSON object, such as {\"command\": \"ls\"}."
	}
	return ""
}

// Lines telling what a check of one hook found
func (s *screen) checkLines(c *hooksv1.Checked) []string {
	t := s.kit.Env.Theme
	var lines []string
	if c.GetRunnable() {
		lines = append(lines, render.Good("The script is there and can run.", t))
	} else {
		lines = append(lines, render.Bad("The script is missing or can't run.", t))
	}
	switch {
	case c.GetAllowed() && c.GetApprovedAt() != "" && c.GetApprovedAt() != "?":
		lines = append(lines, render.Good("You allowed it on "+c.GetApprovedAt()+".", t))
	case c.GetAllowed():
		lines = append(lines, render.Good("You allowed it.", t))
	default:
		lines = append(lines, render.Bad("Not allowed, so it doesn't run.", t))
	}
	switch {
	case c.GetChanged():
		lines = append(lines, render.Bad(changed(c.GetChangedWas(), c.GetChangedNow()), t))
	case c.GetCompared():
		lines = append(lines, render.Good("Unchanged since you allowed it.", t))
	}
	if run := c.GetRun(); run != nil {
		switch c.GetOutput() {
		case "empty":
			lines = append(lines, render.Good("It ran and answered nothing, so it only watches.", t))
		case "json":
			lines = append(lines, render.Good("It ran and its answer is well formed.", t))
		case "invalid":
			lines = append(lines, render.Bad("Its answer is not well formed: "+strings.TrimSpace(run.GetStdout()), t))
		default:
			lines = append(lines, s.runLines(run)[0])
		}
	}
	if n := int(c.GetProblems()); n > 0 {
		return append(lines, render.Bad(render.Count(n, "problem")+" found.", t))
	}
	return append(lines, render.Good("No problems found.", t))
}

// Checks a hook and shows what was found
func (s *screen) check(hook *hooksv1.Hook) tea.Cmd {
	s.hub.Busy("Checking " + hook.GetCommand() + "…")
	return tui.Call(s.hub, func() (*hooksv1.CheckResponse, error) {
		return s.service.Check(s.kit.Env.Ctx, &hooksv1.CheckRequest{Profile: s.profile, Event: hook.GetEvent(), Command: hook.GetCommand()})
	}, func(response *hooksv1.CheckResponse) tea.Cmd {
		s.hub.Quiet()
		lines := []string{render.Heading(hook.GetCommand(), s.kit.Env.Theme), ""}
		for _, c := range response.GetChecks() {
			lines = append(lines, s.checkLines(c)...)
		}
		return s.showPage("Check", "tui:hooks:check", lines)
	})
}

// Checks every hook and shows what was found for each, under its command and event
func (s *screen) checkAll() tea.Cmd {
	s.hub.Busy("Checking every hook…")
	return tui.Call(s.hub, func() (*hooksv1.CheckResponse, error) {
		return s.service.Check(s.kit.Env.Ctx, &hooksv1.CheckRequest{Profile: s.profile})
	}, func(response *hooksv1.CheckResponse) tea.Cmd {
		s.hub.Quiet()
		t := s.kit.Env.Theme
		var lines []string
		problems := 0
		for _, c := range response.GetChecks() {
			problems += int(c.GetProblems())
			lines = append(lines, "", render.Heading(c.GetCommand(), t)+render.Dim(" · "+eventOf(c.GetEvent()).title, t))
			lines = append(lines, s.checkLines(c)...)
		}
		summary := render.Good("Every hook looks healthy.", t)
		if problems > 0 {
			summary = render.Bad(render.Count(problems, "problem")+" found.", t)
		}
		return s.showPage("Check every hook", "tui:hooks:check-all", append([]string{summary}, lines...))
	})
}
