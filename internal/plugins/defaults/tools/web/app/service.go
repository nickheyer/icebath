package app

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	webv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/web/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Ids of what a service is used for, as a choice holds them
const (
	forBoth   = "both"
	forSearch = "search"
	forPages  = "pages"
)

// What each choice of what a service is used for asks the server for
var useOf = map[string]webv1.Use{forBoth: webv1.Use_USE_BOTH, forSearch: webv1.Use_USE_SEARCH, forPages: webv1.Use_USE_PAGES}

// Row of the page of a service: what it is used for, its plan, signing in, an option, or the row of its advanced options
type serviceRow struct {
	use, plan, signIn bool
	setting           *configv1.Setting
	advanced          bool
}

// What a service can be used for, as choices, and the one it is chosen for now; none while it is not chosen
func (p *page) uses(s *webv1.Backend) ([]tui.Choice, string) {
	title := s.GetTitle()
	var choices []tui.Choice
	if s.GetSearch() && s.GetExtract() {
		choices = append(choices, tui.Choice{ID: forBoth, Title: "Searches and pages", Detail: title + " answers searches and reads pages"})
	}
	if s.GetSearch() {
		choices = append(choices, tui.Choice{ID: forSearch, Title: "Only searches", Detail: title + " answers searches, and another service reads pages"})
	}
	if s.GetExtract() {
		choices = append(choices, tui.Choice{ID: forPages, Title: "Only pages", Detail: title + " reads pages, and another service answers searches"})
	}
	// What it is chosen for by what the settings come to: the service chosen for one thing alone wins over the one chosen for both
	name := s.GetName()
	both := p.state.GetBackend() == name
	searches := p.state.GetSearchBackend() == name || both && p.state.GetSearchBackend() == ""
	pages := p.state.GetExtractBackend() == name || both && p.state.GetExtractBackend() == ""
	switch {
	case searches && pages && s.GetSearch() && s.GetExtract():
		return choices, forBoth
	case searches && s.GetSearch():
		return choices, forSearch
	case pages && s.GetExtract():
		return choices, forPages
	}
	return choices, ""
}

// Title of the choice in use, Not chosen for none
func usedWords(choices []tui.Choice, current string) string {
	for _, c := range choices {
		if c.ID == current {
			return c.Title
		}
	}
	return "Not chosen"
}

// First plan of a service that needs a key or an address it lacks, nil when it needs none
func needing(s *webv1.Backend) *webv1.Plan {
	for _, plan := range s.GetPlans() {
		if plan.GetKey() != "" && plan.GetTier() != webconfig.Free {
			return plan
		}
	}
	return nil
}

// Asks what a ready service is used for, from the list of what it can do; a service that is not ready asks its key or address first, in
// a job of two steps that writes both only once the second is answered
func (p *page) useFor(name string) tea.Cmd {
	s := p.find(name)
	if s == nil {
		return nil
	}
	choices, current := p.uses(s)
	intro := []string{"What " + s.GetTitle() + " does for the agent."}
	pick := func(choice tui.Choice) tea.Cmd {
		return p.choose(&webv1.ChooseRequest{Backend: name, Use: useOf[choice.ID]})
	}
	plan := needing(s)
	if s.GetUsable() || plan == nil {
		return p.kit.PickFrom(p.hub, "Use it for", intro, "choose", choices, current, pick)
	}
	f := p.kit.Flow(p.hub, "Use it for")
	return f.Ask("", []cards.Field{keyField(s, plan)}, "Next", func(values map[string]string) tea.Cmd {
		typed := strings.TrimSpace(values["value"])
		return f.PickVerb(intro[0], choices, current, "Save", func(choice tui.Choice) tea.Cmd {
			return p.keyed(plan.GetKey(), typed, &webv1.ChooseRequest{Backend: name, Use: useOf[choice.ID]}, f.End)
		})
	})
}

// Field of the key or the address a plan asks for, its line saying what to type and where to get it
func keyField(s *webv1.Backend, plan *webv1.Plan) cards.Field {
	field := cards.Field{ID: "value", Label: "API key", Kind: cards.FieldSecret, Required: true}
	if plan.GetNeeds() == webv1.Needs_NEEDS_ADDRESS {
		field.Label, field.Kind = "Address", cards.FieldText
	}
	var help []string
	if prompt := plan.GetPrompt(); prompt != "" {
		help = append(help, strings.TrimSuffix(prompt, ".")+".")
	}
	if link := plan.GetLink(); link != "" {
		help = append(help, "Get one at "+link+".")
	}
	if len(help) == 0 && field.Kind == cards.FieldSecret {
		help = append(help, "Your "+s.GetTitle()+" API key.")
	} else if len(help) == 0 {
		help = append(help, "The address of your "+s.GetTitle()+" server, such as http://localhost:8080.")
	}
	field.Help = strings.Join(help, " ")
	return field
}

// Keeps the key typed under its setting, then the choice, then ends the job; a refusal of either is said and nothing more is written
func (p *page) keyed(key, typed string, request *webv1.ChooseRequest, end func() tea.Cmd) tea.Cmd {
	request.Profile = p.profile
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*webv1.ChooseResponse, error) {
		ctx := p.kit.Env.Ctx
		if _, err := p.settings.Set(ctx, &configv1.SetRequest{Profile: p.profile, Key: key, Value: strconv.Quote(typed)}); err != nil {
			return nil, err
		}
		return p.service.Choose(ctx, request)
	}, func(response *webv1.ChooseResponse) tea.Cmd {
		return tea.Batch(p.hub.Done(response.GetNote()), end(), p.load(p.changed))
	})
}

// Plans of a service a person picks between: those that pin a tier, while there are two or more
func plans(s *webv1.Backend) []*webv1.Plan {
	var out []*webv1.Plan
	for _, plan := range s.GetPlans() {
		if plan.GetTier() != "" {
			out = append(out, plan)
		}
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

// Title of the plan the service is on, Automatic while its key decides
func planTitle(s *webv1.Backend) string {
	for _, plan := range plans(s) {
		if plan.GetTier() == s.GetTier() {
			return plan.GetTitle()
		}
	}
	return "Automatic"
}

// Asks the plan of a service; a plan that needs a key the service lacks asks it first, in a job of two steps
func (p *page) planPick(name string) tea.Cmd {
	s := p.find(name)
	if s == nil {
		return nil
	}
	choices := []tui.Choice{{ID: webconfig.Auto, Title: "Automatic", Detail: "The free plan without a key, your own plan with one", Verb: "Save"}}
	current := webconfig.Auto
	for _, plan := range plans(s) {
		// A plan kept at once ends the job; one that needs a key the service lacks asks it next
		verb := "Save"
		if plan.GetKey() != "" && plan.GetTier() != webconfig.Free && !s.GetReady() {
			verb = ""
		}
		choices = append(choices, tui.Choice{ID: plan.GetTier(), Title: plan.GetTitle(), Detail: plan.GetDescription(), Verb: verb})
		if plan.GetTier() == s.GetTier() {
			current = plan.GetTier()
		}
	}
	intro := "Which plan of " + s.GetTitle() + " the agent uses."
	f := p.kit.Flow(p.hub, "Plan")
	return f.Pick(intro, choices, current, func(choice tui.Choice) tea.Cmd {
		request := &webv1.ChooseRequest{Backend: name, Plan: choice.ID}
		for _, plan := range plans(s) {
			if plan.GetTier() == choice.ID && plan.GetKey() != "" && choice.ID != webconfig.Free && !s.GetReady() {
				return f.Ask("", []cards.Field{keyField(s, plan)}, "Save", func(values map[string]string) tea.Cmd {
					return p.keyed(plan.GetKey(), strings.TrimSpace(values["value"]), request, f.End)
				})
			}
		}
		request.Profile = p.profile
		p.hub.Busy("Saving…")
		return tui.Call(p.hub, func() (*webv1.ChooseResponse, error) {
			return p.service.Choose(p.kit.Env.Ctx, request)
		}, func(response *webv1.ChooseResponse) tea.Cmd {
			return tea.Batch(p.hub.Done(response.GetNote()), f.End(), p.load(p.changed))
		})
	})
}

// Opens the page of a service over the first view: how it stands as a sentence, what it is used for, its plan, its key or address and its
// other options
func (p *page) servicePage(name string) tea.Cmd {
	s := p.find(name)
	if s == nil {
		return nil
	}
	t := p.kit.Env.Theme
	var shown []serviceRow
	var view *cards.List
	// A view opened over the page, whose change the service is read again for once it closes
	opened := false
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		OnFocus: func(*cards.List) tea.Cmd {
			if !opened || p.hub.Top() != view {
				return nil
			}
			opened = false
			return p.load(nil)
		},
		Title:   s.GetTitle(),
		Glyph:   t.Glyphs.Fetch,
		Key:     Screen + ":" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return p.serviceState(name) },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.serviceRows(name)
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = p.serviceCells(name, r)
			}
			return cells
		},
		Group: func(i int) string {
			if shown[i].setting != nil || shown[i].advanced {
				return "Options"
			}
			return ""
		},
		Detail: func(i int) []string { return p.serviceDetail(name, shown[i], view.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, s.GetTitle()+" is no longer there.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			switch r := shown[i]; {
			case r.use, r.plan:
				return []wm.Hint{{Action: "hub.use", Label: "change"}}
			case r.signIn, r.advanced:
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			}
			return p.options.Hints(shown[i].setting)
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch r := shown[i]; {
				case r.use:
					return p.useFor(name)
				case r.plan:
					return p.planPick(name)
				case r.signIn:
					if now := p.find(name); now != nil {
						return p.kit.Open(providerapp.Screen, profile.Join(p.profile, providerapp.LoginsOf+signIn(now)))
					}
				case r.setting != nil:
					opened = true
					return p.options.Change(r.setting)
				case r.advanced:
					if _, _, open := p.serviceOptions(name); open != nil {
						opened = true
						return open()
					}
				}
				return nil
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if setting := shown[i].setting; setting != nil {
					return p.options.Reset(l, setting)
				}
				return nil
			},
		},
	})
	view.ToggleDetail()
	p.watch(view)
	return p.hub.Push(view)
}

// Kind of provider a service signs in at, empty for none
func signIn(s *webv1.Backend) string {
	for _, plan := range s.GetPlans() {
		if plan.GetSignIn() != "" {
			return plan.GetSignIn()
		}
	}
	return ""
}

// First lines of the page of a service: how it stands as a sentence
func (p *page) serviceState(name string) []string {
	s := p.find(name)
	if s == nil {
		return nil
	}
	t := p.kit.Env.Theme
	switch {
	case p.usedFor(s) != "":
		return []string{render.Good("In use. "+p.usedFor(s), t)}
	case s.GetReady():
		return []string{"Ready. Use it for picks what it does for the agent."}
	case s.GetUsable():
		return []string{"Free to use. Use it for picks what it does for the agent."}
	}
	switch s.GetState() {
	case "Needs an API key":
		return []string{render.Warm("Needs an API key before it can answer.", t)}
	case "Needs a server address":
		return []string{render.Warm("Needs the address of your server before it can answer.", t)}
	case "Needs a sign-in":
		return []string{render.Warm("Needs a sign-in before it can answer.", t)}
	}
	return []string{render.Warm(s.GetState()+".", t)}
}

// Rows of the page of a service
func (p *page) serviceRows(name string) []serviceRow {
	s := p.find(name)
	if s == nil {
		return nil
	}
	rows := []serviceRow{{use: true}}
	if len(plans(s)) > 0 {
		rows = append(rows, serviceRow{plan: true})
	}
	if signIn(s) != "" {
		rows = append(rows, serviceRow{signIn: true})
	}
	shown, advanced, _ := p.serviceOptions(name)
	for _, setting := range shown {
		rows = append(rows, serviceRow{setting: setting})
	}
	if advanced != nil {
		rows = append(rows, serviceRow{advanced: true})
	}
	return rows
}

// Options of a service: its key or address first, then the other settings of its own section alone, and the cells of the row of the
// advanced ones with what it opens; the advanced ones in place, no row, while they are few
func (p *page) serviceOptions(name string) (shown []*configv1.Setting, advanced []string, open func() tea.Cmd) {
	s := p.find(name)
	if s == nil {
		return nil, nil, nil
	}
	var keys []string
	for _, plan := range s.GetPlans() {
		if key := plan.GetKey(); key != "" && !slices.Contains(keys, key) {
			keys = append(keys, key)
			if setting := p.options.Setting(key); setting != nil && !setting.GetCustom() && !setting.GetInternal() {
				shown = append(shown, setting)
			}
		}
	}
	if s.GetSection() == "" {
		return shown, nil, nil
	}
	everyday, rare := p.options.Own(s.GetSection(), keys...)
	shown = append(shown, everyday...)
	if cells, open, ok := p.options.OwnAdvanced(s.GetSection(), keys...); ok {
		return shown, cells, open
	}
	return append(shown, rare...), nil, nil
}

func (p *page) serviceCells(name string, r serviceRow) []string {
	s := p.find(name)
	t := p.kit.Env.Theme
	switch {
	case r.use:
		choices, current := p.uses(s)
		return []string{"Use it for", "", usedWords(choices, current)}
	case r.plan:
		return []string{"Plan", "", planTitle(s)}
	case r.signIn:
		return []string{"Sign in", render.Dim(t.Glyphs.Forward, t), ""}
	case r.setting != nil:
		return p.options.Cells(r.setting)
	}
	_, advanced, _ := p.serviceOptions(name)
	return advanced
}

// Lines under the rows of the page of a service about the row in hand
func (p *page) serviceDetail(name string, r serviceRow, cut bool) []string {
	s := p.find(name)
	if s == nil {
		return nil
	}
	t := p.kit.Env.Theme
	switch {
	case r.use:
		lines := []string{"Searches and pages, only searches, or only pages."}
		if needing(s) != nil && !s.GetUsable() {
			lines = append(lines, render.Dim("It asks for what "+s.GetTitle()+" needs first.", t))
		}
		return lines
	case r.plan:
		return []string{"Which plan of " + s.GetTitle() + " the agent uses."}
	case r.signIn:
		return []string{"Sign in at the provider whose account " + s.GetTitle() + " runs on."}
	case r.setting != nil:
		return p.options.Detail(r.setting, cut)
	}
	return []string{"Settings of " + s.GetTitle() + " that few people change."}
}
