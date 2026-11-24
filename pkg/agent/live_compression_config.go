package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/plugins"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
)

// Python exception class of an error a conversion or a hosted attribute access raised
func liveConfigErrorKind(err error) string {
	if e, ok := err.(*errorutil.Error); ok {
		return e.Kind
	}
	return plugins.ErrorTypeName(err)
}

// True when the error is a TypeError or ValueError, the two a suppress block swallows
func liveConfigSuppressed(err error) bool {
	kind := liveConfigErrorKind(err)
	return kind == "TypeError" || kind == "ValueError"
}

// Attribute surface of a live context engine the config hot reload writes through
type liveCompressorAttrs interface {
	setAttr(name string, value any) error
	effectiveThresholdPercent(base float64) (any, error)
	setContextLength(n int) error
	configContextLengthSet() bool
	coerceThresholdTokensCap(raw any) (any, error)
}

// Built in compressor whose attributes are its fields
type nativeLiveCompressor struct{ c *ContextCompressor }

func (n nativeLiveCompressor) setAttr(name string, value any) error {
	c := n.c
	switch name {
	case "tail_mode":
		c.tailMode = value.(string)
	case "proactive_prune_tokens":
		c.proactivePruneTokens = value.(int)
	case "proactive_prune_min_result_chars":
		c.proactivePruneMinResultChars = value.(int)
	case "proactive_prune_min_reclaim_tokens":
		c.proactivePruneMinReclaimTokens = value.(int)
	case "protect_last_n":
		c.ProtectLastN = value.(int)
	case "min_tail_user_messages":
		c.minTailUserMessages = value.(int)
	case "summary_target_ratio":
		c.summaryTargetRatio = value.(float64)
	case "model_thresholds":
		c.ModelThresholds = value.(map[string]float64)
	case "_config_threshold_percent":
		pct := value.(float64)
		c.configThresholdPercent = &pct
	case "_configured_threshold_percent":
		c.configuredThresholdPercent = value.(float64)
	case "_base_threshold_percent":
		c.BaseThresholdPercent = value.(float64)
	case "threshold_percent":
		c.ThresholdPercent = value.(float64)
	case "_resolved_context_length":
		c.resolvedContextLength = nil
	case "threshold_tokens_cap":
		c.thresholdTokensCap, _ = value.(*int)
	case "_threshold_tokens":
		c.thresholdTokens = nil
	case "_tail_token_budget":
		c.tailTokenBudget = nil
	default:
		panic("unknown compressor attribute " + name)
	}
	return nil
}

// Resolves the window, which may itself floor the percent, then applies the small window floor to base
func (n nativeLiveCompressor) effectiveThresholdPercent(base float64) (out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return effectiveThresholdPercent(n.c.contextLength(), base), nil
}

func (n nativeLiveCompressor) setContextLength(value int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	n.c.SetContextLength(value)
	return nil
}

func (n nativeLiveCompressor) configContextLengthSet() bool { return n.c.configContextLength != nil }

// Positive int cap or nil
func (n nativeLiveCompressor) coerceThresholdTokensCap(raw any) (any, error) {
	return coerceMaxTokens(raw), nil
}

// Plugin engine whose attributes live on the hosted object
type hostedLiveCompressor struct{ e *hostedContextEngine }

func (h hostedLiveCompressor) setAttr(name string, value any) error {
	switch v := value.(type) {
	case map[string]float64:
		m := make(map[string]any, len(v))
		for k, f := range v {
			m[k] = f
		}
		value = m
	case *int:
		value = optionalInt(v)
	}
	return h.e.obj.Set(h.e.ctx, name, value)
}

func (h hostedLiveCompressor) effectiveThresholdPercent(base float64) (any, error) {
	contextLength, err := h.e.obj.Get(h.e.ctx, "context_length")
	if err != nil {
		return nil, err
	}
	return h.e.obj.Call(h.e.ctx, "_effective_threshold_percent", []any{contextLength, base}, nil)
}

func (h hostedLiveCompressor) setContextLength(value int) error {
	return h.e.obj.Set(h.e.ctx, "context_length", value)
}

func (h hostedLiveCompressor) configContextLengthSet() bool {
	v, err := h.e.obj.Get(h.e.ctx, "_config_context_length")
	return err == nil && v != nil
}

func (h hostedLiveCompressor) coerceThresholdTokensCap(raw any) (any, error) {
	return h.e.obj.Call(h.e.ctx, "_coerce_threshold_tokens_cap", []any{raw}, nil)
}

// Adopts the compression settings in effect for ctx on the live agent and compressor, a removed key restoring its fresh build value
func (a *AIAgent) ApplyLiveCompressionConfig(ctx context.Context) error {
	cs, _ := compressionSettingsFor(ctx, a)
	a.compressionEnabled = cs.Enabled
	a.codexAppServerAutoCompaction, a.codexResponsesNativeCompaction = cs.CodexAppServerAuto, cs.CodexResponsesNative
	a.codexResponsesCompactThreshold = 0
	if cs.CodexResponsesCompactThreshold != nil {
		a.codexResponsesCompactThreshold = float64(*cs.CodexResponsesCompactThreshold)
	}
	a.compressionIdleCompactAfterSeconds = float64(cs.IdleCompactAfterSeconds)
	var cc liveCompressorAttrs
	switch e := a.contextCompressor.(type) {
	case *ContextCompressor:
		cc = nativeLiveCompressor{e}
	case *hostedContextEngine:
		cc = hostedLiveCompressor{e}
		defer e.pull()
	default:
		return nil
	}
	return applyLiveCompressorConfig(ctx, a, cc, cs)
}

// Writes every compressor setting in the order the hot reload does, the first unsuppressed error ending it
func applyLiveCompressorConfig(ctx context.Context, a *AIAgent, cc liveCompressorAttrs, cs CompressionSettings) error {
	if err := cc.setAttr("tail_mode", cs.TailMode); err != nil {
		return err
	}
	for _, attr := range []struct {
		name  string
		value int
	}{
		{"proactive_prune_tokens", cs.ProactivePruneTokens}, {"proactive_prune_min_result_chars", cs.ProactivePruneMinChars},
		{"proactive_prune_min_reclaim_tokens", cs.ProactivePruneMinReclaim}, {"protect_last_n", cs.ProtectLast},
		{"min_tail_user_messages", cs.MinTailUsers},
	} {
		if err := cc.setAttr(attr.name, attr.value); err != nil && !liveConfigSuppressed(err) {
			return err
		}
	}
	if err := cc.setAttr("summary_target_ratio", cs.TargetRatio); err != nil && !liveConfigSuppressed(err) {
		return err
	}
	if err := cc.setAttr("model_thresholds", cs.ModelThresholds); err != nil {
		return err
	}
	if err := cc.setAttr("_config_threshold_percent", cs.Threshold); err != nil {
		return err
	}
	if err := cc.setAttr("_configured_threshold_percent", cs.Threshold); err != nil {
		return err
	}
	base := resolveModelThreshold(a.model, cs.ModelThresholds, cs.Threshold, a.provider)
	if err := cc.setAttr("_base_threshold_percent", base); err != nil {
		return err
	}
	effective, err := cc.effectiveThresholdPercent(base)
	if err == nil {
		err = cc.setAttr("threshold_percent", effective)
	}
	if err != nil {
		if err := cc.setAttr("threshold_percent", cs.Threshold); err != nil {
			return err
		}
	}
	if newCtx := configContextLengthForRuntime(ctx, a); newCtx != nil {
		if err := setConfigContextLength(ctx, a, newCtx); err != nil {
			return err
		}
		if err := cc.setContextLength(*newCtx); err != nil {
			pkgLog().Debug(fmt.Sprintf("Live compression config could not set the context length %d: %v", *newCtx, err))
		}
	} else if cc.configContextLengthSet() {
		if err := setConfigContextLength(ctx, a, nil); err != nil {
			return err
		}
		if err := cc.setAttr("_resolved_context_length", nil); err != nil {
			return err
		}
	}
	var tokensCap any
	if cs.ThresholdTokens != nil {
		tokensCap = *cs.ThresholdTokens
	}
	capValue, err := cc.coerceThresholdTokensCap(tokensCap)
	if err != nil {
		return err
	}
	if err := cc.setAttr("threshold_tokens_cap", capValue); err != nil {
		return err
	}
	if err := cc.setAttr("_threshold_tokens", nil); err != nil {
		return err
	}
	return cc.setAttr("_tail_token_budget", nil)
}
