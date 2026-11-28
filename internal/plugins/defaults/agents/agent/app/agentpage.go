package app

import (
	"cmp"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of the Agent area, and the jobs its argument opens over the page: the model, the reasoning effort, what enter does while the agent
// works, the personality, and the pages of the model shortcuts, of the prompt, of your personalities and of tool loops; the page of one
// shortcut or one personality by the name after its prefix
const (
	agentScreen      = "agent"
	jobModel         = "model"
	jobReasoning     = "reasoning"
	jobBusy          = "busy"
	jobPersonality   = "personality"
	jobShortcuts     = "shortcuts"
	jobPrompt        = "prompt"
	jobPersonalities = "personalities"
	jobLoops         = "loops"
	jobShortcut      = "shortcut:"
	jobOnePersona    = "personality:"
)

// Headings of the Agent page: what the chat the menu was opened from keeps for itself, and what new chats start with
const (
	thisChat = "This chat"
	newChats = "New chats"
)

// The Agent area's page
func (d *desk) agentArea() tui.Screen {
	return tui.Screen{
		ID:      agentScreen,
		Group:   tui.GroupModels,
		Order:   10,
		Title:   "Agent",
		Short:   "Choose the model and how the agent works",
		Area:    agentconfig.Key,
		Pickers: map[string]tui.Picker{auxiliaryconfig.PickEffort: d.effortPicker()},
		Pages:   d.agentPages,
		Open:    func(kit *tui.Kit, argument string) tea.Cmd { return d.openAgent(kit, argument) },
	}
}

type agentPage struct {
	d       *desk
	kit     *tui.Kit
	hub     *cards.Hub
	options *configapp.Rows
	// Profile the page shows, the one in use when empty
	profile string
	// Session of the chat the menu was opened from, and what it keeps for itself once read, nil for none or one of another profile
	chat string
	held *agentv1.ChatResponse
	list *cards.List
	// The settings were read, or why they could not be
	loaded bool
	err    error
}

// Opens the page: what the area is, This chat when the menu was opened from a chat that has a session, New chats, then the pages inside
// it; the job the argument names opens over it once the page is read
func (d *desk) openAgent(kit *tui.Kit, argument string) tea.Cmd {
	name, job := pageArgument(argument)
	p := &agentPage{d: d, kit: kit, profile: name}
	if from, ok := kit.Focused().(*chat); ok {
		p.chat = from.id
	}
	p.list = pageList(kit, pageSpec{
		title:   "Agent",
		key:     agentScreen,
		intro:   p.intro,
		rows:    p.rows,
		empty:   func() string { return tui.Waiting(p.loaded, p.err, "") },
		refresh: func() tea.Cmd { return p.load("") },
	})
	hub, cmd := kit.Show(agentScreen, p.list)
	p.hub = hub
	p.options = configapp.NewRows(kit, hub, d.settings, name)
	p.options.Watch(p.list)
	return tea.Batch(cmd, p.load(job))
}

// What the area is, and in the warm color what is wrong with the chat's model
func (p *agentPage) intro() []string {
	lines := []string{"The model that answers you and how it works."}
	if p.held != nil && p.held.GetModel() == "" {
		lines = append(lines, render.Warm("This chat has no model it can use yet. Pick one under Model.", p.kit.Env.Theme))
	}
	return lines
}

// Reads what the chat keeps for itself, then the settings, then opens the job. The page is made of the settings, so why they can't be
// read is what it says in place of its rows
func (p *agentPage) load(job string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		held := p.read()
		_, err := p.d.settings.Get(p.kit.Env.Ctx, &configv1.GetRequest{Key: agentconfig.Model, Profile: p.profile})
		return func() tea.Cmd {
			p.held, p.err = held, err
			if err != nil {
				p.loaded = false
				p.list.Refresh()
				return nil
			}
			return p.options.Load(func() tea.Cmd {
				p.loaded = true
				p.list.Refresh()
				return p.job(job)
			})
		}
	})
}

// What the chat the menu was opened from keeps for itself, nil for none or for a session of a profile the page does not show
func (p *agentPage) read() *agentv1.ChatResponse {
	if p.chat == "" {
		return nil
	}
	held, err := p.d.agents.Chat(p.kit.Env.Ctx, &agentv1.ChatRequest{SessionId: p.chat, Profile: p.profile})
	if err != nil || !held.GetShown() {
		return nil
	}
	return held
}

// Opens the job over the page, the menu closing once it is done when the person came straight to it
func (p *agentPage) job(job string) tea.Cmd {
	var open func() tea.Cmd
	// A setting this page changes with a view of its own names that view, as Settings opens the page for it
	switch job {
	case agentconfig.Model, agentconfig.Provider:
		p.hub.Direct()
		return p.change(agentconfig.Model, nil)
	case agentconfig.Aliases:
		job = jobShortcuts
	case agentconfig.Personalities:
		job = jobPersonalities
	}
	switch job {
	case jobModel:
		open = func() tea.Cmd { return p.change(agentconfig.Model, p.chooseModel) }
	case jobReasoning:
		open = func() tea.Cmd { return p.change(agentconfig.ReasoningEffort, p.chooseEffort) }
	case jobBusy:
		open = func() tea.Cmd { return p.change(agentconfig.BusyInputMode, p.chooseBusy) }
	case jobPersonality:
		open = func() tea.Cmd { return p.change(agentconfig.Personality, nil) }
	case jobShortcuts:
		return p.shortcuts("")
	case jobPrompt:
		return p.prompt()
	case jobPersonalities:
		return tea.Batch(p.prompt(), p.personalities(""))
	case jobLoops:
		return p.loops()
	default:
		if name, ok := strings.CutPrefix(job, jobShortcut); ok {
			return p.shortcuts(name)
		}
		if name, ok := strings.CutPrefix(job, jobOnePersona); ok {
			return tea.Batch(p.prompt(), p.personalities(name))
		}
		return nil
	}
	p.hub.Direct()
	return open()
}

// The control of This chat when the page shows it, else the row of New chats
func (p *agentPage) change(key string, chat func() tea.Cmd) tea.Cmd {
	if p.held != nil && chat != nil {
		return chat()
	}
	if setting := p.options.Setting(key); setting != nil {
		return p.options.Change(setting)
	}
	return nil
}

// Rows of the page once the settings were read
func (p *agentPage) rows() []pageRow {
	if !p.loaded {
		return nil
	}
	var rows []pageRow
	if p.held != nil {
		rows = append(rows, p.chatRows(p.held)...)
	}
	everyday, _ := p.options.Own(agentconfig.Key)
	rows = append(rows, optionRows(p.options, newChats, p.options.Setting(agentconfig.Model))...)
	rows = append(rows, optionRows(p.options, newChats, everyday...)...)
	rows = append(rows, optionRows(p.options, newChats, p.options.Setting(agentconfig.Personality))...)
	shortcuts := openRow(p.kit, "Model shortcuts", countOf(p.options.Setting(agentconfig.Aliases), "shortcut"),
		"Short names you can use in place of a model's full name.", func() tea.Cmd { return p.shortcuts("") })
	shortcuts.group = newChats
	rows = append(rows, shortcuts)
	if info := p.options.Info(agentconfig.PromptKey); info != nil {
		everyday, advanced := p.options.Own(agentconfig.PromptKey, agentconfig.Personality)
		rows = append(rows, openRow(p.kit, info.GetTitle(), inside(append(append(everyday, advanced...), p.options.Setting(agentconfig.Personalities))),
			info.GetDescription()+".", p.prompt))
	}
	if info := p.options.Info(agentconfig.LoopKey); info != nil {
		everyday, advanced := p.options.Own(agentconfig.LoopKey)
		rows = append(rows, openRow(p.kit, info.GetTitle(), inside(append(everyday, advanced...)), info.GetDescription()+".", p.loops))
	}
	cells, open, ok := p.options.OwnAdvanced(agentconfig.Key)
	return append(rows, advancedRow(cells, open, ok, "Settings of the agent that few people change.")...)
}

// What a row that opens a page says is inside: how many settings, and how many of them were changed
func inside(settings []*configv1.Setting) string {
	said, changed := render.Count(len(settings), "setting"), 0
	for _, setting := range settings {
		if setting.GetSource() != configv1.Source_SOURCE_DEFAULT {
			changed++
		}
	}
	if changed > 0 {
		said += ", " + strconv.Itoa(changed) + " changed"
	}
	return said
}

// How many entries a table holds, with the noun of one, None for none
func countOf(setting *configv1.Setting, noun string) string {
	if n := len(groupOf(setting.GetValue())); n > 0 {
		return render.Count(n, noun)
	}
	return "None"
}

// Rows of what the chat keeps for itself: its model, its effort, what enter does while the agent works, and with two accounts or more the
// account of the model's provider. Each is the control of its row under New chats, for this chat alone
func (p *agentPage) chatRows(h *agentv1.ChatResponse) []pageRow {
	model := "None"
	if h.GetModel() != "" {
		model = h.GetModel() + " · " + h.GetProviderTitle()
	}
	rows := []pageRow{
		p.chatRow("Model", model, "The model this chat uses.", "/model", h.GetOwnModel(), p.chooseModel,
			func() (string, error) { return p.setModel("", "", true) }),
		p.chatRow("Reasoning effort", effortWord(h.GetEffort()), "How much the model thinks before it answers in this chat.", "/reasoning",
			h.GetOwnEffort(), p.chooseEffort, func() (string, error) { return p.setEffort("", true) }),
		p.chatRow("Enter while busy", agentconfig.BusyTitle(h.GetBusy()), "What happens to a message you send while the agent is working in this chat.",
			"/busy", h.GetOwnBusy(), p.chooseBusy, func() (string, error) { return p.setBusy("", true) }),
	}
	if h.GetAccounts() > 1 {
		account := cmp.Or(h.GetAccountTitle(), agentconfig.ProviderChoice)
		rows = append(rows, p.chatRow("Account", account, "The account of "+h.GetProviderTitle()+" that this chat uses.", "/model --login",
			h.GetAccount() != "", p.chooseAccount, func() (string, error) { return p.setAccount(h.GetProvider(), "") }))
	}
	return rows
}

// One row of This chat: its name, the mark of a value of the chat's own, the value; in the pane what it is, the command that changes it too
// and where its value comes from. Enter opens its control, ctrl+d puts back the value of new chats
func (p *agentPage) chatRow(name, value, about, slash string, own bool, open func() tea.Cmd, reset func() (string, error)) pageRow {
	t := p.kit.Env.Theme
	detail := []string{about, render.Dim("Also "+slash, t)}
	hints := []wm.Hint{{Action: "hub.use", Label: "change"}}
	if own {
		detail = append(detail, render.Cool("This chat keeps its own. "+p.kit.Env.Keys.Display("hub.remove")+" puts back the one of new chats.", t))
		hints = append(hints, wm.Hint{Action: "hub.remove", Label: "reset"})
	}
	return pageRow{group: thisChat, cells: []string{name, changed(p.kit, own), value}, detail: detail, hints: hints, words: about + " " + slash,
		use: func(*cards.List) tea.Cmd { return open() },
		remove: func(l *cards.List) tea.Cmd {
			if !own {
				return nil
			}
			l.Confirm(cards.Question{Text: "Reset " + name + " of this chat to the one of new chats?", Verb: "Reset"}, func() tea.Cmd {
				return p.keep(reset)
			})
			return nil
		}}
}

// Reasoning effort in words, the model's default for none
func effortWord(effort string) string {
	if effort == "" {
		return "The model's default"
	}
	return effortTitle(effort)
}

// Keeps a value of the chat through the server's one function for it, then reads the chat again and says what the control says; the job
// finishes, closing the menu when the person came straight to it
func (p *agentPage) keep(set func() (string, error)) tea.Cmd {
	if p.hub.Shown() {
		p.hub.Busy("Saving…")
	}
	return tui.Async(func() func() tea.Cmd {
		said, err := set()
		held := p.read()
		return func() tea.Cmd {
			if err != nil {
				return p.hub.Fail(tui.Reason(err))
			}
			if held != nil {
				p.held = held
			}
			p.list.Refresh()
			return tea.Batch(p.hub.Done(said), p.hub.Finish(nil))
		}
	})
}

func (p *agentPage) setModel(name, model string, clear bool) (string, error) {
	response, err := p.d.agents.SetModel(p.kit.Env.Ctx, &agentv1.SetModelRequest{SessionId: p.chat, Provider: name, Model: model, Clear: clear})
	return response.GetSaid(), err
}

func (p *agentPage) setEffort(effort string, clear bool) (string, error) {
	response, err := p.d.agents.SetEffort(p.kit.Env.Ctx, &agentv1.SetEffortRequest{SessionId: p.chat, Effort: effort, Clear: clear})
	return response.GetSaid(), err
}

func (p *agentPage) setBusy(mode string, clear bool) (string, error) {
	response, err := p.d.agents.SetBusy(p.kit.Env.Ctx, &agentv1.SetBusyRequest{SessionId: p.chat, Mode: mode, Clear: clear})
	return response.GetSaid(), err
}

// Holds the chat to the account of the provider, or with none lets the provider choose again; the provider as the row read it, since this
// runs off the app's goroutine
func (p *agentPage) setAccount(provider, account string) (string, error) {
	response, err := p.d.agents.SetLogin(p.kit.Env.Ctx, &agentv1.SetLoginRequest{SessionId: p.chat, Provider: provider, Login: account,
		Clear: account == ""})
	return response.GetSaid(), err
}

// The model picker for this chat, the model of new chats first
func (p *agentPage) chooseModel() tea.Cmd {
	h := p.held
	ask := tui.Ask{Title: "Model", Intro: []string{"The model this chat uses."}, Profile: p.profile, First: []tui.Choice{{Title: "Same as new chats"}},
		Values: map[string]string{providerconfig.AskProvider: quoted(h.GetProvider()), providerconfig.AskModel: quoted(h.GetModel())}}
	if !h.GetOwnModel() {
		ask.Values[providerconfig.AskModel] = "null"
	}
	ask.Done = func(values map[string]string) tea.Cmd {
		name, model := plain(values[providerconfig.AskProvider]), plain(values[providerconfig.AskModel])
		return p.keep(func() (string, error) { return p.setModel(name, model, name == "" && model == "") })
	}
	cmd, _ := p.kit.Choose(p.hub, providerconfig.PickModel, ask)
	return cmd
}

// How hard the chat's model thinks, picked from the efforts that model has, the effort of new chats first
func (p *agentPage) chooseEffort() tea.Cmd {
	h := p.held
	current := ""
	if h.GetOwnEffort() {
		current = h.GetEffort()
	}
	return p.d.chooseEffort(p.kit, p.hub, p.profile, h.GetProvider(), h.GetModel(), []tui.Choice{{Title: "Same as new chats"}}, current,
		func(effort string) tea.Cmd {
			return p.keep(func() (string, error) { return p.setEffort(effort, effort == "") })
		})
}

// What enter does in the chat while the agent works, picked from the choices of its setting, the choice of new chats first
func (p *agentPage) chooseBusy() tea.Cmd {
	setting := p.options.Setting(agentconfig.BusyInputMode)
	choices := []tui.Choice{{Title: "Same as new chats"}}
	for _, option := range setting.GetOptions() {
		choices = append(choices, tui.Choice{ID: option.GetValue(), Title: option.GetTitle(), Detail: option.GetDescription()})
	}
	current := ""
	if p.held.GetOwnBusy() {
		current = p.held.GetBusy()
	}
	return pickMarked(p.kit, p.hub, "Enter while busy", []string{"What happens to a message you send while the agent is working in this chat."},
		choices, current, func(mode string) tea.Cmd {
			return p.keep(func() (string, error) { return p.setBusy(mode, mode == "") })
		})
}

// The account of the chat's provider, picked from its accounts, the provider's own choice first
func (p *agentPage) chooseAccount() tea.Cmd {
	h := p.held
	ask := tui.Ask{Title: "Account", Intro: []string{"The account of " + h.GetProviderTitle() + " that this chat uses."}, Profile: p.profile,
		First:  []tui.Choice{{Title: agentconfig.ProviderChoice, Detail: "The account that Account used of " + h.GetProviderTitle() + " picks"}},
		Values: map[string]string{providerconfig.AskProvider: quoted(h.GetProvider()), providerconfig.AskAccount: quoted(h.GetAccount())}}
	ask.Done = func(values map[string]string) tea.Cmd {
		account := plain(values[providerconfig.AskAccount])
		return p.keep(func() (string, error) { return p.setAccount(h.GetProvider(), account) })
	}
	cmd, _ := p.kit.Choose(p.hub, providerconfig.PickAccount, ask)
	return cmd
}

// Page of the prompt: your instructions and coding rules, your personalities, the rest of its settings, and those few people change
func (p *agentPage) prompt() tea.Cmd {
	info := p.options.Info(agentconfig.PromptKey)
	if info == nil {
		return nil
	}
	list := pageList(p.kit, pageSpec{
		title: info.GetTitle(),
		key:   agentScreen + ":prompt",
		intro: func() []string { return []string{info.GetDescription() + "."} },
		rows: func() []pageRow {
			everyday, _ := p.options.Own(agentconfig.PromptKey, agentconfig.Personality)
			var rows []pageRow
			for _, setting := range everyday {
				rows = append(rows, optionRow(p.options, setting, ""))
				if setting.GetKey() == agentconfig.CodingInstructions {
					rows = append(rows, openRow(p.kit, "Your personalities", countOf(p.options.Setting(agentconfig.Personalities), "personality"),
						"Personalities of your own that you can pick by name.", func() tea.Cmd { return p.personalities("") }))
				}
			}
			cells, open, ok := p.options.OwnAdvanced(agentconfig.PromptKey, agentconfig.Personality)
			return append(rows, advancedRow(cells, open, ok, "Settings of the prompt that few people change.")...)
		},
	})
	p.options.Watch(list)
	return p.hub.Push(list)
}

// Page of the guards of tool loops
func (p *agentPage) loops() tea.Cmd {
	info := p.options.Info(agentconfig.LoopKey)
	if info == nil {
		return nil
	}
	list := pageList(p.kit, pageSpec{
		title: info.GetTitle(),
		key:   agentScreen + ":loops",
		intro: func() []string { return []string{info.GetDescription() + "."} },
		rows: func() []pageRow {
			everyday, _ := p.options.Own(agentconfig.LoopKey)
			rows := optionRows(p.options, "", everyday...)
			cells, open, ok := p.options.OwnAdvanced(agentconfig.LoopKey)
			return append(rows, advancedRow(cells, open, ok, "Settings of tool loops that few people change.")...)
		},
	})
	p.options.Watch(list)
	return p.hub.Push(list)
}
