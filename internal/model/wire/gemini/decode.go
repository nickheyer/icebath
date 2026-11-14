package gemini

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Most requests one continuation token may ride, so a network that keeps dropping ends in an error
const maxTokenSends = 3

// One function call under assembly across the events and hops of a streamed turn
type callSlot struct {
	index     int
	id        string
	name      string
	arguments string
	extra     json.RawMessage
	notified  bool
}

// Reads the replies of one turn, across the hops of a long decode, with the request that started it
type decoder struct {
	p         Protocol
	req       *wire.Request
	model     string
	body      *wire.Dict
	maxHops   int
	hops      []wire.Value
	hopUsage  []wire.Value
	slots     map[string]*callSlot
	order     []string
	resume    string
	sends     int
	sink      wire.Sink
	content   []string
	reasoning []string
	pending   []string
	signature string
	finish    wire.FinishReason
	rawFinish string
	usage     *wire.Usage
}

// Random lowercase hex digits
func randomHex(digits int) string {
	b := make([]byte, (digits+1)/2)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", digits)
	}
	return hex.EncodeToString(b)[:digits]
}

// Finish reason a native finish word maps to, stop for anything unlisted
func mapFinish(word string) wire.FinishReason {
	switch strings.ToUpper(word) {
	case "MAX_TOKENS", "CONTINUATION":
		return wire.FinishLength
	case "SAFETY", "RECITATION":
		return wire.FinishContentFilter
	}
	return wire.FinishStop
}

// First choice of a reply, absent when it has none
func candidate(payload wire.Value) wire.Value {
	candidates := payload.Get("candidates").Elems()
	if len(candidates) == 0 || !candidates[0].IsObject() {
		return wire.Value{}
	}
	return candidates[0]
}

// Token to resend with when a hop ended on a continuation, empty for any other finish
func continuationToken(payload wire.Value) string {
	cand := candidate(payload)
	if strings.ToUpper(cand.Get("finishReason").Text()) != "CONTINUATION" {
		return ""
	}
	return cand.Get("continuationToken").Text()
}

// Whole number a usage member holds, zero for a missing or empty one
func count(meta wire.Value, key string) int {
	v := meta.Get(key)
	if n, ok := v.Int(); ok {
		return int(n)
	}
	n, _ := strconv.Atoi(strip(v.Text()))
	return n
}

// Usage metadata summed over hops, the first hop's prompt kept apart as the context size
func mergeHopUsage(hops []wire.Value) json.RawMessage {
	merged := &wire.Dict{}
	for _, key := range [...]string{"promptTokenCount", "cachedContentTokenCount", "candidatesTokenCount",
		"thoughtsTokenCount", "toolUsePromptTokenCount", "totalTokenCount"} {
		sum, present := 0, false
		for _, h := range hops {
			present = present || h.Get(key).Exists()
			sum += count(h, key)
		}
		if present {
			merged.SetInt(key, sum)
		}
	}
	var first wire.Value
	if len(hops) > 0 {
		first = hops[0]
	}
	merged.SetInt("initialPromptTokenCount", count(first, "promptTokenCount"))
	merged.SetInt("initialCachedContentTokenCount", count(first, "cachedContentTokenCount"))
	return merged.JSON()
}

// Token counters of native usage metadata, thinking folded into output as the chat vocabulary counts it
func usageOf(meta wire.Value) *wire.Usage {
	if !meta.IsObject() {
		return nil
	}
	prompt, cached, thoughts := count(meta, "promptTokenCount"), count(meta, "cachedContentTokenCount"), count(meta, "thoughtsTokenCount")
	u := &wire.Usage{InputTokens: max(0, prompt-cached), OutputTokens: count(meta, "candidatesTokenCount") + thoughts,
		CacheReadTokens: cached, ReasoningTokens: thoughts, Requests: 1}
	if meta.Get("initialPromptTokenCount").Exists() {
		u.FirstHopInput, u.FirstHopCached = count(meta, "initialPromptTokenCount"), count(meta, "initialCachedContentTokenCount")
	}
	return u
}

// Arguments of a native function call as JSON text, keys sorted when asked
func dumpArgs(fc wire.Value, sorted bool) string {
	args := fc.Get("args")
	if !args.Truthy() {
		return "{}"
	}
	text, ok := wire.FormatJSON(args.Raw(), wire.TextLayout{Spaced: true, UTF8: true, Sorted: sorted})
	if !ok {
		return "{}"
	}
	return text
}

// Provider id of a function call, a freshly minted one when it carries none
func newCallID(fc wire.Value) string {
	if id := fc.Get("id").Text(); id != "" {
		return id
	}
	return "call_" + randomHex(12)
}

// extra_content sidecar holding a part's thought signature, nil when the part has none
func signatureExtra(part wire.Value) json.RawMessage {
	sig := part.Get("thoughtSignature").Text()
	if sig == "" {
		return nil
	}
	inner := (&wire.Dict{}).SetString("thought_signature", sig)
	return (&wire.Dict{}).Set("google", inner.JSON()).JSON()
}

// Function call of a part, absent unless it names a function
func functionCall(part wire.Value) wire.Value {
	fc := part.Get("functionCall")
	if !fc.IsObject() || !fc.Get("name").Truthy() {
		return wire.Value{}
	}
	return fc
}

// Normalized reply for one complete generateContent response
func (d *decoder) translate(resp wire.Value, header http.Header) *wire.Response {
	candidates := resp.Get("candidates").Elems()
	cand := candidate(resp)
	var visible, thoughts []string
	var calls []wire.ToolCall
	signature := ""
	for _, part := range cand.Path("content", "parts").Elems() {
		if !part.IsObject() {
			continue
		}
		if text, ok := part.Get("text").Str(); ok {
			if part.Get("thought").True() {
				thoughts = append(thoughts, text)
			} else {
				visible = append(visible, text)
			}
			if sig := part.Get("thoughtSignature").Text(); sig != "" && !part.Get("functionCall").Exists() {
				signature = sig
			}
		} else if fc := functionCall(part); fc.Exists() {
			calls = append(calls, wire.ToolCall{ID: newCallID(fc), Name: fc.Get("name").Display(), Arguments: dumpArgs(fc, false),
				ExtraContent: signatureExtra(part)})
		}
	}
	finish, raw := wire.FinishToolCalls, cand.Get("finishReason").Text()
	if len(calls) == 0 {
		finish = mapFinish(raw)
	}
	m := wire.Message{Role: wire.RoleAssistant, Text: strings.Join(visible, ""), ToolCalls: calls, ThoughtSignature: signature}
	if reasoning := strings.Join(thoughts, ""); reasoning != "" {
		m.Reasoning, m.ReasoningContent = reasoning, &reasoning
	}
	r := &wire.Response{Message: m, FinishReason: finish, RawFinish: raw, ID: "chatcmpl-" + randomHex(12), Model: d.model,
		Header: header, ServedModel: wire.ServedModel(header)}
	if len(candidates) > 0 {
		r.Usage = usageOf(resp.Get("usageMetadata"))
	}
	return r
}

// One response whose model turn joins the parts of every hop, ending as the last hop did
func mergeHops(hops []wire.Value) wire.Value {
	var parts []json.RawMessage
	usage := make([]wire.Value, len(hops))
	for i, hop := range hops {
		for _, part := range candidate(hop).Path("content", "parts").Elems() {
			parts = append(parts, part.Raw())
		}
		usage[i] = hop.Get("usageMetadata")
	}
	last := hops[len(hops)-1]
	cand := candidate(last)
	content := wire.DictOf(cand.Get("content")).SetString("role", "model").Set("parts", wire.Array(parts...))
	merged := wire.DictOf(cand).Set("content", content.JSON())
	return wire.DictOf(last).Set("candidates", wire.Array(merged.JSON())).Set("usageMetadata", mergeHopUsage(usage)).Value()
}

// Assembles a complete reply body, asking for the next hop while the server answers with a continuation
func (d *decoder) Response(header http.Header, body []byte) (*wire.Response, error) {
	v, err := wire.ParseValue(body)
	if err != nil {
		return nil, wire.NewAPIErrorDetail(http.StatusOK, header, "gemini_invalid_json", "Invalid JSON from Gemini native API", body)
	}
	d.hops = append(d.hops, v)
	if token := continuationToken(v); token != "" && len(d.hops) < d.maxHops {
		return &wire.Response{Next: d.call(token), Header: header}, nil
	}
	if len(d.hops) > 1 {
		v = mergeHops(d.hops)
	}
	return d.translate(v, header), nil
}

// Delivers one event stamped with the time it was read
func (d *decoder) emit(e wire.Event) {
	if d.sink != nil {
		e.At = time.Now()
		d.sink(e)
	}
}

// Releases held-back text to the sink
func (d *decoder) flush() {
	for _, text := range d.pending {
		d.emit(wire.Event{Kind: wire.EventText, Text: text})
	}
	d.pending = nil
}

// Slot a streamed function call belongs to, by provider id and else by position and argument growth
func (d *decoder) slotFor(fc, part wire.Value, partIndex int, args string) (string, *callSlot) {
	if id := fc.Get("id").Text(); id != "" {
		key := "id\x00" + id
		return key, d.slots[key]
	}
	key := "part\x00" + strconv.Itoa(partIndex) + "\x00" + fc.Get("name").Display() + "\x00" + part.Get("thoughtSignature").Text()
	slot := d.slots[key]
	if slot == nil || strings.HasPrefix(args, slot.arguments) {
		return key, slot
	}
	for _, other := range d.order {
		if strings.HasPrefix(other, key+"#") && strings.HasPrefix(args, d.slots[other].arguments) {
			return other, d.slots[other]
		}
	}
	return key + "#" + strconv.Itoa(len(d.slots)), nil
}

// Absorbs one stream event, reporting whether it produced anything for the turn
func (d *decoder) feed(event wire.Value) bool {
	cand := candidate(event)
	if len(event.Get("candidates").Elems()) == 0 {
		return false
	}
	produced := false
	for partIndex, part := range cand.Path("content", "parts").Elems() {
		if !part.IsObject() {
			continue
		}
		text, isText := part.Get("text").Str()
		if isText && part.Get("thought").True() {
			produced = true
			if text != "" {
				previous := ""
				if n := len(d.reasoning); n > 0 {
					previous = d.reasoning[n-1]
				}
				text = wire.SeparateGluedReasoning(previous, text)
				d.reasoning = append(d.reasoning, text)
				d.emit(wire.Event{Kind: wire.EventReasoning, Text: text})
			}
			continue
		}
		if text != "" {
			produced = true
			d.content = append(d.content, text)
			switch {
			case len(d.slots) == 0 && (len(d.pending) > 0 || wire.StreamTextMayBeSSE(text)):
				d.pending = append(d.pending, text)
				if !wire.StreamTextMayBeSSE(strings.Join(d.pending, "")) {
					d.flush()
				}
			default:
				d.emit(wire.Event{Kind: wire.EventText, Text: text})
			}
		}
		if sig := part.Get("thoughtSignature").Text(); sig != "" && !part.Get("functionCall").Exists() {
			produced, d.signature = true, sig
		}
		fc := functionCall(part)
		if !fc.Exists() {
			continue
		}
		produced = true
		d.flush()
		args := dumpArgs(fc, true)
		key, slot := d.slotFor(fc, part, partIndex, args)
		if slot == nil {
			slot = &callSlot{index: len(d.slots), id: newCallID(fc)}
			d.slots[key], d.order = slot, append(d.order, key)
		}
		fragment := strings.TrimPrefix(args, slot.arguments)
		slot.arguments, slot.name = args, fc.Get("name").Display()
		if extra := signatureExtra(part); extra != nil {
			slot.extra = extra
		}
		if !slot.notified {
			slot.notified = true
			d.emit(wire.Event{Kind: wire.EventToolStart, Tool: wire.ToolDelta{Index: slot.index, ID: slot.id, Name: slot.name}})
		}
		if fragment != "" {
			d.emit(wire.Event{Kind: wire.EventToolArgs, Tool: wire.ToolDelta{Index: slot.index, ID: slot.id, Args: fragment}})
		}
	}
	if raw := cand.Get("finishReason").Text(); raw != "" {
		produced = true
		d.rawFinish, d.finish = raw, mapFinish(raw)
		if len(d.slots) > 0 {
			d.finish = wire.FinishToolCalls
		}
		if meta := event.Get("usageMetadata"); meta.Truthy() {
			d.usage = usageOf(meta)
			d.emit(wire.Event{Kind: wire.EventUsage, Usage: d.usage})
		}
	}
	return produced
}

// Splits a continuation off one stream event, holding back the finish and usage of every hop but the last
func (d *decoder) splitContinuation(event wire.Value) (wire.Value, string) {
	cand := candidate(event)
	if token := continuationToken(event); token != "" {
		d.hopUsage = append(d.hopUsage, event.Get("usageMetadata"))
		stripped := wire.DictOf(cand)
		stripped.Delete("finishReason")
		stripped.Delete("continuationToken")
		out := wire.DictOf(event)
		out.Delete("usageMetadata")
		return out.Set("candidates", wire.Array(stripped.JSON())).Value(), token
	}
	if len(d.hopUsage) > 0 && cand.Get("finishReason").Truthy() {
		all := append(append([]wire.Value{}, d.hopUsage...), event.Get("usageMetadata"))
		return wire.DictOf(event).Set("usageMetadata", mergeHopUsage(all)).Value(), ""
	}
	if cand.Get("finishReason").Truthy() {
		return event, ""
	}
	return event, cand.Get("continuationToken").Text()
}

// Reply for what the stream delivered so far
func (d *decoder) partial() *wire.Response {
	m := wire.Message{Role: wire.RoleAssistant, Text: strings.Join(d.content, ""), ThoughtSignature: d.signature}
	if reasoning := strings.Join(d.reasoning, ""); reasoning != "" {
		m.ReasoningContent = &reasoning
	}
	for _, key := range d.order {
		s := d.slots[key]
		m.ToolCalls = append(m.ToolCalls, wire.ToolCall{ID: s.id, Name: s.name, Arguments: s.arguments, ExtraContent: s.extra})
	}
	return &wire.Response{Message: m, Usage: d.usage, Model: d.model, Streamed: true}
}

// Outcome of a stream that reached its end, the next hop, the finished turn, or why unfinished
func (d *decoder) ended() (*wire.Response, error) {
	if d.resume != "" && d.sends < maxTokenSends {
		d.sends++
		return &wire.Response{Next: d.call(d.resume), Streamed: true}, nil
	}
	finished := d.rawFinish != ""
	hasText := len(d.content) > 0 || len(d.reasoning) > 0
	if !finished && !hasText && len(d.slots) == 0 {
		return nil, wire.ErrEmptyStream
	}
	r := d.partial()
	if !finished && hasText && len(d.slots) == 0 && d.usage == nil {
		slog.Warn("gemini stream ended cleanly without a finish reason after delivering text with no tool calls")
		return r, wire.ErrStreamDropped
	}
	r.FinishReason, r.RawFinish = d.finish, d.rawFinish
	if !finished {
		r.FinishReason = wire.FinishStop
	}
	if apiErr := wire.StreamErrorFromText(r.Message.Text, string(r.FinishReason), nil); apiErr != nil {
		return r, apiErr
	}
	d.flush()
	r.ID = "stream-" + randomHex(32)
	return r, nil
}

// Assembles a streamed reply, resuming from the last checkpoint when the stream drops after one
func (d *decoder) Stream(ctx context.Context, body io.Reader, sink wire.Sink) (*wire.Response, error) {
	d.sink = sink
	reader := wire.NewEventReader(body, 0)
	reader.OnComment = func(string) { d.emit(wire.Event{Kind: wire.EventKeepalive}) }
	for {
		ev, err := reader.Next(ctx)
		if err == io.EOF || (err == nil && string(ev.Data) == "[DONE]") {
			return d.ended()
		}
		if err != nil {
			if d.resume != "" && d.sends < maxTokenSends && ctx.Err() == nil {
				slog.Warn("gemini stream dropped, resuming from its last checkpoint", "err", err)
				d.sends++
				return &wire.Response{Next: d.call(d.resume), Streamed: true}, err
			}
			return d.partial(), err
		}
		event, perr := wire.ParseValue(ev.Data)
		if perr != nil || !event.IsObject() {
			slog.Debug("skipping a gemini stream frame that is not a JSON object")
			continue
		}
		done := len(d.hopUsage)
		event, token := d.splitContinuation(event)
		if token != "" && len(d.hopUsage) > done && len(d.hopUsage) >= d.maxHops {
			capped := wire.DictOf(candidate(event)).SetString("finishReason", "MAX_TOKENS")
			event = wire.DictOf(event).Set("candidates", wire.Array(capped.JSON())).Set("usageMetadata", mergeHopUsage(d.hopUsage)).Value()
			token = ""
		}
		if produced := d.feed(event); token != "" || produced {
			if d.resume = token; token != "" {
				d.sends = 0
			}
		}
	}
}
