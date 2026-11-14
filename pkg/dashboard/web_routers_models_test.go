package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Swaps the scoped config read of the model routes for one answering a fixed document
func fakeScopedConfig(t *testing.T, cfg map[string]any) {
	t.Helper()
	real := loadConfigScoped
	loadConfigScoped = func(context.Context, string) (map[string]any, error) { return cfg, nil }
	t.Cleanup(func() { loadConfigScoped = real })
}

// Swaps the context length resolver of the model info route
func fakeContextLength(t *testing.T, fn func(context.Context, provider.ContextLengthRequest) int) {
	t.Helper()
	real := modelInfoContextLength
	modelInfoContextLength = fn
	t.Cleanup(func() { modelInfoContextLength = real })
}

func probeConfig(model, providerName, baseURL string) map[string]any {
	return map[string]any{"model": map[string]any{"default": model, "provider": providerName, "base_url": baseURL}}
}

func TestModelInfoDegradesWhenTheContextProbeExceedsItsBudget(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fakeScopedConfig(t, probeConfig("some-model", "custom-proxy", "http://localhost:9/v1"))
	realBudget := modelInfoProbeBudget
	modelInfoProbeBudget = 200 * time.Millisecond
	t.Cleanup(func() { modelInfoProbeBudget = realBudget })
	fakeContextLength(t, func(context.Context, provider.ContextLengthRequest) int {
		time.Sleep(time.Second)
		panic("the abandoned probe should never be awaited")
	})
	started := time.Now()
	got, err := getModelInfo(context.Background(), "")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	info := pyjson.Plain(got).(map[string]any)
	if info["model"] != "some-model" || info["provider"] != "custom-proxy" || !pyjson.Equal(info["auto_context_length"], int64(0)) {
		t.Fatalf("info %v", info)
	}
	if elapsed >= 900*time.Millisecond {
		t.Fatalf("elapsed %v", elapsed)
	}
	time.Sleep(900 * time.Millisecond)
}

func TestModelInfoSurfacesTheContextValueWhenTheProbeIsFast(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fakeScopedConfig(t, probeConfig("some-model", "custom-proxy", "http://localhost:9/v1"))
	fakeContextLength(t, func(context.Context, provider.ContextLengthRequest) int { return 131072 })
	got, err := getModelInfo(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	info := pyjson.Plain(got).(map[string]any)
	if info["model"] != "some-model" || !pyjson.Equal(info["auto_context_length"], int64(131072)) ||
		!pyjson.Equal(info["effective_context_length"], int64(131072)) {
		t.Fatalf("info %v", info)
	}
}

// Makes the process look booted on one checkout revision while the disk holds another, or current when stale is false
func fakeCodeSkew(t *testing.T, stale bool) {
	t.Helper()
	if !stale {
		t.Cleanup(gateway.SwapCodeSkewFingerprints(nil, func() (string, bool) { return "", false }))
		return
	}
	boot := "git:abc1234567"
	t.Cleanup(gateway.SwapCodeSkewFingerprints(&boot, func() (string, bool) { return "git:def4567890", true }))
}

func TestModelOptionsSkewGuardStaleDashboardReturns503AndSkipsPayloadBuild(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fakeCodeSkew(t, true)
	calls := 0
	real := buildModelOptionsPayload
	buildModelOptionsPayload = func(context.Context, provider.PickerConfigContext, bool, bool, bool) (map[string]any, error) {
		calls++
		return map[string]any{"providers": []any{}}, nil
	}
	t.Cleanup(func() { buildModelOptionsPayload = real })
	_, err := getModelOptions(context.Background(), "", false, false, false)
	h, ok := asHTTPException(err)
	if !ok || h.StatusCode != 503 || !strings.Contains(strings.ToLower(h.Detail.(string)), "restart") {
		t.Fatalf("err %v", err)
	}
	if calls != 0 {
		t.Fatalf("payload built %d times", calls)
	}
}

func TestModelOptionsSkewGuardFreshDashboardBuildsPayloadUnchanged(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fakeCodeSkew(t, false)
	realLoad, realBuild := loadPickerContext, buildModelOptionsPayload
	t.Cleanup(func() { loadPickerContext, buildModelOptionsPayload = realLoad, realBuild })
	loadPickerContext = func(context.Context) (provider.PickerConfigContext, error) {
		return provider.PickerConfigContext{}, nil
	}
	calls := 0
	expected := map[string]any{"providers": []any{}, "model": map[string]any{}, "provider": nil}
	buildModelOptionsPayload = func(context.Context, provider.PickerConfigContext, bool, bool, bool) (map[string]any, error) {
		calls++
		return expected, nil
	}
	got, err := getModelOptions(context.Background(), "", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pyjson.Plain(got), expected) || calls != 1 {
		t.Fatalf("got %v calls %d", pyjson.Plain(got), calls)
	}
}

// Validated body of a model route the way FastAPI hands it to the handler, with the fields the JSON carried
func modelRouteBody(t *testing.T, m *Model, payload map[string]any) (*pyjson.Object, map[string]bool) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := pyjson.LoadsBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	body, errs := m.Validate(decoded)
	if len(errs) > 0 {
		t.Fatalf("payload refused: %+v", errs)
	}
	set := map[string]bool{}
	for k := range payload {
		set[k] = true
	}
	return body.(*pyjson.Object), set
}

func TestModelSetSkewGuardStaleModelSetReturns503AndSkipsAssignment(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fakeCodeSkew(t, true)
	applied := 0
	real := applyModelAssignmentSync
	applyModelAssignmentSync = func(context.Context, string, string, string, string, string, string, provider.AuxReasoningEffort, *provider.PreparedMainAssignment) (map[string]any, error) {
		applied++
		return nil, nil
	}
	t.Cleanup(func() { applyModelAssignmentSync = real })
	body, set := modelRouteBody(t, modelAssignmentModel, map[string]any{"scope": "main", "provider": "nous", "model": "some-model"})
	_, err := setModelAssignment(context.Background(), body, set, "")
	h, ok := asHTTPException(err)
	if !ok || h.StatusCode != 503 || !strings.Contains(strings.ToLower(h.Detail.(string)), "restart") {
		t.Fatalf("err %v", err)
	}
	if applied != 0 {
		t.Fatalf("assignment applied %d times", applied)
	}
}

func TestModelSetSkewGuardFreshModelSetProceeds(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fakeCodeSkew(t, false)
	realWarn, realPrepare, realApply := combinedSelectionWarning, prepareMainAssignment, applyModelAssignmentSync
	t.Cleanup(func() {
		combinedSelectionWarning, prepareMainAssignment, applyModelAssignmentSync = realWarn, realPrepare, realApply
	})
	combinedSelectionWarning = func(context.Context, provider.SelectionRequest) *provider.SelectionWarning {
		panic("_to_thread() got an unexpected keyword argument 'provider'")
	}
	prepareMainAssignment = func(context.Context, map[string]any, string, string, string, string) (*provider.PreparedMainAssignment, error) {
		return &provider.PreparedMainAssignment{}, nil
	}
	applyModelAssignmentSync = func(context.Context, string, string, string, string, string, string, provider.AuxReasoningEffort, *provider.PreparedMainAssignment) (map[string]any, error) {
		return map[string]any{"ok": true, "scope": "main"}, nil
	}
	body, set := modelRouteBody(t, modelAssignmentModel, map[string]any{"scope": "main", "provider": "nous", "model": "some-model"})
	got, err := setModelAssignment(context.Background(), body, set, "")
	if err != nil {
		t.Fatal(err)
	}
	if pyjson.Plain(got).(map[string]any)["ok"] != true {
		t.Fatalf("result %v", pyjson.Plain(got))
	}
}

func TestModelInfoEndpointModelInfoWithDictConfig(t *testing.T) {
	client := newTestClient(t, dashboardTestApp(t))
	fakeScopedConfig(t, map[string]any{"model": map[string]any{"default": "anthropic/claude-opus-4.6", "provider": "openrouter",
		"context_length": 100000}})
	fakeContextLength(t, func(context.Context, provider.ContextLengthRequest) int { return 200000 })
	data := jsonObject(t, client.get("/api/model/info"))
	if data["model"] != "anthropic/claude-opus-4.6" || data["provider"] != "openrouter" ||
		!pyjson.Equal(data["auto_context_length"], int64(200000)) || !pyjson.Equal(data["config_context_length"], int64(100000)) ||
		!pyjson.Equal(data["effective_context_length"], int64(100000)) {
		t.Fatalf("data %v", data)
	}
}

func TestModelInfoEndpointModelInfoGracefulOnMetadataError(t *testing.T) {
	client := newTestClient(t, dashboardTestApp(t))
	fakeScopedConfig(t, map[string]any{"model": "some/obscure-model"})
	fakeContextLength(t, func(context.Context, provider.ContextLengthRequest) int { panic("boom") })
	resp := client.get("/api/model/info")
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	if data := jsonObject(t, resp); !pyjson.Equal(data["auto_context_length"], int64(0)) {
		t.Fatalf("data %v", data)
	}
}

// Raw config.yaml of a home parsed from disk with no cache
func rawConfigOnDisk(t *testing.T, h string) map[string]any {
	t.Helper()
	cfg, err := config.ReadUserConfigRaw(context.Background(), filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Echoes every main slot pick back as an accepted route, the switch_model fake of the profile scoped model cases
func acceptAnyModel(t *testing.T) {
	t.Helper()
	t.Cleanup(provider.SwapAssignmentSwitchModel(func(_ context.Context, opts provider.SwitchModelOptions) (provider.ModelSwitchResult, error) {
		return provider.ModelSwitchResult{Success: true, NewModel: opts.RawInput, TargetProvider: opts.ExplicitProvider}, nil
	}))
}

func TestProfileScopedModelModelSetMainScoped(t *testing.T) {
	c, defaultHome, workerHome := isolatedProfiles(t)
	acceptAnyModel(t)
	resp := c.send("POST", "/api/model/set", map[string]any{"scope": "main", "provider": "openrouter", "model": "test/model-1",
		"confirm_expensive_model": true, "profile": "worker_beta"})
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	modelCfg, ok := rawConfigOnDisk(t, workerHome)["model"].(map[string]any)
	if !ok || modelCfg["provider"] != "openrouter" {
		t.Fatalf("worker model %v", rawConfigOnDisk(t, workerHome)["model"])
	}
	if defaultModel, ok := rawConfigOnDisk(t, defaultHome)["model"].(map[string]any); ok && defaultModel["default"] == "test/model-1" {
		t.Fatalf("default home took the write: %v", defaultModel)
	}
}

func TestProfileScopedModelModelInfoUnknownProfile404(t *testing.T) {
	c, _, _ := isolatedProfiles(t)
	if resp := c.get("/api/model/info?profile=ghost"); resp.Status != 404 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

func moaSlot(providerName, model string) map[string]any {
	return map[string]any{"provider": providerName, "model": model}
}

// Preset payload with one reference slot of the named model and the fixed aggregator
func moaPreset(model string) map[string]any {
	return map[string]any{"reference_models": []any{moaSlot("openai-codex", model)},
		"aggregator": moaSlot("anthropic", "claude-opus-5"), "enabled": true}
}

func threeMoaPresets() map[string]any {
	stored := func(model string) map[string]any {
		return map[string]any{"reference_models": []any{moaSlot("openai-codex", model)},
			"aggregator": moaSlot("anthropic", "claude-opus-5"), "enabled": true}
	}
	return map[string]any{"default_preset": "keep_a", "presets": map[string]any{
		"keep_a": stored("gpt-5.5"), "doomed": stored("gpt-5.6-luna"), "keep_b": stored("gpt-5.7")}}
}

// Temp home whose config.yaml holds the moa section and the extra sections
func seedMoaHome(t *testing.T, moa map[string]any, sections map[string]any) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	doc := map[string]any{"moa": moa}
	for k, v := range sections {
		doc[k] = v
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

// Runs the moa save handler with a validated payload
func saveMoa(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	body, _ := modelRouteBody(t, moaConfigPayloadModel, payload)
	got, err := setMoaModels(context.Background(), body, "")
	if err != nil {
		t.Fatal(err)
	}
	return pyjson.Plain(got).(map[string]any)
}

func moaOnDisk(t *testing.T, h string) map[string]any {
	t.Helper()
	moa, ok := rawConfigOnDisk(t, h)["moa"].(map[string]any)
	if !ok {
		t.Fatalf("moa section missing: %v", rawConfigOnDisk(t, h))
	}
	return moa
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func basePayload() map[string]any {
	return map[string]any{"default_preset": "default", "active_preset": "", "presets": map[string]any{
		"default": map[string]any{"reference_models": []any{moaSlot("openai-codex", "gpt-5.5")},
			"aggregator": moaSlot("openrouter", "anthropic/claude-opus-4.8"), "max_tokens": 4096, "enabled": true}}}
}

func TestSetMoaModelsPreservesUndeclaredKeysSaveTracesPreserved(t *testing.T) {
	h := seedMoaHome(t, map[string]any{"save_traces": true, "trace_dir": "/custom/traces", "default_preset": "default",
		"presets": map[string]any{"default": map[string]any{"reference_models": []any{moaSlot("openai-codex", "gpt-5.5")},
			"aggregator": moaSlot("openrouter", "anthropic/claude-opus-4.8"), "max_tokens": 4096, "enabled": true}}}, nil)
	saveMoa(t, basePayload())
	moa := moaOnDisk(t, h)
	if moa["save_traces"] != true || moa["trace_dir"] != "/custom/traces" {
		t.Fatalf("moa %v", moa)
	}
}

func TestMoaSaveWritesOnlyTheMoaSection(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	chain := []any{map[string]any{"provider": "custom", "model": "glm-5.08", "base_url": "http://gw:8080/v1"}}
	configCaseSave(t, h, map[string]any{"model": map[string]any{"default": "m1", "provider": "custom"}})
	if _, err := webLoadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw := rawConfigOnDisk(t, h)
	raw["fallback_providers"] = chain
	text, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), append(text, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	saveMoa(t, basePayload())
	onDisk := rawConfigOnDisk(t, h)
	if !reflect.DeepEqual(pyjson.Plain(configJSON(onDisk["fallback_providers"])), pyjson.Plain(configJSON(chain))) {
		t.Fatalf("fallback_providers %v", onDisk["fallback_providers"])
	}
	refs := onDisk["moa"].(map[string]any)["presets"].(map[string]any)["default"].(map[string]any)["reference_models"].([]any)
	if refs[0].(map[string]any)["model"] != "gpt-5.5" {
		t.Fatalf("refs %v", refs)
	}
}

func TestOmittedPresetIsDeletedFromDisk(t *testing.T) {
	h := seedMoaHome(t, threeMoaPresets(), nil)
	saveMoa(t, map[string]any{"default_preset": "keep_a", "active_preset": "",
		"presets": map[string]any{"keep_a": moaPreset("gpt-5.5"), "keep_b": moaPreset("gpt-5.7")}})
	if got := sortedKeys(moaOnDisk(t, h)["presets"].(map[string]any)); !reflect.DeepEqual(got, []string{"keep_a", "keep_b"}) {
		t.Fatalf("presets %v", got)
	}
}

func TestDeletionPreservesUndeclaredMoaKeysAndOtherSections(t *testing.T) {
	moa := threeMoaPresets()
	moa["save_traces"], moa["trace_dir"] = true, "/custom/traces"
	chain := []any{map[string]any{"provider": "custom", "model": "glm-5.08", "base_url": "http://gw:8080/v1"}}
	h := seedMoaHome(t, moa, map[string]any{"fallback_providers": chain})
	saveMoa(t, map[string]any{"default_preset": "keep_a", "active_preset": "",
		"presets": map[string]any{"keep_a": moaPreset("gpt-5.5"), "keep_b": moaPreset("gpt-5.7")}})
	onDisk := rawConfigOnDisk(t, h)
	saved := onDisk["moa"].(map[string]any)
	if _, held := saved["presets"].(map[string]any)["doomed"]; held {
		t.Fatal("deleted preset survived")
	}
	if saved["save_traces"] != true || saved["trace_dir"] != "/custom/traces" {
		t.Fatalf("moa %v", saved)
	}
	if !reflect.DeepEqual(pyjson.Plain(configJSON(onDisk["fallback_providers"])), pyjson.Plain(configJSON(chain))) {
		t.Fatalf("fallback_providers %v", onDisk["fallback_providers"])
	}
}

func TestAddingAPresetStillWorks(t *testing.T) {
	h := seedMoaHome(t, threeMoaPresets(), nil)
	saveMoa(t, map[string]any{"default_preset": "keep_a", "active_preset": "", "presets": map[string]any{
		"keep_a": moaPreset("gpt-5.5"), "doomed": moaPreset("gpt-5.6-luna"), "keep_b": moaPreset("gpt-5.7"), "fresh": moaPreset("gpt-5.9")}})
	presets := moaOnDisk(t, h)["presets"].(map[string]any)
	if got := sortedKeys(presets); !reflect.DeepEqual(got, []string{"doomed", "fresh", "keep_a", "keep_b"}) {
		t.Fatalf("presets %v", got)
	}
	if presets["fresh"].(map[string]any)["reference_models"].([]any)[0].(map[string]any)["model"] != "gpt-5.9" {
		t.Fatalf("fresh %v", presets["fresh"])
	}
}

func TestDeletionPreservesPrivacyFilterAndRetainedPresetMetadata(t *testing.T) {
	moa := threeMoaPresets()
	moa["privacy_filter"], moa["save_traces"], moa["trace_dir"] = "full", true, "/custom/traces"
	moa["presets"].(map[string]any)["keep_a"].(map[string]any)["operator_note"] = "retain me"
	h := seedMoaHome(t, moa, nil)
	saveMoa(t, map[string]any{"default_preset": "keep_a", "active_preset": "",
		"presets": map[string]any{"keep_a": moaPreset("gpt-5.5"), "keep_b": moaPreset("gpt-5.7")}})
	onDisk := moaOnDisk(t, h)
	presets := onDisk["presets"].(map[string]any)
	if _, held := presets["doomed"]; held {
		t.Fatal("deleted preset survived")
	}
	if onDisk["privacy_filter"] != "full" || onDisk["save_traces"] != true || onDisk["trace_dir"] != "/custom/traces" ||
		presets["keep_a"].(map[string]any)["operator_note"] != "retain me" {
		t.Fatalf("moa %v", onDisk)
	}
}

func TestLegacyFlatUpdatePreservesNamedPresetsAndValidDefault(t *testing.T) {
	for _, tc := range []struct {
		id             string
		includePresets bool
	}{{"missing-presets", false}, {"empty-presets", true}} {
		t.Run(tc.id, func(t *testing.T) {
			seeded := threeMoaPresets()
			seeded["active_preset"] = "keep_b"
			h := seedMoaHome(t, seeded, nil)
			payload := map[string]any{"active_preset": "", "reference_models": []any{moaSlot("openai-codex", "gpt-5.8")},
				"aggregator": moaSlot("anthropic", "claude-opus-5"), "enabled": true}
			if tc.includePresets {
				payload["presets"] = map[string]any{}
			}
			saveMoa(t, payload)
			moa := moaOnDisk(t, h)
			presets := moa["presets"].(map[string]any)
			if got := sortedKeys(presets); !reflect.DeepEqual(got, []string{"doomed", "keep_a", "keep_b"}) {
				t.Fatalf("presets %v", got)
			}
			if moa["default_preset"] != "keep_a" || moa["active_preset"] != "keep_b" {
				t.Fatalf("names %v %v", moa["default_preset"], moa["active_preset"])
			}
			keepA := pyjson.Plain(configJSON(presets["keep_a"])).(map[string]any)
			if !reflect.DeepEqual(keepA["reference_models"], []any{map[string]any{"provider": "openai-codex", "model": "gpt-5.8", "enabled": true}}) {
				t.Fatalf("keep_a references %v", keepA["reference_models"])
			}
			if !reflect.DeepEqual(keepA["aggregator"], map[string]any{"provider": "anthropic", "model": "claude-opus-5"}) {
				t.Fatalf("keep_a aggregator %v", keepA["aggregator"])
			}
			resolved, err := provider.ResolveMoAPreset(moa, "")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(pyjson.Plain(configJSON(resolved["reference_models"])), keepA["reference_models"]) {
				t.Fatalf("resolved %v", resolved["reference_models"])
			}
			for _, name := range []string{"keep_b", "doomed"} {
				want := pyjson.Plain(configJSON(threeMoaPresets()["presets"].(map[string]any)[name]))
				if got := pyjson.Plain(configJSON(presets[name])); !reflect.DeepEqual(got, want) {
					t.Fatalf("%s %v, want %v", name, got, want)
				}
			}
		})
	}
}

func TestLegacyFlatUpdateWithNamedDefaultAndEmptyMapDoesNotFail(t *testing.T) {
	h := seedMoaHome(t, threeMoaPresets(), nil)
	result := saveMoa(t, map[string]any{"default_preset": "keep_b", "presets": map[string]any{},
		"reference_models": []any{moaSlot("openai-codex", "gpt-5.8")}, "aggregator": moaSlot("anthropic", "claude-opus-5"), "enabled": true})
	moa := moaOnDisk(t, h)
	presets := moa["presets"].(map[string]any)
	if result["ok"] != true || moa["default_preset"] != "keep_a" {
		t.Fatalf("result %v default %v", result["ok"], moa["default_preset"])
	}
	if got := sortedKeys(presets); !reflect.DeepEqual(got, []string{"doomed", "keep_a", "keep_b"}) {
		t.Fatalf("presets %v", got)
	}
	keepA := pyjson.Plain(configJSON(presets["keep_a"])).(map[string]any)
	if !reflect.DeepEqual(keepA["reference_models"], []any{map[string]any{"provider": "openai-codex", "model": "gpt-5.8", "enabled": true}}) {
		t.Fatalf("keep_a references %v", keepA["reference_models"])
	}
}
