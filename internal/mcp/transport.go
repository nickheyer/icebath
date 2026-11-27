package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/strs"
)

// Moves JSON-RPC messages between this side and the other
type Transport interface {
	// Delivers one message to the other side
	Send(ctx context.Context, msg *Message) error
	// Messages from the other side, closed when the transport ends
	Receive() <-chan *Message
	// Why the transport ended, nil while it runs or after a clean close
	Err() error
	// Releases the connection and every process it owns
	Close() error
	// Name of the transport in messages, such as stdio or HTTP
	Label() string
}

// Largest body, and largest single event of a stream, read before any JSON is parsed
const MaxBodyBytes = 10 << 20

var ErrBodyTooLarge = errors.New("the answer is too large to read")

// One server sent event
type SSEEvent struct {
	ID    string
	Event string
	Data  string
	Retry string
}

// Reads a text/event-stream body one event at a time, each under a byte cap
type SSEReader struct {
	r      *bufio.Reader
	limit  int
	source string
	skipLF bool
}

func NewSSEReader(body io.Reader, limit int, source string) *SSEReader {
	return &SSEReader{r: bufio.NewReaderSize(body, 64<<10), limit: limit, source: source}
}

// Next complete event: io.EOF at a clean end, ErrBodyTooLarge past the cap
func (s *SSEReader) Next() (*SSEEvent, error) {
	event := &SSEEvent{}
	var data []string
	counted, seen := 0, false
	for {
		line, err := s.line()
		if err != nil {
			if err == io.EOF && seen && len(data) > 0 {
				event.Data = strings.Join(data, "\n")
				return event, nil
			}
			return nil, err
		}
		if counted += len(line) + 1; counted > s.limit {
			return nil, fmt.Errorf("The server at %s sent an event larger than %s, and %w.", s.source, strs.Size(float64(s.limit)), ErrBodyTooLarge)
		}
		if len(line) == 0 {
			if !seen {
				continue
			}
			event.Data = strings.Join(data, "\n")
			return event, nil
		}
		seen = true
		if line[0] == ':' {
			continue
		}
		field, value := string(line), ""
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = string(line[:i]), strings.TrimPrefix(string(line[i+1:]), " ")
		}
		switch field {
		case "data":
			data = append(data, value)
		case "event":
			event.Event = value
		case "id":
			if !strings.ContainsRune(value, 0) {
				event.ID = value
			}
		case "retry":
			event.Retry = value
		}
	}
}

// One line ended by CR, LF or CRLF, without its end
func (s *SSEReader) line() ([]byte, error) {
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
				s.r.Discard(1)
				continue
			}
		}
		window, _ = s.r.Peek(s.r.Buffered())
		i := bytes.IndexAny(window, "\r\n")
		if i < 0 {
			buf = append(buf, window...)
			s.r.Discard(len(window))
			if len(buf) > s.limit {
				return nil, fmt.Errorf("The server at %s sent a line larger than %s, and %w.", s.source, strs.Size(float64(s.limit)), ErrBodyTooLarge)
			}
			continue
		}
		buf = append(buf, window[:i]...)
		s.skipLF = window[i] == '\r'
		s.r.Discard(i + 1)
		return buf, nil
	}
}

// Whole body, refused past the limit
func readCapped(body io.Reader, limit int, source string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("The server at %s sent an answer larger than %s, and %w.", source, strs.Size(float64(limit)), ErrBodyTooLarge)
	}
	return data, nil
}

// Refuses an answer whose declared length is past the cap before its body is read
type capped struct {
	inner http.RoundTripper
}

func (c capped) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if declared := resp.Header.Get("Content-Length"); declared != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(declared), 10, 64); err == nil && n > MaxBodyBytes {
			resp.Body.Close()
			return nil, fmt.Errorf("The server at %s says its answer has %s bytes, more than %s, and %w.", req.URL, declared, strs.Size(float64(MaxBodyBytes)),
				ErrBodyTooLarge)
		}
	}
	return resp, nil
}
