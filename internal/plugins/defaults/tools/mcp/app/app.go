package app

import (
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	"github.com/nickheyer/icebath/internal/profile"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1/mcpv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/keys"
)

// Screen of the MCP servers
const Screen = "mcp"

// Keymap contexts of the servers screen, the sign in page and the connection card
const (
	serversContext    = "mcp"
	signinContext     = "mcp.signin"
	connectionContext = "mcp.connection"
)

var actions = []keys.Action{
	{ID: "mcp.toggle", Context: serversContext, Label: "turn on/off", Doc: "Turn the highlighted MCP server on or off", Default: []string{"t"}},
	{ID: "mcp.test", Context: serversContext, Label: "test", Doc: "Connect to the MCP server and count its tools", Default: []string{"T"}},
	{ID: "mcp.signin", Context: serversContext, Label: "sign in", Doc: "Sign in to the MCP server in the browser on this computer", Default: []string{"s"}},
	{ID: "mcp.reload", Context: serversContext, Label: "connect again", Doc: "Connect every MCP server again, so open sessions get your changes", Default: []string{"r"}},
	{ID: "mcp.signin.open", Context: signinContext, Label: "open link", Doc: "Open the sign-in link in the browser on this computer", Default: []string{"o"}},
	{ID: "mcp.signin.copy", Context: signinContext, Label: "copy link", Doc: "Copy the sign-in link", Default: []string{"c"}},
	{ID: "mcp.signin.paste", Context: signinContext, Label: "paste address", Doc: "Paste the address the browser ended on, if it could not reach icebath", Default: []string{"p"}},
	{ID: "mcp.connection.open", Context: connectionContext, Label: "open link", Doc: "Open the connection's link in the browser on this computer", Default: []string{"o"}},
	{ID: "mcp.connection.wake", Context: connectionContext, Label: "check now", Doc: "Check the connection again now", Default: []string{"w"}},
	{ID: "mcp.connection.copy", Context: connectionContext, Label: "copy link", Doc: "Copy the connection's link", Default: []string{"c"}},
	{ID: "mcp.connection.paste", Context: connectionContext, Label: "paste address", Doc: "Paste the address the browser ended on, if it could not reach icebath", Default: []string{"p"}},
	{ID: "mcp.connection.retry", Context: connectionContext, Label: "try again", Doc: "Try the failed connection again", Default: []string{"r"}},
	{ID: "mcp.connection.skip", Context: connectionContext, Label: "skip", Doc: "Go on without this server", Default: []string{"x"}},
	{ID: "mcp.connection.continue", Context: connectionContext, Label: "continue", Doc: "Let the agent go on with the servers it has now", Default: []string{"enter"}},
}

type screens struct{}

func Register(registry *app.Registry, views *toolsapp.Registry) {
	registry.Register(screens{})
	views.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := mcpv1connect.NewMcpServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	watcher := &connections{service: service, cards: map[string]*connection{}}
	return []tui.Screen{
		{
			ID:      Screen,
			Group:   tui.GroupAbilities,
			Order:   30,
			Title:   "MCP servers",
			Short:   "Add MCP servers and choose their tools",
			Slash:   []string{"mcp"},
			Usage:   "[@profile] [add|<server>]",
			Area:    mcpconfig.Key,
			Pages:   func(kit *tui.Kit, name string) []tui.Page { return serverPages(kit, service, name) },
			Actions: actions,
			Where:   map[string]string{serversContext: "MCP servers page", signinContext: "MCP sign in", connectionContext: "MCP connection card"},
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				name, rest := profile.Split(argument)
				return (&servers{kit: kit, service: service, settings: settings, profile: name}).open(rest)
			},
		},
		{
			ID:      Screen + ".connections",
			Startup: true,
			Open: func(kit *tui.Kit, _ string) tea.Cmd {
				watcher.kit = kit
				watcher.watch()
				return nil
			},
		},
		{
			ID:    Screen + ".connect",
			Group: tui.GroupAbilities,
			Order: 40,
			Title: "MCP connections",
			Short: "Show the MCP connections the agent waits on",
			Slash: []string{"connections"},
			Ready: func(*tui.Kit) bool { return watcher.kit != nil },
			Open:  func(*tui.Kit, string) tea.Cmd { return watcher.reopen() },
		},
	}
}
