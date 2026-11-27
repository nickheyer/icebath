package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	exaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/exa/config"
	"github.com/nickheyer/icebath/internal/web"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

const (
	name  = "exa"
	title = "Exa"
	site  = "https://exa.ai"
	// Variable of the key, as failures name it
	keyEnv     = "EXA_API_KEY"
	endpoint   = "https://api.exa.ai"
	missingKey = keyEnv + " environment variable not set. Get your API key at " + site
	// Longest a call to the API may take
	timeout = 600 * time.Second
	// Longest stretch of a body that is not JSON quoted in the failure
	quoted = 500
)

type backends struct{}

func Register(registry *web.Registry) {
	registry.Register(backends{})
}

func (backends) Backends() []web.Backend {
	return []web.Backend{{
		Name:     name,
		Title:    title,
		About:    "Exa web search and content extraction. Requires EXA_API_KEY — sign up at https://exa.ai.",
		Settings: []string{exaconfig.APIKey},
		Plans: []web.Plan{
			{
				Title: "Exa · Free (keyless)",
				Badge: "free · no key",
				About: "Semantic + neural web search with content extraction on Exa's anonymous free tier. Rate-limited under burst load.",
				Tier:  webconfig.Free,
			},
			{
				Title:  "Exa · Paid (API key)",
				Badge:  "paid",
				About:  "Semantic + neural web search with content extraction via the Exa SDK. Unthrottled, guaranteed service.",
				Tier:   webconfig.Paid,
				Key:    exaconfig.APIKey,
				Prompt: "Exa API key",
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
		Rank: 5,
	}}
}

func key(cfg *config.Config) string {
	return strings.TrimSpace(cfg.GetString(exaconfig.APIKey))
}

type searchRequest struct {
	Query      string         `json:"query"`
	Contents   searchContents `json:"contents"`
	NumResults int            `json:"numResults"`
}

type searchContents struct {
	Highlights bool `json:"highlights"`
}

type contentsRequest struct {
	URLs []string `json:"urls"`
	Text bool     `json:"text"`
}

// Search returning the passages of each result that bear on the query
func search(ctx context.Context, cfg *config.Config, query string, limit int) (*web.Results, error) {
	token := key(cfg)
	if token == "" {
		return web.Failed(missingKey), nil
	}
	body, err := post(ctx, token, "/search", searchRequest{Query: query, Contents: searchContents{Highlights: true}, NumResults: limit})
	if err != nil {
		return web.Failed(web.Failure(title, "search", err)), nil
	}
	rows, _, err := read(body, false)
	if err != nil {
		return web.Failed(web.Failure(title, "search", err)), nil
	}
	hits := make([]web.Hit, len(rows))
	for i, row := range rows {
		hits[i] = web.Hit{URL: row.URL.String(), Title: row.Title.String(), Description: strings.Join(row.Highlights, " "), Position: i + 1}
	}
	return web.Found(hits), nil
}

// Text of each page, then the pages Exa could not read as its statuses report them
func extract(ctx context.Context, cfg *config.Config, urls []string, _ string) ([]*web.Page, error) {
	token := key(cfg)
	if token == "" {
		return web.Unreadable(urls, missingKey), nil
	}
	body, err := post(ctx, token, "/contents", contentsRequest{URLs: urls, Text: true})
	if err != nil {
		return web.Unreadable(urls, web.Failure(title, "extract", err)), nil
	}
	rows, statuses, err := read(body, true)
	if err != nil {
		return web.Unreadable(urls, web.Failure(title, "extract", err)), nil
	}
	pages := make([]*web.Page, 0, len(rows)+len(statuses))
	for _, row := range rows {
		pages = append(pages, web.Document(row.URL.String(), row.Title.String(), row.Text.String()))
	}
	for _, status := range statuses {
		if status.Status == "error" {
			pages = append(pages, web.Refused(status.ID.String(), status.Error.origin(), status.Error.Tag.String(), ""))
		}
	}
	return pages, nil
}

// JSON answer of the API to a call. A failed status, or a body that is not JSON, is told as it is
func post(ctx context.Context, token, path string, payload any) ([]byte, error) {
	response, err := web.Send(ctx, web.Request{
		Method:  http.MethodPost,
		URL:     endpoint + path,
		Header:  map[string]string{"x-api-key": token, "x-exa-integration": "icebath-agent"},
		Body:    payload,
		Timeout: timeout,
	})
	if err != nil {
		return nil, err
	}
	if response.Status >= 400 {
		return nil, web.Verbatim(fmt.Sprintf("Request failed with status code %d: %s", response.Status, response.Text()))
	}
	body := web.Decoded(response.Header.Get("Content-Type"), response.Body)
	if json.Valid(body) {
		return body, nil
	}
	text := strings.Join(strings.Fields(response.Text()), " ")
	if text == "" {
		return nil, web.Verbatim(fmt.Sprintf("Expected a JSON response but received an empty body (status %d).", response.Status))
	}
	if runes := []rune(text); len(runes) > quoted {
		text = string(runes[:quoted]) + "…"
	}
	return nil, web.Verbatim(fmt.Sprintf("Expected a JSON response but received a non-JSON body (status %d): %s", response.Status, text))
}
