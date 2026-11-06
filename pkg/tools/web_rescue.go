package tools

import (
	"strings"
)

// Ring vendor to the env var holding its paid key
var ringKeyVars = map[string]string{"exa": "EXA_API_KEY", "parallel": "PARALLEL_API_KEY", "firecrawl": "FIRECRAWL_API_KEY", "keenable": "KEENABLE_API_KEY"}

// web.keyless_rescue default on, implicitly off when the keyless tier is disabled
func keylessRescueEnabled(env *Env) bool {
	if v, ok := webCfg(env).Lookup("keyless_rescue"); ok && v != nil && !truthy(v) {
		return false
	}
	return keylessEnabled(env)
}

// Did the vendor's own provider route this call through the anonymous keyless ring
func ringVendorKeyless(env *Env, name string) bool {
	if name == "firecrawl" {
		return firecrawlUseKeylessRing(env, "")
	}
	key := ""
	if keyVar := ringKeyVars[name]; keyVar != "" {
		key = secretValue(env, keyVar)
	}
	return useKeyless(env, name, key)
}

// Truncates to n runes
func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Managed Firecrawl for a failed managed Perplexity call, nil leaves the original error for keyless rescue
func managedSearchFallback(ctx webCallContext, provider WebProvider, originalError, query string, limit int) *WebSearchResponse {
	env := ctx.env
	if provider == nil || provider.Name() != "perplexity" || secretValue(env, "PERPLEXITY_API_KEY") != "" || !managedWebSearch(env) || !firecrawlGatewayReady(env) {
		return nil
	}
	envLog(env).Warn("web_search managed Perplexity failed; serving this call from managed Firecrawl", "error", headRunes(originalError, 200))
	var resp *WebSearchResponse
	if firecrawl := webProvider(env, "firecrawl"); firecrawl != nil {
		r, err := firecrawl.Search(ctx.ctx, env, query, limit)
		if err != nil {
			resp = searchFail(err.Error())
		} else {
			resp = r
		}
	} else {
		resp = searchFail("firecrawl provider is not registered")
	}
	if resp == nil || !resp.Success {
		msg := ""
		if resp != nil {
			msg = resp.Error
		}
		envLog(env).Warn("managed Firecrawl fallback failed too", "error", headRunes(msg, 200))
		return nil
	}
	if originalError == "" {
		originalError = "unknown error"
	}
	data := resp.data()
	data.FallbackFrom = "managed_primary"
	data.BackendError = "Primary managed search failed this call (" + headRunes(originalError, 300) + "); result served by the managed fallback. The next call will use the primary again."
	return resp
}

// True when a failed call on the provider should get a one shot rescue
func rescueEligible(env *Env, provider WebProvider) bool {
	if provider == nil || !keylessRescueEnabled(env) {
		return false
	}
	name := provider.Name()
	return !containsString(keylessRing, name) || !ringVendorKeyless(env, name)
}

// Rescues a failed search via the ring, annotating the result with the original failure
func rescueSearch(ctx webCallContext, providerName, originalError, query string, limit int) (*WebSearchResponse, error) {
	env := ctx.env
	envLog(env).Warn("web_search backend failed; one-shot keyless rescue", "backend", providerName, "error", headRunes(originalError, 200))
	rescued, err := keylessSearchWithFailover(ctx.ctx, env, providerName, query, limit)
	if err != nil {
		return nil, err
	}
	if rescued != nil && rescued.Success {
		shown := originalError
		if shown == "" {
			shown = "unknown error"
		}
		data := rescued.data()
		data.RescuedFrom = providerName
		data.BackendError = "Configured backend '" + providerName + "' failed this call (" + headRunes(shown, 300) + "); result served by the keyless free tier. The next call will use '" + providerName + "' again."
		return rescued, nil
	}
	ringError := "unknown"
	if rescued != nil && rescued.Error != "" {
		ringError = rescued.Error
	}
	lead := originalError
	if lead == "" {
		lead = "search failed"
	}
	return searchFail(lead + " (keyless rescue also failed: " + ringError + ")"), nil
}

// True for a website policy refusal, intentional and never rescued
func policyBlockedResult(p *WebPage) bool {
	return p.BlockedByPolicy != nil || strings.Contains(strings.ToLower(p.Error), "blocked by website policy")
}

// Rescues a whole batch extract failure via the ring, policy blocked entries preserved verbatim, ring errors raised
func rescueExtract(ctx webCallContext, providerName string, urls []string, results []*WebPage) ([]*WebPage, error) {
	env := ctx.env
	parity := len(results) == len(urls)
	var rescueIdx []int
	for i, r := range results {
		if !parity || !policyBlockedResult(r) {
			rescueIdx = append(rescueIdx, i)
		}
	}
	if len(rescueIdx) == 0 {
		return results, nil
	}
	rescueURLs := urls
	if parity {
		rescueURLs = make([]string, 0, len(rescueIdx))
		for _, i := range rescueIdx {
			rescueURLs = append(rescueURLs, urls[i])
		}
	}
	originalError := "extract failed"
	for _, i := range rescueIdx {
		if results[i].Error != "" {
			originalError = results[i].Error
			break
		}
	}
	envLog(env).Warn("web_extract backend failed every URL; one-shot keyless rescue", "backend", providerName, "urls", len(rescueURLs), "error", headRunes(originalError, 200))
	rescued, err := keylessExtractWithFailover(ctx.ctx, env, providerName, rescueURLs)
	if err != nil {
		return nil, err
	}
	if len(rescued) > 0 {
		allFailed := true
		for _, r := range rescued {
			if r.Error == "" {
				allFailed = false
			}
		}
		if allFailed {
			return results, nil
		}
	}
	for _, r := range rescued {
		if r.Error != "" {
			continue
		}
		if r.Metadata == nil {
			r.Metadata = map[string]any{}
		}
		r.Metadata["rescued_from"] = providerName
		r.Metadata["backend_error"] = headRunes(originalError, 300)
	}
	if parity && len(rescued) == len(rescueIdx) {
		out := append([]*WebPage{}, results...)
		for pos, i := range rescueIdx {
			out[i] = rescued[pos]
		}
		return out, nil
	}
	return rescued, nil
}
