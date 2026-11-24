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
)

// Request the transport failed or whose response could not be read or decoded
type clientError struct {
	text string
	err  error
}

func (e *clientError) Error() string { return e.text }

func (e *clientError) Unwrap() error { return e.err }

// Request or connection attempt that ran out of its time limit
type timeoutError struct {
	what  string
	limit time.Duration
}

func (e *timeoutError) Error() string { return e.what + " timed out after " + e.limit.String() }

// Always true: the error is a timeout
func (e *timeoutError) Timeout() bool { return true }

// The deadline the limit stood for, so errors.Is matches context.DeadlineExceeded
func (e *timeoutError) Unwrap() error { return context.DeadlineExceeded }

// Session errors of a request made before connect or after disconnect
var (
	errNoSession     = errors.New("mattermost session is not open")
	errSessionClosed = errors.New("mattermost session is closed")
)

// Content types of a JSON body
var jsonContentTypeRE = regexp.MustCompile(`^application/(?:[\w.+-]+?\+)?json`)

// One fully read HTTP response
type httpResponse struct {
	status int
	header http.Header
	body   []byte
	url    string
}

// Body text decoded with the declared charset or utf-8
func (r *httpResponse) text() string { return httputil.DecodeText(r.header, r.body) }

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

// Decoded JSON body as plain maps, a clientError for a content type that is not JSON
func (r *httpResponse) json() (any, error) {
	ctype := strings.ToLower(r.header.Get("Content-Type"))
	if !jsonContentTypeRE.MatchString(ctype) {
		return nil, &clientError{text: fmt.Sprintf("HTTP %d response from %s is not JSON (content type %q)", r.status, r.url, ctype)}
	}
	parsed, err := jsonutil.Decode(r.text())
	if err != nil {
		return nil, err
	}
	return jsonutil.Plain(parsed), nil
}

// Sends one request on the session and reads the whole body within the total timeout
func (a *MattermostAdapter) request(ctx context.Context, method, rawURL string, headers map[string]string, contentType string,
	body []byte, total time.Duration) (*httpResponse, error) {
	a.mu.Lock()
	session, closed := a.session, a.sessionClosed
	a.mu.Unlock()
	if session == nil {
		return nil, errNoSession
	}
	if closed {
		return nil, errSessionClosed
	}
	return doRequest(ctx, session, method, rawURL, headers, contentType, body, total)
}

// One request on a client reading the whole body within the total time limit, a timeoutError when the limit runs out and a clientError for a transport failure
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
		return nil, &clientError{text: err.Error(), err: err}
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
		if err != nil {
			err = fmt.Errorf("reading the response of %s %s: %w", method, req.URL.Redacted(), err)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if rctx.Err() != nil {
			return nil, &timeoutError{what: method + " " + req.URL.Redacted(), limit: total}
		}
		return nil, &clientError{text: err.Error(), err: err}
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
