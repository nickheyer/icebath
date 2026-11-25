package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const exaMissingKey = "EXA_API_KEY environment variable not set. Get your API key at https://exa.ai"

// Exa API origin the client posts to, swapped by tests
var exaBaseURL = "https://api.exa.ai"

// Exa client limit for connecting and for each wait on the server
var exaRequestTimeout = httputil.EachPhase(time.Duration(float64(600.0) * float64(time.Second)))

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
func (c *exaClient) request(ctx context.Context, env *Env, endpoint string, data *jsonutil.Object) (any, error) {
	body, err := jsonutil.Encode(data)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	resp, err := requestsSend(ctx, exaRequestTimeout, http.MethodPost, c.baseURL+endpoint, []byte(body), c.header)
	if err != nil {
		return nil, exaTypedTimeouts(err, exaRequestTimeout, time.Since(started))
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
		payload, err = jsonutil.Decode(resp.text())
	} else {
		payload, err = jsonutil.Decode(resp.body)
	}
	if err == nil {
		return payload, nil
	}
	text := strings.Join(strings.FieldsFunc(resp.text(), unicode.IsSpace), " ")
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
	d, ok := mappingOf(payload)
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
		w, ok := mappingOf(item)
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
	raw, err := dictGet(data, "costDollars")
	if err != nil {
		return err
	}
	if anyutil.IsTrue(raw) {
		if _, err := dictGet(raw, "total"); err != nil {
			return err
		}
	}
	return nil
}

// Result rows of an answer in snake_case, an error naming a missing results list
func exaResultRows(data any) ([]*jsonutil.Object, error) {
	d, _ := mappingOf(data)
	raw, ok := d.Get("results")
	if !ok {
		return nil, errors.New("the reply has no results field")
	}
	items, err := elementsOf(raw)
	if err != nil {
		return nil, err
	}
	rows := make([]*jsonutil.Object, 0, len(items))
	for _, item := range items {
		row, ok := mappingOf(item)
		if !ok {
			return nil, attributeError(item, "get")
		}
		entities, _ := row.Get("entities")
		if err := exaCheckEntities(entities); err != nil {
			return nil, err
		}
		rows = append(rows, toSnakeCase(row))
	}
	return rows, nil
}

// Mapping the SDK reads with get, the AttributeError of any other value
func exaMapping(v any) (*jsonutil.Object, error) {
	if d, ok := mappingOf(v); ok {
		return d, nil
	}
	return nil, attributeError(v, "get")
}

// Raises what the SDK entity parsing raises on a malformed result entities list
func exaCheckEntities(raw any) error {
	if !anyutil.IsTrue(raw) {
		return nil
	}
	items, err := elementsOf(raw)
	if err != nil {
		return err
	}
	for _, item := range items {
		entity, err := exaMapping(item)
		if err != nil {
			return err
		}
		kind, _ := entity.Get("type")
		check := exaCheckPersonProperties
		switch kind {
		case "company":
			check = exaCheckCompanyProperties
		case "person":
		default:
			continue
		}
		for _, key := range []string{"id", "version"} {
			if _, ok := entity.Get(key); !ok {
				return fmt.Errorf("a %s entity has no %s field", kind, key)
			}
		}
		props, ok := entity.Get("properties")
		if !ok {
			props = jsonutil.NewObject()
		}
		if err := check(props); err != nil {
			return err
		}
	}
	return nil
}

// Reads of an optional nested mapping, skipped when falsy like the SDK's not data guard
func exaCheckOptional(v any) error {
	if !anyutil.IsTrue(v) {
		return nil
	}
	_, err := exaMapping(v)
	return err
}

// Raises what parsing company properties raises, nested blocks in the SDK's order
func exaCheckCompanyProperties(raw any) error {
	d, err := exaMapping(raw)
	if err != nil {
		return err
	}
	workforce, _ := d.Get("workforce")
	headquarters, _ := d.Get("headquarters")
	for _, nested := range []any{workforce, headquarters} {
		if err := exaCheckOptional(nested); err != nil {
			return err
		}
	}
	financials, _ := d.Get("financials")
	if err := exaCheckOptional(financials); err != nil {
		return err
	}
	if anyutil.IsTrue(financials) {
		f, _ := exaMapping(financials)
		round, _ := f.Get("fundingLatestRound")
		if err := exaCheckOptional(round); err != nil {
			return err
		}
	}
	traffic, _ := d.Get("webTraffic")
	return exaCheckOptional(traffic)
}

// Raises what parsing person properties raises, histories iterated when not None
func exaCheckPersonProperties(raw any) error {
	d, err := exaMapping(raw)
	if err != nil {
		return err
	}
	work, _ := d.Get("workHistory")
	education, _ := d.Get("educationHistory")
	for _, history := range []struct {
		entries any
		ref     string
	}{{work, "company"}, {education, "institution"}} {
		if history.entries == nil {
			continue
		}
		entries, err := elementsOf(history.entries)
		if err != nil {
			return err
		}
		for _, raw := range entries {
			entry, err := exaMapping(raw)
			if err != nil {
				return err
			}
			dates, _ := entry.Get("dates")
			ref, _ := entry.Get(history.ref)
			for _, nested := range []any{dates, ref} {
				if err := exaCheckOptional(nested); err != nil {
					return err
				}
			}
		}
	}
	research, _ := d.Get("research")
	if err := exaCheckOptional(research); err != nil || !anyutil.IsTrue(research) {
		return err
	}
	r, _ := exaMapping(research)
	notable, _ := r.Get("notableWorks")
	if notable == nil {
		return nil
	}
	works, err := elementsOf(notable)
	if err != nil {
		return err
	}
	for _, w := range works {
		if _, err := exaMapping(w); err != nil {
			return err
		}
	}
	return nil
}

// Search returning query relevant highlights for each result
func (c *exaClient) search(ctx context.Context, env *Env, query string, numResults int) ([]*jsonutil.Object, error) {
	data, err := c.request(ctx, env, "/search", jsonutil.ObjectOf("query", query, "contents", jsonutil.ObjectOf("highlights", true), "numResults", numResults))
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
func (c *exaClient) getContents(ctx context.Context, env *Env, urls []string) ([]*jsonutil.Object, []exaContentStatus, error) {
	data, err := c.request(ctx, env, "/contents", jsonutil.ObjectOf("urls", urls, "text", true))
	if err != nil {
		return nil, nil, err
	}
	if err := checkExaCostDollars(data); err != nil {
		return nil, nil, err
	}
	d, _ := mappingOf(data)
	rawStatuses, present := d.Get("statuses")
	if !present {
		rawStatuses = []any{}
	}
	items, err := elementsOf(rawStatuses)
	if err != nil {
		return nil, nil, err
	}
	statuses := make([]exaContentStatus, 0, len(items))
	for _, item := range items {
		raw, ok := mappingOf(item)
		if !ok {
			return nil, nil, attributeError(item, "get")
		}
		s := exaContentStatus{}
		s.id, _ = raw.Get("id")
		s.status, _ = raw.Get("status")
		errObj, _ := raw.Get("error")
		if e, ok := mappingOf(errObj); ok {
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

// Origin status of a failed status entry, its http status code when that is a usable HTTP code, else the one its tag names
func exaStatusOrigin(s exaContentStatus) any {
	if !s.errorPresented {
		return nil
	}
	if code, ok := vendorHTTPStatus(s.httpCode); ok {
		return code
	}
	if code, ok := exaCrawlStatus(anyutil.String(anyutil.Or(s.tag, ""))); ok {
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
			envLog(env).Info(fmt.Sprintf("Exa keyless search: '%s' (limit=%d)", query, limit))
			return keylessSearchWithFailover(ctx, env, "exa", query, limit)
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
			hits = append(hits, webHit(anyutil.String(anyutil.Or(u, "")), anyutil.String(anyutil.Or(title, "")), joinText(" ", highlights), i+1))
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
			return keylessExtractWithFailover(ctx, env, "exa", urls)
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
			out = append(out, webDocument(anyutil.String(anyutil.Or(u, "")), anyutil.String(anyutil.Or(title, "")), anyutil.String(anyutil.Or(text, "")), ""))
		}
		for _, s := range statuses {
			if status, ok := s.status.(string); !ok || status != "error" {
				continue
			}
			out = append(out, vendorErrorEntry(anyutil.String(anyutil.Or(s.id, "")), exaStatusOrigin(s), s.tag, nil))
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

// Exa search and extract provider the web/exa plugin registers
func NewExaWebProvider() WebProvider {
	return &exaProvider{baseWebProvider{name: "exa", display: "Exa", keyEnv: "EXA_API_KEY", extract: true, keyless: true}}
}

// Class name the registry log and type errors print, the plugin provider class
func (*exaProvider) ErrorKind() string { return "ExaWebSearchProvider" }
