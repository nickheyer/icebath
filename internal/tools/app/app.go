package app

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	toolsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1/toolsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := toolsv1connect.NewToolServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    "tools",
		Group: tui.GroupAbilities,
		Order: 10,
		Title: "Tools",
		Short: "Turn the agent's tools on or off",
		Slash: []string{"tools"},
		Usage: "[enable|disable <tool>...]",
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			c := &catalog{kit: kit, service: service, settings: settings}
			words := strings.Fields(argument)
			if len(words) > 1 && (words[0] == "enable" || words[0] == "disable") {
				return tea.Batch(c.open(), c.turn(words[0] == "enable", words[1:]))
			}
			return c.open()
		},
	}}
}

type catalog struct {
	kit      *tui.Kit
	service  toolsv1connect.ToolServiceClient
	settings configv1connect.ConfigServiceClient
	hub      *cards.Hub
	list     *cards.List
	rows     []*toolsv1.Tool
	loaded   bool
	err      error
}

func (c *catalog) open() tea.Cmd {
	t := c.kit.Env.Theme
	c.list = cards.NewList(c.kit.Env, cards.ListSpec{
		Title:   "Tools",
		Glyph:   t.Glyphs.Tools,
		Key:     "tools",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search tools",
		Intro: func() []string {
			return []string{"Turn tools on or off. The agent can use only the tools that are on."}
		},
		Columns: []render.Column{{}, {}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(c.rows))
			for i, tool := range c.rows {
				box := "[x]"
				if tool.GetDisabled() {
					box = "[ ]"
				}
				state := ""
				if !tool.GetDisabled() && !tool.GetAvailable() {
					state = render.Warm("Can't run now", t)
				}
				rows[i] = []string{box, tool.GetTitle(), state, render.Dim(strings.TrimSpace(tool.GetAbout()), t)}
			}
			return rows
		},
		Group: func(i int) string { return c.rows[i].GetGroup() },
		Words: func(i int) string {
			return c.rows[i].GetName() + " " + c.rows[i].GetToolset() + " " + c.rows[i].GetGroup() + " " + c.rows[i].GetAbout()
		},
		Detail: func(i int) []string { return c.detail(c.rows[i], !c.list.Shows(3)) },
		Empty:  c.empty,
		Free:   map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			if c.rows[i].GetDisabled() {
				return []wm.Hint{{Action: "hub.use", Label: "turn on"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "turn off"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				tool := c.rows[i]
				return c.turn(tool.GetDisabled(), []string{tool.GetName()})
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return c.load() },
		},
	})
	c.list.ToggleDetail()
	c.hub = c.kit.Hub("tools", c.list)
	return tea.Batch(c.kit.WM.ShowOverlay(c.hub), c.load())
}

// What the list says while it has no rows
func (c *catalog) empty() string {
	switch {
	case c.err != nil:
		return "Could not read the tools. " + sentence(tui.Reason(c.err)) + " " + c.kit.Env.Keys.Display("hub.refresh") + " tries again."
	case !c.loaded:
		return "Reading the tools…"
	}
	return "No tools are installed."
}

// Lines under the list about a tool: what it lets the agent do where its row has no room to say it, whether it can run, and the name
// /tools takes
func (c *catalog) detail(tool *toolsv1.Tool, about bool) []string {
	t := c.kit.Env.Theme
	var lines []string
	if said := strings.TrimSpace(tool.GetAbout()); about && said != "" {
		lines = append(lines, sentence(said))
	}
	switch {
	case tool.GetDisabled():
		lines = append(lines, render.Dim("Off. The agent can't use it.", t))
	case !tool.GetAvailable():
		lines = append(lines, render.Warm("On, but it can't run now: something it needs is missing, such as an API key, a program or a setting.", t))
	}
	return append(lines, render.Dim("Also /tools enable "+tool.GetName()+" and /tools disable "+tool.GetName(), t))
}

// Sentence of a reason a server gave: capital first letter, ending in a period
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ToUpper(text[:1]) + text[1:]
	if !strings.ContainsAny(text[len(text)-1:], ".!?") {
		text += "."
	}
	return text
}

func (c *catalog) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := c.service.List(c.kit.Env.Ctx, &toolsv1.ListRequest{})
		return func() tea.Cmd {
			c.err, c.loaded = err, true
			if err == nil {
				c.rows = response.GetTools()
			}
			c.list.Refresh()
			return nil
		}
	})
}

// What a turn did, told once it is saved
type turned struct {
	value  string
	titles []string
	// Words that named no tool nor group of tools
	unknown []string
}

// Turns on or off the tools the words name, a word naming a tool by its name or every tool of a group by the group's name, then reads the
// tools again
func (c *catalog) turn(on bool, words []string) tea.Cmd {
	return tui.Call(c.hub, func() (*turned, error) {
		ctx := c.kit.Env.Ctx
		listed, err := c.service.List(ctx, &toolsv1.ListRequest{})
		if err != nil {
			return nil, err
		}
		out := &turned{}
		var names []string
		for _, word := range words {
			found := false
			for _, tool := range listed.GetTools() {
				if tool.GetName() == word || tool.GetToolset() == word {
					names, out.titles, found = append(names, tool.GetName()), append(out.titles, tool.GetTitle()), true
				}
			}
			if !found {
				out.unknown = append(out.unknown, word)
			}
		}
		if len(names) == 0 {
			return out, nil
		}
		setting, err := c.settings.Get(ctx, &configv1.GetRequest{Key: toolsconfig.Disabled})
		if err != nil {
			return nil, err
		}
		var disabled []string
		if err := yaml.Unmarshal([]byte(setting.GetSetting().GetValue()), &disabled); err != nil {
			return nil, err
		}
		disabled = slices.DeleteFunc(disabled, func(name string) bool { return slices.Contains(names, name) })
		if !on {
			disabled = append(disabled, names...)
		}
		slices.Sort(disabled)
		text, err := yaml.Marshal(slices.Compact(disabled))
		if err != nil {
			return nil, err
		}
		out.value = string(text)
		if _, err := c.settings.Set(ctx, &configv1.SetRequest{Key: toolsconfig.Disabled, Value: out.value}); err != nil {
			return nil, err
		}
		return out, nil
	}, func(done *turned) tea.Cmd {
		state := "off"
		if on {
			state = "on"
		}
		var said []string
		switch len(done.titles) {
		case 0:
		case 1:
			said = append(said, done.titles[0]+" is now "+state+".")
		default:
			said = append(said, "Turned "+state+" "+render.Count(len(done.titles), "tool")+": "+strings.Join(done.titles, ", ")+".")
		}
		if len(done.unknown) > 0 {
			said = append(said, "No tool or group of tools is called "+strings.Join(done.unknown, " or ")+".")
		}
		cmds := []tea.Cmd{c.load()}
		if done.value != "" {
			cmds = append(cmds, c.kit.Changed(toolsconfig.Disabled, done.value))
		}
		if len(done.unknown) > 0 {
			cmds = append(cmds, c.hub.Fail(strings.Join(said, " ")))
		} else {
			cmds = append(cmds, c.hub.Done(strings.Join(said, " ")))
		}
		return tea.Batch(cmds...)
	})
}
