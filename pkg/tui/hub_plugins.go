package tui

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the plugins and skills screens
const pluginsContext = "tui.plugins"

// Keys of the plugins and skills screens beyond the shared hub keys
func registerPluginsKeys(km *keys.Map) {
	if keys.ContextDocs[pluginsContext] == "" {
		if err := keys.AddContext(pluginsContext, "plugins and skills screen"); err != nil {
			panic(err)
		}
	}
	for _, a := range []keys.Action{
		{ID: "tui.plugins.next", Context: pluginsContext, Label: "next part", Doc: "show the next part: plugins, catalog, skills, skill hub, bundles, sources, trusted projects", Default: []string{"tab"}},
		{ID: "tui.plugins.prev", Context: pluginsContext, Label: "previous part", Doc: "show the previous part of the screen", Default: []string{"shift+tab"}, Quiet: true},
		{ID: "tui.plugins.update", Context: pluginsContext, Label: "update all", Doc: "check every installed plugin or skill and update those that can move", Default: []string{"u"}},
		{ID: "tui.plugins.more", Context: pluginsContext, Label: "more", Doc: "plugin packs, and checking a plugin folder before it is shared", Default: []string{"o"}},
	} {
		km.Register(a)
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "plugins", order: 40, title: txt.HubsTitlePlugins, short: txt.HubsShortPlugins, slash: "plugins", usage: "[update | enable|disable <name>]", keys: registerPluginsKeys,
		open:    func(a *app, part string) tea.Cmd { return a.openPlugins(part) },
		run:     (*app).pluginsSlash,
		suggest: func(string) []string { return []string{"update", "enable", "disable"} }})
}

// Parts of the plugins and skills screen in tab order
var pluginParts = []string{"plugins", "catalog", "skills", "hub", "bundles", "sources", "trusted"}

// Name of a part of the plugins and skills screen in plain words
func pluginPartTitle(part string) string {
	switch part {
	case "catalog":
		return tr(txt.HubsPluginsPartCatalog)
	case "skills":
		return tr(txt.HubsPluginsPartSkills)
	case "hub":
		return tr(txt.HubsPluginsPartHub)
	case "bundles":
		return tr(txt.HubsPluginsPartBundles)
	case "sources":
		return tr(txt.HubsPluginsPartSources)
	case "trusted":
		return tr(txt.HubsPluginsPartTrusted)
	}
	return tr(txt.HubsPluginsPartPlugins)
}

// Trailer naming every part with the one in view in brackets
func pluginPartsLine(current string) string {
	parts := make([]string, len(pluginParts))
	for i, p := range pluginParts {
		parts[i] = pluginPartTitle(p)
		if p == current {
			parts[i] = "[" + parts[i] + "]"
		}
	}
	return strings.Join(parts, " ")
}

// Plugins and skills screen, opened at a part: the installed plugins, the plugin catalog, the installed skills, the skill hub, the bundles,
// the skill sources or the trusted projects, tab moving between them
func (a *app) openPlugins(part string) tea.Cmd {
	if !slices.Contains(pluginParts, part) {
		part = "plugins"
	}
	var h *cards.Hub
	var cycle func(from string, d int) tea.Cmd
	cycle = func(from string, d int) tea.Cmd {
		i := slices.Index(pluginParts, from)
		return h.Swap(a.pluginPart(&h, pluginParts[(i+d+len(pluginParts))%len(pluginParts)], cycle))
	}
	h = a.newHub("tui:plugins", a.pluginPart(&h, part, cycle))
	return a.wm.ShowOverlay(h)
}

// One part of the plugins and skills screen as a list
func (a *app) pluginPart(hub **cards.Hub, part string, cycle func(from string, d int) tea.Cmd) *cards.List {
	switch part {
	case "catalog":
		return a.pluginCatalogPart(hub, cycle)
	case "skills":
		return a.skillsPart(hub, cycle)
	case "hub":
		return a.skillHubPart(hub, cycle)
	case "bundles":
		return a.bundlesPart(hub, cycle)
	case "sources":
		return a.skillSourcesPart(hub, cycle)
	case "trusted":
		return a.trustedPart(hub, cycle)
	}
	return a.installedPluginsPart(hub, cycle)
}

// Spec pieces every part shares: the context, the shared hub keys, the tab keys and the trailer naming the parts
func pluginPartSpec(a *app, part string, cycle func(from string, d int) tea.Cmd, spec cards.ListSpec) cards.ListSpec {
	spec.Title, spec.Glyph, spec.Key = tr(txt.HubsTitlePlugins)+" › "+pluginPartTitle(part), a.toolkit.Glyphs.Ext, "tui:plugins:"+part
	spec.Context, spec.Shared, spec.Primary = pluginsContext, []string{hubContext}, "hub.use"
	spec.Status = func() string { return pluginPartsLine(part) }
	if spec.Free == nil {
		spec.Free = map[string]bool{}
	}
	spec.Free["tui.plugins.next"], spec.Free["tui.plugins.prev"], spec.Free["hub.refresh"] = true, true, true
	spec.Actions["tui.plugins.next"] = func(*cards.List, int) tea.Cmd { return cycle(part, 1) }
	spec.Actions["tui.plugins.prev"] = func(*cards.List, int) tea.Cmd { return cycle(part, -1) }
	hints := spec.Hints
	spec.Hints = func(l *cards.List, i int) []wm.Hint {
		var out []wm.Hint
		if hints != nil {
			out = hints(l, i)
		}
		return append(out, wm.Hint{Action: "tui.plugins.next"})
	}
	return spec
}

// Lines a plugins or skills command printed, on a page of the hub
func (a *app) hubLinesPage(h *cards.Hub, title string, lines []string) tea.Cmd {
	page := cards.NewPage(a.hubEnv(), cards.PageSpec{Title: title, Key: "tui:plugins:output", Lines: func(int) []string {
		if len(lines) == 0 {
			return []string{render.Dim(tr(txt.HubsRunNoOutput), a.toolkit)}
		}
		return lines
	}})
	return h.Push(page)
}

// Installed plugins: on or off, version, source and what each adds; enter shows its actions
func (a *app) installedPluginsPart(hub **cards.Hub, cycle func(from string, d int) tea.Cmd) *cards.List {
	var (
		plugins []tuigateway.AgentPluginRow
		list    *cards.List
		loaded  bool
	)
	reload := func() tea.Cmd {
		return hubCall(a, *hub, "", tuigateway.PluginsManage, tuigateway.PluginsManageParams{Action: "list"}, func(r *tuigateway.PluginsManageResult) tea.Cmd {
			plugins, loaded = r.Plugins, true
			slices.SortStableFunc(plugins, func(x, y tuigateway.AgentPluginRow) int {
				if (x.Source == "bundled") != (y.Source == "bundled") {
					if x.Source == "bundled" {
						return 1
					}
					return -1
				}
				return strings.Compare(x.Name, y.Name)
			})
			list.Refresh()
			return nil
		})
	}
	at := func(i int) (tuigateway.AgentPluginRow, bool) {
		if i < 0 || i >= len(plugins) {
			return tuigateway.AgentPluginRow{}, false
		}
		return plugins[i], true
	}
	manage := func(busy string, params tuigateway.PluginsManageParams, done func(r *tuigateway.PluginsManageResult) tea.Cmd) tea.Cmd {
		return hubCall(a, *hub, busy, tuigateway.PluginsManage, params, func(r *tuigateway.PluginsManageResult) tea.Cmd {
			if r.Error != nil && *r.Error != "" {
				return tea.Batch((*hub).Fail(*r.Error), reload())
			}
			return tea.Batch(done(r), reload(), a.widgets.rescan(nil))
		})
	}
	run := func(p tuigateway.AgentPluginRow, verb, title string) tea.Cmd {
		return hubCall(a, *hub, tr(txt.HubsPluginsReading, p.Name), tuigateway.PluginsHubRun, tuigateway.PluginsHubRunParams{Verb: verb, Name: p.Name}, func(r *tuigateway.PluginsHubRunResult) tea.Cmd {
			return a.hubLinesPage(*hub, title, r.Lines)
		})
	}
	toggle := func(_ *cards.List, i int) tea.Cmd {
		p, ok := at(i)
		if !ok {
			return nil
		}
		done := tr(txt.HubsPluginsEnabled, p.Name)
		if p.Enabled() {
			done = tr(txt.HubsPluginsDisabled, p.Name)
		}
		return manage("", tuigateway.PluginsManageParams{Action: "toggle", Enable: ptr(!p.Enabled()), Key: p.Key, Name: p.Name}, func(*tuigateway.PluginsManageResult) tea.Cmd {
			return (*hub).Done(done)
		})
	}
	remove := func(l *cards.List, i int) tea.Cmd {
		p, ok := at(i)
		if !ok {
			return nil
		}
		if p.Source == "bundled" {
			return (*hub).Fail(tr(txt.HubsPluginsBundledStays, p.Name))
		}
		l.Confirm(tr(txt.HubsPluginsRemoveConfirm, p.Name), func() tea.Cmd {
			return manage(tr(txt.HubsPluginsRemoving, p.Name), tuigateway.PluginsManageParams{Action: "remove", Key: p.Key, Name: p.Name}, func(*tuigateway.PluginsManageResult) tea.Cmd {
				return (*hub).Done(tr(txt.HubsPluginsRemoved, p.Name))
			})
		})
		return nil
	}
	settings := func(_ *cards.List, i int) tea.Cmd {
		p, ok := at(i)
		if !ok {
			return nil
		}
		if len(p.SettingsSchema) == 0 {
			return (*hub).Fail(tr(txt.HubsPluginsNoSettings, p.Name))
		}
		var fields []cards.Field
		for _, s := range p.SettingsSchema {
			f := cards.Field{ID: s.Key, Label: firstText(s.Label, s.Key), Help: s.Description, Value: settingEditText(tuigateway.SettingsRow{Value: s.Value})}
			switch s.Type {
			case "boolean":
				f.Kind = cards.FieldBool
				on, _ := s.Value.(bool)
				f.Value = strconv.FormatBool(on)
			case "secret":
				f.Kind, f.Value = cards.FieldSecret, ""
				if val(s.HasValue) {
					f.Placeholder = tr(txt.HubsPluginsSecretSet)
				}
			case "number":
				f.Kind = cards.FieldNumber
			default:
				f.Kind = cards.FieldText
				for _, c := range s.Choices {
					f.Choices = append(f.Choices, cards.Choice{Value: c, Label: c})
				}
				if len(f.Choices) > 0 {
					f.Kind = cards.FieldChoice
				}
			}
			f.Required = s.Required && !(s.Type == "secret" && val(s.HasValue))
			fields = append(fields, f)
		}
		return a.hubForm(*hub, cards.FormSpec{Title: tr(txt.HubsPluginsSettingsTitle, p.Name), Fields: fields, Submit: tr(txt.HubsSettingsSave)}, func(_ *cards.Form, v map[string]string) tea.Cmd {
			values := map[string]any{}
			for _, s := range p.SettingsSchema {
				text := v[s.Key]
				switch {
				case s.Type == "secret" && text == "":
				case s.Type == "boolean":
					values[s.Key] = text == "true"
				default:
					kind := "string"
					if s.Type == "number" {
						kind = "float"
					}
					value, err := settingValue(kind, text)
					if err != nil {
						return (*hub).Fail(err.Error())
					}
					values[s.Key] = value
				}
			}
			return manage("", tuigateway.PluginsManageParams{Action: "settings", Key: p.Key, Name: p.Name, Values: values}, func(*tuigateway.PluginsManageResult) tea.Cmd {
				return (*hub).Done(tr(txt.HubsPluginsSettingsSaved, p.Name))
			})
		})
	}
	update := func(p tuigateway.AgentPluginRow) tea.Cmd {
		params := tuigateway.PluginsManageParams{Action: "update", Key: p.Key, Name: p.Name}
		(*hub).Busy(tr(txt.HubsPluginsUpdating, p.Name))
		return callWithin(a.gw, tuigateway.PluginsManage, params, pluginsUpdateAllTimeout, func(r *tuigateway.PluginsManageResult, err error) tea.Cmd {
			switch {
			case err != nil:
				return (*hub).Fail(rpcErrorMessage(err))
			case r.Error != nil && *r.Error != "":
				return (*hub).Fail(*r.Error)
			case r.ConsentRequired != nil && *r.ConsentRequired:
				return a.pluginConsent(*hub, p.Name, r, func(sha string) tea.Cmd {
					params.AcceptCapabilities, params.InspectedSha = true, sha
					return manage(tr(txt.HubsPluginsUpdating, p.Name), params, func(*tuigateway.PluginsManageResult) tea.Cmd { return (*hub).Done(tr(txt.HubsPluginsUpdated, p.Name)) })
				})
			}
			return tea.Batch((*hub).Done(tr(txt.HubsPluginsUpdated, p.Name)), reload())
		})
	}
	open := func(_ *cards.List, i int) tea.Cmd {
		p, ok := at(i)
		if !ok {
			return nil
		}
		return a.hubActions(*hub, p.Name, func() []string { return pluginDetail(p) }, func() []hubAction {
			acts := []hubAction{
				{key: "hub.toggle", label: tr(txt.HubsPluginsActToggle), does: tr(txt.HubsPluginsActToggleDoes), run: func() tea.Cmd { return toggle(list, i) }},
				{label: tr(txt.HubsPluginsActShow), does: tr(txt.HubsPluginsActShowDoes), run: func() tea.Cmd { return run(p, "show", p.Name) }},
				{label: tr(txt.HubsPluginsActCapabilities), does: tr(txt.HubsPluginsActCapabilitiesDoes), run: func() tea.Cmd { return run(p, "capabilities", p.Name) }},
			}
			if len(p.SettingsSchema) > 0 {
				acts = append(acts, hubAction{key: "hub.edit", label: tr(txt.HubsPluginsActSettings), does: tr(txt.HubsPluginsActSettingsDoes), run: func() tea.Cmd { return settings(list, i) }})
			}
			if p.Provenance == "self-cloned" {
				acts = append(acts, hubAction{label: tr(txt.HubsPluginsActAdopt), does: tr(txt.HubsPluginsActAdoptDoes), run: func() tea.Cmd {
					return hubCall(a, *hub, tr(txt.HubsPluginsReading, p.Name), tuigateway.PluginsHubRun, tuigateway.PluginsHubRunParams{Verb: "adopt", Name: p.Name}, func(r *tuigateway.PluginsHubRunResult) tea.Cmd {
						return tea.Batch(a.hubLinesPage(*hub, p.Name, r.Lines), reload())
					})
				}})
			}
			if p.Source != "bundled" {
				acts = append(acts,
					hubAction{label: tr(txt.HubsPluginsActUpdate), does: tr(txt.HubsPluginsActUpdateDoes), run: func() tea.Cmd { return update(p) }},
					hubAction{label: tr(txt.HubsPluginsActTrustURL), does: tr(txt.HubsPluginsActTrustURLDoes), confirm: tr(txt.HubsPluginsTrustURLConfirm, p.Name),
						run: func() tea.Cmd { return run(p, "trust-update-url", p.Name) }},
					hubAction{key: "hub.remove", label: tr(txt.HubsPluginsActRemove), does: tr(txt.HubsPluginsActRemoveDoes), confirm: tr(txt.HubsPluginsRemoveConfirm, p.Name),
						run: func() tea.Cmd {
							return manage(tr(txt.HubsPluginsRemoving, p.Name), tuigateway.PluginsManageParams{Action: "remove", Key: p.Key, Name: p.Name}, func(*tuigateway.PluginsManageResult) tea.Cmd {
								return tea.Batch((*hub).Pop(), (*hub).Done(tr(txt.HubsPluginsRemoved, p.Name)))
							})
						}})
			}
			return append(acts, hubAction{label: tr(txt.HubsPluginsActDoctor), does: tr(txt.HubsPluginsActDoctorDoes), run: func() tea.Cmd {
				return hubCall(a, *hub, tr(txt.HubsPluginsReading, p.Name), tuigateway.PluginsHubRun, tuigateway.PluginsHubRunParams{Verb: "doctor", Source: p.InstallDir}, func(r *tuigateway.PluginsHubRunResult) tea.Cmd {
					return a.hubLinesPage(*hub, p.Name, r.Lines)
				})
			}})
		})
	}
	install := func(*cards.List, int) tea.Cmd { return a.installPlugin(*hub, "", reload) }
	spec := cards.ListSpec{
		Columns: []render.Column{{Title: tr(txt.HubsColOn)}, {Title: tr(txt.HubsColPlugin), Max: 30}, {Title: tr(txt.HubsColVersion), Max: 12},
			{Title: tr(txt.HubsColSource)}, {Title: tr(txt.HubsColWhatItAdds), Flex: 1}},
		Yield: []int{2, 3},
		Rows: func() [][]string {
			out := make([][]string, len(plugins))
			for i, p := range plugins {
				mark := "○"
				if p.Enabled() {
					mark = "●"
				}
				version := p.Version
				if p.UpdateAvailable != nil && *p.UpdateAvailable {
					version += " ↑"
				}
				out[i] = []string{mark, p.Name, version, p.Source, strings.Join(strings.Fields(p.Description), " ")}
			}
			return out
		},
		Detail: func(i int) []string {
			p, ok := at(i)
			if !ok {
				return nil
			}
			return pluginDetail(p)
		},
		Empty: func() string {
			if !loaded {
				return tr(txt.HubsLoading)
			}
			return tr(txt.HubsPluginsEmpty, a.keys.Display("hub.add"))
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if _, ok := at(i); !ok {
				return []wm.Hint{{Action: "hub.add", Label: tr(txt.HintsLabelInstall)}, {Action: "tui.plugins.more"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, {Action: "hub.toggle"}, {Action: "hub.add", Label: tr(txt.HintsLabelInstall)}, {Action: "hub.remove"},
				{Action: "tui.plugins.update"}, {Action: "tui.plugins.more"}}
		},
		Free: map[string]bool{"hub.add": true, "tui.plugins.update": true, "tui.plugins.more": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"tui.plugins.more": func(*cards.List, int) tea.Cmd { return a.pluginsMore(*hub, reload) },
			"hub.use":          open,
			"hub.toggle":       toggle,
			"hub.remove":       remove,
			"hub.edit":         settings,
			"hub.add":          install,
			"hub.refresh":      func(*cards.List, int) tea.Cmd { return reload() },
			"tui.plugins.update": func(*cards.List, int) tea.Cmd {
				(*hub).Busy(tr(txt.SlashCmdOpsPluginsUpdateRunning))
				return callWithin(a.gw, tuigateway.PluginsManage, tuigateway.PluginsManageParams{Action: "update_all"}, pluginsUpdateAllTimeout, func(r *tuigateway.PluginsManageResult, err error) tea.Cmd {
					if err != nil {
						return (*hub).Fail(rpcErrorMessage(err))
					}
					(*hub).Quiet()
					return tea.Batch(a.hubLinesPage(*hub, tr(txt.SlashCmdOpsPluginsUpdateTitle), pluginsUpdateAllLines(*r)), reload())
				})
			},
		},
		OnFocus: func(*cards.List) tea.Cmd { return reload() },
	}
	list = cards.NewList(a.hubEnv(), pluginPartSpec(a, "plugins", cycle, spec))
	return list
}

// More for plugins: a pack installed once it was reviewed, this setup exported as a pack, and a plugin folder checked the way the catalog
// checks it; done runs after an install so the installed plugins are read again
func (a *app) pluginsMore(h *cards.Hub, done func() tea.Cmd) tea.Cmd {
	cwd := a.store.sessionCwd()
	run := func(busy string, params tuigateway.PluginsHubRunParams, then func(r *tuigateway.PluginsHubRunResult) tea.Cmd) tea.Cmd {
		h.Busy(tr(txt.HubsPluginsReading, busy))
		return callWithin(a.gw, tuigateway.PluginsHubRun, params, pluginsUpdateAllTimeout, func(r *tuigateway.PluginsHubRunResult, err error) tea.Cmd {
			if err != nil {
				return h.Fail(rpcErrorMessage(err))
			}
			h.Quiet()
			return then(r)
		})
	}
	installPack := func() tea.Cmd {
		return a.hubAsk(h, tr(txt.HubsPluginsPackInstall), tr(txt.HubsPluginsPackSource), "", false, func(source string) tea.Cmd {
			if source == "" {
				return nil
			}
			return run(source, tuigateway.PluginsHubRunParams{Verb: "pack-show", Source: source}, func(r *tuigateway.PluginsHubRunResult) tea.Cmd {
				if !r.OK {
					return a.hubLinesPage(h, source, r.Lines)
				}
				install := func(force bool) func() tea.Cmd {
					return func() tea.Cmd {
						h.Busy(tr(txt.HubsPluginsPackInstalling))
						params := tuigateway.PluginsPackInstallParams{Source: source, Force: force}
						return callWithin(a.gw, tuigateway.PluginsPackInstall, params, pluginsUpdateAllTimeout, func(res *tuigateway.PluginsHubRunResult, err error) tea.Cmd {
							if err != nil {
								return h.Fail(rpcErrorMessage(err))
							}
							h.Quiet()
							return tea.Batch(h.Pop(), a.hubLinesPage(h, source, res.Lines), done(), a.widgets.rescan(nil))
						})
					}
				}
				return a.hubActions(h, source, func() []string { return r.Lines }, func() []hubAction {
					return []hubAction{
						{label: tr(txt.HubsPluginsPackInstallGo), does: tr(txt.HubsPluginsPackInstallGoDoes), run: install(false)},
						{label: tr(txt.HubsPluginsPackReinstall), does: tr(txt.HubsPluginsPackReinstallDoes), run: install(true)},
					}
				})
			})
		})
	}
	exportPack := func() tea.Cmd {
		title := tr(txt.HubsPluginsPackExport)
		spec := cards.FormSpec{Title: title, Submit: tr(txt.SlashFormRun), Fields: []cards.Field{
			{ID: "name", Label: tr(txt.HubsPluginsPackName), Kind: cards.FieldText, Value: "my-icebath-pack", Required: true},
			{ID: "enabled", Label: tr(txt.HubsPluginsPackEnabledOnly), Kind: cards.FieldBool, Value: "false"},
			{ID: "output", Label: tr(txt.HubsPluginsPackOutput), Kind: cards.FieldText, Value: filepath.Join(cwd, "icebath-pack.yaml"), Help: tr(txt.HubsPluginsPackOutputHelp)},
		}}
		return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
			output := strings.TrimSpace(v["output"])
			if output != "" && !filepath.IsAbs(output) {
				return h.Fail(tr(txt.HubsPluginsNeedsFullPath, tr(txt.HubsPluginsPackOutput)))
			}
			params := tuigateway.PluginsHubRunParams{Verb: "pack-export", PackName: strings.TrimSpace(v["name"]), EnabledOnly: v["enabled"] == "true", Output: output}
			return run(title, params, func(r *tuigateway.PluginsHubRunResult) tea.Cmd {
				page := a.hubLinesPage(h, title, r.Lines)
				if output != "" && r.OK {
					return tea.Batch(page, h.Done(tr(txt.HubsPluginsPackSaved, output)))
				}
				return page
			})
		})
	}
	validate := func() tea.Cmd {
		title := tr(txt.HubsPluginsValidate)
		spec := cards.FormSpec{Title: title, Submit: tr(txt.SlashFormRun), Fields: []cards.Field{
			{ID: "path", Label: tr(txt.HubsPluginsValidatePath), Kind: cards.FieldText, Value: cwd, Required: true},
			{ID: "deps", Label: tr(txt.HubsPluginsValidateDeps), Kind: cards.FieldBool, Value: "false"},
		}}
		return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
			path := strings.TrimSpace(v["path"])
			if !filepath.IsAbs(path) {
				return h.Fail(tr(txt.HubsPluginsNeedsFullPath, tr(txt.HubsPluginsValidatePath)))
			}
			params := tuigateway.PluginsHubRunParams{Verb: "validate", Source: path, InstallDeps: v["deps"] == "true"}
			return run(path, params, func(r *tuigateway.PluginsHubRunResult) tea.Cmd { return a.hubLinesPage(h, title, r.Lines) })
		})
	}
	return a.hubActions(h, tr(txt.HubsPluginsMoreTitle), func() []string { return []string{tr(txt.HubsPluginsMoreDetail)} }, func() []hubAction {
		return []hubAction{
			{label: tr(txt.HubsPluginsPackInstall), does: tr(txt.HubsPluginsPackInstallDoes), run: installPack},
			{label: tr(txt.HubsPluginsPackExport), does: tr(txt.HubsPluginsPackExportDoes), run: exportPack},
			{label: tr(txt.HubsPluginsValidate), does: tr(txt.HubsPluginsValidateDoes), run: validate},
		}
	})
}

// Publishes a skill of this machine to a GitHub skills repository as a pull request, its folder read from the skill first
func (a *app) skillPublish(h *cards.Hub, name string) tea.Cmd {
	return hubCall(a, h, "", tuigateway.SkillsManage, tuigateway.SkillsManageParams{Action: "inspect", Query: name}, func(r *tuigateway.SkillsManageResult) tea.Cmd {
		if r.Info == nil || r.Info.Path == "" {
			return h.Fail(tr(txt.HubsSkillsPublishNoPath, name))
		}
		path, title := r.Info.Path, tr(txt.HubsSkillsPublishTitle, name)
		spec := cards.FormSpec{Title: title, Intro: []string{tr(txt.HubsSkillsActPublishDoes), tr(txt.HubsSkillsPathLine, path)}, Submit: tr(txt.HubsSkillsActPublish),
			Fields: []cards.Field{{ID: "repo", Label: tr(txt.HubsSkillsPublishRepo), Kind: cards.FieldText, Help: tr(txt.HubsSkillsPublishRepoHelp), Required: true}}}
		return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
			params := tuigateway.SkillsHubRunParams{Action: "publish", Path: path, To: "github", Repo: strings.TrimSpace(v["repo"])}
			return a.skillsRun(h, title, params, func() tea.Cmd { return nil })
		})
	})
}

// Lines of a plugin's detail pane: what it adds, where it comes from, its version and updates, its servers and tools
func pluginDetail(p tuigateway.AgentPluginRow) []string {
	out := []string{p.Name + " " + p.Version}
	if d := strings.Join(strings.Fields(p.Description), " "); d != "" {
		out = append(out, d)
	}
	out = append(out, tr(txt.HubsPluginsDetailSource, p.Source))
	if p.InstallDir != "" {
		out = append(out, tr(txt.HubsPluginsDetailPath, p.InstallDir))
	}
	if p.UpdateAvailable != nil && *p.UpdateAvailable {
		out = append(out, tr(txt.HubsPluginsDetailUpdate, firstText(val(p.UpdateTarget), val(p.CatalogVersion))))
	}
	for _, srv := range p.Servers {
		out = append(out, "  "+srv.Name+": "+srv.Sentence)
	}
	if len(p.Tools) > 0 {
		out = append(out, tr(txt.HubsPluginsDetailTools, strings.Join(p.Tools, ", ")))
	}
	return out
}

// Installs a plugin from a catalog name, a Git URL or owner/repo: what the source asks for is read first and shown, and the install runs
// once it is accepted, a security scan refusal shown with an explicit way past it
func (a *app) installPlugin(h *cards.Hub, catalogName string, done func() tea.Cmd) tea.Cmd {
	install := func(params tuigateway.PluginsManageParams) tea.Cmd {
		var run func(p tuigateway.PluginsManageParams) tea.Cmd
		run = func(p tuigateway.PluginsManageParams) tea.Cmd {
			h.Busy(tr(txt.HubsPluginsInstalling))
			return callWithin(a.gw, tuigateway.PluginsManage, p, pluginsUpdateAllTimeout, func(r *tuigateway.PluginsManageResult, err error) tea.Cmd {
				switch {
				case err != nil:
					return h.Fail(rpcErrorMessage(err))
				case r.ScanBlocked != nil && *r.ScanBlocked:
					var lines []string
					for _, f := range r.ScanFindings {
						lines = append(lines, f.Severity+" · "+f.File+":"+itoa(f.Line)+" · "+f.Description)
					}
					h.Quiet()
					return a.hubActions(h, tr(txt.HubsPluginsScanBlocked), func() []string { return lines }, func() []hubAction {
						return []hubAction{{label: tr(txt.HubsPluginsInstallAnyway), does: tr(txt.HubsPluginsInstallAnywayDoes), confirm: tr(txt.HubsPluginsInstallAnywayConfirm),
							run: func() tea.Cmd { p.Force = true; return tea.Batch(h.Pop(), run(p)) }}}
					})
				case r.Error != nil && *r.Error != "":
					return h.Fail(*r.Error)
				}
				name := firstText(val(r.PluginName), val(r.Name), p.CatalogName, p.Identifier)
				lines := slices.Concat(r.Warnings, r.KnownIssues)
				if len(r.MissingEnv) > 0 {
					lines = append(lines, tr(txt.HubsPluginsMissingEnv, strings.Join(r.MissingEnv, ", ")))
				}
				cmds := []tea.Cmd{h.Done(tr(txt.HubsPluginsInstalled, name)), done(), a.widgets.rescan(nil)}
				if len(lines) > 0 {
					cmds = append(cmds, a.hubLinesPage(h, name, lines))
				}
				return tea.Batch(cmds...)
			})
		}
		h.Busy(tr(txt.HubsPluginsReading, firstText(params.CatalogName, params.Identifier)))
		inspect := params
		inspect.Action = "inspect"
		return callWithin(a.gw, tuigateway.PluginsManage, inspect, pluginsUpdateAllTimeout, func(r *tuigateway.PluginsManageResult, err error) tea.Cmd {
			if err != nil {
				return h.Fail(rpcErrorMessage(err))
			}
			h.Quiet()
			lines := slices.Concat(r.DeltaLines, r.Warnings, r.KnownIssues)
			if len(r.MissingEnv) > 0 {
				lines = append(lines, tr(txt.HubsPluginsMissingEnv, strings.Join(r.MissingEnv, ", ")))
			}
			if len(lines) == 0 {
				lines = []string{tr(txt.HubsPluginsNothingAsked)}
			}
			params.InspectedSha = val(r.Sha)
			title := firstText(val(r.Name), params.CatalogName, params.Identifier)
			return a.hubActions(h, title, func() []string { return lines }, func() []hubAction {
				return []hubAction{{label: tr(txt.HubsPluginsInstall), does: tr(txt.HubsPluginsInstallDoes), run: func() tea.Cmd { return tea.Batch(h.Pop(), run(params)) }}}
			})
		})
	}
	if catalogName != "" {
		return install(tuigateway.PluginsManageParams{Action: "install", CatalogName: catalogName})
	}
	spec := cards.FormSpec{Title: tr(txt.HubsPluginsInstallTitle), Submit: tr(txt.HubsPluginsInstallNext), Fields: []cards.Field{
		{ID: "source", Label: tr(txt.HubsPluginsSourceLabel), Kind: cards.FieldText, Required: true, Placeholder: "owner/repo", Help: tr(txt.HubsPluginsSourceHelp)},
		{ID: "ref", Label: tr(txt.HubsPluginsRefLabel), Kind: cards.FieldText, More: true, Help: tr(txt.HubsPluginsRefHelp)},
	}}
	return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		source := strings.TrimSpace(v["source"])
		params := tuigateway.PluginsManageParams{Action: "install", Identifier: source, Ref: strings.TrimSpace(v["ref"])}
		if !strings.ContainsAny(source, "/:") {
			params = tuigateway.PluginsManageParams{Action: "install", CatalogName: source}
		}
		return install(params)
	})
}

// Number as text
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Asks before an update that changes what a plugin may do, the changes shown and the update run again once accepted
func (a *app) pluginConsent(h *cards.Hub, name string, r *tuigateway.PluginsManageResult, accept func(sha string) tea.Cmd) tea.Cmd {
	h.Quiet()
	lines := r.DeltaLines
	sha := val(r.Sha)
	return a.hubActions(h, tr(txt.HubsPluginsConsentTitle, name), func() []string { return lines }, func() []hubAction {
		return []hubAction{{label: tr(txt.HubsPluginsAccept), does: tr(txt.HubsPluginsAcceptDoes), run: func() tea.Cmd { return tea.Batch(h.Pop(), accept(sha)) }}}
	})
}

// Plugin catalog: every curated plugin of this build with what it adds, enter installing one
func (a *app) pluginCatalogPart(hub **cards.Hub, cycle func(from string, d int) tea.Cmd) *cards.List {
	var (
		entries []tuigateway.PluginCatalogRow
		list    *cards.List
		loaded  bool
	)
	reload := func() tea.Cmd {
		return hubCall(a, *hub, "", tuigateway.PluginsCatalog, tuigateway.PluginsCatalogParams{}, func(r *tuigateway.PluginsCatalogResult) tea.Cmd {
			entries, loaded = r.Entries, true
			list.Refresh()
			return nil
		})
	}
	at := func(i int) (tuigateway.PluginCatalogRow, bool) {
		if i < 0 || i >= len(entries) {
			return tuigateway.PluginCatalogRow{}, false
		}
		return entries[i], true
	}
	install := func(_ *cards.List, i int) tea.Cmd {
		e, ok := at(i)
		if !ok {
			return nil
		}
		return a.installPlugin(*hub, e.Name, func() tea.Cmd { return nil })
	}
	spec := cards.ListSpec{
		Columns: []render.Column{{Title: tr(txt.HubsColPlugin), Max: 28}, {Title: tr(txt.HubsColKind), Max: 14}, {Title: tr(txt.HubsColWhatItAdds), Flex: 1}},
		Yield:   []int{1},
		Rows: func() [][]string {
			out := make([][]string, len(entries))
			for i, e := range entries {
				out[i] = []string{firstText(e.Title, e.Name), firstText(e.Category, e.Tier), strings.Join(strings.Fields(e.Description), " ")}
			}
			return out
		},
		Detail: func(i int) []string {
			e, ok := at(i)
			if !ok {
				return nil
			}
			out := []string{firstText(e.Title, e.Name) + " " + e.Version, e.Description}
			if e.Repo != "" {
				out = append(out, tr(txt.HubsPluginsDetailSource, e.Repo))
			}
			if len(e.Tools) > 0 {
				out = append(out, tr(txt.HubsPluginsDetailTools, strings.Join(e.Tools, ", ")))
			}
			if len(e.RequiresEnv) > 0 {
				out = append(out, tr(txt.HubsPluginsMissingEnv, strings.Join(e.RequiresEnv, ", ")))
			}
			return append(out, e.KnownIssues...)
		},
		Empty: func() string {
			if !loaded {
				return tr(txt.HubsLoading)
			}
			return tr(txt.HubsPluginsCatalogEmpty)
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelInstall)}, {Action: "hub.add", Label: tr(txt.HubsPluginsFromSource)}}
		},
		Free: map[string]bool{"hub.add": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use":     install,
			"hub.add":     func(*cards.List, int) tea.Cmd { return a.installPlugin(*hub, "", func() tea.Cmd { return nil }) },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return reload() },
		},
		OnFocus: func(*cards.List) tea.Cmd { return reload() },
	}
	list = cards.NewList(a.hubEnv(), pluginPartSpec(a, "catalog", cycle, spec))
	return list
}

// Runs one skills command without questions and shows what it printed on a page of the hub, then reads the skills again
func (a *app) skillsRun(h *cards.Hub, title string, params tuigateway.SkillsHubRunParams, done func() tea.Cmd) tea.Cmd {
	h.Busy(tr(txt.HubsSkillsRunning, title))
	return callWithin(a.gw, tuigateway.SkillsHubRun, params, pluginsUpdateAllTimeout, func(r *tuigateway.SkillsHubRunResult, err error) tea.Cmd {
		if err != nil {
			return h.Fail(rpcErrorMessage(err))
		}
		h.Quiet()
		note := h.Done(tr(txt.HubsSkillsRan, title))
		if !r.OK {
			note = h.Fail(tr(txt.HubsSkillsRunFailed, title))
		}
		return tea.Batch(note, a.hubLinesPage(h, title, r.Lines), done(), a.events.loadSlashCatalog(a.store.ui.Sid))
	})
}

// Installed skills: on or off, category, where each comes from and what it does; t turns one off for the agent, x uninstalls a hub skill
func (a *app) skillsPart(hub **cards.Hub, cycle func(from string, d int) tea.Cmd) *cards.List {
	var (
		data   tuigateway.SkillsHubResult
		list   *cards.List
		loaded bool
	)
	reload := func() tea.Cmd {
		return hubCall(a, *hub, "", tuigateway.SkillsHubList, tuigateway.SkillsHubParams{}, func(r *tuigateway.SkillsHubResult) tea.Cmd {
			data, loaded = *r, true
			list.Refresh()
			return nil
		})
	}
	at := func(i int) (tuigateway.SkillHubRow, bool) {
		if i < 0 || i >= len(data.Skills) {
			return tuigateway.SkillHubRow{}, false
		}
		return data.Skills[i], true
	}
	toggle := func(_ *cards.List, i int) tea.Cmd {
		s, ok := at(i)
		if !ok {
			return nil
		}
		if s.Essential {
			return (*hub).Fail(tr(txt.HubsSkillsEssential, s.Name))
		}
		var disabled []any
		for _, other := range data.Skills {
			if !other.Enabled && other.Name != s.Name {
				disabled = append(disabled, other.Name)
			}
		}
		done := tr(txt.HubsSkillsEnabled, s.Name)
		if s.Enabled {
			disabled = append(disabled, s.Name)
			done = tr(txt.HubsSkillsDisabled, s.Name)
		}
		if disabled == nil {
			disabled = []any{}
		}
		p := tuigateway.SettingsSetParams{Key: "skills.disabled", Value: disabled}
		return hubCall(a, *hub, "", tuigateway.SettingsSet, p, func(*tuigateway.SettingsWriteResult) tea.Cmd {
			return tea.Batch((*hub).Done(done), reload(), a.events.loadSlashCatalog(a.store.ui.Sid))
		})
	}
	uninstall := func(l *cards.List, i int) tea.Cmd {
		s, ok := at(i)
		if !ok {
			return nil
		}
		if s.Source != "hub" {
			return (*hub).Fail(tr(txt.HubsSkillsNotFromHub, s.Name))
		}
		l.Confirm(tr(txt.HubsSkillsUninstallConfirm, s.Name), func() tea.Cmd {
			return a.skillsRun(*hub, tr(txt.HubsSkillsUninstallTitle, s.Name), tuigateway.SkillsHubRunParams{Action: "uninstall", Name: s.Name}, reload)
		})
		return nil
	}
	open := func(_ *cards.List, i int) tea.Cmd {
		s, ok := at(i)
		if !ok {
			return nil
		}
		return a.hubActions(*hub, s.Name, func() []string { return skillDetail(s) }, func() []hubAction {
			acts := []hubAction{
				{key: "hub.toggle", label: tr(txt.HubsSkillsActToggle), does: tr(txt.HubsSkillsActToggleDoes), run: func() tea.Cmd { return toggle(list, i) }},
				{label: tr(txt.HubsSkillsActInspect), does: tr(txt.HubsSkillsActInspectDoes), run: func() tea.Cmd {
					return hubCall(a, *hub, "", tuigateway.SkillsManage, tuigateway.SkillsManageParams{Action: "inspect", Query: s.Name}, func(r *tuigateway.SkillsManageResult) tea.Cmd {
						lines := []string{s.Name}
						if r.Info != nil {
							lines = strings.Split(r.Info.Description, "\n")
							if r.Info.SkillMdPreview != nil {
								lines = append(append(lines, ""), strings.Split(*r.Info.SkillMdPreview, "\n")...)
							}
							if r.Info.Path != "" {
								lines = append(lines, "", tr(txt.HubsSkillsPathLine, r.Info.Path))
							}
						}
						return a.hubLinesPage(*hub, s.Name, lines)
					})
				}},
			}
			switch s.Source {
			case "local":
				acts = append(acts, hubAction{label: tr(txt.HubsSkillsActPublish), does: tr(txt.HubsSkillsActPublishDoes), run: func() tea.Cmd { return a.skillPublish(*hub, s.Name) }})
			case "hub":
				acts = append(acts,
					hubAction{label: tr(txt.HubsSkillsActUpdate), does: tr(txt.HubsSkillsActUpdateDoes), run: func() tea.Cmd {
						return a.skillsRun(*hub, tr(txt.HubsSkillsUpdateTitle, s.Name), tuigateway.SkillsHubRunParams{Action: "update", Name: s.Name}, reload)
					}},
					hubAction{key: "hub.remove", label: tr(txt.HubsSkillsActUninstall), does: tr(txt.HubsSkillsActUninstallDoes), confirm: tr(txt.HubsSkillsUninstallConfirm, s.Name), run: func() tea.Cmd {
						return tea.Batch((*hub).Pop(), a.skillsRun(*hub, tr(txt.HubsSkillsUninstallTitle, s.Name), tuigateway.SkillsHubRunParams{Action: "uninstall", Name: s.Name}, reload))
					}})
			case "builtin":
				acts = append(acts,
					hubAction{label: tr(txt.HubsSkillsActDiff), does: tr(txt.HubsSkillsActDiffDoes), run: func() tea.Cmd {
						return a.skillsRun(*hub, s.Name, tuigateway.SkillsHubRunParams{Action: "diff", Name: s.Name}, reload)
					}},
					hubAction{label: tr(txt.HubsSkillsActReset), does: tr(txt.HubsSkillsActResetDoes), confirm: tr(txt.HubsSkillsResetConfirm, s.Name), run: func() tea.Cmd {
						return a.skillsRun(*hub, s.Name, tuigateway.SkillsHubRunParams{Action: "reset", Name: s.Name, Restore: true}, reload)
					}},
					hubAction{label: tr(txt.HubsSkillsActRepair), does: tr(txt.HubsSkillsActRepairDoes), run: func() tea.Cmd {
						return a.skillsRun(*hub, s.Name, tuigateway.SkillsHubRunParams{Action: "repair-official", Name: s.Name}, reload)
					}})
			}
			return acts
		})
	}
	more := func(*cards.List, int) tea.Cmd {
		optAction, optLabel := "opt-out", tr(txt.HubsSkillsActOptOut)
		if data.OptedOut {
			optAction, optLabel = "opt-in", tr(txt.HubsSkillsActOptIn)
		}
		return a.hubActions(*hub, tr(txt.HubsSkillsEveryTitle), func() []string { return []string{tr(txt.HubsSkillsEveryDetail)} }, func() []hubAction {
			run := func(action, title string) func() tea.Cmd {
				return func() tea.Cmd { return a.skillsRun(*hub, title, tuigateway.SkillsHubRunParams{Action: action}, reload) }
			}
			return []hubAction{
				{key: "tui.plugins.update", label: tr(txt.HubsSkillsActUpdateAll), does: tr(txt.HubsSkillsActUpdateAllDoes), run: run("update", tr(txt.HubsSkillsActUpdateAll))},
				{label: tr(txt.HubsSkillsActCheck), does: tr(txt.HubsSkillsActCheckDoes), run: run("check", tr(txt.HubsSkillsActCheck))},
				{label: tr(txt.HubsSkillsActAudit), does: tr(txt.HubsSkillsActAuditDoes), run: run("audit", tr(txt.HubsSkillsActAudit))},
				{label: tr(txt.HubsSkillsActModified), does: tr(txt.HubsSkillsActModifiedDoes), run: run("list-modified", tr(txt.HubsSkillsActModified))},
				{label: optLabel, does: tr(txt.HubsSkillsActOptDoes), run: run(optAction, optLabel)},
				{label: tr(txt.HubsSkillsActExport), does: tr(txt.HubsSkillsActExportDoes), run: func() tea.Cmd {
					return a.hubAsk(*hub, tr(txt.HubsSkillsActExport), tr(txt.HubsSkillsFileLabel), "skills-snapshot.json", false, func(path string) tea.Cmd {
						return a.skillsRun(*hub, tr(txt.HubsSkillsActExport), tuigateway.SkillsHubRunParams{Action: "snapshot-export", Path: path}, reload)
					})
				}},
				{label: tr(txt.HubsSkillsActImport), does: tr(txt.HubsSkillsActImportDoes), run: func() tea.Cmd {
					return a.hubAsk(*hub, tr(txt.HubsSkillsActImport), tr(txt.HubsSkillsFileLabel), "", false, func(path string) tea.Cmd {
						return a.skillsRun(*hub, tr(txt.HubsSkillsActImport), tuigateway.SkillsHubRunParams{Action: "snapshot-import", Path: path}, reload)
					})
				}},
			}
		})
	}
	spec := cards.ListSpec{
		Columns: []render.Column{{Title: tr(txt.HubsColOn)}, {Title: tr(txt.HubsColSkill), Max: 30}, {Title: tr(txt.HubsColCategory), Max: 18},
			{Title: tr(txt.HubsColSource)}, {Title: tr(txt.HubsColWhatItDoes), Flex: 1}},
		Yield: []int{3, 2},
		Rows: func() [][]string {
			out := make([][]string, len(data.Skills))
			for i, s := range data.Skills {
				mark := "○"
				if s.Enabled {
					mark = "●"
				}
				out[i] = []string{mark, s.Name, s.Category, skillSourceText(s.Source), strings.Join(strings.Fields(s.Description), " ")}
			}
			return out
		},
		Detail: func(i int) []string {
			s, ok := at(i)
			if !ok {
				return nil
			}
			return skillDetail(s)
		},
		Empty: func() string {
			if !loaded {
				return tr(txt.HubsLoading)
			}
			return tr(txt.UserMessagesSkillsNoneInstalled)
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			out := []wm.Hint{}
			if _, ok := at(i); ok {
				out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, wm.Hint{Action: "hub.toggle"}, wm.Hint{Action: "hub.remove", Label: tr(txt.HintsLabelUninstall)})
			}
			return append(out, wm.Hint{Action: "hub.add", Label: tr(txt.HintsLabelInstall)}, wm.Hint{Action: "tui.plugins.update", Label: tr(txt.HubsSkillsEveryLabel)})
		},
		Free: map[string]bool{"hub.add": true, "tui.plugins.update": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use":            open,
			"hub.toggle":         toggle,
			"hub.remove":         uninstall,
			"hub.add":            func(*cards.List, int) tea.Cmd { return cycle("skills", 1) },
			"tui.plugins.update": more,
			"hub.refresh":        func(*cards.List, int) tea.Cmd { return reload() },
		},
		OnFocus: func(*cards.List) tea.Cmd { return reload() },
	}
	list = cards.NewList(a.hubEnv(), pluginPartSpec(a, "skills", cycle, spec))
	return list
}

// Where a skill comes from in plain words
func skillSourceText(source string) string {
	switch source {
	case "hub":
		return tr(txt.HubsSkillsSourceHub)
	case "builtin":
		return tr(txt.HubsSkillsSourceBuiltin)
	}
	return tr(txt.HubsSkillsSourceLocal)
}

// Lines of a skill's detail pane
func skillDetail(s tuigateway.SkillHubRow) []string {
	out := []string{s.Name, strings.Join(strings.Fields(s.Description), " "), tr(txt.HubsPluginsDetailSource, skillSourceText(s.Source))}
	if s.Origin != "" {
		out = append(out, tr(txt.HubsSkillsDetailOrigin, s.Origin, s.Trust))
	}
	if s.Essential {
		out = append(out, tr(txt.HubsSkillsDetailEssential))
	}
	return out
}

// Skill hub: every skill the registries offer, a page at a time, enter installing one
func (a *app) skillHubPart(hub **cards.Hub, cycle func(from string, d int) tea.Cmd) *cards.List {
	var (
		items  []tuigateway.SkillBrowseItem
		list   *cards.List
		loaded bool
		page   int64 = 1
		pages  int64
	)
	reload := func() tea.Cmd {
		params := tuigateway.SkillsManageParams{Action: "browse", Page: page, PageSize: 50}
		return hubCall(a, *hub, tr(txt.HubsSkillsLoadingRegistries), tuigateway.SkillsManage, params, func(r *tuigateway.SkillsManageResult) tea.Cmd {
			items, loaded, pages = r.Items, true, r.TotalPages
			list.Refresh()
			return nil
		})
	}
	at := func(i int) (tuigateway.SkillBrowseItem, bool) {
		if i < 0 || i >= len(items) {
			return tuigateway.SkillBrowseItem{}, false
		}
		return items[i], true
	}
	install := func(_ *cards.List, i int) tea.Cmd {
		it, ok := at(i)
		if !ok {
			return nil
		}
		return a.skillsRun(*hub, tr(txt.HubsSkillsInstallTitle, it.Name), tuigateway.SkillsHubRunParams{Action: "install", Identifier: firstText(it.Identifier, it.Name)}, func() tea.Cmd { return nil })
	}
	spec := cards.ListSpec{
		Columns: []render.Column{{Title: tr(txt.HubsColSkill), Max: 30}, {Title: tr(txt.HubsColSource), Max: 16}, {Title: tr(txt.HubsColTrust)}, {Title: tr(txt.HubsColWhatItDoes), Flex: 1}},
		Yield:   []int{2, 1},
		Rows: func() [][]string {
			out := make([][]string, len(items))
			for i, it := range items {
				out[i] = []string{it.Name, it.Source, it.Trust, strings.Join(strings.Fields(it.Description), " ")}
			}
			return out
		},
		Detail: func(i int) []string {
			it, ok := at(i)
			if !ok {
				return nil
			}
			return []string{it.Name, it.Description, tr(txt.HubsPluginsDetailSource, it.Source), firstText(it.Identifier, it.Name)}
		},
		Status: func() string {
			if pages > 1 {
				return tr(txt.HubsSkillsPage, page, pages) + "  " + pluginPartsLine("hub")
			}
			return pluginPartsLine("hub")
		},
		Empty: func() string {
			if !loaded {
				return tr(txt.HubsLoading)
			}
			return tr(txt.HubsSkillsHubEmpty)
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelInstall)}, {Action: "hub.add", Label: tr(txt.HubsSkillsByName)}, {Action: "tui.plugins.update", Label: tr(txt.HubsSkillsNextPage)}}
		},
		Free: map[string]bool{"hub.add": true, "tui.plugins.update": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use": install,
			"hub.add": func(*cards.List, int) tea.Cmd {
				return a.hubAsk(*hub, tr(txt.HubsSkillsByName), tr(txt.HubsSkillsIdentifierLabel), "", false, func(id string) tea.Cmd {
					return a.skillsRun(*hub, tr(txt.HubsSkillsInstallTitle, id), tuigateway.SkillsHubRunParams{Action: "install", Identifier: id}, func() tea.Cmd { return nil })
				})
			},
			"tui.plugins.update": func(*cards.List, int) tea.Cmd {
				if pages > 0 {
					page = page%pages + 1
				}
				return reload()
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return reload() },
		},
		OnFocus: func(*cards.List) tea.Cmd {
			if loaded {
				return nil
			}
			return reload()
		},
	}
	list = cards.NewList(a.hubEnv(), pluginPartSpec(a, "hub", cycle, spec))
	return list
}

// Bundles: several skills loaded under one slash command, made, changed and removed here
func (a *app) bundlesPart(hub **cards.Hub, cycle func(from string, d int) tea.Cmd) *cards.List {
	var (
		bundles []tuigateway.SkillBundleRow
		list    *cards.List
		loaded  bool
	)
	reload := func() tea.Cmd {
		return hubCall(a, *hub, "", tuigateway.SkillsHubList, tuigateway.SkillsHubParams{}, func(r *tuigateway.SkillsHubResult) tea.Cmd {
			bundles, loaded = r.Bundles, true
			list.Refresh()
			return nil
		})
	}
	at := func(i int) (tuigateway.SkillBundleRow, bool) {
		if i < 0 || i >= len(bundles) {
			return tuigateway.SkillBundleRow{}, false
		}
		return bundles[i], true
	}
	save := func(b tuigateway.SkillBundleRow, replace bool) tea.Cmd {
		spec := cards.FormSpec{Title: tr(txt.HubsBundlesFormTitle), Submit: tr(txt.HubsSettingsSave), Fields: []cards.Field{
			{ID: "name", Label: tr(txt.HubsLoginsNameLabel), Kind: cards.FieldText, Value: b.Name, Required: true, Help: tr(txt.HubsBundlesNameHelp)},
			{ID: "skills", Label: tr(txt.HubsBundlesSkillsLabel), Kind: cards.FieldText, Value: strings.Join(b.Skills, ", "), Required: true, Help: tr(txt.HubsSettingsListHelp)},
			{ID: "description", Label: tr(txt.HubsBundlesDescriptionLabel), Kind: cards.FieldText, Value: b.Description},
			{ID: "instruction", Label: tr(txt.HubsBundlesInstructionLabel), Kind: cards.FieldText, Value: b.Instruction, More: true, Help: tr(txt.HubsBundlesInstructionHelp)},
		}}
		return a.hubForm(*hub, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
			var members []string
			for _, m := range strings.Split(v["skills"], ",") {
				if m = strings.TrimSpace(m); m != "" {
					members = append(members, m)
				}
			}
			params := tuigateway.SkillsBundleSaveParams{Name: strings.TrimSpace(v["name"]), Skills: members, Description: v["description"], Instruction: v["instruction"], Force: replace}
			return hubCall(a, *hub, "", tuigateway.SkillsBundleSave, params, func(*tuigateway.SkillsBundleResult) tea.Cmd {
				return tea.Batch((*hub).Done(tr(txt.HubsBundlesSaved, params.Name)), reload(), a.events.loadSlashCatalog(a.store.ui.Sid))
			})
		})
	}
	spec := cards.ListSpec{
		Columns: []render.Column{{Title: tr(txt.HubsColName), Max: 24}, {Title: tr(txt.HubsBundlesSkillsLabel), Flex: 1}, {Title: tr(txt.HubsColWhatItDoes), Flex: 1}},
		Yield:   []int{2},
		Rows: func() [][]string {
			out := make([][]string, len(bundles))
			for i, b := range bundles {
				out[i] = []string{"/" + b.Name, strings.Join(b.Skills, ", "), b.Description}
			}
			return out
		},
		Detail: func(i int) []string {
			b, ok := at(i)
			if !ok {
				return nil
			}
			return []string{"/" + b.Name, b.Description, strings.Join(b.Skills, ", "), b.Instruction, b.Path}
		},
		Empty: func() string {
			if !loaded {
				return tr(txt.HubsLoading)
			}
			return tr(txt.HubsBundlesEmpty, a.keys.Display("hub.add"))
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if _, ok := at(i); !ok {
				return []wm.Hint{{Action: "hub.add"}}
			}
			return []wm.Hint{{Action: "hub.edit"}, {Action: "hub.add"}, {Action: "hub.remove"}}
		},
		Free: map[string]bool{"hub.add": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.add": func(*cards.List, int) tea.Cmd { return save(tuigateway.SkillBundleRow{}, false) },
			"hub.use": func(_ *cards.List, i int) tea.Cmd { b, _ := at(i); return save(b, true) },
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				b, _ := at(i)
				return save(b, true)
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				b, ok := at(i)
				if !ok {
					return nil
				}
				l.Confirm(tr(txt.HubsBundlesRemoveConfirm, b.Name), func() tea.Cmd {
					return hubCall(a, *hub, "", tuigateway.SkillsBundleDelete, tuigateway.SkillsBundleNameParams{Name: b.Name}, func(*tuigateway.SkillsBundleResult) tea.Cmd {
						return tea.Batch((*hub).Done(tr(txt.HubsBundlesRemoved, b.Name)), reload(), a.events.loadSlashCatalog(a.store.ui.Sid))
					})
				})
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd {
				return hubCall(a, *hub, "", tuigateway.SkillsBundleReload, tuigateway.SkillsHubParams{}, func(r *tuigateway.SkillsBundleReloadResult) tea.Cmd {
					return tea.Batch((*hub).Done(tr(txt.HubsBundlesReloaded, r.Total)), reload())
				})
			},
		},
		OnFocus: func(*cards.List) tea.Cmd { return reload() },
	}
	list = cards.NewList(a.hubEnv(), pluginPartSpec(a, "bundles", cycle, spec))
	return list
}

// Skill sources: GitHub repositories the skill hub searches besides the default registries
func (a *app) skillSourcesPart(hub **cards.Hub, cycle func(from string, d int) tea.Cmd) *cards.List {
	var (
		taps   []tuigateway.SkillTapRow
		list   *cards.List
		loaded bool
	)
	reload := func() tea.Cmd {
		return hubCall(a, *hub, "", tuigateway.SkillsHubList, tuigateway.SkillsHubParams{}, func(r *tuigateway.SkillsHubResult) tea.Cmd {
			taps, loaded = r.Taps, true
			list.Refresh()
			return nil
		})
	}
	spec := cards.ListSpec{
		Columns: []render.Column{{Title: tr(txt.HubsColSource), Flex: 1}, {Title: tr(txt.HubsColWhere), Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, len(taps))
			for i, t := range taps {
				out[i] = []string{t.Name, t.Path}
			}
			return out
		},
		Empty: func() string {
			if !loaded {
				return tr(txt.HubsLoading)
			}
			return tr(txt.HubsSourcesEmpty, a.keys.Display("hub.add"))
		},
		Hints: func(_ *cards.List, i int) []wm.Hint { return []wm.Hint{{Action: "hub.add"}, {Action: "hub.remove"}} },
		Free:  map[string]bool{"hub.add": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.add": func(*cards.List, int) tea.Cmd {
				return a.hubAsk(*hub, tr(txt.HubsSourcesAddTitle), tr(txt.HubsSourcesRepoLabel), "", false, func(repo string) tea.Cmd {
					return a.skillsRun(*hub, tr(txt.HubsSourcesAddTitle), tuigateway.SkillsHubRunParams{Action: "tap-add", Repo: repo}, reload)
				})
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(taps) {
					return nil
				}
				t := taps[i]
				l.Confirm(tr(txt.HubsSourcesRemoveConfirm, t.Name), func() tea.Cmd {
					return a.skillsRun(*hub, tr(txt.HubsSourcesRemoveTitle, t.Name), tuigateway.SkillsHubRunParams{Action: "tap-remove", Repo: t.Name}, reload)
				})
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return reload() },
		},
		OnFocus: func(*cards.List) tea.Cmd { return reload() },
	}
	list = cards.NewList(a.hubEnv(), pluginPartSpec(a, "sources", cycle, spec))
	return list
}

// Trusted projects: folders whose own skills load, this session's folder offered first
func (a *app) trustedPart(hub **cards.Hub, cycle func(from string, d int) tea.Cmd) *cards.List {
	var (
		trusted []string
		list    *cards.List
		loaded  bool
	)
	reload := func() tea.Cmd {
		return hubCall(a, *hub, "", tuigateway.SkillsHubList, tuigateway.SkillsHubParams{}, func(r *tuigateway.SkillsHubResult) tea.Cmd {
			trusted, loaded = r.Trusted, true
			list.Refresh()
			return nil
		})
	}
	spec := cards.ListSpec{
		Columns: []render.Column{{Title: tr(txt.HubsColWhere), Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, len(trusted))
			for i, t := range trusted {
				out[i] = []string{t}
			}
			return out
		},
		Help: func(int) []string { return []string{tr(txt.HubsTrustedHelp)} },
		Empty: func() string {
			if !loaded {
				return tr(txt.HubsLoading)
			}
			return tr(txt.HubsTrustedEmpty, a.keys.Display("hub.add"))
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			return []wm.Hint{{Action: "hub.add", Label: tr(txt.HubsTrustedAdd)}, {Action: "hub.remove"}}
		},
		Free: map[string]bool{"hub.add": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.add": func(*cards.List, int) tea.Cmd {
				return a.hubAsk(*hub, tr(txt.HubsTrustedAdd), tr(txt.HubsTrustedPathLabel), a.store.sessionCwd(), false, func(path string) tea.Cmd {
					return a.skillsRun(*hub, tr(txt.HubsTrustedAdd), tuigateway.SkillsHubRunParams{Action: "trust", Path: path}, reload)
				})
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(trusted) {
					return nil
				}
				path := trusted[i]
				l.Confirm(tr(txt.HubsTrustedRemoveConfirm, path), func() tea.Cmd {
					return a.skillsRun(*hub, tr(txt.HubsTrustedRemoveTitle), tuigateway.SkillsHubRunParams{Action: "untrust", Path: path}, reload)
				})
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return reload() },
		},
		OnFocus: func(*cards.List) tea.Cmd { return reload() },
	}
	list = cards.NewList(a.hubEnv(), pluginPartSpec(a, "trusted", cycle, spec))
	return list
}
