package provider

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// DoneSSEFrame is the terminal sentinel strict OpenAI clients expect at the end of a stream
var DoneSSEFrame = []byte("data: [DONE]\n\n")

const sseByteSpace = " \t\n\r\x0b\x0c"

// SseDoneTracker is an incremental scanner over forwarded SSE chunks deciding whether one [DONE] gets appended
type SseDoneTracker struct {
	SawDone           bool
	SawTerminalFinish bool
	SawLastOne        bool
	SawErrorEvent     bool
	SawMalformedEvent bool
	Interrupted       bool
	buf               []byte
	dataLines         [][]byte
}

// Feed observes a forwarded chunk without modifying its bytes
func (t *SseDoneTracker) Feed(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	t.buf = append(t.buf, chunk...)
	for {
		nl := bytes.IndexByte(t.buf, '\n')
		if nl < 0 {
			return
		}
		line := append([]byte(nil), t.buf[:nl]...)
		t.buf = t.buf[nl+1:]
		t.consumeLine(line)
	}
}

// MarkInterrupted records that the upstream stream ended through an error or cancel
func (t *SseDoneTracker) MarkInterrupted() { t.Interrupted = true }

func (t *SseDoneTracker) blocked() bool {
	return t.SawDone || t.SawErrorEvent || t.SawMalformedEvent
}

// ShouldAppendDone is true when a single terminal [DONE] belongs after a clean EOF
func (t *SseDoneTracker) ShouldAppendDone() bool {
	if t.Interrupted || t.blocked() {
		return false
	}
	if len(t.buf) > 0 {
		t.consumeLine(append([]byte(nil), t.buf...))
		t.buf = nil
	}
	t.dispatchEvent()
	if t.blocked() {
		return false
	}
	return t.SawTerminalFinish || t.SawLastOne
}

// Collects data lines of the current event, a blank line dispatching it
func (t *SseDoneTracker) consumeLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) == 0 {
		t.dispatchEvent()
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	t.dataLines = append(t.dataLines, bytes.Trim(line[5:], sseByteSpace))
}

// Parses the joined data lines of one event and records what it says about the stream's end
func (t *SseDoneTracker) dispatchEvent() {
	if len(t.dataLines) == 0 {
		return
	}
	payload := bytes.Trim(bytes.Join(t.dataLines, []byte("\n")), sseByteSpace)
	t.dataLines = nil
	if string(payload) == "[DONE]" {
		t.SawDone = true
		return
	}
	if len(payload) == 0 {
		return
	}
	if !utf8.Valid(payload) {
		t.SawMalformedEvent = true
		return
	}
	parsed, err := pyjson.Loads(string(payload))
	if err != nil {
		t.SawMalformedEvent = true
		return
	}
	event, ok := parsed.(*pyjson.Object)
	if !ok {
		return
	}
	if v, present := event.Get("error"); present && v != nil {
		t.SawErrorEvent = true
		return
	}
	if lastOne, _ := event.Get("lastOne"); sseLastOne(lastOne) {
		t.SawLastOne = true
	}
	rawChoices, _ := event.Get("choices")
	choices, _ := rawChoices.([]any)
	for _, c := range choices {
		choice, isDict := c.(*pyjson.Object)
		if !isDict {
			continue
		}
		fr, present := choice.Get("finish_reason")
		if present && fr != nil {
			t.SawTerminalFinish = true
		}
		if s, isStr := fr.(string); isStr {
			if lower := strings.ToLower(s); lower == "error" || lower == "provider_error" {
				t.SawErrorEvent = true
			}
		}
	}
}

// True for the lastOne spellings relabelled upstreams send, true, 1 or "true"
func sseLastOne(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x == "true"
	}
	return pyrepr.Equal(v, 1)
}

// True when response headers advertise an SSE body
func contentTypeIsSSE(headers map[string][]string) bool {
	value := ""
	for name, values := range headers {
		if strings.EqualFold(name, "Content-Type") && len(values) > 0 && values[0] != "" {
			value = values[0]
			break
		}
	}
	return strings.Contains(strings.ToLower(value), "text/event-stream")
}
