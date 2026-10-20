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
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

const FetchName = "WebFetch"

// Answers a prompt against fetched text, satisfied by a provider
type Completer interface {
	Complete(ctx context.Context, req protocol.Request) (protocol.Response, error)
}

// Settings of the fetch tool
type Options struct {
	// Built from Timeout and AllowPrivate when nil
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
	text string
	at   time.Time
}

type fetch struct {
	Options
	client *http.Client
	mu     sync.Mutex
	cache  map[string]cached
}

// Tool that fetches a URL, converts HTML to text and optionally answers a prompt about it
func Fetch(o Options) (tool.Tool, error) {
	if o.Summarizer != nil && o.Model == "" {
		return nil, errors.New("web: a summarizer needs a model id")
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: o.timeout(), Transport: transport(o.AllowPrivate)}
	}
	f := &fetch{Options: o, client: client, cache: map[string]cached{}}
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

const fetchSchema = `{"type":"object","properties":{
"url":{"type":"string","description":"Absolute http or https URL to fetch"}%s},
"required":["url"%s],"additionalProperties":false}`

func (f *fetch) Definition() protocol.Tool {
	desc := fmt.Sprintf("Fetches a URL and returns its content as text, converting HTML to plain text with links kept as [text](url). Redirects to another host are reported instead of followed. Responses are read up to %d bytes and the text is cut at %d characters. Results are cached for %s.", f.maxBytes(), f.maxText(), f.cacheTTL())
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

// Signals a redirect that leaves the host the model asked for
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
	text, err := f.text(ctx, u)
	var redirect crossHost
	switch {
	case errors.As(err, &redirect):
		return tool.Errorf("%s redirected to %s on another host, fetch that URL explicitly if it is wanted", in.URL, redirect.to), nil
	case err != nil:
		return tool.Error(err.Error()), nil
	}
	if f.Summarizer == nil {
		return tool.Text(text), nil
	}
	return f.answer(ctx, text, *in.Prompt)
}

// Returns the page text from cache or from a fresh request
func (f *fetch) text(ctx context.Context, u *url.URL) (string, error) {
	key := u.String()
	if !f.NoCache {
		f.mu.Lock()
		c, ok := f.cache[key]
		f.mu.Unlock()
		if ok && time.Since(c.at) < f.cacheTTL() {
			return c.text, nil
		}
	}
	text, err := f.request(ctx, u)
	if err != nil {
		return "", err
	}
	if !f.NoCache {
		f.mu.Lock()
		f.cache[key] = cached{text: text, at: time.Now()}
		f.mu.Unlock()
	}
	return text, nil
}

func (f *fetch) request(ctx context.Context, u *url.URL) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", f.userAgent())
	req.Header.Set("Accept", "text/html, text/plain;q=0.9, application/json;q=0.9, */*;q=0.8")
	client := *f.client
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if r.URL.Host != u.Host {
			return crossHost{to: r.URL.String()}
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		var redirect crossHost
		if errors.As(err, &redirect) {
			return "", redirect
		}
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("%s returned HTTP %d", u, resp.StatusCode)
	}
	kind, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(f.maxBytes())+1))
	if err != nil {
		return "", err
	}
	cut := len(body) > f.maxBytes()
	if cut {
		body = body[:f.maxBytes()]
	}
	var text string
	switch {
	case kind == "text/html" || kind == "application/xhtml+xml":
		text = Text(string(body))
	case strings.HasPrefix(kind, "text/") || kind == "application/json" || kind == "application/xml" || strings.HasSuffix(kind, "+json") || strings.HasSuffix(kind, "+xml") || kind == "":
		text = string(body)
	default:
		return "", fmt.Errorf("%s has unsupported content type %s", u, kind)
	}
	if len(text) > f.maxText() {
		text = text[:f.maxText()] + fmt.Sprintf("\n[text cut at %d characters]", f.maxText())
	} else if cut {
		text += fmt.Sprintf("\n[page cut at %d bytes]", f.maxBytes())
	}
	return fmt.Sprintf("URL: %s\nContent-Type: %s\n\n%s", resp.Request.URL, kind, text), nil
}

// Asks the summarizer to answer the prompt using only the fetched text
func (f *fetch) answer(ctx context.Context, text, prompt string) (tool.Result, error) {
	req := protocol.Request{
		Model:     f.Model,
		System:    "You answer questions about a web page using only its content below. Quote exact wording where it matters and say plainly when the page does not contain the answer.",
		MaxTokens: f.maxSummaryTokens(),
		Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{
			protocol.Text{Text: "<page>\n" + text + "\n</page>\n\nPrompt: " + prompt},
		}}},
	}
	resp, err := f.Summarizer.Complete(ctx, req)
	if err != nil {
		return tool.Errorf("summarizer failed: %v", err), nil
	}
	var sb strings.Builder
	for _, p := range resp.Content {
		if t, ok := p.(protocol.Text); ok {
			sb.WriteString(t.Text)
		}
	}
	if sb.Len() == 0 {
		return tool.Error("summarizer returned no text"), nil
	}
	return tool.Text(sb.String()), nil
}
