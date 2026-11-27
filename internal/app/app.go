package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	appconfig "github.com/nickheyer/icebath/internal/app/config"
	"github.com/nickheyer/icebath/internal/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
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

// Lets every registrant that holds something past the screens, such as a microphone, let go of it as the app exits, the last registered first
func (r *Registry) Close() error {
	var errs []error
	for _, registrant := range slices.Backward(r.registrants) {
		if closer, ok := registrant.(io.Closer); ok {
			errs = append(errs, closer.Close())
		}
	}
	return errors.Join(errs...)
}

type App struct {
	Registry
}

func New(cfg *config.Config) *App {
	appconfig.Register(cfg)
	return &App{}
}

// Runs the screens until the person quits, then lets the registrants let go of what they hold
func (a *App) Run(ctx context.Context, conn *connect.Client) (err error) {
	settings := configv1connect.NewConfigServiceClient(conn)
	get := func(key string) (string, error) {
		response, err := settings.Get(ctx, &configv1.GetRequest{Key: key})
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		return response.GetSetting().GetValue(), nil
	}
	screens := a.Screens(conn)
	defer func() { err = errors.Join(err, a.Close()) }()
	options := tui.Options{Settings: map[string]string{}}
	for key, into := range map[string]any{appconfig.Skin: &options.Skin, appconfig.Keys: &options.Keys} {
		value, err := get(key)
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal([]byte(value), into); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	for _, key := range tui.Followed(screens) {
		value, err := get(key)
		if err != nil {
			return err
		}
		options.Settings[key] = value
	}
	return tui.Run(ctx, screens, options)
}
