package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Client a provider profile hands back in place of the standard OpenAI wire client
type seamClient struct{ kwargs map[string]any }

func (seamClient) Route() provider.Route {
	return provider.Route{APIKey: "k", BaseURL: "acp://seam-test"}
}
func (seamClient) Complete(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, errors.New("unused")
}
func (seamClient) Stream(context.Context, *provider.Request, provider.StreamFunc) (*provider.Response, error) {
	return nil, errors.New("unused")
}
func (seamClient) CountTokens(context.Context, *provider.Request) (int, error) { return 0, nil }

func buildSeamClient(t *testing.T, providerName, baseURL string) any {
	t.Helper()
	a := &AIAgent{provider: providerName}
	client, err := a.createOpenAIClient(context.Background(), map[string]any{"api_key": "k", "base_url": baseURL}, "t", false)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestAnOutOfTreeProfileSuppliesTheClientThroughCreateOpenaiClient(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	provider.RegisterProvider(provider.Profile{Name: "seam-test", Aliases: []string{"seam"}, BaseURL: "acp://seam-test",
		CreateClientHook: func(_ context.Context, kwargs map[string]any) (provider.Client, error) {
			return seamClient{kwargs: kwargs}, nil
		}})
	for _, c := range [][2]string{{"seam-test", "acp://seam-test"}, {"seam", "acp://seam-test"}, {"", "acp://seam-test/x"}} {
		if _, ok := buildSeamClient(t, c[0], c[1]).(seamClient); !ok {
			t.Fatalf("%v did not reach the profile client", c)
		}
	}
	standard := buildSeamClient(t, "openai-api", "https://api.example/v1")
	if _, ok := standard.(seamClient); ok || standard == nil {
		t.Fatalf("standard path client %T", standard)
	}
}

func TestABrokenPluginFallsThroughInsteadOfTakingTheTurnDown(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	provider.RegisterProvider(provider.Profile{Name: "seam-boom", BaseURL: "acp://seam-boom",
		CreateClientHook: func(context.Context, map[string]any) (provider.Client, error) {
			return nil, errors.New("plugin is broken")
		}})
	if got := providerSuppliedClient(context.Background(), &AIAgent{provider: "seam-boom"}, map[string]any{"api_key": "k"}); got != nil {
		t.Fatalf("broken plugin client %v", got)
	}
}
