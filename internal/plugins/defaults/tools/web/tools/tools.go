package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/web/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/tools/document"
	"github.com/nickheyer/icebath/internal/web"
)

const toolset = "web"

// Most URLs one web_extract call reads
const maxURLs = 5

type registrant struct {
	backends *web.Registry
	memo     *memo
	// Held while the index of the page cache is read and written
	index sync.Mutex
	debug *debug
	// Reads PDF and office documents a page turns out to be
	documents *document.Reader
	// Where the agent's commands run, which may see a saved page at a path of their own
	terminal *terminal.Terminal
}

func Register(registry *tools.Registry, backends *web.Registry, host *terminal.Terminal) {
	registry.Register(&registrant{backends: backends, memo: newMemo(), debug: newDebug(), documents: document.New(), terminal: host})
}

func (r *registrant) Tools() []tools.Def {
	return []tools.Def{
		{
			Name:    "web_search",
			Toolset: toolset,
			Title:   "Web search",
			Icon:    tools.IconSearch,
			Subject: "query",
			Schema: tools.Schema(
				`Search the web: returns titles, URLs and snippets (5 by default). Read pages with web_extract. The query is passed through to the configured backend, so operators such as site:domain, filetype:pdf, intitle:word, -term, and "exact phrase" may work when the backend supports them.`,
				[]string{"query"},
				map[string]any{
					"query": tools.Property("string", `The search query to look up on the web. You may include backend-supported operators such as site:example.com, filetype:pdf, intitle:word, -term, or "exact phrase".`),
					"limit": map[string]any{"type": "integer", "description": "Maximum number of results to return. Defaults to 5.", "minimum": 1, "maximum": 100, "default": 5},
				},
			),
			Handler:        r.search,
			Check:          r.available,
			MaxResultChars: 100_000,
			// Reads pages of the web, which hold what anyone wrote, and keeps no state of the session
			Effect:    tools.ReadOnly,
			Parallel:  true,
			Untrusted: true,
		},
		{
			Name:    "web_extract",
			Toolset: toolset,
			Title:   "Read web page",
			Icon:    tools.IconFetch,
			Schema: tools.Schema(
				"Extract content from web page URLs. Returns clean page content in markdown/text (no LLM summarization — fast). Also works with PDF URLs (arxiv papers, documents) — pass the PDF link directly. Pages within the char budget (default 15000) return whole; larger pages return a head+tail window with a footer telling you the full text's saved file path and the read_file call to page through the omitted middle. Inline images appear as [IMAGE: alt] placeholders; real image URLs are kept as links. If a URL fails, times out, or needs JS, login or clicks, use a browser tool instead.",
				[]string{"urls"},
				map[string]any{
					"urls":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "List of URLs to extract content from (max 5 URLs per call)", "maxItems": maxURLs},
					"char_limit": map[string]any{"type": "integer", "description": "Optional per-page character budget sent back (default 15000). Pages larger than this are head+tail truncated with the full text stored to disk. Raise it when you need more of a long page inline.", "minimum": webconfig.MinCharLimit},
				},
			),
			Handler:        r.extract,
			Check:          r.available,
			MaxResultChars: 100_000,
			// A page that won't load is ordinary output
			Effect:    tools.ReadOnly,
			Parallel:  true,
			Untrusted: true,
			Fallible:  true,
		},
	}
}

// Whether some backend can answer for the session's profile
func (r *registrant) available(env *tools.Env) bool {
	return r.backends.Available(env.Config)
}

func (r *registrant) search(ctx context.Context, call *tools.Call) string {
	cfg := call.Env.Config
	query := text(call.Args["query"])
	limit := 5
	if value, ok := call.Args["limit"]; ok && value != nil {
		if n, ok := whole(value); ok {
			limit = min(max(n, 1), 100)
		}
	}
	record := map[string]any{"parameters": map[string]any{"query": query, "limit": limit}, "error": nil, "results_count": 0, "original_response_size": 0, "final_response_size": 0}
	if ctx.Err() != nil {
		return failure("Interrupted", map[string]any{"success": false})
	}
	backend, err := r.backends.Serving(cfg, web.Searching)
	if err != nil {
		result := indented(web.Failed(err.Error()))
		record["final_response_size"] = utf8.RuneCountInString(result)
		r.debug.log(cfg, "web_search_tool", record)
		return result
	}
	results, err := r.answer(ctx, call, backend, query, limit)
	if err != nil {
		message := "Error searching web: " + err.Error()
		record["error"] = message
		r.debug.log(cfg, "web_search_tool", record)
		return failure(message, nil)
	}
	if results.Data != nil {
		record["results_count"] = len(results.Data.Web)
	}
	result := indented(results)
	record["final_response_size"] = utf8.RuneCountInString(result)
	r.debug.log(cfg, "web_search_tool", record)
	return result
}

func (r *registrant) extract(ctx context.Context, call *tools.Call) string {
	cfg := call.Env.Config
	items, _ := call.Args["urls"].([]any)
	items = items[:min(len(items), maxURLs)]
	asked, blocked := validate(items)
	if blocked {
		return tools.JSON(map[string]any{"success": false, "error": "Blocked: URL contains what appears to be an API key or token. Secrets must not be sent in URLs."})
	}
	charLimit := r.charLimit(cfg, call.Args["char_limit"])
	record := map[string]any{"parameters": map[string]any{"urls": asked.urls, "format": "markdown", "char_limit": call.Args["char_limit"]}, "error": nil,
		"pages_extracted": 0, "pages_truncated": 0, "original_response_size": 0, "final_response_size": 0, "truncation_metrics": []any{}, "processing_applied": []any{}}
	fixed := asked.invalid
	var safe []string
	var positions []int
	for i, url := range asked.urls {
		if web.SafeURL(cfg, url) {
			safe = append(safe, url)
			positions = append(positions, asked.positions[i])
		} else {
			fixed[asked.positions[i]] = web.Unread(url, web.BlockedPrivate)
		}
	}
	var pages []*web.Page
	if len(safe) > 0 {
		backend, err := r.backends.Serving(cfg, web.Reading)
		if err != nil {
			return tools.JSON(web.Failed(err.Error()))
		}
		if pages, err = r.pages(ctx, call, backend, safe, "markdown"); err != nil {
			message := "Error extracting content: " + err.Error()
			record["error"] = message
			r.debug.log(cfg, "web_extract_tool", record)
			return failure(message, nil)
		}
	}
	if len(fixed) > 0 {
		pages = merge(len(items), fixed, positions, safe, pages)
	}
	record["pages_extracted"] = len(pages)
	record["original_response_size"] = len(tools.JSON(map[string]any{"results": rows(pages)}))
	record["processing_applied"] = []any{"truncate_and_store"}
	r.truncate(cfg, pages, charLimit, record)
	result := failure("Content was inaccessible or not found", nil)
	if trimmed := rows(pages); len(trimmed) > 0 {
		result = indented(map[string]any{"results": trimmed})
	}
	result = images(result)
	record["final_response_size"] = utf8.RuneCountInString(result)
	record["processing_applied"] = []any{"truncate_and_store", "base64_image_conversion"}
	r.debug.log(cfg, "web_extract_tool", record)
	return result
}

// URLs of the items in the form a request sends, with the position each came at and an error page for each item that is no URL
type wanted struct {
	urls      []string
	positions []int
	invalid   map[int]*web.Page
}

// Reads the items as URLs, refusing the whole call when one carries what looks like a secret
func validate(items []any) (wanted, bool) {
	out := wanted{invalid: map[int]*web.Page{}}
	for i, item := range items {
		url := itemURL(item)
		if url == "" {
			out.invalid[i] = web.Unread("", "Invalid URL item at index "+strconv.Itoa(i)+": expected a URL string or an object with a string 'url' or 'href' field")
			continue
		}
		normalized := web.NormalizeURL(url)
		for _, candidate := range []string{url, unescape(url), normalized, unescape(normalized)} {
			if web.CarriesSecret(candidate) {
				return wanted{}, true
			}
		}
		out.urls = append(out.urls, normalized)
		out.positions = append(out.positions, i)
	}
	return out, false
}

// URL of an item: a string, or an object with a url or else an href
func itemURL(item any) string {
	if object, ok := item.(map[string]any); ok {
		item = object["url"]
		if !config.Truthy(item) {
			item = object["href"]
		}
	}
	url, _ := item.(string)
	return strings.TrimSpace(url)
}

// Pages in the order asked: the fixed ones by position, the read ones at the positions they were asked from, a page the backend gave
// nothing for marked so
func merge(total int, fixed map[int]*web.Page, positions []int, urls []string, read []*web.Page) []*web.Page {
	out := make([]*web.Page, total)
	for position, page := range fixed {
		out[position] = page
	}
	for i, position := range positions {
		if i < len(read) {
			out[position] = read[i]
		} else {
			out[position] = web.Unread(urls[i], "Extract backend returned no result for this URL")
		}
	}
	return out
}

// Rows of the pages as the model reads them
func rows(pages []*web.Page) []any {
	out := make([]any, 0, len(pages))
	for _, page := range pages {
		if page == nil {
			continue
		}
		row := map[string]any{"url": page.URL, "title": page.Title, "content": page.Content, "error": nil}
		if page.Error != "" {
			row["error"] = page.Error
		}
		if page.Blocked != nil {
			row["blocked_by_policy"] = map[string]any{"host": page.Blocked.Host, "rule": page.Blocked.Rule, "source": page.Blocked.Source}
		}
		out = append(out, row)
	}
	return out
}

// Per page character budget: the argument within its bounds when it is a whole number, else the setting within them
func (r *registrant) charLimit(cfg *config.Config, value any) int {
	if n, ok := whole(value); ok {
		return min(max(n, webconfig.MinCharLimit), webconfig.MaxCharLimit)
	}
	return min(max(cfg.GetInt(webconfig.ExtractCharLimit), webconfig.MinCharLimit), webconfig.MaxCharLimit)
}

var wholeText = regexp.MustCompile(`^[+-]?\d+$`)

// Whole number of an argument: an integer, a number cut to one, a boolean as 0 or 1, or decimal text; false for anything else
func whole(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return int(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case string:
		if text := strings.TrimSpace(v); wholeText.MatchString(text) {
			n, err := strconv.Atoi(text)
			return n, err == nil
		}
	}
	return 0, false
}

// Text of an argument: a string as it is, a number or boolean as written, anything else empty
func text(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// Error result the model reads, the message cut to its bound, then the other fields
func failure(message string, extra map[string]any) string {
	fields := map[string]any{"error": tools.BoundError(message)}
	for key, value := range extra {
		fields[key] = value
	}
	return tools.JSON(fields)
}

// JSON indented by two spaces, text left unescaped
func indented(value any) string {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return tools.ErrorResult(err)
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// Percent escapes decoded, a malformed one left as written
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && hexDigit(s[i+1]) && hexDigit(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func hexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	}
	return c - 'a' + 10
}
