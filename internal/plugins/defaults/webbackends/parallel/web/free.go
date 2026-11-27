package web

import (
	"context"
	"encoding/json"

	"github.com/nickheyer/icebath/internal/web"
)

const (
	// Anonymous MCP endpoint of the free plan
	mcp        = "https://search.parallel.ai/mcp"
	unexpected = "Keyless Parallel search returned an unexpected payload: "
)

// Search on the free plan with the query as both the objective and the one query, its results cut to the limit unless it is zero. An
// answer that is not JSON or holds no list of results is unexpected; one that is no object, or whose results are not, fails the call
func (b backends) freeSearch(ctx context.Context, query string, limit int) (*web.Results, error) {
	text, err := web.FreeMCP(ctx, mcp, "web_search", map[string]any{"objective": query, "search_queries": []string{query}, "session_id": b.session})
	if err != nil {
		return web.Failed(freeFailure("search", err)), nil
	}
	data := []byte(text)
	if err := json.Unmarshal(data, new(json.RawMessage)); err != nil {
		return web.Failed(unexpected + err.Error()), nil
	}
	var answer struct {
		Results json.RawMessage `json:"results"`
	}
	if err := web.Object(data, &answer); err != nil {
		return nil, err
	}
	rows, err := items(answer.Results)
	if err != nil {
		return web.Failed(unexpected + err.Error()), nil
	}
	if limit != 0 {
		rows = rows[:min(max(limit, 0), len(rows))]
	}
	hits := make([]web.Hit, len(rows))
	for i, row := range rows {
		var result hit
		if err := web.Object(row, &result); err != nil {
			return nil, err
		}
		hits[i] = result.at(i + 1)
	}
	return web.Found(hits), nil
}

// Pages read on the free plan: those it read, with their full content, else their content, else their excerpts, then those it could not,
// then a failure for each URL it said nothing of. An answer that is not an object of such lists fails the call
func (b backends) freeExtract(ctx context.Context, urls []string) ([]*web.Page, error) {
	text, err := web.FreeMCP(ctx, mcp, "web_fetch", map[string]any{"urls": urls, "objective": "Full page content", "session_id": b.session})
	data := []byte(text)
	if err == nil {
		err = json.Unmarshal(data, new(json.RawMessage))
	}
	if err != nil {
		return web.Unreadable(urls, freeFailure("extract", err)), nil
	}
	var answer struct {
		Results json.RawMessage `json:"results"`
		Errors  json.RawMessage `json:"errors"`
	}
	if err := web.Object(data, &answer); err != nil {
		return nil, err
	}
	rows, err := items(answer.Results)
	if err != nil {
		return nil, err
	}
	pages := []*web.Page{}
	for _, row := range rows {
		var result document
		if err := web.Object(row, &result); err != nil {
			return nil, err
		}
		pages = append(pages, result.page(result.FullContent, result.Content))
	}
	if rows, err = items(answer.Errors); err != nil {
		return nil, err
	}
	for _, row := range rows {
		var miss refusal
		if err := web.Object(row, &miss); err != nil {
			return nil, err
		}
		pages = append(pages, miss.page())
	}
	seen := map[string]bool{}
	for _, page := range pages {
		seen[page.URL] = true
	}
	for _, url := range urls {
		if !seen[url] {
			pages = append(pages, web.Unread(url, "no content returned"))
		}
	}
	return pages, nil
}

func freeFailure(kind string, err error) string {
	return web.FreeFailure(title, kind, err, keyEnv, site, true)
}
