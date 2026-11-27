package app

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/profile"
	profileconfig "github.com/nickheyer/icebath/internal/profile/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	profilev1 "github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1/profilev1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	// Screen of every profile
	Screen = "profiles"
	// Export of a profile to an archive, opened by /export
	ExportScreen = "export"
)

// What the description of a profile is for, under its field
const describeHelp = "One or two sentences on what the profile is for.\nicebath reads it to pick a profile for a task."

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
			Slash: []string{"profiles"},
			Usage: "[name]",
			Open: func(kit *tui.Kit, name string) tea.Cmd {
				p := &profiles{kit: kit, service: service, settings: settings}
				if name = strings.TrimSpace(name); name != "" {
					return p.use(name)
				}
				return p.open()
			},
		},
		{
			ID:    ExportScreen,
			Group: tui.GroupApp,
			Order: 50,
			Title: "Profile export",
			Short: "Save a profile to an archive you can share",
			Slash: []string{"export"},
			Usage: "[profile] [-o output.tar.gz]",
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
	views  []*cards.List
	loaded bool
	err    error
}

// Name a profile shows under, its display name beside it when it has one
func label(row *profilev1.Profile) string {
	return profile.Label(row.GetName(), row.GetDisplayName())
}

// Whether the profile is the one this window runs on and the one icebath opens with, in a few words
func state(row *profilev1.Profile) string {
	switch {
	case row.GetActive() && row.GetSticky():
		return "In use, opens at start"
	case row.GetActive():
		return "In use"
	case row.GetSticky():
		return "Opens at start"
	}
	return ""
}

// Profiles screen: what a profile is, then every profile with the one this window runs on marked, whether icebath opens with it and what
// it is for, the highlighted one explained under the list
func (p *profiles) open() tea.Cmd {
	t := p.kit.Env.Theme
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Profiles",
		Glyph:   t.Glyphs.Profiles,
		Key:     "profiles",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search profiles",
		Intro: func() []string {
			return []string{"A profile is a separate set of settings, providers and sessions. This window runs on the one marked."}
		},
		Columns: []render.Column{{}, {}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(p.rows))
			for i, row := range p.rows {
				rows[i] = []string{p.kit.Mark(true, row.GetActive()), label(row), render.Dim(state(row), t),
					render.Dim(strings.ReplaceAll(row.GetDescription(), "\n", " "), t)}
			}
			return rows
		},
		Words: func(i int) string {
			return p.rows[i].GetModel() + " " + p.rows[i].GetProvider() + " " + p.rows[i].GetDistribution()
		},
		Detail: func(i int) []string { return p.detail(p.rows[i], !p.list.Shows(3)) },
		Empty: func() string {
			switch {
			case p.err != nil:
				return "The profiles could not be read: " + strings.TrimSuffix(tui.Reason(p.err), ".") + ". " + p.kit.Env.Keys.Display("hub.refresh") +
					" tries again."
			case !p.loaded:
				return "Reading the profiles…"
			}
			return "No profiles yet. " + p.kit.Env.Keys.Display("hub.add") + " adds one."
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(p.rows) {
				return []wm.Hint{{Action: "hub.add"}}
			}
			hints := []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.edit", Label: "describe"}, {Action: "hub.rename"}}
			if !p.rows[i].GetDefault() {
				hints = append(hints, wm.Hint{Action: "hub.remove", Label: "delete"})
			}
			return append(hints, wm.Hint{Action: "hub.add"})
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return p.actions(p.rows[i].GetName()) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return p.add() },
			"hub.edit":    func(_ *cards.List, i int) tea.Cmd { return p.describe(p.rows[i]) },
			"hub.rename":  func(_ *cards.List, i int) tea.Cmd { return p.rename(p.rows[i]) },
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return p.remove(p.rows[i]) },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load() },
		},
		OnFocus: func(*cards.List) tea.Cmd { return p.load() },
	})
	p.list.ToggleDetail()
	p.hub = p.kit.Hub(Screen, p.list)
	return tea.Batch(p.kit.WM.ShowOverlay(p.hub), p.load())
}

func (p *profiles) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &profilev1.ListRequest{})
		return func() tea.Cmd {
			p.err, p.loaded = err, true
			if err == nil {
				p.rows = response.GetProfiles()
			}
			if p.list != nil {
				p.list.Refresh()
			}
			for _, view := range p.views {
				view.Refresh()
			}
			return nil
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

// Lines of a profile's detail pane: what it is for when the row can't show it, whether this window runs on it and icebath opens with it,
// then its model, skills, folder, shell command, distribution and former names
func (p *profiles) detail(row *profilev1.Profile, described bool) []string {
	t := p.kit.Env.Theme
	var lines []string
	if description := row.GetDescription(); description != "" && described {
		lines = append(lines, description)
	}
	if row.GetDescription() != "" && row.GetDescriptionAuto() {
		lines = append(lines, render.Dim("The helper model wrote this description.", t))
	}
	switch {
	case row.GetActive() && row.GetSticky():
		lines = append(lines, render.Good("This window runs on this profile, and icebath opens with it.", t))
	case row.GetActive():
		lines = append(lines, render.Good("This window runs on this profile.", t))
	case row.GetSticky():
		lines = append(lines, "icebath opens with this profile.")
	}
	var facts []string
	if model := row.GetModel(); model != "" {
		if row.GetProvider() != "" {
			model += " from " + row.GetProvider()
		}
		facts = append(facts, "Model: "+model)
	}
	facts = append(facts, render.Count(int(row.GetSkills()), "skill"))
	lines = append(lines, render.Dim(strings.Join(facts, " · "), t), render.Dim("Folder: "+profile.Tilde(row.GetPath()), t))
	if alias, _ := profile.AliasOf(row.GetName()); alias != "" && !row.GetDefault() {
		lines = append(lines, render.Dim("Opens with the shell command "+alias+".", t))
	}
	if row.GetDistribution() != "" {
		from := "Installed from the distribution " + row.GetDistribution()
		if version := row.GetDistributionVersion(); version != "" {
			from += ", version " + version
		}
		lines = append(lines, render.Dim(from+".", t))
	}
	if previous := row.GetPreviousNames(); len(previous) > 0 {
		lines = append(lines, render.Dim("Was called "+strings.Join(previous, ", ")+".", t))
	}
	return lines
}

// Something a profile's list of actions offers: its name, what it does, the word on its button and the run of it
type action struct {
	title, about, verb string
	run                func() tea.Cmd
}

// Everything that can be done with one profile, on a list of its own read again with the profiles
func (p *profiles) actions(name string) tea.Cmd {
	t := p.kit.Env.Theme
	row := p.find(name)
	if row == nil {
		return nil
	}
	var shown []action
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   label(row),
		Glyph:   t.Glyphs.Profiles,
		Key:     "profiles:" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			shown = nil
			if now := p.find(name); now != nil {
				shown = p.acts(list, now)
			}
			cells := make([][]string, len(shown))
			for i, act := range shown {
				cells[i] = []string{act.title, render.Dim(act.about, t)}
			}
			return cells
		},
		Detail: func(i int) []string {
			var lines []string
			if i >= 0 && i < len(shown) && !list.Shows(1) {
				lines = append(lines, shown[i].about, "")
			}
			if now := p.find(name); now != nil {
				lines = append(lines, p.detail(now, true)...)
			}
			return lines
		},
		Empty: func() string { return "This profile is gone." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: shown[i].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return shown[i].run() },
		},
	})
	list.ToggleDetail()
	p.views = append(p.views, list)
	return p.hub.Push(list)
}

// Actions of a profile as it is now: switching to it, describing it, renaming and exporting it, its details, its shell command, its
// distribution and deleting it
func (p *profiles) acts(list *cards.List, row *profilev1.Profile) []action {
	alias, _ := profile.AliasOf(row.GetName())
	name := row.GetName()
	var acts []action
	if !row.GetActive() || !row.GetSticky() {
		acts = append(acts, action{"Switch to it", "This window runs on it now, and icebath opens with it from now on", "switch", func() tea.Cmd {
			return tea.Batch(p.hub.PopTo(p.list), p.use(name))
		}})
	}
	acts = append(acts,
		action{"Describe", "Say what the profile is for, so icebath can pick it for a task", "describe", func() tea.Cmd { return p.describe(row) }},
		action{"Write a description for me", "The helper model reads its skills and settings and writes one", "write", func() tea.Cmd { return p.describeAuto(row) }},
		action{"Rename", "Its sessions and shell command follow the new name", "rename", func() tea.Cmd { return p.rename(row) }},
		action{"Export to a file", "Save its settings, skills and look to a file you can share", "export", func() tea.Cmd { return p.export(row) }},
		action{"Show details", "Where it is kept, its model, skills and files", "show", func() tea.Cmd { return p.show(row) }})
	if alias == "" {
		acts = append(acts, action{"Make a shell command", "A command named " + name + " that opens icebath on this profile", "make", func() tea.Cmd {
			return p.alias(row, "")
		}})
	} else {
		acts = append(acts, action{"Remove the shell command", "The command " + alias + " stops opening icebath on this profile", "remove", func() tea.Cmd {
			list.Confirm("Remove the shell command "+alias+"? The profile stays as it is.", func() tea.Cmd { return p.alias(row, alias) })
			return nil
		}})
	}
	if row.GetDistribution() != "" {
		acts = append(acts,
			action{"Distribution details", "Its version, what it needs and where it came from", "show", func() tea.Cmd { return p.info(row) }},
			action{"Update from the distribution", "Get it again, keeping your sessions, memories and keys", "update", func() tea.Cmd {
				list.Confirm("Update "+name+" from "+row.GetDistribution()+"? Your sessions, memories and keys stay.", func() tea.Cmd { return p.update(row) })
				return nil
			}})
	}
	if !row.GetDefault() {
		acts = append(acts, action{"Delete", "Remove the profile with its sessions and files", "delete", func() tea.Cmd {
			return tea.Batch(p.hub.PopTo(p.list), p.remove(row))
		}})
	}
	return acts
}

// Page of lines on the hub
func (p *profiles) page(title, key string, lines []string) tea.Cmd {
	return p.hub.Push(cards.NewPage(p.kit.Env, cards.PageSpec{Title: title, Key: key, Lines: func(w int) []string {
		var out []string
		for _, line := range lines {
			out = append(out, strings.Split(render.Wrap(line, max(w, 8)), "\n")...)
		}
		return out
	}}))
}

// Details of a profile on a page: where it is kept, its model and skills, its files, its distribution and its shell command
func (p *profiles) show(row *profilev1.Profile) tea.Cmd {
	t := p.kit.Env.Theme
	there := func(yes bool) string {
		if yes {
			return "there"
		}
		return "not made yet"
	}
	lines := []string{render.Heading(label(row), t), "", "Folder: " + profile.Tilde(row.GetPath())}
	if model := row.GetModel(); model != "" {
		if row.GetProvider() != "" {
			model += " from " + row.GetProvider()
		}
		lines = append(lines, "Model: "+model)
	}
	lines = append(lines, "Skills: "+strconv.Itoa(int(row.GetSkills())), "Keys file: "+there(row.GetHasEnv()), "Personality file SOUL.md: "+there(row.GetHasSoul()))
	if row.GetDistribution() != "" {
		from := "Distribution: " + row.GetDistribution()
		if version := row.GetDistributionVersion(); version != "" {
			from += ", version " + version
		}
		lines = append(lines, from)
		if source := row.GetDistributionSource(); source != "" {
			lines = append(lines, "Installed from: "+source)
		}
	}
	if alias, _ := profile.AliasOf(row.GetName()); alias != "" {
		command := "Shell command: " + alias
		if path, err := profile.AliasPath(alias); err == nil {
			command += ", kept at " + profile.Tilde(path)
		}
		lines = append(lines, command)
	}
	if previous := row.GetPreviousNames(); len(previous) > 0 {
		lines = append(lines, "Was called: "+strings.Join(previous, ", "))
	}
	return p.page("Details", "profile:"+row.GetName(), lines)
}

// Uses the profile now and when icebath opens, then tells every screen the settings it follows as the profile has them
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
				return p.fail("Could not switch to " + name + ": " + strings.TrimSuffix(tui.Reason(err), ".") + ".")
			}
			told := "This window runs on " + used.GetProfile().GetName() + " now, and icebath opens with it."
			cmds := []tea.Cmd{p.kit.WM.Toast(told, wm.Success, wm.DefaultToastTTL)}
			for key, value := range values {
				cmds = append(cmds, p.kit.Changed(key, value))
			}
			if p.hub != nil && p.hub.Shown() {
				cmds = append(cmds, p.load())
			}
			return tea.Batch(cmds...)
		}
	})
}

// Asks for a profile's description, saved at once
func (p *profiles) describe(row *profilev1.Profile) tea.Cmd {
	field := cards.Field{Label: "Description", Value: row.GetDescription(), Help: describeHelp}
	return p.kit.Ask(p.hub, "Describe", field, "Save", func(text string) tea.Cmd {
		p.hub.Busy("Saving the description of " + row.GetName() + "…")
		return tui.Call(p.hub, func() (*profilev1.DescribeResponse, error) {
			return p.service.Describe(p.kit.Env.Ctx, &profilev1.DescribeRequest{Name: row.GetName(), Text: text})
		}, func(*profilev1.DescribeResponse) tea.Cmd {
			return tea.Batch(p.load(), p.hub.Done("Saved the description of "+row.GetName()+"."))
		})
	})
}

// Has the helper model write a profile's description from its skills and settings
func (p *profiles) describeAuto(row *profilev1.Profile) tea.Cmd {
	p.hub.Busy("The helper model is writing a description of " + row.GetName() + "…")
	return tui.Call(p.hub, func() (*profilev1.DescribeResponse, error) {
		return p.service.Describe(p.kit.Env.Ctx, &profilev1.DescribeRequest{Name: row.GetName(), Auto: true, Overwrite: true})
	}, func(response *profilev1.DescribeResponse) tea.Cmd {
		for _, outcome := range response.GetOutcomes() {
			if !outcome.GetOk() {
				return p.hub.Fail("No description was written: " + strings.TrimSuffix(outcome.GetReason(), ".") + ".")
			}
		}
		return tea.Batch(p.load(), p.hub.Done("The helper model described "+row.GetName()+"."))
	})
}

// Words of the marks of a line a profile command reports, without the mark, and whether it says something failed
func unmark(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if said, failed := strings.CutPrefix(line, "⚠"); failed {
		return strings.TrimSpace(said), true
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "✓")), false
}

// What failed among the lines a profile command reports, without their marks
func failures(lines []string) []string {
	var out []string
	for _, line := range lines {
		if said, failed := unmark(line); failed && said != "" {
			out = append(out, said)
		}
	}
	return out
}

// Renames a profile, the one this window runs on included, or sets the name the default profile shows; its alias follows. What failed is
// said, and a rename whose sessions did not all move can be finished from a list of its own
func (p *profiles) rename(row *profilev1.Profile) tea.Cmd {
	field := cards.Field{Label: "New name", Value: row.GetName(), Help: "Lower case letters, digits and dashes, such as work or research.",
		Check: func(value string) string {
			if _, err := profile.Canon(strings.TrimSpace(value)); err != nil {
				return tui.Reason(err)
			}
			return ""
		}}
	if row.GetDefault() {
		field = cards.Field{Label: "Name shown", Value: row.GetDisplayName(), Help: "The name the default profile is shown by. Its folder keeps its name."}
	}
	return p.kit.Ask(p.hub, "Rename", field, "Rename", func(name string) tea.Cmd {
		if name == "" || name == row.GetName() {
			return nil
		}
		p.hub.Busy("Renaming " + row.GetName() + "…")
		return tui.Call(p.hub, func() (*profilev1.RenameResponse, error) {
			return p.service.Rename(p.kit.Env.Ctx, &profilev1.RenameRequest{Old: row.GetName(), New: name})
		}, func(done *profilev1.RenameResponse) tea.Cmd {
			p.hub.Quiet()
			if row.GetDefault() {
				return tea.Batch(p.load(), p.hub.Done("The default profile is shown as "+name+" now."))
			}
			renamed, _ := profile.Canon(name)
			problems := failures(slices.Concat(done.GetLines(), profile.FollowAlias(row.GetName(), renamed), done.GetLater()))
			cmds := []tea.Cmd{p.load()}
			// Screens that follow the profile in use hear of its new name
			if row.GetActive() {
				cmds = append(cmds, p.kit.Changed(profileconfig.Active, renamed))
			}
			told := "Renamed " + row.GetName() + " to " + renamed + ". Its sessions and shell command follow."
			switch {
			case !done.GetSettled():
				return tea.Batch(append(cmds, p.unsettled("Finish the rename", "Renamed "+row.GetName()+" to "+renamed+", but not everything it keeps moved.",
					problems, func() ([]string, bool, error) {
						moved, err := p.service.MigrateIdentity(p.kit.Env.Ctx, &profilev1.MigrateIdentityRequest{Old: row.GetName(), New: renamed})
						return moved.GetLines(), moved.GetSettled(), err
					}, "Finished the rename of "+renamed+"."))...)
			case len(problems) > 0:
				cmds = append(cmds, p.hub.Fail("Renamed "+row.GetName()+" to "+renamed+", but "+lower(strings.Join(problems, " "))))
			default:
				cmds = append(cmds, p.hub.Done(told))
			}
			return tea.Batch(cmds...)
		})
	})
}

// Text with a lower case first letter, as it reads inside a sentence
func lower(text string) string {
	if text == "" {
		return ""
	}
	return strings.ToLower(text[:1]) + text[1:]
}

// List of what a rename or delete left undone and one row that tries again, the list closing once nothing is left
func (p *profiles) unsettled(title, lead string, problems []string, again func() ([]string, bool, error), finished string) tea.Cmd {
	t := p.kit.Env.Theme
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Profiles,
		Key:     "profiles:unsettled:" + title,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			lines := []string{render.Warm(lead, t)}
			for _, problem := range problems {
				lines = append(lines, render.Dim(problem, t))
			}
			return lines
		},
		Columns: []render.Column{{}, {Flex: 1}},
		Rows:    func() [][]string { return [][]string{{"Try again", render.Dim("Move what is left now", t)}} },
		Hints:   func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.use", Label: "try again"}} },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(*cards.List, int) tea.Cmd {
				p.hub.Busy("Trying again…")
				return tui.Call(p.hub, func() ([]string, error) {
					lines, settled, err := again()
					if err == nil && settled {
						return nil, nil
					}
					if err == nil {
						lines = append(failures(lines), "Some of it is still left.")
					}
					return lines, err
				}, func(left []string) tea.Cmd {
					if left == nil {
						return tea.Batch(p.hub.Close(list), p.load(), p.hub.Done(finished))
					}
					problems = left
					list.Refresh()
					return p.hub.Fail("Not everything moved yet. Try again in a moment.")
				})
			},
		},
	})
	return p.hub.Push(list)
}

// Deletes a profile after a question, refusing the one this window runs on; what failed is said, and a delete that left something behind
// can be finished from a list of its own
func (p *profiles) remove(row *profilev1.Profile) tea.Cmd {
	switch {
	case row.GetDefault():
		return nil
	case row.GetActive():
		return p.hub.Fail("This window runs on " + row.GetName() + ". Switch to another profile first, then delete it.")
	}
	p.list.Confirm("Delete "+row.GetName()+"? Its sessions, settings and files are gone for good.", func() tea.Cmd {
		p.hub.Busy("Deleting " + row.GetName() + "…")
		return tui.Call(p.hub, func() (*profilev1.DeleteResponse, error) {
			return p.service.Delete(p.kit.Env.Ctx, &profilev1.DeleteRequest{Name: row.GetName()})
		}, func(done *profilev1.DeleteResponse) tea.Cmd {
			p.hub.Quiet()
			problems := failures(slices.Concat(done.GetLines(), profile.DropAlias(row.GetName()), done.GetLater()))
			switch {
			case !done.GetSettled():
				return tea.Batch(p.load(), p.unsettled("Finish the delete", "Deleted "+row.GetName()+", but some of what it kept elsewhere is left.",
					problems, func() ([]string, bool, error) {
						purged, err := p.service.PurgeIdentity(p.kit.Env.Ctx, &profilev1.PurgeIdentityRequest{Name: row.GetName()})
						return purged.GetLines(), purged.GetSettled(), err
					}, "Finished the delete of "+row.GetName()+"."))
			case done.GetFailure() != "":
				return tea.Batch(p.load(), p.hub.Fail("Deleted "+row.GetName()+", but its folder stayed: "+strings.TrimSuffix(done.GetFailure(), ".")+
					". Remove "+profile.Tilde(done.GetPath())+" yourself."))
			case len(problems) > 0:
				return tea.Batch(p.load(), p.hub.Fail("Deleted "+row.GetName()+", but "+lower(strings.Join(problems, " "))))
			}
			return tea.Batch(p.load(), p.hub.Done("Deleted "+row.GetName()+"."))
		})
	})
	return nil
}

// Writes a profile to a shareable archive, where asked or in the folder exports go to
func (p *profiles) export(row *profilev1.Profile) tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title: "Export",
		Intro: []string{"Saves the settings, skills and look of " + row.GetName() + " to a file you can share. Keys and sessions stay out."},
		Fields: []cards.Field{{ID: "output", Label: "Save to", Placeholder: "The folder icebath keeps exports in",
			Help: "A file path ending in .tar.gz.\nLeave it empty to save in the folder icebath keeps exports in."}},
		Submit: "Export",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			output := strings.TrimSpace(values["output"])
			p.hub.Busy("Exporting " + row.GetName() + "…")
			return tui.Call(p.hub, func() ([2]string, error) {
				name, path, err := write(p.kit, p.service, row.GetName(), output)
				return [2]string{name, path}, err
			}, func(done [2]string) tea.Cmd {
				return tea.Batch(p.hub.Close(form), p.hub.Done("Exported "+done[0]+" to "+profile.Tilde(done[1])+"."))
			})
		},
	})
	return p.hub.Push(form)
}

// Writes the shell command that opens a profile, or removes the one named
func (p *profiles) alias(row *profilev1.Profile, remove string) tea.Cmd {
	if remove != "" {
		if _, err := profile.RemoveAlias(remove); err != nil {
			return p.hub.Fail("The shell command " + remove + " could not be removed: " + strings.TrimSuffix(tui.Reason(err), ".") + ".")
		}
		return tea.Batch(p.load(), p.hub.Done("Removed the shell command "+remove+"."))
	}
	collision, err := profile.Collision(row.GetName())
	switch {
	case err != nil:
		return p.hub.Fail("The shell command could not be made: " + strings.TrimSuffix(tui.Reason(err), ".") + ".")
	case collision != "":
		return p.hub.Fail("No shell command was made: " + strings.TrimSuffix(collision, ".") + ".")
	}
	path, err := profile.MakeAlias(row.GetName(), "")
	if err != nil {
		return p.hub.Fail("The shell command could not be made: " + strings.TrimSuffix(tui.Reason(err), ".") + ".")
	}
	told := "The shell command " + row.GetName() + " now opens icebath on this profile."
	if onPath, err := profile.AliasesOnPath(); err == nil && !onPath {
		dir, _ := profile.AliasDir()
		told = "Made the shell command " + profile.Tilde(path) + ". Add " + profile.Tilde(dir) + " to your PATH to run it by name."
	}
	return tea.Batch(p.load(), p.hub.Done(told))
}

// What a requirement of a distribution needs, in a few words
func needs(requirement *profilev1.Requirement) string {
	switch {
	case requirement.GetSet():
		return "set"
	case requirement.GetRequired():
		return "needed, not set"
	}
	return "not set"
}

// Lines of what a distribution is: its name and version, what it says it does, who made it, what it needs and where it came from
func manifestLines(m *profilev1.Manifest, t theme.Theme) []string {
	lines := []string{render.Heading(m.GetName()+version(m.GetVersion()), t)}
	if m.GetDescription() != "" {
		lines = append(lines, m.GetDescription())
	}
	lines = append(lines, "")
	for _, fact := range [][2]string{{"Made by", m.GetAuthor()}, {"License", m.GetLicense()}, {"Needs icebath", m.GetRequires()}, {"From", m.GetSource()},
		{"Installed", m.GetInstalledAt()}} {
		if fact[1] != "" {
			lines = append(lines, fact[0]+": "+fact[1])
		}
	}
	if env := m.GetEnv(); len(env) > 0 {
		lines = append(lines, "", render.Heading("Keys it uses", t))
		for _, requirement := range env {
			line := "  " + requirement.GetName() + render.Dim(", "+needs(requirement), t)
			if about := requirement.GetDescription(); about != "" {
				line += render.Dim(". "+about, t)
			}
			if def := requirement.GetDefault(); def != "" {
				line += render.Dim(" Default: "+def, t)
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// Version of a distribution as said after its name, nothing when it has none
func version(v string) string {
	if v == "" {
		return ""
	}
	return ", version " + v
}

// Page of what a profile's distribution is and needs
func (p *profiles) info(row *profilev1.Profile) tea.Cmd {
	return tui.Call(p.hub, func() (*profilev1.InfoResponse, error) {
		return p.service.Info(p.kit.Env.Ctx, &profilev1.InfoRequest{Name: row.GetName()})
	}, func(response *profilev1.InfoResponse) tea.Cmd {
		if !response.GetFound() {
			return p.hub.Fail(row.GetName() + " has no distribution.yaml, so it was not installed from a distribution.")
		}
		return p.page("Distribution", "distribution:"+row.GetName(), manifestLines(response.GetManifest(), p.kit.Env.Theme))
	})
}

// Pulls a profile's distribution again, its config.yaml kept
func (p *profiles) update(row *profilev1.Profile) tea.Cmd {
	p.hub.Busy("Updating " + row.GetName() + " from its distribution…")
	return tui.Call(p.hub, func() (*profilev1.UpdateResponse, error) {
		return p.service.Update(p.kit.Env.Ctx, &profilev1.UpdateRequest{Name: row.GetName()})
	}, func(response *profilev1.UpdateResponse) tea.Cmd {
		m := response.GetPlan().GetManifest()
		told := "Updated " + m.GetName() + version(m.GetVersion()) + "."
		if response.GetPlan().GetHasCron() {
			told += " Its scheduled jobs were refreshed, so check them with icebath -p " + m.GetName() + " cron list."
		}
		return tea.Batch(p.load(), p.hub.Done(told))
	})
}

// Ways to add a profile: create one, import an archive, or install a distribution
func (p *profiles) add() tea.Cmd {
	choices := []tui.Choice{
		{ID: "create", Title: "Create a profile", Detail: "A new set of settings and sessions of its own"},
		{ID: "import", Title: "Import a file", Detail: "Add a profile someone exported to a file"},
		{ID: "install", Title: "Install a distribution", Detail: "A ready-made profile from a git address or a folder"},
	}
	return p.kit.Pick(p.hub, "Add a profile", "add", choices, "", func(choice tui.Choice) tea.Cmd {
		switch choice.ID {
		case "import":
			return p.importArchive()
		case "install":
			return p.install()
		}
		return p.create()
	})
}

// Fields of the form of a new profile for how it starts: a copy names its source and what else it copies, a fresh one whether it takes your
// keys and the skills that come with icebath
func (p *profiles) fields(values map[string]string) []cards.Field {
	var sources []cards.Choice
	for _, row := range p.rows {
		choice := cards.Choice{Value: row.GetName(), Label: label(row)}
		if row.GetActive() {
			choice.Meaning = "The profile this window runs on"
			sources = append([]cards.Choice{choice}, sources...)
			continue
		}
		sources = append(sources, choice)
	}
	from := values["from"]
	if from == "" && len(sources) > 0 {
		from = sources[0].Value
	}
	value := func(id, fallback string) string {
		if v, ok := values[id]; ok {
			return v
		}
		return fallback
	}
	start := value("start", "fresh")
	fields := []cards.Field{
		{ID: "name", Label: "Name", Value: values["name"], Required: true, Help: "Lower case letters, digits and dashes, such as work or research.",
			Check: func(value string) string {
				if _, err := profile.Canon(strings.TrimSpace(value)); err != nil {
					return tui.Reason(err)
				}
				return ""
			}},
		{ID: "description", Label: "Description", Value: values["description"], Help: describeHelp},
		{ID: "start", Label: "Start from", Kind: cards.FieldChoice, Value: start, Choices: []cards.Choice{
			{Value: "fresh", Label: "Nothing", Meaning: "Default settings and the skills that come with icebath"},
			{Value: "settings", Label: "A copy of the settings", Meaning: "The settings, keys and skills of the profile you pick"},
			{Value: "everything", Label: "A full copy", Meaning: "Everything of the profile you pick except its history"}}},
	}
	if start == "fresh" {
		fields = append(fields,
			cards.Field{ID: "logins", Label: "Keys and sign-ins", Kind: cards.FieldChoice, Value: value("logins", "copy"), Choices: []cards.Choice{
				{Value: "copy", Label: "Copy yours", Meaning: "Your API keys, sign-ins and model, copied once. Later changes stay apart"},
				{Value: "none", Label: "None", Meaning: "No keys or sign-ins. Add them in the new profile"}}},
			cards.Field{ID: "skills", Label: "Bundled skills", Kind: cards.FieldBool, Value: value("skills", "true"),
				Help: "The skills that come with icebath, kept up to date as icebath updates."})
	} else {
		fields = append(fields, cards.Field{ID: "from", Label: "Copy from", Kind: cards.FieldChoice, Value: from, Choices: sources})
	}
	fields = append(fields,
		cards.Field{ID: "model", Label: "Model", Value: values["model"], More: true, Help: "Leave it empty to start with the model of this window's profile.\n" +
			"A model goes with the provider that runs it."},
		cards.Field{ID: "provider", Label: "Provider", Value: values["provider"], More: true, Help: "The provider that runs the model, such as anthropic."})
	if start != "fresh" {
		fields = append(fields,
			cards.Field{ID: "channels", Label: "Copy chat apps", Kind: cards.FieldBool, Value: value("channels", "false"), More: true,
				Help: "Bot tokens too. Two profiles on one bot both answer every message."},
			cards.Field{ID: "sync", Label: "Keep syncing imports", Kind: cards.FieldBool, Value: value("sync", "false"), More: true,
				Help: "Keep pulling the same Claude Code or Codex folders that the source profile imports."})
	}
	return append(fields, cards.Field{ID: "alias", Label: "Shell command", Kind: cards.FieldBool, Value: value("alias", "true"), More: true,
		Help: "A command named after the profile that opens icebath on it."})
}

// Form of a new profile whose fields follow how it starts, kept open with what was typed when the create is refused
func (p *profiles) create() tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  "Create a profile",
		Intro:  []string{"A new set of settings, providers and sessions. Switch to it once it is made."},
		Fields: p.fields(map[string]string{}),
		Submit: "Create",
		Cancel: func(*cards.Form) tea.Cmd { return p.hub.Close(form) },
		Changed: func(f *cards.Form, field cards.Field) tea.Cmd {
			if field.ID == "start" {
				f.SetFields(p.fields(f.Values()))
			}
			return nil
		},
		Done: func(f *cards.Form, v map[string]string) tea.Cmd {
			name := strings.TrimSpace(v["name"])
			request := &profilev1.CreateRequest{Name: name, Description: strings.TrimSpace(v["description"]), NoSkills: v["skills"] == "false",
				Model: strings.TrimSpace(v["model"]), Provider: strings.TrimSpace(v["provider"])}
			switch {
			case request.Model != "" && request.Provider == "":
				f.SetError("provider", "Name the provider that runs "+request.Model+".")
				return nil
			case request.Model == "" && request.Provider != "":
				f.SetError("model", "Name a model of "+request.Provider+", or leave both empty.")
				return nil
			}
			if v["start"] != "fresh" {
				request.Clone, request.From, request.All = true, v["from"], v["start"] == "everything"
				request.Channels, request.SyncImports, request.NoSkills = v["channels"] == "true", v["sync"] == "true", false
			} else {
				request.NoCredentials = v["logins"] == "none"
			}
			f.SetBusy(true)
			p.hub.Busy("Creating " + name + "…")
			return tui.Async(func() func() tea.Cmd {
				made, err := p.service.Create(p.kit.Env.Ctx, request)
				return func() tea.Cmd {
					f.SetBusy(false)
					p.hub.Quiet()
					if err != nil {
						f.SetError("name", tui.Reason(err))
						return nil
					}
					canon := made.GetProfile().GetName()
					var problems []string
					if v["alias"] == "true" {
						if collision, err := profile.Collision(canon); err == nil && collision == "" {
							if _, err := profile.MakeAlias(canon, ""); err != nil {
								problems = append(problems, "The shell command could not be made: "+strings.TrimSuffix(tui.Reason(err), ".")+".")
							}
						} else if collision != "" {
							problems = append(problems, "No shell command was made: "+strings.TrimSuffix(collision, ".")+".")
						}
					}
					problems = append(problems, made.GetWarnings()...)
					cmds := []tea.Cmd{p.hub.Close(form), p.load()}
					if len(problems) > 0 {
						return tea.Batch(append(cmds, p.hub.Fail("Created "+canon+", but some steps failed: "+strings.Join(problems, " ")))...)
					}
					return tea.Batch(append(cmds, p.hub.Done("Created "+canon+" at "+profile.Tilde(made.GetPath())+". Open it to switch to it."))...)
				}
			})
		},
		Pick: func(f *cards.Form, field cards.Field) tea.Cmd {
			choices := make([]tui.Choice, len(field.Choices))
			for i, c := range field.Choices {
				choices[i] = tui.Choice{ID: c.Value, Title: c.Label, Detail: c.Meaning}
			}
			return p.kit.Pick(p.hub, field.Label, "choose", choices, field.Value, func(choice tui.Choice) tea.Cmd {
				return f.Commit(field.ID, choice.ID)
			})
		},
	})
	return p.hub.Push(form)
}

// Imports a shared archive as a new profile
func (p *profiles) importArchive() tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title: "Import a file",
		Intro: []string{"Adds a profile from a file that someone exported."},
		Fields: []cards.Field{
			{ID: "archive", Label: "File", Required: true, Placeholder: "~/profile.tar.gz", Help: "The path of the .tar.gz file."},
			{ID: "name", Label: "Name", Help: "Leave it empty to use the name the file carries."},
		},
		Submit: "Import",
		Done: func(_ *cards.Form, v map[string]string) tea.Cmd {
			path := fsx.ExpandHome(strings.TrimSpace(v["archive"]))
			form.SetBusy(true)
			p.hub.Busy("Importing " + filepath.Base(path) + "…")
			return tui.Async(func() func() tea.Cmd {
				imported, err := func() (*profilev1.ImportResponse, error) {
					data, err := os.ReadFile(path)
					if errors.Is(err, fs.ErrNotExist) {
						return nil, errors.New("There is no file at " + path + ".")
					}
					if err != nil {
						return nil, err
					}
					return p.service.Import(p.kit.Env.Ctx, &profilev1.ImportRequest{Archive: data, Name: strings.TrimSpace(v["name"])})
				}()
				return func() tea.Cmd {
					form.SetBusy(false)
					p.hub.Quiet()
					if err != nil {
						form.SetError("archive", tui.Reason(err))
						return nil
					}
					if collision, err := profile.Collision(imported.GetName()); err == nil && collision == "" {
						profile.MakeAlias(imported.GetName(), "")
					}
					return tea.Batch(p.hub.Close(form), p.load(), p.hub.Done("Imported "+imported.GetName()+" at "+profile.Tilde(imported.GetPath())+"."))
				}
			})
		},
	})
	return p.hub.Push(form)
}

// Installs a distribution from a git address or a local folder: what it would do shown first, installed once confirmed
func (p *profiles) install() tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title: "Install a distribution",
		Intro: []string{"A distribution is a ready-made profile someone shares. You see what it brings before it is installed."},
		Fields: []cards.Field{
			{ID: "source", Label: "From", Required: true, Placeholder: "github.com/user/repo", Help: "A git address, or a folder that holds distribution.yaml."},
			{ID: "name", Label: "Name", Help: "Leave it empty to use the name the distribution gives."},
			{ID: "alias", Label: "Shell command", Kind: cards.FieldBool, Value: "false", Help: "A command named after the profile that opens icebath on it."},
			{ID: "force", Label: "Replace a profile of that name", Kind: cards.FieldBool, Value: "false", Help: "Its own sessions, memories and keys stay."},
		},
		Submit: "Next",
		Done: func(_ *cards.Form, v map[string]string) tea.Cmd {
			source := strings.TrimSpace(v["source"])
			if expanded := fsx.ExpandHome(source); fsx.IsDir(expanded) {
				if abs, err := filepath.Abs(expanded); err == nil {
					source = abs
				}
			}
			name, alias, force := strings.TrimSpace(v["name"]), v["alias"] == "true", v["force"] == "true"
			form.SetBusy(true)
			p.hub.Busy("Reading the distribution…")
			return tui.Async(func() func() tea.Cmd {
				planned, err := p.service.Plan(p.kit.Env.Ctx, &profilev1.PlanRequest{Source: source, Name: name})
				return func() tea.Cmd {
					form.SetBusy(false)
					p.hub.Quiet()
					if err != nil {
						form.SetError("source", tui.Reason(err))
						return nil
					}
					return p.hub.Push(p.planPage(form, planned.GetPlan(), source, name, alias, force))
				}
			})
		},
	})
	return p.hub.Push(form)
}

// Lines of what an install would do: the distribution, where it goes, what it replaces, the keys it uses and the jobs it brings
func planLines(plan *profilev1.Plan, t theme.Theme) []string {
	lines := manifestLines(plan.GetManifest(), t)
	lines = append(lines, "", "From: "+plan.GetProvenance(), "Goes to: "+profile.Tilde(plan.GetTarget()))
	if plan.GetExisting() {
		if plan.GetDistribution() {
			lines = append(lines, "", "A profile of that name is there already. Only the files this distribution owns are replaced.")
		} else {
			lines = append(lines, "", render.Warm("A profile of that name is there already, and it is not a distribution. Its SOUL.md, its mcp.json, and any "+
				"skill or scheduled job of the same name are replaced. Your memories, sessions, sign-ins and keys file stay.", t))
		}
	}
	if plan.GetHasCron() {
		lines = append(lines, "", render.Warm("It brings scheduled jobs. They don't run until you turn them on.", t))
	}
	return lines
}

// Page of what an install would do, which installs on enter and then says what is next
func (p *profiles) planPage(form *cards.Form, plan *profilev1.Plan, source, name string, alias, force bool) *cards.Page {
	t := p.kit.Env.Theme
	m := plan.GetManifest()
	lines := planLines(plan, t)
	var page *cards.Page
	page = cards.NewPage(p.kit.Env, cards.PageSpec{
		Title: m.GetName(),
		Key:   "install:" + m.GetName(),
		Lines: func(w int) []string {
			var out []string
			for _, line := range lines {
				out = append(out, strings.Split(render.Wrap(line, max(w, 8)), "\n")...)
			}
			return out
		},
		Context: cards.HubContext,
		Hints:   func(*cards.Page) []wm.Hint { return []wm.Hint{{Action: "hub.use", Label: "install"}} },
		Actions: map[string]func(*cards.Page) tea.Cmd{
			"hub.use": func(*cards.Page) tea.Cmd {
				p.hub.Busy("Installing " + m.GetName() + "…")
				return tui.Call(p.hub, func() (*profilev1.InstallResponse, error) {
					return p.service.Install(p.kit.Env.Ctx, &profilev1.InstallRequest{Source: source, Name: name, Force: force})
				}, func(installed *profilev1.InstallResponse) tea.Cmd {
					p.hub.Quiet()
					done := installed.GetPlan()
					made := done.GetManifest()
					result := []string{render.Good("Installed "+made.GetName()+version(made.GetVersion())+" at "+profile.Tilde(done.GetTarget())+".", t), ""}
					if alias {
						if collision, err := profile.Collision(made.GetName()); err == nil && collision == "" {
							if _, err := profile.MakeAlias(made.GetName(), ""); err != nil {
								result = append(result, render.Warm("The shell command could not be made: "+strings.TrimSuffix(tui.Reason(err), ".")+".", t))
							}
						} else if collision != "" {
							result = append(result, render.Warm("No shell command was made: "+strings.TrimSuffix(collision, ".")+".", t))
						}
					}
					if len(made.GetEnv()) > 0 {
						result = append(result, "It uses keys. Copy .env.EXAMPLE to .env in "+profile.Tilde(done.GetTarget())+" and fill them in.")
					}
					if done.GetHasCron() {
						result = append(result, "Its scheduled jobs don't run until you turn them on. See them with icebath -p "+made.GetName()+" cron list.")
					}
					result = append(result, "Switch to it from the list of profiles, or run icebath -p "+made.GetName()+" chat.")
					return tea.Batch(p.hub.Close(page), p.hub.Close(form), p.load(), p.page("Installed", "installed:"+made.GetName(), result))
				})
			},
		},
	})
	return page
}

func (p *profiles) fail(text string) tea.Cmd {
	if p.hub != nil && p.hub.Shown() {
		return p.hub.Fail(text)
	}
	return p.kit.WM.Toast(text, wm.Error, wm.DefaultToastTTL)
}

// Writes a profile's archive where asked, else in the folder exports go to: its name and where it went
func write(kit *tui.Kit, service profilev1connect.ProfileServiceClient, name, output string) (string, string, error) {
	exported, err := service.Export(kit.Env.Ctx, &profilev1.ExportRequest{Name: name})
	if err != nil {
		return "", "", err
	}
	path, err := profile.SaveArchive(exported.GetArchive(), exported.GetName(), output)
	return exported.GetName(), path, err
}

// /export [profile] [-o output.tar.gz]: the profile named, the one in use for none, to an archive
func exportSlash(kit *tui.Kit, service profilev1connect.ProfileServiceClient, argument string) tea.Cmd {
	const usage = "Type /export, then a profile and -o with the file to write, such as /export work -o work.tar.gz."
	words := strings.Fields(argument)
	name, output := "", ""
	for i := 0; i < len(words); i++ {
		switch {
		case words[i] == "-o":
			if i+1 >= len(words) {
				return kit.WM.Toast(usage, wm.Warn, wm.DefaultToastTTL)
			}
			output = words[i+1]
			i++
		case name == "":
			name = words[i]
		default:
			return kit.WM.Toast(usage, wm.Warn, wm.DefaultToastTTL)
		}
	}
	return tui.Async(func() func() tea.Cmd {
		exported, path, err := write(kit, service, name, output)
		return func() tea.Cmd {
			if err != nil {
				return kit.WM.Toast("The export failed: "+strings.TrimSuffix(tui.Reason(err), ".")+".", wm.Error, wm.DefaultToastTTL)
			}
			return kit.WM.Toast("Exported "+exported+" to "+profile.Tilde(path)+". Someone else adds it with icebath profile import and the file.",
				wm.Success, wm.DefaultToastTTL)
		}
	})
}
