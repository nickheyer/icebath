package app

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	appconfig "github.com/nickheyer/icebath/internal/app/config"
	"github.com/nickheyer/icebath/internal/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of General, the page of its area
const generalScreen = "general"

// What the page of General is opened at to show the keyboard shortcuts, before the words they are searched for or the action they are on
const atKeys = "keys"

// Start of what the page of General is opened at to put the first row of a section folded into it in hand, the key of the section after it
const atSection = "section:"

// Options of General a person sets first, in this order
var firstOptions = []string{appconfig.Skin, config.Timezone, appconfig.Option}

func generalPage(settings configv1connect.ConfigServiceClient) tui.Screen {
	return tui.Screen{
		ID:    generalScreen,
		Group: tui.GroupApp,
		Order: 15,
		Title: "General",
		Short: "Change the colors, keys and time zone of this app",
		Usage: "[@profile] [keys [words to search]]",
		Area:  config.GeneralKey,
		Pages: func(kit *tui.Kit, name string) []tui.Page {
			pages := []tui.Page{{Title: "Keyboard shortcuts", About: "The keys that run each action of this app", Words: "keys keybindings record",
				Argument: atKeys, Keys: []string{appconfig.Keys}}}
			// Each section folded into General is a part of its page, such as Terminal windows, found by its title
			listed, err := settings.List(kit.Env.Ctx, &configv1.ListRequest{Profile: name})
			if err != nil {
				return pages
			}
			for _, section := range listed.GetSections() {
				if section.GetInto() == config.GeneralKey && section.GetKey() != appconfig.Key {
					// Its settings are rows of the page of General itself, where a found one opens with its control, so the page names none
					pages = append(pages, tui.Page{Title: section.GetTitle(), About: section.GetDescription(), Argument: atSection + section.GetKey()})
				}
			}
			return pages
		},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, target := profile.Split(argument)
			g := &general{kit: kit, settings: settings, profile: name}
			return g.open(target)
		},
	}
}

// The page of General for one profile
type general struct {
	kit      *tui.Kit
	settings configv1connect.ConfigServiceClient
	// Profile the page is of, the one in use when empty
	profile string
	hub     *cards.Hub
	root    *cards.List
	options *configapp.Rows
	loaded  bool
	err     error
}

// Row of General: an option, the row that opens the keyboard shortcuts, or the row of the advanced options
type generalRow struct {
	setting  *configv1.Setting
	keys     bool
	advanced bool
}

// The first view: the colors, the time zone and the keyboard of this app, the row of the keyboard shortcuts, then under the title of
// their section the options kept elsewhere that General shows, such as those of terminal windows. Opened at keys, the shortcuts open over
// it once it is read, searched for the words after keys or on the action they name
func (g *general) open(target string) tea.Cmd {
	t := g.kit.Env.Theme
	var shown []generalRow
	g.root = cards.NewList(g.kit.Env, cards.ListSpec{
		Title:   "General",
		Glyph:   t.Glyphs.Settings,
		Key:     generalScreen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{"The colors, keys and time zone of this app."} },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = g.rows()
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = g.cells(r)
			}
			return cells
		},
		Group:  func(i int) string { return g.group(shown[i]) },
		Detail: func(i int) []string { return g.detail(shown[i], g.root.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(g.loaded, g.err, "") },
		Free:   map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			if r := shown[i]; r.setting != nil {
				return g.options.Hints(r.setting)
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch r := shown[i]; {
				case r.setting != nil:
					return g.options.Change(r.setting)
				case r.keys:
					return g.shortcuts("")
				}
				if _, open, ok := g.options.Advanced(config.GeneralKey); ok {
					return open()
				}
				return nil
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if setting := shown[i].setting; setting != nil {
					return g.options.Reset(l, setting)
				}
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return g.load(nil) },
		},
	})
	g.root.ToggleDetail()
	hub, cmd := g.kit.Show(generalScreen, g.root)
	g.hub = hub
	g.options = configapp.NewRows(g.kit, hub, g.settings, g.profile)
	g.options.Watch(g.root)
	return tea.Batch(cmd, g.load(func() tea.Cmd {
		if rest, ok := strings.CutPrefix(target, atKeys); ok && (rest == "" || strings.HasPrefix(rest, " ")) {
			return g.shortcuts(strings.TrimSpace(rest))
		}
		if key, ok := strings.CutPrefix(target, atSection); ok {
			for i, r := range g.rows() {
				if r.setting.GetSection() == key {
					g.root.Select(i)
					break
				}
			}
		}
		return nil
	}))
}

// Reads the option rows, then runs then
func (g *general) load(then func() tea.Cmd) tea.Cmd {
	return g.options.Load(func() tea.Cmd {
		g.loaded, g.err = true, nil
		g.root.Refresh()
		if then == nil {
			return nil
		}
		return then()
	})
}

// Rows of the first view: the options a person sets first, the others of General and Appearance, the keyboard shortcuts, then the options of
// each section General shows under its title, and the advanced ones behind their row or in place when they are few
func (g *general) rows() []generalRow {
	everyday, rare := g.options.Section(config.GeneralKey)
	_, _, apart := g.options.Advanced(config.GeneralKey)
	if !apart {
		everyday = append(everyday, rare...)
	}
	own := func(setting *configv1.Setting) bool {
		return setting.GetSection() == config.GeneralKey || setting.GetSection() == appconfig.Key
	}
	rank := func(setting *configv1.Setting) int {
		if at := slices.Index(firstOptions, setting.GetKey()); at >= 0 {
			return at
		}
		return len(firstOptions)
	}
	var mine, theirs []*configv1.Setting
	for _, setting := range everyday {
		if own(setting) {
			mine = append(mine, setting)
		} else {
			theirs = append(theirs, setting)
		}
	}
	slices.SortStableFunc(mine, func(a, b *configv1.Setting) int { return rank(a) - rank(b) })
	var rows []generalRow
	for _, setting := range mine {
		rows = append(rows, generalRow{setting: setting})
	}
	rows = append(rows, generalRow{keys: true})
	for _, setting := range theirs {
		rows = append(rows, generalRow{setting: setting})
	}
	if apart {
		rows = append(rows, generalRow{advanced: true})
	}
	return rows
}

// Heading of a row: none for the options of General itself and the shortcuts, the title of its section for an option kept elsewhere
func (g *general) group(r generalRow) string {
	switch section := r.setting.GetSection(); {
	case r.setting == nil, section == config.GeneralKey, section == appconfig.Key:
		return ""
	default:
		return g.options.Info(section).GetTitle()
	}
}

func (g *general) cells(r generalRow) []string {
	t := g.kit.Env.Theme
	switch {
	case r.setting != nil:
		return g.options.Cells(r.setting)
	case r.keys:
		changed := 0
		for _, action := range g.kit.Env.Keys.Actions() {
			if !g.kit.Env.Keys.AtDefault(action.ID) {
				changed++
			}
		}
		said := "None changed"
		if changed > 0 {
			said = render.Count(changed, "key") + " changed"
		}
		return []string{"Keyboard shortcuts", render.Dim(t.Glyphs.Forward, t), render.Dim(said, t)}
	}
	cells, _, _ := g.options.Advanced(config.GeneralKey)
	return cells
}

func (g *general) detail(r generalRow, cut bool) []string {
	switch {
	case r.setting != nil:
		return g.options.Detail(r.setting, cut)
	case r.keys:
		return []string{"Every shortcut of this app, under where it works. Enter records a new key for one.",
			render.Dim("Also /keys", g.kit.Env.Theme)}
	}
	return []string{"Settings of General that few people change."}
}

// Opens the keyboard shortcuts over the page, searched for the words or on the action they name
func (g *general) shortcuts(target string) tea.Cmd {
	b := &bindings{kit: g.kit, settings: g.settings, hub: g.hub, refresh: g.root.Refresh}
	return b.push(target)
}
