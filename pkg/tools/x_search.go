package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// x_search defaults and limits
const (
	defaultXSearchModel          = "grok-4.5"
	defaultXSearchTimeoutSeconds = 180
	defaultXSearchRetries        = 2
	xSearchMaxHandles            = 10
)

// Reasoning efforts the xAI x_search model accepts
var xSearchReasoningEfforts = []string{"low", "medium", "high", "xhigh"}

// HTTP client for xAI calls, replaced in tests
var xSearchHTTPClient = func(timeout time.Duration) *http.Client { return webHTTPClient(timeout) }

// Backoff between retried attempts, replaced in tests
var xSearchSleep = func(ctx context.Context, d time.Duration) error { return mediaRetrySleep(ctx, d) }

// Clock for the future date check, replaced in tests
var xSearchNow = time.Now

// The x_search config section
func xSearchConfig(env *Env) map[string]any {
	if m := dictOrNil(cfgTree(env)["x_search"]); m != nil {
		return m
	}
	return map[string]any{}
}

// Reasoning effort from x_search.reasoning_effort, an error naming the allowed values
func xSearchReasoningEffort(env *Env) (string, error) {
	raw, present := xSearchConfig(env)["reasoning_effort"]
	effort := ""
	if present && raw != nil {
		effort = strings.ToLower(strings.TrimSpace(pyStr(raw)))
	}
	if effort == "" {
		return "", nil
	}
	for _, e := range xSearchReasoningEfforts {
		if e == effort {
			return effort, nil
		}
	}
	return "", fmt.Errorf("x_search.reasoning_effort must be one of: %s (got %s)", strings.Join(xSearchReasoningEfforts, ", "), pyConfigRepr(raw))
}

// Integer x_search setting floored, the default when unparseable
func xSearchInt(env *Env, key string, def, floor int) int {
	raw, present := xSearchConfig(env)[key]
	if !present {
		return max(floor, def)
	}
	switch v := raw.(type) {
	case bool:
		if v {
			return max(floor, 1)
		}
		return max(floor, 0)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return def
		}
	}
	n, ok := pyInt(raw)
	if !ok {
		return def
	}
	return max(floor, n)
}

// API key, base URL, and credential source, an explicit XAI_API_KEY winning over OAuth
func resolveXAIBearer(env *Env) (string, string, string, error) {
	creds := resolveXAIHTTPCredentials(env, true)
	apiKey := strings.TrimSpace(creds.APIKey)
	if apiKey == "" {
		return "", "", "", errors.New("No xAI credentials available. Run `icebath auth add xai-oauth` " +
			"to sign in with your SuperGrok subscription, or set XAI_API_KEY.")
	}
	base := strings.TrimSpace(creds.BaseURL)
	if base == "" {
		base = DefaultXAIBaseURL
	}
	source := creds.Provider
	if source == "" {
		source = "xai"
	}
	return apiKey, strings.TrimRight(base, "/"), source, nil
}

// Reports whether xAI credentials resolve to a non empty bearer
func checkXSearchRequirements(env *Env) bool {
	return strings.TrimSpace(resolveXAIHTTPCredentials(env, false).APIKey) != ""
}

// Items of a handle argument the way Python iterates it
func handleItems(handles any) []any {
	switch v := handles.(type) {
	case nil:
		return nil
	case []any:
		return v
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out
	case string:
		var out []any
		for _, r := range v {
			out = append(out, string(r))
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out
	}
	return nil
}

// Handles stripped of whitespace and a leading at sign, at most ten
func normalizeHandles(handles any, field string) ([]string, error) {
	var cleaned []string
	for _, h := range handleItems(handles) {
		s := ""
		if truthy(h) {
			s = pyStr(h)
		}
		if s = strings.TrimLeft(strings.TrimSpace(s), "@"); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	if len(cleaned) > xSearchMaxHandles {
		return nil, fmt.Errorf("%s supports at most %d handles", field, xSearchMaxHandles)
	}
	return cleaned, nil
}

// strptime %Y-%m-%d shape, one or two digit month and day allowed
var isoDateRe = regexp.MustCompile(`^(\d{4})-(1[0-2]|0[1-9]|[1-9])-(3[01]|[12]\d|0[1-9]|[1-9]| [1-9])$`)

// Strict YYYY-MM-DD date, ok false for blank input
func parseISODate(value, field string) (time.Time, bool, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return time.Time{}, false, nil
	}
	m := isoDateRe.FindStringSubmatch(raw)
	if m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(strings.TrimSpace(m[3]))
		t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
		if y >= 1 && t.Day() == d && int(t.Month()) == mo {
			return t, true, nil
		}
	}
	return time.Time{}, false, fmt.Errorf("%s must be YYYY-MM-DD (got %s)", field, pyReprStr(raw))
}

// Both dates parse, from is on or before to, and from is not after today UTC
func validateDateRange(fromDate, toDate string) error {
	from, hasFrom, err := parseISODate(fromDate, "from_date")
	if err != nil {
		return err
	}
	to, hasTo, err := parseISODate(toDate, "to_date")
	if err != nil {
		return err
	}
	if hasFrom && hasTo && from.After(to) {
		return fmt.Errorf("from_date (%s) must be on or before to_date (%s)", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
	now := xSearchNow().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if hasFrom && from.After(today) {
		return fmt.Errorf("from_date (%s) is in the future; X Search only indexes past posts (today UTC is %s)",
			from.Format("2006-01-02"), today.Format("2006-01-02"))
	}
	return nil
}

// Content entries of every message item in a Responses payload
func messageContents(payload map[string]any) []map[string]any {
	var out []map[string]any
	items, _ := payload["output"].([]any)
	for _, it := range items {
		item := dictOrNil(it)
		if item["type"] != "message" {
			continue
		}
		contents, _ := item["content"].([]any)
		for _, c := range contents {
			if m := dictOrNil(c); m != nil {
				out = append(out, m)
			}
		}
	}
	return out
}

// Answer text from output_text, else the joined message text parts
func extractResponseText(payload map[string]any) string {
	if s := strings.TrimSpace(pyStrOrEmpty(falsyNil(payload["output_text"]))); s != "" {
		return s
	}
	var parts []string
	for _, c := range messageContents(payload) {
		if t := c["type"]; t != "output_text" && t != "text" {
			continue
		}
		if p := strings.TrimSpace(pyStrOrEmpty(falsyNil(c["text"]))); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// Nil for Python falsy values so str(x or "") renders empty
func falsyNil(v any) any {
	if !truthy(v) {
		return nil
	}
	return v
}

// One url_citation annotation in wire order
type inlineCitation struct {
	URL        any `json:"url"`
	Title      any `json:"title"`
	StartIndex any `json:"start_index"`
	EndIndex   any `json:"end_index"`
}

// Value at a key, the default when the key is absent
func getOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// url_citation annotations of every message content
func extractInlineCitations(payload map[string]any) []inlineCitation {
	out := []inlineCitation{}
	for _, content := range messageContents(payload) {
		anns, _ := content["annotations"].([]any)
		for _, a := range anns {
			m := dictOrNil(a)
			if m["type"] != "url_citation" {
				continue
			}
			out = append(out, inlineCitation{URL: getOr(m, "url", ""), Title: getOr(m, "title", ""), StartIndex: m["start_index"], EndIndex: m["end_index"]})
		}
	}
	return out
}

// Non success HTTP response from xAI
type xaiHTTPError struct {
	status int
	url    string
	body   []byte
}

func (e *xaiHTTPError) Error() string {
	kind := "Client"
	if e.status >= 500 {
		kind = "Server"
	}
	return fmt.Sprintf("%d %s Error: %s for url: %s", e.status, kind, http.StatusText(e.status), e.url)
}

// Readable xAI error from the JSON body code and error, else the body text, else the status line
func xaiHTTPErrorMessage(e *xaiHTTPError) string {
	payload, err := pyJSONLoads(string(e.body))
	obj, isObj := payload.(*pyJSONObject)
	if err != nil || !isObj {
		if text := strings.TrimSpace(string(e.body)); text != "" {
			return clipRunes(text, 500)
		}
		return e.Error()
	}
	code := ""
	if v, _ := obj.get("code"); pyTruthy(v) {
		code = strings.TrimSpace(pyJSONStr(v))
	}
	message := ""
	if v, _ := obj.get("error"); pyTruthy(v) {
		message = strings.TrimSpace(pyJSONStr(v))
	}
	if message == "" {
		message = pyJSONRepr(obj)
	}
	out := message
	if code != "" && !strings.Contains(message, code) {
		out = code + ": " + message
	}
	if out == "" {
		return e.Error()
	}
	return out
}

// Python repr of a config scalar for validation messages
func pyConfigRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return pyReprStr(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return pyFloatRepr(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	decoded, err := pyJSONLoads(string(b))
	if err != nil {
		return fmt.Sprint(v)
	}
	return pyJSONRepr(decoded)
}

// Ordered failure payload of an x_search call
type xSearchFailure struct {
	Success   bool   `json:"success"`
	Provider  string `json:"provider"`
	Tool      string `json:"tool"`
	Error     string `json:"error"`
	ErrorType string `json:"error_type"`
}

// Structured failure with the Python exception class name
func xSearchErrorJSON(message, errorType string) string {
	return jsonCompact(xSearchFailure{Success: false, Provider: "xai", Tool: "x_search", Error: message, ErrorType: errorType})
}

// Transport failure class of a request error, ReadTimeout and ConnectTimeout told apart from ConnectionError
func xSearchTransportClass(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		if strings.Contains(err.Error(), "dial ") {
			return "ConnectTimeout"
		}
		return "ReadTimeout"
	}
	return "ConnectionError"
}

// Request failure with its Python exception class
type xSearchRequestError struct {
	class string
	err   error
}

func (e *xSearchRequestError) Error() string { return e.err.Error() }
func (e *xSearchRequestError) Unwrap() error { return e.err }

// Posts with retries on 5xx, timeouts, and connection errors, returning the last failure
func xSearchPostWithRetries(ctx context.Context, env *Env, target string, headers map[string]string, payload any) ([]byte, error) {
	timeout := xSearchInt(env, "timeout_seconds", defaultXSearchTimeoutSeconds, 30)
	maxRetries := xSearchInt(env, "retries", defaultXSearchRetries, 0)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	client := xSearchHTTPClient(time.Duration(timeout) * time.Second)
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		var kind, detail string
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			class := xSearchTransportClass(err)
			if attempt >= maxRetries {
				return nil, &xSearchRequestError{class: class, err: err}
			}
			kind, detail = "transient", err.Error()
		} else {
			raw, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr != nil {
				class := xSearchTransportClass(rerr)
				if attempt >= maxRetries {
					return nil, &xSearchRequestError{class: class, err: rerr}
				}
				kind, detail = "transient", rerr.Error()
			} else if resp.StatusCode >= 400 {
				herr := &xaiHTTPError{status: resp.StatusCode, url: resp.Request.URL.String(), body: raw}
				if resp.StatusCode < 500 || attempt >= maxRetries {
					return nil, herr
				}
				kind, detail = "upstream", xaiHTTPErrorMessage(herr)
			} else {
				return raw, nil
			}
		}
		envLog(env).Warn(fmt.Sprintf("x_search %s failure on attempt %d/%d: %s", kind, attempt+1, maxRetries+1, detail))
		if err := xSearchSleep(ctx, time.Duration(math.Min(5.0, 1.5*float64(attempt+1))*float64(time.Second))); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("x_search request did not return a response")
}

// The x_search tool definition sent to xAI
type xSearchToolDef struct {
	Type                     string   `json:"type"`
	AllowedXHandles          []string `json:"allowed_x_handles,omitempty"`
	ExcludedXHandles         []string `json:"excluded_x_handles,omitempty"`
	FromDate                 string   `json:"from_date,omitempty"`
	ToDate                   string   `json:"to_date,omitempty"`
	EnableImageUnderstanding bool     `json:"enable_image_understanding,omitempty"`
	EnableVideoUnderstanding bool     `json:"enable_video_understanding,omitempty"`
}

// Tool definition and the names of the active narrowing filters, an error on invalid filters
func buildXSearchToolDef(allowedHandles, excludedHandles any, fromDate, toDate string, image, video bool) (xSearchToolDef, []string, error) {
	allowed, err := normalizeHandles(allowedHandles, "allowed_x_handles")
	if err != nil {
		return xSearchToolDef{}, nil, err
	}
	excluded, err := normalizeHandles(excludedHandles, "excluded_x_handles")
	if err != nil {
		return xSearchToolDef{}, nil, err
	}
	if len(allowed) > 0 && len(excluded) > 0 {
		return xSearchToolDef{}, nil, errors.New("allowed_x_handles and excluded_x_handles cannot be used together")
	}
	if err := validateDateRange(fromDate, toDate); err != nil {
		return xSearchToolDef{}, nil, err
	}
	def := xSearchToolDef{Type: "x_search", EnableImageUnderstanding: image, EnableVideoUnderstanding: video}
	var active []string
	if len(allowed) > 0 {
		def.AllowedXHandles = allowed
		active = append(active, "allowed_x_handles")
	}
	if len(excluded) > 0 {
		def.ExcludedXHandles = excluded
		active = append(active, "excluded_x_handles")
	}
	if s := strings.TrimSpace(fromDate); s != "" {
		def.FromDate = s
		active = append(active, "from_date")
	}
	if s := strings.TrimSpace(toDate); s != "" {
		def.ToDate = s
		active = append(active, "to_date")
	}
	return def, active, nil
}

// Responses request body of one x_search call
type xSearchPayload struct {
	Model     string           `json:"model"`
	Input     []map[string]any `json:"input"`
	Tools     []xSearchToolDef `json:"tools"`
	Store     bool             `json:"store"`
	Reasoning map[string]any   `json:"reasoning,omitempty"`
}

// Ordered success payload of an x_search call
type xSearchResult struct {
	Success          bool             `json:"success"`
	Provider         string           `json:"provider"`
	CredentialSource string           `json:"credential_source"`
	Tool             string           `json:"tool"`
	Model            string           `json:"model"`
	Query            string           `json:"query"`
	Answer           string           `json:"answer"`
	Citations        []any            `json:"citations"`
	InlineCitations  []inlineCitation `json:"inline_citations"`
	Degraded         bool             `json:"degraded"`
	DegradedReason   *string          `json:"degraded_reason"`
}

// Searches X through the xAI Responses x_search tool and returns the structured result
func xSearchTool(ctx context.Context, env *Env, query string, allowedHandles, excludedHandles any, fromDate, toDate string, image, video bool) string {
	log := envLog(env)
	if strings.TrimSpace(query) == "" {
		return toolErrorJSON("query is required for x_search", nil)
	}
	apiKey, baseURL, source, err := resolveXAIBearer(env)
	if err != nil {
		return toolErrorJSON(err.Error(), nil)
	}
	toolDef, active, err := buildXSearchToolDef(allowedHandles, excludedHandles, fromDate, toDate, image, video)
	if err != nil {
		return toolErrorJSON(err.Error(), nil)
	}
	effort, err := xSearchReasoningEffort(env)
	if err != nil {
		return toolErrorJSON(err.Error(), nil)
	}
	model := strings.TrimSpace(pyStrOrEmpty(falsyNil(xSearchConfig(env)["model"])))
	if model == "" {
		model = defaultXSearchModel
	}
	q := strings.TrimSpace(query)
	payload := xSearchPayload{Model: model, Input: []map[string]any{{"role": "user", "content": q}}, Tools: []xSearchToolDef{toolDef}, Store: false}
	if effort != "" {
		payload.Reasoning = map[string]any{"effort": effort}
	}
	headers := map[string]string{"Authorization": "Bearer " + apiKey, "Content-Type": "application/json", "User-Agent": icebathXAIUserAgent()}
	raw, err := xSearchPostWithRetries(ctx, env, baseURL+"/responses", headers, payload)
	if err != nil {
		var herr *xaiHTTPError
		var rerr *xSearchRequestError
		switch {
		case errors.As(err, &herr):
			log.Error("x_search failed", "error", err)
			return xSearchErrorJSON(xaiHTTPErrorMessage(herr), "HTTPError")
		case errors.As(err, &rerr) && rerr.class == "ReadTimeout":
			log.Error("x_search timed out", "error", err)
			return xSearchErrorJSON(fmt.Sprintf("xAI x_search timed out after %d seconds", xSearchInt(env, "timeout_seconds", defaultXSearchTimeoutSeconds, 30)), "ReadTimeout")
		case errors.As(err, &rerr):
			log.Error("x_search failed", "error", err)
			return xSearchErrorJSON(err.Error(), rerr.class)
		}
		log.Error("x_search failed", "error", err)
		return xSearchErrorJSON(err.Error(), "RuntimeError")
	}
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		msg := err.Error()
		if _, perr := pyJSONLoads(string(raw)); perr != nil {
			msg = perr.Error()
		}
		log.Error("x_search failed", "error", msg)
		return xSearchErrorJSON(msg, "JSONDecodeError")
	}
	data, ok := decoded.(map[string]any)
	if !ok {
		typeName := "NoneType"
		if v, perr := pyJSONLoads(string(raw)); perr == nil {
			typeName = pyJSONTypeName(v)
		}
		msg := fmt.Sprintf("'%s' object has no attribute 'get'", typeName)
		log.Error("x_search failed", "error", msg)
		return xSearchErrorJSON(msg, "AttributeError")
	}
	citations := []any{}
	if list, ok := falsyNil(data["citations"]).([]any); ok {
		citations = list
	}
	inline := extractInlineCitations(data)
	degraded := len(active) > 0 && len(citations) == 0 && len(inline) == 0
	result := xSearchResult{Success: true, Provider: "xai", CredentialSource: source, Tool: "x_search", Model: model, Query: q,
		Answer: extractResponseText(data), Citations: citations, InlineCitations: inline, Degraded: degraded}
	if degraded {
		reason := "no citations returned despite filters: " + strings.Join(active, ", ")
		result.DegradedReason = &reason
	}
	return jsonCompact(result)
}

var xSearchSchema = map[string]any{
	"name": "x_search",
	"description": "Search X (Twitter) posts, profiles and threads via xAI's built-in X Search. Read-only discovery only: use this for current " +
		"discussion, reactions, or claims on public X rather than general web " +
		"pages. Do not use it to post, reply, like, DM, upload media, delete, " +
		"or inspect the user's authenticated X account — those require a " +
		"separate authenticated X API surface outside this tool. Available " +
		"when xAI credentials are configured (SuperGrok OAuth or XAI_API_KEY).",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "What to look up on X."},
			"allowed_x_handles": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Optional list of X handles to include exclusively (max 10).",
			},
			"excluded_x_handles": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Optional list of X handles to exclude (max 10).",
			},
			"from_date": map[string]any{"type": "string", "description": "Optional start date in YYYY-MM-DD format."},
			"to_date":   map[string]any{"type": "string", "description": "Optional end date in YYYY-MM-DD format."},
			"enable_image_understanding": map[string]any{
				"type": "boolean", "description": "Whether xAI should analyze images attached to matching X posts.", "default": false,
			},
			"enable_video_understanding": map[string]any{
				"type": "boolean", "description": "Whether xAI should analyze videos attached to matching X posts.", "default": false,
			},
		},
		"required": []any{"query"},
	},
}

// x_search handler mapping arguments onto the tool
func handleXSearch(ctx context.Context, call *Call) string {
	a := call.Args
	return xSearchTool(ctx, call.Env, argS(a, "query"), a["allowed_x_handles"], a["excluded_x_handles"],
		argS(a, "from_date"), argS(a, "to_date"), truthy(a["enable_image_understanding"]), truthy(a["enable_video_understanding"]))
}

// Registers x_search into the x_search toolset
func registerXSearchTool() {
	Register(Def{Name: "x_search", Toolset: "x_search", Schema: xSearchSchema, Handler: handleXSearch,
		Check: checkXSearchRequirements, RequiresEnv: []string{"XAI_API_KEY"}, Emoji: "\U0001F426", MaxResultChars: 100_000})
}
