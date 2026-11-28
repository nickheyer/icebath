// Page of Sandbox in the settings menu: the places the agent's commands run, the page of each with what was checked and its own options,
// setting one up, the options of where commands run and the key proxy
package app

import (
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	return []tui.Screen{sandboxScreen(terminalv1connect.NewBackendServiceClient(conn), configv1connect.NewConfigServiceClient(conn))}
}
