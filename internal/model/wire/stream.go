package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// Largest stream line or event the readers accept when a caller names no bound of its own
const DefaultFrameLimit = 64 << 20

// Returned when a stream line or event outgrows the bound its reader was given
type FrameSizeError struct {
	Limit int
}

func (e *FrameSizeError) Error() string {
	return fmt.Sprintf("provider stream frame exceeds %d bytes", e.Limit)
}

// Outcome of one read from the underlying stream
type readResult struct {
	n   int
	err error
}

// Splits a byte stream into lines under a size bound, giving up a blocked read when the context ends
type lineReader struct {
	src     io.Reader
	limit   int
	crEnds  bool
	buf     []byte
	start   int
	end     int
	scanned int
	skipLF  bool
	eof     bool
	failure error
	idle    int
	res     chan readResult
}

// Starts a line reader, a limit below one meaning the default bound
func newLineReader(src io.Reader, limit int, crEnds bool) lineReader {
	if limit < 1 {
		limit = DefaultFrameLimit
	}
	return lineReader{src: src, limit: limit, crEnds: crEnds, res: make(chan readResult, 1)}
}

// Next line without its terminator, valid until the following call, io.EOF after the last one
func (r *lineReader) next(ctx context.Context) ([]byte, error) {
	for {
		if r.skipLF && r.start < r.end {
			if r.buf[r.start] == '\n' {
				r.start++
			}
			r.skipLF = false
		}
		if !r.skipLF {
			for i := r.start + r.scanned; i < r.end; i++ {
				c := r.buf[i]
				if c != '\n' && (c != '\r' || !r.crEnds) {
					continue
				}
				line := r.buf[r.start:i]
				r.start, r.scanned, r.skipLF = i+1, 0, c == '\r'
				if len(line) > r.limit {
					r.failure = &FrameSizeError{r.limit}
					return nil, r.failure
				}
				return line, nil
			}
			r.scanned = r.end - r.start
			if r.scanned > r.limit {
				r.failure = &FrameSizeError{r.limit}
			}
		}
		if r.failure != nil {
			return nil, r.failure
		}
		if r.eof {
			line := r.buf[r.start:r.end]
			r.start, r.scanned, r.skipLF = r.end, 0, false
			if len(line) == 0 {
				return nil, io.EOF
			}
			return line, nil
		}
		r.fill(ctx)
	}
}

// Reads more bytes behind the unread ones, recording the end of the stream or the failure that stopped it
func (r *lineReader) fill(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		r.failure = err
		return
	}
	if r.start > 0 {
		r.end = copy(r.buf, r.buf[r.start:r.end])
		r.start = 0
	}
	if r.end == len(r.buf) {
		grown := make([]byte, min(max(32<<10, 2*len(r.buf)), r.limit+32<<10))
		copy(grown, r.buf[:r.end])
		r.buf = grown
	}
	dst := r.buf[r.end:]
	var res readResult
	if ctx.Done() == nil {
		res.n, res.err = r.src.Read(dst)
	} else {
		go func() {
			n, err := r.src.Read(dst)
			r.res <- readResult{n, err}
		}()
		select {
		case res = <-r.res:
		case <-ctx.Done():
			r.failure, r.buf, r.start, r.end = ctx.Err(), nil, 0, 0
			return
		}
	}
	r.end += res.n
	switch {
	case res.err == io.EOF:
		r.eof = true
	case res.err != nil:
		r.failure = res.err
	case res.n == 0:
		if r.idle++; r.idle > 100 {
			r.failure = io.ErrNoProgress
		}
	default:
		r.idle = 0
	}
}

// One dispatched event of a server-sent event stream, ID being the last event id the stream named
type SSEvent struct {
	Event string
	Data  []byte
	ID    string
}

// Reports whether the event is the [DONE] terminator
func (e SSEvent) Done() bool {
	return bytes.HasPrefix(e.Data, []byte("[DONE]"))
}

// JSON payload of the event, a StreamError naming a frame that is empty or not JSON
func (e SSEvent) Payload() (json.RawMessage, error) {
	text := bytes.TrimFunc(e.Data, isSpace)
	if len(text) == 0 {
		return nil, NewStreamError(StreamEmptyFrame, "")
	}
	if !json.Valid(e.Data) {
		return nil, NewStreamError(StreamNonJSON, string(text))
	}
	return trimJSON(e.Data), nil
}

// Reads the events of a server-sent event stream with bounded lines and events, OnComment seeing each comment line
type EventReader struct {
	OnComment func(text string)

	lines   lineReader
	event   string
	data    []byte
	id      string
	began   bool
	hasData bool
	fields  bool
}

// Starts reading server-sent events from r, a limit below one meaning the default bound
func NewEventReader(r io.Reader, limit int) *EventReader {
	return &EventReader{lines: newLineReader(r, limit, true)}
}

// Following event, a final one needing no closing blank line, io.EOF once the stream ends
func (r *EventReader) Next(ctx context.Context) (SSEvent, error) {
	for {
		line, err := r.lines.next(ctx)
		if err == io.EOF && r.fields {
			return r.take(), nil
		}
		if err != nil {
			return SSEvent{}, err
		}
		if !r.began {
			r.began = true
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
		}
		if len(line) == 0 {
			if r.fields {
				return r.take(), nil
			}
			continue
		}
		if line[0] == ':' {
			if r.OnComment != nil {
				r.OnComment(trimSpace(string(line[1:])))
			}
			continue
		}
		name, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			name, value = line[:i], line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		}
		switch string(name) {
		case "event":
			r.event = string(value)
			r.fields = r.fields || len(value) > 0
		case "data":
			if r.hasData {
				r.data = append(r.data, '\n')
			}
			r.data = append(r.data, value...)
			r.hasData, r.fields = true, true
			if len(r.data) > r.lines.limit {
				r.lines.failure = &FrameSizeError{r.lines.limit}
				return SSEvent{}, r.lines.failure
			}
		case "id":
			if bytes.IndexByte(value, 0) < 0 {
				r.id = string(value)
				r.fields = r.fields || len(value) > 0
			}
		case "retry":
			if _, ok := parseInteger(string(value)); ok {
				r.fields = true
			}
		}
	}
}

// Hands out the pending event and starts a new one, the last event id carrying over
func (r *EventReader) take() SSEvent {
	ev := SSEvent{Event: r.event, Data: r.data, ID: r.id}
	r.event, r.data, r.hasData, r.fields = "", nil, false, false
	return ev
}

// Reads newline-delimited JSON values with bounded lines
type JSONLineReader struct {
	lines lineReader
}

// Starts reading one JSON value per line from r, a limit below one meaning the default bound
func NewJSONLineReader(r io.Reader, limit int) *JSONLineReader {
	return &JSONLineReader{lines: newLineReader(r, limit, false)}
}

// Following non-blank line as JSON, a StreamError for a line that is not JSON, io.EOF once the stream ends
func (r *JSONLineReader) Next(ctx context.Context) (json.RawMessage, error) {
	for {
		line, err := r.lines.next(ctx)
		if err != nil {
			return nil, err
		}
		line = bytes.Trim(line, " \t\n\r\v\f")
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return nil, NewStreamError(StreamNonJSON, string(line))
		}
		return bytes.Clone(line), nil
	}
}
