package app

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Pickers of the things the providers screen owns, which every other screen opens through the kit: a model with its provider, a provider,
// and an account of a provider
func pickers(service providerv1connect.ProviderServiceClient, kept *names, read func(kit *tui.Kit, profile string) tea.Cmd) map[string]tui.Picker {
	beside := func(key string) []string {
		if other := providerconfig.Beside(key); other != "" {
			return []string{other}
		}
		return nil
	}
	return map[string]tui.Picker{
		providerconfig.PickModel: {
			Open: func(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd {
				return (&picker{kit: kit, service: service, ask: ask, hub: hub, names: kept}).open()
			},
			Beside: beside,
			Shown:  func(ask tui.Ask) string { return shownModel(kept, ask) },
		},
		providerconfig.PickProvider: {
			Open: func(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd { return pickProvider(kit, service, hub, ask) },
			Shown: func(ask tui.Ask) string {
				id := value(ask, ask.Key, providerconfig.AskProvider)
				if id == "" {
					return ""
				}
				shown, _ := kept.of(ask.Profile, id)
				return shown
			},
		},
		providerconfig.PickAccount: {
			Open:   func(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd { return pickAccount(kit, service, hub, ask) },
			Beside: beside,
		},
	}
}

// Value the ask holds under the key, else under the key a screen asking for itself uses, as text
func value(ask tui.Ask, key, own string) string {
	text, ok := ask.Values[key]
	if key == "" || !ok {
		text = ask.Values[own]
	}
	var decoded any
	if yaml.Unmarshal([]byte(text), &decoded) != nil {
		return ""
	}
	held, _ := decoded.(string)
	return held
}

// Provider a model or an account the ask is about goes with: the one its key belongs to, the one kept beside it, or the one a screen
// asking for itself hands
func owner(ask tui.Ask) string {
	if id := providerconfig.Owner(ask.Key); id != "" {
		return id
	}
	return value(ask, providerconfig.Beside(ask.Key), providerconfig.AskProvider)
}

// A model and its provider as a row shows them, model · provider; the model alone for a setting of one provider; nothing while none is
// picked, which leaves the setting's own words
func shownModel(kept *names, ask tui.Ask) string {
	// An item of a list of models names its provider itself, as provider:model
	if ask.Item {
		id, model := providerconfig.Split(value(ask, ask.Key, ""))
		if id == "" {
			return model
		}
		shown, _ := kept.of(ask.Profile, id)
		return model + " · " + cmp.Or(shown, id)
	}
	model := value(ask, ask.Key, providerconfig.AskModel)
	if model == "" || providerconfig.Owner(ask.Key) != "" {
		return model
	}
	id := owner(ask)
	if id == "" {
		return model
	}
	shown, _ := kept.of(ask.Profile, id)
	return model + " · " + shown
}

// Values a pick hands back as YAML: the model under the setting's key and its provider beside it, or under the keys of a screen asking
// for itself, or one item of a list of models as provider:model; with the effort when one was asked
func (p *picker) values(row offer, effort string) map[string]string {
	quote := func(text string) string {
		encoded, _ := yaml.Marshal(text)
		return strings.TrimSpace(string(encoded))
	}
	out := map[string]string{}
	switch {
	case p.ask.Key == "":
		out[providerconfig.AskProvider], out[providerconfig.AskModel] = quote(row.provider.GetName()), quote(row.model)
	case p.ask.Item:
		out[p.ask.Key] = quote(providerconfig.Pair(row.provider.GetName(), row.model))
	case providerconfig.Owner(p.ask.Key) != "":
		out[p.ask.Key] = quote(row.model)
	default:
		out[p.ask.Key] = quote(row.model)
		if beside := providerconfig.Beside(p.ask.Key); beside != "" {
			out[beside] = quote(row.provider.GetName())
		}
	}
	if p.efforts {
		out[providerconfig.AskEffort] = quote(effort)
	}
	return out
}

type offer struct {
	provider *providerv1.Provider
	model    string
	// What is known of the model, nil when nothing is
	detail *providerv1.Model
	// The row that asks for the name of a model of the provider, with why the provider gave no list of its own when it gave none
	typed bool
	why   string
	// A row of the choices listed before the models, such as what is in effect when nothing is picked
	first *tui.Choice
	// The row that adds a provider of the kind the ask lists the models of, while the profile has none; its kind, nil for a name that is
	// no kind, whose row opens the providers
	adds   bool
	addsOf *providerv1.Spec
}

// The one list of models: every model of every provider that is ready under the name of its provider, searched by typing; a ready
// provider whose models can't be read listed with why and the row that asks for a model name. A setting of one provider lists that
// provider's models alone
type picker struct {
	kit     *tui.Kit
	service providerv1connect.ProviderServiceClient
	ask     tui.Ask
	names   *names
	hub     *cards.Hub
	// The hub is the picker's own dialog
	own  bool
	list *cards.List
	rows []offer
	// Providers of the profile as last read
	providers []*providerv1.Provider
	failed    []string
	pending   int
	loaded    bool
	err       error
	// Asks how hard the picked model thinks before it hands the pick back
	efforts bool
	// Kinds of provider or names of providers whose models alone are listed, as the ask's Of names them; none for every provider
	of []string
	// Reading of the models now, the answers of an earlier one dropped
	gen int
	// Row the list last put in hand itself, so a row a person moved to since is told apart from it
	placed string
}

// What tells a row of the list apart from every other
func (o offer) id() string {
	switch {
	case o.first != nil:
		return "first:" + o.first.ID + ":" + o.first.Title
	case o.adds:
		return "adds"
	case o.typed:
		return "typed:" + o.provider.GetName()
	}
	return "model:" + o.provider.GetName() + ":" + o.model
}

// Row in hand by what tells it apart, empty for none
func (p *picker) held() string {
	if i := p.list.Index(); i >= 0 && i < len(p.rows) {
		return p.rows[i].id()
	}
	return ""
}

// Draws the rows again once they changed, with the same row in hand as before they did: the row held, which a person moved to or searched
// for, wherever the rows that arrived put it; until they move, the model picked now
func (p *picker) place(held string) {
	p.list.Refresh()
	at := -1
	if !p.list.Filtered() && (held == "" || held == p.placed) {
		at = slices.IndexFunc(p.rows, p.current)
	}
	if at < 0 && held != "" {
		at = slices.IndexFunc(p.rows, func(row offer) bool { return row.id() == held })
	}
	if at >= 0 {
		p.list.Select(at)
	}
	p.placed = p.held()
}

const pickerContext = "providers.model"

var pickerKeys = []keys.Action{
	{ID: "providers.model.logins", Context: pickerContext, Label: "show accounts", Doc: "Show your accounts at the provider of the highlighted model",
		Default: []string{"ctrl+d"}},
}

// Columns of the models: the mark of the one in use, the model and its context window, and while a search drops the headings the provider
var (
	modelColumns = []render.Column{{}, {Flex: 1}, {Right: true}}
	foundColumns = []render.Column{{}, {Flex: 1}, {Right: true}, {}}
)

// Opens the list over the hub, in a dialog of its own for none
func (p *picker) open() tea.Cmd {
	t := p.kit.Env.Theme
	_, p.efforts = p.ask.Values[providerconfig.AskEffort]
	p.of = providerconfig.Of(p.ask.Of)
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   cmp.Or(p.ask.Title, "Model"),
		Glyph:   t.Glyphs.Models,
		Key:     "model",
		Context: cards.HubContext,
		Shared:  []string{pickerContext},
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search models",
		Intro: func() []string {
			if lack := p.lack(); lack != "" {
				return append(slices.Clone(p.ask.Intro), lack)
			}
			return p.ask.Intro
		},
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
				switch {
				case row.first != nil:
					rows[i] = []string{p.kit.Mark(true, p.picked() == ""), row.first.Title, ""}
				case row.adds:
					rows[i] = []string{"", p.addWords(row) + "  " + render.Dim(t.Glyphs.Forward, t), ""}
				case row.typed:
					rows[i] = []string{p.kit.Mark(true, false), render.Dim("Type a model name", t), ""}
				default:
					model := row.model
					if note := row.detail.GetNote(); note != "" {
						model += " " + render.Dim(note, t)
					}
					size := ""
					if tokens := models.Tokens(row.detail.GetContext()); tokens != "" {
						size = render.Dim(tokens+" context", t)
					}
					rows[i] = []string{p.kit.Mark(true, p.current(row)), model, size}
				}
				if searching && row.provider != nil {
					rows[i] = append(rows[i], render.Dim(p.named(row.provider), t))
				}
			}
			return rows
		},
		Group: func(i int) string {
			if p.rows[i].first != nil || p.rows[i].adds {
				return ""
			}
			return p.named(p.rows[i].provider)
		},
		Words: func(i int) string {
			row := p.rows[i]
			switch {
			case row.first != nil:
				return row.first.Title
			case row.adds:
				return p.addWords(row)
			}
			words := []string{row.provider.GetDisplayName(), row.provider.GetName(), row.detail.GetAbilities(), row.detail.GetNote()}
			if row.typed {
				words = append(words, "type model name")
			}
			return strings.Join(words, " ")
		},
		Detail: func(i int) []string { return p.detail(p.rows[i]) },
		Banner: func() string {
			if len(p.failed) == 0 {
				return ""
			}
			return "The models of " + strs.And(slices.Sorted(slices.Values(p.failed))) + " could not be read. You can still type a model name. " +
				p.kit.Env.Keys.Display("hub.refresh") + " tries again."
		},
		Empty: func() string {
			switch {
			case p.err != nil:
				return "Your providers could not be read. " + tui.Reason(p.err) + " " + p.kit.Env.Keys.Display("hub.refresh") + " tries again."
			case !p.loaded || p.pending > 0:
				return "Reading the models of your providers…"
			}
			return p.none()
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true, "providers.model.logins": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{{Action: "hub.add", Label: "add provider"}}
			}
			more := []wm.Hint{{Action: "hub.add", Label: "add provider"}, {Action: "providers.model.logins"}}
			verb := cmp.Or(strings.ToLower(p.ask.Verb), "choose")
			switch {
			case p.rows[i].adds:
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			case p.rows[i].typed:
				verb = "type name"
			case p.efforts && p.rows[i].first == nil && p.effortful(p.rows[i]):
				verb = "next"
			}
			return append([]wm.Hint{{Action: "hub.use", Label: verb}}, more...)
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				row := p.rows[i]
				switch {
				case row.first != nil:
					return p.done(p.cleared())
				case row.adds:
					return p.addKind(row)
				case row.typed:
					return p.typing(row)
				case p.efforts && p.effortful(row):
					return p.hub.Push(p.effortList(row))
				}
				return p.done(p.values(row, ""))
			},
			"hub.add": func(*cards.List, int) tea.Cmd {
				if spec := p.lacking(); spec != nil {
					return p.addKind(offer{adds: true, addsOf: spec})
				}
				return p.kit.Open(Screen, profile.Join(p.ask.Profile, ""))
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.read() },
			"providers.model.logins": func(_ *cards.List, i int) tea.Cmd {
				name := owner(p.ask)
				if i >= 0 && i < len(p.rows) && p.rows[i].provider != nil {
					name = p.rows[i].provider.GetName()
				}
				if name == "" {
					return p.kit.Open(Screen, profile.Join(p.ask.Profile, ""))
				}
				return p.kit.Open(Screen, profile.Join(p.ask.Profile, LoginsOf+name))
			},
		},
	})
	p.list.ToggleDetail()
	var shown tea.Cmd
	if p.hub == nil {
		p.hub, p.own = p.kit.Hub("model", p.list), true
		shown = p.kit.WM.ShowOverlay(p.hub)
	} else {
		shown = p.hub.Push(p.list)
	}
	return tea.Batch(shown, p.read())
}

// Hands the values back and closes the list: the dialog of its own, else the list over the hub
func (p *picker) done(values map[string]string) tea.Cmd {
	if p.own {
		return tea.Batch(p.hub.Leave(), p.ask.Done(values))
	}
	return tea.Batch(p.hub.Close(p.list), p.ask.Done(values))
}

// Values that put the default back: null under every key the pick keeps
func (p *picker) cleared() map[string]string {
	out := map[string]string{}
	if p.ask.Key == "" {
		out[providerconfig.AskProvider], out[providerconfig.AskModel] = "null", "null"
		return out
	}
	out[p.ask.Key] = "null"
	if beside := providerconfig.Beside(p.ask.Key); beside != "" {
		out[beside] = "null"
	}
	return out
}

// Model picked now, empty for none
func (p *picker) picked() string {
	if p.ask.Item {
		return value(p.ask, p.ask.Key, "")
	}
	return value(p.ask, p.ask.Key, providerconfig.AskModel)
}

// Whether the row is the model picked now
func (p *picker) current(row offer) bool {
	if row.typed || row.first != nil || row.adds {
		return false
	}
	if p.ask.Item {
		return p.picked() == providerconfig.Pair(row.provider.GetName(), row.model)
	}
	id := owner(p.ask)
	return p.picked() == row.model && (id == "" || id == row.provider.GetName())
}

// Reads the providers of the profile and the models of each one that is ready and offered in the model list; of the one provider alone
// for a setting of one provider; the rows read before dropped
func (p *picker) read() tea.Cmd {
	p.rows, p.providers, p.failed, p.loaded, p.err, p.pending, p.placed = nil, nil, nil, false, nil, 0, ""
	p.gen++
	gen := p.gen
	p.list.Refresh()
	only := providerconfig.Owner(p.ask.Key)
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &providerv1.ListRequest{Profile: p.ask.Profile})
		return func() tea.Cmd {
			if gen != p.gen {
				return nil
			}
			p.err, p.loaded, p.providers = err, true, response.GetProviders()
			if err == nil {
				p.names.keep(p.ask.Profile, p.providers)
			}
			for i := range p.ask.First {
				p.rows = append(p.rows, offer{first: &p.ask.First[i]})
			}
			var cmds []tea.Cmd
			for _, entry := range p.providers {
				switch {
				case only != "" && entry.GetName() != only:
				case len(p.of) > 0 && !p.lists(entry):
				case only != "" || len(p.of) > 0 || Ready(entry) && !entry.GetHidden():
					p.pending++
					cmds = append(cmds, p.load(entry, gen))
				}
			}
			p.place("")
			// None of the providers the ask names is in the profile: one row adds one of that kind
			if err == nil && len(p.of) > 0 && p.pending == 0 {
				cmds = append(cmds, p.offerAdding(gen))
			}
			return tea.Batch(cmds...)
		}
	})
}

// Why the list has no model to offer, by the test the providers page uses: no provider at all, none ready and why the first is not, or
// every ready one left out of the model list
func (p *picker) none() string {
	add := p.kit.Env.Keys.Display("hub.add")
	var waiting *providerv1.Provider
	for _, entry := range p.providers {
		if Ready(entry) {
			return "Every provider that is ready is left out of the model list. " + add + " opens your providers."
		}
		if waiting == nil && entry.GetUsable() {
			waiting = entry
		}
	}
	switch {
	case len(p.providers) == 0:
		return "No provider is connected yet. " + add + " connects one."
	case waiting != nil:
		return "No provider is ready to use. " + waiting.GetWhy() + " " + add + " opens your providers."
	}
	return "No provider is ready to use, because icebath can't send requests to any of them yet. " + add + " opens your providers."
}

// Asks for the name of a model of the row's provider, then hands it back
func (p *picker) typing(row offer) tea.Cmd {
	name := p.named(row.provider)
	field := cards.Field{Label: "Model", Help: "The name of a model that " + name + " serves, as it names it, such as llama3.1:8b.",
		Check: func(value string) string {
			if strings.ContainsFunc(strings.TrimSpace(value), unicode.IsSpace) {
				return "Type the name without spaces."
			}
			return ""
		}}
	return p.kit.Ask(p.hub, "Model of "+name, field, "Choose", func(model string) tea.Cmd {
		typed := offer{provider: row.provider, model: model}
		if p.efforts {
			return p.hub.Push(p.effortList(typed))
		}
		return p.done(p.values(typed, ""))
	})
}

// Name of a provider as the list shows it: its display name, with its name in the profile after it when another provider listed has the
// same display name
func (p *picker) named(entry *providerv1.Provider) string {
	shown := entry.GetDisplayName()
	for _, other := range p.providers {
		if other.GetName() != entry.GetName() && other.GetDisplayName() == shown {
			return shown + " · " + entry.GetName()
		}
	}
	return shown
}

// Whether the model is asked how hard to think: every model but one known to take no effort
func (p *picker) effortful(row offer) bool {
	return row.detail == nil || row.detail.Reasoning == nil || row.detail.GetReasoning()
}

// Lines under the list about the highlighted model: whether it is the one picked, then its limits, prices, abilities and efforts as far
// as they are known, and the account it runs on; nothing the row says already
func (p *picker) detail(row offer) []string {
	t := p.kit.Env.Theme
	switch {
	case row.first != nil:
		return nil
	case row.adds && row.addsOf != nil:
		return []string{"Enter adds " + row.addsOf.GetDisplayName() + " in Providers; its models are listed here once it is connected."}
	case row.adds:
		return []string{"Enter opens your providers."}
	case row.typed:
		var lines []string
		if row.why != "" {
			lines = append(lines, render.Warm(row.why, t))
		}
		return append(lines, "Enter asks for the name of a model that "+p.named(row.provider)+" serves.")
	}
	d := row.detail
	var lines []string
	if p.current(row) {
		lines = append(lines, "The one picked now.")
	}
	var facts []string
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
	if using := row.provider.GetUsing(); using != "" {
		more = append(more, "Account: "+provider.AccountTitle(row.provider, using))
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

// Names of the efforts as a person reads them
var effortWords = map[string]string{"none": "No reasoning", "minimal": "Minimal", "low": "Low", "medium": "Medium", "high": "High",
	"xhigh": "Extra high", "max": "Maximum"}

func effortTitle(effort string) string { return cmp.Or(effortWords[effort], effort) }

// Second step of a pick: how hard the model thinks, the one in use marked, reasoning off where the model allows it, or the effort left as
// it is; going back returns to the models
func (p *picker) effortList(row offer) *cards.List {
	t := p.kit.Env.Theme
	type level struct{ title, value, about string }
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
	now := value(p.ask, providerconfig.AskEffort, providerconfig.AskEffort)
	keep := "The model's default"
	if now != "" {
		keep = effortTitle(now)
	}
	levels = append(levels, level{"Keep the effort as it is", "", keep})
	inUse := slices.IndexFunc(levels, func(l level) bool { return l.value != "" && l.value == now })
	if inUse < 0 {
		inUse = len(levels) - 1
	}
	var efforts *cards.List
	efforts = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Reasoning effort",
		Glyph:   t.Glyphs.Models,
		Key:     "model:effort",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Intro:   func() []string { return []string{"How hard " + row.model + " thinks before it answers."} },
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(levels))
			for i, l := range levels {
				rows[i] = []string{p.kit.Mark(true, i == inUse), l.title, render.Dim(l.about, t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			if efforts.Shows(2) || levels[i].about == "" {
				return nil
			}
			return []string{levels[i].about}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: "choose"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				values := p.values(row, levels[i].value)
				if p.own {
					return tea.Batch(p.hub.Leave(), p.ask.Done(values))
				}
				return tea.Batch(p.hub.Close(efforts), p.hub.Close(p.list), p.ask.Done(values))
			},
		},
	})
	efforts.ToggleDetail()
	efforts.Refresh()
	efforts.Select(inUse)
	return efforts
}

// Adds the models of the provider: those picked for it, else every one it lists, in the order the list shows them, then its default model.
// A provider with no list of its own, or a server of your own, gets the row that asks for a model name, saying why its list is missing
func (p *picker) load(entry *providerv1.Provider, gen int) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		offered := entry.GetModels()
		details := map[string]*providerv1.Model{}
		response, err := p.service.Models(p.kit.Env.Ctx, &providerv1.ModelsRequest{Provider: entry.GetName(), Profile: p.ask.Profile})
		for _, detail := range response.GetDetails() {
			details[detail.GetId()] = detail
		}
		why := ""
		switch {
		case len(offered) > 0 && providerconfig.Owner(p.ask.Key) == "":
		case err != nil:
			why = tui.Reason(err)
		default:
			offered, why = response.GetModels(), response.GetUnlisted()
		}
		return func() tea.Cmd {
			if gen != p.gen {
				return nil
			}
			p.pending--
			held := p.held()
			if why != "" && len(offered) == 0 {
				p.failed = append(p.failed, p.named(entry))
			}
			if entry.GetDefaultModel() != "" && !slices.Contains(offered, entry.GetDefaultModel()) && err == nil {
				offered = append(offered, entry.GetDefaultModel())
			}
			for _, model := range offered {
				p.rows = append(p.rows, offer{provider: entry, model: model, detail: details[model]})
			}
			if len(offered) == 0 || why != "" || entry.GetLocal() {
				p.rows = append(p.rows, offer{provider: entry, typed: true, why: why})
			}
			// The list is drawn again whether it is searched or not, as it acts on the rows by their place among them
			p.sort()
			p.place(held)
			return nil
		}
	})
}

// Orders the rows: the choices before the models first, then the provider picked now, the others by name, and in each the provider's
// default model first, then by name
func (p *picker) sort() {
	using := owner(p.ask)
	rank := func(row offer) int {
		if row.first != nil {
			return 0
		}
		return 1
	}
	slices.SortStableFunc(p.rows, func(a, b offer) int {
		if a.first != nil || b.first != nil {
			return cmp.Compare(rank(a), rank(b))
		}
		return cmp.Or(compareTrue(a.provider.GetName() == using, b.provider.GetName() == using),
			strings.Compare(a.provider.GetDisplayName(), b.provider.GetDisplayName()), strings.Compare(a.provider.GetName(), b.provider.GetName()),
			compareTrue(!a.typed, !b.typed), compareTrue(a.model == a.provider.GetDefaultModel(), b.model == b.provider.GetDefaultModel()),
			strings.Compare(a.model, b.model))
	})
}

// Order of two conditions, the true one first
func compareTrue(x, y bool) int {
	switch {
	case x == y:
		return 0
	case x:
		return -1
	}
	return 1
}

// Reads the providers of the profile of the ask, then runs then; a failure said under the hub, or as a toast without one
func providersOf(kit *tui.Kit, service providerv1connect.ProviderServiceClient, hub *cards.Hub, ask tui.Ask, then func(*providerv1.ListResponse) tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := service.List(kit.Env.Ctx, &providerv1.ListRequest{Profile: ask.Profile})
		return func() tea.Cmd {
			switch {
			case err != nil && hub != nil:
				return hub.Fail(tui.Reason(err))
			case err != nil:
				return kit.WM.Toast(tui.Reason(err), wm.Error, wm.DefaultToastTTL)
			}
			return then(response)
		}
	})
}

// Lists the providers of the profile to pick one, the choices before them first; hands its name back
func pickProvider(kit *tui.Kit, service providerv1connect.ProviderServiceClient, hub *cards.Hub, ask tui.Ask) tea.Cmd {
	return providersOf(kit, service, hub, ask, func(response *providerv1.ListResponse) tea.Cmd {
		choices := slices.Clone(ask.First)
		for _, entry := range response.GetProviders() {
			choices = append(choices, tui.Choice{ID: entry.GetName(), Title: entry.GetDisplayName(), Detail: entry.GetState()})
		}
		// With no provider to list, that is said rather than an empty list opened
		if len(choices) == 0 {
			return said(kit, hub, "This profile has no provider yet. Add one in Providers.")
		}
		key := cmp.Or(ask.Key, providerconfig.AskProvider)
		return kit.PickFrom(hub, cmp.Or(ask.Title, "Provider"), ask.Intro, "choose", choices, value(ask, ask.Key, providerconfig.AskProvider),
			func(choice tui.Choice) tea.Cmd { return ask.Done(map[string]string{key: quoted(choice.ID)}) })
	})
}

// Lists the accounts of the provider the ask is about to pick one, the choices before them and taking turns, when an account takes turns,
// first; hands its name back
func pickAccount(kit *tui.Kit, service providerv1connect.ProviderServiceClient, hub *cards.Hub, ask tui.Ask) tea.Cmd {
	id := owner(ask)
	return providersOf(kit, service, hub, ask, func(response *providerv1.ListResponse) tea.Cmd {
		choices := slices.Clone(ask.First)
		for _, entry := range response.GetProviders() {
			if entry.GetName() != id {
				continue
			}
			if slices.ContainsFunc(entry.GetAccounts(), (*providerv1.Account).GetRotate) {
				choices = append(choices, tui.Choice{ID: providerconfig.Rotation, Title: "Take turns", Detail: "The accounts that take turns"})
			}
			for _, account := range entry.GetAccounts() {
				choices = append(choices, tui.Choice{ID: account.GetName(), Title: account.GetTitle(), Detail: account.GetState()})
			}
		}
		// With no account to list, why is said rather than an empty list opened
		if len(choices) == 0 {
			return said(kit, hub, noAccounts(response.GetProviders(), id))
		}
		key := cmp.Or(ask.Key, providerconfig.AskAccount)
		return kit.PickFrom(hub, cmp.Or(ask.Title, "Account"), ask.Intro, "choose", choices, value(ask, ask.Key, providerconfig.AskAccount),
			func(choice tui.Choice) tea.Cmd { return ask.Done(map[string]string{key: quoted(choice.ID)}) })
	})
}

// Why there is no account to pick: no provider picked beside the setting, a provider the profile no longer has, or one with no account
func noAccounts(providers []*providerv1.Provider, id string) string {
	if id == "" {
		return "Pick the provider first, then one of its accounts."
	}
	for _, entry := range providers {
		if entry.GetName() == id {
			return entry.GetDisplayName() + " has no account to pick. Add one in Providers."
		}
	}
	return "This profile has no provider named " + id + "."
}

// Says one line under the hub, or as a toast without one
func said(kit *tui.Kit, hub *cards.Hub, text string) tea.Cmd {
	if hub != nil {
		return hub.Say(text, cards.NoteInfo)
	}
	return kit.WM.Toast(text, wm.Warn, wm.DefaultToastTTL)
}

// Text as YAML, null for none
func quoted(text string) string {
	if text == "" {
		return "null"
	}
	encoded, _ := yaml.Marshal(text)
	return strings.TrimSpace(string(encoded))
}

// Whether the provider is one the ask lists the models of: of a kind it names, or named by it
func (p *picker) lists(entry *providerv1.Provider) bool {
	return slices.Contains(p.of, entry.GetSpec()) || slices.Contains(p.of, entry.GetName())
}

// Reads the kinds of provider to find the first kind the ask names, then lists the one row that adds a provider of it, or that opens the
// providers when it names no kind
func (p *picker) offerAdding(gen int) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.Specs(p.kit.Env.Ctx, &providerv1.SpecsRequest{Profile: p.ask.Profile})
		return func() tea.Cmd {
			if gen != p.gen {
				return nil
			}
			row := offer{adds: true}
			if err == nil {
				for _, spec := range response.GetSpecs() {
					if spec.GetName() == p.of[0] {
						row.addsOf = spec
					}
				}
			}
			p.rows = append(p.rows, row)
			p.place(row.id())
			return nil
		}
	})
}

// Kind of provider the add row adds, nil while there is no such row or it names no kind
func (p *picker) lacking() *providerv1.Spec {
	for _, row := range p.rows {
		if row.adds {
			return row.addsOf
		}
	}
	return nil
}

// Line under the intro while the profile has none of the providers the ask lists the models of
func (p *picker) lack() string {
	for _, row := range p.rows {
		if !row.adds {
			continue
		}
		if row.addsOf == nil {
			return "This profile has no provider named " + p.of[0] + "."
		}
		return "This profile has no " + row.addsOf.GetDisplayName() + " provider yet, so there are none of its models to list."
	}
	return ""
}

// Words of the add row: adding a provider of the kind, or opening the providers for a name that is no kind
func (p *picker) addWords(row offer) string {
	if row.addsOf == nil {
		return "Open your providers"
	}
	return "Add " + row.addsOf.GetDisplayName()
}

// Opens the providers on adding one of the kind the row names, or on their page for none
func (p *picker) addKind(row offer) tea.Cmd {
	if row.addsOf == nil {
		return p.kit.Open(Screen, profile.Join(p.ask.Profile, ""))
	}
	return p.kit.Open(Screen, profile.Join(p.ask.Profile, AddOf+row.addsOf.GetName()))
}
