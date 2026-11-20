package config

import "strings"

// Provider ids and aliases of the retired hosted provider
var retiredProviderIDs = map[string]bool{"nous": true, "nous-portal": true, "nousresearch": true}

// Domain of the retired hosted provider endpoints
const retiredProviderDomain = "nousresearch.com"

// Cron scheduler the retired hosted provider ran
const retiredCronProvider = "chronos"

// Whether a provider value names the retired hosted provider
func isRetiredProvider(v any) bool {
	s, ok := v.(string)
	return ok && retiredProviderIDs[strings.ToLower(strings.TrimSpace(s))]
}

// Whether a base URL points at a host of the retired hosted provider
func onRetiredProviderHost(v any) bool {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return false
	}
	host, err := pyURLHostname(strings.TrimSpace(s))
	return err == nil && (host == retiredProviderDomain || strings.HasSuffix(host, "."+retiredProviderDomain))
}

// Deletes a retired provider pick and its endpoint from one slot, returning the dotted keys removed
func dropRetiredSlot(slot map[string]any, key, path string) []string {
	if !isRetiredProvider(slot[key]) {
		return nil
	}
	delete(slot, key)
	removed := []string{path + "." + key}
	if onRetiredProviderHost(slot["base_url"]) {
		delete(slot, "base_url")
		removed = append(removed, path+".base_url")
	}
	return removed
}

// Deletes chain entries routed to the retired provider, the whole key when nothing else remains
func dropRetiredChainEntries(parent map[string]any, key, path string) []string {
	switch chain := parent[key].(type) {
	case map[string]any:
		if isRetiredProvider(chain["provider"]) {
			delete(parent, key)
			return []string{path}
		}
	case []any:
		kept := make([]any, 0, len(chain))
		for _, entry := range chain {
			if m, ok := entry.(map[string]any); ok && isRetiredProvider(m["provider"]) {
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) < len(chain) {
			parent[key] = kept
			return []string{path + " entries"}
		}
	}
	return nil
}

// Deletes the retired provider from the reference models and aggregator of one mixture block
func dropRetiredMixtureSlots(block map[string]any, path string) []string {
	removed := dropRetiredChainEntries(block, "reference_models", path+".reference_models")
	if agg, ok := block["aggregator"].(map[string]any); ok && isRetiredProvider(agg["provider"]) {
		delete(block, "aggregator")
		removed = append(removed, path+".aggregator")
	}
	return removed
}

// Tool sections whose gateway flag and selection keys could route through the retired provider
var retiredGatewaySections = []string{"web", "image_gen", "video_gen", "tts", "stt", "browser"}

// Keys a tool section names its backend with
var toolSelectionKeys = []string{"provider", "backend", "cloud_provider", "search_backend", "extract_backend"}

// Deletes the gateway flag and the retired provider picks of one tool section, returning the dotted keys removed
func dropRetiredGatewaySlots(section map[string]any, path string) []string {
	var removed []string
	if _, has := section["use_gateway"]; has {
		delete(section, "use_gateway")
		removed = append(removed, path+".use_gateway")
	}
	for _, key := range toolSelectionKeys {
		if isRetiredProvider(section[key]) {
			delete(section, key)
			removed = append(removed, path+"."+key)
		}
	}
	return removed
}

// Drops the retired provider section, its tool gateway, its cron scheduler and every config slot routed to them
func migrateTo60(r *migrationRun) error {
	cfg := r.read()
	var removed []string
	if _, has := cfg["nous"]; has {
		delete(cfg, "nous")
		removed = append(removed, "nous")
	}
	if model, ok := cfg["model"].(map[string]any); ok {
		removed = append(removed, dropRetiredSlot(model, "provider", "model")...)
	}
	removed = append(removed, dropRetiredChainEntries(cfg, "fallback_providers", "fallback_providers")...)
	removed = append(removed, dropRetiredChainEntries(cfg, "fallback_model", "fallback_model")...)
	if strategies, ok := cfg["credential_pool_strategies"].(map[string]any); ok {
		for _, key := range MappingKeys(strategies) {
			if isRetiredProvider(key) {
				delete(strategies, key)
				removed = append(removed, "credential_pool_strategies."+key)
			}
		}
	}
	if aux, ok := cfg["auxiliary"].(map[string]any); ok {
		for _, task := range MappingKeys(aux) {
			if slot, ok := aux[task].(map[string]any); ok {
				removed = append(removed, dropRetiredSlot(slot, "provider", "auxiliary."+task)...)
			}
		}
	}
	if delegation, ok := cfg["delegation"].(map[string]any); ok {
		removed = append(removed, dropRetiredSlot(delegation, "provider", "delegation")...)
		removed = append(removed, dropRetiredChainEntries(delegation, "fallback_providers", "delegation.fallback_providers")...)
	}
	if moa, ok := cfg["moa"].(map[string]any); ok {
		removed = append(removed, dropRetiredMixtureSlots(moa, "moa")...)
		if presets, ok := moa["presets"].(map[string]any); ok {
			for _, name := range MappingKeys(presets) {
				if preset, ok := presets[name].(map[string]any); ok {
					removed = append(removed, dropRetiredMixtureSlots(preset, "moa.presets."+name)...)
				}
			}
		}
	}
	if catalog, ok := cfg["model_catalog"].(map[string]any); ok && onRetiredProviderHost(catalog["url"]) {
		delete(catalog, "url")
		removed = append(removed, "model_catalog.url")
	}
	if cron, ok := cfg["cron"].(map[string]any); ok {
		removed = append(removed, dropRetiredSlot(cron, "model_provider", "cron")...)
		if lowerIs(retiredCronProvider)(cron["provider"], true) {
			delete(cron, "provider")
			removed = append(removed, "cron.provider")
		}
		if _, has := cron[retiredCronProvider]; has {
			delete(cron, retiredCronProvider)
			removed = append(removed, "cron."+retiredCronProvider)
		}
	}
	for _, name := range retiredGatewaySections {
		if section, ok := cfg[name].(map[string]any); ok {
			removed = append(removed, dropRetiredGatewaySlots(section, name)...)
		}
	}
	if terminal, ok := cfg["terminal"].(map[string]any); ok {
		if _, has := terminal["modal_mode"]; has {
			delete(terminal, "modal_mode")
			removed = append(removed, "terminal.modal_mode")
		}
	}
	if _, has := cfg["tool_gateway_declined_tools"]; has {
		delete(cfg, "tool_gateway_declined_tools")
		removed = append(removed, "tool_gateway_declined_tools")
	}
	if len(removed) == 0 {
		return nil
	}
	list := strings.Join(removed, ", ")
	return r.commit(cfg, "removed "+list,
		"  ✓ Removed "+list+" — that hosted provider, its tool gateway and its cron scheduler are gone, so these settings fall back to their defaults.")
}
