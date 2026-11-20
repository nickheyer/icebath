package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
)

// Transport moves JSON-RPC messages between the client and one server
type Transport interface {
	// Send delivers one message to the server
	Send(ctx context.Context, msg *Message) error
	// Receive returns the channel of inbound messages, closed when the transport ends
	Receive() <-chan *Message
	// Err returns why the transport ended, nil while running or on a clean close
	Err() error
	// Close releases the connection and every child it owns
	Close() error
	// Label names the transport in diagnostics
	Label() string
}

// Wire body cap applied before any JSON parse, per finite body and per SSE event
const MaxHTTPBodyBytes = 10 * 1024 * 1024

// ErrBodyTooLarge marks a response or event over the wire cap
var ErrBodyTooLarge = errors.New("MCP response exceeds the wire body cap")

// SSEEvent is one parsed server sent event
type SSEEvent struct {
	ID    string
	Event string
	Data  string
	Retry string
}

// SSEReader parses a text/event-stream body one event at a time under a per event byte cap
type SSEReader struct {
	r      *bufio.Reader
	limit  int
	src    string
	skipLF bool
}

// NewSSEReader wraps a stream body, capping each event at limit bytes
func NewSSEReader(body io.Reader, limit int, source string) *SSEReader {
	return &SSEReader{r: bufio.NewReaderSize(body, 64*1024), limit: limit, src: source}
}

// Next returns the next complete event, io.EOF at a clean end, and the cap error on an oversized event
func (s *SSEReader) Next() (*SSEEvent, error) {
	ev := &SSEEvent{}
	var data []string
	counted := 0
	seen := false
	for {
		line, err := s.readLine()
		if err != nil {
			if err == io.EOF && seen && len(data) > 0 {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			return nil, err
		}
		counted += len(line) + 1
		if counted > s.limit {
			return nil, fmt.Errorf("MCP SSE event exceeds %d bytes (from %s): %w", s.limit, s.src, ErrBodyTooLarge)
		}
		if len(line) == 0 {
			if !seen {
				continue
			}
			ev.Data = strings.Join(data, "\n")
			return ev, nil
		}
		seen = true
		if line[0] == ':' {
			continue
		}
		field, value := line, ""
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], string(line[i+1:])
			value = strings.TrimPrefix(value, " ")
		}
		switch string(field) {
		case "data":
			data = append(data, value)
		case "event":
			ev.Event = value
		case "id":
			if !strings.ContainsRune(value, 0) {
				ev.ID = value
			}
		case "retry":
			ev.Retry = value
		}
	}
}

// readLine returns one line ended by CR, LF, or CRLF without its terminator
func (s *SSEReader) readLine() ([]byte, error) {
	var buf []byte
	for {
		window, err := s.r.Peek(1)
		if err != nil {
			if len(buf) == 0 {
				return nil, err
			}
			return buf, nil
		}
		if s.skipLF {
			s.skipLF = false
			if window[0] == '\n' {
				_, _ = s.r.Discard(1)
				continue
			}
		}
		window, _ = s.r.Peek(s.r.Buffered())
		i := bytes.IndexAny(window, "\r\n")
		if i < 0 {
			buf = append(buf, window...)
			_, _ = s.r.Discard(len(window))
			if len(buf) > s.limit {
				return nil, fmt.Errorf("MCP SSE line exceeds %d bytes (from %s): %w", s.limit, s.src, ErrBodyTooLarge)
			}
			continue
		}
		buf = append(buf, window[:i]...)
		s.skipLF = window[i] == '\r'
		_, _ = s.r.Discard(i + 1)
		return buf, nil
	}
}

// readCapped reads a whole body refusing anything over limit bytes
func readCapped(body io.Reader, limit int, source string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("MCP HTTP response exceeds %d bytes (from %s): %w", limit, source, ErrBodyTooLarge)
	}
	return data, nil
}

// Boundary carry is the longest SSE event boundary, CRLF CRLF, minus one byte
const sseBoundaryCarry = 3

// sseTerminator returns the length of the CR, LF, or CRLF line terminator starting at i, zero when none
func sseTerminator(b []byte, i int) int {
	switch {
	case i >= len(b):
		return 0
	case b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n':
		return 2
	case b[i] == '\r' || b[i] == '\n':
		return 1
	}
	return 0
}

// sseBoundaryEnds lists the end offsets of non overlapping blank line event boundaries scanned left to right
func sseBoundaryEnds(b []byte) []int {
	var ends []int
	for i := 0; i < len(b); {
		first := sseTerminator(b, i)
		second := 0
		if first > 0 {
			second = sseTerminator(b, i+first)
		}
		if first == 0 || second == 0 {
			i++
			continue
		}
		i += first + second
		ends = append(ends, i)
	}
	return ends
}

// bodyCapTransport caps every response body, rejecting a declared oversize up front and charging SSE per event
type bodyCapTransport struct {
	inner http.RoundTripper
	limit int
}

func (b *bodyCapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := b.inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if values, ok := resp.Header["Content-Length"]; ok && len(values) > 0 && declaredOverCap(values[0], b.limit) {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("MCP HTTP response declares Content-Length %s > %d bytes cap (from %s): %w",
			values[0], b.limit, req.URL.String(), ErrBodyTooLarge)
	}
	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	resp.Body = &cappedBody{inner: resp.Body, isSSE: isSSE, url: req.URL.String(), limit: b.limit}
	return resp, nil
}

// declaredOverCap reports a Content-Length that Python int parses to a value above limit
func declaredOverCap(declared string, limit int) bool {
	s := strings.TrimSpace(declared)
	if !underscoresValid(s) {
		return false
	}
	n, ok := new(big.Int).SetString(strings.ReplaceAll(s, "_", ""), 10)
	return ok && n.Cmp(big.NewInt(int64(limit))) > 0
}

// cappedBody fails a read once a finite body or a single SSE event passes the byte cap
type cappedBody struct {
	inner   io.ReadCloser
	isSSE   bool
	url     string
	limit   int
	counted int
	tail    []byte
	failed  error
}

func (c *cappedBody) reject(kind string) error {
	c.failed = fmt.Errorf("MCP %s exceeds %d bytes (from %s): %w", kind, c.limit, c.url, ErrBodyTooLarge)
	return c.failed
}

func (c *cappedBody) Read(p []byte) (int, error) {
	if c.failed != nil {
		return 0, c.failed
	}
	n, err := c.inner.Read(p)
	if n == 0 {
		return 0, err
	}
	chunk := p[:n]
	if c.isSSE {
		window := append(append([]byte(nil), c.tail...), chunk...)
		pos := 0
		for _, end := range sseBoundaryEnds(window) {
			if end <= len(c.tail) {
				continue
			}
			if c.counted+end-max(pos, len(c.tail)) > c.limit {
				return 0, c.reject("SSE event")
			}
			c.counted, pos = 0, end
		}
		c.counted += len(window) - max(pos, len(c.tail))
		c.tail = append(c.tail[:0], window[max(0, len(window)-sseBoundaryCarry):]...)
	} else {
		c.counted += n
	}
	if c.counted > c.limit {
		if c.isSSE {
			return 0, c.reject("SSE event")
		}
		return 0, c.reject("HTTP response")
	}
	return n, err
}

func (c *cappedBody) Close() error { return c.inner.Close() }
