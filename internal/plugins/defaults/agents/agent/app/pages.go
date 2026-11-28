package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// One row of a page of the areas this plugin builds: what it shows, what the pane under the list says of it, and what its keys do
type pageRow struct {
	// Heading the row is listed under, none for a row under no heading
	group  string
	cells  []string
	detail []string
	hints  []wm.Hint
	// Words a search reads besides the cells
	words  string
	use    func(l *cards.List) tea.Cmd
	remove func(l *cards.List) tea.Cmd
}

// A page of rows made again each time the list draws them
type pageSpec struct {
	title, key string
	intro      func() []string
	rows       func() []pageRow
	empty      func() string
	// What alt+n adds from any row, nil for a page that adds nothing
	add func() tea.Cmd
	// Reads what the page shows again, nil for a page that reads nothing
	refresh func() tea.Cmd
}

// List of a page: the rows under their headings in the columns option rows take, the row in hand explained under the list
func pageList(kit *tui.Kit, spec pageSpec) *cards.List {
	var shown []pageRow
	free := map[string]bool{"hub.add": spec.add != nil, "hub.refresh": spec.refresh != nil}
	list := cards.NewList(kit.Env, cards.ListSpec{
		Title:   spec.title,
		Glyph:   kit.Env.Theme.Glyphs.Settings,
		Key:     spec.key,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Intro:   spec.intro,
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = spec.rows()
			cells := make([][]string, len(shown))
			for i, row := range shown {
				cells[i] = row.cells
			}
			return cells
		},
		Group:  func(i int) string { return shown[i].group },
		Words:  func(i int) string { return shown[i].words },
		Detail: func(i int) []string { return shown[i].detail },
		Empty:  spec.empty,
		Free:   free,
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				if spec.add != nil {
					return []wm.Hint{{Action: "hub.add", Label: "add"}}
				}
				return nil
			}
			return shown[i].hints
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(shown) || shown[i].use == nil {
					return nil
				}
				return shown[i].use(l)
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(shown) || shown[i].remove == nil {
					return nil
				}
				return shown[i].remove(l)
			},
			"hub.add": func(*cards.List, int) tea.Cmd {
				if spec.add == nil {
					return nil
				}
				return spec.add()
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd {
				if spec.refresh == nil {
					return nil
				}
				return spec.refresh()
			},
		},
	})
	list.ToggleDetail()
	return list
}

// Row of a setting as Settings draws and changes it, under the heading
func optionRow(options *configapp.Rows, setting *configv1.Setting, group string) pageRow {
	return pageRow{group: group, cells: options.Cells(setting), detail: options.Detail(setting, false), hints: options.Hints(setting),
		words: setting.GetDescription() + " " + setting.GetKey(),
		use:   func(*cards.List) tea.Cmd { return options.Change(setting) },
		remove: func(l *cards.List) tea.Cmd {
			return options.Reset(l, setting)
		}}
}

// Rows of the settings with the keys, those not listed left out
func optionRows(options *configapp.Rows, group string, settings ...*configv1.Setting) []pageRow {
	var rows []pageRow
	for _, setting := range settings {
		if setting != nil {
			rows = append(rows, optionRow(options, setting, group))
		}
	}
	return rows
}

// Row that opens a page inside this one: its name, the mark of a row that opens, what is inside, and in the pane what it is
func openRow(kit *tui.Kit, name, inside, about string, open func() tea.Cmd) pageRow {
	t := kit.Env.Theme
	return pageRow{cells: []string{name, render.Dim(t.Glyphs.Forward, t), render.Dim(inside, t)}, detail: []string{about},
		hints: []wm.Hint{{Action: "hub.use", Label: "open"}}, words: about, use: func(*cards.List) tea.Cmd { return open() }}
}

// Row of the advanced settings as the option rows give it, nothing when there are too few to put behind a row
func advancedRow(cells []string, open func() tea.Cmd, ok bool, about string) []pageRow {
	if !ok {
		return nil
	}
	return []pageRow{{cells: cells, detail: []string{about}, hints: []wm.Hint{{Action: "hub.use", Label: "open"}}, words: about,
		use: func(*cards.List) tea.Cmd { return open() }}}
}

// Row that adds a thing, the last of the things: its words and the mark of a row that opens
func addRow(kit *tui.Kit, group, words, about string, add func() tea.Cmd) pageRow {
	t := kit.Env.Theme
	return pageRow{group: group, cells: []string{words, render.Dim(t.Glyphs.Forward, t), ""}, detail: []string{about},
		hints: []wm.Hint{{Action: "hub.use", Label: "add"}}, words: "add new " + words, use: func(*cards.List) tea.Cmd { return add() }}
}

// Mark of a value changed from what applies without it, as option rows show it
func changed(kit *tui.Kit, own bool) string {
	if !own {
		return ""
	}
	if kit.Env.Theme.Plain {
		return render.Cool("*", kit.Env.Theme)
	}
	return render.Cool("●", kit.Env.Theme)
}

// Page of an area, the profile it shows and the job it opens with, from the argument the menu hands its screen
func pageArgument(argument string) (string, string) {
	name, job := profile.Split(argument)
	return name, strings.TrimSpace(job)
}

// Value as YAML text
func quoted(text string) string {
	if text == "" {
		return "null"
	}
	return yamlText(text)
}

// Value of a setting as a map of named values, nil for none
func groupOf(value string) map[string]any {
	var decoded map[string]any
	if yaml.Unmarshal([]byte(value), &decoded) != nil {
		return nil
	}
	return decoded
}

// Text as YAML
func yamlText(text string) string {
	encoded, _ := yaml.Marshal(text)
	return strings.TrimSpace(string(encoded))
}

// Text a YAML value holds, empty for null or anything else
func plain(text string) string {
	var held string
	if yaml.Unmarshal([]byte(text), &held) != nil {
		return ""
	}
	return held
}
