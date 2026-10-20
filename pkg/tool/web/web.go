// Web tool that fetches a URL and returns its content as readable text
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

const FetchName = "WebFetch"

// Entries the cache holds before the oldest is dropped
const MaxCached = 64

// Answers a prompt against fetched text, satisfied by a provider
type Completer interface {
	Complete(ctx context.Context, req protocol.Request) (protocol.Response, error)
}

// Settings of the fetch tool
type Options struct {
	// Used as given when set, with the private address guard applied before each request
	Client *http.Client
	// Body bytes read before the page is cut, one MiB when zero
	MaxBytes int
	// Characters of text returned, 100000 when zero
	MaxText int
	// Whole request budget, thirty seconds when zero
	Timeout time.Duration
	// Sent as the User-Agent header, icebath when empty
	UserAgent string
	// Permits loopback, private and link local targets
	AllowPrivate bool
	// How long fetched text is served again without a request, fifteen minutes when zero
	CacheTTL time.Duration
	// Disables the cache
	NoCache bool
	// Model that answers the prompt argument when set
	Summarizer Completer
	// Model id the summarizer is called with
	Model string
	// Output budget of the summarizer, 1024 when zero
	MaxSummaryTokens int
}

// Structured outcome of a fetch
type Page struct {
	URL         string
	FinalURL    string
	Status      int
	ContentType string
	Charset     string
	Title       string
	Text        string
	Bytes       int
	Cached      bool
	Duration    time.Duration
	// Answer the summarizer gave when a prompt was supplied
	Answer string
}

func (o Options) maxBytes() int           { return orInt(o.MaxBytes, 1<<20) }
func (o Options) maxText() int            { return orInt(o.MaxText, 100000) }
func (o Options) timeout() time.Duration  { return orDur(o.Timeout, 30*time.Second) }
func (o Options) cacheTTL() time.Duration { return orDur(o.CacheTTL, 15*time.Minute) }
func (o Options) maxSummaryTokens() int   { return orInt(o.MaxSummaryTokens, 1024) }

func (o Options) userAgent() string {
	if o.UserAgent != "" {
		return o.UserAgent
	}
	return "icebath"
}

func orInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func orDur(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

type cached struct {
	page Page
	at   time.Time
}

type fetch struct {
	Options
	client *http.Client
	custom bool
	mu     sync.Mutex
	cache  map[string]cached
}

// Tool that fetches a URL, converts HTML to text and optionally answers a prompt about it
func Fetch(o Options) (tool.Tool, error) {
	if o.Summarizer != nil && o.Model == "" {
		return nil, errors.New("web: a summarizer needs a model id")
	}
	client := o.Client
	custom := client != nil
	if client == nil {
		client = &http.Client{Timeout: o.timeout(), Transport: transport(o.AllowPrivate)}
	}
	f := &fetch{Options: o, client: client, custom: custom, cache: map[string]cached{}}
	return f, nil
}

// Transport that resolves each host itself and refuses addresses outside the public range
func transport(allowPrivate bool) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if !allowPrivate {
				for _, ip := range ips {
					if isPrivate(ip.IP) {
						return nil, fmt.Errorf("%s resolves to the private address %s", host, ip.IP)
					}
				}
			}
			var last error
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if err == nil {
					return conn, nil
				}
				last = err
			}
			return nil, last
		},
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

func isPrivate(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// Refuses a host whose addresses fall in the private range, used ahead of requests through a supplied client
func guard(ctx context.Context, host string) error {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if isPrivate(ip.IP) {
			return fmt.Errorf("%s resolves to the private address %s", host, ip.IP)
		}
	}
	return nil
}

const fetchSchema = `{"type":"object","properties":{
"url":{"type":"string","description":"Absolute http or https URL to fetch"}%s},
"required":["url"%s],"additionalProperties":false}`

func (f *fetch) Definition() protocol.Tool {
	desc := fmt.Sprintf("Fetches a URL and returns its content as text, converting HTML to plain text with links kept as [text](url) and code blocks fenced. Redirects to another site are reported instead of followed. Responses are read up to %d bytes and the text is cut at %d characters. Results are cached for %s.", f.maxBytes(), f.maxText(), f.cacheTTL())
	props, req := "", ""
	if f.Summarizer != nil {
		desc += " With a prompt, the page is read by a model and only its answer to the prompt is returned."
		props = `,
"prompt":{"type":"string","description":"Question to answer from the page content"}`
		req = `,"prompt"`
	}
	return protocol.Tool{Name: FetchName, Description: desc, Parameters: json.RawMessage(fmt.Sprintf(fetchSchema, props, req))}
}

func (*fetch) Annotations() tool.Annotations {
	return tool.Annotations{ReadOnly: true, OpenWorld: true}
}

// Signals a redirect that leaves the site the model asked for
type crossHost struct{ to string }

func (c crossHost) Error() string { return "redirected to " + c.to }

func (f *fetch) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in struct {
		URL    string  `json:"url"`
		Prompt *string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return tool.Errorf("url must be an absolute http or https URL, got %q", in.URL), nil
	}
	if in.Prompt != nil && f.Summarizer == nil {
		return tool.Error("prompt is not supported, fetch the page and read it instead"), nil
	}
	if f.Summarizer != nil && (in.Prompt == nil || strings.TrimSpace(*in.Prompt) == "") {
		return tool.Error("prompt is required"), nil
	}
	page, err := f.page(ctx, u)
	var redirect crossHost
	switch {
	case errors.As(err, &redirect):
		return tool.Errorf("%s redirected to %s on another host, fetch that URL explicitly if it is wanted", in.URL, redirect.to), nil
	case err != nil:
		return tool.Error(err.Error()), nil
	}
	if f.Summarizer == nil {
		return tool.Text(page.render()).With(page), nil
	}
	return f.answer(ctx, page, *in.Prompt)
}

// Header lines and body the model receives
func (p Page) render() string {
	return fmt.Sprintf("URL: %s\nContent-Type: %s\n\n%s", p.FinalURL, p.ContentType, p.Text)
}

// Returns the page from cache or from a fresh request
func (f *fetch) page(ctx context.Context, u *url.URL) (Page, error) {
	key := u.String()
	if !f.NoCache {
		f.mu.Lock()
		c, ok := f.cache[key]
		f.mu.Unlock()
		if ok && time.Since(c.at) < f.cacheTTL() {
			c.page.Cached = true
			return c.page, nil
		}
	}
	page, err := f.request(ctx, u)
	if err != nil {
		return Page{}, err
	}
	if !f.NoCache {
		f.store(key, page)
	}
	return page, nil
}

// Caches a page, dropping expired entries and then the oldest until the cache fits
func (f *fetch) store(key string, page Page) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for k, c := range f.cache {
		if now.Sub(c.at) >= f.cacheTTL() {
			delete(f.cache, k)
		}
	}
	f.cache[key] = cached{page: page, at: now}
	if len(f.cache) <= MaxCached {
		return
	}
	keys := make([]string, 0, len(f.cache))
	for k := range f.cache {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return f.cache[keys[i]].at.Before(f.cache[keys[j]].at) })
	for _, k := range keys[:len(keys)-MaxCached] {
		delete(f.cache, k)
	}
}

// Reports whether a redirect stays on the same site, port and www prefix aside
func sameSite(from, to *url.URL) bool {
	a, b := strings.ToLower(from.Hostname()), strings.ToLower(to.Hostname())
	return a == b || strings.TrimPrefix(a, "www.") == strings.TrimPrefix(b, "www.")
}

func (f *fetch) request(ctx context.Context, u *url.URL) (Page, error) {
	if f.custom {
		if !f.AllowPrivate {
			if err := guard(ctx, u.Hostname()); err != nil {
				return Page{}, err
			}
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.timeout())
		defer cancel()
	}
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("User-Agent", f.userAgent())
	req.Header.Set("Accept", "text/html, text/plain;q=0.9, application/json;q=0.9, */*;q=0.8")
	client := *f.client
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if !sameSite(u, r.URL) {
			return crossHost{to: r.URL.String()}
		}
		if f.custom && !f.AllowPrivate {
			return guard(r.Context(), r.URL.Hostname())
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		var redirect crossHost
		if errors.As(err, &redirect) {
			return Page{}, redirect
		}
		return Page{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Page{}, fmt.Errorf("%s returned HTTP %d", u, resp.StatusCode)
	}
	kind, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(f.maxBytes())+1))
	if err != nil {
		return Page{}, err
	}
	cut := len(body) > f.maxBytes()
	if cut {
		body = body[:f.maxBytes()]
	}
	isHTML := kind == "text/html" || kind == "application/xhtml+xml"
	charset := strings.ToLower(params["charset"])
	if charset == "" && isHTML {
		charset = sniffCharset(body)
	}
	decoded, charset, err := decode(body, charset)
	if err != nil {
		return Page{}, err
	}
	page := Page{URL: u.String(), FinalURL: resp.Request.URL.String(), Status: resp.StatusCode, ContentType: kind, Charset: charset, Bytes: len(body)}
	switch {
	case isHTML:
		page.Title = title(decoded)
		page.Text = TextFrom(decoded, resp.Request.URL)
	case strings.HasPrefix(kind, "text/") || kind == "application/json" || kind == "application/xml" || strings.HasSuffix(kind, "+json") || strings.HasSuffix(kind, "+xml") || kind == "":
		page.Text = decoded
	default:
		return Page{}, fmt.Errorf("%s has unsupported content type %s", u, kind)
	}
	if n := utf8.RuneCountInString(page.Text); n > f.maxText() {
		runes := []rune(page.Text)
		page.Text = string(runes[:f.maxText()]) + fmt.Sprintf("\n[text cut at %d characters]", f.maxText())
	} else if cut {
		page.Text += fmt.Sprintf("\n[page cut at %d bytes]", f.maxBytes())
	}
	page.Duration = time.Since(started)
	return page, nil
}

// Converts body to UTF-8 from the named charset, passing UTF-8 and unnamed content through
func decode(body []byte, charset string) (string, string, error) {
	switch charset {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return string(body), "utf-8", nil
	}
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return "", "", fmt.Errorf("unsupported charset %q", charset)
	}
	out, _, err := transform.Bytes(enc.NewDecoder(), body)
	if err != nil {
		return "", "", fmt.Errorf("decoding %s: %w", charset, err)
	}
	return string(out), charset, nil
}

// Asks the summarizer to answer the prompt using only the fetched text
func (f *fetch) answer(ctx context.Context, page Page, prompt string) (tool.Result, error) {
	req := protocol.Request{
		Model:     f.Model,
		System:    "You answer questions about a web page using only its content below. Quote exact wording where it matters and say plainly when the page does not contain the answer.",
		MaxTokens: f.maxSummaryTokens(),
		Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{
			protocol.Text{Text: "<page>\n" + page.render() + "\n</page>\n\nPrompt: " + prompt},
		}}},
	}
	resp, err := f.Summarizer.Complete(ctx, req)
	if err != nil {
		return tool.Errorf("summarizer failed: %v", err), nil
	}
	text := resp.Text()
	if text == "" {
		return tool.Error("summarizer returned no text"), nil
	}
	page.Answer = text
	return tool.Text(text).With(page), nil
}
