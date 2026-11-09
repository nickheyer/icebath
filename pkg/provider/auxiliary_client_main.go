package provider

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Full config load seam that api key and base URL reads and the MoA lookup go through
var auxLoadConfigFn = func(ctx context.Context) (config.Tree, error) {
	return config.LoadContext(ctx, home.Dir(ctx), nil)
}

// Main model field from the live runtime binding first, then config.yaml, empty when neither sets it
func auxReadMainField(ctx context.Context, field string, readonly, lower bool) string {
	if override, ok := auxRuntimeMainValue(ctx, field).(string); ok && pystr.Strip(override) != "" {
		value := pystr.Strip(override)
		if lower {
			return pystr.Lower(value)
		}
		return value
	}
	load := auxLoadConfigFn
	if readonly {
		load = auxLoadConfigReadonlyFn
	}
	var cfg config.Tree
	if err := auxRecoverRaise(func() { cfg, _ = load(ctx) }); err != nil || cfg == nil {
		return ""
	}
	modelCfg, present := cfg["model"]
	if !present {
		modelCfg = map[string]any{}
	}
	if s, ok := modelCfg.(string); ok && field == "model" && pystr.Strip(s) != "" {
		return pystr.Strip(s)
	}
	if m, ok := modelCfg.(map[string]any); ok {
		key := field
		if field == "model" {
			key = "default"
		}
		if s, ok := m[key].(string); ok && pystr.Strip(s) != "" {
			value := pystr.Strip(s)
			if lower {
				return pystr.Lower(value)
			}
			return value
		}
	}
	return ""
}

// Main field readers, package vars so tests can swap them as hermes tests patch the partials
var (
	auxReadMainModel    = func(ctx context.Context) string { return auxReadMainField(ctx, "model", true, false) }
	auxReadMainProvider = func(ctx context.Context) string { return auxReadMainField(ctx, "provider", true, true) }
	auxReadMainAPIKey   = func(ctx context.Context) string { return auxReadMainField(ctx, "api_key", false, false) }
	auxReadMainBaseURL  = func(ctx context.Context) string { return auxReadMainField(ctx, "base_url", false, false) }
)

// Aggregator provider and model of a MoA preset, empty when the preset cannot be resolved
func auxResolveMoaAggregator(ctx context.Context, presetName string) (string, string) {
	var cfg config.Tree
	var provider, model string
	ok := false
	err := auxRecoverRaise(func() {
		var lerr error
		if cfg, lerr = auxLoadConfigFn(ctx); lerr != nil {
			panic(lerr)
		}
		provider, model, ok = resolveMoaAggregator(map[string]any(cfg), presetName)
	})
	if err != nil || !ok {
		slog.Debug(fmt.Sprintf("MoA aggregator resolution failed for preset %s", pyrepr.Repr(presetName)), "error", err)
		return "", ""
	}
	if provider != "" && model != "" && pystr.Lower(provider) != "moa" {
		return provider, model
	}
	return "", ""
}

// Main model with a MoA preset unwrapped to its aggregator model, empty when that cannot be resolved
func auxReadMainModelForAux(ctx context.Context) string {
	model := auxReadMainModel(ctx)
	if pystr.Lower(pystr.Strip(auxReadMainProvider(ctx))) == "moa" {
		_, aggModel := auxResolveMoaAggregator(ctx, model)
		return aggModel
	}
	return model
}

type auxOrigin struct {
	scheme, host string
	port         int
}

// Origin triple of a base URL, the urlparse ValueError propagating
func auxBaseURLOrigin(raw string) (auxOrigin, error) {
	s, h, p, err := pyurl.BaseURLOrigin(raw)
	return auxOrigin{s, h, p}, err
}

// Main api key only when the aux base URL shares the main base URL's exact origin, from one runtime source
func auxReadMainAPIKeyIfSameOrigin(ctx context.Context, auxBaseURL string) (any, error) {
	auxOrig, err := auxBaseURLOrigin(auxBaseURL)
	if err != nil || auxOrig.host == "" {
		return "", err
	}
	live := auxNormalizeMainRuntime(ctx, nil)
	_, hasBase := live["base_url"]
	key, hasKey := live["api_key"]
	if hasBase || hasKey {
		liveBase, _ := live["base_url"].(string)
		liveOrig, err := auxBaseURLOrigin(liveBase)
		if err != nil || auxOrig != liveOrig {
			return "", err
		}
		if !hasKey {
			return "", nil
		}
		return key, nil
	}
	mainOrig, err := auxBaseURLOrigin(auxReadMainBaseURL(ctx))
	if err != nil || auxOrig != mainOrig {
		return "", err
	}
	return auxReadMainAPIKey(ctx), nil
}

// Same origin main key reader the keyless local lane tests swap
var auxReadMainAPIKeyIfSameOriginFn = auxReadMainAPIKeyIfSameOrigin
