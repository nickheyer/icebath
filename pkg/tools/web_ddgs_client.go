package tools

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	mrand "math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/unicode/norm"

	"github.com/nickheyer/icebath/pkg/internal/pyhtml"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// pyExc is a raised Python exception, its class name kept for the worker's error envelope
type pyExc struct{ class, msg string }

func (e *pyExc) Error() string { return e.msg }

func raise(class, format string, args ...any) error {
	return &pyExc{class: class, msg: fmt.Sprintf(format, args...)}
}

// Class name of an error raised by the ddgs path, Exception for a foreign error
func pyExcClass(err error) string {
	if e, ok := err.(*pyExc); ok {
		return e.class
	}
	return "Exception"
}

// primp 2.0.1 browser profiles impersonate random picks from, in its declared order
var primpBrowserVariants = []string{
	"ChromeV144", "ChromeV145", "ChromeV146", "ChromeV147", "ChromeV148", "ChromeV149", "ChromeV150", "ChromeV151", "ChromeV152", "ChromeV153",
	"EdgeV144", "EdgeV145", "EdgeV146", "EdgeV147", "EdgeV148", "EdgeV149", "EdgeV150", "EdgeV151", "EdgeV152", "EdgeV153",
	"OperaV126", "OperaV127", "OperaV128", "OperaV129", "OperaV130", "OperaV131", "OperaV132", "OperaV133", "OperaV134", "OperaV135",
	"SafariV18_5", "SafariV26", "SafariV26_3", "SafariV26_4",
	"FirefoxV140", "FirefoxV146", "FirefoxV147", "FirefoxV148", "FirefoxV149", "FirefoxV150", "FirefoxV151",
}

// primp operating systems impersonate_os random picks from with their sec-ch-ua-platform values
var primpOSVariants = [][2]string{{"Windows", `"Windows"`}, {"MacOS", `"macOS"`}, {"Linux", `"Linux"`}, {"Android", `"Android"`}, {"IOS", `"iOS"`}}

// Default headers a primp client sends for one browser profile and OS
func primpHeaders(variant string, osName [2]string) http.Header {
	h := http.Header{}
	ua, ok := primpUserAgents[[2]string{variant, osName[0]}]
	if !ok {
		ua = primpUserAgents[[2]string{variant, "*"}]
	}
	switch {
	case strings.HasPrefix(variant, "Safari"):
		h.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		h.Set("accept-language", "en-US,en;q=0.9")
		h.Set("accept-encoding", "gzip, deflate, br")
		h.Set("sec-fetch-dest", "document")
		h.Set("sec-fetch-mode", "navigate")
		h.Set("sec-fetch-site", "none")
		h.Set("priority", "u=0, i")
	case strings.HasPrefix(variant, "Firefox"):
		h.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		h.Set("accept-language", "en-US,en;q=0.5")
		h.Set("accept-encoding", "gzip, deflate, br, zstd")
		h.Set("dnt", "1")
		h.Set("sec-gpc", "1")
		h.Set("upgrade-insecure-requests", "1")
		h.Set("sec-fetch-dest", "document")
		h.Set("sec-fetch-mode", "navigate")
		h.Set("sec-fetch-site", "none")
		h.Set("sec-fetch-user", "?1")
		h.Set("priority", "u=0, i")
		h.Set("te", "trailers")
	default:
		h.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
		h.Set("accept-encoding", "gzip, deflate, br, zstd")
		h.Set("accept-language", "en-US,en;q=0.9")
		h.Set("upgrade-insecure-requests", "1")
		h.Set("sec-fetch-site", "none")
		h.Set("sec-fetch-mode", "navigate")
		h.Set("sec-fetch-dest", "document")
		h.Set("sec-fetch-user", "?1")
		h.Set("priority", "u=0, i")
		h.Set("sec-ch-ua", primpSecCHUA[variant])
		mobile := "?0"
		if osName[0] == "Android" || osName[0] == "IOS" {
			mobile = "?1"
		}
		h.Set("sec-ch-ua-mobile", mobile)
		h.Set("sec-ch-ua-platform", osName[1])
		if variant == "ChromeV150" || variant == "ChromeV151" || variant == "ChromeV152" {
			h.Set("sec-purpose", "prefetch;prerender")
		}
		if variant == "OperaV131" || variant == "OperaV132" {
			h.Set("cache-control", "max-age=0")
		}
	}
	h.Set("user-agent", ua)
	return h
}

// ddgsHTTPClient is the ddgs HttpClient, one random primp profile with its own cookie store
type ddgsHTTPClient struct {
	header    http.Header
	jar       http.CookieJar
	transport *http.Transport
	timeout   pyrequests.Timeout
}

var ddgsProxyTransports sync.Map

// Transport for the DDGS_PROXY value, the web transport when it is unset
func ddgsTransport(proxy string) *http.Transport {
	if proxy == "" {
		return webHTTPTransport
	}
	if tr, ok := ddgsProxyTransports.Load(proxy); ok {
		return tr.(*http.Transport)
	}
	tr := webHTTPTransport.Clone()
	if u, err := url.Parse(proxy); err == nil {
		tr.Proxy = http.ProxyURL(u)
	}
	got, _ := ddgsProxyTransports.LoadOrStore(proxy, tr)
	return got.(*http.Transport)
}

// Client with a random browser profile, the DDGS proxy and the per request timeout
func newDDGSHTTPClient(proxy string, timeout int) *ddgsHTTPClient {
	jar, _ := cookiejar.New(nil)
	variant := primpBrowserVariants[mrand.IntN(len(primpBrowserVariants))]
	osName := primpOSVariants[mrand.IntN(len(primpOSVariants))]
	return &ddgsHTTPClient{header: primpHeaders(variant, osName), jar: jar, transport: ddgsTransport(proxy), timeout: pyrequests.Seconds(timeout)}
}

// Merges headers into the client defaults, primp headers_update
func (c *ddgsHTTPClient) headersUpdate(h [][2]string) {
	for _, kv := range h {
		c.header.Set(kv[0], kv[1])
	}
}

// Stores cookies for a URL, a bare host read as https
func (c *ddgsHTTPClient) setCookies(raw string, cookies [][2]string) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	jar := make([]*http.Cookie, 0, len(cookies))
	for _, kv := range cookies {
		jar = append(jar, &http.Cookie{Name: kv[0], Value: kv[1]})
	}
	c.jar.SetCookies(u, jar)
}

// application/x-www-form-urlencoded serialization of ordered pairs, the WHATWG byte serializer primp uses
func formEncode(pairs [][2]string) string {
	enc := func(s string) string {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			ch := s[i]
			switch {
			case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '*', ch == '-', ch == '.', ch == '_':
				b.WriteByte(ch)
			case ch == ' ':
				b.WriteByte('+')
			default:
				fmt.Fprintf(&b, "%%%02X", ch)
			}
		}
		return b.String()
	}
	parts := make([]string, 0, len(pairs))
	for _, kv := range pairs {
		parts = append(parts, enc(kv[0])+"="+enc(kv[1]))
	}
	return strings.Join(parts, "&")
}

// ddgsResponse is a decoded HTTP answer
type ddgsResponse struct {
	status int
	text   string
}

// Sends one request, transport failures raised as the ddgs TimeoutException or DDGSException
func (c *ddgsHTTPClient) request(ctx context.Context, method, target string, params, data [][2]string) (*ddgsResponse, error) {
	if len(params) > 0 {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + formEncode(params)
	}
	var body io.Reader
	if data != nil {
		body = strings.NewReader(formEncode(data))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, raise("DDGSException", "Exception: %s", pyrepr.Repr(err.Error()))
	}
	req.Header = c.header.Clone()
	if data != nil {
		req.Header.Set("content-type", "application/x-www-form-urlencoded")
	}
	for _, ck := range c.jar.Cookies(req.URL) {
		req.AddCookie(ck)
	}
	resp, err := pyrequests.Send(ctx, c.transport, c.timeout, req)
	if err != nil {
		if e, ok := err.(*pyrequests.Error); ok {
			if e.Class == "ConnectTimeout" || e.Class == "ReadTimeout" || e.Class == "Timeout" {
				return nil, raise("TimeoutException", "%s", e.Error())
			}
			return nil, raise("DDGSException", "%s: %s(%s)", e.Class, e.Class, pyrepr.StrRepr(e.Error()))
		}
		return nil, raise("DDGSException", "Exception: Exception(%s)", pyrepr.StrRepr(err.Error()))
	}
	if final, err := url.Parse(target); err == nil {
		c.jar.SetCookies(final, (&http.Response{Header: resp.Header}).Cookies())
	}
	raw, err := pyhttpx.DecodeContent(resp.Header, resp.Body)
	if err != nil {
		return nil, raise("DDGSException", "DecodeError: DecodeError(%s)", pyrepr.StrRepr(err.Error()))
	}
	return &ddgsResponse{status: resp.Status, text: ddgsDecodeText(resp.Header.Get("Content-Type"), raw)}, nil
}

// Response text in the charset the content type names, UTF-8 with replacement otherwise
func ddgsDecodeText(contentType string, raw []byte) string {
	if i := strings.Index(strings.ToLower(contentType), "charset="); i >= 0 {
		name := strings.Trim(strings.TrimSpace(strings.SplitN(contentType[i+8:], ";", 2)[0]), `"'`)
		if enc, canonical := charset.Lookup(name); enc != nil && canonical != "utf-8" {
			if out, err := enc.NewDecoder().Bytes(raw); err == nil {
				return string(out)
			}
		}
	}
	return pystr.DecodeUTF8Replace(raw)
}

var ddgsStripTagsRe = regexp.MustCompile(`<.*?>`)

// ddgs _normalize_text, tags stripped, entities unescaped, NFC, control and unassigned runes dropped, whitespace collapsed
func ddgsNormalizeText(v any) (any, error) {
	if !pyTruthy(v) {
		return v, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, raise("TypeError", "expected string or bytes-like object, got '%s'", pyrepr.TypeName(v))
	}
	s = norm.NFC.String(pyhtml.Unescape(ddgsStripTagsRe.ReplaceAllString(s, "")))
	s = strings.Map(func(r rune) rune {
		if pyunicode.Category(r)[0] == 'C' {
			return -1
		}
		return r
	}, s)
	return strings.Join(pystr.SplitWhitespace(s, -1), " "), nil
}

// ddgs _normalize_url, percent decoding with spaces turned into plus signs
func ddgsNormalizeURL(v any) (any, error) {
	if !pyTruthy(v) {
		return v, nil
	}
	switch s := v.(type) {
	case string:
		return strings.ReplaceAll(pyurl.Unquote(s), " ", "+"), nil
	case []any, *pyjson.Object:
		if in, _ := pyContains(v, "%"); in {
			return nil, raise("TypeError", "expected string or bytes-like object, got '%s'", pyrepr.TypeName(v))
		}
		return nil, raise("AttributeError", "'%s' object has no attribute 'split'", pyrepr.TypeName(v))
	}
	return nil, raise("TypeError", "argument of type '%s' is not iterable", pyrepr.TypeName(v))
}

// Python item in container for a decoded value, the TypeError of a non container
func pyContains(container any, item string) (bool, error) {
	switch c := container.(type) {
	case string:
		return strings.Contains(c, item), nil
	case []any:
		for _, x := range c {
			if s, ok := x.(string); ok && s == item {
				return true, nil
			}
		}
		return false, nil
	case *pyjson.Object:
		_, ok := c.Get(item)
		return ok, nil
	}
	return false, raise("TypeError", "argument of type '%s' is not iterable", pyrepr.TypeName(container))
}

// Python len of a decoded value, the TypeError of an unsized one
func pyValueLen(v any) (int, error) {
	switch x := v.(type) {
	case string:
		return pystr.Len(x), nil
	case []any:
		return len(x), nil
	case *pyjson.Object:
		return x.Len(), nil
	}
	return 0, raise("TypeError", "object of type '%s' has no len()", pyrepr.TypeName(v))
}

// Python subscript of a decoded value with the KeyError, IndexError or TypeError it raises
func pySubscript(v any, key any) (any, error) {
	switch c := v.(type) {
	case *pyjson.Object:
		if k, ok := key.(string); ok {
			if val, ok := c.Get(k); ok {
				return val, nil
			}
		}
		return nil, raise("KeyError", "%s", pyrepr.Repr(key))
	case []any:
		i, ok := key.(int)
		if !ok {
			return nil, raise("TypeError", "list indices must be integers or slices, not %s", pyrepr.TypeName(key))
		}
		if i < 0 {
			i += len(c)
		}
		if i < 0 || i >= len(c) {
			return nil, raise("IndexError", "list index out of range")
		}
		return c[i], nil
	case string:
		i, ok := key.(int)
		if !ok {
			return nil, raise("TypeError", "string indices must be integers, not '%s'", pyrepr.TypeName(key))
		}
		rs := []rune(c)
		if i < 0 {
			i += len(rs)
		}
		if i < 0 || i >= len(rs) {
			return nil, raise("IndexError", "string index out of range")
		}
		return string(rs[i]), nil
	}
	return nil, raise("TypeError", "'%s' object is not subscriptable", pyrepr.TypeName(v))
}

// json.loads of an engine answer, the JSONDecodeError text on failure
func ddgsJSON(text string) (any, error) {
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, raise("JSONDecodeError", "%s", err.Error())
	}
	return v, nil
}

// ddgsTextResult is a ddgs TextResult, every assignment normalized like BaseResult.__setattr__
type ddgsTextResult struct {
	title, href, body any
}

func newDDGSTextResult() *ddgsTextResult {
	return &ddgsTextResult{title: "", href: "", body: ""}
}

// Assigns one field through its normalizer
func (r *ddgsTextResult) set(key string, v any) error {
	var err error
	switch key {
	case "title":
		r.title, err = ddgsNormalizeText(v)
	case "body":
		r.body, err = ddgsNormalizeText(v)
	case "href":
		r.href, err = ddgsNormalizeURL(v)
	}
	return err
}

// String form of the href for prefix checks on HTML results
func (r *ddgsTextResult) hrefStr() string {
	s, _ := r.href.(string)
	return s
}

// ddgsQuery carries the text search arguments hermes leaves at their ddgs defaults
type ddgsQuery struct {
	query, region, safesearch, timelimit string
	page                                 int
}

// ddgsEngine is one enabled ddgs text backend with its own HTTP client
type ddgsEngine struct {
	name, provider string
	priority       float64
	method         string
	searchURL      string
	itemsXPath     string
	elements       [][2]string
	client         *ddgsHTTPClient
	lang           string
	build          func(e *ddgsEngine, ctx context.Context, q ddgsQuery) ([][2]string, error)
	pre            func(text string) string
	extract        func(e *ddgsEngine, ctx context.Context, text string) ([]*ddgsTextResult, error)
	post           func(results []*ddgsTextResult) ([]*ddgsTextResult, error)
}

// Answer text of a 200 reply, false for any other status, BaseSearchEngine.request
func (e *ddgsEngine) request(ctx context.Context, method, target string, params, data [][2]string) (string, bool, error) {
	resp, err := e.client.request(ctx, method, target, params, data)
	if err != nil {
		return "", false, err
	}
	if resp.status == 200 {
		return resp.text, true, nil
	}
	return "", false, nil
}

// Splits a region into country and language, the unpacking ValueError when it has another shape
func ddgsRegion(region string) (string, string, error) {
	parts := strings.Split(region, "-")
	if len(parts) > 2 {
		return "", "", raise("ValueError", "too many values to unpack (expected 2, got %d)", len(parts))
	}
	if len(parts) < 2 {
		return "", "", raise("ValueError", "not enough values to unpack (expected 2, got %d)", len(parts))
	}
	return parts[0], parts[1], nil
}

// Builds the payload, requests the page and extracts results, BaseSearchEngine.search
func (e *ddgsEngine) search(ctx context.Context, q ddgsQuery) ([]*ddgsTextResult, error) {
	payload, err := e.build(e, ctx, q)
	if err != nil {
		return nil, err
	}
	var text string
	var ok bool
	if e.method == "GET" {
		text, ok, err = e.request(ctx, e.method, e.searchURL, payload, nil)
	} else {
		text, ok, err = e.request(ctx, e.method, e.searchURL, nil, payload)
	}
	if err != nil {
		return nil, err
	}
	if !ok || text == "" {
		return nil, nil
	}
	extract := e.extract
	if extract == nil {
		extract = (*ddgsEngine).extractHTML
	}
	results, err := extract(e, ctx, text)
	if err != nil || e.post == nil {
		return results, err
	}
	return e.post(results)
}

// Reads every item of the page with the field XPaths, whitespace collapsed like ddgs extract_results
func (e *ddgsEngine) extractHTML(_ context.Context, text string) ([]*ddgsTextResult, error) {
	if e.pre != nil {
		text = e.pre(text)
	}
	tree, err := lxmlFromString(text)
	if err != nil {
		return nil, err
	}
	items, err := tree.xpath(tree.root, e.itemsXPath)
	if err != nil {
		return nil, err
	}
	var results []*ddgsTextResult
	for _, item := range items {
		r := newDDGSTextResult()
		for _, kv := range e.elements {
			joined, err := tree.xpathText(item.n, kv[1])
			if err != nil {
				return nil, err
			}
			if err := r.set(kv[0], strings.Join(pystr.SplitWhitespace(joined, -1), " ")); err != nil {
				return nil, err
			}
		}
		results = append(results, r)
	}
	return results, nil
}

// URL safe random token of n bytes, secrets.token_urlsafe
func tokenURLSafe(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Index into choices drawn from the system random source
func systemChoice(n int) int {
	v, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	return int(v.Int64())
}

// Google WML user agent picked once per process at engine class creation
var ddgsGoogleUA = sync.OnceValue(func() string {
	firmware := []string{"2.0617.1.0.3", "2.0625.2.0.2", "2.0635.2.0.2", "5.0706.4.0.1", "5.0819.4.0.1"}[systemChoice(5)]
	ua := "NokiaN72/" + firmware + " Series60/2.8 Profile/MIDP-2.0 Configuration/CLDC-1.1"
	if systemChoice(2) == 0 {
		ua += "/UC Browser" + []string{"7.9.1.120", "7.9.1.121", "7.9.1.122"}[systemChoice(3)] + "/27/351/UCWEB"
	}
	return ua
})

// Builders for the enabled ddgs 9.16.0 text engines, keyed by engine name in registry order
var ddgsTextEngines = map[string]func(proxy string, timeout int) *ddgsEngine{
	"brave": func(proxy string, timeout int) *ddgsEngine {
		return &ddgsEngine{name: "brave", provider: "brave", priority: 1, method: "GET", searchURL: "https://search.brave.com/search",
			itemsXPath: "//div[@data-type='web']",
			elements: [][2]string{
				{"title", ".//div[(contains(@class,'title') or contains(@class,'sitename-container')) and position()=last()]//text()"},
				{"href", ".//a[div[contains(@class, 'title')]]/@href"},
				{"body", ".//div[contains(@class, 'snippet')]//div[contains(@class, 'content')]//text()"},
			},
			client: newDDGSHTTPClient(proxy, timeout),
			build: func(e *ddgsEngine, _ context.Context, q ddgsQuery) ([][2]string, error) {
				payload := [][2]string{{"q", q.query}, {"source", "web"}}
				country, _, err := ddgsRegion(pystr.Lower(q.region))
				if err != nil {
					return nil, err
				}
				cookies := [][2]string{{country, country}, {"useLocation", "0"}}
				if q.safesearch != "moderate" {
					v := "off"
					if q.safesearch == "on" {
						v = "strict"
					}
					cookies = append(cookies, [2]string{"safesearch", v})
				}
				e.client.setCookies("https://search.brave.com", cookies)
				if q.timelimit != "" {
					tf, ok := map[string]string{"d": "pd", "w": "pw", "m": "pm", "y": "py"}[q.timelimit]
					if !ok {
						return nil, raise("KeyError", "%s", pyrepr.StrRepr(q.timelimit))
					}
					payload = append(payload, [2]string{"tf", tf})
				}
				if q.page > 1 {
					payload = append(payload, [2]string{"offset", fmt.Sprint(q.page - 1)})
				}
				return payload, nil
			}}
	},
	"duckduckgo": func(proxy string, timeout int) *ddgsEngine {
		return &ddgsEngine{name: "duckduckgo", provider: "bing", priority: 1, method: "POST", searchURL: "https://html.duckduckgo.com/html/",
			itemsXPath: "//div[contains(@class, 'body')]",
			elements:   [][2]string{{"title", ".//h2//text()"}, {"href", "./a/@href"}, {"body", "./a//text()"}},
			client:     newDDGSHTTPClient(proxy, timeout),
			build: func(_ *ddgsEngine, _ context.Context, q ddgsQuery) ([][2]string, error) {
				payload := [][2]string{{"q", q.query}, {"b", ""}, {"l", q.region}}
				if q.page > 1 {
					payload = append(payload, [2]string{"s", fmt.Sprint(10 + (q.page-2)*15)})
				}
				if q.timelimit != "" {
					payload = append(payload, [2]string{"df", q.timelimit})
				}
				return payload, nil
			},
			post: func(results []*ddgsTextResult) ([]*ddgsTextResult, error) {
				out := results[:0:0]
				for _, r := range results {
					if !strings.HasPrefix(r.hrefStr(), "https://duckduckgo.com/y.js?") {
						out = append(out, r)
					}
				}
				return out, nil
			}}
	},
	"google": func(proxy string, timeout int) *ddgsEngine {
		e := &ddgsEngine{name: "google", provider: "google", priority: 1, method: "GET", searchURL: "https://www.google.com/wml/search",
			itemsXPath: "//div[./div[1]/a and ./div[2][table]]",
			elements:   [][2]string{{"title", "./div[a]/a/span[1]/text()"}, {"href", "./div[a]/a/@href"}, {"body", "./div[2][table]//text()"}},
			client:     newDDGSHTTPClient(proxy, timeout),
			build: func(e *ddgsEngine, _ context.Context, q ddgsQuery) ([][2]string, error) {
				e.client.setCookies("google.com", [][2]string{{"CONSENT", "YES+"}})
				filter, ok := map[string]string{"on": "2", "moderate": "1", "off": "0"}[pystr.Lower(q.safesearch)]
				if !ok {
					return nil, raise("KeyError", "%s", pyrepr.StrRepr(pystr.Lower(q.safesearch)))
				}
				payload := [][2]string{{"q", q.query}, {"sca_esv", "1"}, {"filter", filter}, {"start", fmt.Sprint((q.page - 1) * 10)}}
				country, lang, err := ddgsRegion(q.region)
				if err != nil {
					return nil, err
				}
				payload = append(payload, [2]string{"hl", lang + "-" + pystr.Upper(country)}, [2]string{"lr", "lang_" + lang}, [2]string{"cr", "country" + pystr.Upper(country)})
				if q.timelimit != "" {
					payload = append(payload, [2]string{"tbs", "qdr:" + q.timelimit})
				}
				return payload, nil
			},
			pre: func(text string) string {
				if i := strings.Index(text, "?>"); i >= 0 {
					return text[i:]
				}
				_, size := utf8.DecodeLastRuneInString(text)
				return text[len(text)-size:]
			},
			post: func(results []*ddgsTextResult) ([]*ddgsTextResult, error) {
				var out []*ddgsTextResult
				for _, r := range results {
					if href := r.hrefStr(); strings.HasPrefix(href, "/url?q=") {
						v := strings.SplitN(strings.SplitN(strings.Split(href, "?q=")[1], "&", 2)[0], "?", 2)[0]
						if err := r.set("href", v); err != nil {
							return nil, err
						}
					}
					if pyTruthy(r.title) && strings.HasPrefix(r.hrefStr(), "http") {
						out = append(out, r)
					}
				}
				return out, nil
			}}
		e.client.headersUpdate([][2]string{{"User-Agent", ddgsGoogleUA()}})
		return e
	},
	"grokipedia": func(proxy string, timeout int) *ddgsEngine {
		return &ddgsEngine{name: "grokipedia", provider: "grokipedia", priority: 1.9, method: "GET", searchURL: "https://grokipedia.com/api/typeahead",
			client: newDDGSHTTPClient(proxy, timeout),
			build: func(_ *ddgsEngine, _ context.Context, q ddgsQuery) ([][2]string, error) {
				return [][2]string{{"query", q.query}, {"limit", "1"}}, nil
			},
			extract: ddgsGrokipediaResults}
	},
	"mojeek": func(proxy string, timeout int) *ddgsEngine {
		return &ddgsEngine{name: "mojeek", provider: "mojeek", priority: 1, method: "GET", searchURL: "https://www.mojeek.com/search",
			itemsXPath: "//ul[contains(@class, 'results')]/li",
			elements:   [][2]string{{"title", ".//h2//text()"}, {"href", ".//h2/a/@href"}, {"body", ".//p[@class='s']//text()"}},
			client:     newDDGSHTTPClient(proxy, timeout),
			build: func(e *ddgsEngine, _ context.Context, q ddgsQuery) ([][2]string, error) {
				country, lang, err := ddgsRegion(pystr.Lower(q.region))
				if err != nil {
					return nil, err
				}
				e.client.setCookies("https://www.mojeek.com", [][2]string{{"arc", country}, {"lb", lang}})
				payload := [][2]string{{"q", q.query}}
				if q.safesearch == "on" {
					payload = append(payload, [2]string{"safe", "1"})
				}
				if q.page > 1 {
					payload = append(payload, [2]string{"s", fmt.Sprint((q.page-1)*10 + 1)})
				}
				return payload, nil
			}}
	},
	"startpage": func(proxy string, timeout int) *ddgsEngine {
		e := &ddgsEngine{name: "startpage", provider: "google", priority: 1, method: "POST", searchURL: "https://www.startpage.com/sp/search",
			itemsXPath: "//div[contains(@class, 'result')][./a]",
			elements:   [][2]string{{"title", ".//h2//text()"}, {"href", "./a/@href"}, {"body", ".//p//text()"}},
			client:     newDDGSHTTPClient(proxy, timeout),
			build: func(e *ddgsEngine, ctx context.Context, q ddgsQuery) ([][2]string, error) {
				country, lang, err := ddgsRegion(pystr.Lower(q.region))
				if err != nil {
					return nil, err
				}
				qadf, ok := map[string]string{"on": "heavy", "moderate": "moderate", "off": "none"}[pystr.Lower(q.safesearch)]
				if !ok {
					return nil, raise("KeyError", "%s", pyrepr.StrRepr(pystr.Lower(q.safesearch)))
				}
				sc, err := ddgsStartpageSC(e, ctx)
				if err != nil {
					return nil, err
				}
				payload := [][2]string{{"query", q.query}, {"cat", "web"}, {"t", "device"}, {"sc", sc}, {"lui", "english"}, {"language", "english"},
					{"abp", "1"}, {"abd", "0"}, {"abe", "0"}, {"qsr", lang + "_" + pystr.Upper(country)}, {"qadf", qadf}, {"segment", "organic"}}
				if q.page > 1 {
					payload = append(payload, [2]string{"page", fmt.Sprint(q.page)})
				}
				if q.timelimit != "" {
					payload = append(payload, [2]string{"with_date", q.timelimit})
				}
				return payload, nil
			}}
		e.client.headersUpdate([][2]string{{"Referer", "https://www.startpage.com/"}})
		return e
	},
	"wikipedia": func(proxy string, timeout int) *ddgsEngine {
		return &ddgsEngine{name: "wikipedia", provider: "wikipedia", priority: 2, method: "GET", searchURL: "https://{lang}.wikipedia.org/w/api.php?action=opensearch&search={query}",
			client: newDDGSHTTPClient(proxy, timeout),
			build: func(e *ddgsEngine, _ context.Context, q ddgsQuery) ([][2]string, error) {
				_, lang, err := ddgsRegion(pystr.Lower(q.region))
				if err != nil {
					return nil, err
				}
				e.searchURL = "https://" + lang + ".wikipedia.org/w/api.php?action=opensearch&profile=fuzzy&limit=1&search=" + pyurl.Quote(q.query, "/")
				e.lang = lang
				return nil, nil
			},
			extract: ddgsWikipediaResults}
	},
	"yahoo": func(proxy string, timeout int) *ddgsEngine {
		return &ddgsEngine{name: "yahoo", provider: "bing", priority: 1, method: "GET", searchURL: "https://search.yahoo.com/search",
			itemsXPath: "//div[contains(@class, 'relsrch')]",
			elements: [][2]string{
				{"title", ".//div[contains(@class, 'Title')]//h3//text()"},
				{"href", ".//div[contains(@class, 'Title')]//a/@href"},
				{"body", ".//div[contains(@class, 'Text')]//text()"},
			},
			client: newDDGSHTTPClient(proxy, timeout),
			build: func(e *ddgsEngine, _ context.Context, q ddgsQuery) ([][2]string, error) {
				e.searchURL = "https://search.yahoo.com/search;_ylt=" + tokenURLSafe(24*3/4) + ";_ylu=" + tokenURLSafe(47*3/4)
				payload := [][2]string{{"p", q.query}}
				if q.page > 1 {
					payload = append(payload, [2]string{"b", fmt.Sprint((q.page-1)*7 + 1)})
				}
				if q.timelimit != "" {
					payload = append(payload, [2]string{"btf", q.timelimit})
				}
				return payload, nil
			},
			post: func(results []*ddgsTextResult) ([]*ddgsTextResult, error) {
				var out []*ddgsTextResult
				for _, r := range results {
					href := r.hrefStr()
					if strings.HasPrefix(href, "https://www.bing.com/aclick?") {
						continue
					}
					if strings.Contains(href, "/RU=") {
						t := strings.SplitN(href, "/RU=", 2)[1]
						t = strings.SplitN(strings.SplitN(t, "/RK=", 2)[0], "/RS=", 2)[0]
						if err := r.set("href", pyurl.Unquote(strings.ReplaceAll(t, "+", " "))); err != nil {
							return nil, err
						}
					}
					out = append(out, r)
				}
				return out, nil
			}}
	},
}

// Search form token of the Startpage home page, empty when the page has none
func ddgsStartpageSC(e *ddgsEngine, ctx context.Context) (string, error) {
	resp, err := e.client.request(ctx, "GET", "https://www.startpage.com/", nil, nil)
	if err != nil {
		return "", err
	}
	tree, err := lxmlFromString(resp.text)
	if err != nil {
		return "", err
	}
	set, err := tree.xpath(tree.root, `//form[@id="search"]//input[@name="sc"]/@value`)
	if err != nil || len(set) == 0 {
		return "", err
	}
	return set[0].str(), nil
}

// Wikipedia opensearch hit with its intro extract, none for a disambiguation page
func ddgsWikipediaResults(e *ddgsEngine, ctx context.Context, text string) ([]*ddgsTextResult, error) {
	data, err := ddgsJSON(text)
	if err != nil {
		return nil, err
	}
	titles, err := pySubscript(data, 1)
	if err != nil {
		return nil, err
	}
	if !pyTruthy(titles) {
		return nil, nil
	}
	r := newDDGSTextResult()
	title, err := pySubscript(titles, 0)
	if err == nil {
		err = r.set("title", title)
	}
	if err != nil {
		return nil, err
	}
	links, err := pySubscript(data, 3)
	var link any
	if err == nil {
		link, err = pySubscript(links, 0)
	}
	if err == nil {
		err = r.set("href", link)
	}
	if err != nil {
		return nil, err
	}
	titleText, ok := r.title.(string)
	if !ok {
		return nil, raise("TypeError", "quote_from_bytes() expected bytes")
	}
	body, found, err := e.request(ctx, "GET", "https://"+e.lang+".wikipedia.org/w/api.php?action=query&format=json&prop=extracts&titles="+pyurl.Quote(titleText, "/")+"&explaintext=0&exintro=0&redirects=1", nil, nil)
	if err != nil {
		return nil, err
	}
	if found && body != "" {
		extract, err := ddgsWikipediaExtract(body)
		if err == nil {
			err = r.set("body", extract)
		}
		if err != nil {
			return nil, err
		}
	}
	refers, err := pyContains(r.body, "may refer to:")
	if err != nil {
		return nil, err
	}
	if refers {
		return nil, nil
	}
	return []*ddgsTextResult{r}, nil
}

// Extract of the first page in a Wikipedia query answer
func ddgsWikipediaExtract(body string) (any, error) {
	data, err := ddgsJSON(body)
	if err != nil {
		return nil, err
	}
	query, err := pySubscript(data, "query")
	if err != nil {
		return nil, err
	}
	pages, err := pySubscript(query, "pages")
	if err != nil {
		return nil, err
	}
	obj, ok := pages.(*pyjson.Object)
	if !ok {
		return nil, raise("AttributeError", "'%s' object has no attribute 'values'", pyrepr.TypeName(pages))
	}
	if obj.Len() == 0 {
		return nil, raise("StopIteration", "")
	}
	first, _ := obj.Get(obj.Keys[0])
	page, ok := pyDict(first)
	if !ok {
		return nil, pyAttributeError(first, "get")
	}
	if v, ok := page.Get("extract"); ok {
		return v, nil
	}
	return "", nil
}

// Grokipedia typeahead hit, its snippet past the title paragraph
func ddgsGrokipediaResults(_ *ddgsEngine, _ context.Context, text string) ([]*ddgsTextResult, error) {
	data, err := ddgsJSON(text)
	if err != nil {
		return nil, err
	}
	d, ok := pyDict(data)
	if !ok {
		return nil, pyAttributeError(data, "get")
	}
	items, present := d.Get("results")
	if !present {
		items = []any{}
	}
	if !pyTruthy(items) {
		return nil, nil
	}
	first, err := pySubscript(items, 0)
	if err != nil {
		return nil, err
	}
	item, ok := pyDict(first)
	if !ok {
		return nil, pyAttributeError(first, "get")
	}
	get := func(key string) any {
		if v, ok := item.Get(key); ok {
			return v
		}
		return ""
	}
	r := newDDGSTextResult()
	title, ok := get("title").(string)
	if !ok {
		return nil, pyAttributeError(get("title"), "strip")
	}
	if err := r.set("title", strings.Trim(title, "_")); err != nil {
		return nil, err
	}
	body := get("snippet")
	split, err := pyContains(body, "\n\n")
	if err != nil {
		return nil, err
	}
	if split {
		s, ok := body.(string)
		if !ok {
			return nil, pyAttributeError(body, "split")
		}
		body = strings.SplitN(s, "\n\n", 2)[1]
	}
	if err := r.set("body", body); err != nil {
		return nil, err
	}
	slug, err := pySubscript(first, "slug")
	if err != nil {
		return nil, err
	}
	if err := r.set("href", "https://grokipedia.com/page/"+pyrepr.Str(slug)); err != nil {
		return nil, err
	}
	return []*ddgsTextResult{r}, nil
}

// ddgsClient is a DDGS instance, engines cached per instance
type ddgsClient struct {
	proxy   string
	timeout int
	engines map[string]*ddgsEngine
}

// DDGS(timeout=...) reading the DDGS_PROXY environment variable
func newDDGSClient(timeout int) *ddgsClient {
	return &ddgsClient{proxy: os.Getenv("DDGS_PROXY"), timeout: timeout, engines: map[string]*ddgsEngine{}}
}

// Text engines for the auto backend, wikipedia and grokipedia first, then by priority with shuffled ties
func (c *ddgsClient) textEngines() []*ddgsEngine {
	keys := []string{"bing", "brave", "duckduckgo", "google", "grokipedia", "mojeek", "startpage", "wikipedia", "yahoo", "yandex"}
	enabled := keys[:0:0]
	for _, k := range keys {
		if _, ok := ddgsTextEngines[k]; ok {
			enabled = append(enabled, k)
		}
	}
	mrand.Shuffle(len(enabled), func(i, j int) { enabled[i], enabled[j] = enabled[j], enabled[i] })
	ordered := []string{"wikipedia", "grokipedia"}
	for _, k := range enabled {
		if k != "wikipedia" && k != "grokipedia" {
			ordered = append(ordered, k)
		}
	}
	var out []*ddgsEngine
	for _, k := range ordered {
		e, ok := c.engines[k]
		if !ok {
			e = ddgsTextEngines[k](c.proxy, c.timeout)
			c.engines[k] = e
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].priority > out[j].priority })
	return out
}

// ddgsFuture is one engine search running on the bounded pool
type ddgsFuture struct {
	engine  *ddgsEngine
	done    chan struct{}
	results []*ddgsTextResult
	err     error
}

// ddgsAggregator deduplicates results by href and orders them by how many engines returned them
type ddgsAggregator struct {
	keys   []string
	cache  map[string]*ddgsTextResult
	counts map[string]int
}

// Registers one occurrence, keeping the copy with the longer body
func (a *ddgsAggregator) append(r *ddgsTextResult) error {
	key := pyrepr.Str(r.href)
	cached, seen := a.cache[key]
	replace := !seen
	if seen {
		n, err := pyValueLen(r.body)
		if err != nil {
			return err
		}
		m, err := pyValueLen(cached.body)
		if err != nil {
			return err
		}
		replace = n > m
	}
	if replace {
		a.cache[key] = r
	}
	if _, ok := a.counts[key]; !ok {
		a.keys = append(a.keys, key)
	}
	a.counts[key]++
	return nil
}

// Results by descending count, ties in first seen order like Counter.most_common
func (a *ddgsAggregator) extract() []*ddgsTextResult {
	keys := append([]string{}, a.keys...)
	sort.SliceStable(keys, func(i, j int) bool { return a.counts[keys[i]] > a.counts[keys[j]] })
	out := make([]*ddgsTextResult, 0, len(keys))
	for _, k := range keys {
		out = append(out, a.cache[k])
	}
	return out
}

// Waits until every future is done, one failed, or the timeout passes, concurrent.futures FIRST_EXCEPTION
func ddgsWait(futures []*ddgsFuture, notify chan struct{}, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		all := true
		for _, f := range futures {
			select {
			case <-f.done:
				if f.err != nil {
					return
				}
			default:
				all = false
			}
		}
		if all {
			return
		}
		select {
		case <-notify:
		case <-timer.C:
			return
		}
	}
}

var ddgsWordSplitRe = sync.OnceValue(func() *regexp2.Regexp {
	re, err := pyre.Compile(`\W+`)
	if err != nil {
		panic(err)
	}
	return re
})

// Query tokens of three or more code points split on non word runs, SimpleFilterRanker._extract_tokens
func ddgsTokens(query string) []string {
	runes := []rune(pystr.Lower(query))
	re := ddgsWordSplitRe()
	var tokens []string
	start := 0
	keep := func(end int) {
		if end-start >= 3 {
			tokens = append(tokens, string(runes[start:end]))
		}
	}
	m, _ := re.FindRunesMatch(runes)
	for m != nil {
		keep(m.RuneIndex)
		start = m.RuneIndex + m.RuneLength
		m, _ = re.FindNextMatch(m)
	}
	keep(len(runes))
	return tokens
}

// Reports any token inside the lower cased text, the AttributeError of a non string
func ddgsHasAnyToken(text any, tokens []string) (bool, error) {
	s, ok := text.(string)
	if !ok {
		return false, pyAttributeError(text, "lower")
	}
	lower := pystr.Lower(s)
	for _, t := range tokens {
		if strings.Contains(lower, t) {
			return true, nil
		}
	}
	return false, nil
}

// Wikipedia hits first, then title and body matches, title only, body only and the rest, Wikimedia category pages dropped
func ddgsRank(docs []*ddgsTextResult, query string) ([]*ddgsTextResult, error) {
	tokens := ddgsTokens(query)
	var wiki, both, titleOnly, bodyOnly, neither []*ddgsTextResult
	for _, d := range docs {
		category, err := pyContains(d.title, "Category:")
		if err != nil {
			return nil, err
		}
		if category {
			wikimedia, err := pyContains(d.title, "Wikimedia")
			if err != nil {
				return nil, err
			}
			if wikimedia {
				continue
			}
		}
		isWiki, err := pyContains(d.href, "wikipedia.org")
		if err != nil {
			return nil, err
		}
		if isWiki {
			wiki = append(wiki, d)
			continue
		}
		hitTitle, err := ddgsHasAnyToken(d.title, tokens)
		if err != nil {
			return nil, err
		}
		hitBody, err := ddgsHasAnyToken(d.body, tokens)
		if err != nil {
			return nil, err
		}
		switch {
		case hitTitle && hitBody:
			both = append(both, d)
		case hitTitle:
			titleOnly = append(titleOnly, d)
		case hitBody:
			bodyOnly = append(bodyOnly, d)
		default:
			neither = append(neither, d)
		}
	}
	out := append(append(append(append(wiki, both...), titleOnly...), bodyOnly...), neither...)
	return out, nil
}

// Text search over the engines with the bounded pool, aggregation and ranking of DDGS._search_sync
func (c *ddgsClient) text(ctx context.Context, q ddgsQuery, maxResults int) ([]*ddgsTextResult, error) {
	if q.query == "" {
		return nil, raise("DDGSException", "query is mandatory.")
	}
	engines := c.textEngines()
	providers := map[string]bool{}
	for _, e := range engines {
		providers[e.provider] = true
	}
	maxWorkers := len(providers)
	if maxResults > 0 {
		maxWorkers = min(len(providers), int(math.Ceil(float64(maxResults)/10))+1)
	}
	sem := make(chan struct{}, maxWorkers)
	notify := make(chan struct{}, len(engines))
	seen := map[string]bool{}
	agg := &ddgsAggregator{cache: map[string]*ddgsTextResult{}, counts: map[string]int{}}
	var all, pending []*ddgsFuture
	var lastErr error
	for i, e := range engines {
		if seen[e.provider] {
			continue
		}
		f := &ddgsFuture{engine: e, done: make(chan struct{})}
		all = append(all, f)
		pending = append(pending, f)
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			f.results, f.err = f.engine.search(ctx, q)
			close(f.done)
			notify <- struct{}{}
		}()
		if len(pending) >= maxWorkers || i+1 >= maxWorkers {
			ddgsWait(pending, notify, time.Duration(c.timeout)*time.Second)
			var still []*ddgsFuture
			for _, p := range pending {
				select {
				case <-p.done:
					if p.err != nil {
						lastErr = p.err
						slog.Info(fmt.Sprintf("Error in engine %s: %s", p.engine.name, pyExcClass(p.err)+"("+pyrepr.StrRepr(p.err.Error())+")"))
						continue
					}
					if len(p.results) > 0 {
						if err := agg.extend(p.results); err != nil {
							lastErr = err
							slog.Info(fmt.Sprintf("Error in engine %s: %s", p.engine.name, pyExcClass(err)+"("+pyrepr.StrRepr(err.Error())+")"))
							continue
						}
						seen[p.engine.provider] = true
					}
				default:
					still = append(still, p)
				}
			}
			pending = still
		}
		if maxResults > 0 && len(agg.keys) >= maxResults {
			break
		}
	}
	for _, f := range all {
		<-f.done
	}
	results, err := ddgsRank(agg.extract(), q.query)
	if err != nil {
		return nil, err
	}
	if len(results) > 0 {
		if maxResults > 0 && len(results) > maxResults {
			results = results[:maxResults]
		}
		return results, nil
	}
	if lastErr != nil && strings.Contains(lastErr.Error(), "timed out") {
		return nil, raise("TimeoutException", "%s", lastErr.Error())
	}
	if lastErr != nil {
		return nil, raise("DDGSException", "%s", lastErr.Error())
	}
	return nil, raise("DDGSException", "No results found.")
}

// Blocking ddgs text query normalized into title first hits, the worker's _run_ddgs_search
func runDDGSSearch(ctx context.Context, query string, safeLimit int) ([]WebSearchHit, error) {
	results, err := ddgsText(ctx, query, safeLimit)
	if err != nil {
		return nil, err
	}
	hits := []WebSearchHit{}
	for i, r := range results {
		if i >= safeLimit {
			break
		}
		href := pyOr(r.href, "")
		hits = append(hits, titleHit(pyrepr.Str(r.title), pyrepr.Str(href), pyrepr.Str(r.body), i+1))
	}
	return hits, nil
}

// Registers every result of one engine, stopping at the first failure like ResultsAggregator.extend
func (a *ddgsAggregator) extend(results []*ddgsTextResult) error {
	for _, r := range results {
		if err := a.append(r); err != nil {
			return err
		}
	}
	return nil
}
