package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Compressor built straight from constructor options over the shipped defaults, quiet
func liveTestCompressor(model string, edit func(*ContextCompressorOptions)) *ContextCompressor {
	o := DefaultContextCompressorOptions(model)
	o.QuietMode = true
	if edit != nil {
		edit(&o)
	}
	return NewContextCompressor(context.Background(), o)
}

// Bare agent carrying only the compression attributes the hot reload reads and writes
func liveTestAgent(model, providerName, baseURL string, cc ContextEngine) *AIAgent {
	a := &AIAgent{model: model, provider: providerName, baseURL: baseURL, compressionEnabled: true,
		codexResponsesCompactThreshold: 200_000}
	if cc != nil {
		a.contextCompressor = cc
	}
	return a
}

// Session agent of the 95151 regression, a codex route with a 272K pinned window
func sessionWithCompressor(edit func(*ContextCompressorOptions)) (*AIAgent, *ContextCompressor) {
	cc := liveTestCompressor("gpt-5.6-sol", func(o *ContextCompressorOptions) {
		o.ThresholdPercent, o.ConfigContextLength = 0.85, intPtr(272_000)
		if edit != nil {
			edit(o)
		}
	})
	return liveTestAgent("gpt-5.6-sol", "openai-codex", "", cc), cc
}

// Session agent on a model with no per model threshold, 600K pinned so no small window floor
func neutralSession(edit func(*ContextCompressorOptions)) (*AIAgent, *ContextCompressor) {
	cc := liveTestCompressor("unset-test-model", func(o *ContextCompressorOptions) {
		o.ConfigContextLength = intPtr(600_000)
		if edit != nil {
			edit(o)
		}
	})
	return liveTestAgent("unset-test-model", "", "", cc), cc
}

// First turn sync of a session, which always applies the config
func syncLiveCompression(t *testing.T, a *AIAgent, cfg map[string]any) {
	t.Helper()
	if err := a.ApplyLiveCompressionConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

// Shipped compression threshold_tokens default
func defaultCap() any {
	v, _ := config.CfgGet(map[string]any(config.Defaults()), "compression", "threshold_tokens")
	return v
}

// Trigger under the shipped default cap
func withDefaultCap(trigger int) int {
	if c, ok := defaultCap().(int); ok && c != 0 {
		return min(trigger, c)
	}
	return trigger
}

// Compares the coerced cap with the shipped default
func capIsDefault(cc *ContextCompressor) bool {
	def := coerceMaxTokens(defaultCap())
	if def == nil || cc.thresholdTokensCap == nil {
		return def == nil && cc.thresholdTokensCap == nil
	}
	return *def == *cc.thresholdTokensCap
}

// test_compression_config_hot_reload.py test_live_threshold_tokens_applies_on_next_turn_without_rebuild
func TestLiveThresholdTokensAppliesOnNextTurnWithoutRebuild(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, cc := sessionWithCompressor(nil)
	if stale := cc.ThresholdTokens(); stale <= 100_000 {
		t.Fatalf("stale threshold %d", stale)
	}
	syncLiveCompression(t, a, map[string]any{
		"model":       map[string]any{"default": "gpt-5.6-sol", "provider": "openai-codex", "context_length": 272_000},
		"compression": map[string]any{"threshold_tokens": 100_000, "proactive_prune_tokens": 48_000, "idle_compact_after_seconds": 1800, "tail_mode": "lean"},
	})
	if a.contextCompressor != ContextEngine(cc) {
		t.Fatal("compressor replaced")
	}
	if got := cc.ThresholdTokens(); got != 100_000 {
		t.Fatalf("threshold %d", got)
	}
	if cc.proactivePruneTokens != 48_000 || cc.tailMode != "lean" || a.compressionIdleCompactAfterSeconds != 1800 {
		t.Fatalf("prune %d tail %q idle %v", cc.proactivePruneTokens, cc.tailMode, a.compressionIdleCompactAfterSeconds)
	}
}

// test_compression_config_hot_reload.py test_live_codex_native_compaction_applies_on_next_turn
func TestLiveCodexNativeCompactionAppliesOnNextTurn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, _ := sessionWithCompressor(nil)
	syncLiveCompression(t, a, map[string]any{"compression": map[string]any{"codex_responses_native": true}})
	if !a.codexResponsesNativeCompaction {
		t.Fatal("native compaction not adopted")
	}
}

// test_compression_config_hot_reload.py test_live_codex_native_threshold_applies_on_next_turn
func TestLiveCodexNativeThresholdAppliesOnNextTurn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, _ := sessionWithCompressor(nil)
	syncLiveCompression(t, a, map[string]any{"compression": map[string]any{"codex_responses_compact_threshold": 120_000}})
	if a.codexResponsesCompactThreshold != 120_000 {
		t.Fatalf("threshold %v", a.codexResponsesCompactThreshold)
	}
}

// test_compression_config_hot_reload.py test_clearing_threshold_tokens_restores_default_cap
func TestClearingThresholdTokensRestoresDefaultCap(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, cc := sessionWithCompressor(func(o *ContextCompressorOptions) { o.ThresholdTokensCap = 100_000 })
	if got := cc.ThresholdTokens(); got != 100_000 {
		t.Fatalf("capped threshold %d", got)
	}
	syncLiveCompression(t, a, map[string]any{"model": map[string]any{"context_length": 272_000}, "compression": map[string]any{}})
	if !capIsDefault(cc) {
		t.Fatalf("cap %v, default %v", cc.thresholdTokensCap, defaultCap())
	}
	if got := cc.ThresholdTokens(); got <= 100_000 {
		t.Fatalf("threshold %d", got)
	}
}

// test_compression_config_hot_reload.py test_absent_threshold_tokens_keeps_default_cap_on_1m_window
func TestAbsentThresholdTokensKeepsDefaultCapOn1mWindow(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cc := liveTestCompressor("unset-test-model", func(o *ContextCompressorOptions) {
		o.ThresholdPercent, o.ConfigContextLength, o.ThresholdTokensCap = 0.85, intPtr(1_000_000), defaultCap()
	})
	a := liveTestAgent("unset-test-model", "", "", cc)
	if got := cc.ThresholdTokens(); got != withDefaultCap(850_000) {
		t.Fatalf("initial threshold %d", got)
	}
	syncLiveCompression(t, a, map[string]any{"model": map[string]any{"context_length": 1_000_000}, "compression": map[string]any{}})
	if !capIsDefault(cc) {
		t.Fatalf("cap %v", cc.thresholdTokensCap)
	}
	if got := cc.ThresholdTokens(); got != withDefaultCap(500_000) {
		t.Fatalf("threshold %d", got)
	}
}

// test_compression_config_hot_reload.py test_removing_compressor_keys_restores_fresh_build_values
func TestRemovingCompressorKeysRestoresFreshBuildValues(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, cc := neutralSession(func(o *ContextCompressorOptions) {
		o.TailMode, o.SummaryTargetRatio, o.ProtectLastN = "legacy", 0.60, 5
		o.ProactivePruneTokens, o.ProactivePruneMinResultChars, o.ProactivePruneMinReclaimTokens = 48_000, 30_000, 1
		o.MinTailUserMessages, o.ModelThresholds = 4, map[string]float64{"unset-test-model": 0.95}
	})
	if cc.ThresholdPercent != 0.95 {
		t.Fatalf("initial percent %v", cc.ThresholdPercent)
	}
	syncLiveCompression(t, a, map[string]any{"compression": map[string]any{}})
	_, fresh := neutralSession(nil)
	attrs := map[string][2]any{
		"tail_mode":                          {cc.tailMode, fresh.tailMode},
		"summary_target_ratio":               {cc.summaryTargetRatio, fresh.summaryTargetRatio},
		"protect_last_n":                     {cc.ProtectLastN, fresh.ProtectLastN},
		"proactive_prune_tokens":             {cc.proactivePruneTokens, fresh.proactivePruneTokens},
		"proactive_prune_min_result_chars":   {cc.proactivePruneMinResultChars, fresh.proactivePruneMinResultChars},
		"proactive_prune_min_reclaim_tokens": {cc.proactivePruneMinReclaimTokens, fresh.proactivePruneMinReclaimTokens},
		"min_tail_user_messages":             {cc.minTailUserMessages, fresh.minTailUserMessages},
		"model_thresholds":                   {cc.ModelThresholds, fresh.ModelThresholds},
		"threshold_percent":                  {cc.ThresholdPercent, fresh.ThresholdPercent},
	}
	for attr, pair := range attrs {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s live %#v fresh %#v", attr, pair[0], pair[1])
		}
	}
}

// test_compression_config_hot_reload.py test_removing_threshold_restores_derived_default
func TestRemovingThresholdRestoresDerivedDefault(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, cc := neutralSession(func(o *ContextCompressorOptions) { o.ThresholdPercent = 0.85 })
	if cc.ThresholdPercent != 0.85 {
		t.Fatalf("initial percent %v", cc.ThresholdPercent)
	}
	syncLiveCompression(t, a, map[string]any{"model": map[string]any{"context_length": 600_000}, "compression": map[string]any{}})
	if cc.configThresholdPercent == nil || *cc.configThresholdPercent != 0.50 || cc.ThresholdPercent != 0.50 {
		t.Fatalf("config percent %v percent %v", cc.configThresholdPercent, cc.ThresholdPercent)
	}
	if got := cc.ThresholdTokens(); got != withDefaultCap(300_000) {
		t.Fatalf("threshold %d", got)
	}
}

// test_compression_config_hot_reload.py test_removing_context_length_reinfers_from_model_metadata
func TestRemovingContextLengthReinfersFromModelMetadata(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, cc := neutralSession(nil)
	if got := cc.ContextLength(); got != 600_000 {
		t.Fatalf("initial window %d", got)
	}
	patchContextLength(t, 1_000_000)
	syncLiveCompression(t, a, map[string]any{"model": map[string]any{}, "compression": map[string]any{}})
	if cc.configContextLength != nil {
		t.Fatalf("pin %v", *cc.configContextLength)
	}
	if got := cc.ContextLength(); got != 1_000_000 {
		t.Fatalf("window %d", got)
	}
	if got := cc.ThresholdTokens(); got != withDefaultCap(500_000) {
		t.Fatalf("threshold %d", got)
	}
}

// test_compression_config_hot_reload.py test_removing_idle_compact_after_seconds_restores_zero
func TestRemovingIdleCompactAfterSecondsRestoresZero(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, _ := neutralSession(nil)
	a.compressionIdleCompactAfterSeconds = 1800
	syncLiveCompression(t, a, map[string]any{"compression": map[string]any{}})
	if a.compressionIdleCompactAfterSeconds != 0 {
		t.Fatalf("idle %v", a.compressionIdleCompactAfterSeconds)
	}
}

// test_compression_config_hot_reload.py test_removing_enabled_restores_true
func TestRemovingEnabledRestoresTrue(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, _ := neutralSession(nil)
	a.compressionEnabled = false
	syncLiveCompression(t, a, map[string]any{"compression": map[string]any{}})
	if !a.compressionEnabled {
		t.Fatal("compression stayed disabled")
	}
}

// test_compression_config_hot_reload.py test_removing_codex_native_compaction_restores_false
func TestRemovingCodexNativeCompactionRestoresFalse(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, _ := neutralSession(nil)
	a.codexResponsesNativeCompaction = true
	syncLiveCompression(t, a, map[string]any{"compression": map[string]any{}})
	if a.codexResponsesNativeCompaction {
		t.Fatal("native compaction stayed on")
	}
}

// test_compression_config_hot_reload.py test_removing_codex_native_threshold_restores_default
func TestRemovingCodexNativeThresholdRestoresDefault(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, _ := neutralSession(nil)
	a.codexResponsesCompactThreshold = 120_000
	syncLiveCompression(t, a, map[string]any{"compression": map[string]any{}})
	if a.codexResponsesCompactThreshold != 200_000 {
		t.Fatalf("threshold %v", a.codexResponsesCompactThreshold)
	}
}

// test_compression_config_hot_reload.py test_apply_live_compression_config_is_self_contained
func TestApplyLiveCompressionConfigIsSelfContained(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := liveTestAgent("unset-test-model", "", "", nil)
	if err := a.ApplyLiveCompressionConfig(context.Background(), map[string]any{"compression": map[string]any{"enabled": true}}); err != nil {
		t.Fatal(err)
	}
	if !a.compressionEnabled || a.codexResponsesNativeCompaction {
		t.Fatalf("enabled %v native %v", a.compressionEnabled, a.codexResponsesNativeCompaction)
	}
}

const pinMirrorPin = 249_000

// Session agent on a custom route whose pin sits on both the agent and its compressor
func pinMirrorSession(pin *int) (*AIAgent, *ContextCompressor) {
	cc := liveTestCompressor("model-a", func(o *ContextCompressorOptions) { o.ThresholdPercent, o.ConfigContextLength = 0.50, pin })
	a := liveTestAgent("model-a", "custom:acme", "http://127.0.0.1:8123/v1", cc)
	if pin != nil {
		a.configContextLength = *pin
	}
	return a, cc
}

// test_live_config_context_pin_mirror.py test_removed_context_length_clears_the_agent_pin_too
func TestRemovedContextLengthClearsTheAgentPinToo(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, cc := pinMirrorSession(intPtr(pinMirrorPin))
	patchContextLength(t, 256_000)
	syncLiveCompression(t, a, map[string]any{"model": map[string]any{}, "compression": map[string]any{}})
	if cc.configContextLength != nil {
		t.Fatalf("compressor pin %v", *cc.configContextLength)
	}
	if got := cc.ContextLength(); got != 256_000 {
		t.Fatalf("window %d", got)
	}
	if a.configContextLength != 0 {
		t.Fatalf("agent pin %d", a.configContextLength)
	}
}

// test_live_config_context_pin_mirror.py test_changed_context_length_refreshes_the_agent_pin_too
func TestChangedContextLengthRefreshesTheAgentPinToo(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, cc := pinMirrorSession(intPtr(pinMirrorPin))
	syncLiveCompression(t, a, map[string]any{"model": map[string]any{"default": "model-a", "provider": "custom:acme",
		"context_length": 400_000}, "compression": map[string]any{}})
	if cc.configContextLength == nil || *cc.configContextLength != 400_000 {
		t.Fatalf("compressor pin %v", cc.configContextLength)
	}
	if got := cc.ContextLength(); got != 400_000 {
		t.Fatalf("window %d", got)
	}
	if a.configContextLength != 400_000 {
		t.Fatalf("agent pin %d", a.configContextLength)
	}
}

// test_live_config_context_pin_mirror.py test_hot_reload_does_not_pin_a_session_on_another_route
func TestHotReloadDoesNotPinASessionOnAnotherRoute(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, cc := pinMirrorSession(nil)
	a.baseURL, a.provider = "https://openrouter.ai/api/v1", "openrouter"
	patchContextLength(t, 256_000)
	syncLiveCompression(t, a, map[string]any{
		"model": map[string]any{"default": "model-b", "provider": "custom:acme", "base_url": "http://127.0.0.1:8123/v1",
			"context_length": pinMirrorPin},
		"custom_providers": []any{map[string]any{"name": "acme", "base_url": "http://127.0.0.1:8123/v1", "models": map[string]any{}}},
		"compression":      map[string]any{},
	})
	if cc.configContextLength != nil {
		t.Fatalf("compressor pin %v", *cc.configContextLength)
	}
	if a.configContextLength != 0 {
		t.Fatalf("agent pin %d", a.configContextLength)
	}
}
