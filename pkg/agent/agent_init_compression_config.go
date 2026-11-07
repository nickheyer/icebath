package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

var (
	compressionThresholdForModel           = provider.CompressionThresholdForModel
	isCodexGPT54OrGPT55                    = provider.IsCodexGPT54OrGPT55
	isCodexSpark                           = provider.IsCodexSpark
	invalidConfigStderr          io.Writer = os.Stderr
)

// Hermes is_truthy_value, nil the default, strings by the 1 true yes on set, the rest by Python truth
func isTruthyValue(v any, def bool) bool {
	switch x := v.(type) {
	case nil:
		return def
	case string:
		switch pystr.Lower(pystr.Strip(x)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyrepr.Truthy(v)
}

// Python int of the value when positive, nil otherwise, bools and floats refused outright when asked
func configPositiveInt(raw any, rejectBoolFloat bool) *int {
	if rejectBoolFloat {
		switch raw.(type) {
		case bool, float32, float64:
			return nil
		}
	}
	n, err := pyint.Of(raw)
	if err != nil || n.Sign() <= 0 || !n.IsInt64() {
		return nil
	}
	v := int(n.Int64())
	return &v
}

// Python int of a config value, its TypeError or ValueError returned
func configInt(cfg map[string]any, key string, def int) (int, error) {
	raw, ok := cfg[key]
	if !ok {
		raw = def
	}
	n, err := pyint.Of(raw)
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

// Python float of a config value, its TypeError or ValueError returned
func configFloat(cfg map[string]any, key string, def float64) (float64, error) {
	raw, ok := cfg[key]
	if !ok {
		raw = def
	}
	return pyint.Float(raw)
}

// Global threshold merged with the per model override, stashing the codex autoraise record on the agent
func compressionThreshold(a *AIAgent, cfg map[string]any) (float64, bool, error) {
	threshold, err := configFloat(cfg, "threshold", 0.50)
	if err != nil {
		return 0, false, err
	}
	autoraise := cfgFlag(cfg, "codex_gpt55_autoraise", true)
	noticeEnabled := cfgFlag(cfg, "codex_gpt55_autoraise_notice", true)
	a.compressionThresholdAutoraised = nil
	func() {
		defer func() { _ = recover() }()
		var modelThreshold *float64
		if v, ok := compressionThresholdForModel(a.model, a.provider, autoraise); ok {
			modelThreshold = &v
		}
		isAutoraise := isCodexGPT54OrGPT55(a.model, a.provider) || isCodexSpark(a.model, a.provider)
		threshold, a.compressionThresholdAutoraised = resolveCompressionThreshold(threshold, modelThreshold, a.model, isAutoraise)
	}()
	return threshold, noticeEnabled, nil
}

// Codex app server auto mode, native Responses compaction opt in and its compact threshold
func compressionCodexSettings(cfg map[string]any) (string, bool, *int) {
	raw, ok := cfg["codex_app_server_auto"]
	if !ok || !pyrepr.Truthy(raw) {
		raw = "native"
	}
	appServerAuto := strings.ToLower(pyrepr.Str(raw))
	if appServerAuto != "native" && appServerAuto != "hermes" && appServerAuto != "off" {
		slog.Warn(fmt.Sprintf("Invalid compression.codex_app_server_auto=%s; using 'native'. Valid values are: native, hermes, off.", pyrepr.Repr(appServerAuto)))
		appServerAuto = "native"
	}
	responsesNative := isTruthyValue(cfgValue(cfg, "codex_responses_native", false), false)
	var compactThreshold *int
	if rawThreshold := cfg["codex_responses_compact_threshold"]; rawThreshold != nil {
		if compactThreshold = configPositiveInt(rawThreshold, true); compactThreshold == nil {
			slog.Warn(fmt.Sprintf("Invalid compression.codex_responses_compact_threshold=%s; using the automatic threshold derived from local compression.", pyrepr.Repr(rawThreshold)))
		}
	}
	return appServerAuto, responsesNative, compactThreshold
}

// Config value or its default when the key is absent
func cfgValue(cfg map[string]any, key string, def any) any {
	if v, ok := cfg[key]; ok {
		return v
	}
	return def
}

// Parses the compression section with every default matching the shipped config, garbage numbers returned as errors
func parseCompressionConfig(a *AIAgent, agentCfg map[string]any) (CompressionSettings, error) {
	cfg := cfgDict(agentCfg, "compression")
	var cs CompressionSettings
	var err error
	if cs.Threshold, cs.AutoraiseNoticeEnabled, err = compressionThreshold(a, cfg); err != nil {
		return cs, err
	}
	if cs.TargetRatio, err = configFloat(cfg, "target_ratio", 0.20); err != nil {
		return cs, err
	}
	if cs.ProtectLast, err = configInt(cfg, "protect_last_n", 20); err != nil {
		return cs, err
	}
	maxAttempts := parseConfigInt(cfgValue(cfg, "max_attempts", 3), 3)
	if maxAttempts < 1 {
		maxAttempts = 3
	}
	defaultTokens, _ := config.CfgGet(map[string]any(config.Defaults()), "compression", "threshold_tokens")
	if tokens := cfgValue(cfg, "threshold_tokens", defaultTokens); tokens != nil {
		cs.ThresholdTokens = configPositiveInt(tokens, false)
	}
	protectFirst, err := configInt(cfg, "protect_first_n", 3)
	if err != nil {
		return cs, err
	}
	cs.ProtectFirst = max(0, protectFirst)
	cs.CheckpointRequired = isTruthyValue(cfg["checkpoint_required"], false)
	if err := refuseCheckpointRequiredOnCodexAppServer(cs.CheckpointRequired, a.apiMode); err != nil {
		return cs, err
	}
	cs.CodexAppServerAuto, cs.CodexResponsesNative, cs.CodexResponsesCompactThreshold = compressionCodexSettings(cfg)
	idle, err := configInt(cfg, "idle_compact_after_seconds", 0)
	if err != nil {
		return cs, err
	}
	cs.IdleCompactAfterSeconds = max(0, idle)
	cs.Enabled = cfgFlag(cfg, "enabled", true)
	cs.TailMode = strings.ToLower(pystr.Strip(pyrepr.Str(cfgValue(cfg, "tail_mode", "lean"))))
	cs.MinTailUsers = max(1, parseConfigInt(cfgValue(cfg, "min_tail_user_messages", 1), 1))
	cs.MaxAttempts = min(maxAttempts, 10)
	cs.ProactivePruneTokens = max(0, parseConfigInt(cfgValue(cfg, "proactive_prune_tokens", 0), 0))
	cs.ProactivePruneMinChars = parseConfigInt(cfgValue(cfg, "proactive_prune_min_result_chars", 8000), 8000)
	cs.ProactivePruneMinReclaim = max(0, parseConfigInt(cfgValue(cfg, "proactive_prune_min_reclaim_tokens", 4096), 4096))
	cs.AbortOnSummaryFailure = cfgFlag(cfg, "abort_on_summary_failure", false)
	cs.ModelThresholds = map[string]float64{}
	for k, v := range cfgDict(cfg, "model_thresholds") {
		switch n := v.(type) {
		case int:
			cs.ModelThresholds[k] = float64(n)
		case int64:
			cs.ModelThresholds[k] = float64(n)
		case float64:
			cs.ModelThresholds[k] = n
		}
	}
	cs.InPlace = isTruthyValue(cfg["in_place"], true)
	cs.MicroCompact = isTruthyValue(cfg["micro_compact"], false)
	cs.MicroCompactEveryNTurns = max(1, parseConfigInt(cfgValue(cfg, "micro_compact_every_n_turns", 1), 1))
	cs.MicroCompactDefragTokens = max(1, parseConfigInt(cfgValue(cfg, "micro_compact_defrag_threshold_tokens", 2000), 2000))
	cs.PrepareAhead = isTruthyValue(cfg["prepare_ahead"], false)
	return cs, nil
}

// Logs an invalid integer config value and prints it to stderr when the warning policy shows diagnostics
func warnInvalidConfigInt(ctx context.Context, what string, value any, requirement, fallback, printFallback string, a *AIAgent) {
	slog.Warn(fmt.Sprintf("Invalid %s: %s — %s. Falling back to %s.", what, pyrepr.Repr(value), requirement, fallback))
	enabled := true
	func() {
		defer func() { _ = recover() }()
		if a != nil {
			enabled = a.warningPresentationEnabled()
			return
		}
		enabled = warningNotificationsEnabled(ctx, "cli", nil)
	}()
	if !enabled {
		return
	}
	if printFallback == "" {
		printFallback = fallback
	}
	fmt.Fprintf(invalidConfigStderr, "\n⚠ Invalid %s: %s\n  %s.\n  Falling back to %s.\n\n", what, pyrepr.Repr(value), pystr.Upper(requirement[:1])+requirement[1:], printFallback)
}
