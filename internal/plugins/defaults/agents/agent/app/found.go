package app

import (
	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	delegationconfig "github.com/nickheyer/icebath/internal/delegation/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Pages inside the Agent area that a search of Settings finds: Model shortcuts and the page of each shortcut, Prompt, Your personalities
// and the page of each one, and Tool loops
func (d *desk) agentPages(kit *tui.Kit, profile string) []tui.Page {
	listed, err := d.settings.List(kit.Env.Ctx, &configv1.ListRequest{Profile: profile})
	if err != nil {
		return nil
	}
	section := func(key string) *configv1.Section {
		for _, each := range listed.GetSections() {
			if each.GetKey() == key {
				return each
			}
		}
		return &configv1.Section{Key: key}
	}
	setting := func(key string) *configv1.Setting {
		for _, each := range listed.GetSettings() {
			if each.GetKey() == key {
				return each
			}
		}
		return nil
	}
	shortcuts := "Model shortcuts"
	pages := []tui.Page{{Title: shortcuts, About: "Short names you can use in place of a model's full name", Words: "aliases",
		Argument: jobShortcuts, Keys: []string{agentconfig.Aliases}}}
	held, order := entriesOf(setting(agentconfig.Aliases))
	for _, name := range order {
		// The provider by its name in the profile: pages read names and titles only, never what takes time to find
		words := shortcutWords(held[name], nil)
		pages = append(pages, tui.Page{Title: name, Trail: []string{shortcuts}, About: "Stands for " + words, Words: words,
			Argument: jobShortcut + name, Keys: []string{agentconfig.Aliases + "." + name + "."}})
	}
	prompt := section(agentconfig.PromptKey)
	pages = append(pages, tui.Page{Title: prompt.GetTitle(), About: prompt.GetDescription(), Argument: jobPrompt,
		Keys: []string{agentconfig.PromptKey + "."}})
	personalities := "Your personalities"
	pages = append(pages, tui.Page{Title: personalities, Trail: []string{prompt.GetTitle()}, About: "Personalities of your own that you can pick by name",
		Words: "personality", Argument: jobPersonalities, Keys: []string{agentconfig.Personalities}})
	held, order = entriesOf(setting(agentconfig.Personalities))
	for _, name := range order {
		pages = append(pages, tui.Page{Title: name, Trail: []string{prompt.GetTitle(), personalities}, About: personalityWords(held[name]),
			Words: "personality", Argument: jobOnePersona + name, Keys: []string{agentconfig.Personalities + "." + name + "."}})
	}
	loops := section(agentconfig.LoopKey)
	return append(pages, tui.Page{Title: loops.GetTitle(), About: loops.GetDescription(), Argument: jobLoops, Keys: []string{agentconfig.LoopKey + "."}})
}

// Pages inside Helper models that a search of Settings finds: the page of each side job
func (d *desk) helperPages(kit *tui.Kit, profile string) []tui.Page {
	listed, err := d.settings.List(kit.Env.Ctx, &configv1.ListRequest{Profile: profile})
	if err != nil {
		return nil
	}
	var pages []tui.Page
	for _, section := range listed.GetSections() {
		if section.GetParent() == auxiliaryconfig.Key {
			pages = append(pages, tui.Page{Title: section.GetTitle(), About: section.GetDescription(), Argument: section.GetKey(),
				Keys: []string{section.GetKey() + "."}})
		}
	}
	return pages
}

// Pages inside Subagents that a search of Settings finds: the method they run with, and the settings of each method on its page
func (d *desk) subagentPages(kit *tui.Kit, profile string) []tui.Page {
	response, err := d.settings.Get(kit.Env.Ctx, &configv1.GetRequest{Key: delegationconfig.Backend, Profile: profile})
	if err != nil {
		return nil
	}
	method := response.GetSetting()
	return []tui.Page{{Title: method.GetTitle(), About: method.GetDescription(), Words: "method backend", Argument: jobMethod,
		Keys: []string{delegationconfig.Backend, delegationconfig.Backends + "."}}}
}
