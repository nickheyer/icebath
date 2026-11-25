package app

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/pkg/tui"
)

type Registrant interface {
	Screens(conn *connect.Client) []tui.Screen
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Screens(conn *connect.Client) []tui.Screen {
	var screens []tui.Screen
	for _, registrant := range r.registrants {
		screens = append(screens, registrant.Screens(conn)...)
	}
	return screens
}

type App struct {
	Registry
}

func New() *App {
	return &App{}
}

func (a *App) Run(ctx context.Context, conn *connect.Client) error {
	return tui.Run(ctx, a.Screens(conn))
}
