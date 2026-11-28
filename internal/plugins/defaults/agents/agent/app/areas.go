package app

import (
	tea "charm.land/bubbletea/v2"

	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	delegationconfig "github.com/nickheyer/icebath/internal/delegation/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screens of the Helper models and Subagents areas, and the job of the second that opens the choice of method over its page
const (
	helperScreen   = "auxiliary"
	subagentScreen = "delegation"
	jobMethod      = "method"
)

// The Helper models area's page
func (d *desk) helperArea() tui.Screen {
	return tui.Screen{
		ID:    helperScreen,
		Group: tui.GroupModels,
		Order: 30,
		Title: "Helper models",
		Short: "Choose the models for side jobs",
		Area:  auxiliaryconfig.Key,
		Pages: d.helperPages,
		Open:  func(kit *tui.Kit, argument string) tea.Cmd { return d.openHelpers(kit, argument) },
	}
}

// The Subagents area's page
func (d *desk) subagentArea() tui.Screen {
	return tui.Screen{
		ID:      subagentScreen,
		Group:   tui.GroupAbilities,
		Order:   70,
		Title:   "Subagents",
		Short:   "Choose the model and the limits of subagents",
		Area:    delegationconfig.Key,
		Pickers: map[string]tui.Picker{delegationconfig.PickMethod: d.methodPicker()},
		Pages:   d.subagentPages,
		Open:    func(kit *tui.Kit, argument string) tea.Cmd { return d.openSubagents(kit, argument) },
	}
}

// Page of the helper models: the jobs, each with the model it uses, then the options of the area; the page of the job whose section the
// argument names opens over it once read
func (d *desk) openHelpers(kit *tui.Kit, argument string) tea.Cmd {
	name, job := pageArgument(argument)
	var options *configapp.Rows
	var jobs []*configv1.Section
	loaded := false
	// Why the settings could not be read, which the page says in place of its rows
	var failed error
	var hub *cards.Hub
	var read func(job string) tea.Cmd
	list := pageList(kit, pageSpec{
		title: "Helper models",
		key:   helperScreen,
		intro: func() []string { return []string{"Models for side jobs such as naming sessions."} },
		rows: func() []pageRow {
			if !loaded {
				return nil
			}
			var rows []pageRow
			for _, job := range jobs {
				rows = append(rows, jobRow(kit, hub, options, job))
			}
			everyday, advanced := options.Own(auxiliaryconfig.Key)
			rows = append(rows, optionRows(options, "Options", everyday...)...)
			if cells, open, ok := options.OwnAdvanced(auxiliaryconfig.Key); ok {
				return append(rows, advancedRow(cells, open, ok, "Settings of the helper models that few people change.")...)
			}
			return append(rows, optionRows(options, "Options", advanced...)...)
		},
		empty:   func() string { return tui.Waiting(loaded, failed, "") },
		refresh: func() tea.Cmd { return read("") },
	})
	hub, cmd := kit.Show(helperScreen, list)
	options = configapp.NewRows(kit, hub, d.settings, name)
	options.Watch(list)
	// Reads the jobs and the settings, then opens the page of the job named
	read = func(job string) tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			listed, err := d.settings.List(kit.Env.Ctx, &configv1.ListRequest{Profile: name})
			return func() tea.Cmd {
				if failed = err; err != nil {
					loaded = false
					list.Refresh()
					return nil
				}
				jobs = nil
				for _, section := range listed.GetSections() {
					if section.GetParent() == auxiliaryconfig.Key {
						jobs = append(jobs, section)
					}
				}
				return options.Load(func() tea.Cmd {
					loaded = true
					list.Refresh()
					for _, section := range jobs {
						if job != "" && section.GetKey() == job {
							return hub.Push(jobPage(kit, options, section))
						}
					}
					return nil
				})
			}
		})
	}
	return tea.Batch(cmd, read(job))
}

// Row of a job: its name and the model it uses, enter opening its page
func jobRow(kit *tui.Kit, hub *cards.Hub, options *configapp.Rows, job *configv1.Section) pageRow {
	key := job.GetKey()
	model := options.Setting(key + "." + auxiliaryconfig.Model)
	cells := []string{job.GetTitle(), "", ""}
	if model != nil {
		shown := options.Cells(model)
		cells[1], cells[2] = shown[1], shown[2]
	}
	return pageRow{group: "Jobs", cells: cells, detail: []string{job.GetDescription() + "."}, words: job.GetDescription(),
		hints: []wm.Hint{{Action: "hub.use", Label: "open"}}, use: func(*cards.List) tea.Cmd { return hub.Push(jobPage(kit, options, job)) }}
}

// Page of one job: the model it uses, its own options, and those few people change, such as its own server and its fallback models
func jobPage(kit *tui.Kit, options *configapp.Rows, job *configv1.Section) *cards.List {
	key := job.GetKey()
	list := pageList(kit, pageSpec{
		title: job.GetTitle(),
		key:   helperScreen + ":" + key,
		intro: func() []string { return []string{job.GetDescription() + "."} },
		rows: func() []pageRow {
			model := options.Setting(key + "." + auxiliaryconfig.Model)
			rows := optionRows(options, "", model)
			everyday, _ := options.Own(key, key+"."+auxiliaryconfig.Model)
			rows = append(rows, optionRows(options, "", everyday...)...)
			cells, open, ok := options.OwnAdvanced(key, key+"."+auxiliaryconfig.Model)
			return append(rows, advancedRow(cells, open, ok, "An address and key of its own, time limits, extra request fields and fallback models.")...)
		},
	})
	options.Watch(list)
	return list
}

// Page of the subagents: their model and how hard it thinks, their fallback models and limits, and those few people change, among them
// the method they run with
func (d *desk) openSubagents(kit *tui.Kit, argument string) tea.Cmd {
	name, job := pageArgument(argument)
	var options *configapp.Rows
	loaded := false
	// Why the settings could not be read, which the page says in place of its rows
	var failed error
	var read func(job string) tea.Cmd
	list := pageList(kit, pageSpec{
		title: "Subagents",
		key:   subagentScreen,
		intro: func() []string { return []string{"Helper agents that split up a big task."} },
		rows: func() []pageRow {
			if !loaded {
				return nil
			}
			everyday, _ := options.Own(delegationconfig.Key)
			rows := optionRows(options, "", everyday...)
			cells, open, ok := options.OwnAdvanced(delegationconfig.Key)
			return append(rows, advancedRow(cells, open, ok, "Settings of subagents that few people change, such as the method they run with.")...)
		},
		empty:   func() string { return tui.Waiting(loaded, failed, "") },
		refresh: func() tea.Cmd { return read("") },
	})
	hub, cmd := kit.Show(subagentScreen, list)
	options = configapp.NewRows(kit, hub, d.settings, name)
	options.Watch(list)
	// Reads the settings, then opens the job named
	read = func(job string) tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			_, err := d.settings.Get(kit.Env.Ctx, &configv1.GetRequest{Key: delegationconfig.Backend, Profile: name})
			return func() tea.Cmd {
				if failed = err; err != nil {
					loaded = false
					list.Refresh()
					return nil
				}
				return options.Load(func() tea.Cmd {
					loaded = true
					list.Refresh()
					method := options.Setting(delegationconfig.Backend)
					if job != jobMethod || method == nil {
						return nil
					}
					hub.Direct()
					return options.Change(method)
				})
			}
		})
	}
	return tea.Batch(cmd, read(job))
}
