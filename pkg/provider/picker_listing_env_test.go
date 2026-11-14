package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestOpencodeGoAppearsWhenAPIKeySet(t *testing.T) {
	ctx, _ := optionsHome(t)
	t.Setenv("OPENCODE_GO_API_KEY", "test-key")
	row := pickerRow(mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter", MaxModels: intPtr(50)}), "opencode-go")
	if row == nil || len(rowModels(row)) == 0 {
		t.Fatalf("opencode-go should appear with models when OPENCODE_GO_API_KEY is set %v", row)
	}
}

func TestOpencodeGoNotAppearsWhenNoCreds(t *testing.T) {
	ctx, _ := optionsHome(t)
	t.Setenv("OPENCODE_GO_API_KEY", "")
	if row := pickerRow(mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter"}), "opencode-go"); row != nil {
		t.Fatalf("opencode-go should not appear without credentials %v", row)
	}
}

func alibabaSlugs(t *testing.T, env map[string]string) []string {
	t.Helper()
	ctx, _ := optionsHome(t)
	for _, k := range []string{"ALIBABA_CODING_PLAN_API_KEY", "ALIBABA_CODING_PLAN_CN_API_KEY", "DASHSCOPE_API_KEY"} {
		t.Setenv(k, env[k])
	}
	var out []string
	for _, r := range mustListProviders(t, ctx, AuthenticatedProvidersOptions{}) {
		if slug := pyStr(r["slug"]); strings.Contains(slug, "coding-plan") {
			out = append(out, slug)
		}
	}
	return out
}

func TestAlibabaCNAppearsWhenOnlyCNKeySet(t *testing.T) {
	if got := alibabaSlugs(t, map[string]string{"ALIBABA_CODING_PLAN_CN_API_KEY": "sk-cn-fake"}); !reflect.DeepEqual(got, []string{"alibaba-coding-plan-cn"}) {
		t.Fatalf("slugs %v", got)
	}
}

func TestAlibabaCNDoesNotAppearWhenOnlyIntlKeySet(t *testing.T) {
	if got := alibabaSlugs(t, map[string]string{"ALIBABA_CODING_PLAN_API_KEY": "sk-intl-fake"}); !reflect.DeepEqual(got, []string{"alibaba-coding-plan"}) {
		t.Fatalf("slugs %v", got)
	}
}

// Offline listing where one models.dev provider is credentialed and serves the cached ids given
func modelsDevOnlyListing(t *testing.T, envName, provider string, cached []string, opts AuthenticatedProvidersOptions) []map[string]any {
	t.Helper()
	ctx, _ := pickerTestHome(t, nil)
	t.Setenv(envName, "test-key")
	pickerFetchModelsDevFn = func(context.Context, bool) map[string]any {
		return map[string]any{provider: map[string]any{"env": []any{envName}, "name": pyTitle(provider)}}
	}
	cachedProviderModelIDsFn = func(context.Context, string, CachedModelsOptions) []string { return cached }
	saved := providerToModelsDev
	providerToModelsDev = map[string]string{provider: provider}
	t.Cleanup(func() { providerToModelsDev = saved })
	return mustListProviders(t, ctx, opts)
}

func TestConfiguredModelsPrecedeAndDeduplicateDiscoveredModels(t *testing.T) {
	rows := modelsDevOnlyListing(t, "DEEPSEEK_API_KEY", "deepseek", []string{"live-a", "shared"}, AuthenticatedProvidersOptions{
		CurrentProvider: "deepseek", UserProviders: map[string]any{"deepseek": map[string]any{"models": map[string]any{
			"configured-x": map[string]any{}, "shared": map[string]any{}}}}})
	row := pickerRow(rows, "deepseek")
	if row == nil || !reflect.DeepEqual(rowModels(row), []string{"configured-x", "shared", "live-a"}) || row["total_models"] != 3 {
		t.Fatalf("row %v", row)
	}
}

func TestModelsDevOnlyProviderIsNotSelectable(t *testing.T) {
	rows := modelsDevOnlyListing(t, "MISTRAL_API_KEY", "mistral", []string{"model-a"}, AuthenticatedProvidersOptions{MaxModels: intPtr(5)})
	if pickerRow(rows, "mistral") != nil {
		t.Fatal("mistral row listed")
	}
	if ok, err := isRuntimeProviderRoutable(modelsPoolEnv(context.Background()), "mistral"); ok || err != nil {
		t.Fatalf("mistral routable %v %v", ok, err)
	}
}

func TestSpecialRuntimeProviderDoesNotRequireRegistryMembership(t *testing.T) {
	newTestHome(t)
	env := modelsPoolEnv(context.Background())
	for _, id := range []string{"openrouter", "custom:local-lab"} {
		if ok, err := isRuntimeProviderRoutable(env, id); !ok || err != nil {
			t.Fatalf("%s routable %v %v", id, ok, err)
		}
	}
}
