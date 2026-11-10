package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func availability(v bool) *bool { return &v }

// Nous feature state of one key for a summary scenario
func featureState(key string, available, managed bool, current string, direct bool) provider.NousFeatureState {
	return provider.NousFeatureState{Key: key, Label: key, IncludedByDefault: true, Available: available, Active: available,
		ManagedByNous: managed, DirectOverride: direct, ToolsetEnabled: true, CurrentProvider: current}
}

// Subscription features with every key unavailable unless overridden
func summaryFeatures(authPresent bool, over map[string]provider.NousFeatureState) provider.NousSubscriptionFeatures {
	features := map[string]provider.NousFeatureState{}
	for _, key := range []string{"web", "image_gen", "video_gen", "tts", "stt", "browser", "modal"} {
		features[key] = featureState(key, false, false, "", false)
		if st, ok := over[key]; ok {
			features[key] = st
		}
	}
	return provider.NousSubscriptionFeatures{NousAuthPresent: authPresent, Features: features}
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

func loadSummaryRefs(t *testing.T) map[string]wizardRef {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "setup_summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]wizardRef{}
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestPrintSetupSummaryScreensMatchHermes(t *testing.T) {
	refs := loadSummaryRefs(t)
	allManaged := map[string]provider.NousFeatureState{}
	for _, key := range []string{"web", "image_gen", "video_gen", "tts", "stt", "browser", "modal"} {
		allManaged[key] = featureState(key, true, true, "", false)
	}
	for _, c := range []struct {
		name   string
		cfg    config.Tree
		env    map[string]string
		probes summaryProbes
	}{
		{"summary_no_provider", config.Tree{}, nil, summaryProbes{features: summaryFeatures(false, nil)}},
		{"summary_all_managed", config.Tree{}, map[string]string{"GITHUB_TOKEN": "ghp_x"}, summaryProbes{ready: true, vision: []string{"openrouter"},
			features: summaryFeatures(false, allManaged)}},
		{"summary_mixed", config.Tree{"tts": map[string]any{"provider": "elevenlabs"}, "stt": map[string]any{"provider": "groq"}, "terminal": map[string]any{"backend": "modal"}},
			map[string]string{"ELEVENLABS_API_KEY": "el"}, summaryProbes{ready: true, modules: []string{"faster_whisper"},
				features: summaryFeatures(false, map[string]provider.NousFeatureState{"web": featureState("web", true, false, "Firecrawl", false),
					"browser": featureState("browser", false, false, "Browser Use", false)}),
				image: []provider.CatalogProviderBase{fakeCatalogProvider{"broken", "Broken", nil}, fakeCatalogProvider{"fal", "FAL", availability(true)},
					fakeCatalogProvider{"test-images", "Test Images", availability(true)}},
				video: []provider.CatalogProviderBase{fakeCatalogProvider{"slow", "Slow", availability(false)}, fakeCatalogProvider{"test-video", "Test Video", availability(true)}}}},
		{"summary_local_engines", config.Tree{"tts": map[string]any{"provider": "neutts"}, "stt": map[string]any{"provider": "xai"}}, nil,
			summaryProbes{ready: true, managedTools: true, features: summaryFeatures(true, nil)}},
		{"summary_kitten", config.Tree{"tts": map[string]any{"provider": "kittentts"}, "stt": map[string]any{"provider": ""}, "terminal": map[string]any{"backend": "modal"}}, nil,
			summaryProbes{ready: true, modules: []string{"kittentts"}, features: summaryFeatures(false, map[string]provider.NousFeatureState{
				"modal": featureState("modal", false, false, "", true), "browser": featureState("browser", true, false, "", false)})}},
		{"summary_unhashable", config.Tree{"tts": map[string]any{"provider": []any{"x"}}}, nil, summaryProbes{ready: true, features: summaryFeatures(false, nil)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWizardRun(t, "", true)
			t.Cleanup(provider.WithSetupConsole(console()))
			w.seed(t, "", nil, c.env)
			installSummaryProbes(t, c.probes)
			err := printSetupSummary(w.ctx, c.cfg, w.home)
			ref := refs[c.name]
			if ref.Err == nil && err != nil || ref.Err != nil && (err == nil || err.Error() != *ref.Err) {
				t.Fatalf("error %v want %v", err, ref.Err)
			}
			if got, want := w.screen(), wizardRenames.Replace(ref.Out); got != want {
				t.Fatalf("screen\n%s\nwant\n%s", got, want)
			}
		})
	}
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
	swapSeam(t, &provider.BrowserHarnessCLI, func(*tools.Env) []string { return []string{"/usr/local/bin/browser-harness"} })
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
