package web

import (
	"cmp"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	tavilyconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/tavily/config"
	"github.com/nickheyer/icebath/internal/web"
)

const (
	name    = "tavily"
	keyLink = "https://app.tavily.com/home"
	// Name Tavily is told its calls come from
	clientName = "icebath-agent"
	timeout    = 60 * time.Second
	// Most results Tavily gives for one search
	maxResults = 20
)

type backends struct{}

func Register(registry *web.Registry) {
	registry.Register(backends{})
}

func (backends) Backends() []web.Backend {
	return []web.Backend{{
		Name:     name,
		Title:    "Tavily",
		About:    "Tavily web search + extract. Opt-in keyless via /web; set TAVILY_API_KEY for higher limits — https://app.tavily.com/home.",
		Settings: []string{tavilyconfig.APIKey, tavilyconfig.BaseURL},
		Plans: []web.Plan{{
			Title:  "Tavily",
			Badge:  "free · key optional",
			About:  "Search + extract. Opt-in keyless; set TAVILY_API_KEY for higher limits.",
			Key:    tavilyconfig.APIKey,
			Prompt: "Tavily API key (optional — keyless works when Tavily is selected)",
			Link:   keyLink,
		}},
		Search:  search,
		Extract: extract,
		Ready:   func(cfg *config.Config) bool { return key(cfg) != "" || web.Picked(cfg, name) },
		Keyless: true,
		Rank:    3,
	}}
}

func key(cfg *config.Config) string {
	return strings.TrimSpace(cfg.GetString(tavilyconfig.APIKey))
}

// Key the call sends, none on the keyless plan, or why the call cannot be made when there is neither
func auth(cfg *config.Config, action string) (string, string) {
	key := key(cfg)
	switch {
	case web.Keyless(cfg, name, key):
		return "", ""
	case key == "":
		return "", "TAVILY_API_KEY is not set. Get a key at " + keyLink + " or select Tavily in /web for opt-in keyless " + action + "."
	}
	return key, ""
}

// Posts to Tavily and reads the JSON answer into v. What Tavily said with a failing status is the failure, given to the model as it is;
// the error is anything else that went wrong
func post(ctx context.Context, cfg *config.Config, endpoint, key string, payload, v any) (string, error) {
	header := map[string]string{"X-Client-Name": clientName}
	if key != "" {
		header["Authorization"] = "Bearer " + key
	} else {
		header["X-Tavily-Access-Mode"] = "keyless"
	}
	base := cmp.Or(strings.TrimRight(strings.TrimSpace(cfg.GetString(tavilyconfig.BaseURL)), "/"), tavilyconfig.DefaultBaseURL)
	response, err := web.Send(ctx, web.Request{Method: http.MethodPost, URL: base + "/" + endpoint, Header: header, Body: payload, Timeout: timeout})
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
	Content web.Text `json:"content"`
}

func search(ctx context.Context, cfg *config.Config, query string, limit int) (*web.Results, error) {
	key, missing := auth(cfg, "search")
	if missing != "" {
		return web.Failed(missing), nil
	}
	var answer struct {
		Results []result `json:"results"`
	}
	payload := map[string]any{"query": query, "max_results": min(limit, maxResults), "include_raw_content": false, "include_images": false}
	failure, err := post(ctx, cfg, "search", key, payload, &answer)
	switch {
	case err != nil:
		return web.Failed(web.Failure("Tavily", "search", err)), nil
	case failure != "":
		return web.Failed(failure), nil
	}
	hits := make([]web.Hit, len(answer.Results))
	for i, row := range answer.Results {
		hits[i] = web.Hit{Title: row.Title.String(), URL: row.URL.String(), Description: row.Content.String(), Position: i + 1}
	}
	return web.Found(hits), nil
}

type document struct {
	URL        web.Text `json:"url"`
	Title      web.Text `json:"title"`
	RawContent web.Text `json:"raw_content"`
	Content    web.Text `json:"content"`
}

type failed struct {
	URL   web.Text `json:"url"`
	Error web.Text `json:"error"`
}

// Pages as Tavily lists them: the ones it read, then the ones it failed with its reason, then the URLs it failed without one
func extract(ctx context.Context, cfg *config.Config, urls []string, _ string) ([]*web.Page, error) {
	key, missing := auth(cfg, "extract")
	if missing != "" {
		return web.Unreadable(urls, missing), nil
	}
	var answer struct {
		Results       []document `json:"results"`
		FailedResults []failed   `json:"failed_results"`
		FailedURLs    []web.Text `json:"failed_urls"`
	}
	failure, err := post(ctx, cfg, "extract", key, map[string]any{"urls": urls, "include_images": false}, &answer)
	switch {
	case err != nil:
		return web.Unreadable(urls, web.Failure("Tavily", "extract", err)), nil
	case failure != "":
		return web.Unreadable(urls, failure), nil
	}
	pages := []*web.Page{}
	for _, row := range answer.Results {
		pages = append(pages, web.Document(row.URL.String(), row.Title.String(), cmp.Or(row.RawContent, row.Content).String()))
	}
	for _, row := range answer.FailedResults {
		pages = append(pages, web.Unread(row.URL.String(), cmp.Or(row.Error.String(), "extraction failed")))
	}
	for _, url := range answer.FailedURLs {
		pages = append(pages, web.Unread(url.String(), "extraction failed"))
	}
	return pages, nil
}
