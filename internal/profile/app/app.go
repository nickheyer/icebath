// Page of Profiles in the settings menu: the profiles with the one in use marked, a profile's own page, adding one in steps and exporting
// one; /profiles with a name switches to it, and /export is the job of exporting
package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/profile"
	profileconfig "github.com/nickheyer/icebath/internal/profile/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	profilev1 "github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1/profilev1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	// Screen of Profiles, the page of its area
	Screen = "profiles"
	// Export of a profile to an archive, opened by /export
	ExportScreen = "export"
)

// What the page of Profiles is opened at: the export of the profile in use, adding a profile, or the page of the profile named after the
// prefix
const (
	atExport  = "export"
	atAdd     = "add"
	atProfile = "profile:"
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := profilev1connect.NewProfileServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{
		{
			ID:    Screen,
			Group: tui.GroupApp,
			Order: 40,
			Title: "Profiles",
			Short: "Switch between separate sets of settings",
			Area:  profileconfig.Key,
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				_, target := profile.Split(argument)
				p := &profiles{kit: kit, service: service, settings: settings}
				return p.open(strings.TrimSpace(target))
			},
			Pages: func(kit *tui.Kit, _ string) []tui.Page {
				pages := []tui.Page{{Title: "Add a profile", About: "Make a profile, import one from a file, or install a ready-made one",
					Words: "new create import install distribution", Argument: atAdd}}
				listed, err := service.List(kit.Env.Ctx, &profilev1.ListRequest{})
				if err != nil {
					return pages
				}
				for _, row := range listed.GetProfiles() {
					pages = append(pages, tui.Page{Title: label(row), About: "The page of the profile " + row.GetName(), Words: row.GetDescription(),
						Argument: atProfile + row.GetName()})
				}
				return pages
			},
		},
		{
			ID:    Screen + ".command",
			Short: "Switch between separate sets of settings",
			Slash: []string{"profiles"},
			Usage: "[name]",
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				if _, name := profile.Split(argument); name != "" {
					p := &profiles{kit: kit, service: service, settings: settings}
					return p.use(name)
				}
				return kit.Open(Screen, "")
			},
		},
		{
			ID:    ExportScreen,
			Short: "Save a profile to a file you can share",
			Slash: []string{"export"},
			Usage: "[profile] [-o file.tar.gz]",
			Open:  func(kit *tui.Kit, argument string) tea.Cmd { return exportSlash(kit, service, argument) },
		},
	}
}

type profiles struct {
	kit      *tui.Kit
	service  profilev1connect.ProfileServiceClient
	settings configv1connect.ConfigServiceClient
	hub      *cards.Hub
	list     *cards.List
	rows     []*profilev1.Profile
	// Lists stacked over the first one, refreshed whenever the profiles are read
	views  cards.Watched
	loaded bool
	err    error
}

// Name a profile shows under, its display name beside it when it has one
func label(row *profilev1.Profile) string {
	return profile.Label(row.GetName(), row.GetDisplayName())
}

// State of a profile in one to three words: in use in this window, or the one icebath opens with
func state(row *profilev1.Profile) string {
	switch {
	case row.GetActive():
		return "In use"
	case row.GetSticky():
		return "Opens at start"
	}
	return ""
}

// The first view: what a profile is, the profiles with the one in use marked, and the row that adds another; opened at export, the export of
// the profile in use opens over it as the job the person came for
func (p *profiles) open(target string) tea.Cmd {
	t := p.kit.Env.Theme
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Profiles",
		Glyph:   t.Glyphs.Profiles,
		Key:     Screen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{"A profile is a separate set of settings, providers and sessions."} },
		Columns: []render.Column{{}, {}, {Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, 0, len(p.rows)+1)
			for _, row := range p.rows {
				rows = append(rows, []string{p.kit.Mark(true, row.GetActive()), label(row), render.Dim(state(row), t)})
			}
			if p.loaded && p.err == nil {
				rows = append(rows, []string{"", "Add a profile", render.Dim(t.Glyphs.Forward, t)})
			}
			return rows
		},
		Detail: func(i int) []string {
			if row := p.at(i); row != nil {
				return p.details(row)
			}
			return []string{"Make a profile, import one from a file, or install a ready-made one."}
		},
		Empty: func() string { return tui.Waiting(p.loaded, p.err, "") },
		Free:  map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			row := p.at(i)
			if row == nil {
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			}
			hints := []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.rename", Label: "rename"}}
			if !row.GetDefault() && !row.GetActive() {
				hints = append(hints, wm.Hint{Action: "hub.remove", Label: "delete"})
			}
			return append(hints, wm.Hint{Action: "hub.add", Label: "add"})
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if row := p.at(i); row != nil {
					return p.page(row.GetName())
				}
				return p.add()
			},
			"hub.add": func(*cards.List, int) tea.Cmd { return p.add() },
			"hub.rename": func(_ *cards.List, i int) tea.Cmd {
				if row := p.at(i); row != nil {
					return p.rename(row, nil)
				}
				return nil
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if row := p.at(i); row != nil {
					return p.remove(l, row, nil)
				}
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load(nil) },
		},
	})
	p.list.ToggleDetail()
	hub, cmd := p.kit.Show(Screen, p.list)
	p.hub = hub
	return tea.Batch(cmd, p.load(func() tea.Cmd {
		switch name, named := strings.CutPrefix(target, atProfile); {
		case target == atExport:
			p.hub.Direct()
			return p.export(p.active())
		case target == atAdd:
			return p.add()
		case named && p.find(name) != nil:
			p.point(name)
			return p.page(name)
		case named:
			return p.hub.Fail("There is no profile named " + name + ".")
		}
		return nil
	}))
}

// Profile on row i of the first view, nil for the row that adds one
func (p *profiles) at(i int) *profilev1.Profile {
	if i < 0 || i >= len(p.rows) {
		return nil
	}
	return p.rows[i]
}

// Reads the profiles, draws every list again and runs then
func (p *profiles) load(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &profilev1.ListRequest{})
		return func() tea.Cmd {
			p.err, p.loaded = err, true
			if err == nil {
				p.rows = response.GetProfiles()
			}
			p.list.Refresh()
			p.views.Refresh()
			switch {
			case err != nil && (len(p.rows) > 0 || p.hub.Top() != p.list):
				// What was read before stays listed, so why it could not be read again goes under it; the first view with nothing to
				// list says why in place of its rows
				return p.hub.Fail(tui.Reason(err))
			case then == nil || err != nil:
				return nil
			}
			return then()
		}
	})
}

// Profile with the name as last read, nil when it is gone
func (p *profiles) find(name string) *profilev1.Profile {
	for _, row := range p.rows {
		if row.GetName() == name {
			return row
		}
	}
	return nil
}

// The profile in use as last read
func (p *profiles) active() *profilev1.Profile {
	for _, row := range p.rows {
		if row.GetActive() {
			return row
		}
	}
	return p.find(profile.Default)
}

// Puts the cursor of the first view on the profile
func (p *profiles) point(name string) {
	for i, row := range p.rows {
		if row.GetName() == name {
			p.list.Select(i)
		}
	}
}

// Lines of what a profile is, under the row in hand: what it is for, then its model, skills, folder, shell command, distribution and former
// names
func (p *profiles) details(row *profilev1.Profile) []string {
	t := p.kit.Env.Theme
	var lines []string
	if description := strings.TrimSpace(row.GetDescription()); description != "" {
		lines = append(lines, description)
	}
	var facts []string
	if model := row.GetModel(); model != "" {
		if row.GetProvider() != "" {
			model += " · " + row.GetProvider()
		}
		facts = append(facts, "Model: "+model)
	}
	facts = append(facts, render.Count(int(row.GetSkills()), "skill"))
	lines = append(lines, render.Dim(strings.Join(facts, " · "), t), render.Dim("Folder: "+profile.Tilde(row.GetPath()), t))
	if alias, _ := profile.AliasOf(row.GetName()); alias != "" {
		lines = append(lines, render.Dim("Shell command: "+alias, t))
	}
	if row.GetDistribution() != "" {
		lines = append(lines, render.Dim("Installed from the distribution "+row.GetDistribution()+version(row.GetDistributionVersion())+".", t))
	}
	if previous := row.GetPreviousNames(); len(previous) > 0 {
		lines = append(lines, render.Dim("Was called "+strings.Join(previous, ", ")+".", t))
	}
	return lines
}

// Uses the profile now and when icebath opens, the one way a profile is switched to, then tells every screen the settings it follows as
// the profile has them
func (p *profiles) use(name string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		ctx := p.kit.Env.Ctx
		used, err := p.service.Use(ctx, &profilev1.UseRequest{Name: name})
		values := map[string]string{}
		if err == nil {
			for _, key := range append(tui.Followed(p.kit.Screens()), profileconfig.Active) {
				if response, err := p.settings.Get(ctx, &configv1.GetRequest{Key: key}); err == nil {
					values[key] = response.GetSetting().GetValue()
				}
			}
		}
		return func() tea.Cmd {
			if err != nil {
				return p.fail("icebath did not switch to " + name + ". " + strs.Full(tui.Reason(err)))
			}
			told := "This window runs on " + used.GetProfile().GetName() + " now, and icebath opens with it."
			var cmds []tea.Cmd
			for key, value := range values {
				cmds = append(cmds, p.kit.Changed(key, value))
			}
			if p.hub != nil && p.hub.Shown() {
				return tea.Batch(append(cmds, p.hub.Done(told), p.load(nil))...)
			}
			return tea.Batch(append(cmds, p.kit.WM.Toast(told, wm.Success, wm.DefaultToastTTL))...)
		}
	})
}

// Says why something failed under the page, or in a toast while it is not on screen
func (p *profiles) fail(text string) tea.Cmd {
	if p.hub != nil && p.hub.Shown() {
		return p.hub.Fail(text)
	}
	return p.kit.WM.Toast(text, wm.Error, wm.DefaultToastTTL)
}

// Sentences joined into one paragraph, empty ones left out
func joined(sentences ...string) string {
	var out []string
	for _, said := range sentences {
		if said = strings.TrimSpace(said); said != "" {
			out = append(out, said)
		}
	}
	return strings.Join(out, " ")
}
