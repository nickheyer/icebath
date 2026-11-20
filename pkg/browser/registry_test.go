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
