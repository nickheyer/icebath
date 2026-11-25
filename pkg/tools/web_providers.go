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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"
	"golang.org/x/net/html/charset"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/providerregistry"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

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

// Text of json.dumps with ensure_ascii off and separators comma and colon without spaces
func jsonCompact(v any) string {
	b, err := jsonutil.Append(nil, v)
	if err != nil {
		return ErrorResult(err)
	}
	return string(b)
}

// Two space indented JSON without HTML escaping
func jsonIndent(v any) string {
	b, err := jsonutil.Append(nil, v, jsonutil.Indent(2))
	if err != nil {
		return ErrorResult(err)
	}
	return string(b)
}

// Error text capped at the tool error budget in code points with the truncation marker appended
func boundErrorText(text string) string { return boundModelErrorText(nil, text) }

// Error text clamped to the display bound
func BoundErrorText(text string) string { return boundErrorText(text) }

// Error envelope with the bounded error first and the extra keys after it in their own order
func toolErrorJSON(message string, extra *jsonutil.Object) string {
	out := Dict("error", boundErrorText(message))
	if extra != nil {
		for _, k := range extra.Keys() {
			out.Set(k, extra.Value(k))
		}
	}
	return JSON(out)
}

// Secret from the .env layer and process environment, trimmed
func secretValue(env *Env, name string) string {
	if env != nil && env.Secrets != nil {
		return strings.TrimSpace(env.Secrets.String(name))
	}
	return strings.TrimSpace(os.Getenv(name))
}

// Logger of the session or the process default, named after this package
func envLog(env *Env) *slog.Logger {
	if env != nil && env.Log != nil {
		return env.Log.With("logger", logName)
	}
	return pkgLog()
}

// Context the web settings of the env's profile are read for
func webCtx(env *Env) context.Context { return envHomeCtx(context.Background(), env) }

// Lower cased web backend choice the key names (backend, search_backend or extract_backend), empty when unset
func webBackendKey(env *Env, key string) string {
	ctx := webCtx(env)
	value := ""
	switch key {
	case "backend":
		value = settings.WebSearch.Get(ctx)
	case "search_backend":
		value = settings.WebSearchBackend.Get(ctx)
	case "extract_backend":
		value = settings.WebExtractBackend.Get(ctx)
	}
	return strings.TrimSpace(strings.ToLower(value))
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

// Reports whether the stripped lower cased string is in the TRUTHY_STRINGS set
func truthyString(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Reports whether a config value is truthy
func truthyValue(v any, fallback bool) bool {
	switch x := v.(type) {
	case nil:
		return fallback
	case string:
		return truthyString(x)
	case bool:
		return x
	}
	return anyutil.IsTrue(v)
}

// Shared transport so vendor calls reuse connections
var webHTTPTransport = &http.Transport{
	DialContext:           netpref.DialContext((&net.Dialer{}).DialContext),
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
	return strings.ToValidUTF8(string(utf8Body(resp.Header, body)), "�")
}

// Body bytes transcoded to UTF-8 from the charset the Content-Type declares, unchanged when it declares UTF-8, none, or one no decoder knows
func utf8Body(header http.Header, body []byte) []byte {
	ct := header.Get("Content-Type")
	if strings.Contains(strings.ToLower(ct), "charset=") {
		if _, params, err := parseMediaTypeParams(ct); err == nil {
			if label := params["charset"]; label != "" && !strings.EqualFold(label, "utf-8") && !strings.EqualFold(label, "utf8") {
				if r, err := charset.NewReaderLabel(label, bytes.NewReader(body)); err == nil {
					if decoded, err := io.ReadAll(r); err == nil {
						return decoded
					}
				}
			}
		}
	}
	return body
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
	case int:
		return strconv.Itoa(s)
	case int64:
		return strconv.FormatInt(s, 10)
	}
	return ""
}

// json.loads of text into plain maps keeping Python int and float types, NaN and Infinity accepted
func decodePyJSON(text string) (any, error) {
	v, err := jsonutil.Decode(text)
	if err != nil {
		return nil, err
	}
	return jsonutil.Plain(v), nil
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
		b, err := jsonutil.Append(nil, r.Data)
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

// Items of a vendor's excerpt or highlight list joined by sep in their text form, null, false, zero and empty items left out, a lone value read as a list of one
func joinText(sep string, v any) string {
	items, ok := v.([]any)
	if !ok {
		items = []any{v}
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if anyutil.IsTrue(item) {
			parts = append(parts, anyutil.String(item))
		}
	}
	return strings.Join(parts, sep)
}

// Items of a vendor's list field, none for an absent or empty value, the TypeError of a value that is no list
func vendorList(v any) ([]any, error) {
	if !anyutil.IsTrue(v) {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, &typeError{"expected a list, got " + errchain.Kind(v)}
	}
	return items, nil
}

// Rank a vendor gives a search row as a whole number, 0 when it gives none or one that is no whole number (NaN, an infinity, a boolean, beyond int)
func vendorPosition(v any) int {
	if _, ok := v.(bool); ok {
		return 0
	}
	n, err := anyutil.ToInt(v)
	if err != nil || int64(int(n)) != n {
		return 0
	}
	return int(n)
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

// Compiles a Python pattern once on first use, panicking on a pattern the translator rejects
func pyreOnce(pattern string) func() *regexp2.Regexp {
	return sync.OnceValue(func() *regexp2.Regexp {
		re, err := regexutil.Compile(pattern)
		if err != nil {
			panic(err)
		}
		return re
	})
}

// Python re search of a compiled pattern, the match groups or nil
func pyreSearch(re *regexp2.Regexp, s string) []string {
	m, err := re.FindStringMatch(s)
	if err != nil || m == nil {
		return nil
	}
	groups := m.Groups()
	out := make([]string, len(groups))
	for i, g := range groups {
		out[i] = g.String()
	}
	return out
}

var originHTTPErrorRe = pyreOnce(`\A(?:HTTP \d{3} from origin(?::|$))`)

// Reports whether an error string names the target page's own HTTP answer
func isOriginHTTPError(err string) bool {
	return pyreSearch(originHTTPErrorRe(), err) != nil
}

// HTTP status code a vendor reports for the target page: a whole three digit code given as a number or as decimal text in any script; false for anything else, NaN, infinities and booleans included
func vendorHTTPStatus(v any) (int, bool) {
	if s, ok := v.(string); ok {
		v = textutil.ASCIIDigits(s)
	}
	code, err := anyutil.ToInt(v)
	if err != nil || code < 100 || code > 999 {
		return 0, false
	}
	return int(code), true
}

// Error text naming the origin's answer when status is an HTTP code of 400 or above, with the trimmed reason after it; empty for any other status
func originHTTPError(status any, reason any) string {
	code, ok := vendorHTTPStatus(status)
	if !ok || code < 400 {
		return ""
	}
	text := "HTTP " + strconv.Itoa(code) + " from origin"
	if detail := strings.TrimSpace(anyutil.String(anyutil.Or(reason, ""))); detail != "" {
		return text + ": " + detail
	}
	return text
}

var exaCrawlStatusRe = pyreOnce(`\bCRAWL_(?:HTTP_(\d{3})|(NOT_FOUND))\s*$`)

// Origin status an Exa crawl error tag ends with, 404 for CRAWL_NOT_FOUND, the digits read in any script
func exaCrawlStatus(text string) (int, bool) {
	m := pyreSearch(exaCrawlStatusRe(), text)
	if m == nil {
		return 0, false
	}
	if m[1] != "" {
		n, err := strconv.Atoi(textutil.ASCIIDigits(m[1]))
		return n, err == nil
	}
	return 404, true
}

var keenableOriginMessageRe = pyreOnce(`(?i)\A(?:the (?:target server|page does not exist)\b)`)

// Origin error when a Keenable fetch error body's message describes the target's answer, empty otherwise
func keenableOriginError(status int, body string) string {
	v, err := jsonutil.Decode(body)
	if err != nil {
		return ""
	}
	message := ""
	if d, ok := v.(*jsonutil.Object); ok {
		m, _ := d.Get("message")
		message = strings.TrimSpace(anyutil.String(anyutil.Or(m, "")))
	}
	if pyreSearch(keenableOriginMessageRe(), message) == nil {
		return ""
	}
	return originHTTPError(status, message)
}

// Marks an extract entry whose target answered 400 or above, keeping the body as content; an unusable status leaves the entry as it is
func withOriginStatus(p *WebPage, status any, reason any) *WebPage {
	if e := originHTTPError(status, reason); e != "" {
		p.Error = e
	}
	return p
}

// Per URL failure from a vendor's own error list, the origin status named when the vendor reports a usable one, else the vendor's body or reason
func vendorErrorEntry(u string, status any, reason any, body any) *WebPage {
	var entry *WebPage
	if origin := originHTTPError(status, nil); origin != "" {
		entry = pageError(u, origin)
		entry.Content = anyutil.String(anyutil.Or(body, ""))
	} else {
		entry = pageError(u, anyutil.String(anyutil.Or(body, reason, "extraction failed")))
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
	if IsInterrupted(ctx) || ctx.Err() != nil {
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
	if IsInterrupted(ctx) || ctx.Err() != nil {
		return extractInterrupted(urls)
	}
	pages, err := body()
	if err != nil {
		return extractFail(urls, failureMessage(vendor, "extract", err, env, verbatimValueError))
	}
	return pages
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
	if b.keyEnv == "" {
		return false
	}
	key, err := providerEnv(context.Background(), env, b.keyEnv)
	return err == nil && key != ""
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

// Web search providers by name, the global layer plus one layer per profile scope
var webRegistry = providerregistry.New[WebProvider](providerregistry.Options{Label: "Web", ProviderCls: "WebSearchProvider", Logger: pkgLog()})

// Untyped view of the web search registry the plugin context register_web_search_provider drives
func WebSearchRegistryScoped() providerregistry.ScopedRegistry { return webRegistry.Scoped() }

// Profile scope the env reads registrations under, the active home when env names none
func webScope(env *Env) string {
	if env == nil {
		return ""
	}
	return env.Home
}

// Normalized plugin key comparison form
func normPluginName(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}

// Registered provider by name in the env scope then globally, nil when no loaded plugin registered it
func webProvider(env *Env, name string) WebProvider {
	if name == "" {
		return nil
	}
	return webRegistry.GetProvider(context.Background(), name, webScope(env))
}

// GetWebProvider is the registry get_provider read of the env scope, nil when no loaded plugin registered it
func GetWebProvider(env *Env, name string) WebProvider { return webProvider(env, name) }

// Every registered provider visible in the env scope sorted by name
func listWebProviders(env *Env) []WebProvider {
	return append([]WebProvider{}, webRegistry.ListProviders(context.Background(), webScope(env))...)
}

// ListWebProviders is the registry list_providers read of the env scope
func ListWebProviders(env *Env) []WebProvider { return listWebProviders(env) }

// Plugin key of a disabled bundled web plugin that would have served the configured backend
func disabledWebPluginFor(env *Env, configured, capability string) string {
	if configured == "" && (capability == "search" || capability == "extract") {
		configured = registryConfiguredBackend(env, capability)
	}
	if configured == "" {
		return ""
	}
	want := normPluginName(configured)
	for _, key := range settings.PluginsDisabled.Strings(webCtx(env)) {
		k := strings.ToLower(strings.TrimSpace(key))
		if strings.HasPrefix(k, "web/") && normPluginName(k[len("web/"):]) == want {
			return k
		}
	}
	return ""
}

// web.<capability>_backend, else web.backend
func registryConfiguredBackend(env *Env, capability string) string {
	if configured := webBackendKey(env, capability+"_backend"); configured != "" {
		return configured
	}
	return webBackendKey(env, "backend")
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

// Web search provider the registry resolves for env, nil when none is available
func ActiveSearchProvider(env *Env) WebProvider { return activeSearchProvider(env) }

func activeExtractProvider(env *Env) WebProvider {
	return resolveWebProvider(env, registryConfiguredBackend(env, "extract"), "extract")
}

// Whether the free keyless tier may answer when a vendor has no key
func keylessEnabled(env *Env) bool {
	return settings.WebKeylessFallback.Get(webCtx(env))
}

// web.provider_tier.<name>, free, paid, or auto
func providerTier(env *Env, name string) string {
	tiers := settings.WebProviderTier.Get(webCtx(env))
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

// Public MCP endpoints of the keyless ring, swapped by tests for a local server
var (
	exaMCPURL      = "https://mcp.exa.ai/mcp"
	parallelMCPURL = "https://search.parallel.ai/mcp"
)

const keenableTitle = "icebath"

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

var vendorRefusalRe = pyreOnce(`(?i)\A(?:\s*(?:keyless\s+\w+\s+search\s+failed:\s*)?(?:http(?:\s+status)?|status(?:\s+code)?|client\s+error|error\s+code)\s*[:=']*\s*(?:40[123]|5\d\d)\b)`)

// Reports whether another anonymous vendor may serve the search
func isSearchFailoverEligible(message string) bool {
	return isRateLimitish(message) || pyreSearch(vendorRefusalRe(), message) != nil
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

var mcpLineBreakRe = regexp.MustCompile(`\r\n|\r|\n`)

// Parses an MCP tools/call body, plain JSON or SSE data frames, into its first text content, every failure a KeylessMCPError
func parseMCPBody(body string) (string, error) {
	stripped := strings.TrimSpace(body)
	candidates := []string{}
	if strings.HasPrefix(stripped, "{") {
		candidates = append(candidates, stripped)
	}
	for _, line := range mcpLineBreakRe.Split(body, -1) {
		if strings.HasPrefix(line, "data: ") {
			candidates = append(candidates, line[len("data: "):])
		}
	}
	envelopeSeen := false
	for _, candidate := range candidates {
		envelope, ok := mcpEnvelope(candidate)
		if !ok {
			continue
		}
		text, err := mcpEnvelopeText(envelope)
		if err != nil || text != "" {
			return text, err
		}
		envelopeSeen = true
	}
	if envelopeSeen {
		return "", keylessErr("MCP response contained no text content")
	}
	return "", keylessErr("Unrecognized MCP response shape")
}

// JSON-RPC envelope a body candidate holds, false when it is not a JSON object
func mcpEnvelope(payload string) (*jsonutil.Object, bool) {
	payload = strings.TrimSpace(payload)
	if !strings.HasPrefix(payload, "{") {
		return nil, false
	}
	data, err := jsonutil.Decode(payload)
	if err != nil {
		return nil, false
	}
	envelope, ok := data.(*jsonutil.Object)
	return envelope, ok
}

// First text content of an MCP envelope, empty when it has none; its error member or a result flagged isError returned as a KeylessMCPError
func mcpEnvelopeText(envelope *jsonutil.Object) (string, error) {
	if errObj, _ := envelope.Get("error"); anyutil.IsTrue(errObj) {
		return "", keylessErr("%s", mcpErrorText(errObj))
	}
	result, _ := envelope.Get("result")
	resultObj, ok := result.(*jsonutil.Object)
	if !ok {
		return "", nil
	}
	content, _ := resultObj.Get("content")
	items, _ := content.([]any)
	texts := []string{}
	for _, item := range items {
		if c, ok := item.(*jsonutil.Object); ok {
			if t, _ := c.Get("text"); anyutil.IsTrue(t) {
				texts = append(texts, anyutil.String(t))
			}
		}
	}
	if isError, _ := resultObj.Get("isError"); anyutil.IsTrue(isError) {
		if len(texts) == 0 {
			return "", keylessErr("MCP tool call failed")
		}
		return "", keylessErr("%s", strings.Join(texts, " "))
	}
	if len(texts) == 0 {
		return "", nil
	}
	return texts[0], nil
}

// Text of a JSON-RPC error member, its message when it is an object with one, else the member itself as text
func mcpErrorText(v any) string {
	if d, ok := v.(*jsonutil.Object); ok {
		if msg, _ := d.Get("message"); anyutil.IsTrue(msg) {
			return anyutil.String(msg)
		}
	}
	return anyutil.String(v)
}

// Posts a JSON-RPC tools/call and returns its text payload, a KeylessMCPError for transport, status and tool failures
func mcpCall(ctx context.Context, target, tool string, arguments map[string]any) (string, error) {
	payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments}}
	headers := map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "User-Agent": "icebath-agent"}
	status, header, raw, err := keylessHTTP(ctx, http.MethodPost, target, nil, payload, headers)
	if err != nil {
		return "", keylessErr("request failed: %v", err)
	}
	text := responseText(&http.Response{Header: header}, raw)
	if status >= 400 {
		return "", keylessErr("HTTP %d: %s", status, truncateRunes(text, 300))
	}
	return parseMCPBody(text)
}

// Sends one keyless request with exactly the given headers and returns the status, headers and raw body
func keylessHTTP(ctx context.Context, method, target string, params map[string]string, payload any, headers map[string]string) (int, http.Header, []byte, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, nil, err
	}
	if len(params) > 0 {
		q := req.URL.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		req.URL.RawQuery = q.Encode()
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Transport: httputil.TrustEnvCA(phasedWebTransport), CheckRedirect: httputil.RedirectPolicy(true)}
	resp, err := httputil.Do(client, req, httputil.EachPhase(keylessTimeout))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, resp.Content, nil
}

// First n characters of a string by rune
func truncateRunes(s string, n int) string { return runePrefix(s, n) }

// Keyless search body, a KeylessMCPError gets the vendor hint, a caught error the formatter, any other error propagates
func keylessSearch(vendor string, rows func() ([]WebSearchHit, error), catch func(error) bool, format func(error) string) (*WebSearchResponse, error) {
	hits, err := rows()
	if err != nil {
		if isKeylessMCPError(err) {
			return searchFail(keylessFailMsg(vendor, "search", err, true)), nil
		}
		if catch != nil && catch(err) {
			return searchFail(format(err)), nil
		}
		return nil, err
	}
	return searchOK(hits), nil
}

// Per url extract loop turning a caught fetch failure into an error entry, nil catch catching every error
func keylessPerURL(urls []string, fetch func(string) (*WebPage, error), vendor string, catch func(error) bool, hint bool) ([]*WebPage, error) {
	out := make([]*WebPage, 0, len(urls))
	for _, u := range urls {
		page, err := fetch(u)
		if err != nil {
			if catch != nil && !catch(err) {
				return nil, err
			}
			out = append(out, pageError(u, keylessFailMsg(vendor, "extract", err, hint)))
			continue
		}
		out = append(out, page)
	}
	return out, nil
}

// Reports whether err is a failed keyless MCP call
func isKeylessMCPError(err error) bool {
	var ke *KeylessMCPError
	return errors.As(err, &ke)
}

// Parallel keyless search, a reply that does not decode or holds no list of results named an unexpected payload and any other error propagated
func parallelSearchKeyless(ctx context.Context, query string, limit int) (*WebSearchResponse, error) {
	return keylessSearch("parallel", func() ([]WebSearchHit, error) {
		text, err := mcpCall(ctx, parallelMCPURL, "web_search", map[string]any{"objective": query, "search_queries": []string{query}, "session_id": keylessSessionID})
		if err != nil {
			return nil, err
		}
		data, err := jsonutil.Decode(text)
		if err != nil {
			return nil, err
		}
		results, err := dictGet(data, "results")
		if err != nil {
			return nil, err
		}
		items, err := vendorList(results)
		if err != nil {
			return nil, err
		}
		if limit != 0 {
			items = items[:min(max(limit, 0), len(items))]
		}
		hits := []WebSearchHit{}
		for i, r := range items {
			fields := make([]any, 3)
			for k, key := range []string{"url", "title", "excerpts"} {
				if fields[k], err = dictGet(r, key); err != nil {
					return nil, err
				}
			}
			hits = append(hits, webHit(anyutil.String(anyutil.Or(fields[0], "")), anyutil.String(anyutil.Or(fields[1], "")), joinText(" ", fields[2]), i+1))
		}
		return hits, nil
	}, func(err error) bool {
		var de *jsonutil.SyntaxError
		var te *typeError
		return errors.As(err, &de) || errors.As(err, &te)
	}, func(err error) string {
		return fmt.Sprintf("Keyless Parallel search returned an unexpected payload: %v", err)
	})
}

// Parallel keyless extract over the public MCP endpoint, an unexpected payload shape raised to the caller
func parallelExtractKeyless(ctx context.Context, urls []string) ([]*WebPage, error) {
	text, err := mcpCall(ctx, parallelMCPURL, "web_fetch", map[string]any{"urls": urls, "objective": "Full page content", "session_id": keylessSessionID})
	var data any
	if err == nil {
		data, err = jsonutil.Decode(text)
	}
	if err != nil {
		return extractFail(urls, keylessFailMsg("parallel", "extract", err, true)), nil
	}
	rows, err := dictGet(data, "results")
	if err != nil {
		return nil, err
	}
	items, err := vendorList(rows)
	if err != nil {
		return nil, err
	}
	results := []*WebPage{}
	for _, r := range items {
		page, err := parallelKeylessDocument(r)
		if err != nil {
			return nil, err
		}
		results = append(results, page)
	}
	failures, err := dictGet(data, "errors")
	if err != nil {
		return nil, err
	}
	if items, err = vendorList(failures); err != nil {
		return nil, err
	}
	for _, e := range items {
		fields := make([]any, 4)
		for i, key := range []string{"url", "http_status_code", "error_type", "content"} {
			if fields[i], err = dictGet(e, key); err != nil {
				return nil, err
			}
		}
		results = append(results, vendorErrorEntry(anyutil.String(anyutil.Or(fields[0], "")), fields[1], fields[2], fields[3]))
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
	return results, nil
}

// Document of one Parallel fetch row, full content then content then the joined excerpts
func parallelKeylessDocument(r any) (*WebPage, error) {
	u, err := dictGet(r, "url")
	if err != nil {
		return nil, err
	}
	title, _ := dictGet(r, "title")
	content, _ := dictGet(r, "full_content")
	if !anyutil.IsTrue(content) {
		content, _ = dictGet(r, "content")
	}
	if !anyutil.IsTrue(content) {
		excerpts, _ := dictGet(r, "excerpts")
		content = joinText("\n\n", excerpts)
	}
	return webDocument(anyutil.String(anyutil.Or(u, "")), anyutil.String(anyutil.Or(title, "")), anyutil.String(content), ""), nil
}

var exaLabels = []string{"Title:", "URL:", "Highlights:", "Published:", "Author:"}

// Parses Exa's --- separated Title URL Published Author Highlights blocks
func parseExaSearchText(text string, limit int) []WebSearchHit {
	results := []WebSearchHit{}
	for _, block := range strings.Split(text, "\n---\n") {
		title, u := "", ""
		highlights := []string{}
		inHighlights := false
		for _, line := range textutil.Lines(block) {
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
		if limit != 0 && len(results) >= limit {
			break
		}
	}
	return results
}

// Exa keyless search over the public MCP endpoint, errors other than a KeylessMCPError propagating
func exaSearchKeyless(ctx context.Context, query string, limit int) (*WebSearchResponse, error) {
	return keylessSearch("exa", func() ([]WebSearchHit, error) {
		text, err := mcpCall(ctx, exaMCPURL, "web_search_exa", map[string]any{"query": query, "numResults": max(1, limit)})
		if err != nil {
			return nil, err
		}
		return parseExaSearchText(text, limit), nil
	}, nil, nil)
}

// Exa keyless extract, one MCP call per url, a crawl status tag naming the origin's answer
func exaExtractKeyless(ctx context.Context, urls []string) ([]*WebPage, error) {
	fetch := func(u string) (*WebPage, error) {
		text, err := mcpCall(ctx, exaMCPURL, "web_fetch_exa", map[string]any{"urls": []string{u}})
		if err != nil {
			if code, ok := exaCrawlStatus(err.Error()); ok && isKeylessMCPError(err) {
				if origin := originHTTPError(code, nil); origin != "" {
					return pageError(u, origin), nil
				}
			}
			return nil, err
		}
		title := ""
		for _, line := range textutil.Lines(text) {
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
	return keylessPerURL(urls, fetch, "exa", isKeylessMCPError, true)
}

// Firecrawl keyless search over the public cloud without an authorization header, every error given the vendor hint
func firecrawlSearchKeyless(ctx context.Context, query string, limit int) (*WebSearchResponse, error) {
	return keylessSearch("firecrawl", func() ([]WebSearchHit, error) {
		client := newKeylessFirecrawlClient(firecrawlCloudAPIURL)
		resp, err := client.search(ctx, query, limit)
		if err != nil {
			return nil, err
		}
		return firecrawlWebSearchResults(resp), nil
	}, func(error) bool { return true }, func(err error) string { return keylessFailMsg("firecrawl", "search", err, true) })
}

// Firecrawl keyless extract reporting the post redirect page so dispatch re gates it
func firecrawlExtractKeyless(ctx context.Context, urls []string) ([]*WebPage, error) {
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
	return keylessPerURL(urls, fetch, "firecrawl", nil, false)
}

// Calls a Keenable public endpoint with the mandatory app identifier, the decoded JSON of any shape returned
func keenableRequest(ctx context.Context, method, path string, params map[string]string, payload any) (any, error) {
	headers := map[string]string{"X-Keenable-Title": keenableTitle}
	if method == http.MethodPost {
		headers["Content-Type"] = "application/json"
	}
	status, header, raw, err := keylessHTTP(ctx, method, keenableAPIURL+path, params, payload, headers)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		body := strings.TrimSpace(responseText(&http.Response{Header: header}, raw))
		if origin := keenableOriginError(status, body); origin != "" {
			return nil, keylessErr("%s", origin)
		}
		return nil, keylessErr("HTTP %d: %s", status, truncateRunes(body, 300))
	}
	return responseJSON(header, raw)
}

// JSON value of a response body, read in the charset the Content-Type declares when it names another one than UTF-8, else as UTF-8 or the UTF-16 or UTF-32 its bytes show
func responseJSON(header http.Header, raw []byte) (any, error) {
	return jsonutil.Decode(utf8Body(header, raw))
}

// Keenable keyless search over the public search endpoint, any error named in the Keenable failure text
func keenableSearchKeyless(ctx context.Context, query string, limit int) (*WebSearchResponse, error) {
	return keylessSearch("keenable", func() ([]WebSearchHit, error) {
		data, err := keenableRequest(ctx, http.MethodPost, "/v1/search/public", nil, map[string]any{"query": query, "max_results": max(1, limit)})
		if err != nil {
			return nil, err
		}
		results, err := dictGet(data, "results")
		if err != nil {
			return nil, err
		}
		items, err := vendorList(results)
		if err != nil {
			return nil, err
		}
		hits := []WebSearchHit{}
		for i, r := range items {
			fields := make([]any, 4)
			for k, key := range []string{"url", "title", "snippet", "description"} {
				if fields[k], err = dictGet(r, key); err != nil {
					return nil, err
				}
			}
			desc := anyutil.Or(fields[2], anyutil.Or(fields[3], ""))
			hits = append(hits, webHit(anyutil.String(anyutil.Or(fields[0], "")), anyutil.String(anyutil.Or(fields[1], "")), anyutil.String(desc), i+1))
		}
		return hits, nil
	}, func(error) bool { return true }, func(err error) string { return fmt.Sprintf("Keyless Keenable search failed: %v.", err) })
}

// Keenable keyless extract over the public fetch endpoint, an origin status relayed by Keenable kept as the page error
func keenableExtractKeyless(ctx context.Context, urls []string) ([]*WebPage, error) {
	fetch := func(u string) (*WebPage, error) {
		data, err := keenableRequest(ctx, http.MethodGet, "/v1/fetch/public", map[string]string{"url": u}, nil)
		if err != nil {
			if isKeylessMCPError(err) && isOriginHTTPError(err.Error()) {
				return pageError(u, err.Error()), nil
			}
			return nil, err
		}
		fields := make([]any, 3)
		for k, key := range []string{"url", "title", "content"} {
			if fields[k], err = dictGet(data, key); err != nil {
				return nil, err
			}
		}
		return webDocument(anyutil.String(anyutil.Or(fields[0], u)), anyutil.String(anyutil.Or(fields[1], "")), anyutil.String(anyutil.Or(fields[2], "")), u), nil
	}
	return keylessPerURL(urls, fetch, "keenable", nil, false)
}

// Round robin ring of anonymous vendors
var keylessRing = []string{"exa", "parallel", "firecrawl", "keenable"}

var keylessSearchers = map[string]func(context.Context, string, int) (*WebSearchResponse, error){
	"exa": exaSearchKeyless, "parallel": parallelSearchKeyless, "firecrawl": firecrawlSearchKeyless, "keenable": keenableSearchKeyless,
}

var keylessExtractors = map[string]func(context.Context, []string) ([]*WebPage, error){
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

// Walks the ring for a search until a vendor is not throttled, a vendor's raised error stopping the walk
func keylessSearchWithFailover(ctx context.Context, env *Env, name, query string, limit int) (*WebSearchResponse, error) {
	order := ringOrder(env, name)
	if len(order) == 0 {
		return searchFail(allPaidMsg), nil
	}
	var result *WebSearchResponse
	vendor := ""
	exhausted := true
	for i, v := range order {
		vendor = v
		var err error
		if result, err = keylessSearchers[v](ctx, query, limit); err != nil {
			return nil, err
		}
		if result.Success || !isSearchFailoverEligible(result.Error) {
			exhausted = false
			break
		}
		if i+1 < len(order) {
			envLog(env).Info(fmt.Sprintf("keyless %s search unavailable; failing over to %s", v, order[i+1]))
		}
	}
	if exhausted {
		result.Error = fmt.Sprintf("%s (all keyless vendors unavailable: %s)", result.Error, strings.Join(order, ", "))
	} else if result.Success && vendor != name {
		result.data().ServedBy = vendor
	}
	return result, nil
}

// Walks the ring for an extract only when every url came back rate limited, a vendor's raised error stopping the walk
func keylessExtractWithFailover(ctx context.Context, env *Env, name string, urls []string) ([]*WebPage, error) {
	order := ringOrder(env, name)
	if len(order) == 0 {
		return extractFail(urls, allPaidMsg), nil
	}
	throttled := func(err string) bool {
		return err != "" && !isOriginHTTPError(err) && isRateLimitish(err)
	}
	var results []*WebPage
	for i, v := range order {
		var err error
		if results, err = keylessExtractors[v](ctx, append([]string{}, urls...)); err != nil {
			return nil, err
		}
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
			envLog(env).Info(fmt.Sprintf("keyless %s extract unavailable; failing over to %s", v, order[i+1]))
		}
	}
	return results, nil
}
