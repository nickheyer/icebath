package tools

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

const (
	perplexityDefaultBaseURL   = "https://api.perplexity.ai"
	perplexityKeyURL           = "https://www.perplexity.ai/account/api"
	perplexityMaxSearchResults = 20
	perplexityMaxTokens        = 16384
	perplexityMaxTokensPerPage = 4096
)

// Static harness identity plus the Perplexity integration header, no per user identifier
func perplexityHeaders() [][2]string {
	return [][2]string{
		{"HTTP-Referer", "https://github.com/nickheyer/icebath"},
		{"X-Title", "Icebath Agent"},
		{"User-Agent", "IcebathAgent/" + icebathVersion()},
		{"X-Pplx-Integration", "icebath-agent"},
	}
}

func perplexityMissingKeyError() string {
	return "PERPLEXITY_API_KEY is not set. Get a key at " + perplexityKeyURL
}

// POST to Perplexity and parse the answer, a missing key or error body raised verbatim
func perplexityRequest(ctx context.Context, env *Env, endpoint string, payload *jsonutil.Object) (any, error) {
	apiKey, err := providerEnv(ctx, env, "PERPLEXITY_API_KEY")
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		return nil, verbatim(perplexityMissingKeyError())
	}
	headers := perplexityHeaders()
	override, err := providerEnv(ctx, env, "PERPLEXITY_BASE_URL")
	if err != nil {
		return nil, err
	}
	if override == "" {
		override = perplexityDefaultBaseURL
	}
	baseURL := strings.TrimRight(override, "/")
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
	results, err := dictGet(response, "results")
	if err != nil {
		return nil, err
	}
	rows, err := elementsOf(anyutil.Or(results, []any{}))
	if err != nil {
		return nil, err
	}
	hits := make([]WebSearchHit, 0, len(rows))
	for i, r := range rows {
		var vals [3]string
		for j, key := range []string{"title", "url", "snippet"} {
			v, err := getDefault(r, key, "")
			if err != nil {
				return nil, err
			}
			vals[j] = anyutil.String(anyutil.Or(v, ""))
		}
		hits = append(hits, titleHit(vals[0], vals[1], vals[2], i+1))
	}
	return searchOK(hits), nil
}

// One document per requested url in request order, omitted or flagged pages carrying an error
func normalizePerplexitySnippets(response any, urls []string) ([]*WebPage, error) {
	results, err := dictGet(response, "results")
	if err != nil {
		return nil, err
	}
	rows, err := elementsOf(anyutil.Or(results, []any{}))
	if err != nil {
		return nil, err
	}
	byURL := map[string]*jsonutil.Object{}
	for _, r := range rows {
		d, ok := mappingOf(r)
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
		case []any, map[string]any, *jsonutil.Object:
			return nil, errors.New("a Perplexity result URL is " + errchain.Kind(k) + ", expected text")
		}
	}
	docs := make([]*WebPage, 0, len(urls))
	for _, u := range urls {
		result := byURL[u]
		if result == nil {
			result = jsonutil.NewObject()
		}
		textVal, _ := result.Get("text")
		text := anyutil.Or(textVal, "")
		doc := &WebPage{URL: u, Content: anyutil.String(text), RawContent: anyutil.String(text), HasRaw: true, Metadata: map[string]any{"sourceURL": u}}
		errVal, _ := result.Get("error")
		if anyutil.IsTrue(errVal) {
			doc.Error = anyutil.String(errVal)
		} else if !anyutil.IsTrue(text) {
			doc.Error = "no content returned"
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// Relevance query from the lowered path words of each url, segment ;parameters left out, the host for a bare root
func perplexityQueryForURLs(urls []string) (string, error) {
	var words []string
	seen := map[string]bool{}
	for _, raw := range urls {
		parsed, err := urlutil.Split(raw)
		if err != nil {
			return "", verbatim(err.Error())
		}
		path := pathWithoutParams(parsed.Path)
		for _, token := range strings.Fields(strings.NewReplacer("-", " ", "_", " ", "/", " ").Replace(path)) {
			lowered := strings.ToLower(token)
			if !seen[lowered] && !strIsDigit(token) {
				seen[lowered] = true
				words = append(words, lowered)
			}
		}
		if strings.Trim(path, "/") == "" {
			words = append(words, parsed.Authority)
			seen[parsed.Authority] = true
		}
	}
	if q := runePrefix(strings.Join(words, " "), 500); q != "" {
		return q, nil
	}
	return runePrefix(strings.Join(urls, " "), 500), nil
}

// URL path with the ;parameters of every segment cut off, leaving the segment names that describe the page
func pathWithoutParams(path string) string {
	if !strings.Contains(path, ";") {
		return path
	}
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i], _, _ = strings.Cut(s, ";")
	}
	return strings.Join(segments, "/")
}

// str.isdigit over a whole string, false for an empty one
func strIsDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

type perplexityProvider struct{ baseWebProvider }

// Available with a Perplexity key
func (p *perplexityProvider) Available(env *Env) bool {
	key, err := providerEnv(context.Background(), env, "PERPLEXITY_API_KEY")
	return err == nil && key != ""
}

// Search with the Perplexity key, failures as search errors
func (p *perplexityProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	return runSearch(ctx, env, "Perplexity", func() (*WebSearchResponse, error) {
		envLog(env).Info("Perplexity search", "query", query, "limit", limit)
		payload := jsonutil.NewObject()
		payload.Set("query", query)
		payload.Set("max_results", max(1, min(limit, perplexityMaxSearchResults)))
		payload.Set("search_context_size", "low")
		raw, err := perplexityRequest(ctx, env, "search", payload)
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
		payload := jsonutil.NewObject()
		payload.Set("query", query)
		payload.Set("urls", list)
		payload.Set("max_tokens", perplexityMaxTokens)
		payload.Set("max_tokens_per_page", perplexityMaxTokensPerPage)
		raw, err := perplexityRequest(ctx, env, "sdk/content/snippets", payload)
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

// Perplexity search and snippet provider the web/perplexity plugin registers
func NewPerplexityWebProvider() WebProvider {
	return &perplexityProvider{baseWebProvider{name: "perplexity", display: "Perplexity", keyEnv: "PERPLEXITY_API_KEY", extract: true}}
}

// Class name the registry log and type errors print, the plugin provider class
func (*perplexityProvider) ErrorKind() string { return "PerplexityWebSearchProvider" }
