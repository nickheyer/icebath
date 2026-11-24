package agent

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysys"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	compressionThresholdForModel           = provider.CompressionThresholdForModel
	isCodexGPT54OrGPT55                    = provider.IsCodexGPT54OrGPT55
	isCodexSpark                           = provider.IsCodexSpark
	invalidConfigStderr          io.Writer = pysys.Stderr
)

// Is_truthy_value, nil the default, strings by the 1 true yes on set, the rest by Python truth
func isTruthyValue(v any, def bool) bool {
	switch x := v.(type) {
	case nil:
		return def
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
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

// Compression threshold in effect for ctx raised to the agent model's own threshold where one applies, with the codex autoraise record
func resolvedCompressionThreshold(ctx context.Context, a *AIAgent) (threshold float64, autoraised map[string]any) {
	threshold = settings.CompressAt.Get(ctx)
	defer func() {
		if recover() != nil {
			autoraised = nil
		}
	}()
	var modelThreshold *float64
	if v, ok := compressionThresholdForModel(a.model, a.provider, settings.CompressionCodexGpt55Autoraise.Get(ctx)); ok {
		modelThreshold = &v
	}
	isAutoraise := isCodexGPT54OrGPT55(a.model, a.provider) || isCodexSpark(a.model, a.provider)
	return resolveCompressionThreshold(threshold, modelThreshold, a.model, isAutoraise)
}

// Codex app server auto mode, native Responses compaction opt in and its compact threshold in effect for ctx, nil for the automatic threshold
func compressionCodexSettings(ctx context.Context) (string, bool, *int) {
	var threshold *int
	if n, ok := settings.CompressionCodexResponsesCompactThreshold.Lookup(ctx); ok {
		if n > 0 {
			threshold = &n
		} else {
			pkgLog().Warn(fmt.Sprintf("Invalid compression.codex_responses_compact_threshold=%d; using the automatic threshold derived from local compression.", n))
		}
	}
	return settings.CompressionCodexAppServerAuto.Get(ctx), settings.CompressionCodexResponsesNative.Get(ctx), threshold
}

// Per model compression thresholds in effect for ctx, entries that are not numbers left out
func compressionModelThresholds(ctx context.Context) map[string]float64 {
	out := map[string]float64{}
	for k, v := range settings.CompressionModelThresholds.Get(ctx) {
		switch n := v.(type) {
		case int:
			out[k] = float64(n)
		case int64:
			out[k] = float64(n)
		case float64:
			out[k] = n
		}
	}
	return out
}

// Token count compression starts at in effect for ctx, nil when the share of the window decides
func compressionThresholdTokens(ctx context.Context) *int {
	if n, ok := settings.CompressionThresholdTokens.Lookup(ctx); ok && n > 0 {
		return &n
	}
	return nil
}

// Seconds of idle time after which a chat is compacted, zero for never
func compressionIdleSeconds(ctx context.Context) int {
	return int(settings.CompressionIdleCompactAfterSeconds.Get(ctx) / time.Second)
}

// Compression settings in effect for ctx with every value inside the bounds the compressor takes, the threshold raised to the model's own, and the codex autoraise record
func compressionSettingsFor(ctx context.Context, a *AIAgent) (CompressionSettings, map[string]any) {
	cs := CompressionSettings{
		AutoraiseNoticeEnabled:   settings.CompressionCodexGpt55AutoraiseNotice.Get(ctx),
		Enabled:                  settings.AutoCompress.Get(ctx),
		TargetRatio:              settings.CompressionTargetRatio.Get(ctx),
		ProtectLast:              settings.CompressionProtectLastN.Get(ctx),
		TailMode:                 settings.CompressionTailMode.Get(ctx),
		MinTailUsers:             settings.CompressionMinTailUserMessages.Get(ctx),
		MaxAttempts:              settings.CompressionMaxAttempts.Get(ctx),
		ProactivePruneTokens:     settings.CompressionProactivePruneTokens.Get(ctx),
		ProactivePruneMinChars:   settings.CompressionProactivePruneMinResultChars.Get(ctx),
		ProactivePruneMinReclaim: settings.CompressionProactivePruneMinReclaimTokens.Get(ctx),
		ProtectFirst:             settings.CompressionProtectFirstN.Get(ctx),
		AbortOnSummaryFailure:    settings.CompressionAbortOnSummaryFailure.Get(ctx),
		ModelThresholds:          compressionModelThresholds(ctx),
		ThresholdTokens:          compressionThresholdTokens(ctx),
		CheckpointRequired:       settings.CompressionCheckpointRequired.Get(ctx),
		InPlace:                  settings.CompressionInPlace.Get(ctx),
		MicroCompact:             settings.CompressionMicroCompact.Get(ctx),
		MicroCompactEveryNTurns:  max(1, settings.CompressionMicroCompactEveryNTurns.Get(ctx)),
		MicroCompactDefragTokens: max(1, settings.CompressionMicroCompactDefragThresholdTokens.Get(ctx)),
		IdleCompactAfterSeconds:  compressionIdleSeconds(ctx),
		PrepareAhead:             settings.CompressionPrepareAhead.Get(ctx),
	}
	cs.CodexAppServerAuto, cs.CodexResponsesNative, cs.CodexResponsesCompactThreshold = compressionCodexSettings(ctx)
	threshold, autoraised := resolvedCompressionThreshold(ctx, a)
	cs.Threshold = threshold
	return cs, autoraised
}

// Codex app server compaction mode in lower case, native and false when unknown
func CodexAppServerAutoMode(raw string) (string, bool) {
	mode := strings.ToLower(raw)
	switch mode {
	case "native", "icebath", "off":
		return mode, true
	}
	return "native", false
}

// Compression settings the agent builds its compressor from, stashing the codex autoraise record and refusing a checkpoint requirement the Codex app server cannot keep
func parseCompressionConfig(ctx context.Context, a *AIAgent) (CompressionSettings, error) {
	cs, autoraised := compressionSettingsFor(ctx, a)
	a.compressionThresholdAutoraised = autoraised
	if err := refuseCheckpointRequiredOnCodexAppServer(cs.CheckpointRequired, a.apiMode); err != nil {
		return cs, err
	}
	return cs, nil
}

// Logs an invalid integer config value and prints it to stderr when the warning policy shows diagnostics
func warnInvalidConfigInt(ctx context.Context, what string, value any, requirement, fallback, printFallback string, a *AIAgent) {
	pkgLog().Warn(fmt.Sprintf("Invalid %s: %s — %s. Falling back to %s.", what, errchain.Show(value), requirement, fallback))
	enabled := true
	func() {
		defer func() { _ = recover() }()
		if a != nil {
			enabled = a.warningPresentationEnabled()
			return
		}
		enabled = warningNotificationsEnabled(ctx, "cli")
	}()
	if !enabled {
		return
	}
	if printFallback == "" {
		printFallback = fallback
	}
	fmt.Fprintf(scopedInvalidConfigStderr(ctx), "\n⚠ Invalid %s: %s\n  %s.\n  Falling back to %s.\n\n", what, errchain.Show(value), strings.ToUpper(requirement[:1])+requirement[1:], printFallback)
}

// Stderr of the invalid config warning as work under ctx sees it, the process stderr routed by ctx or the writer a redirect put in its place
func scopedInvalidConfigStderr(ctx context.Context) io.Writer {
	if invalidConfigStderr != pysys.Stderr {
		return invalidConfigStderr
	}
	return tools.ScopedStderr(ctx)
}
