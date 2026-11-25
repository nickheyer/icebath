package app

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := providerv1connect.NewProviderServiceClient(conn)
	return []tui.Screen{{Title: "providers", Screens: func(ctx context.Context) ([]tui.Screen, error) {
		response, err := service.List(ctx, &providerv1.ListRequest{})
		if err != nil {
			return nil, err
		}
		list := make([]tui.Screen, len(response.GetProviders()))
		for i, entry := range response.GetProviders() {
			title := entry.GetDisplayName()
			if entry.GetCredentialSource() != "" || entry.GetLocal() {
				title += " (ready)"
			}
			list[i] = tui.Screen{Title: title, Text: func(ctx context.Context) (string, error) {
				return describe(ctx, service, entry), nil
			}}
		}
		return list, nil
	}}}
}

func describe(ctx context.Context, service providerv1connect.ProviderServiceClient, entry *providerv1.Provider) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n", entry.GetDisplayName())
	if entry.GetDescription() != "" {
		fmt.Fprintf(&out, "%s\n\n", entry.GetDescription())
	}
	fmt.Fprintf(&out, "- name: `%s`\n", entry.GetName())
	if len(entry.GetAliases()) > 0 {
		fmt.Fprintf(&out, "- aliases: `%s`\n", strings.Join(entry.GetAliases(), "`, `"))
	}
	if entry.GetBaseUrl() != "" {
		fmt.Fprintf(&out, "- endpoint: `%s`\n", entry.GetBaseUrl())
	}
	fmt.Fprintf(&out, "- request format: `%s`\n", entry.GetApiMode())
	if entry.GetDefaultModel() != "" {
		fmt.Fprintf(&out, "- default model: `%s`\n", entry.GetDefaultModel())
	}
	switch {
	case entry.GetCredentialSource() != "":
		fmt.Fprintf(&out, "- credential: %s\n", entry.GetCredentialSource())
	case entry.GetLocal():
		out.WriteString("- credential: none needed\n")
	default:
		out.WriteString("- credential: none")
		if len(entry.GetKeyEnv()) > 0 {
			fmt.Fprintf(&out, ", set `%s`", strings.Join(entry.GetKeyEnv(), "` or `"))
		}
		if entry.GetSignupUrl() != "" {
			fmt.Fprintf(&out, ", get one at %s", entry.GetSignupUrl())
		}
		out.WriteString("\n")
	}
	models, err := service.Models(ctx, &providerv1.ModelsRequest{Provider: entry.GetName()})
	switch {
	case err != nil:
		fmt.Fprintf(&out, "\nModels: %s\n", err)
	case len(models.GetModels()) == 0:
		out.WriteString("\nModels: none listed\n")
	default:
		fmt.Fprintf(&out, "\nModels:\n\n- `%s`\n", strings.Join(models.GetModels(), "`\n- `"))
	}
	return out.String()
}
