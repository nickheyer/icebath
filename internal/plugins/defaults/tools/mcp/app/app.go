package app

import (
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/profile"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
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
	{ID: "mcp.toggle", Context: serversContext, Label: "on/off", Doc: "turn the selected MCP server on or off", Default: []string{"t"}},
	{ID: "mcp.test", Context: serversContext, Label: "test", Doc: "connect to the MCP server and count its tools", Default: []string{"T"}},
	{ID: "mcp.signin", Context: serversContext, Label: "sign in", Doc: "sign in to the MCP server in the browser of this computer", Default: []string{"s"}},
	{ID: "mcp.signin.open", Context: signinContext, Label: "open", Doc: "open the sign in link in the browser of this computer", Default: []string{"o"}},
	{ID: "mcp.signin.copy", Context: signinContext, Label: "copy", Doc: "copy the sign in link", Default: []string{"c"}},
	{ID: "mcp.signin.paste", Context: signinContext, Label: "paste", Doc: "paste the address the browser ended on, when it could not reach icebath", Default: []string{"p"}},
	{ID: "mcp.connection.open", Context: connectionContext, Label: "open", Doc: "open the connection's link in the browser of this computer", Default: []string{"o"}},
	{ID: "mcp.connection.wake", Context: connectionContext, Label: "check now", Doc: "look at the connection again at once", Default: []string{"w"}},
	{ID: "mcp.connection.copy", Context: connectionContext, Label: "copy", Doc: "copy the connection's link", Default: []string{"c"}},
	{ID: "mcp.connection.paste", Context: connectionContext, Label: "paste", Doc: "paste the address the browser ended on, when it could not reach icebath", Default: []string{"p"}},
	{ID: "mcp.connection.retry", Context: connectionContext, Label: "try again", Doc: "run the failed connection again", Default: []string{"r"}},
	{ID: "mcp.connection.skip", Context: connectionContext, Label: "skip", Doc: "go on without this server", Default: []string{"x"}},
	{ID: "mcp.connection.continue", Context: connectionContext, Label: "continue", Doc: "let the agent go on with the servers as they stand", Default: []string{"enter"}},
}

type screens struct{}

func Register(registry *app.Registry, views *toolsapp.Registry) {
	registry.Register(screens{})
	views.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := mcpv1connect.NewMcpServiceClient(conn)
	watcher := &connections{service: service, cards: map[string]*connection{}}
	return []tui.Screen{
		{
			ID:      Screen,
			Title:   "MCP servers",
			Short:   "Add, test, sign in to and pick the tools of MCP servers",
			Slash:   []string{"mcp"},
			Usage:   "[@profile] [add|<server>]",
			Actions: actions,
			Where:   map[string]string{serversContext: "MCP servers screen", signinContext: "MCP sign in", connectionContext: "MCP connection card"},
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				name, rest := profile.Split(argument)
				return (&servers{kit: kit, service: service, profile: name}).open(rest)
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
			Title: "MCP connections",
			Short: "Show the connection cards the agent waits on",
			Slash: []string{"connections"},
			Ready: func(*tui.Kit) bool { return watcher.kit != nil },
			Open:  func(*tui.Kit, string) tea.Cmd { return watcher.reopen() },
		},
	}
}
