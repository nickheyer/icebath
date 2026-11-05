package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Every vendor here caps max_results at 20 server side
const webSearchLimitCap = 20

// Longest error body a tool result may carry
const maxToolErrorChars = 2048

const toolErrorTruncationMarker = "… [truncated]"

// String argument, numbers formatted, anything else empty
func argS(args map[string]any, key string) string {
	switch v := args[key].(type) {
	case string:
		return v
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case json.Number:
		return v.String()
	}
	return ""
}

// Reports whether the argument is present and not null
func argHas(args map[string]any, key string) bool {
	v, ok := args[key]
	return ok && v != nil
}

// Integer argument, the fallback when absent or not numeric
func argI(args map[string]any, key string, fallback int) int {
	if n, ok := config.AsInt(args[key]); ok {
		return n
	}
	return fallback
}

// Boolean argument, the fallback when absent or not boolean like
func argB(args map[string]any, key string, fallback bool) bool {
	if b, ok := config.AsBool(args[key]); ok {
		return b
	}
	return fallback
}

// List argument, nil when absent or not a list
func argL(args map[string]any, key string) []any {
	switch v := args[key].(type) {
	case []any:
		return v
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out
	case []map[string]any:
		out := make([]any, len(v))
		for i, m := range v {
			out[i] = m
		}
		return out
	}
	return nil
}

// Compact JSON without HTML escaping, mirroring ensure_ascii false output
func jsonCompact(v any) string {
	b, err := pyjson.EncodeWTF8(v)
	if err != nil {
		return ErrorResult(err)
	}
	return string(b)
}

// Two space indented JSON without HTML escaping
func jsonIndent(v any) string {
	b, err := pyjson.EncodeWTF8Indent(v, "", "  ")
	if err != nil {
		return ErrorResult(err)
	}
	return string(b)
}

// Error body bounded so a raw exception cannot bloat history across retries
func boundErrorText(text string) string {
	if len(text) <= maxToolErrorChars {
		return text
	}
	return text[:maxToolErrorChars] + toolErrorTruncationMarker
}

// Error envelope with the error key first and extra keys sorted after it
func toolErrorJSON(message string, extra map[string]any) string {
	var buf bytes.Buffer
	buf.WriteString(`{"error":`)
	buf.WriteString(jsonCompact(boundErrorText(message)))
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		buf.WriteString(",")
		buf.WriteString(jsonCompact(k))
		buf.WriteString(":")
		buf.WriteString(jsonCompact(extra[k]))
	}
	buf.WriteString("}")
	return buf.String()
}

// Secret from the .env layer and process environment, trimmed
func secretValue(env *Env, name string) string {
	if env != nil && env.Secrets != nil {
		return strings.TrimSpace(env.Secrets.String(name))
	}
	return strings.TrimSpace(os.Getenv(name))
}

// Logger of the session or the process default
func envLog(env *Env) *slog.Logger {
	if env != nil && env.Log != nil {
		return env.Log
	}
	return slog.Default()
}

// Web section of the merged configuration
func webCfg(env *Env) config.Tree {
	if env == nil || env.Config == nil {
		return config.Tree{}
	}
	return env.Config.Sub("web")
}

// Lower cased web.<key> selection, empty when unset
func webBackendKey(env *Env, key string) string {
	return pystr.Strip(pystr.Lower(webCfg(env).String(key, "")))
}

// Module version from build info, dev when unknown
func icebathVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		v := strings.TrimSpace(info.Main.Version)
		if v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return "dev"
}

// User agent every outbound vendor request carries
func icebathUserAgent() string {
	return "icebath/" + icebathVersion()
}

// Random 32 hex character identifier
func randomHex32() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Web cache directory under the home, created on first use
func webCacheDir(env *Env) (string, error) {
	dir := filepath.Join(home.CacheDir(env.Home), "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Reports whether the string names an enabled value
func truthyString(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	}
	return false
}

// Reports whether a config value is truthy the way hermes is_truthy_value does
func truthyValue(v any, fallback bool) bool {
	switch x := v.(type) {
	case nil:
		return fallback
	case string:
		return truthyString(x)
	case bool:
		return x
	}
	return config.Truthy(v)
}

// Shared transport so vendor calls reuse connections
var webHTTPTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	MaxIdleConns:          32,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 0,
	ForceAttemptHTTP2:     true,
}

// HTTP client with a whole request timeout
func webHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Transport: webHTTPTransport, Timeout: timeout}
}

// Response body decoded with the declared charset, else UTF-8
func responseText(resp *http.Response, body []byte) string {
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(ct), "charset=") {
		if _, params, err := parseMediaTypeParams(ct); err == nil {
			if label := params["charset"]; label != "" && !strings.EqualFold(label, "utf-8") && !strings.EqualFold(label, "utf8") {
				if r, err := charset.NewReaderLabel(label, bytes.NewReader(body)); err == nil {
					if decoded, err := io.ReadAll(r); err == nil {
						return strings.ToValidUTF8(string(decoded), "�")
					}
				}
			}
		}
	}
	return strings.ToValidUTF8(string(body), "�")
}

// Media type and its parameters without pulling in mime for one call site
func parseMediaTypeParams(v string) (string, map[string]string, error) {
	parts := strings.Split(v, ";")
	params := map[string]string{}
	for _, p := range parts[1:] {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) == 2 {
			params[strings.ToLower(strings.TrimSpace(kv[0]))] = strings.Trim(strings.TrimSpace(kv[1]), `"`)
		}
	}
	return strings.ToLower(strings.TrimSpace(parts[0])), params, nil
}

// Reads a response body bounded to a sane maximum
func readBodyLimited(resp *http.Response, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// Posts JSON and returns the status, decoded text, and headers
func postJSON(ctx context.Context, client *http.Client, target string, payload any, headers map[string]string) (int, string, http.Header, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", icebathUserAgent())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	raw, err := readBodyLimited(resp, 64<<20)
	if err != nil {
		return resp.StatusCode, "", resp.Header, err
	}
	return resp.StatusCode, responseText(resp, raw), resp.Header, nil
}

// Gets a URL with query parameters and returns the status and decoded text
func getText(ctx context.Context, client *http.Client, target string, params map[string]string, headers map[string]string) (int, string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return 0, "", err
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("User-Agent", icebathUserAgent())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, err := readBodyLimited(resp, 64<<20)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, responseText(resp, raw), nil
}

// Response body text for a failing reply, or HTTP code when the body is empty
func httpStatusDetail(status int, body string) string {
	if t := strings.TrimSpace(body); t != "" {
		return t
	}
	return fmt.Sprintf("HTTP %d", status)
}

// Map view of a decoded JSON value
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// List view of a decoded JSON value
func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// String view of a decoded JSON value, empty for non strings
func asStr(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(s)
	case json.Number:
		return s.String()
	}
	return ""
}

// Decodes JSON text keeping numbers as float64
func decodeJSON(text string) (any, error) {
	var v any
	err := json.Unmarshal([]byte(text), &v)
	return v, err
}

// Joined string list from a decoded JSON list
func joinStrings(v any, sep string) string {
	parts := []string{}
	for _, item := range asList(v) {
		if s := asStr(item); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, sep)
}

// ---- result shapes -------------------------------------------------------------------------------

// WebSearchHit is one search row, title first for the brave, searxng, ddgs, tavily and xai wire shape
type WebSearchHit struct {
	Title       string
	URL         string
	Description string
	Position    int
	TitleFirst  bool
}

// MarshalJSON keeps the historical key order of each wire shape
func (h WebSearchHit) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{")
	if h.TitleFirst {
		buf.WriteString(`"title":` + jsonCompact(h.Title) + `,"url":` + jsonCompact(h.URL))
	} else {
		buf.WriteString(`"url":` + jsonCompact(h.URL) + `,"title":` + jsonCompact(h.Title))
	}
	buf.WriteString(`,"description":` + jsonCompact(h.Description))
	buf.WriteString(`,"position":` + strconv.Itoa(h.Position) + "}")
	return buf.Bytes(), nil
}

// UnmarshalJSON reads either key order back into a hit
func (h *WebSearchHit) UnmarshalJSON(data []byte) error {
	var raw struct {
		Title       string `json:"title"`
		URL         string `json:"url"`
		Description string `json:"description"`
		Position    int    `json:"position"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	h.Title, h.URL, h.Description, h.Position = raw.Title, raw.URL, raw.Description, raw.Position
	return nil
}

// WebSearchData is the data block of a successful search
type WebSearchData struct {
	Web          []WebSearchHit `json:"web"`
	ServedBy     string         `json:"served_by,omitempty"`
	RescuedFrom  string         `json:"rescued_from,omitempty"`
	FallbackFrom string         `json:"fallback_from,omitempty"`
	BackendError string         `json:"backend_error,omitempty"`
}

// WebSearchResponse is the search contract every provider returns
type WebSearchResponse struct {
	Success bool
	Data    *WebSearchData
	Error   string
}

// MarshalJSON writes success, then data, then error
func (r *WebSearchResponse) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"success":` + strconv.FormatBool(r.Success))
	if r.Data != nil {
		b, err := json.Marshal(r.Data)
		if err != nil {
			return nil, err
		}
		buf.WriteString(`,"data":`)
		buf.Write(b)
	}
	if r.Error != "" {
		buf.WriteString(`,"error":` + jsonCompact(r.Error))
	}
	buf.WriteString("}")
	return buf.Bytes(), nil
}

// UnmarshalJSON reads a response back from cached JSON
func (r *WebSearchResponse) UnmarshalJSON(data []byte) error {
	var raw struct {
		Success bool           `json:"success"`
		Data    *WebSearchData `json:"data"`
		Error   string         `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.Success, r.Data, r.Error = raw.Success, raw.Data, raw.Error
	return nil
}

// Clone returns an independent copy
func (r *WebSearchResponse) Clone() *WebSearchResponse {
	if r == nil {
		return nil
	}
	out := &WebSearchResponse{Success: r.Success, Error: r.Error}
	if r.Data != nil {
		d := *r.Data
		d.Web = append([]WebSearchHit(nil), r.Data.Web...)
		out.Data = &d
	}
	return out
}

// Data block created on demand
func (r *WebSearchResponse) data() *WebSearchData {
	if r.Data == nil {
		r.Data = &WebSearchData{Web: []WebSearchHit{}}
	}
	return r.Data
}

func searchOK(hits []WebSearchHit) *WebSearchResponse {
	if hits == nil {
		hits = []WebSearchHit{}
	}
	return &WebSearchResponse{Success: true, Data: &WebSearchData{Web: hits}}
}

func searchFail(err string) *WebSearchResponse {
	return &WebSearchResponse{Success: false, Error: err}
}

// Url first row shape used by exa, parallel, keenable, firecrawl and perplexity
func webHit(u, title, description string, position int) WebSearchHit {
	return WebSearchHit{URL: u, Title: title, Description: description, Position: position}
}

// Title first row shape used by brave, searxng, ddgs, tavily and xai
func titleHit(title, u, description string, position int) WebSearchHit {
	return WebSearchHit{Title: title, URL: u, Description: description, Position: position, TitleFirst: true}
}

// WebPage is one extract entry as providers report it before trimming
type WebPage struct {
	URL             string
	Title           string
	Content         string
	RawContent      string
	HasRaw          bool
	Error           string
	Metadata        map[string]any
	BlockedByPolicy map[string]any
	Cached          bool
	FinalURL        string
}

// Successful extract entry whose raw content mirrors content
func webDocument(u, title, content string, sourceURL string) *WebPage {
	if sourceURL == "" {
		sourceURL = u
	}
	return &WebPage{URL: u, Title: title, Content: content, RawContent: content, HasRaw: true,
		Metadata: map[string]any{"sourceURL": sourceURL, "title": title}}
}

func pageError(u, err string) *WebPage {
	return &WebPage{URL: u, Error: err}
}

func extractFail(urls []string, err string) []*WebPage {
	out := make([]*WebPage, 0, len(urls))
	for _, u := range urls {
		out = append(out, pageError(u, err))
	}
	return out
}

// Content the truncate pipeline reads, raw first then content
func (p *WebPage) body() string {
	if p.RawContent != "" {
		return p.RawContent
	}
	return p.Content
}

// ---- origin HTTP status ----------------------------------------------------------------------------

var originHTTPErrorRe = regexp.MustCompile(`^HTTP \d{3} from origin(?::|$)`)

// Reports whether an error string names the target page's own HTTP answer
func isOriginHTTPError(err string) bool {
	return err != "" && originHTTPErrorRe.MatchString(err)
}

// Error text when the target answered 400 or above, else empty
func originHTTPError(status any, reason any) string {
	code, ok := statusCode(status)
	if !ok || code < 400 {
		return ""
	}
	detail := strings.TrimSpace(asStr(reason))
	if detail != "" {
		return fmt.Sprintf("HTTP %d from origin: %s", code, detail)
	}
	return fmt.Sprintf("HTTP %d from origin", code)
}

// Integer status from a decoded JSON value, false for booleans and junk
func statusCode(status any) (int, bool) {
	switch v := status.(type) {
	case bool, nil:
		return 0, false
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil
	}
	return 0, false
}

var exaCrawlStatusRe = regexp.MustCompile(`\bCRAWL_(?:HTTP_(\d{3})|(NOT_FOUND))\s*$`)

// Origin status an Exa crawl error tag ends with, 404 for CRAWL_NOT_FOUND
func exaCrawlStatus(text string) (int, bool) {
	m := exaCrawlStatusRe.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	if m[1] != "" {
		n, _ := strconv.Atoi(m[1])
		return n, true
	}
	return 404, true
}

var keenableOriginMessageRe = regexp.MustCompile(`(?i)^the (?:target server|page does not exist)\b`)

// Origin error when a Keenable fetch error describes the target's answer
func keenableOriginError(status int, body string) string {
	v, err := decodeJSON(body)
	if err != nil {
		return ""
	}
	message := strings.TrimSpace(asStr(asMap(v)["message"]))
	if !keenableOriginMessageRe.MatchString(message) {
		return ""
	}
	return originHTTPError(status, message)
}

// Marks an extract entry whose target answered 400 or above, keeping the body as content
func withOriginStatus(p *WebPage, status any, reason any) *WebPage {
	if e := originHTTPError(status, reason); e != "" {
		p.Error = e
	}
	return p
}

// Per URL failure from a vendor's own error list
func vendorErrorEntry(u string, status any, reason any, body any) *WebPage {
	var entry *WebPage
	if origin := originHTTPError(status, nil); origin != "" {
		entry = pageError(u, origin)
		entry.Content = asStr(body)
	} else {
		msg := asStr(body)
		if msg == "" {
			msg = asStr(reason)
		}
		if msg == "" {
			msg = "extraction failed"
		}
		entry = pageError(u, msg)
	}
	entry.Metadata = map[string]any{"sourceURL": u}
	return entry
}

// ---- guarded execution ------------------------------------------------------------------------------

// verbatimError carries a pre formatted message returned to the model as is
type verbatimError struct{ msg string }

func (e *verbatimError) Error() string { return e.msg }

func verbatim(msg string) error { return &verbatimError{msg: msg} }

// Failure message for a vendor call, verbatim errors untouched
func failureMessage(vendor, kind string, err error, env *Env, verbatimValueError bool) string {
	var ve *verbatimError
	if verbatimValueError && errors.As(err, &ve) {
		return ve.msg
	}
	envLog(env).Warn(vendor+" "+kind+" error", "error", err.Error())
	return fmt.Sprintf("%s %s failed: %v", vendor, kind, err)
}

// Interrupt check plus uniform failure classification around a search body
func runSearch(ctx context.Context, env *Env, vendor string, body func() (*WebSearchResponse, error), verbatimValueError bool) *WebSearchResponse {
	if ctx.Err() != nil {
		return searchFail("Interrupted")
	}
	resp, err := body()
	if err != nil {
		return searchFail(failureMessage(vendor, "search", err, env, verbatimValueError))
	}
	return resp
}

// Interrupted entries for every requested url
func extractInterrupted(urls []string) []*WebPage {
	return extractFail(urls, "Interrupted")
}

// Interrupt check plus uniform failure classification around an extract body
func runExtract(ctx context.Context, env *Env, vendor string, urls []string, body func() ([]*WebPage, error), verbatimValueError bool) []*WebPage {
	if ctx.Err() != nil {
		return extractInterrupted(urls)
	}
	pages, err := body()
	if err != nil {
		return extractFail(urls, failureMessage(vendor, "extract", err, env, verbatimValueError))
	}
	return pages
}

// GET and parse JSON, or a search failure naming the vendor
func httpGetJSON(ctx context.Context, env *Env, label, target string, params, headers map[string]string, timeout time.Duration, reachTarget string) (any, *WebSearchResponse) {
	if reachTarget == "" {
		reachTarget = label
	}
	status, text, err := getText(ctx, webHTTPClient(timeout), target, params, headers)
	if err != nil {
		envLog(env).Warn(label+" request error", "error", err.Error())
		return nil, searchFail(fmt.Sprintf("Could not reach %s: %v", reachTarget, err))
	}
	if status >= 400 {
		envLog(env).Warn(label+" HTTP error", "status", status)
		return nil, searchFail(fmt.Sprintf("%s returned HTTP %d", label, status))
	}
	data, err := decodeJSON(text)
	if err != nil {
		envLog(env).Warn(label+" response parse error", "error", err.Error())
		return nil, searchFail(fmt.Sprintf("Could not parse %s response as JSON", label))
	}
	return data, nil
}

// Brave and SearXNG row normalizer with 1 based positions
func titledRows(raw []any, descriptionKey string) []WebSearchHit {
	out := make([]WebSearchHit, 0, len(raw))
	for i, item := range raw {
		m := asMap(item)
		out = append(out, titleHit(asStr(m["title"]), asStr(m["url"]), asStr(m[descriptionKey]), i+1))
	}
	return out
}

// ---- provider contract --------------------------------------------------------------------------------

// WebProvider is one web search or extract backend
type WebProvider interface {
	Name() string
	DisplayName() string
	// Cheap check only, no network, runs at registration and on every picker paint
	Available(env *Env) bool
	// Reports whether anonymous free tier calls can serve without credentials
	KeylessAvailable(env *Env) bool
	SupportsSearch() bool
	SupportsExtract() bool
	// Search returns the response or a pre flight error the dispatcher wraps
	Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error)
	// Extract returns one entry per url or a whole batch error eligible for rescue
	Extract(ctx context.Context, env *Env, urls []string, format string) ([]*WebPage, error)
}

// Shared identity and availability plumbing every bundled provider embeds
type baseWebProvider struct {
	name    string
	display string
	keyEnv  string
	extract bool
	keyless bool
}

func (b *baseWebProvider) Name() string         { return b.name }
func (b *baseWebProvider) DisplayName() string  { return b.display }
func (b *baseWebProvider) SupportsSearch() bool { return true }
func (b *baseWebProvider) SupportsExtract() bool {
	return b.extract
}

// Available ignores the keyless tier so the legacy walk never routes keyed users onto a free tier
func (b *baseWebProvider) Available(env *Env) bool {
	return b.keyEnv != "" && secretValue(env, b.keyEnv) != ""
}

// Keyless vendors are free tier capable unless pinned paid
func (b *baseWebProvider) KeylessAvailable(env *Env) bool {
	return b.keyless && keylessEnabled(env) && providerTier(env, b.name) != "paid"
}

func (b *baseWebProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	return nil, fmt.Errorf("%s does not support search (override supports_search)", b.name)
}

func (b *baseWebProvider) Extract(ctx context.Context, env *Env, urls []string, format string) ([]*WebPage, error) {
	return nil, fmt.Errorf("%s does not support extract (override supports_extract)", b.name)
}

var webRegistry = struct {
	mu        sync.RWMutex
	providers map[string]WebProvider
}{providers: map[string]WebProvider{}}

// RegisterWebProvider adds or replaces a web provider by name
func RegisterWebProvider(p WebProvider) {
	webRegistry.mu.Lock()
	defer webRegistry.mu.Unlock()
	webRegistry.providers[p.Name()] = p
}

// Normalized plugin key comparison form
func normPluginName(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}

// Reports whether plugins.disabled lists the bundled web plugin of a provider
func webPluginDisabled(env *Env, name string) bool {
	if env == nil || env.Config == nil {
		return false
	}
	want := normPluginName(name)
	for _, key := range env.Config.Strings("plugins.disabled") {
		k := strings.ToLower(strings.TrimSpace(key))
		if strings.HasPrefix(k, "web/") && normPluginName(k[len("web/"):]) == want {
			return true
		}
	}
	return false
}

// Registered provider by name, nil when unknown or disabled in config
func webProvider(env *Env, name string) WebProvider {
	if name == "" || webPluginDisabled(env, name) {
		return nil
	}
	webRegistry.mu.RLock()
	defer webRegistry.mu.RUnlock()
	return webRegistry.providers[name]
}

// Every enabled registered provider sorted by name
func listWebProviders(env *Env) []WebProvider {
	webRegistry.mu.RLock()
	names := make([]string, 0, len(webRegistry.providers))
	for n := range webRegistry.providers {
		names = append(names, n)
	}
	webRegistry.mu.RUnlock()
	sort.Strings(names)
	out := make([]WebProvider, 0, len(names))
	for _, n := range names {
		if p := webProvider(env, n); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// Plugin key of a disabled bundled web plugin that would have served the configured backend
func disabledWebPluginFor(env *Env, configured, capability string) string {
	if configured == "" && (capability == "search" || capability == "extract") {
		configured = registryConfiguredBackend(env, capability)
	}
	if configured == "" {
		return ""
	}
	want := normPluginName(configured)
	for _, key := range env.Config.Strings("plugins.disabled") {
		k := strings.ToLower(strings.TrimSpace(key))
		if strings.HasPrefix(k, "web/") && normPluginName(k[len("web/"):]) == want {
			return k
		}
	}
	return ""
}

// web.<capability>_backend, else web.backend, the managed nous selection naming perplexity or firecrawl
func registryConfiguredBackend(env *Env, capability string) string {
	configured := webBackendKey(env, capability+"_backend")
	if configured == "" {
		configured = webBackendKey(env, "backend")
	}
	if configured == nousManagedProvider {
		if capability == "search" {
			return "perplexity"
		}
		return "firecrawl"
	}
	return configured
}

// Paid providers first so existing paid setups never downgrade to a free tier on upgrade
var legacyWebPreference = []string{"firecrawl", "parallel", "tavily", "perplexity", "exa", "searxng", "brave-free", "ddgs"}

// Provider availability without letting a broken probe propagate
func providerAvailable(env *Env, p WebProvider) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			envLog(env).Debug("web provider is_available panicked", "provider", p.Name(), "panic", fmt.Sprint(r))
			ok = false
		}
	}()
	return p.Available(env)
}

// Provider keyless availability without letting a broken probe propagate
func providerKeylessAvailable(env *Env, p WebProvider) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	return p.KeylessAvailable(env)
}

func providerCapable(p WebProvider, capability string) bool {
	switch capability {
	case "search":
		return p.SupportsSearch()
	case "extract":
		return p.SupportsExtract()
	}
	return false
}

// Active provider for a capability by explicit config, single eligible, legacy walk, then keyless walk
func resolveWebProvider(env *Env, configured, capability string) WebProvider {
	if configured != "" {
		if p := webProvider(env, configured); p != nil && providerCapable(p, capability) {
			return p
		}
	}
	eligible := []WebProvider{}
	for _, p := range listWebProviders(env) {
		if providerCapable(p, capability) && providerAvailable(env, p) {
			eligible = append(eligible, p)
		}
	}
	if len(eligible) == 1 {
		return eligible[0]
	}
	for _, legacy := range legacyWebPreference {
		for _, p := range eligible {
			if p.Name() == legacy {
				return p
			}
		}
	}
	if keylessEnabled(env) {
		for _, name := range keylessPreference() {
			p := webProvider(env, name)
			if p == nil || !providerCapable(p, capability) {
				continue
			}
			if providerKeylessAvailable(env, p) {
				return p
			}
		}
	}
	return nil
}

func activeSearchProvider(env *Env) WebProvider {
	return resolveWebProvider(env, registryConfiguredBackend(env, "search"), "search")
}

func activeExtractProvider(env *Env) WebProvider {
	return resolveWebProvider(env, registryConfiguredBackend(env, "extract"), "extract")
}

// web.keyless_fallback, default enabled
func keylessEnabled(env *Env) bool {
	return truthyValue(webCfg(env).Get("keyless_fallback"), true)
}

// web.provider_tier.<name>, free, paid, or auto
func providerTier(env *Env, name string) string {
	tiers := webCfg(env).Map("provider_tier")
	value := strings.ToLower(strings.TrimSpace(asStr(tiers[name])))
	if value == "free" || value == "paid" {
		return value
	}
	return "auto"
}

// Single chokepoint deciding whether a vendor call routes through its anonymous tier
func useKeyless(env *Env, name, apiKey string) bool {
	switch providerTier(env, name) {
	case "free":
		return true
	case "paid":
		return false
	}
	return apiKey == "" && keylessEnabled(env)
}

// Reports whether any web backend key names the provider
func webConfigSelects(env *Env, name string) bool {
	for _, key := range []string{"backend", "search_backend", "extract_backend"} {
		if webBackendKey(env, key) == name {
			return true
		}
	}
	return false
}

// ---- keyless ring -------------------------------------------------------------------------------------

const (
	exaMCPURL      = "https://mcp.exa.ai/mcp"
	parallelMCPURL = "https://search.parallel.ai/mcp"
	keenableAPIURL = "https://api.keenable.ai"
	keenableTitle  = "icebath"
)

const keylessTimeout = 30 * time.Second

// Parallel free tier rate limit correlation id, random per process and never persisted
var keylessSessionID = randomHex32()

// KeylessMCPError reports a failed keyless MCP call
type KeylessMCPError struct{ msg string }

func (e *KeylessMCPError) Error() string { return e.msg }

func keylessErr(format string, a ...any) error {
	return &KeylessMCPError{msg: fmt.Sprintf(format, a...)}
}

var rateLimitMarkers = []string{"rate limit", "rate-limit", "ratelimit", "too many requests", "429", "quota exceeded", "slow down"}

type keylessVendorHint struct{ label, envKey, site string }

var keylessVendorHints = map[string]keylessVendorHint{
	"exa":       {"Exa", "EXA_API_KEY", "https://exa.ai"},
	"parallel":  {"Parallel", "PARALLEL_API_KEY", "https://parallel.ai"},
	"firecrawl": {"Firecrawl", "FIRECRAWL_API_KEY", "https://firecrawl.dev"},
	"keenable":  {"Keenable", "KEENABLE_API_KEY", "https://keenable.ai"},
}

// Reports whether an error message looks like free tier throttling
func isRateLimitish(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range rateLimitMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

var vendorRefusalRe = regexp.MustCompile(`(?i)^\s*(?:keyless\s+\w+\s+search\s+failed:\s*)?(?:http(?:\s+status)?|status(?:\s+code)?|client\s+error|error\s+code)\s*[:=']*\s*(?:40[123]|5\d\d)\b`)

// Reports whether another anonymous vendor may serve the search
func isSearchFailoverEligible(message string) bool {
	return isRateLimitish(message) || vendorRefusalRe.MatchString(message)
}

// Standard hint when a keyless vendor call fails
func keylessFailMsg(vendor, kind string, err any, otherBackends bool) string {
	hint := keylessVendorHints[vendor]
	alt := ""
	if otherBackends {
		alt = " or another web backend via `icebath tools`"
	}
	return fmt.Sprintf("Keyless %s %s failed: %v. Set %s (%s)%s for reliable service.", hint.label, kind, err, hint.envKey, hint.site, alt)
}

// Parses an MCP tools/call body, plain JSON or SSE data frames, into its first text content
func parseMCPBody(body string) (string, error) {
	fromPayload := func(payload string) (string, bool, error) {
		payload = strings.TrimSpace(payload)
		if !strings.HasPrefix(payload, "{") {
			return "", false, nil
		}
		v, err := decodeJSON(payload)
		if err != nil {
			return "", false, err
		}
		data := asMap(v)
		if errObj := data["error"]; errObj != nil && !isEmptyJSON(errObj) {
			if m := asMap(errObj); m != nil && asStr(m["message"]) != "" {
				return "", true, keylessErr("%s", asStr(m["message"]))
			}
			return "", true, keylessErr("%s", jsonCompact(errObj))
		}
		result := asMap(data["result"])
		texts := []string{}
		for _, c := range asList(result["content"]) {
			if cm := asMap(c); cm != nil {
				texts = append(texts, asStr(cm["text"]))
			}
		}
		if config.Truthy(result["isError"]) {
			joined := []string{}
			for _, t := range texts {
				if t != "" {
					joined = append(joined, t)
				}
			}
			msg := strings.Join(joined, " ")
			if msg == "" {
				msg = "MCP tool call failed"
			}
			return "", true, keylessErr("%s", msg)
		}
		for _, t := range texts {
			if t != "" {
				return t, true, nil
			}
		}
		return "", true, nil
	}
	stripped := strings.TrimSpace(body)
	candidates := []string{}
	if strings.HasPrefix(stripped, "{") {
		candidates = append(candidates, stripped)
	}
	for _, line := range regexp.MustCompile(`\r\n|\r|\n`).Split(body, -1) {
		if strings.HasPrefix(line, "data: ") {
			candidates = append(candidates, line[len("data: "):])
		}
	}
	envelopeSeen := false
	for _, candidate := range candidates {
		text, isEnvelope, err := fromPayload(candidate)
		if err != nil {
			var ke *KeylessMCPError
			if errors.As(err, &ke) {
				return "", err
			}
			continue
		}
		if text != "" {
			return text, nil
		}
		envelopeSeen = envelopeSeen || isEnvelope
	}
	if envelopeSeen {
		return "", keylessErr("MCP response contained no text content")
	}
	return "", keylessErr("Unrecognized MCP response shape")
}

// Reports whether a decoded JSON value is falsy the way Python treats it
func isEmptyJSON(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// Posts a JSON-RPC tools/call and returns the text payload
func mcpCall(ctx context.Context, target, tool string, arguments map[string]any) (string, error) {
	payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments}}
	status, text, _, err := postJSON(ctx, webHTTPClient(keylessTimeout), target, payload, map[string]string{"Accept": "application/json, text/event-stream"})
	if err != nil {
		return "", keylessErr("request failed: %v", err)
	}
	if status >= 400 {
		return "", keylessErr("HTTP %d: %s", status, truncateRunes(text, 300))
	}
	return parseMCPBody(text)
}

// First n characters of a string by rune
func truncateRunes(s string, n int) string { return runePrefix(s, n) }

// Keyless search body with the standard hint on MCP errors and a formatter for other errors
func keylessSearch(vendor string, rows func() ([]WebSearchHit, error), format func(error) string) *WebSearchResponse {
	hits, err := rows()
	if err != nil {
		var ke *KeylessMCPError
		if errors.As(err, &ke) {
			return searchFail(keylessFailMsg(vendor, "search", err, true))
		}
		if format != nil {
			return searchFail(format(err))
		}
		return searchFail(keylessFailMsg(vendor, "search", err, true))
	}
	return searchOK(hits)
}

// Per url extract loop turning a fetch failure into an error entry
func keylessPerURL(urls []string, fetch func(string) (*WebPage, error), vendor string, hint bool) []*WebPage {
	out := make([]*WebPage, 0, len(urls))
	for _, u := range urls {
		page, err := fetch(u)
		if err != nil {
			out = append(out, pageError(u, keylessFailMsg(vendor, "extract", err, hint)))
			continue
		}
		out = append(out, page)
	}
	return out
}

// Parallel keyless search over the public MCP endpoint
func parallelSearchKeyless(ctx context.Context, query string, limit int) *WebSearchResponse {
	return keylessSearch("parallel", func() ([]WebSearchHit, error) {
		text, err := mcpCall(ctx, parallelMCPURL, "web_search", map[string]any{"objective": query, "search_queries": []string{query}, "session_id": keylessSessionID})
		if err != nil {
			return nil, err
		}
		v, err := decodeJSON(text)
		if err != nil {
			return nil, &payloadError{err}
		}
		results := asList(asMap(v)["results"])
		if limit > 0 && len(results) > limit {
			results = results[:limit]
		}
		hits := []WebSearchHit{}
		for i, r := range results {
			m := asMap(r)
			hits = append(hits, webHit(asStr(m["url"]), asStr(m["title"]), joinStrings(m["excerpts"], " "), i+1))
		}
		return hits, nil
	}, func(err error) string {
		return fmt.Sprintf("Keyless Parallel search returned an unexpected payload: %v", err)
	})
}

// payloadError marks a vendor payload that did not parse
type payloadError struct{ err error }

func (e *payloadError) Error() string { return e.err.Error() }

// Parallel keyless extract over the public MCP endpoint
func parallelExtractKeyless(ctx context.Context, urls []string) []*WebPage {
	text, err := mcpCall(ctx, parallelMCPURL, "web_fetch", map[string]any{"urls": urls, "objective": "Full page content", "session_id": keylessSessionID})
	var data map[string]any
	if err == nil {
		v, derr := decodeJSON(text)
		if derr != nil {
			err = derr
		} else {
			data = asMap(v)
		}
	}
	if err != nil {
		return extractFail(urls, keylessFailMsg("parallel", "extract", err, true))
	}
	results := []*WebPage{}
	for _, r := range asList(data["results"]) {
		m := asMap(r)
		content := asStr(m["full_content"])
		if content == "" {
			content = asStr(m["content"])
		}
		if content == "" {
			content = joinStrings(m["excerpts"], "\n\n")
		}
		results = append(results, webDocument(asStr(m["url"]), asStr(m["title"]), content, ""))
	}
	for _, e := range asList(data["errors"]) {
		m := asMap(e)
		results = append(results, vendorErrorEntry(asStr(m["url"]), m["http_status_code"], m["error_type"], m["content"]))
	}
	seen := map[string]bool{}
	for _, r := range results {
		seen[r.URL] = true
	}
	for _, u := range urls {
		if !seen[u] {
			results = append(results, pageError(u, "no content returned"))
		}
	}
	return results
}

var exaLabels = []string{"Title:", "URL:", "Highlights:", "Published:", "Author:"}

// Parses Exa's --- separated Title URL Published Author Highlights blocks
func parseExaSearchText(text string, limit int) []WebSearchHit {
	results := []WebSearchHit{}
	for _, block := range strings.Split(text, "\n---\n") {
		title, u := "", ""
		highlights := []string{}
		inHighlights := false
		for _, line := range strings.Split(block, "\n") {
			stripped := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(stripped, "Title:"):
				title = strings.TrimSpace(stripped[len("Title:"):])
			case strings.HasPrefix(stripped, "URL:"):
				u = strings.TrimSpace(stripped[len("URL:"):])
			case inHighlights && stripped != "" && !hasAnyPrefix(stripped, exaLabels):
				highlights = append(highlights, stripped)
			}
			if hasAnyPrefix(stripped, exaLabels) {
				inHighlights = strings.HasPrefix(stripped, "Highlights:")
			}
		}
		if u != "" {
			results = append(results, webHit(u, title, strings.Join(highlights, " "), len(results)+1))
		}
		if limit > 0 && len(results) >= limit {
			break
		}
	}
	return results
}

// Exa keyless search over the public MCP endpoint
func exaSearchKeyless(ctx context.Context, query string, limit int) *WebSearchResponse {
	n := limit
	if n < 1 {
		n = 1
	}
	return keylessSearch("exa", func() ([]WebSearchHit, error) {
		text, err := mcpCall(ctx, exaMCPURL, "web_search_exa", map[string]any{"query": query, "numResults": n})
		if err != nil {
			return nil, err
		}
		return parseExaSearchText(text, limit), nil
	}, nil)
}

// Exa keyless extract, one MCP call per url
func exaExtractKeyless(ctx context.Context, urls []string) []*WebPage {
	fetch := func(u string) (*WebPage, error) {
		text, err := mcpCall(ctx, exaMCPURL, "web_fetch_exa", map[string]any{"urls": []string{u}})
		if err != nil {
			if code, ok := exaCrawlStatus(err.Error()); ok {
				if origin := originHTTPError(code, nil); origin != "" {
					return pageError(u, origin), nil
				}
			}
			return nil, err
		}
		title := ""
		for _, line := range strings.Split(text, "\n") {
			s := strings.TrimSpace(line)
			if strings.HasPrefix(s, "# ") {
				title = strings.TrimSpace(s[2:])
				break
			}
			if strings.HasPrefix(s, "Title:") {
				title = strings.TrimSpace(s[len("Title:"):])
				break
			}
		}
		return webDocument(u, title, text, ""), nil
	}
	return keylessPerURL(urls, fetch, "exa", true)
}

// Firecrawl keyless search over the public cloud without an authorization header
func firecrawlSearchKeyless(ctx context.Context, query string, limit int) *WebSearchResponse {
	return keylessSearch("firecrawl", func() ([]WebSearchHit, error) {
		client := newKeylessFirecrawlClient(firecrawlCloudAPIURL)
		resp, err := client.search(ctx, query, limit)
		if err != nil {
			return nil, err
		}
		return firecrawlWebSearchResults(resp), nil
	}, func(err error) string { return keylessFailMsg("firecrawl", "search", err, true) })
}

// Firecrawl keyless extract reporting the post redirect page so dispatch re gates it
func firecrawlExtractKeyless(ctx context.Context, urls []string) []*WebPage {
	client := newKeylessFirecrawlClient(firecrawlCloudAPIURL)
	fetch := func(u string) (*WebPage, error) {
		raw, err := client.scrape(ctx, u, []string{"markdown"}, 0)
		if err != nil {
			return nil, err
		}
		payload := firecrawlScrapePayload(raw)
		metadata := asMap(payload["metadata"])
		if metadata == nil {
			metadata = map[string]any{}
		}
		content := asStr(payload["markdown"])
		if content == "" {
			content = asStr(payload["html"])
		}
		page := webDocument(firecrawlFinalURL(metadata, u), asStr(metadata["title"]), content, u)
		return firecrawlWithScrapeStatus(page, metadata), nil
	}
	return keylessPerURL(urls, fetch, "firecrawl", false)
}

// Calls a Keenable public endpoint with the mandatory app identifier header
func keenableRequest(ctx context.Context, method, path string, params map[string]string, payload any) (map[string]any, error) {
	headers := map[string]string{"X-Keenable-Title": keenableTitle}
	var status int
	var text string
	var err error
	if method == http.MethodPost {
		status, text, _, err = postJSON(ctx, webHTTPClient(keylessTimeout), keenableAPIURL+path, payload, headers)
	} else {
		status, text, err = getText(ctx, webHTTPClient(keylessTimeout), keenableAPIURL+path, params, headers)
	}
	if err != nil {
		return nil, keylessErr("request failed: %v", err)
	}
	if status >= 400 {
		body := strings.TrimSpace(text)
		if origin := keenableOriginError(status, body); origin != "" {
			return nil, keylessErr("%s", origin)
		}
		return nil, keylessErr("HTTP %d: %s", status, truncateRunes(body, 300))
	}
	v, err := decodeJSON(text)
	if err != nil {
		return nil, keylessErr("invalid JSON: %v", err)
	}
	return asMap(v), nil
}

// Keenable keyless search over the public search endpoint
func keenableSearchKeyless(ctx context.Context, query string, limit int) *WebSearchResponse {
	n := limit
	if n < 1 {
		n = 1
	}
	return keylessSearch("keenable", func() ([]WebSearchHit, error) {
		data, err := keenableRequest(ctx, http.MethodPost, "/v1/search/public", nil, map[string]any{"query": query, "max_results": n})
		if err != nil {
			return nil, err
		}
		hits := []WebSearchHit{}
		for i, r := range asList(data["results"]) {
			m := asMap(r)
			desc := asStr(m["snippet"])
			if desc == "" {
				desc = asStr(m["description"])
			}
			hits = append(hits, webHit(asStr(m["url"]), asStr(m["title"]), desc, i+1))
		}
		return hits, nil
	}, func(err error) string { return fmt.Sprintf("Keyless Keenable search failed: %v.", err) })
}

// Keenable keyless extract over the public fetch endpoint
func keenableExtractKeyless(ctx context.Context, urls []string) []*WebPage {
	fetch := func(u string) (*WebPage, error) {
		data, err := keenableRequest(ctx, http.MethodGet, "/v1/fetch/public", map[string]string{"url": u}, nil)
		if err != nil {
			if isOriginHTTPError(err.Error()) {
				return pageError(u, err.Error()), nil
			}
			return nil, err
		}
		final := asStr(data["url"])
		if final == "" {
			final = u
		}
		return webDocument(final, asStr(data["title"]), asStr(data["content"]), u), nil
	}
	return keylessPerURL(urls, fetch, "keenable", false)
}

// Round robin ring of anonymous vendors
var keylessRing = []string{"exa", "parallel", "firecrawl", "keenable"}

var keylessSearchers = map[string]func(context.Context, string, int) *WebSearchResponse{
	"exa": exaSearchKeyless, "parallel": parallelSearchKeyless, "firecrawl": firecrawlSearchKeyless, "keenable": keenableSearchKeyless,
}

var keylessExtractors = map[string]func(context.Context, []string) []*WebPage{
	"exa": exaExtractKeyless, "parallel": parallelExtractKeyless, "firecrawl": firecrawlExtractKeyless, "keenable": keenableExtractKeyless,
}

var ringMu sync.Mutex

// Per process cursor seeded from the random session id so a fleet spreads across vendors
var ringCursor = func() int {
	n := new(big.Int)
	n.SetString(keylessSessionID, 16)
	return int(new(big.Int).Mod(n, big.NewInt(int64(len(keylessRing)))).Int64())
}()

// Keyless walk order starting at the ring cursor so resolution and dispatch agree
func keylessPreference() []string {
	ringMu.Lock()
	start := ringCursor % len(keylessRing)
	ringMu.Unlock()
	return append(append([]string{}, keylessRing[start:]...), keylessRing[:start]...)
}

// Reports whether config routes web traffic to the vendor by backend key or a free tier pin
func vendorPinned(env *Env, name string) bool {
	return providerTier(env, name) == "free" || webConfigSelects(env, name)
}

// Vendor walk order, pinned vendors first else round robin advancing the cursor
func ringOrder(env *Env, name string) []string {
	start := 0
	if vendorPinned(env, name) {
		for i, v := range keylessRing {
			if v == name {
				start = i
			}
		}
	} else {
		ringMu.Lock()
		start = ringCursor
		ringCursor = (ringCursor + 1) % len(keylessRing)
		ringMu.Unlock()
	}
	ordered := append(append([]string{}, keylessRing[start:]...), keylessRing[:start]...)
	out := []string{}
	for _, v := range ordered {
		if providerTier(env, v) != "paid" {
			out = append(out, v)
		}
	}
	return out
}

const allPaidMsg = "All keyless web providers are pinned to paid tiers."

// Walks the ring for a search until a vendor is not throttled
func keylessSearchWithFailover(ctx context.Context, env *Env, name, query string, limit int) *WebSearchResponse {
	order := ringOrder(env, name)
	if len(order) == 0 {
		return searchFail(allPaidMsg)
	}
	var result *WebSearchResponse
	vendor := ""
	exhausted := true
	for i, v := range order {
		vendor = v
		result = keylessSearchers[v](ctx, query, limit)
		if result.Success || !isSearchFailoverEligible(result.Error) {
			exhausted = false
			break
		}
		if i+1 < len(order) {
			envLog(env).Info("keyless search unavailable, failing over", "vendor", v, "next", order[i+1])
		}
	}
	if exhausted {
		result.Error = fmt.Sprintf("%s (all keyless vendors unavailable: %s)", result.Error, strings.Join(order, ", "))
	} else if result.Success && vendor != name {
		result.data().ServedBy = vendor
	}
	return result
}

// Walks the ring for an extract only when every url came back rate limited
func keylessExtractWithFailover(ctx context.Context, env *Env, name string, urls []string) []*WebPage {
	order := ringOrder(env, name)
	if len(order) == 0 {
		return extractFail(urls, allPaidMsg)
	}
	throttled := func(err string) bool {
		return err != "" && !isOriginHTTPError(err) && isRateLimitish(err)
	}
	var results []*WebPage
	for i, v := range order {
		results = keylessExtractors[v](ctx, append([]string{}, urls...))
		all := len(results) > 0
		for _, r := range results {
			if !throttled(r.Error) {
				all = false
				break
			}
		}
		if !all {
			break
		}
		if i+1 < len(order) {
			envLog(env).Info("keyless extract unavailable, failing over", "vendor", v, "next", order[i+1])
		}
	}
	return results
}
