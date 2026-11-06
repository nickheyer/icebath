package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const exaMissingKey = "EXA_API_KEY environment variable not set. Get your API key at https://exa.ai"

// Exa API origin the client posts to, swapped by tests
var exaBaseURL = "https://api.exa.ai"

// Exa client limit for connecting and for each wait on the server
const exaRequestTimeout = 600 * time.Second

// Longest stretch of a non JSON Exa body quoted in the error
const exaNonJSONBodySnippet = 500

// Native Exa REST client speaking the request shapes of the exa-py SDK
type exaClient struct {
	baseURL string
	header  http.Header
}

// Client authenticated with the key and tagged with the icebath integration header
func newExaClient(apiKey string) *exaClient {
	h := http.Header{}
	h.Set("x-api-key", apiKey)
	h.Set("User-Agent", icebathUserAgent())
	h.Set("Content-Type", "application/json")
	h.Set("x-exa-integration", "icebath-agent")
	return &exaClient{baseURL: exaBaseURL, header: h}
}

// Exa client cached for the current EXA_API_KEY
func getExaClient(ctx context.Context, env *Env) (*exaClient, error) {
	return cachedSDKClient(ctx, env, "_exa_client", "EXA_API_KEY", exaMissingKey, newExaClient)
}

// Posts a payload and returns the decoded body, a failed status or non JSON body raised verbatim
func (c *exaClient) request(ctx context.Context, env *Env, endpoint string, data *pyjson.Object) (any, error) {
	body, err := pyjson.Dumps(data)
	if err != nil {
		return nil, err
	}
	timeouts := sdkTimeouts{connect: exaRequestTimeout, idle: exaRequestTimeout}
	resp, err := sdkSend(ctx, timeouts, http.MethodPost, c.baseURL+endpoint, []byte(body), c.header)
	if err != nil {
		return nil, err
	}
	if resp.status >= 400 {
		return nil, verbatim(fmt.Sprintf("Request failed with status code %d: %s", resp.status, resp.text()))
	}
	payload, err := decodeExaJSON(resp)
	if err != nil {
		return nil, err
	}
	issueExaBodyWarnings(env, endpoint, payload, resp.header.Get("x-request-id"))
	return payload, nil
}

// JSON body of a successful answer, a ValueError naming the status and body otherwise
func decodeExaJSON(resp *sdkResponse) (any, error) {
	var payload any
	var err error
	if strings.Contains(strings.ToLower(resp.header.Get("Content-Type")), "charset=") {
		payload, err = pyjson.Loads(resp.text())
	} else {
		payload, err = pyjson.LoadsBytes(resp.body)
	}
	if err == nil {
		return payload, nil
	}
	text := strings.Join(strings.FieldsFunc(resp.text(), pyIsSpace), " ")
	if text == "" {
		return nil, verbatim(fmt.Sprintf("Expected a JSON response but received an empty body (status %d).", resp.status))
	}
	if r := []rune(text); len(r) > exaNonJSONBodySnippet {
		text = string(r[:exaNonJSONBodySnippet]) + "…"
	}
	return nil, verbatim(fmt.Sprintf("Expected a JSON response but received a non-JSON body (status %d): %s", resp.status, text))
}

// Logs each API warning a search answer carries, like the ExaWarning the SDK issues
func issueExaBodyWarnings(env *Env, endpoint string, payload any, requestID string) {
	if endpoint != "/search" {
		return
	}
	d, ok := pyDict(payload)
	if !ok {
		return
	}
	raw, _ := d.Get("warnings")
	items, ok := raw.([]any)
	if !ok {
		return
	}
	if id, ok := d.Get("requestId"); ok {
		if s, ok := id.(string); ok {
			requestID = s
		}
	}
	for _, item := range items {
		w, ok := pyDict(item)
		if !ok {
			continue
		}
		code, _ := w.Get("code")
		path, _ := w.Get("path")
		message, _ := w.Get("message")
		c, ok1 := code.(string)
		p, ok2 := path.(string)
		m, ok3 := message.(string)
		if ok1 && ok2 && ok3 {
			envLog(env).Warn(fmt.Sprintf("[exa] POST %s %s at %s: %s", endpoint, c, p, m), "request_id", requestID)
		}
	}
}

// Fails like the SDK on a costDollars block it cannot read
func checkExaCostDollars(data any) error {
	raw, err := pyDictGet(data, "costDollars")
	if err != nil {
		return err
	}
	if pyTruthy(raw) {
		if _, err := pyDictGet(raw, "total"); err != nil {
			return err
		}
	}
	return nil
}

// Result rows of an answer in snake_case, the KeyError of a missing results list
func exaResultRows(data any) ([]*pyjson.Object, error) {
	d, _ := pyDict(data)
	raw, ok := d.Get("results")
	if !ok {
		return nil, errors.New("'results'")
	}
	items, err := pyIter(raw)
	if err != nil {
		return nil, err
	}
	rows := make([]*pyjson.Object, 0, len(items))
	for _, item := range items {
		row, ok := pyDict(item)
		if !ok {
			return nil, pyAttributeError(item, "get")
		}
		rows = append(rows, toSnakeCase(row))
	}
	return rows, nil
}

// Search returning query relevant highlights for each result
func (c *exaClient) search(ctx context.Context, env *Env, query string, numResults int) ([]*pyjson.Object, error) {
	data, err := c.request(ctx, env, "/search", pyObject("query", query, "contents", pyObject("highlights", true), "numResults", numResults))
	if err != nil {
		return nil, err
	}
	if err := checkExaCostDollars(data); err != nil {
		return nil, err
	}
	return exaResultRows(data)
}

// exaContentStatus is one entry of a contents answer's statuses
type exaContentStatus struct {
	id, status     any
	tag, httpCode  any
	errorPresented bool
}

// Page text of each URL with the per URL statuses the API reports
func (c *exaClient) getContents(ctx context.Context, env *Env, urls []string) ([]*pyjson.Object, []exaContentStatus, error) {
	data, err := c.request(ctx, env, "/contents", pyObject("urls", urls, "text", true))
	if err != nil {
		return nil, nil, err
	}
	if err := checkExaCostDollars(data); err != nil {
		return nil, nil, err
	}
	d, _ := pyDict(data)
	rawStatuses, present := d.Get("statuses")
	if !present {
		rawStatuses = []any{}
	}
	items, err := pyIter(rawStatuses)
	if err != nil {
		return nil, nil, err
	}
	statuses := make([]exaContentStatus, 0, len(items))
	for _, item := range items {
		raw, ok := pyDict(item)
		if !ok {
			return nil, nil, pyAttributeError(item, "get")
		}
		s := exaContentStatus{}
		s.id, _ = raw.Get("id")
		s.status, _ = raw.Get("status")
		errObj, _ := raw.Get("error")
		if e, ok := pyDict(errObj); ok {
			s.errorPresented = true
			s.tag, _ = e.Get("tag")
			s.httpCode, _ = e.Get("httpStatusCode")
		}
		statuses = append(statuses, s)
	}
	rows, err := exaResultRows(data)
	if err != nil {
		return nil, nil, err
	}
	return rows, statuses, nil
}

// Origin status of a failed status entry, its http status code else the one its tag names
func exaStatusOrigin(s exaContentStatus) any {
	if !s.errorPresented {
		return nil
	}
	if pyTruthy(s.httpCode) {
		return s.httpCode
	}
	if code, ok := exaCrawlStatus(pyStrField(pyOr(s.tag, ""))); ok {
		return code
	}
	return nil
}

type exaProvider struct{ baseWebProvider }

// Search through the keyless ring or the keyed client, every failure a search error result
func (p *exaProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	return runSearch(ctx, env, "Exa", func() (*WebSearchResponse, error) {
		key, err := providerEnv(ctx, env, "EXA_API_KEY")
		if err != nil {
			return nil, err
		}
		if useKeyless(env, "exa", key) {
			envLog(env).Info("Exa keyless search", "query", query, "limit", limit)
			return keylessSearchWithFailover(ctx, env, "exa", query, limit), nil
		}
		envLog(env).Info("Exa search", "query", query, "limit", limit)
		client, err := getExaClient(ctx, env)
		if err != nil {
			return nil, err
		}
		rows, err := client.search(ctx, env, query, limit)
		if err != nil {
			return nil, err
		}
		hits := make([]WebSearchHit, 0, len(rows))
		for i, r := range rows {
			u, _ := r.Get("url")
			title, _ := r.Get("title")
			highlights, _ := r.Get("highlights")
			description, err := pyJoin(" ", pyOr(highlights, []any{}))
			if err != nil {
				return nil, err
			}
			hits = append(hits, webHit(pyStrField(pyOr(u, "")), pyStrField(pyOr(title, "")), description, i+1))
		}
		return searchOK(hits), nil
	}, true), nil
}

// Extract page text, URLs Exa could not fetch reported from its statuses
func (p *exaProvider) Extract(ctx context.Context, env *Env, urls []string, format string) ([]*WebPage, error) {
	return runExtract(ctx, env, "Exa", urls, func() ([]*WebPage, error) {
		key, err := providerEnv(ctx, env, "EXA_API_KEY")
		if err != nil {
			return nil, err
		}
		if useKeyless(env, "exa", key) {
			envLog(env).Info("Exa keyless extract", "urls", len(urls))
			return keylessExtractWithFailover(ctx, env, "exa", urls), nil
		}
		envLog(env).Info("Exa extract", "urls", len(urls))
		client, err := getExaClient(ctx, env)
		if err != nil {
			return nil, err
		}
		rows, statuses, err := client.getContents(ctx, env, urls)
		if err != nil {
			return nil, err
		}
		out := make([]*WebPage, 0, len(rows))
		for _, r := range rows {
			u, _ := r.Get("url")
			title, _ := r.Get("title")
			text, _ := r.Get("text")
			out = append(out, webDocument(pyStrField(pyOr(u, "")), pyStrField(pyOr(title, "")), pyStrField(pyOr(text, "")), ""))
		}
		for _, s := range statuses {
			if status, ok := s.status.(string); !ok || status != "error" {
				continue
			}
			out = append(out, vendorErrorEntry(pyStrField(pyOr(s.id, "")), exaStatusOrigin(s), s.tag, nil))
		}
		return out, nil
	}, true), nil
}

// SetupSchema is the icebath tools picker entry
func (p *exaProvider) SetupSchema() map[string]any {
	return keylessVariantSchema("Exa", "EXA_API_KEY", "https://exa.ai",
		"Semantic + neural web search with content extraction on Exa's anonymous free tier. Rate-limited under burst load.",
		"Semantic + neural web search with content extraction via the Exa SDK. Unthrottled, guaranteed service.")
}

func init() {
	RegisterWebProvider(&exaProvider{baseWebProvider{name: "exa", display: "Exa", keyEnv: "EXA_API_KEY", extract: true, keyless: true}})
}
