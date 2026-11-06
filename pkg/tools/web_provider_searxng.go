package tools

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

type searxngProvider struct{ baseWebProvider }

// Search a user hosted instance, rows ordered by descending score and capped to the limit
func (p *searxngProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	base, err := providerEnv(ctx, env, "SEARXNG_URL")
	if err != nil {
		return nil, err
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		return searchFail("SEARXNG_URL is not set"), nil
	}
	data, failure := httpGetJSON(ctx, env, "SearXNG", base+"/search",
		pyhttpx.Params{{"q", query}, {"format", "json"}, {"pageno", "1"}},
		[][2]string{{"Accept", "application/json"}}, 15*time.Second, "SearXNG at "+base)
	if failure != nil {
		return failure, nil
	}
	results, err := pyGetDefault(data, "results", []any{})
	if err != nil {
		return nil, err
	}
	raw, err := pyIter(results)
	if err != nil {
		return nil, err
	}
	scores := make([]float64, len(raw))
	for i, r := range raw {
		score, err := pyGetDefault(r, "score", 0)
		if err != nil {
			return nil, err
		}
		if scores[i], err = pyint.Float(score); err != nil {
			return nil, err
		}
	}
	order := make([]int, len(raw))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return scores[order[a]] > scores[order[b]] })
	sorted := make([]any, len(raw))
	for i, j := range order {
		sorted[i] = raw[j]
	}
	head, _ := pySeqHead(sorted, limit)
	hits, err := titledRows(head.([]any), "content")
	if err != nil {
		return nil, err
	}
	envLog(env).Info("SearXNG search results", "query", query, "results", len(hits), "raw", len(raw), "limit", limit)
	return searchOK(hits), nil
}

// SetupSchema is the icebath tools picker entry
func (p *searxngProvider) SetupSchema() map[string]any {
	return setupSchema("SearXNG", "free · self-hosted", "Free, privacy-respecting metasearch. Point SEARXNG_URL at your instance.",
		"SEARXNG_URL", "SearXNG instance URL (e.g. http://localhost:8080)", "https://searx.space/", nil)
}

func init() {
	RegisterWebProvider(&searxngProvider{baseWebProvider{name: "searxng", display: "SearXNG", keyEnv: "SEARXNG_URL"}})
}
