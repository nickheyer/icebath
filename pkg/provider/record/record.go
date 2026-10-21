// Records HTTP exchanges to a cassette file and replays them without a network
package record

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

type Mode int

const (
	// Replays recorded exchanges and records anything new
	Auto Mode = iota
	// Sends every request and records it, replacing the cassette
	Record
	// Serves only recorded exchanges and fails on anything else
	Replay
)

// Request and response headers stripped before an exchange is written
var Redacted = []string{"Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "X-Goog-Api-Key", "X-Amz-Security-Token", "Proxy-Authorization"}

// Query parameters whose values are replaced before an exchange is written or matched
var RedactedQuery = []string{"key", "api_key", "apikey", "api-key", "access_token", "token", "sig", "signature", "X-Amz-Signature", "X-Amz-Security-Token", "X-Amz-Credential"}

// Value written in place of a redacted query parameter
const redacted = "redacted"

// One recorded request and the response it received
type Exchange struct {
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Header         http.Header `json:"header"`
	Body           string      `json:"body"`
	BodyBase64     bool        `json:"body_base64,omitempty"`
	Status         int         `json:"status"`
	ResponseHeader http.Header `json:"response_header"`
	Response       string      `json:"response"`
	ResponseBase64 bool        `json:"response_base64,omitempty"`
}

func key(method, url string, body []byte) string {
	sum := sha256.Sum256(body)
	return method + " " + url + " " + hex.EncodeToString(sum[:])
}

// Cassette of exchanges backed by one JSON file
type Cassette struct {
	path      string
	mode      Mode
	mu        sync.Mutex
	exchanges []Exchange
	// Slots whose response body is still being read
	open   map[int]bool
	cursor map[string]int
	dirty  bool
	// Extra header names stripped before writing
	Redact []string
	// Extra query parameter names whose values are replaced before writing or matching
	RedactQuery []string
}

// Opens the cassette at path, loading what it holds unless recording from scratch
func Open(path string, mode Mode) (*Cassette, error) {
	c := &Cassette{path: path, mode: mode, cursor: map[string]int{}}
	if mode == Record {
		return c, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && mode == Auto:
		return c, nil
	case err != nil:
		return nil, fmt.Errorf("record: %w", err)
	}
	if err := json.Unmarshal(data, &c.exchanges); err != nil {
		return nil, fmt.Errorf("record: %s: %w", path, err)
	}
	if err := check(path, c.exchanges); err != nil {
		return nil, err
	}
	return c, nil
}

// Rejects exchanges missing their identity or holding bodies that do not decode
func check(path string, exchanges []Exchange) error {
	for i, ex := range exchanges {
		if ex.Method == "" || ex.URL == "" || ex.Status == 0 {
			return fmt.Errorf("record: %s: exchange %d is incomplete", path, i)
		}
		if _, err := decodeBody(ex.Body, ex.BodyBase64); err != nil {
			return fmt.Errorf("record: %s: exchange %d: request body: %w", path, i, err)
		}
		if _, err := decodeBody(ex.Response, ex.ResponseBase64); err != nil {
			return fmt.Errorf("record: %s: exchange %d: %w", path, i, err)
		}
	}
	return nil
}

func (c *Cassette) Mode() Mode { return c.mode }

// Exchanges whose responses have arrived, in request order
func (c *Cassette) Exchanges() []Exchange {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.complete()
}

func (c *Cassette) complete() []Exchange {
	out := make([]Exchange, 0, len(c.exchanges))
	for i, ex := range c.exchanges {
		if !c.open[i] {
			out = append(out, ex)
		}
	}
	return out
}

// Writes the cassette when anything was recorded
func (c *Cassette) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}
	data, err := json.MarshalIndent(c.complete(), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(c.path, data, 0o600); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

type transport struct {
	c    *Cassette
	next http.RoundTripper
}

// Round tripper that answers from the cassette or records through next, the default transport when nil
func (c *Cassette) Transport(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return transport{c: c, next: next}
}

// Client using the cassette transport
func (c *Cassette) Client(next http.RoundTripper) *http.Client {
	return &http.Client{Transport: c.Transport(next)}
}

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := readBody(r)
	if err != nil {
		return nil, err
	}
	k := key(r.Method, t.c.scrubURL(r.URL.String()), body)
	if t.c.mode != Record {
		if resp, ok := t.c.replay(k, r); ok {
			return resp, nil
		}
		if t.c.mode == Replay {
			return nil, fmt.Errorf("record: no recorded exchange for %s %s", r.Method, r.URL)
		}
	}
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	ex := Exchange{Method: r.Method, URL: t.c.scrubURL(r.URL.String()), Header: t.c.scrub(r.Header), Status: resp.StatusCode, ResponseHeader: t.c.scrub(resp.Header)}
	ex.Body, ex.BodyBase64 = encodeBody(body)
	resp.Body = &recording{ReadCloser: resp.Body, c: t.c, slot: t.c.reserve(k, ex)}
	return resp, nil
}

// Takes the next slot for the exchange so concurrent requests keep request order whatever order their bodies drain
func (c *Cassette) reserve(k string, ex Exchange) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exchanges = append(c.exchanges, ex)
	slot := len(c.exchanges) - 1
	if c.open == nil {
		c.open = map[int]bool{}
	}
	c.open[slot] = true
	c.cursor[k]++
	return slot
}

// Body that passes bytes through as they arrive and completes its slot once the body is drained or closed
type recording struct {
	io.ReadCloser
	c    *Cassette
	slot int
	buf  bytes.Buffer
	once sync.Once
}

func (b *recording) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.buf.Write(p[:n])
	if err == io.EOF {
		b.store()
	}
	return n, err
}

func (b *recording) Close() error {
	b.store()
	return b.ReadCloser.Close()
}

func (b *recording) store() {
	b.once.Do(func() {
		b.c.mu.Lock()
		ex := &b.c.exchanges[b.slot]
		ex.Response, ex.ResponseBase64 = encodeBody(b.buf.Bytes())
		delete(b.c.open, b.slot)
		b.c.dirty = true
		b.c.mu.Unlock()
	})
}

// Serves the next unused exchange matching the key
func (c *Cassette) replay(k string, r *http.Request) (*http.Response, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := 0
	for i, ex := range c.exchanges {
		if c.open[i] || c.exchangeKey(ex) != k {
			continue
		}
		if seen < c.cursor[k] {
			seen++
			continue
		}
		c.cursor[k]++
		data, _ := decodeBody(ex.Response, ex.ResponseBase64)
		return &http.Response{
			Status:        fmt.Sprintf("%d %s", ex.Status, http.StatusText(ex.Status)),
			StatusCode:    ex.Status,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        ex.ResponseHeader.Clone(),
			Body:          io.NopCloser(bytes.NewReader(data)),
			ContentLength: int64(len(data)),
			Request:       r,
		}, true
	}
	return nil, false
}

// Key of a stored exchange, computed over its redacted URL
func (c *Cassette) exchangeKey(e Exchange) string {
	body, _ := decodeBody(e.Body, e.BodyBase64)
	return key(e.Method, c.scrubURL(e.URL), body)
}

// Replaces the values of redacted query parameters, leaving every other URL untouched
func (c *Cassette) scrubURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	drop := map[string]bool{}
	for _, name := range append(append([]string(nil), RedactedQuery...), c.RedactQuery...) {
		drop[strings.ToLower(name)] = true
	}
	values := u.Query()
	changed := false
	for name := range values {
		if drop[strings.ToLower(name)] {
			values[name] = []string{redacted}
			changed = true
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = values.Encode()
	return u.String()
}

// Copies headers without the redacted ones
func (c *Cassette) scrub(h http.Header) http.Header {
	drop := map[string]bool{}
	for _, name := range append(append([]string(nil), Redacted...), c.Redact...) {
		drop[http.CanonicalHeaderKey(name)] = true
	}
	out := http.Header{}
	for k, vs := range h {
		if !drop[http.CanonicalHeaderKey(k)] {
			out[k] = append([]string(nil), vs...)
		}
	}
	return out
}

// Reads the request body and puts it back
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	data, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

// Stores text as is and anything else as base64
func encodeBody(data []byte) (string, bool) {
	if utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
		return string(data), false
	}
	return base64.StdEncoding.EncodeToString(data), true
}

func decodeBody(s string, encoded bool) ([]byte, error) {
	if !encoded {
		return []byte(s), nil
	}
	return base64.StdEncoding.DecodeString(s)
}

// Names of every recorded URL, sorted and unique
func (c *Cassette) URLs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, ex := range c.complete() {
		if !seen[ex.URL] {
			seen[ex.URL] = true
			out = append(out, ex.URL)
		}
	}
	sort.Strings(out)
	return out
}

// Reports whether a cassette file holds valid exchanges
func Valid(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) == "" {
		return fmt.Errorf("record: %s is empty", path)
	}
	var exchanges []Exchange
	if err := json.Unmarshal(data, &exchanges); err != nil {
		return fmt.Errorf("record: %s: %w", path, err)
	}
	return check(path, exchanges)
}
