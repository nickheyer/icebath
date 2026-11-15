package homeassistant

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/ianaindex"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// aiohttp ClientError of a request the transport failed, err the os error behind it
type clientError struct {
	kind string
	text string
	err  error
}

func (e *clientError) Error() string      { return e.text }
func (e *clientError) PyTypeName() string { return e.kind }
func (e *clientError) Unwrap() error      { return e.err }

// asyncio TimeoutError of a request that outlasted its total timeout, its str empty
type timeoutError struct{}

func (e *timeoutError) Error() string      { return "" }
func (e *timeoutError) PyTypeName() string { return "TimeoutError" }

// aiohttp ClientResponseError with its status, message and request URL, ContentTypeError for a non JSON body
type responseError struct {
	status      int
	message     string
	url         string
	contentType bool
}

func (e *responseError) Error() string {
	return fmt.Sprintf("%d, message=%s, url=%s", e.status, pyrepr.Repr(e.message), pyrepr.Repr(e.url))
}

func (e *responseError) PyTypeName() string {
	if e.contentType {
		return "ContentTypeError"
	}
	return "ClientResponseError"
}

// Content types the aiohttp json reader accepts for a JSON body
var jsonContentTypeRE = regexp.MustCompile(`^application/(?:[\w.+-]+?\+)?json`)

// One fully read HTTP response
type httpResponse struct {
	status int
	reason string
	header http.Header
	body   []byte
	url    string
}

// Media type of the response without parameters, octet stream when the header is absent
func (r *httpResponse) contentType() string {
	raw := r.header.Get("Content-Type")
	if raw == "" {
		return "application/octet-stream"
	}
	if mt, _, err := mime.ParseMediaType(raw); err == nil {
		return strings.ToLower(mt)
	}
	head, _, _ := strings.Cut(raw, ";")
	return strings.ToLower(strings.TrimSpace(head))
}

// Body text under the declared charset or a strict utf-8 decode, the decode error aiohttp text raises
func (r *httpResponse) decode(body []byte) (string, error) {
	charset := ""
	if raw := r.header.Get("Content-Type"); raw != "" {
		if _, params, err := mime.ParseMediaType(raw); err == nil {
			charset = strings.ToLower(strings.Trim(params["charset"], `"' `))
		}
	}
	switch charset {
	case "", "utf-8", "utf8", "u8", "utf":
		return pystr.DecodeUTF8(body)
	}
	enc, err := ianaindex.IANA.Encoding(charset)
	if err != nil || enc == nil {
		return pystr.DecodeUTF8(body)
	}
	out, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		return "", &pyErr{"UnicodeDecodeError", err.Error()}
	}
	return string(out), nil
}

// aiohttp text of the body
func (r *httpResponse) text() (string, error) { return r.decode(r.body) }

// aiohttp json of the body, None for a blank body and the ContentTypeError of a non JSON content type
func (r *httpResponse) json() (any, error) {
	ctype := r.contentType()
	if !jsonContentTypeRE.MatchString(ctype) {
		return nil, &responseError{status: r.status, message: "Attempt to decode JSON with unexpected mimetype: " + ctype, url: r.url,
			contentType: true}
	}
	stripped := bytes.Trim(r.body, " \t\n\r\x0b\x0c")
	if len(stripped) == 0 {
		return nil, nil
	}
	text, err := r.decode(stripped)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

// Client like an aiohttp session, proxy and certificate env honoured only when trustEnv
func newSession(trustEnv bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !trustEnv {
		transport.Proxy = nil
	}
	return &http.Client{Transport: transport}
}

// Host and port of a URL with the scheme default port
func hostPort(rawURL string) (string, string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL, ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" || u.Scheme == "wss" {
			port = "443"
		}
	}
	return u.Hostname(), port
}

// aiohttp ClientError a failed request raises, the connector text for a DNS or dial failure
func transportError(rawURL string, err error) error {
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.As(err, &dns):
		host, port := hostPort(rawURL)
		reason := "Name or service not known"
		if dns.IsTemporary || dns.IsTimeout {
			reason = "Temporary failure in name resolution"
		}
		return &clientError{kind: "ClientConnectorDNSError", text: fmt.Sprintf("Cannot connect to host %s:%s ssl:default [%s]", host, port, reason), err: err}
	case errors.As(err, &op) && op.Op == "dial":
		host, port := hostPort(rawURL)
		reason := pyhttpx.OSErrorText(err)
		if tcp, ok := op.Addr.(*net.TCPAddr); ok {
			reason = fmt.Sprintf("Connect call failed ('%s', %d)", tcp.IP.String(), tcp.Port)
		}
		return &clientError{kind: "ClientConnectorError", text: fmt.Sprintf("Cannot connect to host %s:%s ssl:default [%s]", host, port, reason), err: err}
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return &clientError{kind: "ServerDisconnectedError", text: "Server disconnected", err: err}
	}
	return &clientError{kind: "ClientOSError", text: pyhttpx.OSErrorText(err), err: err}
}

// One request reading the whole body within the total timeout, a timeout as the asyncio TimeoutError
func doRequest(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string, body []byte,
	total time.Duration) (*httpResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, rawURL, reader)
	if err != nil {
		return nil, &clientError{kind: "InvalidUrlClientError", text: rawURL, err: err}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if rctx.Err() != nil {
			return nil, &timeoutError{}
		}
		return nil, transportError(rawURL, err)
	}
	reason := strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" ")
	return &httpResponse{status: resp.StatusCode, reason: reason, header: resp.Header, body: raw, url: resp.Request.URL.String()}, nil
}

// One JSON POST, the payload encoded as json.dumps does with the JSON content type
func postJSON(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, payload any,
	total time.Duration) (*httpResponse, error) {
	text, err := pyjson.Dumps(payload)
	if err != nil {
		return nil, err
	}
	merged := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		merged[k] = v
	}
	return doRequest(ctx, client, http.MethodPost, rawURL, merged, []byte(text), total)
}
