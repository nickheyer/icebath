package tools

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const tavilyClientName = "icebath-agent"

// Attribution header plus Bearer auth for a key or the keyless access mode without one
func tavilyHeaders(apiKey string) [][2]string {
	headers := [][2]string{{"X-Client-Name", tavilyClientName}}
	if apiKey != "" {
		return append(headers, [2]string{"Authorization", "Bearer " + apiKey})
	}
	return append(headers, [2]string{"X-Tavily-Access-Mode", "keyless"})
}

// POST to Tavily and parse the JSON answer, a non 2xx body raised verbatim so its text reaches the model
func tavilyRequest(ctx context.Context, env *Env, endpoint string, payload *pyjson.Object, apiKey *string) (any, error) {
	if apiKey == nil {
		key, err := providerEnv(ctx, env, "TAVILY_API_KEY")
		if err != nil {
			return nil, err
		}
		apiKey = &key
	}
	baseURL, err := providerEnv(ctx, env, "TAVILY_BASE_URL")
	if err != nil {
		return nil, err
	}
	if baseURL == "" {
		baseURL = "https://api.tavily.com"
	}
	target := baseURL + "/" + strings.TrimLeft(endpoint, "/")
	envLog(env).Info("Tavily request", "endpoint", endpoint, "url", target)
	resp, err := httpxSend(ctx, http.MethodPost, target, nil, tavilyHeaders(*apiKey), payload, 60*time.Second)
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

// Search answer rows as title first hits
func normalizeTavilySearchResults(response any) (*WebSearchResponse, error) {
	results, err := pyGetDefault(response, "results", []any{})
	if err != nil {
		return nil, err
	}
	rows, err := pyIter(results)
	if err != nil {
		return nil, err
	}
	hits := make([]WebSearchHit, 0, len(rows))
	for i, r := range rows {
		var vals [3]any
		for j, key := range []string{"title", "url", "content"} {
			if vals[j], err = pyGetDefault(r, key, ""); err != nil {
				return nil, err
			}
		}
		hits = append(hits, titleHit(pyStrField(vals[0]), pyStrField(vals[1]), pyStrField(vals[2]), i+1))
	}
	return searchOK(hits), nil
}

// Extract answer as documents, failed results and failed urls as error entries never tied to a request
func normalizeTavilyDocuments(response any) ([]*WebPage, error) {
	var docs []*WebPage
	results, err := pyGetDefault(response, "results", []any{})
	if err != nil {
		return nil, err
	}
	rows, err := pyIter(results)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		u, err := pyGetDefault(r, "url", "")
		if err != nil {
			return nil, err
		}
		title, _ := pyGetDefault(r, "title", "")
		raw, _ := pyGetDefault(r, "raw_content", "")
		if !pyTruthy(raw) {
			raw, _ = pyGetDefault(r, "content", "")
		}
		docs = append(docs, webDocument(pyStrField(u), pyStrField(title), pyStrField(raw), ""))
	}
	failed, err := pyGetDefault(response, "failed_results", []any{})
	if err != nil {
		return nil, err
	}
	if rows, err = pyIter(failed); err != nil {
		return nil, err
	}
	for _, f := range rows {
		u, err := pyGetDefault(f, "url", "")
		if err != nil {
			return nil, err
		}
		msg, _ := pyGetDefault(f, "error", "extraction failed")
		docs = append(docs, tavilyFailedDocument(pyStrField(u), pyStrField(msg)))
	}
	failedURLs, err := pyGetDefault(response, "failed_urls", []any{})
	if err != nil {
		return nil, err
	}
	if rows, err = pyIter(failedURLs); err != nil {
		return nil, err
	}
	for _, u := range rows {
		docs = append(docs, tavilyFailedDocument(pyrepr.Str(u), "extraction failed"))
	}
	if docs == nil {
		docs = []*WebPage{}
	}
	return docs, nil
}

// Error entry with empty content whose source url is its own url
func tavilyFailedDocument(u, msg string) *WebPage {
	return &WebPage{URL: u, Error: msg, HasRaw: true, Metadata: map[string]any{"sourceURL": u}}
}

func tavilyMissingKeyError(action string) string {
	return "TAVILY_API_KEY is not set. Get a key at https://app.tavily.com/home or select Tavily in `icebath tools` for opt-in keyless " + action + "."
}

// Request key, empty when forcing keyless, or the missing key text when neither a key nor keyless applies
func tavilyAuth(ctx context.Context, env *Env, action string) (key string, missing string, prefix string, err error) {
	apiKey, err := providerEnv(ctx, env, "TAVILY_API_KEY")
	if err != nil {
		return "", "", "", err
	}
	forceKeyless := useKeyless(env, "tavily", apiKey)
	if !forceKeyless && apiKey == "" {
		return "", tavilyMissingKeyError(action), "", nil
	}
	if forceKeyless {
		return "", "", "keyless ", nil
	}
	return apiKey, "", "", nil
}

type tavilyProvider struct{ baseWebProvider }

// Search keyed or opt in keyless, every failure a search error result
func (p *tavilyProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	return runSearch(ctx, env, "Tavily", func() (*WebSearchResponse, error) {
		key, missing, prefix, err := tavilyAuth(ctx, env, "search")
		if err != nil {
			return nil, err
		}
		if missing != "" {
			return searchFail(missing), nil
		}
		envLog(env).Info("Tavily "+prefix+"search", "query", query, "limit", limit)
		payload := pyjson.NewObject()
		payload.Set("query", query)
		payload.Set("max_results", min(limit, searchLimitCap))
		payload.Set("include_raw_content", false)
		payload.Set("include_images", false)
		data, err := tavilyRequest(ctx, env, "search", payload, &key)
		if err != nil {
			return nil, err
		}
		return normalizeTavilySearchResults(data)
	}, true), nil
}

// Extract keyed or opt in keyless, failed urls reported as their own entries
func (p *tavilyProvider) Extract(ctx context.Context, env *Env, urls []string, format string) ([]*WebPage, error) {
	return runExtract(ctx, env, "Tavily", urls, func() ([]*WebPage, error) {
		key, missing, prefix, err := tavilyAuth(ctx, env, "extract")
		if err != nil {
			return nil, err
		}
		if missing != "" {
			return extractFail(urls, missing), nil
		}
		envLog(env).Info("Tavily "+prefix+"extract", "urls", len(urls))
		list := make([]any, len(urls))
		for i, u := range urls {
			list[i] = u
		}
		payload := pyjson.NewObject()
		payload.Set("urls", list)
		payload.Set("include_images", false)
		data, err := tavilyRequest(ctx, env, "extract", payload, &key)
		if err != nil {
			return nil, err
		}
		return normalizeTavilyDocuments(data)
	}, true), nil
}

// SetupSchema is the icebath tools picker entry
func (p *tavilyProvider) SetupSchema() map[string]any {
	return setupSchema("Tavily", "free · key optional", "Search + extract. Opt-in keyless; set TAVILY_API_KEY for higher limits.",
		"TAVILY_API_KEY", "Tavily API key (optional — keyless works when Tavily is selected)", "https://app.tavily.com/home", nil)
}

func init() {
	RegisterWebProvider(&tavilyProvider{baseWebProvider{name: "tavily", display: "Tavily", keyEnv: "TAVILY_API_KEY", extract: true, keyless: true}})
}
