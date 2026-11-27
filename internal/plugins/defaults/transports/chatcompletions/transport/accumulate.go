package transport

import (
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// One tool call put together from the deltas of a stream
type entry struct {
	ID        string
	Name      string
	Arguments string
	// Gemini's thought signature of the call's function, and the call's extra_content
	Signature string
	Extra     any
}

// Where the brackets of a call's arguments stand, read so far
type scan struct {
	done             int
	depth            int
	inString, escape bool
	started          bool
}

// Puts streamed tool call deltas together by slot; a new id at an index a finished call holds opens a new slot
type accumulator struct {
	calls    map[int]*entry
	notified map[int]bool
	lastID   map[int]string
	slot     map[int]int
	parts    map[int][]string
	scans    map[int]scan
}

func newAccumulator() *accumulator {
	return &accumulator{calls: map[int]*entry{}, notified: map[int]bool{}, lastID: map[int]string{}, slot: map[int]int{}, parts: map[int][]string{},
		scans: map[int]scan{}}
}

func (a *accumulator) Len() int {
	return len(a.calls)
}

// Slots in order
func (a *accumulator) Slots() []int {
	return slices.Sorted(maps.Keys(a.calls))
}

// Calls with their argument fragments joined
func (a *accumulator) Calls() map[int]*entry {
	for slot, parts := range a.parts {
		a.calls[slot].Arguments = strings.Join(parts, "")
	}
	return a.calls
}

// Whether a named delta at a reused index opens a new call rather than going on with the one there
func (a *accumulator) starts(name string, slot int) bool {
	if name == "" {
		return false
	}
	if current, ok := a.calls[slot]; !ok || current.Name != name {
		return true
	}
	return a.closed(slot)
}

// Whether a slot's arguments are whole: brackets balanced outside strings
func (a *accumulator) closed(slot int) bool {
	parts := a.parts[slot]
	s := a.scans[slot]
	for _, ch := range []byte(strings.Join(parts[s.done:], "")) {
		switch {
		case s.inString && s.escape:
			s.escape = false
		case s.inString && ch == '\\':
			s.escape = true
		case s.inString && ch == '"':
			s.inString = false
		case s.inString:
		case ch == '"':
			s.inString = true
		case ch == '{' || ch == '[':
			s.depth++
			s.started = true
		case ch == '}' || ch == ']':
			s.depth--
		}
	}
	s.done = len(parts)
	a.scans[slot] = s
	return s.started && s.depth == 0 && !s.inString
}

// Id of a delta, a numeric one as its digits
func deltaID(v any) string {
	switch id := v.(type) {
	case string:
		return id
	case float64:
		if id == math.Trunc(id) {
			return strconv.FormatInt(int64(id), 10)
		}
		return strconv.FormatFloat(id, 'f', -1, 64)
	}
	return ""
}

// Merges one delta, giving the call's name the first time it is known, and its slot
func (a *accumulator) Feed(delta map[string]any) (string, int) {
	index := 0
	if n, ok := delta["index"].(float64); ok {
		index = int(n)
	}
	id := deltaID(delta["id"])
	function, _ := delta["function"].(map[string]any)
	name, _ := function["name"].(string)
	if _, ok := a.slot[index]; !ok {
		a.slot[index] = index
	}
	if last, seen := a.lastID[index]; id != "" && seen && id != last && a.starts(name, a.slot[index]) {
		next := -1
		for slot := range a.calls {
			next = max(next, slot)
		}
		a.slot[index] = next + 1
	}
	if id != "" {
		a.lastID[index] = id
	}
	slot := a.slot[index]
	call, ok := a.calls[slot]
	if !ok {
		call = &entry{ID: id}
		a.calls[slot] = call
	}
	if id != "" {
		call.ID = id
	}
	if function != nil {
		if name != "" {
			call.Name = name
		}
		if arguments, ok := function["arguments"].(string); ok && arguments != "" {
			a.parts[slot] = append(a.parts[slot], arguments)
		}
		if signature, ok := function["thought_signature"].(string); ok && signature != "" {
			call.Signature = signature
		}
	}
	if extra, ok := delta["extra_content"]; ok && extra != nil {
		call.Extra = extra
	}
	if call.Name != "" && !a.notified[slot] {
		a.notified[slot] = true
		return call.Name, slot
	}
	return "", slot
}

// Detail types whose text streams in fragments, and the field holding it
var mergeable = map[string]string{"reasoning.text": "text", "reasoning.summary": "summary"}

// Fields a merged detail takes from a later fragment when it lacks them
var backfilled = []string{"signature", "id", "format", "index"}

// Readable text of one reasoning detail, never opaque replay material
func detailText(detail any) string {
	object, ok := detail.(map[string]any)
	if !ok {
		return ""
	}
	kind, _ := object["type"].(string)
	key, ok := mergeable[kind]
	if !ok {
		return ""
	}
	text, _ := object[key].(string)
	return text
}

func empty(v any) bool {
	s, ok := v.(string)
	return v == nil || ok && s == ""
}

// Details with one streamed entry added, a text or summary fragment merged into the entry of its type before it
func appendDetail(details []map[string]any, detail any) []map[string]any {
	object, ok := detail.(map[string]any)
	if !ok {
		return details
	}
	object = maps.Clone(object)
	kind, _ := object["type"].(string)
	key, merges := mergeable[kind]
	if n := len(details); n > 0 && merges {
		last := details[n-1]
		if fragment, ok := object[key].(string); ok && last["type"] == kind {
			previous, _ := last[key].(string)
			last[key] = previous + fragment
			for _, field := range backfilled {
				if empty(last[field]) && !empty(object[field]) {
					last[field] = object[field]
				}
			}
			return details
		}
	}
	return append(details, object)
}

// Delta with a paragraph break in front when it opens a bold heading right after text that didn't end its line
func separated(previous, delta string) string {
	if previous == "" || delta == "" {
		return delta
	}
	runes := []rune(previous)
	if unicode.IsSpace(runes[len(runes)-1]) {
		return delta
	}
	if strings.HasPrefix(delta, "**") && strings.Contains(delta[2:], "**") {
		return "\n\n" + delta
	}
	return delta
}
