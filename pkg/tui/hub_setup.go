package tui

import (
	"runtime"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// One part of setup: its name on the command line, its words, the settings it asks and the form it opens on the setup hub
type setupSection struct {
	key, title, detail string
	// Registry keys or key prefixes ending in a dot the part asks, for the places a setting is changed
	covers []string
	open   func(a *app, h *cards.Hub) tea.Cmd
}

// Parts of setup in the order the setup hub lists them, those this build leaves out dropped
func setupSections() []setupSection {
	sections := []setupSection{
		{"model", tr(txt.HubsSetupModelTitle), tr(txt.HubsSetupModelDetail), []string{"model.provider", "model.default", "model.base_url"},
			func(a *app, _ *cards.Hub) tea.Cmd { return a.openGetStarted() }},
		{"helpers", tr(txt.HubsSetupHelpersTitle), tr(txt.HubsSetupHelpersDetail), []string{"auxiliary."}, (*app).setupHelpers},
		{"agent", tr(txt.HubsSetupAgentTitle), tr(txt.HubsSetupAgentDetail), setupAgentKeys, (*app).setupAgent},
		{"voice", tr(txt.HubsSetupVoiceTitle), tr(txt.HubsSetupVoiceDetail), []string{"tts."}, (*app).setupVoice},
		{"terminal", tr(txt.HubsSetupTerminalTitle), tr(txt.HubsSetupTerminalDetail), []string{"terminal.backend", "terminal.cwd", "terminal.docker_", "terminal.ssh_",
			"terminal.modal_", "terminal.daytona_", "terminal.singularity_", "terminal.vercel_"}, (*app).setupTerminal},
		{"tools", tr(txt.HubsSetupToolsTitle), tr(txt.HubsSetupToolsDetail), []string{"platform_toolsets", "web.backend"}, (*app).setupTools},
		{"chat-apps", tr(txt.HubsSetupChatTitle), tr(txt.HubsSetupChatDetail), nil, func(a *app, h *cards.Hub) tea.Cmd { return a.pickChatApp(h, nil) }},
		{"quick", tr(txt.HubsSetupQuickTitle), tr(txt.HubsSetupQuickDetail), nil, (*app).setupQuick},
		{"blank", tr(txt.HubsSetupBlankTitle), tr(txt.HubsSetupBlankDetail), setupBlankKeys, (*app).setupBlank},
		{"import", tr(txt.HubsSetupImportTitle), tr(txt.HubsSetupImportDetail), nil, (*app).setupImport},
	}
	return slices.DeleteFunc(sections, func(s setupSection) bool { _, absent := setupSectionRefusal(s.key); return absent })
}

// Opens the setup hub, on the named part when one is named, and the get started screen while no model is set up
func (a *app) openSetup(section string) tea.Cmd {
	section = strings.TrimSpace(section)
	if section == "" && a.store.ui.SetupRequired {
		return a.openGetStarted()
	}
	if line, absent := setupSectionRefusal(section); absent {
		a.sys(line)
		return nil
	}
	sections := setupSections()
	var named *setupSection
	keys := make([]string, len(sections))
	for i := range sections {
		keys[i] = sections[i].key
		if sections[i].key == section {
			named = &sections[i]
		}
	}
	if section != "" && named == nil {
		a.sys(tr(txt.HubsSetupUnknown, section, strings.Join(keys, ", ")))
		return nil
	}
	if named != nil && named.key == "model" {
		return a.openGetStarted()
	}
	var h *cards.Hub
	list := cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleSetup), Glyph: a.toolkit.Glyphs.Brand, Key: "tui:setup", Context: hubPickContext, Primary: "hub.pick",
		Columns: []render.Column{{Title: tr(txt.HubsColPart), Max: 20}, {Title: tr(txt.HubsColWhatItDoes), Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, len(sections))
			for i, s := range sections {
				out[i] = []string{s.title, s.detail}
			}
			return out
		},
		Banner: func() string { return tr(txt.HubsSetupIntro) },
		Help:   func(i int) []string { return []string{sections[i].detail, tr(txt.HubsSetupIntro)} },
		Pick:   true,
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.pick", Label: tr(txt.HintsLabelOpen)}}
		},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.pick": func(_ *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(sections) {
					return nil
				}
				return sections[i].open(a, h)
			},
		},
	})
	h = a.newHub("tui:setup", list)
	cmds := []tea.Cmd{a.wm.ShowOverlay(h)}
	if named != nil {
		for i := range sections {
			if sections[i].key == named.key {
				list.Select(i)
			}
		}
		cmds = append(cmds, named.open(a, h))
	}
	return tea.Batch(cmds...)
}

// Titles of the setup parts that ask a setting, for the places the settings screen names
func setupSectionsWriting(key string) []string {
	var out []string
	for _, s := range setupSections() {
		if slices.ContainsFunc(s.covers, func(c string) bool {
			return c == key || (strings.HasSuffix(c, ".") || strings.HasSuffix(c, "_")) && strings.HasPrefix(key, c)
		}) {
			out = append(out, s.title)
		}
	}
	return out
}

// One question of a setup form: a setting of the registry, a key of the home's .env, a field of the form's own, a button or a line of text
type setupItem struct {
	key  string
	env  string
	note string
	// Field of the form's own and what its answer does
	field  *cards.Field
	answer func(value string) tea.Cmd
	// Button label and what pressing it does
	button string
	help   string
	press  func() tea.Cmd
	more   bool
}

// What a setup form reads before it asks: settings by key or prefix, keys of the home's .env and facts of this computer
type setupReads struct {
	keys, prefixes, envs, facts []string
}

// Setup form being answered: what it read, the form, and its questions worked out from what it read
type setupForm struct {
	a     *app
	h     *cards.Hub
	form  *cards.Form
	title string
	intro []string
	reads func() setupReads
	items func(s *setupForm) []setupItem
	rows  map[string]tuigateway.SettingsRow
	keys  map[string]tuigateway.SetupKey
	facts map[string]bool
	// Read before the form opens, such as the providers of the helper models
	extra func(then func() tea.Cmd) tea.Cmd
	// Runs when the form closes
	closed func() tea.Cmd
	shown  []setupItem
}

// Closes the form and runs what follows it
func (s *setupForm) close(f *cards.Form) tea.Cmd {
	cmd := s.h.Close(f)
	if s.closed != nil {
		return tea.Batch(cmd, s.closed())
	}
	return cmd
}

// Text value of a setting, empty when unset
func (s *setupForm) value(key string) string {
	row, ok := s.rows[key]
	if !ok || row.Value == nil {
		return ""
	}
	return settingEditText(row)
}

// Whether a setting of kind bool is on
func (s *setupForm) on(key string) bool {
	on, _ := s.rows[key].Value.(bool)
	return on
}

// Rows of the registry under a prefix in registry order, the settings the form reads only, advanced ones with a default kept back
func (s *setupForm) under(prefix string) []setupItem {
	var keys []string
	for k := range s.rows {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(x, y string) int { return int(s.rows[x].Order - s.rows[y].Order) })
	out := make([]setupItem, len(keys))
	for i, k := range keys {
		// An advanced row with no default still needs an answer, so it stays in view
		row := s.rows[k]
		out[i] = setupItem{key: k, more: row.Advanced && row.Default != nil && row.Default != ""}
	}
	return out
}

// Opens a setup form on the hub once what it asks is read
func (a *app) openSetupForm(h *cards.Hub, s *setupForm) tea.Cmd {
	s.a, s.h = a, h
	return s.load(func() tea.Cmd {
		spec := cards.FormSpec{Title: s.title, Intro: s.intro, Fields: s.fields(), Submit: tr(txt.HubsSetupClose),
			Changed: func(_ *cards.Form, fd cards.Field) tea.Cmd { return s.answer(fd) },
			Press: func(_ *cards.Form, fd cards.Field) tea.Cmd {
				for _, it := range s.shown {
					if it.button != "" && "button:"+it.button == fd.ID {
						return it.press()
					}
				}
				return nil
			},
			Pick: func(f *cards.Form, field cards.Field) tea.Cmd {
				choices := make([]hubChoice, len(field.Choices))
				for i, c := range field.Choices {
					choices[i] = hubChoice{id: c.Value, title: c.Label, detail: c.Meaning}
				}
				return a.hubPick(h, field.Label, choices, field.Value, func(c hubChoice) tea.Cmd { return f.Commit(field.ID, c.id) })
			},
			Done:   func(f *cards.Form, _ map[string]string) tea.Cmd { return s.close(f) },
			Cancel: func(f *cards.Form) tea.Cmd { return s.close(f) },
		}
		s.form = cards.NewForm(a.hubEnv(), spec)
		return h.Push(s.form)
	})
}

// Reads the settings, keys and facts the form asks about, then goes on
func (s *setupForm) load(then func() tea.Cmd) tea.Cmd {
	a, h := s.a, s.h
	r := s.reads()
	s.rows, s.keys, s.facts = map[string]tuigateway.SettingsRow{}, map[string]tuigateway.SetupKey{}, map[string]bool{}
	steps := []func(next func() tea.Cmd) tea.Cmd{}
	if len(r.keys) > 0 {
		steps = append(steps, func(next func() tea.Cmd) tea.Cmd {
			return hubCall(a, h, tr(txt.HubsLoading), tuigateway.SettingsList, tuigateway.SettingsListParams{Keys: r.keys, Advanced: ptr(true)}, func(res *tuigateway.SettingsListResult) tea.Cmd {
				for _, row := range res.Rows {
					s.rows[row.Key] = row
				}
				return next()
			})
		})
	}
	for _, prefix := range r.prefixes {
		steps = append(steps, func(next func() tea.Cmd) tea.Cmd {
			return hubCall(a, h, tr(txt.HubsLoading), tuigateway.SettingsList, tuigateway.SettingsListParams{Prefix: ptr(prefix), Advanced: ptr(true)}, func(res *tuigateway.SettingsListResult) tea.Cmd {
				for _, row := range res.Rows {
					s.rows[row.Key] = row
				}
				return next()
			})
		})
	}
	if len(r.envs) > 0 {
		steps = append(steps, func(next func() tea.Cmd) tea.Cmd {
			return hubCall(a, h, tr(txt.HubsLoading), tuigateway.SetupKeys, tuigateway.SetupKeysParams{Names: r.envs}, func(res *tuigateway.SetupKeysResult) tea.Cmd {
				for _, k := range res.Keys {
					s.keys[k.Name] = k
				}
				return next()
			})
		})
	}
	if len(r.facts) > 0 {
		steps = append(steps, func(next func() tea.Cmd) tea.Cmd {
			return hubCall(a, h, tr(txt.HubsLoading), tuigateway.SetupFacts, tuigateway.SetupFactsParams{Names: r.facts}, func(res *tuigateway.SetupFactsResult) tea.Cmd {
				s.facts = res.Facts
				return next()
			})
		})
	}
	if s.extra != nil {
		steps = append(steps, s.extra)
	}
	var run func(i int) tea.Cmd
	run = func(i int) tea.Cmd {
		if i == len(steps) {
			h.Quiet()
			return then()
		}
		return steps[i](func() tea.Cmd { return run(i + 1) })
	}
	return run(0)
}

// Reads everything again and shows the questions that now apply
func (s *setupForm) reload() tea.Cmd {
	return s.load(func() tea.Cmd {
		s.form.SetFields(s.fields())
		return nil
	})
}

// Form fields of the questions that apply now
func (s *setupForm) fields() []cards.Field {
	s.shown = s.items(s)
	var out []cards.Field
	for _, it := range s.shown {
		switch {
		case it.field != nil:
			fd := *it.field
			fd.More = fd.More || it.more
			out = append(out, fd)
		case it.key != "":
			row, ok := s.rows[it.key]
			if !ok {
				continue
			}
			fd, ok := settingField(row, "key:"+it.key, row.Label)
			if !ok {
				continue
			}
			fd.Help, fd.More = row.Short, it.more
			// A choice that takes other values too is still picked from its list, the value it holds kept as a choice
			if row.Kind == "enum" && row.Open && len(row.Choices) > 0 {
				fd.Kind, fd.Choices = cards.FieldChoice, nil
				for _, c := range row.Choices {
					fd.Choices = append(fd.Choices, cards.Choice{Value: c.Value, Label: c.Label, Meaning: c.Meaning})
				}
				if fd.Value != "" && !slices.ContainsFunc(fd.Choices, func(c cards.Choice) bool { return c.Value == fd.Value }) {
					fd.Choices = append(fd.Choices, cards.Choice{Value: fd.Value, Label: fd.Value})
				}
			}
			out = append(out, fd)
		case it.env != "":
			k, ok := s.keys[it.env]
			if !ok {
				continue
			}
			kind := cards.FieldText
			if k.Secret {
				kind = cards.FieldSecret
			}
			fd := cards.Field{ID: "env:" + it.env, Label: k.Label, Kind: kind, Help: k.Help, More: it.more}
			if k.URL != "" {
				if fd.Help != "" && !strings.HasSuffix(fd.Help, ".") {
					fd.Help += "."
				}
				fd.Help = strings.TrimSpace(fd.Help + " " + tr(txt.HubsSetupKeyURL, k.URL))
			}
			if k.Set {
				fd.Placeholder = tr(txt.HubsSetupKeySet)
			}
			out = append(out, fd)
		case it.button != "":
			out = append(out, cards.Field{ID: "button:" + it.button, Label: it.button, Kind: cards.FieldButton, Help: it.help, More: it.more})
		case it.note != "":
			out = append(out, cards.Field{ID: "note:" + it.note, Label: it.note, Kind: cards.FieldNote, More: it.more})
		}
	}
	return out
}

// Writes one answer the moment it is given: a setting through settings.set, a key into the home's .env, or the form's own field
func (s *setupForm) answer(fd cards.Field) tea.Cmd {
	a, h := s.a, s.h
	switch kind, name, _ := strings.Cut(fd.ID, ":"); kind {
	case "key":
		row := s.rows[name]
		value, err := settingValue(row.Kind, fd.Value)
		if err != nil {
			return h.Fail(err.Error())
		}
		params := tuigateway.SettingsSetParams{Key: name, Value: value, Scope: ptr("global")}
		return hubCall(a, h, tr(txt.HubsSetupSaving), tuigateway.SettingsSet, params, func(r *tuigateway.SettingsWriteResult) tea.Cmd {
			s.rows[name] = r.Row
			s.form.SetFields(s.fields())
			return h.Done(settingAppliedText(row, r))
		})
	case "env":
		params := tuigateway.SetupSaveKeyParams{Name: name, Value: strings.TrimSpace(fd.Value)}
		if params.Value == "" && !s.keys[name].Set {
			return nil
		}
		return hubCall(a, h, tr(txt.HubsSetupSaving), tuigateway.SetupSaveKey, params, func(r *tuigateway.SetupSaveKeyResult) tea.Cmd {
			s.keys[name] = r.Key
			s.form.SetFields(s.fields())
			if r.Key.Set {
				return h.Done(tr(txt.HubsSetupKeySaved, r.Key.Label))
			}
			return h.Done(tr(txt.HubsSetupKeyRemoved, r.Key.Label))
		})
	}
	for _, it := range s.shown {
		if it.field != nil && it.field.ID == fd.ID && it.answer != nil {
			return it.answer(fd.Value)
		}
	}
	return nil
}

// Runs a setup action of the background service on a page of the hub, its output shown as it arrives, then goes on with how it ended
func (a *app) setupAction(h *cards.Hub, title string, action tuigateway.SetupAction, params map[string]string, then func(ok bool) tea.Cmd) tea.Cmd {
	var lines []string
	live, ended, good := "", false, false
	t := a.toolkit
	page := cards.NewPage(a.hubEnv(), cards.PageSpec{Title: title, Key: "hub:setup:action", Follow: true,
		Lines: func(int) []string {
			out := slices.Clone(lines)
			switch {
			case live != "" && !ended:
				out = append(out, live)
			case ended && good:
				out = append(out, "", render.Good(tr(txt.HubsRunDone), t))
			case ended:
				out = append(out, "", render.Bad(tr(txt.HubsSetupActionFailed), t))
			}
			return out
		}})
	var poll func(job string) tea.Cmd
	poll = func(job string) tea.Cmd {
		params := tuigateway.SetupActionPollParams{Job: job, After: int64(len(lines)), Live: ptr(live)}
		return hubCall(a, h, "", tuigateway.SetupActionPoll, params, func(r *tuigateway.SetupActionPollResult) tea.Cmd {
			lines, live = append(lines, r.Lines...), r.Live
			if !r.Done {
				return poll(job)
			}
			ended, good = true, r.Ok
			h.Quiet()
			if !r.Ok {
				return tea.Batch(h.Fail(tr(txt.HubsSetupActionFailed)), then(false))
			}
			return tea.Batch(h.Done(tr(txt.HubsSetupActionDone, title)), then(true))
		})
	}
	start := hubCall(a, h, tr(txt.HubsSetupActionBusy, title), tuigateway.SetupActionStart, tuigateway.SetupActionStartParams{Action: action, Params: params},
		func(r *tuigateway.SetupActionStartResult) tea.Cmd { return poll(r.Job) })
	return tea.Batch(h.Push(page), start)
}

// Settings the agent part asks
var setupAgentKeys = []string{"agent.max_turns", "display.tool_progress", "compression.enabled", "compression.threshold"}

// Agent: how many steps a turn may take, the tool progress shown and when a long conversation is shortened
func (a *app) setupAgent(h *cards.Hub) tea.Cmd {
	return a.openSetupForm(h, &setupForm{title: tr(txt.HubsSetupAgentTitle), intro: []string{tr(txt.HubsSetupAgentDetail)},
		reads: func() setupReads { return setupReads{keys: setupAgentKeys} },
		items: func(s *setupForm) []setupItem {
			out := []setupItem{{key: "agent.max_turns"}, {key: "display.tool_progress"}, {key: "compression.enabled"}}
			if s.on("compression.enabled") {
				out = append(out, setupItem{key: "compression.threshold"})
			}
			return out
		}})
}

// Speech engines that take a key, with the keys of the home's .env that give one, a pasted key going to the first
var voiceKeys = map[string][]string{
	"elevenlabs": {"ELEVENLABS_API_KEY"},
	"openai":     {"VOICE_TOOLS_OPENAI_KEY", "OPENAI_API_KEY"},
	"minimax":    {"MINIMAX_API_KEY"},
	"mistral":    {"MISTRAL_API_KEY"},
	"gemini":     {"GEMINI_API_KEY", "GOOGLE_API_KEY"},
	"xai":        {"XAI_API_KEY"},
}

// Speech engines that run on this computer, installed into icebath's own Python on request
var voiceLocal = map[string]string{"neutts": "NeuTTS", "kittentts": "KittenTTS"}

// Voice: the speech engine, its key or sign in, its install when it runs here, and its own settings
func (a *app) setupVoice(h *cards.Hub) tea.Cmd {
	var s *setupForm
	s = &setupForm{title: tr(txt.HubsSetupVoiceTitle), intro: []string{tr(txt.HubsSetupVoiceDetail)},
		reads: func() setupReads {
			r := setupReads{keys: []string{"tts.provider"}, prefixes: []string{"tts"}, facts: []string{"xai.oauth", "binary:espeak-ng", "binary:espeak"}}
			for _, names := range voiceKeys {
				r.envs = append(r.envs, names...)
			}
			for engine := range voiceLocal {
				r.facts = append(r.facts, "extra:"+engine, "module:"+engine)
			}
			return r
		},
		items: func(s *setupForm) []setupItem {
			engine := s.value("tts.provider")
			out := []setupItem{{key: "tts.provider"}}
			if names, keyed := voiceKeys[engine]; keyed {
				held := slices.ContainsFunc(names, func(n string) bool { return s.keys[n].Set }) || engine == "xai" && s.facts["xai.oauth"]
				if engine == "xai" && !held {
					out = append(out, setupItem{button: tr(txt.HubsSetupVoiceSignIn), help: tr(txt.HubsSetupVoiceSignInHelp), press: func() tea.Cmd {
						return a.setupAction(h, tr(txt.HubsSetupVoiceSignIn), "xai.oauth.login", nil, func(bool) tea.Cmd { return s.reload() })
					}})
				}
				if !held || s.keys[names[0]].Set {
					out = append(out, setupItem{env: names[0]})
				}
			}
			if name, local := voiceLocal[engine]; local {
				switch {
				case !s.facts["extra:"+engine]:
					out = append(out, setupItem{note: tr(txt.HubsSetupVoiceUnsupported, name)})
				case !s.facts["module:"+engine]:
					out = append(out, setupItem{button: tr(txt.HubsSetupVoiceInstall, name), help: tr(txt.HubsSetupVoiceInstallHelp, name), press: func() tea.Cmd {
						return a.setupAction(h, tr(txt.HubsSetupVoiceInstall, name), tuigateway.SetupAction("tts.install."+engine), nil, func(bool) tea.Cmd { return s.reload() })
					}})
				}
				if engine == "neutts" && !s.facts["binary:espeak-ng"] && !s.facts["binary:espeak"] {
					out = append(out, setupItem{note: tr(txt.HubsSetupVoiceEspeak, strings.Join(espeakInstall(), " "))})
				}
			}
			return append(out, s.under("tts."+engine+".")...)
		}}
	return a.openSetupForm(h, s)
}

// Command that installs espeak-ng from the system's packages
func espeakInstall() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"brew", "install", "espeak-ng"}
	case "windows":
		return []string{"choco", "install", "espeak-ng", "-y"}
	}
	return []string{"sudo", "apt", "install", "-y", "espeak-ng"}
}

// Kinds of terminal backend whose own settings start with the backend's name
var terminalBackendKeys = []string{"docker", "ssh", "modal", "daytona", "singularity", "vercel"}

// Terminal: where commands run, that place's own settings, the folder commands start in, and a connection test for a server
func (a *app) setupTerminal(h *cards.Hub) tea.Cmd {
	var s *setupForm
	s = &setupForm{title: tr(txt.HubsSetupTerminalTitle), intro: []string{tr(txt.HubsSetupTerminalDetail)},
		reads: func() setupReads {
			return setupReads{keys: []string{"terminal.backend", "terminal.cwd"}, prefixes: []string{"terminal"}, facts: []string{"binary:docker"}}
		},
		items: func(s *setupForm) []setupItem {
			backend := s.value("terminal.backend")
			out := []setupItem{{key: "terminal.backend"}}
			if backend == "docker" && !s.facts["binary:docker"] {
				out = append(out, setupItem{note: tr(txt.HubsSetupTerminalNoDocker)})
			}
			switch {
			case slices.Contains(terminalBackendKeys, backend):
				out = append(out, s.under("terminal."+backend+"_")...)
			case backend != "" && backend != "local":
				out = append(out, setupItem{button: tr(txt.HubsSetupTerminalPlugin, backend), help: tr(txt.HubsSetupTerminalPluginHelp, backend), press: func() tea.Cmd {
					params := map[string]string{"backend": backend, "cwd": s.value("terminal.cwd")}
					return a.setupAction(h, tr(txt.HubsSetupTerminalPlugin, backend), "terminal.backend.plugin", params, func(bool) tea.Cmd { return s.reload() })
				}})
			}
			out = append(out, setupItem{key: "terminal.cwd"})
			if backend == "ssh" {
				out = append(out, setupItem{button: tr(txt.HubsSetupTerminalTest), help: tr(txt.HubsSetupTerminalTestHelp), press: func() tea.Cmd {
					params := map[string]string{"host": s.value("terminal.ssh_host"), "user": s.value("terminal.ssh_user"), "port": s.value("terminal.ssh_port"), "key": s.value("terminal.ssh_key")}
					return a.setupAction(h, tr(txt.HubsSetupTerminalTest), "terminal.ssh.test", params, func(bool) tea.Cmd { return nil })
				}})
			}
			return out
		}}
	return a.openSetupForm(h, s)
}

// Tool group switches of a setup form from the catalog, each turned on or off through the same writer as /tools
func (a *app) toolsetItems(s *setupForm, groups *[]tuigateway.SetupToolset, withKeys bool) []setupItem {
	var out []setupItem
	for _, g := range *groups {
		name := g.Name
		fd := cards.Field{ID: "toolset:" + name, Label: g.Label, Kind: cards.FieldBool, Value: boolText(g.On), Help: g.Detail}
		out = append(out, setupItem{field: &fd, answer: func(value string) tea.Cmd {
			action := tuigateway.ToolsAction("disable")
			if value == "true" {
				action = "enable"
			}
			return hubCall(a, s.h, tr(txt.HubsSetupSaving), tuigateway.ToolsConfigure, tuigateway.ToolsConfigureParams{Action: action, Names: []string{name}}, func(*tuigateway.ToolsConfigureResult) tea.Cmd {
				return tea.Batch(s.h.Done(tr(txt.HubsSetupToolsetSaved, fd.Label, onOff(value == "true"))), s.reload())
			})
		}})
		if withKeys && g.On && !g.Ready && len(g.Keys) > 0 {
			out = append(out, setupItem{note: tr(txt.HubsSetupToolsNeedsKey, g.Label)})
			for _, k := range g.Keys {
				out = append(out, setupItem{env: k})
			}
		}
	}
	return out
}

// Reads the tool groups of the terminal into groups, for a form's extra read
func (a *app) readToolsets(s *setupForm, groups *[]tuigateway.SetupToolset) func(then func() tea.Cmd) tea.Cmd {
	return func(then func() tea.Cmd) tea.Cmd {
		return hubCall(a, s.h, tr(txt.HubsLoading), tuigateway.SetupToolsets, tuigateway.SetupToolsetsParams{}, func(r *tuigateway.SetupToolsetsResult) tea.Cmd {
			*groups = r.Toolsets
			var names []string
			for _, g := range r.Toolsets {
				names = append(names, g.Keys...)
			}
			if len(names) == 0 {
				return then()
			}
			return hubCall(a, s.h, "", tuigateway.SetupKeys, tuigateway.SetupKeysParams{Names: names}, func(k *tuigateway.SetupKeysResult) tea.Cmd {
				for _, key := range k.Keys {
					s.keys[key.Name] = key
				}
				return then()
			})
		})
	}
}

// Tools: the tool groups the agent may use in this terminal, the keys a group that is on still needs, and the web search engine
func (a *app) setupTools(h *cards.Hub) tea.Cmd {
	var groups []tuigateway.SetupToolset
	s := &setupForm{title: tr(txt.HubsSetupToolsTitle), intro: []string{tr(txt.HubsSetupToolsDetail)},
		reads: func() setupReads { return setupReads{keys: []string{"web.backend"}} },
		items: func(s *setupForm) []setupItem {
			return append(a.toolsetItems(s, &groups, true), setupItem{key: "web.backend"})
		}}
	s.extra = a.readToolsets(s, &groups)
	return a.openSetupForm(h, s)
}

// Settings blank slate turns off besides the tool groups
var setupBlankKeys = []string{"compression.enabled", "memory.memory_enabled", "memory.user_profile_enabled", "checkpoints.enabled"}

// Tool groups blank slate keeps on
var setupBlankKept = []string{"file", "terminal", "vision", "skills"}

// Blank slate: every tool group and the background helpers as switches, and one button that turns off all but the basics
func (a *app) setupBlank(h *cards.Hub) tea.Cmd {
	var groups []tuigateway.SetupToolset
	var s *setupForm
	s = &setupForm{title: tr(txt.HubsSetupBlankTitle), intro: []string{tr(txt.HubsSetupBlankIntro, strings.Join(setupBlankKept, ", "))},
		reads: func() setupReads { return setupReads{keys: setupBlankKeys} },
		items: func(s *setupForm) []setupItem {
			out := []setupItem{{button: tr(txt.HubsSetupBlankApply), help: tr(txt.HubsSetupBlankApplyHelp), press: func() tea.Cmd { return a.applyBlankSlate(s, groups) }}}
			for _, k := range setupBlankKeys {
				out = append(out, setupItem{key: k})
			}
			out = append(out, a.toolsetItems(s, &groups, false)...)
			return append(out, setupItem{button: tr(txt.HubsSetupBlankSkills), help: tr(txt.HubsSetupBlankSkillsHelp), press: func() tea.Cmd {
				return a.hubRunCLI(h, tr(txt.HubsSetupBlankSkills), []string{"skills", "opt-out"}, func(*cliRun) tea.Cmd { return nil })
			}})
		}}
	s.extra = a.readToolsets(s, &groups)
	return a.openSetupForm(h, s)
}

// Turns off every tool group but the basics and the background helpers, one write each, then shows the form again
func (a *app) applyBlankSlate(s *setupForm, groups []tuigateway.SetupToolset) tea.Cmd {
	var off []string
	for _, g := range groups {
		if g.On && !slices.Contains(setupBlankKept, g.Name) {
			off = append(off, g.Name)
		}
	}
	writes := []func(next func() tea.Cmd) tea.Cmd{
		func(next func() tea.Cmd) tea.Cmd {
			return hubCall(a, s.h, tr(txt.HubsSetupSaving), tuigateway.ToolsConfigure, tuigateway.ToolsConfigureParams{Action: "enable", Names: setupBlankKept}, func(*tuigateway.ToolsConfigureResult) tea.Cmd { return next() })
		},
	}
	if len(off) > 0 {
		writes = append(writes, func(next func() tea.Cmd) tea.Cmd {
			return hubCall(a, s.h, tr(txt.HubsSetupSaving), tuigateway.ToolsConfigure, tuigateway.ToolsConfigureParams{Action: "disable", Names: off}, func(*tuigateway.ToolsConfigureResult) tea.Cmd { return next() })
		})
	}
	for _, key := range setupBlankKeys {
		writes = append(writes, func(next func() tea.Cmd) tea.Cmd {
			return hubCall(a, s.h, tr(txt.HubsSetupSaving), tuigateway.SettingsSet, tuigateway.SettingsSetParams{Key: key, Value: false, Scope: ptr("global")}, func(*tuigateway.SettingsWriteResult) tea.Cmd { return next() })
		})
	}
	var run func(i int) tea.Cmd
	run = func(i int) tea.Cmd {
		if i == len(writes) {
			return tea.Batch(s.h.Done(tr(txt.HubsSetupBlankApplied)), s.reload())
		}
		return writes[i](func() tea.Cmd { return run(i + 1) })
	}
	return run(0)
}

// Quick setup: every key of the home's .env still empty, required ones first, and the model when none is picked
func (a *app) setupQuick(h *cards.Hub) tea.Cmd {
	var missing []string
	s := &setupForm{title: tr(txt.HubsSetupQuickTitle), intro: []string{tr(txt.HubsSetupQuickIntro)},
		reads: func() setupReads { return setupReads{} },
		items: func(s *setupForm) []setupItem {
			var out []setupItem
			if a.store.ui.SetupRequired {
				out = append(out, setupItem{button: tr(txt.HubsSetupQuickModel), press: func() tea.Cmd { return a.openGetStarted() }})
			}
			for _, name := range missing {
				k := s.keys[name]
				out = append(out, setupItem{env: name, more: !k.Required && k.Category != "tool"})
			}
			if len(out) == 0 {
				out = append(out, setupItem{note: tr(txt.HubsSetupQuickNone)})
			}
			return out
		}}
	s.extra = func(then func() tea.Cmd) tea.Cmd {
		return hubCall(a, h, tr(txt.HubsLoading), tuigateway.SetupKeys, tuigateway.SetupKeysParams{Missing: true}, func(r *tuigateway.SetupKeysResult) tea.Cmd {
			if missing == nil {
				for _, k := range r.Keys {
					if k.Required || k.Category == "tool" || k.Category == "messaging" {
						missing = append(missing, k.Name)
					}
				}
				slices.SortStableFunc(missing, func(x, y string) int { return boolRank(r, y) - boolRank(r, x) })
			}
			for _, k := range r.Keys {
				s.keys[k.Name] = k
			}
			return then()
		})
	}
	return a.openSetupForm(h, s)
}

// Order of a key in quick setup: required keys first, then tool keys, then the rest
func boolRank(r *tuigateway.SetupKeysResult, name string) int {
	for _, k := range r.Keys {
		if k.Name == name {
			switch {
			case k.Required:
				return 2
			case k.Category == "tool":
				return 1
			}
		}
	}
	return 0
}

// Import: an OpenClaw setup, a session of another tool, or a backup of a home, each through its icebath command on the hub
func (a *app) setupImport(h *cards.Hub) tea.Cmd {
	s := &setupForm{title: tr(txt.HubsSetupImportTitle), intro: []string{tr(txt.HubsSetupImportDetail)},
		reads: func() setupReads { return setupReads{} },
		items: func(*setupForm) []setupItem {
			command := func(label, help, path string) setupItem {
				return setupItem{button: label, help: help, press: func() tea.Cmd { return a.hubCommand(h, path) }}
			}
			return []setupItem{
				command(tr(txt.HubsSetupImportOpenclaw), tr(txt.HubsSetupImportOpenclawHelp), "migrate openclaw"),
				command(tr(txt.HubsSetupImportSession), tr(txt.HubsSetupImportSessionHelp), "sessions import"),
				command(tr(txt.HubsSetupImportBackup), tr(txt.HubsSetupImportBackupHelp), "import"),
			}
		}}
	return a.openSetupForm(h, s)
}

// Opens the form of an icebath command on the hub, built from its flags
func (a *app) hubCommand(h *cards.Hub, path string) tea.Cmd {
	return hubCall(a, h, tr(txt.RunCommandLoading), tuigateway.CommandsTree, tuigateway.CommandsTreeParams{}, func(r *tuigateway.CommandsTreeResult) tea.Cmd {
		for _, c := range r.Commands {
			if c.Path == path {
				return a.commandForm(h, c)
			}
		}
		return h.Fail(tr(txt.HubsSetupNoCommand, path))
	})
}

// Helper model jobs shown before the rest
var setupHelperFirst = []string{"vision", "compression", "title_generation", "approval"}

// Helpers: the provider and model of each side job, picked from the providers that are signed in
func (a *app) setupHelpers(h *cards.Hub) tea.Cmd {
	var providers []tuigateway.ModelOptionProvider
	s := &setupForm{title: tr(txt.HubsSetupHelpersTitle), intro: []string{tr(txt.HubsSetupHelpersIntro)},
		reads: func() setupReads { return setupReads{prefixes: []string{"auxiliary"}} },
		items: func(s *setupForm) []setupItem {
			var jobs []string
			for k := range s.rows {
				if job, ok := strings.CutSuffix(strings.TrimPrefix(k, "auxiliary."), ".provider"); ok && !strings.Contains(job, ".") {
					jobs = append(jobs, job)
				}
			}
			slices.SortFunc(jobs, func(x, y string) int {
				ix, iy := slices.Index(setupHelperFirst, x), slices.Index(setupHelperFirst, y)
				if (ix < 0) != (iy < 0) {
					return map[bool]int{true: 1, false: -1}[ix < 0]
				}
				if ix != iy {
					return ix - iy
				}
				return strings.Compare(x, y)
			})
			var out []setupItem
			for _, job := range jobs {
				pkey, mkey := "auxiliary."+job+".provider", "auxiliary."+job+".model"
				more := !slices.Contains(setupHelperFirst, job)
				current := s.value(pkey)
				pf := cards.Field{ID: "key:" + pkey, Label: s.rows[pkey].Label, Kind: cards.FieldChoice, Value: firstText(current, "auto"), Help: s.rows[pkey].Short,
					Choices: []cards.Choice{{Value: "auto", Label: tr(txt.HubsSetupHelpersAuto), Meaning: tr(txt.HubsSetupHelpersAutoMeaning)}}}
				var models []string
				for _, p := range providers {
					if p.Unauthenticated() {
						continue
					}
					pf.Choices = append(pf.Choices, cards.Choice{Value: p.Slug, Label: providerName(p)})
					if p.Slug == current {
						models = p.Models
					}
				}
				if current != "" && current != "auto" && !slices.ContainsFunc(pf.Choices, func(c cards.Choice) bool { return c.Value == current }) {
					pf.Choices = append(pf.Choices, cards.Choice{Value: current, Label: current})
				}
				out = append(out, setupItem{field: &pf, more: more})
				if current == "" || current == "auto" {
					continue
				}
				model := s.value(mkey)
				mf := cards.Field{ID: "key:" + mkey, Label: s.rows[mkey].Label, Kind: cards.FieldChoice, Value: model, Help: s.rows[mkey].Short, Placeholder: tr(txt.HubsSetupHelpersModelPick)}
				for _, m := range models {
					mf.Choices = append(mf.Choices, cards.Choice{Value: m, Label: m})
				}
				if model != "" && !slices.Contains(models, model) {
					mf.Choices = append(mf.Choices, cards.Choice{Value: model, Label: model})
				}
				if len(mf.Choices) == 0 {
					mf.Kind = cards.FieldText
				}
				out = append(out, setupItem{field: &mf, more: more})
			}
			return out
		}}
	s.extra = func(then func() tea.Cmd) tea.Cmd {
		return hubCall(a, h, tr(txt.HubsLoading), tuigateway.ModelOptions, tuigateway.ModelOptionsParams{SessionID: a.store.ui.Sid}, func(r *tuigateway.ModelOptionsResult) tea.Cmd {
			providers = r.Providers
			return then()
		})
	}
	return a.openSetupForm(h, s)
}

// Picks a chat app on the hub and opens its form, or the one named
func (a *app) pickChatApp(h *cards.Hub, then func() tea.Cmd) tea.Cmd {
	return hubCall(a, h, tr(txt.HubsLoading), tuigateway.SetupKeys, tuigateway.SetupKeysParams{Category: "messaging", Advanced: true}, func(r *tuigateway.SetupKeysResult) tea.Cmd {
		var apps []string
		set := map[string]bool{}
		for _, k := range r.Keys {
			if k.App == "" {
				continue
			}
			if !slices.Contains(apps, k.App) {
				apps = append(apps, k.App)
			}
			set[k.App] = set[k.App] || k.Set
		}
		if len(apps) == 0 {
			return h.Fail(tr(txt.HubsSetupChatNone))
		}
		choices := make([]hubChoice, len(apps))
		for i, app := range apps {
			detail := ""
			if set[app] {
				detail = tr(txt.HubsSetupChatHasKeys)
			}
			choices[i] = hubChoice{id: app, title: app, detail: detail}
		}
		return a.hubPick(h, tr(txt.HubsSetupChatPick), choices, "", func(c hubChoice) tea.Cmd { return a.setupChatApp(h, c.id, r.Keys, then) })
	})
}

// One chat app's keys, each kept the moment it is typed; closing the form reads the chat apps screen again when one is open
func (a *app) setupChatApp(h *cards.Hub, app string, all []tuigateway.SetupKey, then func() tea.Cmd) tea.Cmd {
	var names []string
	for _, k := range all {
		if k.App == app {
			names = append(names, k.Name)
		}
	}
	s := &setupForm{title: tr(txt.HubsSetupChatFormTitle, app), intro: []string{tr(txt.HubsSetupChatIntro)},
		reads: func() setupReads { return setupReads{envs: names, facts: []string{"extra:" + app}} },
		items: func(s *setupForm) []setupItem {
			out := make([]setupItem, 0, len(names)+1)
			for _, n := range names {
				out = append(out, setupItem{env: n, more: s.keys[n].Advanced})
			}
			if s.facts["extra:"+app] {
				out = append(out, setupItem{button: tr(txt.HubsSetupChatInstall, app), help: tr(txt.HubsSetupChatInstallHelp, app), press: func() tea.Cmd {
					return a.hubRunCLI(h, tr(txt.HubsSetupChatInstall, app), []string{"pm", "install", "--extra", app}, func(*cliRun) tea.Cmd { return nil })
				}})
			}
			return out
		}, closed: then}
	return a.openSetupForm(h, s)
}

// Text of a switch value
func boolText(on bool) string {
	if on {
		return "true"
	}
	return "false"
}
