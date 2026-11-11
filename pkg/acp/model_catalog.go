package acp

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

const acpMaxModelsPerProvider = 200

var loadPickerContext = provider.LoadPickerContext

var buildModelsPayload = provider.BuildModelsPayload

var fetchPickerLiveModels = provider.FetchPickerLiveModels

// One named endpoint's slug, label and model rows of id and description
type namedCatalog struct {
	slug, label string
	models      [][2]string
}

func fieldText(entry map[string]any, key string) string {
	return strings.TrimSpace(pyStr(pyrepr.Or(entry[key], "")))
}

func stringHeaders(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, x := range m {
		out[k] = pyStr(x)
	}
	return out
}

// Named endpoints of providers and custom_providers with their declared or live discovered models
func namedCustomProviderCatalogs(ctx context.Context) []namedCatalog {
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err != nil {
		serverLog.Debug("Could not load named custom providers")
		return nil
	}
	entries := provider.CompatibleCustomProviders(map[string]any(cfg))
	disabled := map[string]bool{}
	if raw, ok := cfg["providers"].(map[string]any); ok {
		for k, v := range raw {
			if _, isMap := v.(map[string]any); isMap && !provider.IsProviderEnabled(v) {
				disabled[strings.ToLower(strings.TrimSpace(k))] = true
			}
		}
	}
	var out []namedCatalog
	for _, entry := range entries {
		if c, ok := entryCatalog(ctx, entry, disabled); ok {
			out = append(out, c)
		}
	}
	return out
}

func entryCatalog(ctx context.Context, entry map[string]any, disabled map[string]bool) (namedCatalog, bool) {
	providerKey, name, baseURL := fieldText(entry, "provider_key"), fieldText(entry, "name"), fieldText(entry, "base_url")
	if disabled[strings.ToLower(providerKey)] || name == "" || baseURL == "" {
		return namedCatalog{}, false
	}
	slug := provider.CustomProviderSlug(name, providerKey)
	apiKey := fieldText(entry, "api_key")
	if apiKey == "" {
		keyEnv := strings.TrimSpace(pyStr(pyrepr.Or(pyrepr.Or(entry["key_env"], entry["api_key_env"]), "")))
		if keyEnv != "" {
			apiKey = strings.TrimSpace(os.Getenv(keyEnv))
		}
	}
	modelsCfg := entry["models"]
	var declared []string
	seen := map[string]bool{}
	for _, m := range append([]string{fieldText(entry, "model")}, provider.DeclaredModelIDs(modelsCfg)...) {
		if m != "" && !seen[m] {
			seen[m] = true
			declared = append(declared, m)
		}
	}
	headers := stringHeaders(entry["extra_headers"])
	lowerKey := strings.ToLower(providerKey)
	isOllamaKey := lowerKey == "ollama" || lowerKey == "custom:ollama"
	nativeProvider := "custom"
	if isOllamaKey {
		nativeProvider = providerKey
	}
	isNative := provider.ShouldUseOllamaNativeCatalog(ctx, nativeProvider, baseURL, headers)
	if apiKey == "" && len(declared) == 0 && !isNative {
		return namedCatalog{}, false
	}
	modelIDs := append([]string{}, declared...)
	liveNative := false
	if provider.DiscoverFlag(entry) && (apiKey != "" || isNative) {
		fetchProvider := "custom"
		if isNative && isOllamaKey {
			fetchProvider = providerKey
		}
		apiMode, _ := entry["api_mode"].(string)
		rows, ok := safeFetchPickerLiveModels(ctx, apiKey, baseURL, fetchProvider,
			provider.ModelsConfigIsAllowlist(modelsCfg, provider.EntryModelsDiscovered(entry)), headers, apiMode)
		switch {
		case ok && rows.Native:
			liveNative = true
			modelIDs = append([]string{}, rows.IDs...)
		case ok:
			for _, m := range rows.IDs {
				if !seen[m] {
					modelIDs = append(modelIDs, m)
				}
			}
		}
	}
	if len(modelIDs) == 0 && !liveNative {
		return namedCatalog{}, false
	}
	c := namedCatalog{slug: slug, label: name}
	for _, m := range modelIDs {
		c.models = append(c.models, [2]string{m, ""})
	}
	return c, true
}

// Live picker models of an endpoint, unavailable when the fetch panics
func safeFetchPickerLiveModels(ctx context.Context, apiKey, baseURL, nativeProvider string, preserve bool, headers map[string]string, apiMode string) (rows provider.CatalogRows, ok bool) {
	defer func() {
		if recover() != nil {
			rows, ok = provider.CatalogRows{}, false
		}
	}()
	return fetchPickerLiveModels(ctx, apiKey, baseURL, nativeProvider, preserve, headers, 1500*time.Millisecond, apiMode)
}

func semanticProvider(providerID string) string {
	raw := strings.ToLower(strings.TrimSpace(providerID))
	if raw == "ollama" || raw == "custom:ollama" {
		return "ollama"
	}
	if strings.HasPrefix(raw, "custom:") {
		return raw
	}
	return provider.NormalizeProvider(raw)
}

// Reports whether a named endpoint with an authoritative empty catalog owns providerID
func emptyCatalogApplies(providerID string, empty map[string]bool) bool {
	raw := strings.ToLower(strings.TrimSpace(providerID))
	normalized := provider.NormalizeProvider(raw)
	for c := range empty {
		if normalized == "custom" {
			if c == raw || "custom:"+c == raw || (raw == "custom" && c == "custom") {
				return true
			}
			continue
		}
		if c == raw || c == "custom:"+normalized || c == "custom:"+raw || provider.NormalizeProvider(c) == normalized {
			return true
		}
	}
	return false
}

// Provider prefix of an encoded choice id, the longest configured custom slug winning
func choiceProvider(ctx context.Context, modelID string) string {
	parts := strings.Split(modelID, ":")
	if parts[0] == "custom" && len(parts) > 1 {
		lowered := strings.ToLower(modelID)
		var cands []string
		for p := range provider.ConfiguredCustomProviderIDs(ctx) {
			if strings.HasPrefix(p, "custom:") {
				cands = append(cands, p)
			}
		}
		sort.SliceStable(cands, func(i, j int) bool { return len(cands[i]) > len(cands[j]) })
		for _, c := range cands {
			if strings.HasPrefix(lowered, c+":") {
				return c
			}
		}
		return "custom"
	}
	return parts[0]
}

// Provider and model joined as provider:model so clients keep the provider, the bare model without one
func EncodeModelChoice(prov, model string) string {
	rawModel := strings.TrimSpace(model)
	if rawModel == "" {
		return ""
	}
	if p := strings.ToLower(strings.TrimSpace(prov)); p != "" {
		return p + ":" + rawModel
	}
	return rawModel
}

// Deduplicated ACP model rows from the inventory and named endpoints
type modelCatalog struct {
	currentModel, currentChoiceProvider, currentBaseURL string
	models                                              []pyacp.ModelInfo
	seenIDs, seenSemantic, emptyAuthoritative           map[string]bool
	identityResolved                                    bool
}

func newModelCatalog(currentModel, currentChoiceProvider, currentBaseURL string) *modelCatalog {
	if currentChoiceProvider == "ollama" {
		currentChoiceProvider = "custom:ollama"
	}
	return &modelCatalog{currentModel: currentModel, currentChoiceProvider: currentChoiceProvider, currentBaseURL: currentBaseURL,
		seenIDs: map[string]bool{}, seenSemantic: map[string]bool{}, emptyAuthoritative: map[string]bool{},
		identityResolved: currentChoiceProvider != "" && currentChoiceProvider != "custom"}
}

func (c *modelCatalog) add(providerID, modelID, name, description string) {
	choice := EncodeModelChoice(providerID, modelID)
	semanticID := semanticProvider(providerID) + ":" + modelID
	if choice == "" || c.seenIDs[choice] || c.seenSemantic[semanticID] {
		return
	}
	c.models = append(c.models, pyacp.ModelInfo{ModelID: choice, Name: name, Description: pyacp.Ptr(description)})
	c.seenIDs[choice] = true
	c.seenSemantic[semanticID] = true
}

func (c *modelCatalog) addInventoryRows(rows []map[string]any) {
	for _, row := range rows {
		rawRow := strings.ToLower(strings.TrimSpace(pyStr(pyrepr.Or(row["slug"], ""))))
		rowProvider := provider.NormalizeProvider(rawRow)
		rowBase := strings.ToLower(strings.TrimRight(strings.TrimSpace(pyStr(pyrepr.Or(row["api_url"], ""))), "/"))
		if truthy(row["native_catalog_empty"]) {
			c.emptyAuthoritative[rawRow] = true
		}
		if !c.identityResolved && (rawRow == "ollama" || rawRow == "custom:ollama") && c.currentBaseURL != "" && rowBase == c.currentBaseURL {
			c.currentChoiceProvider = "custom:ollama"
			c.identityResolved = true
		}
		rowModels, ok := anyList(row["models"])
		if rowProvider == "" || !ok {
			continue
		}
		providerName := strings.TrimSpace(pyStr(pyrepr.Or(row["name"], "")))
		if providerName == "" {
			providerName = provider.ProviderLabel(rowProvider)
		}
		encoded := rowProvider
		if rawRow == "ollama" {
			encoded = "custom:ollama"
		} else if strings.HasPrefix(rawRow, "custom:") {
			encoded = rawRow
		}
		for _, entry := range rowModels {
			if m, ok := entry.(map[string]any); ok {
				entry = pyrepr.Or(pyrepr.Or(m["id"], m["model"]), m["name"])
			}
			rendered := strings.TrimSpace(pyStr(pyrepr.Or(entry, "")))
			if rendered == "" {
				continue
			}
			isCurrent := rendered == c.currentModel && semanticProvider(encoded) == semanticProvider(c.currentChoiceProvider)
			desc := "Provider: " + providerName
			if isCurrent {
				desc += " • current"
			}
			c.add(encoded, rendered, providerName+" · "+rendered, desc)
		}
	}
}

func (c *modelCatalog) addNamedCatalogs(catalogs []namedCatalog, currentChoiceProvider string) {
	for _, nc := range catalogs {
		if len(nc.models) == 0 {
			c.emptyAuthoritative[strings.ToLower(strings.TrimSpace(nc.slug))] = true
			continue
		}
		for _, m := range nc.models {
			isCurrent := strings.ToLower(nc.slug) == currentChoiceProvider && m[0] == c.currentModel
			var parts []string
			for _, p := range []string{"Provider: " + nc.label, strings.TrimSpace(m[1]), map[bool]string{true: "current"}[isCurrent]} {
				if p != "" {
					parts = append(parts, p)
				}
			}
			c.add(nc.slug, m[0], m[0], strings.Join(parts, " • "))
		}
	}
}

func anyList(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out, true
	case []map[string]any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

func providerRows(v any) []map[string]any {
	items, _ := anyList(v)
	var out []map[string]any
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Picker state from the inventory and named endpoints, nil when nothing is listable, an error when the inventory fails
func BuildModelState(ctx context.Context, model, prov, baseURL string) (*pyacp.SessionModelState, error) {
	normalized := provider.NormalizeProvider(prov)
	pc, err := loadPickerContext(ctx)
	if err != nil {
		return nil, err
	}
	pc.CurrentProvider, pc.CurrentModel, pc.CurrentBaseURL = normalized, model, baseURL
	maxModels := acpMaxModelsPerProvider
	payload, err := buildModelsPayload(ctx, pc, provider.ModelsPayloadOptions{ExplicitOnly: true, CanonicalOrder: true, MaxModels: &maxModels})
	if err != nil {
		return nil, err
	}
	named := namedCustomProviderCatalogs(ctx)
	namedSlugs := map[string]bool{}
	for _, nc := range named {
		namedSlugs[strings.ToLower(strings.TrimSpace(nc.slug))] = true
	}
	currentChoice := strings.ToLower(strings.TrimSpace(prov))
	currentBase := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	allRows := providerRows(payload["providers"])
	canonicalCurrent := false
	for _, r := range allRows {
		if strings.ToLower(strings.TrimSpace(pyStr(pyrepr.Or(r["slug"], "")))) == currentChoice && !truthy(r["is_user_defined"]) {
			canonicalCurrent = true
			break
		}
	}
	var inventory []map[string]any
	for _, row := range allRows {
		slug := strings.ToLower(strings.TrimSpace(pyStr(pyrepr.Or(row["slug"], ""))))
		if !truthy(row["is_user_defined"]) || !(namedSlugs[slug] || namedSlugs["custom:"+slug]) {
			inventory = append(inventory, row)
			continue
		}
		rowBase := strings.ToLower(strings.TrimRight(strings.TrimSpace(pyStr(pyrepr.Or(row["api_url"], ""))), "/"))
		if strings.TrimPrefix(slug, "custom:") == currentChoice && (currentBase == rowBase || !canonicalCurrent) {
			currentChoice = "custom:" + currentChoice
		}
	}
	cat := newModelCatalog(model, currentChoice, currentBase)
	cat.addInventoryRows(inventory)
	cat.addNamedCatalogs(named, currentChoice)
	available := cat.models
	if len(cat.emptyAuthoritative) > 0 {
		var kept []pyacp.ModelInfo
		for _, m := range available {
			if !emptyCatalogApplies(choiceProvider(ctx, m.ModelID), cat.emptyAuthoritative) {
				kept = append(kept, m)
			}
		}
		available = kept
	}
	currentIsEmpty := emptyCatalogApplies(cat.currentChoiceProvider, cat.emptyAuthoritative)
	if currentIsEmpty {
		var kept []pyacp.ModelInfo
		for _, m := range available {
			if m.Description == nil || !strings.Contains(*m.Description, " • current") {
				kept = append(kept, m)
			}
		}
		available = kept
	}
	currentID := ""
	if !currentIsEmpty {
		currentID = EncodeModelChoice(cat.currentChoiceProvider, model)
	}
	if currentID != "" {
		found := false
		for _, m := range available {
			if m.ModelID == currentID {
				found = true
				break
			}
		}
		if !found {
			name := provider.ProviderLabel(normalized)
			available = append([]pyacp.ModelInfo{{ModelID: currentID, Name: name + " · " + model, Description: pyacp.Ptr("Provider: " + name + " • current")}}, available...)
		}
	}
	if len(available) == 0 && currentIsEmpty {
		return &pyacp.SessionModelState{AvailableModels: []pyacp.ModelInfo{}, CurrentModelID: ""}, nil
	}
	if len(available) > 0 {
		cur := currentID
		if cur == "" && !currentIsEmpty {
			cur = available[0].ModelID
		}
		return &pyacp.SessionModelState{AvailableModels: available, CurrentModelID: cur}, nil
	}
	return nil, nil
}
