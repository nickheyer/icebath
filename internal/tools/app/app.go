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
		Title: "Tools",
		Short: "Enable or disable the agent's tools",
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
	rows     []*toolsv1.Tool
	loaded   bool
	err      error
}

func (c *catalog) open() tea.Cmd {
	list := cards.NewList(c.kit.Env, cards.ListSpec{
		Title:   "Tools",
		Glyph:   c.kit.Env.Theme.Glyphs.Tools,
		Key:     "tools",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search tools",
		Columns: []render.Column{{}, {Title: "Tool"}, {Title: "Name"}, {Title: "Set"}, {Title: "State"}},
		Yield:   []int{3, 2},
		Rows: func() [][]string {
			t := c.kit.Env.Theme
			rows := make([][]string, len(c.rows))
			for i, tool := range c.rows {
				rows[i] = []string{render.ToolGlyph(tool.GetIcon(), t), tool.GetTitle(), render.Dim(tool.GetName(), t), render.Dim(tool.GetToolset(), t), c.state(tool)}
			}
			return rows
		},
		Detail: func(i int) []string { return []string{c.rows[i].GetDescription()} },
		Empty:  func() string { return tui.Waiting(c.loaded, c.err, "No tools") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: "toggle"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				tool := c.rows[i]
				return c.turn(tool.GetDisabled(), []string{tool.GetName()})
			},
		},
	})
	list.ToggleDetail()
	c.hub = c.kit.Hub("tools", list)
	return tea.Batch(c.kit.WM.ShowOverlay(c.hub), c.load())
}

// State of a tool in one word, coloured by whether the agent can use it
func (c *catalog) state(tool *toolsv1.Tool) string {
	t := c.kit.Env.Theme
	switch {
	case tool.GetDisabled():
		return render.Dim("Disabled", t)
	case !tool.GetAvailable():
		return render.Warm("Unavailable", t)
	}
	return render.Good("Enabled", t)
}

func (c *catalog) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := c.service.List(c.kit.Env.Ctx, &toolsv1.ListRequest{})
		return func() tea.Cmd {
			c.rows, c.err, c.loaded = response.GetTools(), err, true
			return nil
		}
	})
}

// Enables or disables the named tools, then reads the tools again
func (c *catalog) turn(on bool, names []string) tea.Cmd {
	return tui.Call(c.hub, func() (*configv1.SetResponse, error) {
		ctx := c.kit.Env.Ctx
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
		return c.settings.Set(ctx, &configv1.SetRequest{Key: toolsconfig.Disabled, Value: string(text)})
	}, func(*configv1.SetResponse) tea.Cmd {
		verb := "Disabled "
		if on {
			verb = "Enabled "
		}
		return tea.Batch(c.load(), c.hub.Done(verb+strings.Join(names, ", ")))
	})
}
