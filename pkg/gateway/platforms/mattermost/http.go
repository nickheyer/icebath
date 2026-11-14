package mattermost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// aiohttp ClientError of a request the transport or the response decoding failed
type clientError struct{ text string }

func (e *clientError) Error() string { return e.text }

// asyncio TimeoutError of a request that outlasted its total timeout, its str empty
type timeoutError struct{}

func (e *timeoutError) Error() string { return "" }

// Content types the aiohttp json reader accepts for a JSON body
var jsonContentTypeRE = regexp.MustCompile(`^application/(?:[\w.+-]+?\+)?json`)

// One fully read HTTP response
type httpResponse struct {
	status int
	header http.Header
	body   []byte
	url    string
}

// Body text decoded with the declared charset or utf-8
func (r *httpResponse) text() string { return pyhttpx.DecodeText(r.header, r.body) }

// Media type of the response without parameters, octet stream when the header is absent
func (r *httpResponse) contentType() string {
	raw := r.header.Get("Content-Type")
	if raw == "" {
		return "application/octet-stream"
	}
	if mt, _, err := mime.ParseMediaType(raw); err == nil {
		return mt
	}
	head, _, _ := strings.Cut(raw, ";")
	return strings.ToLower(strings.TrimSpace(head))
}

// Decoded JSON body as plain maps, the ContentTypeError aiohttp raises for a non JSON content type
func (r *httpResponse) json() (any, error) {
	ctype := strings.ToLower(r.header.Get("Content-Type"))
	if !jsonContentTypeRE.MatchString(ctype) {
		return nil, &clientError{text: fmt.Sprintf("%d, message=%s, url=%s", r.status,
			pyrepr.Repr("Attempt to decode JSON with unexpected mimetype: "+ctype), pyrepr.Repr(r.url))}
	}
	parsed, err := pyjson.Loads(r.text())
	if err != nil {
		return nil, err
	}
	return pyjson.Plain(parsed), nil
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

// str of the aiohttp ClientError a failed request raises
func clientErrorText(rawURL string, err error) string {
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.As(err, &dns):
		host, port := hostPort(rawURL)
		reason := "Name or service not known"
		if dns.IsTemporary || dns.IsTimeout {
			reason = "Temporary failure in name resolution"
		}
		return fmt.Sprintf("Cannot connect to host %s:%s ssl:default [%s]", host, port, reason)
	case errors.As(err, &op) && op.Op == "dial":
		host, port := hostPort(rawURL)
		reason := pyhttpx.OSErrorText(err)
		if tcp, ok := op.Addr.(*net.TCPAddr); ok {
			reason = fmt.Sprintf("Connect call failed ('%s', %d)", tcp.IP.String(), tcp.Port)
		}
		return fmt.Sprintf("Cannot connect to host %s:%s ssl:default [%s]", host, port, reason)
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "Server disconnected"
	}
	return pyhttpx.OSErrorText(err)
}

// Sends one request on the session and reads the whole body within the total timeout
func (a *MattermostAdapter) request(ctx context.Context, method, rawURL string, headers map[string]string, contentType string,
	body []byte, total time.Duration) (*httpResponse, error) {
	a.mu.Lock()
	session, closed := a.session, a.sessionClosed
	a.mu.Unlock()
	if session == nil {
		return nil, &gateway.PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute '" + strings.ToLower(method) + "'"}
	}
	if closed {
		return nil, &gateway.PyError{Kind: "RuntimeError", Msg: "Session is closed"}
	}
	return doRequest(ctx, session, method, rawURL, headers, contentType, body, total)
}

// One request on a client, a timeout as the asyncio TimeoutError and a transport failure as the aiohttp ClientError
func doRequest(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string, contentType string,
	body []byte, total time.Duration) (*httpResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, rawURL, reader)
	if err != nil {
		return nil, &clientError{text: rawURL}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
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
		return nil, &clientError{text: clientErrorText(rawURL, err)}
	}
	return &httpResponse{status: resp.StatusCode, header: resp.Header, body: raw, url: resp.Request.URL.String()}, nil
}

// Multipart form with a channel id field and one file part carrying its name and content type
func uploadForm(channelID string, fileData []byte, filename, contentType string) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("channel_id", channelID); err != nil {
		return nil, "", err
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "files", "filename": filename}))
	header.Set("Content-Type", contentType)
	part, err := w.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(fileData); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}
