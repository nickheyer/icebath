package app

import (
	"errors"
	"fmt"
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
	profilecli "github.com/nickheyer/icebath/internal/profile/cli"
	profileconfig "github.com/nickheyer/icebath/internal/profile/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	profilev1 "github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1/profilev1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	// Screen of every profile
	Screen = "profiles"
	// Export of a profile to an archive, opened by /export
	ExportScreen = "export"
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
			Title: "Profiles",
			Short: "Separate icebaths with their own settings: create, switch, rename",
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
			Title: "Export profile",
			Short: "Export a profile (config, skills, theme) to a shareable archive",
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
	loaded   bool
	err      error
}

// Name a profile shows under, its display name beside it when it has one
func label(row *profilev1.Profile) string {
	return profile.Label(row.GetName(), row.GetDisplayName())
}

// Profiles screen: every profile with its model, skills and what it is for, the one this window runs on and the one icebath opens with
// marked
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
		Columns: []render.Column{{Title: ""}, {Title: "profile", Max: 24}, {Title: "model", Max: 28}, {Title: "skills", Right: true},
			{Title: "what it is for", Flex: 1}},
		Yield: []int{3, 2},
		Rows: func() [][]string {
			rows := make([][]string, len(p.rows))
			for i, row := range p.rows {
				mark := ""
				if row.GetActive() {
					mark += t.Glyphs.Live
				}
				if row.GetSticky() {
					mark += "★"
				}
				model := row.GetModel()
				if model == "" {
					model = "—"
				}
				rows[i] = []string{mark, label(row), model, strconv.Itoa(int(row.GetSkills())), render.Dim(strings.ReplaceAll(row.GetDescription(), "\n", " "), t)}
			}
			return rows
		},
		Detail: func(i int) []string { return p.detail(p.rows[i]) },
		Status: func() string {
			if len(p.rows) == 0 {
				return ""
			}
			return t.Glyphs.Live + " this window  ★ icebath opens with it"
		},
		Empty: func() string {
			if !p.loaded && p.err == nil {
				return "Reading the profiles…"
			}
			return tui.Waiting(p.loaded, p.err, "No profiles to show. alt+n creates one.")
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			hints := []wm.Hint{}
			if i >= 0 && i < len(p.rows) {
				hints = append(hints, wm.Hint{Action: "hub.use", Label: "open"}, wm.Hint{Action: "hub.edit", Label: "describe"}, wm.Hint{Action: "hub.rename"})
				if !p.rows[i].GetDefault() {
					hints = append(hints, wm.Hint{Action: "hub.remove"})
				}
			}
			return append(hints, wm.Hint{Action: "hub.add"})
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return p.actions(p.rows[i]) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return p.add() },
			"hub.edit":    func(_ *cards.List, i int) tea.Cmd { return p.describe(p.rows[i]) },
			"hub.rename":  func(_ *cards.List, i int) tea.Cmd { return p.rename(p.rows[i]) },
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return p.remove(l, p.rows[i]) },
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
			p.rows, p.err, p.loaded = response.GetProfiles(), err, true
			if p.list != nil {
				p.list.Refresh()
			}
			return nil
		}
	})
}

// Lines of a profile's detail pane: where it lives, what it is for, what it runs, how it opens, what it was called and installed from
func (p *profiles) detail(row *profilev1.Profile) []string {
	t := p.kit.Env.Theme
	lines := []string{label(row) + " · " + profile.Tilde(row.GetPath())}
	if description := row.GetDescription(); description != "" {
		if row.GetDescriptionAuto() {
			description += render.Dim(" (written by icebath)", t)
		}
		lines = append(lines, description)
	}
	if model := row.GetModel(); model != "" {
		if row.GetProvider() != "" {
			model += " · " + row.GetProvider()
		}
		lines = append(lines, "model "+model)
	}
	switch {
	case row.GetActive() && row.GetSticky():
		lines = append(lines, render.Good("This window runs on this profile, and icebath opens with it.", t))
	case row.GetActive():
		lines = append(lines, render.Good("This window runs on this profile.", t))
	case row.GetSticky():
		lines = append(lines, "icebath opens with this profile.")
	}
	if alias, _ := profile.AliasOf(row.GetName()); alias != "" && !row.GetDefault() {
		lines = append(lines, render.Dim("opens with the shell command "+alias, t))
	}
	if row.GetDistribution() != "" {
		lines = append(lines, render.Dim(fmt.Sprintf("installed from %s (%s)", row.GetDistribution(), row.GetDistributionSource()), t))
	}
	if previous := row.GetPreviousNames(); len(previous) > 0 {
		lines = append(lines, render.Dim("was called "+strings.Join(previous, ", "), t))
	}
	return lines
}

// What can be done with one profile
func (p *profiles) actions(row *profilev1.Profile) tea.Cmd {
	alias, _ := profile.AliasOf(row.GetName())
	var choices []tui.Choice
	if !row.GetActive() || !row.GetSticky() {
		choices = append(choices, tui.Choice{ID: "use", Title: "Use this profile", Detail: "switch this window to it now, and open icebath on it from now on"})
	}
	choices = append(choices,
		tui.Choice{ID: "describe", Title: "Describe", Detail: "say what the profile is good at, for routing board tasks"},
		tui.Choice{ID: "auto", Title: "Write a description for me", Detail: "have the helper model read its skills and settings and write one"},
		tui.Choice{ID: "rename", Title: "Rename", Detail: "give the profile another name; its sessions and alias follow"},
		tui.Choice{ID: "export", Title: "Export to an archive", Detail: "write its settings, skills and look to a file you can share"},
		tui.Choice{ID: "show", Title: "Show details", Detail: "everything the profile command knows about it"})
	if alias == "" {
		choices = append(choices, tui.Choice{ID: "alias", Title: "Make a shell command", Detail: "a shell command named " + row.GetName() + " that opens this profile"})
	} else {
		choices = append(choices, tui.Choice{ID: "unalias", Title: "Remove the shell command", Detail: "remove the shell command " + alias})
	}
	if row.GetDistribution() != "" {
		choices = append(choices, tui.Choice{ID: "info", Title: "Distribution details", Detail: "version, requirements and where it was installed from"},
			tui.Choice{ID: "update", Title: "Update from its distribution", Detail: "pull the distribution again; your sessions, memories and keys stay"})
	}
	if !row.GetDefault() {
		choices = append(choices, tui.Choice{ID: "delete", Title: "Delete", Detail: "remove the profile, its sessions and its files"})
	}
	return p.kit.Pick(p.hub, label(row), "run", choices, "", func(choice tui.Choice) tea.Cmd {
		switch choice.ID {
		case "use":
			return p.use(row.GetName())
		case "describe":
			return p.describe(row)
		case "auto":
			return p.describeAuto(row)
		case "rename":
			return p.rename(row)
		case "export":
			return p.export(row)
		case "show":
			return p.page(label(row), "profile:"+row.GetName(), profilecli.ShowLines(row))
		case "alias":
			return p.alias(row, "")
		case "unalias":
			cmd := p.hub.PopTo(p.list)
			p.list.Confirm("Remove the shell command "+alias+"?", func() tea.Cmd { return p.alias(row, alias) })
			return cmd
		case "info":
			return p.info(row)
		case "update":
			cmd := p.hub.PopTo(p.list)
			p.list.Confirm("Update "+row.GetName()+" from its distribution?", func() tea.Cmd { return p.update(row) })
			return cmd
		case "delete":
			return p.remove(p.list, row)
		}
		return nil
	})
}

// Page of lines on the hub, such as what a command tells
func (p *profiles) page(title, key string, lines []string) tea.Cmd {
	return p.hub.Push(cards.NewPage(p.kit.Env, cards.PageSpec{Title: title, Key: key, Lines: func(int) []string { return lines }}))
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
				return p.fail(tui.Reason(err))
			}
			told := "This window runs on " + used.GetProfile().GetName() + " and icebath opens with it."
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
	field := cards.Field{Label: "What " + row.GetName() + " is for", Value: row.GetDescription(), Help: "One or two sentences; the board uses it to route tasks."}
	return p.kit.Ask(p.hub, "Describe", field, "Save", func(text string) tea.Cmd {
		p.hub.Busy("Saving…")
		return tui.Call(p.hub, func() (*profilev1.DescribeResponse, error) {
			return p.service.Describe(p.kit.Env.Ctx, &profilev1.DescribeRequest{Name: row.GetName(), Text: text})
		}, func(*profilev1.DescribeResponse) tea.Cmd {
			return tea.Batch(p.load(), p.hub.Done("Saved the description of "+row.GetName()+"."))
		})
	})
}

// Has the helper model write a profile's description from its skills and settings
func (p *profiles) describeAuto(row *profilev1.Profile) tea.Cmd {
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*profilev1.DescribeResponse, error) {
		return p.service.Describe(p.kit.Env.Ctx, &profilev1.DescribeRequest{Name: row.GetName(), Auto: true, Overwrite: true})
	}, func(response *profilev1.DescribeResponse) tea.Cmd {
		for _, outcome := range response.GetOutcomes() {
			if !outcome.GetOk() {
				return p.hub.Fail("No description was written: " + outcome.GetReason() + ".")
			}
		}
		return tea.Batch(p.load(), p.hub.Done("Saved the description of "+row.GetName()+"."))
	})
}

// Renames a profile, the one this window runs on included, or sets the name the default profile shows; its alias follows, and what the
// rename did shows as the command prints it
func (p *profiles) rename(row *profilev1.Profile) tea.Cmd {
	field := cards.Field{Label: "New name", Value: row.GetName(), Help: "Lowercase letters, digits and dashes, such as work or research."}
	if row.GetDefault() {
		field = cards.Field{Label: "Name shown for the default profile", Value: row.GetDisplayName()}
	}
	return p.kit.Ask(p.hub, "Rename", field, "Rename", func(name string) tea.Cmd {
		if name == "" || name == row.GetName() {
			return nil
		}
		return tui.Call(p.hub, func() (*profilev1.RenameResponse, error) {
			return p.service.Rename(p.kit.Env.Ctx, &profilev1.RenameRequest{Old: row.GetName(), New: name})
		}, func(done *profilev1.RenameResponse) tea.Cmd {
			lines := done.GetLines()
			if !row.GetDefault() {
				renamed, _ := profile.Canon(name)
				lines = slices.Concat(lines, profile.FollowAlias(row.GetName(), renamed), done.GetLater())
				if !done.GetSettled() {
					lines = append(lines, "Retry with:", "    icebath profile migrate-identity "+row.GetName()+" "+renamed)
				}
				lines = append(lines, "", "Profile renamed: "+row.GetName()+" → "+name, "Path: "+done.GetPath())
				// Screens that follow the profile in use hear of its new name
				if row.GetActive() {
					return tea.Batch(p.load(), p.kit.Changed(profileconfig.Active, renamed), p.page("Rename", "rename:"+name, lines))
				}
			}
			return tea.Batch(p.load(), p.page("Rename", "rename:"+name, lines))
		})
	})
}

// Deletes a profile after a confirmation, refusing the one this window runs on; what the delete did shows as the command prints it
func (p *profiles) remove(l *cards.List, row *profilev1.Profile) tea.Cmd {
	switch {
	case row.GetDefault():
		return nil
	case row.GetActive():
		return p.hub.Fail("This window runs on " + row.GetName() + ". Switch to another profile first, then delete it.")
	}
	cmd := p.hub.PopTo(p.list)
	l.Confirm("Delete the profile "+row.GetName()+" with its sessions and files?", func() tea.Cmd {
		return tui.Call(p.hub, func() (*profilev1.DeleteResponse, error) {
			return p.service.Delete(p.kit.Env.Ctx, &profilev1.DeleteRequest{Name: row.GetName()})
		}, func(done *profilev1.DeleteResponse) tea.Cmd {
			lines := slices.Concat(done.GetLines(), profile.DropAlias(row.GetName()), done.GetLater())
			switch {
			case done.GetFailure() != "":
				lines = append(lines, "Error: "+done.GetFailure())
			case !done.GetSettled():
				lines = append(lines, "", "Profile '"+row.GetName()+"' deleted.", "Error: Profile '"+row.GetName()+"' was deleted, but its session/routing "+
					"identity settlement is still pending — run: icebath profile purge-identity "+row.GetName())
			default:
				lines = append(lines, "", "Profile '"+row.GetName()+"' deleted.")
			}
			return tea.Batch(p.load(), p.page("Delete", "delete:"+row.GetName(), lines))
		})
	})
	return cmd
}

// Writes a profile to a shareable archive, where asked or in the folder exports go to
func (p *profiles) export(row *profilev1.Profile) tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  "Export to an archive",
		Intro:  []string{"Writes " + row.GetName() + "'s settings, skills and look to an archive. Keys and sessions stay out."},
		Fields: []cards.Field{{ID: "output", Label: "Save to", Placeholder: "the profile-exports folder", Help: "A path ending in .tar.gz; leave it empty for the usual folder."}},
		Submit: "Export",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			output := strings.TrimSpace(values["output"])
			p.hub.Busy("Exporting…")
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
			return p.hub.Fail(err.Error())
		}
		return tea.Batch(p.load(), p.hub.Done("Removed the shell command of "+row.GetName()+"."))
	}
	collision, err := profile.Collision(row.GetName())
	switch {
	case err != nil:
		return p.hub.Fail(err.Error())
	case collision != "":
		return p.hub.Fail("Cannot create alias '" + row.GetName() + "' — " + collision)
	}
	path, err := profile.MakeAlias(row.GetName(), "")
	if err != nil {
		return p.hub.Fail(err.Error())
	}
	told := row.GetName() + " now opens from the shell command at " + profile.Tilde(path) + "."
	if onPath, err := profile.AliasesOnPath(); err == nil && !onPath {
		dir, _ := profile.AliasDir()
		told += " ⚠ " + dir + " is not in your PATH."
	}
	return tea.Batch(p.load(), p.hub.Done(told))
}

// Page of what a profile's distribution is and needs
func (p *profiles) info(row *profilev1.Profile) tea.Cmd {
	return tui.Call(p.hub, func() (*profilev1.InfoResponse, error) {
		return p.service.Info(p.kit.Env.Ctx, &profilev1.InfoRequest{Name: row.GetName()})
	}, func(response *profilev1.InfoResponse) tea.Cmd {
		if !response.GetFound() {
			return p.hub.Fail("Profile '" + row.GetName() + "' is not a distribution (no distribution.yaml).")
		}
		return p.page(label(row), "distribution:"+row.GetName(), profilecli.InfoLines(response.GetManifest()))
	})
}

// Pulls a profile's distribution again, its config.yaml kept
func (p *profiles) update(row *profilev1.Profile) tea.Cmd {
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*profilev1.UpdateResponse, error) {
		return p.service.Update(p.kit.Env.Ctx, &profilev1.UpdateRequest{Name: row.GetName()})
	}, func(response *profilev1.UpdateResponse) tea.Cmd {
		m := response.GetPlan().GetManifest()
		lines := []string{"", fmt.Sprintf("✓ Updated '%s' → v%s", m.GetName(), m.GetVersion())}
		if response.GetPlan().GetHasCron() {
			lines = append(lines, fmt.Sprintf("  Cron files were refreshed.  Review with:  icebath -p %s cron list", m.GetName()))
		}
		p.hub.Quiet()
		return tea.Batch(p.load(), p.page(label(row), "update:"+row.GetName(), lines))
	})
}

// Ways to add a profile: create one, import an archive, or install a distribution
func (p *profiles) add() tea.Cmd {
	choices := []tui.Choice{
		{ID: "create", Title: "Create a profile", Detail: "a new, separate icebath with its own settings and sessions"},
		{ID: "import", Title: "Import a profile archive", Detail: "add a profile someone exported to a file"},
		{ID: "install", Title: "Install a distribution", Detail: "a ready-made profile from a git address or a folder"},
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

// Form of a new profile, kept open with what was typed when the create is refused
func (p *profiles) create() tea.Cmd {
	var sources []cards.Choice
	active := ""
	for _, row := range p.rows {
		choice := cards.Choice{Value: row.GetName(), Label: label(row)}
		if row.GetActive() {
			active = row.GetName()
			sources = append([]cards.Choice{choice}, sources...)
			continue
		}
		sources = append(sources, choice)
	}
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title: "Create a profile",
		Fields: []cards.Field{
			{ID: "name", Label: "Name", Required: true, Help: "Lowercase letters, digits and dashes, such as work or research.", Check: func(value string) string {
				if _, err := profile.Canon(strings.TrimSpace(value)); err != nil {
					return err.Error()
				}
				return ""
			}},
			{ID: "description", Label: "What it is for", Help: "One or two sentences; the board uses it to route tasks."},
			{ID: "start", Label: "Start from", Kind: cards.FieldChoice, Value: "fresh", Choices: []cards.Choice{
				{Value: "fresh", Label: "fresh", Meaning: "default settings and the bundled skills"},
				{Value: "settings", Label: "a copy of the settings", Meaning: "settings, keys and skills of the profile picked below"},
				{Value: "everything", Label: "a full copy", Meaning: "everything of the profile picked below but its history"}}},
			{ID: "from", Label: "Copy from", Kind: cards.FieldChoice, Value: active, Choices: sources, Help: "Used when it starts from a copy."},
			{ID: "logins", Label: "Logins", Kind: cards.FieldChoice, Value: "copy", Choices: []cards.Choice{
				{Value: "copy", Label: "own copy", Meaning: "copy the logins once; later sign-ins stay apart"},
				{Value: "none", Label: "none", Meaning: "no keys or logins; add them in the new profile"}}},
			{ID: "skills", Label: "Bundled skills", Kind: cards.FieldBool, Value: "true", Help: "The skills that come with icebath, kept up to date by updates."},
			{ID: "model", Label: "Model", More: true, Help: "Leave it empty to use the model of this window's profile."},
			{ID: "provider", Label: "Provider", More: true},
			{ID: "channels", Label: "Copy chat apps", Kind: cards.FieldBool, Value: "false", More: true, Help: "Bot tokens too; two profiles on one bot answer twice."},
			{ID: "alias", Label: "Shell command", Kind: cards.FieldBool, Value: "true", More: true, Help: "A command named after the profile that opens it."},
		},
		Submit: "Create",
		Cancel: func(*cards.Form) tea.Cmd { return p.hub.Close(form) },
		Done: func(f *cards.Form, v map[string]string) tea.Cmd {
			name := strings.TrimSpace(v["name"])
			request := &profilev1.CreateRequest{Name: name, Description: strings.TrimSpace(v["description"]), NoSkills: v["skills"] != "true",
				Model: strings.TrimSpace(v["model"]), Provider: strings.TrimSpace(v["provider"])}
			if (request.Model == "") != (request.Provider == "") {
				return p.hub.Fail("Model and Provider go together: a model is pinned with the provider that serves it.")
			}
			if v["start"] != "fresh" {
				request.Clone, request.From, request.All, request.Channels = true, v["from"], v["start"] == "everything", v["channels"] == "true"
				request.NoSkills = false
			} else {
				request.NoCredentials = v["logins"] == "none"
			}
			p.hub.Busy("Creating " + name + "…")
			return tui.Call(p.hub, func() (*profilev1.CreateResponse, error) {
				return p.service.Create(p.kit.Env.Ctx, request)
			}, func(made *profilev1.CreateResponse) tea.Cmd {
				canon := made.GetProfile().GetName()
				told := "Created " + canon + " at " + profile.Tilde(made.GetPath()) + "."
				if v["alias"] == "true" {
					if collision, err := profile.Collision(canon); err == nil && collision == "" {
						if _, err := profile.MakeAlias(canon, ""); err != nil {
							told += " " + err.Error()
						}
					}
				}
				if warnings := made.GetWarnings(); len(warnings) > 0 {
					told += " " + strings.Join(warnings, " ")
				}
				p.hub.Quiet()
				return tea.Batch(p.hub.Close(form), p.load(), p.hub.Done(told))
			})
		},
		Pick: func(f *cards.Form, field cards.Field) tea.Cmd {
			choices := make([]tui.Choice, len(field.Choices))
			for i, c := range field.Choices {
				choices[i] = tui.Choice{ID: c.Value, Title: c.Label, Detail: c.Meaning}
			}
			return p.kit.Pick(p.hub, field.Label, "choose", choices, field.Value, func(choice tui.Choice) tea.Cmd {
				f.SetValue(field.ID, choice.ID)
				return nil
			})
		},
	})
	return p.hub.Push(form)
}

// Imports a shared archive as a new profile
func (p *profiles) importArchive() tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title: "Import a profile archive",
		Fields: []cards.Field{
			{ID: "archive", Label: "Archive file", Required: true, Placeholder: "~/profile.tar.gz"},
			{ID: "name", Label: "Name", Help: "Leave it empty to use the name the archive carries."},
		},
		Submit: "Import",
		Done: func(_ *cards.Form, v map[string]string) tea.Cmd {
			path := fsx.ExpandHome(strings.TrimSpace(v["archive"]))
			p.hub.Busy("Importing…")
			return tui.Call(p.hub, func() (*profilev1.ImportResponse, error) {
				data, err := os.ReadFile(path)
				if errors.Is(err, fs.ErrNotExist) {
					return nil, errors.New("Archive not found: " + path)
				}
				if err != nil {
					return nil, err
				}
				return p.service.Import(p.kit.Env.Ctx, &profilev1.ImportRequest{Archive: data, Name: strings.TrimSpace(v["name"])})
			}, func(imported *profilev1.ImportResponse) tea.Cmd {
				if collision, err := profile.Collision(imported.GetName()); err == nil && collision == "" {
					profile.MakeAlias(imported.GetName(), "")
				}
				return tea.Batch(p.hub.Close(form), p.load(), p.hub.Done("Imported "+imported.GetName()+" at "+profile.Tilde(imported.GetPath())+"."))
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
		Fields: []cards.Field{
			{ID: "source", Label: "From", Required: true, Placeholder: "github.com/user/repo", Help: "A git address or a local folder holding distribution.yaml."},
			{ID: "name", Label: "Name", Help: "Leave it empty to use the name its manifest gives."},
			{ID: "alias", Label: "Shell command", Kind: cards.FieldBool, Value: "false"},
			{ID: "force", Label: "Replace a profile of that name", Kind: cards.FieldBool, Value: "false", Help: "Its own sessions, memories and keys are kept."},
		},
		Submit: "Install",
		Done: func(_ *cards.Form, v map[string]string) tea.Cmd {
			source := strings.TrimSpace(v["source"])
			if expanded := fsx.ExpandHome(source); fsx.IsDir(expanded) {
				if abs, err := filepath.Abs(expanded); err == nil {
					source = abs
				}
			}
			name, alias, force := strings.TrimSpace(v["name"]), v["alias"] == "true", v["force"] == "true"
			p.hub.Busy("Saving…")
			return tui.Call(p.hub, func() (*profilev1.PlanResponse, error) {
				return p.service.Plan(p.kit.Env.Ctx, &profilev1.PlanRequest{Source: source, Name: name})
			}, func(planned *profilev1.PlanResponse) tea.Cmd {
				p.hub.Quiet()
				return p.hub.Push(p.planPage(form, planned.GetPlan(), source, name, alias, force))
			})
		},
	})
	return p.hub.Push(form)
}

// Page of what an install would do, as the command prints it, which installs on enter
func (p *profiles) planPage(form *cards.Form, plan *profilev1.Plan, source, name string, alias, force bool) *cards.Page {
	m := plan.GetManifest()
	lines := append(profilecli.PlanLines(plan), "", "Press enter to install.")
	var page *cards.Page
	page = cards.NewPage(p.kit.Env, cards.PageSpec{
		Title:   "Install a distribution",
		Key:     "install:" + m.GetName(),
		Lines:   func(int) []string { return lines },
		Context: cards.HubContext,
		Hints:   func(*cards.Page) []wm.Hint { return []wm.Hint{{Action: "hub.use", Label: "install"}} },
		Actions: map[string]func(*cards.Page) tea.Cmd{
			"hub.use": func(*cards.Page) tea.Cmd {
				p.hub.Busy("Saving…")
				return tui.Call(p.hub, func() (*profilev1.InstallResponse, error) {
					return p.service.Install(p.kit.Env.Ctx, &profilev1.InstallRequest{Source: source, Name: name, Force: force})
				}, func(installed *profilev1.InstallResponse) tea.Cmd {
					done := installed.GetPlan()
					made := done.GetManifest()
					result := []string{"", fmt.Sprintf("✓ Installed '%s' v%s", made.GetName(), made.GetVersion()), "  Profile path: " + done.GetTarget()}
					if alias {
						if collision, err := profile.Collision(made.GetName()); err == nil && collision == "" {
							if _, err := profile.MakeAlias(made.GetName(), ""); err != nil {
								result = append(result, "⚠ "+err.Error())
							}
						}
					}
					if len(made.GetEnv()) > 0 {
						result = append(result, "  Next: copy .env.EXAMPLE to .env and fill in required keys:", "    "+done.GetTarget()+"/.env.EXAMPLE")
					}
					if done.GetHasCron() {
						result = append(result, "  Cron jobs were included but are NOT scheduled automatically.",
							"  Review them with:  icebath -p "+made.GetName()+" cron list")
					}
					result = append(result, "", "  Use with:      icebath -p "+made.GetName()+" chat")
					p.hub.Quiet()
					return tea.Batch(p.hub.Close(page), p.hub.Close(form), p.load(), p.page("Install a distribution", "installed:"+made.GetName(), result))
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
	const usage = "Usage: /export [profile] [-o output.tar.gz]"
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
				return kit.WM.Toast("Error: "+tui.Reason(err), wm.Error, wm.DefaultToastTTL)
			}
			return kit.WM.Toast(fmt.Sprintf("✓ Exported '%s' to %s\nShare it: the other user runs `icebath profile import <archive>`.", exported, path),
				wm.Success, wm.DefaultToastTTL)
		}
	})
}
