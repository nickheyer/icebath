package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
)

func swapAdvertisedContextLength(t *testing.T, n int) {
	t.Helper()
	prevPins, prevFn := warnedPins, advertisedContextLengthFn
	warnedPins = map[contextPinKey]bool{}
	advertisedContextLengthFn = func(context.Context, string, string) int { return n }
	t.Cleanup(func() { warnedPins, advertisedContextLengthFn = prevPins, prevFn })
}

func TestPinDisagreementWarnsOnceAndKeepsPin(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swapAdvertisedContextLength(t, 200_000)
	logs := captureInfoLogs(t)
	if !warnOnceOnPinDisagreement(t.Context(), "claude-sonnet-4", "", 999_000) {
		t.Fatal("first disagreement did not warn")
	}
	if warnOnceOnPinDisagreement(t.Context(), "claude-sonnet-4", "", 999_000) {
		t.Fatal("second session start warned again")
	}
	if warnOnceOnPinDisagreement(t.Context(), "claude-sonnet-4", "", 200_000) {
		t.Fatal("a pin matching the advertised window warned")
	}
	out := logs.String()
	if n := strings.Count(out, "logger=agent"); n != 1 || !strings.Contains(out, "level=WARN") ||
		!strings.Contains(out, "pins claude-sonnet-4 at 999,000 tokens but the provider advertises 200,000; the pin wins") {
		t.Fatalf("warnings %d log %q", n, out)
	}
}

func TestPinnedLabelOnlyWhenShownValueIsThePin(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if !isContextPinned(999_000, 999_000) || contextPinSuffix(999_000, 999_000) != " (pinned)" {
		t.Fatal("the shown pin is not labelled")
	}
	for _, c := range [][2]any{{200_000, 999_000}, {200_000, nil}, {1, true}} {
		if got := contextPinSuffix(c[0], c[1]); got != "" {
			t.Fatalf("%v labelled %q", c, got)
		}
	}
}

const (
	pinTestPin        = 249_000
	pinTestRoute      = "http://127.0.0.1:8123/v1"
	pinTestOtherRoute = "https://openrouter.ai/api/v1"
)

func pinTestConfig() map[string]any {
	return map[string]any{
		"model":     map[string]any{"default": "model-b", "provider": "custom:acme", "base_url": pinTestRoute, "context_length": pinTestPin},
		"providers": map[string]any{"acme": map[string]any{"base_url": pinTestRoute}},
	}
}

func TestSwitchBackToConfiguredDefaultRouteRestoresContextPin(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cc := &ContextCompressor{}
	a := &AIAgent{model: "other-model", provider: "openrouter", baseURL: pinTestOtherRoute, contextCompressor: cc}
	if err := setConfigContextLength(t.Context(), a, configContextLengthForRuntime(settings.WithFrozen(t.Context(), pinTestConfig()), a)); err != nil {
		t.Fatal(err)
	}
	if a.configContextLength != 0 || cc.configContextLength != nil {
		t.Fatalf("the pin described another route: agent %d engine %v", a.configContextLength, cc.configContextLength)
	}
	a.model, a.provider, a.baseURL = "model-b", "custom:acme", pinTestRoute
	if err := setConfigContextLength(t.Context(), a, configContextLengthForRuntime(settings.WithFrozen(t.Context(), pinTestConfig()), a)); err != nil {
		t.Fatal(err)
	}
	if a.configContextLength != pinTestPin || cc.configContextLength == nil || *cc.configContextLength != pinTestPin {
		t.Fatalf("round trip lost the pin: agent %d engine %v", a.configContextLength, cc.configContextLength)
	}
}

func TestSwitchToOtherRouteDoesNotInheritContextPin(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	pin := pinTestPin
	cc := &ContextCompressor{configContextLength: &pin}
	a := &AIAgent{model: "unrelated-model", provider: "openrouter", baseURL: pinTestOtherRoute, contextCompressor: cc, configContextLength: pin}
	got := configContextLengthForRuntime(settings.WithFrozen(t.Context(), pinTestConfig()), a)
	if got != nil {
		t.Fatalf("unrelated runtime inherited %d", *got)
	}
	if err := setConfigContextLength(t.Context(), a, got); err != nil || a.configContextLength != 0 || cc.configContextLength != nil {
		t.Fatalf("err %v agent %d engine %v", err, a.configContextLength, cc.configContextLength)
	}
}

func TestResolveContextLengthRefusesUnitSuffixAndReadsAuxPin(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{model: "m", platform: "cli"}
	cfg := map[string]any{"model": map[string]any{"context_length": "256K"}, "auxiliary": map[string]any{"compression": map[string]any{"context_length": "64000"}}}
	pin, cps, effective := resolveContextLength(settings.WithFrozen(t.Context(), cfg), a, "")
	if pin != nil || effective != 0 || len(cps) != 0 {
		t.Fatalf("pin %v effective %d providers %v", pin, effective, cps)
	}
	if a.auxCompressionContextLengthConfig == nil || *a.auxCompressionContextLengthConfig != 64000 || a.configContextLength != 0 {
		t.Fatalf("aux %v agent pin %d", a.auxCompressionContextLengthConfig, a.configContextLength)
	}
}

func TestResolveContextLengthKeepsTheScopedPinAndRecordsCustomProviders(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swapAdvertisedContextLength(t, 0)
	a := &AIAgent{model: "model-b", provider: "custom:acme", baseURL: pinTestRoute}
	pin, cps, effective := resolveContextLength(settings.WithFrozen(t.Context(), pinTestConfig()), a, pinTestRoute)
	if pin == nil || *pin != pinTestPin || effective != pinTestPin || a.configContextLength != pinTestPin {
		t.Fatalf("pin %v effective %d agent %d", pin, effective, a.configContextLength)
	}
	if len(cps) != 1 || len(a.customProviderEntries) != 1 || a.customProviderEntries[0]["name"] != "acme" {
		t.Fatalf("custom providers %v agent %v", cps, a.customProviderEntries)
	}
}

func TestConfiguredProviderRouteComesFromItsEnabledProvidersEntry(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	entries := func(block map[string]any) []map[string]any {
		return provider.ProvidersDictToCustomProviders(map[string]any{"acme": block})
	}
	if got := customProviderConfiguredBaseURL("acme", entries(map[string]any{"enabled": false, "base_url": "http://a.example/v1"})); got != "" {
		t.Fatalf("disabled provider resolved %q", got)
	}
	want := provider.NormalizeRouteBaseURL("http://a.example/v1")
	for _, name := range []string{"acme", "custom:acme", "Acme Lab", "custom:acme-lab"} {
		if got := customProviderConfiguredBaseURL(name, entries(map[string]any{"name": "Acme Lab", "base_url": "http://a.example/v1"})); got != want {
			t.Fatalf("%s resolved %q", name, got)
		}
	}
	if got := customProviderConfiguredBaseURL("other", entries(map[string]any{"base_url": "http://a.example/v1"})); got != "" {
		t.Fatalf("another provider resolved %q", got)
	}
}

func TestActiveRouteURLKeepsTheRequestedQueryOnlyForTheSameRoute(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{baseURL: "https://proxy.example/v1"}
	if got := activeRouteURL(a, "https://proxy.example/v1?api-version=2"); got != provider.NormalizeRouteBaseURL("https://proxy.example/v1?api-version=2") {
		t.Fatalf("same route %q", got)
	}
	if got := activeRouteURL(a, "https://other.example/v1?x=1"); got != provider.NormalizeRouteBaseURL("https://proxy.example/v1") {
		t.Fatalf("other route %q", got)
	}
	if got := stripURLQuery("https://h/v1?a=1#frag"); got != "https://h/v1#frag" {
		t.Fatalf("strip %q", got)
	}
}

func TestAdvertisedContextLengthReadsTheDiskCacheWindow(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"acme/ctx-pin-probe-model": {"context_length": 777000}, "acme/bad-window": {"context_length": "1"}}`
	if err := os.WriteFile(filepath.Join(dir, "cache", "openrouter_model_metadata.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := advertisedContextLength(t.Context(), "acme/ctx-pin-probe-model", ""); got != 777000 {
		t.Fatalf("disk cache window %d", got)
	}
	if got, ok := jsonDecodedInt(1.5); ok {
		t.Fatalf("fractional json number read as int %d", got)
	}
	if got, ok := jsonDecodedInt("1"); ok {
		t.Fatalf("string read as int %d", got)
	}
}
