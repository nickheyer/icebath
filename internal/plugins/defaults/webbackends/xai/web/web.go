package web

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/xai/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/web"
)

const (
	// Most domains one filter may name
	maxDomains = 5
	// Characters of the answer before a citation kept as its description
	lead = 200
)

// Braced block of an answer a reasoning model wrapped in prose
var braced = regexp.MustCompile(`\{[\s\S]*\}`)

type backends struct {
	providers *provider.Registry
	creds     *credentials.Registry
}

func Register(registry *web.Registry, providers *provider.Registry, creds *credentials.Registry) {
	registry.Register(backends{providers, creds})
}

func (b backends) Backends() []web.Backend {
	return []web.Backend{{
		Name:     "xai",
		Title:    "xAI Web Search (Grok)",
		About:    "xAI Web Search — search the web via Grok's agentic web_search tool (Responses API). Requires xAI Grok OAuth or XAI_API_KEY (https://x.ai), connected with /providers.",
		Settings: []string{xaiconfig.Model, xaiconfig.Timeout, xaiconfig.AllowedDomains, xaiconfig.ExcludedDomains},
		Plans: []web.Plan{{
			Title:  "xAI Web Search (Grok)",
			Badge:  "paid",
			About:  "Agentic web search via Grok's web_search tool — uses xAI Grok OAuth or XAI_API_KEY.",
			Prompt: "Uses the xAI Grok OAuth sign in or the xAI API key: connect either with /providers",
			Link:   "https://console.x.ai/",
		}},
		Search:   b.search,
		Ready:    func(cfg *config.Config) bool { _, ok := b.bearer(cfg); return ok },
		Explicit: true,
	}}
}

// Bearer of a search, the Grok sign in first
func (b backends) bearer(cfg *config.Config) (web.XAI, bool) {
	return web.XAIBearer(b.providers, b.creds, cfg, false)
}

type request struct {
	Model   string      `json:"model"`
	Input   []message   `json:"input"`
	Tools   []webSearch `json:"tools"`
	Include []string    `json:"include"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type webSearch struct {
	Type    string   `json:"type"`
	Filters *filters `json:"filters,omitempty"`
}

type filters struct {
	AllowedDomains  []string `json:"allowed_domains,omitempty"`
	ExcludedDomains []string `json:"excluded_domains,omitempty"`
}

// Reply of the Responses API. Parts of another shape hold nothing a search reads and are left empty
type reply struct {
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type        string       `json:"type"`
			Text        string       `json:"text"`
			Annotations []annotation `json:"annotations"`
		} `json:"content"`
	} `json:"output"`
	Citations []string `json:"citations"`
	// Set only when the error is an object
	Error map[string]web.Text `json:"error"`
}

type annotation struct {
	Type       string          `json:"type"`
	URL        web.Text        `json:"url"`
	StartIndex json.RawMessage `json:"start_index"`
	EndIndex   json.RawMessage `json:"end_index"`
}

// Asks Grok to search with its web_search tool and to answer with the results as JSON
func (b backends) search(ctx context.Context, cfg *config.Config, query string, limit int) (*web.Results, error) {
	bearer, ok := b.bearer(cfg)
	if !ok {
		return web.Failed("No xAI credentials found. Connect xAI or xAI Grok OAuth with /providers, or set XAI_API_KEY."), nil
	}
	limit = max(1, min(limit, 100))
	tool, ok := filtered(cfg)
	if !ok {
		return web.Failed("web.xai.allowed_domains and web.xai.excluded_domains cannot both be set (xAI restriction)."), nil
	}
	call := web.Request{
		Method: http.MethodPost,
		URL:    bearer.BaseURL + "/responses",
		Header: map[string]string{"Authorization": "Bearer " + bearer.Key, "User-Agent": provider.UserAgent("Icebath-Agent")},
		Body: request{
			Model:   cmp.Or(strings.TrimSpace(cfg.GetString(xaiconfig.Model)), xaiconfig.DefaultModel),
			Input:   []message{{Role: "user", Content: prompt(query, limit)}},
			Tools:   []webSearch{tool},
			Include: []string{"no_inline_citations"},
		},
		Timeout: time.Duration(max(cfg.GetInt(xaiconfig.Timeout), 0)) * time.Second,
	}
	response, err := web.Send(ctx, call)
	// A Grok sign in refused once is renewed and tried again
	if err == nil && response.Status == http.StatusUnauthorized && bearer.Source == web.XAIOAuth && web.RenewXAI(ctx, b.providers, b.creds, cfg) == nil {
		if fresh, ok := b.bearer(cfg); ok && fresh.Key != bearer.Key {
			call.Header["Authorization"] = "Bearer " + fresh.Key
			response, err = web.Send(ctx, call)
		}
	}
	if err != nil {
		return web.Failed("Could not reach xAI: " + err.Error()), nil
	}
	if !response.OK() {
		body := []rune(response.Text())
		message := fmt.Sprintf("xAI web search returned HTTP %d: %s", response.Status, string(body[:min(len(body), 300)]))
		return web.Failed(strings.TrimRightFunc(message, unicode.IsSpace)), nil
	}
	text := web.Decoded(response.Header.Get("Content-Type"), response.Body)
	var answer reply
	if _, invalid := errors.AsType[*json.SyntaxError](json.Unmarshal(text, &answer)); invalid {
		return web.Failed("Could not parse xAI Responses API reply as JSON"), nil
	}
	if !bytes.HasPrefix(bytes.TrimSpace(text), []byte("{")) {
		return nil, errors.New("The xAI Responses API reply is not a JSON object.")
	}
	if answer.Error != nil {
		return web.Failed("xAI returned an error: " + cmp.Or(answer.Error["message"].String(), answer.Error["code"].String(), "unknown error")), nil
	}
	return web.Found(results(answer, limit)), nil
}

// web_search with the domain filters of the settings, false when both kinds are set, which xAI refuses
func filtered(cfg *config.Config) (webSearch, bool) {
	allowed, excluded := domains(cfg, xaiconfig.AllowedDomains), domains(cfg, xaiconfig.ExcludedDomains)
	tool := webSearch{Type: "web_search"}
	switch {
	case len(allowed) > 0 && len(excluded) > 0:
		return tool, false
	case len(allowed) > 0 || len(excluded) > 0:
		tool.Filters = &filters{AllowedDomains: allowed, ExcludedDomains: excluded}
	}
	return tool, true
}

// Domains a filter setting names, blank ones left out, at most five
func domains(cfg *config.Config, key string) []string {
	var out []string
	for _, domain := range cfg.GetStringSlice(key) {
		if domain = strings.TrimSpace(domain); domain != "" {
			out = append(out, domain)
		}
	}
	return out[:min(len(out), maxDomains)]
}

// Asks for one JSON object of results and nothing else
func prompt(query string, limit int) string {
	return "Use the web_search tool to find current information for the query below, then respond with ONLY a single " +
		"JSON object — no prose, no markdown fences, no inline citation links — matching this exact schema:\n\n" +
		`{"results": [{"title": "string", "url": "string", "description": "1-2 sentence summary"}]}` + "\n\n" +
		fmt.Sprintf("Return at most %d results, ordered by relevance, with absolute https:// URLs. If no usable results exist, return ", limit) +
		`{"results": []}.` + "\n\nQuery: " + query
}

// Rows of the reply: the JSON object its text answers with, else its url_citation annotations, else its list of citations
func results(answer reply, limit int) []web.Hit {
	var texts []string
	var annotations []annotation
	for _, item := range answer.Output {
		if item.Type != "message" {
			continue
		}
		for _, content := range item.Content {
			if content.Type != "output_text" {
				continue
			}
			if strings.TrimSpace(content.Text) != "" {
				texts = append(texts, content.Text)
			}
			annotations = append(annotations, content.Annotations...)
		}
	}
	for _, text := range texts {
		if hits := parsed(text, limit); len(hits) > 0 {
			return hits
		}
	}
	if hits := cited(annotations, strings.Join(texts, "\n"), limit); len(hits) > 0 {
		return hits
	}
	return listed(answer.Citations, limit)
}

// Rows of the JSON object with a results list that the text is, or holds within prose
func parsed(text string, limit int) []web.Hit {
	candidates := []string{text}
	if block := braced.FindString(text); block != "" && block != text {
		candidates = append(candidates, block)
	}
	for _, candidate := range candidates {
		var answer struct {
			Results []struct {
				Title       web.Text `json:"title"`
				URL         web.Text `json:"url"`
				Description web.Text `json:"description"`
			} `json:"results"`
		}
		// Text of another shape holds no rows
		json.Unmarshal([]byte(candidate), &answer)
		var hits []web.Hit
		for _, row := range answer.Results[:min(limit, len(answer.Results))] {
			if url := strings.TrimSpace(row.URL.String()); url != "" {
				hits = append(hits, web.Hit{Title: strings.TrimSpace(row.Title.String()), URL: url,
					Description: strings.TrimSpace(row.Description.String()), Position: len(hits) + 1})
			}
		}
		if len(hits) > 0 {
			return hits
		}
	}
	return nil
}

// Rows of the url_citation annotations, each URL once, described by the text before its citation
func cited(annotations []annotation, text string, limit int) []web.Hit {
	runes := []rune(text)
	seen := map[string]bool{}
	var hits []web.Hit
	for _, note := range annotations {
		url := strings.TrimSpace(note.URL.String())
		if note.Type != "url_citation" || url == "" || seen[url] {
			continue
		}
		seen[url] = true
		description := ""
		start, startSet := whole(note.StartIndex)
		end, endSet := whole(note.EndIndex)
		if startSet && endSet && 0 <= start && start < end && end <= len(runes) {
			description = strings.TrimSpace(string(runes[max(0, start-lead):start]))
		}
		hits = append(hits, web.Hit{URL: url, Description: description, Position: len(hits) + 1})
		if len(hits) >= limit {
			break
		}
	}
	return hits
}

// Rows of the reply's list of cited URLs, numbered by their place in it
func listed(citations []string, limit int) []web.Hit {
	var hits []web.Hit
	for i, url := range citations[:min(limit, len(citations))] {
		if url = strings.TrimSpace(url); url != "" {
			hits = append(hits, web.Hit{URL: url, Position: i + 1})
		}
	}
	return hits
}

// Whole number a JSON value is, false for any other value
func whole(raw json.RawMessage) (int, bool) {
	n, err := strconv.Atoi(string(raw))
	return n, err == nil
}
