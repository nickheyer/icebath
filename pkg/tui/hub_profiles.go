package tui

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the profiles screen
const profilesContext = "tui.profiles"

// Waits of the slow profile requests: a create that clones a profile, and a description the auxiliary model writes
const (
	profilesCreateWait   = 3 * time.Minute
	profilesDescribeWait = 2 * time.Minute
)

// Why switching profile waits until icebath closes, as the command tree words it
const profilesSwitchReason = "This window stays on its profile until icebath opens again."

// Why renaming or deleting the profile this window runs on waits until icebath closes, as the command tree words it
const profilesMoveReason = "It can move or remove the profile this window runs on."

// Keys of the profiles screen beyond the shared hub keys
func registerProfilesKeys(km *keys.Map) {
	if keys.ContextDocs[profilesContext] == "" {
		if err := keys.AddContext(profilesContext, "profiles screen"); err != nil {
			panic(err)
		}
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "profiles", order: 85, title: txt.HubsTitleProfiles, short: txt.HubsShortProfiles, slash: "profiles",
		open: func(a *app, _ string) tea.Cmd { return a.openProfiles() }, keys: registerProfilesKeys})
}

// State of the open profiles screen: the rows as last read and the hub they show in
type profilesHub struct {
	a    *app
	h    *cards.Hub
	list *cards.List
	rows []tuigateway.ProfileOverviewRow
	// Profile icebath opens with when none is named
	active  string
	loading bool
}

// Profiles screen: every profile with its model, skills and what it is for, and the profile this window runs on marked
func (a *app) openProfiles() tea.Cmd {
	p := &profilesHub{a: a, loading: true}
	p.list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleProfiles), Glyph: a.toolkit.Glyphs.Profiles, Key: "tui:profiles", Context: profilesContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: ""}, {Title: tr(txt.HubsProfilesColProfile), Max: 24}, {Title: tr(txt.HubsProfilesColModel), Max: 28}, {Title: tr(txt.HubsProfilesColSkills), Right: true},
			{Title: tr(txt.HubsProfilesColFor), Flex: 1}},
		Yield:  []int{3, 2},
		Rows:   p.cells,
		Detail: p.detail,
		Status: func() string {
			if len(p.rows) == 0 {
				return ""
			}
			return tr(txt.HubsProfilesLegend)
		},
		Empty: func() string {
			if p.loading {
				return tr(txt.HubsProfilesLoading)
			}
			return tr(txt.HubsProfilesEmpty)
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			out := []wm.Hint{}
			if i >= 0 && i < len(p.rows) {
				out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, wm.Hint{Action: "hub.default", Label: tr(txt.HubsProfilesUseLabel)},
					wm.Hint{Action: "hub.edit", Label: tr(txt.HubsProfilesDescribeLabel)}, wm.Hint{Action: "hub.rename"})
				if !p.rows[i].IsDefault {
					out = append(out, wm.Hint{Action: "hub.remove"})
				}
			}
			return append(out, wm.Hint{Action: "hub.add"})
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load() },
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return p.open(i) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return p.add() },
			"hub.default": func(l *cards.List, i int) tea.Cmd { return p.use(l, i) },
			"hub.edit":    func(_ *cards.List, i int) tea.Cmd { return p.describe(i) },
			"hub.rename":  func(_ *cards.List, i int) tea.Cmd { return p.rename(i) },
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return p.remove(l, i) },
		},
		OnFocus: func(*cards.List) tea.Cmd { return p.load() },
	})
	p.h = a.newHub("tui:profiles", p.list)
	return a.wm.ShowOverlay(p.h)
}

// Reads the profiles again, a failure staying in the hub
func (p *profilesHub) load() tea.Cmd {
	return hubCall(p.a, p.h, "", tuigateway.ProfilesOverview, tuigateway.ProfilesOverviewParams{}, func(r *tuigateway.ProfilesOverviewResult) tea.Cmd {
		p.loading, p.rows, p.active = false, r.Profiles, r.Active
		p.list.Refresh()
		return nil
	})
}

// Whether the row is the profile this window runs on
func (p *profilesHub) mine(row tuigateway.ProfileOverviewRow) bool {
	return filepath.Clean(row.Path) == filepath.Clean(home.Dir(p.a.ctx))
}

// Row at i, false past the rows
func (p *profilesHub) at(i int) (tuigateway.ProfileOverviewRow, bool) {
	if i < 0 || i >= len(p.rows) {
		return tuigateway.ProfileOverviewRow{}, false
	}
	return p.rows[i], true
}

// Name a profile shows under, its display name beside the id when it has one
func profileLabel(row tuigateway.ProfileOverviewRow) string {
	if row.DisplayName != "" && row.DisplayName != row.Name {
		return row.Name + " (" + row.DisplayName + ")"
	}
	return row.Name
}

// Cells of every profile: this window and the one icebath opens with marked
func (p *profilesHub) cells() [][]string {
	out := make([][]string, len(p.rows))
	for i, row := range p.rows {
		mark := ""
		if p.mine(row) {
			mark += "●"
		}
		if row.Active {
			mark += "★"
		}
		model := row.Model
		if model == "" {
			model = "—"
		}
		out[i] = []string{mark, profileLabel(row), model, strconv.FormatInt(row.SkillCount, 10), strings.ReplaceAll(row.Description, "\n", " ")}
	}
	return out
}

// Lines of a profile's detail pane: where it lives, how it opens, what it was called and installed from
func (p *profilesHub) detail(i int) []string {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	out := []string{profileLabel(row) + " · " + home.Display(row.Path)}
	if row.Description != "" {
		desc := row.Description
		if row.DescriptionAuto {
			desc += " " + tr(txt.HubsProfilesDescriptionAuto)
		}
		out = append(out, desc)
	}
	if row.Model != "" {
		model := row.Model
		if row.Provider != "" {
			model += " · " + row.Provider
		}
		out = append(out, tr(txt.HubsProfilesDetailModel, model))
	}
	switch {
	case p.mine(row) && row.Active:
		out = append(out, tr(txt.HubsProfilesDetailMineActive))
	case p.mine(row):
		out = append(out, tr(txt.HubsProfilesDetailMine))
	case row.Active:
		out = append(out, tr(txt.HubsProfilesDetailActive))
	}
	if row.Alias != "" {
		out = append(out, tr(txt.HubsProfilesDetailAlias, row.Alias))
	}
	if row.Distribution != "" {
		out = append(out, tr(txt.HubsProfilesDetailDistribution, row.Distribution, row.DistributionSource))
	}
	if len(row.PreviousNames) > 0 {
		out = append(out, tr(txt.HubsProfilesDetailWas, strings.Join(row.PreviousNames, ", ")))
	}
	if row.ServiceRunning {
		out = append(out, tr(txt.HubsProfilesDetailService))
	}
	return out
}

// Actions screen of a profile
func (p *profilesHub) open(i int) tea.Cmd {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	return p.a.hubActions(p.h, profileLabel(row), func() []string { return p.detail(i) }, func() []hubAction {
		var acts []hubAction
		if !p.mine(row) || !row.Active {
			acts = append(acts, hubAction{key: "hub.default", label: tr(txt.HubsProfilesUse), does: tr(txt.HubsProfilesUseDoes), run: func() tea.Cmd { return p.use(p.list, i) }})
		}
		acts = append(acts,
			hubAction{key: "hub.edit", label: tr(txt.HubsProfilesDescribe), does: tr(txt.HubsProfilesDescribeDoes), run: func() tea.Cmd { return p.describe(i) }},
			hubAction{label: tr(txt.HubsProfilesDescribeAuto), does: tr(txt.HubsProfilesDescribeAutoDoes), run: func() tea.Cmd { return p.describeAuto(row) }},
			hubAction{key: "hub.rename", label: tr(txt.HubsProfilesRename), does: tr(txt.HubsProfilesRenameDoes), run: func() tea.Cmd { return p.rename(i) }},
			hubAction{label: tr(txt.HubsProfilesExport), does: tr(txt.HubsProfilesExportDoes), run: func() tea.Cmd { return p.export(row) }},
			hubAction{label: tr(txt.HubsProfilesShow), does: tr(txt.HubsProfilesShowDoes), run: func() tea.Cmd {
				return p.a.hubRunCLI(p.h, profileLabel(row), []string{"profile", "show", row.Name}, func(*cliRun) tea.Cmd { return nil })
			}})
		if row.Alias == "" {
			acts = append(acts, hubAction{label: tr(txt.HubsProfilesAliasAdd), does: tr(txt.HubsProfilesAliasAddDoes, row.Name), run: func() tea.Cmd { return p.alias(row, false) }})
		} else {
			acts = append(acts, hubAction{label: tr(txt.HubsProfilesAliasRemove), does: tr(txt.HubsProfilesAliasRemoveDoes, row.Alias), confirm: tr(txt.HubsProfilesAliasRemoveConfirm, row.Alias),
				run: func() tea.Cmd { return p.alias(row, true) }})
		}
		if row.Distribution != "" {
			acts = append(acts,
				hubAction{label: tr(txt.HubsProfilesDistInfo), does: tr(txt.HubsProfilesDistInfoDoes), run: func() tea.Cmd {
					return p.a.hubRunCLI(p.h, profileLabel(row), []string{"profile", "info", row.Name}, func(*cliRun) tea.Cmd { return nil })
				}},
				hubAction{label: tr(txt.HubsProfilesDistUpdate), does: tr(txt.HubsProfilesDistUpdateDoes), confirm: tr(txt.HubsProfilesDistUpdateConfirm, row.Name), run: func() tea.Cmd {
					return p.a.hubRunCLI(p.h, profileLabel(row), []string{"profile", "update", row.Name, "--yes"}, func(*cliRun) tea.Cmd { return p.load() })
				}})
		}
		if !row.IsDefault {
			acts = append(acts, hubAction{key: "hub.remove", label: tr(txt.HubsProfilesDelete), does: tr(txt.HubsProfilesDeleteDoes), run: func() tea.Cmd { return p.remove(p.list, i) }})
		}
		return acts
	})
}

// Switches to a profile: icebath opens on it from now on, this window following once it reopens after the queue runs
func (p *profilesHub) use(l *cards.List, i int) tea.Cmd {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	if p.mine(row) && row.Active {
		return p.h.Done(tr(txt.HubsProfilesAlreadyOn, row.Name))
	}
	queued := p.a.queueAfterExitCmd([]string{"profile", "use", row.Name}, profilesSwitchReason)
	cmd := p.h.PopTo(p.list)
	l.Confirm(tr(txt.HubsProfilesQuitNow, row.Name), func() tea.Cmd { return p.a.die(0) })
	return tea.Batch(cmd, queued)
}

// Asks for a profile's description, saved at once
func (p *profilesHub) describe(i int) tea.Cmd {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	return p.a.hubAsk(p.h, tr(txt.HubsProfilesDescribe), tr(txt.HubsProfilesDescribeField, row.Name), row.Description, false, func(text string) tea.Cmd {
		params := tuigateway.ProfilesDescribeParams{Name: row.Name, Text: text}
		return hubCall(p.a, p.h, tr(txt.HubsProfilesSaving), tuigateway.ProfilesDescribe, params, func(*tuigateway.ProfilesDescribeResult) tea.Cmd {
			return tea.Batch(p.load(), p.h.Done(tr(txt.HubsProfilesDescribed, row.Name)))
		})
	})
}

// Has the auxiliary model write a profile's description from its skills and settings
func (p *profilesHub) describeAuto(row tuigateway.ProfileOverviewRow) tea.Cmd {
	params := tuigateway.ProfilesDescribeParams{Name: row.Name, Auto: true, Overwrite: true}
	return callWithin(p.a.gw, tuigateway.ProfilesDescribe, params, profilesDescribeWait, func(r *tuigateway.ProfilesDescribeResult, err error) tea.Cmd {
		if err != nil {
			return p.h.Fail(rpcErrorMessage(err))
		}
		if r.Reason != "described" {
			return p.h.Fail(tr(txt.HubsProfilesDescribeAutoFailed, r.Reason))
		}
		return tea.Batch(p.load(), p.h.Done(tr(txt.HubsProfilesDescribed, row.Name)))
	})
}

// Renames a profile: another profile at once through the profile command, the one this window runs on once icebath closes
func (p *profilesHub) rename(i int) tea.Cmd {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	label := tr(txt.HubsProfilesRenameField)
	if row.IsDefault {
		label = tr(txt.HubsProfilesRenameDefaultField)
	}
	return p.a.hubAsk(p.h, tr(txt.HubsProfilesRename), label, row.Name, false, func(name string) tea.Cmd {
		if name == "" || name == row.Name {
			return nil
		}
		argv := []string{"profile", "rename", row.Name, name}
		if p.mine(row) && !row.IsDefault {
			return tea.Batch(p.h.PopTo(p.list), p.a.queueAfterExitCmd(argv, profilesMoveReason))
		}
		return p.a.hubRunCLI(p.h, tr(txt.HubsProfilesRename), argv, func(run *cliRun) tea.Cmd { return p.load() })
	})
}

// Deletes a profile after a confirmation, refusing the one this window runs on
func (p *profilesHub) remove(l *cards.List, i int) tea.Cmd {
	row, ok := p.at(i)
	if !ok || row.IsDefault {
		return nil
	}
	if p.mine(row) {
		return p.h.Fail(tr(txt.HubsProfilesDeleteMine, row.Name))
	}
	ask := func() tea.Cmd {
		l.Confirm(tr(txt.HubsProfilesDeleteConfirm, row.Name), func() tea.Cmd {
			return p.a.hubRunCLI(p.h, tr(txt.HubsProfilesDelete), []string{"profile", "delete", row.Name, "--yes"}, func(*cliRun) tea.Cmd { return p.load() })
		})
		return nil
	}
	return tea.Batch(p.h.PopTo(p.list), ask())
}

// Writes a profile to a shareable archive, at the path asked for or the managed export folder
func (p *profilesHub) export(row tuigateway.ProfileOverviewRow) tea.Cmd {
	spec := cards.FormSpec{Title: tr(txt.HubsProfilesExport), Submit: tr(txt.HubsProfilesExportSubmit), Intro: []string{tr(txt.HubsProfilesExportIntro, row.Name)},
		Fields: []cards.Field{{ID: "output", Label: tr(txt.HubsProfilesExportPath), Placeholder: tr(txt.HubsProfilesExportPathDefault), Help: tr(txt.HubsProfilesExportPathHelp)}}}
	return p.a.hubForm(p.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		params := tuigateway.ProfilesExportParams{Name: row.Name, Output: absolutePath(strings.TrimSpace(values["output"]))}
		return hubCall(p.a, p.h, tr(txt.HubsProfilesExporting), tuigateway.ProfilesExport, params, func(r *tuigateway.ProfilesExportResult) tea.Cmd {
			return p.h.Done(tr(txt.HubsProfilesExported, r.Name, home.Display(r.Path)))
		})
	})
}

// Writes or removes the shell alias that opens a profile
func (p *profilesHub) alias(row tuigateway.ProfileOverviewRow, remove bool) tea.Cmd {
	params := tuigateway.ProfilesAliasParams{Name: row.Name, Remove: remove}
	return hubCall(p.a, p.h, tr(txt.HubsProfilesSaving), tuigateway.ProfilesAlias, params, func(r *tuigateway.ProfilesAliasResult) tea.Cmd {
		done := tr(txt.HubsProfilesAliasAdded, r.Name, home.Display(r.Path))
		if remove {
			done = tr(txt.HubsProfilesAliasRemoved, r.Name)
		}
		return tea.Batch(p.h.PopTo(p.list), p.load(), p.h.Done(done))
	})
}

// Ways to add a profile: create one, import an archive, or install a distribution
func (p *profilesHub) add() tea.Cmd {
	choices := []hubChoice{
		{id: "create", title: tr(txt.HubsProfilesCreate), detail: tr(txt.HubsProfilesCreateDoes)},
		{id: "import", title: tr(txt.HubsProfilesImport), detail: tr(txt.HubsProfilesImportDoes)},
		{id: "install", title: tr(txt.HubsProfilesInstall), detail: tr(txt.HubsProfilesInstallDoes)},
	}
	return p.a.hubPick(p.h, tr(txt.HubsProfilesAddTitle), choices, "", func(c hubChoice) tea.Cmd {
		switch c.id {
		case "import":
			return p.importArchive()
		case "install":
			return p.install()
		}
		return p.create()
	})
}

// Names of every profile as form choices, the one this window runs on first
func (p *profilesHub) sources() ([]cards.Choice, string) {
	var out []cards.Choice
	first := ""
	for _, row := range p.rows {
		c := cards.Choice{Value: row.Name, Label: profileLabel(row)}
		if p.mine(row) {
			first = row.Name
			out = append([]cards.Choice{c}, out...)
			continue
		}
		out = append(out, c)
	}
	return out, first
}

// Form of a new profile mapped onto profiles.create, kept open with what was typed when the create is refused
func (p *profilesHub) create() tea.Cmd {
	sources, mine := p.sources()
	start := []cards.Choice{
		{Value: "fresh", Label: tr(txt.HubsProfilesStartFresh), Meaning: tr(txt.HubsProfilesStartFreshMeaning)},
		{Value: "settings", Label: tr(txt.HubsProfilesStartSettings), Meaning: tr(txt.HubsProfilesStartSettingsMeaning)},
		{Value: "everything", Label: tr(txt.HubsProfilesStartEverything), Meaning: tr(txt.HubsProfilesStartEverythingMeaning)},
	}
	logins := []cards.Choice{
		{Value: "share", Label: tr(txt.HubsProfilesLoginsShare), Meaning: tr(txt.HubsProfilesLoginsShareMeaning)},
		{Value: "copy", Label: tr(txt.HubsProfilesLoginsCopy), Meaning: tr(txt.HubsProfilesLoginsCopyMeaning)},
		{Value: "none", Label: tr(txt.HubsProfilesLoginsNone), Meaning: tr(txt.HubsProfilesLoginsNoneMeaning)},
	}
	var form *cards.Form
	spec := cards.FormSpec{Title: tr(txt.HubsProfilesCreate), Submit: tr(txt.HubsProfilesCreateSubmit),
		Fields: []cards.Field{
			{ID: "name", Label: tr(txt.HubsProfilesFieldName), Required: true, Help: tr(txt.HubsProfilesFieldNameHelp)},
			{ID: "description", Label: tr(txt.HubsProfilesFieldDescription), Help: tr(txt.HubsProfilesFieldDescriptionHelp)},
			{ID: "start", Label: tr(txt.HubsProfilesFieldStart), Kind: cards.FieldChoice, Value: "fresh", Choices: start},
			{ID: "from", Label: tr(txt.HubsProfilesFieldFrom), Kind: cards.FieldChoice, Value: mine, Choices: sources, Help: tr(txt.HubsProfilesFieldFromHelp)},
			{ID: "logins", Label: tr(txt.HubsProfilesFieldLogins), Kind: cards.FieldChoice, Value: "share", Choices: logins},
			{ID: "skills", Label: tr(txt.HubsProfilesFieldSkills), Kind: cards.FieldBool, Value: "true", Help: tr(txt.HubsProfilesFieldSkillsHelp)},
			{ID: "model", Label: tr(txt.HubsProfilesFieldModel), More: true, Help: tr(txt.HubsProfilesFieldModelHelp)},
			{ID: "provider", Label: tr(txt.HubsProfilesFieldProvider), More: true},
			{ID: "channels", Label: tr(txt.HubsProfilesFieldChannels), Kind: cards.FieldBool, Value: "false", More: true, Help: tr(txt.HubsProfilesFieldChannelsHelp)},
			{ID: "alias", Label: tr(txt.HubsProfilesFieldAlias), Kind: cards.FieldBool, Value: "true", More: true, Help: tr(txt.HubsProfilesFieldAliasHelp)},
		},
		Cancel: func(*cards.Form) tea.Cmd { return p.h.Close(form) },
		Pick:   p.pickChoice,
	}
	spec.Done = func(f *cards.Form, v map[string]string) tea.Cmd {
		name := strings.TrimSpace(v["name"])
		params := tuigateway.ProfilesCreateParams{Name: name, NoSkills: ptr(v["skills"] != "true"), NoAlias: ptr(v["alias"] != "true"), Report: ptr(true)}
		if d := strings.TrimSpace(v["description"]); d != "" {
			params.Description = &d
		}
		if v["start"] != "fresh" {
			params.CloneFrom = ptr(v["from"])
			params.CloneAll = ptr(v["start"] == "everything")
			params.CloneChannels = ptr(v["channels"] == "true")
		}
		switch v["logins"] {
		case "copy":
			params.ShareAuth = ptr(false)
		case "none":
			params.MirrorCredentials = ptr(false)
		}
		if m := strings.TrimSpace(v["model"]); m != "" {
			params.Model = &m
		}
		if pr := strings.TrimSpace(v["provider"]); pr != "" {
			params.Provider = &pr
		}
		f.SetBusy(true)
		p.h.Busy(tr(txt.HubsProfilesCreating, name))
		return callWithin(p.a.gw, tuigateway.ProfilesCreate, params, profilesCreateWait, func(r *tuigateway.ProfilesCreateResult, err error) tea.Cmd {
			f.SetBusy(false)
			if err != nil {
				return p.h.Fail(rpcErrorMessage(err))
			}
			p.h.Quiet()
			done := tr(txt.HubsProfilesCreated, r.Name, home.Display(r.Path))
			if len(r.Warnings) > 0 {
				done += " " + strings.Join(r.Warnings, " ")
			}
			return tea.Batch(p.h.Close(form), p.load(), p.h.Done(done))
		})
	}
	form = cards.NewForm(p.a.hubEnv(), spec)
	return p.h.Push(form)
}

// Opens the choices of a long choice field in a picker on the hub
func (p *profilesHub) pickChoice(f *cards.Form, field cards.Field) tea.Cmd {
	choices := make([]hubChoice, len(field.Choices))
	for i, c := range field.Choices {
		choices[i] = hubChoice{id: c.Value, title: c.Label, detail: c.Meaning}
	}
	return p.a.hubPick(p.h, field.Label, choices, field.Value, func(c hubChoice) tea.Cmd {
		f.SetValue(field.ID, c.id)
		return nil
	})
}

// Imports a shared profile archive as a new profile
func (p *profilesHub) importArchive() tea.Cmd {
	spec := cards.FormSpec{Title: tr(txt.HubsProfilesImport), Submit: tr(txt.HubsProfilesImportSubmit),
		Fields: []cards.Field{
			{ID: "archive", Label: tr(txt.HubsProfilesImportArchive), Required: true, Placeholder: "~/profile.tar.gz"},
			{ID: "name", Label: tr(txt.HubsProfilesFieldName), Help: tr(txt.HubsProfilesImportNameHelp)},
		}}
	return p.a.hubForm(p.h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		params := tuigateway.ProfilesImportParams{Archive: absolutePath(profilesExpandHome(strings.TrimSpace(v["archive"]))), Name: strings.TrimSpace(v["name"])}
		return hubCall(p.a, p.h, tr(txt.HubsProfilesImporting), tuigateway.ProfilesImport, params, func(r *tuigateway.ProfilesImportResult) tea.Cmd {
			return tea.Batch(p.load(), p.h.Done(tr(txt.HubsProfilesImported, r.Name, home.Display(r.Path))))
		})
	})
}

// Installs a profile distribution from a git address or a local folder through the profile command, its preview and progress in a page
func (p *profilesHub) install() tea.Cmd {
	spec := cards.FormSpec{Title: tr(txt.HubsProfilesInstall), Submit: tr(txt.HubsProfilesInstallSubmit),
		Fields: []cards.Field{
			{ID: "source", Label: tr(txt.HubsProfilesInstallSource), Required: true, Placeholder: "github.com/user/repo", Help: tr(txt.HubsProfilesInstallSourceHelp)},
			{ID: "name", Label: tr(txt.HubsProfilesFieldName), Help: tr(txt.HubsProfilesInstallNameHelp)},
			{ID: "alias", Label: tr(txt.HubsProfilesFieldAlias), Kind: cards.FieldBool, Value: "false"},
			{ID: "force", Label: tr(txt.HubsProfilesInstallForce), Kind: cards.FieldBool, Value: "false", Help: tr(txt.HubsProfilesInstallForceHelp)},
		}}
	return p.a.hubForm(p.h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		argv := []string{"profile", "install", profilesExpandHome(strings.TrimSpace(v["source"])), "--yes"}
		if name := strings.TrimSpace(v["name"]); name != "" {
			argv = append(argv, "--name", name)
		}
		if v["alias"] == "true" {
			argv = append(argv, "--alias")
		}
		if v["force"] == "true" {
			argv = append(argv, "--force")
		}
		return p.a.hubRunCLI(p.h, tr(txt.HubsProfilesInstall), argv, func(*cliRun) tea.Cmd { return p.load() })
	})
}

// Path with a leading ~ standing for the user's home folder
func profilesExpandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	user, err := home.UserHome()
	if err != nil {
		return path
	}
	return filepath.Join(user, strings.TrimPrefix(path, "~"))
}
