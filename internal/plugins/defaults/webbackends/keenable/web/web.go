package web

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	keenableconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/keenable/config"
	"github.com/nickheyer/icebath/internal/web"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	name  = "keenable"
	title = "Keenable"
	api   = "https://api.keenable.ai"
	site  = "https://keenable.ai"
	// App names Keenable is told the calls come from, keyed and on the free plan, which requires one
	keyedApp = "icebath-agent"
	freeApp  = "icebath"
	timeout  = 30 * time.Second
	// Most results a keyed search asks for
	maxResults = 20
)

type backends struct{}

func Register(registry *web.Registry) {
	registry.Register(backends{})
}

func (backends) Backends() []web.Backend {
	return []web.Backend{{
		Name:     name,
		Title:    title,
		About:    "Keenable web search + page fetch (independent web index for AI apps). Works keyless on Keenable's free tier as part of the default rotation; set KEENABLE_API_KEY for higher limits — https://keenable.ai.",
		Settings: []string{keenableconfig.APIKey},
		Plans: []web.Plan{
			{
				Title: "Keenable · Free (keyless)",
				Badge: "free · no key",
				About: "Independent web index for AI apps — fast search + page fetch on Keenable's anonymous free tier.",
				Tier:  webconfig.Free,
			},
			{
				Title:  "Keenable · Paid (API key)",
				Badge:  "paid",
				About:  "Independent web index for AI apps. Keyed access with higher limits and guaranteed service.",
				Tier:   webconfig.Paid,
				Key:    keenableconfig.APIKey,
				Prompt: "Keenable API key",
				Link:   site,
			},
		},
		Search:  search,
		Extract: extract,
		Ready:   func(cfg *config.Config) bool { return key(cfg) != "" },
		Free: &web.Free{
			Search:  freeSearch,
			Extract: freeExtract,
			Used:    func(cfg *config.Config) bool { return web.Keyless(cfg, name, key(cfg)) },
		},
	}}
}

func key(cfg *config.Config) string {
	return strings.TrimSpace(cfg.GetString(keenableconfig.APIKey))
}

// Headers of a keyed call: the app name, and the key when one is set
func header(key string) map[string]string {
	header := map[string]string{"X-Keenable-Title": keyedApp}
	if key != "" {
		header["Authorization"] = "Bearer " + key
	}
	return header
}

func search(ctx context.Context, cfg *config.Config, query string, limit int) (*web.Results, error) {
	hits, err := find(ctx, key(cfg), query, limit)
	if err != nil {
		return web.Failed(web.Failure(title, "search", err)), nil
	}
	return web.Found(hits), nil
}

// Keyed search, a failing status the error carrying what Keenable said
func find(ctx context.Context, key, query string, limit int) ([]web.Hit, error) {
	response, err := web.Send(ctx, web.Request{
		Method:  http.MethodPost,
		URL:     api + "/v1/search",
		Header:  header(key),
		Body:    map[string]any{"query": query, "max_results": min(max(1, limit), maxResults)},
		Timeout: timeout,
	})
	if err != nil {
		return nil, err
	}
	if response.Status >= 400 {
		return nil, errors.New(response.Detail())
	}
	var answer found
	if err := response.Decode(&answer); err != nil {
		return nil, err
	}
	return answer.hits(), nil
}

// Reads the pages one at a time with the key, each failure the page's error
func extract(ctx context.Context, cfg *config.Config, urls []string, _ string) ([]*web.Page, error) {
	key := key(cfg)
	pages := make([]*web.Page, len(urls))
	for i, link := range urls {
		page, err := fetch(ctx, key, link)
		if err != nil {
			page = web.Unread(link, web.Failure(title, "extract", err))
		}
		pages[i] = page
	}
	return pages, nil
}

// Keyed read of one page. Keenable saying the page itself answered with a failing status is the page's error, any other failure the
// error
func fetch(ctx context.Context, key, link string) (*web.Page, error) {
	response, err := web.Send(ctx, web.Request{URL: api + "/v1/fetch", Query: url.Values{"url": {link}}, Header: header(key), Timeout: timeout})
	if err != nil {
		return nil, err
	}
	if response.Status >= 400 {
		if origin := originError(response.Status, response.Text()); origin != "" {
			return web.Unread(link, origin), nil
		}
		return nil, errors.New(response.Detail())
	}
	var read document
	if err := response.Decode(&read); err != nil {
		return nil, err
	}
	return read.page(link), nil
}

// Anonymous search on the free plan
func freeSearch(ctx context.Context, query string, limit int) (*web.Results, error) {
	var answer found
	err := public(ctx, http.MethodPost, "/v1/search/public", nil, map[string]any{"query": query, "max_results": max(1, limit)}, &answer)
	switch {
	case web.IsFreeError(err):
		return web.Failed(web.FreeFailure(title, "search", err, keenableconfig.APIKeyEnv, site, true)), nil
	case err != nil:
		return web.Failed(fmt.Sprintf("Keyless Keenable search failed: %v.", err)), nil
	}
	return web.Found(answer.hits()), nil
}

// Anonymous read of each page on the free plan, the page's own failing answer that Keenable relays kept as its error
func freeExtract(ctx context.Context, urls []string) ([]*web.Page, error) {
	pages := make([]*web.Page, len(urls))
	for i, link := range urls {
		var read document
		err := public(ctx, http.MethodGet, "/v1/fetch/public", url.Values{"url": {link}}, nil, &read)
		switch {
		case err == nil:
			pages[i] = read.page(link)
		case web.IsFreeError(err) && web.Origin(err.Error()):
			pages[i] = web.Unread(link, err.Error())
		default:
			pages[i] = web.Unread(link, web.FreeFailure(title, "extract", err, keenableconfig.APIKeyEnv, site, false))
		}
	}
	return pages, nil
}

// Calls a public endpoint of the free plan and reads the JSON answer into v. A failing status is a FreeError, naming the page's own
// answer when Keenable says the page answered so, else the status and the start of what Keenable said
func public(ctx context.Context, method, path string, query url.Values, body, v any) error {
	response, err := web.Send(ctx, web.Request{
		Method:  method,
		URL:     api + path,
		Query:   query,
		Header:  map[string]string{"X-Keenable-Title": freeApp},
		Body:    body,
		Timeout: web.FreeTimeout,
	})
	if err != nil {
		return err
	}
	if response.Status >= 400 {
		text := strings.TrimSpace(response.Text())
		if origin := originError(response.Status, text); origin != "" {
			return web.FreeErrorf("%s", origin)
		}
		return web.FreeErrorf("HTTP %d: %s", response.Status, strs.Head(text, 300))
	}
	return response.Decode(v)
}

var originMessage = regexp.MustCompile(`(?i)^the (?:target server|page does not exist)\b`)

// Error naming the page's own HTTP answer when the message of Keenable's failure says the page answered so, empty otherwise
func originError(status int, body string) string {
	var failure struct {
		Message web.Text `json:"message"`
	}
	if json.Unmarshal([]byte(body), &failure) != nil {
		return ""
	}
	message := strings.TrimSpace(failure.Message.String())
	if !originMessage.MatchString(message) {
		return ""
	}
	return web.OriginError(status, message)
}

type found struct {
	Results []struct {
		URL         web.Text `json:"url"`
		Title       web.Text `json:"title"`
		Snippet     web.Text `json:"snippet"`
		Description web.Text `json:"description"`
	} `json:"results"`
}

// Results in Keenable's order, the snippet else the description as their text
func (f found) hits() []web.Hit {
	hits := make([]web.Hit, len(f.Results))
	for i, row := range f.Results {
		hits[i] = web.Hit{Title: row.Title.String(), URL: row.URL.String(), Description: cmp.Or(row.Snippet, row.Description).String(), Position: i + 1}
	}
	return hits
}

// Page Keenable read
type document struct {
	URL     web.Text `json:"url"`
	Title   web.Text `json:"title"`
	Content web.Text `json:"content"`
}

// The page of the URL asked for, at the URL Keenable says it ended at
func (d document) page(asked string) *web.Page {
	return &web.Page{URL: cmp.Or(d.URL.String(), asked), Title: d.Title.String(), Content: d.Content.String(), Source: asked}
}
