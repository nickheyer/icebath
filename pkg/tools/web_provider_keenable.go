package tools

import (
	"context"
	"fmt"
	"net/http"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
)

// Test seam for the keyed Keenable API root
var keenableAPIURL = "https://api.keenable.ai"

var keenableTimeouts = pyrequests.Seconds(30)

// App identifier the keyless tier requires plus Bearer auth when a key is set
func keenableHeaders(apiKey string) http.Header {
	h := http.Header{}
	h.Set("X-Keenable-Title", "icebath-agent")
	if apiKey != "" {
		h.Set("Authorization", "Bearer "+apiKey)
	}
	return h
}

// requests call with its default headers, the JSON body written like requests json=
func keenableSend(ctx context.Context, method, target string, payload *pyjson.Object, apiKey string) (*sdkResponse, error) {
	h := keenableHeaders(apiKey)
	h.Set("User-Agent", icebathUserAgent())
	h.Set("Accept", "*/*")
	var body []byte
	if payload != nil {
		text, err := pyjson.Dumps(payload, pyjson.AllowNaN(false))
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
			envLog(env).Info("Keenable keyless search", "query", query, "limit", limit)
			return keylessSearchWithFailover(ctx, env, "keenable", query, limit), nil
		}
		envLog(env).Info("Keenable search", "query", query, "limit", limit)
		payload := pyjson.NewObject()
		payload.Set("query", query)
		payload.Set("max_results", min(max(1, limit), searchLimitCap))
		resp, err := keenableSend(ctx, http.MethodPost, keenableAPIURL+"/v1/search", payload, apiKey)
		if err != nil {
			return nil, err
		}
		if resp.status >= 400 {
			return searchFail("Keenable search failed: " + httpStatusDetail(resp.status, resp.text())), nil
		}
		data, err := pyjson.LoadsBytes(resp.body)
		if err != nil {
			return nil, err
		}
		results, err := pyDictGet(data, "results")
		if err != nil {
			return nil, err
		}
		rows, err := pyIter(pyOr(results, []any{}))
		if err != nil {
			return nil, err
		}
		hits := make([]WebSearchHit, 0, len(rows))
		for i, r := range rows {
			u, err := pyDictGet(r, "url")
			if err != nil {
				return nil, err
			}
			title, _ := pyDictGet(r, "title")
			snippet, _ := pyDictGet(r, "snippet")
			if !pyTruthy(snippet) {
				snippet, _ = pyDictGet(r, "description")
			}
			hits = append(hits, webHit(pyStrField(pyOr(u, "")), pyStrField(pyOr(title, "")), pyStrField(pyOr(snippet, "")), i+1))
		}
		return searchOK(hits), nil
	}, false), nil
}

// One keyed fetch, the origin's own refusal named as such and every other failure raised
func keenableFetch(ctx context.Context, apiKey, u string) (*WebPage, error) {
	target := pyhttpx.MergeParams(keenableAPIURL+"/v1/fetch", pyhttpx.Params{{"url", u}})
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
	data, err := pyjson.LoadsBytes(resp.body)
	if err != nil {
		return nil, err
	}
	final, err := pyDictGet(data, "url")
	if err != nil {
		return nil, err
	}
	title, _ := pyDictGet(data, "title")
	content, _ := pyDictGet(data, "content")
	return webDocument(pyStrField(pyOr(final, u)), pyStrField(pyOr(title, "")), pyStrField(pyOr(content, "")), u), nil
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
			return keylessExtractWithFailover(ctx, env, "keenable", urls), nil
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

func init() {
	RegisterWebProvider(&keenableProvider{baseWebProvider{name: "keenable", display: "Keenable", keyEnv: "KEENABLE_API_KEY", extract: true, keyless: true}})
}
