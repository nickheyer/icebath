package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Catalog provider double whose availability probe answers, or panics like a raising is_available
type fakeCatalogProvider struct {
	name, display string
	available     *bool
}

func (p fakeCatalogProvider) Name() string                   { return p.name }
func (p fakeCatalogProvider) DisplayName() string            { return p.display }
func (p fakeCatalogProvider) GetSetupSchema() map[string]any { return nil }
func (p fakeCatalogProvider) ListModels() []map[string]any   { return nil }
func (p fakeCatalogProvider) DefaultModel() string           { return "" }
func (p fakeCatalogProvider) IsAvailable() bool {
	if p.available == nil {
		panic("probe failed")
	}
	return *p.available
}

// Probe answers of one summary scenario
type summaryProbes struct {
	ready         bool
	vision        []string
	modules       []string
	managedTools  bool
	image, video  []provider.CatalogProviderBase
	features      provider.NousSubscriptionFeatures
	featuresError error
}

// Installs the scenario probes the summary reads
func installSummaryProbes(t *testing.T, p summaryProbes) {
	t.Helper()
	swapSeam(t, &setupResolveProvider, func(context.Context) error {
		if p.ready {
			return nil
		}
		return &provider.AuthError{Message: "No inference provider configured.", Code: "no_provider_configured"}
	})
	swapSeam(t, &setupNousFeatures, func(context.Context, config.Tree) (provider.NousSubscriptionFeatures, error) {
		return p.features, p.featuresError
	})
	swapSeam(t, &setupVisionBackends, func(context.Context) ([]string, error) { return p.vision, nil })
	swapSeam(t, &setupModuleInstalled, func(_ context.Context, name string) bool {
		for _, m := range p.modules {
			if m == name {
				return true
			}
		}
		return false
	})
	swapSeam(t, &setupManagedNousTools, func(context.Context) bool { return p.managedTools })
	swapSeam(t, &setupImageGenProviders, func(context.Context) []provider.CatalogProviderBase { return p.image })
	swapSeam(t, &setupVideoGenProviders, func(context.Context) []provider.CatalogProviderBase { return p.video })
}

// Summary screen with the provider resolver answering ready or raising AuthError and the feature probe failing
func summaryOutput(t *testing.T, providerReady bool) string {
	t.Helper()
	w := newWizardRun(t, "", true)
	t.Cleanup(provider.WithSetupConsole(console()))
	installSummaryProbes(t, summaryProbes{ready: providerReady, featuresError: errors.New("stubbed")})
	if err := printSetupSummary(w.ctx, config.Tree{}, "/tmp/nowhere"); err == nil || err.Error() != "stubbed" {
		t.Fatalf("feature probe failure %v", err)
	}
	return w.out.String()
}

func TestSummaryWarnsWhenNoProvider(t *testing.T) {
	out := summaryOutput(t, false)
	if !strings.Contains(out, "No inference provider is configured") || !strings.Contains(out, "icebath model") || !strings.Contains(out, "icebath setup --portal") {
		t.Fatalf("screen %q", out)
	}
}

func TestSummaryQuietWhenProviderReady(t *testing.T) {
	if out := summaryOutput(t, true); strings.Contains(out, "No inference provider is configured") {
		t.Fatalf("screen %q", out)
	}
}

func TestSetupSummaryLocalBrowserUnavailableWithoutChromium(t *testing.T) {
	w := newWizardRun(t, "", true)
	t.Cleanup(provider.WithSetupConsole(console()))
	cfg := loadHomeConfig(t, w.ctx)
	browser, ok := cfg["browser"].(map[string]any)
	if !ok {
		browser = map[string]any{}
		cfg["browser"] = browser
	}
	browser["cloud_provider"] = "local"
	if err := config.SaveConfig(w.ctx, w.home, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	swapSeam(t, &provider.ChromiumExecutable, func(*tools.Env, bool) string { return "" })
	swapSeam(t, &setupVisionBackends, func(context.Context) ([]string, error) { return nil, nil })
	if err := printSetupSummary(w.ctx, loadHomeConfig(t, w.ctx), w.tmp); err != nil {
		t.Fatal(err)
	}
	out := w.out.String()
	if strings.Contains(out, "Browser Automation (Local browser)") || !strings.Contains(out, "icebath pm install chromium") {
		t.Fatalf("screen %q", out)
	}
}
