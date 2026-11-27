package transport

import (
	"bufio"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// One block of a server sent event stream
type Frame struct {
	Event string
	Data  string
	ID    string
	Retry string
	// Comment lines, and the other fields by name
	Comments []string
	Fields   map[string]string
	// HTTP status a comment or status field of the frame names, 0 for none
	Status int
}

// Whether the frame carries nothing at all, such as the blank line of a keepalive
func (f *Frame) Empty() bool {
	return f.Event == "" && f.ID == "" && f.Retry == "" && len(f.Fields) == 0 && f.Data == ""
}

// Reads the frames of a server sent event stream one at a time
type Events struct {
	reader *bufio.Reader
	done   bool
}

func NewEvents(body io.Reader) *Events {
	return &Events{reader: bufio.NewReaderSize(body, 64*1024)}
}

// The next frame, io.EOF once the stream ends
func (e *Events) Next() (*Frame, error) {
	if e.done {
		return nil, io.EOF
	}
	frame := &Frame{Fields: map[string]string{}}
	var data []string
	seen := false
	finish := func() *Frame {
		frame.Data = strings.Join(data, "\n")
		frame.Status = frame.status()
		return frame
	}
	for {
		line, err := e.reader.ReadString('\n')
		if err != nil && line == "" {
			e.done = true
			if seen {
				return finish(), nil
			}
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if seen {
				return finish(), nil
			}
			if err != nil {
				e.done = true
				return nil, io.EOF
			}
			continue
		}
		seen = true
		if comment, ok := strings.CutPrefix(line, ":"); ok {
			frame.Comments = append(frame.Comments, strings.TrimSpace(comment))
			continue
		}
		field, value, separated := strings.Cut(line, ":")
		field = strings.ToLower(strings.TrimSpace(field))
		value = strings.TrimPrefix(value, " ")
		switch {
		case !separated:
			frame.Fields[field] = ""
		case field == "event":
			frame.Event = strings.TrimSpace(value)
		case field == "data":
			data = append(data, value)
		case field == "id":
			frame.ID = value
		case field == "retry":
			frame.Retry = value
		default:
			frame.Fields[field] = value
		}
		if err != nil {
			e.done = true
			return finish(), nil
		}
	}
}

// Frames of text that looks like a server sent event stream
func ParseEvents(text string) []*Frame {
	var frames []*Frame
	events := NewEvents(strings.NewReader(text))
	for {
		frame, err := events.Next()
		if err != nil {
			return frames
		}
		frames = append(frames, frame)
	}
}

var statusCode = regexp.MustCompile(`(?i)(?:HTTP_STATUS/)?\b([1-5]\d\d)\b`)

// HTTP status a comment or status field of the frame names
func (f *Frame) status() int {
	candidates := append([]string{}, f.Comments...)
	for _, key := range []string{"status", "status_code", "http_status"} {
		if value, ok := f.Fields[key]; ok {
			candidates = append(candidates, value)
		}
	}
	for _, candidate := range candidates {
		if code := StatusOf(candidate); code != 0 {
			return code
		}
	}
	return 0
}

// HTTP status a number or a text such as HTTP_STATUS/429 names, 0 for none
func StatusOf(value any) int {
	switch v := value.(type) {
	case int:
		if v >= 100 && v < 600 {
			return v
		}
	case float64:
		if v == float64(int(v)) && v >= 100 && v < 600 {
			return int(v)
		}
	case string:
		if m := statusCode.FindStringSubmatch(v); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

// Field names of a server sent event stream
var eventFields = []string{"event", "data", "id", "retry"}

// Whether streamed text may still be the start of an event stream's control block rather than words of the reply, as a relay that
// forwards an upstream error as content sends
func MaybeEvents(text string) bool {
	stripped := strings.TrimLeft(text, " \t\r\n")
	if stripped == "" {
		return false
	}
	lines := strings.Split(stripped, "\n")
	ended := strings.HasSuffix(stripped, "\n") || strings.HasSuffix(stripped, "\r")
	field := false
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, ":"):
			field = true
			continue
		}
		name, _, separated := strings.Cut(line, ":")
		name = strings.ToLower(strings.TrimSpace(name))
		if separated && slices.Contains(eventFields, name) {
			field = true
			continue
		}
		if i == len(lines)-1 && !ended {
			for _, known := range eventFields {
				if strings.HasPrefix(known, name) {
					return true
				}
			}
		}
		return false
	}
	return field
}
