package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ReasoningFloorEffort is the lowest effort every reasoning wire accepts
const ReasoningFloorEffort = "low"

type auxFlooredRoute struct{ route, model string }

var (
	auxFlooredRoutesMu sync.Mutex
	auxFlooredRoutes   = map[auxFlooredRoute]bool{}
)

var auxDisabledEfforts = map[string]bool{"none": true, "off": true, "disabled": true, "false": true, "0": true}

var auxNousProviders = map[string]bool{"nous": true, "nous-portal": true, "nousresearch": true}

// True for a reasoning config map whose enabled flag is exactly false
func auxReasoningDisabled(reasoningConfig any) bool {
	m, ok := reasoningConfig.(map[string]any)
	if !ok {
		return false
	}
	enabled, isBool := m["enabled"].(bool)
	return isBool && !enabled
}

func auxDisabledEffort(v any, present bool) bool {
	if !present {
		v = ""
	}
	return auxDisabledEfforts[pystr.Lower(pystr.Strip(pyrepr.Str(v)))]
}

// FloorReasoningConfig lifts a disabled reasoning config to the floor and returns anything else unchanged
func FloorReasoningConfig(reasoningConfig any) any {
	if auxReasoningDisabled(reasoningConfig) {
		return map[string]any{"enabled": true, "effort": ReasoningFloorEffort}
	}
	return reasoningConfig
}

// WithReasoningFloor copies kwargs with every thinking off encoding lifted to the floor, nil when nothing was disabled
func WithReasoningFloor(kwargs map[string]any) map[string]any {
	changed := false
	retry := make(map[string]any, len(kwargs))
	for k, v := range kwargs {
		retry[k] = v
	}
	if v, ok := retry["reasoning_effort"]; auxDisabledEffort(v, ok) {
		retry["reasoning_effort"] = ReasoningFloorEffort
		changed = true
	}
	if auxReasoningDisabled(retry["_reasoning_config"]) {
		retry["_reasoning_config"] = FloorReasoningConfig(retry["_reasoning_config"])
		changed = true
	}
	if extraBody, ok := retry["extra_body"].(map[string]any); ok {
		reasoning := extraBody["reasoning"]
		rm, isMap := reasoning.(map[string]any)
		effortOff := false
		if isMap {
			effort, present := rm["effort"]
			effortOff = auxDisabledEffort(effort, present)
		}
		if auxReasoningDisabled(reasoning) || effortOff {
			merged := make(map[string]any, len(extraBody)+1)
			for k, v := range extraBody {
				merged[k] = v
			}
			merged["reasoning"] = map[string]any{"enabled": true, "effort": ReasoningFloorEffort}
			retry["extra_body"] = merged
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return retry
}

// RememberReasoningFloor records that the route refuses to disable reasoning for the rejected request model
func RememberReasoningFloor(provider, baseURL string, rejectedKwargs map[string]any, _ error) error {
	route, err := auxRouteKey(provider, baseURL)
	if err != nil {
		return err
	}
	model := ""
	if v := rejectedKwargs["model"]; pyrepr.Truthy(v) {
		model = pyrepr.Str(v)
	}
	auxFlooredRoutesMu.Lock()
	auxFlooredRoutes[auxFlooredRoute{route, model}] = true
	auxFlooredRoutesMu.Unlock()
	return nil
}

// Drops every remembered floored route as a fresh process starts
func auxResetReasoningFloors() {
	auxFlooredRoutesMu.Lock()
	auxFlooredRoutes = map[auxFlooredRoute]bool{}
	auxFlooredRoutesMu.Unlock()
}

func auxReasoningFloored(route, model string) bool {
	auxFlooredRoutesMu.Lock()
	defer auxFlooredRoutesMu.Unlock()
	return auxFlooredRoutes[auxFlooredRoute{route, model}]
}

// Catalog seams for the OpenRouter and Portal reasoning lookups and their background warms
var (
	auxOpenRouterReasoningCapsFn = OpenRouterModelReasoningCapabilities
	auxNousReasoningCapsFn       = NousModelReasoningCapabilities
	auxWarmOpenRouterCapsFn      = WarmOpenRouterReasoningCapsAsync
	auxWarmNousCapsFn            = WarmNousReasoningCapsAsync
)

// True when the cached route catalog flags the model reasoning mandatory, warming a cold catalog
func auxCatalogMarksMandatory(ctx context.Context, provider, baseURL, model string) (bool, error) {
	providerNorm := pystr.Lower(pystr.Strip(provider))
	parts, err := pyURLParse(baseURL, "")
	if err != nil {
		return false, err
	}
	host, _, _ := pyNetlocHostPort(parts.netloc)
	var lookup func(context.Context, string, time.Duration, bool) (ReasoningCapabilities, bool)
	var warm func(context.Context)
	switch {
	case providerNorm == "openrouter" || host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai"):
		lookup, warm = auxOpenRouterReasoningCapsFn, auxWarmOpenRouterCapsFn
	case auxNousProviders[providerNorm]:
		lookup, warm = auxNousReasoningCapsFn, auxWarmNousCapsFn
	default:
		return false, nil
	}
	caps, ok := lookup(ctx, model, 0, false)
	if !ok {
		warm(ctx)
		return false, nil
	}
	return caps.Mandatory, nil
}

// KnownReasoningFloor lifts a disabled reasoning config to the floor when the route and model refuse a disable
func KnownReasoningFloor(ctx context.Context, reasoningConfig any, provider, baseURL, model, task string) (any, error) {
	if !auxReasoningDisabled(reasoningConfig) {
		return reasoningConfig, nil
	}
	route, err := auxRouteKey(provider, baseURL)
	if err != nil {
		return nil, err
	}
	if !auxReasoningFloored(route, model) {
		mandatory, err := auxCatalogMarksMandatory(ctx, provider, baseURL, model)
		if err != nil {
			return nil, err
		}
		if !mandatory {
			return reasoningConfig, nil
		}
	}
	label, routeLabel, modelLabel := task, route, model
	if label == "" {
		label = "call"
	}
	if routeLabel == "" {
		routeLabel = "provider"
	}
	if modelLabel == "" {
		modelLabel = "model"
	}
	slog.Info(fmt.Sprintf("Auxiliary %s: %s (%s) cannot disable reasoning; sending effort=%s up front",
		label, routeLabel, modelLabel, ReasoningFloorEffort))
	return FloorReasoningConfig(reasoningConfig), nil
}
