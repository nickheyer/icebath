package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	apluginexamplev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/apluginexample/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/apluginexample/v1/apluginexamplev1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := apluginexamplev1connect.NewExampleServiceClient(conn)
	return []tui.Screen{{
		ID:    "apluginexample",
		Title: "Example plugin",
		Short: "Show the example plugin's greeting",
		Slash: []string{"apluginexample"},
		Open: func(kit *tui.Kit, _ string) tea.Cmd {
			text := render.Dim("Asking the example plugin for its greeting…", kit.Env.Theme)
			page := cards.NewPage(kit.Env, cards.PageSpec{
				Title: "Example plugin",
				Key:   "apluginexample",
				Lines: func(w int) []string { return strings.Split(render.Wrap(text, max(w, 1)), "\n") },
			})
			return tea.Batch(kit.WM.OpenHere(page), tui.Async(func() func() tea.Cmd {
				response, err := service.Greet(kit.Env.Ctx, &apluginexamplev1.GreetRequest{})
				return func() tea.Cmd {
					if text = response.GetGreeting(); err != nil {
						text = render.Bad("The example plugin did not answer. "+tui.Reason(err), kit.Env.Theme)
					}
					return nil
				}
			}))
		},
	}}
}
