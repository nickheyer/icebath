package provider

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	codexTierCatalogTTL  = 3600 * time.Second
	codexTierMaxCatalogs = 128
	// DaybreakProgram is the cyber access program the Daybreak switch requests
	DaybreakProgram = "daybreak_blue"
)

// Speed tiers and cyber programs per model with one fetch time, published as one record
type codexTierCatalog struct {
	models   map[string]map[string]bool
	at       time.Time
	programs map[string]map[string]bool
}

var (
	codexTierCatalogsMu sync.Mutex
	codexTierCatalogs   = map[string]codexTierCatalog{}
)

// Advertised speed tiers of a catalog entry, legacy fast mapped to priority, nil when unknown
func advertisedTiers(entry map[string]any) map[string]bool {
	tiers, tiersIsList := entry["service_tiers"].([]any)
	if tiersIsList {
		for _, item := range tiers {
			obj, ok := item.(map[string]any)
			if !ok {
				return nil
			}
			if _, ok := obj["id"].(string); !ok {
				return nil
			}
		}
		if len(tiers) > 0 {
			out := map[string]bool{}
			for _, item := range tiers {
				out[item.(map[string]any)["id"].(string)] = true
			}
			return out
		}
	}
	if speeds, ok := entry["additional_speed_tiers"].([]any); ok {
		out := map[string]bool{}
		allStrings := true
		for _, item := range speeds {
			s, isString := item.(string)
			if !isString {
				allStrings = false
				break
			}
			if s == "fast" {
				s = "priority"
			}
			out[s] = true
		}
		if allStrings {
			return out
		}
	}
	if tiersIsList && len(tiers) == 0 {
		return map[string]bool{}
	}
	return nil
}

// Cyber access programs a catalog entry lists, nil when the field is absent or malformed
func advertisedCyberPrograms(entry map[string]any) map[string]bool {
	programs, ok := entry["available_access_programs"].(map[string]any)
	if !ok {
		return nil
	}
	cyber, ok := programs["cyber"].([]any)
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, item := range cyber {
		s, isString := item.(string)
		if !isString {
			return nil
		}
		out[s] = true
	}
	return out
}

// Retains speed and access program metadata per slug, evicting the oldest catalog past the cap
func rememberCodexCatalog(cacheKey string, entries []any) {
	models, programs := map[string]map[string]bool{}, map[string]map[string]bool{}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		rawSlug, ok := entry["slug"].(string)
		if !ok {
			continue
		}
		slug := strings.TrimSpace(rawSlug)
		if tiers := advertisedTiers(entry); tiers != nil {
			models[slug] = tiers
		}
		if cyber := advertisedCyberPrograms(entry); cyber != nil {
			programs[slug] = cyber
		}
	}
	now := wallNow()
	codexTierCatalogsMu.Lock()
	defer codexTierCatalogsMu.Unlock()
	if _, ok := codexTierCatalogs[cacheKey]; !ok && len(codexTierCatalogs) >= codexTierMaxCatalogs {
		oldest, first := "", true
		for key, record := range codexTierCatalogs {
			if first || record.at.Before(codexTierCatalogs[oldest].at) {
				oldest, first = key, false
			}
		}
		delete(codexTierCatalogs, oldest)
	}
	codexTierCatalogs[cacheKey] = codexTierCatalog{models: models, at: now, programs: programs}
}

// Fresh catalog record of the credential, refreshed through the metadata probe once stale
func freshCodexTierCatalog(accessToken, baseURL string) (codexTierCatalog, bool) {
	key := codexOAuthTokenFingerprint(accessToken, baseURL)
	lookup := func() (codexTierCatalog, bool) {
		codexTierCatalogsMu.Lock()
		defer codexTierCatalogsMu.Unlock()
		cached, ok := codexTierCatalogs[key]
		return cached, ok && wallNow().Sub(cached.at) < codexTierCatalogTTL
	}
	if cached, ok := lookup(); ok {
		return cached, true
	}
	fetchCodexOAuthContextLengthsWithSource(context.Background(), accessToken, baseURL)
	return lookup()
}

// Speed tiers the credential's catalog advertises for the model, nil when unknown
func tiersForModel(accessToken, baseURL, model string) map[string]bool {
	cached, ok := freshCodexTierCatalog(accessToken, baseURL)
	if !ok {
		return nil
	}
	return cached.models[StripCodexContextVariantSuffix(model)]
}

// Cyber programs the credential's catalog lists for the model, nil when unknown
func cyberProgramsForModel(accessToken, baseURL, model string) map[string]bool {
	cached, ok := freshCodexTierCatalog(accessToken, baseURL)
	if !ok {
		return nil
	}
	return cached.programs[StripCodexContextVariantSuffix(model)]
}

// ChatGPT OAuth JWT reading the account's Codex catalog, the stored Codex login when the caller has none
func accountCatalogToken(accessToken string) (string, error) {
	if accessToken != "" && len(DecodeJWTClaims(accessToken)) > 0 {
		return accessToken, nil
	}
	creds, err := resolveCodexRuntimeCredentialsReadOnly(modelsPoolEnv(context.Background()))
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			return "", nil
		}
		return "", err
	}
	token, _ := creds["api_key"].(string)
	return token, nil
}

// Whether the signed in account's Codex catalog lists the tier for the model, known false when undiscoverable
func codexAccountOffersTier(model, tier, baseURL, accessToken string) (offered bool, known bool, err error) {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = DefaultCodexBaseURL
	}
	if !isOfficialCodexBaseURL(base) {
		return false, false, nil
	}
	token, err := accountCatalogToken(accessToken)
	if err != nil || token == "" {
		return false, false, err
	}
	supported := tiersForModel(token, base, bareCodexSlug(model))
	if supported == nil {
		return false, false, nil
	}
	return supported[tier], true, nil
}

// DaybreakOffered reports whether the Codex subscription route and the account catalog list daybreak for the model
func DaybreakOffered(model, providerName, baseURL, accessToken string) (bool, error) {
	if providerName == "" || NormalizeProvider(providerName) != "openai-codex" {
		return false, nil
	}
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = DefaultCodexBaseURL
	}
	if !isOfficialCodexBaseURL(base) {
		return false, nil
	}
	token, err := accountCatalogToken(accessToken)
	if err != nil || token == "" {
		return false, err
	}
	programs := cyberProgramsForModel(token, base, bareCodexSlug(model))
	return len(programs) > 0 && programs[DaybreakProgram], nil
}

// ApplyDaybreak adds the daybreak cyber access program when enabled and the request credential and model are listed
func ApplyDaybreak(route Route, daybreak bool, overrides map[string]any) {
	if !daybreak || route.Mode != ModeCodexResponses || route.Provider != "openai-codex" || !isOfficialCodexBaseURL(route.BaseURL) {
		return
	}
	if route.APIKey == "" {
		return
	}
	programs := cyberProgramsForModel(route.APIKey, route.BaseURL, overrideModel(route, overrides))
	if len(programs) == 0 || !programs[DaybreakProgram] {
		return
	}
	extraBody := map[string]any{}
	for k, v := range anyMap(overrides["extra_body"]) {
		extraBody[k] = v
	}
	access := map[string]any{}
	for k, v := range anyMap(extraBody["access_programs"]) {
		access[k] = v
	}
	access["cyber"] = DaybreakProgram
	extraBody["access_programs"] = access
	overrides["extra_body"] = extraBody
}

// UltrafastOffered reports whether ultrafast is offered on the route, the Codex plan gated by its own catalog
func UltrafastOffered(model, providerName, baseURL, accessToken string) (bool, error) {
	if ResolveFastModeOverrides(model, providerName, baseURL, "ultrafast") == nil {
		return false, nil
	}
	host := ""
	if parsed, err := url.Parse(baseURL); err == nil {
		host = strings.ToLower(parsed.Hostname())
	}
	onCodexSub := (providerName != "" && NormalizeProvider(providerName) == "openai-codex") || host == "chatgpt.com"
	if !onCodexSub {
		return true, nil
	}
	offered, known, err := codexAccountOffersTier(model, "ultrafast", baseURL, accessToken)
	if err != nil {
		return false, err
	}
	return !known || offered, nil
}

// UltrafastOfferedFor checks ultrafast against a shell's resolved route provider, base url and key
func UltrafastOfferedFor(route Route, model string) (bool, error) {
	return UltrafastOffered(model, route.Provider, route.BaseURL, route.APIKey)
}

// Request model from the overrides when present, else the route model
func overrideModel(route Route, overrides map[string]any) string {
	if raw, ok := overrides["model"]; ok {
		model, _ := raw.(string)
		return model
	}
	return route.Model
}

// NegotiateRequestTier clamps the requested speed tier to what the request credential's catalog advertises
func NegotiateRequestTier(route Route, overrides map[string]any) {
	requested, _ := overrides["service_tier"].(string)
	if (requested != "ultrafast" && requested != "priority") || route.Mode != ModeCodexResponses ||
		route.Provider != "openai-codex" || !isOfficialCodexBaseURL(route.BaseURL) {
		return
	}
	if route.APIKey == "" {
		return
	}
	supported := tiersForModel(route.APIKey, route.BaseURL, overrideModel(route, overrides))
	if supported == nil {
		return
	}
	candidates := []string{"priority"}
	if requested == "ultrafast" {
		candidates = []string{"ultrafast", "priority"}
	}
	effective := "default"
	for _, tier := range candidates {
		if supported[tier] {
			effective = tier
			break
		}
	}
	if effective == "default" {
		delete(overrides, "service_tier")
	} else {
		overrides["service_tier"] = effective
	}
	if effective != requested {
		metadataLog.Debug("Codex service tier not advertised for selected credential", "requested", requested, "effective", effective)
	}
}
