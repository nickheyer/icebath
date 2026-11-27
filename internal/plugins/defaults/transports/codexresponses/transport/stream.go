package transport

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Events that carry the model's progress rather than the stream's liveness
var progressDeltas = []string{"response.output_text.delta", "response.reasoning_summary_text.delta", "response.text.delta", "response.audio.delta",
	"response.function_call_arguments.delta", "response.reasoning_text.delta", "response.refusal.delta"}

// Whether an event carries progress of the model: a delta with something in it, or a function call item that says what it calls
func progress(event map[string]any) bool {
	kind, _ := event["type"].(string)
	if slices.Contains(progressDeltas, kind) {
		return event["delta"] != nil && event["delta"] != ""
	}
	if kind == "response.output_item.added" {
		item, _ := event["item"].(map[string]any)
		if t, _ := item["type"].(string); strings.Contains(t, "function_call") {
			for _, field := range []string{"id", "call_id", "name", "arguments"} {
				if v := item[field]; v != nil && v != "" {
					return true
				}
			}
		}
	}
	return false
}

// Error a stream sent as an event of its own
type eventError struct {
	message, code, param string
}

func (e *eventError) Error() string { return e.message }

// Body of the error as the classifier reads one
func (e *eventError) body() map[string]any {
	return map[string]any{"error": map[string]any{"message": e.message, "code": e.code, "param": e.param, "type": "error"}}
}

// Error of an error event, its top fields first and then its error object's
func streamError(event map[string]any) *eventError {
	nested, _ := event["error"].(map[string]any)
	field := func(name string) string {
		v := event[name]
		if v == nil {
			v = nested[name]
		}
		if v == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	message := field("message")
	message = cmp.Or(message, "stream emitted error event")
	return &eventError{message: message, code: field("code"), param: field("param")}
}

func phase(item map[string]any) string {
	s, _ := item["phase"].(string)
	return strings.ToLower(strings.TrimSpace(s))
}

// Function call announced and not yet done, its arguments as they streamed
type pending struct {
	item      map[string]any
	arguments string
	index     any
	sequence  int
}

// Responses object put together from the events of a stream, its items in the order they were announced
type assembler struct {
	model       string
	text        func(string) error
	reasoning   func(string) error
	toolStart   func(string) error
	calling     bool
	terminal    bool
	completed   bool
	answerAdded bool
	sequence    int
	activePhase string
	summary     any
	state       string
	usage       map[string]any
	id          string
	incomplete  any
	failure     any
	tier        any
	items       []map[string]any
	indexes     []any
	sequences   []int
	deltas      []string
	pending     map[string]*pending
	order       []string
	announced   map[string][2]any
	phases      map[string]string
}

func newAssembler(model string) *assembler {
	return &assembler{model: model, state: "completed", pending: map[string]*pending{}, announced: map[string][2]any{}, phases: map[string]string{}}
}

func number(v any) (float64, bool) {
	n, ok := v.(float64)
	return n, ok
}

func sameIndex(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	fa, okA := number(a)
	fb, okB := number(b)
	if okA && okB {
		return fa == fb
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func (a *assembler) added(event map[string]any) error {
	item, _ := event["item"].(map[string]any)
	kind, _ := item["type"].(string)
	a.activePhase = ""
	if kind == "message" {
		a.activePhase = phase(item)
		if a.activePhase != "commentary" && a.activePhase != "analysis" {
			a.answerAdded = true
		}
	}
	id, _ := item["id"].(string)
	if id != "" && kind == "message" {
		a.phases[id] = a.activePhase
	}
	if _, seen := a.announced[id]; id != "" && !seen {
		a.announced[id] = [2]any{a.sequence, event["output_index"]}
		a.sequence++
	}
	if !strings.Contains(kind, "function_call") {
		return nil
	}
	a.calling = true
	var err error
	if name, _ := item["name"].(string); name != "" && a.toolStart != nil {
		err = a.toolStart(name)
	}
	if id != "" {
		order := a.announced[id]
		if _, exists := a.pending[id]; !exists {
			a.order = append(a.order, id)
		}
		arguments, _ := item["arguments"].(string)
		a.pending[id] = &pending{item: item, arguments: arguments, index: order[1], sequence: order[0].(int)}
	}
	return err
}

func (a *assembler) phaseOf(event map[string]any) string {
	if id, _ := event["item_id"].(string); id != "" {
		if p, ok := a.phases[id]; ok {
			return p
		}
	}
	return a.activePhase
}

func (a *assembler) textDelta(event map[string]any) error {
	delta, _ := event["delta"].(string)
	if delta == "" {
		return nil
	}
	switch a.phaseOf(event) {
	case "commentary", "analysis":
		if a.reasoning != nil {
			return a.reasoning(delta)
		}
		return nil
	}
	a.deltas = append(a.deltas, delta)
	if a.calling || a.text == nil {
		return nil
	}
	return a.text(delta)
}

// Pending call by its item id, else by its output index
func (a *assembler) key(id string, index any) (string, bool) {
	if _, ok := a.pending[id]; ok {
		return id, true
	}
	if index != nil {
		for _, key := range a.order {
			if sameIndex(a.pending[key].index, index) {
				return key, true
			}
		}
	}
	return "", false
}

func (a *assembler) call(event map[string]any, kind string) {
	a.calling = true
	id, _ := event["item_id"].(string)
	key, ok := a.key(id, event["output_index"])
	if !ok {
		return
	}
	p := a.pending[key]
	if strings.Contains(kind, "delta") {
		delta, _ := event["delta"].(string)
		p.arguments += delta
	} else if strings.HasSuffix(kind, "function_call_arguments.done") {
		if done, ok := event["arguments"].(string); ok {
			p.arguments = done
		}
	}
}

func (a *assembler) reasoningDelta(event map[string]any) error {
	text, _ := event["delta"].(string)
	if text == "" || a.reasoning == nil {
		return nil
	}
	if index, ok := event["summary_index"]; ok && index != nil {
		if a.summary != nil && !sameIndex(index, a.summary) {
			text = "\n\n" + text
		}
		a.summary = index
	}
	return a.reasoning(text)
}

func (a *assembler) remove(key string) {
	delete(a.pending, key)
	a.order = slices.DeleteFunc(a.order, func(k string) bool { return k == key })
}

func (a *assembler) done(event map[string]any) {
	item, _ := event["item"].(map[string]any)
	if item == nil {
		return
	}
	a.items = append(a.items, item)
	id, _ := item["id"].(string)
	var sequence, index any
	if order, ok := a.announced[id]; ok {
		sequence, index = order[0], order[1]
	}
	var keys []string
	if kind, _ := item["type"].(string); strings.Contains(kind, "function_call") {
		if call, _ := item["call_id"].(string); strings.TrimSpace(call) != "" {
			for _, key := range a.order {
				if a.pending[key].item["call_id"] == call {
					keys = append(keys, key)
				}
			}
			if len(keys) == 0 {
				if key, ok := a.key(id, event["output_index"]); ok && a.pending[key].item["call_id"] == nil {
					keys = []string{key}
				}
			}
			sequence, index = nil, nil
		} else if key, ok := a.key(id, event["output_index"]); ok {
			keys = []string{key}
		}
		if len(keys) > 0 {
			first := a.pending[keys[0]]
			for _, key := range keys[1:] {
				if p := a.pending[key]; p.sequence < first.sequence {
					first = p
				}
			}
			sequence, index = first.sequence, first.index
		}
	}
	if sequence == nil {
		sequence = a.sequence
		a.sequence++
	}
	if v, ok := event["output_index"]; ok && v != nil {
		index = v
	}
	a.indexes = append(a.indexes, index)
	a.sequences = append(a.sequences, sequence.(int))
	for _, key := range keys {
		a.remove(key)
	}
}

func (a *assembler) end(event map[string]any, kind string) {
	a.terminal = true
	if response, ok := event["response"].(map[string]any); ok {
		a.usage, _ = response["usage"].(map[string]any)
		a.id, _ = response["id"].(string)
		a.tier = response["service_tier"]
		if s, ok := response["status"].(string); ok {
			a.state = s
		}
		switch kind {
		case "response.incomplete":
			a.incomplete = response["incomplete_details"]
		case "response.failed":
			a.failure = response["error"]
		}
	}
	a.completed = a.completed || kind == "response.completed"
	if a.state == "" {
		a.state = strings.TrimPrefix(kind, "response.")
	}
}

// Folds one event in, true at a terminal event; an error for an error event or a consumer that stopped
func (a *assembler) feed(event map[string]any) (bool, error) {
	kind, _ := event["type"].(string)
	switch kind {
	case "error":
		return false, streamError(event)
	case "response.output_item.added":
		return false, a.added(event)
	case "response.output_item.done":
		a.done(event)
		return false, nil
	case "response.completed", "response.incomplete", "response.failed":
		a.end(event, kind)
		return true, nil
	case "response.refusal.delta":
		if refusal, _ := event["delta"].(string); refusal != "" {
			a.deltas = append(a.deltas, refusal)
		}
		return false, nil
	}
	switch {
	case strings.Contains(kind, "output_text.delta"):
		return false, a.textDelta(event)
	case strings.Contains(kind, "function_call"):
		a.call(event, kind)
	case strings.Contains(kind, "reasoning") && strings.Contains(kind, "delta"):
		return false, a.reasoningDelta(event)
	}
	return false, nil
}

// Done items and the calls still pending, in the order the stream put them
func (a *assembler) settled() []map[string]any {
	type entry struct {
		index    any
		sequence int
		item     map[string]any
	}
	var entries []entry
	for i, item := range a.items {
		entries = append(entries, entry{a.indexes[i], a.sequences[i], item})
	}
	for _, key := range a.order {
		p := a.pending[key]
		arguments := strings.TrimSpace(p.arguments)
		arguments = cmp.Or(arguments, "{}")
		entries = append(entries, entry{p.index, p.sequence, map[string]any{"type": "function_call", "id": p.item["id"], "call_id": p.item["call_id"],
			"name": p.item["name"], "status": "completed", "arguments": arguments}})
	}
	indexed, comparable := true, true
	for _, e := range entries {
		if e.index == nil {
			indexed = false
			break
		}
		if _, ok := number(e.index); !ok {
			comparable = false
		}
	}
	switch {
	case indexed && comparable:
		sort.SliceStable(entries, func(i, j int) bool {
			a, _ := number(entries[i].index)
			b, _ := number(entries[j].index)
			return a < b
		})
	case !indexed:
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].sequence < entries[j].sequence })
	}
	out := make([]map[string]any, len(entries))
	for i, e := range entries {
		out[i] = e.item
	}
	return out
}

var errNoTerminal = errors.New("Codex Responses stream did not emit a terminal response")

// Responses object the events made, an error when neither a terminal event nor any output came
func (a *assembler) result() (map[string]any, error) {
	output := slices.Clone(a.items)
	if a.completed {
		output = a.settled()
	}
	answered := slices.ContainsFunc(output, func(item map[string]any) bool {
		p := phase(item)
		return item["type"] == "message" && p != "commentary" && p != "analysis"
	})
	if len(a.deltas) > 0 && (len(output) == 0 && !a.calling || a.terminal && a.answerAdded && !answered) {
		message := map[string]any{"type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": strings.Join(a.deltas, "")}}}
		first := slices.IndexFunc(output, func(item map[string]any) bool {
			t, _ := item["type"].(string)
			return strings.Contains(t, "function_call")
		})
		if first < 0 {
			first = len(output)
		}
		output = slices.Insert(output, first, message)
	}
	if !a.terminal && len(output) == 0 {
		return nil, errNoTerminal
	}
	list := make([]any, len(output))
	for i, item := range output {
		list[i] = item
	}
	response := map[string]any{"output": list, "output_text": strings.Join(a.deltas, ""), "status": a.state, "id": a.id, "model": a.model,
		"incomplete_details": a.incomplete, "error": a.failure, "service_tier": a.tier}
	if a.usage != nil {
		response["usage"] = a.usage
	}
	return response, nil
}
