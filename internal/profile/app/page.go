package app

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/profile"
	profileconfig "github.com/nickheyer/icebath/internal/profile/config"
	profilev1 "github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// What the description of a profile is for, under its field
const describeHelp = "One or two sentences on what the profile is for, such as Work on the company's website.\n" +
	"icebath reads it to pick a profile for a task."

// Rows of a profile's page
const (
	rowSwitch   = "switch"
	rowDescribe = "describe"
	rowWrite    = "write"
	rowRename   = "rename"
	rowExport   = "export"
	rowShell    = "shell"
	rowBundled  = "bundled"
	rowSync     = "sync"
	rowDetails  = "details"
	rowDist     = "distribution"
	rowUpdate   = "update"
	rowDelete   = "delete"
)

// Rows of the page of the profile as it is now: switching to it, what it is for, renaming and exporting it, its switches, its details, its
// distribution and deleting it
func pageRows(row *profilev1.Profile) []string {
	var rows []string
	if !row.GetActive() || !row.GetSticky() {
		rows = append(rows, rowSwitch)
	}
	rows = append(rows, rowDescribe, rowWrite, rowRename, rowExport, rowShell, rowBundled, rowSync, rowDetails)
	if row.GetDistribution() != "" {
		rows = append(rows, rowDist, rowUpdate)
	}
	if !row.GetDefault() && !row.GetActive() {
		rows = append(rows, rowDelete)
	}
	return rows
}

// What the profile is, as the first line of its page
func standing(row *profilev1.Profile) string {
	switch name := row.GetName(); {
	case row.GetActive() && row.GetSticky():
		return "This window runs on " + name + ", and icebath opens with it."
	case row.GetActive():
		return "This window runs on " + name + "."
	case row.GetSticky():
		return "icebath opens with " + name + "."
	}
	return "Not in use."
}

// Opens the page of one profile over the hub
func (p *profiles) page(name string) tea.Cmd {
	return p.hub.Push(p.pageList(name))
}

// The page of one profile, read again with the profiles
func (p *profiles) pageList(name string) *cards.List {
	t := p.kit.Env.Theme
	var shown []string
	now := func() *profilev1.Profile { return p.find(name) }
	title := name
	if row := now(); row != nil {
		title = label(row)
	}
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Profiles,
		Key:     "profiles:" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			if row := now(); row != nil {
				return []string{standing(row)}
			}
			return nil
		},
		Columns: []render.Column{{}, {}, {Flex: 1}},
		Rows: func() [][]string {
			shown = nil
			row := now()
			if row == nil {
				return nil
			}
			shown = pageRows(row)
			cells := make([][]string, len(shown))
			for i, kind := range shown {
				cells[i] = p.pageCells(row, kind, t)
			}
			return cells
		},
		Group: func(i int) string {
			switch shown[i] {
			case rowDist, rowUpdate:
				return "Distribution"
			}
			return ""
		},
		Detail: func(i int) []string {
			if row := now(); row != nil {
				return p.pageDetail(row, shown[i])
			}
			return nil
		},
		Empty: func() string { return name + " is no longer there." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if row := now(); row != nil && i >= 0 && i < len(shown) {
				return pageHints(row, shown[i])
			}
			return nil
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if row := now(); row != nil {
					return p.act(l, row, shown[i])
				}
				return nil
			},
		},
	})
	list.ToggleDetail()
	p.views.Add(list)
	return list
}

func (p *profiles) pageCells(row *profilev1.Profile, kind string, t theme.Theme) []string {
	opens := render.Dim(t.Glyphs.Forward, t)
	switch kind {
	case rowSwitch:
		return []string{"Switch to it", "", ""}
	case rowDescribe:
		description := strings.ReplaceAll(strings.TrimSpace(row.GetDescription()), "\n", " ")
		if description == "" {
			description = render.Dim("None", t)
		}
		return []string{"Description", "", description}
	case rowWrite:
		return []string{"Write one for me", "", ""}
	case rowRename:
		return []string{"Rename", "", ""}
	case rowExport:
		return []string{"Export to a file", opens, ""}
	case rowShell:
		alias, _ := profile.AliasOf(row.GetName())
		return []string{"Shell command", "", strs.OnOff(alias != "")}
	case rowBundled:
		return []string{"Bundled skills", "", strs.OnOff(row.GetBundled())}
	case rowSync:
		return []string{"Keep syncing imports", "", strs.OnOff(row.GetSyncImports())}
	case rowDetails:
		return []string{"Details", "", render.Dim(profile.Tilde(row.GetPath()), t)}
	case rowDist:
		return []string{"Distribution details", opens, render.Dim(row.GetDistribution()+version(row.GetDistributionVersion()), t)}
	case rowUpdate:
		return []string{"Update", "", ""}
	}
	return []string{"Delete", "", ""}
}

// Lines under the rows of a profile's page about the row in hand
func (p *profiles) pageDetail(row *profilev1.Profile, kind string) []string {
	t := p.kit.Env.Theme
	name := row.GetName()
	switch kind {
	case rowSwitch:
		return []string{"This window runs on " + name + " from now on, and icebath opens with it.", render.Dim("Also /profiles "+name, t)}
	case rowDescribe:
		lines := []string{"What the profile is for, which icebath reads to pick a profile for a task."}
		if row.GetDescription() != "" && row.GetDescriptionAuto() {
			lines = append(lines, render.Dim("The helper model wrote it.", t))
		}
		return lines
	case rowWrite:
		return []string{"The helper model reads the skills and settings of " + name + " and writes what it is for."}
	case rowRename:
		if row.GetDefault() {
			return []string{"Give the default profile a name to show, or take it away. Its folder keeps the name default."}
		}
		return []string{"Its sessions and shell command follow the new name."}
	case rowExport:
		return []string{"Save its settings, skills and look to a file you can share. Keys and sessions stay out.", render.Dim("Also /export "+name, t)}
	case rowShell:
		return []string{"A command named " + name + " that opens icebath on this profile."}
	case rowBundled:
		return []string{"Give it every skill that comes with icebath, kept up to date, rather than only the essential ones.",
			render.Dim("Takes effect after icebath restarts", t)}
	case rowSync:
		return []string{"Keep pulling what the profile in use imports from other agents, such as Claude Code and Codex."}
	case rowDetails:
		return p.facts(row)
	case rowDist:
		return []string{"Its version, what it needs and where it came from."}
	case rowUpdate:
		return []string{"Get " + row.GetDistribution() + " again from where it came from. Your sessions, memories and keys stay, and your settings " +
			"too unless you take its own."}
	}
	return []string{"Delete " + name + " with its settings, keys, sessions, skills and files."}
}

// Lines of the details of a profile: where it is kept, its model and skills, its files, its distribution and its shell command
func (p *profiles) facts(row *profilev1.Profile) []string {
	there := func(yes bool) string {
		if yes {
			return "there"
		}
		return "not made yet"
	}
	lines := []string{"Folder: " + profile.Tilde(row.GetPath())}
	if model := row.GetModel(); model != "" {
		if row.GetProvider() != "" {
			model += " · " + row.GetProvider()
		}
		lines = append(lines, "Model: "+model)
	}
	lines = append(lines, "Skills: "+strconv.Itoa(int(row.GetSkills())), "Keys file: "+there(row.GetHasEnv()), "Personality file SOUL.md: "+there(row.GetHasSoul()))
	if row.GetDistribution() != "" {
		lines = append(lines, "Distribution: "+row.GetDistribution()+version(row.GetDistributionVersion()))
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
	return lines
}

func pageHints(row *profilev1.Profile, kind string) []wm.Hint {
	switch kind {
	case rowSwitch:
		return []wm.Hint{{Action: "hub.use", Label: "switch"}}
	case rowDescribe:
		return []wm.Hint{{Action: "hub.use", Label: "change"}}
	case rowWrite:
		return []wm.Hint{{Action: "hub.use", Label: "write"}}
	case rowRename:
		return []wm.Hint{{Action: "hub.use", Label: "rename"}}
	case rowExport, rowDist:
		return []wm.Hint{{Action: "hub.use", Label: "open"}}
	case rowShell:
		alias, _ := profile.AliasOf(row.GetName())
		return []wm.Hint{{Action: "hub.use", Label: turn(alias != "")}}
	case rowBundled:
		return []wm.Hint{{Action: "hub.use", Label: turn(row.GetBundled())}}
	case rowSync:
		return []wm.Hint{{Action: "hub.use", Label: turn(row.GetSyncImports())}}
	case rowDetails:
		return nil
	case rowUpdate:
		return []wm.Hint{{Action: "hub.use", Label: "update"}}
	}
	return []wm.Hint{{Action: "hub.use", Label: "delete"}}
}

// What enter does on a switch that is on or off
func turn(on bool) string {
	if on {
		return "turn off"
	}
	return "turn on"
}

// What enter does on a row of a profile's page
func (p *profiles) act(l *cards.List, row *profilev1.Profile, kind string) tea.Cmd {
	switch kind {
	case rowSwitch:
		return p.use(row.GetName())
	case rowDescribe:
		return p.describe(row)
	case rowWrite:
		return p.describeAuto(row)
	case rowRename:
		return p.rename(row, l)
	case rowExport:
		return p.export(row)
	case rowShell:
		return p.shell(row)
	case rowBundled:
		return p.bundled(row)
	case rowSync:
		return p.syncImports(row)
	case rowDist:
		return p.info(row)
	case rowUpdate:
		return p.askUpdate(row)
	case rowDelete:
		return p.remove(l, row, l)
	}
	return nil
}

// Asks for a profile's description, saved at once; left empty, the description is taken away
func (p *profiles) describe(row *profilev1.Profile) tea.Cmd {
	field := cards.Field{Label: "Description", Value: row.GetDescription(), Help: describeHelp + "\nLeft empty, the profile has none.", Select: true}
	return p.kit.AskAny(p.hub, "Describe "+row.GetName(), field, "Save", func(text string) tea.Cmd {
		if text == "" && row.GetDescription() == "" {
			return nil
		}
		p.hub.Busy("Saving…")
		return tui.Call(p.hub, func() (*profilev1.DescribeResponse, error) {
			// A description replaced by no text is taken away
			return p.service.Describe(p.kit.Env.Ctx, &profilev1.DescribeRequest{Name: row.GetName(), Text: text, Overwrite: text == ""})
		}, func(*profilev1.DescribeResponse) tea.Cmd {
			told := "Saved the description of " + row.GetName() + "."
			if text == "" {
				told = row.GetName() + " has no description now."
			}
			return tea.Batch(p.load(nil), p.hub.Done(told))
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
				return p.hub.Fail("No description was written. " + strs.Full(outcome.GetReason()))
			}
		}
		return tea.Batch(p.load(nil), p.hub.Done("The helper model described "+row.GetName()+"."))
	})
}

// Renames a profile, the one this window runs on included, or sets the name the default profile shows; its shell command follows. What
// failed is said, and what did not move yet is moved again by a second rename to the same name, from the question that offers it
func (p *profiles) rename(row *profilev1.Profile, page *cards.List) tea.Cmd {
	field := cards.Field{Label: "Profile name", Value: row.GetName(), Select: true, Help: "Lower case letters, digits and dashes, such as work or research.",
		Check: func(value string) string {
			if _, err := profile.Canon(strings.TrimSpace(value)); err != nil {
				return tui.Reason(err)
			}
			return ""
		}}
	ask := p.kit.Ask
	if row.GetDefault() {
		// The default profile only takes a name to show, and gives it up for an empty one
		field = cards.Field{Label: "Name shown", Value: row.GetDisplayName(), Select: true,
			Help: "The name the default profile is shown by, such as Home. Its folder keeps its name. Left empty, it is shown as default again."}
		ask = p.kit.AskAny
	}
	return ask(p.hub, "Rename "+label(row), field, "Rename", func(name string) tea.Cmd {
		if name == row.GetName() && !row.GetDefault() || row.GetDefault() && name == row.GetDisplayName() {
			return nil
		}
		p.hub.Busy("Renaming " + row.GetName() + "…")
		return tui.Call(p.hub, func() (*profilev1.RenameResponse, error) {
			return p.service.Rename(p.kit.Env.Ctx, &profilev1.RenameRequest{Old: row.GetName(), New: name})
		}, func(done *profilev1.RenameResponse) tea.Cmd {
			p.hub.Quiet()
			renamed := done.GetName()
			switch {
			case row.GetDefault() && renamed == profile.Default:
				return tea.Batch(p.load(nil), p.hub.Done("The default profile is shown as default again."))
			case row.GetDefault():
				return tea.Batch(p.load(nil), p.hub.Done("The default profile is shown as "+renamed+" now."))
			}
			_, broken := profile.FollowAlias(row.GetName(), renamed)
			failed := append(done.GetFailed(), broken)
			cmds := []tea.Cmd{p.load(func() tea.Cmd {
				p.point(renamed)
				return nil
			})}
			// The page of the profile goes on under its new name
			asked := p.list
			if page != nil {
				asked = p.pageList(renamed)
				cmds = append(cmds, p.hub.Close(page), p.hub.Push(asked))
			}
			// Screens that follow the profile in use hear of its new name
			if row.GetActive() {
				cmds = append(cmds, p.kit.Changed(profileconfig.Active, renamed))
			}
			told := row.GetName() + " is called " + renamed + " now."
			switch {
			case !done.GetSettled():
				cmds = append(cmds, p.unsettled(asked, told, failed, func() ([]string, bool, error) {
					moved, err := p.service.MigrateIdentity(p.kit.Env.Ctx, &profilev1.MigrateIdentityRequest{Old: row.GetName(), New: renamed})
					return moved.GetFailed(), moved.GetSettled(), err
				}, "Everything "+renamed+" keeps moved to its new name."))
			case joined(failed...) != "":
				cmds = append(cmds, p.hub.Fail(joined(told, joined(failed...))))
			default:
				cmds = append(cmds, p.hub.Done(told+" Its sessions and shell command follow."))
			}
			return tea.Batch(cmds...)
		})
	})
}

// Asks on the list whether to finish what an act left undone, saying what failed: the act is the try again, which asks again while
// something is left
func (p *profiles) unsettled(on *cards.List, told string, failed []string, again func() ([]string, bool, error), finished string) tea.Cmd {
	on.Confirm(cards.Question{Text: joined(told, joined(failed...), "Try once more to finish it?"), Verb: "Try again"}, func() tea.Cmd {
		p.hub.Busy("Trying again…")
		return tui.Call(p.hub, func() ([]string, error) {
			left, settled, err := again()
			if err == nil && settled {
				return nil, nil
			}
			return left, err
		}, func(left []string) tea.Cmd {
			if left == nil {
				return tea.Batch(p.load(nil), p.hub.Done(finished))
			}
			return p.unsettled(on, "Not everything is finished yet.", left, again, finished)
		})
	})
	return nil
}

// Asks before a profile is deleted, saying what goes with it; what failed is said, and what was left behind is removed again from the
// question that offers it. The page of the profile closes with it
func (p *profiles) remove(l *cards.List, row *profilev1.Profile, page *cards.List) tea.Cmd {
	switch {
	case row.GetDefault():
		return p.hub.Say("The default profile can't be deleted.", cards.NoteInfo)
	case row.GetActive():
		return p.hub.Say("This window runs on "+row.GetName()+", so it can't be deleted. Switch to another profile first.", cards.NoteInfo)
	}
	name := row.GetName()
	what := "Its settings, keys, sessions, skills and files go with it"
	if alias, _ := profile.AliasOf(name); alias != "" {
		what += ", and so does the shell command " + alias
	}
	l.Confirm(cards.Question{Text: "Delete " + name + " for good? " + what + ".", Verb: "Delete", Deletes: true}, func() tea.Cmd {
		p.hub.Busy("Deleting " + name + "…")
		return tui.Call(p.hub, func() (*profilev1.DeleteResponse, error) {
			return p.service.Delete(p.kit.Env.Ctx, &profilev1.DeleteRequest{Name: name})
		}, func(done *profilev1.DeleteResponse) tea.Cmd {
			p.hub.Quiet()
			_, kept := profile.DropAlias(name)
			failed := append(done.GetFailed(), kept)
			cmds := []tea.Cmd{p.load(nil)}
			if page != nil {
				cmds = append(cmds, p.hub.Close(page))
			}
			told := "Deleted " + name + "."
			// A folder that would not go is said whatever else failed, with what to do about it
			if failure := done.GetFailure(); failure != "" {
				failed = append(failed, failure, "Remove "+profile.Tilde(done.GetPath())+" yourself.")
			}
			switch {
			case !done.GetSettled():
				cmds = append(cmds, p.unsettled(p.list, told, failed, func() ([]string, bool, error) {
					purged, err := p.service.PurgeIdentity(p.kit.Env.Ctx, &profilev1.PurgeIdentityRequest{Name: name})
					return purged.GetFailed(), purged.GetSettled(), err
				}, "Everything "+name+" kept elsewhere is removed."))
			case joined(failed...) != "":
				cmds = append(cmds, p.hub.Fail(joined(told, joined(failed...))))
			default:
				cmds = append(cmds, p.hub.Done(told))
			}
			return tea.Batch(cmds...)
		})
	})
	return nil
}

// Makes the shell command that opens a profile, or removes it
func (p *profiles) shell(row *profilev1.Profile) tea.Cmd {
	if alias, _ := profile.AliasOf(row.GetName()); alias != "" {
		if _, err := profile.RemoveAlias(alias); err != nil {
			return p.hub.Fail("The shell command " + alias + " could not be removed. " + strs.Full(tui.Reason(err)))
		}
		return tea.Batch(p.load(nil), p.hub.Done("Shell command is now Off. "+alias+" no longer opens icebath."))
	}
	done, failed := profile.Command(row.GetName())
	if failed != "" {
		return p.hub.Fail(failed)
	}
	return tea.Batch(p.load(nil), p.hub.Done(done))
}

// Gives a profile every bundled skill, or only the essential ones
func (p *profiles) bundled(row *profilev1.Profile) tea.Cmd {
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*profilev1.BundledResponse, error) {
		return p.service.Bundled(p.kit.Env.Ctx, &profilev1.BundledRequest{Name: row.GetName(), On: !row.GetBundled()})
	}, func(response *profilev1.BundledResponse) tea.Cmd {
		return tea.Batch(p.load(nil), p.hub.Done(response.GetNote()))
	})
}

// Has a profile keep pulling what the profile in use imports from other agents, or stop
func (p *profiles) syncImports(row *profilev1.Profile) tea.Cmd {
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*profilev1.SyncImportsResponse, error) {
		return p.service.SyncImports(p.kit.Env.Ctx, &profilev1.SyncImportsRequest{Name: row.GetName(), On: !row.GetSyncImports()})
	}, func(response *profilev1.SyncImportsResponse) tea.Cmd {
		return tea.Batch(p.load(nil), p.hub.Done(response.GetNote()))
	})
}

// Page of lines on the hub, wrapped to its width
func (p *profiles) lines(title, key string, lines []string) tea.Cmd {
	return p.hub.Push(cards.NewPage(p.kit.Env, cards.PageSpec{Title: title, Key: key, Lines: func(w int) []string {
		var out []string
		for _, line := range lines {
			out = append(out, strings.Split(render.Wrap(line, max(w, 8)), "\n")...)
		}
		return out
	}}))
}

// Page of what a profile's distribution is and needs
func (p *profiles) info(row *profilev1.Profile) tea.Cmd {
	return tui.Call(p.hub, func() (*profilev1.InfoResponse, error) {
		return p.service.Info(p.kit.Env.Ctx, &profilev1.InfoRequest{Name: row.GetName()})
	}, func(response *profilev1.InfoResponse) tea.Cmd {
		if !response.GetFound() {
			return p.hub.Fail(row.GetName() + " has no distribution.yaml, so it was not installed from a distribution.")
		}
		return p.lines("Distribution details", "distribution:"+row.GetName(), manifestLines(response.GetManifest(), p.kit.Env.Theme))
	})
}

// Asks how a profile is updated from its distribution, the last question before it is: its settings left as they are, as an update leaves
// them, or replaced with the distribution's
func (p *profiles) askUpdate(row *profilev1.Profile) tea.Cmd {
	const keep, replace = "keep", "replace"
	choices := []tui.Choice{
		{ID: keep, Title: "Keep my settings", Detail: "config.yaml stays as it is"},
		{ID: replace, Title: "Take its settings too", Detail: "config.yaml is replaced with the distribution's"},
	}
	intro := []string{"Get " + row.GetDistribution() + " again from where it came from. Its own files are replaced, and your sessions, memories and keys stay."}
	return p.kit.PickFrom(p.hub, "Update "+row.GetName(), intro, "update", choices, keep, func(choice tui.Choice) tea.Cmd {
		return p.update(row, choice.ID == replace)
	})
}

// Pulls a profile's distribution again, its config.yaml kept unless the distribution's is to replace it
func (p *profiles) update(row *profilev1.Profile, settings bool) tea.Cmd {
	p.hub.Busy("Updating " + row.GetName() + " from its distribution…")
	return tui.Call(p.hub, func() (*profilev1.UpdateResponse, error) {
		return p.service.Update(p.kit.Env.Ctx, &profilev1.UpdateRequest{Name: row.GetName(), ForceConfig: settings})
	}, func(response *profilev1.UpdateResponse) tea.Cmd {
		m := response.GetPlan().GetManifest()
		told := "Updated " + m.GetName() + version(m.GetVersion()) + "."
		if response.GetPlan().GetPreservesConfig() {
			told += " Your settings in config.yaml stay as they were."
		} else {
			told += " Its settings replaced yours in config.yaml."
		}
		if response.GetPlan().GetHasCron() {
			told += " Its scheduled jobs in cron/jobs.json were updated too, and this build of icebath does not run them."
		}
		return tea.Batch(p.load(nil), p.hub.Done(told))
	})
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
	// What it owns is what an update or an install over the profile replaces
	for _, fact := range [][2]string{{"Made by", m.GetAuthor()}, {"License", m.GetLicense()}, {"Needs icebath", m.GetRequires()}, {"From", m.GetSource()},
		{"Installed", m.GetInstalledAt()}, {"Its own files", strings.Join(m.GetOwned(), ", ")}} {
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
