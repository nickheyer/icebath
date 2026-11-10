package provider

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Swaps the consent question, recording each question and default it was asked with
func swapTelemetryPrompt(t *testing.T, answer func(def bool) bool) (*[]string, *[]bool) {
	t.Helper()
	questions, defaults := &[]string{}, &[]bool{}
	t.Cleanup(SwapSetupTelemetryPrompt(func(question string, def bool, _ string) bool {
		*questions = append(*questions, question)
		*defaults = append(*defaults, def)
		return answer(def)
	}))
	return questions, defaults
}

// Shared metrics mapping of a config, nil when a level is missing
func sharedMetricsOf(cfg map[string]any) map[string]any {
	v, _ := config.CfgGet(cfg, "telemetry", "shared_metrics")
	m, _ := v.(map[string]any)
	return m
}

func TestSharedMetricsAreRegisteredDisabledByDefault(t *testing.T) {
	if v, ok := config.CfgGet(config.Defaults(), "telemetry", "shared_metrics", "enabled"); !ok || v != false {
		t.Fatalf("default enabled %v", v)
	}
}

func TestSetupTelemetryOneAnswerSetsBothKeys(t *testing.T) {
	ctx, _ := scriptedSetupConsole(t)
	questions, _ := swapTelemetryPrompt(t, func(def bool) bool { return !def })
	cfg := map[string]any{}
	if err := SetupTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if m := sharedMetricsOf(cfg); m["enabled"] != true || m["send"] != true {
		t.Fatalf("first answer %v", m)
	}
	if err := SetupTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if m := sharedMetricsOf(cfg); m["enabled"] != false || m["send"] != false {
		t.Fatalf("second answer %v", m)
	}
	if !slices.Equal(*questions, []string{"Send shared metrics to Nous?", "Send shared metrics to Nous?"}) {
		t.Fatalf("questions %v", *questions)
	}
}

func TestSetupTelemetryDefaultsACollectOnlyProfileToNo(t *testing.T) {
	ctx, _ := scriptedSetupConsole(t)
	_, defaults := swapTelemetryPrompt(t, func(def bool) bool { return def })
	cfg := map[string]any{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": true, "send": false}}}
	if err := SetupTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*defaults, []bool{false}) || sharedMetricsOf(cfg)["enabled"] != false {
		t.Fatalf("defaults %v config %v", *defaults, sharedMetricsOf(cfg))
	}
}

// Open send consent windows of the shared metrics store of a home
func openConsentWindows(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM send_consent_windows WHERE closed_at IS NULL").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDisablingCollectionClosesTheSendConsentWindow(t *testing.T) {
	ctx, _ := scriptedSetupConsole(t)
	store, err := observability.NewSharedMetricsStore(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	swapTelemetryPrompt(t, func(bool) bool { return false })
	cfg := map[string]any{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": true, "send": true}}}
	observability.RecordSendConsentChange(ctx, true)
	if openConsentWindows(t, store.DatabasePath) != 1 {
		t.Fatal("the earlier consent did not open a window")
	}
	if err := SetupTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if m := sharedMetricsOf(cfg); m["enabled"] != false || m["send"] != false {
		t.Fatalf("config %v", m)
	}
	if n := openConsentWindows(t, store.DatabasePath); n != 0 {
		t.Fatalf("disabling collection left %d send consent windows open", n)
	}
}

func TestNoAnswerSurvivesTheCallersLaterConfigSave(t *testing.T) {
	ctx, _ := scriptedSetupConsole(t)
	h := home.Dir(ctx)
	swapTelemetryPrompt(t, func(bool) bool { return false })
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetupTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	raw, err := config.ReadRawConfig(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	got := sharedMetricsOf(raw)
	if len(got) != 3 || got["enabled"] != false || got["send"] != false || normalizedJSON(t, got["offer_version"]) != "2" {
		t.Fatalf("raw shared metrics %v", got)
	}
}

func TestEveryCliPromiseThatPreOptInDataStaysLocalNamesTheInstallNote(t *testing.T) {
	var lines []string
	for _, line := range sendConsentExplainer {
		lines = append(lines, line.(string))
	}
	explainer := strings.Join(lines, " ")
	for _, text := range []string{explainer, strings.Join(strings.Fields(observability.OfferDescription()), " ")} {
		before, _, found := strings.Cut(text, "before you opt in")
		if !found || !strings.Contains(before, "install note") {
			t.Fatalf("promise without the install note: %q", text)
		}
	}
	longest := 0
	for _, line := range lines {
		longest = max(longest, len([]rune(line)))
	}
	if longest+2 > 80 {
		t.Fatalf("explainer line of %d columns", longest)
	}
}
