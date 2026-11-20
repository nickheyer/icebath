package pyhttpx

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// One header line as a Python client writes it, its spelling kept
type HeaderLine struct{ Name, Value string }

// Header lines in wire order with the case insensitive editing of httpx Headers
type Headers struct {
	lines []HeaderLine
	// Host written at its carried slot as http.client writes a Host the caller set, else first as h11 writes it
	hostInPlace bool
}

// Marks the lines as an http.client head whose carried Host keeps its slot
func (h *Headers) HostInPlace() *Headers {
	h.hostInPlace = true
	return h
}

// Headers holding the lines in the order given
func NewHeaders(lines ...HeaderLine) *Headers {
	return &Headers{lines: append([]HeaderLine(nil), lines...)}
}

// Copy of the lines in wire order
func (h *Headers) Lines() []HeaderLine { return append([]HeaderLine(nil), h.lines...) }

// First value under the name in any case, false when absent
func (h *Headers) Get(name string) (string, bool) {
	for _, l := range h.lines {
		if strings.EqualFold(l.Name, name) {
			return l.Value, true
		}
	}
	return "", false
}

// Replaces the first line of the name in place under the new spelling and drops the rest, else appends
func (h *Headers) Set(name, value string) {
	at := -1
	kept := h.lines[:0]
	for _, l := range h.lines {
		if strings.EqualFold(l.Name, name) {
			if at >= 0 {
				continue
			}
			at = len(kept)
			l = HeaderLine{name, value}
		}
		kept = append(kept, l)
	}
	h.lines = kept
	if at < 0 {
		h.lines = append(h.lines, HeaderLine{name, value})
	}
}

// First line of the name in any case
func (h *Headers) line(name string) (HeaderLine, bool) {
	for _, l := range h.lines {
		if strings.EqualFold(l.Name, name) {
			return l, true
		}
	}
	return HeaderLine{}, false
}

// Drops every line of the name in any case
func (h *Headers) Del(name string) {
	kept := h.lines[:0]
	for _, l := range h.lines {
		if !strings.EqualFold(l.Name, name) {
			kept = append(kept, l)
		}
	}
	h.lines = kept
}

// Pops every name the other lines carry, then appends all of them in their order
func (h *Headers) Update(other []HeaderLine) {
	for _, l := range other {
		h.Del(l.Name)
	}
	h.lines = append(h.lines, other...)
}

type wireKey struct{}

type targetKey struct{}

// Header map of the lines for readers of the map and the transport validation
func (h *Headers) headerMap() http.Header {
	out := http.Header{}
	for _, l := range h.lines {
		out.Add(l.Name, l.Value)
	}
	return out
}

// Request writing exactly these header lines, its header map rebuilt from them for readers of the map
func WithWire(req *http.Request, h *Headers) *http.Request {
	req.Header = h.headerMap()
	return req.WithContext(context.WithValue(req.Context(), wireKey{}, h))
}

// URL of the request a transport dial serves, an error for a dial made outside a WireTransport
func RequestTarget(ctx context.Context) (*url.URL, error) {
	if u, ok := ctx.Value(targetKey{}).(*url.URL); ok && u != nil {
		return u, nil
	}
	return nil, errors.New("pyhttpx: connection dialed for a request that did not pass through a WireTransport")
}

// Lines a redirect hop writes, the previous hop lines with Authorization and Cookie dropped as httpx _redirect_headers drops them
func redirectLines(prev *Headers, from, to *url.URL) *Headers {
	h := NewHeaders(prev.lines...)
	if !sameOrigin(from, to) && !isHTTPSRedirect(from, to) {
		h.Del("Authorization")
	}
	h.Del("Cookie")
	return h
}

// Port of a URL with the default of its scheme filled in, as httpx _port_or_default reads it
func portOrDefault(u *url.URL) int {
	if p, err := strconv.Atoi(u.Port()); err == nil {
		return p
	}
	return map[string]int{"http": 80, "https": 443}[strings.ToLower(u.Scheme)]
}

// Same scheme, host and port, the httpx _same_origin test
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && portOrDefault(a) == portOrDefault(b)
}

// Plain http on port 80 upgraded to https on port 443 of the same host, the httpx _is_https_redirect test
func isHTTPSRedirect(from, to *url.URL) bool {
	return strings.EqualFold(from.Hostname(), to.Hostname()) && strings.EqualFold(from.Scheme, "http") && portOrDefault(from) == 80 &&
		strings.EqualFold(to.Scheme, "https") && portOrDefault(to) == 443
}

// Header lines the request writes, nil for a request built without them
func WireOf(req *http.Request) *Headers {
	h, _ := req.Context().Value(wireKey{}).(*Headers)
	return h
}

// Connection that rewrites the head of the next request written on it into the lines its request carries
type WireConn struct {
	net.Conn
	mu      sync.Mutex
	pending *Headers
	head    []byte
}

// Wire capable connection over a plaintext HTTP connection
func NewWireConn(c net.Conn) *WireConn { return &WireConn{Conn: c} }

// Connection under the wire layer for socket level shutdown
func (c *WireConn) NetConn() net.Conn { return c.Conn }

func (c *WireConn) expect(h *Headers) {
	c.mu.Lock()
	c.pending, c.head = h, nil
	c.mu.Unlock()
}

// Forgets an expectation whose request never wrote its head
func (c *WireConn) settle(h *Headers) {
	c.mu.Lock()
	if c.pending == h && len(c.head) == 0 {
		c.pending = nil
	}
	c.mu.Unlock()
}

// Holds the bytes of an expected request head until it ends, then writes it in wire order with what follows
func (c *WireConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.pending == nil {
		c.mu.Unlock()
		return c.Conn.Write(p)
	}
	c.head = append(c.head, p...)
	end := bytes.Index(c.head, []byte("\r\n\r\n"))
	if end < 0 {
		c.mu.Unlock()
		return len(p), nil
	}
	out := append(rewriteHead(string(c.head[:end]), c.pending), c.head[end+4:]...)
	c.pending, c.head = nil, nil
	c.mu.Unlock()
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Request head with proxy credentials first, Host first or at its carried slot for http.client, the carried lines in order with framing slotted or last
func rewriteHead(head string, h *Headers) []byte {
	rows := strings.Split(head, "\r\n")
	var b bytes.Buffer
	b.WriteString(rows[0] + "\r\n")
	var proxy []string
	host, framing := map[string]string{}, map[string]string{}
	var hostOrder, framingOrder []string
	for _, row := range rows[1:] {
		name, value, _ := strings.Cut(row, ":")
		value = strings.TrimLeft(value, " \t")
		switch lower := strings.ToLower(name); lower {
		case "host":
			host[lower], hostOrder = value, append(hostOrder, lower)
		case "content-length", "transfer-encoding":
			framing[lower], framingOrder = value, append(framingOrder, lower)
		case "proxy-authorization":
			if _, carried := h.Get(name); !carried {
				proxy = append(proxy, row)
			}
		}
	}
	slotted := map[string]bool{}
	for _, l := range h.lines {
		if _, ok := framing[strings.ToLower(l.Name)]; ok {
			slotted[strings.ToLower(l.Name)] = true
		}
	}
	for _, row := range proxy {
		b.WriteString(row + "\r\n")
	}
	_, hostCarried := h.Get("Host")
	hostCarried = hostCarried && h.hostInPlace
	if !hostCarried {
		for _, name := range hostOrder {
			spelling := "Host"
			if l, ok := h.line(name); ok {
				spelling = l.Name
			}
			b.WriteString(spelling + ": " + host[name] + "\r\n")
		}
	}
	for _, l := range h.lines {
		lower := strings.ToLower(l.Name)
		switch lower {
		case "host":
			if v, ok := host[lower]; ok && hostCarried {
				b.WriteString(l.Name + ": " + v + "\r\n")
			}
			continue
		case "content-length", "transfer-encoding":
			if v, ok := framing[lower]; ok {
				b.WriteString(l.Name + ": " + v + "\r\n")
			}
			continue
		}
		b.WriteString(l.Name + ": " + l.Value + "\r\n")
	}
	for _, name := range framingOrder {
		if !slotted[name] {
			b.WriteString(canonicalFraming[name] + ": " + framing[name] + "\r\n")
		}
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// Spelling net/http writes for each framing line
var canonicalFraming = map[string]string{"content-length": "Content-Length", "transfer-encoding": "Transfer-Encoding"}

// Wire layer of a pooled connection, looked up through every NetConn wrapper short of TLS
func wireConnOf(conn net.Conn) *WireConn {
	for conn != nil {
		switch c := conn.(type) {
		case *WireConn:
			return c
		case *tls.Conn:
			return nil
		case interface{ NetConn() net.Conn }:
			conn = c.NetConn()
		default:
			return nil
		}
	}
	return nil
}

// Transport writing wire requests and their redirect hops in carried order and undoing the content codings the client advertised
type WireTransport struct {
	Base http.RoundTripper
	// Content codings the emulated client decodes, the rest left as received
	Decoders []string
}

// Writes the request in its carried lines, a redirect hop also dropping each name its redirect policy removed
func (t WireTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := context.WithValue(req.Context(), targetKey{}, req.URL)
	h := WireOf(req)
	if h == nil {
		return t.Base.RoundTrip(req.WithContext(ctx))
	}
	hop := req.Response != nil && req.Response.Request != nil
	if hop {
		h = redirectLines(WireOf(req.Response.Request), req.Response.Request.URL, req.URL)
		for _, l := range h.Lines() {
			if !strings.EqualFold(l.Name, "Host") && req.Header.Values(l.Name) == nil {
				h.Del(l.Name)
			}
		}
		ctx = context.WithValue(ctx, wireKey{}, h)
	}
	var bound *WireConn
	var mu sync.Mutex
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if wc := wireConnOf(info.Conn); wc != nil {
			wc.expect(h)
			mu.Lock()
			bound = wc
			mu.Unlock()
		}
	}}
	out := req.WithContext(httptrace.WithClientTrace(ctx, trace))
	if hop {
		out.Header = h.headerMap()
	}
	resp, err := t.Base.RoundTrip(out)
	mu.Lock()
	if bound != nil {
		bound.settle(h)
	}
	mu.Unlock()
	if err != nil {
		return resp, err
	}
	decodeResponseContent(resp, t.Decoders)
	return resp, nil
}

// Drops the idle connections of the base transport
func (t WireTransport) CloseIdleConnections() {
	if c, ok := t.Base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// Wraps the body so it reads decoded through every supported coding in reverse order, headers kept as httpx keeps them
func decodeResponseContent(resp *http.Response, supported []string) {
	var codings []string
	for _, v := range resp.Header.Values("Content-Encoding") {
		for _, c := range strings.Split(v, ",") {
			c = strings.ToLower(strings.TrimSpace(c))
			for _, s := range supported {
				if c == s {
					codings = append(codings, c)
				}
			}
		}
	}
	if len(codings) == 0 || resp.Body == nil {
		return
	}
	body := resp.Body
	var r io.Reader = body
	for i := len(codings) - 1; i >= 0; i-- {
		r = &lazyDecoder{src: r, coding: codings[i]}
	}
	resp.Body = decodedBody{Reader: r, closer: body}
	resp.ContentLength = -1
	resp.Uncompressed = true
}

// Response body reading the decoded stream and closing the raw one
type decodedBody struct {
	io.Reader
	closer io.Closer
}

func (b decodedBody) Close() error { return b.closer.Close() }

// Reader remembering the last error of the stream under a decoder so transport failures pass through undisguised
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}

// Decoder that opens on the first read, deflate taking zlib framing when the stream has it and raw data otherwise
type lazyDecoder struct {
	src    io.Reader
	coding string
	from   *sourceReader
	r      io.Reader
	err    error
}

func (d *lazyDecoder) Read(p []byte) (int, error) {
	if d.r == nil && d.err == nil {
		d.open()
	}
	if d.err != nil {
		return 0, d.err
	}
	n, err := d.r.Read(p)
	return n, d.classify(err)
}

// Transport errors as read, every other failure of the coding a DecodingError
func (d *lazyDecoder) classify(err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	var de *DecodingError
	if d.from.err != nil && errors.Is(err, d.from.err) || errors.As(err, &de) {
		return err
	}
	return &DecodingError{err}
}

func (d *lazyDecoder) open() {
	d.from = &sourceReader{r: d.src}
	br := bufio.NewReader(d.from)
	switch d.coding {
	case "gzip":
		zr, err := gzip.NewReader(br)
		if err != nil {
			d.err = d.classify(err)
			return
		}
		zr.Multistream(false)
		d.r = zr
	case "deflate":
		if hdr, err := br.Peek(2); err == nil && hdr[0]&0x0f == 8 && hdr[0]>>4 <= 7 && (uint16(hdr[0])<<8|uint16(hdr[1]))%31 == 0 {
			zr, err := zlib.NewReader(br)
			if err != nil {
				d.err = d.classify(err)
				return
			}
			d.r = zr
			return
		}
		d.r = flate.NewReader(br)
	case "zstd":
		zr, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1))
		if err != nil {
			d.err = d.classify(err)
			return
		}
		d.r = zr.IOReadCloser()
	default:
		d.r = br
	}
}
