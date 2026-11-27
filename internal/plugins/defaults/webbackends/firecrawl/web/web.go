package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	firecrawlconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/firecrawl/config"
	"github.com/nickheyer/icebath/internal/web"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

const (
	name  = "firecrawl"
	title = "Firecrawl"
	// Where the free plan's failures send the model for a key
	site = "https://firecrawl.dev"
)

const (
	unconfigured = "Web tools are not configured. Set FIRECRAWL_API_KEY for cloud Firecrawl or set FIRECRAWL_API_URL for a self-hosted Firecrawl instance."
	timedOut     = "Scrape timed out after 60s — page may be too large or unresponsive. Try browser_exec instead."
)

type backends struct{}

func Register(registry *web.Registry) {
	registry.Register(backends{})
}

func (backends) Backends() []web.Backend {
	return []web.Backend{{
		Name:     name,
		Title:    title,
		About:    "Firecrawl web search + content extraction. Supports keyless cloud and the direct API.",
		Settings: []string{firecrawlconfig.APIKey, firecrawlconfig.APIURL},
		Plans: []web.Plan{
			{
				Title:  "Firecrawl",
				Badge:  "keyless/paid · optional self-hosted",
				About:  "Full search + extract; supports keyless cloud, direct API, and self-hosted Firecrawl.",
				Key:    firecrawlconfig.APIKey,
				Prompt: "Firecrawl API key (optional; blank = keyless cloud or self-hosted)",
				Link:   "https://docs.firecrawl.dev/introduction",
			},
			{
				Title:  "Firecrawl Self-Hosted",
				Badge:  "free · self-hosted",
				About:  "Run your own Firecrawl instance (Docker)",
				Key:    firecrawlconfig.APIURL,
				Prompt: "Your Firecrawl instance URL (e.g., http://localhost:3002)",
			},
		},
		Search:  search,
		Extract: extract,
		Ready: func(cfg *config.Config) bool {
			return key(cfg) != "" || address(cfg) != "" || web.Picked(cfg, name)
		},
		Free:     &web.Free{Search: freeSearch, Extract: freeExtract, Used: keyless, Raises: true},
		Parser:   &web.Parser{Ready: func(cfg *config.Config) bool { return key(cfg) != "" }, Parse: parse},
		Rank:     1,
		Fallback: true,
	}}
}

func key(cfg *config.Config) string {
	return strings.TrimSpace(cfg.GetString(firecrawlconfig.APIKey))
}

// Address of a self hosted instance, empty for the cloud
func address(cfg *config.Config) string {
	return strings.TrimRight(strings.TrimSpace(cfg.GetString(firecrawlconfig.APIURL)), "/")
}

// Whether calls go to the free ring: neither a key nor an address is set, and the free plan applies
func keyless(cfg *config.Config) bool {
	return key(cfg) == "" && address(cfg) == "" && web.Keyless(cfg, name, "")
}

// Firecrawl the calls for the capability go to: the key or address set, else the keyless cloud when Firecrawl is chosen by name. When
// neither applies, the error says what to set, naming the backend chosen for the capability when there is one
func resolve(cfg *config.Config, pin string) (client, error) {
	if key, address := key(cfg), address(cfg); key != "" || address != "" {
		return client{base: cmp.Or(address, cloud), key: key}, nil
	}
	if web.Picked(cfg, name) {
		return client{base: cloud}, nil
	}
	if chosen := cmp.Or(web.Chosen(cfg, pin), web.Chosen(cfg, webconfig.Backend)); chosen != "" {
		return client{}, fmt.Errorf("web is configured to use %s, but neither %s nor %s is set. Pick another one with /web.",
			chosen, firecrawlconfig.APIKeyEnv, firecrawlconfig.APIURLEnv)
	}
	return client{}, errors.New(unconfigured)
}

func search(ctx context.Context, cfg *config.Config, query string, limit int) (*web.Results, error) {
	c, err := resolve(cfg, webconfig.SearchBackend)
	if err != nil {
		return nil, err
	}
	hits, err := c.search(ctx, query, limit)
	if err != nil {
		return web.Failed(web.Failure(title, "search", err)), nil
	}
	return web.Found(hits), nil
}

// Scrapes the pages one at a time in the format asked for, markdown and HTML when none is. Every failure is the page's error
func extract(ctx context.Context, cfg *config.Config, urls []string, format string) ([]*web.Page, error) {
	formats := []string{"markdown", "html"}
	if format == "markdown" || format == "html" {
		formats = []string{format}
	}
	c, err := resolve(cfg, webconfig.ExtractBackend)
	pages := make([]*web.Page, len(urls))
	for i, url := range urls {
		if ctx.Err() != nil {
			pages[i] = web.Unread(url, "Interrupted")
			continue
		}
		if block := web.Blocked(cfg, url); block != nil {
			pages[i] = &web.Page{URL: url, Error: block.Message, Blocked: block}
			continue
		}
		if err != nil {
			pages[i] = web.Unread(url, err.Error())
			continue
		}
		pages[i] = c.read(ctx, cfg, url, formats, format)
	}
	return pages, nil
}

// Scrapes one page within the scrape timeout. The page it ended at after redirects is refused when it is private or on the website
// blocklist, and marked failed when its own answer was 400 or above
func (c client) read(ctx context.Context, cfg *config.Config, url string, formats []string, format string) *web.Page {
	run, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, meta, err := c.scrape(run, url, formats, timeout)
	if err != nil {
		if errors.Is(run.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return web.Unread(url, timedOut)
		}
		return web.Unread(url, err.Error())
	}
	final, heading := cmp.Or(string(meta.URL), url), string(meta.Title)
	if !web.SafeURL(cfg, final) {
		return &web.Page{URL: final, Title: heading, Error: web.BlockedPrivate}
	}
	if block := web.Blocked(cfg, final); block != nil {
		return &web.Page{URL: final, Title: heading, Error: block.Message, Blocked: block}
	}
	content := string(body.Markdown)
	switch {
	case format == "markdown", format == "" && content != "":
	case body.HTML != "":
		content = string(body.HTML)
	}
	page := &web.Page{URL: final, Title: heading, Content: content, Source: cmp.Or(string(meta.SourceURL), string(meta.SourceSnake), url)}
	return page.WithStatus(meta.status(), meta.Error.String())
}

// Anonymous search on the cloud
func freeSearch(ctx context.Context, query string, limit int) (*web.Results, error) {
	hits, err := client{base: cloud}.search(ctx, query, limit)
	if err != nil {
		return web.Failed(web.FreeFailure(title, "search", err, firecrawlconfig.APIKeyEnv, site, true)), nil
	}
	return web.Found(hits), nil
}

// Anonymous scrape of each page as markdown on the cloud, reported at the URL it ended at after redirects
func freeExtract(ctx context.Context, urls []string) ([]*web.Page, error) {
	c := client{base: cloud}
	pages := make([]*web.Page, len(urls))
	for i, url := range urls {
		body, meta, err := c.scrape(ctx, url, []string{"markdown"}, 0)
		if err != nil {
			pages[i] = web.Unread(url, web.FreeFailure(title, "extract", err, firecrawlconfig.APIKeyEnv, site, false))
			continue
		}
		page := &web.Page{URL: cmp.Or(string(meta.URL), url), Title: string(meta.Title), Content: cmp.Or(string(body.Markdown), string(body.HTML)), Source: url}
		pages[i] = page.WithStatus(meta.status(), meta.Error.String())
	}
	return pages, nil
}
