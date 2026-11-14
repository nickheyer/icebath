package openai

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Shapes of a tool call the model wrote as text instead of emitting a function call item
var (
	harmonyCallLeak = regexp.MustCompile(`(?i)(?:^|[\s>|])to=functions\.[A-Za-z_][\w.]*`)
	shellJSONLeak   = regexp.MustCompile(`(?:^|\n)\s*\{\s*"cmd"\s*:\s*"(?:\\.|[^"\\])*"` +
		`(?:\s*,\s*"[A-Za-z_][\w-]*"\s*:\s*(?:"(?:\\.|[^"\\])*"|true|false|null|-?\d+(?:\.\d+)?))*\s*\}\s*$`)
	actionLeadIn = regexp.MustCompile(`(?i)^(?:(?:next|first|then|okay|ok|alright)\b[\s,—–-]*)?(?:sure,\s*)?(?:now\s+)?` +
		`(?:let\s+me\s+|i(?:'|’)?ll\s+|i\s+will\s+|i(?:'|’)?m\s+|i\s+am\s+)?` +
		`(?:creat(?:e|ing)|writ(?:e|ing)|runn?(?:ing)?|execut(?:e|ing)|check(?:ing)?|verif(?:y|ying)|updat(?:e|ing)|` +
		`install(?:ing)?|edit(?:ing)?|mak(?:e|ing))\b`)
)

// Reads Responses replies with the aliases and issuer stamp its request recorded
type responsesDecoder struct {
	aliases        map[string]string
	issuer         string
	issuerModel    string
	model          string
	trusted        bool
	noContinuation bool
}

// Typed failure for a 4xx or 5xx Responses reply
func (d *responsesDecoder) Error(status int, header http.Header, body []byte) error {
	return wire.NewAPIError(status, header, body)
}

// Reports whether assistant text carries a tool call the model failed to emit as a structured item
func leakedToolCall(text string) bool {
	if harmonyCallLeak.MatchString(text) {
		return true
	}
	loc := shellJSONLeak.FindStringIndex(text)
	if loc == nil {
		return false
	}
	lines := strings.Split(strings.ReplaceAll(strip(text[:loc[0]]), "\r\n", "\n"), "\n")
	last := strip(lines[len(lines)-1])
	return last != "" && actionLeadIn.MatchString(last)
}

// Lowercase trimmed text of a string value, with whether it was a string at all
func lowerOf(v wire.Value) (string, bool) {
	s, ok := v.Str()
	return strings.ToLower(strip(s)), ok
}

// Reports whether a status word marks an item or response as unfinished
func incompleteStatus(status string) bool {
	return status == "queued" || status == "in_progress" || status == "incomplete"
}

// Reports whether an output item is a provider-executed tool call, which stays in progress on a finished response
func serverSideCall(itemType string) bool {
	switch itemType {
	case "web_search_call", "file_search_call", "code_interpreter_call", "image_generation_call", "computer_call",
		"local_shell_call", "mcp_call":
		return true
	}
	return false
}

// Assistant text of a message output item, a refusal part counting as text
func messageText(item wire.Value) string {
	var b strings.Builder
	for _, part := range item.Get("content").Elems() {
		switch part.Get("type").Text() {
		case "refusal":
			b.WriteString(part.Get("refusal").Text())
		case "output_text", "text":
			b.WriteString(part.Get("text").Text())
		}
	}
	return strip(b.String())
}

// Compact reasoning text of a reasoning item, its summary parts first and else its text
func reasoningText(item wire.Value) string {
	var chunks []string
	for _, part := range item.Get("summary").Elems() {
		if text := part.Get("text").Text(); text != "" {
			chunks = append(chunks, text)
		}
	}
	if len(chunks) > 0 {
		return strip(strings.Join(chunks, "\n"))
	}
	return strip(item.Get("text").Text())
}

// Text every output_text part of the message items holds, the aggregate a reply without output_text stands for
func aggregateOutputText(response wire.Value) string {
	if text, ok := response.Get("output_text").Str(); ok {
		return text
	}
	var b strings.Builder
	for _, item := range response.Get("output").Elems() {
		if item.Get("type").Text() != "message" {
			continue
		}
		for _, part := range item.Get("content").Elems() {
			if part.Get("type").Text() == "output_text" {
				b.WriteString(part.Get("text").Text())
			}
		}
	}
	return b.String()
}

// Failure for a response whose status is failed or cancelled
func responseFailure(errObj wire.Value, status string, header http.Header, inStream bool) error {
	field := func(name string) string {
		v := errObj.Get(name)
		if v.IsString() || v.Truthy() {
			return strip(v.Display())
		}
		return ""
	}
	code, message := field("code"), field("message")
	if message == "" && code == "" {
		if errObj.Truthy() {
			message = errObj.Display()
		} else {
			message = "Responses API returned status '" + status + "'"
		}
	}
	body := (&wire.Dict{}).SetString("message", message)
	if code != "" {
		body.SetString("code", code)
	}
	payload := (&wire.Dict{}).Set("error", body.JSON()).JSON()
	if inStream {
		return wire.StreamErrorFromPayload(payload, header)
	}
	return wire.NewAPIErrorDetail(0, header, code, message, payload)
}

// Tool call for a function_call or custom_tool_call output item
func (d *responsesDecoder) toolCall(item wire.Value, itemType string, index int) wire.ToolCall {
	name := ""
	if v := item.Get("name"); v.Truthy() {
		name = v.Display()
	}
	key := "arguments"
	if itemType != "function_call" {
		key = "input"
	}
	arguments := "{}"
	if v := item.Get(key); v.IsString() {
		arguments = v.Text()
	} else if v.Exists() {
		arguments, _ = wire.FormatJSON(v.Raw(), wire.TextLayout{Spaced: true, UTF8: true})
	}
	itemID := item.Get("id").Text()
	callID := resolveCallID(item.Get("call_id").Text(), itemID, name, arguments, index, false)
	if original, ok := d.aliases[name]; ok {
		name = original
	}
	return wire.ToolCall{ID: callID, CallID: callID, ResponseItemID: functionCallItemID(callID, itemID), Name: name, Arguments: arguments}
}

// Replayable form of an encrypted reasoning or compaction item stamped with its issuer, nil without a blob
func (d *responsesDecoder) encryptedItem(item wire.Value, itemType string) json.RawMessage {
	encrypted := item.Get("encrypted_content")
	if encrypted.Text() == "" {
		return nil
	}
	raw := (&wire.Dict{}).SetString("type", itemType).Set("encrypted_content", encrypted.Raw())
	if d.issuer != "" {
		raw.SetString("_issuer_kind", d.issuer)
	}
	if d.issuerModel != "" {
		raw.SetString("_issuer_model", d.issuerModel)
	}
	if itemType != "reasoning" {
		return raw.JSON()
	}
	id := item.Get("id").Text()
	if strings.HasPrefix(id, "rs_tmp_") {
		return nil
	}
	if id != "" {
		raw.SetString("id", id)
	}
	if summary := item.Get("summary"); summary.IsArray() {
		parts := []json.RawMessage{}
		for _, part := range summary.Elems() {
			if text, ok := part.Get("text").Str(); ok {
				parts = append(parts, textPart("summary_text", text))
			}
		}
		raw.Set("summary", wire.Array(parts...))
	}
	return raw.JSON()
}

// Normalizes a response object into the assistant message, its finish reason and the replay sidecars
func (d *responsesDecoder) normalize(response wire.Value, header http.Header, inStream bool) (*wire.Response, error) {
	status, _ := lowerOf(response.Get("status"))
	incompleteReason := strings.ToLower(strip(response.Path("incomplete_details", "reason").Text()))
	contentFilter := status == "incomplete" && incompleteReason == "content_filter"
	output := response.Get("output").Elems()
	outputText := aggregateOutputText(response)
	if len(output) == 0 {
		content := []json.RawMessage{}
		switch {
		case strip(outputText) != "":
			content = append(content, textPart("output_text", strip(outputText)))
		case !contentFilter:
			return nil, ErrInvalidResponse
		}
		synthesized, _ := wire.ParseValue((&wire.Dict{}).SetString("type", "message").SetString("role", "assistant").
			SetString("status", "completed").Set("content", wire.Array(content...)).JSON())
		output = []wire.Value{synthesized}
	}
	if status == "failed" || status == "cancelled" {
		return nil, responseFailure(response.Get("error"), status, header, inStream)
	}
	var contentParts, reasoningParts []string
	var calls []wire.ToolCall
	var reasoningItems, messageItems []json.RawMessage
	hasIncomplete := incompleteStatus(status)
	sawUnfinished := status == "queued" || status == "in_progress"
	sawCommentary, sawFinal, sawReasoning := false, false, false
	for _, item := range output {
		itemType := item.Get("type").Text()
		itemStatus, hasStatus := lowerOf(item.Get("status"))
		if hasStatus && incompleteStatus(itemStatus) && !serverSideCall(itemType) {
			hasIncomplete, sawUnfinished = true, true
		}
		switch {
		case itemType == "message":
			phase, _ := lowerOf(item.Get("phase"))
			commentary := phase == "commentary" || phase == "analysis"
			sawCommentary = sawCommentary || commentary
			sawFinal = sawFinal || phase == "final_answer" || phase == "final"
			text := messageText(item)
			if text == "" {
				continue
			}
			if commentary {
				reasoningParts = append(reasoningParts, text)
			} else {
				contentParts = append(contentParts, text)
			}
			messageItems = append(messageItems, messageItem([]json.RawMessage{textPart("output_text", text)},
				messageStatus(item.Get("status")), item.Get("id").Text(), phase))
		case itemType == "reasoning" || itemType == "compaction":
			if itemType == "reasoning" {
				sawReasoning = true
				if text := reasoningText(item); text != "" {
					reasoningParts = append(reasoningParts, text)
				}
			}
			if raw := d.encryptedItem(item, itemType); raw != nil {
				reasoningItems = append(reasoningItems, raw)
				if itemType == "compaction" {
					slog.Info("native Responses compaction item captured", "chars", len(item.Get("encrypted_content").Text()))
				}
			}
		case itemType == "custom_tool_call", itemType == "function_call" && !(hasStatus && incompleteStatus(itemStatus)):
			calls = append(calls, d.toolCall(item, itemType, len(calls)))
		}
	}
	finalText := strip(strings.Join(contentParts, "\n"))
	if finalText == "" && (sawFinal || !sawCommentary) {
		finalText = strip(outputText)
	}
	leaked := !d.noContinuation && finalText != "" && len(calls) == 0 && leakedToolCall(finalText)
	if leaked {
		slog.Warn("Responses reply wrote a tool call as assistant text, treating the turn as incomplete so it is asked again")
		finalText = ""
	}
	if d.issuer == "xai_responses" && finalText == "" && len(calls) == 0 && len(reasoningParts) > 0 {
		joined := strings.Join(reasoningParts, "\n\n")
		if marker := strings.LastIndex(joined, "<response>"); marker >= 0 {
			salvaged, _, _ := strings.Cut(joined[marker+len("<response>"):], "</response>")
			if salvaged = strip(salvaged); salvaged != "" {
				slog.Warn("xAI delivered its final answer inside the reasoning channel, promoting it to assistant content", "chars", len(salvaged))
				finalText, reasoningParts = salvaged, nil
				if prefix := strip(joined[:marker]); prefix != "" {
					reasoningParts = []string{prefix}
				}
			}
		}
	}
	m := wire.Message{Role: wire.RoleAssistant, Text: finalText, ToolCalls: calls, ResponsesReasoning: reasoningItems}
	m.Reasoning = strip(strings.Join(reasoningParts, "\n\n"))
	if !leaked {
		m.ResponsesMessages = messageItems
	}
	reasoningOnly := (len(reasoningItems) > 0 || len(reasoningParts) > 0 || sawReasoning) && finalText == ""
	trustedFinal := status == "completed" && d.trusted && d.issuer != "codex_backend" && d.issuer != "xai_responses"
	finish := wire.FinishStop
	switch {
	case len(calls) > 0:
		finish = wire.FinishToolCalls
	case contentFilter:
		finish = wire.FinishContentFilter
	case leaked, sawUnfinished, (hasIncomplete || sawCommentary) && !sawFinal, reasoningOnly && !trustedFinal:
		finish = wire.FinishIncomplete
	}
	r := &wire.Response{Message: m, FinishReason: finish, RawFinish: status, Incomplete: incompleteReason,
		ID: response.Get("id").Text(), Model: response.Get("model").Text(), Header: header, ServedModel: wire.ServedModel(header),
		IssuerKind: d.issuer, IssuerModel: d.issuerModel}
	if r.Model == "" {
		r.Model = d.model
	}
	if r.Usage = wire.ParseUsage(response.Get("usage").Raw(), wire.UsageResponses); r.Usage != nil {
		if tier := response.Get("service_tier").Text(); tier != "" {
			r.Usage.ServedTier = tier
		}
	}
	return r, nil
}

// Assembles a complete Responses reply body
func (d *responsesDecoder) Response(header http.Header, body []byte) (*wire.Response, error) {
	v, err := wire.ParseValue(body)
	if err != nil || !v.IsObject() {
		return nil, wire.NewStreamError(wire.StreamNonJSON, string(body))
	}
	return d.normalize(v, header, false)
}

// Function call announced by the stream whose finished item has not arrived
type pendingCall struct {
	key       string
	item      wire.Value
	arguments string
	index     *int64
	sequence  int
}

// Position an output item was first announced at
type announcedOrder struct {
	sequence int
	index    *int64
}

// One finished output item with the position it merges back at
type settledItem struct {
	item     wire.Value
	index    *int64
	sequence int
}

// Assembly state of one Responses event stream, output coming from finished items and never from the terminal frame
type responsesStream struct {
	d              *responsesDecoder
	sink           wire.Sink
	done           []settledItem
	text           []string
	commentary     []string
	pending        []*pendingCall
	announced      map[string]announcedOrder
	phases         map[string]string
	phaseKnown     map[string]bool
	activePhase    string
	summaryIndex   string
	nextSequence   int
	hasToolCalls   bool
	sawTerminal    bool
	sawCompleted   bool
	sawAnswerItem  bool
	terminalStatus string
	terminal       wire.Value
	terminalType   string
}

// Delivers one event stamped with the time it was read
func (s *responsesStream) emit(e wire.Event) {
	if s.sink != nil {
		e.At = time.Now()
		s.sink(e)
	}
}

// Whole-number output index of an event, nil when it has none
func outputIndex(v wire.Value) *int64 {
	if n, ok := v.Int(); ok {
		return &n
	}
	return nil
}

// Reports whether two optional indexes name the same position
func sameIndex(a, b *int64) bool {
	return a != nil && b != nil && *a == *b
}

// Pending call an event refers to, by item id and else by output index, since some backends rotate item ids
func (s *responsesStream) pendingFor(itemID string, index *int64) *pendingCall {
	for _, p := range s.pending {
		if p.key == itemID {
			return p
		}
	}
	for _, p := range s.pending {
		if sameIndex(p.index, index) {
			return p
		}
	}
	return nil
}

// Lowercase phase of a message item, empty when it names none
func phaseOf(item wire.Value) string {
	phase, _ := lowerOf(item.Get("phase"))
	return phase
}

// Absorbs one event, reporting whether it was a terminal frame
func (s *responsesStream) feed(event wire.Value, eventName string) (bool, error) {
	eventType := event.Get("type").Text()
	if eventType == "" {
		eventType = eventName
	}
	switch {
	case eventType == "error":
		nested := event.Get("error")
		field := func(name string) wire.Value {
			if v := event.Get(name); v.Kind() > wire.KindNull {
				return v
			}
			return nested.Get(name)
		}
		message := strip(displayOrEmpty(field("message")))
		if message == "" {
			message = "stream emitted error event"
		}
		body := (&wire.Dict{}).SetString("message", message)
		for _, name := range [...]string{"code", "param"} {
			if v := field(name); v.Kind() > wire.KindNull {
				body.Set(name, v.Raw())
			}
		}
		return false, wire.StreamErrorFromPayload((&wire.Dict{}).Set("error", body.JSON()).JSON(), nil)
	case eventType == "response.output_item.added":
		s.itemAdded(event)
	case eventType == "response.output_item.done":
		s.itemDone(event)
	case eventType == "response.completed" || eventType == "response.incomplete" || eventType == "response.failed":
		s.sawTerminal, s.terminalType = true, eventType
		if response := event.Get("response"); response.Kind() > wire.KindNull {
			s.terminal = response
			if status, ok := response.Get("status").Str(); ok {
				s.terminalStatus = status
			}
			if usage := wire.ParseUsage(response.Get("usage").Raw(), wire.UsageResponses); usage != nil {
				s.emit(wire.Event{Kind: wire.EventUsage, Usage: usage})
			}
		}
		s.sawCompleted = s.sawCompleted || eventType == "response.completed"
		if s.terminalStatus == "" {
			s.terminalStatus = strings.TrimPrefix(eventType, "response.")
		}
		return true, nil
	case eventType == "response.refusal.delta":
		if delta := event.Get("delta").Text(); delta != "" {
			s.text = append(s.text, delta)
			s.emit(wire.Event{Kind: wire.EventRefusal, Text: delta})
		}
	case strings.Contains(eventType, "output_text.delta"):
		s.textDelta(event)
	case strings.Contains(eventType, "function_call"):
		s.hasToolCalls = true
		p := s.pendingFor(event.Get("item_id").Text(), outputIndex(event.Get("output_index")))
		if p == nil {
			break
		}
		if strings.Contains(eventType, "delta") {
			delta := event.Get("delta").Text()
			p.arguments += delta
			if delta != "" {
				s.emit(wire.Event{Kind: wire.EventToolArgs, Tool: wire.ToolDelta{ID: p.item.Get("call_id").Text(), Args: delta}})
			}
		} else if strings.HasSuffix(eventType, "function_call_arguments.done") {
			if args := event.Get("arguments"); args.Kind() > wire.KindNull {
				p.arguments = args.Display()
			}
		}
	case strings.Contains(eventType, "reasoning") && strings.Contains(eventType, "delta"):
		delta := event.Get("delta").Text()
		if delta == "" {
			break
		}
		if index := event.Get("summary_index"); index.Kind() > wire.KindNull {
			if s.summaryIndex != "" && s.summaryIndex != index.Display() {
				delta = "\n\n" + delta
			}
			s.summaryIndex = index.Display()
		}
		s.emit(wire.Event{Kind: wire.EventReasoning, Text: delta})
	default:
		s.emit(wire.Event{Kind: wire.EventKeepalive})
	}
	return false, nil
}

// Records an announced output item, its phase, its position and a function call awaiting its finished item
func (s *responsesStream) itemAdded(event wire.Value) {
	item := event.Get("item")
	itemType := item.Get("type").Text()
	s.activePhase = ""
	if itemType == "message" {
		s.activePhase = phaseOf(item)
		if s.activePhase != "commentary" && s.activePhase != "analysis" {
			s.sawAnswerItem = true
		}
	}
	if s.activePhase == "commentary" {
		s.commentary = nil
	}
	id := displayOrEmpty(item.Get("id"))
	if id != "" && itemType == "message" {
		s.phases[id], s.phaseKnown[id] = s.activePhase, true
	}
	if _, seen := s.announced[id]; id != "" && !seen {
		s.announced[id] = announcedOrder{s.nextSequence, outputIndex(event.Get("output_index"))}
		s.nextSequence++
	}
	if !strings.Contains(itemType, "function_call") {
		return
	}
	s.hasToolCalls = true
	name := item.Get("name").Text()
	s.emit(wire.Event{Kind: wire.EventToolStart, Tool: wire.ToolDelta{ID: item.Get("call_id").Text(), Name: name}})
	if id == "" {
		return
	}
	order := s.announced[id]
	call := &pendingCall{key: id, item: item, arguments: displayOrEmpty(item.Get("arguments")), index: order.index, sequence: order.sequence}
	for i, p := range s.pending {
		if p.key == id {
			s.pending[i] = call
			return
		}
	}
	s.pending = append(s.pending, call)
}

// Routes a text delta by the phase of the message it belongs to
func (s *responsesStream) textDelta(event wire.Value) {
	delta := event.Get("delta").Text()
	if delta == "" {
		return
	}
	phase := s.activePhase
	if id := event.Get("item_id").Text(); s.phaseKnown[id] {
		phase = s.phases[id]
	}
	switch phase {
	case "commentary":
		s.commentary = append(s.commentary, delta)
		s.emit(wire.Event{Kind: wire.EventKeepalive})
	case "analysis":
		s.emit(wire.Event{Kind: wire.EventReasoning, Text: delta})
	default:
		s.text = append(s.text, delta)
		if !s.hasToolCalls {
			s.emit(wire.Event{Kind: wire.EventText, Text: delta})
		}
	}
}

// Records a finished output item at its announced position and retires the pending calls it settles
func (s *responsesStream) itemDone(event wire.Value) {
	item := event.Get("item")
	if item.Kind() <= wire.KindNull {
		return
	}
	id := displayOrEmpty(item.Get("id"))
	sequence, known := 0, false
	var index *int64
	if order, ok := s.announced[id]; ok {
		sequence, index, known = order.sequence, order.index, true
	}
	var settled []*pendingCall
	eventIndex := outputIndex(event.Get("output_index"))
	if strings.Contains(item.Get("type").Text(), "function_call") {
		if callID := item.Get("call_id"); nonblank(callID) {
			for _, p := range s.pending {
				if p.item.Get("call_id").Text() == callID.Text() {
					settled = append(settled, p)
				}
			}
			if len(settled) == 0 {
				if p := s.pendingFor(id, eventIndex); p != nil && !p.item.Get("call_id").Truthy() {
					settled = append(settled, p)
				}
			}
			known, index = false, nil
		} else if p := s.pendingFor(id, eventIndex); p != nil {
			settled = append(settled, p)
		}
		if len(settled) > 0 {
			first := slices.MinFunc(settled, func(a, b *pendingCall) int { return a.sequence - b.sequence })
			sequence, index, known = first.sequence, first.index, true
		}
	}
	if !known {
		sequence = s.nextSequence
		s.nextSequence++
	}
	if eventIndex != nil {
		index = eventIndex
	}
	s.done = append(s.done, settledItem{item, index, sequence})
	s.pending = slices.DeleteFunc(s.pending, func(p *pendingCall) bool { return slices.Contains(settled, p) })
	if phaseOf(item) == "commentary" {
		text := strip(strings.Join(s.commentary, ""))
		if text == "" {
			var b strings.Builder
			for _, part := range item.Get("content").Elems() {
				if part.Get("type").Text() == "output_text" {
					b.WriteString(displayOrEmpty(part.Get("text")))
				}
			}
			text = strip(b.String())
		}
		if text != "" {
			s.emit(wire.Event{Kind: wire.EventMessage, Message: &wire.Message{Role: wire.RoleAssistant, Text: text}})
		}
		s.commentary = nil
	}
}

// Response object the stream amounts to, finished items merged with the calls a successful stream never confirmed
func (s *responsesStream) result() (wire.Value, error) {
	items := slices.Clone(s.done)
	if s.sawCompleted {
		for _, p := range s.pending {
			arguments := strip(p.arguments)
			if arguments == "" {
				arguments = "{}"
			}
			call := (&wire.Dict{}).SetString("type", "function_call")
			for _, key := range [...]string{"id", "call_id", "name"} {
				if v := p.item.Get(key); v.Kind() > wire.KindNull {
					call.Set(key, v.Raw())
				}
			}
			items = append(items, settledItem{call.SetString("status", "completed").SetString("arguments", arguments).Value(), p.index, p.sequence})
		}
		indexed := !slices.ContainsFunc(items, func(it settledItem) bool { return it.index == nil })
		slices.SortStableFunc(items, func(a, b settledItem) int {
			if indexed {
				return int(*a.index - *b.index)
			}
			return a.sequence - b.sequence
		})
	}
	output := make([]json.RawMessage, len(items))
	answered, firstCall := false, len(items)
	for i, it := range items {
		output[i] = it.item.Raw()
		itemType := it.item.Get("type").Text()
		if phase := phaseOf(it.item); itemType == "message" && phase != "commentary" && phase != "analysis" {
			answered = true
		}
		if strings.Contains(itemType, "function_call") && firstCall == len(items) {
			firstCall = i
		}
	}
	text := strings.Join(s.text, "")
	if len(s.text) > 0 && ((len(output) == 0 && !s.hasToolCalls) || (s.sawTerminal && s.sawAnswerItem && !answered)) {
		message := (&wire.Dict{}).SetString("type", "message").SetString("role", "assistant").SetString("status", "completed").
			Set("content", wire.Array(textPart("output_text", text))).JSON()
		output = slices.Insert(output, firstCall, message)
	}
	if !s.sawTerminal && len(output) == 0 {
		return wire.Value{}, ErrNoTerminal
	}
	response := (&wire.Dict{}).Set("output", wire.Array(output...)).SetString("output_text", text).SetString("status", s.terminalStatus).
		SetString("model", s.d.model)
	for _, key := range [...]string{"usage", "id", "service_tier"} {
		if v := s.terminal.Get(key); v.Kind() > wire.KindNull {
			response.Set(key, v.Raw())
		}
	}
	if v := s.terminal.Get("incomplete_details"); s.terminalType == "response.incomplete" && v.Kind() > wire.KindNull {
		response.Set("incomplete_details", v.Raw())
	}
	if v := s.terminal.Get("error"); s.terminalType == "response.failed" && v.Kind() > wire.KindNull {
		response.Set("error", v.Raw())
	}
	return response.Value(), nil
}

// Reply for what the stream delivered before it failed, nil when nothing usable arrived
func (s *responsesStream) partial() *wire.Response {
	text := strings.Join(s.text, "")
	if text == "" && len(s.done) == 0 {
		return nil
	}
	return &wire.Response{Message: wire.Message{Role: wire.RoleAssistant, Text: text}, Model: s.d.model, Streamed: true,
		IssuerKind: s.d.issuer, IssuerModel: s.d.issuerModel}
}

// Assembles a streamed Responses reply, delivering events to sink as frames arrive
func (d *responsesDecoder) Stream(ctx context.Context, body io.Reader, sink wire.Sink) (*wire.Response, error) {
	s := &responsesStream{d: d, sink: sink, announced: map[string]announcedOrder{}, phases: map[string]string{},
		phaseKnown: map[string]bool{}, terminalStatus: "completed"}
	reader := wire.NewEventReader(body, 0)
	reader.OnComment = func(string) { s.emit(wire.Event{Kind: wire.EventKeepalive}) }
	for {
		ev, err := reader.Next(ctx)
		if err == io.EOF || (err == nil && ev.Done()) {
			break
		}
		if err != nil {
			return s.partial(), err
		}
		payload, err := ev.Payload()
		if err != nil {
			return s.partial(), err
		}
		event, err := wire.ParseValue(payload)
		if err != nil {
			return s.partial(), err
		}
		terminal, err := s.feed(event, ev.Event)
		if err != nil {
			return s.partial(), err
		}
		if terminal {
			break
		}
	}
	response, err := s.result()
	if err != nil {
		return s.partial(), err
	}
	r, err := d.normalize(response, nil, true)
	if r != nil {
		r.Streamed = true
	}
	if err != nil {
		return s.partial(), err
	}
	return r, nil
}
