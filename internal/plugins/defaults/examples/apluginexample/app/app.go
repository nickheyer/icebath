package app

import (
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	apluginexamplev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/apluginexample/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/apluginexample/v1/apluginexamplev1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := apluginexamplev1connect.NewExampleServiceClient(conn)
	return []tui.Screen{{
		ID:    "apluginexample",
		Title: "apluginexample",
		Short: "The example plugin's greeting",
		Open: func(kit *tui.Kit) tea.Cmd {
			text := "Loading..."
			page := cards.NewPage(kit.Env, cards.PageSpec{
				Title: "apluginexample",
				Key:   "apluginexample",
				Lines: func(int) []string { return []string{text} },
			})
			return tea.Batch(kit.WM.OpenHere(page), tui.Async(func() func() tea.Cmd {
				response, err := service.Greet(kit.Env.Ctx, &apluginexamplev1.GreetRequest{})
				return func() tea.Cmd {
					if text = response.GetGreeting(); err != nil {
						text = "error: " + err.Error()
					}
					return nil
				}
			}))
		},
	}}
}
