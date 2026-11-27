package web

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	perplexityconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/perplexity/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/web"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	keyLink = "https://www.perplexity.ai/account/api"
	timeout = 60 * time.Second
	// Most results Perplexity gives for one search
	maxResults = 20
	// Most tokens of snippets for all pages of one read, and for each page
	maxTokens        = 16384
	maxTokensPerPage = 4096
	// Longest query the snippets of a read are picked by
	maxQuery = 500
)

type backends struct{}

func Register(registry *web.Registry) {
	registry.Register(backends{})
}

func (backends) Backends() []web.Backend {
	return []web.Backend{{
		Name:     "perplexity",
		Title:    "Perplexity",
		About:    "Ranked web results with the parts of pages that matter",
		Settings: []string{perplexityconfig.APIKey, perplexityconfig.BaseURL},
		Plans: []web.Plan{{
			Title:  "Paid plan",
			Badge:  "paid",
			About:  "Ranked, dated results and the parts of pages that matter",
			Tier:   webconfig.Paid,
			Key:    perplexityconfig.APIKey,
			Prompt: "Perplexity API key",
			Link:   keyLink,
		}},
		Search:  search,
		Extract: extract,
		Ready:   func(cfg *config.Config) bool { return key(cfg) != "" },
		Rank:    4,
	}}
}

func key(cfg *config.Config) string {
	return strings.TrimSpace(cfg.GetString(perplexityconfig.APIKey))
}

// Posts to Perplexity and reads the JSON answer into v. A missing key, or what Perplexity said with a failing status, is the failure,
// given to the model as it is; the error is anything else that went wrong
func post(ctx context.Context, cfg *config.Config, endpoint string, payload, v any) (string, error) {
	key := key(cfg)
	if key == "" {
		return "PERPLEXITY_API_KEY is not set. Get a key at " + keyLink, nil
	}
	base := cmp.Or(strings.TrimRight(strings.TrimSpace(cfg.GetString(perplexityconfig.BaseURL)), "/"), perplexityconfig.DefaultBaseURL)
	response, err := web.Send(ctx, web.Request{
		Method: http.MethodPost,
		URL:    base + "/" + endpoint,
		Header: map[string]string{
			"Authorization":      "Bearer " + key,
			"HTTP-Referer":       "https://github.com/nickheyer/icebath",
			"X-Title":            "icebath",
			"User-Agent":         provider.UserAgent("IcebathAgent"),
			"X-Pplx-Integration": "icebath-agent",
		},
		Body:    payload,
		Timeout: timeout,
	})
	if err != nil {
		return "", err
	}
	if response.Status >= 400 {
		return response.Detail(), nil
	}
	return "", response.Decode(v)
}

type result struct {
	Title   web.Text `json:"title"`
	URL     web.Text `json:"url"`
	Snippet web.Text `json:"snippet"`
}

func search(ctx context.Context, cfg *config.Config, query string, limit int) (*web.Results, error) {
	var answer struct {
		Results []result `json:"results"`
	}
	payload := map[string]any{"query": query, "max_results": max(1, min(limit, maxResults)), "search_context_size": "low"}
	failure, err := post(ctx, cfg, "search", payload, &answer)
	switch {
	case err != nil:
		return web.Failed(web.Failure("Perplexity", "search", err)), nil
	case failure != "":
		return web.Failed(failure), nil
	}
	hits := make([]web.Hit, len(answer.Results))
	for i, row := range answer.Results {
		hits[i] = web.Hit{Title: row.Title.String(), URL: row.URL.String(), Description: row.Snippet.String(), Position: i + 1}
	}
	return web.Found(hits), nil
}

type snippet struct {
	URL   web.Text `json:"url"`
	Text  web.Text `json:"text"`
	Error web.Text `json:"error"`
}

// The parts of each page relevant to the words of the URLs, one page for each URL in the order asked. A page Perplexity left out or
// flagged carries the error
func extract(ctx context.Context, cfg *config.Config, urls []string, _ string) ([]*web.Page, error) {
	var answer struct {
		Results []json.RawMessage `json:"results"`
	}
	payload := map[string]any{"query": relevance(urls), "urls": urls, "max_tokens": maxTokens, "max_tokens_per_page": maxTokensPerPage}
	failure, err := post(ctx, cfg, "sdk/content/snippets", payload, &answer)
	switch {
	case err != nil:
		return web.Unreadable(urls, web.Failure("Perplexity", "extract", err)), nil
	case failure != "":
		return web.Unreadable(urls, failure), nil
	}
	found := map[string]snippet{}
	for _, raw := range answer.Results {
		var row snippet
		if json.Unmarshal(raw, &row) == nil {
			found[row.URL.String()] = row
		}
	}
	pages := make([]*web.Page, len(urls))
	for i, url := range urls {
		row := found[url]
		page := &web.Page{URL: url, Content: row.Text.String()}
		switch {
		case row.Error != "":
			page.Error = row.Error.String()
		case row.Text == "":
			page.Error = "no content returned"
		}
		pages[i] = page
	}
	return pages, nil
}

// Authority and path of a reference, cut as RFC 3986 appendix B reads it
var reference = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9+.\-]*:)?(?://([^/?#]*))?([^?#]*)`)

var separators = strings.NewReplacer("-", " ", "_", " ", "/", " ")

// Query the snippets are picked by: the lower cased words of the URLs' paths, without ;parameters and numbers, and the host of a URL
// without a path; the URLs themselves when that leaves nothing
func relevance(urls []string) string {
	var words []string
	seen := map[string]bool{}
	for _, raw := range urls {
		parts := reference.FindStringSubmatch(raw)
		authority, path := parts[1], unparameterized(parts[2])
		for _, token := range strings.Fields(separators.Replace(path)) {
			if lower := strings.ToLower(token); !seen[lower] && !number(token) {
				seen[lower] = true
				words = append(words, lower)
			}
		}
		if strings.Trim(path, "/") == "" {
			words = append(words, authority)
			seen[authority] = true
		}
	}
	return strs.Head(cmp.Or(strings.Join(words, " "), strings.Join(urls, " ")), maxQuery)
}

// Path without the ;parameters of its segments, leaving the names that describe the page
func unparameterized(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i], _, _ = strings.Cut(segment, ";")
	}
	return strings.Join(segments, "/")
}

func number(word string) bool {
	return strings.IndexFunc(word, func(r rune) bool { return !unicode.IsDigit(r) }) < 0
}
