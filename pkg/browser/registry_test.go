package browser

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

type fakeCloudProvider struct {
	provider.ProviderDefaults
	name      string
	available bool
	explode   bool
}

func newFakeCloud(name string, available bool) *fakeCloudProvider {
	p := &fakeCloudProvider{name: name, available: available}
	p.Self = p
	return p
}

func (p *fakeCloudProvider) Name() string { return p.name }
func (p *fakeCloudProvider) IsAvailable(*tools.Env) bool {
	if p.explode {
		panic("probe failed")
	}
	return p.available
}
func (p *fakeCloudProvider) CreateSession(*tools.Env, string) (map[string]any, error) {
	return nil, nil
}
func (p *fakeCloudProvider) CloseSession(*tools.Env, string) bool { return true }
func (p *fakeCloudProvider) EmergencyCleanup(*tools.Env, string)  {}

func registerClouds(t *testing.T, providers ...*fakeCloudProvider) {
	t.Helper()
	BrowserProviderRegistry.ResetForTests()
	t.Cleanup(BrowserProviderRegistry.ResetForTests)
	for _, p := range providers {
		if err := BrowserProviderRegistry.Register(p, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolveNoneWithNoCredsReturnsNone(t *testing.T) {
	registerClouds(t, newFakeCloud("browser-use", false), newFakeCloud("browserbase", false), newFakeCloud("firecrawl", true))
	if p := resolveBrowserProvider(context.Background(), nil, ""); p != nil {
		t.Fatalf("resolved %s", p.Name())
	}
}

func TestExplicitLocalReturnsNone(t *testing.T) {
	registerClouds(t, newFakeCloud("browser-use", true))
	if p := resolveBrowserProvider(context.Background(), nil, "local"); p != nil {
		t.Fatalf("resolved %s", p.Name())
	}
}

func TestLegacyWalkPrefersBrowserUseOverBrowserbase(t *testing.T) {
	registerClouds(t, newFakeCloud("browserbase", true), newFakeCloud("browser-use", true))
	if p := resolveBrowserProvider(context.Background(), nil, ""); p == nil || p.Name() != "browser-use" {
		t.Fatalf("resolved %v", p)
	}
}

func TestConfiguredProviderWinsEvenWhenUnavailable(t *testing.T) {
	registerClouds(t, newFakeCloud("browser-use", true), newFakeCloud("firecrawl", false))
	if p := resolveBrowserProvider(context.Background(), nil, "firecrawl"); p == nil || p.Name() != "firecrawl" {
		t.Fatalf("resolved %v", p)
	}
}

func TestUnregisteredConfiguredProviderFallsBackToTheLegacyWalk(t *testing.T) {
	exploding := newFakeCloud("browser-use", true)
	exploding.explode = true
	registerClouds(t, exploding, newFakeCloud("browserbase", true))
	if p := resolveBrowserProvider(context.Background(), nil, "missing"); p == nil || p.Name() != "browserbase" {
		t.Fatalf("resolved %v", p)
	}
}

type fakePageBackend struct {
	provider.ProviderDefaults
	name    string
	tasks   map[string]bool
	explode bool
}

func (b *fakePageBackend) Name() string { return b.name }
func (b *fakePageBackend) HasPage(taskID string) bool {
	if b.explode {
		panic("probe failed")
	}
	return b.tasks[taskID]
}
func (b *fakePageBackend) Evaluate(string, string, bool) (any, error) { return nil, nil }

func TestBackendForTaskPicksTheBackendHoldingThePage(t *testing.T) {
	BrowserPageBackendRegistry.ResetForTests()
	t.Cleanup(BrowserPageBackendRegistry.ResetForTests)
	broken := &fakePageBackend{name: "broken", explode: true}
	holder := &fakePageBackend{name: "holder", tasks: map[string]bool{"task-1": true}}
	for _, b := range []*fakePageBackend{broken, holder} {
		b.Self = b
		if err := BrowserPageBackendRegistry.Register(b, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := backendForTask(context.Background(), "task-1"); got != BrowserPageBackend(holder) {
		t.Fatalf("backend %v", got)
	}
	if got := backendForTask(context.Background(), "task-2"); got != nil {
		t.Fatalf("backend %v", got)
	}
}
