package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/big"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Compressor integer settings with the minimum each live value is clamped to
var liveCompressionIntKeys = []struct {
	key      string
	minValue int
}{
	{"proactive_prune_tokens", 0}, {"proactive_prune_min_result_chars", 0}, {"proactive_prune_min_reclaim_tokens", 0},
	{"protect_last_n", 0}, {"min_tail_user_messages", 1},
}

// Default a fresh compressor takes for a constructor argument, read off the construction defaults
func compressorCtorDefault(name string) any {
	o := DefaultContextCompressorOptions("")
	switch name {
	case "threshold_percent":
		return o.ThresholdPercent
	case "tail_mode":
		return o.TailMode
	case "summary_target_ratio":
		return o.SummaryTargetRatio
	case "proactive_prune_tokens":
		return o.ProactivePruneTokens
	case "proactive_prune_min_result_chars":
		return o.ProactivePruneMinResultChars
	case "proactive_prune_min_reclaim_tokens":
		return o.ProactivePruneMinReclaimTokens
	case "protect_last_n":
		return o.ProtectLastN
	case "min_tail_user_messages":
		return o.MinTailUserMessages
	}
	panic("unknown compressor constructor argument " + name)
}

// Shipped compression threshold_tokens default a fresh build installs when the key is absent
func defaultThresholdTokensCap() any {
	v, _ := config.CfgGet(map[string]any(config.Defaults()), "compression", "threshold_tokens")
	return v
}

// Threshold percent a fresh build derives when compression threshold is unset, codex autoraise included
func derivedDefaultThresholdPercent(a *AIAgent, compression map[string]any) (pct float64) {
	pct = compressorCtorDefault("threshold_percent").(float64)
	defer func() { _ = recover() }()
	autoraise := pystr.Lower(pyrepr.Str(cfgValue(compression, "codex_gpt55_autoraise", true)))
	allow := autoraise == "true" || autoraise == "1" || autoraise == "yes"
	var modelThreshold *float64
	if v, ok := compressionThresholdForModel(a.model, a.provider, allow); ok {
		modelThreshold = &v
	}
	isAutoraise := isCodexGPT54OrGPT55(a.model, a.provider) || isCodexSpark(a.model, a.provider)
	resolved, _ := resolveCompressionThreshold(pct, modelThreshold, a.model, isAutoraise)
	pct = resolved
	return pct
}

// Python exception class of an error a conversion or a hosted attribute access raised
func liveConfigErrorKind(err error) string {
	if e, ok := err.(*pyint.Error); ok {
		return e.Kind
	}
	return plugins.ErrorTypeName(err)
}

// True when the error is a TypeError or ValueError, the two a suppress block swallows
func liveConfigSuppressed(err error) bool {
	kind := liveConfigErrorKind(err)
	return kind == "TypeError" || kind == "ValueError"
}

// Python int saturated into the Go int range
func liveConfigInt(n *big.Int) int {
	if n.IsInt64() {
		return int(n.Int64())
	}
	if n.Sign() > 0 {
		return math.MaxInt
	}
	return math.MinInt
}

// Python min of two floats, the first kept unless the second compares smaller
func pyMinFloat(a, b float64) float64 {
	if b < a {
		return b
	}
	return a
}

// Python max of two floats, the first kept unless the second compares larger
func pyMaxFloat(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
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

// Positive int cap or nil, an infinite float raising the OverflowError int does
func (n nativeLiveCompressor) coerceThresholdTokensCap(raw any) (any, error) {
	if raw != nil {
		if _, err := pyint.Of(raw); err != nil && liveConfigErrorKind(err) == "OverflowError" {
			return nil, err
		}
	}
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

// Adopts the compression config on the live agent and compressor, a removed key restoring its fresh build value
func (a *AIAgent) ApplyLiveCompressionConfig(ctx context.Context, cfg map[string]any) error {
	if cfg == nil {
		cfg = map[string]any{}
	}
	compression := config.AsMap(cfg["compression"])
	if compression == nil {
		compression = map[string]any{}
	}
	enabledRaw := cfgValue(compression, "enabled", true)
	if b, ok := enabledRaw.(bool); ok {
		a.compressionEnabled = b
	} else {
		text := pystr.Lower(pyrepr.Str(enabledRaw))
		a.compressionEnabled = text == "true" || text == "1" || text == "yes"
	}
	a.codexResponsesNativeCompaction = isTruthyValue(cfgValue(compression, "codex_responses_native", false), false)
	nativeRaw := cfgValue(compression, "codex_responses_compact_threshold", 200_000)
	nativeThreshold := 0
	if _, isBool := nativeRaw.(bool); !isBool {
		n, err := pyint.Of(nativeRaw)
		if err != nil && !liveConfigSuppressed(err) {
			return err
		}
		if err == nil && n.Sign() > 0 {
			nativeThreshold = liveConfigInt(n)
		}
	}
	if nativeThreshold <= 0 {
		slog.Warn(fmt.Sprintf("Invalid compression.codex_responses_compact_threshold=%s; using 200000.", pyrepr.Repr(nativeRaw)))
		nativeThreshold = 200_000
	}
	a.codexResponsesCompactThreshold = float64(nativeThreshold)
	idleRaw := cfgValue(compression, "idle_compact_after_seconds", 0)
	if !pyrepr.Truthy(idleRaw) {
		idleRaw = 0
	}
	if n, err := pyint.Of(idleRaw); err == nil {
		a.compressionIdleCompactAfterSeconds = float64(max(0, liveConfigInt(n)))
	} else if !liveConfigSuppressed(err) {
		return err
	}
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
	return applyLiveCompressorConfig(ctx, a, cc, cfg, compression)
}

// Writes every compressor setting in the order the hot reload does, the first unsuppressed error ending it
func applyLiveCompressorConfig(ctx context.Context, a *AIAgent, cc liveCompressorAttrs, cfg, compression map[string]any) error {
	defaultTail := pyrepr.Str(compressorCtorDefault("tail_mode"))
	modeRaw := cfgValue(compression, "tail_mode", defaultTail)
	if !pyrepr.Truthy(modeRaw) {
		modeRaw = defaultTail
	}
	mode := pystr.Lower(pystr.Strip(pyrepr.Str(modeRaw)))
	if mode != "legacy" && mode != "lean" {
		mode = defaultTail
	}
	if err := cc.setAttr("tail_mode", mode); err != nil {
		return err
	}
	for _, spec := range liveCompressionIntKeys {
		def := compressorCtorDefault(spec.key).(int)
		raw := cfgValue(compression, spec.key, def)
		value := def
		if raw != nil {
			n, err := pyint.Of(raw)
			if err != nil {
				if liveConfigSuppressed(err) {
					continue
				}
				return err
			}
			value = liveConfigInt(n)
		}
		if err := cc.setAttr(spec.key, max(spec.minValue, value)); err != nil && !liveConfigSuppressed(err) {
			return err
		}
	}
	ratioRaw := cfgValue(compression, "target_ratio", compressorCtorDefault("summary_target_ratio"))
	if ratio, err := pyint.Float(ratioRaw); err == nil {
		if err := cc.setAttr("summary_target_ratio", pyMaxFloat(0.10, pyMinFloat(ratio, 0.80))); err != nil && !liveConfigSuppressed(err) {
			return err
		}
	} else if !liveConfigSuppressed(err) {
		return err
	}
	thresholds := map[string]float64{}
	if raw := config.AsMap(compression["model_thresholds"]); raw != nil {
		for k, v := range raw {
			if _, isBool := v.(bool); isBool {
				continue
			}
			if _, isInt := pyint.Instance(v); !isInt {
				if _, isFloat := v.(float64); !isFloat {
					continue
				}
			}
			f, err := pyint.Float(v)
			if err != nil {
				return err
			}
			thresholds[k] = f
		}
	}
	if err := cc.setAttr("model_thresholds", thresholds); err != nil {
		return err
	}
	var pct *float64
	if raw, present := compression["threshold"]; present {
		f, err := pyint.Float(raw)
		if err == nil {
			pct = &f
		} else if !liveConfigSuppressed(err) {
			return err
		}
	}
	if pct == nil {
		derived := derivedDefaultThresholdPercent(a, compression)
		pct = &derived
	}
	if err := cc.setAttr("_config_threshold_percent", *pct); err != nil {
		return err
	}
	if err := cc.setAttr("_configured_threshold_percent", *pct); err != nil {
		return err
	}
	base := resolveModelThreshold(a.model, thresholds, *pct, a.provider)
	if err := cc.setAttr("_base_threshold_percent", base); err != nil {
		return err
	}
	effective, err := cc.effectiveThresholdPercent(base)
	if err == nil {
		err = cc.setAttr("threshold_percent", effective)
	}
	if err != nil {
		if err := cc.setAttr("threshold_percent", *pct); err != nil {
			return err
		}
	}
	if newCtx := configContextLengthForRuntime(ctx, a, cfg); newCtx != nil {
		if err := setConfigContextLength(ctx, a, newCtx); err != nil {
			return err
		}
		_ = cc.setContextLength(*newCtx)
	} else if cc.configContextLengthSet() {
		if err := setConfigContextLength(ctx, a, nil); err != nil {
			return err
		}
		if err := cc.setAttr("_resolved_context_length", nil); err != nil {
			return err
		}
	}
	capValue, err := cc.coerceThresholdTokensCap(cfgValue(compression, "threshold_tokens", defaultThresholdTokensCap()))
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
