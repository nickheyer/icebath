package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Provider id of an endpoint the person describes by its address
const customProvider = "custom"

// Get started screen: one list of providers, signed in ones first, and a row for an endpoint of your own; picking one adds a login when
// it has none, then picks its model, which becomes the model of every session; nothing is installed and no question is asked twice
func (a *app) openGetStarted() tea.Cmd {
	var (
		providers []tuigateway.ModelOptionProvider
		h         *cards.Hub
		list      *cards.List
		loaded    bool
	)
	var reload func(then func() tea.Cmd) tea.Cmd
	reload = func(then func() tea.Cmd) tea.Cmd {
		params := tuigateway.ModelOptionsParams{SessionID: a.store.ui.Sid, IncludeUnconfigured: true, Refresh: true}
		return hubCall(a, h, tr(txt.HubsLoading), tuigateway.ModelOptions, params, func(r *tuigateway.ModelOptionsResult) tea.Cmd {
			providers, loaded = r.Providers, true
			slices.SortStableFunc(providers, func(x, y tuigateway.ModelOptionProvider) int {
				if x.Unauthenticated() != y.Unauthenticated() {
					if x.Unauthenticated() {
						return 1
					}
					return -1
				}
				return 0
			})
			list.Refresh()
			if then != nil {
				return then()
			}
			return nil
		})
	}
	find := func(slug string) (tuigateway.ModelOptionProvider, bool) {
		for _, p := range providers {
			if p.Slug == slug {
				return p, true
			}
		}
		return tuigateway.ModelOptionProvider{}, false
	}
	// Picks the model of a provider and makes it the model of every session, a session starting when none is open
	var pickModel func(slug string) tea.Cmd
	pickModel = func(slug string) tea.Cmd {
		p, ok := find(slug)
		if !ok {
			return h.Fail(tr(txt.HubsStartNoProvider, slug))
		}
		if len(p.Models) == 0 {
			return h.Fail(tr(txt.HubsStartNoModels, providerName(p)))
		}
		var choices []hubChoice
		for _, m := range p.Models {
			detail := ""
			if price, ok := p.Pricing[m]; ok && price.Input != "" {
				detail = tr(txt.HubsStartPrice, price.Input, price.Output)
			}
			choices = append(choices, hubChoice{id: m, title: m, detail: detail})
		}
		featured := ""
		if len(p.FeaturedModels) > 0 {
			featured = p.FeaturedModels[0]
		}
		return a.hubPick(h, tr(txt.HubsStartPickModel, providerName(p)), choices, featured, func(c hubChoice) tea.Cmd {
			set := func(key, value string, then func() tea.Cmd) tea.Cmd {
				params := tuigateway.SettingsSetParams{Key: key, Value: value, Scope: ptr("global")}
				return hubCall(a, h, tr(txt.HubsStartSaving), tuigateway.SettingsSet, params, func(*tuigateway.SettingsWriteResult) tea.Cmd { return then() })
			}
			finish := func() tea.Cmd {
				cmds := []tea.Cmd{wm.RequestDismissOf(h), wm.Notify(tr(txt.HubsStartReady, c.id, providerName(p)), wm.Success)}
				// A window without a session starts one on the new model; one that has a session switches it
				if a.store.ui.Sid == "" {
					a.store.ui.SetupRequired = false
					cmds = append(cmds, a.newSession(nil))
				} else {
					cmds = append(cmds, a.slash("/model "+c.id+" --provider "+slug, nil, false))
				}
				return tea.Batch(cmds...)
			}
			return set("model.provider", slug, func() tea.Cmd { return set("model.default", c.id, finish) })
		})
	}
	use := func(_ *cards.List, i int) tea.Cmd {
		if i == len(providers) {
			return a.addEndpoint(h, func() tea.Cmd { return reload(func() tea.Cmd { return pickModel(customProvider) }) })
		}
		if i < 0 || i > len(providers) {
			return nil
		}
		p := providers[i]
		if !p.Unauthenticated() {
			return pickModel(p.Slug)
		}
		slug := p.Slug
		return a.addLogin(h, slug, true, func() tea.Cmd { return reload(func() tea.Cmd { return pickModel(slug) }) })
	}
	t := a.toolkit
	list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleGetStarted), Glyph: t.Glyphs.Brand, Key: "tui:start", Context: hubPickContext, Primary: "hub.pick",
		Columns: []render.Column{{Title: ""}, {Title: tr(txt.HubsColProvider), Max: 40}, {Title: tr(txt.HubsColStatus), Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, 0, len(providers)+1)
			for _, p := range providers {
				mark, state := "●", tr(txt.HubsStartSignedIn, len(p.Models))
				if p.Unauthenticated() {
					mark, state = "○", tr(txt.HubsStartNeedsLogin)
				}
				out = append(out, []string{mark, providerName(p), state})
			}
			if loaded {
				out = append(out, []string{"+", tr(txt.HubsStartEndpoint), tr(txt.HubsStartEndpointDetail)})
			}
			return out
		},
		Banner: func() string { return tr(txt.HubsStartIntro) },
		Help:   func(int) []string { return []string{tr(txt.HubsStartIntro), tr(txt.HubsStartHelp)} },
		Empty:  func() string { return tr(txt.HubsLoading) },
		Pick:   true,
		Hints:  func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.pick", Label: tr(txt.HintsLabelUse)}} },
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.pick": use,
		},
	})
	h = a.newHub("tui:start", list)
	return tea.Batch(a.wm.ShowOverlay(h), reload(nil))
}

// Opens the get started screen when a session reports that no model is set up, unless it is already up
func (a *app) offerGetStarted() tea.Cmd {
	if h, ok := a.wm.Overlay().(*cards.Hub); ok && h.Key() == "tui:start" {
		return nil
	}
	return a.openGetStarted()
}

// Describes an endpoint of your own, OpenAI compatible: its address and an optional key; the model comes from what the endpoint lists
func (a *app) addEndpoint(h *cards.Hub, done func() tea.Cmd) tea.Cmd {
	spec := cards.FormSpec{Title: tr(txt.HubsStartEndpoint), Intro: []string{tr(txt.HubsStartEndpointIntro)}, Submit: tr(txt.HubsStartEndpointSubmit), Fields: []cards.Field{
		{ID: "url", Label: tr(txt.HubsStartAddressLabel), Kind: cards.FieldText, Required: true, Placeholder: "http://localhost:11434/v1", Help: tr(txt.HubsStartAddressHelp),
			Check: func(v string) string {
				if v = strings.TrimSpace(v); !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
					return tr(txt.HubsStartAddressBad)
				}
				return ""
			}},
		{ID: "key", Label: tr(txt.HubsLoginsKeyLabel), Kind: cards.FieldSecret, Help: tr(txt.HubsStartKeyHelp)},
	}}
	return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		url := strings.TrimRight(strings.TrimSpace(v["url"]), "/")
		set := func(key string, value any, then func() tea.Cmd) tea.Cmd {
			return hubCall(a, h, tr(txt.HubsStartSaving), tuigateway.SettingsSet, tuigateway.SettingsSetParams{Key: key, Value: value}, func(*tuigateway.SettingsWriteResult) tea.Cmd { return then() })
		}
		saveKey := func() tea.Cmd {
			key := strings.TrimSpace(v["key"])
			if key == "" {
				return done()
			}
			params := tuigateway.LoginAddKeyParams{Provider: customProvider, APIKey: key, MakeDefault: true}
			return hubCall(a, h, tr(txt.HubsLoginsSaving), tuigateway.LoginAddKey, params, func(*tuigateway.LoginResult) tea.Cmd { return done() })
		}
		return set("model.base_url", url, func() tea.Cmd { return set("model.provider", customProvider, saveKey) })
	})
}
