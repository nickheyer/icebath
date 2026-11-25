package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Test seam for the keyed Keenable API root
var keenableAPIURL = "https://api.keenable.ai"

// Keenable client limit for each phase of a request
var keenableTimeouts = httputil.EachPhase(30 * time.Second)

// App identifier the keyless tier requires plus Bearer auth when a key is set
func keenableHeaders(apiKey string) http.Header {
	h := http.Header{}
	h.Set("X-Keenable-Title", "icebath-agent")
	if apiKey != "" {
		h.Set("Authorization", "Bearer "+apiKey)
	}
	return h
}

// One Keenable call with the default headers, a payload sent as a JSON body
func keenableSend(ctx context.Context, method, target string, payload *jsonutil.Object, apiKey string) (*sdkResponse, error) {
	h := keenableHeaders(apiKey)
	h.Set("User-Agent", icebathUserAgent())
	h.Set("Accept", "*/*")
	var body []byte
	if payload != nil {
		text, err := jsonutil.Encode(payload)
		if err != nil {
			return nil, err
		}
		body = []byte(text)
		h.Set("Content-Type", "application/json")
	}
	return requestsSend(ctx, keenableTimeouts, method, target, body, h)
}

type keenableProvider struct{ baseWebProvider }

// Search keyed or through the keyless ring, an error status a search failure carrying the body
func (p *keenableProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	return runSearch(ctx, env, "Keenable", func() (*WebSearchResponse, error) {
		apiKey, err := providerEnv(ctx, env, "KEENABLE_API_KEY")
		if err != nil {
			return nil, err
		}
		if useKeyless(env, "keenable", apiKey) {
			envLog(env).Info(fmt.Sprintf("Keenable keyless search: '%s' (limit=%d)", query, limit))
			return keylessSearchWithFailover(ctx, env, "keenable", query, limit)
		}
		envLog(env).Info("Keenable search", "query", query, "limit", limit)
		payload := jsonutil.NewObject()
		payload.Set("query", query)
		payload.Set("max_results", min(max(1, limit), searchLimitCap))
		resp, err := keenableSend(ctx, http.MethodPost, keenableAPIURL+"/v1/search", payload, apiKey)
		if err != nil {
			return nil, err
		}
		if resp.status >= 400 {
			return searchFail("Keenable search failed: " + httpStatusDetail(resp.status, resp.text())), nil
		}
		data, err := jsonutil.Decode(resp.body)
		if err != nil {
			return nil, err
		}
		results, err := dictGet(data, "results")
		if err != nil {
			return nil, err
		}
		rows, err := elementsOf(anyutil.Or(results, []any{}))
		if err != nil {
			return nil, err
		}
		hits := make([]WebSearchHit, 0, len(rows))
		for i, r := range rows {
			u, err := dictGet(r, "url")
			if err != nil {
				return nil, err
			}
			title, _ := dictGet(r, "title")
			snippet, _ := dictGet(r, "snippet")
			if !anyutil.IsTrue(snippet) {
				snippet, _ = dictGet(r, "description")
			}
			hits = append(hits, webHit(anyutil.String(anyutil.Or(u, "")), anyutil.String(anyutil.Or(title, "")), anyutil.String(anyutil.Or(snippet, "")), i+1))
		}
		return searchOK(hits), nil
	}, false), nil
}

// One keyed fetch, the origin's own refusal named as such and every other failure raised
func keenableFetch(ctx context.Context, apiKey, u string) (*WebPage, error) {
	target := httputil.MergeQuery(keenableAPIURL+"/v1/fetch", url.Values{"url": {u}})
	resp, err := keenableSend(ctx, http.MethodGet, target, nil, apiKey)
	if err != nil {
		return nil, err
	}
	if resp.status >= 400 {
		if origin := keenableOriginError(resp.status, resp.text()); origin != "" {
			return pageError(u, origin), nil
		}
		return nil, fmt.Errorf("%s", httpStatusDetail(resp.status, resp.text()))
	}
	data, err := jsonutil.Decode(resp.body)
	if err != nil {
		return nil, err
	}
	final, err := dictGet(data, "url")
	if err != nil {
		return nil, err
	}
	title, _ := dictGet(data, "title")
	content, _ := dictGet(data, "content")
	return webDocument(anyutil.String(anyutil.Or(final, u)), anyutil.String(anyutil.Or(title, "")), anyutil.String(anyutil.Or(content, "")), u), nil
}

// Extract keyed one url at a time or through the keyless ring, each failure its own entry
func (p *keenableProvider) Extract(ctx context.Context, env *Env, urls []string, format string) ([]*WebPage, error) {
	return runExtract(ctx, env, "Keenable", urls, func() ([]*WebPage, error) {
		apiKey, err := providerEnv(ctx, env, "KEENABLE_API_KEY")
		if err != nil {
			return nil, err
		}
		if useKeyless(env, "keenable", apiKey) {
			envLog(env).Info("Keenable keyless extract", "urls", len(urls))
			return keylessExtractWithFailover(ctx, env, "keenable", urls)
		}
		envLog(env).Info("Keenable extract", "urls", len(urls))
		results := make([]*WebPage, 0, len(urls))
		for _, u := range urls {
			page, err := keenableFetch(ctx, apiKey, u)
			if err != nil {
				page = pageError(u, "Keenable extract failed: "+err.Error())
			}
			results = append(results, page)
		}
		return results, nil
	}, false), nil
}

// SetupSchema is the icebath tools picker entry
func (p *keenableProvider) SetupSchema() map[string]any {
	return keylessVariantSchema("Keenable", "KEENABLE_API_KEY", "https://keenable.ai",
		"Independent web index for AI apps — fast search + page fetch on Keenable's anonymous free tier.",
		"Independent web index for AI apps. Keyed access with higher limits and guaranteed service.")
}

// Keenable search and fetch provider the web/keenable plugin registers
func NewKeenableWebProvider() WebProvider {
	return &keenableProvider{baseWebProvider{name: "keenable", display: "Keenable", keyEnv: "KEENABLE_API_KEY", extract: true, keyless: true}}
}
