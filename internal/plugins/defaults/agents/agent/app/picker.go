package app

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/provider"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type offer struct {
	provider *providerv1.Provider
	model    string
	// What is known of the model, nil when nothing is
	detail *providerv1.Model
}

type picker struct {
	chat    *chat
	list    *cards.List
	rows    []offer
	failed  []string
	pending int
	loaded  bool
	err     error
	// The pick is saved for every session rather than this one alone
	global bool
	// Reading of the models now, the answers of an earlier one dropped
	gen int
}

const pickerContext = "agent.model"

var pickerKeys = []keys.Action{
	{ID: "agent.model.scope", Context: pickerContext, Label: "make default", Doc: "Save the choice for new sessions too, or for this session only", Default: []string{"ctrl+g"}},
	{ID: "agent.model.logins", Context: pickerContext, Label: "show accounts", Doc: "Show your accounts at the provider of the highlighted model", Default: []string{"ctrl+d"}},
}

// Columns of the models: the mark of the one in use, the model and its context window, and while a search drops the headings the provider
var (
	modelColumns = []render.Column{{}, {Flex: 1}, {Right: true}}
	foundColumns = []render.Column{{}, {Flex: 1}, {Right: true}, {}}
)

// List of every model of the connected providers under a heading for each provider, the provider in use first and the one in use marked;
// enter switches to the highlighted model, or first asks how hard it should think when it can be told
func (c *chat) pick() tea.Cmd {
	p := &picker{chat: c}
	kit := c.kit
	t := kit.Env.Theme
	var hub *cards.Hub
	p.list = cards.NewList(kit.Env, cards.ListSpec{
		Title:   "Model",
		Glyph:   t.Glyphs.Models,
		Key:     "model",
		Context: cards.HubContext,
		Shared:  []string{pickerContext},
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search models",
		Intro:   func() []string { return []string{p.intro()} },
		Columns: modelColumns,
		Layout: func() []render.Column {
			if p.list.Query() != "" {
				return foundColumns
			}
			return modelColumns
		},
		Rows: func() [][]string {
			searching := p.list.Query() != ""
			rows := make([][]string, len(p.rows))
			for i, row := range p.rows {
				model := row.model
				if note := row.detail.GetNote(); note != "" {
					model += " " + render.Dim(note, t)
				}
				size := ""
				if tokens := models.Tokens(row.detail.GetContext()); tokens != "" {
					size = render.Dim(tokens+" context", t)
				}
				rows[i] = []string{kit.Mark(true, p.current(row)), model, size}
				if searching {
					rows[i] = append(rows[i], render.Dim(p.named(row.provider), t))
				}
			}
			return rows
		},
		Group: func(i int) string { return p.named(p.rows[i].provider) },
		Words: func(i int) string {
			row := p.rows[i]
			return strings.Join([]string{row.provider.GetDisplayName(), row.provider.GetName(), row.detail.GetAbilities(), row.detail.GetNote()}, " ")
		},
		Detail: func(i int) []string { return p.detail(p.rows[i]) },
		Banner: func() string {
			if len(p.failed) == 0 {
				return ""
			}
			return "The models of " + strings.Join(p.failed, ", ") + " could not be read, so only their default model is listed. " +
				kit.Env.Keys.Display("hub.refresh") + " tries again."
		},
		Empty: func() string {
			switch {
			case p.err != nil:
				return "Your providers could not be read. " + tui.Reason(p.err) + " " + kit.Env.Keys.Display("hub.refresh") + " tries again."
			case !p.loaded || p.pending > 0:
				return "Reading the models of your providers…"
			}
			return "No provider is connected yet. " + kit.Env.Keys.Display("hub.add") + " connects one."
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true, "agent.model.scope": true, "agent.model.logins": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			// With no model to choose, the one thing to do is to connect a provider
			if i < 0 {
				return []wm.Hint{{Action: "hub.add", Label: "add provider"}}
			}
			more := []wm.Hint{{Action: "hub.add", Label: "add provider"}, p.scopeHint(), {Action: "agent.model.logins"}}
			verb := "next"
			if !p.effortful(p.rows[i]) {
				verb = "switch"
			}
			return append([]wm.Hint{{Action: "hub.use", Label: verb}}, more...)
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				row := p.rows[i]
				if !p.effortful(row) {
					return tea.Batch(kit.WM.DismissCard(hub), c.send(p.command(row, "")))
				}
				return hub.Push(p.efforts(hub, row))
			},
			"hub.add":           func(*cards.List, int) tea.Cmd { return kit.Open(providerapp.Screen, profile.Join(c.on(), "")) },
			"hub.refresh":       func(*cards.List, int) tea.Cmd { return p.read() },
			"agent.model.scope": func(*cards.List, int) tea.Cmd { p.global = !p.global; return nil },
			"agent.model.logins": func(_ *cards.List, i int) tea.Cmd {
				// The accounts of the highlighted row's provider, else of the provider the chat runs on
				name := c.log.state.GetProvider()
				if i >= 0 && i < len(p.rows) {
					name = p.rows[i].provider.GetName()
				}
				if name == "" {
					return kit.Open(providerapp.Screen, profile.Join(c.on(), ""))
				}
				return kit.Open(providerapp.Screen, profile.Join(c.on(), providerapp.LoginsOf+name))
			},
		},
	})
	p.list.ToggleDetail()
	hub = kit.Hub("model", p.list)
	return tea.Batch(kit.WM.ShowOverlay(hub), p.read())
}

// Reads the providers of the chat's profile and the models of each connected one, the rows read before dropped
func (p *picker) read() tea.Cmd {
	kit := p.chat.kit
	p.rows, p.failed, p.loaded, p.err, p.pending = nil, nil, false, nil, 0
	p.gen++
	gen := p.gen
	p.list.Refresh()
	return tui.Async(func() func() tea.Cmd {
		response, err := p.chat.desk.providers.List(kit.Env.Ctx, &providerv1.ListRequest{Profile: p.chat.on()})
		return func() tea.Cmd {
			if gen != p.gen {
				return nil
			}
			p.err, p.loaded = err, true
			var cmds []tea.Cmd
			for _, provider := range response.GetProviders() {
				if provider.GetUsable() && !provider.GetHidden() && (provider.GetCredentialOrigin() != providerv1.Origin_ORIGIN_UNSPECIFIED || provider.GetLocal()) {
					p.pending++
					cmds = append(cmds, p.load(provider, gen))
				}
			}
			p.list.Refresh()
			return tea.Batch(cmds...)
		}
	})
}

// First line of the list: what is chosen and where the choice is kept
func (p *picker) intro() string {
	if p.global {
		return "Choose the model for this chat. Your choice also becomes the default for new sessions."
	}
	return "Choose the model for this chat. Your choice applies to this session only."
}

// Button that keeps the choice for new sessions too, or for this session only
func (p *picker) scopeHint() wm.Hint {
	if p.global {
		return wm.Hint{Action: "agent.model.scope", Label: "session only"}
	}
	return wm.Hint{Action: "agent.model.scope"}
}

// Name of a provider as the list shows it: its display name, with its name in the profile after it when another provider listed has the
// same display name, such as two accounts of one kind set up as two providers
func (p *picker) named(provider *providerv1.Provider) string {
	shown := provider.GetDisplayName()
	for _, row := range p.rows {
		if row.provider.GetName() != provider.GetName() && row.provider.GetDisplayName() == shown {
			return shown + " · " + provider.GetName()
		}
	}
	return shown
}

// Whether the model is asked how hard to think before the switch: every model but one known to take no effort
func (p *picker) effortful(row offer) bool {
	return row.detail == nil || row.detail.Reasoning == nil || row.detail.GetReasoning()
}

// Lines under the list about the highlighted model: its whole name and provider, whether the chat uses it, then its limits, prices,
// abilities and efforts as far as they are known, and the account it runs on
func (p *picker) detail(row offer) []string {
	t := p.chat.kit.Env.Theme
	d := row.detail
	lines := []string{row.model + " at " + p.named(row.provider)}
	if p.current(row) {
		lines[0] += ". This chat uses it now."
	}
	var facts []string
	if n := d.GetContext(); n > 0 {
		facts = append(facts, "Context window: "+tokensSaid(n))
	}
	if n := d.GetMaxOutput(); n > 0 {
		facts = append(facts, "Longest reply: "+tokensSaid(n))
	}
	if d.GetInputPrice() != "" || d.GetOutputPrice() != "" {
		price := "Per million tokens: " + models.PerMillion(d.GetInputPrice()) + " to read, " + models.PerMillion(d.GetOutputPrice()) + " to write"
		if d.GetCacheReadPrice() != "" {
			price += ", " + models.PerMillion(d.GetCacheReadPrice()) + " read again from cache"
		}
		facts = append(facts, price)
	}
	if len(facts) > 0 {
		lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	}
	var more []string
	if abilities := strings.TrimSpace(d.GetAbilities()); abilities != "" {
		more = append(more, "Can do: "+abilities)
	}
	switch {
	case d != nil && d.Reasoning != nil && !d.GetReasoning():
		more = append(more, "Takes no reasoning effort")
	case len(d.GetEfforts()) > 0:
		var efforts []string
		for _, effort := range d.GetEfforts() {
			efforts = append(efforts, effortTitle(effort))
		}
		more = append(more, "Reasoning efforts: "+strings.Join(efforts, ", "))
	}
	if d.GetMandatoryReasoning() {
		more = append(more, "Its reasoning can't be turned off")
	}
	if account := row.provider.GetAccount(); account != "" {
		more = append(more, "Account: "+account)
	}
	if len(more) > 0 {
		lines = append(lines, render.Dim(strings.Join(more, " · "), t))
	}
	return lines
}

// Count of tokens with its digits grouped in thousands
func tokensSaid(n int64) string {
	digits := []byte(strconv.FormatInt(n, 10))
	var out []byte
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, d)
	}
	return string(out) + " tokens"
}

// Where the pick is saved, as the /model command takes it
func (p *picker) scope() string {
	if p.global {
		return "--global"
	}
	return "--session"
}

// The /model command of the pick: the model, its provider, the effort when one was picked, and the scope
func (p *picker) command(row offer, effort string) string {
	command := "/model " + row.model + " --provider " + row.provider.GetName()
	if effort != "" {
		command += " --reasoning " + effort
	}
	return command + " " + p.scope()
}

// Second step of a pick: how hard the model thinks, the one in use marked, reasoning off where the model allows it, or the effort left as it
// is; going back returns to the models
func (p *picker) efforts(hub *cards.Hub, row offer) *cards.List {
	kit := p.chat.kit
	t := kit.Env.Theme
	type level struct{ title, value, about string }
	// The efforts the model takes, as its kind or its provider's listing names them, else every effort a wire knows; none is offered only
	// for a model whose reasoning can be turned off
	offered := row.detail.GetEfforts()
	if len(offered) == 0 {
		offered = provider.Efforts
	}
	var levels []level
	for _, effort := range offered {
		if effort != "none" {
			levels = append(levels, level{effortTitle(effort), effort, ""})
		}
	}
	if !row.detail.GetMandatoryReasoning() {
		levels = append(levels, level{effortTitle("none"), "none", "Answers without thinking first"})
	}
	now := p.chat.log.state.GetReasoningEffort()
	keep := "The model's default"
	if now != "" {
		keep = effortTitle(now)
	}
	levels = append(levels, level{"Keep the effort as it is", "", keep})
	// The effort in use is marked where it is offered, else keeping it is
	inUse := slices.IndexFunc(levels, func(l level) bool { return l.value != "" && l.value == now })
	if inUse < 0 {
		inUse = len(levels) - 1
	}
	var list *cards.List
	list = cards.NewList(kit.Env, cards.ListSpec{
		Title:   "Reasoning effort",
		Glyph:   t.Glyphs.Models,
		Key:     "model:effort",
		Context: cards.HubContext,
		Shared:  []string{pickerContext},
		Primary: "hub.use",
		Pick:    true,
		Intro: func() []string {
			return []string{"How hard " + row.model + " thinks before it answers. The chat switches to it once you choose."}
		},
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(levels))
			for i, l := range levels {
				rows[i] = []string{kit.Mark(true, i == inUse), l.title, render.Dim(l.about, t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			if list.Shows(2) || levels[i].about == "" {
				return nil
			}
			return []string{levels[i].about}
		},
		Free: map[string]bool{"agent.model.scope": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{p.scopeHint()}
			}
			return []wm.Hint{{Action: "hub.use", Label: "switch"}, p.scopeHint()}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				return tea.Batch(kit.WM.DismissCard(hub), p.chat.send(p.command(row, levels[i].value)))
			},
			"agent.model.scope": func(*cards.List, int) tea.Cmd { p.global = !p.global; return nil },
		},
	})
	list.ToggleDetail()
	list.Refresh()
	list.Select(inUse)
	return list
}

func (p *picker) current(row offer) bool {
	state := p.chat.log.state
	return state.GetProvider() == row.provider.GetName() && state.GetModel() == row.model
}

// Adds the models of the provider: those picked for it, else every one it lists, in the order the list shows them
func (p *picker) load(provider *providerv1.Provider, gen int) tea.Cmd {
	kit := p.chat.kit
	return tui.Async(func() func() tea.Cmd {
		models := provider.GetModels()
		details := map[string]*providerv1.Model{}
		response, err := p.chat.desk.providers.Models(kit.Env.Ctx, &providerv1.ModelsRequest{Provider: provider.GetName(), Profile: p.chat.on()})
		for _, detail := range response.GetDetails() {
			details[detail.GetId()] = detail
		}
		if len(models) == 0 {
			models = response.GetModels()
		} else {
			err = nil
		}
		return func() tea.Cmd {
			if gen != p.gen {
				return nil
			}
			p.pending--
			if err != nil && !provider.GetLocal() {
				p.failed = append(p.failed, provider.GetDisplayName())
			}
			if provider.GetDefaultModel() != "" && !slices.Contains(models, provider.GetDefaultModel()) && (err == nil || !provider.GetLocal()) {
				models = append(models, provider.GetDefaultModel())
			}
			for _, model := range models {
				p.rows = append(p.rows, offer{provider: provider, model: model, detail: details[model]})
			}
			p.sort()
			if !p.list.Filtered() {
				p.list.Refresh()
				for i, row := range p.rows {
					if p.current(row) {
						p.list.Select(i)
					}
				}
			}
			return nil
		}
	})
}

// Orders the models: the provider the chat runs on first, the others by name, and in each the provider's default model first, then by name
func (p *picker) sort() {
	using := p.chat.log.state.GetProvider()
	slices.SortStableFunc(p.rows, func(a, b offer) int {
		return cmp.Or(compareTrue(a.provider.GetName() == using, b.provider.GetName() == using),
			strings.Compare(a.provider.GetDisplayName(), b.provider.GetDisplayName()), strings.Compare(a.provider.GetName(), b.provider.GetName()),
			compareTrue(a.model == a.provider.GetDefaultModel(), b.model == b.provider.GetDefaultModel()), strings.Compare(a.model, b.model))
	})
}
