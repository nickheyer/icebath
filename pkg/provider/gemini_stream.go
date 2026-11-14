package provider

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Python json.dumps text with ensure_ascii off and sorted keys, false when a value has no JSON form
func pyJSONDumps(v any) (string, bool) {
	var b strings.Builder
	if !pyJSONWrite(&b, v) {
		return "", false
	}
	return b.String(), true
}

func pyJSONWrite(b *strings.Builder, v any) bool {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		pyJSONWriteString(b, t)
	case json.Number:
		b.WriteString(t.String())
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		switch {
		case math.IsNaN(t):
			b.WriteString("NaN")
		case math.IsInf(t, 1):
			b.WriteString("Infinity")
		case math.IsInf(t, -1):
			b.WriteString("-Infinity")
		default:
			b.WriteString(pyFloatRepr(t))
		}
	case []any:
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			if !pyJSONWrite(b, item) {
				return false
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			pyJSONWriteString(b, k)
			b.WriteString(": ")
			if !pyJSONWrite(b, t[k]) {
				return false
			}
		}
		b.WriteByte('}')
	default:
		return false
	}
	return true
}

// JSON string literal escaping only quotes, backslashes and control characters
func pyJSONWriteString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// OpenAI chat.completion.chunk around one Gemini stream delta
type geminiChunkInput struct {
	content       string
	toolCallDelta map[string]any
	finishReason  string
	reasoning     string
	textSignature string
}

// OpenAI chunk map for one translated stream piece
func makeGeminiStreamChunk(model string, in geminiChunkInput) map[string]any {
	delta := map[string]any{"role": "assistant", "content": nil, "tool_calls": nil, "reasoning": nil, "reasoning_content": nil, "extra_content": nil}
	if in.content != "" {
		delta["content"] = in.content
	}
	if d := in.toolCallDelta; d != nil {
		index, _ := anyInt(d["index"])
		extra, _ := d["extra_content"].(map[string]any)
		delta["tool_calls"] = []any{geminiToolCallMap(pyOrStr(d["name"]), pyOrStr(d["arguments"]), index, geminiNewCallID(d), extra)}
	}
	if in.reasoning != "" {
		delta["reasoning"], delta["reasoning_content"] = in.reasoning, in.reasoning
	}
	if extra := toolCallExtraFromPart(map[string]any{"thoughtSignature": in.textSignature}); extra != nil {
		delta["extra_content"] = extra
	}
	var finish any
	if in.finishReason != "" {
		finish = in.finishReason
	}
	return geminiEnvelope(model, "chat.completion.chunk", map[string]any{"index": 0, "delta": delta, "finish_reason": finish}, nil)
}

// One SSE line as a payload, done for the terminal frame, nil for anything else
func parseGeminiSSELine(line string) (map[string]any, bool) {
	line = strings.TrimRight(line, "\r")
	if !strings.HasPrefix(line, "data: ") {
		return nil, false
	}
	data := line[6:]
	if data == "[DONE]" {
		return nil, true
	}
	parsed, err := geminiJSONLoads(data)
	if err != nil {
		catalogLog.Debug("Non-JSON Gemini SSE line: " + geminiTruncateRunes(data, 200))
		return nil, false
	}
	m, _ := parsed.(map[string]any)
	return m, false
}

// First n runes of a string
func geminiTruncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Reads Gemini SSE payloads, stopping at the done frame and flushing an unterminated final line
type geminiSSEReader struct {
	r    *bufio.Reader
	done bool
}

func newGeminiSSEReader(body io.Reader) *geminiSSEReader {
	return &geminiSSEReader{r: bufio.NewReaderSize(body, 64*1024)}
}

// Next payload, io.EOF once the stream or the done frame ends it
func (g *geminiSSEReader) Next() (map[string]any, error) {
	for !g.done {
		line, err := g.r.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		atEOF := err == io.EOF
		if atEOF {
			g.done = true
			if line == "" {
				break
			}
		} else {
			line = strings.TrimSuffix(line, "\n")
		}
		payload, isDone := parseGeminiSSELine(line)
		if isDone {
			g.done = true
			break
		}
		if payload != nil {
			return payload, nil
		}
	}
	return nil, io.EOF
}

// One streamed tool call slot across events and hops
type geminiToolSlot struct {
	index         int
	id            string
	lastArguments string
}

// Insertion ordered tool call slots keyed by provider id or part identity
type geminiToolSlots struct {
	keys  []string
	slots map[string]*geminiToolSlot
}

func newGeminiToolSlots() *geminiToolSlots {
	return &geminiToolSlots{slots: map[string]*geminiToolSlot{}}
}

func (t *geminiToolSlots) set(key string, slot *geminiToolSlot) {
	if _, ok := t.slots[key]; !ok {
		t.keys = append(t.keys, key)
	}
	t.slots[key] = slot
}

// Slot key and existing slot for a streamed functionCall, a resend extends its own slot
func geminiToolCallSlot(fc, part map[string]any, partIndex int, argsStr string, slots *geminiToolSlots) (string, *geminiToolSlot) {
	if id := geminiProviderCallID(fc); id != "" {
		key, _ := pyJSONDumps(map[string]any{"provider_call_id": id})
		return key, slots.slots[key]
	}
	sig, _ := part["thoughtSignature"].(string)
	key, _ := pyJSONDumps(map[string]any{"part_index": partIndex, "name": fc["name"], "thought_signature": sig})
	slot := slots.slots[key]
	if slot == nil || strings.HasPrefix(argsStr, slot.lastArguments) {
		return key, slot
	}
	for _, other := range slots.keys {
		if strings.HasPrefix(other, key+"#") && strings.HasPrefix(argsStr, slots.slots[other].lastArguments) {
			return other, slots.slots[other]
		}
	}
	return fmt.Sprintf("%s#%d", key, len(slots.keys)), nil
}

// OpenAI chunks for one Gemini stream event, tool slots shared across the hops of one turn
func translateGeminiStreamEvent(event map[string]any, model string, slots *geminiToolSlots) []map[string]any {
	candidates, _ := event["candidates"].([]any)
	if len(candidates) == 0 {
		return nil
	}
	cand, _ := candidates[0].(map[string]any)
	if cand == nil {
		cand = map[string]any{}
	}
	content, _ := cand["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	var chunks []map[string]any
	for partIndex, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		text, isThought, _ := geminiPartText(part)
		if isThought {
			chunks = append(chunks, makeGeminiStreamChunk(model, geminiChunkInput{reasoning: text}))
			continue
		}
		if text != "" {
			chunks = append(chunks, makeGeminiStreamChunk(model, geminiChunkInput{content: text}))
		}
		if sig := geminiTextPartSignature(part); sig != "" {
			chunks = append(chunks, makeGeminiStreamChunk(model, geminiChunkInput{textSignature: sig}))
		}
		if fc := geminiPartFunctionCall(part); fc != nil {
			name := pyStr(fc["name"])
			argsStr := geminiDumpCallArgsWith(fc, true)
			key, slot := geminiToolCallSlot(fc, part, partIndex, argsStr, slots)
			if slot == nil {
				slot = &geminiToolSlot{index: len(slots.keys), id: geminiNewCallID(fc)}
				slots.set(key, slot)
			}
			last := slot.lastArguments
			slot.lastArguments = argsStr
			delta := map[string]any{"index": slot.index, "id": slot.id, "name": name, "arguments": strings.TrimPrefix(argsStr, last)}
			if extra := toolCallExtraFromPart(part); extra != nil {
				delta["extra_content"] = extra
			}
			chunks = append(chunks, makeGeminiStreamChunk(model, geminiChunkInput{toolCallDelta: delta}))
		}
	}
	if raw := pyOrStr(cand["finishReason"]); raw != "" {
		finish := "stop"
		if len(slots.keys) > 0 {
			finish = "tool_calls"
		} else if mapped, ok := geminiFinishReasonMap[strings.ToUpper(raw)]; ok {
			finish = mapped
		}
		chunk := makeGeminiStreamChunk(model, geminiChunkInput{finishReason: finish})
		if meta, ok := event["usageMetadata"].(map[string]any); ok && len(meta) > 0 {
			chunk["usage"] = geminiUsageFromMetadata(meta)
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}
