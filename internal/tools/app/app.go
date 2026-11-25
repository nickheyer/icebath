package app

import (
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
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
	return []tui.Screen{{
		ID:    "tools",
		Title: "Tools",
		Short: "What the agent can call, by toolset",
		Open: func(kit *tui.Kit) tea.Cmd {
			return (&catalog{kit: kit, service: service}).open()
		},
	}}
}

type catalog struct {
	kit     *tui.Kit
	service toolsv1connect.ToolServiceClient
	rows    []*toolsv1.Tool
	loaded  bool
	err     error
}

func (c *catalog) open() tea.Cmd {
	list := cards.NewList(c.kit.Env, cards.ListSpec{
		Title:   "Tools",
		Key:     "tools",
		Context: cards.HubContext,
		Columns: []render.Column{{Title: "Tool"}, {Title: "Toolset"}, {Title: "Available"}, {Title: "What it does", Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(c.rows))
			for i, tool := range c.rows {
				available := "yes"
				if !tool.GetAvailable() {
					available = "no"
				}
				rows[i] = []string{tool.GetName(), tool.GetToolset(), available, tool.GetDescription()}
			}
			return rows
		},
		Detail: func(i int) []string { return []string{c.rows[i].GetDescription()} },
		Empty: func() string {
			switch {
			case c.err != nil:
				return c.err.Error()
			case !c.loaded:
				return "Loading…"
			}
			return "No tools"
		},
		Free: map[string]bool{"hub.refresh": true},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.toggle", Label: "details"}, {Action: "hub.refresh"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.toggle":  func(l *cards.List, _ int) tea.Cmd { return l.ToggleDetail() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return c.load() },
		},
	})
	list.ToggleDetail()
	return tea.Batch(c.kit.WM.ShowOverlay(c.kit.Hub("tools", list)), c.load())
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
