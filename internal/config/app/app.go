package app

import (
	"context"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{Title: "settings", Screens: func(ctx context.Context) ([]tui.Screen, error) {
		response, err := service.List(ctx, &configv1.ListRequest{})
		if err != nil {
			return nil, err
		}
		list := []tui.Screen{{
			Title: "set a key that is not listed",
			Text: func(context.Context) (string, error) {
				return "Type a key, a space and its value in YAML, such as `plugins.custom.providers.local.base_url http://localhost:8080/v1`.", nil
			},
			Send: func(ctx context.Context, text string) error {
				key, value, _ := strings.Cut(text, " ")
				_, err := service.Set(ctx, &configv1.SetRequest{Key: key, Value: value})
				return err
			},
		}}
		for _, setting := range response.GetSettings() {
			key := setting.GetKey()
			list = append(list, tui.Screen{
				Title: key + ": " + setting.GetValue(),
				Text: func(ctx context.Context) (string, error) {
					response, err := service.Get(ctx, &configv1.GetRequest{Key: key})
					if err != nil {
						return "", err
					}
					return "`" + key + "`\n\n```yaml\n" + response.GetSetting().GetValue() + "\n```\n\nType a new value in YAML. `null` puts the default back.", nil
				},
				Send: func(ctx context.Context, text string) error {
					_, err := service.Set(ctx, &configv1.SetRequest{Key: key, Value: text})
					return err
				},
			})
		}
		return list, nil
	}}}
}
