package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	r     *bufio.Reader
	limit int
	src   string
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

// readLine returns one line without its CR, LF, or CRLF terminator
func (s *SSEReader) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := s.r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == bufio.ErrBufferFull {
			if len(buf) > s.limit {
				return nil, fmt.Errorf("MCP SSE line exceeds %d bytes (from %s): %w", s.limit, s.src, ErrBodyTooLarge)
			}
			continue
		}
		if err != nil {
			if len(buf) == 0 {
				return nil, err
			}
			return trimEOL(buf), nil
		}
		return trimEOL(buf), nil
	}
}

// trimEOL strips a trailing LF or CRLF
func trimEOL(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\r"))
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
