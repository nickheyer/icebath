package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// One section of the setup flow and the window that configures it
type setupSection struct {
	key    string
	title  string
	detail string
	open   func(a *app) tea.Cmd
}

// Sections in the order the setup menu lists them
func setupSections() []setupSection {
	sections := []setupSection{
		{"model", tr("setupFlow.model.title"), tr("setupFlow.model.detail"), (*app).openProviderSetup},
	}
	for _, spec := range []sectionSpec{agentSection(), ttsSection(), telemetrySection()} {
		sections = append(sections, setupSection{spec.key, spec.title, spec.detail, func(a *app) tea.Cmd { return a.runSection(spec) }})
	}
	return sections
}

// Opens the named section, the menu of every section when none is named
func (a *app) openSetup(section string) tea.Cmd {
	sections := setupSections()
	if section != "" {
		keys := make([]string, len(sections))
		for i, s := range sections {
			if s.key == section {
				return s.open(a)
			}
			keys[i] = s.key
		}
		a.sys(tr("setupFlow.unknownSection", section, strings.Join(keys, ", ")))
		return nil
	}
	items := make([]wm.Item, len(sections))
	for i, s := range sections {
		items[i] = wm.Item{ID: "setup:" + s.key, Title: s.title, Detail: s.detail, Run: func() tea.Cmd { return s.open(a) }}
	}
	p := wm.NewPalette(a.toolkit, items)
	p.Placeholder = tr("setupFlow.placeholder")
	return a.wm.ShowOverlay(p)
}

// Provider chooser, a custom endpoint row above every connected provider and every provider that takes a pasted key
func (a *app) openProviderSetup() tea.Cmd {
	a.store.ui.Status = tr("setupFlow.model.loading")
	return a.events.rpc("model.options", map[string]any{"include_unconfigured": true}, func(r map[string]any) tea.Cmd {
		a.store.ui.Status = a.store.statusFromBusy()
		if r == nil {
			return nil
		}
		items := []wm.Item{{ID: "setup:custom", Title: tr("setupFlow.custom.title"), Detail: tr("setupFlow.custom.detail"), Run: a.promptCustomEndpoint}}
		providers, _ := r["providers"].([]any)
		for _, raw := range providers {
			row, _ := raw.(map[string]any)
			slug, _ := row["slug"].(string)
			name, _ := row["name"].(string)
			if name == "" {
				name = slug
			}
			keyEnv, _ := row["key_env"].(string)
			switch {
			case row["authenticated"] != false:
				models := stringList(row["models"])
				items = append(items, wm.Item{ID: "setup:provider:" + slug, Title: "● " + name, Detail: tr("setupFlow.model.chooseDefault"), Run: func() tea.Cmd {
					return a.pickDefaultModel(slug, name, models, "", "")
				}})
			case keyEnv != "":
				items = append(items, wm.Item{ID: "setup:provider:" + slug, Title: "○ " + name, Detail: tr("setupFlow.model.pasteKey", keyEnv), Run: func() tea.Cmd {
					return a.promptProviderKey(slug, name, keyEnv)
				}})
			}
		}
		p := wm.NewPalette(a.toolkit, items)
		p.Placeholder = tr("setupFlow.model.placeholder")
		return a.wm.ShowOverlay(p)
	})
}

// Prompts for the API key of a provider, saves it and moves on to its models
func (a *app) promptProviderKey(slug, name, keyEnv string) tea.Cmd {
	return a.wm.ShowOverlay(wm.NewPrompt(a.toolkit, tr("setupFlow.model.keyPrompt", keyEnv, name), "", func(key string) tea.Cmd {
		if trimJS(key) == "" {
			return a.openProviderSetup()
		}
		return a.events.rpc("model.save_key", map[string]any{"slug": slug, "api_key": trimJS(key)}, func(res map[string]any) tea.Cmd {
			if res == nil {
				return nil
			}
			a.sys(tr("setupFlow.model.connected", name))
			saved, _ := res["provider"].(map[string]any)
			return a.pickDefaultModel(slug, name, stringList(saved["models"]), "", "")
		})
	}))
}

// Prompts for the base URL and key of an OpenAI compatible endpoint, then lists the models it serves
func (a *app) promptCustomEndpoint() tea.Cmd {
	return a.wm.ShowOverlay(wm.NewPrompt(a.toolkit, tr("setupFlow.custom.urlPrompt"), "", func(url string) tea.Cmd {
		baseURL := trimJS(url)
		if baseURL == "" {
			return a.openProviderSetup()
		}
		return a.wm.ShowOverlay(wm.NewPrompt(a.toolkit, tr("setupFlow.custom.keyPrompt", baseURL), "", func(key string) tea.Cmd {
			apiKey := trimJS(key)
			a.store.ui.Status = tr("setupFlow.custom.probing")
			return a.events.rpc("model.probe", map[string]any{"base_url": baseURL, "api_key": apiKey}, func(res map[string]any) tea.Cmd {
				a.store.ui.Status = a.store.statusFromBusy()
				if res == nil {
					return nil
				}
				resolved, _ := res["base_url"].(string)
				if res["found"] != true {
					probed, _ := res["probed_url"].(string)
					a.sys(tr("setupFlow.custom.unverified", probed))
					if suggested, _ := res["suggested_base_url"].(string); suggested != "" {
						a.sys(tr("setupFlow.custom.suggested", suggested))
					}
				}
				return a.pickDefaultModel("custom", resolved, stringList(res["models"]), resolved, apiKey)
			})
		}))
	}))
}

// Model chooser of one provider, a typed model name when the provider lists none
func (a *app) pickDefaultModel(slug, name string, models []string, baseURL, apiKey string) tea.Cmd {
	if len(models) == 0 {
		return a.wm.ShowOverlay(wm.NewPrompt(a.toolkit, tr("setupFlow.model.namePrompt", name), "", func(model string) tea.Cmd {
			if trimJS(model) == "" {
				return a.openProviderSetup()
			}
			return a.assignDefaultModel(slug, trimJS(model), baseURL, apiKey, false)
		}))
	}
	items := make([]wm.Item, len(models))
	for i, model := range models {
		items[i] = wm.Item{ID: "setup:model:" + slug + "/" + model, Title: model, Detail: name, Run: func() tea.Cmd {
			return a.assignDefaultModel(slug, model, baseURL, apiKey, false)
		}}
	}
	p := wm.NewPalette(a.toolkit, items)
	p.Placeholder = tr("setupFlow.model.defaultPlaceholder", name)
	return a.wm.ShowOverlay(p)
}

// Saves the profile default model, asking first for a guarded pick, and starts the first session once it is saved
func (a *app) assignDefaultModel(slug, model, baseURL, apiKey string, confirmed bool) tea.Cmd {
	params := map[string]any{"provider": slug, "model": model, "base_url": baseURL, "api_key": apiKey, "confirm_expensive_model": confirmed}
	return a.events.rpc("model.assign", params, func(res map[string]any) tea.Cmd {
		if res == nil {
			return nil
		}
		if truthyJS(res["confirm_required"]) {
			detail, _ := res["confirm_message"].(string)
			a.store.overlay.Confirm = &confirmReq{CancelLabel: tr("slashCmd.session.model.cancel"), ConfirmLabel: tr("slashCmd.session.model.switchAnyway"),
				Danger: true, Detail: detail, OnConfirm: func() tea.Cmd { return a.assignDefaultModel(slug, model, baseURL, apiKey, true) },
				Title: tr("slashCmd.session.model.expensiveTitle")}
			return nil
		}
		savedModel, _ := res["model"].(string)
		savedProvider, _ := res["provider"].(string)
		if a.store.ui.Sid != "" {
			a.sys(tr("setupFlow.model.savedLive", savedModel, savedProvider))
			return nil
		}
		a.sys(tr("setupFlow.model.saved", savedModel, savedProvider))
		return a.newSession(nil)
	})
}
