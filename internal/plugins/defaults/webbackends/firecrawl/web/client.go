package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Firecrawl's cloud, called when no address of a self hosted instance is set
const cloud = "https://api.firecrawl.dev"

// Longest a call to Firecrawl may take, and a scrape it is asked to give up after
const timeout = 60 * time.Second

// Firecrawl at an address and the key sent to it, none for the keyless cloud
type client struct {
	base, key string
}

// Posts JSON and returns the JSON answer. A failing status is an error naming it and the start of what Firecrawl said
func (c client) post(ctx context.Context, path string, payload any) (json.RawMessage, error) {
	header := map[string]string{}
	if c.key != "" {
		header["Authorization"] = "Bearer " + c.key
	}
	response, err := web.Send(ctx, web.Request{Method: http.MethodPost, URL: c.base + path, Header: header, Body: payload, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	if response.Status >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", response.Status, strs.Head(strings.TrimSpace(response.Text()), 300))
	}
	var answer json.RawMessage
	if err := response.Decode(&answer); err != nil {
		return nil, err
	}
	return answer, nil
}

// Search results, five when no limit is given
func (c client) search(ctx context.Context, query string, limit int) ([]web.Hit, error) {
	if limit <= 0 {
		limit = 5
	}
	answer, err := c.post(ctx, "/v2/search", map[string]any{"query": query, "limit": limit})
	if err != nil {
		return nil, err
	}
	return hits(answer), nil
}

// Page scraped in the formats given, Firecrawl asked to give up after the wait when there is one
func (c client) scrape(ctx context.Context, url string, formats []string, wait time.Duration) (scraped, metadata, error) {
	request := map[string]any{"url": url, "formats": formats}
	if wait > 0 {
		request["timeout"] = wait.Milliseconds()
	}
	answer, err := c.post(ctx, "/v2/scrape", request)
	if err != nil {
		return scraped{}, metadata{}, err
	}
	var top struct {
		Data json.RawMessage `json:"data"`
	}
	var body scraped
	if object(answer, &top) && !object(top.Data, &body) {
		object(answer, &body)
	}
	var meta metadata
	object(body.Metadata, &meta)
	return body, meta, nil
}

// Scrape answer, in its data object or at the top
type scraped struct {
	Markdown scalar          `json:"markdown"`
	HTML     scalar          `json:"html"`
	Metadata json.RawMessage `json:"metadata"`
}

// What Firecrawl tells of a scraped page
type metadata struct {
	Title scalar `json:"title"`
	// Where the page ended up after redirects
	URL scalar `json:"url"`
	// URL asked for, in either spelling
	SourceURL   scalar `json:"sourceURL"`
	SourceSnake scalar `json:"source_url"`
	// The page's own HTTP status in either spelling, and why it failed
	StatusCode  any      `json:"statusCode"`
	StatusSnake any      `json:"status_code"`
	Error       web.Text `json:"error"`
}

// Status of the page: statusCode when it is usable, else status_code
func (m metadata) status() any {
	if _, ok := web.Status(m.StatusCode); ok {
		return m.StatusCode
	}
	return m.StatusSnake
}

// Search results of the v1 answer, a data list, or of the v2 answer, a web or results list in data or at the top
func hits(answer json.RawMessage) []web.Hit {
	var top struct {
		Data    json.RawMessage `json:"data"`
		Web     json.RawMessage `json:"web"`
		Results json.RawMessage `json:"results"`
	}
	if !object(answer, &top) {
		return nil
	}
	if list(top.Data) {
		return rows(top.Data)
	}
	candidates := []json.RawMessage{top.Web, top.Results}
	var data struct {
		Web     json.RawMessage `json:"web"`
		Results json.RawMessage `json:"results"`
	}
	if object(top.Data, &data) {
		candidates = append([]json.RawMessage{data.Web, data.Results}, candidates...)
	}
	for _, candidate := range candidates {
		if found := rows(candidate); len(found) > 0 {
			return found
		}
	}
	return nil
}

// Results of a list, entries that are no objects left out
func rows(raw json.RawMessage) []web.Hit {
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	var found []web.Hit
	for _, entry := range entries {
		var row struct {
			URL         scalar `json:"url"`
			Title       scalar `json:"title"`
			Description scalar `json:"description"`
			Position    any    `json:"position"`
		}
		if object(entry, &row) {
			found = append(found, web.Hit{Title: string(row.Title), URL: string(row.URL), Description: string(row.Description), Position: position(row.Position)})
		}
	}
	return found
}

// Rank Firecrawl gives a result as a whole number, a fraction cut toward zero; zero when it gives none
func position(value any) int {
	switch v := value.(type) {
	case float64:
		if math.Abs(v) < math.MaxInt64 {
			return int(v)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return 0
}

// Whether the JSON value is an object, read into v when it is
func object(raw json.RawMessage, v any) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{' && json.Unmarshal(raw, v) == nil
}

func list(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '['
}

// JSON scalar read as text, a string as it is and a number or boolean as written; an array, object or null as nothing
type scalar string

func (s *scalar) UnmarshalJSON(data []byte) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	switch v := value.(type) {
	case string:
		*s = scalar(v)
	case float64, bool:
		*s = scalar(bytes.TrimSpace(data))
	default:
		*s = ""
	}
	return nil
}
