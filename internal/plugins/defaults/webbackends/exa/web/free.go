package web

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/web"
)

// Anonymous MCP endpoint of the free plan
const mcp = "https://mcp.exa.ai/mcp"

// Labels opening the lines of a search block
var labels = []string{"Title:", "URL:", "Highlights:", "Published:", "Author:"}

// Search on the free plan, its answer a text of results
func freeSearch(ctx context.Context, query string, limit int) (*web.Results, error) {
	text, err := web.FreeMCP(ctx, mcp, "web_search_exa", map[string]any{"query": query, "numResults": max(1, limit)})
	if err != nil {
		return web.Failed(freeFailure("search", err)), nil
	}
	return web.Found(parse(text, limit)), nil
}

// Results of the blocks between --- lines, each with a Title, URL, Published, Author and Highlights, up to the limit unless it is zero. A
// block without a URL is left out
func parse(text string, limit int) []web.Hit {
	hits := []web.Hit{}
	for block := range strings.SplitSeq(text, "\n---\n") {
		var title, url string
		var highlights []string
		within := false
		for line := range strings.Lines(block) {
			line = strings.TrimSpace(line)
			labeled := slices.ContainsFunc(labels, func(label string) bool { return strings.HasPrefix(line, label) })
			switch {
			case strings.HasPrefix(line, "Title:"):
				title = strings.TrimSpace(strings.TrimPrefix(line, "Title:"))
			case strings.HasPrefix(line, "URL:"):
				url = strings.TrimSpace(strings.TrimPrefix(line, "URL:"))
			case within && line != "" && !labeled:
				highlights = append(highlights, line)
			}
			if labeled {
				within = strings.HasPrefix(line, "Highlights:")
			}
		}
		if url != "" {
			hits = append(hits, web.Hit{URL: url, Title: title, Description: strings.Join(highlights, " "), Position: len(hits) + 1})
		}
		if limit != 0 && len(hits) >= limit {
			break
		}
	}
	return hits
}

// Pages read one at a time on the free plan
func freeExtract(ctx context.Context, urls []string) ([]*web.Page, error) {
	pages := make([]*web.Page, len(urls))
	for i, url := range urls {
		pages[i] = fetch(ctx, url)
	}
	return pages, nil
}

// Page read on the free plan, titled by its first heading or Title line. A crawl failure naming the page's own HTTP answer is told so
func fetch(ctx context.Context, url string) *web.Page {
	text, err := web.FreeMCP(ctx, mcp, "web_fetch_exa", map[string]any{"urls": []string{url}})
	if err != nil {
		if code, ok := crawlStatus(err.Error()); ok {
			if origin := web.OriginError(code, ""); origin != "" {
				return web.Unread(url, origin)
			}
		}
		return web.Unread(url, freeFailure("extract", err))
	}
	return web.Document(url, heading(text), text)
}

func heading(text string) string {
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(rest)
		}
		if rest, ok := strings.CutPrefix(line, "Title:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func freeFailure(kind string, err error) string {
	return web.FreeFailure(title, kind, err, keyEnv, site, true)
}
