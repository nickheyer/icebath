package tools

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

const (
	perplexityDefaultBaseURL    = "https://api.perplexity.ai"
	perplexityKeyURL            = "https://www.perplexity.ai/account/api"
	perplexityMaxSearchResults  = 20
	perplexityMaxTokens         = 16384
	perplexityMaxTokensPerPage  = 4096
	perplexityNoIdentityFailure = "there is no usable Nous identity (sign in with `/login`)"
)

// Static harness identity plus the Perplexity integration header, no per user identifier
func perplexityHeaders() [][2]string {
	return [][2]string{
		{"HTTP-Referer", "https://hermes-agent.nousresearch.com"},
		{"X-Title", "Icebath Agent"},
		{"User-Agent", "IcebathAgent/" + icebathVersion()},
		{"X-Pplx-Integration", "icebath-agent"},
	}
}

func perplexityMissingKeyError() string {
	return "PERPLEXITY_API_KEY is not set. Get a key at " + perplexityKeyURL
}

// Nous gateway config when web search is on the managed route, else nil
func perplexityManagedGateway(env *Env, tokenReader func(*Env) string) (*ManagedToolGatewayConfig, error) {
	if !managedWebSearch(env) {
		return nil, nil
	}
	return ResolveFreeSearchGateway(env, tokenReader)
}

// POST to Perplexity or the gateway and parse the answer, a missing key or error body raised verbatim
func perplexityRequest(ctx context.Context, env *Env, endpoint string, payload *pyjson.Object, gateway *ManagedToolGatewayConfig) (any, error) {
	apiKey, err := providerEnv(ctx, env, "PERPLEXITY_API_KEY")
	if err != nil {
		return nil, err
	}
	headers := perplexityHeaders()
	var baseURL string
	switch {
	case gateway != nil:
		baseURL, apiKey, headers = strings.TrimRight(gateway.GatewayOrigin, "/"), gateway.NousUserToken, [][2]string{headers[2]}
	case apiKey != "":
		override, err := providerEnv(ctx, env, "PERPLEXITY_BASE_URL")
		if err != nil {
			return nil, err
		}
		if override == "" {
			override = perplexityDefaultBaseURL
		}
		baseURL = strings.TrimRight(override, "/")
	default:
		return nil, verbatim(perplexityMissingKeyError())
	}
	target := baseURL + "/" + strings.TrimLeft(endpoint, "/")
	envLog(env).Info("Perplexity request", "endpoint", endpoint, "url", target)
	all := append([][2]string{{"Authorization", "Bearer " + apiKey}, {"Content-Type", "application/json"}}, headers...)
	resp, err := httpxSend(ctx, http.MethodPost, target, nil, all, payload, 60*time.Second)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, verbatim(httpxStatusDetail(resp))
	}
	data, err := resp.JSON()
	if err != nil {
		return nil, verbatim(err.Error())
	}
	return data, nil
}

// Search API rows as url first hits with falsy fields blanked
func normalizePerplexitySearchResults(response any) (*WebSearchResponse, error) {
	results, err := pyDictGet(response, "results")
	if err != nil {
		return nil, err
	}
	rows, err := pyIter(pyOr(results, []any{}))
	if err != nil {
		return nil, err
	}
	hits := make([]WebSearchHit, 0, len(rows))
	for i, r := range rows {
		var vals [3]string
		for j, key := range []string{"title", "url", "snippet"} {
			v, err := pyGetDefault(r, key, "")
			if err != nil {
				return nil, err
			}
			vals[j] = pyStrField(pyOr(v, ""))
		}
		hits = append(hits, titleHit(vals[0], vals[1], vals[2], i+1))
	}
	return searchOK(hits), nil
}

// One document per requested url in request order, omitted or flagged pages carrying an error
func normalizePerplexitySnippets(response any, urls []string) ([]*WebPage, error) {
	results, err := pyDictGet(response, "results")
	if err != nil {
		return nil, err
	}
	rows, err := pyIter(pyOr(results, []any{}))
	if err != nil {
		return nil, err
	}
	byURL := map[string]*pyjson.Object{}
	for _, r := range rows {
		d, ok := pyDict(r)
		if !ok {
			continue
		}
		key, found := d.Get("url")
		if !found {
			key = ""
		}
		switch k := key.(type) {
		case string:
			byURL[k] = d
		case []any, map[string]any, *pyjson.Object:
			return nil, errors.New("unhashable type: '" + pyrepr.TypeName(k) + "'")
		}
	}
	docs := make([]*WebPage, 0, len(urls))
	for _, u := range urls {
		result := byURL[u]
		if result == nil {
			result = pyjson.NewObject()
		}
		textVal, _ := result.Get("text")
		text := pyOr(textVal, "")
		doc := &WebPage{URL: u, Content: pyStrField(text), RawContent: pyStrField(text), HasRaw: true, Metadata: map[string]any{"sourceURL": u}}
		errVal, _ := result.Get("error")
		if pyTruthy(errVal) {
			doc.Error = pyrepr.Str(errVal)
		} else if !pyTruthy(text) {
			doc.Error = "no content returned"
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// Relevance query from the lowered path words of each url, the host for a bare root
func perplexityQueryForURLs(urls []string) (string, error) {
	var words []string
	seen := map[string]bool{}
	for _, raw := range urls {
		parsed, err := pyurl.Parse(raw)
		if err != nil {
			return "", verbatim(err.Error())
		}
		path := strings.NewReplacer("-", " ", "_", " ", "/", " ").Replace(parsed.Path)
		for _, token := range pystr.SplitWhitespace(path, -1) {
			lowered := pystr.Lower(token)
			if !seen[lowered] && !pyStrIsDigit(token) {
				seen[lowered] = true
				words = append(words, lowered)
			}
		}
		if pystr.StripChars(parsed.Path, "/") == "" {
			words = append(words, parsed.Netloc)
			seen[parsed.Netloc] = true
		}
	}
	if q := runePrefix(strings.Join(words, " "), 500); q != "" {
		return q, nil
	}
	return runePrefix(strings.Join(urls, " "), 500), nil
}

// str.isdigit over a whole string, false for an empty one
func pyStrIsDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return true
}

type perplexityProvider struct{ baseWebProvider }

// Available with a key, or on the managed route with a likely usable Nous token
func (p *perplexityProvider) Available(env *Env) bool {
	key, err := providerEnv(context.Background(), env, "PERPLEXITY_API_KEY")
	if err == nil && key != "" {
		return true
	}
	gateway, err := perplexityManagedGateway(env, peekNousAccessToken)
	return err == nil && gateway != nil
}

// Search direct with a key or through the managed fast route, failures as search errors
func (p *perplexityProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	return runSearch(ctx, env, "Perplexity", func() (*WebSearchResponse, error) {
		key, err := providerEnv(ctx, env, "PERPLEXITY_API_KEY")
		if err != nil {
			return nil, err
		}
		managed := key == "" && managedWebSearch(env)
		var gateway *ManagedToolGatewayConfig
		if managed {
			if gateway, err = ResolveFreeSearchGateway(env, nil); err != nil {
				return nil, err
			}
			if gateway == nil {
				return nil, verbatim(SelectionError("web", NousManagedProvider, perplexityNoIdentityFailure))
			}
		}
		envLog(env).Info("Perplexity search", "query", query, "limit", limit, "managed", gateway != nil)
		payload := pyjson.NewObject()
		payload.Set("query", query)
		payload.Set("max_results", max(1, min(limit, perplexityMaxSearchResults)))
		payload.Set("search_context_size", "low")
		if gateway != nil {
			payload.Set("search_type", "fast")
		}
		raw, err := perplexityRequest(ctx, env, "search", payload, gateway)
		if err != nil {
			return nil, err
		}
		return normalizePerplexitySearchResults(raw)
	}, true), nil
}

// Query relevant snippets per url with the direct key, every failure an entry per url
func (p *perplexityProvider) Extract(ctx context.Context, env *Env, urls []string, format string) ([]*WebPage, error) {
	return runExtract(ctx, env, "Perplexity", urls, func() ([]*WebPage, error) {
		envLog(env).Info("Perplexity snippets", "urls", len(urls))
		list := make([]any, len(urls))
		for i, u := range urls {
			list[i] = u
		}
		query, err := perplexityQueryForURLs(urls)
		if err != nil {
			return nil, err
		}
		payload := pyjson.NewObject()
		payload.Set("query", query)
		payload.Set("urls", list)
		payload.Set("max_tokens", perplexityMaxTokens)
		payload.Set("max_tokens_per_page", perplexityMaxTokensPerPage)
		raw, err := perplexityRequest(ctx, env, "sdk/content/snippets", payload, nil)
		if err != nil {
			return nil, err
		}
		return normalizePerplexitySnippets(raw, urls)
	}, true), nil
}

// SetupSchema is the icebath tools picker entry
func (p *perplexityProvider) SetupSchema() map[string]any {
	return setupSchema("Perplexity", "paid",
		"Perplexity Search API — ranked, date-stamped web results plus query-relevant page snippets for extract.",
		"PERPLEXITY_API_KEY", "Perplexity API key", perplexityKeyURL, map[string]any{"web_tier": "paid"})
}

func init() {
	RegisterWebProvider(&perplexityProvider{baseWebProvider{name: "perplexity", display: "Perplexity", keyEnv: "PERPLEXITY_API_KEY", extract: true}})
}
