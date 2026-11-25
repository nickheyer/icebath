package transport

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/internal/provider"
)

type fake struct{ route provider.Route }

func (f fake) Route() provider.Route { return f.route }
func (f fake) Stream(context.Context, *provider.Request, provider.StreamFunc) (*provider.Response, error) {
	return &provider.Response{FinishReason: "stop"}, nil
}

type factories map[provider.APIMode]provider.Factory

func (f factories) Factories() map[provider.APIMode]provider.Factory { return f }

func TestRegistryBuildsTheClientOfTheRouteMode(t *testing.T) {
	host := New()
	nested := new(Registry)
	host.Register(nested)
	build := func(tag string) provider.Factory {
		return func(route provider.Route) (provider.Client, error) {
			route.Model = tag
			return fake{route}, nil
		}
	}
	nested.Register(factories{provider.ModeChatCompletions: build("old")})
	host.Register(factories{provider.ModeChatCompletions: build("new")})

	client, err := host.Client(provider.Route{Provider: "p", Mode: provider.ModeChatCompletions})
	if err != nil {
		t.Fatalf("Client() error = %v", err)
	}
	if got := client.Route().Model; got != "new" {
		t.Fatalf("Client() used factory %q, want the one registered last", got)
	}
	_, err = host.Client(provider.Route{Provider: "p", Mode: provider.ModeGeminiNative})
	if err == nil || !strings.Contains(err.Error(), `no adapter registered for api mode "gemini_native"`) {
		t.Fatalf("Client() error = %v, want a missing adapter error naming the mode", err)
	}
}
