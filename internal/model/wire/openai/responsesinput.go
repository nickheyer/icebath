package openai

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Text an assistant image part becomes, since an assistant input image fails every replay
const assistantImagePlaceholder = "[Assistant image omitted during replay]"

// Reports whether a part type word names text
func textPartType(t string) bool { return t == "text" || t == "input_text" || t == "output_text" }

// Reports whether a part type word names an image the Responses wire can carry
func imagePartType(t string) bool { return t == "image_url" || t == "input_image" }

// Reports whether a part type word names video, which the Responses wire cannot carry
func videoPartType(t string) bool { return t == "video" || t == "video_url" || t == "input_video" }

// Lowercase trimmed type word of a part
func partType(t string) string { return strings.ToLower(strip(t)) }

// Text part type a role writes, output for the assistant and input for everyone else
func textTypeFor(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "input_text"
}

// Text part of the given type
func textPart(typ, text string) json.RawMessage {
	return (&wire.Dict{}).SetString("type", typ).SetString("text", text).JSON()
}

// Media type of an inline image the wire rejects, empty for accepted rasters and remote URLs
func unsupportedInlineImage(url string) string {
	header, _, _ := strings.Cut(url, ",")
	header = strings.ToLower(header)
	if !strings.HasPrefix(header, "data:image/") {
		return ""
	}
	subtype, _, _ := strings.Cut(header[len("data:image/"):], ";")
	if subtype = strip(subtype); subtype == "" {
		subtype = "unknown"
	}
	switch subtype {
	case "jpg", "jpeg", "png", "gif", "webp":
		return ""
	}
	return "image/" + subtype
}

// Image part for a URL, text for the assistant or an unsupported inline type, nil for an unkept empty URL
func (c *inputConv) imagePart(url, detail, role string, keepEmpty bool) json.RawMessage {
	if role == "assistant" {
		return textPart("output_text", assistantImagePlaceholder)
	}
	if url == "" && !keepEmpty {
		return nil
	}
	mime := unsupportedInlineImage(url)
	if mime == "image/svg+xml" && c.o.RasterizeSVG != nil {
		if png, ok := c.o.RasterizeSVG(url); ok {
			url, mime = png, ""
		}
	}
	if mime != "" {
		return textPart("input_text", "[image omitted: "+mime+" is not a supported image format]")
	}
	part := (&wire.Dict{}).SetString("type", "input_image").SetString("image_url", url)
	if strip(detail) != "" {
		part.SetString("detail", strip(detail))
	}
	return part.JSON()
}

// Type word, URL and detail of a part the message layer kept verbatim
func nativeImage(p *wire.Part) (url, detail string) {
	v, err := wire.ParseValue(p.Native)
	if err != nil {
		return "", ""
	}
	image := v.Get("image_url")
	detail = v.Get("detail").Text()
	if image.IsObject() {
		if d := image.Get("detail"); d.Exists() {
			detail = d.Text()
		}
		return image.Get("url").Text(), detail
	}
	return image.Text(), detail
}

// Responses parts of list content for a role with the joined text of its text parts, an error for video
func (c *inputConv) contentParts(parts []wire.Part, role string) ([]json.RawMessage, string, error) {
	for i := range parts {
		if t := partType(parts[i].TypeName); videoPartType(t) {
			return nil, "", &RequestError{"Codex Responses does not support " + t + " input; use a video-capable provider."}
		}
	}
	textType := textTypeFor(role)
	var out []json.RawMessage
	var text strings.Builder
	add := func(part json.RawMessage) {
		if part == nil {
			return
		}
		out = append(out, part)
		if v, err := wire.ParseValue(part); err == nil && v.Get("type").Text() == textType {
			text.WriteString(v.Get("text").Text())
		}
	}
	for i := range parts {
		p := &parts[i]
		t := partType(p.TypeName)
		switch {
		case len(p.Native) > 0 && imagePartType(t):
			url, detail := nativeImage(p)
			add(c.imagePart(url, detail, role, false))
		case len(p.Native) > 0:
		case p.Kind == wire.PartText && (t == "" || textPartType(t)):
			if p.Text != "" {
				add(textPart(textType, p.Text))
			}
		case p.Kind == wire.PartImage && (t == "" || imagePartType(t)):
			add(c.imagePart(p.URL, p.Detail, role, false))
		}
	}
	return out, text.String(), nil
}

// Call id within the 64 characters the API accepts, a digest standing in for a longer one
func clampCallID(id string) string {
	if utf8.RuneCountInString(id) <= maxKeyLength {
		return id
	}
	return "call_" + shaHex(id, 32)
}

// Canonical call id for an fc_ item id, empty for anything else
func canonicalCallID(itemID string) string {
	if strings.HasPrefix(itemID, "fc_") && len(itemID) > 3 {
		return "call_" + itemID[3:]
	}
	return ""
}

// Splits a stored tool id into its call id and response item id
func splitToolID(raw string) (callID, itemID string) {
	value := strip(raw)
	if head, tail, joined := strings.Cut(value, "|"); joined {
		return strip(head), strip(tail)
	}
	if strings.HasPrefix(value, "fc_") {
		return "", value
	}
	return value, ""
}

// Call id of a function call, explicit first, then the one inside a joined id, then a digest
func resolveCallID(rawCallID, rawItemID, name, arguments string, index int, canonicalize bool) string {
	embedded, item := splitToolID(rawItemID)
	callID := rawCallID
	if strip(callID) == "" {
		callID = embedded
	}
	if strip(callID) == "" && canonicalize {
		callID = canonicalCallID(item)
	}
	if strip(callID) == "" {
		callID = wire.DeterministicCallID(name, arguments, index)
	}
	return strip(callID)
}

// Item id of a function call in the fc_ form the Responses API requires
func functionCallItemID(callID, itemID string) string {
	if strings.HasPrefix(strip(itemID), "fc_") {
		return strip(itemID)
	}
	source := strip(callID)
	sanitized := strings.Map(func(r rune) rune {
		if r < utf8.RuneSelf && (r == '_' || r == '-' || unicode.IsDigit(r) || unicode.IsLetter(r)) {
			return r
		}
		return -1
	}, source)
	for _, candidate := range [...]string{source, sanitized} {
		switch {
		case strings.HasPrefix(candidate, "fc_"):
			return candidate
		case strings.HasPrefix(candidate, "call_") && len(candidate) > len("call_"):
			return "fc_" + candidate[len("call_"):]
		}
	}
	if sanitized != "" {
		return "fc_" + sanitized[:min(len(sanitized), 48)]
	}
	seed := source
	if seed == "" {
		seed = itemID
	}
	if seed == "" {
		seed = strings.ReplaceAll(uuid4(), "-", "")
	}
	sum := sha1.Sum([]byte(seed))
	return "fc_" + hex.EncodeToString(sum[:])[:24]
}

// Normalized status of a replayed assistant message, completed for anything unrecognized
func messageStatus(v wire.Value) string {
	if s, ok := v.Str(); ok {
		status := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strip(s)))
		switch status {
		case "completed", "incomplete", "in_progress":
			return status
		}
	}
	return "completed"
}

// Assistant message item with its id and phase added only when present
func messageItem(content []json.RawMessage, status, id, phase string) json.RawMessage {
	item := (&wire.Dict{}).SetString("type", "message").SetString("role", "assistant").SetString("status", status).
		Set("content", wire.Array(content...))
	if id != "" {
		item.SetString("id", id)
	}
	if phase != "" {
		item.SetString("phase", phase)
	}
	return item.JSON()
}

// Plain message item for a role, an assistant phase forwarded only for the values the API accepts on input
func roleMessageItem(role string, content json.RawMessage, phase wire.Value) json.RawMessage {
	item := (&wire.Dict{}).SetString("type", "message").SetString("role", role).Set("content", content)
	if p, ok := phase.Str(); ok && role == "assistant" {
		if cleaned := strings.ToLower(strip(p)); cleaned == "commentary" || cleaned == "final_answer" {
			item.SetString("phase", cleaned)
		}
	}
	return item.JSON()
}

// Replayable assistant message item from a stored one, its id kept only where the endpoint accepts it back
func assistantMessageItem(raw wire.Value, content []json.RawMessage, dropIDs bool, issuer string) json.RawMessage {
	id, phase := "", ""
	if nonblank(raw.Get("id")) && !dropIDs {
		if candidate := strip(raw.Get("id").Text()); utf8.RuneCountInString(candidate) <= maxKeyLength &&
			(issuer != "codex_backend" || strings.HasPrefix(candidate, "msg")) {
			id = candidate
		}
	}
	if nonblank(raw.Get("phase")) {
		phase = strip(raw.Get("phase").Text())
	}
	return messageItem(content, messageStatus(raw.Get("status")), id, phase)
}

// Canonical form of a persisted issuer stamp, a stamp for an unnamed endpoint taking its normalized URL
func canonicalIssuer(kind string) string {
	if rest, ok := strings.CutPrefix(kind, "other:"); ok {
		return IssuerKind(wire.ResponsesGeneric, "", rest)
	}
	return kind
}

// Model family on api.openai.com whose members read each other's reasoning, empty for every other model
func ReasoningFamily(model string) string {
	name := strings.ToLower(strip(model))
	for _, f := range [...]string{"gpt-5.4", "gpt-5.5", "gpt-5.6", "gpt-6"} {
		if name == f || strings.HasPrefix(name, f+"-") || strings.HasPrefix(name, f+".") {
			return f
		}
	}
	return ""
}

// Reports whether the current model can read reasoning a model minted, exact everywhere and by family on api.openai.com
func modelCanReadBlob(issuer string, itemModel wire.Value, current string) bool {
	if current == "" || itemModel.Kind() <= wire.KindNull || itemModel.Text() == current {
		return true
	}
	family := ReasoningFamily(current)
	rest, other := strings.CutPrefix(issuer, "other:")
	return family != "" && itemModel.IsString() && family == ReasoningFamily(itemModel.Text()) && other &&
		strings.ToLower(hostOf(rest)) == "api.openai.com"
}

// State of one conversion of chat history to Responses input items
type inputConv struct {
	p         *ResponsesProtocol
	o         *wire.ResponsesOptions
	issuer    string
	model     string
	replay    bool
	native    bool
	items     []json.RawMessage
	sources   []*wire.Message
	seenItems map[string]bool
	seenCalls map[string]int
	queue     map[string][]string
}

// Appends items that came from one source message
func (c *inputConv) emit(src *wire.Message, items ...json.RawMessage) {
	for _, item := range items {
		c.items = append(c.items, item)
		c.sources = append(c.sources, src)
	}
}

// Wire id for a replayed call, each repeat of an id past the first taking a numbered suffix
func (c *inputConv) callWireID(callID string) string {
	base := clampCallID(callID)
	n := c.seenCalls[base]
	c.seenCalls[base] = n + 1
	id := base
	if n > 0 {
		id = clampCallID(base + "_dup" + itoa(n))
	}
	c.queue[base] = append(c.queue[base], id)
	return id
}

// Wire id for a tool output, the one its call was given in call order
func (c *inputConv) outputWireID(callID string) string {
	base := clampCallID(callID)
	if q := c.queue[base]; len(q) > 0 {
		c.queue[base] = q[1:]
		return q[0]
	}
	return base
}

// Content of a role message, text as typed parts where the endpoint takes nothing else
func (c *inputConv) wireContent(parts []json.RawMessage, text, role string) json.RawMessage {
	if len(parts) > 0 {
		return wire.Array(parts...)
	}
	if c.issuer == "codex_backend" {
		return wire.Array(textPart(textTypeFor(role), text))
	}
	return wire.Quote(text)
}

// Persisted encrypted reasoning and compaction items of one turn that this endpoint and model can read back
func (c *inputConv) reasoningItems(m *wire.Message) []json.RawMessage {
	var out []json.RawMessage
	for _, raw := range m.ResponsesReasoning {
		ri, err := wire.ParseValue(raw)
		if err != nil || !ri.IsObject() || !ri.Get("encrypted_content").Truthy() {
			continue
		}
		id := ""
		if v := ri.Get("id"); v.Truthy() {
			id = v.Display()
		}
		if (id != "" && c.seenItems[id]) || (ri.Get("type").Text() == "compaction" && !c.native) {
			continue
		}
		stamp := ri.Get("_issuer_kind")
		itemIssuer := canonicalIssuer(stamp.Text())
		foreignIssuer := c.issuer != "" && stamp.Kind() > wire.KindNull && itemIssuer != c.issuer
		if foreignIssuer || !modelCanReadBlob(c.issuer, ri.Get("_issuer_model"), c.model) {
			if c.p.crossIssuerWarned.CompareAndSwap(false, true) {
				slog.Warn("dropping a reasoning item sealed to another issuer or model, as happens after a mid-conversation model switch",
					"item_issuer", itemIssuer, "item_model", ri.Get("_issuer_model").Text(), "issuer", c.issuer, "model", c.model)
			}
			continue
		}
		d := wire.DictOf(ri)
		d.Delete("id")
		d.Delete("_issuer_kind")
		d.Delete("_issuer_model")
		out = append(out, d.JSON())
		if id != "" {
			c.seenItems[id] = true
		}
	}
	return out
}

// Exact assistant message items of one turn, an id dropped when the turn carried encrypted reasoning
func (c *inputConv) messageItems(m *wire.Message) []json.RawMessage {
	linked := m.ReasoningTrimmed
	for _, raw := range m.ResponsesReasoning {
		if ri, err := wire.ParseValue(raw); err == nil && ri.Get("encrypted_content").Truthy() {
			linked = true
		}
	}
	var out []json.RawMessage
	for _, raw := range m.ResponsesMessages {
		item, err := wire.ParseValue(raw)
		if err != nil || item.Get("type").Text() != "message" || item.Get("role").Text() != "assistant" {
			continue
		}
		var content []json.RawMessage
		for _, part := range item.Get("content").Elems() {
			if t := strip(displayOrEmpty(part.Get("type"))); part.IsObject() && (t == "output_text" || t == "text") {
				text := part.Get("text")
				content = append(content, textPart("output_text", displayOrEmpty(text)))
			}
		}
		if len(content) == 0 {
			continue
		}
		if linked && item.Get("id").Truthy() {
			d := wire.DictOf(item)
			d.Delete("id")
			item = d.Value()
		}
		out = append(out, assistantMessageItem(item, content, !c.o.ReplayItemIDs, c.issuer))
	}
	return out
}

// Text of a value, empty for an absent or null one
func displayOrEmpty(v wire.Value) string {
	if v.Kind() <= wire.KindNull {
		return ""
	}
	return v.Display()
}

// Function call items for the tool calls of one assistant turn
func (c *inputConv) toolCallItems(m *wire.Message, start int) []json.RawMessage {
	var out []json.RawMessage
	for i := range m.ToolCalls {
		tc := &m.ToolCalls[i]
		if strip(tc.Name) == "" {
			continue
		}
		callID := resolveCallID(tc.CallID, tc.ID, tc.Name, tc.Arguments, start+len(out), true)
		args := strip(tc.Arguments)
		if args == "" {
			args = "{}"
		}
		out = append(out, (&wire.Dict{}).SetString("type", "function_call").SetString("call_id", c.callWireID(callID)).
			SetString("name", wire.CoerceToolName(tc.Name, "fn")).SetString("arguments", args).JSON())
	}
	return out
}

// Function call output item for a tool result, nothing when no call id can be found for it
func (c *inputConv) toolOutput(m *wire.Message) ([]json.RawMessage, error) {
	callID, itemID := splitToolID(m.ToolCallID)
	if callID == "" {
		if callID = canonicalCallID(itemID); callID == "" {
			callID = strip(m.ToolCallID)
		}
	}
	if callID == "" {
		return nil, nil
	}
	output := wire.Quote(m.Text)
	if m.Parts != nil {
		parts, _, err := c.contentParts(m.Parts, "user")
		if err != nil {
			return nil, err
		}
		output = wire.Quote("")
		if len(parts) > 0 {
			output = wire.Array(parts...)
		}
	}
	return []json.RawMessage{(&wire.Dict{}).SetString("type", "function_call_output").
		SetString("call_id", c.outputWireID(callID)).Set("output", output).JSON()}, nil
}

// Value of a member the message layer does not model, absent when the message has none
func memberOf(m *wire.Message, key string) wire.Value {
	for _, mb := range m.Members() {
		if mb.Key == key {
			v, err := wire.ParseValue(mb.Value)
			if err != nil {
				return wire.Value{}
			}
			return v
		}
	}
	return wire.Value{}
}

// Converts chat history to Responses input items, pruning around a replayed checkpoint when compaction is native
func (c *inputConv) convert(messages []wire.Message) ([]json.RawMessage, error) {
	for i := range messages {
		m := &messages[i]
		role := string(m.Role)
		if m.Role == wire.RoleTool {
			items, err := c.toolOutput(m)
			if err != nil {
				return nil, err
			}
			c.emit(m, items...)
			continue
		}
		if m.Role != wire.RoleUser && m.Role != wire.RoleAssistant {
			continue
		}
		var parts []json.RawMessage
		text := m.Text
		if m.Parts != nil {
			var err error
			if parts, text, err = c.contentParts(m.Parts, role); err != nil {
				return nil, err
			}
		}
		if m.Role == wire.RoleUser {
			c.emit(m, roleMessageItem(role, c.wireContent(parts, text, role), wire.Value{}))
			continue
		}
		var reasoning []json.RawMessage
		if c.replay {
			reasoning = c.reasoningItems(m)
		}
		c.emit(m, reasoning...)
		exact := c.messageItems(m)
		c.emit(m, exact...)
		follower, hasFollower := "", false
		if len(exact) == 0 {
			switch {
			case len(parts) > 0, strip(text) != "":
				follower, hasFollower = text, true
			case len(reasoning) > 0:
				hasFollower = true
			}
		}
		start := len(c.items)
		if hasFollower {
			start++
		}
		calls := c.toolCallItems(m, start)
		if hasFollower && (len(parts) > 0 || follower != "" || len(calls) == 0) {
			if len(parts) == 0 && follower == "" {
				follower = " "
			}
			c.emit(m, roleMessageItem("assistant", c.wireContent(parts, follower, role), memberOf(m, "phase")))
		}
		c.emit(m, calls...)
	}
	if !c.native {
		return c.items, nil
	}
	return c.o.Prune(c.items, c.sources), nil
}

// Copy of the messages keeping encrypted reasoning only on the newest assistant turn that has any
func newestReasoningOnly(messages []wire.Message) []wire.Message {
	out := slices.Clone(messages)
	kept := false
	for i := len(out) - 1; i >= 0; i-- {
		m := &out[i]
		if m.Role != wire.RoleAssistant {
			continue
		}
		var checkpoints []json.RawMessage
		blobs := false
		for _, raw := range m.ResponsesReasoning {
			item, err := wire.ParseValue(raw)
			if err != nil || !item.IsObject() {
				continue
			}
			if item.Get("type").Text() == "compaction" {
				checkpoints = append(checkpoints, raw)
			} else {
				blobs = true
			}
		}
		if !blobs {
			continue
		}
		if kept {
			*m = m.Clone()
			m.ReasoningTrimmed, m.ResponsesReasoning = true, checkpoints
		}
		kept = true
	}
	return out
}

// Reports whether the messages end on tool results the preceding assistant turn asked for
func postToolReplay(messages []wire.Message) bool {
	pairIDs := func(raw, explicit string) map[string]bool {
		embedded, item := splitToolID(raw)
		ids := map[string]bool{}
		if embedded != "" {
			ids[embedded] = true
		}
		if strip(explicit) != "" {
			ids[strip(explicit)] = true
		}
		if len(ids) == 0 && strip(raw) != "" {
			ids[strip(raw)] = true
		}
		if canonical := canonicalCallID(item); canonical != "" {
			ids[canonical] = true
		}
		return ids
	}
	trailing := map[string]bool{}
	for i := len(messages) - 1; i >= 0; i-- {
		m := &messages[i]
		switch m.Role {
		case wire.RoleSystem:
			continue
		case wire.RoleTool:
			ids := pairIDs(m.ToolCallID, "")
			if len(ids) == 0 {
				return false
			}
			for id := range ids {
				trailing[id] = true
			}
			continue
		case wire.RoleAssistant:
			for c := range m.ToolCalls {
				for id := range pairIDs(m.ToolCalls[c].ID, m.ToolCalls[c].CallID) {
					if trailing[id] {
						return true
					}
				}
			}
		}
		return false
	}
	return false
}
