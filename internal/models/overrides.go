package models

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	modelsconfig "github.com/nickheyer/icebath/internal/models/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// What a model nothing knows is taken to be
func unknownBase() map[string]any {
	return map[string]any{"limit": map[string]any{"context": float64(200_000)}, "tool_call": true}
}

// Section of the overrides for the route's provider, under its name in the profile, its kind, its models.dev id, or a kind of that id
func (m *Models) section(cfg *config.Config, route provider.Route) map[string]any {
	overrides, _ := cfg.Get(modelsconfig.Overrides).(map[string]any)
	if len(overrides) == 0 {
		return nil
	}
	id := m.devID(cfg, route)
	candidates := []string{route.Provider, route.Spec, id}
	for _, spec := range m.providers.Specs() {
		if id != "" && spec.ModelsDevID == id {
			candidates = append(candidates, spec.Name)
		}
	}
	for _, name := range candidates {
		if section, ok := overrides[strings.ToLower(name)].(map[string]any); ok && name != "" {
			return section
		}
	}
	return nil
}

// Override of the route's model by its id, exactly then ignoring case
func (m *Models) explicit(cfg *config.Config, route provider.Route) map[string]any {
	section := m.section(cfg, route)
	if section == nil || strings.TrimSpace(route.Model) == "" {
		return nil
	}
	if entry, ok := section[route.Model].(map[string]any); ok {
		return entry
	}
	for _, id := range slices.Sorted(maps.Keys(section)) {
		if id != modelsconfig.EveryModel && strings.EqualFold(id, route.Model) {
			if entry, ok := section[id].(map[string]any); ok {
				return entry
			}
		}
	}
	return nil
}

// Override filling what nothing knows of a model: the provider's _default, else the one for every provider
func (m *Models) fallback(cfg *config.Config, route provider.Route) map[string]any {
	if entry, ok := m.section(cfg, route)[modelsconfig.EveryModel].(map[string]any); ok {
		return entry
	}
	overrides, _ := cfg.Get(modelsconfig.Overrides).(map[string]any)
	entry, _ := overrides[modelsconfig.EveryModel].(map[string]any)
	return entry
}

// Positive whole number an override sets, warned of once when it is something else
func (m *Models) overrideInt(cfg *config.Config, override map[string]any, key string) (int, bool) {
	raw, ok := override[key]
	if !ok || raw == nil {
		return 0, false
	}
	n, ok := whole(raw)
	if ok && n > 0 {
		return n, true
	}
	s := m.of(cfg)
	shown := fmt.Sprintf("%#v", raw)
	s.dev.mu.Lock()
	first := !s.dev.warned[key+"\x00"+shown]
	s.dev.warned[key+"\x00"+shown] = true
	s.dev.mu.Unlock()
	if first {
		slog.Warn(fmt.Sprintf("model_overrides: ignoring invalid %s value %s (expected a positive integer)", key, shown))
	}
	return 0, false
}

// Whole number of a setting's value: a number, or text holding one
func whole(raw any) (int, bool) {
	switch v := raw.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil
	}
	return 0, false
}

// Row of a model the kind declares for models the registry has not indexed yet
func (m *Models) builtin(route provider.Route) map[string]any {
	return m.Spec(route).ModelMetadata[strings.ToLower(strings.TrimSpace(route.Model))]
}

// Registry entry of the route's model, or the kind's own row of it, patched by what the kind declares of the model and then by the
// profile's overrides: the model's own, else _default for a model nothing knows. Nil when nothing knows the model at all
func (m *Models) patched(cfg *config.Config, route provider.Route, entry map[string]any) map[string]any {
	spec := m.Spec(route)
	base := entry
	if base == nil {
		base = m.builtin(route)
	}
	if declared := spec.ModelCapabilities[route.Model]; len(declared) > 0 {
		if base == nil {
			base = unknownBase()
		}
		base = m.merge(cfg, base, declared)
	}
	override := m.explicit(cfg, route)
	if override == nil && base == nil {
		override = m.fallback(cfg, route)
	}
	if base == nil && spec.VisionMarked && strings.Contains(strings.ToLower(route.Model), "-vision") {
		base = unknownBase()
		base["modalities"] = map[string]any{"input": []any{"text", "image"}, "output": []any{"text"}}
	}
	if override == nil {
		return base
	}
	if base == nil {
		base = unknownBase()
	}
	return m.merge(cfg, base, override)
}

// Entry with an override laid over it: its context into the limits, its abilities as the registry names them, and vision added to or taken
// from the input modalities
func (m *Models) merge(cfg *config.Config, entry, override map[string]any) map[string]any {
	merged := maps.Clone(entry)
	if n, ok := m.overrideInt(cfg, override, modelsconfig.OverrideContext); ok {
		limits := map[string]any{}
		if old, ok := entry["limit"].(map[string]any); ok {
			maps.Copy(limits, old)
		}
		limits["context"] = float64(n)
		merged["limit"] = limits
	}
	for from, to := range map[string]string{modelsconfig.OverrideTools: "tool_call", modelsconfig.OverrideReasoning: "reasoning"} {
		if v, ok := override[from]; ok {
			merged[to] = config.Truthy(v)
		}
	}
	if v, ok := override[modelsconfig.OverrideVision]; ok {
		vision := config.Truthy(v)
		merged["attachment"] = vision
		modalities := map[string]any{}
		if old, ok := entry["modalities"].(map[string]any); ok {
			maps.Copy(modalities, old)
		}
		input, _ := modalities["input"].([]any)
		input = slices.Clone(input)
		image := slices.Index(input, any("image"))
		switch {
		case vision && image < 0:
			input = append(input, "image")
		case !vision && image >= 0:
			input = slices.Delete(input, image, image+1)
		}
		if input == nil {
			input = []any{}
		}
		modalities["input"] = input
		merged["modalities"] = modalities
	}
	if v, ok := override[modelsconfig.OverrideFamily]; ok {
		family := ""
		if config.Truthy(v) {
			family = text(v)
		}
		merged["family"] = family
	}
	return merged
}

// Text of a decoded value, empty when it is not text
func text(v any) string {
	s, _ := v.(string)
	return s
}

// Texts of a decoded list, nil when it is not one
func texts(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, fmt.Sprint(item))
	}
	return out
}
