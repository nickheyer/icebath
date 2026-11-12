package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func moaEnabledRefs(refs []map[string]any) []any {
	out := []any{}
	for _, slot := range refs {
		copied := map[string]any{"enabled": true}
		for k, v := range slot {
			copied[k] = v
		}
		out = append(out, copied)
	}
	return out
}

func TestMoASlotPickerExcludesUnconfiguredProviders(t *testing.T) {
	prevLoad, prevBuild := moaLoadPickerContextFn, moaBuildModelsPayloadFn
	t.Cleanup(func() { moaLoadPickerContextFn, moaBuildModelsPayloadFn = prevLoad, prevBuild })
	var captured ModelsPayloadOptions
	moaLoadPickerContextFn = func(context.Context) (PickerConfigContext, error) { return PickerConfigContext{}, nil }
	moaBuildModelsPayloadFn = func(_ context.Context, _ PickerConfigContext, opts ModelsPayloadOptions) (map[string]any, error) {
		captured = opts
		return map[string]any{"providers": []any{
			map[string]any{"slug": "moa", "models": []any{"default"}},
			map[string]any{"slug": "opencode-go", "models": []any{"deepseek-v4-pro"}},
		}}, nil
	}
	rows, err := moaModelOptions(context.Background())
	if err != nil || len(rows) != 1 || rows[0]["slug"] != "opencode-go" {
		t.Fatalf("rows %v %v", rows, err)
	}
	if captured.IncludeUnconfigured || !captured.PickerHints || !captured.CanonicalOrder || !captured.Pricing ||
		!captured.Capabilities || captured.MaxModels == nil || *captured.MaxModels != 200 {
		t.Fatalf("payload options %+v", captured)
	}
}

func TestNormalizeMoAConfigUsesDefaultNamedPreset(t *testing.T) {
	cfg := normalizeMoAConfig(map[string]any{})
	mustEqual(t, cfg["default_preset"], any(DefaultMoAPresetName), "default preset")
	if names, _ := moaPresetNames(map[string]any{}); !reflect.DeepEqual(names, []string{DefaultMoAPresetName}) {
		t.Fatalf("names %v", names)
	}
	if !reflect.DeepEqual(cfg["reference_models"], moaEnabledRefs(defaultMoAReferenceModels)) {
		t.Fatalf("refs %v", cfg["reference_models"])
	}
	if !reflect.DeepEqual(cfg["aggregator"], defaultMoAAggregator) {
		t.Fatalf("aggregator %v", cfg["aggregator"])
	}
}

func TestExactPresetMatchingSkipsDisabledPresets(t *testing.T) {
	cfg := map[string]any{"presets": map[string]any{"default": map[string]any{"enabled": false}, "klo": map[string]any{"enabled": false}}}
	if ExactMoAPresetName(cfg, "default") != "" || ExactMoAPresetName(cfg, "klo") != "" {
		t.Fatal("disabled presets must not match the implicit switch")
	}
	if ExactMoAPresetName(map[string]any{"presets": map[string]any{"review": map[string]any{}}}, " review ") != "review" {
		t.Fatal("an enabled preset matches its stripped name")
	}
}

func TestResolveMissingMoAPresetHasActionableError(t *testing.T) {
	cfg := map[string]any{"default_preset": "日常对话-高峰", "presets": map[string]any{"日常对话-高峰": map[string]any{}, "日常对话-非高峰": map[string]any{}}}
	_, err := ResolveMoAPreset(cfg, "日常对话-高峰期")
	var missing *MoAPresetNotFoundError
	if err == nil || !errors.As(err, &missing) {
		t.Fatalf("want a missing preset error, got %v", err)
	}
	for _, want := range []string{"日常对话-高峰期", "日常对话-高峰", "日常对话-非高峰", "icebath moa list"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%q missing from %q", want, err.Error())
		}
	}
}

func TestMissingMoAPresetIsNonRetryable(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	_, err := ResolveMoAPreset(map[string]any{}, "old")
	result := ClassifyError(ClassifyInput{Err: err, Provider: "moa", Model: "old"})
	if result.Reason != ReasonModelNotFound || result.Retryable || result.ShouldFallback {
		t.Fatalf("classified %+v", result)
	}
}

func moaValidPresetPayload() map[string]any {
	return map[string]any{
		"reference_models": []any{map[string]any{"provider": "openrouter", "model": "deepseek/deepseek-v4-pro"}},
		"aggregator":       map[string]any{"provider": "openrouter", "model": "anthropic/claude-opus-4.8"},
	}
}

func TestValidateMoAPayloadAgreesWithCleanSlot(t *testing.T) {
	payload := map[string]any{"presets": map[string]any{"p": moaValidPresetPayload()}}
	if problems := ValidateMoAPayload(payload); len(problems) != 0 {
		t.Fatalf("problems %v", problems)
	}
	preset := anyMap(anyMap(normalizeMoAConfig(payload)["presets"])["p"])
	want := moaEnabledRefs([]map[string]any{{"provider": "openrouter", "model": "deepseek/deepseek-v4-pro"}})
	if !reflect.DeepEqual(preset["reference_models"], want) || !reflect.DeepEqual(preset["aggregator"], moaValidPresetPayload()["aggregator"]) {
		t.Fatalf("slots changed %v", preset)
	}
	bad := map[string]any{"presets": map[string]any{"p": map[string]any{
		"reference_models": []any{map[string]any{"provider": "openrouter"}, map[string]any{"provider": "moa", "model": "x"}},
		"aggregator":       map[string]any{"model": "m"},
	}}}
	want2 := []string{
		"preset 'p' reference 1: model is required (provider 'openrouter' has no model selected)",
		"preset 'p' reference 2: the Mixture of Agents provider cannot be used inside a preset (recursive MoA)",
		"preset 'p': needs at least one complete reference model",
		"preset 'p' aggregator: provider is required",
	}
	if got := ValidateMoAPayload(bad); !reflect.DeepEqual(got, want2) {
		t.Fatalf("problems %q", got)
	}
	if got := ValidateMoAPayload([]any{}); !reflect.DeepEqual(got, []string{"MoA config must be an object"}) {
		t.Fatalf("non object %v", got)
	}
}

func TestConfigKeyAggregatorFieldsAreTheSlotFieldsCleanSlotKeeps(t *testing.T) {
	clean := cleanMoASlot(map[string]any{"provider": "p", "model": "m", "reasoning_effort": "high", "extra": "dropped"}, false)
	keys := map[string]bool{}
	for k := range clean {
		keys[k] = true
	}
	if !reflect.DeepEqual(keys, moaAggregatorSlotFields) {
		t.Fatalf("clean slot keys %v", keys)
	}
}

func TestPrintConfigMarksAggregatorAsBilledAndWarnsOnProviderMismatch(t *testing.T) {
	var out strings.Builder
	moaPrintConfig(&out, map[string]any{"model": map[string]any{"provider": "openai-codex"}})
	var notice string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "Aggregator is on") {
			notice = line
			break
		}
	}
	if !strings.Contains(notice, "openrouter") || !strings.Contains(notice, "openai-codex") {
		t.Fatalf("notice %q in %s", notice, out.String())
	}
}

func TestBillingNoticeSilentWhenProvidersMatchOrMainUnknown(t *testing.T) {
	for _, cfg := range []map[string]any{{"model": map[string]any{"provider": "openrouter"}}, {}} {
		var out strings.Builder
		moaPrintConfig(&out, cfg)
		if strings.Contains(out.String(), "Aggregator is on") {
			t.Fatalf("unexpected notice for %v", cfg)
		}
	}
}

func TestNonfiniteFanoutFallsBackToDefaultCadence(t *testing.T) {
	def := normalizeMoAConfig(map[string]any{})["fanout"]
	for _, fanout := range []any{map[string]any{"mode": "every_n", "n": math.Inf(1)}, map[string]any{"mode": "every_n", "n": "-inf"}, "every_n:inf", "every_n:nan"} {
		mustEqual(t, normalizeMoAConfig(map[string]any{"fanout": fanout})["fanout"], def, "nonfinite fanout")
	}
	mustEqual(t, normalizeMoAConfig(map[string]any{"fanout": map[string]any{"mode": "every_n", "n": "3.0"}})["fanout"], any("every_n:3"), "float string n")
}

func TestNonfiniteTemperaturesDoNotEscapeIntoJSON(t *testing.T) {
	for _, temperature := range []any{math.NaN(), math.Inf(1), "-inf", "NaN"} {
		normalized := normalizeMoAConfig(map[string]any{"reference_temperature": temperature, "aggregator_temperature": temperature})
		if normalized["reference_temperature"] != nil || normalized["aggregator_temperature"] != nil {
			t.Fatalf("%v escaped %v", temperature, normalized)
		}
		if _, err := json.Marshal(normalized); err != nil {
			t.Fatal(err)
		}
	}
	finite := normalizeMoAConfig(map[string]any{"reference_temperature": 0, "aggregator_temperature": "0.75"})
	mustEqual(t, finite["reference_temperature"], any(0.0), "zero temperature")
	mustEqual(t, finite["aggregator_temperature"], any(0.75), "string temperature")
}

func TestMoAFanoutAndNumberCoercion(t *testing.T) {
	for in, want := range map[any]string{"every_n:1": "per_iteration", "every_n:2": "every_n:2", " EVERY_N: 4 ": "every_n:4",
		"every_n": "user_turn", "every_n:-3": "user_turn", "per_iteration": "per_iteration", "bogus": "user_turn",
		5: "user_turn", "every_n:1_0": "every_n:10", "every_n:99999999999999999999": "every_n:99999999999999999999"} {
		mustEqual(t, coerceMoAFanout(in), want, "fanout")
	}
	mustEqual(t, coerceMoAFanout(map[string]any{"mode": "user_turn"}), "user_turn", "mapping mode")
	mustEqual(t, coerceMoAFanout(map[string]any{"mode": "every_n", "n": true}), "per_iteration", "bool n")
	mustEqual(t, coerceMoAFanout(map[string]any{"mode": "every_n", "n": 3.9}), "every_n:3", "float n truncates")
	mustEqual(t, coerceMoAReferenceTimeout(true), any(nil), "bool timeout")
	mustEqual(t, coerceMoAReferenceTimeout("900"), any(900.0), "string timeout")
	mustEqual(t, coerceMoAReferenceTimeout(-1), any(nil), "negative timeout")
	mustEqual(t, coerceMoAReferenceTimeout("1_000.5"), any(1000.5), "underscore timeout")
	mustEqual(t, coerceMoAReferenceTimeout("0x10"), any(nil), "hex timeout")
}

func TestMoAPrivacyFilterAndEffortNormalization(t *testing.T) {
	for in, want := range map[any]string{true: "full", false: "", "display": "display", " FULL ": "full", "yes": "full", 1: "full", "bogus": ""} {
		mustEqual(t, CoercePrivacyFilter(in), want, "privacy filter")
	}
	mustEqual(t, CoercePrivacyFilter(nil), "", "nil filter")
	for in, want := range map[any]string{"HIGH": "high", false: "none", "disabled": "none", "hgih": "", 1: ""} {
		mustEqual(t, cleanMoAReasoningEffort(in), want, "effort")
	}
	mustEqual(t, cleanMoAReasoningEffort(map[string]any{"enabled": false}), "none", "dict disabled")
	mustEqual(t, cleanMoAReasoningEffort(map[string]any{"effort": " fast "}), "fast", "dict passthrough")
	mustEqual(t, cleanMoAReasoningEffort(true), "", "true effort")
}

func TestMoAReferenceSlotsAcceptJSONStringsAndSingleMappings(t *testing.T) {
	preset := normalizeMoAPreset(map[string]any{"reference_models": `[{"provider": "a", "model": "b", "enabled": "off"}, {"provider": 7, "model": 1e5}]`,
		"degraded_reference_policy": " SILENT ", "enabled": "no"})
	want := []any{map[string]any{"provider": "a", "model": "b", "enabled": false}, map[string]any{"provider": "7", "model": "100000.0", "enabled": true}}
	if !reflect.DeepEqual(preset["reference_models"], want) || preset["degraded_reference_policy"] != "silent" || preset["enabled"] != false {
		t.Fatalf("preset %v", preset)
	}
	nan := normalizeMoAPreset(map[string]any{"reference_models": `{"provider": "p", "model": "m", "enabled": NaN}`})
	if !reflect.DeepEqual(nan["reference_models"], []any{map[string]any{"provider": "p", "model": "m", "enabled": true}}) {
		t.Fatalf("NaN literal slot %v", nan["reference_models"])
	}
	broken := normalizeMoAPreset(map[string]any{"reference_models": "[not json", "degraded_reference_policy": "weird"})
	if !reflect.DeepEqual(broken["reference_models"], defaultMoAReferenceSlots()) || broken["degraded_reference_policy"] != "loud" {
		t.Fatalf("broken preset %v", broken)
	}
}

func TestMoADefaultAndActivePresetSelection(t *testing.T) {
	presets := map[string]any{" beta ": map[string]any{"fanout": "per_iteration"}, "alpha": map[string]any{}, "  ": map[string]any{}}
	config.SetMappingKeys(presets, []string{" beta ", "alpha", "  "})
	cfg := normalizeMoAConfig(map[string]any{"default_preset": "missing", "active_preset": "beta", "privacy_filter": "display",
		"presets": presets})
	if names, _ := moaPresetNames(cfg); !reflect.DeepEqual(names, []string{"beta", "alpha"}) {
		t.Fatalf("names %v", names)
	}
	if cfg["default_preset"] != "beta" || cfg["active_preset"] != "beta" || cfg["privacy_filter"] != "display" || cfg["fanout"] != "per_iteration" {
		t.Fatalf("selection %v", cfg)
	}
	provider, model, ok := resolveMoaAggregator(map[string]any{"moa": cfg}, "beta")
	if !ok || provider != "openrouter" || model != "anthropic/claude-opus-4.8" {
		t.Fatalf("aggregator %q %q %v", provider, model, ok)
	}
	if _, _, ok := resolveMoaAggregator(map[string]any{}, "nope"); ok {
		t.Fatal("a missing preset resolves no aggregator")
	}
}

func moaSplitKey(key string) []string {
	var parts []string
	var cur strings.Builder
	for i := 0; i < len(key); i++ {
		if key[i] == '\\' && i+1 < len(key) && key[i+1] == '.' {
			cur.WriteByte('.')
			i++
			continue
		}
		if key[i] == '.' {
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(key[i])
	}
	return append(parts, cur.String())
}

func TestMoAPresetFieldPaths(t *testing.T) {
	for _, name := range []string{"default", "review", "review.v2", "code review"} {
		escaped := strings.ReplaceAll(name, ".", `\.`)
		for _, field := range []string{"fanout", "reference_timeout", "aggregator.provider", "aggregator.model", "aggregator.reasoning_effort"} {
			if !IsPresetFieldPath(moaSplitKey("moa.presets." + escaped + "." + field)) {
				t.Fatalf("%s %s should be a field path", name, field)
			}
		}
		slot := map[string]any{"provider": "openrouter", "model": "test/actor"}
		preset, err := ResolveMoAPreset(map[string]any{"presets": map[string]any{name: map[string]any{"reference_models": []any{slot},
			"aggregator": map[string]any{"provider": "anthropic", "model": "test/judge", "reasoning_effort": "high"}, "fanout": "every_n:5", "reference_timeout": 900}}}, name)
		if err != nil || preset["fanout"] != "every_n:5" || preset["reference_timeout"] != 900.0 ||
			!reflect.DeepEqual(preset["aggregator"], map[string]any{"provider": "anthropic", "model": "test/judge", "reasoning_effort": "high"}) {
			t.Fatalf("preset %v %v", preset, err)
		}
	}
	for _, key := range []string{"moa.presets.default.fanoutt", "moa.presets.review.reference_timeot", `moa.presets.review\.v2.fanoutt`,
		"moa.presets.review.unexpected.fanout", "moa.presets.review.fanout.unexpected", "moa.presets. review .fanout", "moa.presets.review",
		"moa.presets.review.aggregator.bogus", "moa.presets.review.enabled.x", "moa.presets.review.fanout.n"} {
		if IsPresetFieldPath(moaSplitKey(key)) {
			t.Fatalf("%s must keep the unknown key notice", key)
		}
	}
}

func TestMoANamedPresetMapIsAuthoritativeOverDefaults(t *testing.T) {
	user := map[string]any{"moa": map[string]any{"default_preset": "keep_a", "presets": map[string]any{
		"keep_a": map[string]any{}, "doomed": map[string]any{}, "keep_b": map[string]any{}}}}
	merged := config.DeepMerge(map[string]any(config.Defaults()), config.Clone(user).(map[string]any))
	ApplyUserMoAPresets(merged, user)
	names := moaPresetOrder(anyMap(anyMap(merged["moa"])["presets"]))
	if !reflect.DeepEqual(names, []string{"doomed", "keep_a", "keep_b"}) {
		t.Fatalf("schema default resurrected %v", names)
	}
	empty := map[string]any{"moa": map[string]any{"presets": map[string]any{}}}
	merged = config.DeepMerge(map[string]any(config.Defaults()), config.Clone(empty).(map[string]any))
	ApplyUserMoAPresets(merged, empty)
	if len(anyMap(anyMap(merged["moa"])["presets"])) != 0 {
		t.Fatal("an explicit empty map stays empty")
	}
	plain := map[string]any{"model": map[string]any{}}
	merged = config.DeepMerge(map[string]any(config.Defaults()), plain)
	ApplyUserMoAPresets(merged, plain)
	if !reflect.DeepEqual(normalizeMoAConfig(merged["moa"]), normalizeMoAConfig(config.Defaults()["moa"])) {
		t.Fatal("a missing moa map keeps the schema defaults")
	}
	if !SkipDeepMerge("moa", "presets") || SkipDeepMerge("moa", "default_preset") || SkipDeepMerge("agent", "presets") {
		t.Fatal("only moa.presets skips the deep merge")
	}
}

func TestDecodeMoATurn(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"prompt": "hi there", "config": map[string]any{"fanout": "every_n:3"}})
	encoded := base64.URLEncoding.EncodeToString(payload)
	prompt, preset, err := DecodeMoATurn(MoAMarkerPrefix + " " + encoded + "\n")
	if err != nil || prompt != "hi there" || preset["fanout"] != "every_n:3" {
		t.Fatalf("decoded %q %v %v", prompt, preset, err)
	}
	stripped := strings.TrimRight(encoded, "=")
	if len(stripped) != len(encoded) {
		if got, preset, _ := DecodeMoATurn(MoAMarkerPrefix + stripped); preset != nil || got != MoAMarkerPrefix+stripped {
			t.Fatal("missing padding is not a marker")
		}
	}
	for _, msg := range []string{"plain text", MoAMarkerPrefix + "!!!", MoAMarkerPrefix + "é"} {
		if got, preset, err := DecodeMoATurn(msg); got != msg || preset != nil || err != nil {
			t.Fatalf("%q decoded %q %v %v", msg, got, preset, err)
		}
	}
	list := base64.URLEncoding.EncodeToString([]byte(`[1]`))
	if _, _, err := DecodeMoATurn(MoAMarkerPrefix + list); err == nil || err.Error() != "'list' object has no attribute 'get'" {
		t.Fatalf("non object payload %v", err)
	}
	if !strings.HasPrefix(MoAUsage(), "Usage: /moa <prompt>") {
		t.Fatal(MoAUsage())
	}
}

func TestMoAURLSafeBase64MatchesPythonNonStrictDecoding(t *testing.T) {
	for in, want := range map[string]string{"aGk=": "hi", "aG\nk=": "hi", "aGk=trailing": "hi", "aGVsbG8_": "hello?", "aGVsbG8-": "hello>"} {
		got, ok := moaURLSafeB64Decode(in)
		if !ok || string(got) != want {
			t.Fatalf("%q decoded %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"aGk", "a", "aGk=x="[:3]} {
		if _, ok := moaURLSafeB64Decode(in); ok {
			t.Fatalf("%q should be incorrect padding", in)
		}
	}
}

func TestMoAPresetOrderIsDefaultsThenFileOrder(t *testing.T) {
	doc, err := config.FastSafeLoad([]byte("moa:\n  presets:\n    zeta: {}\n    alpha: {}\n    \" alpha \": {}\n    mid: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	user := doc.(map[string]any)
	merged := config.DeepMerge(map[string]any(config.Defaults()), user)
	ApplyUserMoAPresets(merged, user)
	names, _ := moaPresetNames(merged["moa"])
	if !reflect.DeepEqual(names, []string{"zeta", "alpha", "mid"}) {
		t.Fatalf("presets %v", names)
	}
	if def := normalizeMoAConfig(map[string]any{"presets": merged["moa"].(map[string]any)["presets"]})["default_preset"]; def != "zeta" {
		t.Fatalf("first preset fallback %v", def)
	}
	defaults, _ := moaPresetNames(config.Defaults()["moa"])
	if !reflect.DeepEqual(defaults, []string{DefaultMoAPresetName}) {
		t.Fatalf("defaults %v", defaults)
	}
}

func TestNormalizeMoAConfigWrapsUnexported(t *testing.T) {
	raw := map[string]any{"default_preset": "duo", "presets": map[string]any{"duo": map[string]any{"aggregator": map[string]any{"provider": "openrouter", "model": "m"}}}}
	if got, want := NormalizeMoAConfig(raw), normalizeMoAConfig(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
