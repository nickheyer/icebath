package app

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	modelsconfig "github.com/nickheyer/icebath/internal/models/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Page of the Model list area: the limits and abilities set for models, then the options of the online list, then its advanced settings
// with the lists of models of single kinds
type modelList struct {
	// Provider and model whose limits open over the page once it is read, empty for none
	focus, model string
	kit          *tui.Kit
	service      providerv1connect.ProviderServiceClient
	settings     configv1connect.ConfigServiceClient
	names        *names
	profile      string
	hub          *cards.Hub
	list         *cards.List
	options      *configapp.Rows
	limits       []*providerv1.Limit
	families     []string
	catalogs     []*providerv1.Catalog
	loaded       bool
	err          error
	views        cards.Watched
}

// Row of the Model list page: a limit, the row that adds one, an option, or the row that opens the advanced settings
type modelRow struct {
	limit    *providerv1.Limit
	adds     bool
	setting  *configv1.Setting
	advanced bool
}

func (m *modelList) open() tea.Cmd {
	t := m.kit.Env.Theme
	var rows []modelRow
	m.list = cards.NewList(m.kit.Env, cards.ListSpec{
		Title:   "Model list",
		Glyph:   t.Glyphs.Models,
		Key:     "models",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{"Where the list of models and their limits comes from."} },
		Columns: pageColumns,
		Rows: func() [][]string {
			rows = m.rows()
			cells := make([][]string, len(rows))
			for i, row := range rows {
				cells[i] = m.cells(row)
			}
			return cells
		},
		Group: func(i int) string {
			switch {
			case rows[i].limit != nil, rows[i].adds:
				return "Limits and abilities"
			case rows[i].setting != nil:
				return "Options"
			}
			return ""
		},
		Detail: func(i int) []string {
			switch row := rows[i]; {
			case row.limit != nil:
				return []string{"What icebath takes " + limitName(row.limit) + " to be, over what it finds."}
			case row.adds:
				return []string{"Pick a provider, then one of its models or every model of it, and set what it can do."}
			case row.setting != nil:
				return m.options.Detail(row.setting, false)
			}
			return []string{"Settings of the model list that few people change."}
		},
		Empty: func() string { return tui.Waiting(m.loaded, m.err, "") },
		Free:  map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(rows) {
				return []wm.Hint{{Action: "hub.add", Label: "add"}}
			}
			switch row := rows[i]; {
			case row.limit != nil:
				return []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.remove", Label: "remove"}, {Action: "hub.add", Label: "add"}}
			case row.adds:
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			case row.setting != nil:
				return m.options.Hints(row.setting)
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch row := rows[i]; {
				case row.limit != nil:
					return m.limitPage(row.limit.GetProvider(), row.limit.GetModel())
				case row.adds:
					return m.addLimit()
				case row.setting != nil:
					return m.options.Change(row.setting)
				}
				return m.advanced()
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				switch row := rows[i]; {
				case row.limit != nil:
					return m.removeLimit(l, row.limit)
				case row.setting != nil:
					return m.options.Reset(l, row.setting)
				}
				return nil
			},
			"hub.add": func(*cards.List, int) tea.Cmd { return m.addLimit() },
		},
	})
	m.list.ToggleDetail()
	hub, cmd := m.kit.Show(ModelList, m.list)
	m.hub = hub
	m.options = configapp.NewRows(m.kit, hub, m.settings, m.profile)
	m.options.Watch(m.list)
	return tea.Batch(cmd, m.load(), m.options.Load(func() tea.Cmd {
		if m.focus == "" {
			return nil
		}
		return m.limitPage(m.focus, m.model)
	}))
}

// Rows of the page: the limits and the row that adds one, the options of the model list and its online list, and Advanced
func (m *modelList) rows() []modelRow {
	var rows []modelRow
	for _, limit := range m.limits {
		rows = append(rows, modelRow{limit: limit})
	}
	rows = append(rows, modelRow{adds: true})
	for _, key := range []string{modelsconfig.Key, modelsconfig.CatalogKey} {
		everyday, _ := m.options.Section(key)
		for _, setting := range everyday {
			rows = append(rows, modelRow{setting: setting})
		}
	}
	return append(rows, modelRow{advanced: true})
}

// Name of a limit as a sentence says it: the model, or every model, of the provider
func limitName(limit *providerv1.Limit) string {
	if limit.GetModel() == "" {
		return "every model of " + limit.GetProviderTitle()
	}
	return limit.GetModel() + " of " + limit.GetProviderTitle()
}

func (m *modelList) cells(row modelRow) []string {
	t := m.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case row.limit != nil:
		model := cmp.Or(row.limit.GetModel(), "Every model")
		return []string{row.limit.GetProviderTitle() + " · " + model, "", render.Dim(limitWords(row.limit), t)}
	case row.adds:
		return []string{"Add a limit", opens, ""}
	case row.setting != nil:
		return m.options.Cells(row.setting)
	}
	return []string{"Advanced", opens, render.Dim(render.Count(m.rare()+1, "setting"), t)}
}

// What a limit sets, in a few words: its context window first
func limitWords(limit *providerv1.Limit) string {
	var said []string
	if n := limit.GetContextWindow(); n > 0 {
		said = append(said, strs.Count(int(n), "token"))
	}
	for _, ability := range []struct {
		on   *bool
		name string
	}{{limit.Tools, "tools"}, {limit.Vision, "images"}, {limit.Reasoning, "reasons"}} {
		switch {
		case ability.on == nil:
		case *ability.on:
			said = append(said, ability.name)
		default:
			said = append(said, "no "+ability.name)
		}
	}
	if family := limit.GetFamily(); family != "" {
		said = append(said, family)
	}
	return strings.Join(said, " · ")
}

// Settings of the model list and its online list that few people change
func (m *modelList) rare() int {
	_, own := m.options.Section(modelsconfig.Key)
	_, online := m.options.Section(modelsconfig.CatalogKey)
	return len(own) + len(online)
}

// Reads the limits and the lists of models of single kinds again
func (m *modelList) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		limits, err := m.service.Limits(m.kit.Env.Ctx, &providerv1.LimitsRequest{Profile: m.profile})
		var catalogs *providerv1.CatalogsResponse
		if err == nil {
			catalogs, err = m.service.Catalogs(m.kit.Env.Ctx, &providerv1.CatalogsRequest{Profile: m.profile})
		}
		return func() tea.Cmd {
			m.loaded, m.err = true, err
			if err == nil {
				m.limits, m.families, m.catalogs = limits.GetLimits(), limits.GetFamilies(), catalogs.GetCatalogs()
			}
			m.list.Refresh()
			m.views.Refresh()
			return nil
		}
	})
}

// Limit of the model of the provider as last read, a new one setting nothing when there is none yet
func (m *modelList) limit(provider, model string) *providerv1.Limit {
	for _, limit := range m.limits {
		if limit.GetProvider() == provider && limit.GetModel() == model {
			return limit
		}
	}
	title, _ := m.names.of(m.profile, provider)
	return &providerv1.Limit{Provider: provider, Model: model, ProviderTitle: title}
}

// Asks before a limit is removed, then removes it
func (m *modelList) removeLimit(l *cards.List, limit *providerv1.Limit) tea.Cmd {
	l.Confirm(cards.Question{Text: "Remove the limits of " + limitName(limit) + "? icebath goes by what it finds again.", Verb: "Remove"}, func() tea.Cmd {
		return tui.Call(m.hub, func() (*providerv1.RemoveLimitResponse, error) {
			return m.service.RemoveLimit(m.kit.Env.Ctx, &providerv1.RemoveLimitRequest{Profile: m.profile, Provider: limit.GetProvider(), Model: limit.GetModel()})
		}, func(*providerv1.RemoveLimitResponse) tea.Cmd {
			return tea.Batch(m.hub.PopTo(m.list), m.hub.Done("Removed the limits of "+limitName(limit)+"."), m.load())
		})
	})
	return nil
}

// Adds a limit as a job: the provider, then one of its models or every model of it; the limit's page opens with nothing set, and is kept
// once something is
func (m *modelList) addLimit() tea.Cmd {
	j := newJob(m.kit, m.hub, "Add a limit")
	return tui.Call(m.hub, func() (*providerv1.ListResponse, error) {
		return m.service.List(m.kit.Env.Ctx, &providerv1.ListRequest{Profile: m.profile})
	}, func(response *providerv1.ListResponse) tea.Cmd {
		var choices []tui.Choice
		for _, entry := range response.GetProviders() {
			choices = append(choices, tui.Choice{ID: entry.GetName(), Title: entry.GetDisplayName(), Detail: entry.GetState()})
		}
		return j.Show(m.choices(j.title, "Pick the provider whose models the limit is for.", choices, func(choice tui.Choice) tea.Cmd {
			return m.pickModel(j, choice)
		}))
	})
}

// List to pick from as a step of a job
func (m *modelList) choices(title, intro string, choices []tui.Choice, pick func(choice tui.Choice) tea.Cmd) *cards.List {
	t := m.kit.Env.Theme
	list := cards.NewList(m.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Models,
		Key:     "models:" + title,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     len(choices) > 8,
		Intro:   func() []string { return []string{intro} },
		Columns: []render.Column{{}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(choices))
			for i, choice := range choices {
				rows[i] = []string{choice.Title, render.Dim(choice.Detail, t)}
			}
			return rows
		},
		Empty: func() string { return "Add a provider first." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(choices) {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: cmp.Or(strings.ToLower(choices[i].Verb), "choose")}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{"hub.use": func(_ *cards.List, i int) tea.Cmd { return pick(choices[i]) }},
	})
	list.Refresh()
	return list
}

// Step of the job that picks one model of the provider or every model of it, a name typed when its list can't be read
func (m *modelList) pickModel(j *job, entry tui.Choice) tea.Cmd {
	m.hub.Busy("Asking " + entry.Title + " for its models…")
	return tui.Async(func() func() tea.Cmd {
		response, err := m.service.Models(m.kit.Env.Ctx, &providerv1.ModelsRequest{Profile: m.profile, Provider: entry.ID})
		return func() tea.Cmd {
			m.hub.Quiet()
			choices := []tui.Choice{{ID: "", Title: "Every model of this provider", Detail: "Those nothing else sets a limit for"}}
			for _, detail := range response.GetDetails() {
				said := ""
				if n := detail.GetContext(); n > 0 {
					said = strs.Count(int(n), "token")
				}
				choices = append(choices, tui.Choice{ID: detail.GetId(), Title: detail.GetId(), Detail: said, Verb: "Add"})
			}
			typed := err != nil || response.GetUnlisted() != ""
			if typed {
				choices = append(choices, tui.Choice{ID: "\x00", Title: "Type a model name", Detail: cmp.Or(response.GetUnlisted(), tui.Reason(err))})
			}
			return j.Show(m.choices(j.title, "Pick the model of "+entry.Title+" the limit is for.", choices, func(choice tui.Choice) tea.Cmd {
				if choice.ID != "\x00" {
					return tea.Batch(j.End(), m.limitPage(entry.ID, choice.ID))
				}
				field := cards.Field{ID: "model", Label: "Model", Help: "The name of a model that " + entry.Title + " serves, such as llama3.1:8b."}
				return j.Ask("Type the name of the model.", []cards.Field{field}, "Add", func(values map[string]string) tea.Cmd {
					return tea.Batch(j.End(), m.limitPage(entry.ID, strings.TrimSpace(values["model"])))
				})
			}))
		}
	})
}

// Fields of a limit, by name
const (
	fieldContext   = "context"
	fieldTools     = "tools"
	fieldVision    = "vision"
	fieldReasoning = "reasoning"
	fieldFamily    = "family"
	fieldRemove    = "remove"
)

// Opens the page of a limit: its context window, whether the model uses tools, sees images and reasons, its family, and removing it
func (m *modelList) limitPage(provider, model string) tea.Cmd {
	t := m.kit.Env.Theme
	fields := []string{fieldContext, fieldTools, fieldVision, fieldReasoning, fieldFamily, fieldRemove}
	current := func() *providerv1.Limit { return m.limit(provider, model) }
	list := cards.NewList(m.kit.Env, cards.ListSpec{
		Title:   current().GetProviderTitle() + " · " + cmp.Or(model, "Every model"),
		Glyph:   t.Glyphs.Models,
		Key:     "models:limit:" + provider + ":" + model,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			return []string{"What icebath takes " + limitName(current()) + " to be, over what it finds."}
		},
		Columns: pageColumns,
		Rows: func() [][]string {
			limit := current()
			return [][]string{{"Context window", "", windowWords(limit.GetContextWindow())}, {"Uses tools", "", ability(limit.Tools)},
				{"Sees images", "", ability(limit.Vision)}, {"Reasons", "", ability(limit.Reasoning)},
				{"Model family", "", cmp.Or(limit.GetFamily(), "Automatic")}, {"Remove", "", ""}}
		},
		Detail: func(i int) []string {
			return []string{map[string]string{fieldContext: "How much text the model can read at once.", fieldTools: "Whether the model can call tools.",
				fieldVision: "Whether the model can look at images.", fieldReasoning: "Whether the model thinks before it answers.",
				fieldFamily: "Which family of models it belongs to, which shapes some requests.",
				fieldRemove: "icebath goes by what it finds of the model again."}[fields[i]]}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0 || i >= len(fields):
				return nil
			case fields[i] == fieldRemove:
				return []wm.Hint{{Action: "hub.use", Label: "remove"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{"hub.use": func(l *cards.List, i int) tea.Cmd { return m.changeLimit(l, current(), fields[i]) }},
	})
	list.ToggleDetail()
	list.Refresh()
	m.views.Add(list)
	return m.hub.Push(list)
}

// Context window in words, Automatic for none
func windowWords(n int64) string {
	if n <= 0 {
		return "Automatic"
	}
	return strs.Count(int(n), "token")
}

// An ability a limit sets, in words
func ability(on *bool) string {
	switch {
	case on == nil:
		return "Automatic"
	case *on:
		return "Yes"
	}
	return "No"
}

// Changes one field of a limit with the control it takes: the context window typed, an ability picked, the family picked from the
// families icebath knows, or typed while it knows none
func (m *modelList) changeLimit(l *cards.List, limit *providerv1.Limit, field string) tea.Cmd {
	next := &providerv1.Limit{Provider: limit.GetProvider(), Model: limit.GetModel(), ContextWindow: limit.GetContextWindow(), Tools: limit.Tools,
		Vision: limit.Vision, Reasoning: limit.Reasoning, Family: limit.GetFamily()}
	switch field {
	case fieldRemove:
		return m.removeLimit(l, limit)
	case fieldContext:
		input := cards.Field{Label: "Context window", Select: true, Value: map[bool]string{true: strconv.FormatInt(limit.GetContextWindow(), 10)}[limit.GetContextWindow() > 0],
			Help: "Tokens the model reads at once, such as 128000. Left empty, icebath goes by what it finds.", Check: func(text string) string {
				if n, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(text), ",", ""), 10, 64); strings.TrimSpace(text) != "" && (err != nil || n < 0) {
					return "Type a whole number of tokens."
				}
				return ""
			}}
		// An empty answer takes the window away, as its field says
		return m.kit.AskAny(m.hub, "Context window", input, "Save", func(text string) tea.Cmd {
			next.ContextWindow, _ = strconv.ParseInt(strings.ReplaceAll(text, ",", ""), 10, 64)
			return m.keep(next)
		})
	case fieldFamily:
		if len(m.families) == 0 {
			input := cards.Field{Label: "Model family", Value: limit.GetFamily(), Select: true,
				Help: "The family the model belongs to, such as claude-sonnet. Left empty, icebath goes by what it finds."}
			return m.kit.AskAny(m.hub, "Model family", input, "Save", func(text string) tea.Cmd { next.Family = text; return m.keep(next) })
		}
		choices := []tui.Choice{{ID: "", Title: "Automatic", Detail: "What icebath finds"}}
		for _, family := range m.families {
			choices = append(choices, tui.Choice{ID: family, Title: family})
		}
		return m.kit.PickFrom(m.hub, "Model family", []string{"Which family of models it belongs to."}, "choose", choices, limit.GetFamily(),
			func(choice tui.Choice) tea.Cmd { next.Family = choice.ID; return m.keep(next) })
	}
	target := map[string]**bool{fieldTools: &next.Tools, fieldVision: &next.Vision, fieldReasoning: &next.Reasoning}[field]
	held := map[bool]string{true: "yes", false: "no"}
	current := ""
	if *target != nil {
		current = held[**target]
	}
	choices := []tui.Choice{{ID: "", Title: "Automatic", Detail: "What icebath finds"}, {ID: "yes", Title: "Yes"}, {ID: "no", Title: "No"}}
	title := map[string]string{fieldTools: "Uses tools", fieldVision: "Sees images", fieldReasoning: "Reasons"}[field]
	return m.kit.PickFrom(m.hub, title, nil, "choose", choices, current, func(choice tui.Choice) tea.Cmd {
		*target = nil
		if choice.ID != "" {
			on := choice.ID == "yes"
			*target = &on
		}
		return m.keep(next)
	})
}

// Keeps a limit, checked by the server on the way in, and says what it is now
func (m *modelList) keep(limit *providerv1.Limit) tea.Cmd {
	return tui.Call(m.hub, func() (*providerv1.SetLimitResponse, error) {
		return m.service.SetLimit(m.kit.Env.Ctx, &providerv1.SetLimitRequest{Profile: m.profile, Limit: limit})
	}, func(set *providerv1.SetLimitResponse) tea.Cmd {
		said := cmp.Or(limitWords(set.GetLimit()), "what icebath finds")
		return tea.Batch(m.hub.Done("The limits of "+limitName(m.limit(limit.GetProvider(), limit.GetModel()))+" are now "+said+"."), m.load())
	})
}

// Opens the advanced settings of the model list: those of the model list and its online list, then the lists of models of single kinds
func (m *modelList) advanced() tea.Cmd {
	t := m.kit.Env.Theme
	var rows []*configv1.Setting
	list := cards.NewList(m.kit.Env, cards.ListSpec{
		Title:   "Advanced",
		Glyph:   t.Glyphs.Models,
		Key:     "models:advanced",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{"Settings of the model list that few people change."} },
		Columns: pageColumns,
		Rows: func() [][]string {
			_, own := m.options.Section(modelsconfig.Key)
			_, online := m.options.Section(modelsconfig.CatalogKey)
			rows = slices.Concat(online, own)
			cells := make([][]string, 0, len(rows)+1)
			for _, setting := range rows {
				cells = append(cells, m.options.Cells(setting))
			}
			said := "None"
			if len(m.catalogs) > 0 {
				said = render.Count(len(m.catalogs), "catalog")
			}
			return append(cells, []string{"Provider catalogs", render.Dim(t.Glyphs.Forward, t), render.Dim(said, t)})
		},
		Detail: func(i int) []string {
			if i < len(rows) {
				return m.options.Detail(rows[i], false)
			}
			return []string{"Lists of models for single kinds of provider, used over the main list."}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i >= 0 && i < len(rows) {
				return m.options.Hints(rows[i])
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i < len(rows) {
					return m.options.Change(rows[i])
				}
				return m.catalogList()
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i < len(rows) {
					return m.options.Reset(l, rows[i])
				}
				return nil
			},
		},
	})
	list.ToggleDetail()
	list.Refresh()
	m.options.Watch(list)
	m.views.Add(list)
	return m.hub.Push(list)
}

// Opens the lists of models of single kinds: a row per kind with its address, then the row that adds one
func (m *modelList) catalogList() tea.Cmd {
	t := m.kit.Env.Theme
	list := cards.NewList(m.kit.Env, cards.ListSpec{
		Title:   "Provider catalogs",
		Glyph:   t.Glyphs.Models,
		Key:     "models:catalogs",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			return []string{"Lists of models for single kinds of provider, used over the main list."}
		},
		Columns: pageColumns,
		Rows: func() [][]string {
			cells := make([][]string, 0, len(m.catalogs)+1)
			for _, catalog := range m.catalogs {
				cells = append(cells, []string{catalog.GetTitle(), "", render.Dim(catalog.GetAddress(), t)})
			}
			return append(cells, []string{"Add a provider catalog", render.Dim(t.Glyphs.Forward, t), ""})
		},
		Detail: func(i int) []string {
			if i < len(m.catalogs) {
				return []string{"Where the list of models of " + m.catalogs[i].GetTitle() + " is read."}
			}
			return []string{"Pick a kind of provider and give the address of its list of models."}
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i >= 0 && i < len(m.catalogs) {
				return []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.remove", Label: "remove"}, {Action: "hub.add", Label: "add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "add"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i < len(m.catalogs) {
					return m.catalogAddress(nil, m.catalogs[i].GetKind(), m.catalogs[i].GetTitle(), m.catalogs[i].GetAddress())
				}
				return m.addCatalog()
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i >= len(m.catalogs) {
					return nil
				}
				catalog := m.catalogs[i]
				l.Confirm(cards.Question{Text: "Remove the catalog of " + catalog.GetTitle() + "? Its models come from the main list again.", Verb: "Remove"},
					func() tea.Cmd {
						return tui.Call(m.hub, func() (*providerv1.RemoveCatalogResponse, error) {
							return m.service.RemoveCatalog(m.kit.Env.Ctx, &providerv1.RemoveCatalogRequest{Profile: m.profile, Kind: catalog.GetKind()})
						}, func(*providerv1.RemoveCatalogResponse) tea.Cmd {
							return tea.Batch(m.hub.Done("Removed the catalog of "+catalog.GetTitle()+"."), m.load())
						})
					})
				return nil
			},
			"hub.add": func(*cards.List, int) tea.Cmd { return m.addCatalog() },
		},
	})
	list.ToggleDetail()
	list.Refresh()
	m.views.Add(list)
	return m.hub.Push(list)
}

// Adds a catalog as a job: the kind of provider, picked from the kinds, then the address of its list
func (m *modelList) addCatalog() tea.Cmd {
	j := newJob(m.kit, m.hub, "Add a provider catalog")
	return tui.Call(m.hub, func() (*providerv1.SpecsResponse, error) {
		return m.service.Specs(m.kit.Env.Ctx, &providerv1.SpecsRequest{Profile: m.profile})
	}, func(response *providerv1.SpecsResponse) tea.Cmd {
		var choices []tui.Choice
		for _, spec := range response.GetSpecs() {
			choices = append(choices, tui.Choice{ID: spec.GetName(), Title: spec.GetDisplayName(), Detail: spec.GetDescription()})
		}
		slices.SortStableFunc(choices, func(a, b tui.Choice) int { return strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)) })
		return j.Show(m.choices(j.title, "Pick the kind of provider whose models the list holds.", choices, func(choice tui.Choice) tea.Cmd {
			return m.catalogAddress(j, choice.ID, choice.Title, "")
		}))
	})
}

// Asks the address of the list of models of a kind, as the last step of a job or on its own, and keeps it
func (m *modelList) catalogAddress(j *job, kind, title, address string) tea.Cmd {
	field := cards.Field{ID: "address", Label: "List address", Value: address, Select: address != "", Check: checkAddress,
		Help: "Where the list of models of " + title + " is read, such as https://example.com/models.json."}
	keep := func(text string) tea.Cmd {
		return tui.Call(m.hub, func() (*providerv1.SetCatalogResponse, error) {
			return m.service.SetCatalog(m.kit.Env.Ctx, &providerv1.SetCatalogRequest{Profile: m.profile,
				Catalog: &providerv1.Catalog{Kind: kind, Address: strings.TrimSpace(text)}})
		}, func(*providerv1.SetCatalogResponse) tea.Cmd {
			cmds := []tea.Cmd{m.hub.Done("The models of " + title + " now come from " + strings.TrimSpace(text) + "."), m.load()}
			if j != nil {
				cmds = append(cmds, j.End())
			}
			return tea.Batch(cmds...)
		})
	}
	if j == nil {
		return m.kit.Ask(m.hub, "List address", field, "Save", keep)
	}
	return j.Ask("The address of the list of models of "+title+".", []cards.Field{field}, "Add", func(values map[string]string) tea.Cmd {
		return keep(values["address"])
	})
}
