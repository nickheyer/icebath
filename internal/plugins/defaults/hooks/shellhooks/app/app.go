// The page of Hooks: every hook under the event it runs on with whether it may run, every webhook with whether its posts are signed, adding
// either in a flow, the page of each, checking every hook and testing an event, and the options of the area
package app

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	hooksconfig "github.com/nickheyer/icebath/internal/hooks/config"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	"github.com/nickheyer/icebath/internal/profile"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	hooksv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1/hooksv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1/toolsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of Hooks
const Screen = "hooks"

// Keymap context of the rows of hooks
const hooksContext = "hooks"

var actions = []keys.Action{
	{ID: "hooks.allow", Context: hooksContext, Label: "allow", Doc: "Allow the hook in hand, or stop allowing it", Default: []string{"t"}},
	{ID: "hooks.test", Context: hooksContext, Label: "test", Doc: "Run the hook in hand once on a sample of its event", Default: []string{"r"}},
	{ID: "hooks.check", Context: hooksContext, Label: "check", Doc: "Check that the hook in hand can run, is allowed, is unchanged and answers well",
		Default: []string{"c"}},
}

// Headings of the page besides the events: the webhooks, then the options
const (
	webhooksGroup = "Webhooks"
	optionsGroup  = "Options"
)

// What the argument of the screen opens over the page: a hook, a webhook, a section of its options, or a job
const (
	hookOf        = "hook:"
	webhookOf     = "webhook:"
	sectionOf     = "section:"
	argAdd        = "add"
	argAddWebhook = "add-webhook"
	argTest       = "test"
	argCheck      = "check"
)

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := hooksv1connect.NewHooksServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	picker := &tools{service: toolsv1connect.NewToolServiceClient(conn)}
	return []tui.Screen{{
		ID:      Screen,
		Group:   tui.GroupAbilities,
		Order:   100,
		Title:   "Hooks",
		Short:   "Run your own commands when the agent acts",
		Slash:   []string{"hooks"},
		Usage:   "[@profile] [add|add-webhook|test|check]",
		Area:    hooksconfig.Key,
		Actions: actions,
		Where:   map[string]string{hooksContext: "hooks page"},
		Pages:   func(kit *tui.Kit, name string) []tui.Page { return hookPages(kit, service, name) },
		Pickers: map[string]tui.Picker{shellconfig.PickTools: {Open: picker.open, Shown: picker.shown}},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, rest := profile.Split(argument)
			return (&page{kit: kit, service: service, settings: settings, tools: picker, profile: name}).open(rest)
		},
	}}
}

// The page of the area on one profile, and the views it opened over itself
type page struct {
	kit      *tui.Kit
	service  hooksv1connect.HooksServiceClient
	settings configv1connect.ConfigServiceClient
	tools    *tools
	// Profile the page was opened on, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	options *configapp.Rows
	listing *hooksv1.ListResponse
	loaded  bool
	err     error
	// Rows as last listed
	rows []row
	// Lists of the pages opened over the page, read again with it
	views cards.Watched
	// Row put in hand once the hooks are read again: a hook by its event and command, a webhook by its key
	focus string
}

// Row of the page: a hook, a webhook, a row that adds either, something done to every hook, an option, a section of options or the row of
// the advanced options
type row struct {
	hook       *hooksv1.Hook
	webhook    *hooksv1.Webhook
	addHook    bool
	addWebhook bool
	act        *act
	setting    *configv1.Setting
	section    string
	advanced   bool
}

// Something a row does, its name, what it does, the word on its button, and whether it opens a view of its own
type act struct {
	title, about, verb string
	opens              bool
	run                func(l *cards.List) tea.Cmd
}

// Shows the page, then reads the hooks, the tools they may name and the options; an argument opens what it names over the page
func (p *page) open(argument string) tea.Cmd {
	t := p.kit.Env.Theme
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Hooks",
		Glyph:   t.Glyphs.Terminal,
		Key:     Screen,
		Context: cards.HubContext,
		Shared:  []string{hooksContext},
		Primary: "hub.use",
		Intro:   p.intro,
		Columns: configapp.Columns,
		Rows: func() [][]string {
			p.rows = p.order()
			cells := make([][]string, len(p.rows))
			for i, r := range p.rows {
				cells[i] = p.cells(r)
			}
			return cells
		},
		Group: func(i int) string { return p.group(p.rows[i]) },
		Detail: func(i int) []string {
			if i < 0 || i >= len(p.rows) {
				return nil
			}
			return p.detail(p.rows[i], p.list.Cut(i, 2))
		},
		Empty:   func() string { return tui.Waiting(p.loaded, p.err, "") },
		OnFocus: func(*cards.List) tea.Cmd { return p.load() },
		Free:    map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints:   func(_ *cards.List, i int) []wm.Hint { return p.hints(i) },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":     func(l *cards.List, i int) tea.Cmd { return p.use(l, p.rows[i]) },
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return p.remove(l, p.rows[i]) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return p.addHook() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return tea.Batch(p.load(), p.options.Load(nil)) },
			"hooks.allow": p.withHook(func(l *cards.List, hook *hooksv1.Hook) tea.Cmd { return p.toggle(l, hook) }),
			"hooks.test":  p.withHook(func(_ *cards.List, hook *hooksv1.Hook) tea.Cmd { return p.test(hook) }),
			"hooks.check": p.withHook(func(_ *cards.List, hook *hooksv1.Hook) tea.Cmd { return p.check(hook) }),
		},
	})
	p.list.ToggleDetail()
	hub, cmd := p.kit.Show(Screen, p.list)
	p.hub = hub
	p.options = configapp.NewRows(p.kit, hub, p.settings, p.profile)
	p.options.Watch(p.list)
	// What the argument opens may be a section of the options, so they are read first
	return tea.Batch(cmd, p.tools.load(p.kit, p.profile, p.refresh), p.options.Load(func() tea.Cmd {
		return p.read(func() tea.Cmd { return p.reach(strings.TrimSpace(argument)) })
	}))
}

// Opens what the argument names over the page
func (p *page) reach(argument string) tea.Cmd {
	switch {
	case argument == "":
		return nil
	case argument == argAdd:
		return p.addHook()
	case argument == argAddWebhook:
		return p.addWebhook()
	case argument == argTest:
		return p.testEvent()
	case argument == argCheck:
		return p.checkAll()
	case strings.HasPrefix(argument, hookOf):
		event, command, _ := strings.Cut(strings.TrimPrefix(argument, hookOf), " ")
		if p.hook(event, command) != nil {
			return p.hookPage(event, command)
		}
		return p.hub.Fail("There is no hook " + command + " on " + shellconfig.PointOf(event).Title + " now.")
	case strings.HasPrefix(argument, webhookOf):
		key := strings.TrimPrefix(argument, webhookOf)
		if p.webhook(key) != nil {
			return p.webhookPage(key)
		}
		return p.hub.Fail("That webhook is no longer there.")
	case strings.HasPrefix(argument, sectionOf):
		return p.sectionPage(strings.TrimPrefix(argument, sectionOf))
	}
	return p.hub.Fail("The Hooks page has nothing called " + argument + ".")
}

// First line of the page, what the area is; in the warm colour the hooks left out under names that are no event; and while there are no
// hooks that there are none
func (p *page) intro() []string {
	lines := []string{"Your own commands that run when the agent acts."}
	if ignored := p.listing.GetIgnored(); len(ignored) > 0 {
		lines = append(lines, render.Warm(ignoredWords(ignored), p.kit.Env.Theme))
	}
	if p.loaded && p.err == nil && len(p.listing.GetHooks()) == 0 {
		lines = append(lines, "No hooks yet.")
	}
	return lines
}

// Hooks left out under names that are no event, in a sentence
func ignoredWords(names []string) string {
	if len(names) == 1 {
		return "The hooks under " + names[0] + " in your settings file are left out, because icebath has no event of that name."
	}
	return "The hooks under " + strs.And(names) + " in your settings file are left out, because icebath has no events of those names."
}

// Reads the hooks again
func (p *page) load() tea.Cmd { return p.read(nil) }

// Reads the hooks again, then runs then
func (p *page) read(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		listing, err := p.service.List(p.kit.Env.Ctx, &hooksv1.ListRequest{Profile: p.profile})
		return func() tea.Cmd {
			p.err, p.loaded = err, true
			if err == nil {
				p.listing = listing
			}
			p.refresh()
			if then != nil {
				return then()
			}
			return nil
		}
	})
}

// Shows the hooks as last read on the page and on every page opened over it, the row asked for in hand
func (p *page) refresh() {
	p.list.Refresh()
	p.views.Refresh()
	if p.focus == "" {
		return
	}
	for i, r := range p.rows {
		if p.focusOf(r) == p.focus && p.list.Select(i) {
			p.focus = ""
			return
		}
	}
}

// What a row is put in hand by
func (p *page) focusOf(r row) string {
	switch {
	case r.hook != nil:
		return hookOf + r.hook.GetEvent() + " " + r.hook.GetCommand()
	case r.webhook != nil:
		return webhookOf + r.webhook.GetKey()
	}
	return ""
}

// Hook of the event and command as last read, nil for none
func (p *page) hook(event, command string) *hooksv1.Hook {
	for _, hook := range p.listing.GetHooks() {
		if hook.GetEvent() == event && hook.GetCommand() == command {
			return hook
		}
	}
	return nil
}

// Webhook of the key as last read, nil for none
func (p *page) webhook(key string) *hooksv1.Webhook {
	for _, webhook := range p.listing.GetWebhooks() {
		if webhook.GetKey() == key {
			return webhook
		}
	}
	return nil
}

// Hooks under their events in the order the events are listed
func (p *page) sorted() []*hooksv1.Hook {
	shell := slices.Clone(p.listing.GetHooks())
	slices.SortStableFunc(shell, func(a, b *hooksv1.Hook) int { return shellconfig.Place(a.GetEvent()) - shellconfig.Place(b.GetEvent()) })
	return shell
}

// Rows of the page once the hooks are read: the hooks by event, the row that adds one, what is done to every hook while there are any, the
// webhooks and the row that adds one, then the options, the section of long output and the row of the advanced options
func (p *page) order() []row {
	if !p.loaded || p.err != nil {
		return nil
	}
	var rows []row
	shell := p.sorted()
	for _, hook := range shell {
		rows = append(rows, row{hook: hook})
	}
	rows = append(rows, row{addHook: true})
	if len(shell) > 0 {
		for _, a := range p.acts(shell) {
			rows = append(rows, row{act: a})
		}
	}
	for _, webhook := range p.listing.GetWebhooks() {
		rows = append(rows, row{webhook: webhook})
	}
	rows = append(rows, row{addWebhook: true})
	everyday, _ := p.options.Section(hooksconfig.Key)
	for _, setting := range everyday {
		rows = append(rows, row{setting: setting})
	}
	if p.options.Info(hooksconfig.SpillKey) != nil {
		rows = append(rows, row{section: hooksconfig.SpillKey})
	}
	if _, _, ok := p.options.Advanced(hooksconfig.Key); ok {
		rows = append(rows, row{advanced: true})
	}
	return rows
}

// What is done to every hook: allowing those that wait for it, checking them all, and testing an event
func (p *page) acts(shell []*hooksv1.Hook) []*act {
	var out []*act
	waiting := 0
	for _, hook := range shell {
		if !hook.GetAllowed() && !hook.GetUnasked() {
			waiting++
		}
	}
	if waiting > 1 {
		out = append(out, &act{title: "Allow every hook", about: "Lets each hook that is not allowed run, with your full rights.", verb: "allow",
			run: func(l *cards.List) tea.Cmd {
				l.Confirm(cards.Question{Text: "Allow " + render.Count(waiting, "hook") + " to run? Each runs with your full rights when its event happens.",
					Verb: "Allow"}, p.acceptAll)
				return nil
			}})
	}
	return append(out,
		&act{title: "Check every hook", about: "Checks that each hook can run, is allowed, is unchanged and answers well.", verb: "check",
			run: func(*cards.List) tea.Cmd { return p.checkAll() }},
		&act{title: "Test an event", about: "Runs every hook of an event once, on a sample of what happens then.", verb: "test", opens: true,
			run: func(*cards.List) tea.Cmd { return p.testEvent() }})
}

// Heading a row is listed under: a hook the title of its event, the rows after the hooks none so they sit with them, the webhooks theirs,
// and the options theirs
func (p *page) group(r row) string {
	switch {
	case r.hook != nil:
		return shellconfig.PointOf(r.hook.GetEvent()).Title
	case r.webhook != nil, r.addWebhook:
		return webhooksGroup
	case r.setting != nil, r.section != "", r.advanced:
		return optionsGroup
	}
	return ""
}

// Cells of a row: its name, the mark of a changed value or of a row that opens, and its state or value
func (p *page) cells(r row) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case r.hook != nil:
		return []string{r.hook.GetCommand(), "", hookState(r.hook.GetState(), t)}
	case r.webhook != nil:
		return []string{r.webhook.GetLabel(), "", webhookState(r.webhook, t)}
	case r.addHook:
		return []string{"Add a hook", opens, ""}
	case r.addWebhook:
		return []string{"Add a webhook", opens, ""}
	case r.act != nil && r.act.opens:
		return []string{r.act.title, opens, ""}
	case r.act != nil:
		return []string{r.act.title, "", ""}
	case r.setting != nil:
		return p.options.Cells(r.setting)
	case r.section != "":
		return []string{p.options.Info(r.section).GetTitle(), opens, render.Dim(p.inside(r.section), t)}
	}
	cells, _, _ := p.options.Advanced(hooksconfig.Key)
	return cells
}

// State of a hook in its words, in the colour of what it means
func hookState(state string, t theme.Theme) string {
	switch state {
	case "Allowed":
		return render.Good(state, t)
	case "Not allowed":
		return render.Warm(state, t)
	}
	return render.Bad(state, t)
}

// State of a webhook in its words, dim while its posts are not signed
func webhookState(webhook *hooksv1.Webhook, t theme.Theme) string {
	if webhook.GetSigned() {
		return webhook.GetState()
	}
	return render.Dim(webhook.GetState(), t)
}

// How many settings a row that opens a section holds, and how many were changed
func (p *page) inside(key string) string {
	everyday, advanced := p.options.Own(key)
	all := append(everyday, advanced...)
	changed := 0
	for _, setting := range all {
		if setting.GetSource() == configv1.Source_SOURCE_FILE {
			changed++
		}
	}
	said := render.Count(len(all), "setting")
	if changed > 0 {
		said += ", " + strconv.Itoa(changed) + " changed"
	}
	return said
}

// Lines under the list about the row in hand, never what the row or the first line says
func (p *page) detail(r row, cut bool) []string {
	t := p.kit.Env.Theme
	switch {
	case r.hook != nil:
		return p.hookFacts(r.hook)
	case r.webhook != nil:
		return p.webhookFacts(r.webhook)
	case r.addHook:
		return []string{"A command of yours that runs when the agent acts, such as a check before each tool call."}
	case r.addWebhook:
		return []string{"A web address that is told what the agent does, such as a bot of your team."}
	case r.act != nil:
		return []string{r.act.about}
	case r.setting != nil:
		return p.options.Detail(r.setting, cut)
	case r.section != "":
		return []string{strs.Full(p.options.Info(r.section).GetDescription())}
	}
	return []string{render.Dim("Settings that few people change.", t)}
}

// What the list says of a hook under the rows: what its event is, and what its state means for it
func (p *page) hookFacts(hook *hooksv1.Hook) []string {
	t := p.kit.Env.Theme
	lines := []string{strs.Full(shellconfig.PointOf(hook.GetEvent()).About)}
	switch {
	case hook.GetChanged():
		lines = append(lines, render.Warm("Its script changed on "+when(hook.GetChangedNow())+", after you allowed it. It still runs.", t))
	case hook.GetUnasked():
		lines = append(lines, "It runs because Run without asking is on.")
	case !hook.GetAllowed():
		lines = append(lines, "It doesn't run until you allow it.")
	}
	return lines
}

// What the list says of a webhook under the rows: where it posts and which events, and why its posts are not signed
func (p *page) webhookFacts(webhook *hooksv1.Webhook) []string {
	t := p.kit.Env.Theme
	said := "Posts " + eventWords(webhook.GetEvents())
	if webhook.GetName() != "" {
		said += " to " + webhook.GetUrl()
	}
	lines := []string{said + "."}
	if why := webhook.GetUnsigned(); why != "" {
		lines = append(lines, render.Warm("Its posts are not signed, because "+why+".", t))
	}
	return lines
}

// Buttons for the row in hand: what enter does, the keys of a hook, remove where a row can be removed, then add
func (p *page) hints(i int) []wm.Hint {
	add := wm.Hint{Action: "hub.add", Label: "add"}
	if i < 0 || i >= len(p.rows) {
		return []wm.Hint{add}
	}
	switch r := p.rows[i]; {
	case r.hook != nil:
		allow := "allow"
		if r.hook.GetState() == "Allowed" && !r.hook.GetUnasked() {
			allow = "stop allowing"
		}
		return []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hooks.allow", Label: allow}, {Action: "hooks.test", Label: "test"},
			{Action: "hooks.check", Label: "check"}, {Action: "hub.remove", Label: "remove"}, add}
	case r.webhook != nil:
		return []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.remove", Label: "remove"}, add}
	case r.addHook, r.addWebhook:
		return []wm.Hint{{Action: "hub.use", Label: "add"}}
	case r.act != nil:
		return []wm.Hint{{Action: "hub.use", Label: r.act.verb}, add}
	case r.setting != nil:
		return append(p.options.Hints(r.setting), add)
	}
	return []wm.Hint{{Action: "hub.use", Label: "open"}, add}
}

// What enter does on a row
func (p *page) use(l *cards.List, r row) tea.Cmd {
	switch {
	case r.hook != nil:
		return p.hookPage(r.hook.GetEvent(), r.hook.GetCommand())
	case r.webhook != nil:
		return p.webhookPage(r.webhook.GetKey())
	case r.addHook:
		return p.addHook()
	case r.addWebhook:
		return p.addWebhook()
	case r.act != nil:
		return r.act.run(l)
	case r.setting != nil:
		return p.options.Change(r.setting)
	case r.section != "":
		return p.sectionPage(r.section)
	}
	_, open, _ := p.options.Advanced(hooksconfig.Key)
	return open()
}

// What ctrl+d does on a row: removes a hook or a webhook after a question, or puts a changed option back to its default
func (p *page) remove(l *cards.List, r row) tea.Cmd {
	switch {
	case r.hook != nil:
		return p.removeHook(l, r.hook)
	case r.webhook != nil:
		return p.removeWebhook(l, r.webhook)
	case r.setting != nil:
		return p.options.Reset(l, r.setting)
	}
	return nil
}

// Action on the hook of the row in hand, nothing on any other row
func (p *page) withHook(do func(l *cards.List, hook *hooksv1.Hook) tea.Cmd) func(*cards.List, int) tea.Cmd {
	return func(l *cards.List, i int) tea.Cmd {
		if i < 0 || i >= len(p.rows) || p.rows[i].hook == nil {
			return nil
		}
		return do(l, p.rows[i].hook)
	}
}

// Sends one change of a hook, shows the hooks it answers and says what was done; then runs once it was kept
func (p *page) change(request *hooksv1.ChangeRequest, then func(response *hooksv1.ChangeResponse) tea.Cmd) tea.Cmd {
	request.Profile = p.profile
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*hooksv1.ChangeResponse, error) {
		return p.service.Change(p.kit.Env.Ctx, request)
	}, func(response *hooksv1.ChangeResponse) tea.Cmd {
		p.listing = response.GetHooks()
		p.refresh()
		cmds := []tea.Cmd{p.hub.Done(response.GetSaid())}
		if then != nil {
			cmds = append(cmds, then(response))
		}
		return tea.Batch(cmds...)
	})
}

// Lets a hook run after the question, or stops an allowed one at once
func (p *page) toggle(l *cards.List, hook *hooksv1.Hook) tea.Cmd {
	if hook.GetState() == "Allowed" && !hook.GetUnasked() {
		return p.change(&hooksv1.ChangeRequest{Verb: "revoke", Event: hook.GetEvent(), Command: hook.GetCommand()}, nil)
	}
	l.Confirm(cards.Question{Text: "Allow " + hook.GetCommand() + " to run on " + shellconfig.PointOf(hook.GetEvent()).Title + "? It runs with your full " +
		"rights each time.", Verb: "Allow"}, func() tea.Cmd {
		return p.change(&hooksv1.ChangeRequest{Verb: "allow", Event: hook.GetEvent(), Command: hook.GetCommand()}, nil)
	})
	return nil
}

// Takes a hook out after the question, back on the page
func (p *page) removeHook(l *cards.List, hook *hooksv1.Hook) tea.Cmd {
	l.Confirm(cards.Question{Text: "Remove " + hook.GetCommand() + " from " + shellconfig.PointOf(hook.GetEvent()).Title + "? It no longer runs, and " +
		"its script stays on this computer.", Verb: "Remove", Deletes: true}, func() tea.Cmd {
		return p.change(&hooksv1.ChangeRequest{Verb: "remove", Event: hook.GetEvent(), Command: hook.GetCommand()}, func(*hooksv1.ChangeResponse) tea.Cmd {
			return p.hub.PopTo(p.list)
		})
	})
	return nil
}

// Takes a webhook out after the question, back on the page
func (p *page) removeWebhook(l *cards.List, webhook *hooksv1.Webhook) tea.Cmd {
	l.Confirm(cards.Question{Text: "Remove the webhook to " + webhook.GetUrl() + "? The address is no longer told what the agent does.",
		Verb: "Remove", Deletes: true}, func() tea.Cmd {
		p.hub.Busy("Saving…")
		return tui.Call(p.hub, func() (*hooksv1.ChangeResponse, error) {
			return p.service.ChangeWebhook(p.kit.Env.Ctx, &hooksv1.WebhookRequest{Profile: p.profile, Verb: "remove", Key: webhook.GetKey()})
		}, func(response *hooksv1.ChangeResponse) tea.Cmd {
			p.listing = response.GetHooks()
			p.refresh()
			return tea.Batch(p.hub.PopTo(p.list), p.hub.Done(response.GetSaid()), p.options.Load(nil))
		})
	})
	return nil
}

// Lets every hook that waits for it run, after the question
func (p *page) acceptAll() tea.Cmd {
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*hooksv1.AcceptResponse, error) {
		return p.service.Accept(p.kit.Env.Ctx, &hooksv1.AcceptRequest{Profile: p.profile})
	}, func(response *hooksv1.AcceptResponse) tea.Cmd {
		return tea.Batch(p.load(), p.hub.Done("Allowed "+render.Count(int(response.GetAllowed()), "hook")+"."))
	})
}

// Events as a person reads them in a sentence: their titles joined
func eventWords(events []string) string {
	titles := make([]string, len(events))
	for i, event := range events {
		titles[i] = shellconfig.PointOf(event).Title
	}
	return strs.And(titles)
}
