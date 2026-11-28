// Page of Approvals in the settings menu: when the agent asks, skipping approvals in the chat in hand, what runs without asking and the ways
// to add to it, the blocked commands, the check of a command and the rest of the options; and /approvals, the shortcut to them
package app

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/internal/app"
	approvalsconfig "github.com/nickheyer/icebath/internal/approvals/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	approvalsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/approvals/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/approvals/v1/approvalsv1connect"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of Approvals, the page of its area
const Screen = "approvals"

// Help under the field an allowed command is typed into
const patternHelp = "An exact command, or a pattern such as npm run * where * stands for anything.\nA command joined to others with && or ; never matches."

// Rows of the page that do something: allow a command, allow a kind of action, suggest commands, check a command
const (
	actCommand = "command"
	actKind    = "kind"
	actSuggest = "suggest"
	actCheck   = "check"
)

// Heading of what runs without asking
const allowedHeading = "Allowed without asking"

// Cells an allowed command or kind takes in its row before it is cut
const entryRoom = 40

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := approvalsv1connect.NewApprovalsServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{
		{
			ID:    Screen,
			Group: tui.GroupSafety,
			Order: 10,
			Title: "Approvals",
			Short: "Choose when the agent asks before it runs a command",
			Usage: "[@profile] [command|kind|suggest|check]",
			Area:  approvalsconfig.Key,
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				name, target := profile.Split(argument)
				p := &page{kit: kit, service: service, settings: settings, profile: name, session: session(kit)}
				return p.open(strings.ToLower(strings.TrimSpace(target)))
			},
			Pages: func(kit *tui.Kit, name string) []tui.Page {
				allowed := ""
				if listed, err := service.List(kit.Env.Ctx, &approvalsv1.ListRequest{Profile: name}); err == nil {
					allowed = strings.Join(listed.GetAllowed(), " ")
				}
				return []tui.Page{
					{Title: allowedHeading, About: "Commands and kinds of action that run without asking", Words: "allowlist allowed commands " + allowed},
					{Title: "Allow a command", About: "Let a command or a pattern run without asking", Argument: actCommand},
					{Title: "Allow a kind of action", About: "Let a whole kind of risky action run without asking", Argument: actKind},
					{Title: "Suggest commands", About: "Commands you approved often, which could run without asking", Argument: actSuggest},
					{Title: "Check a command", About: "See what the rules do with a command, without running it", Argument: actCheck},
				}
			},
		},
		{
			ID:    Screen + ".command",
			Short: "Choose when the agent asks before risky commands",
			Slash: []string{"approvals"},
			Usage: "[manual|smart|off] [--session]",
			Open:  func(kit *tui.Kit, argument string) tea.Cmd { return command(kit, service, argument) },
		},
	}
}

// Session of the chat in hand, empty when the window in hand shows none
func session(kit *tui.Kit) string {
	if shown, ok := kit.Focused().(tui.Sessioned); ok {
		return shown.Session()
	}
	return ""
}

// /approvals: alone it opens the page; a mode is the shortcut to When to ask, and with --session, or --session alone, to the switch that
// skips approvals in the chat in hand. Each answers with the note of its control
func command(kit *tui.Kit, service approvalsv1connect.ApprovalsServiceClient, argument string) tea.Cmd {
	name, rest := profile.Split(argument)
	mode, chat := "", false
	for _, word := range strings.Fields(strings.ToLower(rest)) {
		switch word {
		case "--session":
			chat = true
		case approvalsconfig.Manual, approvalsconfig.Smart, approvalsconfig.Off:
			mode = word
		default:
			return kit.WM.Toast("/approvals takes manual, smart or off, and --session for this chat alone.", wm.Warn, wm.DefaultToastTTL)
		}
	}
	say := func(note string, err error) tea.Cmd {
		if err != nil {
			return kit.WM.Toast(tui.Reason(err), wm.Error, wm.DefaultToastTTL)
		}
		return kit.WM.Toast(note, wm.Success, wm.DefaultToastTTL)
	}
	switch {
	case !chat && mode == "":
		return kit.Open(Screen, profile.Join(name, ""))
	case !chat:
		return tui.Async(func() func() tea.Cmd {
			response, err := service.Mode(kit.Env.Ctx, &approvalsv1.ModeRequest{Profile: name, Mode: mode})
			return func() tea.Cmd { return say(response.GetNote(), err) }
		})
	}
	id := session(kit)
	return tui.Async(func() func() tea.Cmd {
		on := mode == approvalsconfig.Off
		var note string
		var err error
		if mode == "" && id != "" {
			var listed *approvalsv1.ListResponse
			if listed, err = service.List(kit.Env.Ctx, &approvalsv1.ListRequest{Profile: name, Session: id}); err == nil {
				on = !listed.GetSkipped()
			}
		}
		if err == nil {
			var response *approvalsv1.SkipResponse
			response, err = service.Skip(kit.Env.Ctx, &approvalsv1.SkipRequest{Session: id, On: on})
			note = response.GetNote()
		}
		return func() tea.Cmd { return say(note, err) }
	})
}

// The page of Approvals for one profile
type page struct {
	kit      *tui.Kit
	service  approvalsv1connect.ApprovalsServiceClient
	settings configv1connect.ConfigServiceClient
	// Profile the page is of, the one in use when empty
	profile string
	// Session of the chat the page was opened from, empty for none
	session string
	hub     *cards.Hub
	root    *cards.List
	options *configapp.Rows
	views   cards.Watched
	listed  *approvalsv1.ListResponse
	loaded  bool
	err     error
}

// Row of the page: an option, the switch of the chat in hand, an entry of what runs without asking, a row that does something, or the row
// of the advanced options
type row struct {
	setting  *configv1.Setting
	skip     bool
	entry    *approvalsv1.Entry
	act      string
	advanced bool
}

// The first view: what the page is for and what wins over it, When to ask and the switch of the chat in hand, what runs without asking
// with the rows that add to it, then the blocked commands, the check of a command and the other options. Opened on one of the rows that do
// something, its view opens once the page is read
func (p *page) open(target string) tea.Cmd {
	t := p.kit.Env.Theme
	var shown []row
	p.root = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Approvals",
		Glyph:   t.Glyphs.Settings,
		Key:     Screen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   p.intro,
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.rows()
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = p.cells(r)
			}
			return cells
		},
		Group:  func(i int) string { return group(shown[i]) },
		Detail: func(i int) []string { return p.detail(shown[i], p.root.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "") },
		Free:   map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return []wm.Hint{{Action: "hub.add", Label: "allow a command"}}
			}
			return p.hints(shown[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd { return p.use(l, shown[i]) },
			"hub.add": func(*cards.List, int) tea.Cmd { return p.allowCommand() },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				switch r := shown[i]; {
				case r.entry != nil:
					return p.disallow(l, r.entry)
				case r.setting != nil:
					return p.options.Reset(l, r.setting)
				}
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load(nil) },
		},
	})
	p.root.ToggleDetail()
	hub, cmd := p.kit.Show(Screen, p.root)
	p.hub = hub
	p.options = configapp.NewRows(p.kit, hub, p.settings, p.profile)
	p.watch(p.root)
	return tea.Batch(cmd, p.load(func() tea.Cmd { return p.reach(target) }))
}

// Lines over the rows: what Approvals is for, then while something wins over When to ask what it is
func (p *page) intro() []string {
	lines := []string{"When the agent must ask before it runs a command."}
	if winning := p.listed.GetWinning(); p.loaded && p.err == nil && winning != "" {
		lines = append(lines, render.Warm(winning, p.kit.Env.Theme))
	}
	return lines
}

// Rows of the first view
func (p *page) rows() []row {
	if !p.loaded || p.err != nil {
		return nil
	}
	everyday, rare := p.options.Section(approvalsconfig.Key)
	var rows []row
	for _, setting := range everyday {
		if setting.GetKey() == approvalsconfig.Mode {
			rows = append(rows, row{setting: setting})
		}
	}
	if p.session != "" {
		rows = append(rows, row{skip: true})
	}
	for _, entry := range p.listed.GetEntries() {
		rows = append(rows, row{entry: entry})
	}
	rows = append(rows, row{act: actCommand}, row{act: actKind}, row{act: actSuggest})
	checked := false
	for _, setting := range everyday {
		switch setting.GetKey() {
		case approvalsconfig.Mode:
		case approvalsconfig.Deny:
			rows = append(rows, row{setting: setting}, row{act: actCheck})
			checked = true
		default:
			rows = append(rows, row{setting: setting})
		}
	}
	if !checked {
		rows = append(rows, row{act: actCheck})
	}
	if _, _, apart := p.options.Advanced(approvalsconfig.Key); apart {
		return append(rows, row{advanced: true})
	}
	for _, setting := range rare {
		rows = append(rows, row{setting: setting})
	}
	return rows
}

// Heading a row is listed under: none for When to ask and the switch of the chat, Allowed without asking for what runs without asking and
// the rows that add to it, Options for the rest
func group(r row) string {
	switch {
	case r.skip, r.setting.GetKey() == approvalsconfig.Mode:
		return ""
	case r.entry != nil, r.act == actCommand, r.act == actKind, r.act == actSuggest:
		return allowedHeading
	}
	return "Options"
}

func (p *page) cells(r row) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case r.setting != nil:
		return p.options.Cells(r.setting)
	case r.skip:
		return []string{approvalsconfig.SkipTitle, "", strs.OnOff(p.listed.GetSkipped())}
	case r.entry != nil:
		// A long command or kind is cut in its row and said whole under it
		return []string{ansi.Truncate(r.entry.GetTitle(), entryRoom, t.Glyphs.Ellipsis), "", render.Dim(r.entry.GetThrough(), t)}
	case r.act == actCommand:
		return []string{"Allow a command", opens, ""}
	case r.act == actKind:
		state := ""
		if len(p.kinds()) == 0 && len(p.listed.GetKinds()) > 0 {
			state = render.Dim("Every kind is allowed", t)
		}
		return []string{"Allow a kind of action", opens, state}
	case r.act == actSuggest:
		return []string{"Suggest commands", opens, ""}
	case r.act == actCheck:
		return []string{"Check a command", opens, ""}
	}
	cells, _, _ := p.options.Advanced(approvalsconfig.Key)
	return cells
}

// Lines under the rows about the row in hand
func (p *page) detail(r row, cut bool) []string {
	t := p.kit.Env.Theme
	switch {
	case r.setting != nil:
		return p.options.Detail(r.setting, cut)
	case r.skip:
		lines := []string{"Run every command of this chat without asking, until you turn this off.", render.Dim("Also /yolo", t)}
		if p.listed.GetLaunched() {
			lines = append(lines, render.Warm("Locked. icebath started with ICEBATH_YOLO_MODE set.", t))
		}
		return lines
	case r.entry != nil:
		return p.entryDetail(r.entry)
	case r.act == actCommand:
		return []string{"Let one command, or every command that matches a pattern, run without asking."}
	case r.act == actKind:
		return []string{"Let every action of one risky kind run without asking, such as changing files outside the project."}
	case r.act == actSuggest:
		return []string{"Commands you approved often, which you can let run without asking."}
	case r.act == actCheck:
		return []string{"See what the rules do with a command, without running it."}
	}
	return []string{"Settings of Approvals that few people change."}
}

// What an entry lets through and what asks about it
func (p *page) entryDetail(entry *approvalsv1.Entry) []string {
	t := p.kit.Env.Theme
	switch entry.GetKind() {
	case "kind":
		lines := []string{"The agent may " + entry.GetValue() + " without asking."}
		if about := entry.GetAbout(); about != "" {
			lines = append(lines, render.Dim(strings.TrimSuffix(about, ".")+".", t))
		}
		return lines
	case "pattern":
		return []string{"The agent runs every command that matches " + entry.GetValue() + " without asking.",
			render.Dim("* stands for anything, ? for one character.", t)}
	}
	return []string{"The agent runs " + entry.GetValue() + " without asking, exactly as written."}
}

func (p *page) hints(r row) []wm.Hint {
	switch {
	case r.setting != nil:
		return p.options.Hints(r.setting)
	case r.skip && p.listed.GetLaunched():
		return []wm.Hint{{Label: "locked"}}
	case r.skip && p.listed.GetSkipped():
		return []wm.Hint{{Action: "hub.use", Label: "turn off"}}
	case r.skip:
		return []wm.Hint{{Action: "hub.use", Label: "turn on"}}
	case r.entry != nil && r.entry.GetKind() == "kind":
		return []wm.Hint{{Action: "hub.use", Label: "remove"}, {Action: "hub.add", Label: "allow a command"}}
	case r.entry != nil:
		return []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.remove", Label: "remove"}, {Action: "hub.add", Label: "allow a command"}}
	}
	return []wm.Hint{{Action: "hub.use", Label: "open"}}
}

// What enter does on a row
func (p *page) use(l *cards.List, r row) tea.Cmd {
	switch {
	case r.setting != nil:
		return p.options.Change(r.setting)
	case r.skip:
		return p.skip(!p.listed.GetSkipped())
	case r.entry != nil && r.entry.GetKind() == "kind":
		return p.disallow(l, r.entry)
	case r.entry != nil:
		return p.change(r.entry)
	case r.act == actCommand:
		return p.allowCommand()
	case r.act == actKind:
		return p.allowKind()
	case r.act == actSuggest:
		return p.suggest()
	case r.act == actCheck:
		return p.check()
	}
	if _, open, ok := p.options.Advanced(approvalsconfig.Key); ok {
		return open()
	}
	return nil
}

// Opens what the page was opened on: allowing a command or a kind of action, the suggestions or the check of a command
func (p *page) reach(target string) tea.Cmd {
	switch target {
	case actCommand:
		return p.allowCommand()
	case actKind:
		return p.allowKind()
	case actSuggest:
		return p.suggest()
	case actCheck:
		return p.check()
	}
	return nil
}

// Reads what runs without asking and the option rows, then draws every list again and runs then
func (p *page) load(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &approvalsv1.ListRequest{Profile: p.profile, Session: p.session})
		return func() tea.Cmd {
			return p.options.Load(func() tea.Cmd {
				p.loaded, p.err = true, err
				if err == nil {
					p.listed = response
				}
				p.refresh()
				if then == nil || err != nil {
					return nil
				}
				return then()
			})
		}
	})
}

func (p *page) refresh() {
	p.views.Refresh()
}

// Keeps a list among those drawn again on every read
func (p *page) watch(view *cards.List) {
	p.views.Add(view)
	p.options.Watch(view)
}

// Puts the cursor of the first view on the entry once it is read again
func (p *page) point(value string) {
	for i, r := range p.rows() {
		if r.entry != nil && r.entry.GetValue() == value {
			p.root.Select(i)
		}
	}
}

// Turns skipping approvals in the chat in hand on or off, saying what the switch says
func (p *page) skip(on bool) tea.Cmd {
	if p.listed.GetLaunched() {
		return p.hub.Fail(p.listed.GetWinning())
	}
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*approvalsv1.SkipResponse, error) {
		return p.service.Skip(p.kit.Env.Ctx, &approvalsv1.SkipRequest{Session: p.session, On: on})
	}, func(response *approvalsv1.SkipResponse) tea.Cmd {
		return tea.Batch(p.hub.Done(response.GetNote()), p.load(nil))
	})
}

// Asks for a command or a pattern and lets it run without asking
func (p *page) allowCommand() tea.Cmd {
	field := cards.Field{Label: "Command or pattern", Help: patternHelp}
	return p.kit.Ask(p.hub, "Allow a command", field, "Allow", func(pattern string) tea.Cmd { return p.allow(pattern, pattern) })
}

// Lets the entry run without asking, saying so, or that it did already
func (p *page) allow(entry, title string) tea.Cmd {
	if entry = strings.TrimSpace(entry); entry == "" {
		return nil
	}
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*approvalsv1.AllowResponse, error) {
		return p.service.Allow(p.kit.Env.Ctx, &approvalsv1.AllowRequest{Profile: p.profile, Patterns: []string{entry}})
	}, func(response *approvalsv1.AllowResponse) tea.Cmd {
		if len(response.GetAdded()) == 0 {
			return p.hub.Say(title+" is allowed already.", cards.NoteInfo)
		}
		return tea.Batch(p.hub.Done(title+" now runs without asking."), p.load(func() tea.Cmd {
			p.point(entry)
			return nil
		}))
	})
}

// Changes an allowed command or pattern into another in one write
func (p *page) change(entry *approvalsv1.Entry) tea.Cmd {
	old := entry.GetValue()
	field := cards.Field{Label: "Command or pattern", Value: old, Help: patternHelp, Select: true}
	return p.kit.Ask(p.hub, "Change "+old, field, "Save", func(typed string) tea.Cmd {
		if typed == "" || typed == old {
			return nil
		}
		p.hub.Busy("Saving…")
		return tui.Call(p.hub, func() (*approvalsv1.ChangeResponse, error) {
			return p.service.Change(p.kit.Env.Ctx, &approvalsv1.ChangeRequest{Profile: p.profile, Old: old, New: typed})
		}, func(*approvalsv1.ChangeResponse) tea.Cmd {
			return tea.Batch(p.hub.Done(typed+" now runs without asking, in place of "+old+"."), p.load(func() tea.Cmd {
				p.point(typed)
				return nil
			}))
		})
	})
}

// Asks before an entry stops running without asking
func (p *page) disallow(l *cards.List, entry *approvalsv1.Entry) tea.Cmd {
	name := entry.GetTitle()
	l.Confirm(cards.Question{Text: "Remove " + name + " from what runs without asking? The agent then asks before it each time.", Verb: "Remove"},
		func() tea.Cmd {
			p.hub.Busy("Saving…")
			return tui.Call(p.hub, func() (*approvalsv1.DisallowResponse, error) {
				return p.service.Disallow(p.kit.Env.Ctx, &approvalsv1.DisallowRequest{Profile: p.profile, Patterns: []string{entry.GetValue()}})
			}, func(*approvalsv1.DisallowResponse) tea.Cmd {
				return tea.Batch(p.hub.Done("The agent asks again before "+name+"."), p.load(nil))
			})
		})
	return nil
}

// Kinds of risky action not allowed yet
func (p *page) kinds() []*approvalsv1.Kind {
	return slices.DeleteFunc(slices.Clone(p.listed.GetKinds()), func(kind *approvalsv1.Kind) bool {
		return slices.Contains(p.listed.GetAllowed(), kind.GetValue())
	})
}

// Picks a kind of risky action and lets it run without asking: a list to search, a name too long for the width said whole under it
func (p *page) allowKind() tea.Cmd {
	kinds := p.kinds()
	if len(kinds) == 0 {
		return p.hub.Say("Every kind of risky action runs without asking already.", cards.NoteInfo)
	}
	t := p.kit.Env.Theme
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Allow a kind of action",
		Glyph:   t.Glyphs.Settings,
		Key:     "approvals:kind",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search kinds of action",
		Intro:   func() []string { return []string{"The kind of risky action that runs without asking."} },
		Columns: []render.Column{{Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(kinds))
			for i, kind := range kinds {
				rows[i] = []string{strs.Capital(kind.GetValue())}
			}
			return rows
		},
		Words: func(i int) string { return kinds[i].GetAbout() },
		Detail: func(i int) []string {
			var lines []string
			if list.Cut(i, 0) {
				lines = append(lines, strs.Capital(kinds[i].GetValue()))
			}
			if about := kinds[i].GetAbout(); about != "" {
				lines = append(lines, render.Dim(strings.TrimSuffix(about, ".")+".", t))
			}
			return lines
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: "allow"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				return tea.Batch(p.hub.Close(list), p.allow(kinds[i].GetValue(), strs.Capital(kinds[i].GetValue())))
			},
		},
	})
	list.ToggleDetail()
	return p.hub.Push(list)
}
