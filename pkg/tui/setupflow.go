package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/txt"
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
		{"model", tr(txt.SetupFlowModelTitle), tr(txt.SetupFlowModelDetail), func(a *app) tea.Cmd { return a.openHostedModel("") }},
		{"aux", tr(txt.SetupFlowAuxTitle), tr(txt.SetupFlowAuxDetail), func(a *app) tea.Cmd { return a.openHostedModel("aux-config") }},
	}
	for _, spec := range []sectionSpec{agentSection(), ttsSection(), terminalSection()} {
		sections = append(sections, setupSection{spec.key, spec.title, spec.detail, func(a *app) tea.Cmd { return a.runSection(spec) }})
	}
	sections = append(sections, toolsSetupSection())
	for _, hosted := range []setupSection{
		{key: "gateway", title: tr(txt.SetupFlowGatewayTitle), detail: tr(txt.SetupFlowGatewayDetail)},
		{key: "quick", title: tr(txt.SetupFlowQuickTitle), detail: quickSetupDetail()},
		{key: "blank", title: tr(txt.SetupFlowBlankTitle), detail: tr(txt.SetupFlowBlankDetail)},
		{key: "import", title: tr(txt.SetupFlowImportTitle), detail: tr(txt.SetupFlowImportDetail)},
	} {
		hosted.open = func(a *app) tea.Cmd { return a.openHostedFlow(hosted.key, "") }
		sections = append(sections, hosted)
	}
	return slices.DeleteFunc(sections, func(s setupSection) bool { _, absent := setupSectionRefusal(s.key); return absent })
}

// Opens the named section, the model section at a named provider, the menu of every section when none is named
func (a *app) openSetup(section string) tea.Cmd {
	section, entry, _ := strings.Cut(strings.TrimSpace(section), " ")
	if entry = strings.TrimSpace(entry); section == "model" && entry != "" {
		return a.openHostedModel(entry)
	}
	sections := setupSections()
	if line, absent := setupSectionRefusal(section); absent {
		a.sys(line)
		return nil
	}
	if section != "" {
		keys := make([]string, len(sections))
		for i, s := range sections {
			if s.key == section {
				return s.open(a)
			}
			keys[i] = s.key
		}
		a.sys(tr(txt.SetupFlowUnknownSection, section, strings.Join(keys, ", ")))
		return nil
	}
	items := make([]wm.Item, len(sections))
	for i, s := range sections {
		items[i] = wm.Item{ID: "setup:" + s.key, Title: s.title, Detail: s.detail, Run: func() tea.Cmd { return s.open(a) }}
	}
	p := wm.NewPalette(a.toolkit, items)
	p.Placeholder = tr(txt.SetupFlowPlaceholder)
	return a.wm.ShowOverlay(p)
}
