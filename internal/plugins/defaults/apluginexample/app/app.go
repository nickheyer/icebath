package app

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	apluginexamplev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/apluginexample/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/apluginexample/v1/apluginexamplev1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := apluginexamplev1connect.NewExampleServiceClient(conn)
	return []tui.Screen{{
		Title: "apluginexample",
		Text: func(ctx context.Context) (string, error) {
			response, err := service.Greet(ctx, &apluginexamplev1.GreetRequest{})
			if err != nil {
				return "", err
			}
			return response.GetGreeting(), nil
		},
	}}
}
