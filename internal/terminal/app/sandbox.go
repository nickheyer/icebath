package app

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of Sandbox, the page of its area
const Screen = "sandbox"

// Options of Sandbox in the order a person sets them up: these first, then those of the sections whose settings go into Sandbox, then
// the rest
var (
	firstOptions = []string{terminalconfig.Cwd, terminalconfig.HomeMode, terminalconfig.ContainerPersistent, terminalconfig.SudoPassword,
		terminalconfig.Notifications}
	lastOptions = []string{terminalconfig.EnvPassthrough}
)

func sandboxScreen(service terminalv1connect.BackendServiceClient, settings configv1connect.ConfigServiceClient) tui.Screen {
	return tui.Screen{
		ID:      Screen,
		Group:   tui.GroupAbilities,
		Order:   50,
		Title:   "Sandbox",
		Short:   "Choose and set up where the agent's commands run",
		Slash:   []string{"sandbox"},
		Aliases: []string{"backends"},
		Usage:   "[@profile] [place]",
		Area:    terminalconfig.Key,
		Pages:   func(kit *tui.Kit, name string) []tui.Page { return sandboxPages(kit, service, name) },
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, target := profile.Split(argument)
			p := &sandbox{kit: kit, service: service, settings: settings, profile: name}
			return p.open(strings.ToLower(strings.TrimSpace(target)))
		},
	}
}

// The page of Sandbox for one profile, and what every page inside it reads
type sandbox struct {
	kit      *tui.Kit
	service  terminalv1connect.BackendServiceClient
	settings configv1connect.ConfigServiceClient
	// Profile the page is of, the one in use when empty
	profile string
	hub     *cards.Hub
	root    *cards.List
	options *configapp.Rows
	views   cards.Watched
	// The places as last read, and the environment variable that names the one in use over the settings
	places   []*terminalv1.Backend
	variable string
	loaded   bool
	err      error
	// A view was opened over the first view, which reads again once it is back on top
	opened bool
}

// Row of the first view: a place, an option, a row that opens the page of a setting's own, or the row of the advanced options
type sandboxRow struct {
	place    *terminalv1.Backend
	setting  *configv1.Setting
	opens    bool
	advanced bool
}

// The first view: what Sandbox is, the one thing wrong, the places under Where commands run, then the options with the key proxy and the
// advanced ones. Opened at a place, its page opens once it is read
func (p *sandbox) open(target string) tea.Cmd {
	t := p.kit.Env.Theme
	var shown []sandboxRow
	p.root = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Sandbox",
		Glyph:   t.Glyphs.Terminal,
		Key:     Screen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   p.intro,
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.rows()
			cells := make([][]string, len(shown))
			for i, row := range shown {
				cells[i] = p.cells(row)
			}
			return cells
		},
		Group: func(i int) string {
			if shown[i].place != nil {
				return "Where commands run"
			}
			return "Options"
		},
		Detail: func(i int) []string { return p.detail(shown[i], p.root.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "") },
		Free:   map[string]bool{"hub.refresh": true},
		// Read again as a page over it closes, which may have changed what the first view shows, such as the key proxy
		OnFocus: func(*cards.List) tea.Cmd {
			if !p.opened || p.hub.Depth() > 0 {
				return nil
			}
			p.opened = false
			return p.load(nil)
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return p.hints(shown[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch row := shown[i]; {
				case row.place != nil && row.place.GetReady() && !row.place.GetSelected():
					return p.use(row.place)
				case row.place != nil:
					return p.placePage(row.place.GetName())
				case row.setting != nil:
					p.opened = row.opens
					return p.options.Change(row.setting)
				}
				if _, _, open := p.sandboxOptions(); open != nil {
					p.opened = true
					return open()
				}
				return nil
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if place := shown[i].place; place != nil {
					return p.placePage(place.GetName())
				}
				return nil
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if setting := shown[i].setting; setting != nil {
					return p.options.Reset(l, setting)
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

// Lines over the rows: what Sandbox is, then while something is wrong what and what to do about it
func (p *sandbox) intro() []string {
	t := p.kit.Env.Theme
	lines := []string{"Where the agent's commands run."}
	if !p.loaded || p.err != nil {
		return lines
	}
	for _, place := range p.places {
		switch {
		case !place.GetSelected():
		case p.variable != "":
			lines = append(lines, render.Warm(p.variable+" names "+place.GetTitle()+", which wins over what you choose here while it is set.", t))
		case !place.GetReady():
			lines = append(lines, render.Warm(place.GetTitle()+" can't run the agent's commands yet. Set it up below.", t))
		}
	}
	return lines
}

// Rows of the first view
func (p *sandbox) rows() []sandboxRow {
	if !p.loaded || p.err != nil {
		return nil
	}
	var rows []sandboxRow
	for _, place := range p.places {
		rows = append(rows, sandboxRow{place: place})
	}
	shown, advanced, _ := p.sandboxOptions()
	for _, setting := range shown {
		rows = append(rows, sandboxRow{setting: setting, opens: setting.GetPicks() != ""})
	}
	if advanced != nil {
		rows = append(rows, sandboxRow{advanced: true})
	}
	return rows
}

// Options of Sandbox in the order a person sets them up, a setting whose page is its own last, and the cells of the row of the advanced ones
// with what it opens; the advanced ones in place, no row, while they are few
func (p *sandbox) sandboxOptions() (shown []*configv1.Setting, advanced []string, open func() tea.Cmd) {
	everyday, rare := p.options.Section(terminalconfig.Key)
	advanced, open, apart := p.options.Advanced(terminalconfig.Key)
	if !apart {
		everyday = append(everyday, rare...)
	}
	rank := func(setting *configv1.Setting) int {
		switch key := setting.GetKey(); {
		case setting.GetPicks() != "":
			return 3
		case slices.Contains(firstOptions, key):
			return 0
		case slices.Contains(lastOptions, key):
			return 2
		}
		return 1
	}
	ordered := slices.Clone(everyday)
	slices.SortStableFunc(ordered, func(a, b *configv1.Setting) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		if rank(a) == 0 {
			return slices.Index(firstOptions, a.GetKey()) - slices.Index(firstOptions, b.GetKey())
		}
		return 0
	})
	return ordered, advanced, open
}

func (p *sandbox) cells(row sandboxRow) []string {
	t := p.kit.Env.Theme
	switch {
	case row.place != nil:
		return []string{row.place.GetTitle(), p.kit.Mark(true, row.place.GetSelected()), stateWords(row.place, t)}
	case row.opens:
		return []string{row.setting.GetTitle(), render.Dim(t.Glyphs.Forward, t), p.options.Cells(row.setting)[2]}
	case row.setting != nil:
		return p.options.Cells(row.setting)
	}
	_, advanced, _ := p.sandboxOptions()
	return advanced
}

// State of a place in its words, in the color of how it stands
func stateWords(place *terminalv1.Backend, t theme.Theme) string {
	switch {
	case place.GetReady():
		return render.Good(place.GetState(), t)
	case place.GetMissing():
		return render.Bad(place.GetState(), t)
	}
	return render.Warm(place.GetState(), t)
}

// Lines under the rows about the row in hand: what a place is and what keeps it from running, what an option does
func (p *sandbox) detail(row sandboxRow, cut bool) []string {
	switch {
	case row.place != nil:
		lines := []string{row.place.GetAbout() + "."}
		if why := row.place.GetWhy(); why != "" {
			lines = append(lines, render.Warm(strs.Full(why), p.kit.Env.Theme))
		}
		return lines
	case row.opens:
		lines := []string{row.setting.GetDescription()}
		if help := row.setting.GetHelp(); help != "" {
			lines = append(lines, render.Dim(help, p.kit.Env.Theme))
		}
		return lines
	case row.setting != nil:
		return p.options.Detail(row.setting, cut)
	}
	return []string{"Settings of where commands run that few people change."}
}

func (p *sandbox) hints(row sandboxRow) []wm.Hint {
	switch {
	case row.place != nil && row.place.GetReady() && !row.place.GetSelected():
		return []wm.Hint{{Action: "hub.use", Label: "use"}, {Action: "hub.edit", Label: "open"}}
	case row.place != nil, row.opens, row.advanced:
		return []wm.Hint{{Action: "hub.use", Label: "open"}}
	}
	return p.options.Hints(row.setting)
}

// Goes where the page was opened at: the page of a place named by its name or its title, or the page of an option that has one, named by
// its section
func (p *sandbox) reach(target string) tea.Cmd {
	if target == "" {
		return nil
	}
	for i, row := range p.rows() {
		switch {
		case row.place != nil && (row.place.GetName() == target || strings.EqualFold(row.place.GetTitle(), target)):
			p.root.Select(i)
			return p.placePage(row.place.GetName())
		case row.opens && (row.setting.GetSection() == target || strings.EqualFold(row.setting.GetTitle(), target)):
			p.root.Select(i)
			p.opened = true
			return p.options.Change(row.setting)
		}
	}
	return p.hub.Say("There is no place to run commands named "+target+".", cards.NoteInfo)
}

// Reads the places, then the option rows, then draws every list again and runs then
func (p *sandbox) load(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.Backends(p.kit.Env.Ctx, &terminalv1.BackendsRequest{Profile: p.profile})
		return func() tea.Cmd {
			// What was read of the places shows whether or not the options can be read after it
			p.loaded, p.err = true, err
			if err == nil {
				p.places, p.variable = response.GetBackends(), response.GetVariable()
			}
			p.refresh()
			return p.options.Load(func() tea.Cmd {
				p.refresh()
				if then == nil || err != nil {
					return nil
				}
				return then()
			})
		}
	})
}

// Draws every list of the page again
func (p *sandbox) refresh() {
	p.views.Refresh()
}

// Keeps a list among those drawn again on every read
func (p *sandbox) watch(view *cards.List) {
	p.views.Add(view)
	p.options.Watch(view)
}

// Place of the name as last read, nil when it is gone
func (p *sandbox) find(name string) *terminalv1.Backend {
	for _, place := range p.places {
		if place.GetName() == name {
			return place
		}
	}
	return nil
}

// Runs the profile's commands on the place from the next one on, saying so, and that the environment wins when it names another
func (p *sandbox) use(place *terminalv1.Backend) tea.Cmd {
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*terminalv1.UseResponse, error) {
		return p.service.Use(p.kit.Env.Ctx, &terminalv1.UseRequest{Profile: p.profile, Backend: place.GetName()})
	}, func(response *terminalv1.UseResponse) tea.Cmd {
		var changed tea.Cmd
		if p.profile == "" {
			changed = p.kit.Changed(terminalconfig.Backend, strconv.Quote(place.GetName()))
		}
		said := p.hub.Done(response.GetNote())
		if response.GetHeld() {
			said = p.hub.Say(response.GetNote(), cards.NoteInfo)
		}
		return tea.Batch(changed, said, p.load(nil))
	})
}
