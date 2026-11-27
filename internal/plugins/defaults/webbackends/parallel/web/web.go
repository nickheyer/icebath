package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	parallelconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/parallel/config"
	"github.com/nickheyer/icebath/internal/web"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

const (
	name  = "parallel"
	title = "Parallel"
	site  = "https://parallel.ai"
	// Variable of the key, as failures name it
	keyEnv     = "PARALLEL_API_KEY"
	missingKey = keyEnv + " environment variable not set. Get your API key at " + site
	// Most results the API gives a search
	maxResults = 20
)

type backends struct {
	// Connections to the API, kept from call to call
	client *http.Client
	// Correlation id of the free plan's rate limit, new for each run and never kept
	session string
}

func Register(registry *web.Registry) {
	id := make([]byte, 16)
	rand.Read(id)
	registry.Register(backends{client: newClient(), session: hex.EncodeToString(id)})
}

func (b backends) Backends() []web.Backend {
	return []web.Backend{{
		Name:     name,
		Title:    title,
		About:    "Search tuned for agents, and page reading, free with limits",
		Settings: []string{parallelconfig.APIKey, parallelconfig.BaseURL, parallelconfig.SearchMode, parallelconfig.Headers},
		Plans: []web.Plan{
			{
				Title: "Free plan",
				Badge: "free · no key",
				About: "No key needed, slower under heavy use",
				Tier:  webconfig.Free,
			},
			{
				Title:  "Paid plan",
				Badge:  "paid",
				About:  "Your API key, with no slowdowns",
				Tier:   webconfig.Paid,
				Key:    parallelconfig.APIKey,
				Prompt: "Parallel API key",
				Link:   site,
			},
		},
		Search:  b.search,
		Extract: b.extract,
		Ready:   func(cfg *config.Config) bool { return key(cfg) != "" },
		Free: &web.Free{
			Search:  b.freeSearch,
			Extract: b.freeExtract,
			Used:    func(cfg *config.Config) bool { return web.Keyless(cfg, name, key(cfg)) },
		},
		Rank: 2,
	}}
}

func key(cfg *config.Config) string {
	return strings.TrimSpace(cfg.GetString(parallelconfig.APIKey))
}

// Search mode set, agentic for none or one the API does not know
func mode(cfg *config.Config) string {
	switch mode := strings.ToLower(strings.TrimSpace(cfg.GetString(parallelconfig.SearchMode))); mode {
	case parallelconfig.Agentic, parallelconfig.OneShot, parallelconfig.Fast:
		return mode
	}
	return parallelconfig.Agentic
}

type searchRequest struct {
	MaxResults    int      `json:"max_results"`
	Mode          string   `json:"mode"`
	Objective     string   `json:"objective"`
	SearchQueries []string `json:"search_queries"`
}

type extractRequest struct {
	URLs        []string `json:"urls"`
	FullContent bool     `json:"full_content"`
}

// Search with the query as both the objective and the one query, its results with their excerpts
func (b backends) search(ctx context.Context, cfg *config.Config, query string, limit int) (*web.Results, error) {
	token := key(cfg)
	if token == "" {
		return web.Failed(missingKey), nil
	}
	request := searchRequest{MaxResults: min(limit, maxResults), Mode: mode(cfg), Objective: query, SearchQueries: []string{query}}
	body, err := b.post(ctx, cfg, token, "/v1beta/search", request, false)
	if err != nil {
		return web.Failed(web.Failure(title, "search", err)), nil
	}
	hits, err := found(body)
	if err != nil {
		return web.Failed(web.Failure(title, "search", err)), nil
	}
	return web.Found(hits), nil
}

// Hits of a search answer as the SDK's models read it
func found(body []byte) ([]web.Hit, error) {
	var answer struct {
		Results json.RawMessage `json:"results"`
	}
	if err := model(body, "results", &answer); err != nil {
		return nil, err
	}
	if answer.Results == nil {
		return nil, missing("SearchResult", "results")
	}
	rows, err := items(answer.Results)
	if err != nil {
		return nil, err
	}
	hits := make([]web.Hit, len(rows))
	for i, row := range rows {
		var result hit
		if err := model(row, "url", &result); err != nil {
			return nil, err
		}
		if !result.URL.set {
			return nil, missing("WebSearchResult", "url")
		}
		hits[i] = result.at(i + 1)
	}
	return hits, nil
}

// Full content of each page, its excerpts when it has none, then the pages Parallel could not read as its errors report them
func (b backends) extract(ctx context.Context, cfg *config.Config, urls []string, _ string) ([]*web.Page, error) {
	token := key(cfg)
	if token == "" {
		return web.Unreadable(urls, missingKey), nil
	}
	body, err := b.post(ctx, cfg, token, "/v1beta/extract", extractRequest{URLs: urls, FullContent: true}, true)
	if err != nil {
		return web.Unreadable(urls, web.Failure(title, "extract", err)), nil
	}
	pages, err := read(body)
	if err != nil {
		return web.Unreadable(urls, web.Failure(title, "extract", err)), nil
	}
	return pages, nil
}

// Pages of an extract answer as the SDK's models read it
func read(body []byte) ([]*web.Page, error) {
	var answer struct {
		Results json.RawMessage `json:"results"`
		Errors  json.RawMessage `json:"errors"`
	}
	if err := model(body, "results", &answer); err != nil {
		return nil, err
	}
	if answer.Results == nil {
		return nil, missing("ExtractResponse", "results")
	}
	rows, err := items(answer.Results)
	if err != nil {
		return nil, err
	}
	var pages []*web.Page
	for _, row := range rows {
		var result document
		if err := model(row, "url", &result); err != nil {
			return nil, err
		}
		if !result.URL.set {
			return nil, missing("ExtractResult", "url")
		}
		pages = append(pages, result.page(result.FullContent))
	}
	if answer.Errors == nil {
		return nil, missing("ExtractResponse", "errors")
	}
	if rows, err = items(answer.Errors); err != nil {
		return nil, err
	}
	for _, row := range rows {
		var miss refusal
		if err := model(row, "url", &miss); err != nil {
			return nil, err
		}
		switch {
		case !miss.URL.set:
			return nil, missing("ExtractError", "url")
		case !miss.ErrorType.set:
			return nil, missing("ExtractError", "error_type")
		}
		pages = append(pages, miss.page())
	}
	return pages, nil
}
