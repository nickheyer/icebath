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

// Group of icebath commands that manages one kind of thing and gets a screen of its own: what it shows first and every command under it
type commandGroup struct {
	id, slash, path string
	// Command whose output the screen opens on, the things the group manages
	read         string
	title, short txt.Key
	order        int
}

// Groups of commands with a screen of their own, in the order the hubs screen lists them
var commandGroups = []commandGroup{
	{"fallback", "fallback", "fallback", "fallback list", txt.HubsTitleFallback, txt.HubsShortFallback, 31},
	{"mixes", "mixes", "moa", "moa list", txt.HubsTitleMixes, txt.HubsShortMixes, 32},
	{"memory-provider", "memory-provider", "memory", "memory status", txt.HubsTitleMemoryProvider, txt.HubsShortMemoryProvider, 33},
	{"projects", "projects", "project", "project list", txt.HubsTitleProjects, txt.HubsShortProjects, 34},
	{"hooks", "hooks", "hooks", "hooks list", txt.HubsTitleHooks, txt.HubsShortHooks, 86},
	{"secrets", "secrets", "secrets", "secrets bitwarden status", txt.HubsTitleSecrets, txt.HubsShortSecrets, 87},
	{"vault", "vault", "vault", "vault list", txt.HubsTitleVault, txt.HubsShortVault, 88},
}

func init() {
	for _, g := range commandGroups {
		hubRegistry = append(hubRegistry, hubDef{id: g.id, order: g.order, title: g.title, short: g.short, slash: g.slash,
			open: func(a *app, _ string) tea.Cmd { return a.openCommandGroup(g) }})
	}
}

// Screen of one group of commands: what the group manages first, then every command under it, each run from a form built from its flags
func (a *app) openCommandGroup(g commandGroup) tea.Cmd {
	var (
		h        *cards.Hub
		commands []tuigateway.CLICommand
		read     *tuigateway.CLICommand
	)
	open := func(i int) tea.Cmd {
		if i < 0 || i >= len(commands) {
			return nil
		}
		return a.commandForm(h, commands[i])
	}
	show := func() tea.Cmd {
		if read == nil {
			return nil
		}
		return a.hubRunCLI(h, tr(g.title), strings.Fields(read.Path), func(*cliRun) tea.Cmd { return nil })
	}
	list := cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(g.title), Glyph: a.toolkit.Glyphs.Terminal, Key: "tui:group:" + g.id, Context: hubPickContext, Primary: "hub.pick",
		Columns: []render.Column{{Title: tr(txt.HubsColCommand), Max: 30}, {Title: tr(txt.HubsColWhen)}, {Title: tr(txt.HubsColWhatItDoes), Flex: 1}},
		Yield:   []int{1},
		Rows: func() [][]string {
			out := make([][]string, len(commands))
			for i, c := range commands {
				when := ""
				if c.Class == tuigateway.CLIRunAfterExit {
					when = tr(txt.RunCommandAfterExit)
				}
				out[i] = []string{strings.TrimPrefix(c.Path, g.path+" "), when, c.Short}
			}
			return out
		},
		Banner: func() string { return tr(txt.HubsGroupBanner, "icebath "+g.path) },
		Help: func(i int) []string {
			if i < 0 || i >= len(commands) {
				return nil
			}
			return []string{commands[i].Usage, commands[i].Short, commands[i].Long}
		},
		Empty: func() string { return tr(txt.HubsLoading) },
		Pick:  true,
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.pick", Label: tr(txt.HintsLabelOpen)}, {Action: "hub.refresh", Label: tr(txt.HubsGroupShow)}}
		},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.pick":    func(_ *cards.List, i int) tea.Cmd { return open(i) },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return show() },
		},
	})
	h = a.newHub("tui:group:"+g.id, list)
	load := hubCall(a, h, tr(txt.HubsLoading), tuigateway.CommandsTree, tuigateway.CommandsTreeParams{}, func(r *tuigateway.CommandsTreeResult) tea.Cmd {
		commands = commands[:0]
		for _, c := range r.Commands {
			if c.Hidden || c.Group || c.Class == tuigateway.CLIRunInternal || !strings.HasPrefix(c.Path, g.path+" ") {
				continue
			}
			commands = append(commands, c)
		}
		if i := slices.IndexFunc(commands, func(c tuigateway.CLICommand) bool { return c.Path == g.read }); i >= 0 {
			read = &commands[i]
		}
		list.Refresh()
		if len(commands) == 0 {
			return h.Fail(tr(txt.HubsSetupNoCommand, g.path))
		}
		return show()
	})
	return tea.Batch(a.wm.ShowOverlay(h), load)
}
