package provider

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Every declared wire vocabulary of the module, the dir(mod) *_EFFORTS walk of the hermes tests
var declaredWireEffortSets = map[string][]string{
	"OPENAI_COMPAT_WIRE_EFFORTS": openAICompatWireEfforts, "CODEX_GPT56_EFFORTS": codexGPT56Efforts, "CODEX_LEGACY_EFFORTS": codexLegacyEfforts,
	"CODEX_ASTRA_EFFORTS": codexAstraEfforts, "XAI_GROK46_EFFORTS": xaiGrok46Efforts, "XAI_LEGACY_EFFORTS": xaiLegacyEfforts,
	"ACTUAL_RELAY_EFFORTS": actualRelayEfforts, "KIMI_K3_EFFORTS": kimiK3Efforts, "KIMI_K2_EFFORTS": kimiK2Efforts, "OX_ALPHA_EFFORTS": oxAlphaEfforts,
	"TOKENHUB_EFFORTS": tokenhubEfforts, "NEBIUS_EFFORTS": nebiusEfforts, "SOLAR_EFFORTS": solarEfforts, "GLM52_EFFORTS": glm52Efforts,
	"GLM53_EFFORTS": glm53Efforts, "DEEPSEEK_V4_EFFORTS": deepseekV4Efforts, "OLLAMA_CLOUD_EFFORTS": ollamaCloudEfforts, "META_AI_EFFORTS": metaAIEfforts,
}

func sameSlice(a, b []string) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

func requestedEffortOf(v any) string {
	return requestedEffort(reasoningFromRequest(reasoningFromConfig(v)))
}

func TestSupportedLevelLabelIsTheLevelItself(t *testing.T) {
	if got := effortDisplayLabel("max", "openai-codex", "gpt-5.6-sol", ""); got != "max" {
		t.Fatalf("%q", got)
	}
	if got := effortDisplayLabel("high", "", "", ""); got != "high" {
		t.Fatalf("%q", got)
	}
	if got := effortDisplayLabel("", "", "", ""); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestCodexAppServerSendsUltraVerbatimOnlyWhereTheModelReachesMax(t *testing.T) {
	if got := effortDisplayLabel("ultra", "openai-codex", "gpt-5.6-sol", "codex_app_server"); got != "ultra" {
		t.Fatalf("%q", got)
	}
	if got := effortDisplayLabel("ultra", "openai-codex", "gpt-5.6-sol", ""); !strings.HasPrefix(got, "ultra (sends max") {
		t.Fatalf("%q", got)
	}
	if got := effortDisplayLabel("ultra", "openai-codex", "gpt-5.5", "codex_app_server"); !strings.HasPrefix(got, "ultra (sends xhigh") {
		t.Fatalf("%q", got)
	}
}

func TestLadderContractLadderIsSupersetOfValidEfforts(t *testing.T) {
	for _, level := range config.ValidReasoningEfforts {
		if !slices.Contains(effortLadder, level) {
			t.Fatalf("%s missing from the ladder", level)
		}
	}
}

func TestLadderContractNoDeclaredWireSetContainsUltra(t *testing.T) {
	for name, set := range declaredWireEffortSets {
		if slices.Contains(set, "ultra") {
			t.Fatalf("%s contains ultra", name)
		}
	}
}

func TestClampEffortSupportedLevelsPassThrough(t *testing.T) {
	for _, level := range openAICompatWireEfforts {
		if got := clampEffort(level, openAICompatWireEfforts, nil); got != level {
			t.Fatalf("%s -> %s", level, got)
		}
	}
}

func TestClampEffortUltraClampsToMaxOnOpenaiWire(t *testing.T) {
	if got := clampEffort("ultra", openAICompatWireEfforts, nil); got != "max" {
		t.Fatalf("%q", got)
	}
}

func TestClampEffortNearestWeakerNeverEscalates(t *testing.T) {
	if got := clampEffort("xhigh", []string{"low", "medium", "high", "max"}, nil); got != "high" {
		t.Fatalf("%q", got)
	}
}

func TestClampEffortFloorWhenNothingWeaker(t *testing.T) {
	if got := clampEffort("minimal", []string{"low", "medium"}, nil); got != "low" {
		t.Fatalf("%q", got)
	}
}

func TestClampEffortNoneIsNeverADegradationTarget(t *testing.T) {
	if got := clampEffort("minimal", []string{"none", "low", "high"}, nil); got != "low" {
		t.Fatalf("%q", got)
	}
}

func TestClampEffortNoneStillPassesThroughWhenRequested(t *testing.T) {
	if got := clampEffort("none", []string{"none", "low", "high"}, nil); got != "none" {
		t.Fatalf("%q", got)
	}
}

func TestClampEffortUnknownSupportedSetPassesThrough(t *testing.T) {
	if got := clampEffort("ultra", nil, nil); got != "ultra" {
		t.Fatalf("%q", got)
	}
	if got := clampEffort("ultra", []string{}, nil); got != "ultra" {
		t.Fatalf("%q", got)
	}
}

func TestClampEffortBespokeLevelPassesThrough(t *testing.T) {
	if got := clampEffort("turbo-9000", []string{"low", "high"}, nil); got != "turbo-9000" {
		t.Fatalf("%q", got)
	}
}

func TestClampEffortEmptyEffortPassesThrough(t *testing.T) {
	if got := clampEffort("", []string{"low"}, nil); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestClampEffortOverridesWin(t *testing.T) {
	if got := clampEffort("medium", kimiK3Efforts, kimiK3Overrides); got != "high" {
		t.Fatalf("%q", got)
	}
	if got := clampEffort("xhigh", kimiK3Efforts, kimiK3Overrides); got != "max" {
		t.Fatalf("%q", got)
	}
}

func TestClampEffortOverrideIgnoredWhenTargetUnsupported(t *testing.T) {
	if got := clampEffort("medium", []string{"low", "high"}, map[string]string{"medium": "max"}); got != "low" {
		t.Fatalf("%q", got)
	}
}

func TestClampEffortMonotonicOverFullLadder(t *testing.T) {
	var enabledLadder []string
	for _, l := range effortLadder {
		if l != "none" {
			enabledLadder = append(enabledLadder, l)
		}
	}
	for name, supported := range declaredWireEffortSets {
		prevRank := -1
		for _, level := range enabledLadder {
			out := clampEffort(level, supported, nil)
			rank := ladderIndex(out)
			if rank < prevRank {
				t.Fatalf("%s: %s -> %s inverts the ladder", name, level, out)
			}
			prevRank = rank
		}
	}
}

func TestKimiVocabularyK3Slugs(t *testing.T) {
	for _, model := range []string{"k3", "kimi-k3", "kimi-k3-cot", "moonshotai/kimi-k3", "k3-256k"} {
		if !sameSlice(kimiSupportedEfforts(model), kimiK3Efforts) {
			t.Fatalf("%s is not K3", model)
		}
	}
}

func TestKimiVocabularyK2EraSlugs(t *testing.T) {
	for _, model := range []string{"kimi-k2.6", "moonshotai/kimi-k2-0905", "kimi-latest", "mk3000", ""} {
		if !sameSlice(kimiSupportedEfforts(model), kimiK2Efforts) {
			t.Fatalf("%q is not K2 era", model)
		}
	}
}

func TestGlm52VocabularyTwoLevelSet(t *testing.T) {
	cases := map[string]string{"ultra": "max", "xhigh": "max", "low": "high", "medium": "high"}
	for level, want := range cases {
		if got := clampEffort(level, glm52Efforts, glm52Overrides); got != want {
			t.Fatalf("%s -> %s, want %s", level, got, want)
		}
	}
}

func TestCodexVocabularyMinimalAndUltra(t *testing.T) {
	if got := clampEffort("minimal", codexGPT56Efforts, nil); got != "low" {
		t.Fatalf("%q", got)
	}
	if got := clampEffort("ultra", codexGPT56Efforts, nil); got != "max" {
		t.Fatalf("%q", got)
	}
}

func TestCodexVocabularyPerModelMaxSupport(t *testing.T) {
	for model, want := range map[string][]string{"gpt-5.6": codexGPT56Efforts, "gpt-5.6-codex": codexGPT56Efforts, "gpt-5.5": codexLegacyEfforts, "o5-pro": codexLegacyEfforts} {
		if !sameSlice(codexSupportedEfforts(model), want) {
			t.Fatalf("%s supported set %v", model, codexSupportedEfforts(model))
		}
	}
	for _, c := range []struct {
		effort, want string
		set          []string
	}{{"max", "max", codexGPT56Efforts}, {"max", "xhigh", codexLegacyEfforts}, {"ultra", "xhigh", codexLegacyEfforts}, {"minimal", "low", codexLegacyEfforts}} {
		if got := clampEffort(c.effort, c.set, nil); got != c.want {
			t.Fatalf("%s -> %s, want %s", c.effort, got, c.want)
		}
	}
}

func TestCodexVocabularyDaybreakAliasUsesGpt56MaxSupport(t *testing.T) {
	for _, model := range []string{"gpt-daybreak-blue-latest", "gpt-daybreak-blue-latest-900k"} {
		supported := codexSupportedEfforts(model)
		if clampEffort("max", supported, nil) != "max" || clampEffort("ultra", supported, nil) != "max" {
			t.Fatalf("%s loses max: %v", model, supported)
		}
	}
}

func TestRequestedEffortExtractsEffort(t *testing.T) {
	if got := requestedEffortOf(map[string]any{"enabled": true, "effort": "High"}); got != "high" {
		t.Fatalf("%q", got)
	}
}

func TestRequestedEffortNoneWhenAbsentOrDisabled(t *testing.T) {
	for _, v := range []any{nil, map[string]any{}, map[string]any{"enabled": false, "effort": "high"}, "not-a-dict", map[string]any{"effort": ""}} {
		if got := requestedEffortOf(v); got != "" {
			t.Fatalf("%v -> %q", v, got)
		}
	}
}

func TestClampReasoningConfigDictKeepsTheSameDictWhenUnchanged(t *testing.T) {
	rc := map[string]any{"enabled": true, "effort": "high"}
	if got := clampReasoningConfigDict(rc, openAICompatWireEfforts); !reflect.DeepEqual(got, rc) {
		t.Fatalf("%v", got)
	}
	if got := clampReasoningConfigDict(map[string]any{"enabled": true, "effort": "ultra"}, openAICompatWireEfforts); got["effort"] != "max" {
		t.Fatalf("%v", got)
	}
	if got := clampReasoningConfigDict(map[string]any{"enabled": true, "effort": "xhigh"}, tokenhubEfforts); got["effort"] != "high" {
		t.Fatalf("%v", got)
	}
}
