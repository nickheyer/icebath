package tools

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	xaiWebDefaultModel   = "grok-build-0.1"
	xaiWebDefaultTimeout = 90
	xaiMaxDomainFilters  = 5
)

// Tolerates prose around the JSON block reasoning models sometimes narrate
var xaiJSONBlockRe = regexp.MustCompile(`\{[\s\S]*\}`)

// The web.xai mapping of the config, empty on a miss
func loadXAIWebConfig(env *Env) map[string]any {
	m, _ := webCfg(env).Get("xai").(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// Stripped non empty string entries of a list capped at the xAI filter limit
func coerceDomainList(value any) []any {
	list, ok := value.([]any)
	if !ok {
		return []any{}
	}
	out := []any{}
	for _, item := range list {
		if s, ok := item.(string); ok && pystr.Strip(s) != "" {
			out = append(out, pystr.Strip(s))
		}
	}
	if len(out) > xaiMaxDomainFilters {
		out = out[:xaiMaxDomainFilters]
	}
	return out
}

// float(value) or the default on its TypeError or ValueError, an OverflowError raised
func coerceFloat(value any, def float64) (float64, error) {
	f, err := pyint.Float(value)
	if err == nil {
		return f, nil
	}
	var pe *pyint.Error
	if errors.As(err, &pe) && pe.Kind == "OverflowError" {
		return 0, err
	}
	return def, nil
}

// web_search tool spec with optional domain filters, nil when both filter kinds are set
func xaiWebSearchTool(cfg map[string]any) *pyjson.Object {
	filters := pyjson.NewObject()
	for _, k := range []string{"allowed_domains", "excluded_domains"} {
		if v := coerceDomainList(cfg[k]); len(v) > 0 {
			filters.Set(k, v)
		}
	}
	if filters.Len() == 2 {
		return nil
	}
	tool := pyjson.NewObject()
	tool.Set("type", "web_search")
	if filters.Len() > 0 {
		tool.Set("filters", filters)
	}
	return tool
}

// Prompt asking Grok for one JSON object of results and nothing else
func xaiBuildPrompt(query string, limit int) string {
	return "Use the web_search tool to find current information for the query below, then respond with ONLY a single " +
		"JSON object — no prose, no markdown fences, no inline citation links — matching this exact schema:\n\n" +
		`{"results": [{"title": "string", "url": "string", "description": "1-2 sentence summary"}]}` + "\n\n" +
		fmt.Sprintf("Return at most %d results, ordered by relevance, with absolute https:// URLs. If no usable results exist, return ", limit) +
		`{"results": []}.` + "\n\n" +
		"Query: " + query
}

// POST /responses with one forced OAuth refresh and retry on a first 401, failures as search envelopes
func xaiPostResponses(ctx context.Context, env *Env, baseURL string, payload *pyjson.Object, apiKey string, timeout float64, isOAuthPath bool) (any, *WebSearchResponse, error) {
	headers := [][2]string{{"Authorization", "Bearer " + apiKey}, {"Content-Type", "application/json"}, {"User-Agent", icebathXAIUserAgent()}}
	limit := time.Duration(timeout * float64(time.Second))
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := httpxSend(ctx, http.MethodPost, baseURL+"/responses", nil, headers, payload, limit)
		if err != nil {
			var he *httpxError
			if !errors.As(err, &he) {
				return nil, nil, err
			}
			envLog(env).Warn("xAI web search request error", "error", err.Error())
			return nil, searchFail("Could not reach xAI: " + err.Error()), nil
		}
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			data, err := resp.JSON()
			if err != nil {
				envLog(env).Warn("xAI web search bad JSON", "error", err.Error())
				return nil, searchFail("Could not parse xAI Responses API reply as JSON"), nil
			}
			return data, nil, nil
		}
		if resp.StatusCode == 401 && attempt == 0 && isOAuthPath {
			envLog(env).Info("xAI web search got 401 on first attempt; forcing OAuth refresh and retrying once.")
			newKey := pystr.Strip(resolveXAIHTTPCredentialsRefreshed(env, apiKey).APIKey)
			if newKey != "" && newKey != apiKey {
				apiKey, headers[0][1] = newKey, "Bearer "+newKey
				continue
			}
		}
		body := runePrefix(resp.Text(), 300)
		envLog(env).Warn("xAI web search HTTP error", "status", resp.StatusCode, "body", body)
		return nil, searchFail(pystr.Rstrip(fmt.Sprintf("xAI web search returned HTTP %d: %s", resp.StatusCode, body))), nil
	}
	return nil, searchFail("xAI web search produced no response"), nil
}

// Text blocks and annotation dicts of the output_text chunks of every message item
func xaiCollectOutputText(data any) ([]string, []*pyjson.Object, error) {
	output, err := pyDictGet(data, "output")
	if err != nil {
		return nil, nil, err
	}
	items, _ := output.([]any)
	var chunks []*pyjson.Object
	for _, item := range items {
		d, ok := pyDict(item)
		if !ok {
			continue
		}
		kind, _ := d.Get("type")
		content, _ := d.Get("content")
		list, isList := content.([]any)
		if kind != "message" || !isList {
			continue
		}
		for _, chunk := range list {
			if c, ok := pyDict(chunk); ok {
				if ct, _ := c.Get("type"); ct == "output_text" {
					chunks = append(chunks, c)
				}
			}
		}
	}
	var texts []string
	var annotations []*pyjson.Object
	for _, c := range chunks {
		if t, ok := c.Get("text"); ok {
			if s, isStr := t.(string); isStr && pystr.Strip(s) != "" {
				texts = append(texts, s)
			}
		}
	}
	for _, c := range chunks {
		if anns, ok := c.Get("annotations"); ok {
			if list, isList := anns.([]any); isList {
				for _, a := range list {
					if d, ok := pyDict(a); ok {
						annotations = append(annotations, d)
					}
				}
			}
		}
	}
	return texts, annotations, nil
}

// str(row.get(key, "")).strip() of a decoded row
func xaiRowField(row *pyjson.Object, key string) string {
	v, ok := row.Get(key)
	if !ok {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Rows of a JSON object with a results array in the text or its braced block, nil when absent
func xaiTryParseJSONResults(text string, limit int) []WebSearchHit {
	candidates := []string{text}
	if m := xaiJSONBlockRe.FindString(text); m != "" && m != text {
		candidates = append(candidates, m)
	}
	for _, candidate := range candidates {
		parsed, err := pyjson.Loads(candidate)
		if err != nil {
			continue
		}
		d, ok := pyDict(parsed)
		if !ok {
			continue
		}
		raw, _ := d.Get("results")
		results, ok := raw.([]any)
		if !ok {
			continue
		}
		head, _ := pySeqHead(results, limit)
		var normalized []WebSearchHit
		for _, r := range head.([]any) {
			row, ok := pyDict(r)
			if !ok {
				continue
			}
			if u := xaiRowField(row, "url"); u != "" {
				normalized = append(normalized, titleHit(xaiRowField(row, "title"), u, xaiRowField(row, "description"), len(normalized)+1))
			}
		}
		if len(normalized) > 0 {
			return normalized
		}
	}
	return nil
}

// Python int value of a decoded number, bools included as isinstance int accepts them
func pyBigIntValue(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	case int:
		return big.NewInt(int64(x)), true
	case int64:
		return big.NewInt(x), true
	case *big.Int:
		return x, true
	}
	return nil, false
}

// Rows from url_citation annotations with up to 200 characters of preceding text as the description
func xaiResultsFromAnnotations(annotations []*pyjson.Object, joined string, limit int) []WebSearchHit {
	seen := map[string]bool{}
	runes := []rune(joined)
	var results []WebSearchHit
	for _, ann := range annotations {
		u := ""
		if kind, _ := ann.Get("type"); kind == "url_citation" {
			u = xaiRowField(ann, "url")
		}
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		description := ""
		startV, _ := ann.Get("start_index")
		endV, _ := ann.Get("end_index")
		start, okS := pyBigIntValue(startV)
		end, okE := pyBigIntValue(endV)
		if okS && okE && start.Sign() >= 0 && start.Cmp(end) < 0 && end.Cmp(big.NewInt(int64(len(runes)))) <= 0 {
			s := int(start.Int64())
			description = pystr.Strip(string(runes[max(0, s-200):s]))
			if r := []rune(description); len(r) > 200 {
				description = pystr.Strip(string(r[len(r)-200:]))
			}
		}
		results = append(results, titleHit("", u, description, len(results)+1))
		if len(results) >= limit {
			break
		}
	}
	return results
}

// Rows from the output JSON, then url_citation annotations, then the raw citations list
func xaiExtractResults(data any, limit int) ([]WebSearchHit, error) {
	texts, annotations, err := xaiCollectOutputText(data)
	if err != nil {
		return nil, err
	}
	for _, block := range texts {
		if parsed := xaiTryParseJSONResults(block, limit); len(parsed) > 0 {
			return parsed, nil
		}
	}
	if len(annotations) > 0 {
		if parsed := xaiResultsFromAnnotations(annotations, strings.Join(texts, "\n"), limit); len(parsed) > 0 {
			return parsed, nil
		}
	}
	raw, _ := pyDictGet(data, "citations")
	citations, ok := pyOr(raw, []any{}).([]any)
	if !ok {
		return []WebSearchHit{}, nil
	}
	head, _ := pySeqHead(citations, limit)
	out := []WebSearchHit{}
	for i, u := range head.([]any) {
		if s, isStr := u.(string); isStr && pystr.Strip(s) != "" {
			out = append(out, titleHit("", s, "", i+1))
		}
	}
	return out, nil
}

type xaiWebProvider struct{ baseWebProvider }

// Available when an xAI key or Grok OAuth tokens are present, never refreshing
func (p *xaiWebProvider) Available(env *Env) bool { return hasXAICredentials(env) }

// Grok web_search through the Responses API asked for structured JSON rows
func (p *xaiWebProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	if IsInterrupted(ctx) || ctx.Err() != nil {
		return searchFail("Interrupted"), nil
	}
	creds := resolveXAIHTTPCredentials(env, false)
	apiKey := pystr.Strip(creds.APIKey)
	baseURL := creds.BaseURL
	if baseURL == "" {
		baseURL = "https://api.x.ai/v1"
	}
	baseURL = strings.TrimRight(pystr.Strip(baseURL), "/")
	if apiKey == "" {
		return searchFail("No xAI credentials found. Run `icebath auth` to sign in with xAI Grok OAuth, or set XAI_API_KEY."), nil
	}
	limit = max(1, min(limit, 100))
	cfg := loadXAIWebConfig(env)
	model := ""
	if s, ok := cfg["model"].(string); ok {
		model = pystr.Strip(s)
	}
	if model == "" {
		model = xaiWebDefaultModel
	}
	tool := xaiWebSearchTool(cfg)
	if tool == nil {
		return searchFail("web.xai.allowed_domains and web.xai.excluded_domains cannot both be set (xAI restriction)."), nil
	}
	message := pyjson.NewObject()
	message.Set("role", "user")
	message.Set("content", xaiBuildPrompt(query, limit))
	payload := pyjson.NewObject()
	payload.Set("model", model)
	payload.Set("input", []any{message})
	payload.Set("tools", []any{tool})
	payload.Set("include", []any{"no_inline_citations"})
	envLog(env).Info("xAI web search", "base_url", baseURL, "query", query, "limit", limit, "model", model)
	timeoutValue, ok := cfg["timeout"]
	if !ok {
		timeoutValue = xaiWebDefaultTimeout
	}
	timeout, err := coerceFloat(timeoutValue, xaiWebDefaultTimeout)
	if err != nil {
		return nil, err
	}
	data, failure, err := xaiPostResponses(ctx, env, baseURL, payload, apiKey, timeout, creds.Provider == "xai-oauth")
	if err != nil {
		return nil, err
	}
	if failure != nil {
		return failure, nil
	}
	if d, ok := pyDict(data); ok {
		if apiErr, _ := d.Get("error"); apiErr != nil {
			if e, isDict := pyDict(apiErr); isDict {
				msg, _ := e.Get("message")
				if !pyTruthy(msg) {
					msg, _ = e.Get("code")
				}
				text := pyrepr.Str(pyOr(msg, "unknown error"))
				envLog(env).Warn("xAI web search returned error envelope", "error", text)
				return searchFail("xAI returned an error: " + text), nil
			}
		}
	}
	hits, err := xaiExtractResults(data, limit)
	if err != nil {
		return nil, err
	}
	return searchOK(hits), nil
}

// SetupSchema is the icebath tools picker entry, auth through the shared xai_grok post setup hook
func (p *xaiWebProvider) SetupSchema() map[string]any {
	return setupSchema("xAI Web Search (Grok)", "paid",
		"Agentic web search via Grok's web_search tool — uses xAI Grok OAuth or XAI_API_KEY.", "", "", "",
		map[string]any{"post_setup": "xai_grok"})
}

func init() {
	RegisterWebProvider(&xaiWebProvider{baseWebProvider{name: "xai", display: "xAI Web Search (Grok)"}})
}
